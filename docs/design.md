# okx-tickflow-go 设计

> 状态：v0.5，排期上的五档全部落地。本文记录**取舍与理由**，不只是接口清单——
> 半年后回头看，能想起来「为什么不这么做」比「做了什么」更重要。
>
> 起初是动手前的草稿。实现过程中有几处改了主意，也有几处被实测推翻，
> 那些地方都留了引注说明原来是怎么想的、为什么改——文中带 `>` 的段落多半是。

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
└── adapter/simbar/      → okxsim.Bar（独立嵌套模块，隔离 decimal 依赖）
```

**主模块依赖只有 okx-api-v5-go 一个**（它自己只依赖 gorilla/websocket）。
`shopspring/decimal` 与整个模拟器包被隔离在 `adapter/simbar` 这个嵌套模块里——
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
// 与 simulator 的 decimal 世界的转换收在 adapter/simbar 一个地方。
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
原样透传给 OKX**，自己只需要知道它的对齐网格：

```go
type Period struct { /* 内部：step + anchor，或 months + loc */ }

func (p Period) Truncate(ts int64) int64   // 对齐到所属 K 线的开盘时刻
func (p Period) Next(ts int64) int64       // 下一根的开盘时刻 = 本根的收盘时刻
func (p Period) Closed(ts, now int64) bool
func (p Period) LastClosed(now int64) int64

func ParsePeriod(bar string) (Period, error)
func RegisterFixedPeriod(bar string, step time.Duration, anchor time.Time) error
```

定长周期用 `anchor + k*step` 的网格表达；`1M`/`3M` 不定长，走日历。
`Next` 而不是暴露一个步长，是因为月线的收盘时刻一个 `Duration` 表达不了。

> 实现时把草稿里的 `RegisterPeriod(bar, next func)` 换成了
> `RegisterFixedPeriod(bar, step, anchor)`：只给 `Next` 表达不出 `Truncate`，
> 而 Syncer 对齐区间、Feed 判收盘都要 `Truncate`。`step + anchor` 能表达任何
> 定长网格，正好覆盖「OKX 新增了一个周期」这个唯一的实际需求。

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

限速走上游的 `WithLimiter` 注入点，重试上游已有，本库不重复造；
`okxsource` 另有一个默认 120ms 的保守节流，供没注入限流器时兜底。

`Source` 的形态定成「一次调用缓冲整个区间」而不是回调流式：

```go
type Source interface {
    Fetch(ctx context.Context, req FetchRequest) ([]Candle, error)
}
```

因为 OKX **只能往更旧的方向翻页**（`after` 游标），而 `Store.Append` 要求升序。
要流式升序输出就得先攒完再翻正——那就等于缓冲。既然如此，不如把「控制区间
大小」这件事明确交给上层：`Syncer` 已经按 chunk 切好了，单块内存有界。

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

**落盘位置完全由使用者指定，库里没有任何默认路径**（空字符串直接报错，不猜）。
相对路径在 `Open` 时就换算成绝对路径——之后 `os.Chdir` 不会让同一个 Store
指向别处，这是个真实存在又很难查的坑。

```
<root>/
  .lock                    写锁
  candles/                 K 线的命名空间
    BTC-USDT-SWAP/
      1m.dat               纯定长记录数组，无文件头，offset = i * 64
      1m.meta              JSON，人可读
```

`candles/` 那一层是给以后留位的：逐笔成交、盘口深度是形态完全不同的数据，
将来要放进来时不必迁移已有的 K 线。现在多一个空目录，比以后动全部人的数据
便宜得多。旧布局（v0.3 及之前直接放在 `<root>/<instId>/`）的数据在 `Open`
时会被认出来并报错说明怎么挪——「数据凭空消失」是最难查的一类问题。

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
    // Append 在末尾追加。要求 ts 严格升序且首根晚于 LastTs。同步最新数据的快路径。
    Append(instID, bar string, cs []Candle) error

    // Merge 并入任意位置，同 ts 以新数据为准。回填走这条，代价是一次全文件重写。
    Merge(instID, bar string, cs []Candle) error

    // Iter 返回 [from, to) 的游标。Feed 走这条——大范围回测不该把
    // 几百万根一次性读进内存。
    Iter(instID, bar string, from, to int64) (Iterator, error)

    // Range 是 Iter 的便捷包装，小范围查询用。
    Range(instID, bar string, from, to int64) ([]Candle, error)

    Meta(instID, bar string) (Meta, error)

    // AddCoverage 记录「这一段已请求并确认」。见下。
    AddCoverage(instID, bar string, r Range) error

    Series() ([]SeriesID, error)
    Close() error
}
```

