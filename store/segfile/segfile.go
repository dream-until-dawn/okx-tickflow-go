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
// 根目录由使用者指定，本库没有默认路径：
//
//	<root>/
//	  .lock                        写锁，见 Open
//	  candles/                     K 线的命名空间
//	    BTC-USDT-SWAP/
//	      1m.dat                   纯定长记录数组，【无文件头】，offset = i * 64
//	      1m.meta                  JSON，人可读
//	    ETH-USDT-SWAP/
//	      15m.dat
//	      15m.meta
//
// candles 那一层是给以后留位的：逐笔成交、盘口深度是形态完全不同的数据，
// 将来要放进来时不必迁移已有的 K 线。
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

	// lockFile 是写锁文件名，见 Open 的说明。
	lockFile = ".lock"
)

// SeriesKind 是数据在数据目录下的命名空间，同时也是一个 Option。
//
// Store 按 (instId, bar) 索引，而标记价的 BTC-USDT-SWAP/1m 与普通 K 线的
// BTC-USDT-SWAP/1m 是【两条不同的序列】——同一个键，两份数据。用命名空间分开，
// 而不是拼一个 "BTC-USDT-SWAP:mark" 之类的合成 instId：合成键会渗进 Meta、
// Coverage、SeriesID，以后再拆就是破坏性变更。
//
//	segfile.Open(root)                // candles/       普通 K 线
//	segfile.Open(root, segfile.Mark)  // mark-candles/  标记价
//	segfile.Open(root, segfile.Index) // index-candles/ 指数价
//
// 写锁也在命名空间之内，所以同一进程里同时开一个普通 Store 和一个标记价 Store
// 不会自己把自己锁住。
type SeriesKind string

const (
	// Candles 是普通 K 线，Open 不指定时的默认。
	Candles SeriesKind = "candles"

	// Mark 是标记价 K 线。
	//
	// OKX 的强平按标记价判定而不是成交价。回测里要建模爆仓就必须用这条序列——
	// 用成交价会让影线制造出真实不会发生的强平，对尾部风险就是强平的策略
	// （比如做多网格）尤其致命，而且是假阴性，结果里不留痕迹。
	//
	// ⚠️ 标记价历史有一条硬线：【港时 2020-01-01】。之后上线的合约与成交价同深，
	// 之前上线的一律被截到那天——BTC-USD-SWAP 差了 379 天。回测起点早于它时
	// 标记价根本不存在，见 docs/contract.md。
	Mark SeriesKind = "mark-candles"

	// Index 是指数价 K 线。triggerPxType 为 index 的算法委托用得上。
	// instId 用现货形式，如 "ETH-USDT"。
	Index SeriesKind = "index-candles"
)

func (k SeriesKind) apply(s *Store) { s.kind = k }

// Option 配置 Store 的打开方式。SeriesKind 本身就是一个 Option。
type Option interface{ apply(*Store) }

// knownKinds 用于识别数据目录下哪些子目录是本库的命名空间。
var knownKinds = []SeriesKind{Candles, Mark, Index}

// ErrReadOnly 表示在只读 Store 上尝试了写操作。
var ErrReadOnly = errors.New("segfile: 只读模式下不能写入")

// ErrLocked 表示数据目录已被另一个写者占用。
var ErrLocked = errors.New("segfile: 数据目录已被占用")

// Store 是 tickflow.Store 的定长文件实现。
//
// # 数据目录
//
// 落盘位置【完全由使用者指定】，本库没有任何默认路径。绝对路径、相对路径、
// 尚未创建的多层目录都可以；相对路径在 Open 时就换算成绝对路径，之后程序
// 再 os.Chdir 也不会让同一个 Store 指向别处。
//
// # 并发
//
// 进程内多读单写，由读写锁保证。跨进程靠 Open 时取的【写锁】：一个数据目录
// 同一时刻只能有一个写者，第二个 Open 当场报 ErrLocked，而不是默默把文件写坏。
// 同一进程里对同一目录 Open 两次同样会被挡下——两个 Store 各有各的内存锁，
// 它们之间并不互斥，危险程度和两个进程一样。
//
// 只读用 OpenReadOnly，它【不取锁】，可以与写者并存。读到的是打开那一刻的
// 快照：写者随后追加的数据要重新打开才看得到。追加只往文件尾写，读者按自己
// meta 里认的条数读，因此始终读到一份完整的旧快照。
//
// ⚠️ Windows 上有一条真实的限制：只读端持有某条序列时，写者【回填不了那条
// 序列】。回填走的是「写临时文件再改名」，而 Windows 的 MoveFileEx 在目标
// 被任何句柄打开时一律失败（实测与 FILE_SHARE_DELETE 无关，加了也没用），
// 于是 Merge 会报 Access denied。追加不受影响——它是按偏移写，不改名。
//
// 也就是说：「同步守护进程持续追加最新 K 线 + 回测进程并发读」在 Windows 上
// 完全正常；只有【深度回填】需要先让读者退出。Unix 上没有这个限制，改名不受
// 已打开的句柄影响。
type Store struct {
	root     string
	kind     SeriesKind
	readOnly bool
	locked   bool

	mu     sync.Mutex
	series map[string]*series
	closed bool
}

