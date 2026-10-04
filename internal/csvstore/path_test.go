package csvstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖"结果写到哪个文件"。
//
// 这一处的错误代价特别高而且**没有提示**：默认路径写死成
// data/results.csv 时，每跑一轮就覆盖上一轮。使用者往往是在几轮之间
// 比较不同国家、不同并发下的表现，等发现时上一轮已经没了。

// TestTimestampedPathIsSortable 验证文件名带时间且**按名字排序即按时间排序**。
func TestTimestampedPathIsSortable(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 5, 3, 15, 0, 0, time.UTC)

	early := TimestampedPath(dir, base)
	later := TimestampedPath(dir, base.Add(1*time.Hour))

	if !strings.HasPrefix(filepath.Base(early), "results-") {
		t.Errorf("name = %q, want a results- prefix", filepath.Base(early))
	}
	if !strings.HasSuffix(early, ".csv") {
		t.Errorf("name = %q, want a .csv suffix", early)
	}
	// 目录要听调用方的。
	if filepath.Dir(early) != dir {
		t.Errorf("dir = %q, want %q", filepath.Dir(early), dir)
	}
	// 早的必须排在晚的前面，否则文件管理器里看不出先后。
	if !(filepath.Base(early) < filepath.Base(later)) {
		t.Errorf("%q should sort before %q", filepath.Base(early), filepath.Base(later))
	}

	// 具体格式也钉住：冒号在 Windows 上非法，所以时分秒只能用别的分隔。
	if got := filepath.Base(early); got != "results-20261005-031500.csv" {
		t.Errorf("name = %q, want results-20261005-031500.csv", got)
	}
	if strings.ContainsAny(filepath.Base(early), ":") {
		t.Error("the name contains a colon, which Windows forbids in file names")
	}
}

// TestTimestampedPathDefaultsDir 验证目录留空时用默认目录。
func TestTimestampedPathDefaultsDir(t *testing.T) {
	got := TimestampedPath("", time.Now())
	if filepath.Dir(got) != DefaultDir {
		t.Errorf("dir = %q, want %q", filepath.Dir(got), DefaultDir)
	}
}

// TestUniquePathLeavesFreeNameAlone 验证没冲突时不动名字。
func TestUniquePathLeavesFreeNameAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results-20261005-031500.csv")
	if got := UniquePath(path); got != path {
		t.Errorf("UniquePath = %q, want the path unchanged", got)
	}
}

// TestUniquePathAvoidsOverwriting 验证同名已存在时换一个名字。
//
// 这是"同一秒跑两轮"的护栏，也是整个改动的核心承诺。
//
// 为什么需要：时间戳只精确到秒，而 `--limit 1` 的扫描几百毫秒就结束，
// 脚本里连着跑两轮完全可能落在同一秒。少了这一步，那种情况下**仍然
// 会覆盖**——而"不会覆盖"正是这个改动要保证的事，留一个一秒宽的
// 漏洞等于没修。
func TestUniquePathAvoidsOverwriting(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 5, 3, 15, 0, 0, time.UTC)
	first := TimestampedPath(dir, now)

	// 第一轮"写下"这个文件。
	if err := os.WriteFile(first, []byte("header\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := UniquePath(TimestampedPath(dir, now))
	if second == first {
		t.Fatalf("second run reused %q; it would overwrite the first run", first)
	}
	if _, err := os.Stat(second); err == nil {
		t.Fatalf("%q already exists", second)
	}
	// 新名字要能看出与第一个的关系，而不是一串随机字符。
	if !strings.HasPrefix(filepath.Base(second), "results-20261005-031500-") {
		t.Errorf("name = %q, want the timestamp plus a suffix", filepath.Base(second))
	}

	// 连开三次也各自不同。
	if err := os.WriteFile(second, []byte("header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third := UniquePath(TimestampedPath(dir, now))
	if third == first || third == second {
		t.Errorf("third = %q collides with an earlier run", third)
	}
}

// TestUniquePathTreatsDirectoryAsTaken 验证目标是个目录时也算被占用。
//
// 写进目录名会失败，但那时应当由"打开文件"那一步如实报错；
// 这里把目录当成占用、换一个名字，免得在一开始就撞上。
func TestUniquePathTreatsDirectoryAsTaken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results-20261005-031500.csv")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := UniquePath(path); got == path {
		t.Errorf("UniquePath kept %q although a directory sits there", path)
	}
}
