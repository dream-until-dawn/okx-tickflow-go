// Package indicator 提供技术指标的计算，内置常用的七个，并支持外部自行实现。
//
// # 增量式
//
// 所有指标都是【流式】的：每来一根 K 线调一次 Update，代价与历史长度无关。
// 回测要走上百万步，只能这样。
//
// # 与批量结果一致，不是靠测出来的
//
// 窗口类指标（MA / BOLL / CCI / KDJ 的 RSV）每步在窗口上【重算】，而不是
// 增量维护累加和。增量维护 sum 会随步数累积浮点漂移，跑几百万根之后与
// 「把这 n 根拿出来算一遍」对不上——那正是批量定义。
//
// 递归类指标（EMA / MACD / RSI）本就以递推定义，批量算法与流式算法是同一个。
//
// 于是「增量 == 批量」是结构上成立的，而不是碰巧测过。
//
// 重算的代价是 O(n)，实测（Ryzen 7 5700X）：
//
//	MA(20)      11 ns/步        MA(200)    118 ns/步
//	EMA(20)      5 ns/步        MACD        7 ns/步
//	BOLL(20)    35 ns/步        CCI(20)    39 ns/步
//	KDJ(9,3,3)  42 ns/步        RSI(14)     7 ns/步
//
//	九个指标一整套（3 条 MA + EMA + MACD + KDJ + RSI + CCI + BOLL）202 ns/步
//
// 全部零分配。百万步的回测在指标上一共花 0.2 秒——为省这点开销去换一个会
// 随步数漂移的增量实现，不划算。基准在 bench_test.go，可自行重跑。
//
// # 两套口径
//
// 同一个指标名，TradingView 与国内行情软件算出来的数不一样。拿本库的结果去
// 和某个软件对数之前，先确认口径。
//
// **默认是 CN，因为 OKX 自己的行情界面用的就是这一套**——2026-09-01 拿
// ETH-USDT-SWAP 的日线在 OKX 平台上逐行比对确认，MACD 与 KDJ 两个都对上 CN、
// 对不上 TV。这是给 OKX 用的库，默认就该跟 OKX 一致。
//
//	indicator.MACD(12, 26, 9)                  // CN 口径（默认，与 OKX 一致）
//	indicator.MACD(12, 26, 9, indicator.TV)    // TradingView 口径
//	indicator.MA(20, indicator.Named("fast"))  // 改视图里的键名
//
// 差异【只有三处】，其余两套完全一致。不为了对称而编造差异：
//
//  1. 递归平均的播种。TV 用前 n 个样本的简单平均播种，CN 用首个样本播种。
//     这一条就解释了 EMA、MACD、RSI 的全部分歧——它们底层是同一套递推。
//  2. MACD 柱。TV 是 DIF-DEA，CN 是 2×(DIF-DEA)。
//  3. KDJ 的平滑。TV（即 Stochastic）用简单移动平均且没有 J 线；
//     CN 用 SMA(n,1) 那种指数式平滑。
//
// 两套一致的：MA、BOLL（标准差都按【总体 n】，不是样本 n-1）、CCI、
// 以及 RSI 的 Wilder 平滑本身（只有播种不同）。
package indicator

