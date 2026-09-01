// 完整一条链的最小回测：行情库 → 带指标的步进视图 → 记账内核。
//
//	go run ./examples/sync -inst BTC-USDT-SWAP -bar 15m -days 30 -root ./data   # 主模块里
//	cd adapter/simbar && go run ./examples/backtest -root ../../data
//
// 策略本身是最土的均线金叉死叉，只为把链路跑通——本库不做交易决策，
// 这里也不是在推荐什么策略。
//
// 注意本例属于【嵌套模块】adapter/simbar：它需要 decimal 与记账内核，
// 而主模块承诺依赖树只有 okx-api-v5-go 一个。
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"time"

	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
	"github.com/dream-until-dawn/okx-tickflow-go/adapter/simbar"
	"github.com/dream-until-dawn/okx-tickflow-go/indicator"
	"github.com/dream-until-dawn/okx-tickflow-go/store/segfile"
	"github.com/shopspring/decimal"
)

func main() {
	var (
		inst = flag.String("inst", "BTC-USDT-SWAP", "标的")
		bar  = flag.String("bar", "15m", "主周期")
		root = flag.String("root", "./data", "数据目录")
		cash = flag.Int64("cash", 50000, "初始 USDT")
		sz   = flag.Int64("sz", 2, "每次开仓张数")
	)
	flag.Parse()
	if err := run(*inst, *bar, *root, *cash, *sz); err != nil {
		log.Fatal(err)
	}
}

func run(inst, bar, root string, cash, sz int64) error {
	// 回测只读，不占写锁——同步进程可以同时跑着。
	store, err := segfile.OpenReadOnly(root)
	if err != nil {
		return fmt.Errorf("%w；先在主模块里跑 examples/sync 同步数据", err)
	}
	defer store.Close()

	feed, err := tickflow.NewFeed(store, tickflow.FeedConfig{
		InstID: inst, Base: bar, Extra: []string{"1D"},
		Aggregate: true, Lookback: 2,
		Indicators: map[string][]tickflow.Indicator{
			bar: {
				indicator.MA(5, indicator.Named("fast")),
				indicator.MA(20, indicator.Named("slow")),
			},
			"1D": {indicator.MA(5, indicator.Named("ma5d"))},
		},
	})
	if err != nil {
		return err
	}
	defer feed.Close()

	fast, err := feed.Handle(bar, "fast")
	if err != nil {
		return err
	}
	slow, err := feed.Handle(bar, "slow")
	if err != nil {
		return err
	}

	sim, err := okxsim.New(okxsim.Config{
		PosMode:      types.NetMode,
		RefData:      refdata.MustEmbedded(), // 内置快照：回测要的是不可复现风险为零
		DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		return err
	}
	if err := sim.Deposit("USDT", decimal.NewFromInt(cash)); err != nil {
		return err
	}

	var (
		steps, trades int
		prevDiff      = math.NaN()
		holding       bool
		firstTs       int64
	)

	for feed.Next() {
		v := feed.View()
		if !v.Ready() {
			continue
		}
		steps++
		if firstTs == 0 {
			firstTs = v.Ts()
		}

		// 先推进行情：撮合、强平都发生在这一步里。
		if _, err := simbar.Advance(sim, inst, v); err != nil {
			return fmt.Errorf("第 %d 步: %w", steps, err)
		}

		diff := v.At(fast) - v.At(slow)
		if math.IsNaN(prevDiff) {
			prevDiff = diff
			continue
		}
		goldenCross := prevDiff <= 0 && diff > 0
		deathCross := prevDiff >= 0 && diff < 0
		prevDiff = diff

		switch {
		case goldenCross && !holding:
			if err := order(sim, inst, types.Buy, sz, false, v.Ts(), trades); err != nil {
				return err
			}
			holding, trades = true, trades+1
		case deathCross && holding:
			if err := order(sim, inst, types.Sell, sz, true, v.Ts(), trades); err != nil {
				return err
			}
			holding, trades = false, trades+1
		}
	}
	if err := feed.Err(); err != nil {
		return err
	}
	if steps == 0 {
		return fmt.Errorf("一步都没走出来；库里的数据可能不够 warmup")
	}

	bal, err := sim.BalanceOf("USDT")
	if err != nil {
		return err
	}
	pnl := bal.Eq.Sub(decimal.NewFromInt(cash))

	fmt.Printf("数据目录 %s（只读）\n", store.Root())
	fmt.Printf("%s %s  %s .. %s，%d 步\n", inst, bar,
		ts(firstTs), ts(feed.View().Ts()), steps)
	fmt.Printf("均线金叉死叉，每次 %d 张，成交 %d 笔\n\n", sz, trades)
	fmt.Printf("  初始权益   %10s USDT\n", decimal.NewFromInt(cash))
	fmt.Printf("  期末权益   %10s USDT\n", bal.Eq.Round(2))
	fmt.Printf("  盈亏       %10s USDT（%s%%）\n", pnl.Round(2),
		pnl.Div(decimal.NewFromInt(cash)).Mul(decimal.NewFromInt(100)).Round(2))

	if pos, ok := sim.PositionOf(inst, types.PosNet); ok && !pos.Pos.IsZero() {
		m, err := sim.MetricsOf(inst, types.PosNet)
		if err != nil {
			return err
		}
		fmt.Printf("\n  期末仍持仓 %s 张，均价 %s，浮盈 %s，保证金率 %s%%\n",
			pos.Pos, pos.AvgPx, m.UPL.Round(2), m.MgnRatio.Round(2))
	}

	fmt.Printf("\n⚠️ 未计资金费——OKX 只保留约 3 个月历史，部分区间有比全都没有更糟。\n" +
		"   这会系统性高估多头持仓的收益，解读上面的数字时请扣减。\n")
	return nil
}

func order(sim *okxsim.Simulator, inst string, side types.Side, sz int64, reduce bool, at int64, n int) error {
	_, err := sim.PlaceOrder(okxsim.Order{
		OrdID:      fmt.Sprintf("o%d", n),
		InstID:     inst,
		TdMode:     types.TdIsolated,
		Side:       side,
		PosSide:    types.PosNet,
		OrdType:    types.OrdMarket,
		Sz:         decimal.NewFromInt(sz),
		ReduceOnly: reduce,
		Ts:         at,
	})
	return err
}

func ts(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
}
