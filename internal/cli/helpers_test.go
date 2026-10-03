package cli

import (
	"os"

	"github.com/cf-route-tester/cf-route-tester/internal/source"
)

// sourceDefaultURL 返回默认主数据源地址。
//
// 测试夹具把缓存记录的 source url 设为这个值，命令也必须显式使用它，
// 否则 source.Loader 会判定"缓存属于另一个源"而转去联网。
func sourceDefaultURL() string { return source.DefaultURL }

// osReadFile 是本包测试里的文件读取（集中一处，便于替换）。
func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
