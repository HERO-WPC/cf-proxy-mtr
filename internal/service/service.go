// Package service 承载与**界面无关**的编排逻辑。
//
// 背景：这个项目原本只有命令行界面，编排逻辑（加载目标 → 开数据库 →
// 决定会话 → 跑 scheduler → 记录会话）直接长在 internal/cli 的
// 命令实现里，和 flag 解析、终端输出、退出码绑在一起。
//
// 要加图形界面时，这个结构会逼出两个坏选择：
//
//  1. 图形界面去调用自己的命令行（解析 stdout 拿进度、靠字符串匹配
//     识别错误）—— 脆弱，且在 Windows 上会闪控制台窗口；
//  2. 把编排逻辑复制一份给 HTTP 处理器 —— 然后"CLI 的续测判据"
//     和"界面的续测判据"迟早分叉，而这类分叉在这条链路上是
//     **静默的数据问题**（不是崩溃，是悄悄测错）。
//
// 所以这里把编排抽出来，CLI 与图形界面都调用同一份实现。
// 附带好处：这条链路终于可以用纯 Go 测试直接打，不必经过命令行。
//
// 与 internal/cli 的分工：
//
//	internal/cli       flag 解析、给人看的输出、退出码
//	internal/service   干什么（本包）
//	internal/<能力包>  怎么干（scheduler / storage / export / ...）
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/identity"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/scheduler"
	"github.com/cf-route-tester/cf-route-tester/internal/source"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// Options 是服务层的配置。零值应当可用（走默认值）。
type Options struct {
	// DBPath 是本地 SQLite 路径。
	DBPath string

	// IdentityPath 是本地匿名标识文件路径。
	IdentityPath string

	// Source 是目标列表的来源配置。
	Source source.Config

	// Logf 是可选的日志回调（nil 表示不记录）。
	Logf func(format string, args ...any)
}

// Service 是编排入口。
//
// 它**不持有**打开的数据库连接：每次操作自己开关，
// 因为图形界面可能长时间空闲，而 SQLite 连接持有着 WAL 文件。
type Service struct {
	opts Options
}

// New 创建服务。
func New(opts Options) *Service {
	return &Service{opts: opts}
}

// Options 返回当前配置（只读用途）。
func (s *Service) Options() Options { return s.opts }

