package service

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
)

// TestRunCSVScanWritesRows 验证扫描把结果写进 CSV。
func TestRunCSVScanWritesRows(t *testing.T) {
	port := listenLocal(t)
	cache := writeCache(t, port)
	svc := testService(t, cache)

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath: out,
		Timeout:    2 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}

	if result.Targets != 1 {
		t.Errorf("Targets = %d, want 1", result.Targets)
	}
	if result.Succeeded != 1 {
		t.Errorf("Succeeded = %d, want 1 (a real listener is accepting)", result.Succeeded)
	}
	if result.RowsWritten != 1 {
		t.Errorf("RowsWritten = %d, want 1", result.RowsWritten)
	}
	if result.Errors != 0 {
		t.Errorf("Errors = %d, want 0", result.Errors)
	}

	records := readCSVFile(t, out)
	if len(records) != 2 {
		t.Fatalf("csv rows = %d, want 2 (header + 1)", len(records))
	}
	if records[1][columnOf(t, "target")] == "" {
		t.Error("target column is empty")
	}
	if records[1][columnOf(t, "success")] != "true" {
		t.Errorf("success = %q, want true", records[1][columnOf(t, "success")])
	}
}

// TestRunCSVScanNoDatabase 验证扫描**不会**创建数据库文件。
//
// 这是这次改动的核心诉求：结果只进 CSV。
func TestRunCSVScanNoDatabase(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))
	dbPath := svc.Options().DBPath

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := svc.RunCSVScan(ctx, CSVScanOptions{OutputPath: out, Timeout: 2 * time.Second}); err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}

	if _, err := os.Stat(dbPath); err == nil {
		t.Errorf("a database was created at %s; CSV mode must not touch the database", dbPath)
	}
}

// TestRunCSVScanKeepsResultsWithoutClose 验证进程被中断也不丢已完成的行。
//
// 用取消 context 模拟 Ctrl+C，然后**不开新文件**直接读——
// 已经测完的目标必须都在。
func TestRunCSVScanKeepsResultsOnInterrupt(t *testing.T) {
	// 多个目标，保证有时间在中途取消。
	ports := make([]int, 0, 6)
	for i := 0; i < 6; i++ {
		ports = append(ports, listenLocal(t))
	}
	svc := testService(t, writeCache(t, ports...))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithCancel(context.Background())

	// 测到第二个目标时就取消。
	seen := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.RunCSVScan(ctx, CSVScanOptions{
			OutputPath: out,
			Timeout:    200 * time.Millisecond,
			OnTarget: func(string) {
				seen++
				if seen == 2 {
					cancel()
				}
			},
		})
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("scan did not return after cancel")
	}

	// 现在直接读文件（不依赖任何 Close 语义）。
	records := readCSVFile(t, out)
	if len(records) < 2 {
		t.Fatalf("csv rows = %d; results measured before the interrupt were lost", len(records))
	}
	// 表头必须仍然只有一行。
	if records[0][0] != csvstore.Header[0] {
		t.Errorf("first row is not the header: %v", records[0])
	}
	t.Logf("中断后文件里有 %d 行数据", len(records)-1)
}

// TestRunCSVScanRejectsEmptyPath 验证没给输出路径时明确报错。
func TestRunCSVScanRejectsEmptyPath(t *testing.T) {
	svc := testService(t, writeCache(t, listenLocal(t)))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.RunCSVScan(ctx, CSVScanOptions{}); !IsUsage(err) {
		t.Errorf("error = %v, want a usage error", err)
	}
}

// TestRunCSVScanValidatesTraceModeFirst 验证参数校验发生在副作用之前。
func TestRunCSVScanValidatesTraceModeFirst(t *testing.T) {
	svc := testService(t, writeCache(t, listenLocal(t)))
	dbPath := svc.Options().DBPath

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:  out,
		Trace:       true,
		TraceConfig: TraceOptions{Mode: "gre"},
	})
	if err == nil {
		t.Fatal("RunCSVScan accepted an invalid trace mode")
	}
	if !IsUsage(err) {
		t.Errorf("error = %v, want a usage error", err)
	}
	// 连 CSV 都不该被创建。
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("the CSV was created before the parameters were validated")
	}
	if _, statErr := os.Stat(dbPath); statErr == nil {
		t.Error("a database was created")
	}
}

// TestRunCSVScanHonorsLimit 验证 limit 只测前 N 个。
func TestRunCSVScanHonorsLimit(t *testing.T) {
	p1, p2, p3 := listenLocal(t), listenLocal(t), listenLocal(t)
	svc := testService(t, writeCache(t, p1, p2, p3))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath: out, Limit: 2, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}
	if result.Targets != 2 {
		t.Errorf("Targets = %d, want 2", result.Targets)
	}

	records := readCSVFile(t, out)
	if len(records) != 3 {
		t.Errorf("csv rows = %d, want 3 (header + 2)", len(records))
	}
	// 第三个端口不该出现。
	for _, record := range records[1:] {
		if strings.Contains(record[columnOf(t, "target")], itoaForTest(p3)) {
			t.Errorf("port %d was measured despite limit 2", p3)
		}
	}
}

// TestRunCSVScanReportsProgressAndTargets 验证进度与"当前目标"回调。
func TestRunCSVScanReportsProgressAndTargets(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var (
		targets  []string
		progress []ProgressEvent
	)
	if _, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath: out,
		Timeout:    2 * time.Second,
		OnTarget:   func(target string) { targets = append(targets, target) },
		Progress:   func(event ProgressEvent) { progress = append(progress, event) },
	}); err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}

	if len(targets) != 1 || !strings.HasPrefix(targets[0], "127.0.0.1:") {
		t.Errorf("OnTarget = %v, want one 127.0.0.1 target", targets)
	}
	if len(progress) == 0 {
		t.Fatal("no progress events")
	}
	last := progress[len(progress)-1]
	if last.Completed != last.Total {
		t.Errorf("last progress = %d/%d, want completed == total", last.Completed, last.Total)
	}
	if last.CurrentTarget == "" {
		t.Error("progress event has no current target; the UI could not show what is being measured")
	}
}

// TestRunCSVScanAppendKeepsExisting 验证追加模式不覆盖已有结果。
func TestRunCSVScanAppendKeepsExisting(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := svc.RunCSVScan(ctx, CSVScanOptions{OutputPath: out, Timeout: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath: out, Append: true, Timeout: 2 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	records := readCSVFile(t, out)
	if len(records) != 3 {
		t.Errorf("csv rows = %d, want 3 (one header + two runs)", len(records))
	}
	for i, record := range records {
		if i > 0 && record[0] == csvstore.Header[0] {
			t.Errorf("header repeated at row %d", i)
		}
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func readCSVFile(t *testing.T, path string) [][]string {
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

func columnOf(t *testing.T, name string) int {
	t.Helper()
	for i, header := range csvstore.Header {
		if header == name {
			return i
		}
	}
	t.Fatalf("column %q not found", name)
	return -1
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
