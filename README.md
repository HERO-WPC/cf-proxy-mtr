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
Phase 1  ✅ all.json 获取与解析（source 包 + fetch 命令 + 缓存）
Phase 2  ✅ Target 数据模型（model 包：不变量、归一化、去重、隐私边界）
Phase 3  ✅ TCP Probe（probe 包：错误分类、worker pool、probe 命令）
Phase 4  ✅ SQLite（storage 包：迁移、只追加时间序列、db 命令、probe --db）
Phase 5  ✅ 全量扫描 + Resume（scheduler 包：测量会话、断点续测、scan 命令）
Phase 6  ✅ 本机地区 / 运营商信息（detect 包：可解释的检测源、手动优先）
Phase 7  ⏳ NextTrace 集成
Phase 8  ⏳ TCP Probe + NextTrace 两级测量
Phase 9  ⏳ 结果导出
Phase 10 ⏳ GitHub 上传（按用户要求暂缓，等确定方案后再做）
Phase 11 ⏳ GitHub 数据聚合
Phase 12 ⏳ 查询与统计
Phase 13 ⏳ 跨平台打包与发布
```

已实现的功能（可运行）：

```text
cf-route-tester --help
cf-route-tester version
cf-route-tester fetch          # 下载 / 缓存 / 解析 all.json，输出目标数量
cf-route-tester detect         # 检测测量者所在地区与运营商，写入本地标识
cf-route-tester probe          # 对全部 IP:Port 做 TCP 连通性与延迟测量
cf-route-tester probe --db data/results.db   # 同上，并把结果写入本地 SQLite
cf-route-tester scan           # 全量扫描：测量 + 会话记录，写入数据库
cf-route-tester scan --resume  # 继续上次未完成的扫描，只测没测过的目标
cf-route-tester db stats       # 查看本地数据库状态
cf-route-tester db migrate     # 应用数据库迁移
cf-route-tester db vacuum      # 整理数据库文件
```

尚未实现（执行时明确报 `not implemented yet`，退出码 2）：
`trace`、`export`、`upload`、`aggregate`、`query`。

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

### fetch：下载并解析目标列表

```bash
cf-route-tester fetch                 # 缓存新鲜则直接用缓存，否则联网下载并写缓存
cf-route-tester fetch --refresh       # 忽略缓存，强制重新下载
cf-route-tester fetch --no-cache      # 只下载解析，不读也不写缓存
cf-route-tester fetch --verbose       # 输出解析统计、跳过原因与全部告警
cf-route-tester fetch --cache data/all.json --timeout 30s --retries 3
cf-route-tester fetch --proxy http://127.0.0.1:10808   # 需要代理时
```

输出示例（真实运行结果）：

```text
source:       https://zip.cm.edu.kg/all.json
format:       json
origin:       network
downloaded:   9.93 MiB
elapsed:      2.148s
targets:      14635
              (source reports 11610)
