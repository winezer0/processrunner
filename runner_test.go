package processrunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestProcessHelper runs only inside subprocesses explicitly created by these tests.
func TestProcessHelper(t *testing.T) {
	mode := os.Getenv("PROCESSRUNNER_TEST_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "output":
		fmt.Fprint(os.Stdout, "hello stdout")
		fmt.Fprint(os.Stderr, "hello stderr")
	case "large":
		fmt.Fprint(os.Stdout, strings.Repeat("a", 4096))
	case "failed":
		os.Exit(7)
	case "cwd":
		directory, err := os.Getwd()
		if err != nil {
			os.Exit(8)
		}
		fmt.Fprint(os.Stdout, directory)
	case "heartbeat":
		for {
			_ = os.WriteFile(os.Getenv("PROCESSRUNNER_TEST_HEARTBEAT"), []byte(time.Now().String()), 0o600)
			time.Sleep(30 * time.Millisecond)
		}
	case "spawn":
		child := exec.Command(os.Args[0], "-test.run=TestProcessHelper$")
		child.Env = append(os.Environ(), "PROCESSRUNNER_TEST_HELPER=heartbeat")
		if err := child.Start(); err != nil {
			os.Exit(9)
		}
		for {
			time.Sleep(time.Second)
		}
	case "owner":
		_, _ = (ProcessRunner{}).Run(context.Background(), helperInvocation("spawn"))
	}
	os.Exit(0)
}

// helperInvocation builds a direct invocation of the test binary without a shell.
func helperInvocation(mode string) Invocation {
	return Invocation{Name: "test-helper", Binary: os.Args[0], Args: []string{"-test.run=TestProcessHelper$"}, Environment: []string{"PROCESSRUNNER_TEST_HELPER=" + mode}}
}

// TestRunRecordsBoundedLogs exercises completion, exit codes, capture caps, and file caps.
func TestRunRecordsBoundedLogs(t *testing.T) {
	for _, test := range []struct {
		mode  string
		want  string
		code  int
		limit bool
	}{
		{mode: "output", want: "success", code: 0}, {mode: "failed", want: "failed", code: 7}, {mode: "large", want: "success", code: 0, limit: true},
	} {
		t.Run(test.mode, func(t *testing.T) {
			dir := t.TempDir()
			invocation := helperInvocation(test.mode)
			invocation.CaptureBytes, invocation.LogBytes = 64, 128
			result, err := (ProcessRunner{}).Run(WithLogDirectory(context.Background(), dir), invocation)
			if (err != nil) != (test.code != 0) || result.State != test.want || result.ExitCode != test.code {
				t.Fatalf("result=%#v error=%v", result, err)
			}
			if len(result.Output) > 64 || result.Truncated != test.limit {
				t.Fatalf("output bounds=%#v", result)
			}
			entries, err := ReadLogs(dir, 64)
			if err != nil || len(entries) != 1 {
				t.Fatalf("logs=%#v %v", entries, err)
			}
			if entries[0].PID <= 0 || entries[0].FinishedAt == nil || entries[0].State != test.want {
				t.Fatalf("snapshot=%#v", entries[0])
			}
			if test.mode == "output" && (entries[0].Stdout != "hello stdout" || entries[0].Stderr != "hello stderr") {
				t.Fatalf("streams=%#v", entries[0])
			}
		})
	}
	invocation := helperInvocation("large")
	invocation.RequireCompleteOutput = true
	invocation.CaptureBytes = 10
	if _, err := (ProcessRunner{}).Run(context.Background(), invocation); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("overflow=%v", err)
	}
}

// TestRunHandlesCancellationAndStartupFailures exercises pre-start and timed process failure paths.
func TestRunHandlesCancellationAndStartupFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (ProcessRunner{}).Run(ctx, helperInvocation("output")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := (ProcessRunner{}).Run(context.Background(), Invocation{}); err == nil {
		t.Fatal("empty binary accepted")
	}
	if _, err := (ProcessRunner{}).Run(context.Background(), Invocation{Binary: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing binary accepted")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := (ProcessRunner{}).Run(ctx, helperInvocation("spawn"))
	if !errors.Is(err, context.DeadlineExceeded) || result.State != "timed_out" {
		t.Fatalf("timeout=%#v,%v", result, err)
	}
}

// waitForHeartbeat waits for a child to demonstrate that it is running.
func waitForHeartbeat(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("child did not start")
}

// assertHeartbeatStopped checks that a terminated tree no longer writes its heartbeat.
func assertHeartbeatStopped(t *testing.T, path string) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("descendant process survived owner termination")
	}
}

// TestRunCleansDescendantsAndUnexpectedOwnerExit verifies operating-system lifetime ownership.
func TestRunCleansDescendantsAndUnexpectedOwnerExit(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "heartbeat")
		t.Setenv("PROCESSRUNNER_TEST_HEARTBEAT", path)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := (ProcessRunner{}).Run(ctx, helperInvocation("spawn")); done <- err }()
		waitForHeartbeat(t, path)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("cancellation hung")
		}
		assertHeartbeatStopped(t, path)
	})
	t.Run("owner killed", func(t *testing.T) {
		if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
			t.Skip("unexpected owner death cleanup is supported on Windows and Linux")
		}
		path := filepath.Join(t.TempDir(), "heartbeat")
		owner := exec.Command(os.Args[0], "-test.run=TestProcessHelper$")
		owner.Env = append(os.Environ(), "PROCESSRUNNER_TEST_HELPER=owner", "PROCESSRUNNER_TEST_HEARTBEAT="+path)
		if err := owner.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Process.Kill() }()
		waitForHeartbeat(t, path)
		if err := owner.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = owner.Wait()
		assertHeartbeatStopped(t, path)
	})
}

// TestOutputRedactionAndRootedReads checks secrets split between writes and invalid log paths.
func TestOutputRedactionAndRootedReads(t *testing.T) {
	state := &execution{captureLimit: 1024}
	logs, err := openLogs(context.Background(), 1024, state, []string{"AUTH_TOKEN=secret-token"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = logs.stdout.Write([]byte("hello sec"))
	_, _ = logs.stdout.Write([]byte("ret-token world"))
	if err := logs.close(); err != nil {
		t.Fatal(err)
	}
	if got := string(state.result().Output); got != "hello [REDACTED] world" {
		t.Fatalf("redaction=%q", got)
	}
	if _, err := ReadLogs(t.TempDir(), 0); err == nil {
		t.Fatal("invalid tail accepted")
	}
	if entries, err := ReadLogs(filepath.Join(t.TempDir(), "missing"), 10); err != nil || len(entries) != 0 {
		t.Fatalf("absent logs=%v,%v", entries, err)
	}
}
