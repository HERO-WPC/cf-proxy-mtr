# cf-route-tester

`cf-route-tester` 是一个用 Go 编写的**跨平台网络线路众测工具**。

它不是一个传统的 IP 扫描器。它的目标是：

> 让大量不同地区、不同运营商的用户，在**自己的真实网络环境**中，
> 对**同一批 IP:Port** 做完整线路测量，然后把匿名结果汇总，
> 逐步建立一个**长期维护的公网线路数据库**。

最终希望能回答这类问题：

```text
某个 IP:Port
  从Sample Province移动访问怎么样？
  从Sample Province电信访问怎么样？
  从广东移动访问怎么样？
  从北京联通访问怎么样？
  从日本访问怎么样？

这个 IP 对某个运营商的线路经过哪里？延迟多少？
什么时候变差？是否存在晚高峰拥塞？不同运营商是否走不同出口？
```

数据准确、可重复、可追溯、可聚合，比 UI 更重要。

---

## 当前状态

```text
Phase 0  ✅ 项目初始化（Go module / CLI / version / help / README / 基础测试）
Phase 1  ⏳ all.json 获取与解析
Phase 2  ⏳ Target 数据模型
Phase 3  ⏳ TCP Probe
Phase 4  ⏳ SQLite
Phase 5  ⏳ 全量扫描 + Resume
Phase 6  ⏳ 本机地区 / 运营商信息
Phase 7  ⏳ NextTrace 集成
Phase 8  ⏳ TCP Probe + NextTrace 两级测量
Phase 9  ⏳ 结果导出
Phase 10 ⏳ GitHub 上传
Phase 11 ⏳ GitHub 数据聚合
Phase 12 ⏳ 查询与统计
Phase 13 ⏳ 跨平台打包与发布
```

**Phase 0 只包含**：Go module、CLI 骨架、`version`、`help`、README、基础测试。

Phase 0 **明确不包含**（也没有实现）：
`all.json` 下载、TCP Probe、NextTrace、SQLite、GitHub 上传、Aggregator、GUI。

---

## 安装与构建

需要 Go 1.21 或更高版本（`go.mod` 中的最低版本）。

```bash
git clone <this-repo>
cd cf-route-tester

go build ./...
go build -o bin/cf-route-tester ./cmd/cf-route-tester
```

Windows：

```cmd
go build -o bin\cf-route-tester.exe .\cmd\cf-route-tester
bin\cf-route-tester.exe version
```

发布构建可以注入版本信息（可选，未注入时显示 `unknown`）：

```bash
go build -ldflags "\
  -X github.com/cf-route-tester/cf-route-tester/internal/version.Commit=$(git rev-parse --short HEAD) \
  -X github.com/cf-route-tester/cf-route-tester/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o bin/cf-route-tester ./cmd/cf-route-tester
```

最终产物是**单个可执行文件**，不需要 Python / Node.js / Java。

---

## 使用

```bash
cf-route-tester --help
cf-route-tester version
cf-route-tester version --verbose
cf-route-tester --version
```

`version` 输出：

```text
cf-route-tester
version: 0.1.0
```

`version --verbose` 输出（含构建细节，便于排查“结果来自哪个版本”）：

```text
client:         cf-route-tester
version:        0.1.0
schema_version: 1
commit:         unknown
build_date:     unknown
go_version:     go1.21.x
platform:       windows/amd64
```

退出码约定：

```text
0  成功
1  运行期错误（网络、数据库、外部工具等）
2  用法错误（未知命令、未知参数、参数不合法）
```

---

## 规划中的命令

以下命令**尚未实现**，`--help` 中列在 `Planned commands` 一节，
执行时会明确返回“not implemented yet”，不会静默失败：

| 命令 | 作用 |
| --- | --- |
| `fetch` | 下载并缓存 `all.json` 目标列表 |
| `detect` | 检测测量者地区与运营商信息 |
| `probe` | 对全部 `IP:Port` 执行 TCP 连通性与延迟测量 |
| `scan` | 全量扫描：TCP Probe + NextTrace 两级测量（支持 `--resume`） |
| `trace` | 使用 NextTrace 对指定 `IP:Port` 做线路跟踪 |
| `export` | 导出 measurements / traces 为 JSONL 及压缩批次 |
| `upload` | 将匿名压缩批次上传到 GitHub 数据仓库 |
| `aggregate` | 聚合 `data/raw` 生成按地区 / 运营商分组的统计结果 |
| `query` | 查询某个 `IP:Port` 在不同地区 / 运营商下的线路画像 |
| `db` | 本地 SQLite 数据库维护（统计等） |

