//go:build linux

package processrunner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	guardianArgument           = "--processrunner-internal-guardian"
	guardianPayloadEnvironment = "PROCESSRUNNER_INTERNAL_INVOCATION"
	guardianGracePeriod        = 2 * time.Second
)

type guardianStatus struct {
	ProcessGroupID int    `json:"process_group_id"`
	Error          string `json:"error,omitempty"`
}

// guardianEnvironment excludes the private invocation payload from child environments.
func guardianEnvironment(overrides []string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, guardianPayloadEnvironment+"=") {
			environment = append(environment, entry)
		}
	}
	return append(environment, overrides...)
}

type guardianPipes struct {
	control *os.File
	status  *os.File
	stdout  *os.File
	stderr  *os.File
}

// init dispatches the private guardian entry before the service or test main starts.
func init() {
	if len(os.Args) > 1 && os.Args[1] == guardianArgument {
		os.Exit(runProcessGuardian())
	}
}

// runProcessGuardian owns the process group until it exits or its parent disappears.
func runProcessGuardian() int {
	control := os.NewFile(3, "guardian-control")
	status := os.NewFile(4, "guardian-status")
	defer control.Close()
	defer status.Close()
	payload, err := base64.StdEncoding.DecodeString(os.Getenv(guardianPayloadEnvironment))
	if err != nil {
		writeGuardianStatus(status, guardianStatus{Error: "decode guardian invocation: " + err.Error()})
		return 1
	}
	var invocation Invocation
	if err := json.Unmarshal(payload, &invocation); err != nil {
		writeGuardianStatus(status, guardianStatus{Error: "parse guardian invocation: " + err.Error()})
		return 1
	}
	return executeGuardian(invocation, guardianPipes{control: control, status: status, stdout: os.Stdout, stderr: os.Stderr})
}

// executeGuardian starts and manages one process group on a stable parent thread.
func executeGuardian(invocation Invocation, pipes guardianPipes) int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	command := exec.Command(invocation.Binary, invocation.Args...)
	command.Dir = invocation.Directory
	command.Env = guardianEnvironment(invocation.Environment)
	command.Stdout = pipes.stdout
	command.Stderr = pipes.stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := command.Start(); err != nil {
		writeGuardianStatus(pipes.status, guardianStatus{Error: "start command process: " + err.Error()})
		return 1
	}
	groupID := command.Process.Pid
	if err := json.NewEncoder(pipes.status).Encode(guardianStatus{ProcessGroupID: groupID}); err != nil {
		_ = killProcessGroup(groupID, syscall.SIGKILL)
		_ = command.Wait()
		return 1
	}
	_ = pipes.status.Close()
	parentGone := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(io.Discard, pipes.control)
		parentGone <- struct{}{}
	}()
	processExited := make(chan error, 1)
	go func() {
		var info unix.Siginfo
		for {
			err := unix.Waitid(unix.P_PID, groupID, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			processExited <- err
			return
		}
	}()
	select {
	case <-parentGone:
		_ = killProcessGroup(groupID, syscall.SIGTERM)
		select {
		case <-processExited:
		case <-time.After(guardianGracePeriod):
		}
		_ = killProcessGroup(groupID, syscall.SIGKILL)
		_ = command.Wait()
		return 1
	case waitErr := <-processExited:
		// WNOWAIT keeps the group leader's PID reserved until descendants are killed.
		_ = killProcessGroup(groupID, syscall.SIGKILL)
		exitErr := command.Wait()
		if waitErr != nil {
			return 1
		}
		return guardianExitCode(exitErr)
	}
}

// writeGuardianStatus reports startup success or failure to the owner process.
func writeGuardianStatus(file *os.File, status guardianStatus) {
	_ = json.NewEncoder(file).Encode(status)
}

// guardianExitCode preserves ordinary exit codes and terminating signals through the guardian.
func guardianExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			sig := status.Signal()
			signal.Reset(sig)
			if err := syscall.Kill(os.Getpid(), sig); err == nil {
				// Allow signal delivery before the guardian's normal exit path can run.
				for {
					time.Sleep(time.Hour)
				}
			}
		}
	}
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 && exitErr.ExitCode() < 256 {
		return exitErr.ExitCode()
	}
	return 1
}
