package processrunner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestHandleCancellationAndWaiters checks concurrent observers and independent wait cancellation.
func TestHandleCancellationAndWaiters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	invocation := helperInvocation("heartbeat")
	invocation.Environment = append(invocation.Environment, "PROCESSRUNNER_TEST_HEARTBEAT="+path)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	handle, err := Start(WithLogDirectory(ctx, t.TempDir()), invocation)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Stop()
	waitForHeartbeat(t, path)
	snapshot := handle.Snapshot()
	if snapshot.State != StateRunning || snapshot.PID <= 0 || snapshot.FinishedAt != nil {
		t.Fatalf("running snapshot=%#v", snapshot)
	}
	waitCtx, stopWaiting := context.WithCancel(context.Background())
	stopWaiting()
	if _, err := handle.Wait(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if handle.Snapshot().State != StateRunning {
		t.Fatal("canceling a waiter stopped the process")
	}
	select {
	case <-handle.Done():
		t.Fatal("process completed before Stop")
	default:
	}
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 10 {
				_ = handle.Snapshot()
			}
			handle.Stop()
			result, err := handle.Wait(ctx)
			if !errors.Is(err, context.Canceled) || result.State != StateCanceled || result.FinishedAt == nil {
				t.Errorf("canceled result=%#v error=%v", result, err)
			}
		}()
	}
	wait.Wait()
	select {
	case <-handle.Done():
	default:
		t.Fatal("Done not closed after Wait")
	}
	assertHeartbeatStopped(t, path)
}

// TestHandleResultCopies checks repeated waits and independent snapshot pointer fields.
func TestHandleResultCopies(t *testing.T) {
	handle, err := (ProcessRunner{}).Start(context.Background(), helperInvocation("output"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := handle.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := string(first.Output)
	first.Output[0] = '!'
	*first.FinishedAt = time.Time{}
	*first.LastOutputAt = time.Time{}
	snapshot := handle.Snapshot()
	if snapshot.FinishedAt.IsZero() || snapshot.LastOutputAt.IsZero() {
		t.Fatal("caller mutated stored snapshot")
	}
	*snapshot.FinishedAt = time.Time{}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	second, err := handle.Wait(waitCtx)
	if err != nil || string(second.Output) != want || second.FinishedAt.IsZero() {
		t.Fatalf("second wait=%#v error=%v", second, err)
	}
	handle.Stop()
	result, err := Run(context.Background(), helperInvocation("failed"))
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 7 {
		t.Fatalf("original exit error not preserved: %v", err)
	}
	if err == nil || result.State != StateFailed || result.ExitCode != 7 {
		t.Fatalf("failure=%#v,%v", result, err)
	}
}

// TestStartFailures checks validation, failed log creation, and terminal startup metadata.
func TestStartFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name       string
		ctx        context.Context
		invocation Invocation
	}{
		{"canceled", ctx, helperInvocation("output")},
		{"empty", context.Background(), Invocation{}},
		{"missing", context.Background(), Invocation{Binary: filepath.Join(t.TempDir(), "missing")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if handle, err := Start(test.ctx, test.invocation); err == nil || handle != nil {
				t.Fatalf("start=%v,%v", handle, err)
			}
		})
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(WithLogDirectory(context.Background(), file), helperInvocation("output")); err == nil {
		t.Fatal("file accepted as log directory")
	}
	directory := t.TempDir()
	if _, err := Start(WithLogDirectory(context.Background(), directory), Invocation{Binary: file}); err == nil {
		t.Fatal("invalid executable accepted")
	}
	entries, err := ReadLogs(directory, 1024)
	if err != nil || len(entries) != 1 || entries[0].State != StateFailed || entries[0].FinishedAt == nil {
		t.Fatalf("startup logs=%#v error=%v", entries, err)
	}
}

var _ AsyncRunner = ProcessRunner{}
