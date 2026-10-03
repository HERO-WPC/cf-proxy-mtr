package export

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/privacy"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// ---------------------------------------------------------------------------
// 行构造与隐私过滤
// ---------------------------------------------------------------------------

// publicTarget 返回一个可公开的测试测量行。
func publicMeasurement() storage.ExportMeasurement {
	lat, lng := 37.3382, -121.8863
	colLat, colLng := 37.36, -121.92
	return storage.ExportMeasurement{
		ID:           1,
		TargetID:     "45.63.67.144:2053",
		CollectorPK:  7,
		CollectorAID: "c-0123456789abcdef0123456789abcdef",
		SessionID:    "20260101T000000Z-00000000",
		Timestamp:    testTimestamp,

		Success:   true,
		LatencyMS: 42.5,

		IP:        "45.63.67.144",
		Port:      2053,
		Country:   "US",
		CCA2:      "US",
		Region:    "California",
		City:      "San Jose",
		CountryEN: "United States",

		ColoIATA: "SJC",
		ColoCCA2: "US",
		ColoCity: "San Jose",

		Latitude:      &lat,
		Longitude:     &lng,
		ColoLatitude:  &colLat,
		ColoLongitude: &colLng,

		CollectorCountry:   "CN",
		CollectorProvince:  "Zhejiang",
		CollectorCity:      "Hangzhou",
		CollectorISP:       "China Mobile",
		CollectorASN:       "AS9808",
		CollectorIPVersion: "ipv4",

		SchemaVersion: version.SchemaVersion,
		ClientVersion: "0.1.0",
	}
}

// testTimestamp 是所有测试共用的固定时间。
//
// 固定而不是 time.Now()：导出结果里的时间字段要能被逐字断言，
// 用当前时间会让断言变成"差不多"。
var testTimestamp = time.Date(2026, 10, 3, 13, 0, 24, 0, time.UTC)

// TestMeasurementRowExcludesPrivateTargets 是隐私约束的核心测试。
func TestMeasurementRowExcludesPrivateTargets(t *testing.T) {
	privateTargets := []string{
		"192.168.1.1", "10.0.0.5", "172.16.3.4", "127.0.0.1",
		"100.64.0.1", "203.0.113.7", "169.254.1.1", "fe80::1", "fc00::1",
	}

	for _, ip := range privateTargets {
		t.Run(ip, func(t *testing.T) {
			item := publicMeasurement()
			item.IP = ip
			item.TargetID = ip + ":2053"

			row, stats, ok := MeasurementRow(item, "0.1.0")
			if ok {
				t.Fatalf("private target %s was exported: %+v", ip, row)
			}
			if row != nil {
				t.Errorf("row = %+v, want nil for a private target", row)
			}
			if stats.SkippedPrivateTarget != 1 {
				t.Errorf("SkippedPrivateTarget = %d, want 1", stats.SkippedPrivateTarget)
			}
		})
	}
}

func TestMeasurementRowRejectsInvalidTargets(t *testing.T) {
	for _, ip := range []string{"", "nonsense", "1.2.3.4:443", "999.999.999.999"} {
		t.Run(ip, func(t *testing.T) {
			item := publicMeasurement()
			item.IP = ip

			row, stats, ok := MeasurementRow(item, "0.1.0")
			if ok {
				t.Fatalf("invalid target %q was exported", ip)
			}
			if row != nil {
				t.Error("row should be nil")
			}
			if stats.SkippedInvalidTarget != 1 {
				t.Errorf("SkippedInvalidTarget = %d, want 1", stats.SkippedInvalidTarget)
			}
		})
	}
}

