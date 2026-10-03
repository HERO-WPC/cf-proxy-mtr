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
Phase 7  ✅ NextTrace 集成（trace 包：外部进程、真实 JSON 解析、trace 命令）
Phase 8  ✅ TCP Probe + NextTrace 两级测量（只跟踪探测成功的目标，可续测）
Phase 9  ✅ 结果导出（export 包 + privacy 包：JSONL / gzip、公开 Schema、隐私过滤）
Phase 10 ⏸️ GitHub 上传（按用户要求暂缓：先把全部流程在本地跑通）
Phase 11 ✅ 数据聚合（aggregate 包：按目标 × 地区 × 运营商分组、跨节点对比）
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
cf-route-tester scan --trace   # 扫描后对**探测成功**的目标做线路跟踪
cf-route-tester trace --target 1.1.1.1:443   # 单独跟踪一个目标
cf-route-tester export --list-sessions       # 查看可导出的会话
cf-route-tester export --session <id>        # 导出为公开 JSONL（自动隐私过滤）
cf-route-tester aggregate --input batch.jsonl.gz   # 把导出物聚合为统计结果
cf-route-tester db stats       # 查看本地数据库状态
cf-route-tester db migrate     # 应用数据库迁移
cf-route-tester db vacuum      # 整理数据库文件
```

尚未实现（执行时明确报 `not implemented yet`，退出码 2）：
`query`、`upload`。

> `upload` 按用户要求暂缓：先确保 `fetch → detect → scan → trace → export → aggregate`
> 整条链路在本地完全跑通，凭据与归属方案确定后再做。

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

#### 两级测量：只跟踪探测成功的目标

`scan --trace` 在 TCP 测量**之后再**执行线路跟踪，且**只对探测成功的目标**跟踪：

```bash
cf-route-tester scan --trace
cf-route-tester scan --trace --trace-binary "data/bin/nexttrace.exe" --trace-mode icmp
cf-route-tester scan --trace --trace-workers 4 --trace-timeout 25s
```

```
--- Level 2: route trace (only TCP-successful targets) ---
candidates:  10 TCP-successful target(s)
to trace:    10
Completed:   10 / 10
Success:     10
Failed:      0
Success %:   100.0%
Avg hops:    23.3

stored:      10 trace(s)
```

为什么必须这样分级（需求第 38 条）：连 TCP 都连不上的目标，跑 traceroute
大概率在中途就断了。为它们花几十秒既得不到有效路径，又拖慢整次扫描。

真实运行（12 个目标 / ICMP 模式 / 真实 NextTrace v1.7.3）：

```text
Completed: 12 / 12          <- Level 1: TCP
Success:   10
Failed:    2                 (timeout)
Success %: 83.3%

--- Level 2: route trace (only TCP-successful targets) ---
candidates:  10 TCP-successful target(s)   <- 12 - 2 个超时
to trace:    10
Success %:   100.0%
Avg hops:    23.3
stored:      10 trace(s)
```

注意两级是**分别汇报**的。"跟踪成功率 100%"指的是"能连上的目标里，
全部成功拿到了路径"，与"整体可用率 83.3%"是两个不同的量——
混在一起会让人以为线路质量比实际更好。

#### 第二级也有断点续测

不只看第一级。已经跟踪过的目标在 `--resume` 时会被跳过：

```text
--- Level 2: route trace (only TCP-successful targets) ---
candidates:  10 TCP-successful target(s)
already done:10 target(s) traced in this session, skipped
to trace:    0
```

理由同样是代价：一次 traceroute 要几秒到几十秒（上面实测平均 17 秒），
中断重跑时把这些重跑一遍代价很高。

幂等性也做了实测：

```text
$ scan --trace --limit 12 ...          # 第一次
measurements=12 traces=10

$ scan --trace --resume ...            # 再跑一次
mode:        resume (skipped 12 already-measured target(s))
targets:     12 total, 0 to measure
session:     finished

