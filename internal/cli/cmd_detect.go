package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/detect"
	"github.com/cf-route-tester/cf-route-tester/internal/identity"
)

// detectParams 是 `cf-route-tester detect` 的参数。
type detectParams struct {
	identityPath string
	geoIP        string
	sources      string
	timeout      durationFlag

	write bool
	json  bool

	// 手动覆盖项：与扫描命令一致，命令行给出的值优先。
	manualCountry   string
	manualProvince  string
	manualCity      string
	manualISP       string
	manualASN       string
	manualIPVersion string

	quiet bool
}

// newDetectCommand 构造 detect 子命令。
func newDetectCommand() Command {
	return Command{
		Name:    "detect",
		Summary: "检测测量者所在地区与运营商，写入本地标识文件",
		Usage:   ClientName + " detect [flags]",
		Run:     runDetect,
		Flags: func(w io.Writer) {
			var p detectParams
			fs := detectFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()

			fmt.Fprintf(w, "\nDetection sources:\n")
			fmt.Fprintf(w, "  local   本机网络栈推断出口 IP 版本；不联系任何外部服务\n")
			fmt.Fprintf(w, "  geoip   查询公开 geo-IP API 得到国家/地区/城市/ISP/ASN\n")
			fmt.Fprintf(w, "          该源按定义需要服务端看到请求来源，因此会暴露你的公网 IP。\n")
			fmt.Fprintf(w, "          CLI 会在发起请求前把这一点打印出来。\n")
			fmt.Fprintf(w, "\n自动检测只是辅助：公网 ASN 不一定等于你实际感知的接入线路，\n")
			fmt.Fprintf(w, "手动填写的值永远优先于检测结果（不会被覆盖）。\n")
		},
	}
}

// detectFlagSet 构造 detect 的 FlagSet。
func detectFlagSet(p *detectParams) *flag.FlagSet {
	fs := newFlagSet("detect")

	fs.StringVar(&p.identityPath, "identity", identity.DefaultPath, "本地匿名标识文件路径")
	fs.StringVar(&p.geoIP, "geoip-endpoint", detect.DefaultGeoIPEndpoint,
		"geo-IP API 端点（留空则该源被禁用）")
	fs.StringVar(&p.sources, "source", "", "只使用指定检测源（逗号分隔，默认全部）")
	fs.Var(&p.timeout, "timeout", "单个检测源超时（默认 "+detect.DefaultTimeout.String()+"）")

	fs.BoolVar(&p.write, "write", false, "把检测结果写入标识文件（默认只显示）")
	fs.BoolVar(&p.json, "json", false, "以 JSON 输出结果")
	fs.BoolVar(&p.quiet, "quiet", false, "只输出最终结果，不输出检测过程")

	fs.StringVar(&p.manualCountry, "country", "", "手动指定国家代码（优先于检测结果）")
	fs.StringVar(&p.manualProvince, "province", "", "手动指定省份（优先于检测结果）")
	fs.StringVar(&p.manualCity, "city", "", "手动指定城市（优先于检测结果）")
	fs.StringVar(&p.manualISP, "isp", "", "手动指定运营商（优先于检测结果）")
	fs.StringVar(&p.manualASN, "asn", "", "手动指定 ASN（优先于检测结果）")
	fs.StringVar(&p.manualIPVersion, "ip-version", "", "手动指定 IP 版本（优先于检测结果）")

	return fs
}