**为什么 coverage 要单独一个写入口，而不是从 Append 推导。**
一段区间里一根 K 线都没有，可能是 OKX 本就没产出，也可能是还没拉——
数据本身分不出这两种。只有发起请求的一方知道是哪种，所以由它来记。

**Append 与 Merge 分开**，是因为把两者合成一个「智能写入」会掩盖一个
O(n²) 陷阱：回填若按 chunk 逐块 Merge，每块都要重写整个文件。分成两个方法，
调用方就必须显式决定「这是追加还是回填」，也就必须把整段回填攒成一次 Merge。

### 并发：一个写者，多个读者

进程内多读单写。跨进程靠 `Open` 时取的**写锁**：一个目录同一时刻只能有一个
写者，第二个 `Open` 当场返回 `ErrLocked`，错误信息里带着占用者的 pid、主机名
与占用时刻。同一进程里对同一目录 `Open` 两次同样被挡下——两个 Store 各有各的
内存锁，它们之间并不互斥，危险程度和两个进程一样。

进程崩溃会遗留锁文件。本库**不去猜那个进程是否还活着**：跨平台可靠地判断这件事
要引入平台相关的依赖。`ForceUnlock` 是明写出来的逃生口，而不是一个会在错误时刻
自作聪明的启发式。

`OpenReadOnly` 不取锁，可与写者并存，读到的是**打开那一刻的快照**。

> **Windows 上一条实测出来的限制。** 只读端持有某条序列时，写者回填不了那条
> 序列：回填要改名整个文件，而 `MoveFileEx` 在目标被任何句柄打开时一律失败。
>
> 我一度以为给只读端加上 `FILE_SHARE_DELETE` 就能解决，还写了平台特化的
> `syscall.CreateFile` 版本——**最小复现证明没用**，四种共享标志组合全部
> Access denied。那段代码随即删掉了：留着一段不起作用的平台代码，比没有更糟，
> 它会让下一个人以为这个问题已经解决了。
>
> 追加不受影响（按偏移写，不改名），所以日常同步 + 并发读完全正常，只有深度
> 回填需要先让读者退出。回填失败时数据原封未动、序列继续可用——这一条是测试
> 照出来的：最初的实现在改名失败后把文件句柄置成了 nil，序列从此作废。

遍历期间若有人 `Merge`，文件被整体重写、下标随之失效。游标靠一个 generation
计数发现这件事并**报错**，而不是接着读——那读到的是另一个位置上的数据，
是无声的错。`Append` 不改变已有下标，所以不触发失效。

### Syncer

```go
func (s *Syncer) Sync(ctx context.Context, req SyncRequest) (Report, error)
```

读 `Meta.Coverage` → 求出缺失区间 → 只拉这些 → 落库 → 合并 coverage。
`Report` 报告新增根数、拉取批次、确认为空洞的区间，以及**没落在周期网格上的
根数**（不为 0 就说明该周期的锚点对不上了）。

三条实现时才定下来的规则：

**1. 区间末端截到「最后一根已收盘」。** 当前那根还在走，收盘价尚不可知。
若把它算进 coverage，等它收盘后就再也不会去补——那根 K 线会永久停在一个
半截的值上，而且悄无声息。

**2. 追加与回填走不同的路。** 缺失段整体晚于已有数据 → 逐块拉、逐块落库、
逐块记 coverage，被打断时进度留着；否则是回填 → 整段攒在内存里一次 `Merge`，
成功后才记 coverage。回填有 `WithMaxMergeCandles`（默认 100 万根 ≈ 64MB）
兜底，超了就报错并告诉使用者把窗口切小，而不是默默吃内存。

**3. `From` 必填，不提供「从最早开始」。** history-candles 的可回溯深度按周期
不同且未文档化，无边界地往前探测既慢、又无从判断该在哪停——一段区间没有
K 线，可能是到头了，也可能只是那阵子没成交。要深回填就自己按窗口分段调用。

