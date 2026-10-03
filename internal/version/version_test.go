package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestGetFieldsArePopulated(t *testing.T) {
	info := Get()

	if info.Client != ClientName {
		t.Errorf("Client = %q, want %q", info.Client, ClientName)
	}
	if info.Version != Version {
		t.Errorf("Version = %q, want %q", info.Version, Version)
	}
	if info.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", info.SchemaVersion, SchemaVersion)
	}
	// 公开数据必须带 schema_version，因此不允许为 0 或负数。
	if info.SchemaVersion < 1 {
		t.Errorf("SchemaVersion = %d, want >= 1", info.SchemaVersion)
	}
	// 未通过 ldflags 注入时必须回退到 "unknown"，而不是空字符串。
	if info.Commit == "" || info.BuildDate == "" {
		t.Errorf("Commit/BuildDate must never be empty, got %q / %q", info.Commit, info.BuildDate)
	}
	if info.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
	}
	if info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Errorf("platform = %s/%s, want %s/%s", info.OS, info.Arch, runtime.GOOS, runtime.GOARCH)
	}
}

func TestDefaultVersionIsSemver(t *testing.T) {
	parts := strings.Split(Version, ".")
	if len(parts) != 3 {
		t.Fatalf("Version = %q, want MAJOR.MINOR.PATCH", Version)
	}
	for _, p := range parts {
		if p == "" {
			t.Fatalf("Version = %q has empty component", Version)
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				t.Fatalf("Version = %q has non-numeric component %q", Version, p)
			}
		}
	}
}

func TestLine(t *testing.T) {
	info := Get()
	want := ClientName + "\nversion: " + Version
	if got := info.Line(); got != want {
		t.Errorf("Line() = %q, want %q", got, want)
	}
}

func TestString(t *testing.T) {
	info := Get()
	want := ClientName + " " + Version
	if got := info.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestDetailedContainsAllFields(t *testing.T) {
	info := Get()
	got := info.Detailed()

	for _, want := range []string{
		"client:",
		"version:",
		"schema_version:",
		"commit:",
		"build_date:",
		"go_version:",
		"platform:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Detailed() missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("Detailed() should not have a trailing newline, got %q", got)
	}
}

// TestInjectedBuildInfo 模拟 ldflags 注入，确认注入值会覆盖默认值。
func TestInjectedBuildInfo(t *testing.T) {
	oldCommit, oldDate := Commit, BuildDate
	t.Cleanup(func() {
		Commit, BuildDate = oldCommit, oldDate
	})

	Commit = "abc1234"
	BuildDate = "2026-10-03T10:00:00Z"

	info := Get()
	if info.Commit != "abc1234" {
		t.Errorf("Commit = %q, want abc1234", info.Commit)
	}
	if info.BuildDate != "2026-10-03T10:00:00Z" {
		t.Errorf("BuildDate = %q, want injected value", info.BuildDate)
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"":        unknown,
		"   ":     unknown,
		"\t\n":    unknown,
		"abc1234": "abc1234",
		" 1.2.3 ": "1.2.3",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