// TestEmptyCollectorRegionIsOmitted 验证画像为空时不留一个空对象。
//
// 采集者画像没跑过 detect 时所有字段都是空的；此时 JSON 里出现
// "collector": {} 是纯噪声，看起来还像"我们收集了但值是空"。
func TestEmptyCollectorRegionIsOmitted(t *testing.T) {
	item := publicMeasurement()
	item.CollectorCountry = ""
	item.CollectorProvince = ""
	item.CollectorCity = ""
	item.CollectorISP = ""
	item.CollectorASN = ""
	item.CollectorIPVersion = ""

	row, _, ok := MeasurementRow(item, "0.1.0")
	if !ok {
		t.Fatal("rejected")
	}
	if row.Collector != nil {
		t.Errorf("Collector = %+v, want nil when every field is empty", row.Collector)
	}

	blob, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), `"collector":{}`) {
		t.Errorf("empty collector object leaked into the output: %s", blob)
	}

	// 但 collector_id 必须保留：没有它数据无法归属到节点。
	if row.CollectorID == "" {
		t.Error("CollectorID is empty; attribution would be lost")
	}
	if !strings.Contains(string(blob), `"collector_id"`) {
		t.Errorf("collector_id missing from the output: %s", blob)
	}
}

// TestPartialCollectorRegionIsKept 验证只填了一部分时仍然输出。
func TestPartialCollectorRegionIsKept(t *testing.T) {
	item := publicMeasurement()
	item.CollectorCountry = ""
	item.CollectorProvince = ""
	item.CollectorCity = ""
	item.CollectorISP = ""
	item.CollectorIPVersion = ""
	// 只留 ASN。
	item.CollectorASN = "AS9808"

	row, _, ok := MeasurementRow(item, "0.1.0")
	if !ok {
		t.Fatal("rejected")
	}
	if row.Collector == nil {
		t.Fatal("Collector = nil, want it kept when at least one field is set")
	}
	if row.Collector.ASN != "AS9808" {
		t.Errorf("ASN = %q, want AS9808", row.Collector.ASN)
	}
}

func TestMeasurementRowShape(t *testing.T) {
	row, stats, ok := MeasurementRow(publicMeasurement(), "0.1.0")
	if !ok {
		t.Fatal("public measurement was rejected")
	}

	if row.SchemaVersion != version.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", row.SchemaVersion, version.SchemaVersion)
	}
	if row.Kind != KindMeasurement {
		t.Errorf("Kind = %q, want %q", row.Kind, KindMeasurement)
	}
	if row.ClientVersion != "0.1.0" {
		t.Errorf("ClientVersion = %q, want 0.1.0", row.ClientVersion)
	}
	if row.TargetID != "45.63.67.144:2053" || row.IP != "45.63.67.144" || row.Port != 2053 {
		t.Errorf("target = %s / %s / %d", row.TargetID, row.IP, row.Port)
	}
	if row.TimestampUTC != "2026-10-03T13:00:24Z" {
		t.Errorf("TimestampUTC = %q", row.TimestampUTC)
	}
	if row.CollectorID == "" {
		t.Error("CollectorID is empty; without it data cannot be attributed to a node")
	}
	if row.Collector == nil || row.Collector.ASN != "AS9808" || row.Collector.ISP != "China Mobile" {
		t.Errorf("collector = %+v", row.Collector)
	}
	if row.TargetMeta == nil || row.TargetMeta.City != "San Jose" {
		t.Errorf("target_meta = %+v", row.TargetMeta)
	}
	if row.TargetMeta.Colo == nil || row.TargetMeta.Colo.IATA != "SJC" {
		t.Errorf("colo = %+v", row.TargetMeta.Colo)
	}
	if row.Measurement == nil || !row.Measurement.Success || row.Measurement.LatencyMS != 42.5 {
		t.Errorf("measurement = %+v", row.Measurement)
	}
	if row.Measurement.ErrorType != "" {
		t.Error("successful measurement carries an error type")
	}
	if stats.MeasurementsExported != 1 {
		t.Errorf("MeasurementsExported = %d, want 1", stats.MeasurementsExported)
	}
	if row.Trace != nil {
		t.Error("measurement row carries a trace")
	}
}

