package indicator

import (
	"strconv"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// BOLL 是布林带，输出 mid / up / dn 三路，默认键名 "boll"+周期。
// 经典参数是 BOLL(20, 2)。
//
//	mid = MA(收, n)
//	sd  = 收在窗口内的【总体】标准差（除以 n）
//	up  = mid + k×sd，dn = mid − k×sd
//
// 标准差用总体（除以 n）而不是样本（除以 n−1），两套口径都如此。
// 这一处曾是本库唯一没定下来的参数：TradingView 的 ta.stdev 是总体，
// 通达信的 STD 疑为样本但未经核实。既然差别只是把带宽整体缩放
// √(n/(n−1))（20 周期上约 2.6%），就统一取总体，不留一个会随口径漂移的量。
//
// k 是倍数，通常取 2，允许非整数。
func BOLL(n int, k float64, opts ...Option) Indicator {
	mustPositive("BOLL 的周期", n)
	if k <= 0 {
		panic("indicator: BOLL 的倍数须为正数")
	}
	return &bollIndicator{
		base: newBase("boll"+strconv.Itoa(n), opts),
		n:    n,
		k:    k,
		w:    newWindow(n),
		out:  make([]float64, 3),
	}
}

type bollIndicator struct {
	base
	n   int
	k   float64
	w   *window
	out []float64
}

func (b *bollIndicator) Fields() []string { return []string{"mid", "up", "dn"} }
func (b *bollIndicator) Warmup() int      { return b.n }
func (b *bollIndicator) Reset()           { b.w.reset() }

func (b *bollIndicator) Update(c tickflow.Candle) []float64 {
	b.w.push(c.Close)
	if !b.w.full() {
		return fillNaN(b.out)
	}
	mid, sd := b.w.mean(), b.w.stdPop()
	b.out[0], b.out[1], b.out[2] = mid, mid+b.k*sd, mid-b.k*sd
	return b.out
}
