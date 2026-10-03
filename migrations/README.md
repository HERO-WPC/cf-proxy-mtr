# migrations/

这个目录**不存放 SQL 文件**。数据库迁移语句以**字符串常量**的形式
维护在 Go 源码里（[`internal/storage/migrations.go`](../internal/storage/migrations.go)）。

这样做的理由：

1. **迁移随二进制一起发布**，运行时不需要定位外部 SQL 文件；
2. 数据库版本表与迁移逻辑在同一个包里，容易被测试覆盖；
3. **单文件分发**（Windows 双击 exe）不会因为缺少 SQL 文件而失败。

## 迁移的规则

- 迁移**只增不改**：已经发布过的迁移语句永远不修改。
  用户库里的 `schema_migrations` 记录了它执行过哪些版本，
  改动已发布的迁移会让"老库"与"新库"的结构悄悄分叉。
- 需要变更结构时**新增一条迁移**，而不是改旧的。
- 每条迁移在**事务**里执行，中途失败不会留下半套表结构；
  重复执行是幂等的。
- 有两种版本号，含义不同，不要混淆：
  - `schema_migrations`（本目录相关）：**本地表结构**的演进版本；
  - `schema_version`（见 [`internal/storage/version_info.go`](../internal/storage/version_info.go)）：
    **公开数据格式**的语义版本，写在每条 measurement / trace 上。

## 当前的表结构

初始迁移（版本 1）建立了 5 张表：

| 表 | 用途 | 写入方式 |
| --- | --- | --- |
| `targets` | 测量目标（IP × Port）与 all.json 元数据 | UPSERT（维度表） |
| `collectors` | 匿名采集者（随机 ID + 地区/运营商） | UPSERT |
| `scan_sessions` | 一次测量会话（支持断点续测） | UPSERT |
| `measurements` | TCP 测量结果 | **只追加**（时间序列） |
| `traces` | 线路跟踪结果 | **只追加**（时间序列） |

结构与设计取舍的完整说明见根目录 README 的
「db：本地 SQLite 数据库」一节。

## 这个目录用来放什么

- 迁移的**设计记录**：为什么某个索引存在、某次字段变更的原因；
- 需要人工审查的、较大规模迁移的**草稿**；
- 表结构的**变更历史**。

即：这里放的是"给人看的说明"，可执行的迁移在 Go 源码里。
