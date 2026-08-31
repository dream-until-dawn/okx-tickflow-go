# okx-tickflow-go

用 Go 实现的 OKX 行情数据层：**拉取 → 持久化 → 计算指标 → 向回测引擎提供可步进的视图。**

上游是 [okx-api-v5-go](https://github.com/dream-until-dawn/okx-api-v5-go)（拿数据），
旁边是 [okx-position-simulator-go](https://github.com/dream-until-dawn/okx-position-simulator-go)（记账），
下游是回测引擎（消费视图）。本库**不做交易决策，也不做仓位核算**。

> **状态：v0.1。** 已可用：任意标的任意周期的历史 K 线同步与持久化。
> 指标与视图（`Feed`）尚未实现，见 [设计草稿](docs/design.md) 与下方排期。

## 安装

```
go get github.com/dream-until-dawn/okx-tickflow-go
```

`go 1.22`。主模块的依赖树**只有 okx-api-v5-go 一个**——`shopspring/decimal`
与整个记账内核隔离在 `adapter/okxsim` 子模块里，只想拉数据的使用者不会被牵连。

## 快速开始

```go
client, _ := okx.NewClient()
src, _ := okxsource.New(client)
store, _ := segfile.Open("./data")
defer store.Close()

rep, err := tickflow.NewSyncer(src, store).Sync(ctx, tickflow.SyncRequest{
    InstID: "BTC-USDT-SWAP",
    Bar:    "15m",
    From:   time.Now().AddDate(0, 0, -30).UnixMilli(),
})
// rep.Added = 2880   rep.Fetches = 1   rep.Gaps = 无

cs, _ := store.Range("BTC-USDT-SWAP", "15m", from, to)
```

可运行的完整例子见 [examples/sync](examples/sync/main.go)：

```
go run ./examples/sync -inst BTC-USDT-SWAP -bar 15m -days 30 -root ./data
```

**重复跑不会重复拉。** 已确认过的区间记在 `meta` 的 `coverage` 里，
第二次跑只补上次之后新收盘的那几根（实测 0 请求、1ms 返回）。

## 三条硬保证

**未完结的 K 线绝不进库。** OKX 的 `/market/candles` 会把当前还在走的那根
一并返回。用一根还在变的 K 线的「收盘价」做决策，等于偷看了这根走完之后才
知道的信息——这是回测里最经典的未来函数。本库在 `Source` 那一层就丢弃它，
并且 `coverage` 的末端只记到当前那根的**开盘时刻**：不然等它收盘后就再也
不会去补，那根 K 线会永久停在一个半截的值上，而且悄无声息。

**空洞与「没拉过」分得清。** 小币种或维护期，OKX 根本不产出那几根 K 线。
「这段没有数据」和「这段还没拉」从数据上看长得一模一样。本库把**已请求并
确认**的区间单独记在 `coverage` 里，与数据分开。没有它，增量同步会在每一个
真实空洞上反复重拉，永远收敛不了。

**港时与 UTC 是两条序列。** `1D` 按香港时间开盘对齐（UTC 16:00），`1Dutc`
才按 UTC。6H 及以上都有这个分叉。本库不混存、不互相顶替，收盘判定各按各的
时区。17 个周期的对齐已用真实数据实测（见 [实测记录](docs/design.md)）。

## ⚠️ 本库不提供资金费率，且这是刻意的

OKX 的历史资金费率**只保留约 3 个月**。技术上可以从今天起归档，但
**部分区间有、更早的区间没有，比全都没有更糟**：

它会让同一个策略在不同时间段的回测收益**不可比**，而这种不可比是隐性的，
很容易被误读成「策略在不同市况下的表现差异」。

全都不计至少是**一致**的偏差——系统性高估多头持仓收益、低估空头，方向已知，
可以在解读结果时统一扣减。`adapter/okxsim` 的 `ToBar` 因此永远不填 `Funding`。

## 存储格式

默认实现 `store/segfile` 是定长记录文件，零第三方依赖：

```
data/BTC-USDT-SWAP/
  15m.dat     纯定长记录数组，无文件头，offset = i * 64
  15m.meta    JSON，人可读
```

一条记录 **64 字节**（小端）：`ts int64` + `open high low close vol volCcy
volCcyQuote` 七个 float64。1m 线一年约 33.6MB，五年约 168MB 单文件。

按 ts 升序排列，Seek 时二分。**没有做槽位寻址**——那要求空洞也占 64 字节，
而小币种的 1m 线空洞可能很多；且 `1M` 不定长，除不出槽位号。回测是顺序读，
二分只在起点发生一次。

`Store` 是接口，想接 ClickHouse、SQLite 之类自行实现即可。

## 实测记录

文档里查不到、只能打真实接口问出来的事都记在
[docs/design.md](docs/design.md) 的「实测记录」一节里，
其中最要紧的一条：**history-candles 至少能回溯 6 年**（1m 亦然）。
1m 线六年约 315 万根，远超回填的默认内存上限，必须分窗口调用。

可重跑：

```
TICKFLOW_LIVE=1 go test ./source/okxsource/
```

## 排期

| 版本 | 内容 |
|---|---|
| v0.1 | ✅ `Candle` / `Period` / `Source` / `Store`(segfile) / `Syncer` |
| v0.2 | 七个内置指标（MA EMA MACD KDJ RSI CCI BOLL）+ 两套口径 + golden test |
| v0.3 | `Feed` / `View` / 多周期同步 / 实时 `Push` |
| v0.4 | `adapter/okxsim` |
| v1.0 | 文档 + 真实数据端到端验证 |

设计取舍与理由见 [docs/design.md](docs/design.md)。
