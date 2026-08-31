package indicator

import (
	"strconv"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// CCI 是顺势指标，默认键名 "cci"+周期。经典参数是 CCI(14) 或 CCI(20)。
//
//	TP  = (高 + 低 + 收) / 3
//	CCI = (TP − MA(TP, n)) / (0.015 × 平均绝对偏差)
//
// 分母是【平均绝对偏差】（各点到均值距离的平均），不是标准差——这是 CCI
// 与布林带最容易搞混的一处。0.015 是 Lambert 定的常数，作用是让 CCI 大致
// 落在 ±100 之内，没有更深的来由。
//
// 两套口径完全一致。
//
// 窗口内 TP 恒定时平均绝对偏差为 0，此时返回 0（没有偏离就是没有偏离），
// 而不是让除零产生 ±Inf 传染下去。
func CCI(n int, opts ...Option) Indicator {
	mustPositive("CCI 的周期", n)
	return &cciIndicator{
		base: newBase("cci"+strconv.Itoa(n), opts),
		n:    n,
		w:    newWindow(n),
		out:  make([]float64, 1),
	}
}

type cciIndicator struct {
	base
	n   int
	w   *window
	out []float64
}

func (i *cciIndicator) Fields() []string { return nil }
func (i *cciIndicator) Warmup() int      { return i.n }
func (i *cciIndicator) Reset()           { i.w.reset() }

func (i *cciIndicator) Update(c tickflow.Candle) []float64 {
	tp := (c.High + c.Low + c.Close) / 3
	i.w.push(tp)
	if !i.w.full() {
		return fillNaN(i.out)
	}
	md := i.w.meanAbsDev()
	if md == 0 {
		i.out[0] = 0
		return i.out
	}
	i.out[0] = (tp - i.w.mean()) / (0.015 * md)
	return i.out
}
