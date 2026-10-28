package processrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRelativeDirectory applies a caller-relative directory exactly once, including through Linux guardians.
func TestRelativeDirectory(t *testing.T) {
	// Hosted Windows runners may put the checkout and temp files on different
	// drives. Use a temp cwd so the relative-path fixture stays on one volume.
	t.Chdir(t.TempDir())
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	relative, err := filepath.Rel(working, directory)
	if err != nil {
		t.Fatal(err)
	}
	invocation := helperInvocation("cwd")
	invocation.Directory = relative
	result, err := Run(context.Background(), invocation)
	if err != nil {
		t.Fatalf("cwd=%q error=%v", result.Output, err)
	}
	// macOS may resolve /var to /private/var. Compare directory identity,
	// preserving the check that a relative working directory is applied once.
	want, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(string(result.Output))
	if err != nil || !os.SameFile(want, got) {
		t.Fatalf("cwd=%q want=%q error=%v", result.Output, directory, err)
	}
}

// TestOutputMetadataThrottle keeps frequent output from indefinitely postponing snapshot writes.
func TestOutputMetadataThrottle(t *testing.T) {
	state := &execution{captureLimit: 4096, lastSave: time.Now().Add(-2 * time.Second)}
	logs, err := openLogs(WithLogDirectory(context.Background(), t.TempDir()), 4096, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.close()
	if _, err := logs.stdout.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	firstSave := state.lastSave
	if _, err := logs.stdout.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if state.lastSave != firstSave {
		t.Fatal("frequent output postponed next metadata publication")
	}
}

// TestReadLogsRejectsBrokenAndEscapingEntries validates metadata and rooted access boundaries.
func TestReadLogsRejectsBrokenAndEscapingEntries(t *testing.T) {
	t.Run("malformed metadata", func(t *testing.T) {
		directory := t.TempDir()
		entry := filepath.Join(directory, "process-invalid")
		if err := os.Mkdir(entry, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(entry, "process.json"), []byte("invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadLogs(directory, 1024); err == nil {
			t.Fatal("malformed metadata accepted")
		}
	})
	t.Run("escaping symlink", func(t *testing.T) {
		directory, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(directory, "process-escape")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := ReadLogs(directory, 1024); err == nil {
			t.Fatal("escaping symlink accepted")
		}
	})
}