generated_at: 2026-10-03T03:52:35Z
fetched_at:   2026-10-03T10:28:34Z
cache:        data/all.json (written)
```

行为要点：

- **优先 JSON，失败自动切备用源**：`--url` 失败后依次重试，仍失败则尝试 `--fallback-url`；
  每失败一次都有明确原因，全部失败时把每个地址的失败原因一并输出。
- **格式按内容判断，不按 URL 后缀**：先看首字符是 `{`/`[` 还是文本行。
  这样即使上游把 JSON 放在 `all.txt`、或返回了 HTML 错误页，也能正确处理。
  （返回 HTML 而不是数据时会被判定为失败并切换数据源。）
- **非法记录跳过并记录**：非法 IP、非法端口、缺少端口都会被跳过，
  并在 `--verbose` 下输出原因计数；不会因为个别坏记录导致整体失败。
- **去重**：按 `(IP, Port)` 去重，保留首次出现的记录，输出顺序稳定。
- **缓存是原子写入**：先写临时文件再 rename，进程被中断不会留下半个文件。
- **缓存只与当前主数据源匹配**：若缓存是由备用源写入的（例如 all.txt），
  用 `--url all.json` 运行时不会复用该缓存，避免两类元数据不同的源互相污染。
- **网络失败可回退过期缓存**：默认 `--allow-stale=true`，
  但会在输出中用 `origin: cache (stale)` 与 `warning: using STALE cache` 明确提示。
- **代理**：`--proxy` 显式指定时优先；未指定则沿用环境变量 `HTTP_PROXY` / `HTTPS_PROXY`。

### detect：测量者所在地区与运营商

```bash
cf-route-tester detect                       # 检测并显示（不写入）
cf-route-tester detect --write               # 检测并写入 data/collector.json
cf-route-tester detect --source local        # 只用离线源，不联系任何外部服务
cf-route-tester detect --isp "China Mobile Zhejiang" --province Zhejiang --write
cf-route-tester detect --json                # 机器可读输出
```

输出示例（真实运行结果）：

```text
detection sources:
  [local] local
      inspects the local network stack for an egress IP version; contacts no external service
      (country/ISP cannot be determined offline without guessing)
  [exposes your public IP] geoip
      queries http://ip-api.com/json/?fields=... for the public IP, country, region, city, ISP and ASN

source results:
  local  OK      (no fields)  (1ms)
  geoip  OK      country=CN province=Shanghai city=Shanghai \
                 isp=Example Telecom asn=AS64500 ip_version=ipv4  (1.699s)

field sources:
  country:    geoip
  province:   geoip
  city:       geoip
  isp:        geoip
  asn:        geoip
  ip_version: geoip

collector profile:
  country:    CN
  province:   Shanghai
  city:       Shanghai
  isp:        Example Telecom
  asn:        AS64500
  ip_version: ipv4
```

#### 检测源与隐私

每个源都必须**如实声明**自己会联系谁、是否暴露本机 IP。地理定位在原理上
必须让服务端看到请求来源，这不是可以含糊过去的事，因此 CLI 会在发起请求
**之前**打印这句话，用户可以在那一刻中止：

| 源 | 做什么 | 是否暴露本机 IP |
| --- | --- | --- |
| `local` | 探测本机出口 IP 版本（UDP `dial` 到 RFC 5737 / RFC 3849 文档用途地址，**不发出任何报文**） | ❌ 从不 |
| `geoip` | 查询可配置的公开 geo-IP API，取国家/地区/城市/ISP/ASN | ✅ 按定义会（端点可换） |

`local` 源**只**给 IP 版本，不猜国家/城市/运营商。原因很实在：
离线推断地理位置的唯一"技巧"是问公共 DNS 解析器"我的地址是什么"，
但那返回的是**解析器自己**的出口地址，不是用户的；据此查 ASN 得到的是
DNS 服务商（Cloudflare / Google）的 ASN。把它写进采集者画像就是**错误数据**,
而错误的地区/运营商分组会直接毁掉整个数据库的可比性。因此宁可少给一个字段。

双栈环境下 `local` 会返回空的 IP 版本（无法只凭本机判断出口走哪一族），
这是刻意的：留空比如实猜错好。

#### 手动配置永远优先

公网 ASN 不一定等于用户实际感知的接入线路（家宽可能走母公司 ASN、
企业出口可能走总部 ASN），因此：

```text
手动填写的值（命令行或已有配置文件）  >  自动检测结果
```

真实示例：检测给出 `province=Shanghai, isp=Example Telecom`，
用户在自己机器上改成 `--province Zhejiang --city Hangzhou --isp "China Mobile Zhejiang"`：

```text
$ detect --isp "China Mobile Zhejiang" --province Zhejiang --city Hangzhou --write
$ cat data/collector.json
{
  "collector_id": "c-0b720f1207df74114840f2be949d229b",
  "profile": {
    "country": "CN",
    "province": "Zhejiang",          <- 手动值保持
    "city": "Hangzhou",              <- 手动值保持
    "isp": "China Mobile Zhejiang",  <- 手动值保持
    "asn": "AS64500",               <- 未被手填，仍是检测结果
    "ip_version": "ipv4"
  }
}
```

`collector_id` 在任何情况下都不会被 detect 改动：它是随机生成的匿名标识，
换了就等于丢掉历史数据的关联。

#### 写入的内容边界

标识文件里**只有**两样东西：随机 `collector_id`，以及 6 个粗粒度字段
（country / province / city / isp / asn / ip_version）。

即使 geo-IP 响应里带了 `lat` / `lon` / `zip` / `hostname` / `mac` /
`local_ip` / `device_id`，也**不会**被写入——有测试专门喂一份塞满这些字段的
响应，断言它们一个都没落盘（`TestDetectNeverWritesIdentifyingData`）。

### probe：TCP 连通性与延迟测量

```bash
cf-route-tester probe                                  # 全部目标，默认 100 并发 / 3s 超时
cf-route-tester probe --workers 100 --timeout 3s
cf-route-tester probe --limit 300                      # 只测前 300 个（快速抽样）
cf-route-tester probe --json --quiet > measurements.jsonl
cf-route-tester probe --verbose                        # 逐条输出到 stderr
```

输出示例（真实运行结果，2000 个目标）：

```text
probed:      2000 / 2000 targets
success:     1615 (80.8%)
failed:      385 (19.2%)
elapsed:     19.476s (102.7 targets/s)

latency (ms, 1615 samples):
  min 134.5   p50 234.3   p90 1195.0   p95 1265.8   max 2367.1   avg 368.1

failures by type:
  timeout:               384
  connection_refused:    1
```

行为要点：

- **测的是目标自身的端口**：`1.2.3.4:2053` 一定测 TCP 2053，绝不固定成 443。
  这条由 `TestProbeUsesActualTargetPort` 与 `TestParseTargetDoesNotOverrideExplicitPort` 锁住。
- **按地址族拨号**：IPv4 目标用 `tcp4`、IPv6 用 `tcp6`，
  避免系统把 IPv6 目标解析成 IPv4 而"测了另一个地址"。
- **严格限并发**：Job Queue → 固定 N 个 worker → 结果 channel。
  同时运行的 worker 数永不超过 `--workers`（上限 2000），
  绝不每个目标起一个 goroutine。结果边产生边消费，不把 1.5 万条结果堆在内存里。
- **失败不影响整体**：任何错误都被分类后作为**结果**返回，
  单条失败既不终止扫描也不丢数据。
- **进度走 stderr，结果与汇总走 stdout**，便于管道使用。
- **Ctrl+C**：中断后立即停止，汇总里明确标注 `warning: probing was interrupted`；
  被中断的探测记为 `canceled`，**不计入丢包**。

`--json` 每条结果一行（自带 `schema_version` 与 `client_version`）：

```json
{"schema_version":1,"client_version":"0.1.0","target_id":"45.63.67.144:443","ip":"45.63.67.144","port":443,"success":true,"latency_ms":252.3158,"timestamp":"2026-10-03T11:35:21.8611931Z"}
```

失败分类（数据库与分析的价值就在于"分得清是哪一种失败"）：

| 分类 | 含义 | 计入丢包 |
| --- | --- | --- |
| `timeout` | 超时（含等待超时） | ✅ |
| `connection_refused` | 对端拒绝：IP 可达但该端口没有服务 | ✅ |
| `network_unreachable` | 网络/主机不可达 | ✅ |
| `no_route` | 本机没有可用路由 | ✅ |
| `connection_reset` | 连接被重置 | ✅ |
| `permission_denied` | 本机策略拒绝（防火墙/安全软件） | ✅ |
| `address_not_available` | 本机缺少匹配地址族的地址 | ✅ |
| `other` | 无法归类的连接错误 | ✅ |
| `canceled` | 本次运行被中断 | ❌ 本机主动放弃，不是线路证据 |
| `invalid_target` | 目标数据非法 | ❌ 数据问题，不是线路问题 |

`connection_refused` 与 `timeout` 必须分开：前者意味着"IP 通、端口没服务"，
后者意味着"根本连不上"，两者对线路质量的结论完全不同。

错误码 → 分类的映射是**分平台**的（`internal/probe/errno_windows.go` /
`errno_unix.go`），因为 Windows 用 WSA 错误码（`WSAECONNREFUSED` = 10061）、
Unix 用 POSIX errno（`ECONNREFUSED` = 111），而且 Go 在 Windows 上
还会把部分 WSA 错误归一化成伪 errno。两套数字都注册在案，
因此同一个网络现象在 Windows 与 Linux 上得到同一个分类。

### scan：全量扫描与断点续测

```bash
cf-route-tester scan                       # 全部目标测一遍，写入 data/results.db
cf-route-tester scan --limit 300           # 只扫前 300 个（先验证链路是否通）
cf-route-tester scan --resume              # 继续上次未完成的扫描
cf-route-tester scan --resume --session 20260101T000000Z-00000000   # 指定会话
cf-route-tester scan --new                 # 强制开始新会话（默认行为）
cf-route-tester scan --country cn --province Zhejiang --city Hangzhou \
                    --isp "China Mobile" --asn 9808
```

输出示例（真实运行结果，600 个目标）：

```text
mode:        resume (skipped 156 already-measured target(s))
session:     20260101T000000Z-00000000
targets:     600 total, 444 to measure

Completed: 444 / 444
Success:   300
Failed:    144
Rate:      89.7/s
Success %: 67.6%

failures by type:
  timeout:               144

stored:      444 measurement(s)
session progress: 600 measured (411 ok, 189 failed)
session:     finished
```

#### 断点续测的判据（本阶段的核心）

判断"某个目标是否已经测过"，依据的是：

```text
目标 × 采集者 × 测量会话
```

而**不是**"数据库里有没有这个目标的历史结果"。后者会让同一个节点
永远无法重新测量全部目标——而定期重测正是本项目数据的来源。

因此：

- `scan`（默认）= 新会话 → 全部目标都测，历史数据继续累积（时间序列）；
- `scan --resume` = 继续指定会话 → 只测该会话里**还没有测量结果**的目标；
- 失败结果也算"已测"：超时、连接被拒同样是线路信息，
  重跑时不该被当成"还没测过"。

真实中断测试（1500 个目标跑到第 6 秒被强杀）：

```text
$ kill <pid>                                  # 模拟 Ctrl+C / 崩溃
$ db stats
  scan_sessions:   1       <- 会话保持"未结束"
  measurements:    N       <- 已经测到的部分留在库里

$ scan --resume                               # 只测剩下的
mode:        resume (skipped N already-measured target(s))
```

会话状态存在数据库里，`--resume` 不指定会话时用身份文件里记住的
上一个会话（`data/collector.json` 的 `last_session_id`），
再退一步则取数据库里最近一个未结束的会话。

#### 落库策略：条数 + 时间双触发

测量结果按批写入，触发条件是**两者之一**：

```text
攒够 500 条            -> 写库（保证吞吐）
或距上次写库超过 2 秒   -> 写库（保证安全）
```

只用条数是不够的：大量目标超时时（每个都要占满 timeout），
一批可能要等好几分钟才满。实测 600 个目标 / 1s 超时 / 50 并发时，
第一批 500 条要到第 6 秒才写下去——在那之前被 Ctrl+C，
数据库里**一条都没有**，整段时间白测。加了时间上限之后，
无论快慢都至少每 2 秒落一次盘。

另外，落库与收尾一律使用 **未被取消的 context**：Ctrl+C 之后
正是最需要把已测结果保存下来的时刻，用已取消的 ctx 会直接失败。

#### `--trace` 在 Phase 7 之前会明确说明"没做"

```text
$ cf-route-tester scan --trace
trace:       SKIPPED (--trace 需要 NextTrace，Phase 7 起可用)
```

宁可明确告知跳过，也不静默略过——否则汇总看起来像是跟踪过了。

### db：本地 SQLite 数据库

```bash
cf-route-tester probe --db data/results.db                 # 测量并把结果落库
cf-route-tester probe --db data/results.db --country cn --province Zhejiang \
                     --city Hangzhou --isp "China Mobile" --asn 9808
cf-route-tester db stats                                   # 行数、覆盖率、时间范围
cf-route-tester db migrate                                 # 幂等，可安全重复执行
cf-route-tester db vacuum                                  # 整理数据库文件
```

`db stats` 输出示例（真实运行结果，插入了 400 个目标的两次测量）：

```text
database:       dist/live-db/results.db
size:           440.00 KiB
schema_version: 1

rows:
  targets:         400
  collectors:      1
  scan_sessions:   0
  measurements:    800
  traces:          0

measurement coverage:
  targets with samples:    400
  collectors with samples: 1
  first:  2026-10-03T11:53:10Z
  last:   2026-10-03T11:53:24Z
  span:   14s
```

表结构（`internal/storage/migrations.go`，版本化迁移，绝不修改已发布的迁移）：

| 表 | 语义 | 写入方式 |
| --- | --- | --- |
| `targets` | 测量目标（IP × Port）+ all.json 的 `source_*` / `colo_*` 元数据 | **UPSERT**（维度表：上游地理信息会更新，`first_seen` 保持不变） |
| `collectors` | 匿名采集者（`collector_id` + 地区/运营商） | **UPSERT**（同一节点跨运行只保留一行） |
| `scan_sessions` | 一次测量会话（`finished_at` 为空表示可续测） | UPSERT |
| `measurements` | TCP 测量结果 | **只追加** |
| `traces` | 线路跟踪结果 | **只追加** |

关键设计决定：

- **时间序列只追加**：`measurements` / `traces` 永远不 UPDATE 既有行。
  "10:00 是 42ms、20:00 是 86ms"必须同时存在，否则"什么时候变差"
  根本无法回答。实测：同一批 400 个目标跑两次 → 800 条测量，
  目标表仍然是 400 行。
- **重复导入是幂等的**：每行带一个 `dedup_key`
  （`collector + session + target + timestamp` 的内容哈希，做长度前缀消除歧义），
  上面有 UNIQUE 索引。同一个 batch 重复导入会变成空操作，
  统计不会翻倍——需求第 69 条要求的正是这个。
- **失败同样入库**：`error_type` 非空即失败，且失败是**线路信息**。
  数据库层用 `CHECK (success = 1 OR error_type <> '')` 强制
  "失败必须带分类"，不依赖调用方自觉。
- **延迟用整数微秒存储**，读出时换算成毫秒：避免浮点累积误差。
- **"没有坐标"写成 NULL 而不是 0**：0,0 是几内亚湾的合法坐标，
  用 0 表示"未知"会把一批目标误判到同一个点上。
- **外键约束打开**：测量必须引用已入库的目标与采集者
  （避免产生无法分析的孤儿数据），且目标删除时测量级联删除。
- **不保存隐私信息**：`collectors` 表刻意**没有**
  `public_ip` / `local_ip` / `mac` / `hostname` / `device_id` / 精确经纬度。
  这条约束有测试守卫（直接检查数据库实际创建出来的列名）。
- **`collector_id` 随机生成**：`c-` + 32 位十六进制，来自 `crypto/rand`，
  **不由** MAC / CPU / 磁盘 / 公网 IP / 主机名推导。本地保存在
  `data/collector.json`；文件损坏时**拒绝静默重建**（那会让同一节点
  在数据里变成两个节点），而是提示用户删除文件换新 ID。
- **WAL 模式 + busy_timeout**：写入不阻塞读取，且多进程访问时
  等待而不是立刻失败。
- **迁移在事务里执行**，中途失败不会留下半套表结构；
  重复执行是幂等的。

### 关于 SQLite 依赖

使用纯 Go 的 `modernc.org/sqlite`，**不引入 CGO**。这是硬性要求：
项目要求 Windows 双击 exe 可用，并交叉编译
`linux/darwin/windows × amd64/arm64`——`mattn/go-sqlite3` 需要 CGO，
会同时破坏这两点。CI 里有一步专门在 `CGO_ENABLED=0` 下构建，
一旦有人引入需要 CGO 的依赖就会立刻失败。

代价与取舍：

```text
二进制体积  约 9.5 MiB（无驱动） -> 约 16 MiB（含驱动）
            加 -ldflags "-s -w" 后约 11 MiB
Go 工具链   modernc.org/sqlite 自身要求 go 1.26，因此 go.mod 的
            go 指令是 1.26.0（从源码构建需要 Go 1.26+，
            但发布出去的二进制对使用者没有这个要求）
```

为了"单文件、无外部依赖、可交叉编译"付这个体积是值得的。

---

## 数据源结构（实测）

`https://zip.cm.edu.kg/all.json`（2026-10 实测约 9.93 MiB）：

```json
{
  "generated_at": "2026-10-03T03:52:35.771190",
  "list": { "country": { "US": 1388, "DE": 2495 }, "ips": 11610 },
  "data": [
    {
      "ip": "1.2.3.4",
      "port": [443, 8443],
      "meta": {
        "hostname": "speed.cloudflare.com",
        "asn": 35280,
        "asOrganization": "Example Hosting",
        "country": "US", "city": "Chicago", "region": "Illinois",
        "latitude": "41.85003", "longitude": "-87.65005",
        "colo": { "iata": "ORD", "lat": 41.9786, "lon": -87.9048,
                  "cca2": "US", "region": "North America", "city": "Chicago" },
        "_port": 443,
        "country_en": "United States"
      }
    }
  ]
}
```

解析时需要注意的真实情况：

| 现象 | 处理方式 |
| --- | --- |
| `port` 是**数组**（一个 IP 最多 6 个端口），实测共 14635 个 `IP:Port` | 展开成多个独立 Target，端口原样保留，绝不固定成 443 |
| `list.ips` 是 **11610**（IP 数），不是 `IP:Port` 数 | 输出中如实同时显示 `targets` 与 `source reports`，不假装一致 |
| `latitude` / `longitude` 是**字符串** | 宽松类型转换，字符串与数字都能解析 |
| `asn` 是数字，`asOrganization` 有缺失 | 只用于展示，缺失不影响目标有效性 |
| `colo` 缺失（实测 8/14635 条） | `Target.Colo` 留空，不报错、不用目标位置冒充 |
| `colo.cca2` 与 `meta.country` 实测有 2718 条不一致 | 两者分别保存，不合并：`Location.Country` 取 meta，`Location.CCA2` 取 colo |
| `meta.latitude/longitude` 与 `colo.lat/lon` 是**两个不同位置** | 分别保存在 `Location` 与 `Colo` 中，**不做隐式回退**；需要回退时调用 `Location.ResolveCoordinates(colo)` 并显式处理 `fromColo` |
| `country_cn` 等中文字段在上游已经是乱码（形如 `鎷夎劚缁翠簹`） | 原样保留上游数据、不做静默改写；展示优先使用 `country_en` |
| `generated_at` 没有时区后缀 | 统一按 UTC 解析，保证不同时区采集者得到同一时间点 |

`https://zip.cm.edu.kg/all.txt`（备用，约 310 KiB）：

```text
101.32.169.108:443#SG
101.99.75.101:8443#NL
```

只有 `IP:Port#CC` 三样信息，没有城市 / 经纬度 / IATA，
因此文本源解析出的 `Location` 只填国家代码；两个源解析出的目标数量实测一致（14635）。

---

## 规划中的命令

以下命令**尚未实现**，`--help` 中列在 `Planned commands` 一节，
执行时会明确返回“not implemented yet”，不会静默失败：

| 命令 | 作用 |
| --- | --- |
| `detect` | 检测测量者地区与运营商信息 |
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
│   ├── cli/                      命令分发、帮助、退出码、各子命令参数解析（已实现）
│   ├── version/                  版本与公开数据 schema 版本（已实现）
│   ├── model/                    核心数据模型（已实现）
│   │   ├── model.go              Target / Location / ColoInfo / 不变量 / 归一化
│   │   ├── collector.go          CollectorProfile / collector_id（匿名）
│   │   └── dedup.go              Dedup / SortTargets / ValidateAll / Keys
│   ├── source/                   all.json 获取、解析、缓存（已实现）
│   │   ├── api.go                HTTP 下载：超时、重试、退避、代理、gzip、备用源
│   │   ├── parser.go             JSON / 文本容错解析、校验、去重、source metadata
│   │   ├── cache.go              缓存读写（原子写入）、CacheInfo、格式版本校验
│   │   └── loader.go             完整策略：缓存命中 / 刷新 / 降级 / 格式识别
│   ├── probe/                    TCP Probe 与 worker pool（已实现）
│   │   ├── probe.go              Prober / ProbeResult / 统计 / 错误分类
│   │   ├── errno.go              错误码注册表（平台无关部分）
│   │   ├── errno_windows.go      Windows WSA 错误码表
│   │   ├── errno_unix.go         Unix POSIX errno 表
│   │   └── worker.go             有界 worker pool + Runner（NextTrace 可复用）
│   ├── trace/                    Trace 引擎接口、NextTrace 调用与解析（Phase 7）
│   ├── detect/                   本机地区 / 运营商检测（已实现）
│   │   ├── detect.go             Source 接口、合并规则（手动优先）、报告
│   │   ├── source_local.go       离线源：只推断出口 IP 版本
│   │   └── source_geoip.go       联网源：可配置的 geo-IP API + 宽松解析
│   ├── identity/                 本地匿名标识 collector_id（已实现）
│   ├── scheduler/                扫描编排与断点续测（已实现）
│   │   └── scheduler.go          会话 -> 待测目标 -> Probe -> 落库 -> 收尾
│   ├── storage/                  本地 SQLite（已实现）
│   │   ├── sqlite.go             打开 / PRAGMA / 迁移 / 统计
│   │   ├── migrations.go         版本化迁移（表结构的唯一来源）
│   │   ├── model.go              Measurement / Trace / 目标与采集者 UPSERT
│   │   ├── writer.go             只追加写入（整批一个事务 + 幂等去重）
│   │   └── query.go              按目标 / 采集者 / 会话 / 时间窗口查询
│   ├── export/                   JSONL / gzip / zstd 导出（Phase 9）
│   ├── privacy/                  隐私过滤（本地地址等）（Phase 10）
│   ├── upload/                   GitHub 上传（Phase 10）
│   └── aggregate/                数据聚合（Phase 11）
├── configs/config.example.yaml   配置示例（含详细注释，Phase 13 起真正被读取）
├── data/                         本地数据（内容不提交，仅保留说明文件）
├── migrations/                   迁移说明；SQL 迁移常量放在 internal/storage/migrations.go
├── tests/                        跨模块集成测试
└── .github/workflows/ci.yml      go test / vet / build
```

`cmd` 与 `internal/cli` 只负责参数解析、调用与退出码；
业务逻辑一律放在 `internal/` 下。

排查数据来源问题时可打开内部诊断日志（仅输出到 stderr，默认关闭）：

```bash
CF_ROUTE_TESTER_DEBUG=1 cf-route-tester fetch --verbose
```

---

## 配置

配置文件为 YAML（`configs/config.example.yaml` 为示例，带详细注释）。
命令行参数可以覆盖配置文件。

> Phase 1 状态：`fetch` 的全部参数目前来自命令行（见上文），
> 配置文件尚未被读取——引入 YAML 解析会把第一个第三方依赖带进项目，
> 计划与后续阶段（`scan` / `trace`）需要的一起处理。
> 因此下表中的字段目前只是**已确定的设计约定**，`configs/config.example.yaml`
> 已经按此写好，等配置加载落地后即可直接使用。

```yaml
source:
  url: "https://zip.cm.edu.kg/all.json"
  fallback_url: "https://zip.cm.edu.kg/all.txt"
  cache: "data/all.json"
  timeout: 30s
  retries: 3

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

`all.json` 中对目标 IP 的地理信息是**目标 IP 的属性**，
与**测量者的位置**（`CollectorProfile`）是两件事，两者不会混淆：

```go
Target.Location       // 这个目标 IP 在哪（来自 all.json 的 meta）
Target.Colo           // 这个目标从哪个 Cloudflare 接入点进来（meta.colo）
CollectorProfile      // 我在哪、我用哪家运营商（本机检测或手动配置）
```

`internal/model` 中这三者被刻意分开定义：

- `Target.Location` **只**承载目标 IP 自身的地理位置（来自 `meta.country/region/city/latitude/longitude`）；
- `Target.Colo` 承载接入点信息（`iata/cca2/region/city/lat/lon`），实测约 19% 的记录里
  `meta.country` 与 `colo.cca2` 不一致，混在一起就会得出"目标在荷兰"这种错误结论；
- 坐标**不做隐式回退**：`Location` 没有坐标就是没有坐标，
  需要接入点坐标的调用方必须显式调用 `Location.ResolveCoordinates(colo)`，
  并自行处理 `fromColo` 为真（即"这是接入点坐标，不是目标坐标"）的情况；
- `CollectorProfile` 不允许出现公网 IP / MAC / 内网 IP / 主机名等字段，
  `collector_id` 由 `crypto/rand` 生成（`c-` + 32 位十六进制），**不由任何硬件信息推导**。

模型层还提供统一的不变式与工具，避免各阶段各写一套：

```go
target.Validate()        // ID == TargetID(IP, Port)、IP 合法、端口范围、IPVersion 一致、
                         // 国家代码 2 位大写、坐标成对且在范围内
target.Normalize()       // 归一化（去空白、国家代码大写、IP 规范形式），幂等
model.Dedup              // 按 (IP, Port) 去重的唯一实现点，保留首次出现与顺序
model.SortTargets        // 按 ID 排序（导出/对比用）
model.NewTargetFromStrings / model.ParseAddr / model.ValidPort
```

`Normalize` **不做静默数据修正**：它只处理空白与大小写，遇到越界坐标只会把
`HasCoordinates` 置为 false，不会改写数值——数据问题应当被看见，而不是被抹平。

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
10. 数据正确性优先于"省一次下载 / 少一次请求"：
    例如宁可重新下载，也不复用来自另一个数据源的缓存。

本地验证：

```bash
go test ./...
go vet ./...
go build ./...
```

CI（`.github/workflows/ci.yml`）在以上基础上额外执行：

```text
gofmt -l .                       必须无输出
go test ./... -race              需要 cgo，本地未装 gcc 时可用 CGO_ENABLED=0 跳过
linux/darwin/windows × amd64/arm64 交叉编译
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
