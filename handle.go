package processrunner

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// Handle owns one running process; its methods may be called concurrently.
// Handles must be obtained from Start rather than constructed directly.
type Handle struct {
	state  *execution
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Snapshot returns an independent copy of the current process metadata.
func (handle *Handle) Snapshot() Snapshot { return handle.state.snapshotCopy() }

// Stop requests cancellation of the process tree and is safe to call repeatedly.
// Call Wait to observe completion and release of process resources.
func (handle *Handle) Stop() { handle.cancel() }

// Done closes after process cleanup, output draining, and log closure have completed.
func (handle *Handle) Done() <-chan struct{} { return handle.done }

// Wait returns an independent final result and may be called by multiple waiters.
// Canceling the wait context does not stop the process; use Stop for that.
func (handle *Handle) Wait(ctx context.Context) (Result, error) {
	select {
	case <-handle.done:
		return handle.state.result(), handle.err
	default:
	}
	select {
	case <-handle.done:
		return handle.state.result(), handle.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// waitProcess finalizes a command before publishing its result to every waiter.
func (handle *Handle) waitProcess(ctx context.Context, command *exec.Cmd, tree *processTree, logs *outputLogs, invocation Invocation) {
	defer close(handle.done)
	defer handle.cancel()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var err error
	select {
	case err = <-done:
	case err = <-logs.pipes.failures():
		stopErr := tree.terminate(command)
		select {
		case waitErr := <-done:
			err = errors.Join(err, waitErr, stopErr)
		case <-time.After(processStopTimeout):
			err = errors.Join(err, stopErr, tree.forceTerminate(command), <-done)
		}
	case <-ctx.Done():
		stopErr := tree.terminate(command)
		select {
		case <-done:
		case <-time.After(processStopTimeout):
			stopErr = errors.Join(stopErr, tree.forceTerminate(command))
			<-done
		}
		err = errors.Join(ctx.Err(), stopErr)
	}
	// Linux guardian already cleaned the group before reaping its reserved PID.
	err = errors.Join(err, tree.cleanup(command))
	tree.close()
	err = errors.Join(err, logs.pipes.wait(outputDrainTimeout), logs.flush())
	// Preserve cancellation if it arrived while output or cleanup was finishing.
	err = errors.Join(err, ctx.Err())
	result := handle.state.result()
	if invocation.RequireCompleteOutput && len(result.Output) < int(result.OutputBytes) {
		err = errors.Join(err, ErrOutputLimit)
	}
	status := executionStatus(err)
	handle.state.finish(status, command.ProcessState.ExitCode())
	handle.err = errors.Join(err, logs.close())
	if finalStatus := executionStatus(handle.err); finalStatus != status {
		handle.state.finish(finalStatus, command.ProcessState.ExitCode())
	}
}

// executionStatus gives cancellation precedence over output and cleanup failures.
func executionStatus(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return StateTimedOut
	}
	if errors.Is(err, context.Canceled) {
		return StateCanceled
	}
	if err != nil {
		return StateFailed
	}
	return StateSuccess
}
