package cli

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// probeFixture 构造一份只包含本机监听地址的目标列表。
//
// 用真实的 all.json 形状（port 是数组、meta 含 colo），
// 但 IP 换成本机回环地址，这样探测结果是确定的（必然成功）。
func probeFixture(t *testing.T, ports []int) (jsonBody string, listeners []net.Listener) {
	t.Helper()

	// 一个 IP + 多个端口：正好覆盖"同一个 IP 展开成多个 Target"。
	for range ports {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		listeners = append(listeners, listener)
		go func(l net.Listener) {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}(listener)
	}

	actualPorts := make([]string, 0, len(listeners))
	for _, l := range listeners {
		addr := l.Addr().(*net.TCPAddr)
		actualPorts = append(actualPorts, itoaCLI(addr.Port))
	}

	body := `{
	  "generated_at": "2026-10-03T03:52:35.771190",
	  "list": {"ips": 1},
	  "data": [
	    {
	      "ip": "127.0.0.1",
	      "port": [` + strings.Join(actualPorts, ",") + `],
	      "meta": {
	        "country": "US", "city": "Chicago", "region": "Illinois",
	        "latitude": "41.85003", "longitude": "-87.65005",
	        "colo": {"iata": "ORD", "lat": 41.9786, "lon": -87.9048, "cca2": "US",
	                 "region": "North America", "city": "Chicago"},
	        "_port": 443,
	        "country_en": "United States"
	      }
	    }
	  ]
	}`
	return body, listeners
}

// itoaCLI 在测试里避免再引入 strconv。
func itoaCLI(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// writeProbeCacheFrom 用给定的 all.json 内容生成本地缓存文件，返回路径。
//
// 这样 probe 命令走的是"读取缓存"路径，测试不需要网络。
func writeProbeCacheFrom(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "all.json")
	if err := writeCacheFromJSON(t, path, body); err != nil {
		t.Fatalf("prepare cache: %v", err)
	}
	return path
}

func TestProbeHelpListsFlagsAndClassifications(t *testing.T) {
	code, stdout, stderr := runCLI("probe", "--help")

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeOK, stderr)
	}
	for _, want := range []string{
		"Flags:",
		"-workers",
		"-timeout",
		"-limit",
		"-json",
		"-cache",
		"-refresh",
		"-proxy",
		"Failure classification:",
		"timeout:",
		"connection_refused:",
		"canceled:",
		"invalid_target:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("probe help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

func TestProbeUnknownFlagIsUsageError(t *testing.T) {
	code, _, stderr := runCLI("probe", "--nope")
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "-nope") {
		t.Errorf("stderr = %q, want the offending flag", stderr)
	}
}

func TestProbeNegativeLimitIsUsageError(t *testing.T) {
	code, _, stderr := runCLI("probe", "--limit", "-1")
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeUsage, stderr)
	}
	if !strings.Contains(stderr, "--limit") {
		t.Errorf("stderr = %q, want a message about --limit", stderr)
	}
}

// TestProbeEndToEndAgainstLocalListeners 用真实本机监听验证 probe 的整条链路：
//
//	all.json 缓存 -> 目标展开 -> worker pool -> 真实 TCP 握手 -> 汇总输出
func TestProbeEndToEndAgainstLocalListeners(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0, 0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	code, stdout, stderr := runCLI(offlineProbeArgs(cachePath, "--workers", "4", "--timeout", "2s", "--quiet")...)

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}

	// 汇总必须如实反映三个目标全部成功。
	for _, want := range []string{
		"probed:      3 / 3 targets",
		"success:     3 (100.0%)",
		"failed:      0 (0.0%)",
		"latency (ms, 3 samples):",
		"min ",
		"p50 ",
		"p90 ",
		"p95 ",
		"max ",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\ngot:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "warning:") {
		t.Errorf("unexpected warning in stdout:\n%s", stdout)
	}
}

