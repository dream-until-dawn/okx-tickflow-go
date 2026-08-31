// Package segfile 用定长记录文件持久化 K 线，是 tickflow.Store 的默认实现。
//
// # 为什么不是 SQLite / bbolt / 时序库
//
// K 线是【定长、追加、按时间有序】的负载，和列式定长文件完美契合。其余选项：
// mattn/go-sqlite3 与 DuckDB 需要 CGO；modernc.org/sqlite 纯 Go 可行，但几十 MB
// 的生成代码塞进一个数据层库的依赖树里，代价与收益不成比例；bbolt 可行，但
// B+ 树页会让文件显著大于裸数据，而我们并不需要事务；InfluxDB 之类要外部服务，
// 与「库」的定位冲突。tickflow.Store 是接口，谁想接 ClickHouse 都行。
//
// # 布局
//
//	root/
//	  BTC-USDT-SWAP/
//	    1m.dat     纯定长记录数组，【无文件头】，offset = i * 64
//	    1m.meta    JSON，人可读
//
// .dat 不放文件头，就是纯粹的记录数组——这样 offset = i*64 不用加偏移，
// 二分与随机访问都最干净。magic / version / recordSize 全放 .meta。
//
// 一条记录 64 字节，小端：
//
//	ts int64 | open high low close vol volCcy volCcyQuote (7 × float64)
//	   8B    |                      56B
//
// 1m 线一年约 52.6 万根 ≈ 33.6MB，五年 ≈ 168MB 单文件。v1 不分段：现代文件系统
// 扛这个规模毫无压力，而段索引、跨段查询、段边界处理都是纯粹的复杂度。
//
// # 为什么是二分而不是槽位寻址
//
// 按 (ts-base)/步长 做 O(1) 槽位寻址要求空洞也占 64 字节，而小币种或维护期
// OKX 根本不产出该根 K 线；且 1M 不定长，除不出槽位号。改成「定长记录按 ts
// 升序 + Seek 时二分」：空洞不占空间、所有周期统一。回测是顺序读，二分只在
// 起点发生一次，没有真实性能损失。
package segfile

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

const (
	// Magic 标识文件族，写在 .meta 里。
	Magic = "TKFL"
	// Version 是存储格式版本。
	Version = 1
	// RecordSize 是单条记录的字节数。
	RecordSize = 64

	// blockRecords 是遍历时单次读取的记录数（64KB 一块）。
	blockRecords = 1024
)

// Store 是 tickflow.Store 的定长文件实现。
//
// 并发：进程内安全，多读单写。跨进程写同一份数据【不受保护】——那本就该由
// 使用者规避，一个库无法在所有平台上可靠地做到这点。
type Store struct {
	root string

	mu     sync.Mutex
	series map[string]*series
	closed bool
}

// Open 打开（必要时创建）一个位于 root 的存储。
func Open(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("segfile: root 不能为空")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("segfile: 创建 %s 失败: %w", root, err)
	}
	return &Store{root: root, series: map[string]*series{}}, nil
}

var _ tickflow.Store = (*Store)(nil)

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var firstErr error
	for _, se := range s.series {
		if err := se.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.series = nil
	return firstErr
}

// get 取得（必要时打开）一条序列。create 为 false 且序列不存在时返回 ErrNoSeries。
func (s *Store) get(instID, bar string, create bool) (*series, error) {
	if err := validName(instID, "instId"); err != nil {
		return nil, err
	}
	if err := validName(bar, "bar"); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("segfile: store 已关闭")
	}
	key := instID + "\x00" + bar
	if se, ok := s.series[key]; ok {
		return se, nil
	}
	se, err := openSeries(filepath.Join(s.root, instID), instID, bar, create)
	if err != nil {
		return nil, err
	}
	s.series[key] = se
	return se, nil
}

func (s *Store) Append(instID, bar string, cs []tickflow.Candle) error {
	if len(cs) == 0 {
		return nil
	}
	se, err := s.get(instID, bar, true)
	if err != nil {
		return err
	}
	return se.append(cs)
}

func (s *Store) Merge(instID, bar string, cs []tickflow.Candle) error {
	if len(cs) == 0 {
		return nil
	}
	se, err := s.get(instID, bar, true)
	if err != nil {
		return err
	}
	return se.merge(cs)
}

func (s *Store) Meta(instID, bar string) (tickflow.Meta, error) {
	se, err := s.get(instID, bar, false)
	if err != nil {
		return tickflow.Meta{}, err
	}
	se.mu.RLock()
	defer se.mu.RUnlock()
	return se.meta.Clone(), nil
}

