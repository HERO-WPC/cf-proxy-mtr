package service

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
)

// 本文件覆盖**跟踪阶段**（runTracePhase）。
//
// 为什么单独一个文件：这段代码一度是零覆盖——所有提到 Trace: true 的
// 测试都因为"模式非法"或"引擎不存在"在进入跟踪阶段之前就返回了。
// 于是"跟踪是串行的"这个缺陷（一次 traceroute 十几秒，串行 10 个
// 就是几分钟）没有任何测试能发现。
//
// 用什么替身：真实 nexttrace 在测试里既慢又依赖本机权限
// （TCP/UDP 模式需要管理员 + WinDivert），因此用引擎自带的
// RunCommand 注入点返回固定 JSON，跳过版本探测。

// fakeTracePayload 构造一段 nexttrace 形状的输出。
//
// ⚠️ 字段形状必须与真实引擎一致，尤其是 ASN 的位置：
// 它在 `Geo.asnumber` 里，而且是**字符串** `"AS4134"`，
// 不是顶层整数 `"ASN":4134`。写错这一处不会报错，
// 只会让 `as_path` 悄悄变成空——本项目里真的这样错过一次。
//
// 用 AS4134 / AS4809 而不是文档保留段（AS64500）：
// asnmap 只认识真实存在的骨干网，用未知 ASN 测不出线路名。
func fakeTracePayload() string {
	hop := func(ttl int, ip string, asn string) string {
		return fmt.Sprintf(`{
			"Success": true,
			"Address": {"IP": %q, "Zone": ""},
			"Hostname": "",
			"TTL": %d,
			"RTT": 1200000,
			"Error": null,
			"Geo": {
				"ip": "", "asnumber": %q, "country": "", "country_en": "",
				"prov": "", "prov_en": "", "city": "", "city_en": "",
				"district": "", "owner": "", "isp": "", "domain": "",
				"whois": "", "lat": 0, "lng": 0, "prefix": "", "router": null, "source": ""
			},
			"Lang": "cn",
			"MPLS": null
		}`, ip, ttl, asn)
	}

	// Hops 是"每个 TTL 一组探测"的嵌套数组，与真实输出一致。
	return fmt.Sprintf(`{"Hops":[[%s],[%s]]}`,
		hop(1, "203.0.113.4", "AS4134"),
		hop(2, "198.51.100.7", "AS4809"),
	)
}

// fakeTraceEngine 构造一个"从不真的启动进程"的引擎。
//
// perCallDelay 模拟一次 traceroute 的耗时：并发测试靠它把
// "并行 ≈ 1×delay"与"串行 ≈ N×delay"区分开。
func fakeTraceEngine(t *testing.T, perCallDelay time.Duration) (*trace.NextTraceEngine, *atomic.Int64) {
	t.Helper()

	var calls atomic.Int64
	payload := fakeTracePayload()

	engine, err := trace.NewNextTraceEngine(context.Background(), trace.EngineOptions{
		BinaryPath:       "fake-nexttrace",
		Mode:             trace.ModeICMP,
		SkipVersionCheck: true,
		RunCommand: func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			calls.Add(1)
			if perCallDelay > 0 {
				select {
				case <-time.After(perCallDelay):
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
			}
			return []byte(payload), nil, nil
		},
	})
	if err != nil {
		t.Fatalf("NewNextTraceEngine: %v", err)
	}
	return engine, &calls
}

// traceRowsIn 返回结果文件里带线路信息的行（有 hop_count 的那些）。
func traceRowsIn(t *testing.T, path string) [][]string {
	t.Helper()

	records := readCSVFile(t, path)
	hopIdx := columnOf(t, "hop_count")

	var out [][]string
	for _, record := range records[1:] {
		if record[hopIdx] != "" {
			out = append(out, record)
		}
	}
	return out
}

// TestRunCSVScanTracesSuccessfulTargetsOnly 验证只跟踪探测成功的目标。
func TestRunCSVScanTracesSuccessfulTargetsOnly(t *testing.T) {
	// 两个**不同端口**（同一个 IP:Port 会被去重成一个目标）。
	portA, portB := listenLocal(t), listenLocal(t)
	svc := testService(t, writeCache(t, portA, portB))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	engine, calls := fakeTraceEngine(t, 0)

	result, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:          out,
		Timeout:             2 * time.Second,
		Trace:               true,
		traceEngineOverride: engine,
		// 测试不该联网：假引擎已经给出 ASN，不需要前缀数据。
		NoASNPrefix: true,
	})
	if err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}

	if result.Succeeded != 2 {
		t.Fatalf("succeeded = %d, want 2 (both listeners accept)", result.Succeeded)
	}
	if result.Traced != result.Succeeded {
		t.Errorf("traced = %d, want %d (one trace per successful probe)", result.Traced, result.Succeeded)
	}
	if got := calls.Load(); got != int64(result.Succeeded) {
		t.Errorf("engine invoked %d time(s), want %d", got, result.Succeeded)
	}

	// 每个成功目标一行探测 + 一行跟踪。
	if result.RowsWritten != result.Probed+result.Traced {
		t.Errorf("rows = %d, want %d (probe rows + trace rows)",
			result.RowsWritten, result.Probed+result.Traced)
	}

	if traced := traceRowsIn(t, out); len(traced) != result.Succeeded {
		t.Errorf("trace rows in file = %d, want %d", len(traced), result.Succeeded)
	}
}

