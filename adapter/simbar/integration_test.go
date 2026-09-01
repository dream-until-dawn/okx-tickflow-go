package simbar

import (
	"testing"

	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
	"github.com/dream-until-dawn/okx-tickflow-go/indicator"
	"github.com/dream-until-dawn/okx-tickflow-go/store/segfile"
	"github.com/shopspring/decimal"
)

const inst = "BTC-USDT-SWAP"

// TestThreeLibrariesCompose 把三个库真正串起来跑一遍：
//
//	真实行情 → segfile 落地 → Feed 步进（带指标）→ simbar → 记账内核
//
// 这是本适配层存在的全部意义。单元测试证明每一步对，只有这个测试能证明
// 它们【接得上】——形态、时间戳、精度、以及回测循环的写法本身。
func TestThreeLibrariesCompose(t *testing.T) {
	cs := loadCandles(t)

	// 1. 行情落地。用真实的 15m BTC-USDT-SWAP。
	store, err := segfile.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Append(inst, "15m", cs); err != nil {
		t.Fatal(err)
	}

	// 2. 带指标的可步进视图。
	feed, err := tickflow.NewFeed(store, tickflow.Config{
		InstID: inst, Base: "15m", Extra: []string{"1H"},
		Aggregate: true, Lookback: 3,
		Indicators: map[string][]tickflow.Indicator{
			"15m": {indicator.MA(20), indicator.MACD(12, 26, 9)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer feed.Close()

	// 3. 记账内核。
	sim, err := okxsim.New(okxsim.Config{
		PosMode:      types.NetMode,
		RefData:      refdata.MustEmbedded(),
		DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.Deposit("USDT", decimal.NewFromInt(50000)); err != nil {
		t.Fatal(err)
	}

	// 4. 回测循环。
	var (
		steps    int
		placed   bool
		fills    []okxsim.FillResult
		orderPx  decimal.Decimal
		lastSeen tickflow.Candle
	)
	for feed.Next() {
		v := feed.View()
		if !v.Ready() {
			continue
		}
		steps++
		lastSeen = v.Candle()

		step, err := Advance(sim, inst, v)
		if err != nil {
			t.Fatalf("第 %d 步推进失败: %v", steps, err)
		}
		fills = append(fills, step.Fills...)

		// 行情走到第 40 步时挂一笔买单，价格低于当时收盘 0.5%，
		// 后面总有一根 K 线的最低价会触及它。
		if steps == 40 && !placed {
			orderPx = Dec(v.Close()).Mul(decimal.NewFromFloat(0.995)).Round(1)
			pr, err := sim.PlaceOrder(okxsim.Order{
				OrdID: "o1", InstID: inst, TdMode: types.TdIsolated,
				Side: types.Buy, PosSide: types.PosNet, OrdType: types.OrdLimit,
				Sz: decimal.NewFromInt(4), Px: orderPx, Ts: v.Ts(),
			})
			if err != nil {
				t.Fatalf("下单失败: %v", err)
			}
			if pr.State != types.OrdLive {
				t.Fatalf("委托状态是 %s，期望挂住（价格低于当时最新价）", pr.State)
			}
			placed = true
		}
	}
	if err := feed.Err(); err != nil {
		t.Fatal(err)
	}

	if steps < 100 {
		t.Fatalf("只走了 %d 步，数据不够跑出个像样的回测", steps)
	}
	if !placed {
		t.Fatal("一单都没下出去")
	}

	// 5. 结果。挂单被触及后应当成交。
	if len(fills) == 0 {
		t.Fatalf("挂在 %s 的买单一直没成交——这段行情的最低价没触及过它？", orderPx)
	}
	pos, ok := sim.PositionOf(inst, types.PosNet)
	if !ok || !pos.Pos.IsPositive() {
		t.Fatalf("成交了却没有仓位：%+v", pos)
	}
	t.Logf("走了 %d 步，挂单 %s 成交 %d 笔，持仓 %s 张，均价 %s",
		steps, orderPx, len(fills), pos.Pos, pos.AvgPx)

	// 6. 价格一路无损。记账内核记下的最新价必须与行情层最后那根的收盘价
	//    【逐位相同】——中间经过了 float64 → decimal 的转换。
	if got, want := sim.LastPx(inst), Dec(lastSeen.Close); !got.Equal(want) {
		t.Errorf("记账内核的最新价 %s 与行情的收盘价 %s 对不上", got, want)
	}
	if got := sim.LastPx(inst).InexactFloat64(); got != lastSeen.Close {
		t.Errorf("转回 float64 是 %v，原值 %v", got, lastSeen.Close)
	}

	bal, err := sim.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	if !bal.Eq.IsPositive() {
		t.Fatalf("权益不该是非正数：%+v", bal)
	}
}

// TestAdvanceMatchesManualConversion 保证 Advance 只是「转换 + 推进」的顺手包装，
// 没有夹带任何额外语义。两条路走出来的账必须一模一样。
func TestAdvanceMatchesManualConversion(t *testing.T) {
	cs := loadCandles(t)[:50]

	run := func(useAdvance bool) decimal.Decimal {
		sim, err := okxsim.New(okxsim.Config{
			PosMode: types.NetMode, RefData: refdata.MustEmbedded(),
			DefaultLever: decimal.NewFromInt(5),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := sim.Deposit("USDT", decimal.NewFromInt(50000)); err != nil {
			t.Fatal(err)
		}
		store, err := segfile.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.Append(inst, "15m", cs); err != nil {
			t.Fatal(err)
		}
		feed, err := tickflow.NewFeed(store, tickflow.Config{InstID: inst, Base: "15m"})
		if err != nil {
			t.Fatal(err)
		}
		defer feed.Close()

		for feed.Next() {
			v := feed.View()
			if useAdvance {
				if _, err := Advance(sim, inst, v); err != nil {
					t.Fatal(err)
				}
				continue
			}
			b, err := ToBar(inst, v.Candle())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sim.Advance(b); err != nil {
				t.Fatal(err)
			}
		}
		return sim.LastPx(inst)
	}

	if a, b := run(true), run(false); !a.Equal(b) {
		t.Fatalf("Advance 走出来是 %s，手工转换走出来是 %s", a, b)
	}
}
