package processrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckTools checks stable ordering, bounded version output, and safe failures.
func TestCheckTools(t *testing.T) {
	for _, test := range []struct {
		mode             string
		ready            bool
		version, failure string
	}{
		{"output", true, "", ""},
		{"failed", false, "", "version check failed or timed out"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			t.Setenv("PROCESSRUNNER_TEST_HELPER", test.mode)
			results := CheckTools(context.Background(), []Tool{
				{Name: "helper", Binary: os.Args[0], VersionArgs: []string{"-test.run=TestProcessHelper$"}},
				{Name: "missing", Binary: filepath.Join(t.TempDir(), "missing")},
			})
			if len(results) != 2 || results[0].Name != "helper" || results[0].Ready != test.ready || results[0].Error != test.failure {
				t.Fatalf("health=%#v", results)
			}
			if test.ready && results[0].Version != "hello stdouthello stderr" && results[0].Version != "hello stderrhello stdout" {
				t.Fatalf("version=%q", results[0].Version)
			}
			if results[1] != (ToolHealth{Name: "missing", Error: "executable is missing or not executable"}) {
				t.Fatalf("missing=%#v", results[1])
			}
		})
	}
	if results := CheckTools(context.Background(), nil); len(results) != 0 {
		t.Fatal(results)
	}
}