// TestRunCSVScanTracesInParallel 是跟踪阶段并发性的**回归测试**。
//
// 跟踪串行的代价比探测更大：一次 traceroute 十几秒，
// 串行 10 个目标就是近三分钟，而 `--trace-workers` 照样被打印出来。
//
// 与探测的并发测试用同样的手法：让每次跟踪恰好耗时 delay，
// 于是并行 ≈ delay、串行 ≈ N×delay，差距足够大，不会因抖动而偶发失败。
func TestRunCSVScanTracesInParallel(t *testing.T) {
	const (
		targets = 6
		delayMS = 250
	)

	ports := make([]int, 0, targets)
	for i := 0; i < targets; i++ {
		ports = append(ports, listenLocal(t))
	}
	svc := testService(t, writeCache(t, ports...))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	engine, _ := fakeTraceEngine(t, delayMS*time.Millisecond)

	start := time.Now()
	result, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:          out,
		Timeout:             2 * time.Second,
		Trace:               true,
		TraceConfig:         TraceOptions{Workers: targets},
		traceEngineOverride: engine,
		// 测试不该联网：假引擎已经给出 ASN，不需要前缀数据。
		NoASNPrefix: true,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}
	if result.Traced != targets {
		t.Fatalf("traced = %d, want %d", result.Traced, targets)
	}

	// 串行下限是 targets × delay；探测阶段只有几十毫秒，可忽略。
	serialFloor := time.Duration(targets) * delayMS * time.Millisecond
	if elapsed > serialFloor/2 {
		t.Errorf("tracing %d targets with %d workers took %s; serial would be ~%s, "+
			"so traces are not running concurrently",
			targets, targets, elapsed.Round(time.Millisecond), serialFloor)
	}
	t.Logf("%d 个目标 / %d 并发 / 每个 %dms：全程 %s（串行约 %s）",
		targets, targets, delayMS, elapsed.Round(time.Millisecond), serialFloor)
}

// TestRunCSVScanTraceRowsAreComplete 验证并发写出的跟踪行是完整的，
// 且线路名被正确解析出来。
func TestRunCSVScanTraceRowsAreComplete(t *testing.T) {
	const targets = 8

	ports := make([]int, 0, targets)
	for i := 0; i < targets; i++ {
		ports = append(ports, listenLocal(t))
	}
	svc := testService(t, writeCache(t, ports...))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	engine, _ := fakeTraceEngine(t, 0)

	result, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:          out,
		Timeout:             2 * time.Second,
		Trace:               true,
		TraceConfig:         TraceOptions{Workers: 4},
		traceEngineOverride: engine,
		// 测试不该联网：假引擎已经给出 ASN，不需要前缀数据。
		NoASNPrefix: true,
	})
	if err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}

	records := readCSVFile(t, out)

	// 每一行列数一致：并发下若共享状态没保护好，会写出残缺行。
	for i, record := range records {
		if len(record) != len(csvstore.Header) {
			t.Fatalf("row %d has %d columns, want %d", i, len(record), len(csvstore.Header))
		}
	}

	// 行数必须等于"表头 + 探测行 + 跟踪行"。
	if want := 1 + result.Probed + result.Traced; len(records) != want {
		t.Errorf("csv rows = %d, want %d (header + probes + traces)", len(records), want)
	}

	hopIdx := columnOf(t, "hop_count")
	asIdx := columnOf(t, "as_path")

	traced := traceRowsIn(t, out)
	if len(traced) != result.Traced {
		t.Fatalf("trace rows = %d, want %d", len(traced), result.Traced)
	}
	for _, record := range traced {
		if record[hopIdx] != "2" {
			t.Errorf("hop_count = %q, want 2 (the payload has two hops)", record[hopIdx])
		}
		// AS4134 -> 163、AS4809 -> CN2；解析链路断了这里就会是空。
		if record[asIdx] != "163 > CN2" {
			t.Errorf("as_path = %q, want \"163 > CN2\"; the ASN was not mapped to a route name",
				record[asIdx])
		}
	}

	// traced_ok 是在并发中累加的，累加错了这里会不一致。
	if result.TracedOK != len(traced) {
		t.Errorf("traced_ok = %d, but the file has %d trace rows", result.TracedOK, len(traced))
	}
}

