package processrunner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestRawWritersRemainComplete checks independent streams despite capture and disk caps.
func TestRawWritersRemainComplete(t *testing.T) {
	for _, mode := range []string{"output", "large"} {
		t.Run(mode, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			invocation := helperInvocation(mode)
			invocation.Stdout, invocation.Stderr = &stdout, &stderr
			invocation.CaptureBytes, invocation.LogBytes = 4, 8
			result, err := Run(WithLogDirectory(t.Context(), t.TempDir()), invocation)
			if err != nil {
				t.Fatal(err)
			}
			wantOut, wantErr := "hello stdout", "hello stderr"
			if mode == "large" {
				wantOut, wantErr = strings.Repeat("a", 4096), ""
			}
			if stdout.String() != wantOut || stderr.String() != wantErr {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if !result.Truncated || len(result.Output) != 4 {
				t.Fatalf("capture = %#v", result)
			}
			// Writer objects must never enter the Linux guardian invocation payload.
			payload, err := json.Marshal(invocation)
			if err != nil || bytes.Contains(payload, []byte("Stdout")) || bytes.Contains(payload, []byte("Stderr")) {
				t.Fatalf("payload = %s, %v", payload, err)
			}
		})
	}
}

type failingOutputWriter struct{ err error }

// TestRawWriterDoesNotChangeRedaction checks the separate policies for sinks and capture.
func TestRawWriterDoesNotChangeRedaction(t *testing.T) {
	var stdout bytes.Buffer
	invocation := helperInvocation("output")
	invocation.Stdout = &stdout
	invocation.Environment = append(invocation.Environment, "TEST_TOKEN=hello stdout")
	result, err := Run(t.Context(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello stdout" || bytes.Contains(result.Output, []byte("hello stdout")) || !bytes.Contains(result.Output, []byte("[REDACTED]")) {
		t.Fatalf("raw=%q capture=%q", stdout.String(), result.Output)
	}
}

// Write simulates sink failure or a short write without an error.
func (writer failingOutputWriter) Write([]byte) (int, error) { return 0, writer.err }

// TestWriterFailuresPropagate checks that output failures cannot become successful executions.
func TestWriterFailuresPropagate(t *testing.T) {
	failure := errors.New("sink failed")
	for _, test := range []struct {
		name      string
		err, want error
	}{
		{"failure", failure, failure}, {"short write", nil, io.ErrShortWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			invocation := helperInvocation("large")
			invocation.Stdout = failingOutputWriter{err: test.err}
			result, err := Run(t.Context(), invocation)
			if !errors.Is(err, test.want) || result.State != StateFailed {
				t.Fatalf("result=%#v error=%v", result, err)
			}
		})
	}
}
