// Package detect 检测"测量者自己在哪、用哪家运营商"。
//
// 它检测的是 CollectorProfile（采集者画像），**不是**目标 IP 的位置——
// 后者属于 model.Location，来自 all.json。两者含义完全不同。
//
// 设计原则（对应需求第 22 条）：
//
//  1. 自动检测只是辅助。公网 ASN 不一定等于用户实际感知的接入线路
//     （例如家宽走的是母公司的 ASN、企业出口走的是总部 ASN），
//     因此**手动配置优先**，自动检测只填补空白。
//  2. 每个检测源必须声明"我会把什么发给谁"。地理定位在原理上
//     需要向第三方暴露请求来源，因此这不是可以含糊过去的事：
//     Source 接口里有 Describe()，CLI 会把它打印给用户。
//  3. 不采集任何隐私信息：不读取 MAC、主机名、硬件标识、本地 IP、
//     默认路由。检测结果里只有省/市/运营商/ASN 这种粗粒度信息。
//  4. 单个源失败绝不影响其它源：只把它的字段标记为"未检测到"。
package detect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// 默认参数。
const (
	// DefaultTimeout 是单个检测源的整体超时。
	DefaultTimeout = 8 * time.Second

	// maxFieldLength 限制单个字段长度，避免把异常响应写进配置。
	maxFieldLength = 64
)

// Field 标识检测结果中的一个字段。
//
// 用显式枚举而不是让各源直接改 Result：这样"哪个字段是谁提供的"
// 可以被记录并在报告里展示，用户才能判断哪个值可信。
type Field string

const (
	// FieldCountry 是国家/地区代码（2 位大写）。
	FieldCountry Field = "country"

	// FieldProvince 是一级行政区。
	FieldProvince Field = "province"

	// FieldCity 是城市。
	FieldCity Field = "city"

	// FieldISP 是运营商名称。
	FieldISP Field = "isp"

	// FieldASN 是自治系统号（形如 AS9808）。
	FieldASN Field = "asn"

	// FieldIPVersion 是出口 IP 版本（ipv4 / ipv6）。
	FieldIPVersion Field = "ip_version"
)

// AllFields 返回全部字段（顺序固定，便于输出稳定）。
func AllFields() []Field {
	return []Field{
		FieldCountry, FieldProvince, FieldCity, FieldISP, FieldASN, FieldIPVersion,
	}
}

// Values 是一次检测得到的字段值。
//
// 零值表示"没有检测到"，与"检测到空字符串"是同一种情况——
// 这里刻意不做区分，因为对采集者画像来说两者没有实际差别。
type Values struct {
	Country   string
	Province  string
	City      string
	ISP       string
	ASN       string
	IPVersion string
}

// IsZero 报告是否一个字段都没有。
func (v Values) IsZero() bool {
	return v == Values{}
}

// Get 按字段名取值。
func (v Values) Get(f Field) string {
	switch f {
	case FieldCountry:
		return v.Country
	case FieldProvince:
		return v.Province
	case FieldCity:
		return v.City
	case FieldISP:
		return v.ISP
	case FieldASN:
		return v.ASN
	case FieldIPVersion:
		return v.IPVersion
	default:
		return ""
	}
}

// Set 按字段名赋值。
func (v *Values) Set(f Field, value string) {
	if v == nil {
		return
	}
	value = strings.TrimSpace(value)
	if len(value) > maxFieldLength {
		value = value[:maxFieldLength]
	}
	switch f {
	case FieldCountry:
		v.Country = value
	case FieldProvince:
		v.Province = value
	case FieldCity:
		v.City = value
	case FieldISP:
		v.ISP = value
	case FieldASN:
		v.ASN = value
	case FieldIPVersion:
		v.IPVersion = value
	}
}

// NonEmpty 返回非空字段的数量。
func (v Values) NonEmpty() int {
	n := 0
	for _, f := range AllFields() {
		if v.Get(f) != "" {
			n++
		}
	}
	return n
}

// ToProfile 转换成业务模型（会做归一化）。
func (v Values) ToProfile() model.CollectorProfile {
	profile := model.CollectorProfile{
		Country:   v.Country,
		Province:  v.Province,
		City:      v.City,
		ISP:       v.ISP,
		ASN:       v.ASN,
		IPVersion: model.IPVersion(v.IPVersion),
	}
	profile.Normalize()
	return profile
}

// FromProfile 从业务模型转换（会做归一化）。
func FromProfile(profile model.CollectorProfile) Values {
	profile.Normalize()
	return Values{
		Country:   profile.Country,
		Province:  profile.Province,
		City:      profile.City,
		ISP:       profile.ISP,
		ASN:       profile.ASN,
		IPVersion: string(profile.IPVersion),
	}
}

// ---------------------------------------------------------------------------
// Source
// ---------------------------------------------------------------------------

