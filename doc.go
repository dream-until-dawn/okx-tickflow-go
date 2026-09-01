// Package tickflow 是 OKX 的行情数据层：拉取、持久化、算指标，
// 并向回测引擎提供可步进的多周期视图。
//
// 上游是 okx-api-v5-go（拿数据），旁边是 okx-position-simulator-go（记账），
// 下游是回测引擎（消费视图）。本包【不做交易决策，也不做仓位核算】。
//
// # 四个部件
//
//   - [Source] 从交易所拉 K 线。实现见 source/okxsource。
//   - [Store] 把 K 线存下来。默认实现见 store/segfile。
//   - [Syncer] 编排两者，只拉 coverage 里缺的那些段。
//   - [Feed] 把存下来的 K 线连同指标，变成可以一步一步走的 [View]。
//
// 指标实现在子包 indicator 里，接口 [Indicator] 定义在这里——Feed 要消费指标，
// 而指标要用 [Candle]，定义放在任何一边都会形成循环引用。
//
// 与记账内核对接的 adapter/simbar 是【独立嵌套模块】：它需要
// shopspring/decimal，而主模块的依赖树只有 okx-api-v5-go 一个。
//
// # 从头到尾
//
//	client, _ := okx.NewClient()
//	src, _ := okxsource.New(client)
//	store, _ := segfile.Open("D:/quant/data")     // 目录由你指定
//	defer store.Close()
//
//	// 同步。重复跑不会重复拉。
//	tickflow.NewSyncer(src, store).Sync(ctx, tickflow.SyncRequest{
//	    InstID: "BTC-USDT-SWAP", Bar: "15m",
//	    From: time.Now().AddDate(0, 0, -30).UnixMilli(),
//	})
//
//	// 步进。
//	f, _ := tickflow.NewFeed(store, tickflow.FeedConfig{
//	    InstID: "BTC-USDT-SWAP",
//	    Base:   "15m",
//	    Extra:  []string{"1H", "1D"},
//	    Lookback: 5,
//	    Indicators: map[string][]tickflow.Indicator{
//	        "15m": {indicator.MA(20), indicator.MACD(12, 26, 9)},
//	    },
//	})
//	defer f.Close()
//
//	for f.Next() {
//	    v := f.View()
//	    if !v.Ready() { continue }
//	    v.Close()                 // 当前收盘价
//	    v.Ind("macd.hist")        // 指标
//	    v.Prev(3).Close()         // 往回看
//	    f.TF("1D").Ind("ma20")    // 日线——永远是【已收盘】那根
//	}
//	if err := f.Err(); err != nil { ... }
//
// # 四条硬保证
//
// 本包存在的理由，多半在这四条上。它们各自对应一类【无声的错】——
// 不报错、不崩溃，只是把回测结果悄悄变得好看。
//
// 一、未完结的 K 线绝不进库。OKX 的 /market/candles 会把当前还在走的那根一并
// 返回。用一根还在变的 K 线的「收盘价」做决策，等于偷看了它走完之后才知道的
// 信息。本包在 [Source] 那一层就丢弃它，且 coverage 的末端只记到当前那根的
// 【开盘时刻】——不然等它收盘后就再也不会去补，那根会永久停在半截的值上。
//
// 二、高周期 K 线在收盘前不可见。[Feed.TF] 永远返回【最后一根已收盘】的：
// 主周期走到 10:15 时，TF("1D") 给的是昨天那根，不是今天还没走完的。
//
// 三、空洞与「没拉过」分得清。小币种或维护期，OKX 根本不产出那几根 K 线，
// 而这两种情况从数据上看长得一模一样。本包把【已请求并确认】的区间单独记在
// [Meta].Coverage 里，与数据分开——没有它，增量同步会在每个真实空洞上反复重拉。
//
// 四、港时与 UTC 是两条序列。1D 按香港时间开盘对齐（UTC 16:00），1Dutc 才按
// UTC，6H 及以上都有这个分叉。本包不混存、不互相顶替，收盘判定各按各的时区。
//
// # 未就绪时给 NaN，不给 0
//
// 指标没 warmup 完、[View] 无效（Prev 超出 Lookback）、取了个不存在的键——
// 这些情形一律返回 NaN 而不是 0。0 是个看起来正常的价格，NaN 会沿运算传染，
// 把误用当场暴露出来。循环里用 [View.Ready] 判断是否可以开始决策。
//
// # 时间一律是毫秒
//
// 所有时间戳都是 Unix 毫秒（int64），与 OKX 一致。区间一律左闭右开 [from, to)，
// 其中 to 为 0 表示「直到末尾」。
package tickflow
