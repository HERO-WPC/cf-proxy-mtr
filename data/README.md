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
- 缓存文件格式版本与 `version.SchemaVersion` 绑定，不匹配即视为失效并重新下载，
  因此升级程序不会读到旧格式缓存。
- 写入是**原子**的（临时文件 + rename），被 Ctrl+C 中断不会留下半个文件。
- 缓存只与本次运行的**主数据源**匹配：由 `all.txt` 写入的缓存不会被
  `--url all.json` 复用，避免两个元数据不同的源互相污染。

这里只保留本说明文件与 `.gitignore`，用来保证目录结构在仓库中存在。