// log 记录一条日志（如果有回调）。
func (s *Service) log(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// ---------------------------------------------------------------------------
// 扫描
// ---------------------------------------------------------------------------

// ScanOptions 是一次扫描的参数。
//
// 字段与 CLI 的 --flag 一一对应，但**不带**任何"输出格式"相关的设置：
// 打印什么、怎么打印由调用方决定。
type ScanOptions struct {
	// Workers 是 TCP 探测并发数（<=0 用默认值）。
	Workers int

	// Timeout 是单个 TCP 连接超时（<=0 用默认值）。
	Timeout time.Duration

	// Limit 限制本次测量的目标数（<=0 表示全部）。
	//
	// 它取目标列表的**前 N 个**（保持源顺序），而不是随机抽样：
	// 这样同一个 limit 在不同机器上测的是同一批目标，可比较。
	Limit int

	// SessionID 指定会话 ID（通常由 Resume 推导，一般不用手动设）。
	SessionID string

	// Resume 为真时继续已有会话而不是开新会话。
	Resume bool

	// NewSession 为真时强制开新会话。
	NewSession bool

	// Collector 是手动指定的采集者信息（优先于自动检测）。
	Collector model.CollectorProfile

	// Trace 为真时在探测之后执行线路跟踪。
	Trace bool

	// TraceConfig 是跟踪配置（仅 Trace 为真时使用）。
	TraceConfig TraceOptions

	// Progress 接收进度回调（nil 表示不关心）。
	//
	// 回调会在消费测量结果的 goroutine 上被调用，实现必须尽快返回。
	Progress func(scheduler.ProgressEvent)

	// OnTarget 在每个目标**开始**探测时调用（nil 表示不关心）。
	//
	// 与 Progress 的分工：Progress 回答"完成多少"，
	// OnTarget 回答"现在在测哪个"。界面需要后者才能显示当前目标。
	//
	// 它会被多个 worker **并发**调用，实现必须线程安全且尽快返回。
	OnTarget func(target model.Target)

	// OnTrace 在每个目标的线路跟踪完成时调用（nil 表示不关心）。
	//
	// 与 OnTarget 配对，让界面能显示"这条线路长什么样"。
	OnTrace func(target model.Target, result *trace.TraceResult)
}

// TraceOptions 是线路跟踪的配置。
type TraceOptions struct {
	// Binary 是 NextTrace 可执行文件路径（空表示从 PATH 查找）。
	Binary string

	// Mode 是跟踪模式：tcp / icmp / udp（空表示 tcp）。
	Mode string

	// Workers 是跟踪并发数（<=0 用默认值）。
	Workers int

	// Timeout 是单个跟踪进程的超时（<=0 用默认值）。
	Timeout time.Duration
}

// ScanResult 是一次扫描的结果。
type ScanResult struct {
	// Result 是调度器的原始结果。内嵌是为了让调用方
	// 直接读 SessionID / Probe / Trace 等字段，而不必层层转发。
	scheduler.Result

	// TargetsConsidered 是本次扫描考虑的目标数（应用 Limit 之后）。
	TargetsConsidered int

	// SourceURL 是实际使用的数据源地址。
	SourceURL string

	// SourceFromCache 表示目标列表来自本地缓存而不是网络。
	SourceFromCache bool

	// SourceGeneratedAt 是上游数据自称的生成时间。
	SourceGeneratedAt time.Time

	// TraceUnavailable 说明"用户要求跟踪但引擎不可用"的原因。
	//
	// 非空时 TraceSkipped 为真。保留原文而不是一个布尔值，
	// 是为了让界面能直接展示"为什么不行"（没装？路径错了？）。
	TraceUnavailable string

	// LastSessionSaved 表示"这次会话已被记入本地标识"。
	//
	// 为假时下次 --resume 不会自动找到它。这个问题必须暴露给用户，
	// 否则他会以为续测能用。
	LastSessionSaved bool

	// LastSessionError 记录记忆会话失败的原因。
	LastSessionError string
}

// RunScan 执行一次完整扫描。
//
// 顺序经过刻意安排（这是踩过的坑）：
//
//  1. 先校验**用法类**参数。放在后面的话，参数写错时用户会先经历
//     下载目标、开库、建会话，最后才看到那条错误；更糟的是当目标
//     列表为空时他根本看不到它，只会去排查无关方向。
//  2. 再加载目标列表。
//  3. 再开数据库。
func (s *Service) RunScan(ctx context.Context, opts ScanOptions) (*ScanResult, error) {
	// ---- 1) 用法校验（必须在任何副作用之前） ----
	if opts.Resume && opts.NewSession {
		return nil, fmt.Errorf("%w: --resume and --new are mutually exclusive", ErrUsage)
	}
	if opts.Limit < 0 {
		return nil, fmt.Errorf("%w: --limit must not be negative", ErrUsage)
	}
	if strings.TrimSpace(s.opts.DBPath) == "" {
		return nil, fmt.Errorf("%w: --db must not be empty (scan stores its results)", ErrUsage)
	}
	if opts.Trace {
		// 模式在下载与开库之前就要确认合法。
		if _, err := trace.Mode(opts.TraceConfig.Mode).Normalize(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUsage, err)
		}
	}

	// ---- 2) 目标列表（缓存优先） ----
	targets, meta, fromCache, err := s.loadTargets(ctx)
	if err != nil {
		return nil, err
	}
	if opts.Limit > 0 && opts.Limit < len(targets) {
		targets = targets[:opts.Limit]
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w: no targets to scan (source %s returned an empty list)", ErrNoTargets, meta.URL)
	}
	s.log("scan: %d target(s) from %s (cache=%v)", len(targets), meta.URL, fromCache)

	// ---- 3) 本地匿名标识 ----
	local, err := s.resolveIdentity(opts.Collector)
	if err != nil {
		return nil, err
	}

	// ---- 4) 数据库 ----
	store, err := storage.Open(ctx, storage.Config{Path: s.opts.DBPath})
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			s.log("scan: closing database: %v", cerr)
		}
	}()

	collectorPK, err := store.UpsertCollector(ctx, local.CollectorID, local.Profile.ToModel(), version.Version)
	if err != nil {
		return nil, err
	}

	// ---- 5) 决定会话 ----
	sessionID, resume, err := resolveSession(ctx, store, collectorPK, opts, local)
	if err != nil {
		return nil, err
	}

	// ---- 6) 装配调度器 ----
	cfg := scheduler.Config{
		Probe:       probe.DefaultConfig(),
		CollectorPK: collectorPK,
		SessionID:   sessionID,
		Resume:      resume,
		Trace:       opts.Trace,
		OnTarget:    opts.OnTarget,
		OnTrace:     opts.OnTrace,
	}
	if opts.Workers > 0 {
		cfg.Probe.Workers = opts.Workers
	}
	if opts.Timeout > 0 {
		cfg.Probe.Timeout = opts.Timeout
	}

	// 跟踪引擎不可用时**不终止扫描**：TCP 测量本身仍有价值，
	// 而"机器上没装 nexttrace"是最常见的情况之一。
	// 明确记下原因，由调用方如实展示。
	traceUnavailable := ""
	if opts.Trace {
		engine, engineErr := s.buildTraceEngine(ctx, opts.TraceConfig)
		if engineErr != nil {
			cfg.TraceEngine = nil
			traceUnavailable = engineErr.Error()
			s.log("scan: trace engine unavailable: %v", engineErr)
		} else {
			cfg.TraceEngine = engine
		}
	}

	sched, err := scheduler.New(store, cfg)
	if err != nil {
		return nil, err
	}
	if opts.Progress != nil {
		sched.SetProgress(opts.Progress)
	}

	// 目标元数据必须先入库：测量有外键指向 targets。
	// UPSERT 是幂等的，重复扫描不会产生重复目标。
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		return nil, err
	}

	// ---- 7) 跑 ----
	result, err := sched.Run(ctx, targets, version.Version)
	if err != nil {
		if resume && scheduler.LooksLikeResumeError(err) {
			return nil, fmt.Errorf("%w\n提示：开始一次新扫描，或查看 'db stats' 确认会话状态", err)
		}
		return nil, err
	}

	out := &ScanResult{
		Result:            *result,
		TargetsConsidered: len(targets),
		SourceURL:         meta.URL,
		SourceFromCache:   fromCache,
		SourceGeneratedAt: meta.GeneratorGeneratedAt,
		TraceUnavailable:  traceUnavailable,
	}

	// 记住这次会话，下次续测无需手工指定 ID。
	// 写失败不影响扫描结果，但必须如实上报：否则用户下次续测
	// 会莫名其妙地找不到会话。
	if err := identity.SetLastSession(s.opts.IdentityPath, result.SessionID); err != nil {
		out.LastSessionError = err.Error()
		s.log("scan: could not record last session: %v", err)
	} else {
		out.LastSessionSaved = true
	}

	s.log("scan: session %s finished (probe ok=%d fail=%d stored=%d traces=%d)",
		result.SessionID, result.Probe.Success, result.Probe.Failed, result.Stored, result.TraceStored)

	return out, nil
}

