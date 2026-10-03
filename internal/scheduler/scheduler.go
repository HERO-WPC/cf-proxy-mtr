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
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
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

	// DefaultTraceBatchSize 是跟踪结果的落库批大小。
	//
	// 刻意远小于测量的 500：一次 traceroute 要几秒到几十秒，
	// 攒够 500 条可能要等几小时。而每条跟踪结果都很珍贵
	// （代价高），因此宁可频繁写。
	DefaultTraceBatchSize = 20

	// DefaultTraceFlushInterval 是跟踪结果落库的时间上限。
	DefaultTraceFlushInterval = 5 * time.Second
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

	// Trace 为真时，在探测成功后对**探测成功**的目标执行线路跟踪。
	//
	// 两级测量的实质就在这里（需求第 38 条）：连 TCP 都不通的目标
	// 不值得花几十秒去跑 traceroute——路径大概率中途就断了。
	//
	// 引擎为 nil 时该阶段会被跳过并在结果中如实标记（TraceSkipped），
	// 绝不假装跟踪过。
	Trace bool

	// TraceEngine 是线路跟踪引擎（例如 *trace.NextTraceEngine）。
	//
	// 用接口而不是具体类型：调度器只关心"跟踪一个目标并给我结果"，
	// 这样测试可以注入假引擎，而 CLI 不必把引擎的内部细节传进来。
	TraceEngine TraceEngine

	// TraceBatchSize 是跟踪结果的落库批大小（<=0 时使用 DefaultTraceBatchSize）。
	//
	// 比测量的批小得多：跟踪很慢（每次几秒到几十秒），
	// 攒 500 条要等很久，而每条都很珍贵。
	TraceBatchSize int

	// TraceFlushInterval 是跟踪结果落库的时间上限。
	TraceFlushInterval time.Duration

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

	// TracePending 是第二级需要跟踪的目标数
	// （探测成功、且本会话尚未跟踪过）。
	TracePending int

	// TraceAlreadyDone 是续测时被跳过的"已跟踪"目标数。
	TraceAlreadyDone int

	// Trace 是跟踪阶段统计。
	Trace trace.Stats

	// TraceStored / TraceDuplicates 是落库的跟踪行数与幂等跳过数。
	TraceStored     int
	TraceDuplicates int

	// TraceStoreFailures 是跟踪结果落库失败的次数。
	TraceStoreFailures int

	// TraceAttempted 是尝试跟踪的目标数。
	TraceAttempted int

	// TraceSkipped 表示"用户要求跟踪但当前无法跟踪"（引擎为 nil）。
	//
	// 这个字段存在的意义是**不撒谎**：--trace 在没有引擎时
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