func TestMeasurementRowFailureKeepsClassification(t *testing.T) {
	item := publicMeasurement()
	item.Success = false
	item.ErrorType = "timeout"
	item.ErrorMessage = "dial tcp 45.63.67.144:2053: i/o timeout"

	row, _, ok := MeasurementRow(item, "0.1.0")
	if !ok {
		t.Fatal("failed measurement was rejected")
	}
	if row.Measurement.Success {
		t.Error("Success = true for a failed measurement")
	}
	// 分类必须保留：不同的失败是**不同的线路现象**。
	if row.Measurement.ErrorType != "timeout" {
		t.Errorf("ErrorType = %q, want timeout", row.Measurement.ErrorType)
	}
	if row.Measurement.ErrorMessage == "" {
		t.Error("ErrorMessage is empty")
	}
}

// TestTraceRowRedactsPrivateHops 是跟踪行的核心隐私测试。
//
// 关键点：内网跳要被**替换**而不是丢弃——跳的位置与顺序本身
// 就是线路信息。
func TestTraceRowRedactsPrivateHops(t *testing.T) {
	item := storage.ExportTrace{
		ID:            5,
		TargetID:      "45.63.67.144:443",
		CollectorPK:   7,
		CollectorAID:  "c-abc",
		SessionID:     "20260101T000000Z-00000000",
		Timestamp:     testTimestamp,
		Engine:        "nexttrace",
		EngineVersion: "1.7.3",
		Mode:          "tcp",
		Protocol:      "tcp",
		Port:          443,
		Success:       true,
		DurationMS:    1234.5,
		HopCount:      4,
		TraceJSON: `[
			{"TTL":1,"IP":"192.168.1.1","RTTMS":[1.2],"Timeout":false},
			{"TTL":2,"IP":"10.0.0.1","RTTMS":[2.4],"Timeout":false},
			{"TTL":3,"IP":"","Timeout":true},
			{"TTL":4,"IP":"45.63.67.144","RTTMS":[42.5],"Timeout":false,"ASN":"AS20473","ASOrganization":"Vultr"}
		]`,
		IP:      "45.63.67.144",
		Country: "US",
		City:    "San Jose",
	}

	row, stats, ok := TraceRow(item, "0.1.0")
	if !ok {
		t.Fatal("public trace was rejected")
	}

	if row.Trace == nil {
		t.Fatal("trace payload is nil")
	}
	if len(row.Trace.Hops) != 4 {
		t.Fatalf("hops = %d, want 4 (private hops must be redacted, not dropped)", len(row.Trace.Hops))
	}

	// 内网地址被替换。
	if row.Trace.Hops[0].IP != privacy.RedactedIPv4 {
		t.Errorf("hop 1 IP = %q, want %q", row.Trace.Hops[0].IP, privacy.RedactedIPv4)
	}
	if row.Trace.Hops[1].IP != privacy.RedactedIPv4 {
		t.Errorf("hop 2 IP = %q, want %q", row.Trace.Hops[1].IP, privacy.RedactedIPv4)
	}
	// 公网地址保持原样。
	if row.Trace.Hops[3].IP != "45.63.67.144" {
		t.Errorf("hop 4 IP = %q, want the public address untouched", row.Trace.Hops[3].IP)
	}
	// 超时跳保持没有地址。
	if row.Trace.Hops[2].IP != "" || !row.Trace.Hops[2].Timeout {
		t.Errorf("hop 3 = %+v, want a timeout hop with no address", row.Trace.Hops[2])
	}
	// TTL 顺序必须保持（替换不能打乱路径）。
	for i, hop := range row.Trace.Hops {
		if hop.TTL != i+1 {
			t.Errorf("hops[%d].TTL = %d, want %d", i, hop.TTL, i+1)
		}
	}

	if stats.RedactedHops != 2 {
		t.Errorf("RedactedHops = %d, want 2", stats.RedactedHops)
	}
	if row.Trace.LocalFiltered == false {
		t.Error("LocalFiltered = false, want true (the trace went through redaction)")
	}
	// HopCount 是引擎报的总跳数，RespondedHops 是本包算出的有回复跳数。
	if row.Trace.HopCount != 4 {
		t.Errorf("HopCount = %d, want 4", row.Trace.HopCount)
	}
	if row.Trace.RespondedHops != 3 {
		t.Errorf("RespondedHops = %d, want 3 (the timeout hop does not count)", row.Trace.RespondedHops)
	}
	// 保留全部 RTT 样本，而不是只留最小值（抖动是线路质量的一部分）。
	if len(row.Trace.Hops[3].RTTMS) != 1 {
		t.Errorf("hop 4 RTT samples = %v", row.Trace.Hops[3].RTTMS)
	}
	if row.Trace.Hops[3].ASN != "AS20473" {
		t.Errorf("hop 4 ASN = %q, want AS20473", row.Trace.Hops[3].ASN)
	}
}