import (
	"fmt"
	"math"
	"sync/atomic"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// Indicator 是一个流式技术指标，是 tickflow.Indicator 的别名。
//
// 接口本身定义在根包：Feed 要消费指标，而本包要用 tickflow.Candle，
// 定义放在任何一边都会形成循环引用。两边写哪个都一样。
//
// 外部实现只要满足这个接口，就能和内置指标一样挂进 Feed。
type Indicator = tickflow.Indicator

// Convention 是指标的计算口径。
type Convention int

const (
	// TV 是 TradingView 口径。
	TV Convention = iota

	// CN 是国内行情软件（通达信一系）的口径，也是本库的默认。
	//
	// 默认选它不是因为偏好，而是实测出来的：OKX 自己的行情界面用的就是这一套。
	// 2026-09-01 拿 ETH-USDT-SWAP 的日线逐行比对，MACD 的柱与 KDJ 的 K/D/J
	// 都对上 CN、对不上 TV。见 okx_convention_test.go 里锁住的那组基准值。
	CN
)

func (c Convention) String() string {
	if c == CN {
		return "CN"
	}
	return "TV"
}

// Option 配置指标的构造。Convention 本身就是一个 Option，可以直接传：
//
//	indicator.RSI(14, indicator.CN)
type Option interface{ apply(*base) }

func (c Convention) apply(b *base) { b.conv = c }

type nameOption string

func (n nameOption) apply(b *base) { b.name = string(n) }

// Named 改写指标在视图里的键名。
//
// 同一个 Feed 里挂两个同名指标会在构造时报错，这时用它区分开：
//
//	indicator.MACD(12, 26, 9, indicator.Named("macd_fast"))
func Named(s string) Option { return nameOption(s) }

// defaultConvention 是全局默认口径。
//
// 用原子变量而不是普通的包级变量：SetDefaultConvention 与指标构造会并发发生，
// 普通变量在那里就是数据竞争。这不是假想——竞态检测第一次跑起来就把它抓了出来。
// 文档写着「只在程序初始化时调用一次」，但那是【约定】，约定挡不住并发。
// concurrent_test.go 里有测试盯着这一条。
var defaultConvention atomic.Int32

func init() { defaultConvention.Store(int32(CN)) }

// SetDefaultConvention 设置全局默认口径。
//
// 建议只在【程序初始化时】调用一次：口径是在指标【构造时】读取的，跑到一半再改
// 不会影响已经构造出来的指标，只会让后面新建的那些用上新口径——那种半新半旧的
// 状态很难查。默认已经是 CN（与 OKX 一致），想全局改用 TradingView 口径就在 main
// 开头设一次，省得每个指标都写一遍。
//
// 并发调用是安全的（原子写），但「安全」不等于「结果可预测」，见上。
func SetDefaultConvention(c Convention) { defaultConvention.Store(int32(c)) }

// DefaultConvention 返回当前的全局默认口径。
func DefaultConvention() Convention { return Convention(defaultConvention.Load()) }

type base struct {
	name string
	conv Convention
}

func newBase(name string, opts []Option) base {
	b := base{name: name, conv: DefaultConvention()}
	for _, o := range opts {
		o.apply(&b)
	}
	return b
}

func (b base) Name() string { return b.name }

// Convention 返回该指标实际使用的口径。
func (b base) Convention() Convention { return b.conv }

// mustPositive 校验参数。参数是写死在代码里的常量，写错属于编程错误，
// 当场 panic 比让它悄悄算出一堆 NaN 强。
func mustPositive(what string, n int) {
	if n <= 0 {
		panic(fmt.Sprintf("indicator: %s 须为正整数，实为 %d", what, n))
	}
}

// Keys 返回指标在视图里的全部键名。
//
// 单输出为 Name()，多输出为 Name()+"."+字段名，如 "macd.dif"。
func Keys(ind Indicator) []string { return tickflow.IndicatorKeys(ind) }

// Compute 把一批 K 线依次喂给指标，返回每根对应的值（各自独立，可安全持有）。
//
// 供测试与「一次性算完整段历史」使用。回测请直接用 Update，不要在每一步
// 重算整段。调用前会先 Reset。
func Compute(ind Indicator, cs []tickflow.Candle) [][]float64 {
	ind.Reset()
	out := make([][]float64, len(cs))
	for i, c := range cs {
		out[i] = append([]float64(nil), ind.Update(c)...)
	}
	return out
}

// ComputeField 是 Compute 的单字段版本，返回某一路输出的序列。
//
// field 为空时取第一路（单输出指标就用它）。
func ComputeField(ind Indicator, cs []tickflow.Candle, field string) ([]float64, error) {
	idx := 0
	if field != "" {
		idx = -1
		for i, f := range ind.Fields() {
			if f == field {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("indicator: %s 没有字段 %q，可用的是 %v",
				ind.Name(), field, ind.Fields())
		}
	}
	rows := Compute(ind, cs)
	out := make([]float64, len(rows))
	for i, r := range rows {
		if idx >= len(r) {
			out[i] = math.NaN()
			continue
		}
		out[i] = r[idx]
	}
	return out, nil
}
