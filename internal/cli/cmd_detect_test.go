package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/identity"
)

func TestDetectHelpListsFlagsAndPrivacyNote(t *testing.T) {
	code, stdout, stderr := runCLI("detect", "--help")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	for _, want := range []string{
		"Flags:",
		"-geoip-endpoint",
		"-source",
		"-write",
		"-identity",
		"-timeout",
		"Detection sources:",
		"local",
		"geoip",
		// 隐私说明必须出现在帮助里，而不是只藏在源码注释里。
		"暴露你的公网 IP",
		// 手动优先的原则也要写清楚。
		"手动填写的值永远优先",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("detect help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

// TestDetectOfflineOnlyDoesNotContactExternalServices 验证 --source local
// 时只跑本地源，不会去连 geo-IP 端点。
func TestDetectOfflineOnlyDoesNotContactExternalServices(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"countryCode":"US","query":"1.2.3.4"}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	code, stdout, stderr := runCLI("detect",
		"--identity", filepath.Join(dir, "collector.json"),
		"--geoip-endpoint", server.URL,
		"--source", "local",
		"--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	if hits != 0 {
		t.Errorf("geo-IP endpoint was contacted %d times, want 0 with --source local", hits)
	}
	if !strings.Contains(stdout, "collector profile:") {
		t.Errorf("stdout should carry the profile:\n%s", stdout)
	}
	if !strings.Contains(stdout, "not saved") {
		t.Errorf("detect without --write must not claim to have saved:\n%s", stdout)
	}
}

// TestDetectWritesProfileAndPreservesManualValues 验证写入语义：
//
//   - 检测到的字段写入标识文件；
//   - 文件里已有的手动值在不被覆盖的字段上保持不变。
func TestDetectWritesProfileAndPreservesManualValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","regionName":"California",
		                       "city":"San Jose","isp":"SomeCloud","as":"AS13335 Cloudflare",
		                       "query":"1.2.3.4"}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	identityPath := filepath.Join(dir, "collector.json")

	// 预置一份"用户手填"的标识文件：ISP 与城市是他自己写的。
	existing, err := identity.Load(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	existing.Profile = identity.Profile{
		Country: "CN", Province: "Zhejiang", City: "Hangzhou",
		ISP: "China Mobile", ASN: "AS9808", IPVersion: "ipv4",
	}
	if err := identity.Save(identityPath, existing); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("detect",
		"--identity", identityPath,
		"--geoip-endpoint", server.URL,
		"--source", "geoip",
		"--write",
		"--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "saved to:") {
		t.Errorf("stdout should confirm the write:\n%s", stdout)
	}

	// 已有值优先（它们是 manual），因此检测结果不该覆盖它们。
	updated, err := identity.Load(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.ISP != "China Mobile" || updated.Profile.ASN != "AS9808" {
		t.Errorf("manual ISP/ASN were overwritten: %+v", updated.Profile)
	}
	if updated.Profile.City != "Hangzhou" {
		t.Errorf("manual city was overwritten: %+v", updated.Profile)
	}
	// 匿名 ID 必须保持不变：否则历史数据会与这个节点断开关联。
	if updated.CollectorID != existing.CollectorID {
		t.Errorf("collector_id changed: %q -> %q", existing.CollectorID, updated.CollectorID)
	}
}

// TestDetectFillsEmptyFieldsFromDetection 验证空字段会被检测结果填上。
func TestDetectFillsEmptyFieldsFromDetection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","countryCode":"JP","regionName":"Tokyo",
		                       "city":"Tokyo","isp":"NTT","as":"AS4713 NTT",
		                       "query":"2001:db8::1"}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	identityPath := filepath.Join(dir, "collector.json")

	// 全新的标识文件：没有任何手填值。
	code, _, stderr := runCLI("detect",
		"--identity", identityPath,
		"--geoip-endpoint", server.URL,
		"--source", "geoip",
		"--write",
		"--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	updated, err := identity.Load(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.Country != "JP" || updated.Profile.City != "Tokyo" {
		t.Errorf("profile = %+v, want the detected JP/Tokyo", updated.Profile)
	}
	if updated.Profile.ASN != "AS4713" {
		t.Errorf("ASN = %q, want AS4713 (extracted from the org/as field)", updated.Profile.ASN)
	}
	if updated.Profile.IPVersion != "ipv6" {
		t.Errorf("IPVersion = %q, want ipv6 (inferred from the returned address)", updated.Profile.IPVersion)
	}
}

