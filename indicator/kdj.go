package indicator

import (
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// KDJ 输出 k / d / j 三路，默认键名 "kdj"。经典参数是 KDJ(9, 3, 3)。
//
//	RSV = (收 − n 根内最低) / (n 根内最高 − n 根内最低) × 100
//
// 往下两套口径【结构上就不同】，不只是播种的差别：
//
//	CN：K = SMA(RSV, m1, 1)，D = SMA(K, m2, 1)   —— 指数式平滑，α = 1/m
//	TV：K = MA(RSV, m1)，   D = MA(K, m2)        —— 简单移动平均
//
// TV 这一路就是 TradingView 的 Stochastic。它【没有 J 线】——J = 3K − 2D
// 是国内的画法，本库在两套口径下都按这个公式给出 J，好让字段结构一致。
// 用 TV 口径时 J 是本库替 TradingView 补的，那边没有对应的线可以对数。
//
// CN 的 K、D 用首个样本播种（这正是通达信 SMA(X,N,M) 的语义）。有些软件
// 改成把 K、D 初始化为 50，差别在开头十几根内，之后按 (1−1/m)^k 衰减掉。
func KDJ(n, m1, m2 int, opts ...Option) Indicator {
	mustPositive("KDJ 的 RSV 周期", n)
	mustPositive("KDJ 的 K 平滑周期", m1)
	mustPositive("KDJ 的 D 平滑周期", m2)
	b := newBase("kdj", opts)
	k := &kdjIndicator{
		base: b,
		n:    n, m1: m1, m2: m2,
		wh:  newWindow(n),
		wl:  newWindow(n),
		out: make([]float64, 3),
	}
	if b.conv == CN {
		k.kSm = newSmoother(wilderAlpha(m1), m1, CN)
		k.dSm = newSmoother(wilderAlpha(m2), m2, CN)
	} else {
		k.kw = newWindow(m1)
		k.dw = newWindow(m2)
	}
	return k
}

type kdjIndicator struct {
	base
	n, m1, m2 int
	wh, wl    *window

	kSm, dSm *smoother // CN
	kw, dw   *window   // TV

	out []float64
}

func (k *kdjIndicator) Fields() []string { return []string{"k", "d", "j"} }

func (k *kdjIndicator) Reset() {
	k.wh.reset()
	k.wl.reset()
	if k.conv == CN {
		k.kSm.reset()
		k.dSm.reset()
	} else {
		k.kw.reset()
		k.dw.reset()
	}
}

func (k *kdjIndicator) Warmup() int {
	if k.conv == CN {
		// RSV 在第 n 根有值，K、D 首值播种，同一根就都有值了。
		return k.n
	}
	// TV：RSV 在第 n 根有值；K 要攒 m1 个 RSV，D 再攒 m2 个 K。
	return k.n + k.m1 + k.m2 - 2
}

func (k *kdjIndicator) Update(c tickflow.Candle) []float64 {
	k.wh.push(c.High)
	k.wl.push(c.Low)
	if !k.wh.full() {
		return fillNaN(k.out)
	}

	hi, lo := k.wh.max(), k.wl.min()
	rsv := 50.0
	// 区间内高低相等（一根都没动过）时 RSV 无定义。取 50 而不是 NaN：
	// 让一段横盘把后面所有的 K、D 都污染成 NaN，代价远大于这里取个中性值。
	if hi > lo {
		rsv = (c.Close - lo) / (hi - lo) * 100
	}

	var kv, dv float64
	if k.conv == CN {
		var ok bool
		if kv, ok = k.kSm.update(rsv); !ok {
			return fillNaN(k.out)
		}
		if dv, ok = k.dSm.update(kv); !ok {
			return fillNaN(k.out)
		}
	} else {
		k.kw.push(rsv)
		if !k.kw.full() {
			return fillNaN(k.out)
		}
		kv = k.kw.mean()
		k.dw.push(kv)
		if !k.dw.full() {
			return fillNaN(k.out)
		}
		dv = k.dw.mean()
	}

	k.out[0], k.out[1], k.out[2] = kv, dv, 3*kv-2*dv
	return k.out
}
