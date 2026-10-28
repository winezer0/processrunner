# processrunner

[中文](README.md) | [English](README.en.md)

通用 Go 进程执行包，提供同步执行、异步状态查询、取消与超时、进程树清理，以及受限输出和可选磁盘日志。

模块：`github.com/winezer0/processrunner`；Go ≥ 1.25.5。仅依赖标准库和 `golang.org/x/sys`，不依赖业务数据库、任务队列或 Web 框架。

## 接入

当前尚未发布远端版本。本地开发可在消费项目的 `go.mod` 中配置：

```go
require github.com/winezer0/processrunner v0.0.0

replace github.com/winezer0/processrunner => ../processrunner
```

发布版本后使用对应版本号并删除本地 `replace`。

### 同步执行

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

result, err := processrunner.Run(ctx, processrunner.Invocation{
    Name: "git-version",
    Binary: "git",
    Args: []string{"--version"},
    CaptureBytes: 64 << 10,
    RequireCompleteOutput: true,
})
// Handle err before consuming result.Output; inspect result.ExitCode and State.
```

参数通过数组传递，不经过 shell。启动失败时返回错误，结果为空；成功启动后的执行失败保留退出码和有界输出。错误支持 `errors.Is` 检查 context 错误、`ErrOutputLimit`，以及 `errors.As` 检查 `*exec.ExitError`。

### 异步执行

```go
handle, err := processrunner.Start(ctx, invocation)
if err != nil {
    return err
}
defer handle.Stop()

snapshot := handle.Snapshot() // PID, state, timestamps, output count
_ = snapshot
// handle.Stop() requests process-tree cancellation when needed.
result, err := handle.Wait(context.Background())
// <-handle.Done() can also be used to observe final cleanup completion.
```

- `Start` 在进程启动并进入受控进程树后返回。每次执行有独立的 Handle。
- `Snapshot`、`Stop`、`Wait`、`Done` 支持并发使用；结果和快照均为独立副本。
- `Stop` 可重复调用；停止后调用 `Wait`，等待进程退出、输出排空及日志关闭。
- `Wait(ctx)` 的 context 只控制等待，不会停止进程。`Start(ctx)` 的 context 控制执行生命周期。
- 已完成进程的 `Wait` 可重复调用，即使等待 context 已取消，也优先返回完成结果。
- 状态：`starting`、`running`、`success`、`failed`、`canceled`、`timed_out`。启动失败通过错误和可选磁盘日志报告，不返回 Handle。
- 可注入 `Runner` 或 `AsyncRunner` 接口；默认实现 `ProcessRunner{}` 无全局进程注册表，不保留已完成任务。

## 输出和日志

`Invocation.Stdout` 和 `Invocation.Stderr` 可接收未经截断、未经脱敏的原始输出，适合调用方自有日志和实时活动统计。Writer 由调用方持有，直到 `Done` 关闭或 `Run`/`Wait` 返回后才能关闭；两个流共用 Writer 时必须支持并发写入。写入错误（含短写入）会使执行失败。Writer 不进入 Linux 守护进程 JSON，也不改变库内捕获/磁盘日志的上限与脱敏规则；原始输出的存储、访问控制和敏感信息处理由调用方负责。

默认捕获最多 1 MiB 合并输出；每个磁盘输出流最多 8 MiB。通过 `CaptureBytes`、`LogBytes` 配置正数上限。超限后继续排空管道，`Truncated` 标记截断。

启用 `RequireCompleteOutput` 后，内存输出截断返回 `ErrOutputLimit` 并标记执行失败；适合需要解析完整命令响应的场景。

```go
ctx = processrunner.WithLogDirectory(ctx, "./private-process-logs")
result, err := processrunner.Run(ctx, invocation)
entries, readErr := processrunner.ReadLogs("./private-process-logs", 16<<10)
```

每次执行创建独立目录，包含 `stdout.log`、`stderr.log`、`process.json`。元数据不记录参数和环境变量；读取接口每次最多返回 64 条，每个流最多读取 64 KiB 尾部数据，并拒绝越出日志根目录的符号链接。

显式传入的环境变量名包含 TOKEN、PASSWORD、SECRET 时，其非空值会在输出中脱敏，包括跨写入边界的值。该机制不能替代调用方的敏感信息管理，不会自动识别所有凭证或继承环境中的敏感值。调用方负责日志目录可信性、保留期限和访问权限；Windows 文件访问权限还取决于目录 ACL。

`CheckTools` 提供并行、最多三秒的工具版本检查。它适合只读版本命令，不用于启动常驻服务。

## 平台与异常退出

| 平台 | 取消与后代清理 | 执行方主进程意外退出 |
| --- | --- | --- |
| Windows | Job Object，创建后先纳入 Job 再恢复执行；取消时终止 Job | 最后一个 Job 句柄关闭后自动终止其中进程 |
| Linux | 独立进程组和守护进程；先 SIGTERM，必要时 SIGKILL | 守护进程检测所有权管道断开后清理进程组 |
| 其他 Unix（如 macOS） | 终止进程组 | 当前无自动清理保证 |

Linux 守护进程复用调用方可执行文件，使用保留参数 `--processrunner-internal-guardian` 和内部环境变量 `PROCESSRUNNER_INTERNAL_INVOCATION`。入口由包初始化自动处理，调用方无需另行部署可执行文件。工作目录只应用到实际命令一次。

进程组机制无法约束主动调用 setsid 等方式脱离受控进程组的后代，不应作为恶意程序隔离沙箱。CPU/内存配额、暂停继续、自动重启、持久化调度和服务重启后的任务恢复由上层负责。主进程突然退出时，磁盘状态可能停留在 running，需要上层任务租约判定中断。

## 验证

```powershell
rtk go test ./... -cover
rtk go test -race ./...
rtk go vet ./...
```

测试涵盖同步与异步执行、并发等待与停止、快照副本、输出截断和脱敏、日志访问边界、进程树取消，以及 Windows/Linux 主进程被强制终止后的子进程清理。
