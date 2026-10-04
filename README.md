# cf-route-tester

`cf-route-tester` 是一个用 Go 编写的**跨平台网络线路众测工具**。

它不是一个传统的 IP 扫描器。它的目标是：

> 让大量不同地区、不同运营商的用户，在**自己的真实网络环境**中，
> 对**同一批 IP:Port** 做完整线路测量，然后把匿名结果汇总，
> 逐步建立一个**长期维护的公网线路数据库**。

最终希望能回答这类问题：

```text
某个 IP:Port
  从示例省移动访问怎么样？
  从示例省电信访问怎么样？
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
Phase 5  ✅ 全量扫描（scheduler 包：两级测量编排、scan 命令）
Phase 6  ✅ 本机地区 / 运营商信息（detect 包：可解释的检测源、手动优先）
Phase 7  ✅ NextTrace 集成（trace 包：外部进程、真实 JSON 解析、trace 命令）
Phase 8  ✅ TCP Probe + NextTrace 两级测量（只跟踪探测成功的目标）
Phase 9  ✅ 结果导出（export 包 + privacy 包：JSONL / gzip、公开 Schema、隐私过滤）
Phase 10 ⏸️ GitHub 上传（按用户要求暂缓：先把全部流程在本地跑通）
Phase 11 ✅ 数据聚合（aggregate 包：按目标 × 地区 × 运营商分组、跨节点对比）
Phase 12 ✅ 查询与统计（query 包 + query 命令：单目标跨地区画像）
Phase 13 ✅ 跨平台打包与发布（tools/release：6 平台交叉编译 + 校验和 + 清单）
Phase 14 ✅ 图形界面（webui + service + applog：内置本地网页、令牌与 CSRF 防护、脱离命令行的 GUI 入口）
```

已实现的功能（可运行）：

```text
cf-route-tester --help
cf-route-tester version
cf-route-tester web            # 启动图形界面（本地网页，数据不出本机）
cf-route-tester fetch          # 下载 / 缓存 / 解析 all.json，输出目标数量
cf-route-tester detect         # 检测测量者所在地区与运营商，写入本地标识
cf-route-tester probe          # 对全部 IP:Port 做 TCP 连通性与延迟测量
cf-route-tester scan           # 全量扫描：结果**实时写入 CSV**（默认按日期时间命名，见下）
cf-route-tester scan --append  # 追加到已有结果文件（默认每次覆盖）
cf-route-tester scan --trace   # 扫描后对**探测成功**的目标做线路跟踪
cf-route-tester trace --target 1.1.1.1:443   # 单独跟踪一个目标
cf-route-tester export --list-sessions       # 查看可导出的会话（读取历史数据库）
cf-route-tester export --session <id>        # 导出为公开 JSONL（自动隐私过滤）
cf-route-tester aggregate --input batch.jsonl.gz   # 把导出物聚合为统计结果
cf-route-tester query 1.1.1.1:443 --db data/results.db   # 查单目标线路画像（读历史库）
cf-route-tester db stats       # 查看历史数据库状态
cf-route-tester db migrate     # 应用数据库迁移
cf-route-tester db vacuum      # 整理数据库文件
```

尚未实现（执行时明确报 `not implemented yet`，退出码 2）：`upload`。

> `upload` 按用户要求暂缓：先确保 `fetch → detect → scan → trace → export → aggregate → query`
> 整条链路在本地完全跑通（已完成，见下文各命令的真实运行输出），
> 凭据与归属方案确定后再做。

---

## 安装与构建

**推荐直接用预编译二进制**：从发布页下载对应平台的文件即可，
不需要 Go，也不需要任何运行时（Python / Node / Java 全都不需要）。

完整的安装说明（校验下载、macOS Gatekeeper、NextTrace 安装、
权限要求、最短上手路径）见 **[docs/INSTALL.md](docs/INSTALL.md)**。

### 从源码构建

需要 **Go 1.26 或更高版本**。这个下限不是随便定的：
纯 Go 的 SQLite 驱动 `modernc.org/sqlite` 自身声明了 `go 1.26`，
低于它无法构建。

```bash
# 把 <你的用户名> 换成实际地址（本仓库的 remote 尚未设置）
git clone https://github.com/<你的用户名>/cf-route-tester.git
cd cf-route-tester

go build ./...
go build -o bin/cf-route-tester ./cmd/cf-route-tester

# 确认可用
./bin/cf-route-tester version --verbose
./bin/cf-route-tester --help
```

克隆下来只有约 1.3 MB（源码 + 测试 + 文档），构建**不需要**任何数据文件
或第三方二进制；`data/all.json` 与 NextTrace 都是按需获取的。

Windows：

```cmd
go build -o bin\cf-route-tester.exe .\cmd\cf-route-tester
bin\cf-route-tester.exe version
```

**发版约定：每完成一次改动就打一个 tag。** 打 tag 即构建并发布，
因此"改完了"与"发出去了"之间没有人工步骤——攒着不发会让使用者手上的
版本悄悄落后于代码，而 issue 里描述的往往是已经修好的行为。

```bash
# 1) 同步源码里的版本号与文档里的产物清单
#    （tools/release 会打印真实体积，README / docs/INSTALL.md 按它更新）
# 2) 提交
git tag -a v0.1.13 -m "v0.1.13" && git push origin v0.1.13
```

版本号必须与 tag 一致：工作流按 tag 注入版本，而源码里的常量用于
`--version`。两者不同会让"二进制自报的版本"与"代码里写的版本"对不上，
排查问题时非常误导（这一点踩过）。

#### 生成完整发布包

`tools/release` 是一个 Go 程序（任何平台都能跑，只需要 Go 工具链），
它会先跑 gofmt / vet / test，再交叉编译发布目标，最后生成校验和与发布清单：

```bash
go run ./tools/release              # 版本号从源码读取
go run ./tools/release -version 0.2.0
```

```text
cf-route-tester-0.1.13-windows-amd64.exe    12.64 MiB  # 命令行
cf-route-tester-gui-0.1.13-windows-amd64.exe12.65 MiB  # 图形界面（无控制台窗口）
cf-route-tester-0.1.13-windows-arm64.exe    11.77 MiB
cf-route-tester-gui-0.1.13-windows-arm64.exe11.78 MiB
cf-route-tester-0.1.13-linux-amd64          12.41 MiB
cf-route-tester-0.1.13-linux-arm64          11.75 MiB
cf-route-tester-0.1.13-darwin-amd64         12.43 MiB
cf-route-tester-0.1.13-darwin-arm64         11.84 MiB
SHA256SUMS      # 8 个产物，与 sha256sum -c 兼容
release.json    # 版本 / commit / 每个产物的哈希与大小
```

**下载哪个文件？** Windows 上有两个名字很像的可执行文件：

| 文件 | 用法 |
| --- | --- |
| `cf-route-tester-gui-<版本>-windows-amd64.exe` | **图形界面：双击这个** |
| `cf-route-tester-<版本>-windows-amd64.exe` | 命令行：必须带子命令运行 |

双击**命令行版**只会看到黑框一闪——它需要参数，没有参数就只能打印用法后
退出，而窗口随进程结束立刻关闭。为了不至于让人一脸茫然，命令行版在
"专为自己新建的控制台里运行且没有参数"（也就是双击）时会**停下来**，
给出该下哪个文件的提示并等按键；在已有终端里运行则不会停，以免打断脚本。

Linux / macOS 只有命令行版：这些平台的启动方式本来就不会为程序新开一个
退出即消失的终端窗口，多一个产物只会让人犹豫该下哪个。

图形界面入口**只有 Windows 需要**：其它平台的启动器（`.app`/`.desktop`）
本来就不会显示终端窗口，多一份产物只会让用户困惑该下哪个。

为什么构建工具用 Go 而不是 shell 脚本：一个 `.ps1` Linux 用户跑不了，
一个 `.sh` Windows 用户跑不了，两套脚本必然漂移（命名规则、校验和格式、
清单字段）。用 Go 写就只有一份实现，而且命名规则与校验和格式
能被单元测试覆盖——那两样属于发布产物的一部分，写错了用户就没法校验。

发布构建会注入版本信息（`--version --verbose` 可确认）：

```bash
go build -trimpath -ldflags "\
  -s -w \
  -X github.com/cf-route-tester/cf-route-tester/internal/version.Commit=$(git rev-parse --short HEAD) \
  -X github.com/cf-route-tester/cf-route-tester/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o bin/cf-route-tester ./cmd/cf-route-tester
```

`-trimpath` 去掉构建机器的绝对路径（既是隐私问题，
也让同一份源码在不同机器上构建出的二进制一致）。

最终产物是**单个可执行文件**，`CGO_ENABLED=0` 下即可构建，
不需要 Python / Node.js / Java。

---

## 图形界面（web）

不想用命令行的话，用图形界面：

```bash
cf-route-tester web                  # 启动并自动打开浏览器
cf-route-tester web --no-browser     # 只启动服务（无桌面环境）
cf-route-tester web --verbose        # 同时在终端显示日志
cf-route-tester web --listen 127.0.0.1:8123   # 固定端口
```

Windows 上直接双击 `cf-route-tester-gui-*.exe` 也可以——那是用
`-H=windowsgui` 构建的入口，**不会弹出控制台黑窗口**，工作目录自动
切到 exe 所在目录，因此数据库与日志都落在 exe 旁边（`data/`）。

双击后会看到一个常驻小窗口：

```text
┌─ cf-route-tester ──────────────────────────┐
│  服务已启动，界面在浏览器中打开。            │
│  关闭这个窗口即退出程序。                    │
│  http://127.0.0.1:8236                     │
│                                            │
│  [ 打开界面 ]   [ 退出 ]                    │
└────────────────────────────────────────────┘
```

**为什么必须有这个窗口**：没有控制台的程序在后台跑起来之后，用户
看不到任何东西——不知道是否启动成功、不知道界面地址，也没有正常
途径关掉它，只能去任务管理器杀进程。留一个小窗口比这好得多：
它显示状态与地址，**关掉窗口即退出程序**（点「退出」或右上角 × 都一样）。

窗口里刻意**不显示访问令牌**：它容易被截图或录屏，令牌不该出现在
画面里。需要完整地址时看日志文件。

跟踪时**不会闪出黑框**：NextTrace 是控制台程序，在无控制台的宿主里
启动它会让每个目标都弹出一个控制台窗口。启动子进程时加了
`CREATE_NO_WINDOW`（见 `internal/trace/exec_windows.go`），
实测跟踪 2 个目标（21 秒）期间新增控制台窗口为 0。

启动后终端会打印一个带令牌的地址：

```text
time="..." level=INFO msg="web: starting" version=0.1.13 platform=windows/amd64 results=data/results.db
time="..." level=INFO msg="webui: listening" url=http://127.0.0.1:8236

请在浏览器中打开（地址里带有本次运行的访问令牌）：
  http://127.0.0.1:8236/?token=65fa66747f4219fec23d688daecccfd2c41d2df527a1a8e9fca73101fdb81890

令牌只对本次运行有效，重启后会换一个；它不会写入磁盘。
```

界面能看结果文件概览、启动/停止扫描（实时进度）、查看最近一次扫描的
结果。**所有数据都只写本机**；上传功能尚未实现，因此没有任何数据会
离开这台机器。

### 为什么是"内置网页"而不是原生窗口

原生界面框架要么需要 CGO（会破坏 `CGO_ENABLED=0` 的交叉编译与
"Windows 双击即用"），要么需要随包分发平台运行库；Wails 之类还会
把 npm 构建链带进仓库，新贡献者就得先装 Node 才能构建。内置 Web 服务
加 `go:embed` 的页面没有这些代价：仍然是单文件二进制，
`clone + go build` 依然足够。前端是原生 HTML/CSS/JS，不用框架。

### 安全（单机也一样要做）

**绑定 127.0.0.1 不等于只有本机能访问**：浏览器里任何网页都能向
`http://127.0.0.1:<port>` 发请求，而本工具的能力是"对外发起大量网络
连接"。因此三件事默认开启、不提供关闭开关：

1. 每次启动生成**进程级随机令牌**，所有 `/api` 请求都要带（用常数时间比较）；
2. 校验 `Host` 必须是回环地址——防 DNS rebinding（攻击者让
   `evil.com` 解析到 127.0.0.1，浏览器发出的请求看起来是访问本机）；
3. 变更类请求校验 `Origin`——防 CSRF。同源策略只阻止**读取**响应，
   不阻止**发起**请求，而启动一次扫描的副作用已经发生了。

`--listen 0.0.0.0:8123` 能把服务暴露到局域网，那等于把这台机器发起
网络扫描的能力借给别人，请确认你确实需要。

### 关于日志

图形界面脱离命令行运行时**没有终端**，所以：

- 日志默认写入 `data/logs/cf-route-tester.log`（可用 `--log-dir` /
  `--log-file` 调整），并按 5 MiB 轮转、保留 3 个历史文件；
- **带令牌的地址也会写进日志**。无控制台的进程里 stdout 是无效句柄，
  写进去的内容会丢失，而这个地址是进入界面的唯一凭据；
- 启动时终端会打印日志文件的完整路径。

