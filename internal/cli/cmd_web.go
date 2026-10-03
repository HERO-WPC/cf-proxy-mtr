package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/applog"
	"github.com/cf-route-tester/cf-route-tester/internal/identity"
	"github.com/cf-route-tester/cf-route-tester/internal/service"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
	"github.com/cf-route-tester/cf-route-tester/internal/webui"
)

// newWebCommand 构造 web 命令。
func newWebCommand() Command {
	return Command{
		Name:    "web",
		Summary: "启动图形界面（本地网页，数据不出本机）",
		Usage:   ClientName + " web [flags]",
		Run:     runWeb,
		// 参数说明由 FlagSet 自己渲染，避免帮助与实际参数不一致。
		Flags: func(w io.Writer) {
			var p webParams
			fs := webFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()

			fmt.Fprintf(w, "\n例子:\n")
			fmt.Fprintf(w, "  %s web                      启动界面并自动打开浏览器\n", ClientName)
			fmt.Fprintf(w, "  %s web --no-browser         只启动服务（服务器/无桌面环境）\n", ClientName)
			fmt.Fprintf(w, "  %s web --verbose            同时在终端显示日志\n", ClientName)
			fmt.Fprintf(w, "  %s web --listen 127.0.0.1:8123   固定端口\n", ClientName)

			fmt.Fprintf(w, "\n关于安全:\n")
			fmt.Fprintf(w, "  每次启动都会生成一个随机访问令牌，所有接口都要求带上它。\n")
			fmt.Fprintf(w, "  默认只绑定 127.0.0.1。改成 0.0.0.0 会把「从本机发起大量\n")
			fmt.Fprintf(w, "  网络连接」的能力暴露给整个局域网，请确认你确实需要。\n")

			fmt.Fprintf(w, "\n关于日志:\n")
			fmt.Fprintf(w, "  界面运行时终端可能不可见（例如双击启动），因此日志会写入\n")
			fmt.Fprintf(w, "  --log-dir 下的文件。启动时终端会打印该文件的完整路径。\n")
		},
	}
}

// webParams 是 web 命令的参数。
type webParams struct {
	listen       string
	db           string
	identityPath string
	logDir       string
	logFile      string
	noBrowser    bool
	verbose      bool

	// console 为真时把日志与控制台输出写到 stdout/stderr。
	//
	// 由入口决定，不是命令行参数：
	//
	//	有控制台（cf-route-tester web）           true
	//	无控制台（cf-route-tester-gui 双击启动）   false
	//
	// 后者必须为 false，否则日志会"写往一个不存在的地方"：
	// Windows GUI 子系统进程的 os.Stderr 指向无效句柄，写进去就没了，
	// 而用户恰恰是最需要日志的那种情况（双击之后不知道发生了什么）。
	console bool

	source sourceParams
}

// webFlagSet 定义 web 命令的参数。
func webFlagSet(p *webParams) *flag.FlagSet {
	fs := newFlagSet("web")

	fs.StringVar(&p.listen, "listen", webui.DefaultListen,
		"监听地址（默认只绑本机；改成 0.0.0.0:8123 会暴露到局域网）")
	fs.StringVar(&p.db, "db", storage.DefaultPath, "SQLite 数据库路径")
	fs.StringVar(&p.identityPath, "identity", identity.DefaultPath, "本地匿名标识文件路径")
	fs.StringVar(&p.logDir, "log-dir", applog.DefaultDir,
		"日志目录（脱离命令行运行时，日志文件是唯一的排查入口；留空则不写文件）")
	fs.StringVar(&p.logFile, "log-file", applog.DefaultFileName, "日志文件名")
	fs.BoolVar(&p.noBrowser, "no-browser", false, "启动后不自动打开浏览器")
	fs.BoolVar(&p.verbose, "verbose", false, "在终端同时显示日志（默认只写文件）")

	registerSourceFlags(fs, &p.source)
	return fs
}

