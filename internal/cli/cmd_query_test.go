package cli

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQueryHelpExplainsSourcesAndCaveats(t *testing.T) {
	code, stdout, stderr := runCLI("query", "--help")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	for _, want := range []string{
		"-db",
		"-input",
		"-hops",
		"-series",
		"-list-targets",
		"-stats",
		"-format",
		"Data source",
		"mutually exclusive",
		// 分位数是近似值，必须说清楚。
		"近似",
		"percentiles_approx",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("query help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

func TestQueryRequiresExactlyOneDataSource(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no source", []string{"query", "1.1.1.1:443"}, "data source is required"},
		{"both sources", []string{"query", "1.1.1.1:443", "--db", "x.db", "--input", "y.jsonl"}, "mutually exclusive"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLI(tc.args...)
			if code != ExitCodeUsage {
				t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeUsage, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.want)
			}
		})
	}
}

func TestQueryRequiresTargetOrListingFlags(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "batch.jsonl")

	code, _, stderr := runCLI("query", "--input", input)
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "no target given") {
		t.Errorf("stderr = %q, want 'no target given'", stderr)
	}
}

// queryTestInput 写入一份最小的公开 JSONL（两个地区、一个目标）。
func queryTestInput(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "batch.jsonl")

	rows := []string{
		`{"schema_version":1,"kind":"measurement","client_version":"0.1.0",` +
			`"target_id":"45.63.67.144:443","ip":"45.63.67.144","port":443,` +
			`"timestamp_utc":"2026-10-03T10:00:00Z","collector_id":"c-cn","session_id":"s1",` +
			`"collector":{"country":"CN","province":"Zhejiang","city":"Hangzhou","isp":"China Mobile","asn":"AS9808"},` +
			`"measurement":{"success":true,"latency_ms":300.5}}`,

		`{"schema_version":1,"kind":"measurement","client_version":"0.1.0",` +
			`"target_id":"45.63.67.144:443","ip":"45.63.67.144","port":443,` +
			`"timestamp_utc":"2026-10-03T10:05:00Z","collector_id":"c-us","session_id":"s1",` +
			`"collector":{"country":"US","province":"California","city":"Los Angeles","isp":"Vultr","asn":"AS20473"},` +
			`"measurement":{"success":true,"latency_ms":250.25}}`,

		`{"schema_version":1,"kind":"measurement","client_version":"0.1.0",` +
			`"target_id":"45.63.67.144:443","ip":"45.63.67.144","port":443,` +
			`"timestamp_utc":"2026-10-03T10:10:00Z","collector_id":"c-cn","session_id":"s1",` +
			`"collector":{"country":"CN","province":"Zhejiang","city":"Hangzhou","isp":"China Mobile","asn":"AS9808"},` +
			`"measurement":{"success":false,"latency_ms":2000,"error_type":"timeout"}}`,

		`{"schema_version":1,"kind":"trace","client_version":"0.1.0",` +
			`"target_id":"45.63.67.144:443","ip":"45.63.67.144","port":443,` +
			`"timestamp_utc":"2026-10-03T10:15:00Z","collector_id":"c-cn","session_id":"s1",` +
			`"collector":{"country":"CN","province":"Zhejiang","city":"Hangzhou","isp":"China Mobile","asn":"AS9808"},` +
			`"trace":{"success":true,"engine":"nexttrace","engine_version":"1.7.3","mode":"tcp",` +
			`"duration_ms":12000,"hop_count":3,"responded_hops":2,` +
			`"hops":[{"ttl":1,"ip":"private-v4","rtt_ms":[1.5]},` +
			`{"ttl":2,"ip":"203.0.113.24","rtt_ms":[4.5],"asn":"AS64500"},` +
			`{"ttl":3,"ip":"45.63.67.144","rtt_ms":[300.5],"asn":"AS20473"}]}}`,
	}

	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestQueryProfileFromJSONL(t *testing.T) {
	input := queryTestInput(t)

	code, stdout, stderr := runCLI("query", "45.63.67.144:443", "--input", input, "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	// 两个地区都必须出现。
	if !strings.Contains(stdout, "CN/Zhejiang/Hangzhou/China Mobile/AS9808") {
		t.Errorf("output does not mention the CN collector region:\n%s", stdout)
	}
	if !strings.Contains(stdout, "US/California/Los Angeles/Vultr/AS20473") {
		t.Errorf("output does not mention the US collector region:\n%s", stdout)
	}

	// 成功率 2/3。
	if !strings.Contains(stdout, "3 total, 2 ok") {
		t.Errorf("output does not report the probe counts:\n%s", stdout)
	}
	if !strings.Contains(stdout, "timeout") {
		t.Errorf("output does not report the failure classification:\n%s", stdout)
	}

	// AS 路径必须出现。
	if !strings.Contains(stdout, "AS64500") || !strings.Contains(stdout, "AS20473") {
		t.Errorf("output does not show the AS path:\n%s", stdout)
	}
}

// TestQueryAcceptsInterspersedArgs 是一条命令行可用性的回归测试。
//
// Go 的 flag 包在第一个非选项参数处停止解析，因此
// `query TARGET --input x` 会把 --input 当成位置参数而报错——
// 而帮助里给出的示例正是这个写法。
func TestQueryAcceptsInterspersedArgs(t *testing.T) {
	input := queryTestInput(t)

	forms := [][]string{
		// 目标在前、选项在后（帮助里的写法）。
		{"query", "45.63.67.144:443", "--input", input, "--quiet"},
		// 选项在前、目标在后。
		{"query", "--input", input, "--quiet", "45.63.67.144:443"},
		// 交错。
		{"query", "45.63.67.144:443", "--input", input, "--hops", "--quiet"},
		// 通过 --target 指定。
		{"query", "--target", "45.63.67.144:443", "--input", input, "--quiet"},
	}

	for _, form := range forms {
		t.Run(strings.Join(form[1:], " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(form...)
			if code != ExitCodeOK {
				t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
			}
			if !strings.Contains(stdout, "45.63.67.144:443") {
				t.Errorf("output does not contain the profile:\n%s", stdout)
			}
		})
	}
}

func TestQueryHopsFlag(t *testing.T) {
	input := queryTestInput(t)

	withoutCode, without, _ := runCLI("query", "45.63.67.144:443", "--input", input, "--quiet")
	withCode, with, _ := runCLI("query", "45.63.67.144:443", "--input", input, "--hops", "--quiet")
	if withoutCode != ExitCodeOK || withCode != ExitCodeOK {
		t.Fatalf("exit codes = %d/%d, want both 0", withoutCode, withCode)
	}

	if !strings.Contains(with, "hops") {
		t.Errorf("--hops output has no hop table:\n%s", with)
	}
	// 逐跳表应当比不带时更长。
	if len(with) <= len(without) {
		t.Error("--hops did not add any output")
	}
	if !strings.Contains(with, "TTL") {
		t.Errorf("--hops output has no TTL column:\n%s", with)
	}
}

func TestQuerySeriesFlag(t *testing.T) {
	input := queryTestInput(t)

	code, stdout, stderr := runCLI("query", "45.63.67.144:443", "--input", input, "--series", "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "series") {
		t.Errorf("--series output has no series section:\n%s", stdout)
	}
}

func TestQueryJSONOutput(t *testing.T) {
	input := queryTestInput(t)

	code, stdout, stderr := runCLI("query", "45.63.67.144:443", "--input", input,
		"--format", "json", "--hops", "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	var profiles []struct {
		TargetID     string  `json:"target_id"`
		Regions      int     `json:"regions"`
		ProbeTotal   int64   `json:"probe_total"`
		ProbeSuccess int64   `json:"probe_success"`
		SuccessRate  float64 `json:"success_rate"`
		TraceTotal   int64   `json:"trace_total"`

		LatencySpreadMS float64 `json:"latency_spread_ms"`
		Latency         struct {
			Count             int64   `json:"count"`
			MinMS             float64 `json:"min_ms"`
			P50MS             float64 `json:"p50_ms"`
			MaxMS             float64 `json:"max_ms"`
			PercentilesApprox bool    `json:"percentiles_approx"`
		} `json:"latency"`

		RegionProfiles []struct {
			Label      string `json:"label"`
			Country    string `json:"country"`
			ProbeTotal int64  `json:"probe_total"`
			ASPaths    []struct {
				Signature string `json:"signature"`
				Count     int64  `json:"count"`
			} `json:"as_paths"`
			Hops []struct {
				TTL   int     `json:"ttl"`
				IP    string  `json:"ip"`
				P50MS float64 `json:"p50_ms"`
			} `json:"hops"`
		} `json:"region_profiles"`
	}
	if err := json.Unmarshal([]byte(stdout), &profiles); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	if len(profiles) != 1 {
		t.Fatalf("profiles = %d, want 1", len(profiles))
	}

	profile := profiles[0]
	if profile.TargetID != "45.63.67.144:443" {
		t.Errorf("TargetID = %q", profile.TargetID)
	}
	if profile.Regions != 2 {
		t.Errorf("Regions = %d, want 2", profile.Regions)
	}
	if profile.ProbeTotal != 3 || profile.ProbeSuccess != 2 {
		t.Errorf("probes = %d/%d, want 2/3", profile.ProbeSuccess, profile.ProbeTotal)
	}
	if profile.TraceTotal != 1 {
		t.Errorf("TraceTotal = %d, want 1", profile.TraceTotal)
	}

	// 分位数必须被标记为近似值。
	if !profile.Latency.PercentilesApprox {
		t.Error("percentiles_approx = false; callers must know these are approximations")
	}
	// 延迟统计必须自洽（实测踩过 p90 > max 的坑）。
	if profile.Latency.MinMS > profile.Latency.P50MS || profile.Latency.P50MS > profile.Latency.MaxMS {
		t.Errorf("latency is self-contradictory: min=%v p50=%v max=%v",
			profile.Latency.MinMS, profile.Latency.P50MS, profile.Latency.MaxMS)
	}

	// 两个地区都必须在 JSON 里。
	countries := map[string]bool{}
	for _, region := range profile.RegionProfiles {
		countries[region.Country] = true
	}
	if !countries["CN"] || !countries["US"] {
		t.Errorf("region profiles = %+v, want both CN and US", countries)
	}

	// 跨地区差异应当约等于 300.5 - 250.25。
	if profile.LatencySpreadMS < 40 || profile.LatencySpreadMS > 60 {
		t.Errorf("latency_spread_ms = %v, want about 50", profile.LatencySpreadMS)
	}

	// 至少有一个分组带 AS 路径与逐跳数据。
	var sawPath, sawHops bool
	for _, region := range profile.RegionProfiles {
		if len(region.ASPaths) > 0 {
			sawPath = true
		}
		if len(region.Hops) > 0 {
			sawHops = true
		}
	}
	if !sawPath {
		t.Error("no AS paths in the JSON output")
	}
	if !sawHops {
		t.Error("no hop statistics in the JSON output with --hops")
	}
}

func TestQueryListTargets(t *testing.T) {
	input := queryTestInput(t)

	t.Run("text", func(t *testing.T) {
		code, stdout, stderr := runCLI("query", "--list-targets", "--input", input, "--quiet")
		if code != ExitCodeOK {
			t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
		}
		if !strings.Contains(stdout, "45.63.67.144:443") {
			t.Errorf("target list does not include the target:\n%s", stdout)
		}
		if !strings.Contains(stdout, "TARGET") {
			t.Errorf("target list has no header:\n%s", stdout)
		}
	})

	t.Run("json", func(t *testing.T) {
		code, stdout, stderr := runCLI("query", "--list-targets", "--input", input,
			"--format", "json", "--quiet")
		if code != ExitCodeOK {
			t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
		}
		var entries []struct {
			TargetID string `json:"target_id"`
			Regions  int    `json:"regions"`
		}
		if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
		if len(entries) != 1 || entries[0].TargetID != "45.63.67.144:443" {
			t.Errorf("entries = %+v", entries)
		}
	})
}

func TestQueryStats(t *testing.T) {
	input := queryTestInput(t)

	code, stdout, stderr := runCLI("query", "--stats", "--input", input, "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "targets:") {
		t.Errorf("stats output has no target count:\n%s", stdout)
	}
	if !strings.Contains(stdout, "probes:") {
		t.Errorf("stats output has no probe count:\n%s", stdout)
	}
}

// TestQueryMissingTargetWarnsButSucceeds 验证找不到目标时明确告警。
func TestQueryMissingTargetWarnsButSucceeds(t *testing.T) {
	input := queryTestInput(t)

	code, _, stderr := runCLI("query", "8.8.8.8:443", "--input", input, "--quiet")
	if code == ExitCodeOK {
		t.Fatal("query succeeded for a target with no data")
	}
	if !strings.Contains(stderr, "no data for") {
		t.Errorf("stderr = %q, want an explicit warning", stderr)
	}
}

func TestQueryRefusesToOverwriteOutput(t *testing.T) {
	input := queryTestInput(t)
	out := filepath.Join(t.TempDir(), "report.json")

	if code, _, stderr := runCLI("query", "45.63.67.144:443", "--input", input,
		"--format", "json", "--out", out, "--quiet"); code != ExitCodeOK {
		t.Fatalf("first query failed: %d (%s)", code, stderr)
	}

	code, _, stderr := runCLI("query", "45.63.67.144:443", "--input", input,
		"--format", "json", "--out", out, "--quiet")
	if code == ExitCodeOK {
		t.Fatal("query overwrote an existing file")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want an 'already exists' message", stderr)
	}
}

// ---------------------------------------------------------------------------
// 交错参数的解析器本身
// ---------------------------------------------------------------------------

// TestParseFlagsAllowInterspersed 直接测试解析器的重排逻辑。
func TestParseFlagsAllowInterspersed(t *testing.T) {
	newFS := func() (*flag.FlagSet, *string, *string, *bool, *int) {
		fs := newFlagSet("test")
		db := fs.String("db", "", "")
		input := fs.String("input", "", "")
		hops := fs.Bool("hops", false, "")
		limit := fs.Int("limit", 0, "")
		return fs, db, input, hops, limit
	}

	cases := []struct {
		name      string
		args      []string
		wantDB    string
		wantInput string
		wantHops  bool
		wantLimit int
		wantArgs  []string
	}{
		{
			name:     "positional first",
			args:     []string{"1.1.1.1:443", "--db", "x.db"},
			wantDB:   "x.db",
			wantArgs: []string{"1.1.1.1:443"},
		},
		{
			name:     "positional last",
			args:     []string{"--db", "x.db", "1.1.1.1:443"},
			wantDB:   "x.db",
			wantArgs: []string{"1.1.1.1:443"},
		},
		{
			name:     "interspersed",
			args:     []string{"a:1", "--db", "x.db", "b:2", "--hops", "c:3"},
			wantDB:   "x.db",
			wantHops: true,
			wantArgs: []string{"a:1", "b:2", "c:3"},
		},
		{
			name:     "inline value",
			args:     []string{"a:1", "--db=y.db"},
			wantDB:   "y.db",
			wantArgs: []string{"a:1"},
		},
		{
			name:     "bool does not eat the next arg",
			args:     []string{"--hops", "a:1"},
			wantHops: true,
			wantArgs: []string{"a:1"},
		},
		{
			name:      "value flag eats the next arg even if it looks positional",
			args:      []string{"a:1", "--input", "b:2"},
			wantInput: "b:2",
			wantArgs:  []string{"a:1"},
		},
		{
			name:     "terminator sends everything after to positionals",
			args:     []string{"--db", "x.db", "--", "--hops", "a:1"},
			wantDB:   "x.db",
			wantHops: false,
			wantArgs: []string{"--hops", "a:1"},
		},
		{
			name:     "dash is a positional",
			args:     []string{"--db", "x.db", "-"},
			wantDB:   "x.db",
			wantArgs: []string{"-"},
		},
		{
			name:      "int flag",
			args:      []string{"a:1", "--limit", "5"},
			wantLimit: 5,
			wantArgs:  []string{"a:1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, db, input, hops, limit := newFS()
			positionals, err := parseFlagsAllowInterspersed(fs, tc.args)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if *db != tc.wantDB {
				t.Errorf("db = %q, want %q", *db, tc.wantDB)
			}
			if *input != tc.wantInput {
				t.Errorf("input = %q, want %q", *input, tc.wantInput)
			}
			if *hops != tc.wantHops {
				t.Errorf("hops = %v, want %v", *hops, tc.wantHops)
			}
			if *limit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", *limit, tc.wantLimit)
			}
			if strings.Join(positionals, ",") != strings.Join(tc.wantArgs, ",") {
				t.Errorf("positionals = %v, want %v", positionals, tc.wantArgs)
			}
		})
	}
}

// TestParseFlagsAllowInterspersedReportsErrors 验证错误仍然被报出。
func TestParseFlagsAllowInterspersedReportsErrors(t *testing.T) {
	fs := newFlagSet("test")
	fs.String("db", "", "")

	// 未知选项必须报错，而不是被当成位置参数静默接受。
	if _, err := parseFlagsAllowInterspersed(fs, []string{"--nope", "a:1"}); err == nil {
		t.Error("unknown flag was accepted")
	}

	// 需要值的选项缺少值时必须报错。
	fs2 := newFlagSet("test")
	fs2.String("db", "", "")
	if _, err := parseFlagsAllowInterspersed(fs2, []string{"--db"}); err == nil {
		t.Error("missing flag value was accepted")
	}
}
