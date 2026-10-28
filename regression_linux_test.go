//go:build linux

package processrunner

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestRegressionSignalExitPreservesStatus requires the target's signal status,
// rather than a synthetic guardian exit code, to survive the execution boundary.
func TestRegressionSignalExitPreservesStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := Run(ctx, Invocation{Binary: "/bin/sh", Args: []string{"-c", "kill -TERM $$"}})
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("want signal ExitError, got %v", err)
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM || result.ExitCode != -1 {
		t.Errorf("target signal lost: state=%s exit=%d status=%v error=%v; want SIGTERM and exit=-1", result.State, result.ExitCode, status, err)
	}
}
