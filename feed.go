package tickflow

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// warmupSlack 是预热时多读的倍数。
//
// 指标需要 n 根才收敛，但「n 根 K 线」占多长时间是不确定的——小币种或维护期
// OKX 根本不产出那几根。按周期步长直接换算出来的时间窗口会偏短，多读一倍来兜底。
// 真的碰上空洞多到读不满的情况，View.Ready() 会如实报 false，而不是给出一个
// 用半截历史算出来的指标值。
const warmupSlack = 2

// FeedConfig 描述一个 Feed。
type FeedConfig struct {
	InstID string

	// Base 是【步进】的主周期。Next 每次前进一根主周期 K 线。
	Base string

	// Extra 是辅周期，只读不步进。它们永远停在【最后一根已收盘】的位置上。
	//
	// 辅周期必须长于主周期。
	Extra []string

	// From / To 划定步进的区间，左闭右开，单位毫秒。
	// From 为 0 表示从库里最早一根开始，To 为 0 表示直到最后一根。
	From int64
	To   int64

	// Indicators 按周期挂指标。键是周期字符串，须出现在 Base 或 Extra 里。
	//
	// 按周期挂而不是拉平成一个列表：1D 的 MA20 和 15m 的 MA20 是完全不同的
	// 东西，混在一起迟早出事。
	Indicators map[string][]Indicator

	// Lookback 是视图能往回看多少根，决定 View.Prev(n) 的 n 上限。
	Lookback int

	// Aggregate 让辅周期由主周期【聚合】而来，而不是从库里读独立序列。
	//
	// 默认（false）从库里读，那是 OKX 自己算的，最准，但要求先把对应周期
	// 同步下来。开启后只需同步主周期一个，代价是【空洞与边界会让聚合结果
	// 与交易所的官方 K 线有偏差】——别拿聚合出来的日线去和交易所对数。
	//
	// 开启时辅周期的边界必须落在主周期的网格上（比如 15m 聚合 1H 可以，
	// 4H 聚合 6H 不行），构造时会校验。
	Aggregate bool

	// WarmFrom 覆盖自动算出的预热起点（毫秒）。0 表示自动。
	//
	// 自动预热会按各周期的 max(Settle) 加 Lookback 往前多读一段，喂给指标但不
	// 产出步进。用 Settle 而不是 Warmup：递归类指标（MACD 等）在「有定义」之后
	// 还要走上百根才不再受播种影响，只按 Warmup 预热会让值错到一倍。
	// 知道自己要多少历史时可以用它接管。
	WarmFrom int64

	// NoAutoWarmup 关掉自动预热，直接从 From 开始读。
	//
	// 此时开头若干根的指标会是 NaN 或【尚未收敛】，用 View.Ready() 判断——
	// 它按「已收敛」判，不是「有定义」。
	NoAutoWarmup bool

	// MarkStore 提供【标记价】K 线，跟着主周期锁步推进，由 View.MarkPx 与
	// View.Mark 取用。
	//
	// 为什么要单独一个 Store：标记价的 BTC-USDT-SWAP/1m 与普通 K 线的
	// BTC-USDT-SWAP/1m 是两条不同的序列，同一个 (instId, bar) 键。
	// store/segfile 用命名空间把它们分开：
	//
	//	store, _ := segfile.OpenReadOnly(root)                 // 普通 K 线
	//	mark, _  := segfile.OpenReadOnly(root, segfile.Mark)   // 标记价
	//
	// 为什么值得费这个事：OKX 的强平按标记价判定，不是成交价。缺了它，
	// okx-position-simulator-go 会退回用最新成交价顶替，于是影线会制造出
	// 真实【不会发生】的强平——对尾部风险就是强平的策略（比如做多网格）
	// 尤其致命，而且是假阴性，结果里不留痕迹。
	//
	// 只对【主周期】生效：记账内核是按主周期一步一步推进的，辅周期只提供
	// 上下文，不参与撮合与强平。
	MarkStore Store
}