---

## 五、指标

```go
type Indicator interface {
    Name() string              // 视图 key，默认 "ema20" / "macd"
    Fields() []string          // 单输出返回 nil；MACD 返回 ["dif","dea","hist"]
    Warmup() int               // 出第一个有效值需要多少根
    Update(c Candle) []float64 // 复用内部切片，热路径零分配
    Reset()
}
```

配置走 Option，与本仓其余部分一致。`Convention` 本身就是一个 Option：

```go
indicator.MACD(12, 26, 9)                      // TradingView 口径（默认）
indicator.MACD(12, 26, 9, indicator.CN)        // 国内软件口径
indicator.MA(20, indicator.Named("fast"))      // 改视图里的键名
indicator.SetDefaultConvention(indicator.CN)   // 或改全局默认，程序初始化时设一次
```

> 草稿里设想的 `.With(conv)` / `.As(name)` 链式调用换成了 Option。链式要求每个
> 指标类型各自实现一遍 `As`、`With` 才能保住返回类型，七个指标就是十四个样板
> 方法；而 Option 一次写好，且 `indicator.MACD(12,26,9, indicator.CN)` 读起来
> 并不比链式差。

**warmup 未满时返回 `NaN`，不返回 nil 也不返回 0。**
NaN 在列式缓冲里天然占位；更要紧的是它会沿运算传染，能把「拿未就绪的指标去
比较」这类误用**暴露出来**，而返回 0 会让策略在开头几十步安静地做出错误决策。

视图 key 展平：单输出用 `Name()`，多输出用 `Name() + "." + field`——
`"ema20"`、`"macd.dif"`。**同名冲突在 `NewFeed` 构造时报错**，不留到运行期。

`Warmup()` 说的是「值从此**有定义**」，不是「值已**收敛**」。EMA 一族有了定义
之后还要再走若干个周期才和播种方式无关——这一条与口径无关，是递推本身的性质。

### 两套口径：差异只有三处

草稿里列了四处，实现时发现 **EMA 播种和 RSI 播种是同一条规则**，不是两条：
两者底层都是 `y ← y + α(x − y)` 的递归平均，只是 α 不同（EMA 取 `2/(n+1)`，
Wilder 取 `1/n`）。把它们收进同一个 `smoother` 之后，口径分歧就只剩：

| 处 | TV | CN | 影响 |
|---|---|---|---|
| **递归平均的播种** | 前 n 个样本的简单平均 | 首个样本 | EMA、MACD、RSI |
| **MACD 柱** | `DIF − DEA` | `2 × (DIF − DEA)` | MACD |
| **KDJ 平滑** | `%K = MA(RSV,m1)`、`%D = MA(%K,m2)`，**简单均**，无 J 线 | `K = SMA(RSV,m1,1)`、`D = SMA(K,m2,1)`，**指数式** | KDJ |

播种那一条是**暂时**的：影响按 `(1−α)^k` 衰减，走够长两套口径会收敛到同一个值。
实测 5760 根 15m 之后，两套口径的 RSI(14) 完全相同。柱子乘 2 和 KDJ 的平滑方式
则是**永久**差异。

两套完全一致的：**MA**、**CCI**、**BOLL**。

**BOLL 的标准差按总体 `n`**（不是样本 `n−1`），两套口径都如此。这曾是唯一悬着
的参数：TradingView 的 `ta.stdev` 是总体，通达信的 `STD` 疑为样本但未经核实。
既然差别只是把带宽整体缩放 `√(n/(n−1))`（20 周期上约 2.6%），就统一取总体，
不留一个会随口径漂移的量。

TV 口径下的 **J 线是本库补的**——TradingView 的 Stochastic 没有 J，
这里在两套口径下都按 `J = 3K − 2D` 给出，好让字段结构一致。用 TV 口径时
那边没有对应的线可以对数。

### 「增量 == 批量」是结构上成立的，不是碰巧测过

窗口类指标（MA / BOLL / CCI / KDJ 的 RSV）每步在窗口上**重算**，而不是增量
维护累加和。增量维护 `sum` 会随步数累积浮点漂移，跑几百万根之后与「把这 n 根
拿出来算一遍」对不上——而后者正是批量定义。递归类指标（EMA / MACD / RSI）
本就以递推定义，批量算法与流式算法是同一个。

