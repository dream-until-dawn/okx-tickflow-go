package okxsource_test

import (
	"context"
	"os"
	"testing"
	"time"

	okx "github.com/dream-until-dawn/okx-api-v5-go"
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
	okxsource "github.com/dream-until-dawn/okx-tickflow-go/source/okxsource"
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

// TestLiveMarkAndIndexCandles 实拉标记价与指数价，并验证它们确实是【另外两条】
// 序列，而不是成交价的别名。
//
// 这条测试的重点不是「接口能调通」，而是标记价【比成交价平稳】——那正是回测里
// 必须用它的理由：用成交价，影线会制造出真实不会发生的强平。
func TestLiveMarkAndIndexCandles(t *testing.T) {
	trades := liveSource(t)
	mark, err := okxsource.New(liveClient(t), okxsource.MarkPrice)
	if err != nil {
		t.Fatal(err)
	}
	index, err := okxsource.New(liveClient(t), okxsource.IndexPrice)
	if err != nil {
		t.Fatal(err)
	}

	p := tickflow.MustParsePeriod("1H")
	now := time.Now().UnixMilli()
	to := p.Truncate(now)
	from := to - 200*p.Step().Milliseconds()
	ctx := context.Background()

	get := func(src *okxsource.Source, inst string) []tickflow.Candle {
		t.Helper()
		cs, err := src.Fetch(ctx, tickflow.FetchRequest{InstID: inst, Bar: "1H", From: from, To: to})
		if err != nil {
			t.Fatalf("%s 拉取失败: %v", src.Series(), err)
		}
		if len(cs) == 0 {
			t.Fatalf("%s 一根都没拉到", src.Series())
		}
		if badTs, ok := tickflow.VerifyAlignment(p, cs); !ok {
			t.Fatalf("%s 的 %d 没落在网格上", src.Series(), badTs)
		}
		return cs
	}

	tc := get(trades, "BTC-USDT-SWAP")
	mc := get(mark, "BTC-USDT-SWAP")
	ic := get(index, "BTC-USDT") // 指数用现货形式的 instId

	t.Logf("成交价 %d 根、标记价 %d 根、指数价 %d 根", len(tc), len(mc), len(ic))

	// 标记价与指数价没有成交量——上游文档这么写的，这里当场确认。
	for _, c := range mc {
		if c.Vol != 0 || c.VolCcy != 0 || c.VolCcyQuote != 0 {
			t.Fatalf("标记价 K 线不该有成交量：%+v", c)
			break
		}
	}

	// 按 ts 对齐，比较两条序列的振幅。
	byTs := map[int64]tickflow.Candle{}
	for _, c := range mc {
		byTs[c.Ts] = c
	}
	var paired, markNarrower int
	var sumT, sumM float64
	for _, c := range tc {
		m, ok := byTs[c.Ts]
		if !ok {
			continue
		}
		paired++
		rt := (c.High - c.Low) / c.Close
		rm := (m.High - m.Low) / m.Close
		sumT, sumM = sumT+rt, sumM+rm
		if rm < rt {
			markNarrower++
		}
		// 标记价与成交价必须是【不同】的数——相等说明拿错了端点。
		if paired < 5 && c.Close == m.Close && c.High == m.High && c.Low == m.Low {
			t.Errorf("%d 处标记价与成交价完全相同，多半是端点搞错了", c.Ts)
		}
	}
	if paired < 50 {
		t.Fatalf("只对上 %d 根，样本太少", paired)
	}
	t.Logf("对齐 %d 根：平均振幅 成交价 %.4f%% / 标记价 %.4f%%，标记价更窄的占 %d/%d",
		paired, sumT/float64(paired)*100, sumM/float64(paired)*100, markNarrower, paired)

	if sumM >= sumT {
		t.Errorf("标记价的平均振幅没有比成交价小——它本该由指数价平滑而来。"+
			"成交价 %.6f，标记价 %.6f", sumT/float64(paired), sumM/float64(paired))
	}
}

// TestLiveMarkPriceHistoryFloor 量出标记价历史那条硬线，【在 1D 上】。
//
// ⚠️ 这条测试只覆盖 1D。边界随周期而变——日内档（1H/15m/1m）比日线档晚两天，
// 成交价那一侧的边界也随周期变。跨周期的部分由 TestLiveHistoryFloorVariesByBar
// 覆盖，contract.md 里的表带 bar 列。
//
// 这段限定语是补上去的：本测试最初的注释把边界写得像是与周期无关，
// contract.md 照抄了那个说法，于是两个下游仓都把日线的边界当成了通用边界。
//
// 这一条一度被写成「标记价与普通 K 线同深」——那是错的，而错误的来源值得记下来：
// 最初的测量是在【模拟盘】上做的，模拟盘的两条序列在同一天一起截断（那是模拟盘
// 自己的数据上限），于是「一起返回空」被读成了「一样深」，实际是「一样测不到」。
//
// 生产环境上标记价有一条硬线。它对「回测能从哪天开始」是硬约束：早于这条线的
// 区间标记价根本不存在，此时打开记账内核的 AllowMarkPxFallback 是【正当的】，
// 而不是将就。
func TestLiveMarkPriceHistoryFloor(t *testing.T) {
	trades := liveSource(t)
	mark, err := okxsource.New(liveClient(t), okxsource.MarkPrice)
	if err != nil {
		t.Fatal(err)
	}

	p := tickflow.MustParsePeriod("1D")
	from := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	to := p.Truncate(time.Now().UnixMilli())
	ctx := context.Background()
	hk := time.FixedZone("HKT", 8*3600)

	earliest := func(src *okxsource.Source, inst string) int64 {
		t.Helper()
		cs, err := src.Fetch(ctx, tickflow.FetchRequest{
			InstID: inst, Bar: "1D", From: from, To: to,
		})
		if err != nil {
			t.Fatalf("%s %s: %v", inst, src.Series(), err)
		}
		if len(cs) == 0 {
			t.Fatalf("%s %s 一根都没拉到", inst, src.Series())
		}
		return cs[0].Ts
	}

	// 这条线是【港时】的——1D 按港时对齐，UTC 2019-12-31 16:00 开盘那根就是港时
	// 2020-01-01。用 1Dutc 的人看到的日期会不一样。
	//
	// 这是【日线档】的硬线。日内档是 2020-01-03，见 TestLiveHistoryFloorVariesByBar。
	floorHK := "2020-01-01"

	var older, sameDepth int
	for _, inst := range []string{
		"BTC-USDT-SWAP", "ETH-USDT-SWAP", "BTC-USD-SWAP",
		"SOL-USDT-SWAP", "DOGE-USDT-SWAP",
	} {
		tTs, mTs := earliest(trades, inst), earliest(mark, inst)
		tDay := time.UnixMilli(tTs).In(hk).Format("2006-01-02")
		mDay := time.UnixMilli(mTs).In(hk).Format("2006-01-02")
		gap := (mTs - tTs) / 86400000

		t.Logf("%-16s 成交价 %s  标记价 %s  差 %d 天", inst, tDay, mDay, gap)

		if mTs < tTs {
			t.Errorf("%s 的标记价比成交价还早，与已知的形态不符", inst)
		}
		if gap == 0 {
			sameDepth++
			// 同深的合约必然是在硬线【之后】上线的。
			if tDay < floorHK {
				t.Errorf("%s 上线于 %s（早于 %s）却与标记价同深——硬线的说法要重验",
					inst, tDay, floorHK)
			}
			continue
		}
		older++
		// 有差距的，标记价必须恰好停在硬线上。
		if mDay != floorHK {
			t.Errorf("%s 的标记价起点是 %s，不是预期的硬线 %s——"+
				"这条线是观察结论不是官方承诺，变了就该改文档", inst, mDay, floorHK)
		}
	}

	if older == 0 || sameDepth == 0 {
		t.Fatalf("样本没覆盖两种情形（比硬线早的 %d 个、同深的 %d 个），这个测试说明不了什么",
			older, sameDepth)
	}
}

// oldestBefore 问「有没有早于 ts 的数据」。OKX 的 after 语义就是「返回早于该 ts 的」，
// 所以这是个直接的边界探针，不必把整条序列翻一遍。
func oldestBefore(t *testing.T, c *okx.Client, series okxsource.Series, inst, bar string, ts int64) int64 {
	t.Helper()
	r := okx.CandlesRequest{InstID: inst, Bar: bar, After: ts, Limit: 100}
	var (
		cs  []okx.Candle
		err error
	)
	switch series {
	case okxsource.MarkPrice:
		cs, err = c.Market.HistoryMarkPriceCandles(context.Background(), r)
	default:
		cs, err = c.Market.HistoryCandles(context.Background(), r)
	}
	if err != nil {
		t.Fatalf("%s %s/%s after=%d: %v", series, inst, bar, ts, err)
	}
	time.Sleep(90 * time.Millisecond)
	if len(cs) == 0 {
		return 0
	}
	m := cs[0].Ts
	for _, x := range cs {
		if x.Ts < m {
			m = x.Ts
		}
	}
	return m
}

// historyFloor 二分出最早可得的那根。
func historyFloor(t *testing.T, c *okx.Client, series okxsource.Series, inst, bar string) int64 {
	t.Helper()
	lo := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	hi := time.Now().UnixMilli()
	if oldestBefore(t, c, series, inst, bar, hi) == 0 {
		return 0
	}
	best := int64(0)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if got := oldestBefore(t, c, series, inst, bar, mid); got != 0 {
			best, hi = got, mid
		} else {
			lo = mid + 1
		}
	}
	return best
}

