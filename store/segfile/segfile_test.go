package segfile

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

const (
	inst = "BTC-USDT-SWAP"
	bar  = "1m"
	step = int64(60_000)
	base = int64(1767225600000) // 2026-01-01T00:00:00Z
)

func mkSeries(start int64, n int) []tickflow.Candle {
	out := make([]tickflow.Candle, n)
	for i := range out {
		f := float64(i)
		out[i] = tickflow.Candle{
			Ts:   start + int64(i)*step,
			Open: f, High: f + 1, Low: f - 1, Close: f + 0.5,
			Vol: f * 10, VolCcy: f * 100, VolCcyQuote: f * 1000,
		}
	}
	return out
}

func open(t *testing.T, root string) *Store {
	t.Helper()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustRange(t *testing.T, s *Store, from, to int64) []tickflow.Candle {
	t.Helper()
	cs, err := s.Range(inst, bar, from, to)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// TestRoundTrip 保证 64 字节定长记录的编解码是无损的——float64 的每一位都要原样回来。
func TestRoundTrip(t *testing.T) {
	s := open(t, t.TempDir())
	in := []tickflow.Candle{{
		Ts: base, Open: 78123.456789, High: 1e-9, Low: -0.0,
		Close: math.MaxFloat64, Vol: math.SmallestNonzeroFloat64,
		VolCcy: 1.0 / 3.0, VolCcyQuote: 987654321.123456789,
	}}
	if err := s.Append(inst, bar, in); err != nil {
		t.Fatal(err)
	}
	got := mustRange(t, s, 0, 0)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("往返后不一致：\n得到 %+v\n期望 %+v", got[0], in[0])
	}
}

func TestAppendAndMeta(t *testing.T) {
	s := open(t, t.TempDir())
	in := mkSeries(base, 100)
	if err := s.Append(inst, bar, in[:60]); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(inst, bar, in[60:]); err != nil {
		t.Fatal(err)
	}

	m, err := s.Meta(inst, bar)
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 100 || m.FirstTs != in[0].Ts || m.LastTs != in[99].Ts {
		t.Errorf("meta = %+v", m)
	}
	if m.Magic != Magic || m.RecordSize != RecordSize || m.Version != Version {
		t.Errorf("meta 的格式标识不对：%+v", m)
	}
	if got := mustRange(t, s, 0, 0); !reflect.DeepEqual(got, in) {
		t.Errorf("读回的数据与写入的不一致")
	}
}

func TestAppendRejectsOutOfOrder(t *testing.T) {
	s := open(t, t.TempDir())
	in := mkSeries(base, 10)

	bad := append([]tickflow.Candle(nil), in...)
	bad[3], bad[4] = bad[4], bad[3]
	if err := s.Append(inst, bar, bad); err == nil {
		t.Error("乱序的批次应当被拒绝")
	}

	if err := s.Append(inst, bar, in); err != nil {
		t.Fatal(err)
	}
	// 追加一段与已有数据重叠的，必须被拒绝并指向 Merge。
	err := s.Append(inst, bar, mkSeries(base+5*step, 10))
	if err == nil {
		t.Fatal("与已有数据重叠的追加应当被拒绝")
	}
	if !contains(err.Error(), "Merge") {
		t.Errorf("错误信息该告诉使用者改用 Merge，实为：%v", err)
	}
}

// TestRangeBoundaries 把二分的边界钉死：左闭右开，且不能漏掉或多带一根。
func TestRangeBoundaries(t *testing.T) {
	s := open(t, t.TempDir())
	in := mkSeries(base, 100)
	if err := s.Append(inst, bar, in); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		from, to int64
		want     []tickflow.Candle
	}{
		{"全部", 0, 0, in},
		{"到末尾", in[0].Ts, 0, in},
		{"精确切片", in[10].Ts, in[20].Ts, in[10:20]},
		{"上界是开区间", in[0].Ts, in[0].Ts, nil},
		{"只要一根", in[7].Ts, in[7].Ts + 1, in[7:8]},
		{"落在两根之间", in[10].Ts + 1, in[20].Ts + 1, in[11:21]},
		{"早于全部数据", 0, in[0].Ts, nil},
		{"晚于全部数据", in[99].Ts + 1, 0, nil},
		{"跨过右端", in[95].Ts, in[99].Ts + 10*step, in[95:]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mustRange(t, s, c.from, c.to)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("得到 %d 根（%v..），期望 %d 根", len(got), firstTs(got), len(c.want))
			}
		})
	}
}

