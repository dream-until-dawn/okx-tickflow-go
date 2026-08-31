# okx-tickflow-go 设计草稿

> 状态：草稿，待实现。本文记录**取舍与理由**，不只是接口清单——
> 半年后回头看，能想起来「为什么不这么做」比「做了什么」更重要。

## 定位

一句话职责：**「OKX 行情 → 带指标的、可步进的历史视图」的数据层。**

上游是 [okx-api-v5-go](https://github.com/dream-until-dawn/okx-api-v5-go)（拿数据），
下游是回测引擎（消费视图），旁边是
[okx-position-simulator-go](https://github.com/dream-until-dawn/okx-position-simulator-go)（记账）。
本库**不做交易决策，不做仓位核算**——那是另外两个库的事。

```
okx-api-v5-go ──拉取──> okx-tickflow-go ──视图──> 回测引擎 ──成交──> okx-position-simulator-go
                          （本库）                                      （记账）
```

---

## 一、分层与包结构

```
okx-tickflow-go/
├── candle.go            Candle / Period（周期解析、边界计算）
├── source.go            Source 接口
├── source/okxsource/    Source 的 okx-api-v5-go 实现
├── store.go             Store / Iterator 接口
├── store/segfile/       默认实现：定长文件
├── sync.go              Syncer：按 coverage 只拉缺失区间
├── indicator/           Indicator 接口 + MA EMA MACD KDJ RSI CCI BOLL
├── feed.go              Feed / View / 多周期同步
└── adapter/okxsim/      → okxsim.Bar（独立嵌套模块，隔离 decimal 依赖）
```

**主模块依赖只有 okx-api-v5-go 一个**（它自己只依赖 gorilla/websocket）。
`shopspring/decimal` 与整个模拟器包被隔离在 `adapter/okxsim` 这个嵌套模块里——
只想拉数据存数据的使用者不该被迫拉进一个记账内核。这跟
okx-position-simulator-go 自己把 `net/http` 隔离在 `refdata/live`、
把对拍工具做成独立嵌套模块是同一个思路。

> 嵌套模块的代价：本地联调需要 `go.work`（已在 `.gitignore` 里），
> 且 adapter 引用主模块要走已发布的版本号或 replace。

Go 版本取 **1.22**，与两个上游对齐。因此 `Iterator` 用显式接口而非
`iter.Seq`——为一个语法糖抬高使用者的 Go 版本门槛不划算。

---

## 二、数据模型

```go
// Candle 是一根已完结的 K 线。
//
// OHLCV 用 float64 而非 decimal：指标计算是本库的热路径，decimal 会慢一到两个
// 数量级且没有任何精度收益——OKX 的价格位数远在 float64 的 15 位有效数字之内。
// 与 simulator 的 decimal 世界的转换收在 adapter/okxsim 一个地方。
type Candle struct {
    Ts          int64   // 开盘时间，毫秒
    Open        float64
    High        float64
    Low         float64
    Close       float64
    Vol         float64 // 张 / 基础货币
    VolCcy      float64 // 基础货币
    VolCcyQuote float64 // 计价货币
}
```

**未完结的 K 线（上游 `Confirm == false`）绝不进入本库的任何一层。**
在 `Source` 那一层就丢弃。这是回测里最经典的未来函数来源：用一根还在变的
K 线的「收盘价」做决策，等于偷看了这根 K 线走完之后才知道的信息。

### Period：不硬编码周期表

OKX 会加新周期，硬编码一张 bar 列表意味着每次都要跟着改。本库**把 bar 字符串
原样透传给 OKX**，自己只需要知道两件事：

```go
type Period struct {
    Bar  string
    // Next 给定一根 K 线的开盘 ts，返回下一根的开盘 ts——也就是本根的收盘时刻。
    //
    // 定长周期就是 ts + 步长；1M/3M 这类按自然月走日历。统一成 Next 而不是
    // 「步长」，是因为月线不定长，一个 Duration 表达不了它。
    Next func(ts int64) int64
}

func ParsePeriod(bar string) (Period, error)
func RegisterPeriod(bar string, next func(int64) int64)   // 未知周期的逃生口
```

**时区对齐必须由 Period 携带。** `1D` 按香港时间 UTC+8 对齐，`1Dutc` 才是 UTC，
两者是**两条不同的序列**，不可混存、不可互相顶替。收盘判定也各按各的时区。
`1s`~`4H` 无此问题（天然 UTC epoch 对齐）。

---

## 三、拉取

```go
type Source interface {
    // Fetch 返回 [from, to) 内的 K 线，按 ts 升序，只含已完结的。
    Fetch(ctx context.Context, req FetchRequest) ([]Candle, error)
}
```

`source/okxsource` 的实现要消化上游的四个粗糙面：

| 问题 | 处理 |
|---|---|
| OKX 返回**倒序**（第 0 条最新） | 翻正为升序 |
| 分页语义反直觉：`After`=更旧，`Before`=更新 | 封在内部，对外只暴露 `[from, to)` |
| 单次最多 ~300 条 | 自动翻页直到覆盖请求区间 |
| 近端 / 远端是两个端点 | 近端走 `Candles`，远端走 `HistoryCandles`，自动路由 |
| 含未完结的当前一根 | 丢弃 `Confirm == false` |

限速走上游的 `WithLimiter` 注入点，重试上游已有，本库不重复造。

> ⚠️ **history-candles 的可回溯深度按周期不同、且 OKX 未文档化。**
> 1m 能回溯多久必须实测。`Syncer` 遇到「请求了更早的区间但返回空」时，
> 应把该区间记为「已确认无数据」并停止继续往前，而不是无限翻页。

---

## 四、持久化

### 为什么是自研定长文件

K 线是**定长、追加、按时间有序**的负载，和列式定长文件完美契合。
其余选项被排除的理由：

- `mattn/go-sqlite3` / DuckDB — 需要 CGO，本机无 C 编译器
- `modernc.org/sqlite` — 纯 Go 可行，但几十 MB 的生成代码塞进一个「数据层库」
  的依赖树里，代价与收益不成比例
- bbolt — 可行的折中，但 B+ 树页会让文件比裸数据膨胀，且我们不需要事务
- InfluxDB / TimescaleDB — 需要外部服务，与「库」的定位冲突

`Store` 是接口，上面这些谁想接都能接。

### 布局

```
data/
  BTC-USDT-SWAP/
    1m.dat     纯定长记录数组，无文件头，offset = i * 64
    1m.meta    JSON，人可读
```

`.dat` **不放文件头**，就是纯粹的记录数组——这样 `offset = i*64` 不用加偏移，
二分和随机访问都最干净。magic / version / recordSize 全放 `.meta`。

一条记录 **64 字节**（小端）：

```
ts int64 | open high low close vol volCcy volCcyQuote  (7 × float64)
   8B    |                    56B
```

1m 线一年 ≈ 525,600 根 ≈ 33.6 MB；五年 ≈ 168 MB 单文件。
**v1 不分段。** 现代文件系统扛这个规模毫无压力，分段带来的段索引、跨段查询、
段边界处理都是纯粹的复杂度。等真的需要了再加。

### 为什么是二分而不是槽位寻址

初稿想按 `(ts - baseTs) / 步长` 做 O(1) 槽位寻址。放弃，两个原因：

1. **空洞要占位。** 小币种或维护期 OKX 根本不产出该根 K 线，槽位方案下这些
   空洞每个仍占 64 字节。1m 线上空洞比例可能很高。
2. **月线不成立。** `1M` 不定长，除不出槽位号。

改成「定长记录按 ts 升序 + Seek 时二分」：空洞不占空间、所有周期统一。
性能上没有真实损失——回测是**顺序读**，二分只在起点 Seek 时发生一次。

### coverage：本设计里最容易被忽略、也最要命的一块

```json
{
  "instId": "BTC-USDT-SWAP", "bar": "1m",
  "magic": "TKFL", "version": 1, "recordSize": 64,
  "count": 2628000, "firstTs": 1609459200000, "lastTs": 1767225600000,
  "coverage": [[1609459200000, 1767225600000]],
  "updatedAt": 1767225600000
}
```

`coverage` 记录**「我请求过这个区间并已确认」**，与数据本身分开存。

没有它就无法区分两种情况：**「这根 K 线 OKX 根本没产出」** 和
**「我还没拉过这一段」**。靠数据的时间戳连续性去推断，会让增量同步在每一个
真实空洞上反复重拉，永远收敛不了。

### 接口

```go
type Store interface {
    // Append 追加已完结 K 线。要求按 ts 升序且大于当前 lastTs；重复 ts 忽略。
    Append(instID, bar string, cs []Candle) error

    // Iter 返回 [from, to) 的游标。Feed 走这条——大范围回测不该把
    // 几百万根一次性读进内存。
    Iter(instID, bar string, from, to int64) (Iterator, error)

    // Range 是 Iter 的便捷包装，小范围查询用。
    Range(instID, bar string, from, to int64) ([]Candle, error)

    Meta(instID, bar string) (Meta, error)

    // Replace 覆盖已有区间。罕见路径——OKX 偶尔会修正历史 K 线。
    Replace(instID, bar string, cs []Candle) error

    Close() error
}

type Iterator interface {
    Next() bool
    Candle() Candle
    Err() error
    Close() error
}
```

**并发：v1 只保证进程内安全**（`sync.RWMutex`，单 writer 多 reader）。
跨进程另外用 best-effort 锁文件（内含 PID + 时间戳，便于识别陈旧锁），
但不承诺——跨进程写同一份数据本身就该由使用者规避。

### Syncer

```go
func (s *Syncer) Sync(ctx context.Context, req SyncRequest) (SyncReport, error)
```

读 `Meta.coverage` → 求出缺失区间 → 只拉这些 → 落库 → 合并 coverage。
`SyncReport` 报告新增根数、HTTP 请求次数、以及确认为空洞的区间。

---

## 五、指标

```go
type Indicator interface {
    Name() string              // 视图 key，默认 "ema20" / "macd"；可 .As("fast") 改名
    Fields() []string          // 单输出返回 nil；MACD 返回 ["dif","dea","hist"]
    Warmup() int               // 需要多少根才有有效值
    Update(c Candle) []float64 // 复用内部切片，热路径零分配
    Reset()
}
```

**warmup 未满时返回 `NaN`，不返回 nil 也不返回 0。**
NaN 在列式缓冲里天然占位；更要紧的是它会沿运算传染，能把「拿未就绪的指标去
比较」这类误用**暴露出来**，而返回 0 会让策略在开头几十步安静地做出错误决策。

视图 key 展平：单输出用 `Name()`，多输出用 `Name() + "." + field`——
`"ema20"`、`"macd.dif"`。**同名冲突在 `NewFeed` 构造时报错**，不留到运行期。

### 两套口径

两套都内置，构造时选，默认 `TV`：

```go
indicator.MACD(12, 26, 9)                       // TradingView 口径
indicator.MACD(12, 26, 9).With(indicator.CN)    // 国内软件口径
indicator.SetDefaultConvention(indicator.CN)    // 或改全局默认
```

`With` 返回新实例，不改原对象——指标实例带状态，就地改口径会是个隐蔽的坑。

**实际有差异的只有三处**，其余两套一致。不为了对称而编造差异：

| 处 | TV | CN |
|---|---|---|
| **EMA 播种** | 首值用 `SMA(n)` 播种 | 首值播种（`Y₀ = X₀`） |
| **MACD 柱** | `DIF - DEA` | `2 × (DIF - DEA)` |
| **KDJ 平滑** | `%K = SMA(RSV,3)`、`%D = SMA(%K,3)`，**简单均**，无 J 线 | `K = SMA(RSV,3,1)`、`D = SMA(K,3,1)`，**指数式**，`J = 3K - 2D` |

两套一致的部分：

- **RSI** — 两边都是 Wilder 平滑（通达信的 `SMA(X,N,1)` 就是 α=1/N 的 RMA）
- **CCI** — `TP = (H+L+C)/3`，`CCI = (TP - MA(TP,n)) / (0.015 × 平均绝对偏差)`
- **MA / BOLL 中轨** — 简单移动平均

⚠️ **BOLL 的标准差用总体 `n` 还是样本 `n-1`，两边可能不同，需实测校准后再定。**
TradingView 的 `ta.stdev` 是总体；通达信 `STD` 疑为样本，未经验证，实现前先对数。
未验证的事不写进默认值。

### 一致性保证

增量式 `Update` 与「一次性喂全量历史」必须给出**逐位相同**的结果。
配 golden test：用真实的 OKX 历史数据跑一遍，结果存成基线文件。

内置：`MA(n)` `EMA(n)` `MACD(f,s,sig)` `KDJ(n,m1,m2)` `RSI(n)` `CCI(n)` `BOLL(n,k)`。

外部指标只需实现 `Indicator` 接口。**v1 不做字符串规格解析与注册表**
（`"ema:20"` 这种配置驱动的写法留到 v2）——构造函数已经够用，先别急着上抽象。

---

## 六、Feed 与视图

```go
f, err := tickflow.NewFeed(store, tickflow.Config{
    InstID: "BTC-USDT-SWAP",
    Base:   "15m",                      // 步进的主周期
    Extra:  []string{"1H", "1D"},       // 辅周期，只读不步进
    From:   t0, To: t1,
    Indicators: map[string][]indicator.Indicator{
        "15m": {indicator.EMA(20), indicator.MACD(12, 26, 9)},
        "1D":  {indicator.MA(20).As("ma20d")},
    },
    Lookback: 30,                       // 视图可回看的根数
})

for f.Next() {
    v := f.View()
    v.Ts(); v.Close()
    v.Ind("ema20")
    v.Prev(3).Ind("macd.dif")           // 前 3 根

    d := f.TF("1D")                     // 辅周期视图
    d.Ind("ma20d"); d.Prev(2).Close()

    if !v.Ready() { continue }          // 还有指标没 warmup 完
}
```

**指标按周期挂**——1D 的 MA20 和 15m 的 MA20 是完全不同的东西，
挂在一个扁平列表里迟早出事。

### 三条硬保证

**1. `f.TF(bar)` 永远返回最后一根已收盘的 K 线。**

主周期走到 `2026-01-15 10:15` 时，`f.TF("1D")` 给的是 **01-14** 那根，
不是当天那根还没走完的。这是本库最主要的价值——「高周期 K 线在收盘前不可见」
是自制回测里最常见、也最难自查的未来函数。

收盘判定用 `Period.Next(ts) <= 当前主周期 K 线的 ts`，各周期按各自的时区边界。

**2. warmup 自动前推。**

`MA(200)` 需要 200 根。Feed 内部按 `max(warmup)` 自动向前多读一段历史喂指标、
但不产出步进，使用者不必手工把 `From` 往前挪。多读的量会留余量以应对空洞。
`Config.NoAutoWarmup` 可关掉。

**3. `View` 是值类型句柄，热路径零分配。**

```go
type View struct {
    w *window   // 列式环形缓冲：[]float64 per field + []Candle
    i int       // 环形索引
}
func (v View) Prev(n int) View   // 只是换索引，栈上分配
```

`v.Ind("ema20")` 有一次 map 查找。热路径用预解析句柄绕开：

```go
h := f.Handle("15m", "ema20")   // 循环外
v.At(h)                          // 循环内，零查找
```

两套 API 并存：`Ind` 图方便，`At` 图快。

### 辅周期的数据来源

**默认从 Store 读该周期的独立序列**（OKX 自己算的，最准）。
这要求先 `Sync` 过那个周期。

`Config.Aggregate = true` 时改为从主周期实时聚合——只同步一个周期就够，
代价是**空洞和时区边界会让聚合结果与 OKX 的官方 K 线有偏差**。
用之前请知悉，别拿聚合出来的日线去和交易所的日线对数。

### 实时模式

```go
func (f *Feed) Push(c Candle) error
```

手工喂一根，走同一套指标与视图。目的是让 **WS 实盘和回测复用同一份策略代码**——
接口成本几乎为零，但省掉了「回测跑通了实盘对不上」这类最难查的问题。

---

## 七、与 okx-position-simulator-go 对接

`adapter/okxsim` 是**独立嵌套模块**，主模块不依赖它。

```go
func ToBar(instID string, c tickflow.Candle) okxsim.Bar
```

| okxsim.Bar 字段 | 取值 |
|---|---|
| `High` / `Low` | 直取 |
| `Last` | `Close` |
| `Ts` | 直取 |
| `MarkPx` | **留空** |
| `IdxPx` | **留空** |
| `Funding` | **永远 nil** |

`float64 → decimal` 用 `decimal.NewFromFloat`，它取能往返的最短十进制表示，
OKX 的价格位数远在 float64 的精度内，**转换无损**。

### MarkPx / IdxPx 留空

标记价和指数价在 OKX 是**各自独立的 K 线序列**（`mark-price-candles` /
`index-candles`），不是普通行情里的字段。okx-api-v5-go 目前没有这两个端点。

v1 留空，simulator 的 `Bar.markPx()` 会自动用 `Last` 顶替；
`IdxPx` 缺失时按 index 触发的算法委托会被跳过并在 `StepResult` 里说明原因——
它不会拿别的价格顶替，这个行为是对的。

> 上游补齐这两个端点后，本库加对应的 Source 即可接上，
> `Candle` 结构与存储格式都不需要改。

### 不计资金费——以及为什么这是刻意的

OKX 的历史资金费率**只保留约 3 个月**。上游有 `FundingRateHistory`，
技术上可以从今天起归档，但**部分区间有、更早的区间没有，比全都没有更糟**：

它会让同一个策略在不同时间段的回测收益**不可比**，而这种不可比是隐性的，
很容易被误读成「策略在不同市况下的表现差异」。

全都不计至少是**一致**的偏差：系统性高估多头持仓收益、低估空头，
方向已知，可以在解读结果时统一扣减。

**这一点必须在 README 显著位置标注，不能只写在函数注释里。**

---

## 八、明确不做

- **逐笔成交 / 盘口深度** —— 库名叫 tickflow，但 v1 只做 K 线。接口上留了泛化的余地。
- **交割合约换月拼接** —— 只保证单一 `instId` 的序列。SPOT / SWAP 无此问题。
- **前复权 / 后复权** —— 加密市场没有这个概念。
- **资金费率** —— 见上。
- **指标注册表与字符串规格** —— v2。
- **分布式 / 多进程写** —— 见「并发」。

---

## 九、排期

| 版本 | 内容 |
|---|---|
| v0.1 | `Candle` / `Period` / `Source`(okx) / `Store`(segfile) / `Syncer` |
| v0.2 | 七个内置指标 + 两套口径 + golden test |
| v0.3 | `Feed` / `View` / 多周期同步 / `Push` |
| v0.4 | `adapter/okxsim` |
| v1.0 | 文档 + 真实数据端到端验证 |