// loadTargets 加载目标列表（缓存优先），并做隐私/合法性过滤。
func (s *Service) loadTargets(ctx context.Context) ([]model.Target, source.SourceMeta, bool, error) {
	loader, err := source.NewLoader(s.opts.Source)
	if err != nil {
		return nil, source.SourceMeta{}, false, err
	}

	result, err := loader.Load(ctx, source.LoadOptions{})
	if err != nil {
		return nil, source.SourceMeta{}, false, err
	}
	return result.Targets, result.Meta, result.FromCache, nil
}

// resolveIdentity 读取本地标识，并让调用方给出的画像覆盖其中的非空项。
//
// 与 CLI 的 resolveIdentity 是同一套语义（手动值优先、先归一化再校验、
// 变了才落盘）。抽到这里是为了让命令行与图形界面不会各自演化出
// 一套"合并规则"——那类分叉会直接变成数据里的地区/运营商错误。
func (s *Service) resolveIdentity(overrides model.CollectorProfile) (*identity.Local, error) {
	local, err := identity.Load(s.opts.IdentityPath)
	if err != nil {
		return nil, err
	}

	merged := local.Profile
	if v := strings.TrimSpace(overrides.Country); v != "" {
		merged.Country = v
	}
	if v := strings.TrimSpace(overrides.Province); v != "" {
		merged.Province = v
	}
	if v := strings.TrimSpace(overrides.City); v != "" {
		merged.City = v
	}
	if v := strings.TrimSpace(overrides.ISP); v != "" {
		merged.ISP = v
	}
	// ASN 与 IPVersion 在模型层的类型与文件里的字符串不同
	// （模型层有类型约束），因此都转成字符串后按"非空即覆盖"处理。
	if v := strings.TrimSpace(overrides.ASN); v != "" {
		merged.ASN = v
	}
	if v := strings.TrimSpace(string(overrides.IPVersion)); v != "" {
		merged.IPVersion = v
	}

	if merged == local.Profile {
		return local, nil
	}

	// 顺序很重要：用户手写 "cn" 是完全合理的输入，
	// 若先校验就会因为"必须是大写"而被拒绝——那是在为难用户。
	// 归一化后再校验，非法值（例如 3 位国家代码）依然被挡住。
	profile := merged.ToModel()
	profile.Normalize()
	if err := profile.Validate(); err != nil {
		return nil, fmt.Errorf("%w: collector profile: %v", ErrUsage, err)
	}

	local.Profile = identity.FromModel(profile)
	if err := identity.Save(s.opts.IdentityPath, local); err != nil {
		return nil, err
	}
	s.log("identity: collector profile updated from overrides")
	return local, nil
}