// Feed 把存储里的 K 线连同指标，变成可以一步一步走的视图。
//
// 它是本库对回测引擎的主要接口，也是「防未来函数」这件事真正落地的地方，
// 见 TF 的说明。
//
//	f, _ := tickflow.NewFeed(store, tickflow.FeedConfig{
//	    InstID: "BTC-USDT-SWAP",
//	    Base:   "15m",
//	    Extra:  []string{"1H", "1D"},
//	    Lookback: 5,
//	    Indicators: map[string][]tickflow.Indicator{
//	        "15m": {indicator.MA(20), indicator.MACD(12, 26, 9)},
//	        "1D":  {indicator.MA(5, indicator.Named("ma5d"))},
//	    },
//	})
//	defer f.Close()
//
//	h, _ := f.Handle("15m", "macd.hist")   // 循环外预解析
//	for f.Next() {
//	    v := f.View()
//	    if !v.Ready() { continue }
//	    v.Close(); v.At(h); v.Prev(3).Close()
//	    f.TF("1D").Ind("ma5d")             // 永远是最后一根已收盘的日线
//	}
//	if err := f.Err(); err != nil { ... }
//
// 热路径实测（Ryzen 7 5700X，全部零分配）：
//
//	推进一步（单周期）        30 ns      推进一步（三周期聚合）  65 ns
//	v.At(handle)            1.7 ns      v.Ind("name")         10.6 ns
//	v.Prev(3).Close()       3.8 ns      f.View()               0.5 ns
//
// 按句柄取值比按名字快六倍，热路径值得预解析一次。
//
// Feed 不是并发安全的：一个回测循环就是一条时间线，多个 goroutine 同时推进
// 没有意义。要并发跑多组参数，各建各的 Feed。
type Feed struct {
	store Store
	inst  string

	from, to int64

	base   *tfSeries
	extras []*tfSeries
	markIt *peekIter
	byBar  map[string]*tfSeries
	order  []*tfSeries // base 在前，便于统一遍历

	err    error
	closed bool
}

