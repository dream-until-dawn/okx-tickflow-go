package tickflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// ---------------------------------------------------------------- 测试替身

// memStore 是内存里的 Store，用于把 Syncer 的路由行为（追加 vs 回填）验清楚。
// 它对 Append 的约束比真实实现更严——路由错了要当场炸，而不是悄悄写进去。
type memStore struct {
	data map[string][]Candle
	cov  map[string]Ranges

	appends, merges int
}

func newMemStore() *memStore {
	return &memStore{data: map[string][]Candle{}, cov: map[string]Ranges{}}
}

func key(instID, bar string) string { return instID + "/" + bar }

func (m *memStore) Append(instID, bar string, cs []Candle) error {
	if len(cs) == 0 {
		return nil
	}
	m.appends++
	k := key(instID, bar)
	have := m.data[k]
	if len(have) > 0 && cs[0].Ts <= have[len(have)-1].Ts {
		return fmt.Errorf("memStore: Append 只能追加在末尾，首根 %d 不晚于 LastTs %d",
			cs[0].Ts, have[len(have)-1].Ts)
	}
	m.data[k] = append(have, cs...)
	return nil
}

func (m *memStore) Merge(instID, bar string, cs []Candle) error {
	if len(cs) == 0 {
		return nil
	}
	m.merges++
	k := key(instID, bar)
	byTs := map[int64]Candle{}
	for _, c := range m.data[k] {
		byTs[c.Ts] = c
	}
	for _, c := range cs {
		byTs[c.Ts] = c
	}
	out := make([]Candle, 0, len(byTs))
	for _, c := range byTs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts < out[j].Ts })
	m.data[k] = out
	return nil
}

func (m *memStore) Meta(instID, bar string) (Meta, error) {
	k := key(instID, bar)
	cs, ok := m.data[k]
	if _, hasCov := m.cov[k]; !ok && !hasCov {
		return Meta{}, ErrNoSeries
	}
	meta := Meta{InstID: instID, Bar: bar, Count: int64(len(cs)), Coverage: m.cov[k]}
	if len(cs) > 0 {
		meta.FirstTs, meta.LastTs = cs[0].Ts, cs[len(cs)-1].Ts
	}
	return meta, nil
}

func (m *memStore) AddCoverage(instID, bar string, r Range) error {
	k := key(instID, bar)
	m.cov[k] = m.cov[k].Add(r)
	return nil
}

func (m *memStore) Iter(instID, bar string, from, to int64) (Iterator, error) {
	cs, err := m.Range(instID, bar, from, to)
	if err != nil {
		return nil, err
	}
	return &sliceIter{cs: cs, i: -1}, nil
}

func (m *memStore) Range(instID, bar string, from, to int64) ([]Candle, error) {
	k := key(instID, bar)
	if _, ok := m.data[k]; !ok {
		// 与 segfile 保持一致：没存过的序列要报 ErrNoSeries，而不是给个空切片。
		// 测试替身在这里撒谎的话，「辅周期没同步过」这类错误就测不出来。
		return nil, fmt.Errorf("%w: %s/%s", ErrNoSeries, instID, bar)
	}
	var out []Candle
	for _, c := range m.data[k] {
		if c.Ts >= from && (to == 0 || c.Ts < to) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memStore) Series() ([]SeriesID, error) { return nil, nil }
func (m *memStore) Close() error                { return nil }

type sliceIter struct {
	cs []Candle
	i  int
}

func (s *sliceIter) Next() bool     { s.i++; return s.i < len(s.cs) }
func (s *sliceIter) Candle() Candle { return s.cs[s.i] }
func (s *sliceIter) Err() error     { return nil }
func (s *sliceIter) Close() error   { return nil }

var _ Store = (*memStore)(nil)

// fakeSource 从一段预置数据里切片返回，并记下每次被请求的区间。
type fakeSource struct {
	have  []Candle
	calls []Range
	fail  error
}

func (f *fakeSource) Fetch(_ context.Context, req FetchRequest) ([]Candle, error) {
	f.calls = append(f.calls, Range{From: req.From, To: req.To})
	if f.fail != nil {
		return nil, f.fail
	}
	var out []Candle
	for _, c := range f.have {
		if c.Ts >= req.From && c.Ts < req.To {
			out = append(out, c)
		}
	}
	return out, nil
}

// series 造一串等间隔的 K 线。
func series(start int64, step int64, n int) []Candle {
	out := make([]Candle, n)
	for i := range out {
		out[i] = Candle{
			Ts:   start + int64(i)*step,
			Open: float64(i), High: float64(i) + 1, Low: float64(i) - 1,
			Close: float64(i) + 0.5, Vol: float64(i) * 10,
		}
	}
	return out
}

func tsOf(cs []Candle) []int64 {
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.Ts
	}
	return out
}