// TestLiveHistoryFloorVariesByBar 是本文件里最容易被读错的一条：
// **历史边界随【周期】而变，两条序列都是。**
//
// 这条曾经以另一种形式写错过：contract.md 里那张表量的是 1D，却没有 bar 列，
// 于是两个下游仓都把日线的边界当成了通用边界，写进各自的文档。日内周期上，
// 标记价晚两天、成交价晚十几到二十几天——按 1D 的数去规划 1m 的回测区间，
// 差出来的那一段根本不存在。
//
// 上一次的教训是「两条都取不到」与「两条一样深」长得一样；这一次是
// **「1D 的边界」与「与周期无关的边界」在表格上长得一样**。同一类错。
func TestLiveHistoryFloorVariesByBar(t *testing.T) {
	c := liveClient(t)
	hk := time.FixedZone("HKT", 8*3600)
	day := func(ms int64) string {
		if ms == 0 {
			return "取不到"
		}
		return time.UnixMilli(ms).In(hk).Format("2006-01-02 15:04")
	}

	// 只取两档代表：1D 与 1m。中间的 15m / 1H 实测与 1m 同档，
	// 全测一遍要多花两倍时间而不多说明什么。
	for _, inst := range []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP"} {
		var (
			tradeDaily = historyFloor(t, c, okxsource.Trades, inst, "1D")
			tradeMin   = historyFloor(t, c, okxsource.Trades, inst, "1m")
			markDaily  = historyFloor(t, c, okxsource.MarkPrice, inst, "1D")
			markMin    = historyFloor(t, c, okxsource.MarkPrice, inst, "1m")
		)
		t.Logf("%-14s 成交价 1D %s / 1m %s   标记价 1D %s / 1m %s",
			inst, day(tradeDaily), day(tradeMin), day(markDaily), day(markMin))

		for _, v := range []struct {
			name string
			ts   int64
		}{{"成交价 1D", tradeDaily}, {"成交价 1m", tradeMin},
			{"标记价 1D", markDaily}, {"标记价 1m", markMin}} {
			if v.ts == 0 {
				t.Fatalf("%s 的 %s 一根都取不到", inst, v.name)
			}
		}

		// 核心断言：日内档【严格晚于】日线档。两条序列都是。
		// 这一条不成立，就说明边界与周期无关了，那张表可以合并——但在此之前，
		// 任何不带 bar 的边界表述都是错的。
		if tradeMin <= tradeDaily {
			t.Errorf("%s 的成交价 1m 边界 %s 不晚于 1D 的 %s——"+
				"「边界随周期而变」这条要重验", inst, day(tradeMin), day(tradeDaily))
		}
		if markMin <= markDaily {
			t.Errorf("%s 的标记价 1m 边界 %s 不晚于 1D 的 %s——同上",
				inst, day(markMin), day(markDaily))
		}
		// 同一档内，标记价晚于成交价（这两个合约都早于标记价那条硬线上线）。
		if markDaily <= tradeDaily || markMin <= tradeMin {
			t.Errorf("%s 的标记价边界不晚于成交价——与已知形态不符", inst)
		}
	}

	// 标记价那两档是【跨合约一致】的硬线，成交价则各合约不同（跟上线时间走）。
	const (
		markDailyHK = "2020-01-01 00:00"
		markMinHK   = "2020-01-03 00:00"
	)
	for _, inst := range []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP"} {
		if got := day(historyFloor(t, c, okxsource.MarkPrice, inst, "1D")); got != markDailyHK {
			t.Errorf("%s 的标记价日线硬线是 %s，文档记的是 %s——观察结论变了，该改文档",
				inst, got, markDailyHK)
		}
		if got := day(historyFloor(t, c, okxsource.MarkPrice, inst, "1m")); got != markMinHK {
			t.Errorf("%s 的标记价日内硬线是 %s，文档记的是 %s——同上",
				inst, got, markMinHK)
		}
	}
}