func (s *Store) AddCoverage(instID, bar string, r tickflow.Range) error {
	if r.Empty() {
		return nil
	}
	se, err := s.get(instID, bar, true)
	if err != nil {
		return err
	}
	se.mu.Lock()
	defer se.mu.Unlock()
	se.meta.Coverage = se.meta.Coverage.Add(r)
	return se.writeMeta()
}

func (s *Store) Iter(instID, bar string, from, to int64) (tickflow.Iterator, error) {
	se, err := s.get(instID, bar, false)
	if err != nil {
		return nil, err
	}
	return se.iter(from, to)
}

func (s *Store) Range(instID, bar string, from, to int64) ([]tickflow.Candle, error) {
	it, err := s.Iter(instID, bar, from, to)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var out []tickflow.Candle
	for it.Next() {
		out = append(out, it.Candle())
	}
	return out, it.Err()
}

func (s *Store) Series() ([]tickflow.SeriesID, error) {
	dirs, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []tickflow.SeriesID
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.root, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if name := strings.TrimSuffix(f.Name(), ".meta"); name != f.Name() {
				out = append(out, tickflow.SeriesID{InstID: d.Name(), Bar: name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].InstID != out[j].InstID {
			return out[i].InstID < out[j].InstID
		}
		return out[i].Bar < out[j].Bar
	})
	return out, nil
}

// ---------------------------------------------------------------- series

type series struct {
	mu       sync.RWMutex
	dat      string
	metaPath string
	f        *os.File
	meta     tickflow.Meta

	// gen 每次【重写整个文件】时递增。追加不会改变已有记录的下标，所以不递增。
	// 遍历中的游标靠它发现「脚下的文件被换掉了」，从而报错而不是读到错位的数据。
	gen uint64
}

func openSeries(dir, instID, bar string, create bool) (*series, error) {
	se := &series{
		dat:      filepath.Join(dir, bar+".dat"),
		metaPath: filepath.Join(dir, bar+".meta"),
	}

	raw, err := os.ReadFile(se.metaPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &se.meta); err != nil {
			return nil, fmt.Errorf("segfile: 解析 %s 失败: %w", se.metaPath, err)
		}
		if se.meta.Magic != Magic || se.meta.RecordSize != RecordSize {
			return nil, fmt.Errorf("segfile: %s 不是本库的文件（magic=%q recordSize=%d）",
				se.metaPath, se.meta.Magic, se.meta.RecordSize)
		}
		if se.meta.Version > Version {
			return nil, fmt.Errorf("segfile: %s 的格式版本 %d 高于本库支持的 %d",
				se.metaPath, se.meta.Version, Version)
		}
	case errors.Is(err, os.ErrNotExist):
		if !create {
			return nil, fmt.Errorf("%w: %s/%s", tickflow.ErrNoSeries, instID, bar)
		}
		se.meta = tickflow.Meta{
			InstID: instID, Bar: bar,
			Magic: Magic, Version: Version, RecordSize: RecordSize,
		}
	default:
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if se.f, err = os.OpenFile(se.dat, os.O_RDWR|os.O_CREATE, 0o644); err != nil {
		return nil, err
	}
	if err := se.reconcile(); err != nil {
		se.f.Close()
		return nil, err
	}
	return se, nil
}

// reconcile 处理「写完数据、还没写 meta 就崩了」的残留。
//
// 落盘顺序是【先数据后 meta】，所以崩溃后只可能是数据比 meta 多。多出来的那截
// 没有被 coverage 认领，直接截掉重拉即可——这是安全的一侧。反过来 meta 比数据多
// 意味着真的丢了数据，此时不做静默修复：那会把一个本不该发生的状态掩盖过去。
func (se *series) reconcile() error {
	st, err := se.f.Stat()
	if err != nil {
		return err
	}
	have := st.Size() / RecordSize
	switch {
	case have == se.meta.Count && st.Size()%RecordSize == 0:
		return nil
	case have > se.meta.Count:
		return se.f.Truncate(se.meta.Count * RecordSize)
	case st.Size()%RecordSize != 0 && have >= se.meta.Count:
		return se.f.Truncate(se.meta.Count * RecordSize)
	default:
		return fmt.Errorf("segfile: %s 只有 %d 条记录，而 %s 声称有 %d 条——数据缺失。"+
			"删掉 .meta 让本库按文件重建，或从备份恢复",
			se.dat, have, se.metaPath, se.meta.Count)
	}
}

func (se *series) close() error {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.f == nil {
		return nil
	}
	err := se.f.Close()
	se.f = nil
	return err
}

