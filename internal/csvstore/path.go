package csvstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本文件决定"结果写到哪个文件"。
//
// == 为什么默认要带日期时间 ==
//
// 默认路径以前是固定的 data/results.csv，于是**每跑一轮就覆盖上一轮**。
// 而覆盖的后果比"丢了一份数据"更糟：使用者往往是在几轮之间比较
// 不同国家、不同并发下的表现，等发现时上一轮已经没了，而且没有任何
// 提示（默认就是不追加）。中文里这叫"静默覆盖"，是最难防的一类损失。
//
// 带时间戳之后，"跑一轮"天然得到一个独立文件，想对比就并排看。
// 需要写进同一个文件时仍然可以显式指定路径 + --append。
//
// == 命名 ==
//
//	results-20261005-031500.csv
//
// 用 `-` 而不是 `:` 分隔时分秒：Windows 的文件名不允许冒号。
// 年月日在前、时分秒在后，因此**按文件名排序就是按时间排序**，
// 在文件管理器里一眼能看出先后。

// DefaultDir 是结果文件的默认目录。
const DefaultDir = "data"

// pathTimeLayout 是文件名里的时间格式。
//
// 紧凑写法（20261005-031500）而不是 RFC3339：文件名要短、要能排序、
// 不能有冒号。
const pathTimeLayout = "20060102-150405"

// TimestampedPath 返回按当前时间命名的结果文件路径。
//
// 目录为空时用 DefaultDir。
func TimestampedPath(dir string, now time.Time) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = DefaultDir
	}
	name := "results-" + now.Format(pathTimeLayout) + ".csv"
	return filepath.Join(dir, name)
}

// UniquePath 在 path 已被占用时换一个不冲突的名字。
//
// 为什么需要：时间戳精确到秒，而**同一秒内跑两轮是完全可能的**
// （`--limit 1` 的扫描几百毫秒就结束，脚本里连着跑更是常态）。
// 没有这一步，那种情况下仍然会覆盖——而"不会覆盖"正是这个改动
// 要保证的事，留个一秒宽的漏洞等于没修。
//
// 做法是在扩展名前加 -2 / -3 …… 而不是加随机串：仍然可读、可排序，
// 而且"同一秒的第二轮"这个含义是看得出来的。
func UniquePath(path string) string {
	return UniquePathAmong(path, nil)
}

// UniquePathAmong 与 UniquePath 相同，但额外接受一个"已被占用"的判定。
//
// 用途：调用方自己发出过、但**还没落到磁盘上**的名字也算占用。
// 只查磁盘是不够的——名字是在测量开始前定的，文件要到写第一行时才
// 创建，中间那一小段里"同一个名字"在磁盘上并不存在。实测踩过：
// 连续两次取名字得到同一个，而使用者看到的仍然是覆盖。
//
// taken 为 nil 时只查磁盘。
func UniquePathAmong(path string, taken func(string) bool) string {
	free := func(candidate string) bool {
		if fileExists(candidate) {
			return false
		}
		return taken == nil || !taken(candidate)
	}

	if free(path) {
		return path
	}

	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if free(candidate) {
			return candidate
		}
	}

	// 极端情况（同一秒一千轮）：退回带纳秒的名字，宁可难看也不要覆盖。
	return fmt.Sprintf("%s-%d%s", stem, time.Now().UnixNano(), ext)
}

// fileExists 报告路径是否已经存在。
//
// 目录也算"存在"：写进一个目录名会失败，但那时应当由打开文件报错，
// 而不是在这里悄悄换名字。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
