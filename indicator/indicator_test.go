package indicator

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

const eps = 1e-9

func closes(vs ...float64) []tickflow.Candle {
	cs := make([]tickflow.Candle, len(vs))
	for i, v := range vs {
		cs[i] = tickflow.Candle{Ts: int64(i) * 60000, Open: v, High: v, Low: v, Close: v}
	}
	return cs
}

func hlc(rows ...[3]float64) []tickflow.Candle {
	cs := make([]tickflow.Candle, len(rows))
	for i, r := range rows {
		cs[i] = tickflow.Candle{Ts: int64(i) * 60000, High: r[0], Low: r[1], Close: r[2], Open: r[2]}
	}
	return cs
}

// col 取某一路输出的整列。
func col(t *testing.T, ind Indicator, cs []tickflow.Candle, k int) []float64 {
	t.Helper()
	rows := Compute(ind, cs)
	out := make([]float64, len(rows))
	for i, r := range rows {
		out[i] = r[k]
	}
	return out
}

func wantSeq(t *testing.T, name string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: 长度 %d，期望 %d", name, len(got), len(want))
	}
	for i := range want {
		if math.IsNaN(want[i]) {
			if !math.IsNaN(got[i]) {
				t.Errorf("%s[%d] = %v，期望 NaN", name, i, got[i])
			}
			continue
		}
		if math.IsNaN(got[i]) || math.Abs(got[i]-want[i]) > eps {
			t.Errorf("%s[%d] = %v，期望 %v", name, i, got[i], want[i])
		}
	}
}

var nan = math.NaN()

// TestHandComputed 用手算得出的值把每个指标的公式钉死。
// 数字都取得能心算验证，改公式时这里会第一个响。
func TestHandComputed(t *testing.T) {
	up := closes(1, 2, 3, 4, 5)

	t.Run("MA", func(t *testing.T) {
		wantSeq(t, "ma3", col(t, MA(3), up, 0), []float64{nan, nan, 2, 3, 4})
	})

	t.Run("EMA/TV", func(t *testing.T) {
		// α = 2/(3+1) = 0.5，用前 3 根的简单平均（= 2）播种。
		wantSeq(t, "ema3", col(t, EMA(3, TV), up, 0), []float64{nan, nan, 2, 3, 4})
	})

	t.Run("EMA/CN", func(t *testing.T) {
		// 同样 α = 0.5，但拿第一根收盘价（= 1）播种，从第一根就有值。
		wantSeq(t, "ema3", col(t, EMA(3, CN), up, 0),
			[]float64{1, 1.5, 2.25, 3.125, 4.0625})
	})

	t.Run("BOLL", func(t *testing.T) {
		sd := math.Sqrt(2.0 / 3.0) // 总体标准差：((1-2)²+0+(3-2)²)/3
		rows := Compute(BOLL(3, 2), up)
		for _, c := range []struct {
			i             int
			mid, upB, dnB float64
		}{
			{2, 2, 2 + 2*sd, 2 - 2*sd},
			{4, 4, 4 + 2*sd, 4 - 2*sd},
		} {
			got := rows[c.i]
			for k, w := range []float64{c.mid, c.upB, c.dnB} {
				if math.Abs(got[k]-w) > eps {
					t.Errorf("boll3[%d][%d] = %v，期望 %v", c.i, k, got[k], w)
				}
			}
		}
		if !math.IsNaN(rows[1][0]) {
			t.Errorf("boll3 在第 2 根应当还是 NaN")
		}
	})

	t.Run("RSI/TV", func(t *testing.T) {
		// 涨跌幅：+1, -1, +2。n=2，前两个涨跌幅的简单平均播种。
		// 播种后 均涨=0.5 均跌=0.5 → 50；再走一步 均涨=1.25 均跌=0.25 → 83.33
		wantSeq(t, "rsi2", col(t, RSI(2, TV), closes(10, 11, 10, 12), 0),
			[]float64{nan, nan, 50, 100 - 100.0/6})
	})

	t.Run("RSI/CN", func(t *testing.T) {
		// 首个涨跌幅播种：均涨=1 均跌=0 → 只有涨没有跌，按约定给 100。
		wantSeq(t, "rsi2", col(t, RSI(2, CN), closes(10, 11, 10, 12), 0),
			[]float64{nan, 100, 50, 100 - 100.0/6})
	})

	t.Run("CCI", func(t *testing.T) {
		// TP 依次为 1、2、3。均值 2，平均绝对偏差 2/3。
		// CCI = (3-2)/(0.015 × 2/3) = 100
		wantSeq(t, "cci3", col(t, CCI(3), closes(1, 2, 3), 0), []float64{nan, nan, 100})
	})

	t.Run("KDJ/CN", func(t *testing.T) {
		cs := hlc([3]float64{10, 8, 9}, [3]float64{12, 9, 11}, [3]float64{11, 7, 10}, [3]float64{11, 9, 11})
		rows := Compute(KDJ(3, 3, 3, CN), cs)
		// 第 3 根：窗口 hi=12 lo=7，RSV=(10-7)/5×100=60；K、D 首值播种都是 60。
		wantSeq(t, "k", []float64{rows[2][0], rows[2][1], rows[2][2]}, []float64{60, 60, 60})
		// 第 4 根：窗口 hi=12 lo=7，RSV=(11-7)/5×100=80
		// K = 60 + (80-60)/3 = 200/3；D = 60 + (200/3-60)/3 = 560/9；J = 3K-2D = 680/9
		wantSeq(t, "kdj4", []float64{rows[3][0], rows[3][1], rows[3][2]},
			[]float64{200.0 / 3, 560.0 / 9, 680.0 / 9})
	})

	t.Run("KDJ/TV", func(t *testing.T) {
		// m1=m2=1 时简单平均是恒等的，K=D=RSV，J=3K-2D=RSV。
		cs := hlc([3]float64{10, 8, 9}, [3]float64{12, 9, 11}, [3]float64{11, 7, 10})
		rows := Compute(KDJ(3, 1, 1, TV), cs)
		wantSeq(t, "kdj", []float64{rows[2][0], rows[2][1], rows[2][2]}, []float64{60, 60, 60})
	})
}

