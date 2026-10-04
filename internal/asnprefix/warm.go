package asnprefix

import (
	"context"
	"sync"
)

// Warmer 让前缀抓取与 TCP 探测**并行**进行。
//
// == 为什么需要它 ==
//
// 抓取本身已经并发（见 DefaultConcurrency），但它在跟踪阶段
// 开始时才启动。使用者看到的现象是：TCP 几秒就扫完了，
// 然后**卡住一段时间**才出第一条线路——即使抓取只要几秒，
// 那几秒也是纯粹的等待。
//
// 真跑一轮 11600 个目标时，TCP 阶段本身要几分钟，而抓取只要几秒。
// 两者并行之后，探测结束时前缀早就就绪了，感知上的停顿是零。
//
// == 用法 ==
//
//	w := asnprefix.Warm(ctx, opts)   // 立刻返回，后台开始抓
//	...探测...
//	resolver := w.Get()              // 需要时等待，通常立即返回
type Warmer struct {
	done     chan struct{}
	resolver *Resolver
	once     sync.Once

	// cancel 停掉后台抓取，供 Cancel 使用。
	cancel context.CancelFunc
}

// Warm 在后台开始加载前缀，立即返回。
//
// 返回的 Warmer 在需要时用 Get 取值。ctx 取消会让抓取尽快停止，
// Get 仍然能拿到"已经抓到的部分"——半份前缀比没有强。
func Warm(ctx context.Context, opts Options) *Warmer {
	// 派生一个子 context：调用方因此可以在"发现根本不需要前缀"时
	// 立刻停掉抓取，而不是让它把整张表下完。
	fetchCtx, cancel := context.WithCancel(ctx)

	w := &Warmer{done: make(chan struct{}), cancel: cancel}

	go func() {
		defer close(w.done)
		w.resolver = Load(fetchCtx, opts)
	}()

	return w
}

// Cancel 停止后台抓取，并**等待协程真正退出**。
//
// 两件事都要，缺一不可：
//
//  1. **取消**：不然"不需要前缀"的那条路径（例如跟踪引擎不可用）
//     会白白把整张 iptoasn 表下完，使用者为此白等几十秒。
//  2. **等待退出**：抓取过程中会往调用方给的日志回调里写字。协程还
//     活着就返回，等于让它在**调用方已经返回之后**继续写输出——
//     实测这是一个真实的数据竞争（`-race` 报 bytes.Buffer 读写冲突），
//     而且那次写入可能落在一个已经被丢弃的缓冲区上。
//
// 幂等，nil 安全。
func (w *Warmer) Cancel() {
	if w == nil {
		return
	}
	if w.cancel != nil {
		w.cancel()
	}
	<-w.done
}

// Get 等待加载完成并返回 Resolver。
//
// 重复调用是安全的（内部只等待同一个 channel）。
// w 为 nil 时返回 nil，调用方因此不必到处判空。
func (w *Warmer) Get() *Resolver {
	if w == nil {
		return nil
	}
	w.once.Do(func() { <-w.done })
	return w.resolver
}

// Ready 报告加载是否已经完成（非阻塞）。
//
// 用途：调用方可以先问一句"好了吗"，没好就说明"正在解析线路"，
// 而不是让使用者对着一个不动的进度条猜。
func (w *Warmer) Ready() bool {
	if w == nil {
		return false
	}
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}