// TestTraceRowNoLeakOfPrivateAddresses 是一道"整体扫描"式的防线。
//
// 把含各类内网地址的轨迹序列化后逐字符串检查：任何原始内网地址
// 出现在输出里都算失败。这比逐字段断言更能抓住"新加字段忘了过滤"。
func TestTraceRowNoLeakOfPrivateAddresses(t *testing.T) {
	private := []string{
		"192.168.1.1", "10.0.0.1", "172.16.0.1", "127.0.0.1",
		"100.64.9.9", "169.254.10.10", "203.0.113.99",
		"fe80::abcd", "fc00::1234", "2001:db8::99",
	}

	// 构造一条"每一跳都是内网地址"的轨迹。
	var hops []string
	for i, ip := range private {
		hops = append(hops, `{"TTL":`+strconv.Itoa(i+1)+`,"IP":"`+ip+`","RTTMS":[1.5],"Timeout":false}`)
	}

	item := storage.ExportTrace{
		TargetID:     "45.63.67.144:443",
		CollectorAID: "c-abc",
		Timestamp:    testTimestamp,
		Success:      true,
		Port:         443,
		HopCount:     len(private),
		TraceJSON:    "[" + strings.Join(hops, ",") + "]",
		IP:           "45.63.67.144",
	}

	row, _, ok := TraceRow(item, "0.1.0")
	if !ok {
		t.Fatal("public trace was rejected")
	}

	blob, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)

	for _, ip := range private {
		if strings.Contains(text, ip) {
			t.Errorf("exported row leaks private address %q:\n%s", ip, text)
		}
	}
	// 占位符必须出现，否则说明整跳被静默删掉了。
	if !strings.Contains(text, privacy.RedactedIPv4) {
		t.Error("no IPv4 redaction placeholder in the output")
	}
	if !strings.Contains(text, privacy.RedactedIPv6) {
		t.Error("no IPv6 redaction placeholder in the output")
	}
}

// TestTraceRowHandlesUnparsableHops 验证坏数据不会让整行丢掉。
func TestTraceRowHandlesUnparsableHops(t *testing.T) {
	item := storage.ExportTrace{
		TargetID:     "45.63.67.144:443",
		CollectorAID: "c-abc",
		Timestamp:    testTimestamp,
		Success:      true,
		Port:         443,
		HopCount:     5,
		TraceJSON:    "{not json",
		IP:           "45.63.67.144",
	}

	row, _, ok := TraceRow(item, "0.1.0")
	if !ok {
		t.Fatal("a row with unparsable hops was dropped entirely; the measurement itself is still valid")
	}
	if len(row.Trace.Hops) != 0 {
		t.Errorf("hops = %d, want 0", len(row.Trace.Hops))
	}
	// 必须说明原因，而不是静默给出空路径。
	if row.Trace.ErrorType == "" {
		t.Error("ErrorType is empty; the unparsable path must be reported")
	}
}

