// Package okxsource 用 okx-api-v5-go 实现 tickflow.Source。
//
// 它把上游那几个粗糙面收在这里，不让它们渗到存储与回测层：
//
//   - OKX 返回【倒序】（第 0 条最新），本包翻正为升序；
//   - 分页语义与直觉相反——After 是「更旧」、Before 是「更新」，本包只对外暴露
//     一个左闭右开的 [From, To)；
//   - 单次条数有上限，本包自动翻页直到覆盖整个请求区间；
//   - 近端与远端是两个不同的接口（/market/candles 与 /market/history-candles），
//     本包按游标位置自动路由；
//   - /market/candles 会带回一根【未完结】的当前 K 线，本包丢弃它。
package okxsource

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	okx "github.com/dream-until-dawn/okx-api-v5-go"
	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

const (
	// OKX 对两个接口的单次条数上限不同。
	candlesLimit = 300
	historyLimit = 100
)

// Source 从 OKX 拉 K 线。
// Series 选择拉哪一条价格序列。
//
// 三条序列在 OKX 是【各自独立】的端点，形态一样（同样的 ts + OHLC），
// 但标记价与指数价【没有成交量】，那三个字段恒为 0。
type Series int

const (
	// Trades 是普通成交价 K 线，New 不指定时的默认。
	Trades Series = iota

	// MarkPrice 是标记价 K 线。
	//
	// OKX 的强平按标记价判定而不是成交价。回测里要建模爆仓就必须用这条——
	// 用成交价会让影线制造出真实不会发生的强平。
	//
	// ⚠️ 标记价历史有一条硬线，而且【随周期分两档】：日线档港时 2020-01-01，
	// 日内档（1H/15m/1m）2020-01-03。早于它的区间会出现在 SyncReport.Gaps 里，
	// 那不是同步没做好。
	//
	// 成交价的边界同样随周期变（BTC 的 1D 是 2019-11-28、1m 是 2019-12-16），
	// 所以任何不带 bar 前提的边界表述都是错的。见 TestLiveHistoryFloorVariesByBar。
	MarkPrice

	// IndexPrice 是指数价 K 线。instId 用【现货形式】，如 "ETH-USDT"
	// 而不是 "ETH-USDT-SWAP"。
	IndexPrice
)

func (s Series) apply(src *Source) { src.series = s }

func (s Series) String() string {
	switch s {
	case MarkPrice:
		return "mark-price"
	case IndexPrice:
		return "index"
	default:
		return "trades"
	}
}

type Source struct {
	c      *okx.Client
	series Series

	recentBars  int
	minInterval time.Duration
	maxPages    int
	forceEP     endpoint

	mu   sync.Mutex
	last time.Time

	now func() time.Time
}

type endpoint int

const (
	epAuto endpoint = iota
	epCandles
	epHistory
)

// Option 配置 Source。Series 本身就是一个 Option：
//
//	okxsource.New(client)                        // 普通 K 线
//	okxsource.New(client, okxsource.MarkPrice)   // 标记价
//	okxsource.New(client, okxsource.IndexPrice)  // 指数价
type Option interface{ apply(*Source) }

type optFunc func(*Source)

func (f optFunc) apply(s *Source) { f(s) }

// WithRecentBars 设置「近端」的宽度，默认 1000 根。
//
// 游标落在最近这么多根之内时走 /market/candles，否则走 /market/history-candles。
// OKX 文档称前者只覆盖最近约 1440 根，这里留了余量。
func WithRecentBars(n int) Option {
	return optFunc(func(s *Source) {
		if n > 0 {
			s.recentBars = n
		}
	})
}

// WithMinInterval 设置两次请求之间的最小间隔，默认 120ms。
//
// 这是个保守的节流，用于在没有给 okx.Client 注入限流器时也不至于打爆接口。
// OKX 各接口的限速档位不同且会调整，以官方文档为准；真要精确控制，
// 请用 okx.WithLimiter 给客户端注入限流器，并把这里设为 0。
func WithMinInterval(d time.Duration) Option {
	return optFunc(func(s *Source) {
		if d >= 0 {
			s.minInterval = d
		}
	})
}

// WithMaxPages 设置单次 Fetch 的最大翻页数，默认 10000。这是个防跑飞的兜底。
func WithMaxPages(n int) Option {
	return optFunc(func(s *Source) {
		if n > 0 {
			s.maxPages = n
		}
	})
}

// ForceCandles 强制只用近端接口，不走 history 变体。
func ForceCandles() Option { return optFunc(func(s *Source) { s.forceEP = epCandles }) }

// ForceHistoryCandles 强制只用 history 变体。
func ForceHistoryCandles() Option { return optFunc(func(s *Source) { s.forceEP = epHistory }) }

