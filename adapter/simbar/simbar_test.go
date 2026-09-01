package simbar

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"

	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
	"github.com/shopspring/decimal"
)

func loadCandles(t *testing.T) []tickflow.Candle {
	t.Helper()
	raw, err := os.ReadFile("testdata/candles.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs []tickflow.Candle
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	if len(cs) == 0 {
		t.Fatal("基线数据是空的")
	}
	return cs
}

// TestFloat64ToDecimalIsLossless 是本包最要紧的一条断言：
// 行情层的 float64 转成记账层的 decimal 之后【逐位相同】，再转回来还是同一个数。
//
// 这条不成立的话，整个「行情用 float64、记账用 decimal」的分工就站不住了——
// 而误差会以极其隐蔽的方式出现在盈亏上。
func TestFloat64ToDecimalIsLossless(t *testing.T) {
	check := func(t *testing.T, f float64) {
		t.Helper()
		d := Dec(f)

		// 往返：decimal 转回 float64 必须是同一个数（按位相等，不是近似）。
		if back := d.InexactFloat64(); back != f {
			t.Errorf("%v 转 decimal 再转回来成了 %v", f, back)
		}
		// 逐位：decimal 的十进制形态与 float64 的最短往返表示一致，
		// 既没有多出来的尾数，也没有丢掉的位。
		if got, want := d.String(), strconv.FormatFloat(f, 'f', -1, 64); got != want {
			t.Errorf("%v 转成的 decimal 是 %s，期望 %s", f, got, want)
		}
	}

	t.Run("真实行情", func(t *testing.T) {
		for _, c := range loadCandles(t) {
			for _, f := range []float64{c.Open, c.High, c.Low, c.Close, c.Vol, c.VolCcy, c.VolCcyQuote} {
				check(t, f)
			}
		}
	})

	// OKX 上的刻度跨了很多个数量级：BTC 是 0.1，小币种能到 1e-8。
	t.Run("各量级的刻度", func(t *testing.T) {
		for _, base := range []float64{
			0.00000001, 0.0000123, 0.001, 0.015, 1, 3.14159, 78123.4, 99999.99,
			1234567.89, 0.1, 0.2, 0.3, // 0.1+0.2 那类二进制表示不精确的经典值
		} {
			for i := 0; i < 40; i++ {
				check(t, base*float64(i+1))
			}
		}
	})

	t.Run("边界", func(t *testing.T) {
		for _, f := range []float64{
			math.SmallestNonzeroFloat64, math.MaxFloat64,
			1e-300, 1e300, 1.0 / 3.0, 2.0 / 3.0,
		} {
			if back := Dec(f).InexactFloat64(); back != f {
				t.Errorf("%v 往返之后成了 %v", f, back)
			}
		}
	})
}

// TestDecHandlesNaNWithoutPanic：shopspring 的 NewFromFloat 碰上 NaN 会 panic，
// 而回测循环里最不该出现的就是从库深处炸出来的 panic。
func TestDecHandlesNaNWithoutPanic(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := Dec(f); !got.IsZero() {
			t.Errorf("Dec(%v) = %s，期望零值", f, got)
		}
	}
}

func candle() tickflow.Candle {
	return tickflow.Candle{
		Ts: 1767225600000, Open: 78000.1, High: 78650, Low: 77940.5, Close: 78481.4,
		Vol: 293000.88,
	}
}

func TestToBarMapping(t *testing.T) {
	c := candle()
	b, err := ToBar("BTC-USDT-SWAP", c)
	if err != nil {
		t.Fatal(err)
	}
	if b.InstID != "BTC-USDT-SWAP" || b.Ts != c.Ts {
		t.Errorf("instId / ts 没传对：%+v", b)
	}
	for _, tc := range []struct {
		name string
		got  decimal.Decimal
		want float64
	}{
		{"High", b.High, c.High},
		{"Low", b.Low, c.Low},
		// Last 取收盘价——记账内核用它撮合限价单、算最新价。
		{"Last", b.Last, c.Close},
	} {
		if !tc.got.Equal(Dec(tc.want)) {
			t.Errorf("%s = %s，期望 %v", tc.name, tc.got, tc.want)
		}
	}
	// 标记价与指数价默认留空：本库还拿不到这两条独立序列，
	// 与其拿别的价格顶替，不如留给记账内核自己决定怎么退让。
	if !b.MarkPx.IsZero() || !b.IdxPx.IsZero() {
		t.Errorf("MarkPx / IdxPx 默认应当留空，实为 %s / %s", b.MarkPx, b.IdxPx)
	}
}