// buildTraceEngine 构造线路跟踪引擎。
//
// 构造时会查询一次引擎版本（可在测试里跳过）。路径写错、没装、
// 没有执行权限都在这里暴露，而不是等到每个目标都失败之后
// ——后者会让用户以为"线路全都不通"。
func (s *Service) buildTraceEngine(ctx context.Context, opts TraceOptions) (*trace.NextTraceEngine, error) {
	engineOpts := trace.EngineOptions{
		BinaryPath: strings.TrimSpace(opts.Binary),
	}
	if opts.Timeout > 0 {
		engineOpts.Timeout = opts.Timeout
	}
	if strings.TrimSpace(opts.Mode) != "" {
		mode, err := trace.Mode(opts.Mode).Normalize()
		if err != nil {
			return nil, err
		}
		engineOpts.Mode = mode
	}
	return trace.NewNextTraceEngine(ctx, engineOpts)
}

// ---------------------------------------------------------------------------
// 会话
// ---------------------------------------------------------------------------

// resolveSession 决定本次扫描使用的会话 ID。
//
// 语义与 CLI 完全一致（抽出来就是为了两边不会漂移）：
//
//	--resume            继续：优先用本地记住的会话；没有就找数据库里
//	                    最近一个未结束的会话
//	--session <id>      指定 ID 但**没加** --resume：理解为续测这个会话
//	                    （想新建的话 ID 由程序生成即可，没必要手写）
//	--new + --session   冲突，明确报错
//	默认                新建会话
func resolveSession(
	ctx context.Context,
	store *storage.Store,
	collectorPK int64,
	opts ScanOptions,
	local *identity.Local,
) (string, bool, error) {
	if strings.TrimSpace(opts.SessionID) != "" && opts.NewSession {
		return "", false, fmt.Errorf("%w: --session and --new are mutually exclusive", ErrUsage)
	}

	switch {
	case opts.Resume:
		id := strings.TrimSpace(opts.SessionID)
		if id == "" {
			// 优先用本地记住的会话；没有就退回数据库里最近一个未结束的。
			// 这条兜底很重要：身份文件可能被删掉或换了路径，
			// 而数据库里明明有一次中断的扫描在等着续测。
			id = strings.TrimSpace(local.LastSessionID)
			if id == "" {
				latest, err := store.LatestOpenSession(ctx, collectorPK)
				if err != nil {
					return "", false, fmt.Errorf(
						"%w: no session to resume (specify a session ID, or run a scan first): %v",
						ErrNoSession, err)
				}
				id = latest.ID
			}
		}
		return id, true, nil

	case strings.TrimSpace(opts.SessionID) != "":
		return opts.SessionID, true, nil

	default:
		id, err := model.NewSessionID(time.Now().UTC())
		if err != nil {
			return "", false, err
		}
		return id, false, nil
	}
}

// ---------------------------------------------------------------------------
// 状态查询
// ---------------------------------------------------------------------------

// Stats 是本地数据库的概览。
//
// 刻意**直接复用** storage.Stats 而不是定义一套平行字段：
// 图形界面与 `db stats` 必须显示同一组数字，
// 两套结构迟早会漂移成一个显示 20 条、一个显示 21 条。
type Stats struct {
	// Storage 是数据库自身的统计（与 `db stats` 同源）。
	Storage storage.Stats

	// LatestSession 是最近一次会话的摘要（可能为 nil）。
	LatestSession *storage.SessionSummary

	// SessionCount 是会话总数。
	SessionCount int64
}

