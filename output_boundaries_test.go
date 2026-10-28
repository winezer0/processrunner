package processrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRedactionAtEveryWriteBoundary checks every two-chunk split as well as
// one-byte writes; no chunking may expose a configured secret in either sink.
func TestRedactionAtEveryWriteBoundary(t *testing.T) {
	input := "prefix alpha-secret / short / alpha-secret suffix"
	want := "prefix [REDACTED] / [REDACTED] / [REDACTED] suffix"
	for split := 0; split <= len(input); split++ {
		t.Run(fmt.Sprintf("split-%d", split), func(t *testing.T) {
			checkRedactionChunks(t, []string{input[:split], input[split:]}, want)
		})
	}
	t.Run("one-byte-writes", func(t *testing.T) {
		chunks := make([]string, len(input))
		for index := range input {
			chunks[index] = input[index : index+1]
		}
		checkRedactionChunks(t, chunks, want)
	})
}

func checkRedactionChunks(t *testing.T, chunks []string, want string) {
	t.Helper()
	state := &execution{captureLimit: 4096}
	directory := t.TempDir()
	logs, err := openLogs(WithLogDirectory(t.Context(), directory), 4096, state, []string{"API_TOKEN=alpha-secret", "db_password=short"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if logs.stdout.file != nil || logs.stderr.file != nil {
			_ = logs.close()
		}
	})
	for _, chunk := range chunks {
		if n, err := logs.stdout.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("write: n=%d want=%d error=%v", n, len(chunk), err)
		}
	}
	state.finish(StateSuccess, 0)
	if err := logs.close(); err != nil {
		t.Fatal(err)
	}
	result := state.result()
	if string(result.Output) != want || result.OutputBytes != int64(len(want)) || result.Truncated {
		t.Errorf("redaction: output=%q bytes=%d truncated=%v", result.Output, result.OutputBytes, result.Truncated)
	}
	entries, err := ReadLogs(directory, 4096)
	if err != nil || len(entries) != 1 {
		t.Fatalf("logs: entries=%d error=%v", len(entries), err)
	}
	if entries[0].Stdout != want || entries[0].Stderr != "" {
		t.Errorf("disk redaction: stdout=%q stderr=%q", entries[0].Stdout, entries[0].Stderr)
	}
}

// TestReadLogsTailBoundaries checks both endpoints and exact tail selection.
func TestReadLogsTailBoundaries(t *testing.T) {
	directory := t.TempDir()
	state := &execution{captureLimit: 1 << 20}
	logs, err := openLogs(WithLogDirectory(t.Context(), directory), 1<<20, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Repeat("0123456789", 7000)
	if _, err := logs.stdout.Write([]byte(input)); err != nil {
		_ = logs.close()
		t.Fatal(err)
	}
	if err := logs.close(); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1, 65536} {
		entries, err := ReadLogs(directory, size)
		if err != nil || len(entries) != 1 {
			t.Fatalf("tail=%d entries=%d error=%v", size, len(entries), err)
		}
		if entries[0].Stdout != input[len(input)-size:] {
			t.Errorf("tail=%d did not return the exact last bytes", size)
		}
	}
	for _, size := range []int{-1, 0, 65537} {
		if _, err := ReadLogs(directory, size); err == nil {
			t.Errorf("accepted out-of-range tail=%d", size)
		}
	}
}

// TestCheckToolsCanceledContext verifies cancellation does not start new helpers
// and still produces one correctly ordered result per configured tool.
func TestCheckToolsCanceledContext(t *testing.T) {
	t.Setenv(regressionHelperEnvironment, "record-start")
	path := filepath.Join(t.TempDir(), "must-not-start")
	t.Setenv("PROCESSRUNNER_REGRESSION_HEARTBEAT", path)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tools := []Tool{
		{Name: "first", Binary: os.Args[0], VersionArgs: []string{"-test.run=^TestRegressionProcessHelper$"}},
		{Name: "second", Binary: os.Args[0], VersionArgs: []string{"-test.run=^TestRegressionProcessHelper$"}},
	}
	results := CheckTools(ctx, tools)
	if len(results) != len(tools) {
		t.Fatalf("results=%d want=%d", len(results), len(tools))
	}
	for index, result := range results {
		if result.Name != tools[index].Name || result.Ready || result.Version != "" || result.Error == "" {
			t.Errorf("canceled tool[%d]=%#v", index, result)
		}
	}
	if _, err := os.Stat(path + ".pid"); !os.IsNotExist(err) {
		t.Errorf("canceled health check started a helper: %v", err)
	}
}

// TestCheckToolsTruncatesVersionOutput verifies the health API's 4 KiB bound.
func TestCheckToolsTruncatesVersionOutput(t *testing.T) {
	t.Setenv(regressionHelperEnvironment, "version-large")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	results := CheckTools(ctx, []Tool{{Name: "large", Binary: os.Args[0], VersionArgs: []string{"-test.run=^TestRegressionProcessHelper$"}}})
	if len(results) != 1 || !results[0].Ready || results[0].Error != "" || results[0].Version != strings.Repeat("a", 4096) {
		t.Fatalf("health=%#v", results)
	}
}
