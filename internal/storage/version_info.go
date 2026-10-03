package storage

import "github.com/cf-route-tester/cf-route-tester/internal/version"

// 落库时写入的版本信息。
//
// 每条 measurement / trace 都会带上这两个字段，原因（需求第 16、58 条）：
//
//   - schema_version：公开数据的格式版本。数据库里混着不同版本的数据是
//     常态（用户升级程序后继续累积），分析时必须能按版本区分字段含义；
//   - client_version：产生该条数据的程序版本。测量逻辑本身会演进
//     （超时策略、错误分类），把不同版本的样本混在一起做统计分析
//     会得出错误结论，因此必须可追溯。
//
// 这里刻意不使用 storage 自己的"表结构版本"（见 schema_migrations）：
// 前者是数据语义版本，后者只是本地表结构的演进，两者含义不同。
const (
	// schemaVersionForMeasurements 是测量/跟踪数据的公开格式版本。
	schemaVersionForMeasurements = version.SchemaVersion

	// clientVersion 是产生数据的程序版本。
	clientVersion = version.Version
)
