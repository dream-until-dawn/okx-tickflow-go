package tickflow

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 造数

// genBars 按周期造一串连续的 K 线，价格是个确定性的锯齿，够指标算出不同的值。
func genBars(p Period, start int64, n int) []Candle {
	out := make([]Candle, 0, n)
	ts := p.Truncate(start)
	px := 100.0
	for i := 0; i < n; i++ {
		px += float64((i*37)%13) - 6
		if px < 10 {
			px = 10
		}
		o := px - 1
		out = append(out, Candle{
			Ts: ts, Open: o, High: px + 2, Low: o - 2, Close: px,
			Vol: float64(10 + i%7), VolCcy: float64(i), VolCcyQuote: float64(i) * px,
		})
		ts = p.Next(ts)
	}
	return out
}

// aggregateBy 独立地把低周期并成高周期，供聚合器对照。
// 故意不复用 aggregator——那样测的就是它自己。
func aggregateBy(p Period, cs []Candle) []Candle {
	var out []Candle
	var cur *Candle
	for _, c := range cs {
		open := p.Truncate(c.Ts)
		if cur == nil || cur.Ts != open {
			if cur != nil {
				out = append(out, *cur)
			}
			cp := c
			cp.Ts = open
			cur = &cp
			continue
		}
		if c.High > cur.High {
			cur.High = c.High
		}
		if c.Low < cur.Low {
			cur.Low = c.Low
		}
		cur.Close = c.Close
		cur.Vol += c.Vol
		cur.VolCcy += c.VolCcy
		cur.VolCcyQuote += c.VolCcyQuote
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

func fill(t *testing.T, st *memStore, inst, bar string, cs []Candle) {
	t.Helper()
	if err := st.Append(inst, bar, cs); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCoverage(inst, bar, Range{From: cs[0].Ts, To: cs[len(cs)-1].Ts + 1}); err != nil {
		t.Fatal(err)
	}
}

// testInd 是个最小的自定义指标，用来验证外部实现能像内置的一样挂进 Feed。
type testInd struct {
	name   string
	fields []string
	warm   int
	n      int
	out    []float64
}

func (i *testInd) Name() string     { return i.name }
func (i *testInd) Fields() []string { return i.fields }
func (i *testInd) Warmup() int      { return i.warm }
func (i *testInd) Reset()           { i.n = 0 }
func (i *testInd) Update(c Candle) []float64 {
	i.n++
	if i.out == nil {
		i.out = make([]float64, max(1, len(i.fields)))
	}
	if i.n < i.warm {
		for k := range i.out {
			i.out[k] = math.NaN()
		}
		return i.out
	}
	for k := range i.out {
		i.out[k] = c.Close + float64(k)
	}
	return i.out
}

func newInd(name string, warm int, fields ...string) *testInd {
	return &testInd{name: name, fields: fields, warm: warm}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- 核心保证

// TestTFNeverShowsUnclosedBar 是本库最主要的那条保证，也是自制回测里最常见、
// 最难自查的未来函数：高周期 K 线在收盘之前不可见。
//
// 每一步同时验两个方向——【不能超前】（看到的那根必须已经收盘）与
// 【不能落后】（下一根必须还没收盘）。只验一个方向的话，一个「永远返回第一根」
// 的实现也能通过。
func TestTFNeverShowsUnclosedBar(t *testing.T) {
	st := newMemStore()
	base := MustParsePeriod("15m")
	start := MustParsePeriod("1D").Truncate(t0)

	m15 := genBars(base, start, 4*96) // 四天
	fill(t, st, "X", "15m", m15)
	fill(t, st, "X", "1H", genBars(MustParsePeriod("1H"), start, 4*24))
	fill(t, st, "X", "1D", genBars(MustParsePeriod("1D"), start, 6))

	f, err := NewFeed(st, Config{
		InstID: "X", Base: "15m", Extra: []string{"1H", "1D"},
		From: m15[0].Ts, Lookback: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var steps, sawH, sawD int
	for f.Next() {
		v := f.View()
		closeTs := base.Next(v.Ts())
		steps++

		for _, bar := range []string{"1H", "1D"} {
			tv := f.TF(bar)
			if !tv.Valid() {
				continue
			}
			p := MustParsePeriod(bar)

			// 不能超前：看到的这根必须在主周期这根收盘之前就已经收盘。
			if p.Next(tv.Ts()) > closeTs {
				t.Fatalf("主周期 %s：%s 给出了 %s，它要到 %s 才收盘——这是未来函数",
					str(v.Ts()), bar, str(tv.Ts()), str(p.Next(tv.Ts())))
			}
			// 不能落后：下一根若也已收盘，说明给的不是【最后一根】。
			if nxt := p.Next(tv.Ts()); p.Next(nxt) <= closeTs {
				t.Fatalf("主周期 %s：%s 给出了 %s，但 %s 也已经收盘了——落后了一根",
					str(v.Ts()), bar, str(tv.Ts()), str(nxt))
			}
			if bar == "1H" {
				sawH++
			} else {
				sawD++
			}
		}
	}
	if err := f.Err(); err != nil {
		t.Fatal(err)
	}
	if steps != len(m15) {
		t.Errorf("走了 %d 步，期望 %d", steps, len(m15))
	}
	if sawH == 0 || sawD == 0 {
		t.Fatalf("辅周期一次都没有效过（1H %d 次，1D %d 次），这个测试什么都没验到", sawH, sawD)
	}
}

// TestTFSpotCheck 用一个具体时刻把上面那条规则钉成看得见的数。
func TestTFSpotCheck(t *testing.T) {
	st := newMemStore()
	d1 := MustParsePeriod("1D") // 港时对齐：一天从 UTC 16:00 起
	start := d1.Truncate(ms(t, "2026-01-10T00:00:00Z"))

	fill(t, st, "X", "15m", genBars(MustParsePeriod("15m"), start, 8*96))
	fill(t, st, "X", "1D", genBars(d1, start, 10))

	f, err := NewFeed(st, Config{
		InstID: "X", Base: "15m", Extra: []string{"1D"},
		From: ms(t, "2026-01-15T10:15:00Z"), To: ms(t, "2026-01-15T10:30:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if !f.Next() {
		t.Fatalf("一步都没走出来: %v", f.Err())
	}
	if got, want := f.View().Ts(), ms(t, "2026-01-15T10:15:00Z"); got != want {
		t.Fatalf("主周期停在 %s，期望 %s", str(got), str(want))
	}
	// 港时 01-15 那天从 UTC 01-14 16:00 起，此刻还没走完；
	// 最后一根已收盘的是港时 01-14，即 UTC 01-13 16:00 那根。
	if got, want := f.TF("1D").Ts(), ms(t, "2026-01-13T16:00:00Z"); got != want {
		t.Fatalf("TF(\"1D\") 给了 %s，期望 %s（港时 01-14 那根）", str(got), str(want))
	}
}

// TestAggregateMatchesStore 保证两种辅周期来源【可见性时点完全一致】。
//
// 不然的话，Aggregate 只是个开关，一拨回测结果就变了——而变化的原因藏在
// 「高周期什么时候算收盘」这种没人会去查的地方。
func TestAggregateMatchesStore(t *testing.T) {
	base := MustParsePeriod("15m")
	h1 := MustParsePeriod("1H")
	start := MustParsePeriod("1D").Truncate(t0)
	m15 := genBars(base, start, 3*96)

	st := newMemStore()
	fill(t, st, "X", "15m", m15)
	fill(t, st, "X", "1H", aggregateBy(h1, m15))

	collect := func(agg bool) []Candle {
		f, err := NewFeed(st, Config{
			InstID: "X", Base: "15m", Extra: []string{"1H"},
			From: m15[0].Ts, Aggregate: agg,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var out []Candle
		for f.Next() {
			out = append(out, f.TF("1H").Candle())
		}
		if err := f.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	fromStore, fromAgg := collect(false), collect(true)
	if len(fromStore) != len(fromAgg) {
		t.Fatalf("步数不同：库 %d，聚合 %d", len(fromStore), len(fromAgg))
	}
	for i := range fromStore {
		if fromStore[i] != fromAgg[i] {
			t.Fatalf("第 %d 步不一致：\n库   %+v\n聚合 %+v", i, fromStore[i], fromAgg[i])
		}
	}
}

// recorder 是个把喂进来的每一根都记下来的指标，用于验证某个周期【实际收到了】
// 哪些 K 线——这和「视图当前停在哪一根」是两回事：一步之内收掉两根时，
// 视图只会停在较新的那根。
type recorder struct {
	seen []Candle
	out  []float64
}

func (r *recorder) Name() string     { return "rec" }
func (r *recorder) Fields() []string { return nil }
func (r *recorder) Warmup() int      { return 1 }
func (r *recorder) Reset()           { r.seen = nil }
func (r *recorder) Update(c Candle) []float64 {
	r.seen = append(r.seen, c)
	if r.out == nil {
		r.out = make([]float64, 1)
	}
	r.out[0] = c.Close
	return r.out
}

// TestAggregateAcrossGap 让主周期缺一整段，逼聚合器在一步里收掉两根高周期 K 线。
// 缺的那几根不能让整根小时线凭空消失——它只是内容不完整。
func TestAggregateAcrossGap(t *testing.T) {
	base := MustParsePeriod("15m")
	h1 := MustParsePeriod("1H")
	start := MustParsePeriod("1D").Truncate(t0)
	full := genBars(base, start, 24)

	// 挖掉横跨两根小时线的一段，制造真实存在的空洞。
	var holed []Candle
	for i, c := range full {
		if i >= 5 && i <= 10 {
			continue
		}
		holed = append(holed, c)
	}

	st := newMemStore()
	fill(t, st, "X", "15m", holed)

	rec := &recorder{}
	f, err := NewFeed(st, Config{
		InstID: "X", Base: "15m", Extra: []string{"1H"},
		From: holed[0].Ts, Aggregate: true, Lookback: 2,
		Indicators: map[string][]Indicator{"1H": {rec}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for f.Next() {
		tv := f.TF("1H")
		if !tv.Valid() {
			continue
		}
		// 同一条保证：跨过空洞之后也不能超前。
		if h1.Next(tv.Ts()) > base.Next(f.View().Ts()) {
			t.Fatalf("空洞之后 1H 给出了未收盘的 %s", str(tv.Ts()))
		}
	}
	if err := f.Err(); err != nil {
		t.Fatal(err)
	}

	// 独立聚合一遍作为对照，只取【到数据末尾为止已经收盘】的那些——
	// 末尾那根小时线收没收盘，取决于数据恰好停在哪里，不能拍脑袋减一。
	lastClose := base.Next(holed[len(holed)-1].Ts)
	var want []Candle
	for _, c := range aggregateBy(h1, holed) {
		if h1.Next(c.Ts) <= lastClose {
			want = append(want, c)
		}
	}
	if len(rec.seen) != len(want) {
		t.Fatalf("聚合出了 %d 根小时线，期望 %d 根", len(rec.seen), len(want))
	}
	for i, c := range rec.seen {
		if c != want[i] {
			t.Fatalf("第 %d 根小时线不一致：\n聚合器 %+v\n对照   %+v", i, c, want[i])
		}
	}
}

// ---------------------------------------------------------------- 视图

func TestLookbackAndPrev(t *testing.T) {
	base := MustParsePeriod("1H")
	cs := genBars(base, t0, 50)
	st := newMemStore()
	fill(t, st, "X", "1H", cs)

	f, err := NewFeed(st, Config{InstID: "X", Base: "1H", From: cs[0].Ts, Lookback: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for i := 0; f.Next(); i++ {
		v := f.View()
		if got := v.Ts(); got != cs[i].Ts {
			t.Fatalf("第 %d 步在 %s，期望 %s", i, str(got), str(cs[i].Ts))
		}
		for k := 1; k <= 3; k++ {
			pv := v.Prev(k)
			if i-k < 0 {
				if pv.Valid() {
					t.Errorf("第 %d 步 Prev(%d) 不该有效", i, k)
				}
				if !math.IsNaN(pv.Close()) {
					t.Errorf("无效视图的 Close 应当是 NaN，实为 %v", pv.Close())
				}
				continue
			}
			if !pv.Valid() {
				t.Fatalf("第 %d 步 Prev(%d) 应当有效", i, k)
			}
			if pv.Close() != cs[i-k].Close {
				t.Fatalf("第 %d 步 Prev(%d).Close = %v，期望 %v", i, k, pv.Close(), cs[i-k].Close)
			}
		}
		// 超出 Lookback 的必须失效，而不是给出一根错位的 K 线。
		if v.Prev(4).Valid() && i >= 4 {
			t.Fatalf("第 %d 步 Prev(4) 超出了 Lookback=3，不该有效", i)
		}
	}
}

func TestIndicatorsInView(t *testing.T) {
	base := MustParsePeriod("1H")
	cs := genBars(base, t0, 60)
	st := newMemStore()
	fill(t, st, "X", "1H", cs)

	f, err := NewFeed(st, Config{
		InstID: "X", Base: "1H", From: cs[30].Ts, Lookback: 2,
		Indicators: map[string][]Indicator{
			"1H": {newInd("one", 5), newInd("three", 5, "a", "b", "c")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	want := []string{"one", "three.a", "three.b", "three.c"}
	if got := f.Keys("1H"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Keys = %v，期望 %v", got, want)
	}

	h, err := f.Handle("1H", "three.b")
	if err != nil {
		t.Fatal(err)
	}
	if !f.Next() {
		t.Fatal(f.Err())
	}
	v := f.View()
	// 自动预热应当已经把指标喂饱。
	if !v.Ready() {
		t.Fatal("自动预热之后指标仍未就绪")
	}
	if got, want := v.Ind("one"), v.Close(); got != want {
		t.Errorf("Ind(\"one\") = %v，期望 %v", got, want)
	}
	if got, want := v.Ind("three.b"), v.Close()+1; got != want {
		t.Errorf("Ind(\"three.b\") = %v，期望 %v", got, want)
	}
	if got := v.At(h); got != v.Ind("three.b") {
		t.Errorf("At(handle) = %v，与 Ind 不一致 %v", got, v.Ind("three.b"))
	}
	if got := v.Ind("没这个键"); !math.IsNaN(got) {
		t.Errorf("未知键应当返回 NaN，实为 %v", got)
	}
}

// TestHandleFromOtherTimeframe：拿错周期的 Handle 去取值必须返回 NaN。
// 静默返回另一个周期的数是最难查的一类错。
func TestHandleFromOtherTimeframe(t *testing.T) {
	base := MustParsePeriod("15m")
	start := MustParsePeriod("1D").Truncate(t0)
	fillSt := newMemStore()
	fill(t, fillSt, "X", "15m", genBars(base, start, 200))
	fill(t, fillSt, "X", "1H", genBars(MustParsePeriod("1H"), start, 60))

	f, err := NewFeed(fillSt, Config{
		InstID: "X", Base: "15m", Extra: []string{"1H"},
		Indicators: map[string][]Indicator{
			"15m": {newInd("x", 1)},
			"1H":  {newInd("x", 1)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	hourly, err := f.Handle("1H", "x")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && f.Next(); i++ {
	}
	if got := f.View().At(hourly); !math.IsNaN(got) {
		t.Errorf("拿 1H 的 Handle 去取 15m 的值应当返回 NaN，实为 %v", got)
	}
	if got := f.TF("1H").At(hourly); math.IsNaN(got) {
		t.Error("拿 1H 的 Handle 去取 1H 的值不该是 NaN")
	}
}

func TestWarmupAutoExtends(t *testing.T) {
	base := MustParsePeriod("1H")
	cs := genBars(base, t0, 300)
	st := newMemStore()
	fill(t, st, "X", "1H", cs)

	mk := func(noWarm bool) bool {
		f, err := NewFeed(st, Config{
			InstID: "X", Base: "1H", From: cs[200].Ts, NoAutoWarmup: noWarm,
			Indicators: map[string][]Indicator{"1H": {newInd("slow", 100)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if !f.Next() {
			t.Fatal(f.Err())
		}
		return f.View().Ready()
	}

	if !mk(false) {
		t.Error("自动预热开启时，第一步就该就绪")
	}
	if mk(true) {
		t.Error("关掉自动预热之后，第一步不该就绪")
	}
}

// TestNoAutoWarmupStillEmitsNaN 确认关掉预热时给的是 NaN 而不是半成品。
func TestNoAutoWarmupStillEmitsNaN(t *testing.T) {
	base := MustParsePeriod("1H")
	cs := genBars(base, t0, 50)
	st := newMemStore()
	fill(t, st, "X", "1H", cs)

	f, err := NewFeed(st, Config{
		InstID: "X", Base: "1H", From: cs[0].Ts, NoAutoWarmup: true,
		Indicators: map[string][]Indicator{"1H": {newInd("w", 10)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for i := 0; f.Next(); i++ {
		v := f.View()
		if i < 9 {
			if v.Ready() {
				t.Fatalf("第 %d 步不该就绪", i)
			}
			if !math.IsNaN(v.Ind("w")) {
				t.Fatalf("第 %d 步的指标应当是 NaN，实为 %v", i, v.Ind("w"))
			}
			continue
		}
		if !v.Ready() || math.IsNaN(v.Ind("w")) {
			t.Fatalf("第 %d 步应当就绪且有值", i)
		}
	}
}

// ---------------------------------------------------------------- 实盘 Push

func TestPushRealtime(t *testing.T) {
	base := MustParsePeriod("15m")
	start := MustParsePeriod("1D").Truncate(t0)
	m15 := genBars(base, start, 20)

	// store 为 nil：纯实盘形态，只靠 Push 驱动。
	f, err := NewFeed(nil, Config{
		InstID: "X", Base: "15m", Extra: []string{"1H"}, Aggregate: true, Lookback: 2,
		Indicators: map[string][]Indicator{"15m": {newInd("c", 1)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if f.Next() {
		t.Error("没有 store 时 Next 应当直接返回 false")
	}
	for _, c := range m15 {
		if err := f.Push("15m", c); err != nil {
			t.Fatal(err)
		}
		v := f.View()
		if v.Ts() != c.Ts || v.Close() != c.Close {
			t.Fatalf("Push 之后视图没跟上：%s / %v", str(v.Ts()), v.Close())
		}
		if tv := f.TF("1H"); tv.Valid() {
			if MustParsePeriod("1H").Next(tv.Ts()) > base.Next(c.Ts) {
				t.Fatalf("Push 驱动下 1H 也不能超前：%s", str(tv.Ts()))
			}
		}
	}
	if got := f.View().Prev(1).Ts(); got != m15[len(m15)-2].Ts {
		t.Errorf("Prev(1) = %s，期望 %s", str(got), str(m15[len(m15)-2].Ts))
	}
}

// TestPushValidation：实盘里重复推送与乱序推送都真实存在，静默接受会让指标悄悄算错。
func TestPushValidation(t *testing.T) {
	base := MustParsePeriod("15m")
	cs := genBars(base, t0, 5)
	f, err := NewFeed(nil, Config{InstID: "X", Base: "15m"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := f.Push("1H", cs[0]); err == nil {
		t.Error("推给没登记的周期应当报错")
	}
	bad := cs[0]
	bad.Ts += 1000
	if err := f.Push("15m", bad); err == nil {
		t.Error("没对齐的时间戳应当报错")
	}
	if err := f.Push("15m", cs[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.Push("15m", cs[0]); err == nil {
		t.Error("重复推送同一根应当报错")
	}
	if err := f.Push("15m", cs[1]); err != nil {
		t.Fatal(err)
	}
	if err := f.Push("15m", cs[0]); err == nil {
		t.Error("乱序推送应当报错")
	}
}

// ---------------------------------------------------------------- 构造校验

func TestNewFeedValidation(t *testing.T) {
	st := newMemStore()
	fill(t, st, "X", "15m", genBars(MustParsePeriod("15m"), t0, 100))

	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"缺 instId", Config{Base: "15m"}, "instId"},
		{"缺主周期", Config{InstID: "X"}, "Base"},
		{"未知主周期", Config{InstID: "X", Base: "7分钟"}, "未知周期"},
		{"未知辅周期", Config{InstID: "X", Base: "15m", Extra: []string{"nope"}}, "未知周期"},
		{"辅周期比主周期短", Config{InstID: "X", Base: "1H", Extra: []string{"15m"}}, "必须长于"},
		{"辅周期与主周期同长", Config{InstID: "X", Base: "1H", Extra: []string{"1H"}}, "重复"},
		{"负 Lookback", Config{InstID: "X", Base: "15m", Lookback: -1}, "Lookback"},
		{"指标挂在陌生周期上", Config{
			InstID: "X", Base: "15m",
			Indicators: map[string][]Indicator{"4H": {newInd("a", 1)}},
		}, "既不是 Base"},
		{"同周期上的指标重名", Config{
			InstID: "X", Base: "15m",
			Indicators: map[string][]Indicator{"15m": {newInd("dup", 1), newInd("dup", 1)}},
		}, "都叫"},
		{"聚合时周期不嵌套", Config{
			InstID: "X", Base: "4H", Extra: []string{"6H"}, Aggregate: true,
		}, "无法聚合"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewFeed(st, c.cfg)
			if err == nil {
				t.Fatalf("应当报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息里应当提到 %q，实为：%v", c.want, err)
			}
		})
	}
}

// TestMissingExtraSeriesSaysWhatToDo：辅周期没同步过是个常见错误，
// 错误信息得把出路说清楚，而不是丢一个 ErrNoSeries 了事。
func TestMissingExtraSeriesSaysWhatToDo(t *testing.T) {
	st := newMemStore()
	fill(t, st, "X", "15m", genBars(MustParsePeriod("15m"), t0, 100))

	_, err := NewFeed(st, Config{InstID: "X", Base: "15m", Extra: []string{"1H"}})
	if err == nil {
		t.Fatal("辅周期不在库里应当报错")
	}
	if !strings.Contains(err.Error(), "Aggregate") {
		t.Errorf("错误信息该提示可以改用 Aggregate，实为：%v", err)
	}
}

func TestInvalidViewIsQuietButNaN(t *testing.T) {
	f, err := NewFeed(nil, Config{InstID: "X", Base: "15m"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for _, v := range []View{f.View(), f.TF("没这个周期"), {}} {
		if v.Valid() {
			t.Error("空视图不该有效")
		}
		if v.Ts() != 0 {
			t.Errorf("无效视图的 Ts 应当是 0，实为 %d", v.Ts())
		}
		for name, got := range map[string]float64{
			"Open": v.Open(), "High": v.High(), "Low": v.Low(),
			"Close": v.Close(), "Vol": v.Vol(), "Ind": v.Ind("x"),
		} {
			if !math.IsNaN(got) {
				t.Errorf("无效视图的 %s 应当是 NaN，实为 %v", name, got)
			}
		}
		if v.Candle() != (Candle{}) {
			t.Error("无效视图的 Candle 应当是零值")
		}
	}
}

func TestFromToBounds(t *testing.T) {
	base := MustParsePeriod("1H")
	cs := genBars(base, t0, 100)
	st := newMemStore()
	fill(t, st, "X", "1H", cs)

	f, err := NewFeed(st, Config{InstID: "X", Base: "1H", From: cs[10].Ts, To: cs[20].Ts})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var got []int64
	for f.Next() {
		got = append(got, f.View().Ts())
	}
	if len(got) != 10 {
		t.Fatalf("走了 %d 步，期望 10（左闭右开）", len(got))
	}
	if got[0] != cs[10].Ts || got[9] != cs[19].Ts {
		t.Errorf("区间不对：%s .. %s", str(got[0]), str(got[9]))
	}
}