// ---------------------------------------------------------------------------
// 错误信息清洗
// ---------------------------------------------------------------------------

// TestSanitizeErrorMessage 验证错误信息里的 IP / 路径 / 用户名被替换。
func TestSanitizeErrorMessage(t *testing.T) {
	cases := []struct {
		name           string
		in             string
		mustNotContain []string
	}{
		{
			name: "public ip in message",
			in:   "dial tcp 45.63.67.144:443: connect: connection refused",
			// 公网 IP 也要替换：错误信息里的地址可能来自本机侧，
			// 而且保留它对分析没有价值（目标 IP 另有字段）。
			mustNotContain: []string{"45.63.67.144"},
		},
		{
			name:           "private ip in message",
			in:             "dial tcp 192.168.1.1:80: i/o timeout",
			mustNotContain: []string{"192.168.1.1"},
		},
		{
			name:           "unix path",
			in:             "open /Users/alice/.config/nexttrace: permission denied",
			mustNotContain: []string{"/Users/alice"},
		},
		{
			name:           "windows path",
			in:             `CreateProcess C:\Users\bob\bin\nexttrace.exe failed`,
			mustNotContain: []string{`C:\Users\bob`},
		},
		{
			name:           "ipv6 address",
			in:             "dial tcp [2001:db8::1]:443: network is unreachable",
			mustNotContain: []string{"2001:db8::1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := sanitizeErrorMessage(tc.in)
			if !changed {
				t.Errorf("changed = false for %q", tc.in)
			}
			for _, forbidden := range tc.mustNotContain {
				if strings.Contains(got, forbidden) {
					t.Errorf("sanitized message still contains %q: %q", forbidden, got)
				}
			}
			if got == "" {
				t.Error("sanitized message is empty; classification context would be lost")
			}
		})
	}
}

func TestSanitizeErrorMessageKeepsUsefulText(t *testing.T) {
	// 没有隐私内容的消息应当保持可读（分类信息不能丢）。
	in := "connection refused"
	got, changed := sanitizeErrorMessage(in)
	if changed {
		t.Errorf("changed = true for a message with nothing to redact: %q", got)
	}
	if got != in {
		t.Errorf("got %q, want %q", got, in)
	}

	// 时间字符串不能被误判成 IPv6。
	//
	// 这是宽松正则的典型风险：把 "12:30:45" 换成 "[addr]"
	// 会丢掉诊断信息，比不过滤更糟（用户看不到重试时间）。
	for _, text := range []string{
		"retry after 12:30:45",
		"started at 2026-10-03T13:00:24Z",
		"elapsed 00:00:01.500",
		"nexttrace --tcp --port 443 --json 1.1.1.1",
	} {
		got, _ := sanitizeErrorMessage(text)
		if strings.Contains(got, "[addr]") && !strings.Contains(text, "1.1.1.1") {
			t.Errorf("sanitizeErrorMessage(%q) = %q, want no address redaction", text, got)
		}
	}

	// 空消息保持空。
	if got, changed := sanitizeErrorMessage(""); got != "" || changed {
		t.Errorf("empty message = %q/%v, want \"\"/false", got, changed)
	}
}

// TestSanitizeErrorMessageHandlesBracketedIPv6 验证 [addr]:port 形式。
//
// Go 的错误信息在 IPv6 上习惯写 "[2001:db8::1]:443"，
// 括号与端口都不该阻止地址被识别。
func TestSanitizeErrorMessageHandlesBracketedIPv6(t *testing.T) {
	cases := []string{
		"dial tcp [2001:db8::1]:443: network is unreachable",
		"dial tcp [2606:4700:4700::1111]:443: i/o timeout",
		"dial udp [fe80::1%eth0]:53: no route to host",
		"connect to ::1 failed",
		"address 2001:db8::1 is unreachable",
	}
	for _, in := range cases {
		got, _ := sanitizeErrorMessage(in)
		if got == in {
			t.Errorf("sanitizeErrorMessage(%q) left the message untouched", in)
		}
		for _, forbidden := range []string{"2001:db8", "2606:4700", "fe80", "::1"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("sanitized %q still contains %q: %q", in, forbidden, got)
			}
		}
	}
}

