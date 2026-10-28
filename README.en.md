# processrunner

[中文](README.md) | [English](README.en.md)

A Go package for synchronous execution, asynchronous status queries, cancellation and timeouts, descendant cleanup, bounded output capture, and optional disk logs.

Module: `github.com/winezer0/processrunner`; requires **Go 1.25.5 or later**. Depends on the standard library and `golang.org/x/sys`, without application framework dependencies. Native CI covers Windows, Linux, and macOS on amd64 and arm64.

## Installation

```sh
go get github.com/winezer0/processrunner@v0.0.1
```

For local development, add this to the consuming project's `go.mod`, adjusting the path:

```go
replace github.com/winezer0/processrunner => ../processrunner
```

## Quick start

This complete example requires `git` to be installed:

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

`Run` waits for process exit, descendant cleanup, output draining, and log closure. Arguments go directly to the executable without a shell; redirection, pipelines, and wildcards are not expanded. Callers needing shell behavior must explicitly choose a shell and handle input safely.

### Execution options

On Unix, `Start` and `Run` add owner execute permission (`u+x`) to the target regular file before starting it, preserving other permission bits. Permission failures are returned to the caller. This supports explicit paths (relative to `Directory`) and files found through `PATH`. If ordinary executable lookup fails, the first regular file in an absolute `PATH` entry is used; implicit relative `PATH` matches are rejected. Windows permissions are unchanged. `CheckTools` never repairs permissions and reports non-executable files as unavailable.

| `Invocation` field | Behavior |
| --- | --- |
| `Name` | Optional name recorded in snapshots and log metadata |
| `Binary` | Required executable name or path; names are resolved using `os/exec` rules |
| `Args` | Argument slice, excluding the executable name |
| `Directory` | Working directory; empty inherits the current directory, relative paths resolve against the owner's current directory |
| `Environment` | `KEY=value` entries appended to the inherited environment; later values override duplicate names |
| `CaptureBytes` | Combined memory capture limit; nonpositive values use `DefaultCaptureBytes` (1 MiB) |
| `LogBytes` | Limit per disk output stream; nonpositive values use `DefaultLogBytes` (8 MiB) |
| `RequireCompleteOutput` | Adds `ErrOutputLimit` when memory capture is incomplete; disk-only truncation does not trigger it |
| `Stdout` / `Stderr` | Optional raw output `io.Writer` sinks, independent of capture and disk limits |

`Start` copies argument and environment slices. There is no standard input option; command stdin is connected to the null device. This package is not intended for interactive terminal sessions.

## Asynchronous execution and state

This fragment assumes `ctx` and `invocation` already exist and is used inside a function returning `error`:

```go
handle, err := processrunner.Start(ctx, invocation)
if err != nil {
    return err
}
defer handle.Stop()

snapshot := handle.Snapshot()
_ = snapshot
// Call handle.Stop() when cancellation is needed.
result, err := handle.Wait(context.Background())
_ = result
return err
```

- `Start(ctx)` returns after startup and platform process-tree attachment. Startup failure returns no Handle; `Run` then returns an empty `Result` and an error.
- The `Start` context controls execution. The context passed to `Wait(ctx)` controls only that wait. If the wait context expires, it returns an empty `Result` and a context error while execution may continue.
- `Stop` is repeatable. Follow it with `Wait` or wait for `Done` to confirm cleanup. An execution deadline does not guarantee immediate return at that instant; finalization takes additional time.
- `Snapshot`, `Stop`, `Wait`, and `Done` support concurrent use. Results and snapshots are independent copies. After completion, repeated `Wait` calls return the final result even if their wait context is already canceled.
- `Done` closes after descendant cleanup, output draining, and log closure. The default `ProcessRunner{}` is safe for concurrent reuse and retains no completed tasks. Use `Runner` or `AsyncRunner` for dependency injection.

