package indicator

import (
	"strconv"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// MA 是收盘价的简单移动平均。默认键名 "ma"+周期，如 MA(20) 是 "ma20"。
//
// 两套口径下算法完全一致。
func MA(n int, opts ...Option) Indicator {
	mustPositive("MA 的周期", n)
	return &maIndicator{
		base: newBase("ma"+strconv.Itoa(n), opts),
		n:    n,
		w:    newWindow(n),
		out:  make([]float64, 1),
	}
}

type maIndicator struct {
	base
	n   int
	w   *window
	out []float64
}

func (m *maIndicator) Fields() []string { return nil }
func (m *maIndicator) Warmup() int      { return m.n }
func (m *maIndicator) Reset()           { m.w.reset() }

func (m *maIndicator) Update(c tickflow.Candle) []float64 {
	m.w.push(c.Close)
	if !m.w.full() {
		return fillNaN(m.out)
	}
	m.out[0] = m.w.mean()
	return m.out
}

// EMA 是收盘价的指数移动平均，α = 2/(n+1)。默认键名 "ema"+周期。
//
// 两套口径的差别只在【怎么起头】：TV 用前 n 根收盘价的简单平均播种，
// 因此第 n 根才有值；CN 直接拿第一根收盘价播种，从第一根就有值。
//
// 「有值」不等于「稳了」。首值播种的 EMA 在开头几十根里被初始值拖着，
// 要走上若干个周期才和播种方式无关。拿本库的 EMA 去和某个软件对数时，
// 请从足够靠后的位置比。
func EMA(n int, opts ...Option) Indicator {
	mustPositive("EMA 的周期", n)
	b := newBase("ema"+strconv.Itoa(n), opts)
	return &emaIndicator{
		base: b,
		n:    n,
		s:    newSmoother(emaAlpha(n), n, b.conv),
		out:  make([]float64, 1),
	}
}

type emaIndicator struct {
	base
	n   int
	s   *smoother
	out []float64
}

func (e *emaIndicator) Fields() []string { return nil }
func (e *emaIndicator) Reset()           { e.s.reset() }

func (e *emaIndicator) Warmup() int {
	if e.conv == CN {
		return 1
	}
	return e.n
}

func (e *emaIndicator) Update(c tickflow.Candle) []float64 {
	v, ok := e.s.update(c.Close)
	if !ok {
		return fillNaN(e.out)
	}
	e.out[0] = v
	return e.out
}
