package indicator

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// 本文件锁住一件【外部验证过】的事：OKX 自己的行情界面用的是 CN 口径。
//
// 2026-09-01，拿 ETH-USDT-SWAP 的日线在 OKX 平台上逐行比对了最近 10 根的
// KDJ(9,3,3) 与 MACD(12,26,9)：CN 口径对得上，TV 口径对不上。本库的默认口径
// 因此定为 CN——这是给 OKX 用的库，默认就该跟 OKX 一致。
//
// 下面这组数就是当时比对的那十行。它把一次人工核对变成了永久的守卫：
// 谁要是改动了 CN 那条计算路径，或者把默认口径改回 TV，这里会先炸。
//
// 数据是完整的 500 根日线（testdata/eth-usdt-swap-1d.json）——CN 口径首值播种，
// 递归平均要走够长才收敛，截短了重算就对不上这组值了。

// okxVerified 是经 OKX 平台核对的基准值。
// 字段顺序：ts, K, D, J, DIF, DEA, 柱。
var okxVerified = [][7]float64{
	{1787328000000, 80.43, 71.99, 97.30, 106.93, 49.79, 114.27},  // 港时 2026-08-22
	{1787414400000, 81.50, 75.16, 94.18, 125.66, 64.97, 121.39},  // 港时 2026-08-23
	{1787500800000, 84.62, 78.31, 97.22, 142.81, 80.54, 124.55},  // 港时 2026-08-24
	{1787587200000, 86.43, 81.02, 97.25, 154.19, 95.27, 117.85},  // 港时 2026-08-25
	{1787673600000, 85.91, 82.65, 92.44, 158.75, 107.96, 101.57}, // 港时 2026-08-26
	{1787760000000, 88.30, 84.53, 95.83, 166.22, 119.62, 93.20},  // 港时 2026-08-27
	{1787846400000, 85.84, 84.97, 87.58, 166.21, 128.94, 74.56},  // 港时 2026-08-28
	{1787932800000, 74.90, 81.61, 61.47, 162.14, 135.58, 53.13},  // 港时 2026-08-29
	{1788019200000, 69.60, 77.61, 53.58, 159.99, 140.46, 39.06},  // 港时 2026-08-30
	{1788105600000, 63.99, 73.07, 45.84, 155.42, 143.45, 23.94},  // 港时 2026-08-31
}

func loadETHDaily(t *testing.T) []tickflow.Candle {
	t.Helper()
	raw, err := os.ReadFile("testdata/eth-usdt-swap-1d.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs []tickflow.Candle
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	if len(cs) < 400 {
		t.Fatalf("基线只有 %d 根；CN 口径首值播种，截短了递归平均来不及收敛，"+
			"这组基准值就不再成立", len(cs))
	}
	return cs
}

// TestMatchesOKXPlatform 把与 OKX 平台核对过的那十行钉死。
func TestMatchesOKXPlatform(t *testing.T) {
	cs := loadETHDaily(t)
	kdj := Compute(KDJ(9, 3, 3, CN), cs)
	macd := Compute(MACD(12, 26, 9, CN), cs)

	off := len(cs) - len(okxVerified)
	hk := time.FixedZone("HKT", 8*3600)

	for i, want := range okxVerified {
		c := cs[off+i]
		day := time.UnixMilli(c.Ts).In(hk).Format("2006-01-02")
		if c.Ts != int64(want[0]) {
			t.Fatalf("第 %d 行的时间戳是 %d，基准是 %.0f——基线数据被换过了",
				i, c.Ts, want[0])
		}
		got := []float64{
			kdj[off+i][0], kdj[off+i][1], kdj[off+i][2],
			macd[off+i][0], macd[off+i][1], macd[off+i][2],
		}
		for k, name := range []string{"K", "D", "J", "DIF", "DEA", "柱"} {
			// 基准值按两位小数记的，容差取半个末位。
			if math.Abs(got[k]-want[k+1]) > 0.005 {
				t.Errorf("港时 %s 的 %s = %.4f，与 OKX 核对过的 %.2f 不符",
					day, name, got[k], want[k+1])
			}
		}
	}
}

// TestTVWouldNotMatchOKX 反过来验一遍：TV 口径【对不上】OKX。
//
// 没有这一条的话，上面那个测试只说明「CN 算出来是这些数」，说明不了
// 「为什么默认要选 CN」。两条合起来才是完整的理由。
func TestTVWouldNotMatchOKX(t *testing.T) {
	cs := loadETHDaily(t)
	kdj := Compute(KDJ(9, 3, 3, TV), cs)
	macd := Compute(MACD(12, 26, 9, TV), cs)
	off := len(cs) - len(okxVerified)

	var kdjDiff, histDiff int
	for i, want := range okxVerified {
		if math.Abs(kdj[off+i][0]-want[1]) > 0.005 {
			kdjDiff++
		}
		if math.Abs(macd[off+i][2]-want[6]) > 0.005 {
			histDiff++
		}
	}
	if kdjDiff == 0 {
		t.Error("TV 口径的 K 全部对上了 OKX——那两套口径就没有区别，" +
			"要么是 KDJ 的实现塌成了一套，要么这组基准值不该用来区分口径")
	}
	if histDiff == 0 {
		t.Error("TV 口径的 MACD 柱全部对上了 OKX——CN 的柱本该是 TV 的两倍")
	}

	// MACD 的 DIF / DEA 两套口径应当已经收敛到一起（500 根足够），
	// 差异只剩柱子的倍数。这条顺带印证「播种差异是暂时的」。
	for i, want := range okxVerified {
		if math.Abs(macd[off+i][0]-want[4]) > 0.005 {
			t.Errorf("第 %d 行：走了 500 根之后两套口径的 DIF 仍差得出来（TV %.2f vs CN %.2f）",
				i, macd[off+i][0], want[4])
		}
		if got, want2 := macd[off+i][2]*2, want[6]; math.Abs(got-want2) > 0.01 {
			t.Errorf("第 %d 行：TV 柱 ×2 = %.2f，期望等于 CN 柱 %.2f", i, got, want2)
		}
	}
}

// TestDefaultIsWhatOKXUses 保证「不传口径」拿到的就是与 OKX 一致的那套。
// 使用者不会先读文档才开始算指标。
func TestDefaultIsWhatOKXUses(t *testing.T) {
	if DefaultConvention() != CN {
		t.Fatalf("默认口径是 %s，应当是 CN——OKX 平台用的就是这一套", DefaultConvention())
	}
	cs := loadETHDaily(t)
	def := Compute(MACD(12, 26, 9), cs) // 不传口径
	off := len(cs) - len(okxVerified)
	for i, want := range okxVerified {
		if math.Abs(def[off+i][2]-want[6]) > 0.005 {
			t.Fatalf("第 %d 行：默认口径算出的柱 %.2f，与 OKX 核对过的 %.2f 不符",
				i, def[off+i][2], want[6])
		}
	}
}
