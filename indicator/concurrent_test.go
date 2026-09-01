package indicator

import (
	"sync"
	"testing"
)

// 本文件为竞态检测而写，见 store/segfile/concurrent_test.go 顶部的说明。
//
//	go test -race ./indicator/

// TestConcurrentDefaultConvention 压全局默认口径。
//
// SetDefaultConvention 写一个全局变量，而每次构造指标都会读它。文档说「只在
// 程序初始化时调用一次」，但那是【约定】——约定挡不住并发，而无保护的读写就是
// 数据竞争。这个测试存在的意义就是把约定变成担保。
func TestConcurrentDefaultConvention(t *testing.T) {
	orig := DefaultConvention()
	defer SetDefaultConvention(orig)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if i%2 == 0 {
					SetDefaultConvention(Convention(j % 2))
					continue
				}
				// 构造指标要读全局默认。
				_ = MACD(12, 26, 9)
				_ = DefaultConvention()
			}
		}(i)
	}
	wg.Wait()
}

// TestConcurrentIndicatorConstruction：多个 goroutine 同时构造指标，
// 只读全局默认而不写它——这是实盘/多策略并行时的真实形态。
func TestConcurrentIndicatorConstruction(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				for _, ind := range []Indicator{
					MA(20), EMA(20), MACD(12, 26, 9),
					KDJ(9, 3, 3), RSI(14), CCI(20), BOLL(20, 2),
				} {
					if ind.Name() == "" {
						t.Error("构造出来的指标没有名字")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
