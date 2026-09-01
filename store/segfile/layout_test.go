package segfile

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	tickflow "github.com/dream-until-dawn/okx-tickflow-go"
)

// TestRootIsCallerChosen 把「落盘位置完全由使用者指定」钉死：
// 库里没有任何默认路径，各种路径形态都得能用。
func TestRootIsCallerChosen(t *testing.T) {
	tmp := t.TempDir()

	for _, name := range []string{
		"plain", "nested/several/levels", "with space", "中文目录", "dots.in.name",
	} {
		root := filepath.Join(tmp, name)
		s, err := Open(root)
		if err != nil {
			t.Fatalf("Open(%q): %v", name, err)
		}
		if err := s.Append(inst, bar, mkSeries(base, 3)); err != nil {
			t.Fatalf("%q 写入失败: %v", name, err)
		}
		abs, _ := filepath.Abs(root)
		if s.Root() != abs {
			t.Errorf("%q 的 Root() = %q，期望绝对路径 %q", name, s.Root(), abs)
		}
		s.Close()
	}

	if _, err := Open(""); err == nil {
		t.Error("空 root 应当报错，而不是落到某个默认目录")
	}
}

// TestPathMatchesReality 保证 Path() 报的位置就是文件实际所在的位置——
// 它是给人做诊断和备份用的，报错地方比不报更糟。
func TestPathMatchesReality(t *testing.T) {
	root := t.TempDir()
	s := open(t, root)
	if err := s.Append(inst, bar, mkSeries(base, 5)); err != nil {
		t.Fatal(err)
	}

	dat, meta, err := s.Path(inst, bar)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(s.Root(), "candles", inst)
	if filepath.Dir(dat) != wantDir {
		t.Errorf("Path 给的目录是 %q，期望 %q", filepath.Dir(dat), wantDir)
	}
	for _, p := range []string{dat, meta} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("Path 报了 %q，但那里没有文件: %v", p, err)
		}
	}
	// 命名空间那一层必须真的存在，不然「以后放逐笔数据」这个理由就落了空。
	if _, err := os.Stat(filepath.Join(s.Root(), "candles")); err != nil {
		t.Errorf("candles 命名空间目录不存在: %v", err)
	}

	if _, _, err := s.Path("../逃逸", bar); err == nil {
		t.Error("Path 也该挡住会跑出 root 的名字")
	}
}

// TestWriteLockIsExclusive 是加锁的全部理由：两个写者撞在同一个目录上时，
// 第二个必须当场报错，而不是默默把文件写坏。
func TestWriteLockIsExclusive(t *testing.T) {
	root := t.TempDir()
	s1, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	// 同一进程里再开一次同样要被挡下——两个 Store 各有各的内存锁，
	// 它们之间并不互斥，危险程度和两个进程一样。
	_, err = Open(root)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("第二次 Open 应当返回 ErrLocked，实为 %v", err)
	}
	// 错误信息要能指认占用者，否则使用者只能干瞪眼。
	if pid := strconv.Itoa(os.Getpid()); !strings.Contains(err.Error(), pid) {
		t.Errorf("错误信息里应当带上占用者 pid %s，实为：%v", pid, err)
	}
	if !strings.Contains(err.Error(), "ForceUnlock") {
		t.Errorf("错误信息该说明怎么脱困，实为：%v", err)
	}

	// 关掉之后锁要还回去。
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s1.Root(), lockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("Close 之后锁文件应当被清掉")
	}
	s2, err := Open(root)
	if err != nil {
		t.Fatalf("前一个写者退出后应当能重新打开: %v", err)
	}
	s2.Close()
}

// TestForceUnlock 处理进程崩溃遗留的陈旧锁。
// 本库不去猜持有者是否还活着——跨平台可靠地判断这件事要引入平台相关的依赖。
func TestForceUnlock(t *testing.T) {
	root := t.TempDir()
	s1, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃：进程没了，锁文件还在（这里不调 Close，直接丢掉引用）。
	_ = s1

	if _, err := Open(root); !errors.Is(err, ErrLocked) {
		t.Fatalf("陈旧锁应当仍然拦住新的写者，实为 %v", err)
	}
	if err := ForceUnlock(root); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(root)
	if err != nil {
		t.Fatalf("ForceUnlock 之后应当能打开: %v", err)
	}
	s2.Close()

	// 没有锁时 ForceUnlock 是个空操作，不该报错。
	if err := ForceUnlock(root); err != nil {
		t.Errorf("对没上锁的目录 ForceUnlock 不该报错，实为 %v", err)
	}
}

