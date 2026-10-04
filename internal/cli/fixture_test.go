package cli

import (
	"path/filepath"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/source"
)

// writeCacheFromJSON 把一段 all.json 内容解析后写入本地缓存文件。
//
// 这样 CLI 测试可以让 probe / 后续命令走"读取缓存"的路径，
// 既不需要网络，也不需要真的先跑一次 fetch。
//
// 关键点：缓存里记录的 source url 必须与命令实际使用的主数据源一致，
// 否则 source.Loader 会判定"缓存属于另一个源"并转而联网下载——
// 那会让离线测试悄悄变成真实网络请求（既慢又不稳定）。
// 因此这里用 source.DefaultURL，并且调用方必须同时显式传
// --url <同一个值>、--fallback-url ""，把测试完全限制在本地。
//
// 解析失败直接让测试失败：这些测试的前提是"缓存里有一份合法目标列表"。
func writeCacheFromJSON(t *testing.T, path string, body string) error {
	t.Helper()

	parsed, err := source.ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseJSON fixture: %v", err)
	}
	if len(parsed.Targets) == 0 {
		t.Fatalf("fixture parsed to zero targets")
	}

	meta := parsed.Meta
	meta.URL = source.DefaultURL
	meta.Format = "json"

	// 写缓存前确认每个目标都满足模型不变式：
	// 否则一旦失败，测试会在 probe 阶段报一个与真正原因无关的错。
	if err := model.ValidateAll(parsed.Targets); err != nil {
		t.Fatalf("fixture produced an invalid target: %v", err)
	}

	return source.WriteCache(path, meta, parsed.Stats, parsed.Targets)
}

// offlineProbeArgs 构造一组把命令完全限制在本地的参数。
//
// 显式禁用备用数据源是刻意的：万一缓存没命中，
// 我们也希望测试立刻失败，而不是去访问 zip.cm.edu.kg。
func offlineProbeArgs(cachePath string, extra ...string) []string {
	args := []string{
		"probe",
		"--url", source.DefaultURL,
		"--source-cache-first",
		"--fallback-url", "",
		"--cache", cachePath,
		"--source-retries", "0",
	}
	return append(args, extra...)
}

// writeRawCacheFromJSON 写入一份**允许为空**的缓存，返回缓存路径。
//
// 与 writeCacheFromJSON 的区别：它不要求解析出目标。
// 用于需要"合法但空"的目标列表的测试（例如无目标可测时的报错路径）。
func writeRawCacheFromJSON(t *testing.T, body string) string {
	t.Helper()

	parsed, err := source.ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseJSON fixture: %v", err)
	}

	meta := parsed.Meta
	meta.URL = source.DefaultURL
	meta.Format = "json"

	path := filepath.Join(t.TempDir(), "all.json")
	if err := source.WriteCache(path, meta, parsed.Stats, parsed.Targets); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	return path
}