// runDetect 实现 `cf-route-tester detect`。
func runDetect(env *Env, args []string) error {
	var p detectParams
	fs := detectFlagSet(&p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	// 标识文件里的现有值既是"手动配置"的来源，也是写入目标。
	local, err := identity.Load(p.identityPath)
	if err != nil {
		return err
	}

	// 手动值 = 现有文件内容 + 命令行覆盖（命令行优先）。
	manual := detect.FromProfile(local.Profile.ToModel())
	applyOverrides(&manual, map[detect.Field]string{
		detect.FieldCountry:   p.manualCountry,
		detect.FieldProvince:  p.manualProvince,
		detect.FieldCity:      p.manualCity,
		detect.FieldISP:       p.manualISP,
		detect.FieldASN:       p.manualASN,
		detect.FieldIPVersion: p.manualIPVersion,
	})

	// 归一化命令行给出的值。
	//
	// 这一步不能省：用户手写 "--country cn" / "--asn 9808" 是完全合理的，
	// 而模型要求 "CN" / "AS9808"。若不归一化，这些值会被当成"用户原文"
	// 直接出现在结果与配置文件里，既与检测结果形式不一致，
	// 也会让按国家/ASN 分组出现两套写法。
	manual = detect.FromProfile(manual.ToProfile())

	// 组装检测源。
	sources := detect.AllSources(p.geoIP)
	if names := splitList(p.sources); len(names) > 0 {
		sources, err = detect.FilterSources(sources, names)
		if err != nil {
			return usageError("%v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opts := detect.Options{
		Sources: sources,
		Manual:  manual,
	}
	if p.timeout.set {
		opts.Timeout = p.timeout.d
	}

	// 在**发起任何请求之前**把"谁会看到你的 IP"打印出来。
	//
	// 这是本命令最重要的输出：地理定位在原理上必须让服务端看到来源，
	// 用户有权在那一刻知道，并选择中止（Ctrl+C）或换一个端点。
	if !p.quiet && !p.json {
		printDetectPlan(env.Stderr, sources, manual, p)
	}

	result, err := detect.Detect(ctx, opts)
	if err != nil {
		return err
	}

	if p.json {
		if err := writeDetectJSON(env.Stdout, result, p.write); err != nil {
			return err
		}
	} else if !p.quiet {
		printDetectReports(env.Stderr, result)
	}

	// 写入标识文件（仅在 --write 时）。
	if p.write {
		updated := *local
		// 只覆盖**检测到或手动给出**的字段；留空的字段保持原值，
		// 避免一次失败的检测把已有配置清空。
		updated.Profile = mergeDetectedProfile(local.Profile, result.Values)
		if err := identity.Save(p.identityPath, &updated); err != nil {
			return err
		}
		if !p.quiet && !p.json {
			fmt.Fprintf(env.Stderr, "\nwrote %s\n", p.identityPath)
		}
	} else if !p.quiet && !p.json {
		fmt.Fprintf(env.Stderr, "\n(dry run: pass --write to save these values to %s)\n", p.identityPath)
	}

	if !p.json {
		printDetectResult(env.Stdout, result, p)
	}
	return nil
}

// applyOverrides 把非空覆盖项写入 values。
func applyOverrides(values *detect.Values, overrides map[detect.Field]string) {
	for field, value := range overrides {
		if strings.TrimSpace(value) != "" {
			values.Set(field, value)
		}
	}
}

// mergeDetectedProfile 把检测到的字段并入已有画像。
//
// 语义：**检测结果优先**（因为用户显式要求检测），但空字段保持原值。
// 用户想固定某个字段时，可以在命令行上手动指定（那会进入 result.Values
// 并标记为 manual），或者干脆不跑 detect。
func mergeDetectedProfile(existing identity.Profile, detected detect.Values) identity.Profile {
	values := detect.FromProfile(existing.ToModel())
	for _, field := range detect.AllFields() {
		if v := detected.Get(field); v != "" {
			values.Set(field, v)
		}
	}
	return identity.FromModel(values.ToProfile())
}

// splitList 把逗号分隔的参数拆成列表（忽略空项与空白）。
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

// printDetectPlan 在请求发出之前说明将要发生什么。
func printDetectPlan(w io.Writer, sources []detect.Source, manual detect.Values, p detectParams) {
	fmt.Fprintf(w, "detection sources:\n")
	for _, source := range sources {
		marker := "[local] "
		if source.ExposesLocalIP() {
			marker = "[exposes your public IP] "
		}
		fmt.Fprintf(w, "  %s%s\n      %s\n", marker, source.Name(), source.Describe())
	}

	if fields := describeValues(manual); fields != "" {
		fmt.Fprintf(w, "\nmanual values (highest priority): %s\n", fields)
	}
	fmt.Fprintf(w, "\n")
}

// printDetectReports 输出各源的实际结果。
func printDetectReports(w io.Writer, result *detect.Result) {
	fmt.Fprintf(w, "source results:\n")
	for _, report := range result.Reports {
		if !report.Used {
			continue
		}
		if report.Err != nil {
			fmt.Fprintf(w, "  %-6s FAILED  %s (%s)\n", report.Name, report.Err, report.Duration.Round(time.Millisecond))
			continue
		}
		fields := describeValues(report.Values)
		if fields == "" {
			fields = "(no fields)"
		}
		fmt.Fprintf(w, "  %-6s OK      %s  (%s)\n", report.Name, fields, report.Duration.Round(time.Millisecond))
	}

	fmt.Fprintf(w, "\nfield sources:\n")
	for _, field := range detect.AllFields() {
		source := result.FieldSources[field]
		if source == "" {
			source = "-"
		}
		fmt.Fprintf(w, "  %-11s %s\n", string(field)+":", source)
	}
	fmt.Fprintf(w, "\n")
}

// printDetectResult 输出最终的采集者画像。
func printDetectResult(w io.Writer, result *detect.Result, p detectParams) {
	values := result.Values

	fmt.Fprintf(w, "collector profile:\n")
	fmt.Fprintf(w, "  country:    %s\n", displayOrDash(values.Country))
	fmt.Fprintf(w, "  province:   %s\n", displayOrDash(values.Province))
	fmt.Fprintf(w, "  city:       %s\n", displayOrDash(values.City))
	fmt.Fprintf(w, "  isp:        %s\n", displayOrDash(values.ISP))
	fmt.Fprintf(w, "  asn:        %s\n", displayOrDash(values.ASN))
	fmt.Fprintf(w, "  ip_version: %s\n", displayOrDash(values.IPVersion))

	if values.NonEmpty() < len(detect.AllFields()) {
		fmt.Fprintf(w, "\nnote: %d of %d fields could not be detected.\n",
			len(detect.AllFields())-values.NonEmpty(), len(detect.AllFields()))
		fmt.Fprintf(w, "      自动检测只是辅助：公网 ASN 不一定等于你实际感知的接入线路。\n")
		fmt.Fprintf(w, "      可以用 --country/--province/--city/--isp/--asn/--ip-version 手动指定，\n")
		fmt.Fprintf(w, "      并在 %s scan 时通过同样参数覆盖。\n", ClientName)
	}

	if p.write {
		fmt.Fprintf(w, "\nsaved to: %s\n", p.identityPath)
	} else {
		fmt.Fprintf(w, "\nnot saved (dry run). pass --write to save.\n")
	}
}

// describeValues 把字段值拼成一行，便于在源报告里展示。
func describeValues(values detect.Values) string {
	parts := make([]string, 0, len(detect.AllFields()))
	for _, field := range detect.AllFields() {
		if v := values.Get(field); v != "" {
			parts = append(parts, string(field)+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// detectJSONOutput 是 --json 的输出结构。
//
// 字段刻意保持扁平且稳定：脚本会解析它。
type detectJSONOutput struct {
	CollectorProfile struct {
		Country   string `json:"country"`
		Province  string `json:"province"`
		City      string `json:"city"`
		ISP       string `json:"isp"`
		ASN       string `json:"asn"`
		IPVersion string `json:"ip_version"`
	} `json:"collector_profile"`

	FieldSources map[string]string `json:"field_sources"`

	Sources []struct {
		Name           string            `json:"name"`
		Description    string            `json:"description"`
		ExposesLocalIP bool              `json:"exposes_local_ip"`
		DurationMS     float64           `json:"duration_ms"`
		Error          string            `json:"error,omitempty"`
		Values         map[string]string `json:"values,omitempty"`
	} `json:"sources"`

	ElapsedMS float64 `json:"elapsed_ms"`
	Written   bool    `json:"written"`
}

// writeDetectJSON 输出 JSON 结果。written 表示这次是否真的写入了标识文件。
func writeDetectJSON(w io.Writer, result *detect.Result, written bool) error {
	out := detectJSONOutput{
		FieldSources: make(map[string]string, len(result.FieldSources)),
		ElapsedMS:    float64(result.Elapsed().Microseconds()) / 1000,
		Written:      written,
	}

	out.CollectorProfile.Country = result.Values.Country
	out.CollectorProfile.Province = result.Values.Province
	out.CollectorProfile.City = result.Values.City
	out.CollectorProfile.ISP = result.Values.ISP
	out.CollectorProfile.ASN = result.Values.ASN
	out.CollectorProfile.IPVersion = result.Values.IPVersion

	for field, source := range result.FieldSources {
		out.FieldSources[string(field)] = source
	}

	for _, report := range result.Reports {
		entry := struct {
			Name           string            `json:"name"`
			Description    string            `json:"description"`
			ExposesLocalIP bool              `json:"exposes_local_ip"`
			DurationMS     float64           `json:"duration_ms"`
			Error          string            `json:"error,omitempty"`
			Values         map[string]string `json:"values,omitempty"`
		}{
			Name:           report.Name,
			Description:    report.Description,
			ExposesLocalIP: report.ExposesLocalIP,
			DurationMS:     float64(report.Duration.Microseconds()) / 1000,
		}
		if report.Err != nil {
			entry.Error = report.Err.Error()
		} else {
			entry.Values = map[string]string{}
			for _, field := range detect.AllFields() {
				if v := report.Values.Get(field); v != "" {
					entry.Values[string(field)] = v
				}
			}
		}
		out.Sources = append(out.Sources, entry)
	}

	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, string(blob)); err != nil {
		return err
	}
	return nil
}
