package indicator

import (
	"testing"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// 包注释里说「窗口类指标每步在窗口上重算，O(n)，与 I/O 相比可以忽略」。
// 这里把它量出来，免得成为一句没人验证过的话。
//
//	go test ./indicator/ -bench . -benchmem

var sink []float64

func benchUpdate(b *testing.B, ind Indicator, cs []tickflow.Candle) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink = ind.Update(cs[i%len(cs)])
	}
}

func BenchmarkIndicator(b *testing.B) {
	cs := randomWalk(4096, 20260831)
	for _, c := range []struct {
		name string
		ind  Indicator
	}{
		{"MA/20", MA(20)},
		{"MA/200", MA(200)},
		{"EMA/20", EMA(20)},
		{"MACD/12_26_9", MACD(12, 26, 9)},
		{"KDJ/9_3_3/TV", KDJ(9, 3, 3)},
		{"KDJ/9_3_3/CN", KDJ(9, 3, 3, CN)},
		{"RSI/14", RSI(14)},
		{"CCI/20", CCI(20)},
		{"BOLL/20", BOLL(20, 2)},
	} {
		b.Run(c.name, func(b *testing.B) { benchUpdate(b, c.ind, cs) })
	}
}

// BenchmarkTypicalStack 是一套常见的指标组合，量的是「回测每走一步在指标上
// 花多少时间」。拿它和一根 K 线从存储里读出来的开销比，才知道值不值得为了
// 省这几十次浮点运算去换一个会漂移的增量实现。
func BenchmarkTypicalStack(b *testing.B) {
	cs := randomWalk(4096, 7)
	inds := []Indicator{
		MA(5), MA(20), MA(60),
		EMA(20),
		MACD(12, 26, 9),
		KDJ(9, 3, 3),
		RSI(14),
		CCI(20),
		BOLL(20, 2),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := cs[i%len(cs)]
		for _, ind := range inds {
			sink = ind.Update(c)
		}
	}
}