重算的代价实测（Ryzen 7 5700X，全部零分配）：

| 指标 | ns/步 | 指标 | ns/步 |
|---|---|---|---|
| MA(20) | 11 | MA(200) | 118 |
| EMA(20) | 5 | MACD(12,26,9) | 7 |
| BOLL(20) | 35 | CCI(20) | 39 |
| KDJ(9,3,3) | 42 | RSI(14) | 7 |
| **九个指标一整套** | **202** | | |

百万步的回测在指标上一共花 0.2 秒。为省这点开销去换一个会随步数漂移的增量
实现，不划算。

### 测试的三层

三层各挡一类错，缺一层都不够：

1. **手算基线**（`indicator_test.go`）——数字取得能心算，把每个公式钉死。
2. **朴素批量参考**（`reference_test.go`）——独立写一遍笨办法，不碰
   `smoother`/`window` 那套机器，专挑流式实现的下标记账、播种时机、窗口边界。
   7 个指标 × 2 套口径 × 3 组随机种子 × 常规与偏门两套参数。
3. **真实行情 golden**（`golden_test.go`）——300 根真实的 15m K 线，
   26 列输出锁成基线，防止重构中被无意改动。

只有 2 而没有 3，改公式时两边一起改就发现不了；只有 3 而没有 1、2，第一次
就算错的话会把错的值锁进去。

另有跨指标的契约测试：`Warmup()` 报的位置**恰好**是第一个非 NaN 的位置
（早一根全 NaN、正好那根全部有值）、退化行情（价格纹丝不动）下每个边界分支的
取值、`Reset` 后重算一致、以及「一根一根喂」与「分两批喂」结果逐位相同。

内置：`MA(n)` `EMA(n)` `MACD(f,s,sig)` `KDJ(n,m1,m2)` `RSI(n)` `CCI(n)` `BOLL(n,k)`。
外部指标只需实现 `Indicator` 接口。**不做字符串规格解析与注册表**
（`"ema:20"` 这种配置驱动的写法留到以后）——构造函数已经够用。

---

## 六、Feed 与视图

```go
f, err := tickflow.NewFeed(store, tickflow.FeedConfig{
    InstID: "BTC-USDT-SWAP",
    Base:   "15m",                      // 步进的主周期
    Extra:  []string{"1H", "1D"},       // 辅周期，只读不步进
    From:   t0, To: t1,                 // 毫秒；0 表示库里的首尾
    Lookback: 5,
    Indicators: map[string][]tickflow.Indicator{
        "15m": {indicator.MA(20), indicator.MACD(12, 26, 9)},
        "1D":  {indicator.MA(5, indicator.Named("ma5d"))},
    },
})
defer f.Close()

h, _ := f.Handle("15m", "macd.hist")    // 循环外预解析
for f.Next() {
    v := f.View()
    if !v.Ready() { continue }
    v.Close(); v.At(h); v.Prev(3).Close()
    f.TF("1D").Ind("ma5d")
}
if err := f.Err(); err != nil { ... }
```

**指标按周期挂**——1D 的 MA20 和 15m 的 MA20 是完全不同的东西，
挂在一个扁平列表里迟早出事。同周期上的键名冲突在 `NewFeed` 构造时报错。

> 接口定义在根包（`tickflow.Indicator`），实现在 `indicator` 子包，后者用
> 类型别名指回来。这是循环引用逼出来的：Feed 要消费指标，而指标要用 Candle，
> 定义放在任何一边都成环。别名让两边写哪个都一样。

### 三条硬保证

**1. `f.TF(bar)` 永远返回最后一根已收盘的 K 线。**

推进规则一句话：主周期第 `t` 根上，辅周期看到的是
`extra.LastClosed(base.Next(t))`——即「主周期这根**收盘时**，辅周期最后一根
已收盘的」。用收盘时刻而不是开盘时刻，是因为决策发生在收盘之后。

实测：主周期 `08-31 14:30`（14:45 收盘）时，`1H` 给 13:00 那根（14:00 收盘），
`1D` 给 08-29 16:00 那根（港时 08-30 全天，08-30 16:00 收盘）。