// New 构造一个 Source。
func New(c *okx.Client, opts ...Option) (*Source, error) {
	if c == nil {
		return nil, errors.New("okxsource: client 不能为 nil")
	}
	s := &Source{
		c:           c,
		recentBars:  1000,
		minInterval: 120 * time.Millisecond,
		maxPages:    10000,
		now:         time.Now,
	}
	for _, o := range opts {
		o.apply(s)
	}
	return s, nil
}

// Series 返回本 Source 拉的是哪一条价格序列。
func (s *Source) Series() Series { return s.series }

var _ tickflow.Source = (*Source)(nil)

// Fetch 拉取 [From, To) 内已完结的 K 线，按 ts 升序返回。
//
// 整个区间缓冲在内存里，所以调用方要控制单次区间的大小；tickflow.Syncer
// 已经按 chunk 分好了。
func (s *Source) Fetch(ctx context.Context, req tickflow.FetchRequest) ([]tickflow.Candle, error) {
	if req.InstID == "" {
		return nil, errors.New("okxsource: instId 不能为空")
	}
	p, err := tickflow.ParsePeriod(req.Bar)
	if err != nil {
		return nil, err
	}
	if req.To <= req.From {
		return nil, nil
	}

	nowMs := s.now().UnixMilli()
	boundary := recentBoundary(p, nowMs, s.recentBars)

	// OKX 只能往【更旧】的方向翻页，所以从区间末端倒着走，最后整体翻正。
	var desc []tickflow.Candle
	cursor := req.To
	for page := 0; page < s.maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := s.page(ctx, req.InstID, req.Bar, cursor, s.pick(cursor, boundary))
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			break
		}

		oldest := cursor
		for _, c := range raw {
			if c.Ts < oldest {
				oldest = c.Ts
			}
			// 未完结的那一根绝不能进来：它的收盘价还在变，用它做决策就是未来函数。
			if !c.Confirm {
				continue
			}
			if c.Ts < req.From || c.Ts >= req.To {
				continue
			}
			desc = append(desc, tickflow.Candle{
				Ts: c.Ts, Open: c.Open, High: c.High, Low: c.Low, Close: c.Close,
				Vol: c.Vol, VolCcy: c.VolCcy, VolCcyQuote: c.VolCcyQuote,
			})
		}

		// 没往更旧的方向推进，说明再翻也是原地打转，停。
		if oldest >= cursor {
			break
		}
		cursor = oldest
		if cursor <= req.From {
			break
		}
	}

	// 翻正为升序。上游返回的是倒序，跨页收集之后整体也是倒序。
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	return desc, nil
}

func (s *Source) pick(cursor, boundary int64) endpoint {
	if s.forceEP != epAuto {
		return s.forceEP
	}
	if cursor > boundary {
		return epCandles
	}
	return epHistory
}

func (s *Source) page(ctx context.Context, instID, bar string, cursor int64, ep endpoint) ([]okx.Candle, error) {
	s.throttle()
	r := okx.CandlesRequest{InstID: instID, Bar: bar, After: cursor}
	near := ep == epCandles
	if near {
		r.Limit = candlesLimit
	} else {
		r.Limit = historyLimit
	}

	var (
		cs  []okx.Candle
		err error
	)
	switch s.series {
	case MarkPrice:
		if near {
			cs, err = s.c.Market.MarkPriceCandles(ctx, r)
		} else {
			cs, err = s.c.Market.HistoryMarkPriceCandles(ctx, r)
		}
	case IndexPrice:
		if near {
			cs, err = s.c.Market.IndexCandles(ctx, r)
		} else {
			cs, err = s.c.Market.HistoryIndexCandles(ctx, r)
		}
	default:
		if near {
			cs, err = s.c.Market.Candles(ctx, r)
		} else {
			cs, err = s.c.Market.HistoryCandles(ctx, r)
		}
	}
	if err != nil {
		kind := "history-"
		if near {
			kind = ""
		}
		return nil, fmt.Errorf("okxsource: %s%s-candles %s/%s after=%d: %w",
			kind, s.series, instID, bar, cursor, err)
	}
	return cs, nil
}

func (s *Source) throttle() {
	if s.minInterval <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if wait := s.minInterval - s.now().Sub(s.last); wait > 0 {
		time.Sleep(wait)
	}
	s.last = s.now()
}

// recentBoundary 返回「最近 n 根」的起点时间。
func recentBoundary(p tickflow.Period, now int64, n int) int64 {
	cur := p.Truncate(now)
	if p.Fixed() {
		return cur - int64(n)*p.Step().Milliseconds()
	}
	for i := 0; i < n; i++ {
		prev := p.Truncate(cur - 1)
		if prev >= cur {
			break
		}
		cur = prev
	}
	return cur
}