// TestProbeReportsFailuresByType 验证失败被分类汇总。
func TestProbeReportsFailuresByType(t *testing.T) {
	// 一个真实监听 + 一个确定关闭的端口。
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()

	// 拿一个空闲端口再关闭，制造 connection refused / timeout。
	idle, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	idlePort := idle.Addr().(*net.TCPAddr).Port
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}

	// 把关闭的端口追加到同一份 fixture 的目标里。
	body = strings.Replace(body, `"port": [`, `"port": [`+itoaCLI(idlePort)+`,`, 1)
	cachePath := writeProbeCacheFrom(t, body)

	code, stdout, stderr := runCLI(offlineProbeArgs(cachePath, "--workers", "2", "--timeout", "1s", "--quiet")...)

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "probed:      2 / 2 targets") {
		t.Errorf("stdout should report 2 targets:\n%s", stdout)
	}
	if !strings.Contains(stdout, "success:     1 (50.0%)") {
		t.Errorf("stdout should report a 50%% success rate:\n%s", stdout)
	}
	if !strings.Contains(stdout, "failures by type:") {
		t.Errorf("stdout should break failures down by type:\n%s", stdout)
	}
	// 分类必须是这两者之一，且必须是已定义分类。
	hasKnown := strings.Contains(stdout, "connection_refused:") || strings.Contains(stdout, "timeout:")
	if !hasKnown {
		t.Errorf("stdout should classify the failure:\n%s", stdout)
	}
}