日志与命令行输出是**两条线**，刻意不合并：日志给排查用（时间戳、级别、
结构化字段），`scan` 之类的进度与汇总给使用者看。
把每一行进度都塞进日志只会让真正重要的信息被淹没。

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
version: 0.1.13
```

`version --verbose` 输出（含构建细节，便于排查“结果来自哪个版本”）：

```text
client:         cf-route-tester
version:        0.1.13
schema_version: 1
commit:         unknown
build_date:     unknown
go_version:     go1.26.5
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
cf-route-tester fetch                 # 下载并写缓存（默认每次都会去下载最新的一份）
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
- **默认网络优先，缓存只作兜底**：每次运行都先试着下载最新的目标列表，
  **下载失败才退回缓存**（不看新鲜度）。想反过来——"本地有新鲜缓存就不联网"——
  用 `--source-cache-first`（`fetch` 上是同一个旗标）。

  为什么默认这样：目标列表是这份工具的全部输入，上游随时会增删目标。
  以前"缓存 6 小时内就直接用"意味着这 6 小时里新增的目标一个都测不到，
  而且**没有任何提示**——使用者以为自己测的是全部，实际测的是一份旧快照。
  缓存兜底仍然默认开启：断网、被墙、上游临时挂掉时手上那份仍然有用。
- **代理**：`--proxy` 显式指定时优先；未指定则沿用环境变量 `HTTP_PROXY` / `HTTPS_PROXY`。

### detect：测量者所在地区与运营商

