package csvstore

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// readCSV 把文件读成记录切片。
func readCSV(t *testing.T, path string) [][]string {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	return records
}

// sampleRow 构造一行测试数据。
func sampleRow(target string, success bool, latency float64) Row {
	return Row{
		Timestamp:     time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC),
		Target:        target,
		IP:            strings.Split(target, ":")[0],
		Port:          443,
		Success:       success,
		LatencyMS:     latency,
		ClientVersion: "0.1.0",
	}
}

// TestWritesHeaderAndRows 验证表头与数据行。
func TestWritesHeaderAndRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.csv")

	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Append(sampleRow("1.1.1.1:443", true, 12.5)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(sampleRow("8.8.8.8:443", false, 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records := readCSV(t, path)
	if len(records) != 3 {
		t.Fatalf("rows = %d, want 3 (header + 2)", len(records))
	}

	// 表头必须与 Header 完全一致。
	for i, want := range Header {
		if records[0][i] != want {
			t.Errorf("header[%d] = %q, want %q", i, records[0][i], want)
		}
	}
	// 每行列数一致，否则表格会错位。
	for i, record := range records {
		if len(record) != len(Header) {
			t.Errorf("row %d has %d columns, want %d", i, len(record), len(Header))
		}
	}
}

// TestRowSurvivesWithoutClose 验证**不调用 Close 也不丢数据**。
//
// 这是这个包存在的核心理由：中途 Ctrl+C、进程被杀、断电，
// 已经完成的测量必须还在文件里。
func TestRowSurvivesWithoutClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.csv")

	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := store.Append(sampleRow("1.1.1.1:443", true, 10)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(sampleRow("1.1.1.1:8443", true, 20)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// 刻意**不**调用 Close，直接读文件——模拟"进程被杀"。
	records := readCSV(t, path)
	if len(records) != 3 {
		t.Fatalf("rows on disk = %d, want 3; data was lost before Close", len(records))
	}
	if records[1][1] != "1.1.1.1:443" {
		t.Errorf("first data row target = %q", records[1][1])
	}

	_ = store.Close()
}

// TestAppendDoesNotRepeatHeader 验证追加模式不重复写表头。
//
// 追加模式下重复写表头，会让表头出现在文件中间——
// 那种文件用 pandas 读会直接报错。
func TestAppendDoesNotRepeatHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.csv")

	first, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Append(sampleRow("1.1.1.1:443", true, 10)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(Options{Path: path, Append: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Append(sampleRow("8.8.8.8:443", true, 20)); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	records := readCSV(t, path)
	if len(records) != 3 {
		t.Fatalf("rows = %d, want 3 (one header + two data)", len(records))
	}

	// 表头只能出现一次，且必须在第一行。
	for i, record := range records {
		if i > 0 && record[0] == Header[0] {
			t.Errorf("header repeated at row %d", i)
		}
	}
}

// TestUnmeasuredLatencyIsEmpty 验证"没测到"与 0ms 可区分。
func TestUnmeasuredLatencyIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.csv")

	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(sampleRow("1.1.1.1:443", false, 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(sampleRow("1.1.1.1:443", true, 0.5)); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	records := readCSV(t, path)
	index := columnIndex(t, "latency_ms")

	if records[1][index] != "" {
		t.Errorf("failed measurement latency = %q, want empty", records[1][index])
	}
	if records[2][index] != "0.500" {
		t.Errorf("successful measurement latency = %q, want 0.500", records[2][index])
	}
}

// TestConcurrentAppendIsSafe 验证并发写入不丢行、不串行错乱。
//
// 探测与跟踪会从多个 goroutine 写入；本机跑不了 -race，
// 因此用并发压力代替。
func TestConcurrentAppendIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.csv")

	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	const writers = 8
	const perWriter = 50

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				row := sampleRow("1.1.1.1:443", true, float64(id*100+i))
				if err := store.Append(row); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if got := store.Rows(); got != writers*perWriter {
		t.Errorf("Rows() = %d, want %d", got, writers*perWriter)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	records := readCSV(t, path)
	if len(records) != writers*perWriter+1 {
		t.Errorf("rows on disk = %d, want %d", len(records), writers*perWriter+1)
	}
	// 每一行都必须是完整的（列数正确），否则说明有交错写入。
	for i, record := range records {
		if len(record) != len(Header) {
			t.Errorf("row %d has %d columns, want %d", i, len(record), len(Header))
		}
	}
}

// TestSpecialCharactersAreQuoted 验证逗号与引号被正确转义。
func TestSpecialCharactersAreQuoted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.csv")

	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	row := sampleRow("1.1.1.1:443", false, 0)
	row.ErrorMessage = `bad "thing", with a comma`
	row.ASPath = "163 > CN2, 备用"
	if err := store.Append(row); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	records := readCSV(t, path)
	if len(records) != 2 {
		t.Fatalf("rows = %d, want 2", len(records))
	}
	msg := records[1][columnIndex(t, "error_message")]
	if !strings.Contains(msg, `bad "thing", with a comma`) {
		t.Errorf("error_message = %q, want the original text", msg)
	}
}

// TestSanitizeErrorMessageStripsLocalPaths 验证本机路径被替换掉。
func TestSanitizeErrorMessageStripsLocalPaths(t *testing.T) {
	cases := []struct {
		in       string
		mustNot  []string
		mustHave []string
	}{
		{
			in:       `nexttrace binary "C:\Users\alice\tools\nexttrace.exe" is not usable`,
			mustNot:  []string{"alice", `C:\Users`},
			mustHave: []string{"<path>"},
		},
		{
			in:       `exec /home/alice/bin/nexttrace: no such file or directory`,
			mustNot:  []string{"alice"},
			mustHave: []string{"<path>"},
		},
		{
			// 目标地址必须保留：那是"哪个目标失败了"。
			in:       `dial tcp 1.1.1.1:443: i/o timeout`,
			mustHave: []string{"1.1.1.1:443", "i/o timeout"},
		},
		{
			// "i/o" 里的斜杠不能被当成路径。
			in:       `read: i/o error`,
			mustHave: []string{"i/o error"},
			mustNot:  []string{"<path>"},
		},
	}

	for _, tc := range cases {
		got := SanitizeErrorMessage(tc.in)
		for _, bad := range tc.mustNot {
			if strings.Contains(got, bad) {
				t.Errorf("SanitizeErrorMessage(%q) = %q, must not contain %q", tc.in, got, bad)
			}
		}
		for _, want := range tc.mustHave {
			if !strings.Contains(got, want) {
				t.Errorf("SanitizeErrorMessage(%q) = %q, must contain %q", tc.in, got, want)
			}
		}
	}
}

// TestSanitizeTruncatesLongMessages 验证超长信息被截断且不切坏 UTF-8。
func TestSanitizeTruncatesLongMessages(t *testing.T) {
	long := strings.Repeat("错误", 400) // 每个汉字 3 字节，远超 300
	got := SanitizeErrorMessage(long)

	if len(got) > 320 {
		t.Errorf("length = %d, want it truncated near 300 bytes", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("truncated message should end with an ellipsis")
	}
	// 必须仍是合法 UTF-8（截断落在字符中间会产生乱码）。
	if !isValidUTF8(got) {
		t.Error("truncation produced invalid UTF-8")
	}
}

// TestOpenCreatesDirectory 验证父目录不存在时会被创建。
func TestOpenCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "out.csv")

	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Append(sampleRow("1.1.1.1:443", true, 1)); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("file was not created: %v", err)
	}
}

// TestOpenRejectsEmptyPath 验证空路径被拒绝。
func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(Options{Path: "  "}); err == nil {
		t.Error("Open accepted an empty path")
	}
}

// TestCloseIsIdempotent 验证重复 Close 不报错。
func TestCloseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.csv")
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// columnIndex 返回某列的下标。
func columnIndex(t *testing.T, name string) int {
	t.Helper()
	for i, header := range Header {
		if header == name {
			return i
		}
	}
	t.Fatalf("column %q not found", name)
	return -1
}

// isValidUTF8 报告字符串是否为合法 UTF-8。
func isValidUTF8(s string) bool {
	return utf8.ValidString(s)
}
