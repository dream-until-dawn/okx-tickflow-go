package indicator

import (
	"math"
	"testing"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// 本文件用【朴素的批量实现】重新算一遍每个指标，再和流式实现对。
//
// 这些参考实现故意写得又笨又直白：每个下标都把窗口整段拿出来重算，不复用任何
// 中间状态，也不碰 smoother / window 那套机器。它们和被测代码没有共享路径，
// 因此对得上就说明流式的下标记账、播种时机、窗口边界都没错——这正是流式指标
// 最容易出错、又最难从结果上看出来的地方。
//
// 包注释里说「增量 == 批量是结构上成立的」，指的是算法本身；这里是把那句话
// 真的验一遍。

func refMA(cs []tickflow.Candle, n int) []float64 {
	out := make([]float64, len(cs))
	for i := range cs {
		if i < n-1 {
			out[i] = math.NaN()
			continue
		}
		var sum float64
		for j := i - n + 1; j <= i; j++ {
			sum += cs[j].Close
		}
		out[i] = sum / float64(n)
	}
	return out
}

// refSmooth 是递归平均的参考实现：xs 里的 NaN 表示「这一根还没有输入」。
func refSmooth(xs []float64, alpha float64, seedN int) []float64 {
	out := make([]float64, len(xs))
	for i := range out {
		out[i] = math.NaN()
	}
	var seen []float64
	ready := false
	var val float64
	for i, x := range xs {
		if math.IsNaN(x) {
			continue
		}
		if !ready {
			seen = append(seen, x)
			if len(seen) < seedN {
				continue
			}
			var sum float64
			for _, v := range seen {
				sum += v
			}
			val = sum / float64(len(seen))
			ready = true
			out[i] = val
			continue
		}
		val += alpha * (x - val)
		out[i] = val
	}
	return out
}

func seedN(n int, conv Convention) int {
	if conv == CN {
		return 1
	}
	return n
}

func refEMA(cs []tickflow.Candle, n int, conv Convention) []float64 {
	xs := make([]float64, len(cs))
	for i, c := range cs {
		xs[i] = c.Close
	}
	return refSmooth(xs, 2/(float64(n)+1), seedN(n, conv))
}

func refMACD(cs []tickflow.Candle, fast, slow, signal int, conv Convention) (dif, dea, hist []float64) {
	ef := refEMA(cs, fast, conv)
	es := refEMA(cs, slow, conv)
	dif = make([]float64, len(cs))
	for i := range cs {
		if math.IsNaN(ef[i]) || math.IsNaN(es[i]) {
			dif[i] = math.NaN()
			continue
		}
		dif[i] = ef[i] - es[i]
	}
	dea = refSmooth(dif, 2/(float64(signal)+1), seedN(signal, conv))

	mul := 1.0
	if conv == CN {
		mul = 2
	}
	hist = make([]float64, len(cs))
	for i := range cs {
		if math.IsNaN(dea[i]) {
			hist[i] = math.NaN()
			dif[i] = math.NaN() // 三路同时就绪，与流式实现的约定一致
			continue
		}
		hist[i] = (dif[i] - dea[i]) * mul
	}
	return dif, dea, hist
}

func refRSI(cs []tickflow.Candle, n int, conv Convention) []float64 {
	gains := make([]float64, len(cs))
	losses := make([]float64, len(cs))
	for i := range cs {
		if i == 0 {
			gains[i], losses[i] = math.NaN(), math.NaN()
			continue
		}
		d := cs[i].Close - cs[i-1].Close
		gains[i], losses[i] = math.Max(d, 0), math.Max(-d, 0)
	}
	a := 1 / float64(n)
	up := refSmooth(gains, a, seedN(n, conv))
	down := refSmooth(losses, a, seedN(n, conv))

	out := make([]float64, len(cs))
	for i := range cs {
		switch {
		case math.IsNaN(up[i]) || math.IsNaN(down[i]):
			out[i] = math.NaN()
		case up[i] == 0 && down[i] == 0:
			out[i] = 50
		case down[i] == 0:
			out[i] = 100
		default:
			out[i] = 100 - 100/(1+up[i]/down[i])
		}
	}
	return out
}

func refCCI(cs []tickflow.Candle, n int) []float64 {
	tp := make([]float64, len(cs))
	for i, c := range cs {
		tp[i] = (c.High + c.Low + c.Close) / 3
	}
	out := make([]float64, len(cs))
	for i := range cs {
		if i < n-1 {
			out[i] = math.NaN()
			continue
		}
		var sum float64
		for j := i - n + 1; j <= i; j++ {
			sum += tp[j]
		}
		m := sum / float64(n)
		var dev float64
		for j := i - n + 1; j <= i; j++ {
			dev += math.Abs(tp[j] - m)
		}
		md := dev / float64(n)
		if md == 0 {
			out[i] = 0
			continue
		}
		out[i] = (tp[i] - m) / (0.015 * md)
	}
	return out
}

func refBOLL(cs []tickflow.Candle, n int, k float64) (mid, up, dn []float64) {
	mid = make([]float64, len(cs))
	up = make([]float64, len(cs))
	dn = make([]float64, len(cs))
	for i := range cs {
		if i < n-1 {
			mid[i], up[i], dn[i] = math.NaN(), math.NaN(), math.NaN()
			continue
		}
		var sum float64
		for j := i - n + 1; j <= i; j++ {
			sum += cs[j].Close
		}
		m := sum / float64(n)
		var sq float64
		for j := i - n + 1; j <= i; j++ {
			d := cs[j].Close - m
			sq += d * d
		}
		sd := math.Sqrt(sq / float64(n)) // 总体标准差
		mid[i], up[i], dn[i] = m, m+k*sd, m-k*sd
	}
	return
}

func refKDJ(cs []tickflow.Candle, n, m1, m2 int, conv Convention) (kv, dv, jv []float64) {
	rsv := make([]float64, len(cs))
	for i := range cs {
		if i < n-1 {
			rsv[i] = math.NaN()
			continue
		}
		hi, lo := math.Inf(-1), math.Inf(1)
		for j := i - n + 1; j <= i; j++ {
			hi = math.Max(hi, cs[j].High)
			lo = math.Min(lo, cs[j].Low)
		}
		if hi <= lo {
			rsv[i] = 50
			continue
		}
		rsv[i] = (cs[i].Close - lo) / (hi - lo) * 100
	}

	if conv == CN {
		kv = refSmooth(rsv, 1/float64(m1), 1)
		dv = refSmooth(kv, 1/float64(m2), 1)
	} else {
		kv = refSimpleMA(rsv, m1)
		dv = refSimpleMA(kv, m2)
	}

	jv = make([]float64, len(cs))
	for i := range cs {
		if math.IsNaN(kv[i]) || math.IsNaN(dv[i]) {
			kv[i], dv[i], jv[i] = math.NaN(), math.NaN(), math.NaN()
			continue
		}
		jv[i] = 3*kv[i] - 2*dv[i]
	}
	return
}

// refSimpleMA 对一串带 NaN 前缀的序列做简单移动平均，NaN 不计入窗口。
func refSimpleMA(xs []float64, n int) []float64 {
	out := make([]float64, len(xs))
	var seen []float64
	for i, x := range xs {
		out[i] = math.NaN()
		if math.IsNaN(x) {
			continue
		}
		seen = append(seen, x)
		if len(seen) < n {
			continue
		}
		var sum float64
		for _, v := range seen[len(seen)-n:] {
			sum += v
		}
		out[i] = sum / float64(n)
	}
	return out
}

// ---------------------------------------------------------------- 对比

func cmpCol(t *testing.T, label string, ind Indicator, cs []tickflow.Candle, k int, want []float64) {
	t.Helper()
	rows := Compute(ind, cs)
	for i := range cs {
		got := rows[i][k]
		if math.IsNaN(want[i]) {
			if !math.IsNaN(got) {
				t.Fatalf("%s[%d]：流式给了 %v，批量参考是 NaN", label, i, got)
			}
			continue
		}
		if math.IsNaN(got) {
			t.Fatalf("%s[%d]：流式给了 NaN，批量参考是 %v", label, i, want[i])
		}
		tol := 1e-9 * math.Max(1, math.Abs(want[i]))
		if math.Abs(got-want[i]) > tol {
			t.Fatalf("%s[%d]：流式 %v，批量参考 %v，差 %v", label, i, got, want[i], got-want[i])
		}
	}
}

func TestStreamingMatchesBatchReference(t *testing.T) {
	for _, seed := range []uint64{1, 42, 20260831} {
		cs := randomWalk(1000, seed)
		for _, conv := range []Convention{TV, CN} {
			c := conv
			t.Run(c.String(), func(t *testing.T) {
				cmpCol(t, "ma20", MA(20, c), cs, 0, refMA(cs, 20))
				cmpCol(t, "ema20", EMA(20, c), cs, 0, refEMA(cs, 20, c))
				cmpCol(t, "rsi14", RSI(14, c), cs, 0, refRSI(cs, 14, c))
				cmpCol(t, "cci20", CCI(20, c), cs, 0, refCCI(cs, 20))

				mid, up, dn := refBOLL(cs, 20, 2)
				cmpCol(t, "boll.mid", BOLL(20, 2, c), cs, 0, mid)
				cmpCol(t, "boll.up", BOLL(20, 2, c), cs, 1, up)
				cmpCol(t, "boll.dn", BOLL(20, 2, c), cs, 2, dn)

				dif, dea, hist := refMACD(cs, 12, 26, 9, c)
				cmpCol(t, "macd.dif", MACD(12, 26, 9, c), cs, 0, dif)
				cmpCol(t, "macd.dea", MACD(12, 26, 9, c), cs, 1, dea)
				cmpCol(t, "macd.hist", MACD(12, 26, 9, c), cs, 2, hist)

				kv, dv, jv := refKDJ(cs, 9, 3, 3, c)
				cmpCol(t, "kdj.k", KDJ(9, 3, 3, c), cs, 0, kv)
				cmpCol(t, "kdj.d", KDJ(9, 3, 3, c), cs, 1, dv)
				cmpCol(t, "kdj.j", KDJ(9, 3, 3, c), cs, 2, jv)
			})
		}
	}
}

// TestOddParams 换一批不常见的参数再对一遍。
// 窗口与播种的下标记账最容易在「参数不等」时露馅——n、m1、m2 都一样的话，
// 差一位的错误可能恰好被掩盖过去。
func TestOddParams(t *testing.T) {
	cs := randomWalk(600, 99)
	for _, conv := range []Convention{TV, CN} {
		c := conv
		t.Run(c.String(), func(t *testing.T) {
			cmpCol(t, "ma1", MA(1, c), cs, 0, refMA(cs, 1))
			cmpCol(t, "ema1", EMA(1, c), cs, 0, refEMA(cs, 1, c))
			cmpCol(t, "rsi2", RSI(2, c), cs, 0, refRSI(cs, 2, c))
			cmpCol(t, "cci7", CCI(7, c), cs, 0, refCCI(cs, 7))

			dif, dea, hist := refMACD(cs, 5, 35, 5, c)
			cmpCol(t, "macd.dif", MACD(5, 35, 5, c), cs, 0, dif)
			cmpCol(t, "macd.dea", MACD(5, 35, 5, c), cs, 1, dea)
			cmpCol(t, "macd.hist", MACD(5, 35, 5, c), cs, 2, hist)

			// n、m1、m2 三者互不相等，专挑下标记账的错。
			kv, dv, jv := refKDJ(cs, 14, 5, 3, c)
			cmpCol(t, "kdj.k", KDJ(14, 5, 3, c), cs, 0, kv)
			cmpCol(t, "kdj.d", KDJ(14, 5, 3, c), cs, 1, dv)
			cmpCol(t, "kdj.j", KDJ(14, 5, 3, c), cs, 2, jv)

			mid, up, dn := refBOLL(cs, 34, 2.5)
			cmpCol(t, "boll.mid", BOLL(34, 2.5, c), cs, 0, mid)
			cmpCol(t, "boll.up", BOLL(34, 2.5, c), cs, 1, up)
			cmpCol(t, "boll.dn", BOLL(34, 2.5, c), cs, 2, dn)
		})
	}
}

// TestStreamingIsIncremental 保证「一根一根喂」与「先喂一半再喂另一半」结果相同，
// 也就是指标除了自己的状态之外不依赖任何全局或批次信息。
func TestStreamingIsIncremental(t *testing.T) {
	cs := randomWalk(300, 17)
	for _, mk := range allIndicators() {
		for _, conv := range []Convention{TV, CN} {
			whole := Compute(mk(conv), cs)

			ind := mk(conv)
			ind.Reset()
			var split [][]float64
			for _, c := range cs[:137] {
				split = append(split, append([]float64(nil), ind.Update(c)...))
			}
			for _, c := range cs[137:] {
				split = append(split, append([]float64(nil), ind.Update(c)...))
			}

			for i := range whole {
				for k := range whole[i] {
					a, b := whole[i][k], split[i][k]
					if math.IsNaN(a) != math.IsNaN(b) || (!math.IsNaN(a) && a != b) {
						t.Fatalf("%s/%s 第 %d 根第 %d 路：整批 %v，分两批 %v",
							ind.Name(), conv, i, k, a, b)
					}
				}
			}
		}
	}
}
