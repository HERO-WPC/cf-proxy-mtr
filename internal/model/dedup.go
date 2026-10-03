package model

import (
	"sort"
	"strconv"
)

// Dedup 按 Target.ID 去重，并保持首次出现的顺序。
//
// 为什么放在模型层：
//
//	"两个 Target 何时算同一个目标"（IP × Port）是业务规则，
//	不是解析细节。解析层、缓存层、数据库读取层都需要同一套判断，
//	否则会出现"解析去重了、入库又多了两行"这类问题。
//	这里提供唯一实现点。
//
// 去重语义（刻意选择"保留首次出现"）：
//
//	同一份输入无论解析多少次，结果必须完全相同——包括目标顺序。
//	扫描进度、断点续测、导出 diff 都依赖这个性质。
//	因此不做"用更完整的记录替换已有记录"这种会引入不确定性的行为；
//	需要合并元数据时，调用方应显式使用 Target.Merge。
//
// Dedup 不是并发安全的：解析过程本身是单协程的，
// 需要并发去重时请由调用方自行加锁。
type Dedup struct {
	seen    map[string]struct{}
	targets []Target
}

// NewDedup 创建去重器。capacity 是预估的目标数量，仅用于预分配内存。
func NewDedup(capacity int) *Dedup {
	if capacity < 0 {
		capacity = 0
	}
	return &Dedup{
		seen:    make(map[string]struct{}, capacity),
		targets: make([]Target, 0, capacity),
	}
}

// Add 尝试加入一个目标，返回 false 表示该目标已经出现过。
//
// 传入前不需要 Normalize：本方法用 Target.ID 作为身份，
// 而 ID 由调用方（NewTarget / Normalize）保证来自 (IP, Port)。
// 若 ID 为空则退化为按 (IP, Port) 计算，避免手工构造的 Target
// 全部被当成同一个空 ID 而被误去重。
func (d *Dedup) Add(t Target) bool {
	if d == nil {
		return false
	}
	if d.seen == nil {
		d.seen = make(map[string]struct{})
	}

	key := t.ID
	if key == "" {
		key = TargetID(t.IP, t.Port)
	}

	if _, dup := d.seen[key]; dup {
		return false
	}
	d.seen[key] = struct{}{}
	d.targets = append(d.targets, t)
	return true
}

// Has 报告某个 ID 是否已经加入过。
func (d *Dedup) Has(id string) bool {
	if d == nil {
		return false
	}
	_, ok := d.seen[id]
	return ok
}

// Len 返回已去重的目标数量。
func (d *Dedup) Len() int {
	if d == nil {
		return 0
	}
	return len(d.targets)
}

// Targets 返回去重后的目标列表。
//
// 返回的是内部切片本身（不复制），以匹配"解析 1.5 万个目标"的场景：
// 这里多一次 1.5 万元素的复制没有收益。调用方不应修改返回的切片。
func (d *Dedup) Targets() []Target {
	if d == nil {
		return nil
	}
	return d.targets
}

// ---------------------------------------------------------------------------
// 列表级辅助
// ---------------------------------------------------------------------------

// DedupTargets 对目标列表去重并保持顺序。
//
// 便捷封装，等价于 NewDedup + 逐个 Add。
// 输入列表不会被修改。
func DedupTargets(targets []Target) []Target {
	d := NewDedup(len(targets))
	for _, t := range targets {
		d.Add(t)
	}
	return d.Targets()
}

// SortTargets 按 ID 升序排序。
//
// ID 是唯一的，因此排序结果与输入顺序无关，天然可复现。
//
// 为什么用 ID 而不是 (IP, Port) 排序：ID 就是 (IP, Port) 的规范字符串，
// 排序规则只有一处，不会出现"排序用一套规则、去重用另一套"的分歧。
func SortTargets(targets []Target) {
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
}

// Keys 返回目标的 ID 列表（顺序与输入一致）。
//
// 用于日志、进度显示与"两份目标列表是否一致"的快速比较。
func Keys(targets []Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.ID)
	}
	return out
}

// ValidateAll 校验整个列表，返回第一个非法目标的错误。
//
// 空列表返回 nil。
func ValidateAll(targets []Target) error {
	for i, t := range targets {
		if err := t.Validate(); err != nil {
			return &ListValidationError{Index: i, Target: t, Err: err}
		}
	}
	return nil
}

// ListValidationError 携带非法目标在列表中的位置，便于定位问题数据。
type ListValidationError struct {
	Index  int
	Target Target
	Err    error
}

func (e *ListValidationError) Error() string {
	return "target #" + strconv.Itoa(e.Index) + " (" + e.Target.String() + "): " + e.Err.Error()
}

// Unwrap 支持 errors.Is(err, ErrInvalidTarget)。
func (e *ListValidationError) Unwrap() error { return e.Err }
