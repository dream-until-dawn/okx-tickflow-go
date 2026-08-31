package tickflow

import (
	"testing"
	"time"
)

func ms(t *testing.T, s string) int64 {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("解析 %q: %v", s, err)
	}
	return v.UnixMilli()
}

func str(v int64) string { return time.UnixMilli(v).UTC().Format(time.RFC3339) }

func TestPeriodTruncate(t *testing.T) {
	// 期望值一律写成 UTC。港时对齐的周期（6H 及以上不带 utc 后缀的），
	// 边界落在 UTC 的 16:00 / 22:00 / 04:00 / 10:00 上，那是港时的 00/06/12/18。
	cases := []struct{ bar, in, want string }{
		{"1s", "2026-01-15T08:07:33.412Z", "2026-01-15T08:07:33Z"},
		{"1m", "2026-01-15T08:07:33Z", "2026-01-15T08:07:00Z"},
		{"3m", "2026-01-15T08:07:33Z", "2026-01-15T08:06:00Z"},
		{"5m", "2026-01-15T08:07:33Z", "2026-01-15T08:05:00Z"},
		{"15m", "2026-01-15T08:07:33Z", "2026-01-15T08:00:00Z"},
		{"30m", "2026-01-15T08:37:33Z", "2026-01-15T08:30:00Z"},
		{"1H", "2026-01-15T08:07:33Z", "2026-01-15T08:00:00Z"},
		{"2H", "2026-01-15T09:07:33Z", "2026-01-15T08:00:00Z"},
		{"4H", "2026-01-15T08:07:33Z", "2026-01-15T08:00:00Z"},

		// 6H 港时：网格在 UTC 的 04 / 10 / 16 / 22。
		{"6H", "2026-01-15T08:07:33Z", "2026-01-15T04:00:00Z"},
		{"6H", "2026-01-15T03:59:59Z", "2026-01-14T22:00:00Z"},
		{"6Hutc", "2026-01-15T08:07:33Z", "2026-01-15T06:00:00Z"},

		{"12H", "2026-01-15T08:07:33Z", "2026-01-15T04:00:00Z"},
		{"12Hutc", "2026-01-15T08:07:33Z", "2026-01-15T00:00:00Z"},

		// 1D 港时：港时 2026-01-15 那天 = UTC 01-14 16:00 起。
		{"1D", "2026-01-15T08:07:33Z", "2026-01-14T16:00:00Z"},
		{"1D", "2026-01-15T15:59:59Z", "2026-01-14T16:00:00Z"},
		{"1D", "2026-01-15T16:00:00Z", "2026-01-15T16:00:00Z"},
		{"1Dutc", "2026-01-15T08:07:33Z", "2026-01-15T00:00:00Z"},

		// 周线以【周一】为界。2026-01-15 是周四，所属周从 01-12（周一）起。
		{"1Wutc", "2026-01-15T08:07:33Z", "2026-01-12T00:00:00Z"},
		{"1W", "2026-01-15T08:07:33Z", "2026-01-11T16:00:00Z"},
		{"1Wutc", "2026-01-12T00:00:00Z", "2026-01-12T00:00:00Z"},
		{"1Wutc", "2026-01-11T23:59:59Z", "2026-01-05T00:00:00Z"},

		// 月线走日历。
		{"1Mutc", "2026-01-15T08:07:33Z", "2026-01-01T00:00:00Z"},
		{"1M", "2026-01-15T08:07:33Z", "2025-12-31T16:00:00Z"},
		{"1Mutc", "2026-03-31T23:59:59Z", "2026-03-01T00:00:00Z"},

		// 季线按自然季对齐。
		{"3Mutc", "2026-01-15T08:07:33Z", "2026-01-01T00:00:00Z"},
		{"3Mutc", "2026-05-20T08:07:33Z", "2026-04-01T00:00:00Z"},
		{"3Mutc", "2026-12-31T23:59:59Z", "2026-10-01T00:00:00Z"},
	}

	for _, c := range cases {
		p, err := ParsePeriod(c.bar)
		if err != nil {
			t.Fatalf("%s: %v", c.bar, err)
		}
		got := p.Truncate(ms(t, c.in))
		if want := ms(t, c.want); got != want {
			t.Errorf("%s Truncate(%s) = %s，期望 %s", c.bar, c.in, str(got), c.want)
		}
	}
}

