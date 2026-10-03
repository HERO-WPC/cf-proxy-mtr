package export

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"time"
)

// Format 是导出文件的格式。
type Format string

const (
	// FormatJSONL 是纯文本 JSONL（一行一个 JSON 对象）。
	FormatJSONL Format = "jsonl"

	// FormatJSONLGz 是 gzip 压缩的 JSONL。
	FormatJSONLGz Format = "jsonl.gz"

	// FormatJSONLZst 是 zstd 压缩的 JSONL。
	//
	// **当前不可用**：Go 标准库的 compress 包在 1.26 里还没有公开的
	// zstd 编码器（internal/zstd 不可导入），而引入第三方实现会
	// 显著增加二进制体积与依赖面。判断是：先用 gzip 满足实际需求
	// （缩小上传体积），把 zstd 留作后续可选优化。
	//
	// 保留常量与明确的报错，是为了让"不支持"是**清楚的**——
	// 用户写 --format zstd 时应当得到"为什么不行、该用什么"，
	// 而不是一个看不懂的解析错误。
	FormatJSONLZst Format = "jsonl.zst"

	// FormatCSV 是扁平 CSV（一行一条记录，带表头）。
	//
	// 存在的意义是"能直接用"：JSONL 适合程序处理，
	// 而大多数人拿到数据第一件事是拖进 Excel 看一眼——
	// 那需要真正的一行一条，而不是嵌着 JSON 的单元格。
	FormatCSV Format = "csv"

	// FormatCSVGz 是 gzip 压缩的 CSV。
	FormatCSVGz Format = "csv.gz"
)

// zeroTime 是 gzip 头部里使用的固定时间。
//
// 默认情况下 compress/gzip 会把**当前时间**写进 Header.ModTime，
// 导致同一份数据两次导出的字节不同，于是"重新导出并比对哈希"
// 这种校验方式失效。置零之后导出是确定性的。
var zeroTime = time.Unix(0, 0).UTC()

// AllFormats 返回所有已知格式（含暂不支持的，便于生成帮助文本）。
func AllFormats() []Format {
	return []Format{FormatJSONL, FormatJSONLGz, FormatCSV, FormatCSVGz, FormatJSONLZst}
}

// SupportedFormats 返回当前**真正可用**的格式。
func SupportedFormats() []Format {
	return []Format{FormatJSONL, FormatJSONLGz, FormatCSV, FormatCSVGz}
}

// NormalizeFormat 归一化用户输入的格式名。
//
// 接受常见的等价写法：".gz" / "gz" / "gzip" 都当作 jsonl.gz。
func NormalizeFormat(raw string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "jsonl", "json", "ndjson":
		return FormatJSONL, nil
	case "gz", "gzip", "jsonl.gz", ".gz":
		return FormatJSONLGz, nil
	case "csv":
		return FormatCSV, nil
	case "csv.gz", "csv-gz":
		return FormatCSVGz, nil
	case "zst", "zstd", "jsonl.zst", ".zst":
		return FormatJSONLZst, nil
	default:
		return "", fmt.Errorf("unknown export format %q (available: %s)",
			raw, JoinFormats(SupportedFormats()))
	}
}

// Available 报告该格式当前是否可用。
func (f Format) Available() bool {
	switch f {
	case FormatJSONL, FormatJSONLGz, FormatCSV, FormatCSVGz:
		return true
	default:
		return false
	}
}

// IsCSV 报告该格式是不是 CSV 系（决定用哪个写出器）。
func (f Format) IsCSV() bool {
	return f == FormatCSV || f == FormatCSVGz
}

// IsCompressed 报告该格式是否压缩。
func (f Format) IsCompressed() bool {
	return f == FormatJSONLGz || f == FormatCSVGz
}

// UnavailableReason 返回格式不可用时的原因（可用时返回空字符串）。
//
// 明确给出原因而不是一句 "not supported"：用户需要知道
// "这是暂未实现"还是"参数写错了"。
func (f Format) UnavailableReason() string {
	if f.Available() {
		return ""
	}
	if f == FormatJSONLZst {
		return "zstd is not available in this build: Go 1.26's standard library does not " +
			"expose a zstd encoder, and adding a third-party one is deferred. " +
			"Use --format jsonl.gz instead."
	}
	return fmt.Sprintf("format %q is not supported", f)
}