```bash
cf-route-tester detect                       # 检测并显示（不写入）
cf-route-tester detect --write               # 检测并写入 data/collector.json
cf-route-tester detect --source local        # 只用离线源，不联系任何外部服务
cf-route-tester detect --isp "China Mobile" --province Sample Province --write
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
                 isp=China Mobile asn=AS56046 ip_version=ipv4  (1.699s)

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
  isp:        China Mobile
  asn:        AS56046
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

真实示例：检测给出 `province=Shanghai, isp=China Mobile`，
用户在自己机器上改成 `--province Sample Province --city Sample City --isp "China Mobile"`：

```text
$ detect --isp "China Mobile" --province Sample Province --city Sample City --write
$ cat data/collector.json
{
  "collector_id": "c-0b720f1207df74114840f2be949d229b",
  "profile": {
    "country": "CN",
    "province": "Sample Province",          <- 手动值保持
    "city": "Sample City",              <- 手动值保持
    "isp": "China Mobile",  <- 手动值保持
    "asn": "AS56046",               <- 未被手填，仍是检测结果
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
{"schema_version":1,"client_version":"0.1.13","target_id":"45.63.67.144:443","ip":"45.63.67.144","port":443,"success":true,"latency_ms":252.3158,"timestamp":"2026-10-03T11:35:21.8611931Z"}
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

### scan：全量扫描，结果实时写入 CSV

```bash
cf-route-tester scan                       # 全部目标测一遍，按日期时间命名结果文件
cf-route-tester scan --limit 300           # 只扫前 300 个（先验证链路是否通）
cf-route-tester scan --out my.csv          # 换一个结果文件
cf-route-tester scan --append              # 追加到指定文件（需配合 --out）

# 先按国家测 TCP，再按延迟挑一批跟踪——不必一次跟几千个。
cf-route-tester scan  --target-country DE --out de.csv   # 只测德国
cf-route-tester trace --from de.csv --max-latency 200ms --limit 10
cf-route-tester scan --trace               # 对**探测成功**的目标做线路跟踪
cf-route-tester scan --country cn --province Sample Province --city Sample City \
                    --isp "China Mobile" --asn 9808
```

#### 结果文件默认按日期时间命名

`--out` **留空**（默认）时，结果写到 `data/results-<日期时间>.csv`：

```text
data/results-20261005-031500.csv
```

**每跑一轮就是一个新文件，不会覆盖上一轮。** 这一点是刻意的：以前默认
写死 `data/results.csv`，而"覆盖"又是默认行为，于是想对比不同国家、不同
并发下的表现时就只能重测——两轮的条件也不可能完全一致。现在直接在文件
管理器里并排看即可，按文件名排序就是按时间排序。

同一秒内跑两轮也不会撞车（`--limit 1` 的扫描几百毫秒就结束，脚本里连着
跑很常见），这时第二个文件带 `-2` 后缀。

想写进同一个文件才需要显式给 `--out` 并加 `--append`。


输出示例（真实运行结果）：

```text
output:      data/docs.csv（覆盖）
limit:       4 个目标
workers:     100
timeout:     2s
trace:       disabled

output:      data/docs.csv
considered:  4 target(s)
probed:      4
succeeded:   3
failed:      1
rows:        4 written
elapsed:     3.07s
```

#### 结果只进 CSV：没有数据库、没有会话、没有续测

这个工具的实际用法是"跑一轮、看结果"。数据库带来的是会话、迁移、
幂等去重、恢复逻辑——一整套使用者并不需要的东西。CSV 可以直接看、
可以直接拖进表格，也**不会因为少了某个组件就读不出来**。

因此 `scan` 与图形界面的扫描：

- **不写数据库**。跑完只有一份 `data/results-<日期时间>.csv`（以及日志），不会有 `.db` 文件；
- **没有会话（session）**。每次 scan 就是一次独立的测量，没有会话 ID 这回事；
- **没有续测（resume）**。中断就是中断，工具不假装知道你想不想接着跑；
- **不做去重**。重跑就再写一遍，想保留多轮结果就加 `--append`。

#### 中断不丢已测结果（本阶段的重点）

每一行测完就**立即刷盘**，不等攒批、不等扫描结束。因此：

```text
Ctrl+C / 进程被杀 / 断电
  -> 已经测完的目标，一行都不会丢
  -> 只损失"正在测、还没测完"的那一个
```

实测（12 个目标，跑到第 2 个时取消）：

```text
$ scan --limit 12 ... &        # 中途 Ctrl+C
$ wc -l data/results-*.csv
3                              # 表头 + 2 行，已完成的两条都在
```

这里刻意用 `Flush` 而不是 `fsync`：Flush 把数据交给操作系统，
进程被杀也不会丢；而 fsync 要真的等磁盘落盘——每秒几十次会明显拖慢测量，
而"操作系统崩溃"不是本工具需要防的场景。

表头**只在文件为空时写一次**。追加模式下不重复写表头，
否则表头会出现在文件中间，那种文件用 pandas 读会直接报错。

#### CSV 的列

```text
timestamp_utc, target, ip, port, success, latency_ms,
error_type, error_message, hop_count, as_path, hops, client_version, cca2
```

`cca2` 是**目标**的国家两位码（ISO 3166-1 alpha-2，来自目标列表的
`location.cca2`）。它放在 CSV 里而不是事后去目标列表里查：CSV 是这个
项目唯一的数据源，界面按国家筛选/分组时不能依赖"目标列表此刻是否还在、
内容是否已经变了"。新列一律追加在末尾——列名与列序是契约。

上游数据里 `location.country` 与 `location.cca2` 有约 18% 的条目
**互相矛盾**（例如 `country=FI` 而 `cca2=SE`）。本项目**只认 `cca2`**：
界面上的国家数字、`--target-country` 的筛选、CSV 里的这一列，
三者用同一个字段，因此"下拉框说 3670 个"与"筛出来 3670 个"必然一致。

真实数据行：

```csv
2026-10-03T18:00:17Z,159.60.146.81:443,159.60.146.81,443,true,284.683,,,,,,0.1.13,US
2026-10-03T18:00:18Z,45.63.67.144:443,45.63.67.144,443,false,,timeout,dial tcp4 45.63.67.144:443: i/o timeout,,,,0.1.13,US
```

**失败时 `latency_ms` 是空单元格，不是 `0`。** 这一点很重要：
0 毫秒与"没测到"在表格里是两回事，混在一起会让平均延迟、分位数
全部失真。超时 1 秒的目标如果写成 `1000`，看起来就像"延迟 1 秒"，
而它其实根本没连上。

`hops` 把整条路径压进一个单元格，而不是每跳一行：

```text
1:10.0.0.1;2:203.0.113.4:163;3:*;4:198.51.100.7:CN2
```

`*` 表示该跳超时（留空会被读成"没有这一跳"）。一条路径 10~30 跳，
每跳一行会让同一个目标在 CSV 里重复几十次，表格就没法用了。

`error_message` 写进文件前会去掉**本机文件路径**（例如找不到引擎时
报出的可执行文件路径），因为这份文件是要拿去看、可能要分享的。
但**不会**去掉目标地址——`dial tcp 1.2.3.4:443: timeout` 里的地址
正是"哪个目标失败了"。

#### `--trace` 在引擎不可用时会明确说明"没做"

（以下为真实运行输出，`--trace-binary no-such-engine` 模拟未安装 NextTrace）

```text
output:      data/doc-b.csv（覆盖）
trace:       enabled (mode tcp) — 只跟踪 TCP 探测成功的目标

output:      data/doc-b.csv
considered:  2 target(s)
probed:      2
succeeded:   1
failed:      1
rows:        2 written
elapsed:     2.353s

trace:       SKIPPED
             file does not exist: "no-such-engine" not found in PATH
             NextTrace not found.
             Please install NextTrace or configure trace.nexttrace.binary.
```

两个刻意的决定：

1. **引擎缺失不终止扫描**。TCP 测量本身仍有价值，
   "用户没装 NextTrace"是最常见的情况之一，而且那 2 行 TCP 结果
   已经写进 CSV 了。
2. **绝不静默跳过**。明确标为 `SKIPPED`，并给出原因，
   否则汇总看起来像是跟踪过了——那会让用户基于错误的前提去分析数据。

#### 两级测量：只跟踪探测成功的目标

`scan --trace` 在 TCP 测量**之后再**执行线路跟踪，且**只对探测成功的目标**跟踪：

```bash
cf-route-tester scan --trace
cf-route-tester scan --trace --trace-binary "data/bin/nexttrace.exe" --trace-mode icmp
cf-route-tester scan --trace --trace-workers 4 --trace-timeout 25s
```

为什么必须这样分级：连 TCP 都连不上的目标，跑 traceroute 大概率在
中途就断了。为它们花几十秒既得不到有效路径，又拖慢整次扫描。
实测一次 traceroute 平均 17 秒，而 TCP 探测是毫秒级。

`workers` 与 `trace-workers` 是**两套独立的并发**：探测是纯 socket
（默认 100），跟踪每个 worker 都要启动一个外部进程（默认 10）。
混在一个数字里会让用户把并发调到 100 去跑跟踪，那会拖垮机器。

两级也会**分别汇报**：

```text
probed:      12
succeeded:   10      <- Level 1: TCP，整体可用率 83.3%
failed:      2

traced:      10 (ok 10)   <- Level 2: 只对那 10 个成功的
```

"跟踪成功率 100%"指的是"能连上的目标里，全部成功拿到了路径"，
与"整体可用率 83.3%"是两个不同的量——混在一起会让人以为线路质量
比实际更好。

跟踪结果与探测结果写在**同一个 CSV** 里，各自占一行：
探测行没有 `hop_count` / `as_path` / `hops`，跟踪行则额外带上线路信息。
这样既能看"通不通、多快"，也能看"走的是哪条线路"。

#### 日志：每个 IP 的延迟与连通性，每条线路的 ASN 与线路名

图形界面的日志面板实时输出**每个目标的一行结果**（真实输出）：

```text
连通 64.177.113.100:443  238.7 ms  落地 US/Illinois/Elk Grove Village
连通 121.127.34.119:443  254.5 ms  落地 US/Illinois/Chicago
连通 45.63.67.144:443  258.5 ms  落地 US/Illinois/Elk Grove Village
TCP 探测完成：5 个目标，成功 5，失败 0
测量完成：用时 0.3s，成功 5，失败 0，写入 5 行
结果文件：data/results-20261005-031500.csv
```

失败时给出**分类**（`timeout` / `refused` / …）而不是整条错误信息：

```text
不通 217.177.35.182:8443  (timeout)  落地 US/Texas/Dallas
```

为什么用分类而不是完整信息：完整信息形如
`dial tcp4 217.177.35.182:8443: i/o timeout`，每一行都重复一遍目标
地址，逐行看很吵。分类足以说明问题，**完整原因留在 CSV 的
`error_message` 列**里，需要时查得到。

跟踪出了**优质线路**（CMIN2 / CN2 / 9929/CUII）的那一行会被特别标出：

```text
线路 45.63.67.144:443：20 跳，CMNET(AS56046) > CN2(AS4809) > Vultr(AS20473)  落地 US/...
```

判定发生在服务端而不是界面：线路本来就是在那一层解析出来的（手里就是
ASN 列表），让界面拿日志文本再猜一次既重复，又会随格式调整而悄悄失效——
而失效后的表现只是"某些行不再被标出"，没有人会去核对。界面照着一个
`highlight` 字段上色即可。

标记与**级别**是两件事，因此叠加而不是替换：级别（info / good / warn /
error）说的是"这条消息有多严重"，只有四档；"走了好线路"不是一种严重
程度。把两者混在一起会让红色同时意味着"出错了"和"线路很好"。
失败的跟踪**不会**被标出——那会让人以为"这条路走了好线路"，而实际上
这次跟踪根本没成。

跟踪阶段输出**ASN 编号 + 线路名称 + 落地地区**（真实输出）：

```text
线路 159.60.146.81:443：15 跳，CMNET(AS56046) > CMNET(AS9808) > CMI(AS58453) > AS35280  落地 US/Illinois/Chicago
线路 216.128.154.87:8443：25 跳，CMNET(AS56046) > CMNET(AS9808) > CMI(AS58453) > Cogent(AS174) > Vultr(AS20473)  落地 US/Illinois/Elk Grove Village
```

**为什么要同时给编号和名称**：只有 `CMNET` 无法核对到底是不是
AS9808，只有 `AS9808` 又认不出这条线路意味着什么（普通出口还是优质
出口）。两者都给出才能既核对又理解。

**为什么要带落地地区**：同一个 IP 段在不同地区表现差别很大，
没有地区就无法判断"延迟高"是因为目标远，还是线路本身差。
地区来自 `all.json` 里**目标 IP 自身**的地理数据，不是本机位置。

命令行用 `--verbose` 得到同样的逐条输出（默认关闭：11600 多个目标
逐条打印会把终端刷爆，而"测完了多少"由进度行表达）：

```bash
cf-route-tester scan --verbose
cf-route-tester scan --trace --verbose
```

CSV 的 `as_path` 列写**线路名称**（如 `CMNET > CMI > Cogent`），
日志里写**编号 + 名称**。两者分工：列是要拿去排序聚合的，
越干净越好；日志是给人看的，越能核对越好。

#### 失败分类

失败分类（数据库与分析的价值就在于"分得清是哪一种失败"）延续
`probe` 的定义，写进 `error_type` 列：

| 分类 | 含义 |
| --- | --- |
| `timeout` | 连接超时，目标可能在黑洞路由后面或被防火墙丢弃 |
| `refused` | 端口关闭（RST），主机活着但服务没监听 |
| `unreachable` | 网络/主机不可达（ICMP 反馈） |
| `dns` | 域名解析失败 |
| `canceled` | 本地主动放弃（Ctrl+C） |
| `permission_denied` | 权限不足（跟踪模式下常见，需要管理员 + WinDivert） |

同一分类在不同平台由不同错误码映射而来，具体见表
`internal/probe/errno_windows.go` / `errno_unix.go`。

### trace：使用 NextTrace 做线路跟踪

```bash
cf-route-tester trace --target 1.1.1.1:443                  # 默认 TCP，测 443
cf-route-tester trace --target 1.1.1.1:2053                 # 端口原样传给引擎
cf-route-tester trace --target 1.1.1.1 --mode icmp          # 无需管理员权限
cf-route-tester trace --binary "C:/Tools/nexttrace.exe" --target 1.1.1.1:443
cf-route-tester trace --limit 20 --workers 10 --json        # 从目标列表批量跟踪
cf-route-tester trace --target 1.1.1.1:443 --verbose        # 打印完整跳表
cf-route-tester trace --target 1.1.1.1:443 --data-provider IPInfo   # 换 GeoIP 数据源
cf-route-tester trace --target 1.1.1.1:443 --pow-provider sakura    # 换 PoW 令牌源
```

输出示例（真实运行结果，NextTrace v1.7.3，ICMP 模式）：

```text
1.1.1.1:443  30 hops (15072.2 ms)
   1  192.168.1.1                                 0.75 ms
   2  192.168.1.1                                 1.23 ms
   3  *
   4  203.0.113.4                               3.45 ms  AS56046  example.net
   5  203.0.113.5                              3.97 ms  AS56046  example.net
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

#### nexttrace 从哪儿来：查找顺序与自动下载

线路跟踪依赖一个外部程序。此前"没装"就等于"跟踪不可用"，而使用者
要自己搜平台、挑架构、下对一个 32 MB 的文件、再放到对的位置——
这一步劝退的人比任何技术问题都多。现在它会自己解决。

**查找顺序（越靠前优先级越高）**：

| 顺序 | 位置 | 说明 |
| --- | --- | --- |
| 1 | 显式路径（`--binary` / `--trace-binary`） | **最高优先级，且不会被自动下载覆盖** |
| 2 | 与本程序**同目录**的 `nexttrace[.exe]` | 绿色版把两个文件放一起即可 |
| 3 | 程序目录下的 `data/bin/` | 自动下载的落点 |
| 4 | 当前工作目录下的 `data/bin/` | 从终端在别处运行时 |
| 5 | `PATH` | |
| 6 | 都没有 → **自动下载**（默认开） | 落到程序目录的 `data/bin/` |

第 2 项优先于 `PATH` 是刻意的：使用者把 nexttrace 放在程序旁边时，
那份就是他想要的，而 `PATH` 里可能另有版本。

**自动下载**取固定版本 `v1.7.3`（与测试里锁定的 JSON 结构、
参数行为一致），落到程序目录的 `data/bin/`：

```text
trace: 正在下载 nexttrace v1.7.3（nexttrace_windows_amd64.exe）
      来源：https://github.com/nxtrace/NTrace-core/releases/download/v1.7.3/...
      已下载到 .../data/bin/nexttrace_windows_amd64.exe（32.1 MB）
```

下载后**会真的执行一次 `--version`**才认账。体积检查只能挡住
"上游返回了 HTML 错误页"这类情况；"下到了别的架构的二进制"只有
执行一次才知道。校验失败会把文件删掉——留一个永远跑不起来的残骸
比什么都不留更糟，因为下次启动会以为"已经装好了"。

关掉自动下载：

```bash
cf-route-tester scan --trace --trace-no-download
cf-route-tester trace --target 1.1.1.1:443 --no-download
cf-route-tester scan --trace --trace-download-dir /opt/nt   # 换落点
```

图形界面里是跟踪选项下的一个勾选框。

**Windows 的 TCP/UDP 模式：WinDivert 也会自动下载。**

这两个模式要靠 WinDivert 抓包，需要 `WinDivert.dll`（用户态）与
`WinDivert64.sys`（内核驱动）与 nexttrace 放在同一目录。上游 nexttrace
的发布物里**只有二进制本体**（实测 v1.7.3 的 99 个资源全是可执行文件，
没有任何压缩包），所以这两个文件从 WinDivert 自己的发布里取：

```text
TCP/UDP 模式需要 WinDivert，正在下载 WinDivert-2.2.2-A.zip
压缩包哈希校验通过
已安装 .../data/bin/WinDivert.dll（46.5 KB）
已安装 .../data/bin/WinDivert64.sys（91.9 KB）
```

**只在 `--trace-mode tcp` / `udp` 且文件缺失时才下载**。ICMP 模式用系统
自带能力，不需要它——给只用 ICMP 的人装一个内核驱动是不合适的。

下载后比对**钉住的 SHA256**：

```text
trace: WinDivert 压缩包哈希不符，已放弃
  期望 63cb4176...
  实际 ...
这可能意味着下载被中间人替换。
```

这是一个会被以管理员权限加载的**内核驱动**，而上游不提供校验和
（GitHub API 的 `digest` 字段为空），所以由我们记下核对过的哈希。
走代理时这一点尤其重要：HTTPS 只能证明"来自 github.com"，不能证明
"是未被改动的原件"。哈希不符是**硬失败**，不会降级成警告，也不会落盘。

WinDivert 是独立项目（[basil00/WinDivert](https://github.com/basil00/WinDivert)），
有自己的许可证：本项目**不重新分发**它，只在使用者机器上按需下载。

**TCP/UDP 模式仍然需要管理员权限**（内核驱动要加载），所以即使文件齐了，
普通权限下跟踪也会报 `permission_denied`。想省事就用 ICMP 模式。

**两个边界**：

- **显式路径不会被自动下载顶掉。** 使用者写了 `--binary /path/to/nt`，
  那即使那个文件不可用也只报"不可用"，不会换成别的东西——否则他会
  以为自己配的生效了。
- **自动下载默认只在生产路径打开**（CLI / 图形界面）。`EngineOptions`
  的零值是**不下载**，因此单元测试里构造一个不存在的引擎时绝不会
  偷偷联网下 32 MB、把测试变成依赖外网。

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
          "prov": "示例省", "prov_en": "Sample Province",
          "city": "示例市", "city_en": "Sample City",
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

#### 数据源可选（GeoIP / ASN / 地区从哪儿来）

`NextTrace-API`、`IP.SB`、`IPInfo`、`IPInsight`、`IP-API.com`、
`IPInfoLocal`、`ipdb.one`、`chunzhen`、`disable-geoip`
—— 清单与本地 nexttrace 的 `--help` 一致，有测试钉住。

```bash
cf-route-tester trace --target 1.1.1.1:443 --data-provider IPInfo
cf-route-tester scan --trace --trace-data-provider IPInfo
cf-route-tester scan --trace --trace-data-provider NextTrace-API --trace-pow-provider sakura
cf-route-tester scan --trace --trace-data-provider disable-geoip   # 不查地区，最快
```

图形界面里是跟踪选项下的两个下拉框（数据源、PoW 令牌源）。

**为什么默认是 `NextTrace-API`**：它的 ASN 覆盖与线路命名最完整，
也就是本项目输出 `CMNET(AS9808) > CMI(AS58453)` 这种信息的来源。

**但它也是唯一与 PoW 令牌绑定的源。** 令牌拿不到时整次跟踪会失败，
而不是降级成"没有 ASN 的路径"：

```text
线路 45.63.67.144:443 跟踪失败：pow token fetch failed: RetToken failed
after 3 attempts (host=api.nxtrace.org): too many requests
```

撞上这个时有两个办法：

| 办法 | 说明 |
| --- | --- |
| 换第三方源 | `--data-provider IPInfo` 等，完全绕开 PoW |
| 换 PoW 源 | `--pow-provider sakura` —— nexttrace 帮助里写明"For China mainland users, please use sakura" |

`--pow-provider` **只在数据源是 `NextTrace-API` 时才传给 nexttrace**：
用第三方源时这个参数没有意义，传了只会让命令行更难读。

**`disable-geoip` 的代价要说清楚**：不做地区查询，因此 `as_path` 与
落地地区必然为空。选它意味着只要路径、不要归属，不是"跟踪坏了"。

#### 本地 ASN 前缀识别：无限、不限流、不需要账号

所有免费的 GeoIP 服务都有限流：NextTrace-API 卡在 PoW 令牌、
IPinfo 卡在额度、ip-api 免费版 45 次/分钟。**因为每查一个 IP
都要发一次网络请求，而限流就是为这个设计的。**

本项目用的办法是**把它反过来**：

```text
不去查"这个 IP 属于哪个 ASN"，而是先拿到"关心那几条线路的 IP 段"，
再看路径里有没有跳落在这些段里。
```

要认的线路就是 asnmap 里那 30 条（163 / CN2 / 169 / CUII / CUG /
CMNET / CMI / CMIN2 + 国际骨干），而它们的 IP 段来自**一张全量表**：

```bash
curl -L "https://iptoasn.com/data/ip2asn-combined.tsv.gz" | gunzip | grep -P '\t(4809|58453)\t'
# 1.71.103.0	1.71.103.255	4809	CN	CHINATELECOM-CORE-WAN-CN2
# 103.11.109.0	103.11.109.255	58453	HK	CMI-INT-HK China Mobile
```

数据来自 **iptoasn.com**（公有领域，源自 BGP 表）：**不需要账号、
不需要 token**。整份约 8.6 MB，从里面只抽出那 30 条线路的段并缓存
（约 1.7 MB / 30 个文件，7 天过期），之后**完全离线**——不再有任何
网络查询。

**为什么是"整表下载"而不是"每个 ASN 查一次"。** 这一条是踩过坑之后
改的：最初用 RouteViews 的 `asn/<n>?af=4` 接口，每个 ASN 一个请求，
30 个 ASN 共 60 次。它**匿名可用但配额有限**，连续跑几轮之后开始返回：

```text
http 429: Please register for a token: https://api.routeviews.org/docs/#access
```

后果不是"报错"，而是**半成品**：60 个请求成功 18 个，于是 12 条线路
静默失去识别能力，而使用者只看到"线路名少了几条"。整表下载是
**全有或全无**，而且请求数从 60 降到 1。

（顺带说明：`iptoasn.com` 给的是地址**区间**，本项目内部与缓存都用
CIDR，因此有一次区间→CIDR 的转换。那段代码有 9 条精确断言 +
"转换结果恰好覆盖原区间"的语义断言——写错它不会报错，
只会让某段 IP 悄悄落在集合外，表现成"线路名偶尔少一条"。
第一版就是错的：`1.1.1.0/24` 膨胀成了覆盖大半个地址空间的级联，
被测试当场抓住。）

#### 下载失败时用内置快照兜底

整表下载是**单点故障**：离线、站点挂掉、被中间设备拦掉，都会让线路名
彻底失效。因此二进制里内置了一份快照（gzip 后约 300 KB，
30 个 ASN / 95446 条前缀），在下载失败**且本地没有缓存**时接手：

```text
asnprefix: 全量映射表不可用：after 3 attempts: ... connection refused
asnprefix: 退回内置快照：就绪 30/30 个 ASN（生成于 2026-10-03T20:37:25Z），线路段可能略旧
线路 159.60.146.81:443：15 跳，CMNET(AS56046) > CMNET(AS9808) > CMI(AS58453)
```

回退顺序是三级，**越靠前越新**：

| 顺序 | 来源 | 何时使用 |
| --- | --- | --- |
| 1 | 本地缓存（7 天内） | 正常情况，**完全不联网** |
| 2 | 下载全量表 | 缓存过期或缺失 |
| 3 | 本地缓存（**过期**） | 下载失败 |
| 4 | **内置快照** | 下载失败且没有任何缓存 |

快照是**兜底而不是数据源**：它不会盖过更新的缓存或刚下载的数据
（有测试分别钉住这两点）。它会随时间过期，因此使用时会明确
说出生成时间——否则容易把旧数据当成现状。

重新生成（骨干线路的 IP 段会变，建议随版本发布更新一次）：

```bash
cd internal/asnprefix
go generate ./...        # 读 data/asnprefix/*.json，写 snapshot/snapshot.json.gz
```

数据源地址也可以覆盖，便于用镜像或本地文件：

```bash
cf-route-tester scan --trace --trace-asn-bulk-url "https://mirror.example/ip2asn.tsv.gz"
```

**抓取与 TCP 探测并行。** 这一条直接影响体感：

| | 抓取方式 | TCP 完成 → 出线路 |
| --- | --- | --- |
| 最初 | 串行 60 次请求，且在跟踪前才启动 | **72 秒**（TCP 在 7.5s 就测完了） |
| 现在 | 8 并发，且探测一开始就预热 | 约 4 秒；上万个目标时**完全隐藏** |

下载整张表要几秒，而如果它在跟踪前才开始，那几秒就是纯粹的等待：
没开始——使用者看到的就是"TCP 秒完，然后卡住一分钟"。现在抓取与
探测并行：真跑一轮 11600 个目标时 TCP 阶段要几分钟，抓取只要几秒，
感知上的停顿是零。

``Warm`` 的"必须不阻塞"有测试守着：测试先卡住一个请求不放，
断言 ``Ready()`` 仍为假——只有下载真在后台跑才会这样。

于是可以这样跑，让引擎彻底不碰 GeoIP：

```bash
cf-route-tester scan --trace --trace-data-provider disable-geoip --out data/results.csv
```

真实输出（`--verbose`）：

```text
线路 159.60.146.81:443：15 跳，CMNET(AS56046) > CMI(AS58453)  落地 US/Illinois/Chicago
线路 216.128.154.87:8443：25 跳，CMNET(AS9808) > CMNET(AS56046) > CMI(AS58453) > Arelion(AS1299) > Vultr(AS20473)  落地 US/Illinois/Elk Grove Village
```

注意第二行认出了 `Arelion(AS1299)` —— 那是国际骨干，说明这条路径
**没有**全程走 CMI，而是从 CMI 出去转到了 Arelion。这正是
"看它有没有走某条线路"要回答的问题。

代价只有一个：**只认得出这 30 条线路**，别的 IP 归属不知道。
而本项目的线路名本来就只来自这 30 条，落地地区又来自 `all.json`，
所以这个代价实际上不是代价。

相关开关：

| 开关 | 作用 |
| --- | --- |
| （默认开启） | 首次抓取约 1.7 MB 前缀并缓存 7 天，之后离线 |
| `--trace-no-asn-prefix` | 关掉，退回用引擎给的逐跳 ASN |
| `--trace-asn-prefix-dir` | 换缓存目录（默认 `data/asnprefix`） |
| `--trace-refresh-asn-prefix` | 忽略缓存立即重抓 |

图形界面里是跟踪选项下的一个勾选框：

```text
[x] 用本地 ASN 前缀识别线路（无限、不限流、无需账号）
```

**前两级的分工**：前缀匹配优先，因为它的数据直接来自 BGP 表；
前缀没认出任何线路时才退回引擎给的 ASN（有些路径确实不经过
这 30 条线路，而引擎可能有更广的覆盖）。CSV 的 `as_path` 与日志
走的是同一个判断，因此**不会**出现"日志说走 CN2、CSV 说走 163"
这种自相矛盾。

#### 数据源名字写错会被挡住，而不是静默换源

这是必须做成受校验类型（而不是透传字符串）的原因：
**nexttrace 拿到不认识的数据源名时不报错，而是换一个源继续跑。**
那意味着"我以为在用 IPInfo，实际在用别的"——从结果上完全看不出来。

因此校验发生在**构造引擎时**，也发生在任何副作用之前：

```text
$ cf-route-tester scan --trace --trace-data-provider nope --out data/x.csv
Error: usage error: unknown data provider "nope" (choose one of
IP-API.com, IP.SB, IPInfo, IPInfoLocal, IPInsight, NextTrace-API,
chunzhen, disable-geoip, ipdb.one)
```

目标是**一个字节都不该测**：结果文件不会被创建，目标列表也不会去下载。
放到跟踪阶段才校验的话，后果是"先测完所有目标、写好 CSV，然后报跟踪被
跳过"——那看起来像引擎没装，排查方向完全错。

顺带一提：拼错的名字里带点的写法也会被规范化，`ipinfo`、`ipapi`、
`leomoeapi`、`local`、`none` 这类简写都能用（大小写不敏感）。

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

#### 输出格式：JSONL 与 CSV

```bash
cf-route-tester export --format csv.gz --out batch.csv.gz   # 给人看/进表格
cf-route-tester export --format jsonl.gz --out batch.jsonl.gz  # 给程序处理
```

**CSV 是扁平的一行一条**（29 列，带表头），可以直接拖进 Excel /
Numbers / pandas。JSONL 保留嵌套结构，适合程序与后续聚合。

CSV 里有两列专门回答"这条线路走的是什么"：

| 列 | 内容 |
| --- | --- |
| `trace_as_path` | 路径上的线路名，如 `163(AS4134) > CN2(AS4809) > Cloudflare(AS13335)` |
| `trace_hops` | 逐跳，如 `1:10.0.0.1;2:203.0.113.4:163;4:*` |

逐跳压进**一个单元格**（用 `;` 分隔）而不是每跳一行，是刻意的：
一条记录有 10~30 跳，每跳一行会让 CSV 膨胀成"同一目标重复几十次"，
反而没法做透视表。需要逐跳细看时用 JSONL。

> CSV 的列名是契约：一旦被人拿去做了透视表，改列名就等于破坏他们的表。
> 因此新增列一律**追加在末尾**，不重排、不改名。


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
  "client_version": "0.1.13",
  "target_id": "159.60.146.81:443",
  "ip": "159.60.146.81",
  "port": 443,
  "timestamp_utc": "2026-10-03T13:17:30.295Z",
  "session_id": "20260101T000000Z-00000000",
  "collector_id": "c-00000000000000000000000000000000",
  "collector": { "country": "CN", "province": "Sample Province", "city": "Sample City",
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
只有「从某地某运营商看是多少」。同一个 IP 从示例省移动和从德国电信看过去
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
  CN/Sample Province/Sample City/China Mobile/AS9808      10         10    90.0%    321.3
  US/California/Los Angeles/Vultr/AS204…      10         10   100.0%    276.6

top targets by cross-region coverage:
  TARGET                         REGIONS   PROBES      OK%   SPREAD ms
  121.127.34.119:443                   2        2   100.0%        55.4
  216.128.154.87:8443                  2        2   100.0%      1065.0

groups (target x region x ISP):
  TARGET                     REGION/ISP              PROBES      OK%   P50 ms
  121.127.34.119:443         CN/Sample Province/Sample City/Ch…       1   100.0%    305.6
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

### query：查询单个 IP:Port 的线路画像

```bash
cf-route-tester query 1.1.1.1:443 --db data/results.db        # 位置参数最自然
cf-route-tester query 1.1.1.1:443 --input data/batches        # 查公开 JSONL
cf-route-tester query 1.1.1.1:443 --db x.db --hops --series   # 逐跳 + 时间序列
cf-route-tester query --list-targets --input data/batches     # 列出数据里的目标
cf-route-tester query --stats --db data/results.db            # 只看全局统计
cf-route-tester query 1.1.1.1:443 --db x.db --format json     # 机器可读
```

两个数据源**必须恰好给一个**（`--db` 与 `--input` 互斥）：两个都给会让
"这个数字从哪来"说不清。位置参数与选项可以**任意交错**——
`query TARGET --db x` 与 `query --db x TARGET` 都可以。

输出示例（真实数据，两个采集者）：

```text
45.63.67.144:443
  regions:     2
  probes:      3 total, 2 ok (66.7%)
  traces:      1 total, 1 ok
  latency:     min 250.2  p50 250.2  p90 300.5  p95 300.5  max 300.5  avg 275.4 ms
  spread:      50.2 ms between the best and worst region (p50)

  failures:
    timeout:               1

  by region / ISP:
    REGION                                          PROBES      OK      OK%    P50ms
    CN/Sample Province/Sample City/China Mobile/AS9808               2       1    50.0%    300.5
    US/California/Los Angeles/Vultr/AS20473              1       1   100.0%    250.2

  AS path (CN/Sample Province/Sample City/China Mobile/AS9808):
    AS56046-AS20473                                              1
```

`--hops` 会额外给出逐跳延迟与超时情况（按 TTL 聚合，而不是按 IP——
同一个 TTL 在不同时间可能由不同的等价路由器应答）：

```text
  hops (CN/Sample Province/Sample City/China Mobile/AS9808):
    TTL  IP                             ASN           TO     P50ms     MAXms
      1  192.168.1.1                                   0      1.31      6.25
      2  192.168.1.1                                   0      1.19      1.22
      3  *                                             1      0.00      0.00
      4  203.0.113.24                  AS56046        0      4.58     16.54
      6  203.0.113.6                 AS9808         0     10.34     10.71
      9  203.0.113.9                  AS58453        0    195.12    196.45
```

#### 与 aggregate 的分工

| 命令 | 回答的问题 | 数据源 |
| --- | --- | --- |
| `aggregate` | 这一批数据**整体**如何（跨目标） | 公开 JSONL |
| `query` | **这一个**目标如何（跨维度下钻） | 本地库或公开 JSONL |

两者共用同一套统计逻辑（`aggregate.LatencyHistogram` 与分组键），
因此"查数据库得到的数字"与"聚合 JSONL 得到的数字"是一致的。

#### 裸 IP 有歧义时报错，不瞎猜

```text
$ cf-route-tester query 1.1.1.1 --input data/batches
Error: 1.1.1.1 matches 2 ports (443, 2053); specify the port, e.g. 1.1.1.1:443
```

一个 IP 有多个端口时静默返回其中一个会给出**错误的目标**，
而用户会以为那就是他要的。只有一个端口时才允许省略端口。

#### 分位数是近似值，且被标记

延迟用固定对数直方图（相对误差约 3.5%），因此分位数是近似值，
JSON 输出里带 `percentiles_approx: true`。**极值与平均值是精确的**。

分位数一定落在 `[min, max]` 之内——桶的代表值是几何中点，
可能略微超出桶内真实样本的范围，于是会出现"p90 = 305.6 而 max = 300.1"
这种自相矛盾的输出（实测踩到过），因此显式夹紧到精确极值。

同理，**0 毫秒不会被计入延迟样本**：0 在真实数据里意味着"没有测到"
（失败的探测、超时跳），而不是"0 毫秒"。收进来会让分位数被一堆 0
拉垮——实测表现是"只有一个 1.31ms 样本的跳，p50 却报成 0.00"。
"有多少次没测到"由超时列（`TO`）单独回答。

### db：本地 SQLite 数据库

```bash
cf-route-tester probe --db data/results.db                 # 测量并把结果落库
cf-route-tester probe --db data/results.db --country cn --province Sample Province \
                     --city Sample City --isp "China Mobile" --asn 9808
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

只剩**一个**命令尚未实现，它在 `cf-route-tester --help` 里列在
`Planned commands` 一节，执行时会明确返回 `not implemented yet`
（退出码 2），不会静默失败：

| 命令 | 作用 | 状态 |
| --- | --- | --- |
| `upload` | 将匿名压缩批次上传到 GitHub 数据仓库 | **暂缓**：先把本地链路跑通，凭据与归属方案确定后再做 |

其余命令均已实现，见上文「使用」一节：

| 命令 | 作用 |
| --- | --- |
| `web` | 启动图形界面（本地网页，数据不出本机） |
| `fetch` | 下载 / 缓存 / 解析 `all.json` |
| `detect` | 检测本机地区与运营商，写入本地标识文件 |
| `probe` | TCP 连通性与延迟测量 |
| `scan` | 全量扫描：结果实时写入 CSV、两级测量 |
| `trace` | 使用 NextTrace 对 `IP:Port` 做线路跟踪 |
| `export` | 导出为可公开的 JSONL（自动隐私过滤） |
| `aggregate` | 把公开 JSONL 聚合为按地区 / 运营商分组的统计 |
| `query` | 查询某个 `IP:Port` 在不同地区 / 运营商下的线路画像 |
| `db` | 本地 SQLite 维护（`stats` / `migrate` / `vacuum`） |
| `version` | 版本信息（`--verbose` 显示构建细节） |

`help` 与 `version` 之外的命令都在 `README.md` 的「使用」一节里有
完整示例与真实运行输出。

---

## 设计要点

### ASN 会翻译成线路名称

`AS4134`、`AS4809`、`AS4837` 这些数字对多数人没有意义，而它们代表的
恰恰是"这条线路好不好"的关键信息。因此逐跳输出与 CSV 都会把 ASN
翻成运营商内部的线路名：

```text
 4  203.0.113.4   2.84 ms  AS4134 (163)   China Telecom
 5  203.0.113.5   3.85 ms  AS4809 (CN2)   China Telecom

route:      CN2(AS4809) > Cloudflare(AS13335)
```

内置的映射（`internal/asnmap`）：

| ASN | 线路 | 运营商 |
| --- | --- | --- |
| AS4134 | 163 | 中国电信（普通出口） |
| AS4809 | CN2 | 中国电信（优质出口） |
| AS4837 | 169 | 中国联通（普通出口） |
| AS9929 | 9929/CUII | 中国联通（工业互联网骨干） |
| AS9808 | CMNET | 中国移动（普通出口） |
| AS58453 | CMI | 中国移动国际 |
| AS58807 | CMIN2 | 中国移动（优质出口） |
| AS10099 | CUG | 中国联通国际 |

另外收录了 Cloudflare、AWS、Google、NTT、PCCW、Vultr 等广为人知的
网络与云厂商。

三条维护原则：

1. **只做名称翻译，不给好坏评分。** 163 在空闲时段可能比 CN2 还快，
   评价取决于用途与时段，程序不替使用者下结论。
2. **未知 ASN 只显示编号，绝不猜名字。** 猜错的名字比没有名字更糟——
   使用者会据此判断线路质量。`AS65001` 就是 `AS65001`。
3. **相邻重复合并，不相邻的重复保留。** `163 > CN2 > 163` 表示流量
   出去又绕回普通出口，这是重要的路径异常，合并掉就等于隐瞒。

要补充映射，改 `internal/asnmap/asnmap.go` 里的 `routes` 表即可；
`asnmap_test.go` 会检查新增项自洽（名称非空、编号规范、排序正确）。

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
示例省移动 / 1.2.3.4:443 / 成功率 12%
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
├── cmd/
│   ├── cf-route-tester/          命令行入口（web 是它的一个子命令）
│   └── cf-route-tester-gui/      无控制台的图形入口（Windows 双击用）
├── internal/                     全部业务逻辑（20 个包）
│   ├── cli/                      命令分发、帮助、退出码、各子命令
│   ├── service/                  **与界面无关**的编排（CLI 与图形界面共用）
│   ├── webui/                    图形界面：HTTP 服务 + 嵌入式页面
│   ├── applog/                   日志（文件 + 控制台双写、大小轮转）
│   ├── version/                  程序版本与公开数据 schema 版本
│   ├── model/                    核心数据模型与不变量
│   ├── source/                   all.json 获取、解析、缓存
│   ├── probe/                    TCP 探测与错误分类
│   ├── worker/                   通用有界 worker pool（probe / trace 共用）
│   ├── trace/                    NextTrace 集成（外部进程 + JSON 归一化）
│   ├── detect/                   本机地区 / 运营商检测
│   ├── identity/                 本地匿名标识 collector_id
│   ├── scheduler/                扫描编排（历史数据库路径仍在用）
│   ├── csvstore/                 结果实时追加写入 CSV
│   ├── storage/                  本地 SQLite（迁移 / 只追加写入 / 查询）
│   ├── privacy/                  隐私过滤（内网地址判定与替换）
│   ├── export/                   公开导出（JSONL / CSV，隐私过滤的唯一出口）
│   ├── aggregate/                数据聚合
│   ├── query/                    单目标线路画像查询
│   └── asnmap/                   ASN -> 线路名称（163 / CN2 / CMIN2 ...）
├── tools/                        构建与测试辅助工具（独立可执行）
│   ├── release/                  跨平台发布构建
│   └── write-cache/              生成确定性缓存夹具（CI 用）
├── docs/INSTALL.md               安装、校验、权限、NextTrace 配置
├── .github/workflows/ci.yml      CI：格式 / vet / 测试 / 交叉编译 / 端到端冒烟
├── migrations/README.md          迁移规则与表结构说明（SQL 在 Go 代码里）
├── tests/README.md               为什么集成测试不集中放在这里
├── configs/config.example.yaml   配置设计草案（**当前未被读取**）
├── data/                         本地数据目录（内容不提交）
├── dist/                         构建产物（不提交）
├── LICENSE                       GPL-3.0 全文 + 第三方组件说明
└── README.md / doc.go / go.mod / go.sum / .gitignore / .gitattributes
```

> `internal/source/` 里**没有** `api_test.go`——下载层的重试与超时
> 由 `loader_test.go` 覆盖，因为两者走同一条 HTTP 客户端路径。
> 文件清单里如实标注了这一点，而不是列一个不存在的文件。

分层原则：`cmd` 与 `internal/cli` 只负责参数解析、调用与退出码；
业务逻辑一律在 `internal/` 下的具体包里。`internal/` 前缀意味着
这些包不对外暴露为公共 API——本项目发布的是**可执行文件**，
不是库，因此没有义务维护 Go API 的向后兼容。

---

## 文件清单

下面逐个说明**仓库里每一个被跟踪的文件**是做什么的。
（构建产物 `dist/`、本地数据 `data/all.json`、第三方二进制
`data/bin/` 都被 `.gitignore` 忽略，不会上传，见文末说明。）

### 顶层与构建

| 文件 | 作用 |
| --- | --- |
| `go.mod` | 模块声明。**Go 版本是 1.26**（`modernc.org/sqlite` 强制要求），唯一直接依赖是纯 Go 的 SQLite 驱动。 |
| `go.sum` | 依赖校验和。9 个间接依赖全部来自 SQLite 驱动。 |
| `doc.go` | 仓库级包文档：列出所有 `internal/` 包的职责与设计边界。 |
| `LICENSE` | **许可证尚未选定**，文件里写明了发布前必须完成的事项（详见下文「发布前的法律事项」）。 |
| `.gitignore` | 忽略 `dist/`、`bin/`、`data/` 等产物。 |
| `.gitattributes` | 强制 LF 换行与文本规范化。Windows 上编辑过的文件不会因 CRLF 产生噪声 diff。 |

### 程序入口

| 文件 | 作用 |
| --- | --- |
| `cmd/cf-route-tester/main.go` | 命令行入口：构造 `cli.Env`、调用 `cli.Run`、把退出码交给操作系统。**这里没有业务逻辑**，因此不需要测试。 |
| `cmd/cf-route-tester-gui/main.go` | 无控制台的图形入口（用 `-H=windowsgui` 构建）。负责三件有副作用的事：把工作目录切到 exe 所在目录（否则双击启动时数据库与日志会散落在 `C:\Windows` 之类）、标记 `Env.Detached`（告诉 web 命令"没有可用的 stderr，请只写日志文件"）、以及通过 `Env.OnServerReady` 创建常驻窗口。不带参数时直接启动图形界面，带子命令时仍走完整 CLI。 |
| `cmd/cf-route-tester-gui/window_windows.go` | 常驻小窗口：直接调 Win32（`user32` / `gdi32` / `shell32`，`syscall.NewLazyDLL`），不引入任何 GUI 框架，因此不破坏 `CGO_ENABLED=0`。显示状态与地址，提供「打开界面」「退出」，关窗即退出。**注意字体必须逐个控件发 `WM_SETFONT`**（发给顶层窗口无效），以及 `GetStockObject` 在 `gdi32` 而不是 `user32`——这两处都踩过，且都不报错/直接 panic。 |
| `cmd/cf-route-tester-gui/window_other.go` | 非 Windows 的同名桩函数。只为让本命令在所有平台都能编译（CI 的交叉编译步骤会跑）。 |

### `internal/applog/` — 日志

| 文件 | 作用 |
| --- | --- |
| `applog.go` | 文件 + 控制台双写、按大小轮转（5 MiB × 3 个备份）、级别过滤、nil 安全的级别方法。文件名/父目录不存在时自动创建；**打不开日志文件也不让程序起不来**，而是退化成只写控制台并说明原因。 |
| `applog_test.go` | 双写、轮转与备份上限、**不截断日志行**、启动时发现超大文件先轮转、目录不可写时优雅降级、重复 Close 幂等。 |

### `internal/service/` — 与界面无关的编排

| 文件 | 作用 |
| --- | --- |
| `service.go` | `ProgressHub` / `CSVProgressHub` / `Stats` 与共用的错误分类。`ProgressHub` 保留供历史数据库路径使用；`CSVProgressHub` 服务 CSV 扫描（事件多带 `CurrentTarget`，且 `Publish` 永不阻塞、慢订阅者丢自己的事件）。 |
| `country.go` | 「按国家挑目标」的实现。**只认 `location.cca2`**：上游两个国家字段有约 18% 互相矛盾，退回读另一个会让同一个国家的目标时而被选中时而不被选中，且没有任何提示。`targetsByCountry` 与 `filterByCountries` 共用同一字段与归一化，因此界面上的数字必然等于筛出来的条数。 |
| `trace_selection.go` | 「先测 TCP、再挑一批跟踪」的第二步：`SelectForTrace` （按国家/延迟上限/条数挑，同一目标取最快那行，重建 `model.Target` 时**必须填 `IPVersion`** ——实测漏填会让每一步都看着正常而跟踪全部 `invalid_target`）、`PreviewTraceSelection`（与实际执行共用选取逻辑，所以「预览 N 个」就是真会跑的个数）、`RunTraceSelection`。 |
| `csvscan_goroutine_test.go` | 「后台协程不能比函数活得久」。判据是**对前缀源的请求次数**而不是日志条数——后者要等重试耗尽与 120 秒超时才会出现，窗口内根本看不见，实测写成那样时「把修复禁用」它照样通过，等于没有护栏。另有一条守相反的错误：Cancel 会等协程退出，若下载不理会取消，这个等待就变成白等整张表。 |
| `trace_selection_test.go` | 国家筛选只认 `CCA2`、大小写与空列表、无国家的目标不被算进任何国家、统计与筛选口径一致；选取时填对 `IPVersion`（并**直接过一遍引擎用的同一套校验**）、延迟上限、跳过没测到的行、去重取最快、条数上限、按国家挑、国家从行里带过来、坏 IP 跳过、预览与实际选取一致。 |
| `csvscan.go` | **结果只进 CSV 的扫描实现**：加载目标 → 开 CSV → 逐个探测并**立即写入一行** → 只对探测成功的目标跟踪并再写一行。没有会话、没有续测、没有去重。`CSVScanOptions.Progress` / `OnTarget` / `OnTrace` 供界面显示进度与线路信息。 |
| `service_test.go` | 历史数据库路径的测试：真实本机监听 + 真实 SQLite、参数校验发生在任何副作用之前、进度回调、引擎不可用时仍完成 TCP 测量。 |
| `csvscan_test.go` | CSV 路径的测试：真实写入、**不创建数据库**、参数校验先于副作用、limit、进度与当前目标、追加不覆盖、以及**取消后已测行仍在文件里**（不调用任何 Close 直接读文件，并断言行数只增不减、没有半行残留）。 |
| `csvscan_parallel_test.go` | 探测**并发性**的回归测试。注入一个"每次都耗满 timeout"的确定性拨号器，于是并行 ≈ 1×timeout、串行 ≈ N×timeout，差距足够大不会因网络抖动而偶发失败；另有一个测试证明并发峰值**不超过**配置值。两个都必须存在：前者守"确实并发了"，后者守"没有并发过头"。 |
| `csvscan_prefix_test.go` | 本地 ASN 前缀识别的接线：引擎**不给任何 ASN**（`disable-geoip` 的效果）时仍由前缀匹配产出线路名、前缀匹配优先于引擎的 ASN、前缀没认出时退回引擎 ASN、关掉开关时退回原样、CSV 的 `as_path` 与日志走同一份判断、`NoASNPrefix` 时不建缓存。 |
| `csvscan_trace_test.go` | 跟踪阶段的测试（这段代码一度零覆盖）。用引擎的 `RunCommand` 注入点，不需要 nexttrace 也不需要管理员权限：只跟踪探测成功的目标、跟踪并发、并发下跟踪行完整（含 `as_path` 解析出 `163 > CN2`）、引擎失败仍保留探测行、取消停止剩余跟踪、CSV 列顺序被钉住。 |

### `internal/csvstore/` — 结果实时写入 CSV

| 文件 | 作用 |
| --- | --- |
| `csvstore.go` | 结果文件的追加写入器。三条约定：**每行写完立即 `Flush`**（中断不丢已完成结果）、**表头只在文件为空时写一次**（追加模式不重复写，否则表头会落在文件中间）、**不做会话与恢复**。未测到的延迟写空单元格而不是 `0`（0ms 与"没测到"在表格里含义不同）。`SanitizeErrorMessage` 在写入前去掉本机文件路径，但保留目标地址。 |
| `read.go` | CSV 的**读取**侧：`ReadAll`（按列名定位，因此老文件缺 `cca2` 也能读；坏行跳过而不是让整份结果打不开）、`Sort`（延迟/线路/目标，没测到的一律排最后）、`Countries`（按国家统计）、`CollapseByTarget`（把同一目标的探测行与跟踪行合并成一行，否则表格里同一目标会出现两次、其中一次「线路是空的」）。 |
| `path.go` | 结果文件默认按日期时间命名（`results-20261005-031500.csv`）：**每轮一个独立文件**。以前默认写死 `data/results.csv` 且不追加，于是每跑一轮就静默覆盖上一轮。`UniquePathAmong` 处理同一秒跑两轮的情况——时间戳只到秒，而 `--limit 1` 几百毫秒就结束，光查磁盘也不够（名字是测量前定的，文件要到写第一行时才创建）。 |
| `path_test.go` | 名字可排序（按文件名排序即按时间）、格式钉住（**不能有冒号**，Windows 上非法）、同名时换名字、目录也算被占用、连续取两次名字不重复。 |
| `read_test.go` | 写出去再读回来一致、**老文件少一列也不错位**、末行被写坏时前面的行完好、未闭合引号的行为（CSV 语义会吞掉余下内容，这里固定住事实）、排序把「没测到」排最后且顺序稳定、国家统计、合并规则（取最快延迟、保留线路、成功过就不留失败原因）。 |
| `csvstore_test.go` | 表头与列数、**不调用 Close 也不丢数据**（模拟进程被杀）、追加不重复表头、未测延迟为空、并发写入不丢行不串行、特殊字符转义、路径清洗（含 `i/o` 不被误判为路径）、超长信息按 rune 边界截断、父目录自动创建、重复 Close 幂等。 |

### `internal/webui/` — 图形界面

| 文件 | 作用 |
| --- | --- |
| `server.go` | HTTP 服务：路由、内嵌页面、SSE 进度推送、单次扫描约束、优雅关闭。安全默认开启且**不提供关闭开关**：进程级随机令牌（常数时间比较）、`Host` 回环校验（防 DNS rebinding）、变更请求的 `Origin` 校验（防 CSRF）。 |
| `results.go` | 两段式流程的接口：`/api/countries`（目标列表的国家分布）、`/api/results`（读 CSV 返回排好序的行，**排序在截断之前做**，否则「最快的那个」会取决于文件顺序）、`/api/trace/preview`、`/api/trace`。跟踪只收**筛选条件**而不收目标列表：CSV 是唯一数据源，让界面回传目标等于承认「界面手里那份」才是真相。 |
| `results_test.go` | 结果表格与国家清单的口径：**可选项清单不随筛选收缩**（勾了美国之后清单里不能只剩美国，否则想换一个国家也无从选起——这正是前端曾被迫多发一次请求绕过去的原因）、同一目标的探测行与跟踪行合并成一行、结果文件不存在不算错误。请求走真实 HTTP，因此顺带覆盖 Host 必须是本机与 token 两道访问控制。 |
| `log_test.go` | 优质线路的日志行带高亮标记、普通线路不带（一屏全红等于没标）、**失败**的跟踪不带（那会让人以为「这条路走了好线路」）、没识别出线路时不带；以及标记真的被序列化进发给界面的 JSON（少一个 tag 界面就永远收不到，而表现只是「某些行没变红」）。 |
| `resultspath.go` | 管理当前结果文件。三种操作含义不同，**不能共用一个静态默认值**：测量要新文件（不覆盖的落点）、跟踪要追加到刚测出的那份、读取要当前那份。`ResultsDir` 从配置的文件路径里取**目录**——直接当目录用会得到 `data/results.csv/results-....csv` 这种把文件当目录的嵌套路径（实测踩过）。 |
| `resultspath_test.go` | 目录推导（含空路径与只有文件名的回退）、当前文件稳定不跳动、测量每次要新文件（**即使同一秒**）、显式路径优先且空值不覆盖它、按名字排序即按时间排序。 |
| `assets/index.html` | 界面本体，原生 HTML/CSS/JS（不用框架，因此不需要 Node 工具链），通过 `go:embed` 打进二进制。 |
| `browser_windows.go` | 用默认浏览器打开地址（`cmd /c start` 需要一个空标题参数，否则 URL 会被当成窗口标题而不打开）。 |
| `browser_darwin.go` | 用 `open` 打开。 |
| `browser_unix.go` | 依次尝试 `xdg-open` / `gio open` / `x-www-browser` / `sensible-browser`；全失败时返回错误，由调用方降级为"打印地址让用户自己点"（服务器上这是常态）。 |
| `server_test.go` | 令牌缺失/错误/正确/查询参数、**非回环 Host 被拒**、**跨站 POST 被拒**、同源与无 Origin 放行、页面与安全响应头、扫描启动到结果、并发扫描被拒、用法错误映射、SSE 端到端送达进度、`Done()` 的两个方向。 |
| `events.go` | 界面日志事件的广播器：保留历史（刷新页面不空）、自增序号（重连去重）、`Emit` **永不阻塞**（慢页面丢自己的事件，不拖住测量）。 |
| `log.go` | 把扫描过程翻译成给**使用者看**的日志行。TCP 阶段**每个目标一行**，带延迟与连通性：`连通 1.2.3.4:443  285.4 ms  落地 US/Illinois/Chicago`；失败时给分类而不是整条错误信息：`不通 1.2.3.4:443  (timeout)  落地 …`（完整原因在 CSV 的 `error_message` 列）。跟踪阶段给出**ASN 编号 + 线路名称 + 落地地区**：`线路 1.2.3.4:443：15 跳，CMNET(AS9808) > CMI(AS58453)  落地 US/Illinois/Chicago`。只有名称无法核对编号，只有编号又认不出线路，因此两者都要。`emitTarget` 刻意**不写日志行**（只更新状态接口的"当前目标"），否则每个目标会有两行、其中一行没有结论。 |
| `events_test.go` | 历史与序号、历史上限、并发发布安全、慢订阅者不阻塞、`Emit` 不阻塞、日志接口要 token、一次扫描产生完整日志序列（含被测 IP 与实测延迟）、状态接口报告当前目标且结束后清空、失败也写日志。 |
| `log_format_test.go` | 锁定**日志行的内容契约**（纯格式，不需要真扫描）：每个 IP 的成功行含目标/延迟/落地地区、失败行给分类、线路行**同时含 ASN 编号与线路名称**与落地地区、识别不出线路时如实说明且不留悬空逗号、以及无地区数据时不产生"落地 -"这种空标注。 |

### `internal/version/` — 版本信息

| 文件 | 作用 |
| --- | --- |
| `version.go` | 程序版本（`0.1.13`）、公开数据 schema 版本（`1`）、构建期注入的 commit 与构建时间。`SchemaVersion` 独立于程序版本：程序可以频繁升级，公开数据结构不变它就不变。 |
| `version_test.go` | 保证版本字段永不为空（公开数据里不能出现空字符串版本号）。 |

### `internal/model/` — 核心数据模型

| 文件 | 作用 |
| --- | --- |
| `model.go` | `Target`（IP × Port）、`Location`、`ColoInfo`。不变量、归一化、`HasCoordinates`（0,0 是合法坐标，不能用 0 表示缺失）。 |
| `collector.go` | `CollectorProfile` 与分组键 `GroupKey()`（固定 6 字段顺序，保证同分组在任何时间/节点生成一致的键）。含 `IsAnonymous()` 这一可测试的隐私断言。 |
| `dedup.go` | 去重、排序、`ValidateAll`、批量键生成。解析层去重与存储层去重共用它，避免两套判据。 |
| `session.go` | `MeasurementSession` 与 `NewSessionID()`（UTC 时间前缀 + 随机后缀，同一秒并发也不撞 ID、不承载可识别信息）。 |
| `model_test.go` | Target 不变量、归一化、坐标边界（含 0,0 与越界）。 |
| `collector_test.go` | 分组键稳定性、ASN 归一化、匿名性断言。 |
| `session_test.go` | 会话 ID 格式与时区稳定性、生命周期（`Finished` / `Duration` / `Remaining`）、零值不 panic。 |
| `schema_test.go` | 公开 schema 相关的不变量（字段语义一旦确定就不能悄悄改）。 |

### `internal/source/` — all.json 获取、解析、缓存

| 文件 | 作用 |
| --- | --- |
| `api.go` | HTTP 下载：超时、重试与退避、代理支持、gzip 解压、主源失败后降级到备用源。**没有单独的 `api_test.go`**：它的重试/超时行为由 `loader_test.go` 覆盖（两者走同一条 HTTP 客户端路径）。 |
| `parser.go` | JSON / 文本容错解析、端口取并集、字段校验、去重、来源元数据（`SourceMeta` / `ParseStats`）。 |
| `cache.go` | 缓存读写（**原子写入**）、`cacheSchemaVersion` 格式版本闸门、`CacheInfo`。 |
| `loader.go` | 完整策略：新鲜缓存命中 → 网络 → 过期缓存降级；并把"缓存属于哪个源"作为判据的一部分。 |
| `parser_test.go` | 解析：字段缺失、类型异常、端口并集、去重、统计口径。 |
| `cache_test.go` | 缓存：原子写入、格式版本拒绝、损坏文件处理。 |
| `loader_test.go` | 加载策略：缓存命中、强制刷新、网络失败时降级、缓存源不匹配。 |
| `contract_test.go` | **上游结构契约**：用真实的 all.json 片段固定字段形态，上游改结构时立刻失败，而不是静默产出空数据。 |
| `live_test.go` | 需要真实网络的只读检查，网络不可用时**自动跳过**（不把环境问题报成代码问题）。 |

### `internal/probe/` — TCP 探测

| 文件 | 作用 |
| --- | --- |
| `probe.go` | `Prober`、`ProbeResult`、统计与**错误分类**。分类是这一层的核心价值：超时 / 连接被拒 / 网络不可达是不同的线路现象。 |
| `errno.go` | 错误码注册表的平台无关部分与查找逻辑。 |
| `errno_windows.go` | Windows WSA 错误码表（10061 / 10054 等）。 |
| `errno_unix.go` | Unix POSIX errno 表。 |
| `worker.go` | `probe.Run` 与 `Runner`。池子实现已抽到 `internal/worker`，这里只保留 probe 侧的名字作为薄适配器。 |
| `probe_test.go` | 分类映射、结果自洽性（不能既成功又带错误分类）、并发正确性。 |
| `worker_test.go` | 池子语义：并发上限、恰好处理一次、取消后计入 Skipped、生产者不尊重取消时不死锁。 |

### `internal/worker/` — 通用有界 worker pool

| 文件 | 作用 |
| --- | --- |
| `worker.go` | `Run` / `Producer` / `ProcessFunc` / `RunStats`。probe 与 trace 共用同一份并发与取消语义，因此不会出现"probe 能正常取消、trace 取消后卡死"这类漂移。 |
| `worker_test.go` | 独有行为：`emit` panic 被隔离、参数缺失不 panic、取消后账目闭合（`Processed + Skipped + Abandoned == Produced`）。 |

### `internal/trace/` — NextTrace 集成

| 文件 | 作用 |
| --- | --- |
| `engine.go` | `TraceEngine` 接口、`Hop` / `TraceResult`、失败分类（含"引擎不存在""权限不足"这类**环境问题**，它们不算线路质量）。 |
| `ntrace.go` | 外部进程调用：路径解析（PATH / 绝对路径 / 补 `.exe`）、参数构造（含数据源与 PoW 源）、超时、版本查询。 |
| `fetch.go` | nexttrace 的查找与自动下载。`SearchDirs`（同目录 → `data/bin` → cwd/data/bin）、`AssetName`（上游命名 `nexttrace_<os>_<arch>[.exe]`）、`Download`（原子落盘、体积下限挡住错误页、下载后真的跑一次 `--version`、失败则删掉残骸）。固定版本 `v1.7.3`：跟随 latest 意味着上游某天改了 JSON 结构，使用者的线路名会在毫无征兆的情况下变成空。 |
| `windivert.go` | Windows TCP/UDP 模式需要的 WinDivert。上游 nexttrace 的发布物里没有它，因此从 WinDivert 官方发布取 zip 并解出 `x64/` 那两份（用后缀匹配，不写死带版本号的顶层目录）。**只在 TCP/UDP 模式且文件缺失时下载**；下载后比对钉住的 SHA256——它是内核驱动，而上游不提供校验和，代理链路上被替换是现实风险，因此哈希不符是硬失败且不落盘。解压先写临时文件再改名，中断不会留下可能被加载的半截驱动。 |
| `windivert_test.go` | ICMP 模式下**不**下载（判错会让所有 ICMP 用户平白多一个驱动）、解出的是 x64 而非 x86、坏 zip 与缺文件的包被拒、**哈希不符是硬失败且不落盘**、非 Windows 上**一次请求都不发**、缺失检测，以及两个项目的默认版本号都带 `v` 前缀（曾把 `v2.2.2` 写成 `2.2.2`，症状只是一个 404）。 |
| `fetch_test.go` | 资源名与上游一致、不支持的平台在联网前就被拒绝、同目录优先于 `PATH`、`data/bin` 也在查找范围、`PATH` 兜底仍在、**找不到时的错误列出全部搜索位置**、显式路径判定、下载成功路径、**拒绝 HTML 错误页且不留残骸**、非 200 报错、跑不起来的二进制被清理、以及两条边界：未启用时**一次网络请求都不发**、显式路径失败时不下载别的。 |
| `provider.go` | 两个「源」选项：`DataProvider`（9 个 GeoIP 源）与 `PowProvider`（NextTrace API v3 的令牌源）。做成受校验类型而不是透传字符串，因为 **nexttrace 拿到不认识的源名不报错、而是换一个源继续跑**——那会让人以为在用 IPInfo 而实际不是，从结果上看不出来。含大小写不敏感与常见简写（`ipinfo` / `ipapi` / `leomoeapi` / `none` …）。 |
| `parser.go` | NextTrace JSON → `TraceResult` 的归一化。**含纳秒→毫秒换算**、二维 `Hops` 聚合、乱码 `*_en` 字段优先。 |
| `testdata/nexttrace_v1.7.3_icmp.json` | **真实** NextTrace v1.7.3 输出夹具。解析契约的依据；有测试断言它没被手工改过。 |
| `trace_test.go` | 用真实夹具锁定解析契约、参数构造（端口必须用目标自己的）、失败分类、超时、假二进制端到端。 |
| `provider_test.go` | 数据源与 PoW 源：规范值/别名/大小写、空值走默认、**拼错必须被拒绝且错误里列出可选值**、`disable-geoip` 不查地区、参数里带上 `--data-provider`、PoW 源只在 NextTrace-API 时才传、构造期挡住非法源、以及**可选值清单与 nexttrace `--help` 一致**（外部契约，不能随手改）。 |
| `exec_windows.go` | 启动 NextTrace 子进程时加 `CREATE_NO_WINDOW` + `HideWindow`：它是控制台程序，在无控制台的宿主里启动会让每个目标都弹出一个黑框。 |
| `exec_unix.go` | 非 Windows 的同名空操作（Unix 下启动子进程本来就不会开终端窗口）。 |

### `internal/detect/` — 本机地区 / 运营商检测

| 文件 | 作用 |
| --- | --- |
| `detect.go` | `Source` 接口、`Result`、合并规则（**手动配置优先**）、每源报告与暴露声明。 |
| `source_local.go` | 离线源：只推断出口 IP 版本。不猜国家/运营商（离线推断出的其实是 DNS 服务商的 ASN）。 |
| `source_geoip.go` | 联网源：可配置的 geo-IP API + 宽松但**不猜测**的解析（含 `_en` 字段优先、ASN 从 `org` 提取、去掉 ISP 名的 ASN 前缀）。 |
| `detect_test.go` | 合并优先级、单源失败不影响其它源、**绝不写入隐私字段**（喂一份塞满 MAC/主机名/坐标的响应，断言一个都没落盘）。 |

### `internal/identity/` — 本地匿名标识

| 文件 | 作用 |
| --- | --- |
| `identity.go` | `collector_id`（`c-` + 32 位十六进制，`crypto/rand` 生成，**不由 MAC/CPU/磁盘/公网 IP/主机名推导**）、画像、上一个会话 ID 的记忆与原子写入。文件损坏时**拒绝静默重建**。 |
| `identity_test.go` | 原子写入、损坏文件拒绝重建、ID 格式、跨进程一致。 |

### `internal/scheduler/` — 扫描编排与两级测量

| 文件 | 作用 |
| --- | --- |
| `scheduler.go` | 一次完整测量的编排（**历史数据库路径**）：会话确定 → 待测目标 → TCP 探测 → 落库 → **只对成功目标跟踪** → 收尾。`scan` 命令已改用 `csvscan.go`；此包保留供 `db` / `export` / `query` 读取既有数据。含条数+时间双触发落库、中断后保留会话。 |
| `scheduler_test.go` | 续测判据（三元组）、只测未完成目标、中断后会话保持未结束、两级过滤、跟踪失败不中断、幂等去重、落库失败被计数。 |

### `internal/storage/` — 本地 SQLite

| 文件 | 作用 |
| --- | --- |
| `sqlite.go` | 打开数据库、PRAGMA（WAL / busy_timeout / foreign_keys）、迁移执行、文件统计。 |
| `migrations.go` | **版本化迁移，表结构的唯一来源**。只增不改。 |
| `model.go` | `Measurement` / `Trace` / `TargetView`、目标与采集者 UPSERT、`NewTrace`（引擎结果 → 入库行）。 |
| `writer.go` | 只追加写入：整批一个事务 + `dedup_key` 幂等去重。失败结果同样入库。 |
| `query.go` | 按目标 / 采集者 / 会话 / 时间窗口查询，`PendingTargets` 等续测判据。 |
| `export_query.go` | 导出与查询专用的**流式**读取（带目标元数据的 3 表 JOIN、按会话过滤、会话列表）。 |
| `version_info.go` | 落库时写入的 `schema_version` 与 `client_version`，并说明它与"表结构版本"的区别。 |
| `storage_test.go` | 迁移幂等、只追加语义、外键约束、幂等去重、`collectors` 表**没有**隐私列（直接检查实际创建的列名）。 |
| `export_query_test.go` | 导出查询真的能执行（30 多列 JOIN 写错列名只有执行时才暴露）、数据库层**不**过滤私有目标（过滤是导出层的职责）。 |

### `internal/privacy/` — 隐私过滤

| 文件 | 作用 |
| --- | --- |
| `privacy.go` | 内网/保留地址判定（比 RFC1918 更宽：CGNAT、链路本地、文档用途、基准测试、保留、组播）、替换为 `private-v4` / `private-v6`、目标分类。判定基于解析后的地址，因此 `::ffff:192.168.1.1` 与 `192.168.1.1` 结论一致。 |
| `privacy_test.go` | 逐条地址边界（含 `172.15` / `172.32` / `100.63` / `100.128` 这些"紧邻但不属于"的地址，最容易写错一位）。 |

### `internal/export/` — 公开 JSONL 导出

| 文件 | 作用 |
| --- | --- |
| `schema.go` | **公开 Schema**：`Row` / `Measurement` / `Trace` / `TargetMeta`。字段名一旦确定，聚合、网站、第三方分析都依赖它。 |
| `convert.go` | `storage` → `Row` 转换，**在转换时**应用隐私过滤（不是转换后）、错误信息清洗（IP / 路径 / 用户名替换）。 |
| `output.go` | JSONL / gzip 编码。gzip 头 `ModTime` 置零 → **同一份数据导出两次字节相同**。 |
| `export_test.go` | 私有目标整行丢弃、内网跳**替换而非删除**、整体扫描式的"轨迹里任何原始内网地址都不得出现"、gzip 往返与确定性、`Close` 幂等。 |

### `internal/aggregate/` — 数据聚合

| 文件 | 作用 |
| --- | --- |
| `aggregate.go` | 输入行类型、JSONL 流式读取、**延迟直方图**（内存与样本数无关、可合并）、分组键、错误计数。 |
| `collector.go` | 累积状态：分组（目标×地区×运营商）、目标、采集者三个维度。 |
| `report.go` | 报告生成、`MinSamples` 过滤（作用在**目标**层面，否则会毁掉跨地区对比）、`notes`（防止误读数字）。 |
| `source.go` | 文件/目录发现、gzip 读取、**单个坏文件不终止整批**。 |
| `aggregate_test.go` | 分组语义、成功率、**失败样本不污染延迟统计**、直方图边界（分位数不得超出精确极值）、坏行计数、坏文件容错、notes 完整性。 |

### `internal/query/` — 单目标线路画像

| 文件 | 作用 |
| --- | --- |
| `query.go` | 数据集索引、目标与地区画像、AS 路径签名、逐跳统计（按 TTL 聚合而非按 IP）、时间序列。 |
| `load.go` | 两个数据源适配：**SQLite**（本地历史）与 **JSONL**（公开数据）。两条路径共用同一套统计逻辑，因此数字一致。 |
| `query_test.go` | 分组、裸 IP 有歧义时报错不瞎猜、IPv6 规范化、AS 路径去重、逐跳超时不计入延迟、时间序列保留最近点。 |

### `internal/asnmap/` — ASN 到线路名称

| 文件 | 作用 |
| --- | --- |
| `asnmap.go` | `routes` 表（163 / CN2 / 169 / 9929 / CMNET / CMI / CMIN2 / CUG 等）、`Normalize`、`Lookup`、`Label`、`ShortPath` / `FullPath`。**只翻名字，不给评分**；未知 ASN 返回空名称而不是猜一个。 |
| `premium.go` | 优质线路（CMIN2 / CN2 / 9929）的定义，**只有这一份**。两个使用者吃的东西不同：service 手里是 ASN（日志要不要标红），csvstore 手里是 CSV 短名（表格要不要置顶）。两处各存一份必然漂移，而漂移的表现只是「日志标红了但表格没置顶」这类没有报错的怪现象。 |
| `premium_test.go` | 清单里的 ASN 与名字确实存在于线路表（写错一位不会报错，只会永远匹配不上）、按 ASN 判定、多线路取等级最高的、按短名判定、**不做子串匹配**。 |
| `asnmap_test.go` | 用户给出的八条映射逐条固定、各种 ASN 写法归一化、拒绝路径串等非 ASN、未知不编名、短路径略去未知项、相邻去重但**保留不相邻重复**、CMI 与 CMIN2 必须能区分。 |

### `internal/asnprefix/` — 用线路的 IP 段识别线路

把"查每个 IP 属于哪个 ASN"反过来做：只取**关心那几条线路的 IP 段**，
再看路径里有没有跳落在段里。因为只需要 30 条线路的前缀，
所以抓一次就能缓存，之后完全离线——**没有限流、不需要账号、不需要 token**。

| 文件 | 作用 |
| --- | --- |
| `prefix.go` | IP 段集合与包含判断。按前缀起始地址排序后用二分查找，因此几万条前缀也是微秒级；前缀**主机位被 mask 掉**（上游数据里出现过 `1.2.3.4/24` 这种写法，不规范化会漏判）；容忍嵌套前缀；v4/v6 分开存。 |
| `store.go` | 加载与缓存。策略是"**缓存全新鲜就不联网，否则整张表下载一次**"：请求数从 60 降到 1，而且不会出现"60 个成功 18 个"的半成品状态。缓存 7 天过期（骨干线路的段变化很慢）；写入是**原子的**（临时文件 + rename，否则半截 JSON 会被当成"没有缓存"而反复重下）；下载失败时退回**已有缓存**（过期也比没有强）；带 3 次重试与退避（上游会暂时性失败）。 |
| `resolver.go` | `Match`（一个 IP 命中哪几条线路，顺序固定）、`ResolvePath`（按跳序只去掉**相邻**重复）、`PathWithNames`（`CN2(AS4809) > CMI(AS58453)`）、`Size` / `Ready`。 |
| `snapshot.go` | 内置快照：`go:embed` 一份 gzip 压缩的线路前缀，在「下载失败且无缓存」时兜底。它是**最后手段**而不是数据源——有更新的缓存或刚下载的数据时绝不使用。使用时会说出快照的生成时间，免得把过期数据当成现状。 |
| `snapshot_test.go` | 快照存在且可解析（同时是**构建护栏**：改了包结构却没重新生成会让测试直接失败）、ASN 写法归一化（`AS4809` 与 `4809` 必须都能取到，否则一个都匹配不到且无报错）、下载失败且无缓存时确实退回快照、**不盖过可用缓存**、**不盖过刚下载的数据**、快照里没有的 ASN 不被假装加载成功。 |
| `snapshotgen/` | 生成内置快照的小程序：读 `data/asnprefix/*.json`，写 `snapshot/snapshot.json.gz`。按 ASN 排序输出，让快照文件的 diff 稳定，便于 review。 |
| `warm.go` | `Warmer`：让抓取与 TCP 探测**并行**。`Warm` 立刻返回并在后台抓，`Get` 在需要时等待，`Ready` 非阻塞地问一句"好了吗"（用于提示"正在解析线路"，而不是让人对着不动的进度条猜）。nil 安全；上下文取消后仍返回已经抓到的部分——半份前缀比没有强。 |
| `iptoasn.go` | 全量映射表的下载与解析（gzip + 逐行流式，只保留 asnmap 那 30 条线路）。含**区间→CIDR 转换**：上游给的是起始/结束地址，本项目内部与缓存用 CIDR。转换用 big.Int（IPv6 是 128 位，手写位移容易出错，而错了不会报错——只会让某段 IP 悄悄落在集合外）。 |
| `asnprefix_test.go` | 区间→CIDR 的 9 条精确断言 + "恰好覆盖原区间"的语义断言（两端在内、紧邻地址在外）、整表解析（含坏行与 ASN 写法归一化）、**一次请求拿到所有 ASN**、**全有或全无**（表里没有段的 ASN 也要写缓存）、缓存新鲜时不联网、注入时钟决定过期、下载失败退回缓存、全盘失败不 panic、v4/v6 包含判断（含边界：`/24` 的最后一个地址与刚好越界）、带主机位的写法、嵌套前缀、坏数据被跳过而不是让整批失败、匹配顺序稳定、私有/非法地址不匹配、`ResolvePath` 只去相邻重复（"出去绕一圈又回来"必须保留）、抓取与缓存、过期重抓、抓取失败退回过期缓存、全盘失败不 panic、单个地址族 404 不算失败、缓存写入不留临时文件。 |

**为什么 `Match` 返回全部命中而不是第一个**：一个 IP 可能同时落在多条线路的宣告里（转售、代理、嵌套宣告），挑一个就等于丢信息。顺序固定为 asnmap 的顺序，否则同一份数据两次运行会给出不同线路名。

**为什么 `ResolvePath` 只去相邻重复**：一条路径可能先走 CN2、出去绕一圈又回到 CN2 —— 那是真实且有意义的信息，全局去重会抹掉它。

### `internal/export/` 的 CSV

| 文件 | 作用 |
| --- | --- |
| `csv.go` | 29 列扁平 CSV（含表头）：`CSVWriter`、列定义、逐跳压进单元格、`trace_as_path` 用线路名、`CSVOutput`（缓冲 + 可选 gzip，且 gzip 头时间置零保证可复现）。列名是契约，新增列只追加在末尾。 |
| `csv_test.go` | 表头与列数一致、特殊字符（逗号/引号/换行）正确转义、超时跳标成 `*`、未测到的延迟留空而不是 0、gzip 往返与确定性、无数据时不写表头。 |

### `internal/cli/` — 命令行

入口与分发：

| 文件 | 作用 |
| --- | --- |
| `cli.go` | 命令注册表、分发、退出码约定（`0` 成功 / `1` 运行期错误 / `2` 用法错误）。 |
| `help.go` | 顶层帮助与路线图。**命令一旦实现必须从路线图移到可用列表**，两边都有会让用户无法判断它到底能不能用。 |
| `cmd_source.go` | 各命令共用的数据源 flag（`--url` / `--fallback-url` / `--cache` / `--refresh` / `--proxy` 等）。 |
| `cmd_fetch.go` | `fetch` 命令；同时提供 `newFlagSet` / `parseFlags` / `parseFlagsAllowInterspersed` 三个解析助手。 |

各子命令（一个命令一个文件）：

| 文件 | 命令 | 作用 |
| --- | --- | --- |
| `cmd_probe.go` | `probe` | TCP 探测，支持 `--db` 落库、`--json` 输出 JSONL、失败分类汇总。 |
| `cmd_scan.go` | `scan` | 全量扫描：结果实时写入 CSV（`--out` / `--append`）、进度、两级测量汇总、`--verbose` 逐条输出、`--trace-data-provider` / `--trace-pow-provider`。上线前先校验用法类参数（模式、数据源、PoW 源），确保写错时**一个字节都不测**。 |
| `countrylist.go` | `--target-country` 的参数类型。会拆分逗号并归一化大小写——现有的 `stringList` 不拆逗号，`--target-country US,DE` 会变成一个匹配不上任何东西的值，而且不报错，表现只是「筛完一个目标都没有」。 |
| `console_windows.go` | `OwnsConsole()`：用 `GetConsoleProcessList` 判断控制台是不是专为本进程新建的（双击时只有自己，在已有终端里运行时终端也在同一控制台上）。 |
| `console_other.go` | 非 Windows 上恒为 false：这些平台双击不会凭空造出一个「退出即消失」的终端。 |
| `console_test.go` | 真的调用一次 `OwnsConsole`。看着单薄，挡的是一类真实事故：`syscall.NewLazyDLL` 取函数时名字写错会在**第一次调用**时 panic，而不是编译期报错（本项目踩过 `GetStockObject` 找错 DLL）。 |
| `cmd_trace_selection.go` | `trace --from` 的实现：从结果 CSV 里按国家/延迟上限/条数挑一批目标再跟踪。与图形界面走**同一个** `service.RunTraceSelection`，避免两边在「什么算符合条件的行」上分歧。 |
| `cmd_trace.go` | `trace` | 单个/批量线路跟踪，`--hops` 逐跳表、`--json` JSONL、`--data-provider` / `--pow-provider`。 |
| `providers.go` | 从 `trace.DataProviders()` / `PowProviders()` 生成 `--help` 里的可选值列表。手工维护的列表迟早与真正接受的值漂移，而「帮助里列了但用不了」最烦人。 |
| `cmd_detect.go` | `detect` | 检测本机地区/运营商，写入标识文件；发起请求**之前**打印"谁会看到你的 IP"。 |
| `cmd_db.go` | `db` | `db stats` / `migrate` / `vacuum`。 |
| `cmd_export.go` | `export` | 导出公开 JSONL，含 `--dry-run`、`--list-sessions`、格式与隐私报告。 |
| `cmd_aggregate.go` | `aggregate` | 聚合报告（text / json / jsonl）。 |
| `cmd_query.go` | `query` | 单目标画像；位置参数与选项可任意交错。 |
| `cmd_web.go` | `web` | 启动图形界面；同时提供 `RunDefault`（无子命令时的入口）与 `Env.Detached` 的处理。 |
| `cmd_source.go` | — | 各命令共用的数据源 flag，以及 `sourceParams.toConfig()`（避免每个命令各写一套转换规则而漂移）。 |
| `asnpath.go` | — | 把逐跳 ASN 转成线路串（`163(AS4134) > CN2(AS4809)`）并抽 `hopASNs`。规则本身在 `internal/asnmap`，与 web 界面共用同一份实现。 |

测试（全部在包内，用真实本机监听与真实 SQLite）：

| 文件 | 覆盖 |
| --- | --- |
| `cli_test.go` | 分发、退出码、帮助与路线图一致性、未实现命令明确报错。 |
| `fixture_test.go` | 测试夹具：写一份合法目标缓存（**离线**，绝不偷偷联网）。 |
| `helpers_test.go` | 测试计时助手。 |
| `cmd_probe_test.go` | 探测命令参数、JSONL 形状、失败分类输出。 |
| `cmd_scan_test.go` | 会话创建/续测/中断、`--limit` 保持源顺序、`--stdout`/`--quiet` 行为。 |
| `cmd_trace_test.go` | 跟踪命令：帮助文本、非法模式/目标、**引擎缺失时给出安装提示且退出码为 1（运行期错误）而非 2（用法错误）**、多个 `--target`、失败时不输出半截 JSONL。 |
| `cmd_detect_test.go` | 检测命令：手动值优先、写入语义、**绝不写入隐私字段**、JSON 输出可解析。 |
| `cmd_export_test.go` | 导出：隐私报告、gzip 可解、拒绝覆盖、zstd 明确拒绝并给替代方案。 |
| `cmd_query_test.go` | 查询：两个数据源互斥、交错参数、JSON 结构、`--hops` / `--series` / `--stats` / `--list-targets`。 |
| `cmd_db_test.go` | `db` 子命令：统计口径、迁移幂等、vacuum 行为。 |
| `cmd_web_test.go` | `web` 命令表面：帮助里说明了安全边界与日志位置、**默认只绑回环**、默认会写日志文件、未知参数与空 `--db` 被拒、不在"规划中命令"里。 |
| `cmd_fetch_test.go` | `fetch` 参数与输出。 |
| `cmd_fetch_cache_test.go` | 缓存命中路径（离线）。 |
| `cmd_fetch_e2e_test.go` | 端到端：下载 → 缓存 → 解析，使用本地 HTTP 服务器，不依赖外网。 |

### `tools/` — 构建与测试辅助

| 文件 | 作用 |
| --- | --- |
| `tools/release/main.go` | 跨平台发布构建：跑 gofmt/vet/test → 交叉编译 6 个平台 → 生成 `SHA256SUMS` 与 `release.json`。构建时间取自 **commit 时间**（而非 `time.Now()`），配合 `-trimpath` 实现**可复现构建**。 |
| `tools/release/main_test.go` | 钉住产物命名规则与校验和格式（它们属于发布产物的一部分，写错了用户就没法校验）、语义化版本校验、git 不可用时优雅降级。 |
| `tools/write-cache/main.go` | 生成**确定性**的目标缓存夹具，供 CI 端到端冒烟使用（不依赖外网数据源）。调用真实的 `source.WriteCache`，避免手工拼缓存格式而与实现漂移。 |

### `docs/`、`migrations/`、`tests/`、`configs/`、`data/`

| 文件 | 作用 |
| --- | --- |
| `docs/INSTALL.md` | 安装完整说明：校验下载、macOS Gatekeeper、交叉编译、可复现构建、NextTrace 安装与权限、最短上手路径。 |
| `migrations/README.md` | 迁移规则（只增不改、事务、幂等）与当前 5 张表的结构说明。**SQL 本身在 Go 源码里**，这里只有给人看的说明。 |
| `tests/README.md` | 说明为什么跨模块集成测试**不集中放在这里**，以及端到端覆盖实际在哪个文件里。 |
| `configs/config.example.yaml` | 配置文件的**设计草案**。当前版本**不读取它**——所有可配置项都走命令行 flag。保留它是为了固定键名、默认值与"Token 只能来自环境变量"这条硬约束。 |
| `data/.gitignore` | 忽略 `data/` 下的一切（数据与第三方二进制都不上传）。 |
| `data/README.md` | 说明 `data/` 目录里各文件是什么、为什么不上传。 |

### CI 与发布

`.github/workflows/ci.yml` 管每次推送的验证（gofmt / vet / `-race` /
六平台交叉编译 / CGO 检查 / 端到端冒烟 / 发布工具自检）。

`.github/workflows/release.yml` 管发布：打一个 `v*` tag 即构建并创建
| `.github/release-notes.md` | 发布说明的模板，由工作流读入并在末尾追加「完整变更」链接。开头就是「该下哪个文件」——四个 Windows exe 名字很像，实测有人下成命令行版然后双击，只看到一个闪过的黑框。 |
GitHub Release。它用工作流自己的临时 `GITHUB_TOKEN`，因此**不需要任何
长期密钥**——本地脚本则要在某台机器上放一个长期有效的 PAT，既容易泄露
也容易在换机器时失效。产物由干净 checkout 从 tag 指向的提交构建，而且
"打 tag"与"发布"之间没有人工步骤，不会出现 tag 打了却忘了传。
发布前会重跑完整测试，并用 `sha256sum -c` 校验刚构建的产物：清单与文件
对不上会让所有认真校验的人以为下载被篡改。

| 文件 | 作用 |
| --- | --- |
| `.github/workflows/ci.yml` | 11 个步骤：gofmt 检查 → `go vet` → `go test -race` → `go build` → 6 平台交叉编译 → `CGO_ENABLED=0` 校验（防止有人引入需要 CGO 的依赖）→ 二进制冒烟 → **完整链路端到端冒烟**（scan → export → aggregate/query，含隐私回归断言）→ 发布工具自检。 |

---

## 哪些文件不会上传

以下内容被 `.gitignore` 忽略，**不会**出现在 GitHub 仓库里：

| 路径 | 大小（本机实测） | 为什么不提交 |
| --- | --- | --- |
| `data/all.json` | 5.1 MB | 上游目标列表的缓存，随时可重新下载；提交它会让仓库无谓地变大且迅速过期。 |
| `data/bin/nexttrace_*.exe` | 32.1 MB | 第三方二进制（GPL-3.0），由用户按 `docs/INSTALL.md` 自行下载。 |
| `data/bin/WinDivert.dll`、`WinDivert64.sys` | 约 140 KB | NextTrace 的运行时依赖，由 `nexttrace --init` 生成。 |
| `dist/` | 81.7 MB | 构建产物（含 `dist/release/` 的 6 个平台二进制，每个约 11 MB）。发布走 GitHub Releases，不进仓库历史。 |
| `data/*.db` | 视使用而定 | 本地测量数据库（个人数据），绝不提交。 |
| `data/collector.json` | 约 235 B | 本地匿名标识。虽然不含隐私信息，但它是**这台机器**的身份，提交它会让不同人的数据混在同一个 ID 下。 |

因此别人 `git clone` 下来只有 **128 个文件 / 约 1.57 MB**（源码 + 测试 + 文档），
不含任何数据与二进制。这一点已实测：把仓库克隆到临时目录后，
`go build ./...`、`go vet ./...`、`go test ./...` 全部通过
（16 个包全绿），说明**没有遗漏任何构建所需的文件**。

> 上面的大小与文件数是**实测值**而不是估计值。README 里写估计数字
> （"大约 33 MB"）迟早会与事实脱节，而读者没有理由怀疑它。
> 仓库体积这类可测量的数字，应当用命令量出来再写进去。

### 排查问题时的内部诊断日志

排查数据来源问题时可以打开内部诊断日志（仅输出到 stderr，默认关闭）：

```bash
CF_ROUTE_TESTER_DEBUG=1 cf-route-tester fetch --verbose
```

---

## 配置

> **当前版本不读取配置文件。** 所有可配置项都通过命令行参数传入
> （见 `cf-route-tester <command> --help`）。
>
> `configs/config.example.yaml` 是一份**设计草案**：它固定了将来的键名、
> 各项的合理默认值，以及"上传 Token 只能来自环境变量"这条硬约束。
> 保留它是为了避免将来实现时随手发明另一套键名。
> 现在想要这些设置请用对应的 flag，例如：
>
> ```bash
> cf-route-tester probe --workers 100 --timeout 3s
> cf-route-tester scan  --workers 100 --timeout 3s --trace --trace-mode tcp
> cf-route-tester trace --target 1.1.1.1:443 --binary nexttrace --timeout 15s
> ```
>
> 引入 YAML 解析会把第一个非必要第三方依赖带进项目（当前唯一直接依赖是
> SQLite 驱动），因此这件事被推迟到确有需要时。

下面是已经确定的设计约定，`configs/config.example.yaml` 按此编写：

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
       示例省移动        示例省电信        广东移动
          │              │              │
          └──────────────┼──────────────┘
                         ▼
              IP:Port 线路画像数据库
```

---

## 许可证

**GNU General Public License v3.0**（GPL-3.0），全文见 [`LICENSE`](LICENSE)。

```text
cf-route-tester
Copyright (C) 2026 HERO-WPC

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.
```

### 这意味着什么

- 你可以自由使用、修改、再分发本项目，**包括商业用途**；
- 但如果你**修改后分发**，必须以同样的 GPL-3.0 开源你的版本；
- 不提供任何担保（见 `LICENSE` 第 15、16 条）。

如果你想要一个可以被闭源项目直接使用的版本，GPL-3.0 做不到——
那是 MIT / Apache-2.0 的适用范围。

### 第三方组件

本项目**不内嵌** NextTrace：它是以独立子进程方式调用的外部程序
（`exec.Command`），源码不复制、不静态链接、二进制不入库
（`data/bin/` 已被 `.gitignore` 忽略）。用户在需要线路跟踪功能时，
按 [`docs/INSTALL.md`](docs/INSTALL.md) 自行从上游获取并遵守其许可证
（NextTrace-core 同样是 GPL-3.0，与本项目方向一致）。

Go 依赖的许可证清单（BSD-3-Clause 与 MIT，均与 GPL-3.0 兼容）
列在 [`LICENSE`](LICENSE) 末尾，逐项是**读模块缓存里的 LICENSE 文件
核对过**的，不是凭印象写的。
