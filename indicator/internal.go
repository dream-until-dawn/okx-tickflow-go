package indicator

import "math"

// smoother 是「递归平均」的统一实现：y ← y + α(x − y)。
//
// EMA 与 Wilder 平滑是同一套递推，只是 α 不同——EMA 取 2/(n+1)，Wilder 取
// 1/n。而两套口径的分歧【全部】落在播种上：TradingView 用前 n 个样本的简单
// 平均起头，国内软件用首个样本起头。
//
// 把这两件事收在一个类型里，是因为它们本就是一回事。EMA、MACD、RSI 三个指标
// 在 TV 与 CN 下的差别，追到底都是这里的 seedN 不同——分散到三处各写一遍，
// 就会显得像三条独立的规则，而它只有一条。
type smoother struct {
	alpha float64
	seedN int

	acc   float64
	cnt   int
	val   float64
	ready bool
}

// newSmoother 构造一个递归平均器。n 是指标周期，用于决定播种样本数。
func newSmoother(alpha float64, n int, conv Convention) *smoother {
	seed := 1
	if conv == TV {
		seed = n
	}
	return &smoother{alpha: alpha, seedN: seed}
}

// emaAlpha 是 EMA 的平滑系数。
func emaAlpha(n int) float64 { return 2 / (float64(n) + 1) }

// wilderAlpha 是 Wilder 平滑的系数，也就是通达信 SMA(X,N,1) 的系数。
func wilderAlpha(n int) float64 { return 1 / float64(n) }

// update 喂入一个样本。播种未完成时返回 (NaN, false)。
func (s *smoother) update(x float64) (float64, bool) {
	if !s.ready {
		s.acc += x
		s.cnt++
		if s.cnt < s.seedN {
			return math.NaN(), false
		}
		s.val = s.acc / float64(s.cnt)
		s.ready = true
		return s.val, true
	}
	// 写成 val += α(x-val) 而不是 α·x + (1-α)·val：数学上等价，
	// 但前者在 α 很小时误差更小，长序列上差别会累出来。
	s.val += s.alpha * (x - s.val)
	return s.val, true
}

func (s *smoother) reset() {
	s.acc, s.cnt, s.val, s.ready = 0, 0, 0, false
}

// window 是定长滑动窗口。
//
// 它只存值，统计量每次现算——见包注释里「与批量结果一致」那一段：
// 增量维护累加和会随步数累积浮点漂移，几百万根之后就和批量定义对不上了。
type window struct {
	buf  []float64
	i    int
	fill int
}

func newWindow(size int) *window { return &window{buf: make([]float64, size)} }

func (w *window) push(x float64) {
	w.buf[w.i] = x
	w.i = (w.i + 1) % len(w.buf)
	if w.fill < len(w.buf) {
		w.fill++
	}
}

func (w *window) full() bool { return w.fill == len(w.buf) }

func (w *window) size() int { return len(w.buf) }

// at 取窗口内第 k 新的值，k=0 是最新的一根。
func (w *window) at(k int) float64 {
	return w.buf[((w.i-1-k)%len(w.buf)+len(w.buf))%len(w.buf)]
}

func (w *window) mean() float64 {
	var sum float64
	for _, v := range w.buf {
		sum += v
	}
	return sum / float64(len(w.buf))
}

// stdPop 是【总体】标准差（除以 n），不是样本标准差（除以 n-1）。
//
// 布林带两套口径都用总体标准差。用样本标准差在 20 周期上会让带宽偏大约
// 2.6%——一眼看不出来，却足以让开平仓点位错开。
func (w *window) stdPop() float64 {
	m := w.mean()
	var sum float64
	for _, v := range w.buf {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(w.buf)))
}

// meanAbsDev 是平均绝对偏差，CCI 用它作分母。
func (w *window) meanAbsDev() float64 {
	m := w.mean()
	var sum float64
	for _, v := range w.buf {
		sum += math.Abs(v - m)
	}
	return sum / float64(len(w.buf))
}

func (w *window) max() float64 {
	m := math.Inf(-1)
	for _, v := range w.buf {
		if v > m {
			m = v
		}
	}
	return m
}

func (w *window) min() float64 {
	m := math.Inf(1)
	for _, v := range w.buf {
		if v < m {
			m = v
		}
	}
	return m
}

func (w *window) reset() {
	for i := range w.buf {
		w.buf[i] = 0
	}
	w.i, w.fill = 0, 0
}

func nan1() []float64 { return []float64{math.NaN()} }

func fillNaN(s []float64) []float64 {
	for i := range s {
		s[i] = math.NaN()
	}
	return s
}
