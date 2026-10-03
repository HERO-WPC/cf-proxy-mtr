// Package cli 实现 cf-route-tester 的命令行前端。
//
// Phase 0 只实现命令分发骨架、help 和 version。
// 后续阶段的子命令（fetch / detect / probe / scan / trace / export /
// upload / aggregate / query / db）在这里注册，业务逻辑全部放在 internal/ 下，
// 保持 cmd 与 internal/cli 只负责参数解析、调用和退出码。
//
// 设计约定：
//
//   - 所有输出通过 io.Writer 注入，便于测试，避免直接写 os.Stdout；
//   - 退出码集中由 ExitCode* 常量定义；
//   - 任何一个子命令失败都不能 panic，必须返回 error。
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// 退出码约定。
//
// 0 表示成功；1 表示运行期错误（网络、数据库、外部工具等）；
// 2 表示用法错误（未知命令、未知参数、参数不合法），
// 与 Go 标准 flag 包在解析失败时返回 2 的习惯保持一致。
const (
	ExitCodeOK    = 0
	ExitCodeError = 1
	ExitCodeUsage = 2
)

// ErrUsage 表示调用方用法错误。
//
// 使用 errors.Is(err, ErrUsage) 判断是否为用法错误，
// 而不是用字符串匹配错误信息。
var ErrUsage = errors.New("usage error")

// usageError 包装用法错误，并带上具体原因。
func usageError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, args...))
}

// Command 是一个子命令。
//
// Phase 0 只有 help 和 version 两个命令实现了 Run；
// 后续阶段为每个子命令提供一个 Command 实例并注册到 newCommands，
// 不需要改动 Run 的分发逻辑。
type Command struct {
	// Name 是命令行中使用的名字，例如 "scan"。
	Name string

	// Summary 是一行说明，出现在顶层 help 的一行列表中。
	Summary string

	// Usage 是该子命令的用法行，例如 "cf-route-tester scan [flags]"。
	Usage string

	// Run 执行该子命令。args 是不含命令名的剩余参数。
	// 返回 error 时由调用方决定退出码。
	Run func(env *Env, args []string) error

	// Flags 可选地把该子命令的完整参数说明写入 w。
	//
	// 存在的意义：参数定义只写一次（在 FlagSet 里），
	// 帮助文本由 FlagSet 自己渲染，避免"帮助与实际参数不一致"。
	Flags func(w io.Writer)
}

// Env 是一次 CLI 调用的运行环境。
//
// 把它显式传下去，而不是让子命令去读全局的 os.Args / os.Stdout，
// 这样测试可以完全接管输入输出，也不会互相干扰。
type Env struct {
	// Args 是完整参数，Args[0] 是程序名。
	Args []string

	// Stdout 是标准输出。
	Stdout io.Writer

	// Stderr 是标准错误。
	Stderr io.Writer

	// Info 是当前二进制的版本信息。
	Info version.Info
}

// NewEnv 使用真实进程环境构造 Env。
//
// 只有 main 包应该调用它；测试请直接构造 Env。
func NewEnv() *Env {
	return &Env{
		Args:   os.Args,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Info:   version.Get(),
	}
}

// Run 解析参数、分发命令，并返回进程退出码。
//
// Run 永远不会 panic，也永远不会调用 os.Exit，
// 退出码由 main 包负责生效。
func Run(env *Env, args []string) int {
	if env == nil {
		return ExitCodeError
	}
	env.Args = args
	if env.Stdout == nil {
		env.Stdout = io.Discard
	}
	if env.Stderr == nil {
		env.Stderr = io.Discard
	}

	err := dispatch(env, args)
	code := codeForError(err)
	if code == ExitCodeOK {
		return code
	}

	fmt.Fprintln(env.Stderr, "Error: "+err.Error())
	if code == ExitCodeUsage {
		fmt.Fprintln(env.Stderr, "Run '"+env.Info.Client+" --help' for usage.")
	}
	return code
}

// codeForError 把 error 映射为退出码。
//
// 单独抽出来是为了让 ExitCode* 的语义只有一个实现点，
// 也便于测试断言，而不必真的启动进程。
func codeForError(err error) int {
	switch {
	case err == nil:
		return ExitCodeOK
	case errors.Is(err, ErrUsage):
		return ExitCodeUsage
	default:
		return ExitCodeError
	}
}