// TraceEngine 是线路跟踪引擎。
//
// 刻意在 scheduler 自己的包里定义（而不是直接用 trace.TraceEngine）：
// 调度器只需要"跟踪一个目标并返回结果"这一个能力，
// 这样就**不会**把 trace 包的全部表面积变成调度器的依赖，
// 测试里注入一个三行的假引擎即可。
//
// 形状与 trace.TraceEngine 一致，因此 *trace.NextTraceEngine
// 可以直接传进来。
type TraceEngine interface {
	Trace(ctx context.Context, target model.Target) (*trace.TraceResult, error)
	Name() string
}

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
	if cfg.TraceBatchSize <= 0 {
		cfg.TraceBatchSize = DefaultTraceBatchSize
	}
	if cfg.TraceFlushInterval <= 0 {
		cfg.TraceFlushInterval = DefaultTraceFlushInterval
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
func (s *Scheduler) Run(ctx context.Context, targets []model.Target, clientVersion string) (*Result, error) {
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

	// 续测时可能所有目标都已经测过：那就**什么都不做**，
	// 而不是假装"扫了一遍"。
	//
	// 这里刻意不复用 finish()：finish 会把会话标记为已结束，
	// 而"没有待测目标"的常见原因是**上一次已经把这个会话跑完了**。
	// 对已结束的会话再调 finish 虽然无害，但会掩盖一个真实场景：
	// 用户对着一个已完成的会话反复执行 --resume，应当稳定地
	// 得到"没有要测的"这一结论，而不是第一次成功、第二次报错
	// （会话已结束）。
	if len(pending) == 0 {
		return s.reportNothingToDo(ctx, result)
	}

	// 走到这里说明确实还有目标要测。如果会话却已被标记结束，
	// 那是一个真实的数据问题（会话被提前结束），必须报错而不是
	// 往一个"已完成"的会话里继续追加数据。
	if resumed {
		if state, err := s.store.LoadSession(ctx, sessionID); err == nil && state.Finished() {
			return nil, fmt.Errorf(
				"session %q is already finished but %d target(s) are still unmeasured; "+
					"the session was closed prematurely — start a new scan with --new",
				sessionID, len(pending))
		}
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
	// 4) Level 2：只对**探测成功**的目标做线路跟踪
	// -----------------------------------------------------------------
	if s.cfg.Trace {
		if s.cfg.TraceEngine == nil {
			// 明确标记"要求了但做不到"，绝不静默跳过。
			result.TraceSkipped = true
		} else if err := s.tracePhase(ctx, pending, sessionID, result); err != nil {
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
		// 会话已结束时**不在这里报错**，而是先看还剩多少目标没测：
		//   - 一个已完成的会话被再次 --resume，是用户的正常操作
		//     （"我上次是不是跑完了？"），应当稳定地回答"没有要测的"，
		//     而不是报错；
		//   - 但如果它已结束却还有未测目标，那才是真问题
		//     （会话被提前标记结束），由 Run 在算出待测列表后报错。
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

// tracePhase 执行第二级：对**探测成功**的目标做线路跟踪并落库。
//
// 三个刻意的决定：
//
//  1. **只跟踪 TCP 成功的目标**（需求第 38 条）。连不上的目标去跑
//     traceroute 是浪费：路径大概率中途就断了，而且每次要几十秒。
//     判定直接查数据库（PendingTraceTargets），而不是靠内存里记的
//     success 列表——这样续测时同样正确。
//  2. **第二级也有断点续测**：已经跟踪过的目标被跳过。否则中断重跑
//     要把线路全部重跑一遍。
//  3. **一条目标一个事务**（按批）：跟踪很慢且代价高，
//     攒够一批再写会让中断丢掉大量已完成的工作。因此批很小
//     （默认 20）且有 5 秒时间上限。
func (s *Scheduler) tracePhase(ctx context.Context, pending []model.Target, sessionID string, result *Result) error {
	// 只保留"探测成功且尚未跟踪"的目标。
	candidates, err := s.store.PendingTraceTargets(ctx, pending, s.cfg.CollectorPK, sessionID)
	if err != nil {
		return err
	}

	// 如实汇报"第一级成功了多少、其中已经跟踪过多少"。
	successfulCount, err := s.store.CountSuccessfulTargets(ctx, s.cfg.CollectorPK, sessionID)
	if err != nil {
		return err
	}
	result.TraceAlreadyDone = successfulCount - len(candidates)
	result.TracePending = len(candidates)

	if len(candidates) == 0 {
		s.emitProgress(PhaseTrace, 0, 0, 0, 0, true)
		return nil
	}

	batch := make([]storage.Trace, 0, s.cfg.TraceBatchSize)
	var (
		completed int
		success   int
		failed    int
		lastFlush = s.cfg.Now()
	)

	for _, target := range candidates {
		if ctx.Err() != nil {
			break
		}

		traceResult, traceErr := s.cfg.TraceEngine.Trace(ctx, target)
		if traceErr != nil {
			// 引擎层面的失败（不是"这个目标跟踪失败"）。
			// 计入统计并继续下一个目标：一个目标的问题不该终止整批。
			result.TraceStoreFailures++
			completed++
			failed++
			result.Trace.Add(&trace.TraceResult{
				TargetID:     target.String(),
				IP:           target.IP,
				Port:         target.Port,
				Engine:       s.cfg.TraceEngine.Name(),
				ErrorType:    trace.ErrorTypeOther,
				ErrorMessage: traceErr.Error(),
				Timestamp:    s.cfg.Now().UTC(),
			})
			result.TraceAttempted++
			s.emitProgress(PhaseTrace, completed, len(candidates), success, failed, false)
			continue
		}

		completed++
		if traceResult.Success {
			success++
		} else {
			failed++
		}
		result.Trace.Add(traceResult)
		result.TraceAttempted++

		batch = append(batch, storage.NewTrace(s.cfg.CollectorPK, sessionID, traceResult))

		if len(batch) >= s.cfg.TraceBatchSize || s.cfg.Now().Sub(lastFlush) >= s.cfg.TraceFlushInterval {
			s.flushTraces(context.WithoutCancel(ctx), &batch, result)
			lastFlush = s.cfg.Now()
		}

		s.emitProgress(PhaseTrace, completed, len(candidates), success, failed, false)
	}

	// 最后一批同样用未被取消的 context。
	s.flushTraces(context.WithoutCancel(ctx), &batch, result)
	s.emitProgress(PhaseTrace, completed, len(candidates), success, failed, true)

	return nil
}

// flushTraces 写入当前跟踪批次并清空缓冲。
func (s *Scheduler) flushTraces(ctx context.Context, batch *[]storage.Trace, result *Result) {
	if len(*batch) == 0 {
		return
	}

	saved, skipped, err := s.store.SaveTraces(ctx, *batch)
	result.TraceStored += saved
	result.TraceDuplicates += skipped
	if err != nil {
		// 与测量一致：落库失败不让整次扫描崩掉，计数并如实汇报。
		result.TraceStoreFailures++
	}
	*batch = (*batch)[:0]
}

// reportNothingToDo 在"没有待测目标"时收尾。
//
// 语义刻意做成**幂等**的：
//
//   - 会话还开着（例如上次被 Ctrl+C 打断，但实际数据已经齐了）
//     -> 标记为已结束，这样会话状态与数据事实一致；
//   - 会话已经结束（用户对同一个已完成的会话再次 --resume）
//     -> 什么都不做，稳定地报告"没有要测的"，而不是报错。
//
// 这两种情况对用户是同一个问题（"还有要测的吗？"），
// 因此不该表现为"第一次成功、第二次报错"。
// 只在数据库不可读时返回错误。
func (s *Scheduler) reportNothingToDo(ctx context.Context, result *Result) (*Result, error) {
	// 数据库操作一律用未被取消的 context：被 Ctrl+C 时正是
	// 最需要把会话状态写对的时刻。
	pctx := context.WithoutCancel(ctx)

	progress, err := s.store.LoadSessionProgress(pctx, s.cfg.CollectorPK, result.SessionID)
	if err != nil {
		return nil, err
	}
	result.Progress = progress

	state, err := s.store.LoadSession(pctx, result.SessionID)
	if err != nil {
		return nil, err
	}

	if state.Finished() {
		// 已经是结束状态：不重复写，也不报错。
		result.SessionFinished = true
	} else {
		if err := s.store.FinishSession(pctx, result.SessionID, int(progress.Measured), s.cfg.Now().UTC()); err != nil {
			return nil, err
		}
		result.SessionFinished = true
	}

	result.FinishedAt = s.cfg.Now().UTC()
	return result, nil
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
