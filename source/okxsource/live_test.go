package okxsource_test

import (
	"context"
	"os"
	"testing"
	"time"

	okx "github.com/dream-until-dawn/okx-api-v5-go"
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
	"github.com/dream-until-dawn/okx-tickflow-go/source/okxsource"
)

// 打真实接口的测试。默认跳过，设 TICKFLOW_LIVE=1 才跑。
//
// 这些测试存在的理由：本库对 OKX 有几个【文档里查不到、只能实测】的假设——
// 各周期的对齐锚点、history-candles 的可回溯深度、未完结 K 线的出现位置。
// 把它们写成能重跑的检查，比在注释里写一句「据推测」踏实。
func liveClient(t *testing.T) *okx.Client {
	t.Helper()
	if os.Getenv("TICKFLOW_LIVE") == "" {
		t.Skip("跳过实网测试；设 TICKFLOW_LIVE=1 开启")
	}
	c, err := okx.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func liveSource(t *testing.T, opts ...okxsource.Option) *okxsource.Source {
	t.Helper()
	s, err := okxsource.New(liveClient(t), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestLiveAlignment 是内置周期表的校准检查。
//
// 内置表里 2D / 3D 的锚点是【推定】的——OKX 没有文档说明哪两天、哪三天归为
// 一根。这个测试拿真实数据把每个周期的开盘时间对一遍网格，不符就说明该用
// RegisterFixedPeriod 覆盖它。
func TestLiveAlignment(t *testing.T) {
	src := liveSource(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	for _, bar := range []string{
		"1m", "5m", "15m", "1H", "4H",
		"6H", "6Hutc", "12H", "12Hutc",
		"1D", "1Dutc", "2D", "3D", "1W", "1Wutc", "1M", "1Mutc",
	} {
		bar := bar
		t.Run(bar, func(t *testing.T) {
			p := tickflow.MustParsePeriod(bar)
			// 往回取约 30 根。
			from := now
			for i := 0; i < 30; i++ {
				from = p.Truncate(from - 1)
			}
			cs, err := src.Fetch(ctx, tickflow.FetchRequest{
				InstID: "BTC-USDT-SWAP", Bar: bar, From: from, To: p.Truncate(now),
			})
			if err != nil {
				t.Fatalf("拉取失败: %v", err)
			}
			if len(cs) == 0 {
				t.Fatalf("一根都没拉到")
			}
			if badTs, ok := tickflow.VerifyAlignment(p, cs); !ok {
				t.Errorf("锚点推定有误：%s 的 K 线开盘于 %s，本库算出的网格点是 %s。"+
					"请用 RegisterFixedPeriod 覆盖 %q 的锚点",
					bar,
					time.UnixMilli(badTs).UTC().Format(time.RFC3339),
					time.UnixMilli(p.Truncate(badTs)).UTC().Format(time.RFC3339),
					bar)
			}
			t.Logf("%s: %d 根，%s .. %s", bar, len(cs),
				time.UnixMilli(cs[0].Ts).UTC().Format(time.RFC3339),
				time.UnixMilli(cs[len(cs)-1].Ts).UTC().Format(time.RFC3339))
		})
	}
}

// TestLiveFetchContract 验证 Source 对外承诺的那几条：升序、落在区间内、
// 且【不含未完结的那一根】。最后一条最要紧——OKX 的 /market/candles 会把
// 当前还在走的那根带回来，漏掉过滤就等于给回测开了后门。
func TestLiveFetchContract(t *testing.T) {
	src := liveSource(t)
	p := tickflow.MustParsePeriod("1m")
	now := time.Now().UnixMilli()
	cur := p.Truncate(now) // 当前这根的开盘时刻，它还在走
	from := cur - 120*time.Minute.Milliseconds()

	// 故意把 To 要到「当前这根之后」，看它会不会漏进来。
	cs, err := src.Fetch(context.Background(), tickflow.FetchRequest{
		InstID: "BTC-USDT-SWAP", Bar: "1m", From: from, To: cur + p.Step().Milliseconds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) == 0 {
		t.Fatal("一根都没拉到")
	}
	for i, c := range cs {
		if i > 0 && c.Ts <= cs[i-1].Ts {
			t.Fatalf("第 %d 根没有严格升序：%d 之后是 %d", i, cs[i-1].Ts, c.Ts)
		}
		if c.Ts < from {
			t.Errorf("第 %d 根 %d 早于请求的起点 %d", i, c.Ts, from)
		}
		if c.Ts >= cur {
			t.Errorf("第 %d 根 %s 是【还在走】的那根，不该被返回",
				i, time.UnixMilli(c.Ts).UTC().Format(time.RFC3339))
		}
		if c.High < c.Low || c.Close <= 0 {
			t.Errorf("第 %d 根的 OHLC 不合理：%+v", i, c)
		}
	}
	t.Logf("拉到 %d 根 1m，最后一根 %s（当前这根 %s 已正确排除）",
		len(cs),
		time.UnixMilli(cs[len(cs)-1].Ts).UTC().Format(time.RFC3339),
		time.UnixMilli(cur).UTC().Format(time.RFC3339))
}

// TestLiveHistoryDepth 探一探 history-candles 到底能回溯多久。
//
// OKX 未文档化这一点，而它直接决定了使用者能回测多久。跑一次记下结果，
// 比对着一个猜的数字规划回测区间强。
func TestLiveHistoryDepth(t *testing.T) {
	if os.Getenv("TICKFLOW_LIVE") == "" {
		t.Skip("跳过实网测试；设 TICKFLOW_LIVE=1 开启")
	}
	src := liveSource(t, okxsource.ForceHistoryCandles())
	ctx := context.Background()
	now := time.Now()

	for _, bar := range []string{"1m", "5m", "1H", "1D"} {
		p := tickflow.MustParsePeriod(bar)
		var deepest time.Time
		// 按年往回探，找到最早还能取到数据的那一年。
		for back := 1; back <= 8; back++ {
			at := now.AddDate(-back, 0, 0)
			from := p.Truncate(at.UnixMilli())
			cs, err := src.Fetch(ctx, tickflow.FetchRequest{
				InstID: "BTC-USDT-SWAP", Bar: bar,
				From: from, To: p.Next(from + 20*p.Step().Milliseconds()),
			})
			if err != nil {
				t.Logf("%s 回溯 %d 年: %v", bar, back, err)
				break
			}
			if len(cs) == 0 {
				break
			}
			deepest = time.UnixMilli(cs[0].Ts).UTC()
		}
		if deepest.IsZero() {
			t.Logf("%s: 一年前就已经取不到数据", bar)
		} else {
			t.Logf("%s: 至少能回溯到 %s", bar, deepest.Format("2006-01-02"))
		}
	}
}