// Open 以【写模式】打开（必要时创建）位于 root 的存储，并取得排他写锁。
//
// root 已被另一个写者占用时返回 ErrLocked，错误信息里带着占用者的进程号、
// 主机名与占用时刻。进程崩溃会遗留陈旧的锁文件——本库【不去猜那个进程是否
// 还活着】：跨平台可靠地判断这件事需要平台相关的依赖，而本库刻意不引入。
// 确认无人使用后用 ForceUnlock 清掉即可。
func Open(root string, opts ...Option) (*Store, error) {
	s, err := newStore(root, false, opts)
	if err != nil {
		return nil, err
	}
	if err := s.acquire(); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenReadOnly 以只读模式打开位于 root 的存储。不取锁，可与写者并存。
//
// 所有写操作返回 ErrReadOnly。看到的是打开那一刻的快照。
func OpenReadOnly(root string, opts ...Option) (*Store, error) {
	return newStore(root, true, opts)
}

func newStore(root string, readOnly bool, opts []Option) (*Store, error) {
	if root == "" {
		return nil, errors.New("segfile: root 不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("segfile: 解析 %s 失败: %w", root, err)
	}
	s := &Store{root: abs, kind: Candles, readOnly: readOnly, series: map[string]*series{}}
	for _, o := range opts {
		o.apply(s)
	}
	if s.kind == "" {
		return nil, errors.New("segfile: 命名空间不能为空")
	}
	if err := validName(string(s.kind), "命名空间"); err != nil {
		return nil, err
	}

	if readOnly {
		if _, err := os.Stat(s.dir()); err != nil {
			return nil, fmt.Errorf("segfile: 只读打开 %s 失败: %w", s.dir(), err)
		}
	} else if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return nil, fmt.Errorf("segfile: 创建 %s 失败: %w", s.dir(), err)
	}
	if err := checkLegacyLayout(abs); err != nil {
		return nil, err
	}
	return s, nil
}

// dir 返回本 Store 所属命名空间的目录。
func (s *Store) dir() string { return filepath.Join(s.root, string(s.kind)) }

// Kind 返回本 Store 所属的命名空间。
func (s *Store) Kind() SeriesKind { return s.kind }

// Root 返回数据目录的绝对路径，供日志与诊断使用。
func (s *Store) Root() string { return s.root }

// ReadOnly 报告本 Store 是否为只读。
func (s *Store) ReadOnly() bool { return s.readOnly }

// Path 返回某条序列的两个文件路径，供诊断、备份或手工检查使用。
// 文件不一定已经存在。
func (s *Store) Path(instID, bar string) (dat, meta string, err error) {
	if err := validName(instID, "instId"); err != nil {
		return "", "", err
	}
	if err := validName(bar, "bar"); err != nil {
		return "", "", err
	}
	dir := filepath.Join(s.dir(), instID)
	return filepath.Join(dir, bar+".dat"), filepath.Join(dir, bar+".meta"), nil
}

// ---------------------------------------------------------------- 写锁

// lockInfo 记在锁文件里，纯粹是给人看的诊断信息。
type lockInfo struct {
	PID   int    `json:"pid"`
	Host  string `json:"host"`
	Since string `json:"since"`
	Root  string `json:"root"`
}

// lockPath 是写锁的位置。锁在【命名空间之内】——不然同一进程里开一个普通 Store
// 加一个标记价 Store 就会自己把自己锁住，而它们写的本就是两堆互不相干的文件。
func (s *Store) lockPath() string { return filepath.Join(s.dir(), lockFile) }

func (s *Store) acquire() error {
	// v1.1 及更早的写锁在数据目录【根上】，而那时的写者写的正是 candles/ 下的
	// 文件。若那种进程还活着，我们拿到命名空间里的新锁也照样会和它撞车——
	// 所以旧锁还在时，普通 K 线这个命名空间要当作已被占用。
	if s.kind == Candles {
		legacy := filepath.Join(s.root, lockFile)
		if _, err := os.Stat(legacy); err == nil {
			return fmt.Errorf("%w: %s 上有 v1.1 及更早遗留的旧锁（%s）。"+
				"确认无人使用后调 segfile.ForceUnlock 清掉，或直接删除 %s",
				ErrLocked, s.root, describeLock(legacy), legacy)
		}
	}
	path := s.lockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("segfile: 建立写锁 %s 失败: %w", path, err)
		}
		return fmt.Errorf("%w: %s（%s）。若确认那个进程已经退出，"+
			"调 segfile.ForceUnlock 清掉，或直接删除 %s",
			ErrLocked, s.root, describeLock(path), path)
	}
	host, _ := os.Hostname()
	raw, _ := json.MarshalIndent(lockInfo{
		PID: os.Getpid(), Host: host,
		Since: time.Now().Format(time.RFC3339), Root: s.root,
	}, "", "  ")
	_, werr := f.Write(append(raw, '\n'))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(path)
		if werr != nil {
			return werr
		}
		return cerr
	}
	s.locked = true
	return nil
}

