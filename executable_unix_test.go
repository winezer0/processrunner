//go:build unix

package processrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStartRepairsExecutablePermission(t *testing.T) {
	for _, lookup := range []string{"absolute", "relative", "path"} {
		t.Run(lookup, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "tool")
			if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf repaired"), 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
			invocation := Invocation{Binary: path}
			switch lookup {
			case "relative":
				invocation.Binary, invocation.Directory = "./tool", directory
			case "path":
				t.Setenv("PATH", directory)
				invocation.Binary = "tool"
			}
			result, err := Run(context.Background(), invocation)
			if err != nil || string(result.Output) != "repaired" {
				t.Fatalf("run output=%q err=%v", result.Output, err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0740 {
				t.Fatalf("mode=%o, want 740", info.Mode().Perm())
			}
		})
	}
}

func TestCheckToolsDoesNotRepairPermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0010} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tool")
			if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf version"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			CheckTools(context.Background(), []Tool{{Binary: path}})
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != mode {
				t.Fatalf("health check changed mode to %o", info.Mode().Perm())
			}
		})
	}
}

func TestStartDoesNotChmodDirectory(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0700) })
	if _, err := Start(context.Background(), Invocation{Binary: path}); err == nil {
		t.Fatal("directory accepted as executable")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("directory permissions changed: info=%v err=%v", info, err)
	}
}