测试同时验两个方向——**不能超前**（看到的那根必须已收盘）与**不能落后**
（下一根必须还没收盘）。只验一个方向的话，一个「永远返回第一根」的实现
也能通过。

**2. warmup 自动前推。**

Feed 按各周期自己的 `max(Warmup) + Lookback` 往前多读一段喂指标、不产出步进。
多读的倍数是 2：指标要 n 根，但「n 根 K 线」占多长时间是不确定的——空洞会让
按步长换算出来的窗口偏短。真读不满时 `v.Ready()` 如实报 false，而不是给出一个
用半截历史算出来的值。`WarmFrom` 可以接管，`NoAutoWarmup` 可以关掉。

**3. `View` 是值类型句柄，热路径零分配。**

`View` 只有「一个 `*tfSeries` 加一个绝对下标」，`Prev(n)` 把下标往回挪。
底下是列式环形缓冲：K 线一份，每路指标各占一列。列式而不是每根一个 map——
回测走上百万步，每步给每个指标分配一个 map 是纯粹的浪费。

实测（Ryzen 7 5700X，全部零分配）：

| 操作 | ns | 操作 | ns |
|---|---|---|---|
| 推进一步（单周期） | 30 | 推进一步（三周期聚合） | 65 |
| `v.At(handle)` | 1.7 | `v.Ind("name")` | 10.6 |
| `v.Prev(3).Close()` | 3.8 | `f.View()` | 0.5 |

按句柄取值比按名字快六倍，两套 API 并存是值的。

> 聚合器一度在每次收盘时 `append` 一个新切片——15m 聚 1H 就是每四步一次分配，
> 基准上是 16 B/op。改成复用定长数组后不但归零，三周期步进还从 82ns 降到 65ns：
> 那次分配本身就是主要开销。

### 无效视图给 NaN，不给 0

`Prev(n)` 超出 Lookback、或还没走到第 n 根时得到无效视图。它不 panic，
但 `Close()`、`Ind()` 一律返回 NaN——0 是个看起来正常的价格，NaN 会沿运算传染，
把「用了一根不存在的 K 线」当场暴露出来。这与指标未 warmup 时返回 NaN 是同一条
原则。

拿**别的周期**的 Handle 去取值也返回 NaN（一次指针比较）。取错周期的指标是个
安静的错，换一个当场暴露，值。

### 辅周期的数据来源

**默认从 Store 读该周期的独立序列**（OKX 自己算的，最准），要求先 `Sync` 过。
辅周期不在库里时错误信息会指出可以改用聚合，而不是丢一个 `ErrNoSeries` 了事。

`Aggregate: true` 改为从主周期实时聚合——只同步一个周期就够。

**失真只来自缺根，不来自聚合本身。** 这一条起初只是个谨慎的免责声明，v1.0 时
量化了：把 120 天的 15m 聚成 1H，与 OKX 官方的 1H 逐位比对 2879 根完整小时线，
OHLC 共 11516 项外加成交量，**全部零不一致**。底层齐全时聚合是精确的；只有某根
高周期缺了底层 K 线时才失真，那种小时在这 120 天里只有 2 个（数据两端）。

所以正确的说法不是「聚合是近似的」，而是「聚合在底层缺根处失真」——后者可以
用 `SyncReport.Gaps` 事先查出来。

两种模式的**可见性时点完全一致**，有测试盯着：不然 `Aggregate` 只是个开关，
一拨回测结果就变了，而变化的原因藏在「高周期什么时候算收盘」这种没人会去查的
地方。聚合模式下还会校验周期能否嵌套（15m 聚 1H 可以，4H 聚 6H 不行）——
用真实边界抽样验，而不是去推导两套锚点的整除关系：后者要对每种组合分别论证，
容易漏。

主周期有空洞时，一步可能跨过一整根高周期，此时聚合器一次吐出两根。
那根不完整的高周期 K 线**不会被整根丢掉**，只是内容不完整。

### 实时模式

```go
f, _ := tickflow.NewFeed(nil, cfg)   // store 传 nil 就是纯实盘形态
f.Push("15m", candle)
```

WebSocket 收到收盘 K 线后 `Push`，指标与视图的代码和回测完全一样，省掉
「回测跑通了实盘对不上」这类最难查的问题。

