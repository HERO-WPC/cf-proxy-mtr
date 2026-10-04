// Package version 提供 cf-route-tester 的版本信息。
//
// 版本信息有三个来源，优先级从高到低：
//
//  1. 编译期通过 -ldflags "-X ..." 注入（发布构建使用）；
//  2. 源码中的默认常量（默认版本号）；
//  3. 未知值。
//
// 该包不允许引入任何网络、文件系统或第三方依赖，
// 这样任何模块（包括未来的 export / upload）都可以安全地引用版本号，
// 并把 client_version 写入公开数据。
package version

import (
	"runtime"
	"strconv"
	"strings"
)

// 默认版本号。发布新版本时同时修改这里和 README。
//
// 该值表示公开数据中 client_version 的语义版本，
// 一旦发布过携带某个版本号的公开数据，就不要复用同一个版本号。
const (
	// Version 是 cf-route-tester 的语义化版本号。
	Version = "0.1.1"

	// SchemaVersion 是公开数据（导出 / 上传 JSONL）的 Schema 版本。
	//
	// 它独立于程序版本：程序可以频繁升级，但只要公开数据结构不变，
	// SchemaVersion 就不应变化。公开数据结构一旦发生不兼容变更，
	// 必须递增该值。
	SchemaVersion = 1

	// ClientName 是程序名称，用于日志、导出元数据和 GitHub 目录命名。
	ClientName = "cf-route-tester"

	// unknown 用于表示编译期未注入的信息。
	unknown = "unknown"
)

// 编译期注入变量。使用方式：
//
//	go build -ldflags "-X github.com/cf-route-tester/cf-route-tester/internal/version.Commit=$(git rev-parse --short HEAD) \
//	                   -X github.com/cf-route-tester/cf-route-tester/internal/version.BuildDate=2026-10-03T10:00:00Z"
//
// 注意：不要在发布构建中注入 Token 或任何个人信息。
var (
	// Commit 是构建所基于的 git commit。
	Commit = unknown

	// BuildDate 是构建时间，建议使用 RFC3339 格式。
	BuildDate = unknown
)

// Info 描述当前二进制的完整版本信息。
type Info struct {
	// Client 是程序名称。
	Client string `json:"client"`

	// Version 是程序语义化版本号。
	Version string `json:"version"`

	// SchemaVersion 是公开数据 Schema 版本。
	SchemaVersion int `json:"schema_version"`

	// Commit 是 git commit（可能为 unknown）。
	Commit string `json:"commit"`

	// BuildDate 是构建时间（可能为 unknown）。
	BuildDate string `json:"build_date"`

	// GoVersion 是构建所用的 Go 工具链版本。
	GoVersion string `json:"go_version"`

	// OS 是目标操作系统，例如 windows/linux/darwin。
	OS string `json:"os"`

	// Arch 是目标架构，例如 amd64/arm64。
	Arch string `json:"arch"`
}

// Get 返回当前二进制的版本信息。
//
// 该函数总是返回可用的值（永不返回空字符串字段），
// 因此可以安全地写入导出数据与日志。
func Get() Info {
	return Info{
		Client:        ClientName,
		Version:       normalize(Version),
		SchemaVersion: SchemaVersion,
		Commit:        normalize(Commit),
		BuildDate:     normalize(BuildDate),
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
	}
}

// String 返回单行版本字符串，例如 "cf-route-tester 0.1.0"。
func (i Info) String() string {
	return i.Client + " " + i.Version
}

// Line 返回 "cf-route-tester" 与 "version: x.y.z" 两行文本。
//
// 这是 `cf-route-tester version` 的默认输出格式，
// 格式被测试固定，不要随意改动。
func (i Info) Line() string {
	return i.Client + "\nversion: " + i.Version
}

// Detailed 返回多行详细信息，用于 `version --verbose`。
//
// 输出顺序固定，便于脚本解析和测试。
func (i Info) Detailed() string {
	var b strings.Builder
	b.WriteString("client:         " + i.Client + "\n")
	b.WriteString("version:        " + i.Version + "\n")
	b.WriteString("schema_version: " + strconv.Itoa(i.SchemaVersion) + "\n")
	b.WriteString("commit:         " + i.Commit + "\n")
	b.WriteString("build_date:     " + i.BuildDate + "\n")
	b.WriteString("go_version:     " + i.GoVersion + "\n")
	b.WriteString("platform:       " + i.OS + "/" + i.Arch + "\n")
	return strings.TrimRight(b.String(), "\n")
}

// normalize 把空白值统一成 "unknown"，避免公开数据里出现空字段。
func normalize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return unknown
	}
	return s
}