func (s *Store) release() {
	if !s.locked {
		return
	}
	os.Remove(s.lockPath())
	s.locked = false
}

// describeLock 把锁文件里的信息读成一句人话。读不出来也不要紧，锁本身仍然生效。
func describeLock(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "锁文件存在但读不出内容"
	}
	var li lockInfo
	if json.Unmarshal(raw, &li) != nil || li.PID == 0 {
		return "锁文件内容无法解析"
	}
	return fmt.Sprintf("持有者 pid=%d host=%s 自 %s 起", li.PID, li.Host, li.Since)
}

// ForceUnlock 强行清掉某个命名空间上的写锁，不指定时清 Candles 的。
//
// 只在【确认没有任何进程正在写】这个命名空间时使用——进程崩溃会遗留锁文件，
// 这是它存在的唯一理由。在写者仍活着时调用它，等于把两个写者放进同一堆文件。
//
// 顺带清掉 v1.1 及更早遗留在数据目录根上的那个锁（那时候锁不分命名空间）。
func ForceUnlock(root string, opts ...Option) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	s := &Store{root: abs, kind: Candles}
	for _, o := range opts {
		o.apply(s)
	}
	for _, p := range []string{s.lockPath(), filepath.Join(abs, lockFile)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// isKnownKind 报告某个子目录名是否本库的命名空间。
func isKnownKind(name string) bool {
	for _, k := range knownKinds {
		if string(k) == name {
			return true
		}
	}
	return false
}

// checkLegacyLayout 挡住「数据还在旧布局里」这种情况。
//
// v0.3 及之前把序列直接放在 <root>/<instId>/ 下，v0.4 起挪进了
// <root>/candles/<instId>/。不检查的话，指着旧目录打开会得到一个空库，
// 而旧数据就在旁边躺着——这种「数据凭空消失」最难查。
func checkLegacyLayout(root string) error {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil // 目录还不存在或读不了，交给后续流程报错
	}
	for _, d := range dirs {
		if !d.IsDir() || isKnownKind(d.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".meta") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, d.Name(), f.Name()))
			if err != nil {
				continue
			}
			var m tickflow.Meta
			if json.Unmarshal(raw, &m) == nil && m.Magic == Magic {
				return fmt.Errorf("segfile: %s 下的数据还在旧布局里（v0.3 及之前直接放在 "+
					"<root>/<instId>/），v0.4 起挪到了 <root>/%s/<instId>/。"+
					"把 %s 这样的目录整个移进 %s 即可，文件本身不用动",
					root, Candles,
					filepath.Join(root, d.Name()), filepath.Join(root, string(Candles)))
			}
		}
	}
	return nil
}

var _ tickflow.Store = (*Store)(nil)

// Close 关掉全部已打开的序列，并释放写锁（只读 Store 本就没有锁）。
// 重复调用是安全的。
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
	s.release()
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
	se, err := openSeries(filepath.Join(s.dir(), instID), instID, bar, create, s.readOnly)
	if err != nil {
		return nil, err
	}
	s.series[key] = se
	return se, nil
}

// Append 在序列末尾追加，实现 tickflow.Store。
//
// 要求 cs 按 ts 严格升序，且首根晚于当前 LastTs——不满足就报错并指向 Merge。
// 这是同步最新数据的快路径：按偏移写，不改名，因此【不受只读端持有文件的影响】。
// 只读 Store 上返回 ErrReadOnly。
func (s *Store) Append(instID, bar string, cs []tickflow.Candle) error {
	if len(cs) == 0 {
		return nil
	}
	if s.readOnly {
		return ErrReadOnly
	}
	se, err := s.get(instID, bar, true)
	if err != nil {
		return err
	}
	return se.append(cs)
}

