# 安装与构建

## 一、直接用预编译二进制（推荐）

从发布页下载对应平台的文件，**不需要安装任何运行时**（不需要 Python / Node / Java，
也不需要 Go）。产物是静态单文件二进制。

| 平台 | 文件名 |
| --- | --- |
| Windows x64 | `cf-route-tester-<版本>-windows-amd64.exe` |
| Windows ARM64 | `cf-route-tester-<版本>-windows-arm64.exe` |
| Linux x86_64 | `cf-route-tester-<版本>-linux-amd64` |
| Linux ARM64 | `cf-route-tester-<版本>-linux-arm64` |
| macOS Intel | `cf-route-tester-<版本>-darwin-amd64` |
| macOS Apple Silicon | `cf-route-tester-<版本>-darwin-arm64` |

### 校验下载（别跳过）

发布页附带 `SHA256SUMS`。**先校验再运行**，这是唯一能确认
"我运行的就是发布者构建的那个文件"的方式。

```bash
# Linux / macOS
sha256sum -c SHA256SUMS --ignore-missing
# 或者 macOS 自带：
shasum -a 256 -c SHA256SUMS
```

```powershell
# Windows PowerShell
Get-Content SHA256SUMS | ForEach-Object {
    $hash, $file = $_ -split '  ', 2
    $actual = (Get-FileHash $file -Algorithm SHA256).Hash.ToLower()
    if ($actual -eq $hash) { "OK   $file" } else { "BAD  $file" }
}
```

`release.json` 里还有每个产物的字节数、构建时间与 commit，
可以用来核对"我这个产物对应哪次提交"。

### 发布产物是可复现的

同一份提交（干净的工作区）无论何时、在哪台机器上构建，
产物的 SHA256 都相同：

```text
$ go run ./tools/release -skip-tests   # 第一次
  cf-route-tester-0.1.11-linux-amd64   12.41 MiB  <hash>
$ go run ./tools/release -skip-tests   # 第二次
  cf-route-tester-0.1.11-linux-amd64   12.41 MiB  <hash>   <- 与上面完全相同
```

（这里不写死具体哈希：它随每次提交变化。实际使用时两次输出一致即可。）

这靠三件事：

1. `-trimpath` 去掉构建机器的绝对路径；
2. 构建时间取自 **commit 时间**，而不是 `time.Now()`；
3. `CGO_ENABLED=0`（不引入 C 工具链的差异）。

因此"下载后比对校验和"真的能证明拿到的是发布者构建的那个文件，
而不只是"文件没损坏"。你可以自己构建一份来核对：

```bash
go run ./tools/release -version <发布页上的版本>
# 与发布页的 SHA256SUMS 比对
```

工作区有未提交改动时，commit 会被标注为 `-dirty`，
此时哈希自然不同——`release.json` 里的 `dirty` 字段说明了这一点。

### 赋予执行权限（Linux / macOS）

```bash
chmod +x cf-route-tester-*-linux-amd64
mv cf-route-tester-*-linux-amd64 /usr/local/bin/cf-route-tester
```

### macOS 的 Gatekeeper 提示

未签名的二进制第一次运行会被拦下。两种处理方式：

```bash
# 方式一：去掉隔离属性（明确表示你信任这个来源）
xattr -d com.apple.quarantine ./cf-route-tester-*-darwin-arm64
```

或在「系统设置 → 隐私与安全性」里点「仍要打开」。

本项目**不做代码签名**：签名需要付费证书，而校验和 +
源码可复现构建已经能提供同样的完整性保证。

## 二、从源码构建

需要 Go 1.26 或更高版本。

```bash
git clone <repo>
cd cf-route-tester
go build -o cf-route-tester ./cmd/cf-route-tester
```

零 CGO：`CGO_ENABLED=0` 是默认可用的构建方式。

### 交叉编译

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o cf-route-tester-linux-amd64   ./cmd/cf-route-tester
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o cf-route-tester-darwin-arm64  ./cmd/cf-route-tester
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o cf-route-tester-windows-amd64.exe ./cmd/cf-route-tester
```

### 注入版本信息

```bash
go build -trimpath \
  -ldflags "-s -w \
    -X github.com/cf-route-tester/cf-route-tester/internal/version.Commit=$(git rev-parse --short HEAD) \
    -X github.com/cf-route-tester/cf-route-tester/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o cf-route-tester ./cmd/cf-route-tester
