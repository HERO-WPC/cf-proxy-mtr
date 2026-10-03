package service

import (
	"context"
	"encoding/csv"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
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

// TestRunCSVScanKeepsResultsOnInterrupt 验证中断不丢**已经写盘**的行。
//
// 这个测试在探测改成并发之后重写过，原因值得记下来：
//
// 旧写法用 OnTarget 计数来掐取消时机（"测到第 2 个目标就取消"）。
// 串行时成立；并发之后 OnTarget 会为多个目标几乎同时触发，
// 于是取消发生在**任何探测完成之前**，文件里一行都没有，
// 测试报告"结果丢了"——而实际上产品行为是对的，
// 是测试的前提（"OnTarget 逐个串行触发"）失效了。
//
// 现在直接验证真正要守的性质：**取消不许让已经落盘的行消失，
// 也不许把文件写坏**。做法是先让若干探测立刻成功、落盘，
// 等到文件里确实有行之后再取消，然后比对前后行数。
func TestRunCSVScanKeepsResultsOnInterrupt(t *testing.T) {
	// 8 个目标；下面的拨号器让前 3 个立刻成功，其余的挂到超时。
	ports := make([]int, 0, 8)
	for i := 0; i < 8; i++ {
		ports = append(ports, listenLocal(t))
	}
	svc := testService(t, writeCache(t, ports...))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := probe.DefaultConfig()
	cfg.Workers = 4
	cfg.Timeout = 5 * time.Second // 足够长，保证取消时确实有在途探测
	cfg.Dialer = succeedThenBlockDialer(3)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.RunCSVScan(ctx, CSVScanOptions{
			OutputPath:    out,
			probeOverride: &cfg,
		})
	}()

	// 等文件里出现至少一行数据，然后取消。
	var rowsAtCancel int
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if count, err := countDataRows(out); err == nil && count >= 1 {
			rowsAtCancel = count
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rowsAtCancel == 0 {
		t.Fatal("no row was written before the interrupt; the test premise did not hold")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("scan did not return after cancel")
	}

	// 取消之后直接读文件（不依赖任何 Close 语义）。
	records := readCSVFile(t, out)
	if len(records) < 2 {
		t.Fatalf("csv rows = %d; results written before the interrupt were lost", len(records))
	}

	// 表头必须仍然只有一行，且是第一行。
	if records[0][0] != csvstore.Header[0] {
		t.Errorf("first row is not the header: %v", records[0])
	}

	// 落盘的行只能增加，绝不能因为取消而减少——这是本测试的核心断言。
	after := len(records) - 1
	if after < rowsAtCancel {
		t.Errorf("data rows went from %d to %d after the interrupt; flushed rows must survive",
			rowsAtCancel, after)
	}

	// 每一行都必须是完整的：取消若发生在写一行的中途，
	// 会留下列数不对的残行，那种文件用 pandas 读会报错。
	for i, record := range records {
		if len(record) != len(csvstore.Header) {
			t.Errorf("row %d has %d columns, want %d (a partial write survived the interrupt)",
				i, len(record), len(csvstore.Header))
		}
	}
	t.Logf("取消时已落盘 %d 行，取消后 %d 行（只增不减）", rowsAtCancel, after)
}

// countDataRows 数 CSV 里的数据行数（不含表头）。
func countDataRows(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1 // 中断时可能有半行，不要为此报错
	records, err := reader.ReadAll()
	if err != nil {
		return 0, err
	}
	if len(records) <= 1 {
		return 0, nil
	}
	return len(records) - 1, nil
}

// succeedThenBlockDialer 让前 successes 次拨号立刻成功，其余挂到超时。
//
// 用途是把"已完成的测量"与"在途的测量"同时制造出来，
// 这样中断测试才能验证"前者保留、后者丢弃"。
func succeedThenBlockDialer(successes int) probe.DialContextFunc {
	var calls atomic.Int64
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if calls.Add(1) <= int64(successes) {
			// net.Pipe 给出一对真实连接：客户端拿到一端，
			// 另一端立刻关掉。探测只做握手/关闭，因此够用。
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		}
		<-ctx.Done()
		return nil, errors.New("dialer: blocked until deadline")
	}
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