// Source 是一个检测源。
//
// 约定：
//
//   - Detect 只返回"它确实检测到的字段"，其余留空——绝不猜测；
//   - Detect 失败时返回 error，调用方会记录该源失败并继续其它源；
//   - Describe 必须**如实**说明它会向哪个外部地址发送请求、
//     以及本机 IP 是否因此被对方看到。这是隐私约束的落点：
//     用户必须能在命令行上看到这句话，而不是去读源码。
type Source interface {
	// Name 是源的短名字（用于日志与 --source 参数）。
	Name() string

	// Describe 返回一句人类可读的说明，包含它联系的外部服务。
	Describe() string

	// ExposesLocalIP 报告该源是否会让外部服务看到本机的公网 IP。
	//
	// 这是用户决定"要不要用这个源"的关键信息：
	// 地理定位类 API 按定义需要看到请求来源，因此为 true；
	// 纯本地推断为 false。
	ExposesLocalIP() bool

	// Detect 执行检测。
	Detect(ctx context.Context) (Values, error)
}

// ---------------------------------------------------------------------------
// Detector
// ---------------------------------------------------------------------------

// Options 是检测配置。
type Options struct {
	// Sources 是启用的检测源，按**优先级从高到低**排列。
	//
	// 先出现的源提供的字段不会被后面的源覆盖。
	Sources []Source

	// Manual 是手动配置的值（来自 CollectorProfile）。
	//
	// 手动配置**优先于所有检测源**：用户明确写下的值不该被自动检测
	// 覆盖。它同时也会出现在结果来源里，便于用户核对。
	Manual Values

	// Timeout 是单个检测源的超时（<=0 时使用 DefaultTimeout）。
	Timeout time.Duration

	// Now 允许注入当前时间（测试用）。
	Now func() time.Time
}

// SourceReport 是单个检测源的执行结果，用于向用户如实汇报。
type SourceReport struct {
	// Name 是源名字。
	Name string

	// Description 是源的说明（来自 Describe）。
	Description string

	// ExposesLocalIP 表示该源是否会让外部服务看到本机公网 IP。
	ExposesLocalIP bool

	// Values 是该源检测到的字段。
	Values Values

	// Err 是失败原因；为 nil 表示成功。
	Err error

	// Duration 是该源的耗时。
	Duration time.Duration

	// Used 表示本次是否真的执行了（未被 --source 过滤掉）。
	Used bool
}

// Succeeded 报告该源是否执行成功。
func (r SourceReport) Succeeded() bool { return r.Used && r.Err == nil }

// Result 是一次完整检测的结果。
type Result struct {
	// Values 是合并后的字段值。
	Values Values

	// Profile 是转换后的采集者画像（已归一化）。
	Profile model.CollectorProfile

	// Sources 是每个字段的提供者（字段 -> 源名字）。
	//
	// manual 表示该值来自用户配置，其余是检测源的名字。
	FieldSources map[Field]string

	// Reports 是各源的执行报告（顺序与 Options.Sources 一致）。
	Reports []SourceReport

	// StartedAt / FinishedAt 是本次检测的起止时间。
	StartedAt  time.Time
	FinishedAt time.Time
}