func TestSanitizeErrorMessageStripsControlCharsAndTruncates(t *testing.T) {
	// ANSI 颜色码必须去掉（否则 JSONL 难以处理）。
	colored := "\x1b[31mtimeout\x1b[0m"
	got, _ := sanitizeErrorMessage(colored)
	if strings.ContainsAny(got, "\x1b") {
		t.Errorf("control characters survived: %q", got)
	}
	if !strings.Contains(got, "timeout") {
		t.Errorf("got %q, want the text preserved without escapes", got)
	}

	// 超长消息必须被截断（没有上限的公开字段是滥用面）。
	long := strings.Repeat("x", 5000)
	got, _ = sanitizeErrorMessage(long)
	if len(got) > 320 {
		t.Errorf("length = %d, want truncation to about 300", len(got))
	}
}

// ---------------------------------------------------------------------------
// JSONL 写出
// ---------------------------------------------------------------------------

func TestWriterProducesOneLinePerRow(t *testing.T) {
	var buffer bytes.Buffer
	writer := NewWriter(&buffer)

	for i := 0; i < 3; i++ {
		row, _, ok := MeasurementRow(publicMeasurement(), "0.1.0")
		if !ok {
			t.Fatal("rejected")
		}
		if err := writer.Write(row); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if writer.Written() != 3 {
		t.Errorf("Written() = %d, want 3", writer.Written())
	}

	text := buffer.String()
	if !strings.HasSuffix(text, "\n") {
		t.Error("output does not end with a newline")
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	for i, line := range lines {
		var row Row
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
		if row.SchemaVersion == 0 {
			t.Errorf("line %d has no schema_version; every row must be self-describing", i)
		}
	}
}

func TestWriterDoesNotEscapeHTML(t *testing.T) {
	var buffer bytes.Buffer
	writer := NewWriter(&buffer)

	row := baseRow(KindMeasurement, "0.1.0")
	row.Collector = &Region{ISP: "AT&T <Communications>"}
	if err := writer.Write(&row); err != nil {
		t.Fatal(err)
	}

	// 默认的 HTML 转义会把 & 与 < > 变成 \u0026 之类，可读性明显变差。
	if strings.Contains(buffer.String(), `\u0026`) || strings.Contains(buffer.String(), `\u003c`) {
		t.Errorf("HTML escaping is on: %s", buffer.String())
	}
	if !strings.Contains(buffer.String(), "AT&T <Communications>") {
		t.Errorf("ISP was mangled: %s", buffer.String())
	}
}

func TestWriterSkipsNilRow(t *testing.T) {
	var buffer bytes.Buffer
	writer := NewWriter(&buffer)
	if err := writer.Write(nil); err != nil {
		t.Errorf("Write(nil) = %v, want nil", err)
	}
	if writer.Written() != 0 {
		t.Errorf("Written() = %d, want 0", writer.Written())
	}
}

// ---------------------------------------------------------------------------
// 输出格式与压缩
// ---------------------------------------------------------------------------

func TestNormalizeFormat(t *testing.T) {
	cases := map[string]Format{
		"":         FormatJSONL,
		"jsonl":    FormatJSONL,
		"JSONL":    FormatJSONL,
		"ndjson":   FormatJSONL,
		" jsonl ":  FormatJSONL,
		"gz":       FormatJSONLGz,
		"gzip":     FormatJSONLGz,
		"jsonl.gz": FormatJSONLGz,
		".gz":      FormatJSONLGz,
		"zst":      FormatJSONLZst,
		"zstd":     FormatJSONLZst,
	}

	for in, want := range cases {
		got, err := NormalizeFormat(in)
		if err != nil {
			t.Errorf("NormalizeFormat(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeFormat(%q) = %q, want %q", in, got, want)
		}
	}

	if _, err := NormalizeFormat("parquet"); err == nil {
		t.Error("NormalizeFormat accepted an unknown format")
	}
}

// TestZstdIsHonestlyUnavailable 验证 zstd 的不支持是明确的。
func TestZstdIsHonestlyUnavailable(t *testing.T) {
	if FormatJSONLZst.Available() {
		t.Skip("zstd became available; update this test")
	}

	reason := FormatJSONLZst.UnavailableReason()
	if reason == "" {
		t.Fatal("UnavailableReason is empty for an unavailable format")
	}
	// 必须告诉用户"为什么"以及"该用什么"，而不是一句 not supported。
	if !strings.Contains(reason, "Go 1.26") {
		t.Errorf("reason = %q, want the actual cause", reason)
	}
	if !strings.Contains(reason, "jsonl.gz") {
		t.Errorf("reason = %q, want a suggested alternative", reason)
	}

	// 构造编码器时必须失败，而不是产出一个空文件。
	if _, err := NewEncoder(FormatJSONLZst, &bytes.Buffer{}); err == nil {
		t.Error("NewEncoder succeeded for zstd, which is not supported")
	}

	// 支持的格式必须在列表里。
	supported := SupportedFormats()
	if len(supported) != 2 {
		t.Errorf("SupportedFormats() = %v, want 2 formats", supported)
	}
}

// TestGzipRoundTrip 验证 gzip 输出可以被正确解回 JSONL。
func TestGzipRoundTrip(t *testing.T) {
	var compressed bytes.Buffer

	encoder, err := NewEncoder(FormatJSONLGz, &compressed)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}

	const rows = 25
	for i := 0; i < rows; i++ {
		row, _, ok := MeasurementRow(publicMeasurement(), "0.1.0")
		if !ok {
			t.Fatal("rejected")
		}
		if err := encoder.Writer.Write(row); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Close 必须被调用，否则 gzip 尾部与缓冲都会丢。
	if err := encoder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := DetectFormatFromBytes(compressed.Bytes()); got != FormatJSONLGz {
		t.Fatalf("output is not gzip (detected %q)", got)
	}

	reader, err := gzip.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(plain), "\n"), "\n")
	if len(lines) != rows {
		t.Fatalf("lines after decompression = %d, want %d", len(lines), rows)
	}
	for i, line := range lines {
		var row Row
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
	}
}

