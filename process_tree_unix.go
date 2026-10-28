//go:build unix && !linux

package processrunner

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type processTree struct {
	processGroupID int
}

// newProcessTree creates a separate command group on non-Linux Unix systems.
func newProcessTree(command *exec.Cmd, _ Invocation) (*processTree, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processTree{}, nil
}

// attach records the owned group leader after startup.
func (tree *processTree) attach(command *exec.Cmd) error {
	tree.processGroupID = command.Process.Pid
	return nil
}

// terminate kills all processes that remain in the owned group.
func (tree *processTree) terminate(*exec.Cmd) error {
	if tree.processGroupID == 0 {
		return nil
	}
	err := syscall.Kill(-tree.processGroupID, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// forceTerminate sends SIGKILL to the group and its direct process.
func (tree *processTree) forceTerminate(command *exec.Cmd) error {
	groupErr := tree.terminate(command)
	processErr := command.Process.Kill()
	if errors.Is(processErr, syscall.ESRCH) || errors.Is(processErr, os.ErrProcessDone) {
		processErr = nil
	}
	return errors.Join(groupErr, processErr)
}

// close performs best-effort final cleanup of the owned process group.
func (tree *processTree) close() {
	_ = tree.terminate(nil)
}

// cleanup stops remaining descendants in the command's process group.
func (tree *processTree) cleanup(command *exec.Cmd) error { return tree.terminate(command) }

// processPID returns the Unix command process identifier.
func (*processTree) processPID(command *exec.Cmd) int {
	return command.Process.Pid
}
