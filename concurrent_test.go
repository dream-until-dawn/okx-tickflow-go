package tickflow

import (
	"sync"
	"testing"
	"time"
)

// 本文件为竞态检测而写。
//
// 其余测试都是单 goroutine 的，拿它们跑 -race 几乎证明不了什么——竞态检测只报
// 【实际发生过】的竞争。有并发承诺的地方，就该有真的并发起来的测试：
//
//	go test -race ./...
//
// Windows 上跑 -race 需要 cgo，也就是一套 MinGW-w64 的 gcc。
//
// Feed 明确【不是】并发安全的（一个回测循环就是一条时间线），所以这里不测它。
// 要并发跑多组参数，各建各的 Feed。

// TestConcurrentPeriodRegistry 压全局的周期表。
//
// ParsePeriod 读它、RegisterFixedPeriod 写它，两者由一把读写锁保护。
// 使用者在 main 里注册新周期、同时别的 goroutine 已经在解析，是个真实的形态。
func TestConcurrentPeriodRegistry(t *testing.T) {
	var wg sync.WaitGroup
	bars := []string{"1m", "15m", "1H", "1D", "1Dutc", "1W", "1M"}

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				if i == 0 {
					// 一个 goroutine 不停注册自定义周期。
					_ = RegisterFixedPeriod("custom7x", 7*time.Minute, time.Unix(0, 0))
					continue
				}
				p, err := ParsePeriod(bars[j%len(bars)])
				if err != nil {
					t.Error(err)
					return
				}
				// 顺带用一下，确保读出来的是个能用的值而不是半个。
				if ts := p.Truncate(1767225600000); ts <= 0 {
					t.Errorf("%s 解析出来的周期算不出对齐时刻", p.Bar())
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestConcurrentFeedsAreIndependent：各建各的 Feed 并行推进，互不干扰。
// 这正是「要并发跑多组参数就各建各的」那句话所承诺的。
func TestConcurrentFeedsAreIndependent(t *testing.T) {
	st := newMemStore()
	base := MustParsePeriod("15m")
	cs := genBars(base, MustParsePeriod("1D").Truncate(t0), 500)
	fill(t, st, "X", "15m", cs)

	var wg sync.WaitGroup
	results := make([][]int64, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f, err := NewFeed(st, FeedConfig{
				InstID: "X", Base: "15m", From: cs[0].Ts, Lookback: 3,
				Indicators: map[string][]Indicator{
					"15m": {newInd("x", 1+i)}, // 每个 Feed 挂各自的指标实例
				},
			})
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			var got []int64
			for f.Next() {
				got = append(got, f.View().Ts())
			}
			if err := f.Err(); err != nil {
				t.Error(err)
				return
			}
			results[i] = got
		}(i)
	}
	wg.Wait()

	// 各自独立推进，走出来的时间线必须完全一样。
	for i := 1; i < len(results); i++ {
		if len(results[i]) != len(results[0]) {
			t.Fatalf("第 %d 个 Feed 走了 %d 步，第 0 个走了 %d 步",
				i, len(results[i]), len(results[0]))
		}
		for k := range results[i] {
			if results[i][k] != results[0][k] {
				t.Fatalf("第 %d 个 Feed 的第 %d 步是 %d，第 0 个是 %d",
					i, k, results[i][k], results[0][k])
			}
		}
	}
	if len(results[0]) != len(cs) {
		t.Errorf("走了 %d 步，期望 %d", len(results[0]), len(cs))
	}
}
