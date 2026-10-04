package webui

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
)

// 本文件管理"这一轮的结果写到哪个文件"。
//
// == 为什么默认要按日期时间命名 ==
//
// 默认路径以前是固定的 data/results.csv，于是每跑一轮就**覆盖上一轮**，
// 而且默认就是不追加、没有任何提示。想对比不同国家、不同并发下的
// 表现时就只能重测——那既费时间又让两轮的条件不可能完全一致。
//
// 带时间戳之后，跑一轮天然得到一个独立文件；需要写进同一个文件时
// 仍然可以显式填路径 + 勾选追加。
//
// == 三种操作对"默认路径"的含义不同 ==
//
//   - **测量**：要一份**新的**文件（这正是"不覆盖"的落点）
//   - **跟踪**：要**追加到刚测出的那份**——你就是在给这份结果补线路信息，
//     写成另一个文件会让结果表格突然找不到自己的线路
//   - **读取**（结果表格、跟踪预览）：读**当前**那份；一轮都没跑过时
//     还没有文件，返回"不存在"而不是报错
//
// 把这三件事混成一个"默认路径"是错的：测量和跟踪用同一个默认值的话，
// 跟踪会把线路写进一份新文件，而表格还在读旧的。
// resultsPathState 管理"当前结果文件"。
type resultsPathState struct {
	mu   sync.Mutex
	path string

	// issued 是本进程**已经发出过**的名字。
	//
	// 只靠"磁盘上是否存在"不够：名字是在测量开始前定的，而文件要到
	// 写第一行时才创建，中间那一小段里同一个名字在磁盘上并不存在。
	// 实测踩过——连续两次取名字得到同一个，使用者看到的仍然是覆盖。
	issued map[string]bool
}

// reserve 生成一个此前没用过的名字并记为已发出。
//
// 调用方必须持有 s.mu。
func (s *resultsPathState) reserve(dir string) string {
	if s.issued == nil {
		s.issued = make(map[string]bool, 4)
	}

	name := csvstore.UniquePathAmong(
		csvstore.TimestampedPath(dir, time.Now()),
		func(candidate string) bool { return s.issued[candidate] },
	)
	s.issued[name] = true
	return name
}

// ResultsDir 从配置里的结果文件路径推出**目录**。
//
// 配置给的是一个文件路径（例如 data/results.csv），但自动命名只需要
// 它的目录——名字由时间决定。直接把那个文件路径当目录用会得到一个
// 嵌套路径（data/results.csv/results-20261005-031500.csv），
// 也就是把一个本该是文件的名字变成了目录。实测踩过。
//
// 取不到目录时回落到 csvstore.DefaultDir。
func ResultsDir(configuredPath string) string {
	configuredPath = strings.TrimSpace(configuredPath)
	if configuredPath != "" {
		if dir := filepath.Dir(configuredPath); dir != "" && dir != "." {
			return dir
		}
	}
	return csvstore.DefaultDir
}

// current 返回当前结果文件；还没有时先生成一个名字并记住。
//
// 生成一次就固定下来，而不是每次调用都算一个新名字：读取接口要能
// 稳定地报告"同一个文件不存在"，否则界面每次刷新看到的路径都在变，
// 会让人以为自己在看不同的东西。
//
// fallbackDir 是配置里那个结果路径**所在**的目录（见 ResultsDir），
// 不是文件路径本身。
func (s *resultsPathState) current(fallbackDir string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path != "" {
		return s.path
	}
	s.path = s.reserve(fallbackDir)
	return s.path
}

// next 生成一份**新的**结果文件并记为当前。
//
// 用于测量：每跑一轮一个新文件。
func (s *resultsPathState) next(fallbackDir string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.path = s.reserve(fallbackDir)
	return s.path
}

// set 记录调用方显式给出的路径，使其成为后续读取与跟踪的目标。
func (s *resultsPathState) set(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
}