// TestProbeJSONOutput 验证 --json 输出可被机器解析，且字段齐备。
func TestProbeJSONOutput(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	code, stdout, stderr := runCLI(offlineProbeArgs(cachePath, "--workers", "2", "--timeout", "2s", "--json", "--quiet")...)

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("json lines = %d, want 2\ngot:\n%s", len(lines), stdout)
	}

	seenTargets := make(map[string]bool, 2)
	for _, line := range lines {
		var record struct {
			SchemaVersion int     `json:"schema_version"`
			ClientVersion string  `json:"client_version"`
			TargetID      string  `json:"target_id"`
			IP            string  `json:"ip"`
			Port          int     `json:"port"`
			Success       bool    `json:"success"`
			LatencyMS     float64 `json:"latency_ms"`
			ErrorType     string  `json:"error_type"`
			Timestamp     string  `json:"timestamp"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid json line %q: %v", line, err)
		}

		if record.SchemaVersion != version.SchemaVersion {
			t.Errorf("schema_version = %d, want %d", record.SchemaVersion, version.SchemaVersion)
		}
		if record.ClientVersion != version.Version {
			t.Errorf("client_version = %q, want %q", record.ClientVersion, version.Version)
		}
		if !record.Success {
			t.Errorf("success = false for a live listener: %+v", record)
		}
		if record.ErrorType != "" {
			t.Errorf("error_type = %q, want empty on success", record.ErrorType)
		}
		if record.IP != "127.0.0.1" {
			t.Errorf("ip = %q, want 127.0.0.1", record.IP)
		}
		if record.Port == 0 {
			t.Error("port = 0, want the real port")
		}
		if record.LatencyMS < 0 {
			t.Errorf("latency_ms = %v, want >= 0", record.LatencyMS)
		}
		if _, err := time.Parse(time.RFC3339Nano, record.Timestamp); err != nil {
			t.Errorf("timestamp %q is not RFC3339: %v", record.Timestamp, err)
		}
		// 每行必须自描述：不能只有数字没有目标。
		if record.TargetID == "" {
			t.Error("target_id is empty")
		}
		if seenTargets[record.TargetID] {
			t.Errorf("duplicate target_id %q", record.TargetID)
		}
		seenTargets[record.TargetID] = true
	}
}

// TestProbeLimitSamplesTargets 验证 --limit 只测前 N 个目标。
func TestProbeLimitSamplesTargets(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0, 0, 0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	code, stdout, stderr := runCLI(offlineProbeArgs(cachePath, "--workers", "2", "--timeout", "2s", "--limit", "2", "--quiet")...)

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "probed:      2 / 2 targets") {
		t.Errorf("stdout should report 2 probed targets:\n%s", stdout)
	}

	// 不限制时应测全部 4 个。
	_, all, _ := runCLI(offlineProbeArgs(cachePath, "--workers", "2", "--timeout", "2s", "--quiet")...)
	if !strings.Contains(all, "probed:      4 / 4 targets") {
		t.Errorf("stdout should probe all 4 targets without --limit:\n%s", all)
	}
}

// TestProbeProgressOnStderrAndSummaryOnStdout 验证输出流分离：
// 进度走 stderr，结果与汇总走 stdout，便于脚本使用。
func TestProbeProgressOnStderrAndSummaryOnStdout(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	code, stdout, stderr := runCLI(offlineProbeArgs(cachePath, "--timeout", "2s")...)

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "source:") || !strings.Contains(stderr, "concurrency:") {
		t.Errorf("stderr should carry the header, got:\n%s", stderr)
	}
	if strings.Contains(stdout, "concurrency:") {
		t.Errorf("stdout should not carry the header:\n%s", stdout)
	}
	if !strings.Contains(stdout, "probed:") {
		t.Errorf("stdout should carry the summary:\n%s", stdout)
	}
}

// TestProbeVerboseListsEachTarget 验证 --verbose 才逐条输出。
func TestProbeVerboseListsEachTarget(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	_, _, quietErr := runCLI(offlineProbeArgs(cachePath, "--workers", "4", "--timeout", "2s", "--quiet")...)
	if strings.Contains(quietErr, " 127.0.0.1:") {
		t.Errorf("per-target lines must not appear without --verbose:\n%s", quietErr)
	}

	_, _, verboseErr := runCLI(offlineProbeArgs(cachePath, "--workers", "4", "--timeout", "2s", "--verbose")...)
	if !strings.Contains(verboseErr, "127.0.0.1:") || !strings.Contains(verboseErr, "ok") {
		t.Errorf("--verbose should list each target:\n%s", verboseErr)
	}
}

// TestProbeEmptyTargetListIsError 验证"没有目标可测"会明确报错，
// 而不是静默输出一份 0 目标的成功报告。
func TestProbeEmptyTargetListIsError(t *testing.T) {
	// 这里不能用 writeProbeCacheFrom：它刻意拒绝"解析出 0 个目标"的夹具
	// （那通常意味着夹具写错了）。本测试正好需要一份合法的空列表。
	empty := `{"generated_at":"2026-10-03T00:00:00","list":{"ips":0},"data":[]}`
	cachePath := writeRawCacheFromJSON(t, empty)

	code, stdout, stderr := runCLI(offlineProbeArgs(cachePath, "--quiet")...)
	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeError, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "no targets to probe") {
		t.Errorf("stderr = %q, want a clear message", stderr)
	}
}

// TestProbeInvalidSourceURLIsRuntimeError 验证配置错误属于运行期错误。
func TestProbeInvalidSourceURLIsRuntimeError(t *testing.T) {
	code, _, stderr := runCLI("probe", "--url", "://bad", "--quiet")
	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeError, stderr)
	}
	if !strings.Contains(stderr, "invalid source url") {
		t.Errorf("stderr = %q, want invalid url message", stderr)
	}
}

// ---------------------------------------------------------------------------
// 纯函数测试
// ---------------------------------------------------------------------------

func TestPercentile(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	cases := map[float64]float64{
		0.0:  1,
		0.5:  5.5,
		0.9:  9.1,
		0.95: 9.55,
		1.0:  10,
	}
	for q, want := range cases {
		got := percentile(sorted, q)
		// 线性插值会产生浮点误差（例如 9.549999999999999），
		// 因此用容差比较，而不是精确相等。
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("percentile(q=%v) = %v, want %v", q, got, want)
		}
	}

	// 边界：空、单元素、越界 q。
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
	if got := percentile([]float64{7}, 0.9); got != 7 {
		t.Errorf("percentile(single) = %v, want 7", got)
	}
	if got := percentile(sorted, -1); got != 1 {
		t.Errorf("percentile(-1) = %v, want first element", got)
	}
	if got := percentile(sorted, 2); got != 10 {
		t.Errorf("percentile(2) = %v, want last element", got)
	}
}

func TestSummarizeLatencies(t *testing.T) {
	if got := summarizeLatencies(nil); got.Count != 0 {
		t.Errorf("empty summary = %+v, want zero value", got)
	}

	got := summarizeLatencies([]float64{50, 10, 30, 20, 40})
	if got.Count != 5 {
		t.Errorf("Count = %d, want 5", got.Count)
	}
	if got.Min != 10 || got.Max != 50 {
		t.Errorf("Min/Max = %v/%v, want 10/50", got.Min, got.Max)
	}
	if got.P50 != 30 {
		t.Errorf("P50 = %v, want 30", got.P50)
	}
	if got.Avg != 30 {
		t.Errorf("Avg = %v, want 30", got.Avg)
	}
	// 分位数必须单调不减。
	if !(got.Min <= got.P50 && got.P50 <= got.P90 && got.P90 <= got.P95 && got.P95 <= got.Max) {
		t.Errorf("percentiles are not monotonic: %+v", got)
	}
}

func TestFormatErrorCounts(t *testing.T) {
	counts := map[probe.ErrorType]int{
		probe.ErrorTypeTimeout:             10,
		probe.ErrorTypeConnectionRefused:   5,
		probe.ErrorTypeCanceled:            1,
		probe.ErrorTypeOther:               1,
		probe.ErrorTypeNetworkUnreachable:  1,
		probe.ErrorTypeConnectionReset:     1,
		probe.ErrorTypePermissionDenied:    1,
		probe.ErrorTypeNoRoute:             1,
		probe.ErrorTypeAddressNotAvailable: 1,
		probe.ErrorTypeInvalidTarget:       1,
	}

	lines := formatErrorCounts(counts, false)
	if len(lines) == 0 {
		t.Fatal("no lines")
	}
	// 第一条必须是数量最多的那一类。
	if !strings.Contains(lines[0], "timeout:") || !strings.Contains(lines[0], "10") {
		t.Errorf("first line = %q, want the most frequent type", lines[0])
	}
	// 非 verbose 时必须折叠其余项。
	if !strings.Contains(strings.Join(lines, "\n"), "... others:") {
		t.Errorf("non-verbose output should collapse the tail:\n%s", strings.Join(lines, "\n"))
	}

	// verbose 时必须全部列出。
	verboseLines := formatErrorCounts(counts, true)
	if strings.Contains(strings.Join(verboseLines, "\n"), "... others:") {
		t.Errorf("verbose output should list everything:\n%s", strings.Join(verboseLines, "\n"))
	}
	if len(verboseLines) != len(counts) {
		t.Errorf("verbose lines = %d, want %d", len(verboseLines), len(counts))
	}

	if got := formatErrorCounts(nil, false); len(got) != 0 {
		t.Errorf("empty counts = %v, want empty", got)
	}
}

func TestDurationFlagDistinguishesUnset(t *testing.T) {
	var f durationFlag
	if f.set {
		t.Error("zero value should be unset")
	}
	if f.String() != "" {
		t.Errorf("String() = %q, want empty when unset", f.String())
	}

	if err := f.Set("1500ms"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !f.set {
		t.Error("set = false after Set")
	}
	if f.String() != "1.5s" {
		t.Errorf("String() = %q, want 1.5s", f.String())
	}

	if err := f.Set("not-a-duration"); err == nil {
		t.Error("Set accepted an invalid duration")
	}
}
