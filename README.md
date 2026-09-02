# okx-tickflow-go

用 Go 实现的 OKX 行情数据层：**拉取 → 持久化 → 计算指标 → 向回测引擎提供可步进的视图。**

上游是 [okx-api-v5-go](https://github.com/dream-until-dawn/okx-api-v5-go)（拿数据），
旁边是 [okx-position-simulator-go](https://github.com/dream-until-dawn/okx-position-simulator-go)（记账），
下游是回测引擎（消费视图）。本库**不做交易决策，也不做仓位核算**。

> **状态：v1.2。** 公开 API 已收口。指标默认口径与 OKX 平台一致（实测确认）。
> 支持标记价 / 指数价序列——回测里建模强平必须用标记价，见下。两年真实数据端到端验收过：同步的根数精确到
> 个位、重跑零请求、自聚合与 OKX 官方逐位相同、11520 步里对辅周期做了 22974 次
> 防未来函数检查零违规。详见 [docs/design.md](docs/design.md) 的「v1.0 验收」。

## 先读这个

**[docs/contract.md](docs/contract.md) —— 能力、边界、置信度与已知风险。**
引入本库前值得花五分钟读完那一份：它把每条结论标了**置信度**（实测 / 有测试 /
推定），并列了一张已知风险表，其中「是否静默」那一列最要紧——会报错的问题不可怕，
不报错的才是。

一句话版本：

| | |
|---|---|
| **做** | 拉 K 线（成交价 / 标记价 / 指数价）· 增量同步不重复拉 · 定长文件落盘 · 七个内置指标（两套口径）· 可步进的多周期视图 · 喂给记账内核 |
| **不做** | 资金费率（**刻意**，见下）· 逐笔与盘口 · 交割合约换月拼接 · 交易决策与仓位核算 · 多进程并发写 |
| **最该知道的一条** | 强平判据必须用**标记价**。用成交价会让影线造出真实不会发生的强平，而且是假阴性。配 `FeedConfig.MarkStore`，`simbar.Advance` 会自动带上 |

## 目录

- [安装](#安装) · [快速开始](#快速开始) · [三条硬保证](#三条硬保证)
- [不提供资金费率](#-本库不提供资金费率且这是刻意的) · [技术指标](#技术指标) · [Feed 多周期视图](#feed可步进的多周期视图)
- [标记价](#标记价回测里建模强平必须用它) · [持久化](#持久化目录由你指定) · [与记账内核对接](#与记账内核对接)
- [实测记录](#实测记录) · [已知的空白](#已知的空白) · [排期](#排期)

## 安装

```
go get github.com/dream-until-dawn/okx-tickflow-go
```

`go 1.22`。主模块的依赖树**只有 okx-api-v5-go 一个**：

```
$ go list -m all
github.com/dream-until-dawn/okx-tickflow-go
github.com/dream-until-dawn/okx-api-v5-go v0.1.0
github.com/gorilla/websocket v1.5.3
```

`shopspring/decimal` 与整个记账内核隔离在 `adapter/simbar` 这个**独立嵌套模块**
里，只想拉数据的使用者不会被牵连（实测：`decimal` 在主模块的依赖图里出现 0 次）。

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
可以在解读结果时统一扣减。`adapter/simbar` 的 `ToBar` 因此永远不填 `Funding`，
本包也**不提供**设置它的选项；自备数据的使用者可以在拿到 `Bar` 之后自行赋值——
那是明确的一步动作，而不是本库替谁做的默认。

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

### 两套口径，默认与 OKX 一致

同一个指标名，TradingView 与国内行情软件算出来的数不一样。

**默认是 CN 口径，因为 OKX 自己的行情界面用的就是这一套。** 2026-09-01 拿
ETH-USDT-SWAP 的日线在 OKX 平台上逐行比对了 KDJ(9,3,3) 与 MACD(12,26,9)：
CN 对得上，TV 对不上。那十行值已固化成回归测试（`indicator/okx_convention_test.go`），
把一次人工核对变成了永久的守卫。

```go
indicator.MACD(12, 26, 9)                    // CN 口径（默认，与 OKX 一致）
indicator.MACD(12, 26, 9, indicator.TV)      // TradingView 口径
indicator.SetDefaultConvention(indicator.TV) // 或改全局默认
```

> **v1.1.0 起默认由 TV 改为 CN。** 若你在 v1.0.0 上跑过、且没有显式指定口径，
> MACD 的柱会变成原来的两倍、KDJ 的 K/D/J 会变——那不是回归，是默认值终于对上了
> 交易所。想保持旧行为就显式传 `indicator.TV`。

| 处 | TV | CN | 影响 |
|---|---|---|---|
| **递归平均的播种** | 前 n 个样本的简单平均 | 首个样本 | EMA、MACD、RSI |
| **MACD 柱** | `DIF − DEA` | `2 × (DIF − DEA)` | MACD |
| **KDJ 平滑** | 简单移动平均，无 J 线 | 指数式 `SMA(n,1)` | KDJ |

**MA、CCI、BOLL 两套完全一致**——不为了对称而编造差异。BOLL 的标准差按
**总体 `n`**（不是样本 `n−1`），两套口径都如此。

播种那一条是**暂时**的：影响按 `(1−α)^k` 衰减。实测走满 5760 根 15m 之后，
两套口径的 RSI(14) 已经完全相同，500 根日线之后 MACD 的 DIF/DEA 也完全相同；
而柱子乘 2 和 KDJ 的平滑方式是**永久**差异——这也正是能用来判别 OKX 用哪套口径的
两处。

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
f, _ := tickflow.NewFeed(store, tickflow.FeedConfig{
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

**预热自动前推，而且按「已收敛」算，不是按「有定义」。** Feed 自己往前多读一段
喂指标、不产出步进，使用者不必手工把 `From` 往前挪。

这两件事对 `MA(20)` 是一回事，对递归类指标差一到两个数量级：**MACD(12,26,9)
国内口径的 `Warmup()` 报 1，实测要 428 根才收敛；只喂 4 根时 dif 的相对误差是
1.01——值错了一倍。** 所以 `View.Ready()` 按收敛判，历史不够时它一直是 false。
要那个弱判据（有定义但未必收敛）用 `View.Defined()`。详见
[contract.md](docs/contract.md)。

**辅周期默认从库里读独立序列**（OKX 自己算的）。`Aggregate: true` 改为从主周期
聚合，只需同步一个周期。

聚合的失真**只来自缺根**——实测把 120 天的 15m 聚成 1H，与 OKX 官方的 1H 逐位
比对 2879 根完整小时线：OHLC 四个字段共 11516 项、外加成交量，**全部零不一致**。
底层齐全时聚合是精确的；只有当某根高周期缺了几根底层 K 线时才会失真，而那种
小时在这 120 天里只有 2 个（数据两端）。

两种模式的**可见性时点完全一致**，有测试盯着：不然一拨开关回测结果就变了，
而原因藏在没人会查的地方。

**实盘复用同一套代码。** `f.Push(bar, candle)` 手工喂一根，`store` 传 nil 就是
纯实盘形态。WebSocket 收到收盘 K 线后调它，指标与视图的代码和回测完全一样。
ts 必须严格递增且对齐，重复与乱序推送会报错——实盘里这两种都真实存在。

**热路径零分配**（Ryzen 7 5700X）：推进一步 30ns（单周期）/ 65ns（三周期聚合），
`v.At(handle)` 1.7ns、`v.Ind("name")` 10.6ns、`v.Prev(3).Close()` 3.8ns。
`View` 是「一个指针加一个下标」的值类型，`Prev(n)` 只是把下标往回挪。

## 标记价：回测里建模强平必须用它

OKX 的强平按**标记价**判定，不是成交价。缺了标记价，
[okx-position-simulator-go](https://github.com/dream-until-dawn/okx-position-simulator-go)
会退回用最新成交价顶替——于是**影线会制造出真实不会发生的强平**。对尾部风险就是
强平的策略（比如做多网格）尤其致命，而且是**假阴性**，结果里不留任何痕迹。

标记价由指数价平滑而来，比成交价平稳。实测 200 根 BTC-USDT-SWAP 的 1H：
平均振幅 **成交价 0.6433% vs 标记价 0.6300%**，标记价更窄的占 146/200。

```
go run ./examples/sync -inst BTC-USDT-SWAP -bar 15m -days 30 -root ./data -kind mark
```

```go
store, _ := segfile.OpenReadOnly(root)                 // 成交价
mark, _  := segfile.OpenReadOnly(root, segfile.Mark)   // 标记价

f, _ := tickflow.NewFeed(store, tickflow.FeedConfig{
    InstID: "BTC-USDT-SWAP", Base: "15m",
    MarkStore: mark,                  // ← 标记价跟着主周期锁步推进
})

for f.Next() {
    v := f.View()
    bar, _ := simbar.ToBar(inst, v.Candle(), simbar.WithMarkPx(v.MarkPx()))
    sim.Advance(bar)
}
```

**对齐规则是「同 ts」而不是「最后一根已收盘」**——标记价 K 线与主周期这一根是
同时收盘的，它的收盘价在这一刻就已知，用它不构成未来函数。主周期有空洞时标记价
不会错位到别的时刻上去（错位一根的标记价比没有更糟，它看起来是对的）。

`simbar.Advance` **会自动从视图里带上标记价**，不必每次手写 `WithMarkPx`：

```go
step, err := simbar.Advance(sim, inst, v)   // 视图有标记价就自动带上
```

标记价在某个时刻缺根时 `v.MarkPx()` 返回 **NaN**，等同于不设。

> **记账内核 okx-position-simulator-go 从 v1.0.0 起默认【拒绝】缺标记价的 Bar**
> ——不再静默退回成交价。确实拿不到数据时才打开它的
> `Config.AllowMarkPxFallback`，打开就是接受这份偏差。

### ⚠️ 历史边界随【周期】而变

标记价历史**不如成交价长**，而且**两条序列的边界都随 `bar` 变**。生产环境二分
查边界（港时）：

| 合约 | `bar` | 成交价最早 | 标记价最早 |
|---|---|---|---|
| BTC-USDT-SWAP | `1D` | 2019-11-28 | **2020-01-01** |
| BTC-USDT-SWAP | `1m` / `15m` / `1H` | 2019-12-16 | **2020-01-03** |
| ETH-USDT-SWAP | `1D` | 2019-11-30 | **2020-01-01** |
| ETH-USDT-SWAP | `1m` / `15m` / `1H` | 2019-12-25 | **2020-01-03** |

标记价那条硬线**跨合约一致**，分日线档与日内档两档（日内晚两天）；成交价的边界
各合约不同，但同样分两档，日内档要晚上十几到二十几天。

**按日线的数去规划日内回测，差出来的那一段根本不存在。**

回测起点早于对应边界时标记价拿不到——那时打开 `AllowMarkPxFallback` 是**正当的**，
不是将就。详见 [contract.md](docs/contract.md)，那里也记着这条**改过两次**的经过：
第一次错在拿模拟盘测（两条一起截断被读成一样深），第二次错在只量了 `1D` 而表里
没有 `bar` 列，被下游照抄。

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
  candles/                 成交价 K 线
    .lock                  写锁（在命名空间之内）
    BTC-USDT-SWAP/
      15m.dat              纯定长记录数组，无文件头，offset = i * 64
      15m.meta             JSON，人可读
  mark-candles/            标记价 K 线
    .lock
    BTC-USDT-SWAP/
      15m.dat
      15m.meta
  index-candles/           指数价 K 线
```

```go
segfile.Open(root)                 // candles/
segfile.Open(root, segfile.Mark)   // mark-candles/
segfile.Open(root, segfile.Index)  // index-candles/
```

标记价的 `BTC-USDT-SWAP/15m` 与成交价的 `BTC-USDT-SWAP/15m` 是**两条不同的
序列，同一个 `(instId, bar)` 键**。用命名空间分开，而不是拼一个
`BTC-USDT-SWAP:mark` 之类的合成 instId——合成键会渗进 `Meta`、`Coverage`、
`SeriesID`，以后再拆就是破坏性变更。

**写锁也在命名空间之内**，所以同一进程里同时开一个成交价 Store 和一个标记价
Store 不会自己把自己锁住。

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

## 与记账内核对接

`adapter/simbar` 把 K 线转成 [okx-position-simulator-go](https://github.com/dream-until-dawn/okx-position-simulator-go)
的 `Bar`。它是**独立嵌套模块**——只想拉行情的使用者不会被拖进 `decimal` 与整个
记账内核。

```
go get github.com/dream-until-dawn/okx-tickflow-go/adapter/simbar
```

```go
import (
    okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
    "github.com/dream-until-dawn/okx-tickflow-go/adapter/simbar"
)

for feed.Next() {
    v := feed.View()
    if !v.Ready() { continue }

    step, err := simbar.Advance(sim, "BTC-USDT-SWAP", v)   // 转换 + 推进一步
    // step.Fills / step.Liquidations / step.Canceled ...
}
```

也可以只做转换：`simbar.ToBar(instID, candle)` / `ToBars(instID, candles)`。

| `okxsim.Bar` 字段 | 取值 |
|---|---|
| `High` / `Low` / `Ts` | 直取 |
| `Last` | `Close`——记账内核用它撮合限价单 |
| `MarkPx` / `IdxPx` | 留空，除非 `WithMarkPx` / `WithIdxPx` 给出 |
| `Funding` | **永远 nil**，见上一节 |

**返回 `error` 而不是直接给 `Bar`**：`decimal.NewFromFloat` 碰上 NaN 会 panic，
而 NaN 恰恰是「用了一个无效 `View`」的正常产物。与其让它从库深处炸出来，
不如在这里拦住并说清楚。

> 包名叫 `simbar` 而不是 `okxsim`：记账内核自己的包名就是 `okxsim`，同名的话
> 每个同时用到两边的使用者都得起别名。

### float64 → decimal 无损，这条是测出来的

行情层用 float64（指标计算是热路径），记账层用 decimal（钱的事不能有误差）。
转换收在 `simbar.Dec` 一处，走 `decimal.NewFromFloat`——它取能往返回原值的最短
十进制表示，OKX 的价格有效数字远在 float64 的 15 位之内。

测试覆盖 300 根真实 15m 行情的全部 OHLCV，加上从 `1e-8` 到 `1234567.89` 的各量级
刻度，断言两条：转回 float64 **按位相等**，且 decimal 的十进制形态与 float64 的
最短表示**逐位相同**。端到端还验了一遍：一段真实行情走完，记账内核记下的最新价
与行情层最后那根的收盘价逐位相同。

### 完整一条链

```
cd adapter/simbar && go run ./examples/backtest -root ../../data
```

行情库 → 带指标的步进视图 → 记账内核，均线金叉死叉跑 5741 步、400 笔成交。
策略本身是最土的那种，只为把链路跑通——本库不做交易决策。

> 嵌套模块的代价：仓库根目录的 `go build ./...` 与 `go test ./...` 覆盖不到它，
> 要单独 `cd adapter/simbar` 执行。

## 已知的空白

完整的**已知风险表**（含「是否静默」一列）在
[docs/contract.md](docs/contract.md)。这里只列剩下的空白：

**2D / 3D 的锚点是推定的。** OKX 没有文档说明哪两天、哪三天归为一根。实测对上了，
但交易所改起来不必通知谁。真出问题时 `SyncReport.Misaligned` 会不为零，
用 `RegisterFixedPeriod` 覆盖即可。

**`history-candles` 的深度上限未知。** 只知道**至少** 6 年。更早的区间会出现在
`SyncReport.Gaps` 里，而不是静默给空。

（标记价与指数价此前也在这张单子上，v1.2.0 已补齐。竞态检测此前也在，
装了 MinGW-w64 之后闭环，还顺带抓到一个真的数据竞争。）

### 竞态检测

已经跑过了，`go test -race ./...` 两个模块全绿。

但**拿单 goroutine 的测试跑 `-race` 几乎什么都证明不了**——竞态检测只报实际发生过
的竞争。所以另写了会真并发起来的测试（一写六读同压 Store、多游标同时遍历、并发
构造指标、多个独立 Feed 并行推进），并检查读到的记录自洽而不只是「没报错」。

这一跑就抓到一个真的：`indicator` 的全局默认口径是个无保护的包级变量，
已改成 `atomic.Int32`。

> Windows 上跑 `-race` 需要 cgo，装一套 MinGW-w64 即可：
> `winget install BrechtSanders.WinLibs.POSIX.UCRT`（要 POSIX 线程那个变体）。

## 排期

| 版本 | 内容 |
|---|---|
| v0.1 | ✅ `Candle` / `Period` / `Source` / `Store`(segfile) / `Syncer` |
| v0.2 | ✅ 七个内置指标（MA EMA MACD KDJ RSI CCI BOLL）+ 两套口径 + 三层测试 |
| v0.3 | ✅ `Feed` / `View` / 多周期同步 / 实时 `Push` |
| v0.4 | ✅ 数据目录、写锁与只读模式、`candles/` 命名空间 |
| v0.5 | ✅ `adapter/simbar`——与记账内核对接，三库端到端跑通 |
| v1.0 | ✅ API 收口、根包文档、两年真实数据端到端验收 |
| v1.1 | ✅ 默认口径改为 CN——实测确认 OKX 平台用的就是这一套 |
| v1.2 | ✅ 标记价 / 指数价序列，命名空间分离，Feed 锁步对齐 |

设计取舍与理由见 [docs/design.md](docs/design.md)。
