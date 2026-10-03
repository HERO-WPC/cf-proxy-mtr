// Package cfroutertester 是 cf-route-tester 的仓库根包。
//
// cf-route-tester 是一个用 Go 编写的跨平台网络线路众测工具：
// 大量不同地区、不同运营商的用户，在各自的真实网络环境中对同一批
// IP:Port 做完整线路测量（TCP 延迟 / 丢包 + NextTrace 路径），
// 结果匿名汇总后形成按 地区 × 运营商 × 时间 组织的长期线路数据库。
//
// 本文件只用于承载仓库级文档，没有可执行代码。
// 可执行入口在 cmd/cf-route-tester，业务逻辑在 internal/ 下：
//
//	internal/cli       命令分发、帮助、退出码、子命令参数解析
//	internal/version   版本与公开数据 schema 版本
//	internal/model     Target / Location / ColoInfo / CollectorProfile
//	internal/source    all.json / all.txt 的下载、容错解析与本地缓存
//	internal/probe     TCP 探测、错误分类
//	internal/worker    通用有界 worker pool（probe 与 trace 共用）
//	internal/trace     NextTrace 集成：外部进程调用与 JSON 归一化
//	internal/storage   本地 SQLite：迁移、只追加时间序列、查询
//	internal/scheduler 扫描编排：测量会话、断点续测、两级测量顺序
//	internal/detect    测量者地区 / 运营商检测（可解释的源、手动优先）
//	internal/identity  本地匿名标识 collector_id（随机生成，非硬件指纹）
//
// 后续阶段的包（export / privacy / upload / aggregate）
// 会随对应 Phase 逐步加入。
//
// 本地验证要求（每个 Phase 都必须通过）：
//
//	go test ./...
//	go vet ./...
//	go build ./...
package cfroutertester
