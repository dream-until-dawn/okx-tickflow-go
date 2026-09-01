# 能力、边界、置信度与已知风险

写给**引入本库的人**。回答四个问题：它能做什么、明确不做什么、每条结论的把握有多大、
以及哪些东西即便一切按设计工作也可能咬你。

设计取舍与「为什么不那么做」在 [design.md](design.md)；那是写给改这个库的人的。

---

## 一、能力

| 能力 | 入口 | 说明 |
|---|---|---|
| 拉任意标的任意周期的 K 线 | `source/okxsource` | 消化了 OKX 的倒序、反直觉分页、单次条数上限、近端/远端两个端点 |
| 拉标记价 / 指数价 | `okxsource.MarkPrice` / `IndexPrice` | 各自独立的序列，**没有成交量** |
| 增量同步，不重复拉 | `tickflow.Syncer` | 靠 `Meta.Coverage` 记「已请求并确认」的区间 |
| 落盘，目录自选 | `store/segfile` | 64 字节定长记录；`candles` / `mark-candles` / `index-candles` 三个命名空间 |
| 七个内置指标 | `indicator` | MA EMA MACD KDJ RSI CCI BOLL，两套口径，流式增量 |
| 自定义指标 | `tickflow.Indicator` 接口 | 实现四个方法即可，与内置的一视同仁 |
| 可步进的多周期视图 | `tickflow.Feed` | 主周期步进，辅周期只给「最后一根已收盘」的 |
| 往回看 N 根 | `View.Prev(n)` | 由 `FeedConfig.Lookback` 决定上限 |
| 标记价随视图给出 | `FeedConfig.MarkStore` + `View.MarkPx()` | 按**同 ts** 与主周期锁步；`simbar.Advance` 自动带上 |
| 实盘复用同一套代码 | `Feed.Push` / `PushWithMark` | `store` 传 nil 即纯实盘形态 |
| 喂给记账内核 | `adapter/simbar` | 独立嵌套模块，主模块不依赖 decimal |

**依赖树**：主模块只有 `okx-api-v5-go`（它自己只依赖 `gorilla/websocket`）。
`shopspring/decimal` 与整个记账内核隔离在 `adapter/simbar` 这个嵌套模块里。

---

## 二、边界：明确不做的

不是没来得及做，是**决定不做**，各有理由。

| 不做 | 理由 |
|---|---|
| **资金费率** | OKX 只保留约 3 个月历史。部分区间有、更早的没有，比全都没有**更糟**——它让同一策略在不同时段的收益不可比，而这种不可比是隐性的 |
| 逐笔成交 / 盘口深度 | 库名叫 tickflow，但只做 K 线。命名空间已为它们留位 |
| 交割合约换月拼接 | 只保证单一 `instId` 的序列。SPOT / SWAP 无此问题 |
| 前复权 / 后复权 | 加密市场没有这个概念 |
| 指标注册表与字符串规格 | `"ema:20"` 那类配置驱动的写法。构造函数已经够用 |
| 多进程并发**写** | 一个命名空间同一时刻只允许一个写者，第二个拿到 `ErrLocked`。并发**读**是支持的 |
| 交易决策 / 仓位核算 | 那是回测引擎与 `okx-position-simulator-go` 的事 |

另外，`Feed` **不是并发安全的**——一个回测循环就是一条时间线。要并发跑多组参数，
各建各的 `Feed`（有测试盯着它们互不干扰）。

---

## 三、置信度：每条结论是怎么来的

同一份文档里混着「实测出来的数」和「推出来的判断」，等于没有说明。这里分三级：

| 级别 | 含义 |
|---|---|
| **实测** | 有真实数据或工具验证，数字可重跑 |
| **有测试** | 由测试锁住，但断言的是**本库自己**的行为，不是外部世界 |
| **推定** | 从外部**未文档化**的行为推出来的。交易所改了不会通知谁 |

### 实测

