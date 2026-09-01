package tickflow

import (
	"math"
	"strings"
	"testing"
)

// markOf 把一串行情造成对应的「标记价」序列：比成交价平稳，正是标记价的实际形态。
func markOf(cs []Candle, skip func(int) bool) []Candle {
	var out []Candle
	for i, c := range cs {
		if skip != nil && skip(i) {
			continue
		}
		mid := (c.High + c.Low) / 2
		out = append(out, Candle{Ts: c.Ts, Open: mid, High: mid + 0.5, Low: mid - 0.5, Close: mid})
	}
	return out
}

// TestMarkPxTracksBase 验证标记价跟着主周期【同一个 ts】走。
//
// 与辅周期不同：标记价 K 线与主周期这一根是同时收盘的，它的收盘价在这一刻
// 就已知，用它不构成未来函数。所以对齐规则是「同 ts」而不是「最后一根已收盘」。
func TestMarkPxTracksBase(t *testing.T) {
	base := MustParsePeriod("15m")
	cs := genBars(base, MustParsePeriod("1D").Truncate(t0), 200)

	st, mk := newMemStore(), newMemStore()
	fill(t, st, "X", "15m", cs)
	fill(t, mk, "X", "15m", markOf(cs, nil))

	f, err := NewFeed(st, FeedConfig{
		InstID: "X", Base: "15m", From: cs[0].Ts, Lookback: 3, MarkStore: mk,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var n int
	for f.Next() {
		v := f.View()
		m, ok := v.Mark()
		if !ok {
			t.Fatalf("第 %d 步没有标记价", n)
		}
		if m.Ts != v.Ts() {
			t.Fatalf("第 %d 步：标记价的 ts %d 与行情的 %d 对不上", n, m.Ts, v.Ts())
		}
		if want := (v.High() + v.Low()) / 2; v.MarkPx() != want {
			t.Fatalf("第 %d 步：MarkPx = %v，期望 %v", n, v.MarkPx(), want)
		}
		// 往回看也该带着标记价。
		if n >= 2 {
			pm, ok := v.Prev(2).Mark()
			if !ok || pm.Ts != cs[n-2].Ts {
				t.Fatalf("第 %d 步：Prev(2) 的标记价对不上", n)
			}
		}
		n++
	}
	if err := f.Err(); err != nil {
		t.Fatal(err)
	}
	if n != len(cs) {
		t.Fatalf("走了 %d 步，期望 %d", n, len(cs))
	}
}

// TestMarkPxMissingGivesNaN：标记价序列缺根是真实存在的，缺了要给 NaN 不是 0。
// 0 是个看起来正常的价格，会让强平判据悄悄用一个假值。
func TestMarkPxMissingGivesNaN(t *testing.T) {
	base := MustParsePeriod("15m")
	cs := genBars(base, MustParsePeriod("1D").Truncate(t0), 60)

	st, mk := newMemStore(), newMemStore()
	fill(t, st, "X", "15m", cs)
	// 挖掉中间一段标记价。
	fill(t, mk, "X", "15m", markOf(cs, func(i int) bool { return i >= 20 && i < 30 }))

	f, err := NewFeed(st, FeedConfig{
		InstID: "X", Base: "15m", From: cs[0].Ts, MarkStore: mk,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var missing int
	for i := 0; f.Next(); i++ {
		v := f.View()
		_, ok := v.Mark()
		hole := i >= 20 && i < 30
		if hole {
			if ok {
				t.Fatalf("第 %d 步本该没有标记价", i)
			}
			if !math.IsNaN(v.MarkPx()) {
				t.Fatalf("第 %d 步缺标记价时 MarkPx = %v，应当是 NaN", i, v.MarkPx())
			}
			missing++
			continue
		}
		if !ok || math.IsNaN(v.MarkPx()) {
			t.Fatalf("第 %d 步应当有标记价", i)
		}
	}
	if missing != 10 {
		t.Fatalf("缺了 %d 步，期望 10", missing)
	}
}

// TestMarkPxSurvivesBaseGap：主周期有空洞时，标记价不能错位到别的时刻上去。
// 错位一根的标记价比没有标记价更糟——它看起来是对的。
func TestMarkPxSurvivesBaseGap(t *testing.T) {
	base := MustParsePeriod("15m")
	full := genBars(base, MustParsePeriod("1D").Truncate(t0), 60)

	var holed []Candle
	for i, c := range full {
		if i >= 15 && i < 25 {
			continue // 主周期缺一段，标记价那边是全的
		}
		holed = append(holed, c)
	}

	st, mk := newMemStore(), newMemStore()
	fill(t, st, "X", "15m", holed)
	fill(t, mk, "X", "15m", markOf(full, nil))

	f, err := NewFeed(st, FeedConfig{InstID: "X", Base: "15m", From: holed[0].Ts, MarkStore: mk})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for f.Next() {
		v := f.View()
		m, ok := v.Mark()
		if !ok {
			t.Fatalf("%s 处没有标记价", str(v.Ts()))
		}
		if m.Ts != v.Ts() {
			t.Fatalf("空洞之后标记价错位了：行情 %s，标记价 %s", str(v.Ts()), str(m.Ts))
		}
	}
	if err := f.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestNoMarkStoreMeansNaN：没配 MarkStore 时不该凭空造出标记价。
func TestNoMarkStoreMeansNaN(t *testing.T) {
	base := MustParsePeriod("15m")
	cs := genBars(base, MustParsePeriod("1D").Truncate(t0), 20)
	st := newMemStore()
	fill(t, st, "X", "15m", cs)

	f, err := NewFeed(st, FeedConfig{InstID: "X", Base: "15m", From: cs[0].Ts})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !f.Next() {
		t.Fatal(f.Err())
	}
	v := f.View()
	if _, ok := v.Mark(); ok {
		t.Error("没配 MarkStore 却给出了标记价")
	}
	if !math.IsNaN(v.MarkPx()) {
		t.Errorf("MarkPx = %v，应当是 NaN", v.MarkPx())
	}
}

func TestPushWithMark(t *testing.T) {
	base := MustParsePeriod("15m")
	cs := genBars(base, MustParsePeriod("1D").Truncate(t0), 5)
	marks := markOf(cs, nil)

	f, err := NewFeed(nil, FeedConfig{
		InstID: "X", Base: "15m", Lookback: 2, MarkStore: newMemStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for i, c := range cs {
		if err := f.PushWithMark("15m", c, marks[i]); err != nil {
			t.Fatal(err)
		}
		if got := f.View().MarkPx(); got != marks[i].Close {
			t.Fatalf("第 %d 步 MarkPx = %v，期望 %v", i, got, marks[i].Close)
		}
	}

	// ts 对不上要报错：那多半是取错了根，静默接受会让强平用错价格。
	bad := marks[0]
	bad.Ts += 1
	err = f.PushWithMark("15m", genBars(base, cs[4].Ts, 2)[1], bad)
	if err == nil || !strings.Contains(err.Error(), "对不上") {
		t.Errorf("ts 不一致应当报错，实为 %v", err)
	}
	// 标记价只对主周期生效。
	if err := f.PushWithMark("1H", cs[0], marks[0]); err == nil {
		t.Error("推给辅周期应当报错")
	}
}

// TestMissingMarkSeriesSaysWhatToDo：标记价没同步过是个新错误，话要说明白。
func TestMissingMarkSeriesSaysWhatToDo(t *testing.T) {
	base := MustParsePeriod("15m")
	cs := genBars(base, MustParsePeriod("1D").Truncate(t0), 20)
	st := newMemStore()
	fill(t, st, "X", "15m", cs)

	_, err := NewFeed(st, FeedConfig{InstID: "X", Base: "15m", MarkStore: newMemStore()})
	if err == nil {
		t.Fatal("MarkStore 里没有该序列时应当报错")
	}
	for _, want := range []string{"标记价", "MarkPrice", "Mark"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息该提到 %q，实为：%v", want, err)
		}
	}
}
