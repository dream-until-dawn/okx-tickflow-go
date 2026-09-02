package indicator

import (
	"math"
	"testing"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// 本文件盯住「有定义」与「已收敛」的区别。
//
// Warmup() 报的是「算得出一个数」，Settle() 报的是「这个数不再取决于从哪根开始喂」。
// 窗口类指标两者相同；递归类相差一到两个数量级——而在补上 Settler 之前，
// Feed 是按 Warmup 决定预热量的，于是国内口径的 MACD 只预读 4 根，
// 值错了一倍而 Ready() 照报 true。

// measureSettle 实测「预读多少根之后，末根的值与从头喂到底【逐位相等】」。
// 不用容差——容差要挑一个数，而挑多少本身就是要论证的东西。
func measureSettle(t *testing.T, mk func() Indicator, cs []tickflow.Candle, field int) int {
	t.Helper()
	full := Compute(mk(), cs)
	want := full[len(full)-1][field]
	if math.IsNaN(want) {
		t.Fatalf("基线数据不够长，末根仍是 NaN")
	}
	for pre := 1; pre < len(cs); pre++ {
		part := Compute(mk(), cs[len(cs)-pre:])
		if part[len(part)-1][field] == want {
			return pre
		}
	}
	return -1
}

// TestSettleCoversActualConvergence 是 Settler 的核心契约：
// **Settle() 报的根数必须真的够用。**
//
// 期望值不写死——当场实测出逐位收敛点再比。写死的话，改了 settleEps 或换了
// 基线数据，这条测试要么假绿要么假红。
func TestSettleCoversActualConvergence(t *testing.T) {
	cs := loadETHDaily(t)

	for _, c := range []struct {
		name  string
		mk    func() Indicator
		field int
	}{
		{"MA(20)", func() Indicator { return MA(20) }, 0},
		{"BOLL(20,2).up", func() Indicator { return BOLL(20, 2) }, 1},
		{"CCI(20)", func() Indicator { return CCI(20) }, 0},
		{"KDJ(9,3,3)/TV.k", func() Indicator { return KDJ(9, 3, 3, TV) }, 0},
		{"KDJ(9,3,3)/CN.k", func() Indicator { return KDJ(9, 3, 3, CN) }, 0},
		{"EMA(20)/TV", func() Indicator { return EMA(20, TV) }, 0},
		{"EMA(20)/CN", func() Indicator { return EMA(20, CN) }, 0},
		{"RSI(14)/TV", func() Indicator { return RSI(14, TV) }, 0},
		{"RSI(14)/CN", func() Indicator { return RSI(14, CN) }, 0},
		{"MACD/TV.dea", func() Indicator { return MACD(12, 26, 9, TV) }, 1},
		{"MACD/CN.dea", func() Indicator { return MACD(12, 26, 9, CN) }, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			ind := c.mk()
			measured := measureSettle(t, c.mk, cs, c.field)
			if measured < 0 {
				t.Fatalf("500 根之内没收敛，基线不够长")
			}
			declared := tickflow.IndicatorSettle(ind)
			if declared < measured {
				t.Errorf("Settle() 报 %d，实测要 %d 根才逐位收敛——报少了 %d 根，"+
					"Feed 会照着预热不足的量去读", declared, measured, measured-declared)
			}
			// 也不该离谱地保守：多读几百根 K 线是要花时间和内存的。
			if declared > measured*3+50 {
				t.Errorf("Settle() 报 %d，实测只要 %d——过于保守了", declared, measured)
			}
			t.Logf("Warmup %d / Settle %d / 实测 %d", ind.Warmup(), declared, measured)
		})
	}
}

// TestWindowedIndicatorsSettleAtWarmup：窗口类指标两者必须相等。
// 窗口滑过去就与更早的数据无关，多报一根都是白读。
func TestWindowedIndicatorsSettleAtWarmup(t *testing.T) {
	for _, ind := range []Indicator{
		MA(20), BOLL(20, 2), CCI(20), KDJ(9, 3, 3, TV),
	} {
		if got, want := tickflow.IndicatorSettle(ind), ind.Warmup(); got != want {
			t.Errorf("%s 的 Settle() = %d，窗口类指标应当等于 Warmup() = %d",
				ind.Name(), got, want)
		}
	}
}

// TestOnlyWarmupIsBadlyInsufficient 把「为什么要有 Settler」量成一个数。
//
// 没有这一条，上面那些测试只说明「Settle() 够用」，说明不了「不用它会怎样」。
func TestOnlyWarmupIsBadlyInsufficient(t *testing.T) {
	cs := loadETHDaily(t)
	mk := func() Indicator { return MACD(12, 26, 9, CN) }

	full := Compute(mk(), cs)
	want := full[len(full)-1][0]

	// 按 Warmup 预热（还乘了 Feed 的 slack=2）能拿到的根数。
	warm := mk().Warmup() * 2
	part := Compute(mk(), cs[len(cs)-warm:])
	got := part[len(part)-1][0]
	rel := math.Abs(got-want) / math.Abs(want)

	if rel < 0.5 {
		t.Fatalf("只预读 %d 根时相对误差只有 %.2e——这个测试的前提不成立了，"+
			"要么 Warmup 的语义变了，要么基线数据变得太平缓", warm, rel)
	}
	t.Logf("只按 Warmup 预读 %d 根：MACD.dif = %.4f，收敛值 %.4f，相对误差 %.2f",
		warm, got, want, rel)

	// 按 Settle 预热则应当已经逐位收敛。
	settle := tickflow.IndicatorSettle(mk())
	if settle >= len(cs) {
		t.Skipf("基线只有 %d 根，装不下 Settle() 要的 %d 根", len(cs), settle)
	}
	part = Compute(mk(), cs[len(cs)-settle:])
	if part[len(part)-1][0] != want {
		t.Errorf("按 Settle() 预读 %d 根仍未逐位收敛", settle)
	}
}