$ 再次统计
measurements=12 traces=10              # 没有翻倍
```

#### 会话状态与数据事实必须一致

有一个容易忽略的边界：对一个**已经跑完**的会话再执行 `--resume`。

这不该报错——那是用户的正常疑问（"我上次跑完了吗？"）——
但也不该往一个已结束的会话里继续追加数据。现在的语义是：

| 情况 | 行为 |
| --- | --- |
| 会话还开着，但数据已经齐了 | 正常收尾，标记会话结束（幂等） |
| 会话已结束，再次 `--resume` | 稳定报告"没有要测的"，**不报错、不重复测量** |
| 会话已结束，**却还有目标没测** | 报错。这是真问题：会话被提前关闭，状态与数据矛盾 |

第三种情况以前会被伪装成"你续测了一个已结束的会话，请用 --new"，
让人以为是操作问题；现在的错误信息直说会话是被提前关闭的：

```text
session "..." is already finished but 3 target(s) are still unmeasured;
the session was closed prematurely — start a new scan with --new
```

#### 跟踪结果的落库批更小

| 级别 | 批大小 | 时间上限 | 理由 |
| --- | --- | --- | --- |
| measurements | 500 | 2 秒 | 快（毫秒级），攒大点省 fsync |
| traces | **20** | **5 秒** | 慢（十几秒一条），攒 500 条要几小时 |

跟踪结果里同时保存两样东西，用途完全不同：

- `trace_json`：**归一化后**的跳列表（业务查询只依赖它）；
- `raw_json`：引擎原始输出（仅诊断；上游改格式不会让历史数据失去意义）。

本地库**不做隐私过滤**：`LocalFiltered` 为 false，内网地址原样保留，
供用户自己诊断路径。过滤属于导出层（Phase 9）。

### trace：使用 NextTrace 做线路跟踪

```bash
cf-route-tester trace --target 1.1.1.1:443                  # 默认 TCP，测 443
cf-route-tester trace --target 1.1.1.1:2053                 # 端口原样传给引擎
cf-route-tester trace --target 1.1.1.1 --mode icmp          # 无需管理员权限
cf-route-tester trace --binary "C:/Tools/nexttrace.exe" --target 1.1.1.1:443
cf-route-tester trace --limit 20 --workers 10 --json        # 从目标列表批量跟踪
cf-route-tester trace --target 1.1.1.1:443 --verbose        # 打印完整跳表
```

输出示例（真实运行结果，NextTrace v1.7.3，ICMP 模式）：

```text
1.1.1.1:443  30 hops (15072.2 ms)
   1  192.168.1.1                                 0.75 ms
   2  192.168.1.1                                 1.23 ms
   3  *
   4  203.0.113.4                               3.45 ms  AS64500  example.net
   5  203.0.113.5                              3.97 ms  AS64500  example.net
   6  *
   ...
  30  *

traced:      1 target(s)
success:     1 (100.0%)
failed:      0 (0.0%)
average hops: 30.0
```

#### 引擎调用方式（实测修正）

需求文档里给的形态是：

```bash
nexttrace --traceroute --tcp --port <PORT> --json <IP>
```

**实测 NextTrace v1.7.3 里 `--traceroute` 这个参数并不存在**，传了会得到
`unknown arguments`。该版本的相关参数是：

| 参数 | 实际作用 |
| --- | --- |
| `--mtr` / `-t` | MTR 模式 |
| `--report` / `-r` | 报告模式（**隐含 MTR**，与 `--json` 互斥） |
| `--classic` / `-c` | 经典模式 |
| `--tcp` / `--udp` | 指定探测协议 |
| `--icmp-mode <n>` | ICMP 模式（**不是** `--icmp`） |

因此本项目实际使用（并已在真实二进制上验证）：

```bash
nexttrace --json --tcp --port <目标的端口> <IP>
nexttrace --json --udp --port <目标的端口> <IP>
nexttrace --json --icmp-mode 0 <IP>
```

原文档"必须显式 `--traceroute`"的**意图**仍然被遵守了，只是用另一种方式表达：
它的本意是"不要依赖默认模式，因为它会变"。这里的做法是
**显式写死探测协议开关**，并且完全不碰 MTR 相关参数——
即使将来默认模式真的变成 MTR，我们要的仍然是"传统 traceroute + 指定协议 + 指定端口"。

#### 端口必须原样传递

`1.2.3.4:2053` 一定用 `--port 2053`，绝不固定成 443。
这条由 `TestBuildArgsUsesActualTargetPort` 逐端口覆盖。

#### Windows 上 TCP/UDP 模式需要管理员权限

NextTrace 在 Windows 上的 TCP/UDP 探测依赖 **WinDivert**：

```text
1. 首次使用先释放运行时（普通权限即可）：
     nexttrace --init          # 会生成 WinDivert.dll 与 WinDivert64.sys
