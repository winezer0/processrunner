# processrunner

[中文](README.md) | [English](README.en.md)

A general-purpose Go package for synchronous execution, asynchronous status queries, cancellation and timeouts, process-tree cleanup, bounded output capture, and optional disk logs.

Module: `github.com/winezer0/processrunner`; Go ≥ 1.25.5. Depends only on the standard library and `golang.org/x/sys`, with no dependency on application databases, task queues, or web frameworks.

## Integration

No remote version has been released yet. For local development, add the following to the consuming project's `go.mod`:

```go
require github.com/winezer0/processrunner v0.0.0

replace github.com/winezer0/processrunner => ../processrunner
```

Once a version is released, use that version number and remove the local `replace` directive.

### Synchronous execution

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

Arguments are passed as a slice without shell interpolation. Startup failures return an error and an empty result; execution failures after startup preserve the exit code and bounded output. Use `errors.Is` to check context errors and `ErrOutputLimit`, and `errors.As` to inspect `*exec.ExitError`.

### Asynchronous execution

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

- `Start` returns after the process has started and entered its managed process tree. Each execution has an independent Handle.
- `Snapshot`, `Stop`, `Wait`, and `Done` support concurrent use; results and snapshots are independent copies.
- `Stop` may be called repeatedly. Call `Wait` afterward to wait for process exit, output draining, and log closure.
- The context passed to `Wait(ctx)` controls only the wait and does not stop the process. The context passed to `Start(ctx)` controls the execution lifecycle.
- `Wait` may be called repeatedly after completion. A completed result takes priority even if the wait context is already canceled.
- States: `starting`, `running`, `success`, `failed`, `canceled`, and `timed_out`. Startup failures are reported through errors and optional disk logs; no Handle is returned.
- Use the `Runner` or `AsyncRunner` interface for dependency injection. The default `ProcessRunner{}` implementation has no global process registry and does not retain completed tasks.

## Output and logs

Optional `Invocation.Stdout` and `Invocation.Stderr` writers receive raw, uncapped output before redaction. Callers own these writers until process cleanup finishes (`Done` closes or `Run`/`Wait` returns); shared writers must support concurrent writes from both streams. Writer failures, including short writes, fail execution. Writers are excluded from the Linux guardian JSON and do not change bounded capture or redacted disk logs. Callers control raw output retention, access, and sensitive data handling.

By default, combined output capture is limited to 1 MiB, and each disk output stream is limited to 8 MiB. Set positive limits through `CaptureBytes` and `LogBytes`. Pipes continue to drain after limits are reached, and `Truncated` indicates truncation.

When `RequireCompleteOutput` is enabled, truncated in-memory output returns `ErrOutputLimit` and marks execution as failed. This is useful when parsing a complete command response is required.

```go
ctx = processrunner.WithLogDirectory(ctx, "./private-process-logs")
result, err := processrunner.Run(ctx, invocation)
entries, readErr := processrunner.ReadLogs("./private-process-logs", 16<<10)
```

Each execution creates a separate directory containing `stdout.log`, `stderr.log`, and `process.json`. Metadata does not record arguments or environment variables. The read API returns at most 64 entries per call, reads at most 64 KiB from the tail of each stream, and rejects symbolic links that escape the log root.

Nonempty values of explicitly supplied environment variables whose names contain TOKEN, PASSWORD, or SECRET are redacted from output, including values spanning write boundaries. This does not replace the caller's handling of sensitive information and does not automatically identify all credentials or sensitive values in the inherited environment. The caller is responsible for trusting the log directory, log retention, and access permissions; Windows file access also depends on directory ACLs.

`CheckTools` runs tool version checks in parallel with a context timeout of up to three seconds per check. It is intended for read-only version commands, not for starting long-running services.

## Platforms and unexpected exits

| Platform | Cancellation and descendant cleanup | Unexpected exit of the owning application |
| --- | --- | --- |
| Windows | Job Object; the process is assigned to the Job before execution resumes, and cancellation terminates the Job | Processes in the Job terminate automatically when its last handle closes |
| Linux | Separate process group and guardian; SIGTERM first, then SIGKILL if necessary | The guardian cleans up the process group when the ownership pipe disconnects |
| Other Unix systems, such as macOS | Terminates the process group | No automatic cleanup guarantee currently |

The Linux guardian reuses the caller's executable with the reserved argument `--processrunner-internal-guardian` and the internal environment variable `PROCESSRUNNER_INTERNAL_INVOCATION`. Package initialization handles the entry point automatically, so no separate executable deployment is required. The working directory is applied only once, to the actual command.

Process groups cannot constrain descendants that deliberately leave the managed process group, for example by calling setsid, and should not be treated as a sandbox for malicious programs. CPU and memory quotas, pause/resume, automatic restarts, persistent scheduling, and task recovery after service restarts are the responsibility of the calling application. If the owning application exits abruptly, disk metadata may remain in the running state; the calling application needs a task lease mechanism to detect interruption.

## Validation

```powershell
rtk go test ./... -cover
rtk go test -race ./...
rtk go vet ./...
```

Tests cover synchronous and asynchronous execution, concurrent waiting and stopping, snapshot copies, output truncation and redaction, log access boundaries, process-tree cancellation, and child-process cleanup after the owning application is forcibly terminated on Windows and Linux.