// TestGzipIsDeterministic 验证同一个输入导出两次得到相同字节。
//
// gzip 头部默认会写入当前时间，导致"重新导出并比对哈希"这种
// 校验方式失效。这个测试钉住我们把 ModTime 置零的行为。
func TestGzipIsDeterministic(t *testing.T) {
	render := func() []byte {
		var buffer bytes.Buffer
		encoder, err := NewEncoder(FormatJSONLGz, &buffer)
		if err != nil {
			t.Fatal(err)
		}
		row, _, _ := MeasurementRow(publicMeasurement(), "0.1.0")
		if err := encoder.Writer.Write(row); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}

	first := render()
	// 等一下，确保如果实现里用了 time.Now()，两次会不同。
	time.Sleep(1100 * time.Millisecond)
	second := render()

	if !bytes.Equal(first, second) {
		t.Error("gzip output is not deterministic; an identical export produced different bytes")
	}
}

func TestEncoderCloseIsIdempotent(t *testing.T) {
	var buffer bytes.Buffer
	encoder, err := NewEncoder(FormatJSONLGz, &buffer)
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// 调用方常常在 defer 与错误路径里都写一遍，必须允许。
	if err := encoder.Close(); err != nil {
		t.Errorf("second Close: %v, want nil", err)
	}

	// 同理，nil 接收者也不该 panic。
	var nilEncoder *Encoder
	if err := nilEncoder.Close(); err != nil {
		t.Errorf("nil Close: %v, want nil", err)
	}
}