// Elapsed 返回本次检测耗时。
func (r Result) Elapsed() time.Duration {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// ExposureNote 汇总"哪些启用的源会暴露本机 IP"。
//
// 供 CLI 在真正发起请求**之前**打印，让用户有机会中止。
func (r Result) ExposureNote() string {
	var exposing []string
	var local []string
	for _, report := range r.Reports {
		if !report.Used {
			continue
		}
		if report.ExposesLocalIP {
			exposing = append(exposing, report.Name)
		} else {
			local = append(local, report.Name)
		}
	}

	switch {
	case len(exposing) == 0 && len(local) == 0:
		return "no detection source enabled"
	case len(exposing) == 0:
		return "none of the enabled sources (" + strings.Join(local, ", ") + ") contact an external service"
	default:
		return "these sources will send a request to an external service, which will see your public IP: " +
			strings.Join(exposing, ", ")
	}
}

// DescribeSources 返回各启用源的自述，供 CLI 展示。
func (r Result) DescribeSources() []string {
	out := make([]string, 0, len(r.Reports))
	for _, report := range r.Reports {
		if !report.Used {
			continue
		}
		line := fmt.Sprintf("%s: %s", report.Name, report.Description)
		if report.ExposesLocalIP {
			line += " [sends your public IP to that service]"
		} else {
			line += " [does not contact an external service with your IP]"
		}
		out = append(out, line)
	}
	return out
}

// ErrNoSource 表示没有任何启用的检测源。
var ErrNoSource = errors.New("detect: no detection source enabled")

// Detect 依次执行各检测源并合并结果。
//
// 合并规则（按优先级）：
//
//  1. 手动配置的字段最优先，永不被覆盖；
//  2. 其余字段按 Sources 的顺序，**先出现的源优先**；
//  3. 每个字段记录它的提供者，便于用户在报告里核对。
//
// 单个源失败只被记录，不影响其它源，也不会让 Detect 返回错误——
// "某个 API 挂了"不应该让整个检测失败。
// 只有"没有任何可用源"才返回错误。
func Detect(ctx context.Context, opts Options) (*Result, error) {
	if len(opts.Sources) == 0 {
		return nil, ErrNoSource
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	result := &Result{
		FieldSources: make(map[Field]string, len(AllFields())),
		Reports:      make([]SourceReport, 0, len(opts.Sources)),
		StartedAt:    opts.Now().UTC(),
	}

	// 1) 先落手动配置：它永远是最高优先级。
	for _, field := range AllFields() {
		if value := opts.Manual.Get(field); value != "" {
			result.Values.Set(field, value)
			result.FieldSources[field] = "manual"
		}
	}

	// 2) 依次执行各源。
	for _, source := range opts.Sources {
		report := runSource(ctx, source, opts.Timeout, opts.Now)
		result.Reports = append(result.Reports, report)

		if report.Err != nil {
			continue
		}
		mergeValues(&result.Values, result.FieldSources, report.Values, report.Name)
	}

	result.Profile = result.Values.ToProfile()
	result.FinishedAt = opts.Now().UTC()
	return result, nil
}

// runSource 执行单个源并测量耗时。
//
// 每个源都有独立的超时：一个卡住的 API 不应该拖住整个检测。
func runSource(ctx context.Context, source Source, timeout time.Duration, now func() time.Time) SourceReport {
	report := SourceReport{
		Name:           source.Name(),
		Description:    source.Describe(),
		ExposesLocalIP: source.ExposesLocalIP(),
		Used:           true,
	}

	// 用 context.WithoutCancel 保留父 ctx 的值（例如追踪信息），
	// 但不受父 ctx 取消影响——真正的取消由下面的 Timeout 负责，
	// 而 CLI 层的 Ctrl+C 会通过父 ctx 生效。
	sourceCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := now()
	values, err := source.Detect(sourceCtx)
	report.Duration = now().Sub(start)

	if err != nil {
		// 超时错误统一成一个可读的形式，避免把
		// "context deadline exceeded" 原样丢给用户。
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		report.Err = err
		return report
	}
	report.Values = values
	return report
}

// mergeValues 把 src 的字段补进 dst（只填补空字段）。
func mergeValues(dst *Values, fieldSources map[Field]string, src Values, sourceName string) {
	for _, field := range AllFields() {
		value := src.Get(field)
		if value == "" {
			continue
		}
		// 已被手动配置或更高优先级的源填充：不覆盖。
		if dst.Get(field) != "" {
			continue
		}
		dst.Set(field, value)
		fieldSources[field] = sourceName
	}
}

// ---------------------------------------------------------------------------
// 源集合
// ---------------------------------------------------------------------------

// AllSources 返回全部内置源，顺序即默认优先级。
//
// 顺序理由：先离线、后联网。离线源不向任何外部服务透露本机 IP，
// 因此应当先跑；联网源（geo-IP）只在离线源留空时才需要。
//
// geoIPEndpoint 为空时不会构造 geo-IP 源。
func AllSources(geoIPEndpoint string) []Source {
	sources := []Source{NewLocalSource()}
	if strings.TrimSpace(geoIPEndpoint) != "" {
		sources = append(sources, NewGeoIPSource(geoIPEndpoint, nil))
	}
	return sources
}

// FilterSources 按名字筛选源（名字不区分大小写）。
//
// 用于 --source 参数。未知名字返回错误，而不是静默忽略——
// 静默忽略会让用户以为某个源被启用了。
func FilterSources(sources []Source, names []string) ([]Source, error) {
	if len(names) == 0 {
		return sources, nil
	}

	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[strings.ToLower(strings.TrimSpace(name))] = true
	}

	available := make(map[string]bool, len(sources))
	for _, source := range sources {
		available[strings.ToLower(source.Name())] = true
	}

	var unknown []string
	for name := range wanted {
		if !available[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		// 排序保证错误信息稳定。
		sortStrings(unknown)
		return nil, fmt.Errorf("unknown detection source(s): %s (available: %s)",
			strings.Join(unknown, ", "), strings.Join(sourceNames(sources), ", "))
	}

	out := make([]Source, 0, len(sources))
	for _, source := range sources {
		if wanted[strings.ToLower(source.Name())] {
			out = append(out, source)
		}
	}
	return out, nil
}

// sourceNames 返回全部源名字。
func sourceNames(sources []Source) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		out = append(out, source.Name())
	}
	return out
}

// sortStrings 对字符串切片做插入排序。
//
// 只用于几条源名字，不值得引入 sort。
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
