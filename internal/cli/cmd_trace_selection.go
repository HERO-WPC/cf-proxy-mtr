package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/asnprefix"
	"github.com/cf-route-tester/cf-route-tester/internal/service"
)

// runTraceFromSelection 实现"先测 TCP、再挑一批跟踪"的第二步（命令行侧）。
//
// 用法：
//
//	cf-route-tester scan  --country US --out results.csv
//	cf-route-tester trace --from results.csv --country US --max-latency 200ms --limit 10
//
// 与图形界面走**同一个** service.RunTraceSelection：两边各写一套
// 选取逻辑，迟早会在"什么算符合条件的行"上分歧，而那时使用者
// 看到的是"界面说 12 个、命令行跟了 9 个"。
func runTraceFromSelection(ctx context.Context, env *Env, p traceParams) error {
	path := strings.TrimSpace(p.from)

	// 先预览一次：既能提前报出"一个都没选到"（避免白等引擎初始化），
	// 也让命令行把**将要跟踪什么**说清楚——跟踪一次十几秒，
	// 使用者应该在开始前就知道选了哪些。
	preview, err := service.PreviewTraceSelection(
		path, p.countries, p.maxLatency.d.Seconds()*1000, p.limit)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if !p.quiet {
		printSelectionPreview(env, path, preview, p)
	}

	if preview.Count == 0 {
		// 不是错误：条件太紧而已。退出码保持 0，让脚本能区分
		// "没有符合条件的"与"出错了"。
		fmt.Fprintln(env.Stdout, "没有符合条件的目标，未做跟踪。")
		return nil
	}

	svc := newServiceForTrace(env, p)

	result, err := svc.RunTraceSelection(ctx, service.TraceSelectionOptions{
		CSVPath:      path,
		Countries:    p.countries,
		MaxLatencyMS: p.maxLatency.d.Seconds() * 1000,
		Limit:        p.limit,
		TraceConfig: service.TraceOptions{
			Binary:       p.binary,
			Mode:         p.mode,
			DataProvider: p.dataProvider,
			PowProvider:  p.powProvider,
			// 命令行默认自动下载；--no-download 关掉。
			AutoDownload: !p.noDownload,
			DownloadDir:  p.downloadDir,
			Workers:      p.workers,
			Timeout:      p.timeout.d,
		},
		ASNPrefixOptions: asnprefix.Options{},
	})
	if err != nil {
		return err
	}

	if result.TraceUnavailable != "" {
		// 与扫描一致：引擎不可用不算失败，但必须说清楚。
		fmt.Fprintln(env.Stderr, "Error: trace engine unavailable: "+result.TraceUnavailable)
		return nil
	}

	if !p.quiet {
		fmt.Fprintf(env.Stdout, "已跟踪 %d 个目标（成功 %d），追加写入 %s\n",
			result.Traced, result.TracedOK, result.OutputPath)
	}
	if result.Interrupted {
		fmt.Fprintln(env.Stderr, "跟踪被中断；已完成的线路信息都已写入文件。")
	}
	return nil
}

// printSelectionPreview 说明"按当前条件会跟踪哪些目标"。
func printSelectionPreview(env *Env, path string, preview service.SelectionPreview, p traceParams) {
	conditions := make([]string, 0, 3)
	if len(p.countries) > 0 {
		conditions = append(conditions, "国家="+strings.Join(p.countries, ","))
	}
	if p.maxLatency.d > 0 {
		conditions = append(conditions, "延迟上限="+p.maxLatency.d.String())
	}
	if p.limit > 0 {
		conditions = append(conditions, fmt.Sprintf("最多 %d 个", p.limit))
	}
	if len(conditions) == 0 {
		conditions = append(conditions, "不限")
	}

	fmt.Fprintf(env.Stdout, "按 %s 从 %s 中选出 %d 个目标（可测行 %d）\n",
		strings.Join(conditions, " "), path, preview.Count, preview.Total)
	if preview.Count > 0 {
		fmt.Fprintf(env.Stdout, "延迟范围 %.1f ~ %.1f ms\n", preview.FastestMS, preview.SlowestMS)
	}
}

// newServiceForTrace 造一个只用于跟踪的 Service。
//
// 跟踪不需要数据源：目标来自 CSV，而线路名由本地 ASN 前缀判断。
// 因此这里刻意**不**配置 source，避免为一个用不到的东西去联网。
func newServiceForTrace(env *Env, p traceParams) *service.Service {
	return service.New(service.Options{
		Logf: func(format string, args ...any) {
			// 不加前缀：service 的日志行自己已经带上了阶段名（如 "trace: ..."），
			// 再加一次会变成 "trace: trace: ..."。
			fmt.Fprintf(env.Stderr, format+"\n", args...)
		},
	})
}
