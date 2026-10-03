// Package scheduler 负责把"一次完整测量"编排起来。
//
// 它回答的问题（需求第 7、20、21、38 条）：
//
//	读取 all.json -> 加载 Collector Profile -> 创建 Scan Session
//	-> TCP Probe worker pool -> 写入 measurements
//	-> 成功目标进入 Trace Queue -> 写入 traces -> 会话完成
//
// 为什么单独一个包而不是写在 CLI 里：
//
//   - 断点续测的判据（target × collector × session）是业务规则，
//     需要有测试守着，而 CLI 层很难测；
//   - 后续的守护进程（周期扫描）会复用同一套编排逻辑；
//   - CLI 只应负责解析参数与展示结果。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
)

// 默认参数。
const (
	// DefaultBatchSize 是测量结果的落库批大小。
	//
	// 500 是一个折中：太小会让 SQLite 频繁 fsync（扫描速度掉到
	// 几百条/秒），太大则崩溃时丢失的进度更多。
	DefaultBatchSize = 500

	// DefaultFlushInterval 是落库的**时间**上限。
	//
	// 为什么除了条数还需要时间：满 500 条才写库，在"目标大量超时"的
	// 情况下可能要等很久——因为每个超时都要占满 timeout。
	// 实测：600 个目标、1s 超时、50 并发时，第一批 500 条要到
	// 第 6 秒才写下去；在那之前被 Ctrl+C，数据库里**一条都没有**，
	// 等于整段时间白测。
	//
	// 加了时间上限之后，无论快慢都至少每 2 秒落一次盘。
	DefaultFlushInterval = 2 * time.Second

	// DefaultProgressInterval 是两次进度回调之间的最小间隔。
	DefaultProgressInterval = 2 * time.Second
)

// Phase 表示当前处于哪一级测量。
type Phase string

const (
	// PhaseProbe 是 Level 1：TCP 连通性与延迟。
	PhaseProbe Phase = "probe"

	// PhaseTrace 是 Level 2：线路跟踪。
	PhaseTrace Phase = "trace"
)

// Config 是扫描配置。
type Config struct {
	// Probe 是探测配置（并发数、超时）。
	Probe probe.Config

	// CollectorPK 是采集者在数据库中的主键（>0）。
	CollectorPK int64

	// SessionID 是要使用的会话 ID。
	//
	// Resume 为真时它必须是**已存在**的会话；否则会被创建。
	SessionID string

	// Resume 为真表示继续一个已有会话，只测量尚未完成的目标。
	Resume bool

	// Trace 为真时，在探测成功后对成功目标执行线路跟踪。
	//
	// 注意：Phase 7 才会实现 NextTrace 引擎。这里只建立顺序与
	// 接口位置，TraceFn 为空时该阶段会被跳过并在结果中如实标记，
	// 绝不会假装跟踪过。
	Trace bool

	// BatchSize 是落库批大小（<=0 时使用 DefaultBatchSize）。
	BatchSize int

	// FlushInterval 是落库的时间上限（<=0 时使用 DefaultFlushInterval）。
	//
	// 即使一批还没攒够 BatchSize 条，超过这个间隔也会先写下去，
	// 保证中断时不会丢失已经测到的结果。
	FlushInterval time.Duration

	// ProgressInterval 是进度回调间隔（<=0 时使用默认值）。
	ProgressInterval time.Duration

	// Now 允许注入当前时间（测试用）。
	Now func() time.Time
}

// Result 是一次扫描的结果。
type Result struct {
	// SessionID 是本次扫描使用的会话。
	SessionID string

	// Resumed 表示这次是继续一个已有会话。
	Resumed bool

	// TargetsTotal 是本次扫描**考虑**的目标总数。
	TargetsTotal int

	// TargetsPending 是实际需要测量的目标数（续测时会小于总数）。
	TargetsPending int

	// AlreadyDone 是续测时被跳过的"已完成"目标数。
	AlreadyDone int

	// Probe 是探测阶段统计。
	Probe probe.Stats

	// Stored / Duplicates 是落库的测量行数与跳过数。
	Stored     int
	Duplicates int

	// StoreFailures 是落库失败的批次数。
	StoreFailures int

	// TraceAttempted 是尝试跟踪的目标数（Phase 7 之前恒为 0）。
	TraceAttempted int

	// TraceSkipped 表示"用户要求跟踪但当前无法跟踪"。
	//
	// 这个字段存在的意义是**不撒谎**：--trace 在 Phase 7 之前
	// 无法生效，必须明确告知，而不是静默跳过让用户以为做了。
	TraceSkipped bool

	// Interrupted 表示扫描被取消（例如 Ctrl+C），结果不完整。
	Interrupted bool

	// SessionFinished 表示会话已被标记结束。
	SessionFinished bool

	// Progress 是会话在数据库中的最终进度。
	Progress storage.SessionProgress

	// StartedAt / FinishedAt 是本次运行的起止时间。
	StartedAt  time.Time
	FinishedAt time.Time
}