2. 跟踪时必须用「以管理员身份运行」的终端：
     cf-route-tester trace --target 1.1.1.1:443
```

没有管理员权限时，程序会**如实分类**而不是报一句没用的错误：

```text
traced:      1 target(s)
success:     0 (0.0%)
failed:      1 (100.0%)

failures by type:
  permission_denied:       1

note: "permission_denied" means the trace could not be performed
      (environment / permission), not that the path is bad.
```

这个分类是**中英文双语匹配**的：NextTrace 在 Windows 上会用中文报
"依赖 WinDivert，但当前进程没有管理员权限"，只匹配英文关键词会让中文用户
看到毫无帮助的 `other`。ICMP 模式（`--mode icmp`）不需要管理员权限，
可以先用它验证链路。

#### 真实 JSON 结构（v1.7.3 实测，与最初的假设有三处关键差异）

```json
{
  "Hops": [
    [
      {
        "Success": true,
        "Address": { "IP": "203.0.113.4", "Zone": "" },
        "Hostname": "",
        "TTL": 4,
        "RTT": 4996800,
        "Error": null,
        "Geo": {
          "asnumber": "64500",
          "country": "中国", "country_en": "China",
          "prov": "Sample Province", "prov_en": "Zhejiang",
          "city": "Sample City", "city_en": "Hangzhou",
          "owner": "example.net ", "isp": "移动",
          "lat": 30.29, "lng": 120.16
        }
      }
    ]
  ],
  "StopReason": { "hop": 30, "reason": "max_hops" },
  "TraceMapUrl": "https://assets.nxtrace.org/tracemap/....html"
}
```

三个**凭文档猜一定会错**的地方：

1. **`RTT` 的单位是纳秒**。`4996800` 表示 4.9968 ms，不是 4996.8 ms。
   当毫秒用会把延迟放大 100 万倍，而"数字看起来大"很容易被误读成
   "网络很差"而不是"单位错了"。解析层统一除以 `1e6`。
2. **`Hops` 是二维数组**：外层是 TTL，内层是同一 TTL 的多次探测。
   同一跳的多个 RTT 必须**聚合成一跳**，否则 30 跳会变成 90 跳，
   "平均跳数"直接错三倍。
3. **只有 `*_en` 字段是干净文本**：`country` / `prov` / `city` 是本地化文本，
   而上游给的是**乱码**（GBK 被当 UTF-8 解：`"�й�"`）。因此优先采用
   `country_en` / `prov_en` / `city_en`。这与 all.json 里 `country_cn`
   乱码是同一类上游问题。

另外 `country_en` 是国家**全称**（"China"）而不是代码。我们没有权威的
全称→代码映射表，因此**留空**而不是编一个代码——编错的国家代码会让
聚合出现错误的地区维度。

真实输出已作为测试夹具保存在
`internal/trace/testdata/nexttrace_v1.7.3_icmp.json`，
并有测试断言它没有被手工改过（夹具一旦被编辑就不再是"真实输出"）。

#### Trace 与 TCP Probe 完全解耦

NextTrace 不存在时，`fetch` / `probe` / `scan` 全都照常可用：

```text
$ cf-route-tester trace --target 1.1.1.1:443
Error: ... NextTrace not found.
Please install NextTrace or configure trace.nexttrace.binary.
```

只有 `trace` 命令（以及后续的 `scan --trace`）受影响，退出码为 1（运行期错误），
而不是 2（用法错误）——脚本可以据此区分"参数写错"与"环境缺东西"。

#### 并发必须比 Probe 小得多

```text
probe   默认 100 并发（纯 socket，无外部进程）
trace   默认 10 并发，上限 64（每个 worker 会启动一个 nexttrace 进程）
```

两者共用同一个有界 worker pool（`internal/worker`），
因此并发与取消语义只有一份实现；区别只在"每个 job 做什么"。

### export：导出可公开的 JSONL

```bash
cf-route-tester export --list-sessions              # 先看有哪些会话
cf-route-tester export --session <id> --dry-run     # 看会导出多少行、过滤掉多少
cf-route-tester export --session <id>               # 导出（默认 gzip）
cf-route-tester export --format jsonl --out -       # 纯 JSONL 到标准输出
cf-route-tester export --kind measurements          # 只导出测量
cf-route-tester export --since 2026-10-03 --until 2026-10-03
```

真实运行结果（15 个目标 / 14 条跟踪）：

```text
session:     (all sessions of this collector)
kind:        all
output:      dist/exp/all.jsonl.gz
format:      jsonl.gz