// TestRunCSVScanTraceEngineFailureKeepsProbeRows 验证跟踪失败不影响已写的探测结果。
//
// 关于断言的取舍（这里踩过一次）：`NextTraceEngine.Trace` 在进程失败时
// 返回的是 `(result, nil)` —— 失败记在 `result.ErrorType` 里，**不是**
// Go error。因此这种失败：
//
//   - 会写一行 Success=false 的结果（带 error_type / error_message），
//     这是对的：跟踪失败本身就是一条数据；
//   - **不该**增加 `result.Errors`，那个计数专指"落盘失败"。
//
// 所以这里不断言 Errors，而是断言"探测结果完好 + 跟踪行带上了失败原因"。
func TestRunCSVScanTraceEngineFailureKeepsProbeRows(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	broken, err := trace.NewNextTraceEngine(context.Background(), trace.EngineOptions{
		BinaryPath:       "fake-nexttrace",
		Mode:             trace.ModeICMP,
		SkipVersionCheck: true,
		RunCommand: func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			return nil, []byte("engine exploded"), fmt.Errorf("exit status 1")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:          out,
		Timeout:             2 * time.Second,
		Trace:               true,
		traceEngineOverride: broken,
		NoASNPrefix:         true,
	})
	if err != nil {
		t.Fatalf("RunCSVScan returned an error even though probing succeeded: %v", err)
	}

	// 探测结果必须完好——跟踪坏掉不该让整轮白跑。
	if result.Succeeded != 1 {
		t.Errorf("succeeded = %d, want 1", result.Succeeded)
	}
	if result.RowsWritten < 2 {
		t.Errorf("rows = %d, want at least 2 (the probe row plus the failed trace row)", result.RowsWritten)
	}
	if result.TracedOK != 0 {
		t.Errorf("traced_ok = %d, want 0 for a broken engine", result.TracedOK)
	}

	// 失败原因必须落到结果文件里，而不是被静默丢掉。
	records := readCSVFile(t, out)
	successIdx := columnOf(t, "success")
	errTypeIdx := columnOf(t, "error_type")

	var failedTraceRows int
	for _, record := range records[1:] {
		if record[successIdx] == "false" && record[errTypeIdx] != "" {
			failedTraceRows++
		}
	}
	if failedTraceRows == 0 {
		t.Error("no row recorded the trace failure; the reason was silently dropped")
	}
}

// TestRunCSVScanTraceRespectsInterrupt 验证跟踪阶段中断后不继续跑完剩下的目标。
func TestRunCSVScanTraceRespectsInterrupt(t *testing.T) {
	const targets = 10

	ports := make([]int, 0, targets)
	for i := 0; i < targets; i++ {
		ports = append(ports, listenLocal(t))
	}
	svc := testService(t, writeCache(t, ports...))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine, calls := fakeTraceEngine(t, 60*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.RunCSVScan(ctx, CSVScanOptions{
			OutputPath:          out,
			Timeout:             2 * time.Second,
			Trace:               true,
			TraceConfig:         TraceOptions{Workers: 1},
			traceEngineOverride: engine,
			// 测试不该联网：假引擎已经给出 ASN，不需要前缀数据。
			NoASNPrefix: true,
		})
	}()

	// 等跟踪阶段真的开始，然后取消。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && calls.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("scan did not return after cancel")
	}

	// 不该把 10 个目标全部跟踪完。
	if got := calls.Load(); got >= targets {
		t.Errorf("engine was called %d times out of %d targets despite the cancel", got, targets)
	}

	// 已经写下的行必须完好。
	for i, record := range readCSVFile(t, out) {
		if len(record) != len(csvstore.Header) {
			t.Errorf("row %d is incomplete (%d columns)", i, len(record))
		}
	}
}

// TestCSVColumnIndexesAreStable 是一道便宜的护栏：列的**顺序**是契约。
//
// 上面几个测试都按列名取下标，因此如果有人重排 Header，
// 它们会一起去读错的列。这里把顺序钉住，让那种改动必须是有意的。
func TestCSVColumnIndexesAreStable(t *testing.T) {
	want := []string{
		"timestamp_utc", "target", "ip", "port", "success", "latency_ms",
		"error_type", "error_message", "hop_count", "as_path", "hops",
		"client_version",
	}
	if len(csvstore.Header) != len(want) {
		t.Fatalf("header has %d columns, want %d: %v", len(csvstore.Header), len(want), csvstore.Header)
	}
	for i, name := range want {
		if csvstore.Header[i] != name {
			t.Errorf("header[%d] = %q, want %q", i, csvstore.Header[i], name)
		}
	}
}
