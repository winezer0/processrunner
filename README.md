# processrunner

[中文](README.md) | [English](README.en.md)

Go 进程执行包，提供同步执行、异步状态查询、取消与超时、后代进程清理、受限输出捕获和可选磁盘日志。

模块：`github.com/winezer0/processrunner`；要求 Go **1.25.5 或更新版本**。依赖标准库和 `golang.org/x/sys`，不依赖业务框架。Windows、Linux、macOS 的 amd64/arm64 均有原生 CI 测试。

## 安装

```sh
go get github.com/winezer0/processrunner@v0.0.1
```

本地联调可在消费项目的 `go.mod` 中添加（路径按实际位置调整）：

```go
replace github.com/winezer0/processrunner => ../processrunner
```

## 快速开始

下面的完整示例需要系统已安装 `git`：

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/winezer0/processrunner"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    result, err := processrunner.Run(ctx, processrunner.Invocation{
        Name:                  "git-version",
        Binary:                "git",
        Args:                  []string{"--version"},
        CaptureBytes:          64 << 10,
        RequireCompleteOutput: true,
    })
    if err != nil {
        log.Fatalf("state=%s exit=%d: %v", result.State, result.ExitCode, err)
    }
    fmt.Print(string(result.Output))
}
```

`Run` 等待进程退出、后代清理、输出排空和日志关闭。参数直接传给可执行文件，不经过 shell；重定向、管道和通配符不会自动展开。需要 shell 行为时，由调用方显式选择 shell 并处理输入安全。

### 执行配置

Unix 下，`Start` 和 `Run` 会在启动前为目标普通文件添加所有者执行权限（`u+x`），保留其他权限位；修改失败则返回错误。支持显式路径（相对于 `Directory`）和 `PATH` 查找。常规可执行文件查找失败时，选择 `PATH` 中首个普通文件，但拒绝隐式的相对 `PATH` 匹配。Windows 权限不变。`CheckTools` 不修复权限，缺少执行权限时报告不可用。

| `Invocation` 字段 | 行为 |
| --- | --- |
| `Name` | 记录到快照和日志元数据的名称，可为空 |
| `Binary` | 必填的可执行文件名或路径；名称按 `os/exec` 规则查找 |
| `Args` | 参数数组，不包含可执行文件名 |
| `Directory` | 命令工作目录；空值继承当前目录，相对路径相对执行方当前目录解析 |
| `Environment` | 追加到继承环境的 `KEY=value` 项；同名变量由后面的值覆盖 |
| `CaptureBytes` | 合并内存捕获上限；非正数使用 `DefaultCaptureBytes`（1 MiB） |
| `LogBytes` | 每个磁盘输出流的上限；非正数使用 `DefaultLogBytes`（8 MiB） |
| `RequireCompleteOutput` | 内存捕获不完整时附加 `ErrOutputLimit`；仅磁盘截断不会触发该错误 |
| `Stdout` / `Stderr` | 可选原始输出 `io.Writer`，独立于捕获和磁盘上限 |

`Start` 会复制参数和环境切片。包没有标准输入配置；命令标准输入连接空设备，不用于交互式终端会话。

## 异步执行与状态

以下片段假设已有 `ctx` 和 `invocation`，并位于返回 `error` 的函数中：

```go
handle, err := processrunner.Start(ctx, invocation)
if err != nil {
    return err
}
defer handle.Stop()

