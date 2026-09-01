# okx-tickflow-go

用 Go 实现的 OKX 行情数据层：**拉取 → 持久化 → 计算指标 → 向回测引擎提供可步进的视图。**

上游是 [okx-api-v5-go](https://github.com/dream-until-dawn/okx-api-v5-go)（拿数据），
旁边是 [okx-position-simulator-go](https://github.com/dream-until-dawn/okx-position-simulator-go)（记账），
下游是回测引擎（消费视图）。本库**不做交易决策，也不做仓位核算**。

> **状态：v0.3。** 数据层与视图层已完整：同步与持久化、七个内置指标（两套口径）、
> 多周期同步的可步进视图。剩下与记账器对接的适配层，见下方排期。

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

## 技术指标

七个内置指标，全部是流式的——每来一根 K 线调一次 `Update`，代价与历史长度无关：

```go
inds := []indicator.Indicator{
    indicator.MA(20), indicator.EMA(20),
    indicator.MACD(12, 26, 9),
    indicator.KDJ(9, 3, 3),
    indicator.RSI(14), indicator.CCI(20),
    indicator.BOLL(20, 2),
}
for it.Next() {
    for _, ind := range inds {
        vals := ind.Update(it.Candle())   // 未 warmup 完时给 NaN，不是 0
    }
}
```

外部指标只要实现 `indicator.Indicator` 接口，就能和内置的一样用。

跑一遍看看：

```
go run ./examples/indicators -inst BTC-USDT-SWAP -bar 15m -root ./data
```

### 两套口径，差异只有三处

同一个指标名，TradingView 与国内行情软件算出来的数不一样。默认 TV，构造时可换：

```go
indicator.MACD(12, 26, 9, indicator.CN)      // 国内软件口径
indicator.SetDefaultConvention(indicator.CN) // 或改全局默认
```

| 处 | TV | CN | 影响 |
|---|---|---|---|
| **递归平均的播种** | 前 n 个样本的简单平均 | 首个样本 | EMA、MACD、RSI |
| **MACD 柱** | `DIF − DEA` | `2 × (DIF − DEA)` | MACD |
| **KDJ 平滑** | 简单移动平均，无 J 线 | 指数式 `SMA(n,1)` | KDJ |

**MA、CCI、BOLL 两套完全一致**——不为了对称而编造差异。BOLL 的标准差按
**总体 `n`**（不是样本 `n−1`），两套口径都如此。

播种那一条是**暂时**的：影响按 `(1−α)^k` 衰减。实测走满 5760 根 15m 之后，
两套口径的 RSI(14) 已经完全相同；而柱子乘 2 和 KDJ 的平滑方式是**永久**差异。

> TV 口径下的 **J 线是本库补的**——TradingView 的 Stochastic 没有 J 线，
> 这里在两套口径下都按 `J = 3K − 2D` 给出，好让字段结构一致。

### 「增量 == 批量」是结构上成立的

窗口类指标（MA / BOLL / CCI / KDJ）每步在窗口上**重算**，而不是增量维护累加和——
增量维护 `sum` 会随步数累积浮点漂移，几百万根之后就和「把这 n 根拿出来算一遍」
对不上了，而后者正是批量定义。递归类指标本就以递推定义，两种算法是同一个。

代价实测（Ryzen 7 5700X，全部零分配）：MA(20) 11ns、EMA(20) 5ns、MACD 7ns、
BOLL(20) 35ns、CCI(20) 39ns、KDJ 42ns、RSI 7ns。**九个指标一整套 202ns/步**——
百万步的回测在指标上一共花 0.2 秒。

## Feed：可步进的多周期视图

回测引擎消费本库的方式。主周期步进，辅周期只提供**最后一根已收盘**的上下文：

```go
f, _ := tickflow.NewFeed(store, tickflow.Config{
    InstID:   "BTC-USDT-SWAP",
    Base:     "15m",                    // 步进的主周期
    Extra:    []string{"1H", "1D"},     // 辅周期，只读不步进
    Lookback: 5,                        // 视图能往回看几根
    Indicators: map[string][]tickflow.Indicator{
        "15m": {indicator.MA(20), indicator.MACD(12, 26, 9), indicator.RSI(14)},
        "1H":  {indicator.EMA(20)},
        "1D":  {indicator.MA(5, indicator.Named("ma5d"))},
    },
})
defer f.Close()

h, _ := f.Handle("15m", "macd.hist")    // 循环外预解析，热路径省一次 map 查找

for f.Next() {
    v := f.View()
    if !v.Ready() { continue }          // 指标还没 warmup 完

    v.Close()                            // 当前收盘价
    v.At(h)                              // MACD 柱，按句柄取
    v.Prev(3).Close()                    // 前 3 根的收盘价
    f.TF("1D").Ind("ma5d")               // 日线的 MA5——永远是【已收盘】那根
}
if err := f.Err(); err != nil { ... }
```

跑一遍看看（含对「高周期收盘前不可见」的现场验证）：

```
go run ./examples/feed -inst BTC-USDT-SWAP -root ./data
```

**指标按周期挂**，不是拉平成一个列表——1D 的 MA20 和 15m 的 MA20 是完全不同的
东西，混在一起迟早出事。

### `f.TF(bar)` 永远给最后一根已收盘的

这是本库最主要的价值。主周期走到 `08-31 14:30`（这根 14:45 收盘）时：

```
1H  给 13:00 那根（14:00 收盘）——14:00 那根要到 15:00 才收盘，不可见
1D  给 08-29 16:00 那根（08-30 16:00 收盘，港时 08-30 全天）
```

「高周期 K 线在收盘前不可见」是自制回测里最常见、也最难自查的未来函数。
测试同时验两个方向：**不能超前**（看到的必须已收盘）和**不能落后**
（下一根必须还没收盘）——只验一个方向的话，一个「永远返回第一根」的实现也能通过。

### 其余几条

**warmup 自动前推。** `MA(200)` 需要 200 根，Feed 自己往前多读一段喂指标、
不产出步进，使用者不必手工把 `From` 往前挪。没读满时 `v.Ready()` 如实报 false，
而不是给出一个用半截历史算出来的值。

**辅周期默认从库里读独立序列**（OKX 自己算的，最准）。`Aggregate: true` 改为
从主周期聚合，只需同步一个周期，代价是空洞与边界会让结果与交易所的官方 K 线
有偏差——别拿聚合出来的日线去和交易所对数。两种模式的**可见性时点完全一致**，
有测试盯着：不然一拨开关回测结果就变了，而原因藏在没人会查的地方。

**实盘复用同一套代码。** `f.Push(bar, candle)` 手工喂一根，`store` 传 nil 就是
纯实盘形态。WebSocket 收到收盘 K 线后调它，指标与视图的代码和回测完全一样。
ts 必须严格递增且对齐，重复与乱序推送会报错——实盘里这两种都真实存在。

**热路径零分配**（Ryzen 7 5700X）：推进一步 30ns（单周期）/ 65ns（三周期聚合），
`v.At(handle)` 1.7ns、`v.Ind("name")` 10.6ns、`v.Prev(3).Close()` 3.8ns。
`View` 是「一个指针加一个下标」的值类型，`Prev(n)` 只是把下标往回挪。

## 持久化：目录由你指定

默认实现 `store/segfile` 是定长记录文件，零第三方依赖。**落盘位置完全由使用者
指定,库里没有任何默认路径**（空字符串直接报错，不猜）：

```go
store, err := segfile.Open("D:/quant/data")     // 写模式，取排他写锁
store, err := segfile.OpenReadOnly("D:/quant/data")  // 只读，不取锁
defer store.Close()

store.Root()                    // 绝对路径，供打日志
store.Path("BTC-USDT-SWAP", "15m")  // 这条序列的 .dat / .meta 在哪
```

绝对路径、相对路径、尚未创建的多层目录、带空格或中文的路径都可以。**相对路径在
`Open` 时就换算成绝对路径**——之后程序 `os.Chdir` 不会让同一个 Store 指向别处。

### 目录结构

```
<root>/
  .lock                    写锁
  candles/                 K 线的命名空间
    BTC-USDT-SWAP/
      15m.dat              纯定长记录数组，无文件头，offset = i * 64
      15m.meta             JSON，人可读
    ETH-USDT-SWAP/
      1H.dat
      1H.meta
```

`candles/` 那一层是给以后留位的：逐笔成交、盘口深度是形态完全不同的数据，
将来要放进来时不必迁移已有的 K 线。

一条记录 **64 字节**（小端）：`ts int64` + `open high low close vol volCcy
volCcyQuote` 七个 float64。1m 线一年约 33.6MB，五年约 168MB 单文件。

按 ts 升序排列，Seek 时二分。**没有做槽位寻址**——那要求空洞也占 64 字节，
而小币种的 1m 线空洞可能很多；且 `1M` 不定长，除不出槽位号。回测是顺序读，
二分只在起点发生一次。

### 并发：一个写者，多个读者

进程内多读单写。跨进程靠 `Open` 时取的**写锁**：一个数据目录同一时刻只能有
一个写者，第二个 `Open` 当场返回 `ErrLocked`，错误信息里带着占用者的 pid、
主机名和占用时刻——而不是默默把文件写坏。同一进程里对同一目录 `Open` 两次
同样被挡下（两个 Store 各有各的内存锁，它们之间并不互斥）。

进程崩溃会遗留锁文件。本库**不去猜那个进程是否还活着**——跨平台可靠地判断
这件事要引入平台相关的依赖。确认无人使用后 `segfile.ForceUnlock(root)` 清掉。

`OpenReadOnly` 不取锁，可以和写者并存，读到的是**打开那一刻的快照**。这正是
「同步守护进程持续追加 + 回测进程并发读」这个场景，`examples/feed` 与
`examples/indicators` 都是这么打开的。

> ⚠️ **Windows 上有一条实测出来的限制**：只读端持有某条序列时，写者**回填不了
> 那条序列**。回填要把整个文件换掉（写临时文件再改名），而 Windows 的
> `MoveFileEx` 在目标被任何句柄打开时一律失败——实测与 `FILE_SHARE_DELETE`
> 无关，加了也没用，所以本库没有为此写平台特化代码。
>
> **追加不受影响**（按偏移写，不改名），所以日常同步与并发读完全正常；
> 只有**深度回填**需要先让读者退出。回填失败时数据原封未动，序列继续可用。
> Unix 上没有这个限制。

### 换掉它

`Store` 是接口，想接 ClickHouse、SQLite 之类自行实现即可：

```go
type Store interface {
    Append(instID, bar string, cs []Candle) error   // 末尾追加，快路径
    Merge(instID, bar string, cs []Candle) error    // 任意位置，回填走这条
    Iter(instID, bar string, from, to int64) (Iterator, error)
    Range(instID, bar string, from, to int64) ([]Candle, error)
    Meta(instID, bar string) (Meta, error)
    AddCoverage(instID, bar string, r Range) error
    Series() ([]SeriesID, error)
    Close() error
}
```

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
| v0.2 | ✅ 七个内置指标（MA EMA MACD KDJ RSI CCI BOLL）+ 两套口径 + 三层测试 |
| v0.3 | ✅ `Feed` / `View` / 多周期同步 / 实时 `Push` |
| v0.4 | `adapter/okxsim` |
| v1.0 | 文档 + 真实数据端到端验证 |

设计取舍与理由见 [docs/design.md](docs/design.md)。
