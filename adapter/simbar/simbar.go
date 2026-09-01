// Package simbar 把 okx-tickflow-go 的 K 线转成 okx-position-simulator-go
// 能吃的 Bar，是行情层与记账层之间唯一的那道缝。
//
// # 为什么是独立模块
//
// 本包需要 shopspring/decimal 与整个记账内核，而主模块承诺「依赖树只有
// okx-api-v5-go 一个」。做成嵌套模块之后，只想拉行情、不碰仓位核算的使用者
// 不会被牵连进来。
//
// # 为什么包名不叫 okxsim
//
// 记账内核自己的包名就是 okxsim。若本包也叫这个名字，每个同时用到两边的
// 使用者都得给其中一个起别名。叫 simbar 反而更直白——它产出的正是 Bar。
//
//	import (
//	    okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
//	    "github.com/dream-until-dawn/okx-tickflow-go/adapter/simbar"
//	)
//
// # float64 → decimal 是无损的
//
// 行情层用 float64（指标计算是热路径），记账层用 decimal（钱的事不能有误差）。
// 转换收在本包一处，走 decimal.NewFromFloat——它取的是能往返回原值的最短十进制
// 表示。OKX 的价格有效数字远在 float64 的 15 位之内，因此转换后的 decimal
// 与原始报价逐位相同，再转回 float64 也是同一个数。这一条有测试盯着，
// 既用真实行情也用生成的极端刻度，见 simbar_test.go。
//
// # 不填资金费，这是刻意的
//
// ToBar 永远不设 Bar.Funding，本包也不提供设置它的选项。
//
// OKX 的历史资金费率只保留约 3 个月。部分区间有、更早的区间没有，比全都没有
// 更糟：它会让同一个策略在不同时间段的回测收益【不可比】，而这种不可比是隐性的，
// 很容易被误读成「策略在不同市况下的表现差异」。全都不计至少是一致的偏差——
// 系统性高估多头持仓收益、低估空头，方向已知，解读结果时可以统一扣减。
//
// 自备了资金费数据的使用者可以在拿到 Bar 之后自行赋值，那是明确的一步动作，
// 而不是本库替谁做的默认。
package simbar

import (
	"errors"
	"fmt"
	"math"

	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
	"github.com/shopspring/decimal"
)

// Option 配置一次转换。
type Option func(*okxsim.Bar)

// WithMarkPx 设置标记价。
//
// 不设时留空，记账内核会用最新价顶替（它的 markPx() 就是这么做的）。**那是个
// 会静默产生假阴性的退化**：强平判据本该看标记价，用最新成交价会让影线扫掉
// 本不该爆的仓位。能拿到真的就该给。
//
// 拿法：tickflow 的 Feed 配上 MarkStore 之后，v.MarkPx() 就是这一根的标记价。
//
//	bar, _ := simbar.ToBar(inst, v.Candle(), simbar.WithMarkPx(v.MarkPx()))
//
// px 为 NaN（标记价在这个时刻缺根）时【等同于不设】——Dec 把 NaN 转成零值，
// 而记账内核只认正数的 MarkPx。
//
// 从 okx-position-simulator-go v1.0.0 起，**缺标记价是默认报错的**（字段反转成了
// Config.AllowMarkPxFallback，打开它才退回用最新成交价顶替）。所以不给标记价
// 不会静默出错，会当场炸——这是对的。
func WithMarkPx(px float64) Option {
	return func(b *okxsim.Bar) { b.MarkPx = Dec(px) }
}

// WithIdxPx 设置指数价。
//
// 只有 triggerPxType 为 index 的算法委托用得上。不设时那类委托会被记账内核
// 跳过并在 StepResult 里说明原因——它【不拿别的价格顶替】，那是对的。
func WithIdxPx(px float64) Option {
	return func(b *okxsim.Bar) { b.IdxPx = Dec(px) }
}

// Dec 把行情层的 float64 转成记账层的 decimal，是本包唯一的数值转换点。
//
// NaN 与 Inf 转成零值而不是 panic——shopspring 的 NewFromFloat 碰上它们会
// panic，而一个回测循环里最不该出现的就是从库深处炸出来的 panic。真正该拦住
// 非法价格的地方是 ToBar 的校验。
func Dec(f float64) decimal.Decimal {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return decimal.Zero
	}
	return decimal.NewFromFloat(f)
}

