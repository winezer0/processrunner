package processrunner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// A sink failure must stop a command that keeps running after its first output.
func TestWriterFailureStopsRunningProcess(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "short-write"}[short], func(t *testing.T) {
			failure := errors.New("output sink failed")
			writer := failingOutputWriter{err: failure}
			want := failure
			if short {
				writer.err, want = nil, io.ErrShortWrite
			}
			invocation := regressionInvocation("output-and-wait")
			invocation.Stdout = writer
			handle, err := Start(t.Context(), invocation)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				handle.Stop()
				_, _ = handle.Wait(context.Background())
			})
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			result, err := handle.Wait(ctx)
			if !errors.Is(err, want) || result.State != StateFailed || result.FinishedAt == nil {
				t.Fatalf("state=%s error=%v; want completed sink failure %v", result.State, err, want)
			}
		})
	}
}

// Model descriptors retained outside the owned tree: draining must have a bound.
func TestOutputDrainTimeoutClosesRetainedPipes(t *testing.T) {
	output := &commandOutput{results: make(chan error, 2), failed: make(chan error, 2)}
	t.Cleanup(output.closeFiles)
	for i := range output.readers {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		output.readers[i], output.writers[i] = reader, writer
		go func() {
			_, err := io.Copy(io.Discard, reader)
			output.results <- err
		}()
	}
	if err := output.wait(20 * time.Millisecond); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("want drain timeout, got %v", err)
	}
	for _, writer := range output.writers {
		if _, err := writer.Write([]byte("closed")); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("pipe writer not closed: %v", err)
		}
	}
}
