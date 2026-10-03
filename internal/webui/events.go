package webui

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// uiEvent 是一条要显示在界面日志面板里的事件。
//
// 与 applog 的分工（刻意不合并）：
//
//	applog   给排查用：时间戳、级别、结构化字段，写文件
//	uiEvent  给使用者看：一行人类可读的话，走 SSE 到网页
//
// 把每一条 applog 都推到界面会让日志面板刷满内部细节；
// 反过来把界面事件写进日志文件又会让文件充满"正在测 X"这种噪音。
type uiEvent struct {
	// Seq 是自增序号，前端用它去重（SSE 断线重连会重发）。
	Seq int64 `json:"seq"`

	// Time 是事件时间（RFC3339，前端自己按本地时区显示）。
	Time string `json:"time"`

	// Level 是级别：info / good / warn / error。
	//
	// 前端按它上色。刻意只有这四档：日志面板不是调试器，
	// 分级太多会让颜色失去意义。
	Level string `json:"level"`

	// Text 是要显示的一行。
	Text string `json:"text"`
}

// 级别常量。
const (
	levelInfo  = "info"
	levelGood  = "good"
	levelWarn  = "warn"
	levelError = "error"
)

// eventBroker 把界面事件广播给所有连接的页面，并保留一段历史。
//
// 为什么要保留历史：用户刷新页面（或重新打开）时，
// 应当还能看到刚才发生了什么，而不是面对一个空面板。
type eventBroker struct {
	mu      sync.Mutex
	subs    map[int]chan uiEvent
	nextID  int
	seq     int64
	history []uiEvent
	limit   int
}

// defaultHistoryLimit 是保留的事件条数。
//
// 2000 行足够回溯好几轮扫描的开头，又不会让内存随长时间运行无限增长。
//
// 刻意**不**在每次扫描开始时清空历史：用户明确要求日志一直存留。
// 前端也保留同样规模的面板。
const defaultHistoryLimit = 2000

// newEventBroker 创建广播器。
func newEventBroker() *eventBroker {
	return &eventBroker{
		subs:  make(map[int]chan uiEvent),
		limit: defaultHistoryLimit,
	}
}

// Emit 记录并广播一条事件。
//
// 它**不会阻塞**：单个订阅者消费不过来时丢弃它的事件，
// 而不是让扫描停下来等一个慢页面。这是刻意的取舍——
// 事件是"告知"，不是"数据"；卡住测量去送日志是不可接受的。
func (b *eventBroker) Emit(level, text string) {
	if b == nil || strings.TrimSpace(text) == "" {
		return
	}

	b.mu.Lock()
	b.seq++
	event := uiEvent{
		Seq:   b.seq,
		Time:  time.Now().UTC().Format(time.RFC3339),
		Level: level,
		Text:  text,
	}

	// 追加历史并裁剪。
	b.history = append(b.history, event)
	if len(b.history) > b.limit {
		// 一次性砍掉超出部分，避免每来一条就搬一次整个切片。
		excess := len(b.history) - b.limit
		b.history = append([]uiEvent(nil), b.history[excess:]...)
	}

	subs := make([]chan uiEvent, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- event:
		default:
			// 订阅者满了：丢掉最旧的一条腾位置，保住最新的。
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

// Info / Good / Warn / Error 是 Emit 的便捷包装。
func (b *eventBroker) Info(format string, args ...any) {
	b.Emit(levelInfo, fmt.Sprintf(format, args...))
}

func (b *eventBroker) Good(format string, args ...any) {
	b.Emit(levelGood, fmt.Sprintf(format, args...))
}

func (b *eventBroker) Warn(format string, args ...any) {
	b.Emit(levelWarn, fmt.Sprintf(format, args...))
}

func (b *eventBroker) Error(format string, args ...any) {
	b.Emit(levelError, fmt.Sprintf(format, args...))
}

// Subscribe 订阅事件。
//
// 返回的通道会先收到**历史事件**（快照），随后是实时事件。
// 调用方必须调用 cancel 释放订阅。
func (b *eventBroker) Subscribe() (<-chan uiEvent, func()) {
	if b == nil {
		ch := make(chan uiEvent)
		close(ch)
		return ch, func() {}
	}

	// 缓冲要能装下整段历史，否则一次性灌历史时会丢。
	ch := make(chan uiEvent, b.historyLimit()+16)

	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	snapshot := append([]uiEvent(nil), b.history...)
	b.mu.Unlock()

	// 先补历史，让刚连上的页面立刻有上下文。
	for _, event := range snapshot {
		select {
		case ch <- event:
		default:
		}
	}

	cancel := func() {
		b.mu.Lock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// historyLimit 返回历史上限（加锁读取）。
func (b *eventBroker) historyLimit() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

// Recent 返回最近 n 条历史事件（最新在后）。
func (b *eventBroker) Recent(n int) []uiEvent {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.history) {
		n = len(b.history)
	}
	return append([]uiEvent(nil), b.history[len(b.history)-n:]...)
}
