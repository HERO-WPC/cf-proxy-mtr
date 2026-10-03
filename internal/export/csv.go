package export

import (
	"bufio"
	"compress/gzip"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnmap"
)

// csvHeader 是 CSV 的列顺序。
//
// 顺序按"看表格的人是怎么读的"排：先认出是哪条目标、
// 什么时候测的、从哪儿测的，再看结果，最后才是逐跳细节。
//
// 改名/加列要谨慎：CSV 一旦被人拿去做了透视表，
// 改列名就等于破坏他们的表。因此新增列一律**追加在末尾**。
var csvHeader = []string{
	"schema_version",
	"kind",
	"client_version",
	"target_id",
	"ip",
	"port",
	"timestamp_utc",
	"session_id",
	"collector_id",
	"collector_country",
	"collector_province",
	"collector_city",
	"collector_isp",
	"collector_asn",
	"target_country",
	"target_cca2",
	"target_region",
	"target_city",
	"colo_iata",
	// 测量结果
	"success",
	"latency_ms",
	"error_type",
	"error_message",
	// 线路跟踪
	"trace_engine",
	"trace_hop_count",
	"trace_responded_hops",
	"trace_timeout_hops",
	"trace_as_path",
	"trace_hops",
}

// CSVWriter 把导出行写成扁平 CSV。
//
// 为什么单独一个类型而不是复用 Writer：CSV 需要表头，
// 而且每条记录是"一行固定列"，与 JSONL 的嵌套结构不是一回事。
// 硬把两者塞进一个抽象只会让两边都难懂。
type CSVWriter struct {
	csv    *csv.Writer
	header bool
}

// NewCSVWriter 创建 CSV 写出器。
func NewCSVWriter(w io.Writer) *CSVWriter {
	return &CSVWriter{csv: csv.NewWriter(w)}
}

// WriteRow 写一行。
//
// 表头在第一次写入时输出：只在真有数据时才写表头，
// 可以避免"空文件带表头"让人以为导出成功但内容丢了。
func (w *CSVWriter) WriteRow(row *Row) error {
	if !w.header {
		if err := w.csv.Write(csvHeader); err != nil {
			return fmt.Errorf("write csv header: %w", err)
		}
		w.header = true
	}

	if err := w.csv.Write(csvRecord(*row)); err != nil {
		return fmt.Errorf("write csv row: %w", err)
	}
	return nil
}

// Flush 刷出缓冲。
func (w *CSVWriter) Flush() error {
	w.csv.Flush()
	return w.csv.Error()
}

// csvRecord 把一行导出数据摊平成字符串切片。
//
// 缺失的字段写**空字符串**而不是 "0" 或 "false"：
// 表格里 0 与"没测到"是完全不同的意思，用空值区分它们。
func csvRecord(row Row) []string {
	record := []string{
		strconv.Itoa(row.SchemaVersion),
		string(row.Kind),
		row.ClientVersion,
		row.TargetID,
		row.IP,
		strconv.Itoa(row.Port),
		row.TimestampUTC,
		row.SessionID,
		row.CollectorID,
	}

	// 采集者地区
	if row.Collector != nil {
		record = append(record,
			row.Collector.Country,
			row.Collector.Province,
			row.Collector.City,
			row.Collector.ISP,
			row.Collector.ASN,
		)
	} else {
		record = append(record, "", "", "", "", "")
	}

	// 目标元数据
	if meta := row.TargetMeta; meta != nil {
		record = append(record,
			meta.Country,
			meta.CCA2,
			meta.Region,
			meta.City,
			meta.Colo.IATA,
		)
	} else {
		record = append(record, "", "", "", "", "")
	}

	// 测量结果
	if m := row.Measurement; m != nil {
		record = append(record,
			strconv.FormatBool(m.Success),
			formatLatency(m.LatencyMS),
			m.ErrorType,
			m.ErrorMessage,
		)
	} else {
		record = append(record, "", "", "", "")
	}

	// 线路跟踪
	if t := row.Trace; t != nil {
		// 超时跳 = 总跳数 - 有回应的跳数。分开两列而不只给一个总数：
		// "路径很长"与"路径上很多路由器不回 ICMP"是两回事。
		timeouts := t.HopCount - t.RespondedHops
		if timeouts < 0 {
			timeouts = 0
		}
		record = append(record,
			t.Engine,
			strconv.Itoa(t.HopCount),
			strconv.Itoa(t.RespondedHops),
			strconv.Itoa(timeouts),
			formatASPath(t.Hops),
			formatHops(t.Hops),
		)
	} else {
		record = append(record, "", "", "", "", "", "")
	}

	return record
}

