# data/

本目录存放本地运行产生的数据。**目录内容不提交到仓库**（见 `data/.gitignore`）。

```text
data/
├── all.json      Phase 1：all.json 的本地缓存（解析后的目标列表，非原始响应）
├── results.db    Phase 4：本地 SQLite 历史数据库
├── bin/          Phase 7：可选的 nexttrace 等外部二进制
├── raw/          Phase 10：待上传 / 已上传的匿名压缩批次
└── aggregate/    Phase 11：聚合结果
```

关于 `all.json` 缓存：

- 它是**解析后**的目标列表（含 `schema_version` / `written_at` / `source` /
  `stats` / `targets`），不是原始响应字节。
  这样后续 `scan` 不必每次重新解析约 10 MiB 的 JSON。
- 缓存文件格式版本是**独立**的 `cacheSchemaVersion`（见 `internal/source/cache.go`），
  与公开数据的 `version.SchemaVersion` 无关。版本不匹配时缓存直接视为不存在并重新下载，
  因此升级程序后不需要手工删除缓存文件。
  之所以必须独立：缓存结构会随内部重构变化（v2 把 Cloudflare 接入点 `colo`
  从 `location` 中拆了出来），若与公开 schema 绑定，旧缓存会被"成功解析"却
  悄悄丢掉字段，或者内部重构会被误判成公开数据格式变更。
- 写入是**原子**的（临时文件 + rename），被 Ctrl+C 中断不会留下半个文件。
- 缓存只与本次运行的**主数据源**匹配：由 `all.txt` 写入的缓存不会被
  `--url all.json` 复用，避免两个元数据不同的源互相污染（否则会出现
  "JSON 源返回文本源数据"这种静默替换）。

这里只保留本说明文件与 `.gitignore`，用来保证目录结构在仓库中存在。