Snapshots contain `Name`, the actual command `PID`, `State`, `ExitCode`, UTC start/end timestamps, the last output timestamp, `OutputBytes`, and `Truncated`. `Result` adds combined `Output`.

States are `starting`, `running`, `success`, `failed`, `canceled`, and `timed_out`. Terminal precedence is timeout, cancellation, other failure, then success. Output-limit or cleanup errors do not replace cancellation/timeout semantics. Even with command exit code 0, output writing, cleanup, or final log closure/persistence errors can fail the overall execution. Check the error, state, and exit code together.

Errors may combine multiple causes. Use `errors.Is` for `context.Canceled`, `context.DeadlineExceeded`, `ErrOutputLimit`, `io.ErrShortWrite`, or `exec.ErrWaitDelay`; use `errors.As` for `*exec.ExitError` on ordinary execution paths. Cancellation primarily reports the context error and does not guarantee an accompanying exit error. `ExitCode` may be `-1` when the process did not exit normally.

## Output and logs

stdout and stderr are read separately and combined into bounded capture; strict chronological ordering between streams is not guaranteed. `OutputBytes` counts bytes after redaction and before memory truncation. `Truncated` indicates truncation in memory capture or either disk stream. Output continues to drain after limits are reached.

### Raw output writers

`Stdout` / `Stderr` receive raw bytes before redaction and without capture limits. Callers own storage, permissions, and sensitive data handling. The package does not close these writers or serialize them into the Linux guardian payload. Writers shared between streams must support concurrent writes.

Writer errors or short writes trigger process termination and fail execution; short writes become `io.ErrShortWrite`. `Write` must return promptly: closing pipes cannot interrupt a permanently blocked user callback.

Keep writers available until `Done` closes, `Run` returns, or `Wait` returns the final result. A `Wait` returning because its wait context was canceled does not mean execution is complete; do not close writers at that point.

Process exit and output draining are independent: managed descendants are cleaned up before waiting for both output streams to finish. Descendants retaining pipes after a successful parent exit therefore do not cause `ErrWaitDelay` merely because cleanup was ordered after draining. The current post-cleanup drain limit is 2 seconds; expiry closes pipes and adds `exec.ErrWaitDelay`, and output may be incomplete. This limit cannot interrupt a blocked writer.

### Disk logs

```go
ctx = processrunner.WithLogDirectory(ctx, "./private-process-logs")
result, err := processrunner.Run(ctx, invocation)
entries, readErr := processrunner.ReadLogs("./private-process-logs", 16<<10)
```

Without a log directory, only memory capture is enabled. With logging enabled, each execution creates a `process-*` directory containing `stdout.log`, `stderr.log`, and `process.json`. Metadata omits arguments and environment variables; output files can still contain sensitive information. Creation modes are `0700` for directories and `0600` for files; permissions on existing directories are not tightened automatically. Windows access also depends on directory ACLs.

Nonempty values from explicit `Environment` entries whose names contain `TOKEN`, `PASSWORD`, or `SECRET` (case insensitive) are redacted in internal capture and disk output, including matches spanning writes. Inherited credentials are not scanned, and raw writers are not redacted. This mechanism does not replace the caller's sensitive data handling.

`ReadLogs(directory, tailBytes)` returns `LogEntry` values containing `ID`, a snapshot, `Stdout`, and `Stderr`:

- `tailBytes` must be 1–65536; each stream returns at most that many trailing bytes. Byte-based tails can split UTF-8 characters.
- A missing directory returns an empty list. Reads are constrained to the log root and reject escaping symlinks. Corrupt or unreadable entries cause the call to return an error.
- Currently, only the first 64 directory entries are enumerated, then filtered by `process-*` and sorted by name. Other entries also consume that enumeration budget. Latest records, time ordering, and complete enumeration are not guaranteed; there is no pagination or cursor.
- Callers own directory trust, retention, and cleanup. Metadata may lag during execution, and reads are not transactional snapshots. Final persistence failure or abrupt owner exit can leave disk state inconsistent with the final in-memory state.

