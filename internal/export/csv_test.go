package export

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"io"
	"strings"
	"testing"
	"time"
)

// sampleTraceRow 构造一条带逐跳的跟踪记录。
func sampleTraceRow() Row {
	return Row{
		SchemaVersion: 1,
		Kind:          KindTrace,
		ClientVersion: "0.1.0",
		TargetID:      "1.1.1.1:443",
		IP:            "1.1.1.1",
		Port:          443,
		TimestampUTC:  "2026-10-03T10:00:00Z",
		SessionID:     "20260101T000000Z-00000000",
		CollectorID:   "c-00000000",
		Collector: &Region{
			Country: "CN", Province: "Sample Province", City: "Sample City",
			ISP: "China Mobile", ASN: "AS9808",
		},
		Trace: &Trace{
			Success:       true,
			Engine:        "nexttrace",
			HopCount:      4,
			RespondedHops: 3,
			Hops: []Hop{
				{TTL: 1, IP: "192.168.1.1"},
				{TTL: 2, IP: "203.0.113.4", ASN: "AS4134"},
				{TTL: 3, IP: "203.0.113.5", ASN: "AS4809"},
				{TTL: 4, Timeout: true},
			},
		},
	}
}

// TestCSVHasHeaderAndOneRowPerRecord 验证 CSV 表头与行数。
func TestCSVHasHeaderAndOneRowPerRecord(t *testing.T) {
	var buf bytes.Buffer

	writer := NewCSVWriter(&buf)
	row := sampleTraceRow()
	if err := writer.WriteRow(&row); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	records, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("rows = %d, want 2 (header + data)", len(records))
	}

	// 表头必须与 csvHeader 完全一致（列顺序是文件契约的一部分）。
	if len(records[0]) != len(csvHeader) {
		t.Fatalf("header has %d columns, want %d", len(records[0]), len(csvHeader))
	}
	for i, want := range csvHeader {
		if records[0][i] != want {
			t.Errorf("header[%d] = %q, want %q", i, records[0][i], want)
		}
	}

	// 数据行的列数必须与表头一致，否则 Excel 会错位。
	if len(records[1]) != len(csvHeader) {
		t.Errorf("data row has %d columns, want %d", len(records[1]), len(csvHeader))
	}
}

// TestCSVCarriesRouteNames 验证 CSV 里带上了线路名称。
//
// 这是这份 CSV 的主要用途：一眼看出回程走的是 163 还是 CN2。
func TestCSVCarriesRouteNames(t *testing.T) {
	var buf bytes.Buffer
	writer := NewCSVWriter(&buf)
	row := sampleTraceRow()
	if err := writer.WriteRow(&row); err != nil {
		t.Fatal(err)
	}
	_ = writer.Flush()

	records, _ := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	data := records[1]

	index := func(name string) int {
		for i, h := range csvHeader {
			if h == name {
				return i
			}
		}
		t.Fatalf("column %q not found", name)
		return -1
	}

	path := data[index("trace_as_path")]
	if !strings.Contains(path, "163") || !strings.Contains(path, "CN2") {
		t.Errorf("trace_as_path = %q, want it to name 163 and CN2", path)
	}
	// 编号也要保留，便于核对。
	if !strings.Contains(path, "AS4134") || !strings.Contains(path, "AS4809") {
		t.Errorf("trace_as_path = %q, want it to keep the AS numbers", path)
	}

	hops := data[index("trace_hops")]
	if !strings.Contains(hops, "203.0.113.4") {
		t.Errorf("trace_hops = %q, missing a hop IP", hops)
	}
	// 超时跳必须明确标成 "*"，而不是留空——
	// 留空会被读成"没有这一跳"。
	if !strings.Contains(hops, "4:*") {
		t.Errorf("trace_hops = %q, want the timeout hop marked as *", hops)
	}
}

// TestCSVTimeoutHopsCounted 验证超时跳数被正确计算。
func TestCSVTimeoutHopsCounted(t *testing.T) {
	var buf bytes.Buffer
	writer := NewCSVWriter(&buf)
	row := sampleTraceRow()
	if err := writer.WriteRow(&row); err != nil {
		t.Fatal(err)
	}
	_ = writer.Flush()

	records, _ := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	data := records[1]

	for i, h := range csvHeader {
		switch h {
		case "trace_hop_count":
			if data[i] != "4" {
				t.Errorf("hop_count = %q, want 4", data[i])
			}
		case "trace_responded_hops":
			if data[i] != "3" {
				t.Errorf("responded_hops = %q, want 3", data[i])
			}
		case "trace_timeout_hops":
			if data[i] != "1" {
				t.Errorf("timeout_hops = %q, want 1", data[i])
			}
		}
	}
}