// TestMACDHistConvention 锁住 hist 的口径差异：TV 是 DIF-DEA，CN 是 2×(DIF-DEA)。
//
// 注意不能拿 TV 的 hist 直接和 CN 的 hist/2 比——两套口径【底下的三条 EMA
// 播种方式也不同】，早期的 dif、dea 本就不相等。乘不乘 2 只能在同一口径内验证。
func TestMACDHistConvention(t *testing.T) {
	cs := randomWalk(400, 42)
	for _, tc := range []struct {
		conv Convention
		mul  float64
	}{{TV, 1}, {CN, 2}} {
		for i, r := range Compute(MACD(12, 26, 9, tc.conv), cs) {
			if math.IsNaN(r[0]) {
				continue
			}
			if want := (r[0] - r[1]) * tc.mul; math.Abs(r[2]-want) > eps {
				t.Fatalf("%s 第 %d 根：hist = %v，期望 %v", tc.conv, i, r[2], want)
			}
		}
	}

	// 播种差异是【暂时】的。EMA(26) 的初值影响按 (1-2/27)^k 衰减，走上几百根
	//　之后两套口径的 dif 应当收敛到一起——剩下的差别就只有柱子的倍数了。
	// 这条不成立就说明播种以外还混进了别的分歧。
	tv := Compute(MACD(12, 26, 9, TV), cs)
	cn := Compute(MACD(12, 26, 9, CN), cs)
	last := len(cs) - 1
	if d := math.Abs(tv[last][0] - cn[last][0]); d > 1e-8 {
		t.Errorf("走满 %d 根后两套口径的 dif 仍差 %v，播种影响本该已衰减掉", len(cs), d)
	}
}

// TestWarmupIsExact 保证 Warmup() 说的就是实情：
// 前 Warmup()-1 根【全部】是 NaN，第 Warmup() 根【全部】不是。
//
// 这条不成立的话，使用者靠 Warmup 预留的历史长度就是错的，
// 而错的表现是开头几根拿到 NaN 或者拿到半成品——两种都不好查。
func TestWarmupIsExact(t *testing.T) {
	cs := randomWalk(400, 7)
	for _, mk := range allIndicators() {
		for _, conv := range []Convention{TV, CN} {
			ind := mk(conv)
			t.Run(ind.Name()+"/"+conv.String(), func(t *testing.T) {
				w := ind.Warmup()
				if w < 1 {
					t.Fatalf("Warmup() = %d，至少该是 1", w)
				}
				rows := Compute(ind, cs)
				for i := 0; i < w-1; i++ {
					for k, v := range rows[i] {
						if !math.IsNaN(v) {
							t.Fatalf("第 %d 根（warmup=%d）的第 %d 路是 %v，应当还是 NaN", i, w, k, v)
						}
					}
				}
				for k, v := range rows[w-1] {
					if math.IsNaN(v) {
						t.Fatalf("第 %d 根（warmup=%d）的第 %d 路仍是 NaN，Warmup 报小了", w-1, w, k)
					}
				}
			})
		}
	}
}