// Merge 把 cs 并入序列的任意位置，同 ts 以 cs 为准，实现 tickflow.Store。
//
// 代价是一次全文件重写（写临时文件再改名），所以【一整段回填要攒成一次调用】，
// 分块多次调用会是 O(n²)。Windows 上只读端正持有该序列时会失败——见 Store 的
// 说明；失败时数据原封未动，序列仍可用。只读 Store 上返回 ErrReadOnly。
func (s *Store) Merge(instID, bar string, cs []tickflow.Candle) error {
	if len(cs) == 0 {
		return nil
	}
	if s.readOnly {
		return ErrReadOnly
	}
	se, err := s.get(instID, bar, true)
	if err != nil {
		return err
	}
	return se.merge(cs)
}

// Meta 返回序列的元信息，实现 tickflow.Store。
// 序列不存在时返回 tickflow.ErrNoSeries。返回的是副本，改它不会影响存储。
func (s *Store) Meta(instID, bar string) (tickflow.Meta, error) {
	se, err := s.get(instID, bar, false)
	if err != nil {
		return tickflow.Meta{}, err
	}
	se.mu.RLock()
	defer se.mu.RUnlock()
	return se.meta.Clone(), nil
}

// AddCoverage 记录「[r.From, r.To) 这一段已请求并确认」，实现 tickflow.Store。
//
// 覆盖区间不能从数据推出来：一段区间里一根 K 线都没有，可能是 OKX 本就没产出，
// 也可能是还没拉，只有发起请求的一方知道。只读 Store 上返回 ErrReadOnly。
func (s *Store) AddCoverage(instID, bar string, r tickflow.Range) error {
	if r.Empty() {
		return nil
	}
	if s.readOnly {
		return ErrReadOnly
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

// Iter 返回 [from, to) 的升序游标，实现 tickflow.Store。to 为 0 表示直到末尾。
//
// 起点用二分定位。遍历期间若该序列被 Merge 整体重写，游标会【报错而不是接着读】
// ——那读到的是另一个位置上的数据。追加不影响已有下标，因此不会让游标失效。
// 用完记得 Close。
func (s *Store) Iter(instID, bar string, from, to int64) (tickflow.Iterator, error) {
	se, err := s.get(instID, bar, false)
	if err != nil {
		return nil, err
	}
	return se.iter(from, to)
}

// Range 是 Iter 的便捷包装，一次性读进内存，实现 tickflow.Store。
// 大范围请用 Iter——五年的 1m 线有两百多万根。
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

// Series 列出数据目录里已存在的全部序列，按 instId、bar 排序，
// 实现 tickflow.Store。目录为空时返回 nil 而不是错误。
func (s *Store) Series() ([]tickflow.SeriesID, error) {
	base := s.dir()
	dirs, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []tickflow.SeriesID
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(base, d.Name()))
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

func openSeries(dir, instID, bar string, create, readOnly bool) (*series, error) {
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

	if readOnly {
		if se.f, err = os.Open(se.dat); err != nil {
			return nil, err
		}
		// 只读时不修文件，只核对一下并如实报告不一致。
		return se, se.verify()
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

// verify 是 reconcile 的只读版本：发现不一致就报告，绝不改文件。
//
// 只读 Store 与写者并存时，数据比 meta 多是【正常】的——写者刚追加完数据、
// 还没更新 meta。读者按自己 meta 里认的条数读，读到的仍是一份完整的旧快照。
func (se *series) verify() error {
	st, err := se.f.Stat()
	if err != nil {
		return err
	}
	if have := st.Size() / RecordSize; have < se.meta.Count {
		return fmt.Errorf("segfile: %s 只有 %d 条记录，而 %s 声称有 %d 条",
			se.dat, have, se.metaPath, se.meta.Count)
	}
	return nil
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
	if rerr := os.Rename(tmp, se.dat); rerr != nil {
		// 改名失败时【数据文件原封未动】，这次回填等于没发生。把句柄接回去，
		// 序列继续可用——否则一次可恢复的失败会让这条序列在本进程里彻底作废，
		// 而使用者从错误信息上完全看不出这一点。
		if f, oerr := os.OpenFile(se.dat, os.O_RDWR, 0o644); oerr == nil {
			se.f = f
		}
		os.Remove(tmp)
		return fmt.Errorf("segfile: 替换 %s 失败: %w；"+
			"Windows 上这通常意味着有只读端正开着这条序列——"+
			"回填要改名整个文件，而 MoveFileEx 在目标被打开时会失败。"+
			"让读者先退出即可（追加不受此限制）。本次回填未生效，数据未改动",
			se.dat, rerr)
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