// TestCSVMeasurementLatencyEmptyWhenUnmeasured 验证"没测到"与 0ms 可区分。
//
// 表格里 0 与"没测到"是完全不同的意思，必须能分开。
func TestCSVMeasurementLatencyEmptyWhenUnmeasured(t *testing.T) {
	row := Row{
		SchemaVersion: 1,
		Kind:          KindMeasurement,
		TargetID:      "1.1.1.1:443",
		IP:            "1.1.1.1",
		Port:          443,
		Measurement: &Measurement{
			Success:   false,
			LatencyMS: 0,
			ErrorType: "timeout",
		},
	}

	var buf bytes.Buffer
	writer := NewCSVWriter(&buf)
	if err := writer.WriteRow(&row); err != nil {
		t.Fatal(err)
	}
	_ = writer.Flush()

	records, _ := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	data := records[1]

	for i, h := range csvHeader {
		switch h {
		case "latency_ms":
			if data[i] != "" {
				t.Errorf("latency_ms = %q, want empty for an unmeasured latency", data[i])
			}
		case "success":
			if data[i] != "false" {
				t.Errorf("success = %q, want false", data[i])
			}
		case "error_type":
			if data[i] != "timeout" {
				t.Errorf("error_type = %q, want timeout", data[i])
			}
		}
	}
}

// TestCSVQuotesSpecialCharacters 验证逗号、引号、换行被正确转义。
//
// 错误信息里经常带逗号与引号（`dial tcp 1.1.1.1:443: ...`），
// 转义错了整个文件都会错位。
func TestCSVQuotesSpecialCharacters(t *testing.T) {
	row := Row{
		SchemaVersion: 1,
		Kind:          KindMeasurement,
		TargetID:      "1.1.1.1:443",
		IP:            "1.1.1.1",
		Port:          443,
		Measurement: &Measurement{
			Success:      false,
			ErrorType:    "other",
			ErrorMessage: `bad "thing", with a comma` + "\nand a newline",
		},
	}

	var buf bytes.Buffer
	writer := NewCSVWriter(&buf)
	if err := writer.WriteRow(&row); err != nil {
		t.Fatal(err)
	}
	_ = writer.Flush()

	// 必须能被标准 CSV 解析器读回，且内容一致。
	records, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("rows = %d, want 2 (the newline must not create a new record)", len(records))
	}

	for i, h := range csvHeader {
		if h == "error_message" {
			got := records[1][i]
			if !strings.Contains(got, `bad "thing", with a comma`) {
				t.Errorf("error_message = %q, want the original text preserved", got)
			}
		}
	}
}

// TestCSVGzIsDeterministic 验证 gzip CSV 导出是确定性的。
//
// 与 JSONL 同样的要求：同一份数据导出两次字节必须相同，
// 否则"重新导出并比对哈希"这种校验方式失效。
func TestCSVGzIsDeterministic(t *testing.T) {
	render := func() []byte {
		var buf bytes.Buffer
		out, err := NewCSVOutput(FormatCSVGz, &buf)
		if err != nil {
			t.Fatalf("NewCSVOutput: %v", err)
		}
		row := sampleTraceRow()
		if err := out.Writer.WriteRow(&row); err != nil {
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	first := render()
	// 隔一段时间再导出：如果实现把当前时间写进了 gzip 头，这里就会不同。
	time.Sleep(20 * time.Millisecond)
	second := render()

	if !bytes.Equal(first, second) {
		t.Errorf("two exports of the same data differ (%d vs %d bytes); "+
			"the gzip header timestamp is probably not zeroed", len(first), len(second))
	}
}

// TestCSVGzRoundTrip 验证 gzip CSV 能解回可读的 CSV。
func TestCSVGzRoundTrip(t *testing.T) {
	var compressed bytes.Buffer

	out, err := NewCSVOutput(FormatCSVGz, &compressed)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		row := sampleTraceRow()
		if err := out.Writer.WriteRow(&row); err != nil {
			t.Fatal(err)
		}
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := gzip.NewReader(&compressed)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	records, err := csv.NewReader(strings.NewReader(string(plain))).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(records) != 11 {
		t.Errorf("rows = %d, want 11 (header + 10)", len(records))
	}
}

// TestCSVWriterWithoutRowsWritesNothing 验证没有数据时不写表头。
//
// 空文件带表头会让人以为"导出成功了但内容丢了"；
// 什么都不写则明确表示没有数据。
func TestCSVWriterWritesNoHeaderWithoutRows(t *testing.T) {
	var buf bytes.Buffer
	writer := NewCSVWriter(&buf)
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want empty when nothing was written", buf.String())
	}
}