| 结论 | 证据 | 怎么重跑 |
|---|---|---|
| 17 个周期的对齐网格全部正确（含港时 / UTC 分叉） | 真实 OKX 数据逐根核对 | `TICKFLOW_LIVE=1 go test -run TestLiveAlignment ./source/okxsource/` |
| `history-candles` 至少能回溯 6 年（1m 亦然） | 逐年往回探 | `TICKFLOW_LIVE=1 go test -run TestLiveHistoryDepth ./source/okxsource/` |
| `/market/candles` 会返回**未完结**的当前一根 | 实拉确认 | `TICKFLOW_LIVE=1 go test -run TestLiveFetchContract ./source/okxsource/` |
| 标记价比成交价平稳：200 根 BTC 1H，平均振幅 **0.6433% vs 0.6300%**，标记价更窄占 146/200 | 两条序列按 ts 对齐比较 | `TICKFLOW_LIVE=1 go test -run TestLiveMarkAndIndex ./source/okxsource/` |
| 同步的根数精确到个位：两年 1H = 17520 根（=730×24），一根不缺 | 按周期网格数 | 见 design.md「v1.0 验收」 |
| 自聚合与 OKX 官方 1H **逐位相同**：2879 根，OHLC 共 11516 项 + 成交量，零不一致 | 120 天 15m 聚成 1H 对账 | 同上 |
| **OKX 平台的指标用 CN 口径**（MACD 柱与 KDJ 都对上 CN、对不上 TV） | 使用者在 OKX 平台上逐行比对 ETH 日线 | `go test -run TestMatchesOKXPlatform ./indicator/` |
| `float64 → decimal` 无损：按位相等且逐位相同 | 真实行情全部 OHLCV + 各量级刻度 | `cd adapter/simbar && go test -run TestFloat64ToDecimalIsLossless` |
| 指标性能：九个一整套 **202 ns/步**，零分配 | Ryzen 7 5700X | `go test -bench . ./indicator/` |
| Feed 热路径零分配：推进一步 30ns（单周期）/ 65ns（三周期聚合），`At(handle)` 1.7ns | 同上 | `go test -bench Feed .` |
| 并发安全：一写六读、多游标、并发构造指标，竞态检测无告警 | 真并发的测试 + `-race` | `go test -race ./...`（Windows 需 MinGW-w64） |
| Windows 上 `MoveFileEx` 在目标被打开时一律失败，与 `FILE_SHARE_DELETE` **无关** | 最小复现，四种共享标志全部 Access denied | 见 design.md 的并发一节 |

### 有测试

这些是本库自己的行为契约，由测试锁住，但它们不涉及外部世界：

- 未完结的 K 线不进入任何一层；`coverage` 末端只记到当前那根的**开盘时刻**
- `Feed.TF(bar)` 既**不超前**（看到的必须已收盘）也**不落后**（下一根必须还没收盘）
- 聚合模式与读库模式的**可见性时点完全一致**
- 标记价按**同 ts** 对齐；主周期有空洞时不会错位；缺根给 NaN 而不是上一根的值
- 指标的增量结果与批量参考实现一致（7 指标 × 2 口径 × 3 随机种子 × 两套参数）
- `Warmup()` 报的位置**恰好**是第一个非 NaN 的位置
- 无效视图、未就绪的指标、未知的键一律给 **NaN**，不给 0
- 写锁互斥；只读 Store 看到的是打开那一刻的快照
- 旧布局与旧版锁文件会被认出来并报错，不会静默当成空库

### 推定

**外部未文档化，可能在不通知的情况下改变。**

| 推定 | 现状 | 变了会怎样 |
|---|---|---|
| `2D` / `3D` 的锚点（哪两天、哪三天归为一根） | 按「自 epoch 起按港时自然日计数」推定，实测对上了 | `SyncReport.Misaligned` 会不为零；用 `RegisterFixedPeriod` 覆盖 |
| `history-candles` 的深度上限 | 只知道**至少** 6 年，上限未知 | 更早的区间会出现在 `SyncReport.Gaps` 里 |
| 标记价历史有一条硬线：**港时 2020-01-01** | 本库在生产环境自己翻页到底测的，5 个合约（`TestLiveMarkPriceHistoryFloor`） | 硬线之后上线的合约与成交价同深；之前上线的，标记价一律被截到那天 |
| OKX 平台的指标口径是 CN | 2026-09-01 实测 | `TestMatchesOKXPlatform` 会先炸——但只在有人跑测试时 |
| 6H 及以上按香港时间对齐 | OKX 文档说的，且实测对上 | 同 2D/3D |

