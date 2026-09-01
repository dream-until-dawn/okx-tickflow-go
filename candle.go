package tickflow

import (
	"fmt"
	"sync"
	"time"
)

// Candle 是一根【已完结】的 K 线。
//
// OHLCV 用 float64 而非 decimal：指标计算是本库的热路径，decimal 会慢一到两个
// 数量级，且没有任何精度收益——OKX 的价格位数远在 float64 的 15 位有效数字之内。
// 与 okx-position-simulator-go 的 decimal 世界的转换收在 adapter/simbar 一处。
//
// 未完结的 K 线（上游 Confirm == false）不会进入本库的任何一层，在 Source 那一层
// 就已丢弃。用一根还在变的 K 线的「收盘价」做决策，等于偷看了这根 K 线走完之后
// 才知道的信息，这是回测里最经典的未来函数。
type Candle struct {
	Ts          int64 // 开盘时间，毫秒
	Open        float64
	High        float64
	Low         float64
	Close       float64
	Vol         float64 // 成交量（张 / 基础货币）
	VolCcy      float64 // 成交量（基础货币）
	VolCcyQuote float64 // 成交额（计价货币）
}

const (
	msSecond = int64(time.Second / time.Millisecond)
	msMinute = 60 * msSecond
	msHour   = 60 * msMinute
	msDay    = 24 * msHour

	// hkOffset 是香港时间相对 UTC 的偏移。
	//
	// 用固定偏移而不是 time.LoadLocation("Asia/Hong_Kong")：香港自 1979 年起
	// 不再实行夏令时，偏移恒为 +8；而 Windows 上没有 zoneinfo 目录，LoadLocation
	// 会失败，除非额外 import time/tzdata 把 450KB 的时区库嵌进二进制。
	// 为一个恒定值付这个代价不划算。
	hkOffset = 8 * msHour

	// mondayUTC 是 1970 年 1 月 1 日之前最近的一个周一零点（UTC）。
	//
	// epoch 那天是【周四】，所以周线不能直接按 7 天整除对齐，要以这个点为基准。
	mondayUTC = -3 * msDay
)

// Period 是一个 K 线周期。
//
// 本库【不硬编码一张 bar 列表】——OKX 会加新周期，写死就得跟着改。bar 字符串
// 原样透传给 OKX，本库只需要知道它的对齐网格，用于收盘判定与区间对齐。
// 内置表覆盖了当前已知的周期，未知周期用 RegisterFixedPeriod 补。
type Period struct {
	bar string

	// step 是定长周期的步长（毫秒）；日历周期为 0。
	step int64
	// anchor 是对齐网格的基准点（毫秒 epoch）。UTC 对齐的周期为 0，
	// 港时对齐的为 -hkOffset，周线另有周一偏移。
	anchor int64

	// months 是日历周期的月数（1M 为 1，3M 为 3）；定长周期为 0。
	// 月不定长，一个 Duration 表达不了它的收盘时刻，只能走日历。
	months int
	loc    *time.Location
}

// Bar 返回原始的周期字符串。
func (p Period) Bar() string { return p.bar }

// IsZero 报告 Period 是否为零值（未初始化）。
func (p Period) IsZero() bool { return p.step == 0 && p.months == 0 }

// Fixed 报告本周期是否定长。1M / 3M 这类日历周期返回 false。
func (p Period) Fixed() bool { return p.step != 0 }

// Step 返回定长周期的步长；日历周期返回 0。
func (p Period) Step() time.Duration { return time.Duration(p.step) * time.Millisecond }

// Truncate 把任意时刻对齐到它所属那根 K 线的开盘时间。
//
// 零值 Period 会 panic：那一定是编程错误——Period 只能由 ParsePeriod 构造，
// 拿一个零值去对齐时间，得到的任何结果都是错的。与其让它从 time 包深处炸出
// 一句「missing Location」，不如在这里说清楚。
func (p Period) Truncate(ts int64) int64 {
	if p.IsZero() {
		panic("tickflow: 用了一个零值 Period；Period 须由 ParsePeriod / MustParsePeriod 构造")
	}
	if p.step != 0 {
		return p.anchor + floorDiv(ts-p.anchor, p.step)*p.step
	}
	t := time.UnixMilli(ts).In(p.loc)
	m := int(t.Month()) - 1
	m -= m % p.months
	return time.Date(t.Year(), time.Month(m+1), 1, 0, 0, 0, 0, p.loc).UnixMilli()
}

// Next 给定一根 K 线的开盘时间，返回下一根的开盘时间——也就是本根的【收盘时刻】。
//
// 统一成 Next 而不是暴露一个步长，是因为月线不定长：1M 的收盘时刻只能按日历算。
// 传入的 ts 不必已对齐，内部会先 Truncate。
func (p Period) Next(ts int64) int64 {
	base := p.Truncate(ts)
	if p.step != 0 {
		return base + p.step
	}
	return time.UnixMilli(base).In(p.loc).AddDate(0, p.months, 0).UnixMilli()
}

// Closed 报告开盘于 ts 的那根 K 线在 now 时刻是否已经收盘。
func (p Period) Closed(ts, now int64) bool { return p.Next(ts) <= now }

