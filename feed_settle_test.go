package tickflow

import (
	"math"
	"testing"
)

// settleInd 是个能分别报告「有定义」与「已收敛」的测试指标。
// 值只在收敛之后才稳定——收敛前每根都不一样，好让「用了未收敛的值」当场显形。
type settleInd struct {
	warm, set int
	n         int
	out       []float64
}

func (s *settleInd) Name() string     { return "s" }
func (s *settleInd) Fields() []string { return nil }
func (s *settleInd) Warmup() int      { return s.warm }
func (s *settleInd) Settle() int      { return s.set }
func (s *settleInd) Reset()           { s.n = 0 }
func (s *settleInd) Update(c Candle) []float64 {
	s.n++
	if s.out == nil {
		s.out = make([]float64, 1)
	}
	switch {
	case s.n < s.warm:
		s.out[0] = math.NaN() // 还没定义
	case s.n < s.set:
		s.out[0] = float64(s.n) // 有定义但没收敛：值随喂了多少根而变
	default:
		s.out[0] = 1e6 // 收敛之后是个定值
	}
	return s.out
}

// TestFeedWarmsUpBySettleNotWarmup 是这次修复的核心：
// Feed 的自动预热必须按 Settle 算，不是 Warmup。
//
// 补上 Settler 之前，国内口径的 MACD 只会被预读 4 根（Warmup=1 乘 slack=2 再加
// Lookback），而它要 400 多根才收敛——值错了一倍，Ready() 还报 true。
func TestFeedWarmsUpBySettleNotWarmup(t *testing.T) {
	base := MustParsePeriod("1H")
	cs := genBars(base, t0, 800)
	st := newMemStore()
	fill(t, st, "X", "1H", cs)

	f, err := NewFeed(st, FeedConfig{
		InstID: "X", Base: "1H", From: cs[600].Ts,
		Indicators: map[string][]Indicator{"1H": {&settleInd{warm: 2, set: 300}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if !f.Next() {
		t.Fatal(f.Err())
	}
	v := f.View()
	if !v.Ready() {
		t.Fatal("自动预热之后第一步就该就绪——按 Settle 预读的话历史是够的")
	}
	if got := v.Ind("s"); got != 1e6 {
		t.Errorf("第一步拿到 %v，那是【未收敛】的值；预热量按 Warmup 算了？", got)
	}
}

// TestReadyMeansConvergedNotDefined：历史不够收敛时，Ready 必须报 false。
//
// 报 true 才是真正的问题——那等于把一个还在受播种影响的值当成可信的交出去。
func TestReadyMeansConvergedNotDefined(t *testing.T) {
	base := MustParsePeriod("1H")
	cs := genBars(base, t0, 60) // 只有 60 根，装不下 set=300
	st := newMemStore()
	fill(t, st, "X", "1H", cs)

	f, err := NewFeed(st, FeedConfig{
		InstID: "X", Base: "1H", From: cs[0].Ts,
		Indicators: map[string][]Indicator{"1H": {&settleInd{warm: 2, set: 300}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var steps, ready, defined int
	for f.Next() {
		v := f.View()
		steps++
		if v.Ready() {
			ready++
		}
		if v.Defined() {
			defined++
		}
	}
	if err := f.Err(); err != nil {
		t.Fatal(err)
	}
	if steps != len(cs) {
		t.Fatalf("走了 %d 步，期望 %d", steps, len(cs))
	}
	if ready != 0 {
		t.Errorf("历史只有 %d 根、收敛要 300 根，却有 %d 步报了 Ready", len(cs), ready)
	}
	// 但「有定义」是成立的——两个判据必须分得开，否则 Defined 就没有存在的意义。
	if defined != steps-1 {
		t.Errorf("Defined 的步数是 %d，期望 %d（warmup=2，第一步还没定义）", defined, steps-1)
	}
}

// TestSettlerIsOptional：不实现 Settler 的指标退回用 Warmup，行为不变。
// 窗口类指标本来就该如此，外部实现也不该被迫改。
func TestSettlerIsOptional(t *testing.T) {
	plain := newInd("plain", 30) // testInd 不实现 Settler
	if got, want := IndicatorSettle(plain), 30; got != want {
		t.Errorf("未实现 Settler 时应当退回 Warmup()=%d，实为 %d", want, got)
	}

	base := MustParsePeriod("1H")
	cs := genBars(base, t0, 200)
	st := newMemStore()
	fill(t, st, "X", "1H", cs)
	f, err := NewFeed(st, FeedConfig{
		InstID: "X", Base: "1H", From: cs[100].Ts,
		Indicators: map[string][]Indicator{"1H": {plain}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !f.Next() || !f.View().Ready() {
		t.Error("只实现 Warmup 的指标，预热之后也该就绪")
	}
}

// TestSettleBelowWarmupIsIgnored：Settle 报得比 Warmup 还小是没有意义的，
// 取两者较大者——一个写错的外部实现不该让预热量反而变少。
func TestSettleBelowWarmupIsIgnored(t *testing.T) {
	bad := &settleInd{warm: 50, set: 3}
	if got := IndicatorSettle(bad); got != 50 {
		t.Errorf("Settle(3) < Warmup(50) 时应当取 50，实为 %d", got)
	}
}
