// Command write-cache 写入一份 cf-route-tester 的目标缓存文件。
//
// 用途：给 CI 的端到端冒烟测试准备一份**确定性的**目标列表，
// 不依赖外网数据源（CI 的失败原因必须是代码问题，而不是
// "上游今天挂了"或"那个公网 IP 不通"）。
//
// 为什么不直接把 JSON 交给 --cache：--cache 读的是本工具自己的
// 缓存格式（由 source.WriteCache 写出，含 meta/stats/targets），
// 不是上游的原始 all.json。手工拼那个格式必然与实现漂移，
// 因此这里调用真实的写缓存函数。
//
// 这是**测试夹具工具**，不是产品功能。
//
// 用法：
//
//	go run ./tools/write-cache -out /tmp/all.json -target 1.1.1.1:80 -target 8.8.8.8:53
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/source"
)

// targetList 收集可重复出现的 -target。
type targetList []string

func (l *targetList) String() string { return strings.Join(*l, ",") }

func (l *targetList) Set(value string) error {
	*l = append(*l, strings.TrimSpace(value))
	return nil
}

func main() {
	var (
		out     string
		targets targetList
		url     string
	)

	flag.StringVar(&out, "out", "", "缓存输出路径（必填）")
	flag.StringVar(&url, "url", source.DefaultURL, "缓存里记录的来源 URL（必须与命令行的 --url 一致）")
	flag.Var(&targets, "target", "目标 IP:Port（可重复）")
	flag.Parse()

	if strings.TrimSpace(out) == "" {
		fmt.Fprintln(os.Stderr, "Error: -out is required")
		os.Exit(2)
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "Error: at least one -target is required")
		os.Exit(2)
	}

	parsed := make([]model.Target, 0, len(targets))
	for _, raw := range targets {
		target, err := parseTarget(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(2)
		}
		parsed = append(parsed, target)
	}

	if err := model.ValidateAll(parsed); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid target: %v\n", err)
		os.Exit(2)
	}

	// meta.URL 必须与命令行的 --url 一致：缓存与来源不匹配时
	// source.Loader 会判定"缓存属于另一个源"并转而联网——
	// 那会让"离线测试"悄悄变成真实网络请求。
	generatedAt := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	meta := source.SourceMeta{
		URL:                  url,
		Format:               "json",
		GeneratorGeneratedAt: generatedAt,
		HasGeneratorTime:     true,
		ReportedCount:        len(parsed),
	}

	stats := source.ParseStats{
		RawItems:         len(parsed),
		TargetCandidates: len(parsed),
		RawCombos:        len(parsed),
	}

	if err := source.WriteCache(out, meta, stats, parsed); err != nil {
		fmt.Fprintf(os.Stderr, "Error: write cache: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s with %d target(s)\n", out, len(parsed))
	for _, target := range parsed {
		fmt.Printf("  %s\n", target.ID)
	}
}

// parseTarget 解析 "IP:Port"。
//
// 只支持带端口的形式：这个工具是给 CI 夹具用的，
// 让"忘了写端口"这种输入直接报错比猜一个默认端口更好。
func parseTarget(raw string) (model.Target, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return model.Target{}, fmt.Errorf("%s: want IP:PORT", raw)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return model.Target{}, fmt.Errorf("%s: bad port %q", raw, portText)
	}
	target, err := model.NewTargetFromStrings(host, port)
	if err != nil {
		return model.Target{}, fmt.Errorf("%s: %w", raw, err)
	}
	return target, nil
}
