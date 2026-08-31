package indicator

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

var update = flag.Bool("update", false, "重新生成 testdata/golden.csv")

// golden 测试用【真实的 OKX 行情】把每个指标每一路的输出锁成基线。
//
// 与 reference_test.go 的分工：那边证明公式对（拿独立写的朴素实现对照），
// 这边防止公式在重构中被无意改动，且用的是带真实缺口、跳空与极端波动的数据，
// 而不是随机游走。两者缺一：只有参考实现，改公式时两边一起改就发现不了；
// 只有 golden，第一次就算错的话会把错的值锁进去。
//
// testdata/candles.json 是 BTC-USDT-SWAP 的 300 根 15m K 线，同步自实盘。
// 测试本身不联网。改动指标后若确认新值是对的：
//
//	go test ./indicator/ -run TestGolden -update
const (
	goldenCandles = "testdata/candles.json"
	goldenFile    = "testdata/golden.csv"
)

// goldenSet 是被锁住的指标集合。参数取常用值，两套口径都算。
func goldenSet() []Indicator {
	var out []Indicator
	for _, conv := range []Convention{TV, CN} {
		suffix := "_" + strings.ToLower(conv.String())
		out = append(out,
			MA(20, conv, Named("ma20"+suffix)),
			EMA(20, conv, Named("ema20"+suffix)),
			MACD(12, 26, 9, conv, Named("macd"+suffix)),
			KDJ(9, 3, 3, conv, Named("kdj"+suffix)),
			RSI(14, conv, Named("rsi14"+suffix)),
			CCI(20, conv, Named("cci20"+suffix)),
			BOLL(20, 2, conv, Named("boll20"+suffix)),
		)
	}
	return out
}

func loadGoldenCandles(t *testing.T) []tickflow.Candle {
	t.Helper()
	raw, err := os.ReadFile(goldenCandles)
	if err != nil {
		t.Fatal(err)
	}
	var cs []tickflow.Candle
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	if len(cs) < 100 {
		t.Fatalf("基线数据只有 %d 根，太少", len(cs))
	}
	return cs
}

func TestGolden(t *testing.T) {
	cs := loadGoldenCandles(t)

	inds := goldenSet()
	header := []string{"ts"}
	cols := make([][]float64, 0, 32)
	for _, ind := range inds {
		rows := Compute(ind, cs)
		for k, key := range Keys(ind) {
			header = append(header, key)
			col := make([]float64, len(cs))
			for i := range cs {
				col[i] = rows[i][k]
			}
			cols = append(cols, col)
		}
	}

	if *update {
		writeGolden(t, header, cs, cols)
		t.Logf("已重新生成 %s：%d 根 × %d 列", goldenFile, len(cs), len(cols))
		return
	}

	wantHeader, wantRows := readGolden(t)
	if strings.Join(header, ",") != strings.Join(wantHeader, ",") {
		t.Fatalf("列集合变了：\n现在 %v\n基线 %v\n确认无误后用 -update 重新生成",
			header, wantHeader)
	}
	if len(wantRows) != len(cs) {
		t.Fatalf("基线有 %d 行，数据有 %d 根", len(wantRows), len(cs))
	}

	for i := range cs {
		if got, want := cs[i].Ts, int64(wantRows[i][0]); got != want {
			t.Fatalf("第 %d 行的时间戳 %d 与基线 %d 对不上——基线数据被换过了", i, got, want)
		}
		for k, col := range cols {
			got, want := col[i], wantRows[i][k+1]
			if math.IsNaN(want) {
				if !math.IsNaN(got) {
					t.Errorf("%s[%d] = %v，基线是 NaN", header[k+1], i, got)
				}
				continue
			}
			if math.IsNaN(got) {
				t.Errorf("%s[%d] = NaN，基线是 %v", header[k+1], i, want)
				continue
			}
			// 基线按 12 位有效数字存，容差放在这个精度之下。
			if tol := 1e-10 * math.Max(1, math.Abs(want)); math.Abs(got-want) > tol {
				t.Errorf("%s[%d] = %.12g，基线 %.12g，差 %g", header[k+1], i, got, want, got-want)
			}
		}
	}
}

func writeGolden(t *testing.T, header []string, cs []tickflow.Candle, cols [][]float64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(goldenFile), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	w.UseCRLF = false
	if err := w.Write(header); err != nil {
		t.Fatal(err)
	}
	rec := make([]string, len(header))
	for i := range cs {
		rec[0] = strconv.FormatInt(cs[i].Ts, 10)
		for k, col := range cols {
			if math.IsNaN(col[i]) {
				rec[k+1] = "NaN"
				continue
			}
			rec[k+1] = strconv.FormatFloat(col[i], 'g', 12, 64)
		}
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
}

func readGolden(t *testing.T) ([]string, [][]float64) {
	t.Helper()
	f, err := os.Open(goldenFile)
	if err != nil {
		t.Fatalf("%v；首次生成请跑 go test ./indicator/ -run TestGolden -update", err)
	}
	defer f.Close()

	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 2 {
		t.Fatal("基线文件是空的")
	}
	rows := make([][]float64, len(recs)-1)
	for i, r := range recs[1:] {
		row := make([]float64, len(r))
		for k, s := range r {
			if s == "NaN" {
				row[k] = math.NaN()
				continue
			}
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				t.Fatalf("第 %d 行第 %d 列 %q 解析失败: %v", i+1, k, s, err)
			}
			row[k] = v
		}
		rows[i] = row
	}
	return recs[0], rows
}

// TestGoldenDataIsReal 挡住「基线数据被换成合成数据」这种悄悄的退化。
// golden 的价值就在于数据是真的——有跳空、有极端波动、有真实的价格分布。
func TestGoldenDataIsReal(t *testing.T) {
	cs := loadGoldenCandles(t)
	p := tickflow.MustParsePeriod("15m")

	for i, c := range cs {
		if p.Truncate(c.Ts) != c.Ts {
			t.Fatalf("第 %d 根的时间戳 %d 没落在 15m 网格上", i, c.Ts)
		}
		if i > 0 && c.Ts != p.Next(cs[i-1].Ts) {
			t.Fatalf("第 %d 根与前一根不连续：%d 之后是 %d", i, cs[i-1].Ts, c.Ts)
		}
		if c.High < c.Low || c.Close <= 0 || c.Open <= 0 {
			t.Fatalf("第 %d 根的 OHLC 不合理：%+v", i, c)
		}
		if c.Close > c.High || c.Close < c.Low || c.Open > c.High || c.Open < c.Low {
			t.Fatalf("第 %d 根的开收价跑出了高低区间：%+v", i, c)
		}
	}

	// 合成数据往往整齐得过分。真实行情里高低价几乎不会长期相等。
	var flat int
	for _, c := range cs {
		if c.High == c.Low {
			flat++
		}
	}
	if flat*4 > len(cs) {
		t.Errorf("%d/%d 根的最高价等于最低价，这不像真实行情", flat, len(cs))
	}
}