// JoinFormats 把格式列表拼成可读文本。
func JoinFormats(formats []Format) string {
	parts := make([]string, 0, len(formats))
	for _, f := range formats {
		parts = append(parts, string(f))
	}
	return strings.Join(parts, ", ")
}

// Encoder 是一个已经套好缓冲与（可选）压缩的 JSONL 写出器。
type Encoder struct {
	// Format 是实际使用的格式。
	Format Format

	// Writer 是写 JSONL 的地方。调用方只管往这里写。
	Writer *Writer

	// finish 负责刷出压缩流尾部与缓冲。
	finish func() error
}

// NewEncoder 创建编码器。
//
// **必须**调用 Close（通常在写完之后、关闭底层文件之前）：
// gzip 的尾部与 bufio 的缓冲区都在里面。忘记调用会得到一个
// 看起来正常、实际被截断的文件——这是压缩输出最常见的错误。
func NewEncoder(format Format, w io.Writer) (*Encoder, error) {
	if !format.Available() {
		return nil, fmt.Errorf("%s", format.UnavailableReason())
	}
	if w == nil {
		return nil, fmt.Errorf("export: nil output writer")
	}

	// 64 KiB 缓冲：JSONL 单行通常几百字节，
	// 这个大小能把系统调用次数降到很低，又不至于占用可观内存。
	buffered := bufio.NewWriterSize(w, 64*1024)

	switch format {
	case FormatJSONL:
		return &Encoder{
			Format: format,
			Writer: NewWriter(buffered),
			finish: buffered.Flush,
		}, nil

	case FormatJSONLGz:
		// 压缩级别用默认值：JSONL 的可压缩比很高（跳表里重复字段多），
		// 而更高压缩级别在"一次性导出"这种场景没有明显收益。
		compressor := gzip.NewWriter(buffered)
		compressor.ModTime = zeroTime
		compressor.Name = ""
		compressor.Comment = ""

		return &Encoder{
			Format: format,
			Writer: NewWriter(compressor),
			finish: func() error {
				// 顺序很重要：先结束压缩流（写入 gzip 尾部），
				// 再刷出 bufio。颠倒会丢掉最后不足一个缓冲块的数据。
				if err := compressor.Close(); err != nil {
					return fmt.Errorf("close gzip stream: %w", err)
				}
				if err := buffered.Flush(); err != nil {
					return fmt.Errorf("flush output buffer: %w", err)
				}
				return nil
			},
		}, nil

	default:
		return nil, fmt.Errorf("%s", format.UnavailableReason())
	}
}

// Close 收尾。
//
// 幂等：重复调用只会真正执行一次，因为调用方常常在
// defer 与错误路径里都写一遍。
func (e *Encoder) Close() error {
	if e == nil || e.finish == nil {
		return nil
	}
	finish := e.finish
	e.finish = nil
	return finish()
}

// SuggestFilename 根据格式与用途给出建议文件名。
//
// 形如 cf-route-tester-20260101T000000Z-00000000-measurements.jsonl.gz：
// 会话 ID 让一批数据自证来源，"measurements"/"traces" 让文件用途自明。
func SuggestFilename(format Format, sessionID, kind string) string {
	parts := []string{"cf-route-tester"}
	if sessionID != "" {
		parts = append(parts, sessionID)
	}
	if kind != "" {
		parts = append(parts, kind)
	}
	base := strings.Join(parts, "-")

	switch format {
	case FormatJSONLGz:
		return base + ".jsonl.gz"
	case FormatJSONLZst:
		return base + ".jsonl.zst"
	default:
		return base + ".jsonl"
	}
}

// DetectFormatFromBytes 判断一段数据是否是 gzip（给测试与校验用）。
func DetectFormatFromBytes(data []byte) Format {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		return FormatJSONLGz
	}
	return FormatJSONL
}