// ---------------------------------------------------------------- 测试

const t0 = int64(1767225600000) // 2026-01-01T00:00:00Z，正好落在 1m / 1H / 1Dutc 的网格上

func TestSyncBasic(t *testing.T) {
	src := &fakeSource{have: series(t0, msMinute, 1000)}
	st := newMemStore()
	// now 定在第 500 根之后半分钟，确保 To 不会被 safeTo 截到。
	sy := NewSyncer(src, st, WithChunkCandles(30), WithClock(func() int64 { return t0 + 500*msMinute + 30000 }))

	rep, err := sy.Sync(context.Background(), SyncRequest{
		InstID: "BTC-USDT-SWAP", Bar: "1m", From: t0, To: t0 + 100*msMinute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 100 {
		t.Errorf("Added = %d，期望 100", rep.Added)
	}
	if rep.Fetches != 4 { // 30 + 30 + 30 + 10
		t.Errorf("Requests = %d，期望 4（chunk=30 切 100 根）", rep.Fetches)
	}
	if rep.Misaligned != 0 {
		t.Errorf("Misaligned = %d，应为 0", rep.Misaligned)
	}
	if want := rs(t0, t0+100*msMinute); !reflect.DeepEqual(rep.Covered, want) {
		t.Errorf("Covered = %v，期望 %v", rep.Covered, want)
	}
	if st.merges != 0 {
		t.Errorf("首次同步应当全走追加，实际 Merge 了 %d 次", st.merges)
	}

	got, _ := st.Range("BTC-USDT-SWAP", "1m", 0, 0)
	if !reflect.DeepEqual(tsOf(got), tsOf(src.have[:100])) {
		t.Errorf("落库的时间戳与源数据不一致")
	}
}

// TestSyncSkipsCovered 是 coverage 存在的全部理由：已经确认过的区间不再重拉。
func TestSyncSkipsCovered(t *testing.T) {
	src := &fakeSource{have: series(t0, msMinute, 1000)}
	st := newMemStore()
	sy := NewSyncer(src, st, WithChunkCandles(50), WithClock(func() int64 { return t0 + 500*msMinute }))
	req := SyncRequest{InstID: "X", Bar: "1m", From: t0, To: t0 + 100*msMinute}

	if _, err := sy.Sync(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	first := len(src.calls)

	rep, err := sy.Sync(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetches != 0 {
		t.Errorf("第二次同步发了 %d 次请求，应当一次都不发", rep.Fetches)
	}
	if len(src.calls) != first {
		t.Errorf("第二次同步仍在请求：%v", src.calls[first:])
	}
	if rep.Added != 0 {
		t.Errorf("第二次同步 Added = %d，应为 0", rep.Added)
	}
}

// TestSyncGapIsRemembered 是 coverage 与数据分开记的意义所在：
// 一段区间【确实没有】K 线，和【还没拉过】，从数据上看长得一模一样。
// 不把「请求过」单独记下来，同步就会在每个真实空洞上无限重拉。
func TestSyncGapIsRemembered(t *testing.T) {
	// 只有前 20 根和后 20 根有数据，中间 60 根是真空洞。
	have := append(series(t0, msMinute, 20), series(t0+80*msMinute, msMinute, 20)...)
	src := &fakeSource{have: have}
	st := newMemStore()
	sy := NewSyncer(src, st, WithChunkCandles(20), WithClock(func() int64 { return t0 + 500*msMinute }))
	req := SyncRequest{InstID: "X", Bar: "1m", From: t0, To: t0 + 100*msMinute}

	rep, err := sy.Sync(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 40 {
		t.Errorf("Added = %d，期望 40", rep.Added)
	}
	if want := rs(t0+20*msMinute, t0+80*msMinute); !reflect.DeepEqual(rep.Gaps, want) {
		t.Errorf("Gaps = %v，期望 %v", rep.Gaps, want)
	}
	// 空洞也算「已确认」，所以整段都被覆盖了。
	if want := rs(t0, t0+100*msMinute); !reflect.DeepEqual(rep.Covered, want) {
		t.Errorf("Covered = %v，期望 %v", rep.Covered, want)
	}

	rep2, err := sy.Sync(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Fetches != 0 {
		t.Errorf("空洞被重拉了 %d 次；coverage 没起作用", rep2.Fetches)
	}
}

// TestSyncNeverCoversUnclosedBar 锁住另一条要命的规则：当前这根 K 线还在走，
// 收盘价尚不可知。若把它算进覆盖，等它收盘后就再也不会去补，那根 K 线会永久
// 停在一个半截的值上——而且悄无声息。
func TestSyncNeverCoversUnclosedBar(t *testing.T) {
	src := &fakeSource{have: series(t0, msHour, 100)}
	st := newMemStore()
	// now 落在第 10 根 1H K 线走到一半的时候。
	now := t0 + 10*msHour + 37*msMinute
	sy := NewSyncer(src, st, WithChunkCandles(100), WithClock(func() int64 { return now }))

	// To 给到远处的未来，也必须被截到「当前这根的开盘时刻」。
	rep, err := sy.Sync(context.Background(), SyncRequest{
		InstID: "X", Bar: "1H", From: t0, To: t0 + 50*msHour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Range{From: t0, To: t0 + 10*msHour}); rep.Range != want {
		t.Errorf("处理区间 = %v，期望 %v（第 10 根还在走）", rep.Range, want)
	}
	if rep.Added != 10 {
		t.Errorf("Added = %d，期望 10", rep.Added)
	}
	for _, r := range rep.Covered {
		if r.To > t0+10*msHour {
			t.Errorf("覆盖区间 %v 越过了未收盘的那根", r)
		}
	}

	// 时间推进到那根收盘之后，它才该被补上。
	sy2 := NewSyncer(src, st, WithClock(func() int64 { return t0 + 12*msHour }))
	rep2, err := sy2.Sync(context.Background(), SyncRequest{InstID: "X", Bar: "1H", From: t0})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Added != 2 {
		t.Errorf("推进两小时后 Added = %d，期望 2", rep2.Added)
	}
}

// TestSyncBackfillUsesMerge 验证路由：比已有数据更早的缺口只能走 Merge。
// memStore 的 Append 会拒绝乱序写入，所以路由错了这里会直接失败。
func TestSyncBackfillUsesMerge(t *testing.T) {
	src := &fakeSource{have: series(t0, msMinute, 1000)}
	st := newMemStore()
	clock := func() int64 { return t0 + 500*msMinute }

	// 先同步后半段。
	sy := NewSyncer(src, st, WithChunkCandles(25), WithClock(clock))
	if _, err := sy.Sync(context.Background(), SyncRequest{
		InstID: "X", Bar: "1m", From: t0 + 50*msMinute, To: t0 + 100*msMinute,
	}); err != nil {
		t.Fatal(err)
	}
	if st.merges != 0 {
		t.Fatalf("首段同步不该走 Merge")
	}

	// 再补前半段——它整体早于已有数据。
	rep, err := sy.Sync(context.Background(), SyncRequest{
		InstID: "X", Bar: "1m", From: t0, To: t0 + 100*msMinute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.merges != 1 {
		t.Errorf("回填应当【一次】Merge 写完，实际 %d 次", st.merges)
	}
	if rep.Added != 50 {
		t.Errorf("Added = %d，期望 50", rep.Added)
	}

	got, _ := st.Range("X", "1m", 0, 0)
	if !reflect.DeepEqual(tsOf(got), tsOf(src.have[:100])) {
		t.Errorf("回填后顺序或内容不对")
	}
}

// TestSyncBackfillRespectsBudget 保证回填不会不声不响地吃掉一大块内存。
func TestSyncBackfillRespectsBudget(t *testing.T) {
	src := &fakeSource{have: series(t0, msMinute, 1000)}
	st := newMemStore()
	clock := func() int64 { return t0 + 900*msMinute }
	sy := NewSyncer(src, st, WithChunkCandles(50), WithMaxMergeCandles(60), WithClock(clock))

	if _, err := sy.Sync(context.Background(), SyncRequest{
		InstID: "X", Bar: "1m", From: t0 + 500*msMinute, To: t0 + 600*msMinute,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := sy.Sync(context.Background(), SyncRequest{
		InstID: "X", Bar: "1m", From: t0, To: t0 + 600*msMinute,
	})
	if err == nil {
		t.Fatal("回填超出上限应当报错，而不是默默吃内存")
	}
	if !contains(err.Error(), "WithMaxMergeCandles") {
		t.Errorf("错误信息应当告诉使用者怎么办，实为：%v", err)
	}
}

func TestSyncMisalignedIsReported(t *testing.T) {
	// 造一批偏了 30 秒的 1m K 线——真实数据里这意味着周期锚点推错了。
	bad := series(t0+30000, msMinute, 10)
	src := &fakeSource{have: bad}
	st := newMemStore()
	sy := NewSyncer(src, st, WithClock(func() int64 { return t0 + 500*msMinute }))

	rep, err := sy.Sync(context.Background(), SyncRequest{
		InstID: "X", Bar: "1m", From: t0, To: t0 + 10*msMinute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Misaligned != 10 {
		t.Errorf("Misaligned = %d，期望 10", rep.Misaligned)
	}
}

func TestSyncValidation(t *testing.T) {
	sy := NewSyncer(&fakeSource{}, newMemStore())
	ctx := context.Background()

	if _, err := sy.Sync(ctx, SyncRequest{Bar: "1m", From: t0}); err == nil {
		t.Error("instId 为空应当报错")
	}
	if _, err := sy.Sync(ctx, SyncRequest{InstID: "X", Bar: "1m"}); err == nil {
		t.Error("From 缺失应当报错")
	}
	if _, err := sy.Sync(ctx, SyncRequest{InstID: "X", Bar: "7分钟", From: t0}); err == nil {
		t.Error("未知周期应当报错")
	}
}

func TestSyncPropagatesFetchError(t *testing.T) {
	boom := errors.New("网络炸了")
	src := &fakeSource{fail: boom}
	sy := NewSyncer(src, newMemStore(), WithClock(func() int64 { return t0 + 500*msMinute }))

	_, err := sy.Sync(context.Background(), SyncRequest{
		InstID: "X", Bar: "1m", From: t0, To: t0 + 10*msMinute,
	})
	if !errors.Is(err, boom) {
		t.Fatalf("拉取失败应当原样透出，实为 %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestSyncForceRefetches 补上 coverage 唯一的救济手段。
//
// coverage 只追加，这在正常情况下是对的。但它有一个【会静默固化的失败模式】：
// 交易所一次抖动返回空，那一段就被记成「确认无数据」，此后每次同步都跳过，
// 而下游的回测在一个不该存在的空洞上照跑不误——本库不报错，错的是下游的结论。
// 没有 Force 的话，唯一的补救是手删 .meta，连带丢掉整条序列的覆盖记录。
func TestSyncForceRefetches(t *testing.T) {
	full := series(t0, msMinute, 100)
	src := &fakeSource{} // 先装作交易所抽风，什么都不返回
	st := newMemStore()
	clock := func() int64 { return t0 + 500*msMinute }
	sy := NewSyncer(src, st, WithChunkCandles(50), WithClock(clock))
	req := SyncRequest{InstID: "X", Bar: "1m", From: t0, To: t0 + 100*msMinute}

	// 第一次：拉到空，整段被记成「确认无数据」。
	rep, err := sy.Sync(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 0 || len(rep.Gaps) == 0 {
		t.Fatalf("应当一根没有且报出空洞，实为 Added=%d Gaps=%v", rep.Added, rep.Gaps)
	}

	// 交易所恢复了。但常规同步【不会】再去看一眼——这正是那个静默固化。
	src.have = full
	rep, err = sy.Sync(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetches != 0 || rep.Added != 0 {
		t.Fatalf("常规同步不该重拉已覆盖的区间，实为 Fetches=%d Added=%d",
			rep.Fetches, rep.Added)
	}

	// Force 才能救回来。
	rep, err = sy.Sync(context.Background(), req)
	_ = rep
	if err != nil {
		t.Fatal(err)
	}
	forced := req
	forced.Force = true
	rep, err = sy.Sync(context.Background(), forced)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 100 {
		t.Fatalf("Force 之后应当补齐 100 根，实为 %d", rep.Added)
	}
	got, _ := st.Range("X", "1m", 0, 0)
	if !reflect.DeepEqual(tsOf(got), tsOf(full)) {
		t.Error("Force 补回来的数据与源不一致")
	}
}

// TestSyncForcePicksUpRevisions：OKX 偶尔会修正历史 K 线，Force 能把修正值取回来。
func TestSyncForcePicksUpRevisions(t *testing.T) {
	orig := series(t0, msMinute, 50)
	src := &fakeSource{have: orig}
	st := newMemStore()
	sy := NewSyncer(src, st, WithClock(func() int64 { return t0 + 500*msMinute }))
	req := SyncRequest{InstID: "X", Bar: "1m", From: t0, To: t0 + 50*msMinute}

	if _, err := sy.Sync(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	// 交易所修正了第 20 根。
	revised := append([]Candle(nil), orig...)
	revised[20].Close = 99999
	src.have = revised

	forced := req
	forced.Force = true
	if _, err := sy.Sync(context.Background(), forced); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Range("X", "1m", 0, 0)
	if got[20].Close != 99999 {
		t.Errorf("第 20 根的收盘价是 %v，期望取回修正值 99999", got[20].Close)
	}
	if len(got) != 50 {
		t.Errorf("重拉不该改变条数，实为 %d", len(got))
	}
}
