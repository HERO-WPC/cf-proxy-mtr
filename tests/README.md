# tests/

**这个目录目前是空的（只有这份说明），这是有意的。**

跨模块的集成测试没有集中放在这里，而是放在**被测行为所属的包内**，
理由如下：

1. **Go 的测试惯例是"测试文件紧挨着被测代码"**。放在 `internal/cli/`
   的 `cmd_*_test.go` 里，才能直接构造 CLI 的 `Env`、
   复用测试夹具（缓存文件、本机监听目标），并且跟包一起被
   `go test ./...` 跑到。
2. **端到端测试需要内部夹具**。例如 `internal/cli/fixture_test.go`
   提供了"写一份合法的目标缓存"的能力，那是 `internal/source` 的
   内部知识；放到 `tests/` 就得把它们导出成公共 API，
   为了测试而扩大生产代码的表面积。
3. 分散之后**没有出现重复**：各包测试各测各的边界，
   整条链路（`fetch → scan → export → aggregate → query`）
   由 CLI 的端到端测试与 CI 冒烟测试覆盖。

## 端到端覆盖在哪里

| 关注点 | 测试位置 |
| --- | --- |
| 完整链路（真实本机监听 + 真实 SQLite） | `internal/cli/cmd_scan_test.go`、`cmd_export_test.go`、`cmd_query_test.go` |
| 两级测量（真实 TCP 探测 + 假跟踪引擎） | `internal/scheduler/scheduler_test.go` |
| NextTrace 输出契约（用**真实** v1.7.3 输出做夹具） | `internal/trace/trace_test.go` + `internal/trace/testdata/` |
| 隐私过滤（导出物里不得出现内网地址） | `internal/privacy/privacy_test.go`、`internal/export/export_test.go` |
| 数据库行级行为（迁移、幂等、外键） | `internal/storage/storage_test.go` |
| 真实网络的只读检查（上游结构是否变过） | `internal/source/live_test.go` |

## 什么情况下应该放进这个目录

- 需要**跨多个包**、且无法通过某一个包的公开 API 表达的场景；
- 需要**真实外部进程**（NextTrace 二进制）且不适合放进单元测试的契约检查；
- 需要长时间运行、默认跳过（`testing.Short()`）的压力测试。

这类测试如果将来真的出现，就放到这里，并在包内测试里保持
"不依赖外部环境"的原则。

平时跑全部测试：

```bash
go test ./...
```

跳过需要外部依赖的测试：

```bash
go test ./... -short
```