// TestFundingIsNeverSet 把「不计资金费」这个刻意的取舍锁成测试。
// 它不是遗漏，改动前请先读包注释里的理由。
func TestFundingIsNeverSet(t *testing.T) {
	b, err := ToBar("BTC-USDT-SWAP", candle(), WithMarkPx(78000), WithIdxPx(78010))
	if err != nil {
		t.Fatal(err)
	}
	if b.Funding != nil {
		t.Fatal("ToBar 永远不该设置 Funding——部分区间有、更早没有，比全都没有更糟")
	}
}

func TestOptions(t *testing.T) {
	b, err := ToBar("BTC-USDT-SWAP", candle(), WithMarkPx(78123.4), WithIdxPx(78125.6))
	if err != nil {
		t.Fatal(err)
	}
	if !b.MarkPx.Equal(Dec(78123.4)) {
		t.Errorf("MarkPx = %s", b.MarkPx)
	}
	if !b.IdxPx.Equal(Dec(78125.6)) {
		t.Errorf("IdxPx = %s", b.IdxPx)
	}
}

// TestToBarRejectsBadInput：非法价格要在这里被拦住。放进记账内核之后再炸，
// 出错点离病因就隔了十万八千里。
func TestToBarRejectsBadInput(t *testing.T) {
	ok := candle()
	cases := []struct {
		name   string
		inst   string
		c      tickflow.Candle
		expect string
	}{
		{"空 instId", "", ok, "instId"},
		{"ts 为零", "X", func() tickflow.Candle { c := ok; c.Ts = 0; return c }(), "ts"},
		{"收盘价是 NaN", "X", func() tickflow.Candle { c := ok; c.Close = math.NaN(); return c }(), "View"},
		{"最高价是 Inf", "X", func() tickflow.Candle { c := ok; c.High = math.Inf(1); return c }(), "high"},
		{"最低价为零", "X", func() tickflow.Candle { c := ok; c.Low = 0; return c }(), "正数"},
		{"收盘价为负", "X", func() tickflow.Candle { c := ok; c.Close = -1; return c }(), "正数"},
		{"高低倒置", "X", func() tickflow.Candle { c := ok; c.High, c.Low = c.Low, c.High; return c }(), "低于最低价"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ToBar(tc.inst, tc.c)
			if err == nil {
				t.Fatal("应当报错")
			}
			if !contains(err.Error(), tc.expect) {
				t.Errorf("错误信息里应当提到 %q，实为：%v", tc.expect, err)
			}
		})
	}
}

func TestToBars(t *testing.T) {
	cs := loadCandles(t)
	bars, err := ToBars("BTC-USDT-SWAP", cs)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != len(cs) {
		t.Fatalf("转出 %d 根，期望 %d", len(bars), len(cs))
	}
	if !bars[0].Last.Equal(Dec(cs[0].Close)) {
		t.Error("首根的 Last 不对")
	}

	// 有一根坏的就整批失败，并指明是第几根——半批数据喂进去之后再报错，
	// 记账内核的状态就没法收拾了。
	bad := append([]tickflow.Candle(nil), cs...)
	bad[7].Close = math.NaN()
	if _, err := ToBars("X", bad); err == nil {
		t.Error("含坏数据的批次应当整批失败")
	} else if !contains(err.Error(), "第 7 根") {
		t.Errorf("错误信息该指明是第几根，实为：%v", err)
	}
}

func TestAdvanceRejectsBadArgs(t *testing.T) {
	if _, err := Advance(nil, "X", tickflow.View{}); err == nil {
		t.Error("nil simulator 应当报错")
	}
	sim := newSim(t)
	if _, err := Advance(sim, "X", tickflow.View{}); err == nil {
		t.Error("无效视图应当报错")
	} else if !contains(err.Error(), "Lookback") {
		t.Errorf("错误信息该点出无效视图的常见来由，实为：%v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func newSim(t *testing.T) *okxsim.Simulator {
	t.Helper()
	sim, err := okxsim.New(okxsim.Config{
		PosMode:      types.NetMode,
		RefData:      refdata.MustEmbedded(), // 内置快照，回测要的就是不可变
		DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	return sim
}
