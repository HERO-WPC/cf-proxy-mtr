# data/

本目录存放本地运行产生的数据。**目录内容不提交到仓库**（见 `.gitignore`）。

```text
data/
├── all.json      Phase 1 起：all.json 本地缓存
├── results.db    Phase 3 起：本地 SQLite 历史数据库
├── bin/          Phase 7 起：可选的 nexttrace 等外部二进制
├── raw/          Phase 10 起：待上传 / 已上传的匿名压缩批次
└── aggregate/    Phase 11 起：聚合结果
```

这里只保留本说明文件，用来保证目录结构在仓库中存在。