// TestTruncateIdempotent 对齐必须是幂等的：已对齐的时刻再 Truncate 一次不能变。
// 这条一旦不成立，Syncer 的区间对齐与 Feed 的收盘判定都会错位。
func TestTruncateIdempotent(t *testing.T) {
	start := ms(t, "2019-03-07T13:41:09Z")
	for _, bar := range []string{
		"1s", "1m", "3m", "5m", "15m", "30m", "1H", "2H", "4H",
		"6H", "6Hutc", "12H", "12Hutc", "1D", "1Dutc", "2D", "3D",
		"1W", "1Wutc", "1M", "1Mutc", "3M", "3Mutc",
	} {
		p := MustParsePeriod(bar)
		for i := int64(0); i < 400; i++ {
			ts := start + i*7*msHour + i*i*13*msMinute
			a := p.Truncate(ts)
			if b := p.Truncate(a); b != a {
				t.Fatalf("%s: Truncate 不幂等，%s -> %s -> %s", bar, str(ts), str(a), str(b))
			}
			if a > ts {
				t.Fatalf("%s: Truncate(%s) = %s，跑到了未来", bar, str(ts), str(a))
			}
			if n := p.Next(a); n <= a {
				t.Fatalf("%s: Next(%s) = %s，没有前进", bar, str(a), str(n))
			}
		}
	}
}

func TestPeriodNext(t *testing.T) {
	cases := []struct{ bar, in, want string }{
		{"1m", "2026-01-15T08:07:00Z", "2026-01-15T08:08:00Z"},
		{"1D", "2026-01-14T16:00:00Z", "2026-01-15T16:00:00Z"},
		{"1Wutc", "2026-01-12T00:00:00Z", "2026-01-19T00:00:00Z"},
		// 月线不定长：1 月 31 天、2 月 28 天，Next 必须走日历而不是加固定步长。
		{"1Mutc", "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"},
		{"1Mutc", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"1Mutc", "2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"3Mutc", "2026-10-01T00:00:00Z", "2027-01-01T00:00:00Z"},
	}
	for _, c := range cases {
		p := MustParsePeriod(c.bar)
		got := p.Next(ms(t, c.in))
		if want := ms(t, c.want); got != want {
			t.Errorf("%s Next(%s) = %s，期望 %s", c.bar, c.in, str(got), c.want)
		}
	}
}

// TestLastClosed 锁住本库最重要的一条不变量：当前这根 K 线还在走，
// 它的收盘价尚不可知，所以「最后一根已收盘」永远是它的【前一根】。
func TestLastClosed(t *testing.T) {
	p := MustParsePeriod("1Dutc")
	now := ms(t, "2026-01-15T08:07:33Z")
	if got, want := p.LastClosed(now), ms(t, "2026-01-14T00:00:00Z"); got != want {
		t.Errorf("LastClosed = %s，期望 %s", str(got), str(want))
	}
	// 恰好落在边界上时，前一根也才刚收盘。
	if got, want := p.LastClosed(ms(t, "2026-01-15T00:00:00Z")), ms(t, "2026-01-14T00:00:00Z"); got != want {
		t.Errorf("边界上 LastClosed = %s，期望 %s", str(got), str(want))
	}

	if !p.Closed(ms(t, "2026-01-14T00:00:00Z"), now) {
		t.Error("01-14 那根在 01-15 08:07 时应当已收盘")
	}
	if p.Closed(ms(t, "2026-01-15T00:00:00Z"), now) {
		t.Error("01-15 那根在 01-15 08:07 时还在走，不该算已收盘")
	}
}

func TestParsePeriodUnknown(t *testing.T) {
	if _, err := ParsePeriod("7x"); err == nil {
		t.Fatal("未知周期应当报错")
	}
	if err := RegisterFixedPeriod("7x", 7*time.Minute, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	p, err := ParsePeriod("7x")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Truncate(ms(t, "1970-01-01T00:16:00Z")), ms(t, "1970-01-01T00:14:00Z"); got != want {
		t.Errorf("自定义周期 Truncate = %s，期望 %s", str(got), str(want))
	}
	if err := RegisterFixedPeriod("bad", 0, time.Unix(0, 0)); err == nil {
		t.Error("步长为 0 应当报错")
	}
}

func TestVerifyAlignment(t *testing.T) {
	p := MustParsePeriod("1H")
	good := []Candle{{Ts: ms(t, "2026-01-15T08:00:00Z")}, {Ts: ms(t, "2026-01-15T09:00:00Z")}}
	if _, ok := VerifyAlignment(p, good); !ok {
		t.Error("对齐的数据不该被判为不对齐")
	}
	bad := append(good, Candle{Ts: ms(t, "2026-01-15T09:30:00Z")})
	if got, ok := VerifyAlignment(p, bad); ok || got != ms(t, "2026-01-15T09:30:00Z") {
		t.Errorf("应当报出 09:30 不对齐，实得 %s ok=%v", str(got), ok)
	}
}
