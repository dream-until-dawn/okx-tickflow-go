package indicator

import (
	"strconv"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// RSI 是相对强弱指数，默认键名 "rsi"+周期。
//
//	涨幅 = max(收 − 前收, 0)，跌幅 = max(前收 − 收, 0)
//	均涨、均跌各走一路 Wilder 平滑（α = 1/n）
//	RSI = 100 − 100 / (1 + 均涨/均跌)
//
// 两套口径【用的是同一个平滑公式】——通达信的 SMA(X,N,1) 就是 Wilder 平滑，
// 只是播种不同：TV 用前 n 个涨跌幅的简单平均，CN 用第一个涨跌幅。
//
// 三个边界的取值（各家实现不尽相同，这里明确下来）：
//
//   - 均跌为 0、均涨为正：返回 100
//   - 均涨为 0、均跌为正：返回 0（由公式自然得出）
//   - 两者都为 0（窗口内价格纹丝不动）：返回 50，而不是 NaN。
//     不动就是不强不弱，50 比让 NaN 传染下去有用。
func RSI(n int, opts ...Option) Indicator {
	mustPositive("RSI 的周期", n)
	b := newBase("rsi"+strconv.Itoa(n), opts)
	return &rsiIndicator{
		base: b,
		n:    n,
		up:   newSmoother(wilderAlpha(n), n, b.conv),
		down: newSmoother(wilderAlpha(n), n, b.conv),
		out:  make([]float64, 1),
	}
}

type rsiIndicator struct {
	base
	n        int
	up, down *smoother
	prev     float64
	hasPrev  bool
	out      []float64
}

func (r *rsiIndicator) Fields() []string { return nil }

func (r *rsiIndicator) Reset() {
	r.up.reset()
	r.down.reset()
	r.prev, r.hasPrev = 0, false
}

func (r *rsiIndicator) Warmup() int {
	// 第一根没有「前收」，涨跌幅从第二根才开始有，所以都要 +1。
	if r.conv == CN {
		return 2
	}
	return r.n + 1
}

// Settle 见 tickflow.Settler。均涨与均跌用同一个 α，取其一即可；
// 加 1 是因为第一根没有「前收」，涨跌幅从第二根才开始有。
func (r *rsiIndicator) Settle() int { return r.up.settle() + 1 }

func (r *rsiIndicator) Update(c tickflow.Candle) []float64 {
	if !r.hasPrev {
		r.prev, r.hasPrev = c.Close, true
		return fillNaN(r.out)
	}
	change := c.Close - r.prev
	r.prev = c.Close

	gain, loss := 0.0, 0.0
	if change > 0 {
		gain = change
	} else {
		loss = -change
	}

	avgUp, ok1 := r.up.update(gain)
	avgDown, ok2 := r.down.update(loss)
	if !ok1 || !ok2 {
		return fillNaN(r.out)
	}

	switch {
	case avgDown == 0 && avgUp == 0:
		r.out[0] = 50
	case avgDown == 0:
		r.out[0] = 100
	default:
		r.out[0] = 100 - 100/(1+avgUp/avgDown)
	}
	return r.out
}