---

## 四、已知风险

即便一切按设计工作，这些仍可能咬你。**「是否静默」那一列最要紧**——
会报错的问题不可怕，不报错的才是。

| 风险 | 静默？ | 后果 | 怎么办 |
|---|---|---|---|
| **不计资金费** | 是 | 系统性**高估多头**持仓收益、低估空头 | 方向已知，解读结果时统一扣减。自备数据可在拿到 `Bar` 后自行赋值 |
| **缺标记价退回成交价** | **否**（记账内核 v1.0.0+ 默认报错） | 影线制造出真实不会发生的强平；对尾部风险就是强平的策略（做多网格）是**假阴性** | 配 `FeedConfig.MarkStore`，`simbar.Advance` 会自动带上。**回测起点早于港时 2020-01-01 时标记价根本不存在**，那时打开 `Config.AllowMarkPxFallback` 是正当的，不是将就 |
| **回测起点早于标记价硬线** | 是 | 拉不到标记价，却容易以为是同步没做好 | 见下方「标记价的历史硬线」。`SyncReport.Gaps` 会把取不到的区间列出来 |
| **`Aggregate` 在底层缺根处失真** | 是 | 聚合出的高周期 K 线与交易所对不上 | 底层齐全时聚合是**精确**的（实测零不一致）。用 `SyncReport.Gaps` 事先查缺根 |
| **`1D` 与 `1Dutc` 是两条序列** | 是 | 选错了整个回测偏移一天 | 6H 及以上都有这个分叉。OKX 默认是港时那条（不带 `utc` 后缀） |
| **`SetDefaultConvention` 跑到一半改** | 是 | 新旧口径的指标混在一起 | 只在程序初始化时设一次。并发调用本身是安全的（原子），但结果不可预测 |
| **`indicator` 实例是有状态的** | **否**，构造时报错 | 同一个实例挂在多处会让两边的值互相污染 | 各建各的（`indicator.MA(20)` 调两次）。`NewFeed` 会按指针身份拦住同一 Feed 内的共用；**跨 Feed 复用同一实例仍拦不住** |
| 深度回填超出内存上限（默认 100 万根） | 否，报错 | 回填失败 | 把 `From`/`To` 切成更小的窗口，或 `WithMaxMergeCandles` 调高 |
| **Windows 上只读端阻塞回填** | 否，报错 | 有读者时 `Merge` 失败（数据原封未动） | 让读者退出。**追加不受影响**，所以日常同步 + 并发读完全正常 |
| 跨进程双写 | 否，`ErrLocked` | — | 一个命名空间只允许一个写者。**跨机器 / 网络文件系统完全无保护** |
| 进程崩溃遗留锁文件 | 否，`ErrLocked` | 下次开不了 | 确认无人使用后 `segfile.ForceUnlock(root)`。本库**不猜**持有者是否还活着 |
| `Feed` 并发推进 | 否，会乱 | — | `Feed` 不是并发安全的。各建各的 |
| 指标口径与你对数的软件不一致 | 是 | 数对不上，容易误以为算错了 | 默认 CN（与 OKX 平台一致）。要 TradingView 口径显式传 `indicator.TV` |

### 标记价的历史硬线：港时 2020-01-01

**这不是「标记价与成交价同深」。** 生产环境实测 5 个合约：

| 合约 | 成交价最早 | 标记价最早 | 差 |
|---|---|---|---|
| BTC-USDT-SWAP | 2019-11-28 | 2020-01-01 | 34 天 |
| ETH-USDT-SWAP | 2019-11-30 | 2020-01-01 | 32 天 |
| BTC-USD-SWAP | 2018-12-18 | 2020-01-01 | **379 天** |
| SOL-USDT-SWAP | 2021-01-22 | 2021-01-22 | 同深 |
| DOGE-USDT-SWAP | 2020-07-10 | 2020-07-10 | 同深 |

