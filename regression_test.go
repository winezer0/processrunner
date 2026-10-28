package processrunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const regressionHelperEnvironment = "PROCESSRUNNER_REGRESSION_HELPER"

// TestRegressionProcessHelper supplies bounded subprocesses without requiring a shell.
func TestRegressionProcessHelper(t *testing.T) {
	mode := os.Getenv(regressionHelperEnvironment)
	if mode == "" {
		return
	}
	switch mode {
	case "exit":
	case "version-large":
		if _, err := fmt.Fprint(os.Stdout, strings.Repeat("a", 8192)); err != nil {
			os.Exit(10)
		}
	case "record-start":
		path := os.Getenv("PROCESSRUNNER_REGRESSION_HEARTBEAT")
		if err := os.WriteFile(path+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(11)
		}
	case "large-both":
		for range 32 {
			if _, err := fmt.Fprint(os.Stdout, strings.Repeat("x", 32<<10)); err != nil {
				os.Exit(2)
			}
			if _, err := fmt.Fprint(os.Stderr, strings.Repeat("y", 32<<10)); err != nil {
				os.Exit(3)
			}
		}
	case "output-and-wait":
		if _, err := fmt.Fprint(os.Stdout, strings.Repeat("x", 8192)); err != nil {
			os.Exit(4)
		}
		time.Sleep(30 * time.Second)
	case "hold-pipes":
		path := os.Getenv("PROCESSRUNNER_REGRESSION_HEARTBEAT")
		if err := os.WriteFile(path+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(5)
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if err := os.WriteFile(path, []byte(time.Now().String()), 0o600); err != nil {
				os.Exit(6)
			}
			time.Sleep(30 * time.Millisecond)
		}
	case "spawn-and-exit":
		child := exec.Command(os.Args[0], "-test.run=^TestRegressionProcessHelper$")
		child.Env = append(os.Environ(), regressionHelperEnvironment+"=hold-pipes")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(7)
		}
		// Keep the parent alive until the descendant demonstrably holds the pipes.
		path := os.Getenv("PROCESSRUNNER_REGRESSION_HEARTBEAT")
		deadline := time.Now().Add(10 * time.Second)
		for {
			if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
				break
			}
			if time.Now().After(deadline) {
				_ = child.Process.Kill()
				_ = child.Wait()
				os.Exit(8)
			}
			time.Sleep(10 * time.Millisecond)
		}
	default:
		os.Exit(9)
	}
	// Do not emit the testing package's summary into the captured command output.
	os.Exit(0)
}

func regressionInvocation(mode string) Invocation {
	return Invocation{
		Name:        "regression-" + mode,
		Binary:      os.Args[0],
		Args:        []string{"-test.run=^TestRegressionProcessHelper$"},
		Environment: []string{regressionHelperEnvironment + "=" + mode},
	}
}

func waitForRegressionOutput(t *testing.T, handle *Handle, minimum int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if handle.Snapshot().OutputBytes >= minimum {
			return
		}
		select {
		case <-handle.Done():
			result, err := handle.Wait(context.Background())
			t.Fatalf("helper completed before producing output: state=%s bytes=%d error=%v", result.State, result.OutputBytes, err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not produce the expected output")
}

// TestRegressionCancellationPreservesStateWithOutputLimit checks error precedence.
func TestRegressionCancellationPreservesStateWithOutputLimit(t *testing.T) {
	for _, test := range []struct {
		name  string
		state string
		cause error
	}{
		{"cancel", StateCanceled, context.Canceled},
		{"deadline", StateTimedOut, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if test.cause == context.DeadlineExceeded {
				ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
			} else {
				ctx, cancel = context.WithCancel(t.Context())
			}
			defer cancel()
			invocation := regressionInvocation("output-and-wait")
			invocation.CaptureBytes = 64
			invocation.RequireCompleteOutput = true
			handle, err := Start(ctx, invocation)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				handle.Stop()
				waitCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
				defer stop()
				if _, err := handle.Wait(waitCtx); errors.Is(err, context.DeadlineExceeded) && handle.Snapshot().FinishedAt == nil {
					t.Error("helper did not finish during cleanup")
				}
			}()
			waitForRegressionOutput(t, handle, 8192)
			if test.cause == context.Canceled {
				cancel()
			}
			waitCtx, stop := context.WithTimeout(t.Context(), 15*time.Second)
			defer stop()
			result, err := handle.Wait(waitCtx)
			if !errors.Is(err, test.cause) || !errors.Is(err, ErrOutputLimit) {
				t.Errorf("want both %v and ErrOutputLimit, got %v", test.cause, err)
			}
			if !result.Truncated || len(result.Output) != 64 || result.FinishedAt == nil {
				t.Errorf("incomplete final result: state=%s captured=%d bytes=%d truncated=%v", result.State, len(result.Output), result.OutputBytes, result.Truncated)
			}
			if result.State != test.state {
				t.Errorf("output limit replaced terminal state: got %s, want %s", result.State, test.state)
			}
		})
	}
}