// TestDetectNeverWritesIdentifyingData 是隐私回归测试。
//
// 标识文件里只能有随机 ID 与粗粒度的地区/运营商。
// 检测结果（尤其是 geo-IP 响应）里的 IP、坐标等信息**不得**被写入。
func TestDetectNeverWritesIdentifyingData(t *testing.T) {
	// 响应里塞满各种可能的隐私字段，看它们会不会被写进文件。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
		  "status": "success",
		  "countryCode": "US", "regionName": "California", "city": "San Jose",
		  "isp": "SomeCloud", "as": "AS13335 Cloudflare",
		  "query": "203.0.113.55",
		  "lat": 37.3382, "lon": -121.8863,
		  "zip": "95113", "timezone": "America/Los_Angeles",
		  "hostname": "my-laptop.local",
		  "mac": "aa:bb:cc:dd:ee:ff",
		  "local_ip": "192.168.1.42",
		  "device_id": "deadbeef"
		}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	identityPath := filepath.Join(dir, "collector.json")

	if code, _, stderr := runCLI("detect",
		"--identity", identityPath,
		"--geoip-endpoint", server.URL,
		"--source", "geoip",
		"--write",
		"--quiet"); code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	blob, err := readFileString(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(blob)

	for _, forbidden := range []string{
		"203.0.113.55", // 公网 IP
		"37.3382",      // 纬度
		"-121.8863",    // 经度
		"95113",        // 邮编
		"my-laptop",    // 主机名
		"aa:bb:cc",     // MAC
		"192.168.1.42", // 内网 IP
		"deadbeef",     // 设备标识
		"mac", "hostname", "device_id", "local_ip", "lat\"", "lon\"",
	} {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			t.Errorf("identity file contains %q, which must never be persisted:\n%s", forbidden, blob)
		}
	}

	// 该有的还是要有。
	if !strings.Contains(blob, "US") || !strings.Contains(blob, "AS13335") {
		t.Errorf("identity file should contain the coarse profile:\n%s", blob)
	}
}

// TestDetectPrintsExposureBeforeRequesting 验证"谁会看到你的 IP"会被打印，
// 而且是在发起请求之前（因此输出到 stderr 的头部）。
func TestDetectPrintsExposureBeforeRequesting(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"countryCode":"US","query":"1.2.3.4"}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	code, stdout, stderr := runCLI("detect",
		"--identity", filepath.Join(dir, "collector.json"),
		"--geoip-endpoint", server.URL)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if hits == 0 {
		t.Error("the geo-IP endpoint was never contacted; the test is not exercising the path")
	}

	// 暴露声明必须在 stderr 的头部（用户先看到，再决定是否继续）。
	if !strings.Contains(stderr, "exposes your public IP") {
		t.Errorf("stderr should mark the exposing source:\n%s", stderr)
	}
	if !strings.Contains(stderr, server.URL) {
		t.Errorf("stderr should name the actual endpoint:\n%s", stderr)
	}
	if !strings.Contains(stderr, "[local]") {
		t.Errorf("stderr should mark the local source as not exposing:\n%s", stderr)
	}

	// 各源的结果与字段来源也必须可见。
	if !strings.Contains(stderr, "source results:") {
		t.Errorf("stderr should report per-source results:\n%s", stderr)
	}
	if !strings.Contains(stderr, "field sources:") {
		t.Errorf("stderr should report which source provided each field:\n%s", stderr)
	}

	// stdout 只放最终画像。
	if !strings.Contains(stdout, "collector profile:") || !strings.Contains(stdout, "country:    US") {
		t.Errorf("stdout should carry the resolved profile:\n%s", stdout)
	}
	if strings.Contains(stdout, "source results:") {
		t.Errorf("stdout should not carry the detection process:\n%s", stdout)
	}
}

func TestDetectUnknownSourceIsUsageError(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := runCLI("detect",
		"--identity", filepath.Join(dir, "collector.json"),
		"--source", "nope", "--quiet")

	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "nope") {
		t.Errorf("stderr = %q, want the unknown source named", stderr)
	}
	if !strings.Contains(stderr, "local") {
		t.Errorf("stderr = %q, want the available sources listed", stderr)
	}
}

