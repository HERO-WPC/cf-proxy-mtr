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
//	internal/cli       命令分发、帮助、退出码
//	internal/version   版本与公开数据 schema 版本
//
// 后续阶段的包（source / probe / trace / storage / export / privacy /
// upload / aggregate / detect / scheduler）会随对应 Phase 逐步加入。
//
// 本地验证要求（每个 Phase 都必须通过）：
//
//	go test ./...
//	go vet ./...
//	go build ./...
package cfroutertester