// TestFlatMarket 把「价格纹丝不动」这个退化场景下的每个分支都走一遍。
// 这些边界各家实现取值不同，本库的选择在各指标的文档里写着，这里钉死。
func TestFlatMarket(t *testing.T) {
	cs := closes(100, 100, 100, 100, 100, 100, 100, 100, 100, 100,
		100, 100, 100, 100, 100, 100, 100, 100, 100, 100)
	last := len(cs) - 1

	if got := Compute(MA(5), cs)[last][0]; math.Abs(got-100) > eps {
		t.Errorf("MA = %v，期望 100", got)
	}
	if got := Compute(EMA(5), cs)[last][0]; math.Abs(got-100) > eps {
		t.Errorf("EMA = %v，期望 100", got)
	}
	// 不涨不跌：均涨与均跌都是 0，按约定给 50 而不是 NaN。
	if got := Compute(RSI(5), cs)[last][0]; math.Abs(got-50) > eps {
		t.Errorf("RSI = %v，期望 50（不强不弱）", got)
	}
	// 没有偏离：平均绝对偏差为 0，给 0 而不是让除零产生 Inf。
	if got := Compute(CCI(5), cs)[last][0]; got != 0 {
		t.Errorf("CCI = %v，期望 0", got)
	}
	// 标准差为 0，三轨重合。
	b := Compute(BOLL(5, 2), cs)[last]
	if math.Abs(b[0]-100) > eps || b[1] != b[0] || b[2] != b[0] {
		t.Errorf("BOLL = %v，期望三轨都是 100", b)
	}
	// 区间高低相等，RSV 无定义，按约定取 50。
	for _, conv := range []Convention{TV, CN} {
		k := Compute(KDJ(5, 3, 3, conv), cs)[last]
		if math.Abs(k[0]-50) > eps || math.Abs(k[1]-50) > eps || math.Abs(k[2]-50) > eps {
			t.Errorf("KDJ/%s = %v，期望 K=D=J=50", conv, k)
		}
	}
	// MACD：快慢两条 EMA 相等，dif、dea、hist 全为 0。
	m := Compute(MACD(3, 5, 3), cs)[last]
	for i, v := range m {
		if math.Abs(v) > eps {
			t.Errorf("MACD 第 %d 路 = %v，期望 0", i, v)
		}
	}
}

// TestNaNNotZero 单独把「未就绪时给 NaN 而不是 0」拎出来。
// 给 0 的话，策略在开头几十根里会安静地拿一堆假值做决策。
func TestNaNNotZero(t *testing.T) {
	cs := randomWalk(50, 3)
	for _, mk := range allIndicators() {
		ind := mk(TV)
		if ind.Warmup() < 2 {
			continue
		}
		for k, v := range Compute(ind, cs)[0] {
			if v == 0 {
				t.Errorf("%s 第一根的第 %d 路给了 0，应当是 NaN", ind.Name(), k)
			}
			if !math.IsNaN(v) {
				t.Errorf("%s 第一根的第 %d 路 = %v，应当是 NaN", ind.Name(), k, v)
			}
		}
	}
}

func TestReset(t *testing.T) {
	cs := randomWalk(120, 11)
	for _, mk := range allIndicators() {
		ind := mk(TV)
		first := Compute(ind, cs)
		second := Compute(ind, cs) // Compute 内部会先 Reset
		if !reflect.DeepEqual(fmtRows(first), fmtRows(second)) {
			t.Errorf("%s: Reset 之后重算的结果与第一次不一致", ind.Name())
		}
	}
}

func TestNamingAndKeys(t *testing.T) {
	cases := []struct {
		ind  Indicator
		name string
		keys []string
	}{
		{MA(20), "ma20", []string{"ma20"}},
		{EMA(60), "ema60", []string{"ema60"}},
		{RSI(14), "rsi14", []string{"rsi14"}},
		{CCI(20), "cci20", []string{"cci20"}},
		{BOLL(20, 2), "boll20", []string{"boll20.mid", "boll20.up", "boll20.dn"}},
		{MACD(12, 26, 9), "macd", []string{"macd.dif", "macd.dea", "macd.hist"}},
		{KDJ(9, 3, 3), "kdj", []string{"kdj.k", "kdj.d", "kdj.j"}},
		{MA(20, Named("fast")), "fast", []string{"fast"}},
		{MACD(5, 35, 5, Named("slow")), "slow", []string{"slow.dif", "slow.dea", "slow.hist"}},
	}
	for _, c := range cases {
		if got := c.ind.Name(); got != c.name {
			t.Errorf("Name() = %q，期望 %q", got, c.name)
		}
		if got := Keys(c.ind); !reflect.DeepEqual(got, c.keys) {
			t.Errorf("%s 的 Keys() = %v，期望 %v", c.name, got, c.keys)
		}
	}
}