ts 必须严格递增且落在该周期的对齐网格上，否则报错——实盘里重复推送与乱序推送
都真实存在，静默接受会让指标悄悄算错。

聚合模式下推主周期会带着辅周期一起推进；否则辅周期要调用方自己 `Push`——
实盘没有库可读，本库不替使用者臆造那几根 K 线。

---

## 七、与 okx-position-simulator-go 对接

`adapter/simbar` 是**独立嵌套模块**，主模块不依赖它。

> 草稿里叫 `adapter/okxsim`，实现时改了叶子名：**记账内核自己的包名就是
> `okxsim`**。同名的话，每个同时用到两边的使用者都得给其中一个起别名。
> 叫 `simbar` 反而更直白——它产出的正是 `Bar`。隔离策略（独立嵌套模块）没变。

```go
func ToBar(instID string, c tickflow.Candle, opts ...Option) (okxsim.Bar, error)
func ToBars(instID string, cs []tickflow.Candle, opts ...Option) ([]okxsim.Bar, error)
func Advance(sim *okxsim.Simulator, instID string, v tickflow.View, opts ...Option) (okxsim.StepResult, error)

func WithMarkPx(px float64) Option
func WithIdxPx(px float64) Option
func Dec(f float64) decimal.Decimal
```

**返回 error 而不是直接给 Bar**：`decimal.NewFromFloat` 碰上 NaN 会 **panic**，
而 NaN 恰恰是「用了一个无效 View」的正常产物。与其让它从库深处炸出来，
不如在这里拦住并说清楚。校验 instId、ts、三个价格的有限性与正负、以及高低顺序。

**隔离是实测过的**，不是声称的：主模块 `go list -m all` 只有 `okx-api-v5-go`，
`shopspring/decimal` 在主模块依赖图里出现 **0 次**，根目录 `go list ./...`
也不包含 adapter。代价是根目录的 `go build ./...` / `go test ./...` 覆盖不到它，
要单独 `cd adapter/simbar` 执行。

| okxsim.Bar 字段 | 取值 |
|---|---|
| `High` / `Low` | 直取 |
| `Last` | `Close` |
| `Ts` | 直取 |
| `MarkPx` | **留空** |
| `IdxPx` | **留空** |
| `Funding` | **永远 nil** |

### float64 → decimal 无损，这条有测试盯着

行情层用 float64（指标计算是热路径），记账层用 decimal（钱的事不能有误差）。
转换收在 `Dec` 一处，走 `decimal.NewFromFloat`——它取的是能往返回原值的最短
十进制表示，而 OKX 的价格有效数字远在 float64 的 15 位之内。

这曾经只是个「应该没问题」的判断，现在是测出来的：

- **真实行情**：300 根 15m BTC-USDT-SWAP 的全部 OHLCV 字段
- **各量级的刻度**：从 `1e-8`（小币种）到 `1234567.89`，含 `0.1/0.2/0.3`
  这类二进制表示不精确的经典值
- 断言两条：`Dec(f).InexactFloat64() == f`（按位相等，不是近似），
  且 `Dec(f).String() == strconv.FormatFloat(f, 'f', -1, 64)`（逐位相同，
  既没多出尾数也没丢位）

端到端还验了一遍：一段真实行情走完之后，记账内核记下的最新价与行情层最后那根
的收盘价逐位相同。

`Dec` 对 NaN / Inf 返回零值而不是 panic——回测循环里最不该出现的就是从库深处
炸出来的 panic。真正该拦住非法价格的地方是 `ToBar` 的校验。

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

**这一点必须在 README 显著位置标注，不能只写在函数注释里**——使用者不会先去读
`ToBar` 的注释才开始跑回测。本包也**不提供**设置 `Funding` 的选项：自备了数据
的使用者可以在拿到 `Bar` 之后自行赋值，那是明确的一步动作，而不是本库替谁做的
默认。有一个测试专门锁住「`ToBar` 永远不设 `Funding`」，免得日后被当成遗漏补上。

### 三个库真的接得上

单元测试证明每一步对，只有集成测试能证明它们【接得上】。
`adapter/simbar/integration_test.go` 把整条链跑通：

	真实行情 → segfile 落地 → Feed 步进（带指标）→ simbar → 记账内核