// TestIterAcrossBlocks 让遍历跨过多个读取块，验证块间衔接没有漏根或重根。
func TestIterAcrossBlocks(t *testing.T) {
	s := open(t, t.TempDir())
	const n = blockRecords*3 + 7
	in := mkSeries(base, n)
	if err := s.Append(inst, bar, in); err != nil {
		t.Fatal(err)
	}

	it, err := s.Iter(inst, bar, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	var got int
	prev := int64(-1)
	for it.Next() {
		c := it.Candle()
		if c.Ts <= prev {
			t.Fatalf("第 %d 根的 ts %d 没有大于前一根 %d", got, c.Ts, prev)
		}
		prev = c.Ts
		got++
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Errorf("遍历到 %d 根，期望 %d", got, n)
	}
}

func TestMerge(t *testing.T) {
	s := open(t, t.TempDir())
	// 先写后半段，再回填前半段——这是回填的典型形态。
	later := mkSeries(base+50*step, 50)
	if err := s.Append(inst, bar, later); err != nil {
		t.Fatal(err)
	}
	earlier := mkSeries(base, 50)
	if err := s.Merge(inst, bar, earlier); err != nil {
		t.Fatal(err)
	}

	got := mustRange(t, s, 0, 0)
	if len(got) != 100 {
		t.Fatalf("合并后 %d 根，期望 100", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Ts <= got[i-1].Ts {
			t.Fatalf("合并后第 %d 根乱序", i)
		}
	}
	m, _ := s.Meta(inst, bar)
	if m.Count != 100 || m.FirstTs != base || m.LastTs != base+99*step {
		t.Errorf("合并后 meta 不对：%+v", m)
	}
}

// TestMergeOverwritesSameTs 锁住「同 ts 以新数据为准」——OKX 偶尔会修正历史 K 线。
func TestMergeOverwritesSameTs(t *testing.T) {
	s := open(t, t.TempDir())
	if err := s.Append(inst, bar, mkSeries(base, 10)); err != nil {
		t.Fatal(err)
	}
	fixed := tickflow.Candle{Ts: base + 3*step, Open: 111, High: 222, Low: 333, Close: 444}
	if err := s.Merge(inst, bar, []tickflow.Candle{fixed}); err != nil {
		t.Fatal(err)
	}
	got := mustRange(t, s, 0, 0)
	if len(got) != 10 {
		t.Fatalf("覆盖同 ts 不该改变条数，实得 %d", len(got))
	}
	if got[3] != fixed {
		t.Errorf("第 3 根 = %+v，期望 %+v", got[3], fixed)
	}
}

// TestIterInvalidatedByMerge：Merge 会整体重写文件，下标随之失效。
// 此时继续读会读到【另一个位置】上的数据——那是无声的错，必须报出来。
func TestIterInvalidatedByMerge(t *testing.T) {
	s := open(t, t.TempDir())
	if err := s.Append(inst, bar, mkSeries(base, blockRecords*2)); err != nil {
		t.Fatal(err)
	}
	it, err := s.Iter(inst, bar, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	for i := 0; i < 10; i++ {
		if !it.Next() {
			t.Fatal("提前结束")
		}
	}
	if err := s.Merge(inst, bar, []tickflow.Candle{{Ts: base - step}}); err != nil {
		t.Fatal(err)
	}
	// 走完当前这一块之后就会去读下一块，那时才会发现文件已被换掉。
	for it.Next() {
	}
	if it.Err() == nil {
		t.Fatal("文件被重写后继续遍历应当报错，而不是读到错位的数据")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	root := t.TempDir()
	in := mkSeries(base, 42)

	s1 := open(t, root)
	if err := s1.Append(inst, bar, in); err != nil {
		t.Fatal(err)
	}
	if err := s1.AddCoverage(inst, bar, tickflow.Range{From: base, To: base + 42*step}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open(t, root)
	m, err := s2.Meta(inst, bar)
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 42 {
		t.Errorf("重开后 Count = %d，期望 42", m.Count)
	}
	want := tickflow.Ranges{{From: base, To: base + 42*step}}
	if !reflect.DeepEqual(m.Coverage, want) {
		t.Errorf("重开后 Coverage = %v，期望 %v", m.Coverage, want)
	}
	if got := mustRange(t, s2, 0, 0); !reflect.DeepEqual(got, in) {
		t.Error("重开后数据对不上")
	}
}

// TestReconcileTruncatesCrashResidue 模拟「数据写进去了、meta 还没更新就崩了」。
// 落盘顺序是先数据后 meta，所以多出来的那截没有被 coverage 认领，截掉重拉即可。
func TestReconcileTruncatesCrashResidue(t *testing.T) {
	root := t.TempDir()
	s1 := open(t, root)
	if err := s1.Append(inst, bar, mkSeries(base, 10)); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	dat := filepath.Join(root, string(Candles), inst, bar+".dat")
	f, err := os.OpenFile(dat, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// 半条残缺记录 + 一条完整的，都属于 meta 不认的部分。
	if _, err := f.Write(make([]byte, RecordSize+RecordSize/2)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2 := open(t, root)
	m, err := s2.Meta(inst, bar)
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 10 {
		t.Errorf("Count = %d，期望 10", m.Count)
	}
	if got := mustRange(t, s2, 0, 0); len(got) != 10 {
		t.Errorf("读回 %d 根，期望 10——残留没被截掉", len(got))
	}
	st, _ := os.Stat(dat)
	if st.Size() != 10*RecordSize {
		t.Errorf("文件大小 %d，期望 %d", st.Size(), 10*RecordSize)
	}
}

// TestReconcileRefusesToHideDataLoss：meta 比数据【多】意味着真丢了数据。
// 这一侧不做静默修复——那会把一个本不该发生的状态掩盖过去。
func TestReconcileRefusesToHideDataLoss(t *testing.T) {
	root := t.TempDir()
	s1 := open(t, root)
	if err := s1.Append(inst, bar, mkSeries(base, 10)); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	dat := filepath.Join(root, string(Candles), inst, bar+".dat")
	if err := os.Truncate(dat, 5*RecordSize); err != nil {
		t.Fatal(err)
	}

	s2 := open(t, root)
	if _, err := s2.Meta(inst, bar); err == nil {
		t.Fatal("数据缺失时应当报错，而不是当作只有 5 根继续跑")
	}
}

func TestErrNoSeries(t *testing.T) {
	s := open(t, t.TempDir())
	if _, err := s.Meta(inst, bar); !errors.Is(err, tickflow.ErrNoSeries) {
		t.Fatalf("未存过的序列应当返回 ErrNoSeries，实为 %v", err)
	}
	if _, err := s.Iter(inst, bar, 0, 0); !errors.Is(err, tickflow.ErrNoSeries) {
		t.Fatalf("Iter 也该返回 ErrNoSeries，实为 %v", err)
	}
}

// TestRejectsUnsafeNames 挡住把 instId 直接当路径分量用带来的目录穿越。
func TestRejectsUnsafeNames(t *testing.T) {
	s := open(t, t.TempDir())
	for _, name := range []string{"", "..", ".", "../etc", `a\b`, "a/b", "a:b", "a*b"} {
		if err := s.Append(name, bar, mkSeries(base, 1)); err == nil {
			t.Errorf("instId %q 应当被拒绝", name)
		}
		if err := s.Append(inst, name, mkSeries(base, 1)); err == nil {
			t.Errorf("bar %q 应当被拒绝", name)
		}
	}
}

func TestSeries(t *testing.T) {
	s := open(t, t.TempDir())
	for _, sid := range []tickflow.SeriesID{
		{InstID: "ETH-USDT-SWAP", Bar: "1H"},
		{InstID: inst, Bar: "1m"},
		{InstID: inst, Bar: "1Dutc"},
	} {
		if err := s.Append(sid.InstID, sid.Bar, mkSeries(base, 1)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Series()
	if err != nil {
		t.Fatal(err)
	}
	want := []tickflow.SeriesID{
		{InstID: inst, Bar: "1Dutc"},
		{InstID: inst, Bar: "1m"},
		{InstID: "ETH-USDT-SWAP", Bar: "1H"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Series = %v，期望 %v", got, want)
	}
}

func firstTs(cs []tickflow.Candle) int64 {
	if len(cs) == 0 {
		return -1
	}
	return cs[0].Ts
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