rows in scope:
  measurements: 15 read, 15 exported
  traces:       14 read, 14 exported
  total exported: 29
  output size:    5.95 KiB

privacy filtering:
  28 private hop address(es) replaced with private-v4/private-v6
  1 error message(s) had IPs/paths/usernames replaced
```

#### 这一层是唯一的"对外闸门"

`export` 是整个项目里**唯一**允许产生可公开数据的路径。它承担三件事：

1. **定型公开 Schema**：字段名一旦确定，聚合、网站、第三方分析都依赖它；
2. **应用隐私过滤**：本地库保留内网地址供用户诊断，导出时必须过滤；
3. **流式输出**：库可能有几十万行，不能先读进内存。

#### 隐私过滤的规则

| 对象 | 处理 | 理由 |
| --- | --- | --- |
| 目标是内网 / 保留地址的行 | **整行丢弃** | 那是用户自己的测试数据（例如用本机监听验证），既无普遍价值又会暴露内网地址规划 |
| 路径上的内网地址 | 替换成 `private-v4` / `private-v6` | 跳的位置与顺序本身是线路信息（"第 3 跳还在内网"说明流量尚未出局域网） |
| 错误信息里的 IP / 路径 / 用户名 | 替换成 `[addr]` / `[path]` / `Users\[user]` | 错误信息可能来自本机侧，而这些内容对分析没有价值 |
| 采集者 `collector_id` | **保留** | 随机生成的匿名标识，不含可定位到个人的信息；没有它就无法回答"不同节点看到的线路是否不同" |
| 目标 IP / 端口 | **保留** | 来自公开的 all.json，本身就是公开数据 |
| `raw_json`（引擎原始输出） | **不导出** | 体积大（实测单条 24 KB）且含未经脱敏的中间信息 |

过滤结果**必须被明确报出**。静默丢弃数据的导出是不可信的：
用户无法判断产物是否完整，也就无法信任它。

#### 判定内网地址的范围比 RFC1918 更宽

```
127.0.0.0/8, ::1          环回
10/8, 172.16/12, 192.168/16   RFC1918
169.254/16, fe80::/10      链路本地（暴露本地拓扑）
100.64/10                  运营商级 NAT（能定位运营商内网）
192.0.2/24, 198.51.100/24, 203.0.113/24, 2001:db8::/32  文档用途
198.18/15                  基准测试
240/4, 224/4, ff00::/8     保留与组播
fc00::/7                   IPv6 唯一本地
```

边界由单元测试逐条钉住（含 `172.15` / `172.32` / `100.63` / `100.128`
这些"紧邻但不属于"的地址，最容易写错一位）。

#### 压缩：gzip 可用，zstd 暂不可用

```text
$ cf-route-tester export --format zstd
Error: usage error: zstd is not available in this build: Go 1.26's standard
library does not expose a zstd encoder, and adding a third-party one is
deferred. Use --format jsonl.gz instead.
```

Go 1.26 的 `compress` 包只有 gzip（`internal/zstd` 不可导入），
而引入第三方实现会明显增加二进制体积。因此先用 gzip——它已经能满足
实际需求（实测 53.6 KiB → 5.95 KiB，约 **9 倍**）。

不支持时给出**原因**与**替代方案**，而不是一句 "not supported"。

#### 导出是确定性的

同一个数据库导出两次，gzip 产物**字节完全相同**：

```text
a: A36F04FAD963807AFB3EDB38FB5B2F2D564805B6F9534CA86006810845F61D70
b: A36F04FAD963807AFB3EDB38FB5B2F2D564805B6F9534CA86006810845F61D70
IDENTICAL — export is reproducible
```

默认情况下 `compress/gzip` 会把**当前时间**写进头部，导致两次导出字节不同，
"重新导出并比对哈希"这种校验方式就失效了。这里把 `ModTime` 置零。

#### 每个导出行的字段

```json
{
  "schema_version": 1,
  "kind": "measurement",
  "client_version": "0.1.0",
  "target_id": "159.60.146.81:443",
  "ip": "159.60.146.81",
  "port": 443,
  "timestamp_utc": "2026-10-03T13:17:30.295Z",
  "session_id": "20260101T000000Z-00000000",
  "collector_id": "c-00000000000000000000000000000000",
  "collector": { "country": "CN", "province": "Zhejiang", "city": "Hangzhou",
                 "isp": "China Mobile", "asn": "AS9808", "ip_version": "ipv4" },
  "target_meta": {
    "country": "US", "cca2": "US", "region": "Illinois", "city": "Chicago",
    "country_en": "United States",
    "latitude": 41.85003, "longitude": -87.65005,
    "colo": { "iata": "ORD", "cca2": "US", "city": "Chicago",
              "latitude": 41.9786, "longitude": -87.9048 }
  },
  "measurement": { "success": true, "latency_ms": 279.042 }
}
```

几个刻意的决定：

- **`schema_version` 写在每一行上**，而不是只在文件头：文件会被拆分、
  抽样、打乱，行级版本号是唯一可靠的自描述方式。
- **`client_version` 必须保留**：测量逻辑会演进，分析时要能把样本
  按版本区分，否则不同口径的数据会被混在一起比较。
- **失败样本仍保留 `latency_ms`**，但语义是"失败发生前等了多久"。
  用 `error_type` 区分：`timeout` / `connection_refused` /
  `network_unreachable` 是不同的线路现象，合并成一个"失败"会让数据失去价值。
- **`latitude` / `longitude` 缺失时字段不出现**（不是 0）。
  0,0 是几内亚湾的合法坐标，用它表示"未知"会把一批目标误判到同一个点上。
- **跟踪行保留全部 RTT 样本**（`rtt_ms` 是数组），
  而不是只留最小值：抖动是线路质量的重要维度。
- **`hop_count` 与 `responded_hops` 分开**：前者是路径总跳数，
  后者是有回复的跳数。前者大而后者小，说明路径上有大量不回 ICMP
  的路由器，而不是路径很长。
- **`local_filtered` 显式标记**：分析者需要知道 `private-v4`
  是我们替换的，而不是真的有个主机叫这个名字。

#### 不静默覆盖已有文件

```text
$ cf-route-tester export --out all.jsonl.gz
Error: output file "all.jsonl.gz" already exists; remove it or choose
another path with --out
```

导出物是用户可能已经上传过的东西，悄悄覆盖会让人分不清"哪一份被上传了"。

#### `--out -` 时汇总走 stderr

数据必须保持干净，否则管道里的 `jq` 之类会解析失败：

```bash
cf-route-tester export --format jsonl --out - | jq -c 'select(.kind=="trace")' | head
```

### aggregate：把公开数据聚合为统计结果

```bash
cf-route-tester aggregate --input batch.jsonl.gz           # 单个批次
cf-route-tester aggregate --input data/batches             # 整个目录（递归）
cf-route-tester aggregate --input a.jsonl --input b.jsonl.gz
cf-route-tester aggregate --input data/batches --format json --out report.json
cf-route-tester aggregate --input data/batches --min-samples 5 --top-groups 100
```

它**只读 JSONL**（本地导出物或从别处下载的批次），**不读数据库**。
这个边界是刻意的：聚合结果必须能从公开数据复现，如果它依赖本地库，
第三方就无法独立验证我们的数字。

#### 分组键：目标 × 地区 × 运营商

```text
(IP:Port) × (国家/省/市) × (运营商/ASN)
```

必须这样分组，因为「1.1.1.1:443 的延迟是多少」**没有唯一答案**——
只有「从某地某运营商看是多少」。同一个 IP 从Sample Province移动和从德国电信看过去
是完全不同的线路。

真实运行结果（两个节点的批次，各 10 个目标）：

```text
input:
  files:        2
  rows:         20 (20 measurements, 0 traces)
  collectors:   2