// dispatch 是纯分发逻辑，把 error 交给 Run 统一处理。
func dispatch(env *Env, args []string) error {
	// args[0] 是程序名；为空时按“没有参数”处理。
	rest := args
	if len(rest) > 0 {
		rest = rest[1:]
	}

	commands := newCommands()

	// 没有参数：打印帮助。这比报错更友好，也方便 Windows 双击 exe。
	if len(rest) == 0 {
		printHelp(env.Stdout, env.Info, commands)
		return nil
	}

	first := rest[0]

	// 顶层帮助开关。
	if isHelpToken(first) {
		printHelp(env.Stdout, env.Info, commands)
		return nil
	}

	// 顶层版本开关。允许 `--version` / `-V` 直接输出版本，
	// 这样脚本不必写 `version` 子命令。
	// 注意：`-v` 保留给各子命令的 verbose 语义，不作为版本开关。
	if first == "--version" || first == "-V" {
		fmt.Fprintln(env.Stdout, env.Info.Line())
		return nil
	}

	// 未知的全局开关：报用法错误，避免被当成未知命令产生误导信息。
	if strings.HasPrefix(first, "-") {
		return usageError("unknown flag: %s", first)
	}

	cmd, ok := commands[first]
	if !ok {
		return usageError("unknown command: %s", first)
	}

	cmdArgs := rest[1:]

	// 子命令级帮助：命令本身不处理 --help，统一在这里拦截，
	// 这样每个子命令的 Run 不需要重复实现帮助逻辑。
	if hasHelpToken(cmdArgs) {
		printCommandHelp(env.Stdout, env.Info, cmd)
		return nil
	}

	if cmd.Run == nil {
		return usageError("command %q is not implemented yet", cmd.Name)
	}
	return cmd.Run(env, cmdArgs)
}

// newCommands 返回当前已注册的命令表。
//
// 顺序无关，帮助输出会按固定顺序渲染。
func newCommands() map[string]Command {
	cmds := []Command{
		{
			Name:    "help",
			Summary: "显示帮助信息",
			Usage:   ClientName + " help",
			Run: func(env *Env, args []string) error {
				printHelp(env.Stdout, env.Info, newCommands())
				return nil
			},
		},
		newFetchCommand(),
		newProbeCommand(),
		newScanCommand(),
		newDetectCommand(),
		newTraceCommand(),
		newExportCommand(),
		newDBCommand(),
		{
			Name:    "version",
			Summary: "显示版本信息（--verbose 显示构建细节）",
			Usage:   ClientName + " version [--verbose]",
			Run:     runVersion,
		},
	}

	m := make(map[string]Command, len(cmds)+len(roadmapOrder))
	for _, c := range cmds {
		m[c.Name] = c
	}

	// 规划中的命令注册为占位实现：
	//   - 未知命令（真正的拼写错误）仍然是 "unknown command"；
	//   - 规划中命令返回明确的 "not implemented yet"，
	//     不会让用户误以为功能已经可用，也不会静默失败；
	//   - 帮助仍然可用：`cf-route-tester scan --help` 会打印用法。
	for _, name := range roadmapOrder {
		if _, exists := m[name]; exists {
			continue
		}
		name := name
		m[name] = Command{
			Name:    name,
			Summary: roadmapSummary[name],
			Usage:   ClientName + " " + name + " [flags]",
			Run: func(*Env, []string) error {
				return usageError("command %q is not implemented yet", name)
			},
		}
	}

	return m
}

// ClientName 是程序名称的简写，避免在参数解析里重复写版本包常量。
const ClientName = version.ClientName

// runVersion 实现 `cf-route-tester version`。
func runVersion(env *Env, args []string) error {
	verbose := false
	for _, a := range args {
		switch a {
		case "--verbose", "-verbose":
			verbose = true
		default:
			return usageError("unknown flag for version: %s", a)
		}
	}

	if verbose {
		fmt.Fprintln(env.Stdout, env.Info.Detailed())
		return nil
	}

	fmt.Fprintln(env.Stdout, env.Info.Line())
	return nil
}

// isHelpToken 判断单个 token 是否是帮助开关。
func isHelpToken(s string) bool {
	switch s {
	case "-h", "--h", "-help", "--help", "help":
		return true
	default:
		return false
	}
}

// hasHelpToken 判断参数列表中是否出现帮助开关。
//
// 只检查第一个参数，避免把子命令参数里作为值出现的 "-h" 误判成帮助请求。
func hasHelpToken(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return isHelpToken(args[0])
}
