package tickflow

import "testing"

// View 的注释说它「一个指针加一个下标，在栈上，回测每步取十几次值都不会有分配」。
// 这里把它量出来。
//
//	go test . -bench Feed -benchmem

var (
	sinkF float64
	sinkV View
)

func benchFeed(b *testing.B, extra []string, agg bool) (*Feed, []Candle) {
	b.Helper()
	base := MustParsePeriod("15m")
	cs := genBars(base, MustParsePeriod("1D").Truncate(t0), 4096)
	f, err := NewFeed(nil, Config{
		InstID: "X", Base: "15m", Extra: extra, Aggregate: agg, Lookback: 8,
		Indicators: map[string][]Indicator{
			"15m": {newInd("a", 1), newInd("b", 1, "x", "y", "z")},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	return f, cs
}

// BenchmarkFeedStep 量一步的总开销：辅周期推进 + 指标更新 + 环形缓冲写入。
func BenchmarkFeedStep(b *testing.B) {
	b.Run("单周期", func(b *testing.B) {
		f, cs := benchFeed(b, nil, false)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := f.step(cs[i%len(cs)]); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("三周期聚合", func(b *testing.B) {
		f, cs := benchFeed(b, []string{"1H", "1D"}, true)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := f.step(cs[i%len(cs)]); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkViewAccess 量取值的三条路：按名字查、按预解析句柄查、往回看。
func BenchmarkViewAccess(b *testing.B) {
	f, cs := benchFeed(b, nil, false)
	for i := 0; i < 64; i++ {
		if err := f.step(cs[i]); err != nil {
			b.Fatal(err)
		}
	}
	v := f.View()
	h, err := f.Handle("15m", "b.y")
	if err != nil {
		b.Fatal(err)
	}

	b.Run("Ind/按名字", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkF = v.Ind("b.y")
		}
	})
	b.Run("At/按句柄", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkF = v.At(h)
		}
	})
	b.Run("Prev(3).Close", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkF = v.Prev(3).Close()
		}
	})
	b.Run("View", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkV = f.View()
		}
	})
}