---

## 设计要点

### 每个用户测全部目标

不是把目标拆给不同用户，而是每个用户都完整测量一次全部 `IP:Port`：

```text
用户 A → 完整 all.json → 全部 IP:Port
用户 B → 完整 all.json → 全部 IP:Port
用户 C → 完整 all.json → 全部 IP:Port
```

因此每个用户都是一个**完整测量节点**，汇总后才有地区 × 运营商的横向可比性。

### 核心数据关系

```text
IP:Port
  + 测试者地区 / 运营商
  + IPv4 / IPv6
  + 时间
  + TCP 延迟 / 丢包
  + Traceroute / NextTrace 路径
```

### 两级测量

```text
全部 Target
     ↓  Level 1：轻量 TCP Probe（默认 workers=100）
可连接 Target
     ↓  Level 2：NextTrace（默认 workers=10）
线路路径
```

TCP Probe 与 Trace 是**两个独立的 worker pool**。
绝不会出现同时启动上万个 NextTrace 进程的情况。

### TCP Probe 与 NextTrace 的职责不同

```text
TCP Probe  → 这个 IP:Port 能不能连？连接需要多少毫秒？
NextTrace  → 去这个 IP:Port 的路径经过哪里？
```

两者结果**分别保存**，绝不混在一张表里。

### 失败也是线路信息

`timeout`、`connection refused`、`network unreachable` 同样会被保存。
因为对众测数据库来说：

```text
Sample Province移动 / 1.2.3.4:443 / 成功率 12%
```

本身就是非常有价值的事实。

### 历史数据永不覆盖

```text
2026-10-03 10:00 → 42ms
2026-10-03 20:00 → 86ms
```

两条结果同时保留，数据库是 time series，使用 `INSERT` 而不是 `UPDATE`。

### 隐私

以下几类信息**永远不采集、不保存、不上传**：

```text
公网 IP / MAC / 内网 IP / 主机名 / 家庭住址 / 设备唯一标识
```

`collector_id` 是随机生成的匿名标识，**不由硬件信息推导**，
仅用于把同一个匿名节点的历史数据关联起来。

Trace 结果在**公开上传之前**必须过滤本地网络地址：

```text
IPv4: 127.0.0.0/8, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16
IPv6: ::1, fc00::/7, fe80::/10
```

目标 IP 本身不属于过滤对象。本地数据库可以保留完整路径。

### 外部依赖

