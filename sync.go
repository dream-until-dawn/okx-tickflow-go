package tickflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SyncRequest 是一次同步请求。
type SyncRequest struct {
	InstID string
	Bar    string

	// From 是起始时刻（含），【必填】。
	//
	// 不提供「从最早开始」的语义：history-candles 的可回溯深度按周期不同，
	// 且 OKX 未文档化，无边界地往前探测既慢又无从判断该在哪停——一段区间没有
	// K 线，可能是到头了，也可能只是那阵子没成交。要深回填就自己按窗口分段调用。
	From int64

	// To 是结束时刻（不含）。0 表示同步到【最后一根已收盘】的 K 线。
	To int64
}

// Report 是一次同步的结果。
type Report struct {
	InstID string
	Bar    string

	// Range 是本次实际处理的区间（已按周期对齐、并被「最后一根已收盘」截断）。
	Range Range

	Added   int64 // 新落库的 K 线根数
	Fetches int   // 调用 Source.Fetch 的次数（不是 HTTP 请求数，一次 Fetch 内部可能翻多页）

	// Covered 是本次新增的覆盖区间。
	Covered Ranges

	// Gaps 是【请求过但一根 K 线都没返回】的区间。
	//
	// 这不一定是故障：小币种在冷清时段本就不产出 K 线。列出来是为了让使用者
	// 自己判断——同一段区间反复出现在这里，才说明可能真的到了历史尽头。
	Gaps Ranges

	// Misaligned 是开盘时间没落在周期网格上的 K 线根数。
	//
	// 正常应当恒为 0。不为 0 说明该周期的锚点对不上了——内置表里 6H 及以上的
	// 锚点是从 OKX 未文档化的行为里推定的，交易所改了不会通知谁。此时应当用
	// RegisterFixedPeriod 覆盖该周期的锚点，再重新同步。
	Misaligned int64
}

// Syncer 把 Source 的数据落进 Store，只拉 coverage 里缺的那些段。
type Syncer struct {
	src   Source
	store Store

	chunk    int
	maxMerge int
	now      func() int64
}

// SyncOption 配置 Syncer。
type SyncOption func(*Syncer)

// WithChunkCandles 设置单次请求覆盖多少根 K 线，默认 3000。
//
// 它决定了内存占用的粒度与请求的次数：调大省请求，调小省内存、且被打断时
// 丢掉的进度更少（追加路径每个 chunk 落一次库）。
func WithChunkCandles(n int) SyncOption {
	return func(s *Syncer) {
		if n > 0 {
			s.chunk = n
		}
	}
}

// WithMaxMergeCandles 设置一次回填最多在内存里攒多少根，默认 100 万（约 64MB）。
//
// 回填（要写的数据比已有数据更早）必须攒成一次 Merge——分块多次 Merge 会是
// O(n²) 的全文件重写。超过上限时返回错误，请把 From/To 切小分几次调用。
func WithMaxMergeCandles(n int) SyncOption {
	return func(s *Syncer) {
		if n > 0 {
			s.maxMerge = n
		}
	}
}

// WithClock 注入时钟，仅供测试。
func WithClock(now func() int64) SyncOption {
	return func(s *Syncer) {
		if now != nil {
			s.now = now
		}
	}
}

