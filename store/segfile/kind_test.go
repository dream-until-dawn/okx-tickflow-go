package segfile

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestKindsDoNotCollide 是加命名空间的全部理由：
// 标记价的 BTC-USDT-SWAP/1m 与普通 K 线的 BTC-USDT-SWAP/1m 是【两条不同的序列】，
// 同一个 (instId, bar) 键。不分开的话后写的会把先写的整个覆盖掉。
func TestKindsDoNotCollide(t *testing.T) {
	root := t.TempDir()

	write := func(k SeriesKind, n int, closePx float64) {
		t.Helper()
		s, err := Open(root, k)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		cs := mkSeries(base, n)
		for i := range cs {
			cs[i].Close = closePx
		}
		if err := s.Append(inst, bar, cs); err != nil {
			t.Fatal(err)
		}
	}
	write(Candles, 10, 100)
	write(Mark, 20, 200)
	write(Index, 30, 300)

	for _, c := range []struct {
		kind  SeriesKind
		count int
		px    float64
	}{{Candles, 10, 100}, {Mark, 20, 200}, {Index, 30, 300}} {
		s, err := OpenReadOnly(root, c.kind)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Kind(); got != c.kind {
			t.Errorf("Kind() = %q，期望 %q", got, c.kind)
		}
		cs, err := s.Range(inst, bar, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) != c.count || cs[0].Close != c.px {
			t.Errorf("%s 读到 %d 根、收盘 %v，期望 %d 根、%v",
				c.kind, len(cs), cs[0].Close, c.count, c.px)
		}
		// 文件确实落在各自的命名空间目录下。
		dat, _, _ := s.Path(inst, bar)
		if filepath.Base(filepath.Dir(filepath.Dir(dat))) != string(c.kind) {
			t.Errorf("%s 的文件落在了 %s", c.kind, dat)
		}
		s.Close()
	}
}

// TestLocksArePerKind：写锁在命名空间之内，不然同一进程里开一个普通 Store
// 加一个标记价 Store 就会自己把自己锁住——而它们写的本就是两堆互不相干的文件。
func TestLocksArePerKind(t *testing.T) {
	root := t.TempDir()

	c, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	m, err := Open(root, Mark)
	if err != nil {
		t.Fatalf("普通 Store 持锁时，标记价 Store 应当也能开：%v", err)
	}
	defer m.Close()

	// 但同一个命名空间仍然互斥。
	if _, err := Open(root, Mark); !errors.Is(err, ErrLocked) {
		t.Errorf("同一命名空间的第二个写者应当拿到 ErrLocked，实为 %v", err)
	}
	for _, k := range []SeriesKind{Candles, Mark} {
		if _, err := os.Stat(filepath.Join(root, string(k), lockFile)); err != nil {
			t.Errorf("%s 的锁文件不在命名空间目录下: %v", k, err)
		}
	}
}

// TestLegacyRootLockBlocksCandles 挡住一个很窄但真实的回归窗口：
// v1.1 及更早的写者持有 <root>/.lock，写的却是 candles/ 下的文件。
// 新版把锁挪进了命名空间，若不管旧锁，两个写者会同时写同一堆文件。
func TestLegacyRootLockBlocksCandles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, string(Candles)), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, lockFile)
	if err := os.WriteFile(legacy, []byte(`{"pid":4242,"host":"old","since":"2026-08-31T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Open(root)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("旧版根锁应当拦住普通 K 线的写者，实为 %v", err)
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Errorf("错误信息该指认旧锁的持有者，实为：%v", err)
	}

	// 标记价是新命名空间，旧写者不可能在写它，不该被拦。
	m, err := Open(root, Mark)
	if err != nil {
		t.Fatalf("旧锁不该拦住标记价命名空间：%v", err)
	}
	m.Close()

	// ForceUnlock 要能把旧锁一并清掉。
	if err := ForceUnlock(root); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatalf("清掉旧锁之后应当能打开：%v", err)
	}
	s.Close()
}

// TestSeriesListingIsPerKind：Series() 只列本命名空间里的。
func TestSeriesListingIsPerKind(t *testing.T) {
	root := t.TempDir()
	c, _ := Open(root)
	m, _ := Open(root, Mark)
	defer c.Close()
	defer m.Close()

	if err := c.Append("A", "1m", mkSeries(base, 1)); err != nil {
		t.Fatal(err)
	}
	if err := m.Append("B", "5m", mkSeries(base, 1)); err != nil {
		t.Fatal(err)
	}

	got, err := c.Series()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InstID != "A" {
		t.Errorf("普通命名空间列出 %v，期望只有 A/1m", got)
	}
	got, err = m.Series()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InstID != "B" {
		t.Errorf("标记价命名空间列出 %v，期望只有 B/5m", got)
	}
}

// TestKnownKindsNotMistakenForLegacy：新加的命名空间目录不能被旧布局检测误判。
func TestKnownKindsNotMistakenForLegacy(t *testing.T) {
	root := t.TempDir()
	for _, k := range []SeriesKind{Candles, Mark, Index} {
		s, err := Open(root, k)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if err := s.Append(inst, bar, mkSeries(base, 2)); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	// 三个命名空间都建好之后，重新打开任意一个都不该报「旧布局」。
	for _, k := range []SeriesKind{Candles, Mark, Index} {
		s, err := Open(root, k)
		if err != nil {
			t.Fatalf("%s 重开失败：%v", k, err)
		}
		s.Close()
	}
	names, _ := os.ReadDir(root)
	var dirs []string
	for _, d := range names {
		if d.IsDir() {
			dirs = append(dirs, d.Name())
		}
	}
	want := []string{"candles", "index-candles", "mark-candles"}
	if !reflect.DeepEqual(dirs, want) {
		t.Errorf("数据目录下是 %v，期望 %v", dirs, want)
	}
}