func TestConventionSelection(t *testing.T) {
	// 默认是 CN——OKX 自己的行情界面用的就是这一套，实测确认过。
	if got := MA(5).(*maIndicator).Convention(); got != CN {
		t.Errorf("默认口径应当是 CN，实为 %s", got)
	}
	if got := MA(5, TV).(*maIndicator).Convention(); got != TV {
		t.Errorf("显式传 TV 未生效")
	}

	SetDefaultConvention(TV)
	defer SetDefaultConvention(CN)
	if got := MA(5).(*maIndicator).Convention(); got != TV {
		t.Errorf("全局默认未生效")
	}
	// 显式传的要盖过全局默认。
	if got := MA(5, CN).(*maIndicator).Convention(); got != CN {
		t.Errorf("显式口径应当盖过全局默认")
	}
}

func TestBadParamsPanic(t *testing.T) {
	cases := map[string]func(){
		"MA(0)":        func() { MA(0) },
		"EMA(-1)":      func() { EMA(-1) },
		"RSI(0)":       func() { RSI(0) },
		"CCI(0)":       func() { CCI(0) },
		"BOLL(20,0)":   func() { BOLL(20, 0) },
		"MACD 快线不小于慢线": func() { MACD(26, 12, 9) },
		"KDJ(0,3,3)":   func() { KDJ(0, 3, 3) },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("非法参数应当 panic")
				}
			}()
			fn()
		})
	}
}

func TestComputeField(t *testing.T) {
	cs := randomWalk(80, 5)
	dif, err := ComputeField(MACD(12, 26, 9), cs, "dif")
	if err != nil {
		t.Fatal(err)
	}
	if len(dif) != len(cs) {
		t.Fatalf("长度 %d，期望 %d", len(dif), len(cs))
	}
	if _, err := ComputeField(MACD(12, 26, 9), cs, "没有这个字段"); err == nil {
		t.Error("未知字段应当报错")
	}
	// 单输出指标传空字段名即可。
	if _, err := ComputeField(MA(5), cs, ""); err != nil {
		t.Error(err)
	}
}

// ---------------------------------------------------------------- 辅助

// allIndicators 返回全部内置指标的构造函数，供跨指标的契约测试遍历。
func allIndicators() []func(Convention) Indicator {
	return []func(Convention) Indicator{
		func(c Convention) Indicator { return MA(20, c) },
		func(c Convention) Indicator { return EMA(20, c) },
		func(c Convention) Indicator { return MACD(12, 26, 9, c) },
		func(c Convention) Indicator { return KDJ(9, 3, 3, c) },
		func(c Convention) Indicator { return RSI(14, c) },
		func(c Convention) Indicator { return CCI(20, c) },
		func(c Convention) Indicator { return BOLL(20, 2, c) },
	}
}

// randomWalk 造一段确定性的随机游走行情，同一个 seed 永远给出同一串数。
func randomWalk(n int, seed uint64) []tickflow.Candle {
	cs := make([]tickflow.Candle, n)
	px := 100.0
	s := seed
	next := func() float64 { // xorshift64，不依赖 math/rand 的实现细节
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		return float64(s%20001)/10000 - 1 // [-1, 1]
	}
	for i := range cs {
		o := px
		px += next() * 2
		if px < 1 {
			px = 1
		}
		hi, lo := math.Max(o, px)+math.Abs(next()), math.Min(o, px)-math.Abs(next())
		if lo < 0.5 {
			lo = 0.5
		}
		cs[i] = tickflow.Candle{
			Ts: int64(i) * 60000, Open: o, High: hi, Low: lo, Close: px,
			Vol: 1000 + float64(i),
		}
	}
	return cs
}

// fmtRows 把 NaN 也纳入可比较的形式，供 DeepEqual 使用。
func fmtRows(rows [][]float64) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		var b strings.Builder
		for _, v := range r {
			if math.IsNaN(v) {
				b.WriteString("NaN;")
				continue
			}
			b.WriteString(strconv.FormatFloat(v, 'g', 17, 64))
			b.WriteByte(';')
		}
		out[i] = b.String()
	}
	return out
}
