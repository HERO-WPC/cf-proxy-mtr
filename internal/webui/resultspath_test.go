package webui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
)

// 本文件覆盖"当前结果文件"的推导。
//
// 这一处出错的代价很隐蔽：它不会崩，只会把结果写到一个奇怪的地方，
// 而接口报出的路径看起来又"像是对的"。实测踩过一次——配置里的
// 结果路径是 data/results.csv（一个**文件**），被当成目录用之后
// 生成的是 data/results.csv/results-20261005-033610.csv：
// 把一个本该是文件的名字变成了目录，写入也随之失败。

// TestResultsDirTakesDirectoryOfConfiguredPath 验证取的是目录而不是文件路径本身。
func TestResultsDirTakesDirectoryOfConfiguredPath(t *testing.T) {
	cases := map[string]string{
		"data/results.csv":               "data",
		"data":                           csvstore.DefaultDir, // 没有文件名，取不到目录
		"results.csv":                    csvstore.DefaultDir, // 只有文件名
		"":                               csvstore.DefaultDir,
		"   ":                            csvstore.DefaultDir,
		filepath.Join("a", "b", "c.csv"): filepath.Join("a", "b"),
	}

	for input, want := range cases {
		if got := ResultsDir(input); got != want {
			t.Errorf("ResultsDir(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestResultsPathStateKeepsCurrentStable 验证读取用的"当前文件"是稳定的。
//
// 每次调用都算一个新名字的话，界面每次刷新看到的路径都不一样，
// 会让人以为自己在看不同的文件；而"还没有结果"也应当稳定地
// 指向同一个不存在的名字。
func TestResultsPathStateKeepsCurrentStable(t *testing.T) {
	var state resultsPathState

	first := state.current(t.TempDir())
	second := state.current(t.TempDir()) // 再传一个不同的目录，也不该换名字
	if first != second {
		t.Errorf("current changed between calls: %q then %q", first, second)
	}
	if !strings.HasSuffix(first, ".csv") {
		t.Errorf("current = %q, want a .csv path", first)
	}
}

// TestResultsPathStateNextGeneratesNewFile 验证测量每次要一个新文件。
//
// 这是"不覆盖上一轮"的落点。用同一个目录连续取两次，必须得到两个
// 不同的名字——**即使两次落在同一秒**（时间戳只到秒，而
// `--limit 1` 的测量几百毫秒就结束）。
func TestResultsPathStateNextGeneratesNewFile(t *testing.T) {
	dir := t.TempDir()

	var state resultsPathState
	first := state.next(dir)
	second := state.next(dir)

	if first == second {
		t.Fatalf("next returned the same path twice: %q", first)
	}
	if filepath.Dir(first) != dir || filepath.Dir(second) != dir {
		t.Errorf("paths landed outside the configured directory: %q, %q", first, second)
	}
}

// TestResultsPathStateSetWins 验证显式给的路径会成为当前文件。
//
// 使用者填了路径就得按他说的来：后续的读取与跟踪都该指向那份文件，
// 而不是另外自动命名一个。
func TestResultsPathStateSetWins(t *testing.T) {
	var state resultsPathState
	custom := filepath.Join(t.TempDir(), "my.csv")

	state.set(custom)
	if got := state.current(t.TempDir()); got != custom {
		t.Errorf("current = %q, want the explicitly set %q", got, custom)
	}

	// 空路径不该把已记下的覆盖掉。
	state.set("   ")
	if got := state.current(t.TempDir()); got != custom {
		t.Errorf("current = %q after an empty set, want %q", got, custom)
	}
}

// TestTimestampedNamesSortChronologically 验证按文件名排序就是按时间排序。
//
// 使用者会直接在文件管理器里按名字排，因此这个性质是有用的：
// 一眼看出哪份是哪一轮。
func TestTimestampedNamesSortChronologically(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 5, 3, 15, 0, 0, time.UTC)

	names := []string{
		filepath.Base(csvstore.TimestampedPath(dir, base)),
		filepath.Base(csvstore.TimestampedPath(dir, base.Add(2*time.Hour))),
		filepath.Base(csvstore.TimestampedPath(dir, base.Add(30*time.Minute))),
	}

	if !(names[0] < names[2] && names[2] < names[1]) {
		t.Errorf("names do not sort chronologically: %v", names)
	}
}