totals:
  targets:      10
  groups:       4 shown of 20
  probes:       20 total, 19 ok (95.0%)

by region / ISP:
  REGION                                 TARGETS     PROBES      OK%   P50 ms
  CN/Zhejiang/Hangzhou/China Mobile/AS9808      10         10    90.0%    321.3
  US/California/Los Angeles/Vultr/AS204…      10         10   100.0%    276.6

top targets by cross-region coverage:
  TARGET                         REGIONS   PROBES      OK%   SPREAD ms
  121.127.34.119:443                   2        2   100.0%        55.4
  216.128.154.87:8443                  2        2   100.0%      1065.0

groups (target x region x ISP):
  TARGET                     REGION/ISP              PROBES      OK%   P50 ms
  121.127.34.119:443         CN/Zhejiang/Hangzhou/Ch…       1   100.0%    305.6
  121.127.34.119:443         US/California/Los Ang…       1   100.0%    250.2
```

#### 它回答的三个问题

| 输出 | 回答的问题 |
| --- | --- |
| `regions[]` | 哪类网络（地区/运营商）整体表现更好 |
| `latency_spread_ms` | **同一个目标跨地区的表现差异有多大** |
| `distinct_as_paths` | **不同运营商是否走了不同的 AS 路径** |

`latency_spread_ms` 是各分组延迟中位数的极差。上面例子里的
`216.128.154.87:8443` 差异高达 **1065 ms**，说明这条线路对位置
极其敏感——这类目标才是众测数据真正有价值的部分。

`distinct_as_paths` 由路径的 AS 序列签名（`AS9808-AS58453-AS13335`）
去重得到。>1 就证实了"同一个目标在不同运营商下走不同出口"。

#### 不做主观评分

输出里只有计数、成功率与分位数，没有任何"评分""等级""推荐"。
需求明确要求客观指标：一个 290 ms 的 p50 是好是坏取决于用途
（网页浏览可以，实时游戏不行），程序不该替用户下这个结论。

#### 延迟分位数是近似值，且被明确标记

延迟用**固定对数直方图**统计（每倍频程 20 个桶，相对误差约 3.5%），
而不是把所有样本存进内存：

- 聚合的输入是多节点长期的公开数据，单个目标可能积累几十万个样本；
- 把每个样本都留在内存里会让聚合无法在普通机器上跑完；
- 直方图可合并，因此"每个分组各算一份再合并出全局"是可行的。

代价是分位数近似，因此每个结果都带 `percentiles_approx: true`，
绝不把近似值当精确值报出去。**极值与平均值是精确的**
（极值单独维护，平均值用累加和）。

#### `--min-samples` 作用在**目标**层面，不是分组层面

这个区别很关键。跨地区对比的常态是"每个地区只有一两个样本"
（一个节点一次扫描对同一目标只测一次）。若按分组过滤，
`--min-samples 2` 会把所有分组都藏掉，跨地区对比直接失效。

因此语义是「这个目标被足够多次地测过没有」。明细分组
（`groups[]`）不受该选项影响——它是原始观察记录，不是结论。

（这一条是实测发现的：第一版按分组过滤，用两个真实节点跑
`--min-samples 2` 时 `groups: 0 shown of 20`，跨节点对比完全看不到。）

#### 坏数据不会被静默忽略

| 情况 | 行为 |
| --- | --- |
| 行无法解析 | 跳过并计数，出现在 `input.bad_lines` 与 notes 里 |
| `kind` 无法识别 | 计入 `input.unknown_kinds`，不参与分组 |
| 单个文件打不开/损坏 | **跳过该文件并继续**，warning 里报出路径与原因 |
| 分组数超过上限 | 丢弃并计数，notes 里说明（不静默丢） |
| 目标样本不足 `--min-samples` | 隐藏并计数，notes 里说明 |

单个坏文件不终止整批：众测数据里混着坏文件是常态，
一个坏文件不该让整批数据作废。

#### notes：不让人误读数字

结果里带一组提醒，专门用于防止误读：

```text
notes:
  - only one collector contributed data; cross-region comparisons are not meaningful yet
  - 3 of 25 group(s) are shown; use --min-samples 1 and --top-groups 0 to see them all
  - all data was collected within one hour; this is a snapshot, not a trend