// TestDetectJSONOutputIsParseable 验证 --json 的输出可被机器解析。
func TestDetectJSONOutputIsParseable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","countryCode":"SG","regionName":"Singapore",
		                       "city":"Singapore","isp":"DigitalOcean","as":"AS14061 DigitalOcean",
		                       "query":"1.2.3.4"}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	code, stdout, stderr := runCLI("detect",
		"--identity", filepath.Join(dir, "collector.json"),
		"--geoip-endpoint", server.URL,
		"--source", "geoip",
		"--json")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty in json mode", stderr)
	}

	var out struct {
		CollectorProfile struct {
			Country   string `json:"country"`
			City      string `json:"city"`
			ISP       string `json:"isp"`
			ASN       string `json:"asn"`
			IPVersion string `json:"ip_version"`
		} `json:"collector_profile"`
		FieldSources map[string]string `json:"field_sources"`
		Sources      []struct {
			Name           string            `json:"name"`
			ExposesLocalIP bool              `json:"exposes_local_ip"`
			Error          string            `json:"error"`
			Values         map[string]string `json:"values"`
		} `json:"sources"`
		ElapsedMS float64 `json:"elapsed_ms"`
		Written   bool    `json:"written"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("json output is not parseable: %v\n%s", err, stdout)
	}

	if out.CollectorProfile.Country != "SG" || out.CollectorProfile.City != "Singapore" {
		t.Errorf("profile = %+v, want SG/Singapore", out.CollectorProfile)
	}
	if out.CollectorProfile.ASN != "AS14061" {
		t.Errorf("ASN = %q, want AS14061", out.CollectorProfile.ASN)
	}
	if out.Written {
		t.Error("written = true without --write")
	}
	if len(out.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(out.Sources))
	}
	if !out.Sources[0].ExposesLocalIP {
		t.Error("sources[0].exposes_local_ip = false, want true for geoip")
	}
	if out.Sources[0].Error != "" {
		t.Errorf("sources[0].error = %q, want empty", out.Sources[0].Error)
	}
	if out.ElapsedMS < 0 {
		t.Errorf("elapsed_ms = %v, want >= 0", out.ElapsedMS)
	}
}

// TestDetectReportsSourceFailureWithoutFailingOverall 验证端点挂掉时
// 仍然能用本地源给出结果，并如实报告失败。
func TestDetectReportsSourceFailureWithoutFailingOverall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	dir := t.TempDir()
	code, stdout, stderr := runCLI("detect",
		"--identity", filepath.Join(dir, "collector.json"),
		"--geoip-endpoint", server.URL)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want 0 even when one source fails (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "FAILED") {
		t.Errorf("stderr should report the failing source:\n%s", stderr)
	}
	// 最终画像仍然要输出（本地源提供了 IP 版本）。
	if !strings.Contains(stdout, "collector profile:") {
		t.Errorf("stdout should still carry a profile:\n%s", stdout)
	}
	if !strings.Contains(stdout, "could not be detected") {
		t.Errorf("stdout should admit which fields are missing:\n%s", stdout)
	}
}

// TestDetectManualFlagsWinOverDetection 验证命令行手动值的优先级。
func TestDetectManualFlagsWinOverDetection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","city":"San Jose",
		                       "isp":"SomeCloud","as":"AS13335","query":"1.2.3.4"}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	identityPath := filepath.Join(dir, "collector.json")

	code, stdout, stderr := runCLI("detect",
		"--identity", identityPath,
		"--geoip-endpoint", server.URL,
		"--source", "geoip",
		"--country", "cn",
		"--isp", "China Mobile",
		"--asn", "9808",
		"--write",
		"--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "country:    CN") {
		t.Errorf("manual country should win:\n%s", stdout)
	}
	if !strings.Contains(stdout, "isp:        China Mobile") {
		t.Errorf("manual ISP should win:\n%s", stdout)
	}

	updated, err := identity.Load(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.Country != "CN" || updated.Profile.ISP != "China Mobile" {
		t.Errorf("profile = %+v, want the manual values preserved", updated.Profile)
	}
	// ASN 手动给的是 "9808"，必须被归一化成 AS9808。
	if updated.Profile.ASN != "AS9808" {
		t.Errorf("ASN = %q, want normalized AS9808", updated.Profile.ASN)
	}
	// 未被手填的字段仍然由检测填充。
	if updated.Profile.City != "San Jose" {
		t.Errorf("City = %q, want the detected San Jose", updated.Profile.City)
	}
}
