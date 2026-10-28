# 实施记录

## Tanscan 输出适配

- `Invocation.Stdout/Stderr` 为可选调用方 Writer，仅接收原始输出且不负责关闭。两个流并发执行；共用 Writer 需自行同步。Writer 字段排除出 Linux guardian JSON，guardian 继续使用父进程输出管道。
- 内部受限捕获及脱敏日志保持原语义；原始 Writer 在此之前执行，故调用方可以独立维护完整追加日志与实时活动。短写入转换为 `io.ErrShortWrite`，I/O 错误通过执行结果返回。
- 新增完整原始输出、独立流、捕获与磁盘上限、JSON 排除及 Writer 失败测试；Tanscan 使用该接口消除重复进程管理实现。
- 接入回归同时修复生命周期问题：取消/超时终态优先于输出超限；最终日志保存失败同步反映到返回状态；Windows 独立等待内核进程退出并终止 Job 后代，避免后代持有管道制造 `ErrWaitDelay`；Linux guardian 对终止信号重新发信号给自身以保留 SIGTERM 的退出信息。
- 本轮 Windows/Linux 全量测试及竞态检测通过（非 `-short`），覆盖率分别为 86.3% / 72.4%。

## 2026-10-28 独立模块

- 从 TanSpec 的 `internal/processrun` 提取为根包 `processrunner`，模块地址 `github.com/winezer0/processrunner`。依赖方向为业务项目 → processrunner → 标准库/x/sys；不导入 TanSpec 或 TanScan。
- 同步 `Run` 和异步 `Start` 使用唯一执行路径。Handle 仅持有单次执行状态，提供 Snapshot、Wait、Stop 和 Done；不存在包级可变进程注册表。
- Start 同步完成启动和进程树绑定，失败时清理资源并返回错误。Wait 的 context 不改变执行生命周期；Stop 通过执行 context 请求取消，Wait/Done 确认清理完成。
- 状态和输出读取使用锁及副本；完成通道用于向多个等待方发布最终错误。工具检查、输出限制、日志脱敏和进程生命周期在同一权威实现中维护。
- 修复持续输出推迟元数据更新的问题。Linux 守护进程不重复应用相对工作目录，实际子进程不继承内部序列化调用载荷；正常退出后不再向可能已复用的进程组 ID 发送信号。
- Windows 使用 Job Object；Linux 使用所有权管道和守护进程；其他 Unix 仅提供进程组取消，明确不保证执行方崩溃后的自动清理。
- TanSpec 使用本地 replace 开发，移除内部重复执行层。远端仓库尚未创建或推送，版本尚未发布；本地 v0.0.0 仅用于 replace。

## 验证结果

- Windows 完整测试及竞态检测通过，语句覆盖率 85.0%。
- WSL Ubuntu/Linux 完整测试及竞态检测通过，语句覆盖率 72.3%；包含主进程强制退出、进程树取消及相对工作目录用例。
- TanSpec 完整后端测试通过：431 项、28 个包。此前临时 OSV 数据库已不存在，本轮未重跑需外部数据库的真实扫描 E2E。
- 静态检查通过；macOS/amd64 交叉构建通过，尚未在 macOS 运行测试。
