package indicator

import (
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// MACD 输出 dif / dea / hist 三路。默认键名 "macd"，视图里是
// "macd.dif" / "macd.dea" / "macd.hist"。经典参数是 MACD(12, 26, 9)。
//
//	DIF  = EMA(fast) − EMA(slow)
//	DEA  = EMA(signal) of DIF
//	hist = DIF − DEA        （TV）
//	hist = 2 × (DIF − DEA)  （CN）
//
// 两套口径差在两处：柱子要不要乘 2，以及底下三个 EMA 怎么播种。
// 乘不乘 2 只影响柱子的刻度、不影响它的正负与形状——但如果策略里写了
// 「柱子大于某个绝对值」这类阈值，换口径就会失效。
//
// 默认键名不带参数（就叫 "macd"），是因为同一个 Feed 里挂两套不同参数的
// MACD 极少见。真要挂两套，用 Named 区分——不然 Feed 构造时会因重名报错。
func MACD(fast, slow, signal int, opts ...Option) Indicator {
	mustPositive("MACD 的快线周期", fast)
	mustPositive("MACD 的慢线周期", slow)
	mustPositive("MACD 的信号线周期", signal)
	if fast >= slow {
		panic("indicator: MACD 的快线周期须小于慢线周期")
	}
	b := newBase("macd", opts)
	mul := 1.0
	if b.conv == CN {
		mul = 2.0
	}
	return &macdIndicator{
		base:    b,
		fast:    fast,
		slow:    slow,
		signal:  signal,
		emaFast: newSmoother(emaAlpha(fast), fast, b.conv),
		emaSlow: newSmoother(emaAlpha(slow), slow, b.conv),
		dea:     newSmoother(emaAlpha(signal), signal, b.conv),
		histMul: mul,
		out:     make([]float64, 3),
	}
}

type macdIndicator struct {
	base
	fast, slow, signal int
	emaFast, emaSlow   *smoother
	dea                *smoother
	histMul            float64
	out                []float64
}

func (m *macdIndicator) Fields() []string { return []string{"dif", "dea", "hist"} }

func (m *macdIndicator) Reset() {
	m.emaFast.reset()
	m.emaSlow.reset()
	m.dea.reset()
}

func (m *macdIndicator) Warmup() int {
	if m.conv == CN {
		return 1
	}
	// 慢线 EMA 在第 slow 根才有值，DIF 随之才有值；DEA 又要拿 signal 个 DIF
	// 做简单平均来播种，于是再往后 signal-1 根。
	return m.slow + m.signal - 1
}

func (m *macdIndicator) Update(c tickflow.Candle) []float64 {
	fast, okF := m.emaFast.update(c.Close)
	slow, okS := m.emaSlow.update(c.Close)
	if !okF || !okS {
		return fillNaN(m.out)
	}
	dif := fast - slow
	dea, okD := m.dea.update(dif)
	if !okD {
		// DIF 已经有值，但 DEA 还在播种。三路一起给 NaN——只给出 dif、
		// 另外两路留 NaN 会让「指标是否就绪」有两种答案，调用方迟早踩到。
		return fillNaN(m.out)
	}
	m.out[0], m.out[1], m.out[2] = dif, dea, (dif-dea)*m.histMul
	return m.out
}