// Stats 打开数据库并统计。
func (s *Service) Stats(ctx context.Context) (*Stats, error) {
	if strings.TrimSpace(s.opts.DBPath) == "" {
		return nil, fmt.Errorf("%w: no database configured", ErrUsage)
	}

	store, err := storage.Open(ctx, storage.Config{Path: s.opts.DBPath})
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	inner, err := store.CollectStats(ctx)
	if err != nil {
		return nil, err
	}

	out := &Stats{Storage: inner}
	out.SessionCount = inner.Tables["scan_sessions"]

	// 最近会话是附加信息：取不到不该让整个统计失败。
	//
	// collectorPK=0 表示"所有采集者"（本机通常只有一个）。
	sessions, err := store.ListSessions(ctx, 0)
	if err != nil {
		s.log("stats: list sessions: %v", err)
	} else if len(sessions) > 0 {
		latest := sessions[0]
		out.LatestSession = &latest
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// 这些错误用于让调用方区分"用户写错了"与"环境/数据有问题"，
// 从而给出不同的提示与退出码（CLI）或不同的 HTTP 状态码（界面）。
var (
	// ErrUsage 表示参数有问题（用户可修正）。
	ErrUsage = errors.New("usage error")

	// ErrNoTargets 表示数据源返回了空列表。
	ErrNoTargets = errors.New("no targets")

	// ErrNoSession 表示没有可续测的会话。
	ErrNoSession = errors.New("no session to resume")
)

// IsUsage 判断错误是否属于"参数问题"。
func IsUsage(err error) bool { return errors.Is(err, ErrUsage) }

// IsNoTargets 判断错误是否属于"没有目标可测"。
func IsNoTargets(err error) bool { return errors.Is(err, ErrNoTargets) }

// IsNoSession 判断错误是否属于"没有可续测的会话"。
func IsNoSession(err error) bool { return errors.Is(err, ErrNoSession) }

// ---------------------------------------------------------------------------
// 让进度回调可以被多个观察者订阅
// ---------------------------------------------------------------------------

// ProgressHub 把一次扫描的进度事件广播给多个订阅者。
//
// 为什么需要它：调度器的进度回调是**单个函数**，而图形界面可能
// 同时有多个客户端（多个标签页、刷新后重连的页面）。
// 直接把回调替换掉会让先连上的客户端永远停在旧进度。
//
// 语义要点：
//
//   - Publish 绝不阻塞：慢订阅者会**丢事件**而不是拖慢扫描。
//     进度是"最新状态"，丢中间态无害；卡住扫描有害。
//   - 每个订阅者收到的是一个带缓冲的通道，缓冲区满时丢弃最旧的事件。
type ProgressHub struct {
	mu   sync.Mutex
	subs map[int]chan scheduler.ProgressEvent
	next int

	// last 保存最近一次事件，供后连上的订阅者立刻看到当前状态。
	last    *scheduler.ProgressEvent
	lastSet bool
}

// NewProgressHub 创建广播中心。
func NewProgressHub() *ProgressHub {
	return &ProgressHub{subs: make(map[int]chan scheduler.ProgressEvent)}
}

// Subscribe 订阅进度事件。
//
// 返回的通道会立刻收到一条"当前状态"的事件（如果已有的话），
// 这样新连上的页面不会空白等待到下一次进度更新。
// 调用方必须调用返回的 cancel 函数，否则订阅者会一直留在表里。
func (h *ProgressHub) Subscribe() (<-chan scheduler.ProgressEvent, func()) {
	if h == nil {
		ch := make(chan scheduler.ProgressEvent)
		close(ch)
		return ch, func() {}
	}

	// 缓冲区小是刻意的：它是"来不及消费"的信号，
	// 满了就丢事件，而不是让扫描等这个订阅者。
	ch := make(chan scheduler.ProgressEvent, 16)

	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = ch
	var snapshot *scheduler.ProgressEvent
	if h.lastSet && h.last != nil {
		event := *h.last
		snapshot = &event
	}
	h.mu.Unlock()

	if snapshot != nil {
		select {
		case ch <- *snapshot:
		default:
		}
	}

	cancel := func() {
		h.mu.Lock()
		if existing, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(existing)
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

// Publish 广播一次进度。
//
// 它**不会阻塞**：这是刻意的。订阅者消费不过来时宁可丢事件，
// 也不能让整个扫描停下来等一个慢客户端。
func (h *ProgressHub) Publish(event scheduler.ProgressEvent) {
	if h == nil {
		return
	}

	h.mu.Lock()
	h.last = &event
	h.lastSet = true
	// 复制一份订阅者列表再解锁发送：避免在持锁期间阻塞。
	subs := make([]chan scheduler.ProgressEvent, 0, len(h.subs))
	for _, ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- event:
		default:
			// 订阅者满了：丢掉**最旧**的一条腾出位置，再放最新的。
			// 保留最新状态比保留完整历史有用。
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- event:
			default:
			}
		}
	}
}

// Last 返回最近一次进度事件。
func (h *ProgressHub) Last() (scheduler.ProgressEvent, bool) {
	if h == nil {
		return scheduler.ProgressEvent{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.lastSet || h.last == nil {
		return scheduler.ProgressEvent{}, false
	}
	return *h.last, true
}

// Reset 清空"最近一次进度"（新一轮扫描开始时调用）。
func (h *ProgressHub) Reset() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.last = nil
	h.lastSet = false
	h.mu.Unlock()
}
