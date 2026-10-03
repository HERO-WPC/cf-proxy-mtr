package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// helpOrder 固定顶层帮助中“当前可用命令”的显示顺序。
//
// 显式列出顺序，而不是遍历 map，保证帮助输出稳定、可测试。
var helpOrder = []string{"help", "fetch", "detect", "probe", "scan", "trace", "export", "aggregate", "db", "version"}

// roadmapOrder 固定“规划中命令”的显示顺序，方便用户理解路线图。
//
// 这些命令尚未实现，这里只做说明，避免用户误以为功能已可用。
var roadmapOrder = []string{
	"upload", // 上传匿名 batch 到 GitHub
	"query",  // 查询 IP:Port 线路画像
}

// roadmapSummary 是规划中命令的一行说明。
var roadmapSummary = map[string]string{
	"upload": "将匿名压缩批次上传到 GitHub 数据仓库",
	"query":  "查询某个 IP:Port 在不同地区 / 运营商下的线路画像",
}

// printHelp 输出顶层帮助。
func printHelp(w io.Writer, info version.Info, commands map[string]Command) {
	var b strings.Builder

	b.WriteString(info.Client + " — 众测网络线路测量工具\n")
	b.WriteString("version: " + info.Version + "\n")
	b.WriteString("\n")
	b.WriteString("Usage:\n")
	b.WriteString("  " + info.Client + " <command> [flags]\n")
	b.WriteString("  " + info.Client + " --help\n")
	b.WriteString("  " + info.Client + " --version\n")
	b.WriteString("\n")
	b.WriteString("Commands:\n")

	rows := make([][2]string, 0, len(helpOrder))
	for _, name := range helpOrder {
		cmd, ok := commands[name]
		if !ok {
			continue
		}
		rows = append(rows, [2]string{cmd.Name, cmd.Summary})
	}
	b.WriteString(renderRows(rows))

	b.WriteString("\nPlanned commands (not implemented yet):\n")
	planned := make([][2]string, 0, len(roadmapOrder))
	for _, name := range roadmapOrder {
		planned = append(planned, [2]string{name, roadmapSummary[name]})
	}
	b.WriteString(renderRows(planned))

	b.WriteString("\nFlags:\n")
	b.WriteString(renderRows([][2]string{
		{"-h, --help", "显示帮助信息"},
		{"--version", "显示程序版本"},
	}))

	b.WriteString("\n" + info.Client + " 会把每个 IP:Port 的 TCP 延迟、丢包与 NextTrace 路径\n")
	b.WriteString("按 地区 × 运营商 × 时间 聚合，构建长期维护的公网线路数据库。\n")
	b.WriteString("所有公开数据都带 schema_version=" + fmt.Sprint(info.SchemaVersion) + "。\n")

	fmt.Fprint(w, strings.TrimRight(b.String(), "\n")+"\n")
}

// printCommandHelp 输出单个子命令的帮助。
func printCommandHelp(w io.Writer, info version.Info, cmd Command) {
	var b strings.Builder
	if cmd.Summary != "" {
		b.WriteString(cmd.Summary + "\n\n")
	}
	b.WriteString("Usage:\n")
	usage := cmd.Usage
	if usage == "" {
		usage = info.Client + " " + cmd.Name
	}
	b.WriteString("  " + usage + "\n")

	// 参数说明来自子命令自己的 FlagSet，保证帮助与实际参数一致。
	if cmd.Flags != nil {
		b.WriteString("\nFlags:\n")
		fmt.Fprint(w, b.String())
		cmd.Flags(w)
		return
	}

	fmt.Fprint(w, b.String())
}

// renderRows 把两列文本对齐渲染，避免手工拼空格。
//
// 宽度按显示宽度估算：中日韩全角字符记 2 列，其余记 1 列。
// 这样中英混排的帮助文本在终端里也能基本对齐。
func renderRows(rows [][2]string) string {
	width := 0
	for _, r := range rows {
		if n := displayWidth(r[0]); n > width {
			width = n
		}
	}

	var b strings.Builder
	for _, r := range rows {
		pad := width - displayWidth(r[0])
		if pad < 0 {
			pad = 0
		}
		b.WriteString("  " + r[0] + strings.Repeat(" ", pad) + "  " + r[1] + "\n")
	}
	return b.String()
}

// displayWidth 估算字符串在等宽终端中的显示宽度。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWideRune(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// isWideRune 判断 rune 是否属于需要占两列的宽字符区间。
//
// 这里是一个刻意保守的近似实现：只覆盖常见区间，
// 宁可少算一列（略微不对齐），也不引入额外依赖去追求精确的
// East Asian Width 表。
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F: // 韩文字母
		return true
	case r >= 0x2E80 && r <= 0x303E: // 中日韩部首、标点
		return true
	case r >= 0x3041 && r <= 0x33FF: // 平假名、片假名、注音、兼容字符
		return true
	case r >= 0x3400 && r <= 0x4DBF: // 中日韩扩展 A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // 中日韩统一表意文字
		return true
	case r >= 0xA000 && r <= 0xA4CF: // 彝文
		return true
	case r >= 0xAC00 && r <= 0xD7A3: // 韩文音节
		return true
	case r >= 0xF900 && r <= 0xFAFF: // 兼容表意文字
		return true
	case r >= 0xFE30 && r <= 0xFE6F: // 中日韩兼容形式、小写变体
		return true
	case r >= 0xFF00 && r <= 0xFF60: // 全角形式
		return true
	case r >= 0xFFE0 && r <= 0xFFE6: // 全角符号
		return true
	default:
		return false
	}
}
