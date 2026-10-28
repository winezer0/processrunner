//go:build unix

package processrunner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// prepareExecutable adds only owner execute permission, preserving other mode bits.
func prepareExecutable(binary, directory string) (string, error) {
	path := binary
	searchPath := !strings.ContainsRune(binary, '/')
	if searchPath {
		resolved, err := exec.LookPath(binary)
		if err == nil {
			path = resolved
		} else {
			if !errors.Is(err, os.ErrPermission) && !errors.Is(err, exec.ErrNotFound) {
				return "", err
			}
			path = ""
			for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
				candidate := filepath.Join(entry, binary)
				info, statErr := os.Stat(candidate)
				if statErr == nil && info.Mode().IsRegular() {
					path = candidate
					break
				}
			}
			if path == "" {
				return "", err
			}
			// Preserve os/exec's refusal to implicitly execute from the current directory.
			if !filepath.IsAbs(path) {
				return "", &exec.Error{Name: binary, Err: exec.ErrDot}
			}
		}
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(directory, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("prepare executable %q: not a regular file", path)
	}
	if info.Mode()&0100 == 0 {
		if err := os.Chmod(path, info.Mode()|0100); err != nil {
			return "", fmt.Errorf("add owner execute permission: %w", err)
		}
	}
	return path, nil
}