// formatLatency 格式化延迟。
//
// 未测到（<=0）写空：0ms 与"没测到"必须能区分。
func formatLatency(ms float64) string {
	if ms <= 0 {
		return ""
	}
	return strconv.FormatFloat(ms, 'f', 3, 64)
}

// formatHops 把逐跳压成 "TTL:IP:线路" 用 ";" 连接的字符串。
//
// 为什么放进一个单元格而不是每个跳一行：一条记录的跳数是 10~30，
// 每个跳一行会让 CSV 膨胀成"同一目标重复几十次"，
// 反而没法做透视表。需要逐跳细看时用 JSONL。
func formatHops(hops []Hop) string {
	if len(hops) == 0 {
		return ""
	}

	parts := make([]string, 0, len(hops))
	for _, hop := range hops {
		// 超时跳没有 IP，用 "*" 明确表示"这里没回应"，
		// 而不是留空（留空会被当成"没有这一跳"）。
		ip := hop.IP
		if strings.TrimSpace(ip) == "" {
			ip = "*"
		}

		entry := strconv.Itoa(hop.TTL) + ":" + ip
		if label := asnmap.ShortLabel(hop.ASN); label != "" {
			entry += ":" + label
		}
		parts = append(parts, entry)
	}
	return strings.Join(parts, ";")
}

// formatASPath 把路径去重后压成 "AS4134 > AS4809" 形式。
//
// 用的是**线路名称**而不是编号：这正是这份 CSV 的用处——
// 一眼看出回程走的是 163 还是 CN2。
// 同时保留编号在括号里，便于核对。
func formatASPath(hops []Hop) string {
	seen := make(map[string]bool, len(hops))
	labels := make([]string, 0, len(hops))

	for _, hop := range hops {
		asn := asnmap.Normalize(hop.ASN)
		if asn == "" || seen[asn] {
			continue
		}
		seen[asn] = true

		if name := asnmap.Name(asn); name != "" {
			labels = append(labels, name+"("+asn+")")
		} else {
			labels = append(labels, asn)
		}
	}
	return strings.Join(labels, " > ")
}

// ---------------------------------------------------------------------------
// CSV 输出流
// ---------------------------------------------------------------------------

// CSVOutput 把 CSV 写出器套上缓冲与（可选）压缩。
//
// 与 Encoder 分开是因为两者的收尾顺序不同（CSV 要先 Flush 再关压缩流），
// 混在一起容易把顺序写反。
type CSVOutput struct {
	// Writer 是写 CSV 的地方。
	Writer *CSVWriter

	finish func() error
}

// NewCSVOutput 创建 CSV 输出流。
func NewCSVOutput(format Format, w io.Writer) (*CSVOutput, error) {
	if !format.Available() || !format.IsCSV() {
		return nil, fmt.Errorf("export: %s is not a CSV format", format)
	}
	if w == nil {
		return nil, fmt.Errorf("export: nil output writer")
	}

	buffered := bufio.NewWriterSize(w, 64*1024)

	if !format.IsCompressed() {
		writer := NewCSVWriter(buffered)
		return &CSVOutput{
			Writer: writer,
			finish: func() error {
				if err := writer.Flush(); err != nil {
					return err
				}
				return buffered.Flush()
			},
		}, nil
	}

	compressor := gzip.NewWriter(buffered)
	// 与 JSONL 导出一致：ModTime 置零，保证同一份数据导出两次字节相同。
	compressor.ModTime = time.Unix(0, 0).UTC()
	compressor.Name = ""
	compressor.Comment = ""

	writer := NewCSVWriter(compressor)
	return &CSVOutput{
		Writer: writer,
		finish: func() error {
			// 顺序很重要：先把 CSV 缓冲写进压缩流，
			// 再结束压缩流（写 gzip 尾部），最后刷出 bufio。
			if err := writer.Flush(); err != nil {
				return err
			}
			if err := compressor.Close(); err != nil {
				return fmt.Errorf("close gzip stream: %w", err)
			}
			return buffered.Flush()
		},
	}, nil
}

// Close 收尾（幂等）。
func (o *CSVOutput) Close() error {
	if o == nil || o.finish == nil {
		return nil
	}
	finish := o.finish
	o.finish = nil
	return finish()
}