// NewFeed 构造一个 Feed。
//
// store 可以为 nil——此时 Next 立即返回 false，只能用 Push 手工喂数据。
// 实盘接 WebSocket 时就这么用，指标与视图的代码和回测完全一样。
func NewFeed(store Store, cfg FeedConfig) (*Feed, error) {
	if cfg.InstID == "" {
		return nil, errors.New("tickflow: Feed 的 instId 不能为空")
	}
	if cfg.Base == "" {
		return nil, errors.New("tickflow: Feed 的 Base 周期不能为空")
	}
	if cfg.Lookback < 0 {
		return nil, fmt.Errorf("tickflow: Lookback 不能为负，实为 %d", cfg.Lookback)
	}

	basePeriod, err := ParsePeriod(cfg.Base)
	if err != nil {
		return nil, err
	}

	f := &Feed{
		store: store,
		inst:  cfg.InstID,
		from:  cfg.From,
		to:    cfg.To,
		byBar: map[string]*tfSeries{},
	}

	// 校验 Indicators 里的周期都是登记过的。写错周期名却什么都不发生，
	// 会让人对着一堆 NaN 找半天。
	known := map[string]bool{cfg.Base: true}
	for _, b := range cfg.Extra {
		known[b] = true
	}
	for bar := range cfg.Indicators {
		if !known[bar] {
			return nil, fmt.Errorf("tickflow: 指标挂在了周期 %q 上，但它既不是 Base 也不在 Extra 里", bar)
		}
	}

	// 同一个指标实例挂到两个周期上，两边会往同一个【有状态】的对象里推，
	// 两个周期的值都会被污染——而且不报错。构造时按指针身份认出来。
	if err := checkSharedIndicators(cfg); err != nil {
		return nil, err
	}

	if f.base, err = newTFSeries(cfg.Base, basePeriod, cfg.Indicators[cfg.Base], cfg.Lookback); err != nil {
		return nil, err
	}
	f.byBar[cfg.Base] = f.base
	f.order = append(f.order, f.base)

	baseSpan := periodSpan(basePeriod)
	seen := map[string]bool{cfg.Base: true}
	for _, bar := range cfg.Extra {
		if seen[bar] {
			return nil, fmt.Errorf("tickflow: 周期 %q 重复出现", bar)
		}
		seen[bar] = true

		p, err := ParsePeriod(bar)
		if err != nil {
			return nil, err
		}
		if periodSpan(p) <= baseSpan {
			return nil, fmt.Errorf("tickflow: 辅周期 %q 必须长于主周期 %q；"+
				"辅周期只提供「最后一根已收盘」的上下文，比主周期还短就没有意义", bar, cfg.Base)
		}
		if cfg.Aggregate {
			if err := checkNesting(basePeriod, p); err != nil {
				return nil, err
			}
		}
		s, err := newTFSeries(bar, p, cfg.Indicators[bar], cfg.Lookback)
		if err != nil {
			return nil, err
		}
		if cfg.Aggregate {
			s.agg = &aggregator{p: p}
		}
		f.byBar[bar] = s
		f.extras = append(f.extras, s)
		f.order = append(f.order, s)
	}

	if cfg.MarkStore != nil {
		f.base.marks = make([]Candle, f.base.capN)
	}
	if store == nil {
		return f, nil
	}
	if err := f.openIterators(cfg); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// openIterators 按各周期各自的预热需求打开存储游标。
func (f *Feed) openIterators(cfg FeedConfig) error {
	meta, err := f.store.Meta(f.inst, cfg.Base)
	if err != nil {
		return fmt.Errorf("tickflow: 读取 %s/%s 失败: %w", f.inst, cfg.Base, err)
	}
	if f.from == 0 {
		f.from = meta.FirstTs
	}

	// 每个周期按自己的 max(Settle) + Lookback 往前多读一段。
	warmOf := func(s *tfSeries) int64 {
		if cfg.WarmFrom > 0 {
			return cfg.WarmFrom
		}
		if cfg.NoAutoWarmup {
			return f.from
		}
		return warmStart(s.period, f.from, (s.settle+s.capN)*warmupSlack)
	}

	baseWarm := warmOf(f.base)
	if cfg.Aggregate {
		// 聚合模式下辅周期的历史只能从主周期喂出来，主周期得读到最早的那个需求。
		for _, s := range f.extras {
			if w := warmOf(s); w < baseWarm {
				baseWarm = w
			}
		}
	}
	if baseWarm < meta.FirstTs {
		baseWarm = meta.FirstTs
	}

	it, err := f.store.Iter(f.inst, cfg.Base, baseWarm, f.to)
	if err != nil {
		return err
	}
	f.base.it = &peekIter{it: it}

	if cfg.MarkStore != nil {
		mit, err := cfg.MarkStore.Iter(f.inst, cfg.Base, baseWarm, f.to)
		if err != nil {
			if errors.Is(err, ErrNoSeries) {
				return fmt.Errorf("tickflow: 标记价序列 %s/%s 不在 MarkStore 里——"+
					"先同步它（okxsource.MarkPrice + segfile.Mark 命名空间）: %w",
					f.inst, cfg.Base, err)
			}
			return err
		}
		f.markIt = &peekIter{it: mit}
	}

	if cfg.Aggregate {
		return nil
	}
	for _, s := range f.extras {
		w := warmOf(s)
		eit, err := f.store.Iter(f.inst, s.bar, w, f.to)
		if err != nil {
			// 辅周期没同步过是个常见错误，把话说明白。
			if errors.Is(err, ErrNoSeries) {
				return fmt.Errorf("tickflow: 辅周期 %s/%s 在库里不存在——"+
					"先同步它，或者用 FeedConfig.Aggregate 从主周期聚合: %w", f.inst, s.bar, err)
			}
			return err
		}
		s.it = &peekIter{it: eit}
	}
	return nil
}

// Next 前进一根主周期 K 线。返回 false 表示走完了或出错，用 Err 区分。
//
// 每前进一根，辅周期会被推进到【本根收盘时刻之前最后一根已收盘】的位置。
func (f *Feed) Next() bool {
	if f.err != nil || f.closed || f.base.it == nil {
		return false
	}
	for {
		c, ok := f.base.it.take()
		if !ok {
			f.err = f.base.it.err
			return false
		}
		if f.to > 0 && c.Ts >= f.to {
			return false
		}
		if err := f.step(c); err != nil {
			f.err = err
			return false
		}
		// 预热阶段只喂指标，不产出步进。
		if c.Ts >= f.from {
			return true
		}
	}
}

// step 处理一根主周期 K 线：先把辅周期推进到位，再推入主周期。
//
// 顺序要紧。辅周期先到位，View() 与 TF() 才在同一时刻上；反过来的话，
// 主周期这一根看到的是【上一根时刻】的辅周期，晚了整整一步。
func (f *Feed) step(c Candle) error {
	closeTs := f.base.period.Next(c.Ts)

	for _, s := range f.extras {
		if s.agg != nil {
			for _, done := range s.agg.add(c, closeTs) {
				if err := s.push(done); err != nil {
					return err
				}
			}
			continue
		}
		if s.it == nil {
			continue
		}
		// 只吃掉在本根收盘之前就已经收盘的那些。
		target := s.period.LastClosed(closeTs)
		for {
			ec, ok := s.it.peek()
			if !ok || ec.Ts > target {
				if s.it.err != nil {
					return s.it.err
				}
				break
			}
			s.it.take()
			if err := s.push(ec); err != nil {
				return err
			}
		}
	}
	if err := f.base.push(c); err != nil {
		return err
	}
	f.advanceMark(c.Ts)
	return nil
}

// advanceMark 把标记价序列推到与主周期这一根【同一个】开盘时刻上。
//
// 用同 ts 而不是「最后一根已收盘」：标记价 K 线与主周期这一根是同时收盘的，
// 它的收盘价在这一刻就已知，不构成未来函数。更早的那些说明主周期有空洞，
// 丢掉即可；更晚的说明标记价这一根缺失，留空。
func (f *Feed) advanceMark(ts int64) {
	if f.markIt == nil || f.base.marks == nil {
		return
	}
	for {
		m, ok := f.markIt.peek()
		if !ok || m.Ts > ts {
			return
		}
		f.markIt.take()
		if m.Ts == ts {
			f.base.setMark(m)
			return
		}
	}
}

// Push 手工喂一根【已完结】的 K 线，供实盘使用。
//
// 实盘从 WebSocket 拿到收盘的 K 线后调它，指标与视图的代码就和回测完全一样，
// 省掉「回测跑通了实盘对不上」这类最难查的问题。
//
// 喂主周期时：Aggregate 模式下辅周期会跟着自动推进；否则辅周期需要调用方
// 自己 Push——实盘没有库可读，本库不替使用者臆造那几根 K 线。
//
// ts 必须严格递增且落在该周期的对齐网格上，否则返回错误。实盘里重复推送与
// 乱序推送都真实存在，静默接受会让指标悄悄算错。
func (f *Feed) Push(bar string, c Candle) error {
	s, ok := f.byBar[bar]
	if !ok {
		return fmt.Errorf("tickflow: 周期 %q 不属于本 Feed（有 %s）", bar, strings.Join(f.Bars(), ", "))
	}
	if s.period.Truncate(c.Ts) != c.Ts {
		return fmt.Errorf("tickflow: K 线 %d 没落在 %s 的对齐网格上，应当是 %d",
			c.Ts, bar, s.period.Truncate(c.Ts))
	}
	if s.n > 0 {
		if last := s.at(s.n - 1).Ts; c.Ts <= last {
			return fmt.Errorf("tickflow: %s 的 K 线必须严格递增，%d 不晚于已有的 %d", bar, c.Ts, last)
		}
	}
	if s == f.base {
		return f.step(c)
	}
	return s.push(c)
}

// PushWithMark 同 Push，并给出这一根对应的【标记价】K 线，供实盘使用。
//
// 只能推主周期——标记价只对主周期生效，见 FeedConfig.MarkStore。
// mark 的 ts 必须与 c 相同：它们是同一个时刻的两条序列，对不上多半是取错了根。
func (f *Feed) PushWithMark(bar string, c, mark Candle) error {
	s, ok := f.byBar[bar]
	if !ok || s != f.base {
		return fmt.Errorf("tickflow: 标记价只对主周期 %q 生效，不能推给 %q", f.base.bar, bar)
	}
	if f.base.marks == nil {
		return errors.New("tickflow: 本 Feed 没有配 MarkStore，View.MarkPx 不会有值")
	}
	if mark.Ts != c.Ts {
		return fmt.Errorf("tickflow: 标记价 K 线的 ts %d 与行情的 %d 对不上——"+
			"它们是同一时刻的两条序列", mark.Ts, c.Ts)
	}
	if err := f.Push(bar, c); err != nil {
		return err
	}
	f.base.setMark(mark)
	return nil
}

// View 返回主周期【当前这一根】的视图。
func (f *Feed) View() View { return f.base.view() }

// TF 返回某个周期的视图。主周期给当前这根，辅周期给【最后一根已收盘】的。
//
// 这是本库最主要的价值：主周期走到 2026-01-15 10:15 时，TF("1D") 给的是
// 01-14 那根，不是当天那根还没走完的。「高周期 K 线在收盘前不可见」是自制
// 回测里最常见、也最难自查的未来函数。
//
// 周期不属于本 Feed 时返回一个无效视图（Valid() 为 false），
// 取值一律是 NaN——不 panic，但也不会静默给出一个像模像样的数。
func (f *Feed) TF(bar string) View {
	s, ok := f.byBar[bar]
	if !ok {
		return View{}
	}
	return s.view()
}

// Handle 预解析一个键名，供热路径使用。
//
//	h, _ := f.Handle("15m", "ema20")   // 循环外
//	v.At(h)                            // 循环内，没有 map 查找
func (f *Feed) Handle(bar, key string) (Handle, error) {
	s, ok := f.byBar[bar]
	if !ok {
		return Handle{}, fmt.Errorf("tickflow: 周期 %q 不属于本 Feed（有 %s）", bar, strings.Join(f.Bars(), ", "))
	}
	col, ok := s.keyIdx[key]
	if !ok {
		return Handle{}, fmt.Errorf("tickflow: 周期 %s 上没有指标 %q（有 %s）",
			bar, key, strings.Join(s.keys, ", "))
	}
	return Handle{s: s, col: col}, nil
}

// Bars 返回本 Feed 登记的全部周期，主周期在最前。
func (f *Feed) Bars() []string {
	out := make([]string, 0, len(f.order))
	for _, s := range f.order {
		out = append(out, s.bar)
	}
	return out
}

// Keys 返回某周期上全部指标的键名。
func (f *Feed) Keys(bar string) []string {
	s, ok := f.byBar[bar]
	if !ok {
		return nil
	}
	return append([]string(nil), s.keys...)
}

// Ready 报告【所有】周期的指标是否都已 warmup 完。
func (f *Feed) Ready() bool {
	for _, s := range f.order {
		if !s.ready() {
			return false
		}
	}
	return true
}

// Err 返回推进过程中发生的错误。正常走完时为 nil。
func (f *Feed) Err() error { return f.err }

// Close 释放存储游标。
func (f *Feed) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	var first error
	its := make([]*peekIter, 0, len(f.order)+1)
	for _, s := range f.order {
		its = append(its, s.it)
	}
	its = append(its, f.markIt)
	for _, it := range its {
		if it == nil {
			continue
		}
		if err := it.it.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---------------------------------------------------------------- 单周期序列

// tfSeries 是一个周期上的环形缓冲：K 线一份，指标各占一列。
//
// 列式而不是每根一个 map：回测要走上百万步，每步给每个指标分配一个 map
// 是纯粹的浪费。列式之下 View 只是「一个指针加一个下标」，取值是一次数组寻址。
type tfSeries struct {
	bar    string
	period Period

	inds   []Indicator
	widths []int
	keys   []string
	keyIdx map[string]int
	warmup int // 值从此【有定义】所需的根数
	settle int // 值从此【不再取决于从哪根开始喂】所需的根数

	capN    int
	candles []Candle
	marks   []Candle // 仅主周期，且只在配了 MarkStore 时分配
	cols    [][]float64
	n       int64 // 已推入的总根数，也是下一根的绝对序号

	it  *peekIter
	agg *aggregator
}

func newTFSeries(bar string, p Period, inds []Indicator, lookback int) (*tfSeries, error) {
	s := &tfSeries{
		bar:    bar,
		period: p,
		inds:   inds,
		keyIdx: map[string]int{},
		capN:   lookback + 1,
	}
	for _, ind := range inds {
		if ind == nil {
			return nil, fmt.Errorf("tickflow: 周期 %s 上挂了一个 nil 指标", bar)
		}
		ks := IndicatorKeys(ind)
		for _, k := range ks {
			if _, dup := s.keyIdx[k]; dup {
				return nil, fmt.Errorf("tickflow: 周期 %s 上有两个指标都叫 %q；"+
					"用 indicator.Named(...) 给其中一个改名", bar, k)
			}
			s.keyIdx[k] = len(s.keys)
			s.keys = append(s.keys, k)
		}
		s.widths = append(s.widths, len(ks))
		if w := ind.Warmup(); w > s.warmup {
			s.warmup = w
		}
		// Warmup 与 Settle 是两回事：前者是「有定义」，后者是「不再取决于从哪根
		// 开始喂」。递归类指标（EMA / MACD / RSI / 国内口径 KDJ）两者相差一到两个
		// 数量级——只按 Warmup 预热，MACD 的值能错一倍，而 Ready() 还报 true。
		if n := IndicatorSettle(ind); n > s.settle {
			s.settle = n
		}
		ind.Reset()
	}
	s.candles = make([]Candle, s.capN)
	s.cols = make([][]float64, len(s.keys))
	for i := range s.cols {
		s.cols[i] = make([]float64, s.capN)
		for j := range s.cols[i] {
			s.cols[i][j] = math.NaN()
		}
	}
	return s, nil
}

func (s *tfSeries) push(c Candle) error {
	slot := int(s.n % int64(s.capN))
	s.candles[slot] = c
	// 环形缓冲会复用槽位。不清空的话，标记价缺根时这一格里留着的是
	// capN 根之前那一根的值——那比没有标记价更糟，它看起来是对的。
	if s.marks != nil {
		s.marks[slot] = Candle{}
	}
	off := 0
	for i, ind := range s.inds {
		v := ind.Update(c)
		if len(v) != s.widths[i] {
			return fmt.Errorf("tickflow: 指标 %s 的 Update 返回了 %d 个值，"+
				"但它声明的字段数是 %d——两者必须一致", ind.Name(), len(v), s.widths[i])
		}
		for k, x := range v {
			s.cols[off+k][slot] = x
		}
		off += s.widths[i]
	}
	s.n++
	return nil
}

func (s *tfSeries) at(abs int64) Candle { return s.candles[int(abs%int64(s.capN))] }

// setMark 给【刚推入的那一根】挂上标记价 K 线。须在 push 之后调用。
func (s *tfSeries) setMark(m Candle) {
	if s.marks == nil || s.n == 0 {
		return
	}
	s.marks[int((s.n-1)%int64(s.capN))] = m
}

func (s *tfSeries) markAt(abs int64) Candle {
	if s.marks == nil {
		return Candle{}
	}
	return s.marks[int(abs%int64(s.capN))]
}

// ready 报告本周期的指标值是否【可信】。
//
// 用 settle 而不是 warmup：warmup 只保证「算得出一个数」，而递归类指标在那之后
// 还要走上百根才不再受播种影响。历史不够长时 ready 会一直是 false——那是对的，
// 让人拿到一个错一倍的 MACD 却报 ready，才是真正的问题。
func (s *tfSeries) ready() bool { return s.n >= int64(s.settle) }

// defined 报告指标值是否已有定义（不是 NaN），但不保证已收敛。
func (s *tfSeries) defined() bool { return s.n >= int64(s.warmup) }

func (s *tfSeries) view() View { return View{s: s, abs: s.n - 1} }

// ---------------------------------------------------------------- 视图

// View 是某个周期上某一根 K 线的视图。
//
// 它是值类型：一个指针加一个下标，在栈上，Prev(n) 只是把下标往回挪。
// 回测每步取十几次值都不会有分配。
type View struct {
	s   *tfSeries
	abs int64
}

// Valid 报告本视图是否指向一根真实存在、且还在回看窗口内的 K 线。
//
// Prev(n) 超出 Lookback、或还没走到第 n 根时会得到无效视图。无效视图不 panic，
// 取值一律是 NaN 或零值——但请先判 Valid，别把 NaN 当成行情。
func (v View) Valid() bool {
	return v.s != nil && v.abs >= 0 && v.abs < v.s.n && v.abs >= v.s.n-int64(v.s.capN)
}

// Bar 返回本视图所属的周期。
func (v View) Bar() string {
	if v.s == nil {
		return ""
	}
	return v.s.bar
}

// Prev 往回看 n 根。n 不能超过 FeedConfig.Lookback，否则得到无效视图。
func (v View) Prev(n int) View { return View{s: v.s, abs: v.abs - int64(n)} }

// Ready 报告本周期的指标值是否【可信】——不只是「算得出来」，而是「不再取决于
// 从哪根开始喂」。典型用法是在循环开头 if !v.Ready() { continue }。
//
// 这两件事对窗口类指标是一回事，对递归类（EMA / MACD / RSI / 国内口径 KDJ）
// 差一到两个数量级。实测 MACD(12,26,9) 国内口径：Warmup() 报 1，而只喂 4 根时
// dif 的相对误差是 1.01——值错了一倍。所以 Ready 按 Settle 判，不按 Warmup。
//
// 历史不够长时 Ready 会一直是 false。那是对的：拿不到可信的值，就该说拿不到。
// 真要那个「已有定义但未必收敛」的弱判据，用 Defined。
func (v View) Ready() bool { return v.s != nil && v.s.ready() }

// Defined 报告指标值是否已有定义（不是 NaN），但【不保证已收敛】。
//
// 这是比 Ready 弱的判据。除非你清楚递归类指标的播种影响对你的策略无所谓，
// 否则用 Ready。
func (v View) Defined() bool { return v.s != nil && v.s.defined() }

// Candle 返回这一根 K 线；视图无效时返回零值。
func (v View) Candle() Candle {
	if !v.Valid() {
		return Candle{}
	}
	return v.s.at(v.abs)
}

// Ts 返回开盘时间（毫秒）；视图无效时返回 0。
func (v View) Ts() int64 { return v.Candle().Ts }

// Open 返回开盘价；视图无效时返回 NaN。
//
// 五个取值器都返回 NaN 而不是 0：0 是个看起来正常的价格，NaN 会沿运算传染，
// 把「用了一根不存在的 K 线」这件事当场暴露出来。
func (v View) Open() float64 { return v.px(func(c Candle) float64 { return c.Open }) }

// High 返回最高价；视图无效时返回 NaN。
func (v View) High() float64 { return v.px(func(c Candle) float64 { return c.High }) }

// Low 返回最低价；视图无效时返回 NaN。
func (v View) Low() float64 { return v.px(func(c Candle) float64 { return c.Low }) }

// Close 返回收盘价；视图无效时返回 NaN。
func (v View) Close() float64 { return v.px(func(c Candle) float64 { return c.Close }) }

// Vol 返回成交量；视图无效时返回 NaN。
func (v View) Vol() float64 { return v.px(func(c Candle) float64 { return c.Vol }) }

func (v View) px(get func(Candle) float64) float64 {
	if !v.Valid() {
		return math.NaN()
	}
	return get(v.s.at(v.abs))
}

// Mark 返回这一根对应的【标记价】K 线。
//
// 第二个返回值为 false 表示没有：Feed 没配 MarkStore、视图无效、或者标记价
// 序列在这个时刻缺根。缺根是真实存在的——别把零值当成价格。
func (v View) Mark() (Candle, bool) {
	if !v.Valid() {
		return Candle{}, false
	}
	m := v.s.markAt(v.abs)
	return m, m.Ts != 0
}

// MarkPx 返回标记价（标记价 K 线的收盘价）；没有时返回 NaN。
//
// 喂给 okx-position-simulator-go 时用它：
//
//	bar, _ := simbar.ToBar(inst, v.Candle(), simbar.WithMarkPx(v.MarkPx()))
//
// 缺标记价时给 NaN 而不是 0，与本库其余部分一致——0 是个看起来正常的价格。
// simbar.ToBar 会把 NaN 的标记价当作「没有」处理，而不是写进 Bar。
func (v View) MarkPx() float64 {
	m, ok := v.Mark()
	if !ok {
		return math.NaN()
	}
	return m.Close
}

// Ind 按键名取指标值。键名未知或视图无效时返回 NaN。
//
// 热路径请改用 Handle + At，省掉这里的一次 map 查找。
func (v View) Ind(key string) float64 {
	if !v.Valid() {
		return math.NaN()
	}
	col, ok := v.s.keyIdx[key]
	if !ok {
		return math.NaN()
	}
	return v.s.cols[col][int(v.abs%int64(v.s.capN))]
}

// At 按预解析的 Handle 取指标值，没有 map 查找。
//
// Handle 来自另一个周期时返回 NaN——取错周期的指标是个安静的错，
// 一次指针比较换一个当场暴露，值。
func (v View) At(h Handle) float64 {
	if !v.Valid() || h.s != v.s {
		return math.NaN()
	}
	return v.s.cols[h.col][int(v.abs%int64(v.s.capN))]
}

// Handle 是预解析过的指标键名，绑定在某个周期上。
type Handle struct {
	s   *tfSeries
	col int
}

// ---------------------------------------------------------------- 聚合

// aggregator 把主周期的 K 线并成高周期的 K 线。
//
// 它只吐【已收盘】的高周期 K 线：一根在它的收盘时刻被主周期越过时才出来。
// 这与从库里读独立序列的时点完全一致——两种模式给出的可见性必须一样，
// 不然换个 Aggregate 开关回测结果就变了。
type aggregator struct {
	p   Period
	cur Candle
	has bool

	// out 是复用的返回缓冲。一步最多收两根，用定长数组而不是 append：
	// 每次收盘 append 一个新切片，在 15m 聚 1H 的场合就是每四步一次分配，
	// 而整套视图机制的立足点正是热路径零分配。
	out [2]Candle
}

// add 喂入一根主周期 K 线，返回本步收盘的高周期 K 线（0 到 2 根）。
//
// 会有 2 根，是因为主周期【有空洞】时可能一步跨过一整根高周期：手上那根
// 就此收盘，而新落进来的这根若又恰好是它所属高周期的最后一根，也当场收盘。
//
// 返回的切片下一次调用就会被覆盖，调用方须当场用掉。
func (a *aggregator) add(c Candle, baseClose int64) []Candle {
	n := 0
	open := a.p.Truncate(c.Ts)

	if a.has && a.cur.Ts != open {
		a.out[n] = a.cur
		n++
		a.has = false
	}
	if !a.has {
		a.cur = Candle{Ts: open, Open: c.Open, High: c.High, Low: c.Low}
		a.has = true
	}
	if c.High > a.cur.High {
		a.cur.High = c.High
	}
	if c.Low < a.cur.Low {
		a.cur.Low = c.Low
	}
	a.cur.Close = c.Close
	a.cur.Vol += c.Vol
	a.cur.VolCcy += c.VolCcy
	a.cur.VolCcyQuote += c.VolCcyQuote

	if a.p.Next(a.cur.Ts) <= baseClose {
		a.out[n] = a.cur
		n++
		a.has = false
	}
	return a.out[:n]
}

// ---------------------------------------------------------------- 游标

// peekIter 给 Iterator 加一格预读，好在不消费的前提下判断该不该推进。
type peekIter struct {
	it   Iterator
	cur  Candle
	has  bool
	done bool
	err  error
}

func (p *peekIter) peek() (Candle, bool) {
	if p.has {
		return p.cur, true
	}
	if p.done || p.err != nil {
		return Candle{}, false
	}
	if !p.it.Next() {
		p.done = true
		p.err = p.it.Err()
		return Candle{}, false
	}
	p.cur, p.has = p.it.Candle(), true
	return p.cur, true
}

func (p *peekIter) take() (Candle, bool) {
	c, ok := p.peek()
	p.has = false
	return c, ok
}

// ---------------------------------------------------------------- 周期辅助

// refTs 是比较周期长短时的参照点：2026-01-01T00:00:00Z。
const refTs = int64(1767225600000)

// periodSpan 返回一个周期在参照点上的长度，用于比较两个周期谁更长。
// 月线不定长，所以只能取某一点上的实际长度来比。
func periodSpan(p Period) int64 {
	base := p.Truncate(refTs)
	return p.Next(base) - base
}

// checkNesting 校验高周期的边界都落在低周期的网格上。
//
// 聚合要求一根主周期 K 线完整落在某一根高周期里；4H 聚合 6H 就不成立——
// 主周期会横跨两根高周期，聚合出来的东西没有意义。这里用真实边界抽样验，
// 而不是去推导两套锚点的整除关系：后者要对每种周期组合分别论证，容易漏。
func checkNesting(base, high Period) error {
	ts := high.Truncate(refTs)
	for i := 0; i < 64; i++ {
		if base.Truncate(ts) != ts {
			return fmt.Errorf("tickflow: %s 的边界 %d 没落在 %s 的网格上，无法聚合；"+
				"关掉 FeedConfig.Aggregate 改为从库里读独立序列",
				high.Bar(), ts, base.Bar())
		}
		ts = high.Next(ts)
	}
	return nil
}

// checkSharedIndicators 挡住「同一个指标实例挂在多处」。
//
// 指标是有状态的：Update 要往里写。同一个实例被两个周期共用，两边的 K 线会
// 交替推进同一份状态，算出来的值两边都是错的——而且【不报错】，是这个库里
// 最容易产生无声错误的一处使用方式。
//
// 按指针身份判定。非指针的实现存不住状态，共用它也不会被污染，跳过。
func checkSharedIndicators(cfg FeedConfig) error {
	// 按 Base、Extra 的顺序遍历，让错误信息稳定可复现——map 的遍历顺序是随机的。
	bars := append([]string{cfg.Base}, cfg.Extra...)
	seen := map[any]string{}
	seenBar := map[string]bool{}
	for _, bar := range bars {
		// 同一个周期出现两次是【另一回事】（Base 与 Extra 撞了），由后面那条更精确
		// 的检查去报。这里若不跳过，会把它误诊成「指标实例被共用」。
		if seenBar[bar] {
			continue
		}
		seenBar[bar] = true
		for _, ind := range cfg.Indicators[bar] {
			if ind == nil || reflect.ValueOf(ind).Kind() != reflect.Pointer {
				continue
			}
			if prev, dup := seen[ind]; dup {
				where := fmt.Sprintf("周期 %q 与 %q", prev, bar)
				if prev == bar {
					where = fmt.Sprintf("周期 %q 上出现了两次", bar)
				}
				return fmt.Errorf("tickflow: 指标 %q 的同一个实例挂在了%s；"+
					"指标是有状态的，共用一个实例会让两边的值互相污染。"+
					"给每处各建一个（indicator.MA(20) 调两次）", ind.Name(), where)
			}
			seen[ind] = bar
		}
	}
	return nil
}

// warmStart 从 from 往回退 bars 根，返回预热的起点。
func warmStart(p Period, from int64, bars int) int64 {
	if bars <= 0 {
		return from
	}
	if p.Fixed() {
		return p.Truncate(from) - int64(bars)*p.Step().Milliseconds()
	}
	ts := p.Truncate(from)
	for i := 0; i < bars; i++ {
		prev := p.Truncate(ts - 1)
		if prev >= ts {
			break
		}
		ts = prev
	}
	return ts
}