// ToBar 把一根【已完结】的 K 线转成 okxsim.Bar。
//
//	High / Low  直取
//	Last        取收盘价——记账内核用它撮合限价单、算最新价
//	Ts          直取
//	MarkPx      留空，除非用 WithMarkPx 给出
//	IdxPx       留空，除非用 WithIdxPx 给出
//	Funding     永远为 nil，见包注释
//
// 价格非有限值或非正数时返回错误，而不是把一个 NaN 或负价喂进记账内核——
// 那会在离出错点很远的地方炸出一个看不懂的结果。
func ToBar(instID string, c tickflow.Candle, opts ...Option) (okxsim.Bar, error) {
	if instID == "" {
		return okxsim.Bar{}, errors.New("simbar: instId 不能为空")
	}
	if c.Ts <= 0 {
		return okxsim.Bar{}, fmt.Errorf("simbar: %s 的 ts 须为正的毫秒时间戳，实为 %d", instID, c.Ts)
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"high", c.High}, {"low", c.Low}, {"close", c.Close}} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) {
			return okxsim.Bar{}, fmt.Errorf("simbar: %s 在 %d 的 %s 是 %v；"+
				"NaN 多半来自一个无效的 View，先判 View.Valid()", instID, c.Ts, f.name, f.v)
		}
		if f.v <= 0 {
			return okxsim.Bar{}, fmt.Errorf("simbar: %s 在 %d 的 %s 须为正数，实为 %v",
				instID, c.Ts, f.name, f.v)
		}
	}
	if c.High < c.Low {
		return okxsim.Bar{}, fmt.Errorf("simbar: %s 在 %d 的最高价 %v 低于最低价 %v",
			instID, c.Ts, c.High, c.Low)
	}

	b := okxsim.Bar{
		InstID: instID,
		High:   Dec(c.High),
		Low:    Dec(c.Low),
		Last:   Dec(c.Close),
		Ts:     c.Ts,
	}
	for _, o := range opts {
		o(&b)
	}
	return b, nil
}

// ToBars 批量转换。任意一根出错就整批返回错误并指明是第几根——
// 半批数据喂进记账内核之后再报错，状态就没法收拾了。
func ToBars(instID string, cs []tickflow.Candle, opts ...Option) ([]okxsim.Bar, error) {
	out := make([]okxsim.Bar, 0, len(cs))
	for i, c := range cs {
		b, err := ToBar(instID, c, opts...)
		if err != nil {
			return nil, fmt.Errorf("simbar: 第 %d 根: %w", i, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// Advance 把视图【当前这一根】转成 Bar 并推进记账内核一步。
//
// 这是回测循环里最常见的一行：
//
//	for f.Next() {
//	    v := f.View()
//	    if !v.Ready() { continue }
//	    step, err := simbar.Advance(sim, "BTC-USDT-SWAP", v)
//	    ...
//	}
//
// **视图带着标记价时会自动带上**（Feed 配了 FeedConfig.MarkStore 的话）。
// 标记价 K 线与这一根同时收盘，本就是该用的那个值，不必让每个调用方都写一遍
// WithMarkPx(v.MarkPx())。显式传的 WithMarkPx 会覆盖它。
//
// 视图没有标记价时不填——从 okx-position-simulator-go v1.0.0 起那会让 Advance
// 直接报错（除非对方打开了 Config.AllowMarkPxFallback）。那是对的：强平判据用
// 最新成交价会让影线扫掉本不该爆的仓位，而且是假阴性。
//
// 视图无效时返回错误而不是喂一根 NaN 进去。
func Advance(sim *okxsim.Simulator, instID string, v tickflow.View, opts ...Option) (okxsim.StepResult, error) {
	if sim == nil {
		return okxsim.StepResult{}, errors.New("simbar: simulator 不能为 nil")
	}
	if !v.Valid() {
		return okxsim.StepResult{}, fmt.Errorf("simbar: %s 的视图无效——"+
			"Prev(n) 超出了 Lookback，或者还没走到第 n 根", instID)
	}
	// 视图知道自己这一根的标记价，自动带上；显式的 WithMarkPx 排在后面会覆盖它。
	if px := v.MarkPx(); !math.IsNaN(px) {
		opts = append([]Option{WithMarkPx(px)}, opts...)
	}
	b, err := ToBar(instID, v.Candle(), opts...)
	if err != nil {
		return okxsim.StepResult{}, err
	}
	return sim.Advance(b)
}