## Tool health checks

```go
health := processrunner.CheckTools(ctx, []processrunner.Tool{
    {Name: "git", Binary: "git", VersionArgs: []string{"--version"}},
})
```

Results preserve input order. Each `ToolHealth` contains `Name`, `Ready`, `Version`, and `Error`. Executables are located first, then version commands run through the same runner. Each execution context has a maximum 3-second timeout, subject to the caller's context; cleanup can increase elapsed time. Successful checks capture up to 4096 combined bytes and trim surrounding whitespace. Version-output truncation alone does not set `Ready` to false.

Each tool starts its own goroutine; there is currently no concurrency limit. Batch or limit large lists in the calling application. Checks are intended for read-only version commands, and `Ready` does not establish full application capability.

## Platform guarantees and limits

| Platform | Cancellation and descendant cleanup after normal exit | Unexpected owner exit |
| --- | --- | --- |
| Windows | Starts suspended, assigns to a Job Object, then resumes; terminates descendants in the managed Job | Closing the last Job handle triggers kill-on-close |
| Linux | Guardian manages a separate process group; cancellation sends SIGTERM then SIGKILL after a grace period; normal exit cleans remaining group members | Guardian cleans the group when the ownership pipe disconnects |
| macOS / other Unix | Separate process group; SIGKILL cleans group members on cancellation or normal exit | No automatic cleanup guarantee after owner failure |

The Linux guardian re-executes the owner's executable. Package initialization recognizes the first argument `--processrunner-internal-guardian`, dispatches the internal entry, and calls `os.Exit`. That argument and the `PROCESSRUNNER_INTERNAL_INVOCATION` environment variable are reserved. Re-execution also runs dependency initialization preceding the guardian entry, so account for initialization side effects. No separate guardian deployment is needed. Invocation data travels as JSON/base64 in an environment variable, adding to system argument/environment size limits for large payloads. The internal payload is removed before passing the environment to the actual command.

Process groups cannot constrain descendants deliberately leaving via `setsid` / `setpgid`. Owner-exit guarantees also depend on the guardian or Job mechanism functioning. This package does not provide a malicious-code sandbox, CPU/memory quotas, pause/resume, automatic restart, persistent scheduling, or recovery after service restart. Other Unix systems use the generic implementation but are outside the current native CI matrix. Platforms outside Windows/Unix are not claimed as supported.

## Tests and CI

The [GitHub Actions workflow](.github/workflows/test.yml) runs on pushes, pull requests, and manual dispatch. It uses the toolchain specified in `go.mod`, verifies dependencies, runs `go vet` and full tests, and uploads coverage.

| Native target | Runner | Tests / vet | Race |
| --- | --- | --- | --- |
| Windows amd64 | `windows-2025` | Yes | Yes |
| Windows arm64 | `windows-11-arm` | Yes | Skipped by configuration |
| Linux amd64 | `ubuntu-24.04` | Yes | Yes |
| Linux arm64 | `ubuntu-24.04-arm` | Yes | Yes |
| macOS amd64 | `macos-15-intel` | Yes | Yes |
| macOS arm64 | `macos-15` | Yes | Yes |

As of 2026-10-28, all six targets have [passed native execution](https://github.com/winezer0/processrunner/actions/runs/40651935431). Race testing is disabled on Windows/arm64. Per-platform coverage artifacts are retained for 14 days.

Local validation:

```sh
go test -count=1 -timeout=5m -cover ./...
go test -race -count=1 -timeout=5m ./...
go vet ./...
```

Run race tests only on supported toolchain targets with the required CGO compiler environment. Tests cover synchronous/asynchronous execution, concurrent waiting and stopping, snapshot copies, output truncation/redaction, large dual-stream output, writer errors/short writes, cancellation/timeout precedence, final metadata failures, log access boundaries, descendants retaining output pipes, and cleanup after forced owner termination on Windows/Linux.

## License

[Apache License 2.0](LICENSE).