// runWeb 启动图形界面。
func runWeb(env *Env, args []string) error {
	var p webParams
	fs := webFlagSet(&p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// 有控制台时日志同时写到 stderr；无控制台（双击启动的 GUI）
	// 时只写文件，因为写 stderr 等于丢弃。
	p.console = env == nil || !env.Detached

	if strings.TrimSpace(p.db) == "" {
		return usageError("--db must not be empty")
	}

	// 日志：**先建日志再干别的**。
	//
	// 这是"脱离命令行运行"能成立的前提：没有终端就没有 stderr，
	// 而"双击之后什么都没发生"是最难排查的故障。
	// 因此日志文件必须在任何可能失败的操作之前就位。
	//
	// 注意 Console / AlsoStderr 在有控制台时才设置：
	// 无控制台的进程里 os.Stderr 指向无效句柄，写进去等于丢弃，
	// 而那种情况恰恰最需要日志（实测踩过：GUI 启动后日志文件里
	// 一条记录都没有，只剩端口在监听）。
	var console io.Writer
	if p.console {
		console = env.Stderr
	}

	logger := applog.New(applog.Options{
		Dir:        p.logDir,
		FileName:   p.logFile,
		Console:    console,
		AlsoStderr: p.console && p.verbose,
	})
	defer func() { _ = logger.Close() }()

	logger.Info("web: starting",
		"version", env.Info.Version,
		"platform", env.Info.OS+"/"+env.Info.Arch,
		"db", p.db,
		"console", p.console)

	// 启动信息既给人也进日志。
	//
	// **必须同时写日志**：无控制台时 Printf 到一个无效句柄等于丢弃，
	// 而那个带 token 的地址是用户进入界面的唯一凭据。
	startInfo := webStartMessage(nil, logger.Path())
	if p.console {
		fmt.Fprint(env.Stdout, startInfo)
	}

	svc := service.New(service.Options{
		DBPath:       p.db,
		IdentityPath: p.identityPath,
		Source:       p.source.toConfig(),
		Logf: func(format string, args ...any) {
			// service 用 printf 风格，applog 用键值风格；
			// 这里转发成一条日志，保持单一时间线。
			logger.Info(fmt.Sprintf(format, args...))
		},
	})

	server, err := webui.New(webui.Config{
		Listen:      p.listen,
		Service:     svc,
		Logger:      logger,
		OpenBrowser: !p.noBrowser,
	})
	if err != nil {
		return err
	}
	if err := server.Start(); err != nil {
		// 端口被占用是最常见的启动失败。给出可操作的建议，
		// 而不是让用户面对一句 "address already in use"。
		if isAddressInUse(err) {
			return fmt.Errorf("%w\n提示：端口已被占用。用 --listen 127.0.0.1:0 让系统分配一个空闲端口", err)
		}
		return err
	}

	// 把带 token 的地址**写进日志**，并在有控制台时打印。
	//
	// 写日志是必需的：无控制台时 stdout 是无效句柄，
	// 而这个地址是用户进入界面的唯一凭据——丢了就只能重启服务。
	logger.Info("web: ready", "url", server.URL(), "page", server.PageURL())
	if p.console {
		fmt.Fprint(env.Stdout, webStartMessage(server, logger.Path()))
	}

	// 等 Ctrl+C 或终止信号。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	select {
	case <-ctx.Done():
		if p.console {
			fmt.Fprintln(env.Stdout, "\n正在关闭…")
		}
	case <-server.Done():
		// 服务自己退出了（异常路径）。
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), webui.DefaultShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("web: shutdown did not complete cleanly", "error", err)
		if p.console {
			fmt.Fprintln(env.Stderr, "warning: shutdown did not complete cleanly: "+err.Error())
		}
	}
	logger.Info("web: stopped")
	return nil
}

// webStartMessage 组装启动提示。
//
// server 可以为 nil：那时只输出日志路径（启动早期的提示用）。
func webStartMessage(server *webui.Server, logPath string) string {
	var b strings.Builder
	if logPath != "" {
		fmt.Fprintf(&b, "log:        %s\n", logPath)
	} else {
		fmt.Fprintf(&b, "log:        (disabled)\n")
	}
	if server == nil {
		return b.String()
	}

	fmt.Fprintf(&b, "listening:  %s\n", server.URL())
	fmt.Fprintf(&b, "\n请在浏览器中打开（地址里带有本次运行的访问令牌）：\n")
	fmt.Fprintf(&b, "  %s\n", server.PageURL())
	fmt.Fprintf(&b, "\n令牌只对本次运行有效，重启后会换一个；它不会写入磁盘。\n")
	fmt.Fprintf(&b, "按 Ctrl+C 停止。\n")
	return b.String()
}

// isAddressInUse 判断错误是否为"端口被占用"。
//
// 不用字符串匹配：不同平台与 Go 版本的措辞不一致
// （Windows 与 Linux 的 syscall 错误文本不同）。
func isAddressInUse(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// syscall.EADDRINUSE 在 Windows 上也会被映射到这里。
		return strings.Contains(strings.ToLower(opErr.Err.Error()), "address already in use") ||
			strings.Contains(strings.ToLower(opErr.Err.Error()), "only one usage of each socket address")
	}
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
}

// ---------------------------------------------------------------------------
// 无参数启动（图形界面入口）
// ---------------------------------------------------------------------------

// RunDefault 是"不带子命令"时的入口。
//
// 双击可执行文件时（Windows 上会带上 -H windowsgui，没有控制台）
// 走的就是这里：直接启动图形界面，而不是打印一个用户看不见的帮助文本。
//
// 它与命令行共享同一份日志与编排逻辑，因此"界面里跑的东西"
// 与"命令行里跑的东西"永远是一回事。
//
// 返回退出码（而不是 error），因为 GUI 入口没有别的错误处理层：
// 它拿到 error 也只能打印然后映射成退出码，而打印在无控制台时
// 是看不见的——因此错误必须先由本函数写进日志（runWeb 已经这么做）。
func RunDefault(env *Env, args []string) int {
	// args[0] 是程序名；把其余参数当作 web 子命令的参数。
	rest := args
	if len(rest) > 0 {
		rest = rest[1:]
	}

	// 顶层帮助/版本仍然照常工作，这样 GUI 二进制也能当 CLI 用。
	if len(rest) > 0 && (isHelpToken(rest[0]) || rest[0] == "--version" || rest[0] == "-V") {
		return Run(env, args)
	}

	// 如果用户给了**子命令**（fetch / scan / ...），就走完整 CLI。
	//
	// 用"第一个参数不是选项"来判定：`--listen 127.0.0.1:8123` 里的
	// 第一个参数是选项，因此走 GUI 分支；`scan --limit 5` 的第一个
	// 参数是子命令，走 CLI 分支。
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		if _, known := newCommands()[rest[0]]; known {
			return Run(env, args)
		}
	}

	// 其余情况：直接启动图形界面，并把参数原样交给 web 命令。
	return codeForError(runWeb(env, rest))
}
