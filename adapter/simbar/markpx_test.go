package simbar

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
	"github.com/dream-until-dawn/okx-tickflow-go/store/segfile"
	"github.com/shopspring/decimal"
)

// 本文件盯住整条链上最贵的一处退化：标记价。
//
// okx-position-simulator-go 的 Advance 在 Bar.MarkPx 为空时会退回用最新成交价
// 顶替。强平判据本该看标记价，用成交价会让【影线扫掉本不该爆的仓位】——对尾部
// 风险就是强平的策略（比如做多网格）尤其致命，而且是假阴性，结果里不留痕迹。
//
// 数据是真的：500 根 ETH-USDT-SWAP 日线，成交价与标记价各一份，都从 OKX 拉的。

func loadJSON(t *testing.T, path string) []tickflow.Candle {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cs []tickflow.Candle
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

// TestMarkPxReachesSimulator 走完整条链，并把结论落到一个能验的点上：
// 记账内核收到的标记价与最新价【确实不同】。相同就说明退化还在。
func TestMarkPxReachesSimulator(t *testing.T) {
	trades := loadJSON(t, "testdata/eth-usdt-swap-1d.json")
	marks := loadJSON(t, "testdata/eth-usdt-swap-1d-mark.json")
	if len(trades) == 0 || len(marks) == 0 {
		t.Fatal("基线数据是空的")
	}

	root := t.TempDir()
	st, err := segfile.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Append(inst2, "1D", trades); err != nil {
		t.Fatal(err)
	}
	mkst, err := segfile.Open(root, segfile.Mark)
	if err != nil {
		t.Fatal(err)
	}
	defer mkst.Close()
	if err := mkst.Append(inst2, "1D", marks); err != nil {
		t.Fatal(err)
	}

	feed, err := tickflow.NewFeed(st, tickflow.FeedConfig{
		InstID: inst2, Base: "1D", MarkStore: mkst,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer feed.Close()

	// RequireMarkPx 打开：缺标记价就报错，不再静默退回最新价。
	// 这正是网格引擎打算从第一天就开着的那个开关。
	sim, err := okxsim.New(okxsim.Config{
		PosMode: types.NetMode, RefData: refdata.MustEmbedded(),
		DefaultLever: decimal.NewFromInt(5), RequireMarkPx: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.Deposit("USDT", decimal.NewFromInt(50000)); err != nil {
		t.Fatal(err)
	}

	var steps, differed int
	for feed.Next() {
		v := feed.View()
		px := v.MarkPx()
		if math.IsNaN(px) {
			t.Fatalf("%d 处没有标记价，而两条序列本该是齐的", v.Ts())
		}
		if _, err := Advance(sim, inst2, v, WithMarkPx(px)); err != nil {
			t.Fatalf("第 %d 步推进失败（RequireMarkPx 开着）：%v", steps, err)
		}
		steps++
		if px != v.Close() {
			differed++
		}
	}
	if err := feed.Err(); err != nil {
		t.Fatal(err)
	}
	if steps < 400 {
		t.Fatalf("只走了 %d 步", steps)
	}
	// 标记价与最新价是两条独立的序列，绝大多数时刻都不相等。
	// 若几乎处处相等，多半是标记价没真的传进去。
	if differed*2 < steps {
		t.Fatalf("%d/%d 步的标记价与最新价相同——标记价多半没真的进来",
			steps-differed, steps)
	}
	t.Logf("走了 %d 步，其中 %d 步标记价与最新价不同", steps, differed)
}

// TestRequireMarkPxCatchesDegradation 反过来验一遍：不给标记价时，
// 打开 RequireMarkPx 的记账内核必须【报错】而不是悄悄退回最新价。
//
// 没有这一条，上面那个测试只说明「给了标记价能跑通」，说明不了
// 「不给会被拦住」——而后者才是那个开关的全部价值。
func TestRequireMarkPxCatchesDegradation(t *testing.T) {
	sim, err := okxsim.New(okxsim.Config{
		PosMode: types.NetMode, RefData: refdata.MustEmbedded(),
		DefaultLever: decimal.NewFromInt(5), RequireMarkPx: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.Deposit("USDT", decimal.NewFromInt(10000)); err != nil {
		t.Fatal(err)
	}
	c := loadJSON(t, "testdata/eth-usdt-swap-1d.json")[0]

	// 不给标记价。
	b, err := ToBar(inst2, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sim.Advance(b); err == nil {
		t.Fatal("RequireMarkPx 开着却接受了没有标记价的 Bar")
	}

	// NaN 等同于不给——View.MarkPx 在标记价缺根时返回的正是 NaN。
	b, err = ToBar(inst2, c, WithMarkPx(math.NaN()))
	if err != nil {
		t.Fatal(err)
	}
	if !b.MarkPx.IsZero() {
		t.Errorf("NaN 的标记价应当等同于不设，实为 %s", b.MarkPx)
	}
	if _, err := sim.Advance(b); err == nil {
		t.Fatal("NaN 标记价本该等同于缺失，却被接受了")
	}

	// 给了就该过。
	b, err = ToBar(inst2, c, WithMarkPx(c.Close*1.001))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sim.Advance(b); err != nil {
		t.Fatalf("给了标记价却推进失败：%v", err)
	}
}

const inst2 = "ETH-USDT-SWAP"
