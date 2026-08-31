// 把 OKX 的历史 K 线同步到本地，并打印落库结果。
//
//	go run ./examples/sync -inst BTC-USDT-SWAP -bar 15m -days 30 -root ./data
//
// 重复跑不会重复拉：已确认过的区间记在 meta 的 coverage 里，第二次跑只会补
// 上次之后新收盘的那几根。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	okx "github.com/dream-until-dawn/okx-api-v5-go"
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
	"github.com/dream-until-dawn/okx-tickflow-go/source/okxsource"
	"github.com/dream-until-dawn/okx-tickflow-go/store/segfile"
)

func main() {
	var (
		inst = flag.String("inst", "BTC-USDT-SWAP", "标的")
		bar  = flag.String("bar", "15m", "周期")
		days = flag.Int("days", 30, "往回同步多少天")
		root = flag.String("root", "./data", "数据目录")
	)
	flag.Parse()

	if err := run(*inst, *bar, *days, *root); err != nil {
		log.Fatal(err)
	}
}

func run(inst, bar string, days int, root string) error {
	p, err := tickflow.ParsePeriod(bar)
	if err != nil {
		return err
	}

	client, err := okx.NewClient()
	if err != nil {
		return err
	}
	src, err := okxsource.New(client)
	if err != nil {
		return err
	}
	store, err := segfile.Open(root)
	if err != nil {
		return err
	}
	defer store.Close()

	from := time.Now().AddDate(0, 0, -days).UnixMilli()
	start := time.Now()
	rep, err := tickflow.NewSyncer(src, store).Sync(context.Background(), tickflow.SyncRequest{
		InstID: inst, Bar: bar, From: from,
	})
	if err != nil {
		return err
	}

	fmt.Printf("同步 %s/%s：新增 %d 根，拉取 %d 批，耗时 %s\n",
		inst, bar, rep.Added, rep.Fetches, time.Since(start).Round(time.Millisecond))
	fmt.Printf("  处理区间 %s .. %s\n", ts(rep.Range.From), ts(rep.Range.To))
	if len(rep.Gaps) > 0 {
		fmt.Printf("  空洞（请求过但一根都没有）：%d 段\n", len(rep.Gaps))
		for _, g := range rep.Gaps {
			fmt.Printf("    %s .. %s\n", ts(g.From), ts(g.To))
		}
	}
	if rep.Misaligned > 0 {
		fmt.Printf("  ⚠️ %d 根没落在 %s 的对齐网格上——该周期的锚点推定有误\n", rep.Misaligned, bar)
	}

	meta, err := store.Meta(inst, bar)
	if err != nil {
		return err
	}
	fmt.Printf("落库共 %d 根，%s .. %s；覆盖区间 %d 段\n",
		meta.Count, ts(meta.FirstTs), ts(meta.LastTs), len(meta.Coverage))

	// 当前这根还在走，不该出现在库里——这是最容易被写出未来函数的地方。
	if cur := p.Truncate(time.Now().UnixMilli()); meta.LastTs >= cur {
		return fmt.Errorf("未完结的 K 线 %s 进库了", ts(meta.LastTs))
	}
	fmt.Printf("最后一根 %s，当前那根 %s 仍在走，已正确排除\n",
		ts(meta.LastTs), ts(p.Truncate(time.Now().UnixMilli())))

	last, err := store.Range(inst, bar, meta.LastTs-4*p.Step().Milliseconds(), 0)
	if err != nil {
		return err
	}
	fmt.Println("\n最后几根：")
	for _, c := range last {
		fmt.Printf("  %s  O %-12g H %-12g L %-12g C %-12g V %g\n",
			ts(c.Ts), c.Open, c.High, c.Low, c.Close, c.Vol)
	}

	if fi, err := os.Stat(fmt.Sprintf("%s/%s/%s.dat", root, inst, bar)); err == nil {
		fmt.Printf("\n数据文件 %d 字节 = %d 根 × %d 字节\n",
			fi.Size(), fi.Size()/segfile.RecordSize, segfile.RecordSize)
	}
	return nil
}

func ts(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05Z")
}