// NewSyncer 构造一个 Syncer。
func NewSyncer(src Source, store Store, opts ...SyncOption) *Syncer {
	s := &Syncer{
		src:      src,
		store:    store,
		chunk:    3000,
		maxMerge: 1_000_000,
		now:      func() int64 { return time.Now().UnixMilli() },
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Sync 把 [From, To) 内缺失的 K 线拉下来落库。
//
// 已经覆盖过的区间不会重复请求。区间末端会被截到【最后一根已收盘】的 K 线：
// 当前那根还在走，收盘价尚不可知，若把它算进覆盖，等它收盘后就再也不会去补了。
func (s *Syncer) Sync(ctx context.Context, req SyncRequest) (Report, error) {
	rep := Report{InstID: req.InstID, Bar: req.Bar}

	if req.InstID == "" {
		return rep, errors.New("tickflow: instId 不能为空")
	}
	if req.From <= 0 {
		return rep, errors.New("tickflow: From 必填，且须为正的毫秒时间戳")
	}
	p, err := ParsePeriod(req.Bar)
	if err != nil {
		return rep, err
	}

	// 当前这根 K 线还在走，它的收盘价尚不可知——覆盖区间只能记到它的开盘时刻为止。
	safeTo := p.Truncate(s.now())
	to := safeTo
	if req.To > 0 {
		to = min64(req.To, safeTo)
	}
	from := p.Truncate(req.From)
	if from >= to {
		return rep, nil
	}
	want := Range{From: from, To: to}
	rep.Range = want

	meta, err := s.store.Meta(req.InstID, req.Bar)
	if err != nil && !errors.Is(err, ErrNoSeries) {
		return rep, err
	}

	for _, miss := range meta.Coverage.Missing(want) {
		// 缺的这段整体晚于已有数据 → 纯追加，可以逐块落库；
		// 否则是回填，必须攒成一次 Merge，否则会 O(n²) 地反复重写整个文件。
		tail := meta.Count == 0 || miss.From > meta.LastTs
		var err error
		if tail {
			err = s.syncTail(ctx, p, req, miss, &rep)
		} else {
			err = s.syncBackfill(ctx, p, req, miss, &rep)
		}
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// syncTail 处理「缺失段整体晚于已有数据」的情形：逐块拉、逐块落库、逐块记覆盖。
// 被打断时已落库的部分连同覆盖都留着，下次接着跑。
func (s *Syncer) syncTail(ctx context.Context, p Period, req SyncRequest, miss Range, rep *Report) error {
	for _, ck := range s.chunks(p, miss) {
		cs, err := s.fetch(ctx, p, req, ck, rep)
		if err != nil {
			return err
		}
		if len(cs) > 0 {
			if err := s.store.Append(req.InstID, req.Bar, cs); err != nil {
				return fmt.Errorf("tickflow: 落库失败 %s/%s %s: %w", req.InstID, req.Bar, ck, err)
			}
			rep.Added += int64(len(cs))
		} else {
			rep.Gaps = rep.Gaps.Add(ck)
		}
		if err := s.store.AddCoverage(req.InstID, req.Bar, ck); err != nil {
			return err
		}
		rep.Covered = rep.Covered.Add(ck)
	}
	return nil
}

// syncBackfill 处理回填：整段攒在内存里，一次 Merge 写完，成功后才记覆盖。
func (s *Syncer) syncBackfill(ctx context.Context, p Period, req SyncRequest, miss Range, rep *Report) error {
	var buf []Candle
	for _, ck := range s.chunks(p, miss) {
		cs, err := s.fetch(ctx, p, req, ck, rep)
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			rep.Gaps = rep.Gaps.Add(ck)
			continue
		}
		if len(buf)+len(cs) > s.maxMerge {
			return fmt.Errorf("tickflow: 回填 %s/%s %s 需要在内存里攒超过 %d 根 K 线，"+
				"已超出上限；请把 From/To 切成更小的窗口分几次同步，"+
				"或用 WithMaxMergeCandles 调高上限",
				req.InstID, req.Bar, miss, s.maxMerge)
		}
		buf = append(buf, cs...)
	}
	if len(buf) > 0 {
		if err := s.store.Merge(req.InstID, req.Bar, buf); err != nil {
			return fmt.Errorf("tickflow: 回填落库失败 %s/%s %s: %w", req.InstID, req.Bar, miss, err)
		}
		rep.Added += int64(len(buf))
	}
	if err := s.store.AddCoverage(req.InstID, req.Bar, miss); err != nil {
		return err
	}
	rep.Covered = rep.Covered.Add(miss)
	return nil
}

// fetch 拉一块，并把结果清洗成「升序、去重、落在区间内」的样子。
// Source 本就该保证这些，这里再兜一道——数据层的脏数据会在指标上放大成无声的错。
func (s *Syncer) fetch(ctx context.Context, p Period, req SyncRequest, ck Range, rep *Report) ([]Candle, error) {
	cs, err := s.src.Fetch(ctx, FetchRequest{
		InstID: req.InstID, Bar: req.Bar, From: ck.From, To: ck.To,
	})
	rep.Fetches++
	if err != nil {
		return nil, fmt.Errorf("tickflow: 拉取失败 %s/%s %s: %w", req.InstID, req.Bar, ck, err)
	}

	out := cs[:0:0]
	var last int64
	for _, c := range cs {
		if !ck.Contains(c.Ts) {
			continue
		}
		if len(out) > 0 && c.Ts <= last {
			continue // 去重并丢弃乱序
		}
		if p.Truncate(c.Ts) != c.Ts {
			rep.Misaligned++
		}
		out = append(out, c)
		last = c.Ts
	}
	return out, nil
}

// chunks 把一个区间切成每块约 chunk 根 K 线的若干小段。
func (s *Syncer) chunks(p Period, r Range) []Range {
	var out []Range
	if p.Fixed() {
		span := int64(s.chunk) * p.Step().Milliseconds()
		for cur := r.From; cur < r.To; cur += span {
			out = append(out, Range{From: cur, To: min64(cur+span, r.To)})
		}
		return out
	}
	// 日历周期（1M / 3M）不定长，只能一根一根往前推。这类周期总量本就极小。
	for cur := r.From; cur < r.To; {
		end := cur
		for i := 0; i < s.chunk && end < r.To; i++ {
			end = p.Next(end)
		}
		out = append(out, Range{From: cur, To: min64(end, r.To)})
		cur = end
	}
	return out
}