func (se *series) append(cs []tickflow.Candle) error {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.f == nil {
		return errors.New("segfile: 序列已关闭")
	}

	for i, c := range cs {
		if i > 0 && c.Ts <= cs[i-1].Ts {
			return fmt.Errorf("segfile: Append 要求 ts 严格升序，第 %d 根 %d 不大于前一根 %d",
				i, c.Ts, cs[i-1].Ts)
		}
	}
	if se.meta.Count > 0 && cs[0].Ts <= se.meta.LastTs {
		return fmt.Errorf("segfile: Append 只能追加在末尾，首根 %d 不晚于已有的 LastTs %d；"+
			"要写更早的数据请用 Merge", cs[0].Ts, se.meta.LastTs)
	}

	buf := make([]byte, len(cs)*RecordSize)
	for i, c := range cs {
		encode(buf[i*RecordSize:], c)
	}
	if _, err := se.f.WriteAt(buf, se.meta.Count*RecordSize); err != nil {
		return err
	}
	// 先把数据落到盘上，再更新 meta——顺序反了的话，崩溃会留下一份「meta 声称
	// 已覆盖、数据却不在」的状态，那是会静默产生空洞的一侧。
	if err := se.f.Sync(); err != nil {
		return err
	}

	if se.meta.Count == 0 {
		se.meta.FirstTs = cs[0].Ts
	}
	se.meta.Count += int64(len(cs))
	se.meta.LastTs = cs[len(cs)-1].Ts
	return se.writeMeta()
}

func (se *series) merge(in []tickflow.Candle) error {
	in = append([]tickflow.Candle(nil), in...)
	sort.Slice(in, func(i, j int) bool { return in[i].Ts < in[j].Ts })
	// 去重：同 ts 以后来者为准。
	dedup := in[:0]
	for i, c := range in {
		if i > 0 && c.Ts == dedup[len(dedup)-1].Ts {
			dedup[len(dedup)-1] = c
			continue
		}
		dedup = append(dedup, c)
	}
	in = dedup

	se.mu.Lock()
	defer se.mu.Unlock()
	if se.f == nil {
		return errors.New("segfile: 序列已关闭")
	}

	tmp := se.dat + ".tmp"
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if out != nil {
			out.Close()
			os.Remove(tmp)
		}
	}()

	w := bufio.NewWriterSize(out, 1<<20)
	r := bufio.NewReaderSize(io.NewSectionReader(se.f, 0, se.meta.Count*RecordSize), 1<<20)

	var (
		rec         = make([]byte, RecordSize)
		count       int64
		first, last int64
		cur         tickflow.Candle
		haveCur     bool
		i           int
	)
	emit := func(c tickflow.Candle) error {
		encode(rec, c)
		if _, err := w.Write(rec); err != nil {
			return err
		}
		if count == 0 {
			first = c.Ts
		}
		last = c.Ts
		count++
		return nil
	}
	next := func() (bool, error) {
		if _, err := io.ReadFull(r, rec); err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		cur = decode(rec)
		return true, nil
	}

	if haveCur, err = next(); err != nil {
		return err
	}
	for haveCur || i < len(in) {
		switch {
		case !haveCur:
			if err := emit(in[i]); err != nil {
				return err
			}
			i++
		case i >= len(in):
			old := cur
			if haveCur, err = next(); err != nil {
				return err
			}
			if err := emit(old); err != nil {
				return err
			}
		case in[i].Ts < cur.Ts:
			if err := emit(in[i]); err != nil {
				return err
			}
			i++
		case in[i].Ts > cur.Ts:
			old := cur
			if haveCur, err = next(); err != nil {
				return err
			}
			if err := emit(old); err != nil {
				return err
			}
		default: // ts 相同，新数据覆盖旧的
			if err := emit(in[i]); err != nil {
				return err
			}
			i++
			if haveCur, err = next(); err != nil {
				return err
			}
		}
	}

	if err := w.Flush(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	out = nil // 交给下面的 rename，defer 不再清理

	if err := se.f.Close(); err != nil {
		return err
	}
	se.f = nil
	if err := os.Rename(tmp, se.dat); err != nil {
		return fmt.Errorf("segfile: 替换 %s 失败: %w", se.dat, err)
	}
	if se.f, err = os.OpenFile(se.dat, os.O_RDWR, 0o644); err != nil {
		return err
	}
	se.gen++

	se.meta.Count, se.meta.FirstTs, se.meta.LastTs = count, first, last
	return se.writeMeta()
}

// writeMeta 原子地写 meta：先写临时文件并 fsync，再 rename 覆盖。
// 调用方须持有写锁。
func (se *series) writeMeta() error {
	se.meta.UpdatedAt = time.Now().UnixMilli()
	raw, err := json.MarshalIndent(se.meta, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmp := se.metaPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, se.metaPath)
}

// search 二分出第一个 ts >= target 的记录下标；都比 target 小则返回 Count。
// 调用方须持有读锁。
func (se *series) search(target int64) (int64, error) {
	lo, hi := int64(0), se.meta.Count
	buf := make([]byte, 8)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if _, err := se.f.ReadAt(buf, mid*RecordSize); err != nil {
			return 0, err
		}
		if int64(binary.LittleEndian.Uint64(buf)) < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}

