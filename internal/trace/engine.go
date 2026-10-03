// Package trace 把线路跟踪（traceroute）封装成项目内部的统一模型。
//
// 设计边界（需求第 6、17、18、23、24 条）：
//
//  1. **不自己实现 traceroute**：调用外部 nexttrace 可执行文件。
//     这样升级引擎不需要动本项目代码，也避免把 GPL-3.0 的源码
//     复制进来（NextTrace-core 标注 GPL-3.0）。
//  2. **不把 NextTrace 的 JSON 当作数据库 Schema**：
//     NextTrace JSON -> 本包解析 -> TraceResult -> storage。
//     上游输出格式变化时只改 parser.go。
//  3. **Trace 与 TCP Probe 完全解耦**：nexttrace 不存在时，
//     TCP Probe 必须照常可用（见 engine.go 的 Availability）。
//  4. 数据库里保存归一化后的 TraceResult，原始 JSON 只作为
//     诊断信息另存一列，业务逻辑不依赖它。
package trace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// 默认参数。
const (
	// DefaultTimeout 是单个 nexttrace 进程的超时。
	//
	// 比 TCP Probe 的 3 秒长得多是必然的：一次 traceroute 要发
	// 几十个 TTL 探测，每跳都要等回包。15 秒是常见的可接受上限。
	DefaultTimeout = 15 * time.Second

	// DefaultBinary 是默认的 nexttrace 可执行文件名（从 PATH 查找）。
	DefaultBinary = "nexttrace"

	// DefaultWorkers 是默认的跟踪并发数。
	//
	// 必须显著小于 probe 的 100（需求第 31 条）：每个 worker
	// 都会启动一个外部进程，100 个并发进程会把机器拖垮。
	DefaultWorkers = 10

	// MaxWorkers 是允许配置的跟踪并发上限。
	MaxWorkers = 64

	// maxStderrBytes 限制收集的标准错误长度。
	maxStderrBytes = 4 << 10
)

// Mode 是跟踪模式。
//
// 默认 tcp 是刻意的：本项目测的是 IP:Port，
// 例如 "1.2.3.4:443" 真正相关的是 TCP -> 443，
// 而不是 ICMP ping。
type Mode string

const (
	// ModeTCP 使用 TCP 探测（默认）。
	ModeTCP Mode = "tcp"

	// ModeICMP 使用 ICMP 回显探测。
	ModeICMP Mode = "icmp"

	// ModeUDP 使用 UDP 探测。
	ModeUDP Mode = "udp"
)

// Valid 报告模式是否受支持。
func (m Mode) Valid() bool {
	switch m {
	case ModeTCP, ModeICMP, ModeUDP:
		return true
	default:
		return false
	}
}

// Normalize 把用户输入归一化成合法模式。
func (m Mode) Normalize() (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(string(m)))) {
	case "", ModeTCP:
		return ModeTCP, nil
	case ModeICMP:
		return ModeICMP, nil
	case ModeUDP:
		return ModeUDP, nil
	default:
		return "", fmt.Errorf("unsupported trace mode %q (want tcp, icmp or udp)", m)
	}
}

// ---------------------------------------------------------------------------
// 结果模型
// ---------------------------------------------------------------------------

// Hop 是路径上的一跳。
//
// 这是**归一化后**的结构：字段来自 NextTrace 的 JSON，
// 但命名与类型由本项目决定，因此上游改字段名不会波及数据库。
type Hop struct {
	// TTL 是该跳的生存时间（从 1 开始）。
	TTL int

	// IP 是该跳回复的地址；为空表示超时（没有回复）。
	IP string

	// Hostname 是反解得到的主机名（可能为空）。
	Hostname string

	// RTTMS 是这一跳的往返时延（毫秒），可能多个探测值。
	RTTMS []float64

	// Timeout 表示这一跳没有任何回复。
	Timeout bool

	// ASN / ASOrganization 是该跳所属的自治系统信息（可能为空）。
	//
	// 这两个字段是"路径经过哪里"的关键：只有 IP 列表看不出
	// 走的是哪家运营商，加上 ASN 才能回答"不同运营商是否
	// 走不同出口"。
	ASN            string
	ASOrganization string

	// Country / Province / City 是该跳的地理位置（来自 NextTrace
	// 内置的 IP 库，可能为空）。
	Country  string
	Province string
	City     string
}

// Target 返回该跳的 "IP" 或空字符串。
func (h Hop) Target() string {
	if h.IP == "" {
		return ""
	}
	return h.IP
}

// MinRTT 返回该跳的最小往返时延；没有样本时返回 0。
func (h Hop) MinRTT() float64 {
	if len(h.RTTMS) == 0 {
		return 0
	}
	min := h.RTTMS[0]
	for _, v := range h.RTTMS[1:] {
		if v < min {
			min = v
		}
	}
	return min
}