// TestReadOnlyCoexistsWithWriter：只读打开不取锁，可以和写者并存。
// 这正是「同步守护进程跑着，回测同时读」这个真实场景。
func TestReadOnlyCoexistsWithWriter(t *testing.T) {
	root := t.TempDir()
	w := open(t, root)
	if err := w.Append(inst, bar, mkSeries(base, 20)); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("写者持锁时只读打开应当成功: %v", err)
	}
	defer r.Close()

	if !r.ReadOnly() || w.ReadOnly() {
		t.Error("ReadOnly() 的报告不对")
	}
	got, err := r.Range(inst, bar, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("只读读到 %d 根，期望 20", len(got))
	}

	// 写操作一律拒绝。
	for name, err := range map[string]error{
		"Append":      r.Append(inst, bar, mkSeries(base+100*step, 1)),
		"Merge":       r.Merge(inst, bar, mkSeries(base, 1)),
		"AddCoverage": r.AddCoverage(inst, bar, tickflow.Range{From: 1, To: 2}),
	} {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("只读 Store 的 %s 应当返回 ErrReadOnly，实为 %v", name, err)
		}
	}

	// 只读打开一个不存在的目录要报错，而不是凭空造一个。
	if _, err := OpenReadOnly(filepath.Join(root, "没有这个目录")); err == nil {
		t.Error("只读打开不存在的目录应当报错")
	}
}

