package tickflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ErrNoSeries 表示该 (instID, bar) 从未存过任何数据。
var ErrNoSeries = errors.New("tickflow: 该序列不存在")

// Range 是一个左闭右开的毫秒时间区间 [From, To)。
//
// JSON 形态是二元数组 [from, to]，这样 meta 文件里一长串区间读起来还算清爽。
type Range struct {
	From int64
	To   int64
}

// Empty 报告区间是否为空。
func (r Range) Empty() bool { return r.To <= r.From }

// Contains 报告 ts 是否落在区间内。
func (r Range) Contains(ts int64) bool { return ts >= r.From && ts < r.To }

// String 返回 "[from,to)" 形式，左闭右开一目了然。
func (r Range) String() string { return fmt.Sprintf("[%d,%d)", r.From, r.To) }

// MarshalJSON 把区间写成二元数组 [from, to]。
//
// 用数组而不是对象，是因为 meta 文件里可能有一长串区间，写成对象会长得读不动。
// 这是落盘格式的一部分，改了就读不了旧的 meta 文件。
func (r Range) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]int64{r.From, r.To})
}

// UnmarshalJSON 解析 [from, to] 形式的二元数组。
func (r *Range) UnmarshalJSON(b []byte) error {
	var a [2]int64
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	r.From, r.To = a[0], a[1]
	return nil
}

// Ranges 是一组【已归一化】的区间：升序、互不重叠、互不相邻。
//
// 它承载 Meta.Coverage，也就是本库最容易被忽略、却最要命的一块状态：
// 「我请求过这一段并已确认」。没有它就无法区分【这根 K 线 OKX 根本没产出】
// 和【我还没拉过这一段】——小币种或维护期确实会缺根。靠数据本身的时间戳
// 连续性去推断，增量同步会在每一个真实空洞上反复重拉，永远收敛不了。
type Ranges []Range

// Add 并入一个区间并重新归一化。
func (rs Ranges) Add(r Range) Ranges {
	if r.Empty() {
		return rs
	}
	out := make(Ranges, 0, len(rs)+1)
	out = append(out, rs...)
	out = append(out, r)
	return out.normalize()
}

func (rs Ranges) normalize() Ranges {
	if len(rs) < 2 {
		return rs
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].From < rs[j].From })
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.From <= last.To { // 重叠或相邻，都并掉
			if r.To > last.To {
				last.To = r.To
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// Missing 返回 r 之中【尚未被覆盖】的部分。
func (rs Ranges) Missing(r Range) Ranges {
	if r.Empty() {
		return nil
	}
	var out Ranges
	cur := r.From
	for _, c := range rs {
		if c.To <= cur {
			continue
		}
		if c.From >= r.To {
			break
		}
		if c.From > cur {
			out = append(out, Range{From: cur, To: min64(c.From, r.To)})
		}
		if c.To > cur {
			cur = c.To
		}
		if cur >= r.To {
			return out
		}
	}
	if cur < r.To {
		out = append(out, Range{From: cur, To: r.To})
	}
	return out
}

// Covers 报告 r 是否已被完全覆盖。
func (rs Ranges) Covers(r Range) bool { return len(rs.Missing(r)) == 0 }

// Meta 是一条序列的元信息。
type Meta struct {
	InstID string `json:"instId"`
	Bar    string `json:"bar"`

	Magic      string `json:"magic"`      // 恒为 "TKFL"，用于识别文件族
	Version    int    `json:"version"`    // 格式版本
	RecordSize int    `json:"recordSize"` // 单条记录字节数

	Count   int64 `json:"count"`   // 记录条数
	FirstTs int64 `json:"firstTs"` // 最早一根的开盘时间；无数据时为 0
	LastTs  int64 `json:"lastTs"`  // 最晚一根的开盘时间；无数据时为 0

	// Coverage 是【已确认拉取过】的区间，与数据本身分开记。见 Ranges 的说明。
	Coverage Ranges `json:"coverage"`

	UpdatedAt int64 `json:"updatedAt"`
}

// SeriesID 标识一条序列。
type SeriesID struct {
	InstID string
	Bar    string
}

// String 返回 "instId/bar"，如 "BTC-USDT-SWAP/15m"。
func (s SeriesID) String() string { return s.InstID + "/" + s.Bar }

// Iterator 按时间升序遍历一段 K 线。
//
// Feed 走这条而不是 Range：五年的 1m 线有两百多万根，不该一次性读进内存。
type Iterator interface {
	// Next 前进一根，无更多数据或出错时返回 false。
	Next() bool
	// Candle 返回当前这根。仅在 Next 返回 true 后有效。
	Candle() Candle
	// Err 返回遍历中发生的错误。正常结束时为 nil。
	Err() error
	Close() error
}

// Store 是 K 线的持久化后端。
//
// 默认实现是 store/segfile（定长文件，零第三方依赖）。这里做成接口，是为了
// 让使用者能换成 ClickHouse 之类——本库不该替使用者决定数据放在哪。
type Store interface {
	// Append 在序列【末尾】追加。要求 cs 按 ts 严格升序，且首根晚于当前 LastTs。
	// 这是同步最新数据的快路径，代价与 len(cs) 成正比。
	Append(instID, bar string, cs []Candle) error

	// Merge 把 cs 并入序列的任意位置，ts 相同的以 cs 为准。
	//
	// 回填历史（要写的数据比已有数据更早）只能走这条，代价是一次全文件重写。
	// 调用方应把一整段回填攒成一次 Merge，而不是分块多次调用——那会是 O(n²)。
	Merge(instID, bar string, cs []Candle) error

	// Iter 返回 [from, to) 的升序游标。to 为 0 表示直到序列末尾。
	Iter(instID, bar string, from, to int64) (Iterator, error)

	// Range 是 Iter 的便捷包装，小范围查询用。大范围请用 Iter。
	Range(instID, bar string, from, to int64) ([]Candle, error)

	// Meta 返回元信息。序列不存在时返回 ErrNoSeries。
	Meta(instID, bar string) (Meta, error)

	// AddCoverage 记录「[r.From, r.To) 这一段已请求并确认」。
	//
	// 覆盖区间【不能】从数据推出来：一段区间里一根 K 线都没有，可能是 OKX
	// 本就没产出，也可能是还没拉。只有发起请求的一方知道是哪种，所以由它来记。
	AddCoverage(instID, bar string, r Range) error

	// Series 列出已存在的全部序列。
	Series() ([]SeriesID, error)

	Close() error
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Clone 深拷贝 Meta，避免调用方改到 Store 内部持有的 Coverage 切片。
func (m Meta) Clone() Meta {
	m.Coverage = append(Ranges(nil), m.Coverage...)
	return m
}
