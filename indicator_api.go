package tickflow

// Indicator 是一个流式技术指标。
//
// 接口定义在根包，而实现（MA / EMA / MACD / KDJ / RSI / CCI / BOLL）在子包
// indicator 里。这么分是因为 Feed 要消费指标，而 indicator 包要用 Candle——
// 定义放在任何一边都会形成循环引用。indicator.Indicator 是本类型的别名，
// 两边写哪个都一样。
//
// 外部实现只要满足这个接口，就能和内置指标一样挂进 Feed。
type Indicator interface {
	// Name 是指标在视图里的键名，如 "ma20"、"macd"。
	Name() string

	// Fields 是多输出指标的字段名，如 MACD 的 ["dif","dea","hist"]。
	// 单输出指标返回 nil。
	Fields() []string

	// Warmup 是【出第一个有效值】所需的 K 线根数。
	//
	// 含义是「值从此有定义」，不是「值已收敛」。EMA 一族即便有了定义，
	// 也还要再走若干个周期才稳定下来。
	Warmup() int

	// Update 喂入一根【已完结】的 K 线，返回本根对应的指标值。
	//
	// 尚未 warmup 完时返回 NaN——不是 nil，也不是 0。返回的切片由指标复用，
	// 调用方不得把它留到下一次 Update 之后。
	//
	// 返回的长度必须恒等于 IndicatorKeys 的长度，也就是
	// max(1, len(Fields()))。Feed 会在第一次推进时校验这一条。
	Update(c Candle) []float64

	// Reset 清空全部内部状态，回到刚构造出来的样子。
	Reset()
}

// IndicatorKeys 返回指标在视图里的全部键名。
//
// 单输出为 Name()，多输出为 Name()+"."+字段名，如 "macd.dif"。
func IndicatorKeys(ind Indicator) []string {
	f := ind.Fields()
	if len(f) == 0 {
		return []string{ind.Name()}
	}
	out := make([]string, len(f))
	for i, k := range f {
		out[i] = ind.Name() + "." + k
	}
	return out
}
