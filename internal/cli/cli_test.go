package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// testEnv 构造一个完全可控的 Env，避免测试写真实终端输出。
//
// 固定 Info（而不是 version.Get()）是为了让断言与编译期注入无关。
func testEnv(args ...string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	env := &Env{
		Args:   append([]string{"cf-route-tester"}, args...),
		Stdout: stdout,
		Stderr: stderr,
		Info: version.Info{
			Client:        "cf-route-tester",
			Version:       "0.1.0",
			SchemaVersion: 1,
			Commit:        "testcommit",
			BuildDate:     "2026-10-03T10:00:00Z",
			GoVersion:     "go1.21",
			OS:            "testos",
			Arch:          "testarch",
		},
	}
	return env, stdout, stderr
}

// runCLI 执行一次 CLI 调用并返回退出码与输出。
func runCLI(args ...string) (code int, stdout, stderr string) {
	env, out, errOut := testEnv(args...)
	code = Run(env, env.Args)
	return code, out.String(), errOut.String()
}

func TestVersionCommand(t *testing.T) {
	code, stdout, stderr := runCLI("version")

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeOK, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	// 输出格式被需求固定，属于契约，不要随意修改。
	const want = "cf-route-tester\nversion: 0.1.0\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestVersionFlagAtTopLevel(t *testing.T) {
	for _, flag := range []string{"--version", "-V"} {
		flag := flag
		t.Run(flag, func(t *testing.T) {
			code, stdout, stderr := runCLI(flag)
			if code != ExitCodeOK {
				t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeOK, stderr)
			}
			if stdout != "cf-route-tester\nversion: 0.1.0\n" {
				t.Errorf("stdout = %q, want version output", stdout)
			}
		})
	}
}

func TestVersionVerbose(t *testing.T) {
	code, stdout, stderr := runCLI("version", "--verbose")

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeOK, stderr)
	}
	for _, want := range []string{
		"client:         cf-route-tester",
		"version:        0.1.0",
		"schema_version: 1",
		"commit:         testcommit",
		"platform:       testos/testarch",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\ngot:\n%s", want, stdout)
		}
	}
}

func TestVersionUnknownFlagIsUsageError(t *testing.T) {
	code, stdout, stderr := runCLI("version", "--nope")

	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "unknown flag for version: --nope") {
		t.Errorf("stderr = %q, want unknown flag message", stderr)
	}
	if !strings.Contains(stderr, "--help") {
		t.Errorf("stderr = %q, want hint to run --help", stderr)
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(args...)

			if code != ExitCodeOK {
				t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeOK, stderr)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
			for _, want := range []string{
				"Usage:",
				"cf-route-tester <command> [flags]",
				"Commands:",
				"version",
				"schema_version=1",
			} {
				if !strings.Contains(stdout, want) {
					t.Errorf("help output missing %q\ngot:\n%s", want, stdout)
				}
			}
		})
	}
}

func TestNoArgsPrintsHelp(t *testing.T) {
	code, stdout, stderr := runCLI()

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeOK, stderr)
	}
	if !strings.Contains(stdout, "Usage:") {
		t.Errorf("stdout = %q, want help text", stdout)
	}
}

func TestCommandHelpForPlannedCommand(t *testing.T) {
	// Phase 0 尚未实现 scan，但帮助必须可用，并明确说明未实现。
	code, stdout, stderr := runCLI("scan", "--help")

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeOK, stderr)
	}
	if !strings.Contains(stdout, "cf-route-tester scan") {
		t.Errorf("stdout = %q, want scan usage", stdout)
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	code, stdout, stderr := runCLI("frobnicate")

	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "unknown command: frobnicate") {
		t.Errorf("stderr = %q, want unknown command message", stderr)
	}
}

func TestUnknownTopLevelFlagIsUsageError(t *testing.T) {
	code, _, stderr := runCLI("--frobnicate")

	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "unknown flag: --frobnicate") {
		t.Errorf("stderr = %q, want unknown flag message", stderr)
	}
}

// TestPlannedCommandsAreNotImplemented 保证 Phase 0 没有偷偷实现后续阶段功能，
// 同时保证“未实现”是用法错误而不是崩溃。
func TestPlannedCommandsAreNotImplemented(t *testing.T) {
	for _, name := range roadmapOrder {
		name := name
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runCLI(name)
			if code != ExitCodeUsage {
				t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeUsage, stderr)
			}
			if !strings.Contains(stderr, "not implemented yet") {
				t.Errorf("stderr = %q, want 'not implemented yet'", stderr)
			}
		})
	}
}

// TestHelpListsEveryPlannedCommand 防止路线图与帮助文本脱节。
func TestHelpListsEveryPlannedCommand(t *testing.T) {
	_, stdout, _ := runCLI("--help")
	for _, name := range roadmapOrder {
		if !strings.Contains(stdout, name) {
			t.Errorf("help output missing planned command %q", name)
		}
		if roadmapSummary[name] == "" {
			t.Errorf("planned command %q has no summary", name)
		}
	}
}

func TestExitCodesDistinguishUsageFromRuntimeError(t *testing.T) {
	env, _, _ := testEnv("boom")
	cmds := newCommands()
	cmds["boom"] = Command{
		Name: "boom",
		Run: func(*Env, []string) error {
			return errors.New("runtime failure")
		},
	}

	err := cmds["boom"].Run(env, nil)
	if err == nil {
		t.Fatal("expected error from command")
	}

	// 运行期错误必须是 ExitCodeError，而不是 ExitCodeUsage。
	got := codeForError(err)
	if got != ExitCodeError {
		t.Errorf("codeForError(runtime error) = %d, want %d", got, ExitCodeError)
	}
	if got := codeForError(usageError("bad flag")); got != ExitCodeUsage {
		t.Errorf("codeForError(usage error) = %d, want %d", got, ExitCodeUsage)
	}
	if got := codeForError(nil); got != ExitCodeOK {
		t.Errorf("codeForError(nil) = %d, want %d", got, ExitCodeOK)
	}
}

// TestRunNeverPanics 用一批畸形输入确认分发逻辑健壮。
func TestRunNeverPanics(t *testing.T) {
	inputs := [][]string{
		nil,
		{},
		{"cf-route-tester"},
		{"cf-route-tester", ""},
		{"cf-route-tester", "version", "--verbose", "extra"},
		{"cf-route-tester", "-"},
		{"cf-route-tester", "--"},
		{"cf-route-tester", "version", "-h"},
		{"cf-route-tester", strings.Repeat("a", 4096)},
	}

	for _, args := range inputs {
		args := args
		t.Run(strings.Join(args, "|"), func(t *testing.T) {
			env, _, _ := testEnv()
			if code := Run(env, args); code < 0 || code > 2 {
				t.Fatalf("unexpected exit code %d for args %v", code, args)
			}
		})
	}
}

// TestRunWithNilSinks 确认 stdout/stderr 为 nil 时不会 panic。
func TestRunWithNilSinks(t *testing.T) {
	env := &Env{Info: version.Info{Client: "cf-route-tester", Version: "0.1.0"}}
	if code := Run(env, []string{"cf-route-tester", "version"}); code != ExitCodeOK {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeOK)
	}
}

// TestRunWithNilEnv 确认 env 为 nil 时返回错误码而不是崩溃。
func TestRunWithNilEnv(t *testing.T) {
	if code := Run(nil, []string{"cf-route-tester"}); code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeError)
	}
}