挂一笔限价买单，等行情的最低价触及它，验证成交、持仓与价格无损。
`adapter/simbar/examples/backtest` 是同一条链的可运行版本（均线金叉死叉，
5741 步、400 笔成交）——策略本身是最土的那种，只为把链路跑通。

---

## 八、明确不做

- **逐笔成交 / 盘口深度** —— 库名叫 tickflow，但 v1 只做 K 线。接口上留了泛化的余地。
- **交割合约换月拼接** —— 只保证单一 `instId` 的序列。SPOT / SWAP 无此问题。
- **前复权 / 后复权** —— 加密市场没有这个概念。
- **资金费率** —— 见上。
- **指标注册表与字符串规格** —— v2。
- **分布式 / 多进程写** —— 见「并发」。

---

## 实测记录（2026-08-31，BTC-USDT-SWAP）

文档里查不到、只能打真实接口问出来的三件事。可重跑：
`TICKFLOW_LIVE=1 go test ./source/okxsource/`。

**周期对齐全部对上。** 17 个周期（`1m` `5m` `15m` `1H` `4H` `6H` `6Hutc`
`12H` `12Hutc` `1D` `1Dutc` `2D` `3D` `1W` `1Wutc` `1M` `1Mutc`）的开盘时间
都落在内置表算出的网格上。其中 **`2D` / `3D` 的锚点原本是推定的**——OKX 没有
文档说明哪两天、哪三天归为一根——按「自 epoch 起按港时自然日计数」推出来的
网格，实测对上了。港时与 UTC 的分叉也确认了：`1D` 的开盘落在 UTC 16:00，
`1Dutc` 落在 UTC 00:00，确实是两条不同的序列。

**history-candles 至少能回溯 6 年。** `1m` / `5m` / `1H` / `1D` 都能取到
2020-08-31 的数据（那多半是该合约自己的上线日，不是接口的深度上限）。
比预想的深得多——这让回填的内存上限成了真问题而不是理论问题：
1m 线六年约 315 万根，远超默认的 100 万上限，必须分窗口回填。

**未完结的 K 线确实会混在返回里。** `/market/candles` 把当前还在走的那根
一并返回，`Confirm` 为 false。过滤掉它是 `okxsource` 的责任，
`TestLiveFetchContract` 专门盯着这一条。

## v1.0 验收（2026-09-01）

换了一个干净的数据目录，拉两年真实数据从头跑一遍。

**同步的根数精确到个位。** BTC-USDT-SWAP 两年 1H 得 17520 根（= 730×24）、
两年 1D 得 730 根、120 天 15m 得 11520 根（= 120×96）——按周期网格数，
**一根不缺**。三条序列的对齐、升序、无重复全部通过。落盘 1.8MB。

**重跑零请求。** 三条序列再同步一次，1H 与 1D 各 0 次拉取、1ms 返回；
15m 新增 1 根，那是期间真的又收了一根。

**自聚合与官方逐位相同**（见上）。

**真实辅周期下的防未来函数。** `Aggregate=false`，15m 主周期配 1H + 1D 辅周期，
三条都是 OKX 自己算的独立序列：走 11520 步，对辅周期做了 22974 次
「不能超前、也不能落后」的双向检查，**零违规**。

**完整一条链。** 两年 1H 跑均线金叉死叉：17501 步、1149 笔成交、期末权益
49133.98（-1.73%，几乎全是手续费），期末仍持仓 2 张、保证金率 44.28%——
记账内核的强平判据、保证金核算一路都在算。

## 九、排期

| 版本 | 内容 |
|---|---|
| v0.1 | ✅ `Candle` / `Period` / `Source`(okx) / `Store`(segfile) / `Syncer` |
| v0.2 | ✅ 七个内置指标（MA EMA MACD KDJ RSI CCI BOLL）+ 两套口径 + 三层测试 |
| v0.3 | ✅ `Feed` / `View` / 多周期同步 / 实时 `Push` |
| v0.4 | ✅ 数据目录、写锁与只读模式、`candles/` 命名空间 |
| v0.5 | ✅ `adapter/simbar`——与记账内核对接，三库端到端跑通 |
| v1.0 | ✅ API 收口、根包文档、两年真实数据端到端验收 |
