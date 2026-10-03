package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// MeasurementSession 表示一次测量会话。
//
// 它回答的问题是："这条 measurement 属于哪一次扫描？"
//
// 为什么必须有它（需求第 20、21 条）：
// 判断"某个目标是否已经测过"不能只看"数据库里有没有历史结果"，
// 否则同一个采集者永远无法重新测量全部目标——而"定期重测"正是
// 本项目数据的来源。正确判据是 (目标, 采集者, 会话) 三元组：
//
//	2026-10-03 10:00 的完整扫描   是一个会话
//	2026-10-03 20:00 的完整扫描   是另一个会话
//
// 这也是后续分析"线路随时间如何变化"的基础：同一目标在两个会话
// 里的延迟差异，才是"晚高峰变差"这类结论的依据。
type MeasurementSession struct {
	// ID 是会话标识，由 NewSessionID 生成。
	ID string

	// CollectorPK 是采集者在数据库中的主键（0 表示尚未入库）。
	//
	// 这里用主键而非匿名 collector_id：会话总是与数据库一起存在，
	// 主键更省空间，也避免同一个 ID 在两张表里重复存储。
	CollectorPK int64

	// StartedAt 是会话开始时间（UTC）。
	StartedAt time.Time

	// FinishedAt 是会话结束时间；零值表示尚未结束（可续测）。
	FinishedAt time.Time

	// TargetCount 是本次会话计划测量的目标总数。
	TargetCount int

	// CompletedCount 是本次会话已完成的目标数（用于展示进度）。
	//
	// 注意：断点续测**不**依赖这个计数，而是从 measurements 表
	// 反推事实（见 storage.LoadSessionProgress）。计数可能因为
	// 进程被杀而落后于事实，事实不会。
	CompletedCount int

	// ClientVersion 是产生该会话的程序版本。
	ClientVersion string
}

// Finished 报告该会话是否已经结束。
func (s MeasurementSession) Finished() bool {
	return !s.FinishedAt.IsZero()
}

// Duration 返回会话已持续的时间（未结束时以 end 为截止）。
func (s MeasurementSession) Duration(end time.Time) time.Duration {
	if s.StartedAt.IsZero() {
		return 0
	}
	if !s.FinishedAt.IsZero() {
		return s.FinishedAt.Sub(s.StartedAt)
	}
	if end.Before(s.StartedAt) {
		return 0
	}
	return end.Sub(s.StartedAt)
}

// Remaining 返回本次会话尚未完成的目标数。
func (s MeasurementSession) Remaining() int {
	if s.CompletedCount >= s.TargetCount {
		return 0
	}
	return s.TargetCount - s.CompletedCount
}

// sessionIDLayout 是会话 ID 中时间部分的格式（UTC，秒级）。
const sessionIDLayout = "20060102T150405Z"

// sessionIDRandomBytes 是会话 ID 中随机后缀的字节数。
const sessionIDRandomBytes = 4

// NewSessionID 生成会话 ID，形如 "20260101T000000Z-00000000"。
//
// 设计取舍：
//
//   - 前缀是开始时间：会话 ID 天然有序、可读，便于按时间归档与人工识别；
//   - 后缀是 4 字节随机值：同一个采集者在同一秒内启动两次也不会撞 ID
//     （脚本重试、并行启动都可能发生），同时不承载任何可识别信息。
//
// 为什么不使用自增序号：多机并行时会冲突，而且序号会让
// "这是同一个人的第几次扫描"变得可追踪——那不是我们想要的。
func NewSessionID(startedAt time.Time) (string, error) {
	buf := make([]byte, sessionIDRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return startedAt.UTC().Format(sessionIDLayout) + "-" + hex.EncodeToString(buf), nil
}

// IsSessionID 报告字符串是否符合会话 ID 的格式。
//
// 用于校验从命令行（--session <id>）或身份文件读入的值，
// 及早给出"这个 ID 明显不对"的提示，而不是查不到才报错。
func IsSessionID(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return false
	}
	if len(parts[0]) != len("20060102T150405Z") {
		return false
	}
	if _, err := time.Parse(sessionIDLayout, parts[0]); err != nil {
		return false
	}
	if len(parts[1]) != sessionIDRandomBytes*2 {
		return false
	}
	for i := 0; i < len(parts[1]); i++ {
		c := parts[1][i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