// Elapsed 返回本次运行的耗时。
func (r Result) Elapsed() time.Duration {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// ProgressEvent 是一次进度回调。
type ProgressEvent struct {
	Phase     Phase
	Completed int
	Total     int
	Success   int
	Failed    int
}

// ProgressFunc 接收进度事件。实现应当尽快返回（它会在消费结果的
// 同一个 goroutine 里被调用）。
type ProgressFunc func(ProgressEvent)

// TraceFunc 执行一次线路跟踪并写入数据库。
//
// Phase 7 会提供真正的实现；在那之前为 nil，scheduler 会跳过该阶段
// 并把 TraceSkipped 置为真。
type TraceFunc func(ctx context.Context, target model.Target) error

// Scheduler 编排一次扫描。
type Scheduler struct {
	store  *storage.Store
	cfg    Config
	runner *probe.Runner

	// progress 是进度回调与节流状态。
	progress         ProgressFunc
	lastProgressCall time.Time
}

// New 创建 Scheduler。
//
// store 必须已经打开并完成迁移；CollectorPK 必须有效。
func New(store *storage.Store, cfg Config) (*Scheduler, error) {
	if store == nil {
		return nil, errors.New("scheduler: nil store")
	}
	if cfg.CollectorPK <= 0 {
		return nil, errors.New("scheduler: collector primary key is required (create the collector first)")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultFlushInterval
	}
	if cfg.ProgressInterval <= 0 {
		cfg.ProgressInterval = DefaultProgressInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &Scheduler{
		store:  store,
		cfg:    cfg,
		runner: probe.NewRunner(probe.RunnerConfig{Probe: cfg.Probe}),
	}, nil
}

// SetProgress 设置进度回调。
//
// 单独一个 setter 而不是构造函数参数：进度回调是**可选**的展示细节，
// 放进构造函数会让所有调用点都要传一个 nil。
func (s *Scheduler) SetProgress(fn ProgressFunc) { s.progress = fn }

// Run 执行扫描。
//
// targets 是候选目标列表（通常来自 all.json，已按 limit 截断）。
// 续测时其中已完成的部分会被跳过——判据是
// (target, collector, session) 三元组，而不是"数据库里有没有历史结果"。
//
// 返回 error 的情形限于**全局错误**：会话不可用、数据库不可写。
// 单个目标测量失败只是结果，不会中断扫描。
func (s *Scheduler) Run(ctx context.Context, targets []model.Target, clientVersion string, traceFn TraceFunc) (*Result, error) {
	result := &Result{
		TargetsTotal: len(targets),
		StartedAt:    s.cfg.Now().UTC(),
	}

	// -----------------------------------------------------------------
	// 1) 确定会话：新建或续测
	// -----------------------------------------------------------------
	sessionID, resumed, err := s.prepareSession(ctx, len(targets), clientVersion)
	if err != nil {
		return nil, err
	}
	result.SessionID = sessionID
	result.Resumed = resumed

	// -----------------------------------------------------------------
	// 2) 确定待测目标
	// -----------------------------------------------------------------
	pending := targets
	if resumed {
		var err error
		pending, err = s.store.PendingTargets(ctx, targets, s.cfg.CollectorPK, sessionID)
		if err != nil {
			return nil, err
		}
		result.AlreadyDone = len(targets) - len(pending)
	}
	result.TargetsPending = len(pending)

	// 续测时可能所有目标都已经测过：那就直接结束会话，
	// 而不是假装"扫了一遍"。
	if len(pending) == 0 {
		return s.finish(ctx, result)
	}

	// -----------------------------------------------------------------
	// 3) Level 1：TCP Probe -> measurements
	// -----------------------------------------------------------------
	if err := s.probePhase(ctx, pending, sessionID, result); err != nil {
		return nil, err
	}

	// 取消后不再进入下一阶段：续测时需要知道"进度到哪了"，
	// 而下一阶段依赖上一阶段的结果。
	if ctx.Err() != nil {
		result.Interrupted = true
		return s.finish(ctx, result)
	}

	// -----------------------------------------------------------------
	// 4) Level 2：Trace（Phase 7 之前无法执行）
	// -----------------------------------------------------------------
	if s.cfg.Trace {
		if traceFn == nil {
			// 明确标记"要求了但做不到"，绝不静默跳过。
			result.TraceSkipped = true
		} else if err := s.tracePhase(ctx, pending, traceFn, result); err != nil {
			return nil, err
		}
	}

	result.Interrupted = ctx.Err() != nil
	return s.finish(ctx, result)
}

// prepareSession 返回要使用的会话 ID，以及是否是续测。
func (s *Scheduler) prepareSession(ctx context.Context, targetCount int, clientVersion string) (string, bool, error) {
	if s.cfg.Resume {
		if s.cfg.SessionID == "" {
			return "", false, errors.New("scheduler: resume requires a session id")
		}

		state, err := s.store.LoadSession(ctx, s.cfg.SessionID)
		if err != nil {
			// 会话不存在时必须报错：静默新建一个会话会让
			// "续测"变成"重测一遍"，用户不会察觉。
			return "", false, fmt.Errorf("resume session %q: %w", s.cfg.SessionID, err)
		}
		// 先判断归属，再判断是否已结束。
		//
		// 顺序有讲究：会话不属于本采集者是更根本的问题
		// （说明用户在续测别人的会话），而"已结束"可能只是
		// 因为另一个采集者跑完了它。先报后者会让用户
		// 在错误的方向上排查。
		if state.CollectorPK != s.cfg.CollectorPK {
			// 会话属于另一个采集者：续测它会把两个节点的数据混在一起。
			return "", false, fmt.Errorf("session %q belongs to another collector", s.cfg.SessionID)
		}
		if state.Finished() {
			return "", false, fmt.Errorf("session %q is already finished; start a new scan instead", s.cfg.SessionID)
		}
		return state.ID, true, nil
	}

	if s.cfg.SessionID == "" {
		return "", false, errors.New("scheduler: empty session id")
	}
	if err := s.store.DefineSession(ctx, s.cfg.SessionID, s.cfg.CollectorPK, targetCount, clientVersion, s.cfg.Now().UTC()); err != nil {
		return "", false, err
	}
	return s.cfg.SessionID, false, nil
}

// probePhase 执行 TCP 探测并把结果落库。
func (s *Scheduler) probePhase(ctx context.Context, targets []model.Target, sessionID string, result *Result) error {
	handle := s.runner.Start(ctx, targets)

	batch := make([]storage.Measurement, 0, s.cfg.BatchSize)
	var (
		completed int
		success   int
		failed    int
		lastFlush = s.cfg.Now()
	)

	for res := range handle.Results() {
		completed++
		if res.Success {
			success++
		} else {
			failed++
		}

		batch = append(batch, storage.NewMeasurement(s.cfg.CollectorPK, sessionID, res))

		// 两个触发条件：攒够条数（吞吐）或超过时间（安全）。
		//
		// 只用条数是不够的：大量目标超时时，一批可能要等几分钟才满，
		// 中途被打断就等于这段时间白测。
		//
		// 写库时用**未被取消的** context：Ctrl+C 之后正是最需要
		// 把已测结果保存下来的时刻，用已取消的 ctx 会直接失败。
		if len(batch) >= s.cfg.BatchSize || s.cfg.Now().Sub(lastFlush) >= s.cfg.FlushInterval {
			s.flush(context.WithoutCancel(ctx), &batch, result, sessionID, completed)
			lastFlush = s.cfg.Now()
		}

		s.emitProgress(PhaseProbe, completed, len(targets), success, failed, false)
	}

	// 最后一批同样用未取消的 context。
	s.flush(context.WithoutCancel(ctx), &batch, result, sessionID, completed)

	stats := handle.Wait()
	stats.Total = len(targets)
	result.Probe = stats
	s.emitProgress(PhaseProbe, completed, len(targets), success, failed, true)

	return nil
}

// flush 写入当前批次并清空缓冲。
func (s *Scheduler) flush(ctx context.Context, batch *[]storage.Measurement, result *Result, sessionID string, completed int) {
	if len(*batch) == 0 {
		return
	}

	saved, skipped, err := s.store.SaveMeasurements(ctx, *batch)
	result.Stored += saved
	result.Duplicates += skipped
	if err != nil {
		// 落库失败不让整次扫描崩掉：已经测到的结果仍在内存里，
		// 只是这一批没能写入。计数并在最后如实汇报。
		result.StoreFailures++
	}
	*batch = (*batch)[:0]

	// 周期性地把进度写进会话，这样进程被杀之后
	// 用户仍能看到"上次跑到哪了"。
	//
	// 写失败不影响扫描：进度计数只是给人看的，
	// 真正的续测判据来自 measurements 表（见 LoadSessionProgress）。
	_ = s.store.UpdateSessionProgress(ctx, sessionID, completed)
}

// tracePhase 执行线路跟踪。
func (s *Scheduler) tracePhase(ctx context.Context, targets []model.Target, traceFn TraceFunc, result *Result) error {
	var completed int
	for _, target := range targets {
		if ctx.Err() != nil {
			return nil
		}
		// 注意：这里对**所有待测目标**尝试跟踪，是否真的跟踪
		// 由 traceFn 决定（Phase 8 会只跟踪 TCP 成功的目标）。
		if err := traceFn(ctx, target); err != nil {
			// 单个目标的跟踪失败不能中断整体。
			result.StoreFailures++
		}
		completed++
		result.TraceAttempted++
		s.emitProgress(PhaseTrace, completed, len(targets), 0, 0, false)
	}
	s.emitProgress(PhaseTrace, completed, len(targets), 0, 0, true)
	return nil
}

// finish 结束会话并汇总结果。
//
// 注意：这里对数据库的**读写都使用未被取消的 context**。
// 原因很实际：被 Ctrl+C 中断时，恰恰最需要把会话状态写对
// （保持未结束，好让 --resume 能继续）。如果沿用已取消的 ctx，
// 这一步必然失败，中断后既没保存进度也没法续测。
func (s *Scheduler) finish(ctx context.Context, result *Result) (*Result, error) {
	pctx := context.WithoutCancel(ctx)

	// 会话的完成计数以**数据事实**为准，而不是计数器：
	// 即使上次运行崩溃导致计数没更新，这里也能算出正确值。
	progress, err := s.store.LoadSessionProgress(pctx, s.cfg.CollectorPK, result.SessionID)
	if err != nil {
		return nil, err
	}
	result.Progress = progress

	// 被取消时不标记会话结束：那正是断点续测要用的状态。
	if ctx.Err() == nil {
		if err := s.store.FinishSession(pctx, result.SessionID, int(progress.Measured), s.cfg.Now().UTC()); err != nil {
			return nil, err
		}
		result.SessionFinished = true
	}

	result.FinishedAt = s.cfg.Now().UTC()
	return result, nil
}

// emitProgress 按节流间隔调用进度回调。
//
// force 为真时强制回调（用于阶段结束，让界面能显示最终状态）。
func (s *Scheduler) emitProgress(phase Phase, completed, total, success, failed int, force bool) {
	if s.progress == nil {
		return
	}
	now := s.cfg.Now()
	if !force && now.Sub(s.lastProgressCall) < s.cfg.ProgressInterval {
		return
	}
	s.lastProgressCall = now
	s.progress(ProgressEvent{
		Phase:     phase,
		Completed: completed,
		Total:     total,
		Success:   success,
		Failed:    failed,
	})
}

// LooksLikeResumeError 报告错误是否属于"无法续测"（会话不存在 / 已结束 / 不属于本采集者）。
//
// CLI 用它把这类错误转成更友好的提示（"改用 --new 重新开始"），
// 而不是让用户面对一条原始错误。
func LooksLikeResumeError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, storage.ErrNotFound) ||
		errors.Is(err, storage.ErrInvalidInput)
}