snapshot := handle.Snapshot()
_ = snapshot
// 需要取消时调用 handle.Stop()。
result, err := handle.Wait(context.Background())
_ = result
return err
```

- `Start(ctx)` 在进程启动并完成平台进程树绑定后返回；启动失败不返回 Handle，`Run` 此时返回空 `Result` 和错误。
- `Start` 的 context 控制执行生命周期；`Wait(ctx)` 的 context 仅控制本次等待。等待超时返回空 `Result` 和 context 错误，进程可能仍在运行。
- `Stop` 可重复调用。调用后仍需 `Wait` 或等待 `Done`，确认清理完成。执行 deadline 不保证函数在该时刻立即返回，收尾仍需时间。
- `Snapshot`、`Stop`、`Wait`、`Done` 可并发使用；结果和快照均为独立副本。完成后的 `Wait` 可重复调用，并优先于已取消的等待 context 返回最终结果。
- `Done` 在后代清理、输出排空和日志关闭完成后关闭。默认 `ProcessRunner{}` 可并发复用，不保留已完成任务；`Runner` 和 `AsyncRunner` 可用于依赖注入。

快照包含 `Name`、实际命令 `PID`、`State`、`ExitCode`、UTC 起止时间、最近输出时间、`OutputBytes` 和 `Truncated`；`Result` 另含合并输出 `Output`。

状态包括 `starting`、`running`、`success`、`failed`、`canceled`、`timed_out`。终态优先级为超时、取消、其他失败、成功。输出超限或清理错误不会覆盖取消/超时语义。即使命令退出码为 0，输出写入、清理或最终日志关闭/保存失败也可能使整体执行失败；应同时检查错误、状态和退出码。

错误可合并多个原因：用 `errors.Is` 检查 `context.Canceled`、`context.DeadlineExceeded`、`ErrOutputLimit`、`io.ErrShortWrite` 或 `exec.ErrWaitDelay`；用 `errors.As` 检查正常执行路径的 `*exec.ExitError`。取消路径以 context 错误为主，不保证同时返回退出错误。未正常退出时，`ExitCode` 可能为 `-1`。

## 输出与日志

stdout/stderr 分别读取，再合并到有界捕获；两路之间不保证严格的时间顺序。`OutputBytes` 统计脱敏后、内存截断前的输出字节数。`Truncated` 表示内存捕获或任一磁盘流发生截断；上限到达后仍继续读取输出。

### 原始输出 Writer

`Stdout` / `Stderr` 在脱敏前接收未经上限截断的原始字节。调用方负责 Writer 的存储、权限和敏感信息处理；库不会关闭它们，也不会将它们序列化到 Linux guardian 载荷。两个流共用 Writer 时，Writer 必须支持并发写入。

Writer 错误或短写会触发进程终止，并使执行失败；短写转换为 `io.ErrShortWrite`。Writer 的 `Write` 必须能够及时返回，关闭管道无法中断永久阻塞的用户回调。

Writer 必须保持可用，直到 `Done` 关闭、`Run` 返回，或 `Wait` 返回最终结果。仅因等待 context 取消而返回的 `Wait` 不代表执行完成，此时不能关闭 Writer。

进程退出与输出排空独立进行：先清理受控后代，再等待两路输出完成。因此正常主进程退出后，后代持有管道不会仅因清理顺序而产生 `ErrWaitDelay`。清理后的输出排空时限目前为 2 秒；到期关闭管道并附加 `exec.ErrWaitDelay`，输出可能不完整。该时限不能中断阻塞中的 Writer。

### 磁盘日志

```go
ctx = processrunner.WithLogDirectory(ctx, "./private-process-logs")
result, err := processrunner.Run(ctx, invocation)
entries, readErr := processrunner.ReadLogs("./private-process-logs", 16<<10)
```

未设置日志目录时仅做内存捕获。启用后，每次执行创建 `process-*` 目录，包含 `stdout.log`、`stderr.log` 和 `process.json`。元数据不记录参数或环境变量；输出文件仍可能含敏感信息。文件创建使用目录 `0700`、文件 `0600`，已有目录权限不会自动收紧；Windows 访问控制还取决于目录 ACL。

显式 `Environment` 项的变量名包含 `TOKEN`、`PASSWORD` 或 `SECRET`（不区分大小写）时，其非空值会在库内捕获和磁盘输出中脱敏，支持跨写入边界匹配。不扫描继承环境中的凭证，也不处理原始 Writer；该机制不能代替调用方的敏感信息管理。

`ReadLogs(directory, tailBytes)` 返回含 `ID`、快照、`Stdout`、`Stderr` 的 `LogEntry` 切片：

- `tailBytes` 必须为 1–65536，每个流最多返回相应数量的尾部字节；按字节截取可能切开 UTF-8 字符。
- 目录不存在时返回空列表。读取通过根目录约束，拒绝越出日志根目录的符号链接；损坏或不可读条目会导致调用返回错误。
- 当前只枚举前 64 个目录项，再过滤 `process-*` 并按名称排序；其他项也占枚举额度。不保证最新记录、时间顺序或完整列表，没有分页/游标。
- 调用方负责日志目录可信性、保留期限和清理。运行中元数据可能滞后，读取不是事务快照；最终保存失败或宿主异常退出后，磁盘状态可能与内存终态不一致。

## 工具健康检查

```go
health := processrunner.CheckTools(ctx, []processrunner.Tool{
    {Name: "git", Binary: "git", VersionArgs: []string{"--version"}},
})
```

返回顺序与输入一致，每项 `ToolHealth` 包含 `Name`、`Ready`、`Version`、`Error`。先查找可执行文件，再通过同一执行器运行版本命令；每项执行 context 最多 3 秒，并受调用方 context 限制，收尾可能延长实际耗时。成功时合并捕获最多 4096 字节并去除首尾空白，版本输出截断本身不会使 `Ready` 变为 false。

每个工具独立启动 goroutine，目前没有并发上限；大列表应由调用方分批或限流。检查适用于只读版本命令，`Ready` 不代表完整业务能力验证。

## 平台保证与边界

| 平台 | 取消及正常退出后的后代清理 | 宿主意外退出 |
| --- | --- | --- |
| Windows | 创建挂起进程，先加入 Job Object 再恢复；终止受控 Job 内后代 | 最后一个 Job 句柄关闭时触发 kill-on-close |
| Linux | guardian 管理独立进程组；取消先 SIGTERM，宽限期后 SIGKILL；正常退出后清理组内后代 | 所有权管道断开后 guardian 清理进程组 |
| macOS / 其他 Unix | 独立进程组，取消或正常退出后以 SIGKILL 清理组内后代 | 无宿主异常退出后的自动清理保证 |

Linux guardian 重新运行宿主可执行文件；包初始化识别首个参数 `--processrunner-internal-guardian` 并分派内部入口，随后调用 `os.Exit`。该参数和 `PROCESSRUNNER_INTERNAL_INVOCATION` 环境变量保留给内部使用。重新执行也会运行 guardian 入口之前的依赖初始化，需要考虑初始化副作用；无需单独部署 guardian。调用载荷通过 JSON/base64 环境变量传递，大参数/环境会额外占用系统启动空间；内部载荷在传给实际命令前移除。

进程组无法约束主动 `setsid` / `setpgid` 脱离的后代；宿主退出保证也依赖 guardian 或 Job 机制正常工作。本包不提供恶意程序沙箱、CPU/内存配额、暂停恢复、自动重启、持久化调度或服务重启后的任务恢复。其他 Unix 使用通用实现，但未纳入当前原生 CI；不声明支持 Windows/Unix 以外的平台。

## 测试与 CI

[GitHub Actions 工作流](.github/workflows/test.yml) 在 push、pull request 和手动触发时运行，使用 `go.mod` 指定的工具链，执行依赖校验、`go vet`、完整测试和覆盖率上传。

| 原生目标 | Runner | 普通测试 / vet | race |
| --- | --- | --- | --- |
| Windows amd64 | `windows-2025` | 是 | 是 |
| Windows arm64 | `windows-11-arm` | 是 | 按配置跳过 |
| Linux amd64 | `ubuntu-24.04` | 是 | 是 |
| Linux arm64 | `ubuntu-24.04-arm` | 是 | 是 |
| macOS amd64 | `macos-15-intel` | 是 | 是 |
| macOS arm64 | `macos-15` | 是 | 是 |

截至 2026-10-28，以上六项均已[原生运行通过](https://github.com/winezer0/processrunner/actions/runs/40651935431)。Windows/arm64 未启用 race。覆盖率文件作为每个平台独立 artifact 保留 14 天。

本地验证：

```sh
go test -count=1 -timeout=5m -cover ./...
go test -race -count=1 -timeout=5m ./...
go vet ./...
```

race 命令需在工具链支持的目标及具备相应 CGO 编译环境时运行。测试覆盖同步/异步执行、并发等待和停止、状态副本、输出截断/脱敏、大量双路输出、Writer 错误/短写、取消/超时状态优先级、最终元数据失败、日志访问边界、后代持有输出管道，以及 Windows/Linux 宿主强制退出后的清理。

## 许可证

[Apache License 2.0](LICENSE)。