func TestNewEncoderRejectsNilWriter(t *testing.T) {
	if _, err := NewEncoder(FormatJSONL, nil); err == nil {
		t.Error("NewEncoder accepted a nil writer")
	}
}

// TestPlainJSONLIsNotCompressed 验证未压缩输出保持可读。
func TestPlainJSONLIsNotCompressed(t *testing.T) {
	var buffer bytes.Buffer
	encoder, err := NewEncoder(FormatJSONL, &buffer)
	if err != nil {
		t.Fatal(err)
	}
	row, _, _ := MeasurementRow(publicMeasurement(), "0.1.0")
	if err := encoder.Writer.Write(row); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}

	if got := DetectFormatFromBytes(buffer.Bytes()); got != FormatJSONL {
		t.Errorf("format detected as %q, want plain jsonl", got)
	}
	if !strings.HasPrefix(buffer.String(), "{") {
		t.Errorf("output does not look like JSON: %q", buffer.String())
	}
}

func TestSuggestFilename(t *testing.T) {
	cases := []struct {
		format    Format
		session   string
		kind      string
		wantParts []string
	}{
		{FormatJSONLGz, "20260101T000000Z-00000000", "measurements",
			[]string{"cf-route-tester", "20260101T000000Z-00000000", "measurements", ".jsonl.gz"}},
		{FormatJSONL, "s1", "traces", []string{"cf-route-tester", "s1", "traces", ".jsonl"}},
		{FormatJSONL, "", "", []string{"cf-route-tester", ".jsonl"}},
	}

	for _, tc := range cases {
		got := SuggestFilename(tc.format, tc.session, tc.kind)
		for _, part := range tc.wantParts {
			if !strings.Contains(got, part) {
				t.Errorf("SuggestFilename(%q,%q,%q) = %q, want it to contain %q",
					tc.format, tc.session, tc.kind, got, part)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 过滤统计
// ---------------------------------------------------------------------------

func TestFilterStatsAggregation(t *testing.T) {
	var total FilterStats

	total.Add(FilterStats{MeasurementsTotal: 10, MeasurementsExported: 9, SkippedPrivateTarget: 1, RedactedHops: 3})
	total.Add(FilterStats{TracesTotal: 5, TracesExported: 5, RedactedHops: 2, SanitizedMessages: 1})

	if total.RowsExported() != 14 {
		t.Errorf("RowsExported() = %d, want 14", total.RowsExported())
	}
	if total.RowsSkipped() != 1 {
		t.Errorf("RowsSkipped() = %d, want 1", total.RowsSkipped())
	}
	if total.RedactedHops != 5 {
		t.Errorf("RedactedHops = %d, want 5", total.RedactedHops)
	}
	if total.SanitizedMessages != 1 {
		t.Errorf("SanitizedMessages = %d, want 1", total.SanitizedMessages)
	}
}

// TestPrivacyNoteIsHonest 验证过滤结果必须被明确报出。
//
// 静默丢弃数据的导出是不可信的：用户无法判断产物是否完整。
func TestPrivacyNoteIsHonest(t *testing.T) {
	clean := FilterStats{MeasurementsExported: 5}
	if note := clean.PrivacyNote(); !strings.Contains(note, "nothing was filtered") {
		t.Errorf("note = %q, want it to say nothing was filtered", note)
	}

	dirty := FilterStats{SkippedPrivateTarget: 3, RedactedHops: 7}
	note := dirty.PrivacyNote()
	if !strings.Contains(note, "3 row(s)") {
		t.Errorf("note = %q, want the dropped row count", note)
	}
	if !strings.Contains(note, "7 private hop") {
		t.Errorf("note = %q, want the redacted hop count", note)
	}
	// 必须说明占位符的名字，用户才知道输出里的 private-v4 是什么。
	if !strings.Contains(note, privacy.RedactedIPv4) {
		t.Errorf("note = %q, want the placeholder name mentioned", note)
	}
}
