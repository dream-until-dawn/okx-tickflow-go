package tickflow

import (
	"encoding/json"
	"reflect"
	"testing"
)

func rs(pairs ...int64) Ranges {
	var out Ranges
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Range{From: pairs[i], To: pairs[i+1]})
	}
	return out
}

func TestRangesAdd(t *testing.T) {
	cases := []struct {
		name string
		in   Ranges
		add  Range
		want Ranges
	}{
		{"空集", nil, Range{10, 20}, rs(10, 20)},
		{"不相交", rs(0, 10), Range{20, 30}, rs(0, 10, 20, 30)},
		// 相邻必须并掉，否则每同步一块就多攒一条区间，meta 会无限膨胀，
		// 且 Missing 里会冒出一堆零宽的缝。
		{"相邻并掉", rs(0, 10), Range{10, 20}, rs(0, 20)},
		{"重叠", rs(0, 15), Range{10, 20}, rs(0, 20)},
		{"包含", rs(0, 30), Range{10, 20}, rs(0, 30)},
		{"被包含", rs(10, 20), Range{0, 30}, rs(0, 30)},
		{"插在中间并把两侧连起来", rs(0, 10, 20, 30), Range{10, 20}, rs(0, 30)},
		{"乱序插入", rs(20, 30), Range{0, 10}, rs(0, 10, 20, 30)},
		{"空区间忽略", rs(0, 10), Range{5, 5}, rs(0, 10)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.Add(c.add); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Add(%v) = %v，期望 %v", c.add, got, c.want)
			}
		})
	}
}

// TestRangesAddDoesNotAlias 保证 Add 不改到入参——Store 内部持有 Coverage，
// 调用方拿到的副本被改会污染存储层的状态。
func TestRangesAddDoesNotAlias(t *testing.T) {
	orig := rs(0, 10)
	got := orig.Add(Range{5, 20})
	if !reflect.DeepEqual(orig, rs(0, 10)) {
		t.Fatalf("Add 改到了入参：%v", orig)
	}
	if !reflect.DeepEqual(got, rs(0, 20)) {
		t.Fatalf("Add 结果不对：%v", got)
	}
}

func TestRangesMissing(t *testing.T) {
	cases := []struct {
		name string
		cov  Ranges
		want Range
		out  Ranges
	}{
		{"全空", nil, Range{0, 100}, rs(0, 100)},
		{"全覆盖", rs(0, 100), Range{10, 90}, nil},
		{"恰好覆盖", rs(10, 90), Range{10, 90}, nil},
		{"缺中间", rs(0, 10, 20, 30), Range{5, 25}, rs(10, 20)},
		{"缺头", rs(20, 30), Range{0, 30}, rs(0, 20)},
		{"缺尾", rs(0, 10), Range{0, 30}, rs(10, 30)},
		{"缺两头", rs(10, 20), Range{0, 30}, rs(0, 10, 20, 30)},
		{"多段缺口", rs(0, 10, 20, 30, 40, 50), Range{0, 60}, rs(10, 20, 30, 40, 50, 60)},
		{"覆盖在请求之外", rs(100, 200), Range{0, 50}, rs(0, 50)},
		{"空请求", rs(0, 10), Range{5, 5}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cov.Missing(c.want); !reflect.DeepEqual(got, c.out) {
				t.Errorf("Missing(%v) = %v，期望 %v", c.want, got, c.out)
			}
		})
	}
}

func TestRangesCovers(t *testing.T) {
	cov := rs(0, 10, 20, 30)
	if !cov.Covers(Range{0, 10}) {
		t.Error("应当已覆盖 [0,10)")
	}
	if cov.Covers(Range{5, 25}) {
		t.Error("[5,25) 中间有缺口，不该算已覆盖")
	}
}

// TestRangeJSON 锁住 meta 里的区间形态：二元数组而不是对象。
// 改了它就读不了旧的 meta 文件。
func TestRangeJSON(t *testing.T) {
	in := rs(1609459200000, 1767225600000, 10, 20)
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[[1609459200000,1767225600000],[10,20]]`; string(raw) != want {
		t.Fatalf("序列化为 %s，期望 %s", raw, want)
	}
	var back Ranges
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, in) {
		t.Fatalf("往返后为 %v，期望 %v", back, in)
	}
}
