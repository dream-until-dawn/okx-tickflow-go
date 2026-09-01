// 从本地库里读一段 K 线，算一组指标并打印最后几根。
//
//	go run ./examples/sync       -inst BTC-USDT-SWAP -bar 15m -days 30 -root ./data
//	go run ./examples/indicators -inst BTC-USDT-SWAP -bar 15m -root ./data
//
// 顺带把两套口径的差异摆在一起看：同一段行情，TradingView 与国内软件算出来的
// MACD 柱、KDJ 不是一个数。
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/dream-until-dawn/okx-tickflow-go/indicator"
	"github.com/dream-until-dawn/okx-tickflow-go/store/segfile"
)

func main() {
	var (
		inst = flag.String("inst", "BTC-USDT-SWAP", "标的")
		bar  = flag.String("bar", "15m", "周期")
		root = flag.String("root", "./data", "数据目录")
		n    = flag.Int("n", 8, "打印最后几根")
	)
	flag.Parse()

	if err := run(*inst, *bar, *root, *n); err != nil {
		log.Fatal(err)
	}
}

func run(inst, bar, root string, n int) error {
	// 只读，不取写锁，可与同步进程并跑。
	store, err := segfile.OpenReadOnly(root)
	if err != nil {
		return err
	}
	defer store.Close()

	meta, err := store.Meta(inst, bar)
	if err != nil {
		return fmt.Errorf("%w；先跑 examples/sync 把数据同步下来", err)
	}

	// 使用者自选要哪些指标——这就是 Feed 将来接收的那个列表。
	inds := []indicator.Indicator{
		indicator.MA(5),
		indicator.MA(20),
		indicator.EMA(20),
		indicator.MACD(12, 26, 9),
		indicator.KDJ(9, 3, 3),
		indicator.RSI(14),
		indicator.CCI(20),
		indicator.BOLL(20, 2),
	}

	// warmup 决定了要往前多读多少根才能让最后 n 根都有有效值。
	var warmup int
	for _, ind := range inds {
		if w := ind.Warmup(); w > warmup {
			warmup = w
		}
	}
	fmt.Printf("%s/%s 共 %d 根；最长 warmup %d 根\n\n", inst, bar, meta.Count, warmup)

	it, err := store.Iter(inst, bar, 0, 0)
	if err != nil {
		return err
	}
	defer it.Close()

	// 列名：单输出是指标名，多输出是「指标名.字段」。
	var keys []string
	for _, ind := range inds {
		keys = append(keys, indicator.Keys(ind)...)
	}

	type row struct {
		ts   int64
		vals []float64
	}
	var tail []row
	for it.Next() {
		c := it.Candle()
		vals := make([]float64, 0, len(keys))
		for _, ind := range inds {
			vals = append(vals, ind.Update(c)...)
		}
		tail = append(tail, row{ts: c.Ts, vals: vals})
		if len(tail) > n {
			tail = tail[1:]
		}
	}
	if err := it.Err(); err != nil {
		return err
	}
	if len(tail) == 0 {
		return fmt.Errorf("库里一根 K 线都没有")
	}

	fmt.Printf("%-14s", "时间(UTC)")
	for _, r := range tail {
		fmt.Printf("%12s", time.UnixMilli(r.ts).UTC().Format("01-02 15:04"))
	}
	fmt.Printf("\n%s\n", strings.Repeat("-", 14+12*len(tail)))
	for ki, k := range keys {
		fmt.Printf("%-14s", k)
		for _, r := range tail {
			fmt.Printf("%12s", fmtVal(r.vals[ki]))
		}
		fmt.Println()
	}

	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("同一段行情，两套口径算出来不是一个数：")
	compare(store, inst, bar)
	return nil
}

// compare 把 TV 与 CN 的差异摆出来。差异只有三处，但落到数上很显眼。
func compare(store *segfile.Store, inst, bar string) {
	cs, err := store.Range(inst, bar, 0, 0)
	if err != nil || len(cs) == 0 {
		return
	}
	for _, c := range []struct {
		label string
		tv    indicator.Indicator
		cn    indicator.Indicator
		field int
		why   string
	}{
		{"MACD 柱", indicator.MACD(12, 26, 9), indicator.MACD(12, 26, 9, indicator.CN), 2,
			"CN 的柱子乘 2"},
		{"KDJ 的 K", indicator.KDJ(9, 3, 3), indicator.KDJ(9, 3, 3, indicator.CN), 0,
			"TV 用简单平均，CN 用指数平滑"},
		{"KDJ 的 J", indicator.KDJ(9, 3, 3), indicator.KDJ(9, 3, 3, indicator.CN), 2,
			"同上；TV 本身没有 J 线，是本库按 3K-2D 补的"},
		{"RSI(14)", indicator.RSI(14), indicator.RSI(14, indicator.CN), 0,
			"只差播种，走够长就收敛到同一个值"},
		{"BOLL 上轨", indicator.BOLL(20, 2), indicator.BOLL(20, 2, indicator.CN), 1,
			"同一个算法，标准差都按总体 n"},
	} {
		tv := indicator.Compute(c.tv, cs)
		cn := indicator.Compute(c.cn, cs)
		last := len(cs) - 1
		a, b := tv[last][c.field], cn[last][c.field]
		fmt.Printf("  %-9s TV %11s  CN %11s   %s\n", c.label, fmtVal(a), fmtVal(b), c.why)
	}
	fmt.Printf("\n（比的是第 %d 根，播种造成的差异到这里早已衰减掉了）\n", len(cs))
}

func fmtVal(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.4g", v)
}