func (se *series) iter(from, to int64) (tickflow.Iterator, error) {
	se.mu.RLock()
	defer se.mu.RUnlock()
	if se.f == nil {
		return nil, errors.New("segfile: 序列已关闭")
	}
	start, err := se.search(from)
	if err != nil {
		return nil, err
	}
	end := se.meta.Count
	if to > 0 {
		if end, err = se.search(to); err != nil {
			return nil, err
		}
	}
	return &iterator{se: se, idx: start, end: end, gen: se.gen}, nil
}

// ---------------------------------------------------------------- iterator

type iterator struct {
	se  *series
	idx int64
	end int64
	gen uint64

	buf  []byte
	n    int // buf 里的记录数
	at   int // buf 内的游标
	cur  tickflow.Candle
	err  error
	done bool
}

func (it *iterator) Next() bool {
	if it.err != nil || it.done {
		return false
	}
	if it.at >= it.n {
		if !it.fill() {
			return false
		}
	}
	it.cur = decode(it.buf[it.at*RecordSize:])
	it.at++
	it.idx++
	return true
}

func (it *iterator) fill() bool {
	if it.idx >= it.end {
		it.done = true
		return false
	}
	it.se.mu.RLock()
	defer it.se.mu.RUnlock()

	if it.se.f == nil {
		it.err = errors.New("segfile: 遍历期间序列被关闭")
		return false
	}
	// 只有整体重写（Merge）会打乱下标；追加不会。发现被重写就报错，
	// 而不是接着读——那读到的是另一个位置上的数据，是无声的错。
	if it.se.gen != it.gen {
		it.err = errors.New("segfile: 序列在遍历期间被重写（Merge），游标已失效；请重新取一次 Iter")
		return false
	}

	want := it.end - it.idx
	if want > blockRecords {
		want = blockRecords
	}
	if cap(it.buf) < int(want)*RecordSize {
		it.buf = make([]byte, int(want)*RecordSize)
	}
	it.buf = it.buf[:int(want)*RecordSize]
	if _, err := it.se.f.ReadAt(it.buf, it.idx*RecordSize); err != nil {
		it.err = err
		return false
	}
	it.n, it.at = int(want), 0
	return true
}

func (it *iterator) Candle() tickflow.Candle { return it.cur }
func (it *iterator) Err() error              { return it.err }
func (it *iterator) Close() error            { it.done = true; return nil }

// ---------------------------------------------------------------- 编解码

func encode(b []byte, c tickflow.Candle) {
	binary.LittleEndian.PutUint64(b[0:], uint64(c.Ts))
	binary.LittleEndian.PutUint64(b[8:], math.Float64bits(c.Open))
	binary.LittleEndian.PutUint64(b[16:], math.Float64bits(c.High))
	binary.LittleEndian.PutUint64(b[24:], math.Float64bits(c.Low))
	binary.LittleEndian.PutUint64(b[32:], math.Float64bits(c.Close))
	binary.LittleEndian.PutUint64(b[40:], math.Float64bits(c.Vol))
	binary.LittleEndian.PutUint64(b[48:], math.Float64bits(c.VolCcy))
	binary.LittleEndian.PutUint64(b[56:], math.Float64bits(c.VolCcyQuote))
}

func decode(b []byte) tickflow.Candle {
	return tickflow.Candle{
		Ts:          int64(binary.LittleEndian.Uint64(b[0:])),
		Open:        math.Float64frombits(binary.LittleEndian.Uint64(b[8:])),
		High:        math.Float64frombits(binary.LittleEndian.Uint64(b[16:])),
		Low:         math.Float64frombits(binary.LittleEndian.Uint64(b[24:])),
		Close:       math.Float64frombits(binary.LittleEndian.Uint64(b[32:])),
		Vol:         math.Float64frombits(binary.LittleEndian.Uint64(b[40:])),
		VolCcy:      math.Float64frombits(binary.LittleEndian.Uint64(b[48:])),
		VolCcyQuote: math.Float64frombits(binary.LittleEndian.Uint64(b[56:])),
	}
}

// validName 挡住会跑出 root 的名字。instId 与 bar 都直接作为路径分量使用。
func validName(s, what string) error {
	if s == "" {
		return fmt.Errorf("segfile: %s 不能为空", what)
	}
	if s == "." || s == ".." || strings.ContainsAny(s, `/\:*?"<>|`) {
		return fmt.Errorf("segfile: %s %q 含有不能用作路径的字符", what, s)
	}
	for _, r := range s {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if !ok {
			return fmt.Errorf("segfile: %s %q 含有不能用作路径的字符 %q", what, s, r)
		}
	}
	return nil
}