// TraceResult 是一次线路跟踪的归一化结果。
//
// 字段刻意保持扁平：它会直接进入 SQLite 的 traces 表，
// 也会进入公开的 JSONL 导出数据。
type TraceResult struct {
	// TargetID 是 "IP:Port" 形式的稳定标识（与 model.Target 一致）。
	TargetID string

	// IP 是目标地址。
	IP string

	// Port 是跟踪使用的**目标端口**（tcp 模式下有意义）。
	Port int

	// Engine 是引擎名（当前固定为 "nexttrace"）。
	Engine string

	// EngineVersion 是引擎版本。
	//
	// 必须保存（需求第 58 条）：不同版本的输出结构与跳数
	// 判定可能不同，分析时要能把样本按版本区分。
	EngineVersion string

	// Mode 是跟踪模式；Protocol 是实际使用的协议（通常是 tcp）。
	Mode     Mode
	Protocol string

	// Success 表示跟踪是否成功完成。
	Success bool

	// DurationMS 是整个跟踪过程的耗时。
	DurationMS float64

	// Hops 是路径上的各跳（按 TTL 升序）。
	Hops []Hop

	// ErrorType / ErrorMessage 在失败时说明原因。
	ErrorType    ErrorType
	ErrorMessage string

	// Timestamp 是本次跟踪开始的时间（UTC）。
	Timestamp time.Time

	// RawJSON 是引擎原始输出，仅用于诊断。
	//
	// 它不参与任何业务判断；数据库里单独一列存放。
	RawJSON string

	// Stderr 是引擎的标准错误（截断后），仅用于诊断。
	Stderr string
}

// HopCount 返回路径跳数。
func (r TraceResult) HopCount() int { return len(r.Hops) }

// RespondedHops 返回有回复的跳数。
func (r TraceResult) RespondedHops() int {
	n := 0
	for _, hop := range r.Hops {
		if !hop.Timeout && hop.IP != "" {
			n++
		}
	}
	return n
}

// Valid 检查结果的内部一致性。
func (r TraceResult) Valid() error {
	if r.TargetID == "" {
		return errors.New("trace: empty target id")
	}
	if _, ok := model.ParseAddr(r.IP); !ok {
		return fmt.Errorf("trace: invalid ip %q", r.IP)
	}
	if !model.ValidPort(r.Port) {
		return fmt.Errorf("trace: port %d out of range", r.Port)
	}
	if r.Success {
		if r.ErrorType != ErrorTypeNone {
			return fmt.Errorf("trace: successful result carries error type %q", r.ErrorType)
		}
		return nil
	}
	if r.ErrorType == ErrorTypeNone {
		return errors.New("trace: failed result has no error type")
	}
	if !r.ErrorType.Valid() {
		return fmt.Errorf("trace: unknown error type %q", r.ErrorType)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 引擎接口
// ---------------------------------------------------------------------------

// TraceEngine 是线路跟踪引擎。
//
// 需求第 26 条要求这个接口形状：单个方法、接收 model.Target、
// 返回归一化结果。当前的实现是外部 nexttrace 进程；
// 将来若要换成库调用，只需另写一个实现。
type TraceEngine interface {
	// Trace 跟踪一个目标。
	//
	// 实现约定：
	//   - 单个目标跟踪失败**不返回 error**，而是返回带 ErrorType 的结果；
	//     只有"引擎层面不可用"才返回 error（例如二进制路径配错）。
	//   - 必须尊重 ctx 的取消与超时。
	Trace(ctx context.Context, target model.Target) (*TraceResult, error)

	// Name 返回引擎名字（写入 traces.engine）。
	Name() string
}

// ---------------------------------------------------------------------------
// 统计
// ---------------------------------------------------------------------------

// Stats 是一批跟踪的汇总。
type Stats struct {
	// Total 是提交的目标数。
	Total int

	// Completed 是实际得到结果的目标数。
	Completed int

	// Success 是跟踪成功数。
	Success int

	// Failed 是失败数。
	Failed int

	// ErrorCounts 是各失败分类的计数。
	ErrorCounts map[ErrorType]int

	// HopCounts 是成功跟踪的跳数样本，用于算平均跳数。
	HopCounts []int

	// Interrupted 表示被取消。
	Interrupted bool
}

// SuccessRate 返回成功率。没有样本时返回 0。
func (s Stats) SuccessRate() float64 {
	if s.Completed == 0 {
		return 0
	}
	return float64(s.Success) / float64(s.Completed)
}

// AverageHops 返回成功跟踪的平均跳数。没有样本时返回 0。
func (s Stats) AverageHops() float64 {
	if len(s.HopCounts) == 0 {
		return 0
	}
	sum := 0
	for _, n := range s.HopCounts {
		sum += n
	}
	return float64(sum) / float64(len(s.HopCounts))
}

// Add 把一条结果计入统计。
func (s *Stats) Add(result *TraceResult) {
	if s == nil || result == nil {
		return
	}
	s.Completed++
	if result.Success {
		s.Success++
		s.HopCounts = append(s.HopCounts, result.HopCount())
		return
	}
	s.Failed++
	if s.ErrorCounts == nil {
		s.ErrorCounts = make(map[ErrorType]int)
	}
	s.ErrorCounts[result.ErrorType]++
}