```

当只有一个节点贡献数据时会直接说明「跨地区对比还没有意义」——
否则一个 100% 的成功率看起来像是普遍结论，实际只是单点观察。

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
│   ├── probe/                    TCP Probe（已实现）
│   │   ├── probe.go              Prober / ProbeResult / 统计 / 错误分类
│   │   ├── errno.go              错误码注册表（平台无关部分）
│   │   ├── errno_windows.go      Windows WSA 错误码表
│   │   ├── errno_unix.go         Unix POSIX errno 表
│   │   └── worker.go             probe.Run（委托给 internal/worker）
│   ├── worker/                   通用有界 worker pool（probe 与 trace 共用）
│   │   └── worker.go             Run / Producer / ProcessFunc / RunStats
│   ├── trace/                    NextTrace 集成（已实现）
│   │   ├── engine.go             TraceEngine 接口、Hop/TraceResult、失败分类
│   │   ├── ntrace.go             外部进程调用：路径解析、参数构造、超时、版本
│   │   ├── parser.go             NextTrace JSON -> TraceResult（含纳秒换算）
│   │   └── testdata/             真实 v1.7.3 输出夹具（解析契约的依据）
│   ├── detect/                   本机地区 / 运营商检测（已实现）
│   │   ├── detect.go             Source 接口、合并规则（手动优先）、报告
│   │   ├── source_local.go       离线源：只推断出口 IP 版本
│   │   └── source_geoip.go       联网源：可配置的 geo-IP API + 宽松解析
│   ├── identity/                 本地匿名标识 collector_id（已实现）
│   ├── scheduler/                扫描编排、断点续测与两级测量（已实现）
│   │   └── scheduler.go          会话 -> 待测目标 -> Probe -> 落库
│   │                             -> 只对成功目标 Trace -> 落库 -> 收尾
│   ├── storage/                  本地 SQLite（已实现）
│   │   ├── sqlite.go             打开 / PRAGMA / 迁移 / 统计
│   │   ├── migrations.go         版本化迁移（表结构的唯一来源）
│   │   ├── model.go              Measurement / Trace / 目标与采集者 UPSERT
│   │   ├── writer.go             只追加写入（整批一个事务 + 幂等去重）
│   │   └── query.go              按目标 / 采集者 / 会话 / 时间窗口查询
│   │                             + 导出专用流式读取（export_query.go）
│   ├── privacy/                  隐私过滤（已实现）
│   │   └── privacy.go            内网/保留地址判定、坐标与目标分类
│   ├── export/                   公开 JSONL 导出（已实现）
│   │   ├── schema.go             公开 Schema（Row / Measurement / Trace）
│   │   ├── convert.go            storage -> Row 转换 + 隐私过滤 + 错误信息清洗
│   │   └── output.go             JSONL / gzip 编码（确定性 gzip 头）
│   ├── aggregate/                数据聚合（已实现）
│   │   ├── aggregate.go          输入读取、延迟直方图、分组键、错误计数
│   │   ├── collector.go          累积状态（分组 / 目标 / 采集者维度）
│   │   ├── report.go             报告生成、MinSamples 过滤、notes
│   │   └── source.go             文件/目录发现、gzip 读取、坏文件容错
│   └── upload/                   GitHub 上传（暂缓）
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
