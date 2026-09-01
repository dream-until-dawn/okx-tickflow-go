// 用 Feed 走一遍历史：主周期步进，辅周期只给【最后一根已收盘】的上下文。
//
//	go run ./examples/sync -inst BTC-USDT-SWAP -bar 15m -days 30 -root ./data
//	go run ./examples/feed -inst BTC-USDT-SWAP -root ./data
//
// 这就是回测引擎将来消费本库的样子：Next 推进一步，View 取当前，
// TF 取高周期，Prev 往回看。
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"time"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
	"github.com/dream-until-dawn/okx-tickflow-go/indicator"
	"github.com/dream-until-dawn/okx-tickflow-go/store/segfile"
)

func main() {
	var (
		inst = flag.String("inst", "BTC-USDT-SWAP", "标的")
		base = flag.String("base", "15m", "主周期")
		root = flag.String("root", "./data", "数据目录")
		show = flag.Int("show", 5, "打印最后几步")
	)
	flag.Parse()
	if err := run(*inst, *base, *root, *show); err != nil {
		log.Fatal(err)
	}
}

func run(inst, base, root string, show int) error {
	// 回测只读，用 OpenReadOnly：不取写锁，可以和正在同步的 examples/sync 并跑。
	store, err := segfile.OpenReadOnly(root)
	if err != nil {
		return err
	}
	defer store.Close()

	f, err := tickflow.NewFeed(store, tickflow.FeedConfig{
		InstID: inst,
		Base:   base,
		Extra:  []string{"1H", "1D"},

		// 库里只同步了主周期，所以让辅周期从它聚合出来。
		// 库里有独立的 1H / 1D 序列时，去掉这行会更准——那是 OKX 自己算的。
		Aggregate: true,

		Lookback: 5,
		Indicators: map[string][]tickflow.Indicator{
			base: {indicator.MA(20), indicator.MACD(12, 26, 9), indicator.RSI(14)},
			"1H": {indicator.EMA(20)},
			"1D": {indicator.MA(5, indicator.Named("ma5d"))},
		},
	})
	if err != nil {
		return fmt.Errorf("%w；先跑 examples/sync 把数据同步下来", err)
	}
	defer f.Close()

	// 热路径上用预解析的句柄，省掉按名字查的那次 map 查找。
	hMACD, err := f.Handle(base, "macd.hist")
	if err != nil {
		return err
	}

	type snap struct {
		ts              int64
		close           float64
		ma20, rsi, hist float64
		prev3           float64
		hourTs, dayTs   int64
		hourEMA, dayMA  float64
		ready           bool
	}
	var last []snap
	var steps, notReady, crossUp int
	var prevHist float64 = math.NaN()

	for f.Next() {
		v := f.View()
		steps++
		if !v.Ready() {
			notReady++
			continue
		}

		hist := v.At(hMACD)
		if !math.IsNaN(prevHist) && prevHist <= 0 && hist > 0 {
			crossUp++
		}
		prevHist = hist

		h, d := f.TF("1H"), f.TF("1D")
		last = append(last, snap{
			ts: v.Ts(), close: v.Close(),
			ma20: v.Ind("ma20"), rsi: v.Ind("rsi14"), hist: hist,
			prev3:  v.Prev(3).Close(),
			hourTs: h.Ts(), dayTs: d.Ts(),
			hourEMA: h.Ind("ema20"), dayMA: d.Ind("ma5d"),
			ready: v.Ready(),
		})
		if len(last) > show {
			last = last[1:]
		}
	}
	if err := f.Err(); err != nil {
		return err
	}
	if len(last) == 0 {
		return fmt.Errorf("一步都没走出来；库里的数据可能不够 warmup")
	}

	fmt.Printf("数据目录 %s（只读打开，不占写锁）\n", store.Root())
	fmt.Printf("%s 主周期 %s，走了 %d 步（其中 %d 步指标未就绪，已跳过）\n",
		inst, base, steps, notReady)
	fmt.Printf("MACD 柱由负转正 %d 次\n\n", crossUp)

	fmt.Printf("%-17s %10s %10s %8s %10s %10s | %-17s %10s | %-17s %10s\n",
		"时间(UTC)", "收盘", "ma20", "rsi14", "macd.hist", "前3根收盘",
		"1H 最后已收盘", "ema20", "1D 最后已收盘", "ma5d")
	for _, s := range last {
		fmt.Printf("%-17s %10.2f %10.2f %8.2f %10.2f %10.2f | %-17s %10.2f | %-17s %10.2f\n",
			ts(s.ts), s.close, s.ma20, s.rsi, s.hist, s.prev3,
			ts(s.hourTs), s.hourEMA, ts(s.dayTs), s.dayMA)
	}

	// 把「高周期在收盘前不可见」这条保证在最后一步上验一遍。
	v := f.View()
	bp := tickflow.MustParsePeriod(base)
	for _, bar := range []string{"1H", "1D"} {
		p := tickflow.MustParsePeriod(bar)
		tv := f.TF(bar)
		fmt.Printf("\n%s 这一根开于 %s，收于 %s\n", bar, ts(tv.Ts()), ts(p.Next(tv.Ts())))
		fmt.Printf("  主周期这一根收于 %s —— %s 已收盘，可见 ✓\n", ts(bp.Next(v.Ts())), bar)
		fmt.Printf("  下一根 %s 要到 %s 才收盘，不可见\n",
			ts(p.Next(tv.Ts())), ts(p.Next(p.Next(tv.Ts()))))
	}
	return nil
}

func ts(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
}