// LastClosed 返回 now 时刻【最后一根已收盘】K 线的开盘时间。
//
// Truncate(now) 是当前这根【还在走】的 K 线，它的收盘价尚不可知，
// 所以最后一根已收盘的是它的前一根。
func (p Period) LastClosed(now int64) int64 {
	cur := p.Truncate(now)
	return p.Truncate(cur - 1)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

var (
	periodMu sync.RWMutex
	periods  = map[string]Period{}

	utcLoc = time.UTC
	hkLoc  = time.FixedZone("HKT", 8*3600)
)

func init() {
	// 1s ~ 4H 全都整除 24 小时，因此「按 epoch 对齐」与「按自然日对齐」等价，
	// 不存在时区问题，OKX 也没有为它们提供 utc 变体。
	for bar, step := range map[string]int64{
		"1s":  msSecond,
		"1m":  msMinute,
		"3m":  3 * msMinute,
		"5m":  5 * msMinute,
		"15m": 15 * msMinute,
		"30m": 30 * msMinute,
		"1H":  msHour,
		"2H":  2 * msHour,
		"4H":  4 * msHour,
	} {
		periods[bar] = Period{bar: bar, step: step, anchor: 0}
	}

	// 6H 及以上，OKX 默认按【香港时间】开盘对齐，utc 后缀的变体才按 UTC。
	// 两者是两条【不同的序列】，不可混存、不可互相顶替。
	//
	// 2D / 3D 的锚点（哪两天、哪三天归为一根）OKX 未文档化，这里按「自 epoch
	// 起按港时自然日计数」推定。2026-08-31 用 BTC-USDT-SWAP 的真实数据实测，
	// 全部 17 个周期的开盘时间都落在本表算出的网格上（见 source/okxsource 的
	// TestLiveAlignment）。OKX 若改了对齐方式，那个测试会先炸。
	for bar, step := range map[string]int64{
		"6H":  6 * msHour,
		"12H": 12 * msHour,
		"1D":  msDay,
		"2D":  2 * msDay,
		"3D":  3 * msDay,
	} {
		periods[bar] = Period{bar: bar, step: step, anchor: -hkOffset}
		periods[bar+"utc"] = Period{bar: bar + "utc", step: step, anchor: 0}
	}

	// 周线以周一零点为界。epoch 那天是周四，所以要额外挪 mondayUTC。
	periods["1W"] = Period{bar: "1W", step: 7 * msDay, anchor: mondayUTC - hkOffset}
	periods["1Wutc"] = Period{bar: "1Wutc", step: 7 * msDay, anchor: mondayUTC}

	for bar, months := range map[string]int{"1M": 1, "3M": 3} {
		periods[bar] = Period{bar: bar, months: months, loc: hkLoc}
		periods[bar+"utc"] = Period{bar: bar + "utc", months: months, loc: utcLoc}
	}
}

// ParsePeriod 解析周期字符串。未知周期返回错误，提示用 RegisterFixedPeriod 补。
func ParsePeriod(bar string) (Period, error) {
	periodMu.RLock()
	p, ok := periods[bar]
	periodMu.RUnlock()
	if !ok {
		return Period{}, fmt.Errorf("tickflow: 未知周期 %q；若这是 OKX 新增的周期，"+
			"用 RegisterFixedPeriod 注册它的步长与锚点即可", bar)
	}
	return p, nil
}

// MustParsePeriod 同 ParsePeriod，解析失败时 panic。仅用于常量周期。
func MustParsePeriod(bar string) Period {
	p, err := ParsePeriod(bar)
	if err != nil {
		panic(err)
	}
	return p
}

// RegisterFixedPeriod 注册（或覆盖）一个定长周期。
//
// step 是步长，anchor 是对齐网格上的【任意一个】边界点——比如 1D 按港时对齐，
// 就传任意一天的香港零点。内置周期也可以被覆盖，用于修正实测发现的锚点偏差。
func RegisterFixedPeriod(bar string, step time.Duration, anchor time.Time) error {
	if bar == "" {
		return fmt.Errorf("tickflow: 周期名不能为空")
	}
	ms := step.Milliseconds()
	if ms <= 0 {
		return fmt.Errorf("tickflow: 周期 %q 的步长须为正，实为 %s", bar, step)
	}
	periodMu.Lock()
	defer periodMu.Unlock()
	periods[bar] = Period{bar: bar, step: ms, anchor: anchor.UnixMilli() % ms}
	return nil
}

// VerifyAlignment 检查一组 K 线的开盘时间是否都落在 p 的对齐网格上。
//
// 内置周期表里 2D / 3D 的锚点是从 OKX 未文档化的行为里推定出来的，虽已实测
// 对上，但交易所改起来不必通知谁。拿到新数据时对一遍，比信一个常量踏实。
// 返回第一个不对齐的时间戳。
func VerifyAlignment(p Period, cs []Candle) (badTs int64, ok bool) {
	for _, c := range cs {
		if p.Truncate(c.Ts) != c.Ts {
			return c.Ts, false
		}
	}
	return 0, true
}