// TestReadOnlyDuringAppend 是并发访问里最常见、也最要紧的那一半：
// 同步守护进程持续追加最新 K 线，回测进程同时在读。这条必须在所有平台上成立。
func TestReadOnlyDuringAppend(t *testing.T) {
	root := t.TempDir()
	w := open(t, root)
	original := mkSeries(base, 30)
	if err := w.Append(inst, bar, original); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	snapshot, err := r.Range(inst, bar, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, original) {
		t.Fatal("快照与写入的数据不一致")
	}

	// 读者开着的同时追加——追加是按偏移写，不改名，不该受任何影响。
	if err := w.Append(inst, bar, mkSeries(base+30*step, 10)); err != nil {
		t.Fatalf("有只读端在读时追加失败了: %v", err)
	}

	// 读者仍然只看见自己打开那一刻的那 30 根，一根不多一根不少。
	after, err := r.Range(inst, bar, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, snapshot) {
		t.Fatalf("追加之后只读端读到了 %d 根，快照是 %d 根", len(after), len(snapshot))
	}

	// 重新打开才看得到新数据——这是文档里写明的语义。
	r2, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	fresh, err := r2.Range(inst, bar, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 40 {
		t.Errorf("重新打开后应当看到 40 根，实得 %d 根", len(fresh))
	}
}

// TestReadOnlyBlocksMergeOnWindows 把回填与只读端的冲突【实测出来的样子】钉住。
//
// 回填要把整个文件换掉（写临时文件再改名）。Windows 的 MoveFileEx 在目标被
// 任何句柄打开时一律失败——实测与 FILE_SHARE_DELETE 无关，加了也没用，所以
// 本库没有为此写平台特化代码。Unix 上改名不受已打开的句柄影响，照常成功。
//
// 无论哪种结果，都不能出现【数据损坏】：要么回填整个没发生，要么完整发生。
func TestReadOnlyBlocksMergeOnWindows(t *testing.T) {
	root := t.TempDir()
	w := open(t, root)
	original := mkSeries(base+50*step, 30)
	if err := w.Append(inst, bar, original); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := r.Range(inst, bar, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	mergeErr := w.Merge(inst, bar, mkSeries(base, 40))

	if runtime.GOOS == "windows" {
		if mergeErr == nil {
			t.Fatal("Windows 上只读端开着时回填本该失败；若这里通过了，" +
				"说明平台行为变了，文档与错误提示都要跟着改")
		}
		// 错误信息必须点破原因，否则使用者只会看到一句 Access denied。
		for _, want := range []string{"只读端", "追加不受此限制"} {
			if !strings.Contains(mergeErr.Error(), want) {
				t.Errorf("错误信息该提到 %q，实为：%v", want, mergeErr)
			}
		}
	} else if mergeErr != nil {
		t.Fatalf("Unix 上改名不受已打开句柄影响，回填不该失败: %v", mergeErr)
	}

	// 不管成没成，读者手上的快照都得原封不动。
	after, err := r.Range(inst, bar, 0, 0)
	if err != nil {
		t.Fatalf("回填之后只读端读失败: %v", err)
	}
	if !reflect.DeepEqual(after, snapshot) {
		t.Fatalf("只读端的快照被破坏了：%d 根 vs 原本 %d 根", len(after), len(snapshot))
	}

	// 回填失败不能把这条序列在写者这边弄成废的——数据文件原封未动，
	// 序列理应继续可读可写。这一条是上面那个失败路径照出来的。
	if got, err := w.Range(inst, bar, 0, 0); err != nil {
		t.Fatalf("回填失败之后写者读不了了: %v", err)
	} else if len(got) != len(original) {
		t.Errorf("回填失败之后写者读到 %d 根，期望原样的 %d 根", len(got), len(original))
	}
	if err := w.Append(inst, bar, mkSeries(base+200*step, 2)); err != nil {
		t.Fatalf("回填失败之后写者写不了了: %v", err)
	}
	if err := w.Merge(inst, bar, mkSeries(base+200*step, 2)); err != nil && runtime.GOOS != "windows" {
		t.Fatalf("回填失败之后再次回填也失败: %v", err)
	}

	// 读者退出之后，回填必须能做成。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Merge(inst, bar, mkSeries(base, 40)); err != nil {
		t.Fatalf("只读端退出后回填仍失败: %v", err)
	}
	all, err := w.Range(inst, bar, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 回填的 40 根（base..base+39）与原有的 30 根（base+50..base+79）不重叠。
	// 回填的 40 根（base..base+39）、原有的 30 根（base+50..base+79），
	// 外加上面为验证「失败后仍可用」而追加的 2 根。
	if len(all) != 72 {
		t.Errorf("回填后共 %d 根，期望 72", len(all))
	}
}

// TestLegacyLayoutIsDetected：旧布局的数据不能被当作「空库」静默忽略。
// 「数据凭空消失」是最难查的一类问题。
func TestLegacyLayoutIsDetected(t *testing.T) {
	root := t.TempDir()

	// 手工摆一份 v0.3 布局的数据：直接放在 <root>/<instId>/ 下。
	old := filepath.Join(root, inst)
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	metaJSON := `{"instId":"` + inst + `","bar":"` + bar +
		`","magic":"` + Magic + `","version":1,"recordSize":64,"count":0,` +
		`"firstTs":0,"lastTs":0,"coverage":null,"updatedAt":0}`
	if err := os.WriteFile(filepath.Join(old, bar+".meta"), []byte(metaJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Open(root)
	if err == nil {
		t.Fatal("旧布局的数据应当被认出来并报错")
	}
	for _, want := range []string{"旧布局", "candles"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里应当提到 %q，实为：%v", want, err)
		}
	}

	// 按提示挪进命名空间之后就能正常打开。
	if err := os.MkdirAll(filepath.Join(root, "candles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, filepath.Join(root, "candles", inst)); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatalf("挪好之后应当能打开: %v", err)
	}
	defer s.Close()
	if got, err := s.Series(); err != nil || len(got) != 1 {
		t.Errorf("Series() = %v, %v，期望认出那一条", got, err)
	}
}

// TestUnrelatedDirsAreNotMistakenForLegacy：数据目录里放着别的东西不该被误判。
func TestUnrelatedDirsAreNotMistakenForLegacy(t *testing.T) {
	root := t.TempDir()
	junk := filepath.Join(root, "logs")
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	// 一个 .meta 后缀但不是本库的文件。
	if err := os.WriteFile(filepath.Join(junk, "app.meta"), []byte(`{"foo":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatalf("无关目录不该被当成旧布局: %v", err)
	}
	s.Close()
}

// TestRootSurvivesChdir：相对路径在 Open 时就绝对化，之后进程换工作目录
// 不会让同一个 Store 指向别处。这是个真实存在、又很难查的坑。
func TestRootSurvivesChdir(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(home); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	s, err := Open("relative-store")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rootAtOpen := s.Root()

	if err := os.Chdir(elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(inst, bar, mkSeries(base, 3)); err != nil {
		t.Fatal(err)
	}
	if s.Root() != rootAtOpen {
		t.Errorf("换了工作目录之后 Root() 变成了 %q，原本是 %q", s.Root(), rootAtOpen)
	}
	dat, _, err := s.Path(inst, bar)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dat, rootAtOpen) {
		t.Errorf("文件跑到了 %q，本该在 %q 下", dat, rootAtOpen)
	}
	if _, err := os.Stat(dat); err != nil {
		t.Errorf("数据没落在原来的目录下: %v", err)
	}
}