// TestRegressionFinalMetadataFailureMarksExecutionFailed isolates the final save:
// periodic saves are suppressed so the injected error occurs only in logs.close.
func TestRegressionFinalMetadataFailureMarksExecutionFailed(t *testing.T) {
	invocation := regressionInvocation("exit")
	command := exec.Command(invocation.Binary, invocation.Args...)
	command.Env = append(os.Environ(), invocation.Environment...)
	command.WaitDelay = 2 * time.Second
	tree, err := newProcessTree(command, invocation)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		tree.close()
		t.Fatal(err)
	}
	if err := tree.attach(command); err != nil {
		_ = tree.forceTerminate(command)
		_ = command.Wait()
		tree.close()
		t.Fatal(err)
	}
	state := &execution{
		snapshot:     Snapshot{State: StateRunning, PID: tree.processPID(command), StartedAt: time.Now()},
		captureLimit: 1024,
		metadataPath: filepath.Join(t.TempDir(), "missing-parent", "process.json"),
		lastSave:     time.Now().Add(time.Hour),
	}
	logs := &outputLogs{stdout: &outputStream{state: state}, stderr: &outputStream{state: state}}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	handle := &Handle{state: state, cancel: cancel, done: make(chan struct{})}
	go handle.waitProcess(ctx, command, tree, logs, invocation)
	// The wait must not share the execution context: waitProcess cancels the
	// execution context before publishing Done as part of normal finalization.
	waitCtx, stopWaiting := context.WithTimeout(t.Context(), 20*time.Second)
	defer stopWaiting()
	result, err := handle.Wait(waitCtx)
	// Ensure the owner goroutine finishes even if the test's wait context expires.
	if result.FinishedAt == nil {
		cancel()
		cleanupCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, _ = handle.Wait(cleanupCtx)
		t.Fatalf("execution did not complete: %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want injected final metadata error, got %v", err)
	}
	if result.ExitCode != 0 || result.State != StateFailed {
		t.Errorf("successful command with failed final log save: exit=%d state=%s; want exit=0 state=failed", result.ExitCode, result.State)
	}
	select {
	case <-handle.Done():
	default:
		t.Error("Wait returned before Done closed")
	}
}

// TestRegressionSuccessfulParentCleansInheritedPipes expects normal parent exit
// to clean up descendants without manufacturing an output-drain failure.
func TestRegressionSuccessfulParentCleansInheritedPipes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	t.Cleanup(func() {
		// Fallback cleanup if the implementation fails to terminate the descendant.
		data, err := os.ReadFile(path + ".pid")
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(string(data))
		if err != nil {
			return
		}
		// Only kill if the heartbeat is still advancing, avoiding stale PID cleanup.
		before, _ := os.ReadFile(path)
		time.Sleep(100 * time.Millisecond)
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			if process, err := os.FindProcess(pid); err == nil {
				_ = process.Kill()
				_ = process.Release()
			}
		}
	})
	invocation := regressionInvocation("spawn-and-exit")
	invocation.Environment = append(invocation.Environment, "PROCESSRUNNER_REGRESSION_HEARTBEAT="+path)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	result, err := Run(ctx, invocation)
	if errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("descendant-held pipes caused ErrWaitDelay after normal parent exit: %v", err)
	}
	if err != nil || result.State != StateSuccess || result.ExitCode != 0 {
		t.Errorf("normal parent exit: state=%s exit=%d error=%v", result.State, result.ExitCode, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("descendant did not establish its heartbeat: %v", err)
	}
	assertHeartbeatStopped(t, path)
}

// TestRegressionLargeBothStreamsDrain verifies sustained output cannot deadlock
// either stream after both memory and disk limits have been reached.
func TestRegressionLargeBothStreamsDrain(t *testing.T) {
	directory := t.TempDir()
	invocation := regressionInvocation("large-both")
	invocation.CaptureBytes, invocation.LogBytes = 128, 96
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	result, err := Run(WithLogDirectory(ctx, directory), invocation)
	if err != nil || result.State != StateSuccess || result.OutputBytes != 2<<20 || len(result.Output) != 128 || !result.Truncated {
		t.Fatalf("drain result: state=%s bytes=%d captured=%d truncated=%v error=%v", result.State, result.OutputBytes, len(result.Output), result.Truncated, err)
	}
	entries, err := ReadLogs(directory, 65536)
	if err != nil || len(entries) != 1 {
		t.Fatalf("logs: entries=%d error=%v", len(entries), err)
	}
	if entries[0].Stdout != strings.Repeat("x", 96) || entries[0].Stderr != strings.Repeat("y", 96) {
		t.Errorf("disk stream bounds: stdout=%d stderr=%d", len(entries[0].Stdout), len(entries[0].Stderr))
	}
}

// TestRegressionDiskOnlyTruncationAllowsCompleteCapture separates disk caps
// from the RequireCompleteOutput contract for in-memory output.
func TestRegressionDiskOnlyTruncationAllowsCompleteCapture(t *testing.T) {
	invocation := helperInvocation("large")
	invocation.CaptureBytes, invocation.LogBytes = 8192, 16
	invocation.RequireCompleteOutput = true
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := Run(WithLogDirectory(ctx, t.TempDir()), invocation)
	if err != nil || result.State != StateSuccess || !result.Truncated || string(result.Output) != strings.Repeat("a", 4096) {
		t.Errorf("disk cap must not become ErrOutputLimit: state=%s captured=%d truncated=%v error=%v", result.State, len(result.Output), result.Truncated, err)
	}
}