规律：**硬线之后上线的合约两条序列同深；之前上线的，标记价一律被截到硬线那天。**
从合约上线跑全历史的人一定会撞上——`BTC-USD-SWAP` 差了整整一年。

日期都是**港时**（`1D` 按港时对齐；UTC 是 2019-12-31 16:00 那根）。用 `1Dutc`
看到的日期会不一样。

`2020-01-01` 是**观察结论，不是官方承诺**。`TestLiveMarkPriceHistoryFloor` 会在它
变化时报错。

> 这条一度被写成「标记价与普通 K 线同深」，来源是 okx-position-simulator-go——
> 而那次测量是在**模拟盘**上做的。模拟盘的两条序列在同一天一起截断（模拟盘自己的
> 数据上限），于是「一起返回空」被读成了「一样深」，实际是「一样测不到」。
> 对方主动撤回了，本库随后在生产环境自己复测，上表是自己的数。
>
> 教训不在于谁错了，而在于**「两条都取不到」和「两条一样深」在数据上长得一模一样**
> ——这正是本文档要给每条结论标来源与重跑方式的理由。

### 三处最容易被误读的

**「测试全绿」不等于并发安全。** 竞态检测只报**实际发生过**的竞争。单 goroutine
的测试跑 `-race` 必然全绿，而那份绿是假的。本库另写了会真并发起来的测试
（`*/concurrent_test.go`），第一次跑就抓到一个真的数据竞争。

**「聚合是近似的」是错的说法。** 正确的说法是「聚合在底层缺根处失真」——
底层齐全时它与交易所逐位相同。前者会让人白白不敢用，后者可以事先查。

**NaN 不是故障。** 指标未 warmup、视图无效、标记价缺根，都给 NaN。这是刻意的：
0 是个看起来正常的价格，NaN 会沿运算传染，把误用当场暴露出来。循环里用
`View.Ready()` 判断是否可以开始决策。

---

## 五、版本与兼容性

| | 承诺 |
|---|---|
| **主模块** `v1.x` | 公开 API 已收口。`tickflow.Store` / `Source` / `Indicator` 三个接口是对外契约，改它们要换模块路径 |
| **`adapter/simbar`** | 独立嵌套模块，独立打 tag（`adapter/simbar/vX.Y.Z`），`require` 指向它实际需要的最低 tickflow 版本 |
| **落盘格式** | `.meta` 里带 `magic` / `version` / `recordSize`。格式变更会升 `version` 并在打开时报错说明怎么迁移（v0.3→v0.4 的目录迁移就是这么做的） |
| **行为变更** | 不改签名但改结果的变更（如 v1.1.0 把默认口径从 TV 改为 CN）会走 minor 版本并在 README 显著标注 |
| Go 版本 | 1.22，与两个上游对齐 |

**根目录的 `go build ./...` 与 `go test ./...` 不包含 `adapter/simbar`**——
它是独立模块，要单独 `cd` 进去执行。

---

## 六、怎么自己重新验证

不必信这份文档，都能自己跑：

```bash
go test ./...                                  # 主模块
cd adapter/simbar && go test ./...             # 嵌套模块（单独跑）
go test -race ./...                            # 并发；Windows 需 MinGW-w64
go test -bench . ./indicator/                  # 指标性能
TICKFLOW_LIVE=1 go test ./source/okxsource/    # 打真实接口，验对齐与历史深度
```

> Windows 上跑 `-race` 需要 cgo：
> `winget install BrechtSanders.WinLibs.POSIX.UCRT`（要 **POSIX 线程**那个变体，
> MSVC 不行）。装完 `gcc` 可能不在 PATH 上，在 winget 的包目录里。

端到端的例子：

```bash
go run ./examples/sync -inst BTC-USDT-SWAP -bar 15m -days 30 -root ./data
go run ./examples/sync -inst BTC-USDT-SWAP -bar 15m -days 30 -root ./data -kind mark
go run ./examples/indicators -root ./data      # 两套口径的差异
go run ./examples/feed -root ./data            # 含「高周期收盘前不可见」的现场验证
cd adapter/simbar && go run ./examples/backtest -root ../../data
```
