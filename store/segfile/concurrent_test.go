package segfile

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件专门为竞态检测而写。
//
// 其余测试全是单 goroutine 的，拿它们跑 -race 几乎什么都证明不了——竞态检测
// 只报【实际发生过】的竞争，没有并发就没有可报的东西。Store 的文档承诺
// 「进程内多读单写」，那条承诺得由真的并发起来的测试来担保：
//
//	go test -race ./store/segfile/
//
// Windows 上跑 -race 需要 cgo，也就是一套 MinGW-w64 的 gcc。

// TestConcurrentReadersSingleWriter 让一个写者与若干读者同时压同一个 Store，
// 这正是文档承诺的那个形态。
//
// 除了竞态检测本身，还要求读到的每一根 K 线都是【自洽】的：撕裂的读会读到
// 半根旧半根新的记录，那种数据在竞态检测里未必报错，却会让回测悄悄算错。
func TestConcurrentReadersSingleWriter(t *testing.T) {
	s := open(t, t.TempDir())
	if err := s.Append(inst, bar, mkSeries(base, 200)); err != nil {
		t.Fatal(err)
	}

	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		reads    atomic.Int64
		appended atomic.Int64
		failures = make(chan string, 16)
	)

	// 一个写者，持续往末尾追加。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 120 && !stop.Load(); i++ {
			n := int64(200 + i*5)
			if err := s.Append(inst, bar, mkSeries(base+n*step, 5)); err != nil {
				select {
				case failures <- "写入失败: " + err.Error():
				default:
				}
				return
			}
			appended.Add(5)
		}
	}()

	// 若干读者，一边全量读一边查 meta。
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				m, err := s.Meta(inst, bar)
				if err != nil {
					select {
					case failures <- "读 meta 失败: " + err.Error():
					default:
					}
					return
				}
				cs, err := s.Range(inst, bar, 0, 0)
				if err != nil {
					select {
					case failures <- "读区间失败: " + err.Error():
					default:
					}
					return
				}
				reads.Add(1)

				// 读到的必须是自洽的：升序、无重复、条数不少于读 meta 那一刻的
				// 数量（追加只会让它变多），且每根记录的字段没有撕裂。
				if int64(len(cs)) < m.Count {
					select {
					case failures <- "读到的条数少于 meta 声称的":
					default:
					}
					return
				}
				for i, c := range cs {
					if i > 0 && c.Ts <= cs[i-1].Ts {
						select {
						case failures <- "读到的数据乱序或重复":
						default:
						}
						return
					}
					// mkSeries 造的数据满足 High == Open+1、Low == Open-1。
					// 撕裂的读会打破这个关系。
					if c.High != c.Open+1 || c.Low != c.Open-1 {
						select {
						case failures <- "读到了撕裂的记录":
						default:
						}
						return
					}
				}
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	close(failures)

	for msg := range failures {
		t.Error(msg)
	}
	if reads.Load() < 10 || appended.Load() == 0 {
		t.Fatalf("并发压得不够（读 %d 次、追加 %d 根），这个测试什么都没验到",
			reads.Load(), appended.Load())
	}
	t.Logf("并发读 %d 次、追加 %d 根", reads.Load(), appended.Load())
}

// TestConcurrentIterators 让多个游标同时遍历同一条序列。
//
// 游标各持各的读取缓冲，但共用底下那个文件句柄与 generation 计数，
// 是最容易写出共享状态的地方。
func TestConcurrentIterators(t *testing.T) {
	s := open(t, t.TempDir())
	const n = blockRecords*2 + 37
	in := mkSeries(base, n)
	if err := s.Append(inst, bar, in); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// 各自从不同的位置起步，让二分与块边界都错开。
			from := in[g*13].Ts
			it, err := s.Iter(inst, bar, from, 0)
			if err != nil {
				errs <- err.Error()
				return
			}
			defer it.Close()
			want := n - g*13
			got, prev := 0, int64(-1)
			for it.Next() {
				c := it.Candle()
				if c.Ts <= prev {
					errs <- "游标读出乱序"
					return
				}
				prev = c.Ts
				got++
			}
			if it.Err() != nil {
				errs <- it.Err().Error()
				return
			}
			if got != want {
				errs <- "遍历到的根数不对"
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestConcurrentSeriesOpen 让多个 goroutine 同时第一次触碰不同的序列，
// 压 Store 那张 series map 的懒加载。
func TestConcurrentSeriesOpen(t *testing.T) {
	s := open(t, t.TempDir())
	bars := []string{"1m", "5m", "15m", "1H", "4H", "1D", "1Dutc", "1W"}

	var wg sync.WaitGroup
	errs := make(chan string, len(bars)*4)
	for round := 0; round < 4; round++ {
		for _, b := range bars {
			wg.Add(1)
			go func(b string) {
				defer wg.Done()
				if err := s.Append(inst, b, mkSeries(base, 1)); err != nil {
					// 同一序列被并发追加同一根，后到的会因「不晚于 LastTs」被拒，
					// 那是预期内的；其余错误才算问题。
					if !contains(err.Error(), "只能追加在末尾") {
						errs <- err.Error()
					}
					return
				}
				if _, err := s.Meta(inst, b); err != nil {
					errs <- err.Error()
				}
			}(b)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	got, err := s.Series()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(bars) {
		t.Errorf("并发建了 %d 条序列，期望 %d", len(got), len(bars))
	}
}