线路跟踪使用 [NextTrace-core](https://github.com/nxtrace/NTrace-core) 作为
**外部可执行文件**调用，本项目**不重新实现 traceroute**，
也**不把 NextTrace 源码复制进本仓库**（便于独立升级，并避免把外部项目源码
耦合进本项目）。NextTrace-core 当前仓库标注为 **GPL-3.0**，
正式发布前需要复核其许可证及依赖的许可证要求。

调用形式会显式指定传统 traceroute 模式，并使用 TCP + 实际端口：

```bash
nexttrace --traceroute --tcp --port <PORT> --json <IP>
```

显式 `--traceroute` 的原因：NextTrace 官方说明未来默认模式会迁移到 MTR，
传统 traceroute 模式将继续通过 `--traceroute` 提供，因此不能依赖默认行为。
`IP:Port` 中的 `Port` 必须原样传给 NextTrace，不能固定成 443。

NextTrace 不存在时，**TCP Probe 功能不受影响**，只会提示：

```text
NextTrace not found.
Please install NextTrace or configure trace.nexttrace.binary.
```

### 公开数据的 Schema

所有公开数据都带 `schema_version`，且内部业务模型**不直接绑定
NextTrace 的原始 JSON**：

```text
NextTrace JSON → NextTrace Parser → cf-route-tester TraceResult → SQLite
```

NextTrace 更新输出格式时，只需要改解析层。

### 客观指标，不做主观评分

数据库只保存客观事实：

```text
latency / loss / samples / success_rate / path / timestamp
```

第一版不会引入 `★★★★★`、`极品线路`、`垃圾线路`、`推荐线路` 这类主观评分。
把原始事实保存好，后续由使用者自行分析。

---

## 项目结构

```text
cf-route-tester/
├── cmd/cf-route-tester/main.go   入口：构造环境、调用 CLI、返回退出码
├── internal/
│   ├── cli/                      命令分发、帮助、退出码（Phase 0 已实现）
│   ├── version/                  版本与 schema 版本（Phase 0 已实现）
│   ├── model/                    Target / Location / CollectorProfile 等
│   ├── source/                   all.json 获取、解析、缓存
│   ├── probe/                    TCP Probe 与 worker pool
│   ├── trace/                    Trace 引擎接口、NextTrace 调用与解析
│   ├── detect/                   本机地区 / 运营商检测
│   ├── scheduler/                任务编排与断点续测
│   ├── storage/                  SQLite 与 migrations
│   ├── export/                   JSONL / gzip / zstd 导出
│   ├── privacy/                  隐私过滤（本地地址等）
│   ├── upload/                   GitHub 上传
│   └── aggregate/                数据聚合
├── configs/config.example.yaml   配置示例（含详细注释）
├── data/                         本地数据（内容不提交，仅保留说明文件）
├── migrations/                   迁移说明；SQL 迁移常量放在 internal/storage/migrations.go
├── tests/                        跨模块集成测试
└── .github/workflows/ci.yml      go test / vet / build
```

`cmd` 与 `internal/cli` 只负责参数解析、调用与退出码；
业务逻辑一律放在 `internal/` 下。

---

## 配置

配置文件为 YAML（`configs/config.example.yaml` 为示例，带详细注释）。
命令行参数可以覆盖配置文件。

```yaml
source:
  url: "https://zip.cm.edu.kg/all.json"
  fallback_url: "https://zip.cm.edu.kg/all.txt"
  cache: "data/all.json"

probe:
  workers: 100
  timeout: 3s

trace:
  enabled: true
  workers: 10
  timeout: 15s
  mode: tcp
  nexttrace:
    binary: "nexttrace"

storage:
  sqlite: "data/results.db"

collector:
  country: ""
  province: ""
  city: ""
  isp: ""
  asn: ""
  ip_version: "ipv4"
```

`collector` 允许手动填写：公网 ASN 不一定等于用户实际感知的运营商 / 接入线路，
所以自动检测只作为辅助，手动配置优先。

---

## 数据来源

```text
主：https://zip.cm.edu.kg/all.json
备：https://zip.cm.edu.kg/all.txt
```

`all.json` 中对目标 IP 的地理信息（`source_country` / `source_region` /
`source_city` / `source_iata` / `source_latitude` / `source_longitude`）
是**目标 IP 的属性**，与**测量者的位置**是两件事，两者不会混淆。

---

## 开发约束

1. 使用 Go；CLI 优先，第一阶段不做 GUI（不引入 Electron / Wails / Fyne / Qt）。
2. 跨平台：Windows / Linux / macOS，amd64 与 arm64。
3. 不重新实现 traceroute，使用 NextTrace 作为线路跟踪引擎。
4. 所有数据保存到本地 SQLite，历史数据永不覆盖。
5. 不允许为了性能丢弃结果：失败结果同样入库。
6. 单个目标失败不能影响整个任务；外部工具异常必须可恢复。
7. 严格控制并发，绝不无限创建 goroutine 或进程。
8. 每个阶段完成后必须 `go test ./...`、`go vet ./...`、`go build ./...` 全部通过，
   才进入下一阶段。
9. 不硬编码任何 GitHub Token；Token 不写入日志、JSON，也不提交到仓库。

本地验证：

```bash
go test ./...
go vet ./...
go build ./...
```

---

## 数据流总览

```text
                   zip.cm.edu.kg
                         │
                         ▼
                    all.json
                         │
                         ▼
               每个用户完整下载
                         │
                         ▼
               全部 IP:Port TCP Probe
                         │
                         ▼
                   NextTrace
                         │
                         ▼
            本地 SQLite 历史数据库
                         │
                         ▼
                 匿名结果压缩
                         │
                         ▼
                    GitHub Raw
                         │
                         ▼
                  GitHub Actions
                         │
                         ▼
                 Aggregated DB
                         │
          ┌──────────────┼──────────────┐
          ▼              ▼              ▼
       Sample Province移动        Sample Province电信        广东移动
          │              │              │
          └──────────────┼──────────────┘
                         ▼
              IP:Port 线路画像数据库
```

---

## 许可证

尚未指定。在正式发布之前需要确定本项目的许可证，
并复核 NextTrace-core（GPL-3.0）等外部依赖的许可证要求。
详见 `LICENSE`。