```

`-trimpath` 会去掉产物里的构建机器绝对路径（既是隐私问题，
也让同一份源码在不同机器上构建出的二进制一致）。

用 `--version --verbose` 可以确认注入是否生效：

```text
client:         cf-route-tester
version:        0.1.11
schema_version: 1
commit:         <短 commit 号，由构建时的 git HEAD 决定>
build_date:     <取自该 commit 的提交时间>
go_version:     go1.26.5
platform:       windows/amd64
```

这里刻意不写死一个具体的 commit 号：每次提交它都会变，
写进文档只会在下一次提交后变成错误信息。

## 三、生成完整发布包

`tools/release` 是一个 Go 程序（**任何平台都能跑**，只需要 Go 工具链），
它会交叉编译全部 6 个目标平台、生成校验和与发布清单：

```bash
# 版本号从源码读取
go run ./tools/release

# 发布时显式指定，避免忘记同步改代码
go run ./tools/release -version 0.2.0

# 跳过 gofmt / vet / test（不建议）
go run ./tools/release -version 0.2.0 -skip-tests
```

产物落在 `dist/release/`：

```text
cf-route-tester-0.1.11-windows-amd64.exe   12.63 MiB
cf-route-tester-0.1.11-windows-arm64.exe   11.76 MiB
cf-route-tester-0.1.11-linux-amd64         12.41 MiB
cf-route-tester-0.1.11-linux-arm64         11.75 MiB
cf-route-tester-0.1.11-darwin-amd64        12.42 MiB
cf-route-tester-0.1.11-darwin-arm64        11.81 MiB
SHA256SUMS
release.json
```

为什么用 Go 而不是 shell 脚本：一个 `.ps1` Linux 用户跑不了，
一个 `.sh` Windows 用户跑不了，两套脚本必然漂移（命名规则、
校验和格式、清单字段）。用 Go 写就只有一份实现，而且命名规则
与校验和格式能被单元测试覆盖——那两样属于发布产物的一部分，
写错了用户就没法校验。

**发布前必须做**：确认 README 里写的版本号与 `-version` 一致，
否则文档与产物会互相矛盾。

## 四、线路跟踪需要额外的 NextTrace

`probe` / `scan` / `export` / `aggregate` / `query` **不需要** NextTrace。
只有 `trace` 与 `scan --trace`（线路跟踪）需要。

NextTrace 是**独立的外部程序**，本项目以子进程方式调用它，
不内嵌其源码（它是 GPL-3.0，本项目不是）。

### 安装

从 [NextTrace releases](https://github.com/nxtrace/NTrace-core/releases) 下载，
例如 Windows x64：

```text
https://github.com/nxtrace/NTrace-core/releases/download/v1.7.3/nexttrace_windows_amd64.exe
```

放到 PATH 里的任意位置，或放到项目目录里用 `--binary` 指定路径。

### Windows：释放 WinDivert 运行时（只需一次）

Windows 上的 TCP/UDP 探测依赖 WinDivert：

```powershell
nexttrace --init      # 生成 WinDivert.dll 与 WinDivert64.sys（普通权限即可）
```

### Windows：跟踪需要管理员权限

```powershell
# 以管理员身份运行终端，然后：
cf-route-tester trace --target 1.1.1.1:443
```

没有管理员权限时会得到明确分类，而不是一句没用的错误：

```text
failures by type:
  permission_denied:       1

note: "permission_denied" means the trace could not be performed
      (environment / permission), not that the path is bad.
```

**没有管理员权限时也可以用 ICMP 模式验证链路**（不需要 WinDivert）：

```powershell
cf-route-tester trace --target 1.1.1.1:443 --mode icmp
cf-route-tester scan --trace --trace-mode icmp
```

### Linux / macOS

TCP/UDP 跟踪通常需要 root 或相应 capability：

```bash
sudo cf-route-tester trace --target 1.1.1.1:443
# 或者只给这一个可执行文件所需的能力：
sudo setcap cap_net_raw,cap_net_admin+eip ./cf-route-tester-*-linux-amd64
```

### 找不到 NextTrace 时

只影响跟踪，其它功能照常可用：

```text
$ cf-route-tester trace --target 1.1.1.1:443
Error: ... NextTrace not found.
Please install NextTrace or configure trace.nexttrace.binary.
```

## 五、从零到第一次测量的最短路径

```bash
cf-route-tester fetch                      # 下载并解析目标列表
cf-route-tester detect --write             # 检测本机地区 / 运营商并写入标识
cf-route-tester scan --limit 50            # 先小批量验证链路是否通
cf-route-tester scan --db data/results.db  # 全量扫描（可随时 Ctrl+C，之后 --resume）
```

只想先看看工具能不能用，可以完全不落库：

```bash
cf-route-tester fetch
cf-route-tester probe --limit 20
```
