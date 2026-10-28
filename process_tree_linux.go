//go:build linux

package processrunner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const guardianStartTimeout = 5 * time.Second

type processTree struct {
	controlRead    *os.File
	controlWrite   *os.File
	statusRead     *os.File
	statusWrite    *os.File
	processGroupID int
}

// newProcessTree wraps a process command with the internal Linux guardian.
func newProcessTree(command *exec.Cmd, invocation Invocation) (*processTree, error) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create guardian control pipe: %w", err)
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		_ = controlRead.Close()
		_ = controlWrite.Close()
		return nil, fmt.Errorf("create guardian status pipe: %w", err)
	}
	tree := &processTree{controlRead: controlRead, controlWrite: controlWrite, statusRead: statusRead, statusWrite: statusWrite}
	payload, err := json.Marshal(invocation)
	if err != nil {
		tree.close()
		return nil, fmt.Errorf("encode guardian invocation: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		tree.close()
		return nil, fmt.Errorf("locate guardian executable: %w", err)
	}
	command.Env = append(command.Environ(), guardianPayloadEnvironment+"="+base64.StdEncoding.EncodeToString(payload))
	command.Path = executable
	command.Args = []string{executable, guardianArgument}
	// Only the actual child applies Directory; the guardian retains the owner's cwd.
	command.Dir = ""
	command.ExtraFiles = []*os.File{controlRead, statusWrite}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return tree, nil
}

// attach waits until the guardian confirms the process's process group.
func (tree *processTree) attach(*exec.Cmd) error {
	_ = tree.controlRead.Close()
	tree.controlRead = nil
	_ = tree.statusWrite.Close()
	tree.statusWrite = nil
	result := make(chan error, 1)
	var status guardianStatus
	statusRead := tree.statusRead
	go func() { result <- json.NewDecoder(statusRead).Decode(&status) }()
	select {
	case err := <-result:
		_ = tree.statusRead.Close()
		tree.statusRead = nil
		if err != nil {
			return fmt.Errorf("read guardian startup status: %w", err)
		}
		if status.Error != "" {
			return errors.New(status.Error)
		}
		if status.ProcessGroupID <= 0 {
			return errors.New("guardian did not report a process group")
		}
		tree.processGroupID = status.ProcessGroupID
		return nil
	case <-time.After(guardianStartTimeout):
		_ = tree.statusRead.Close()
		tree.statusRead = nil
		return errors.New("guardian startup timed out")
	}
}

// terminate closes the owner pipe so the guardian can stop the process gracefully.
func (tree *processTree) terminate(*exec.Cmd) error {
	if tree.controlWrite == nil {
		return nil
	}
	err := tree.controlWrite.Close()
	tree.controlWrite = nil
	return err
}

// forceTerminate kills the process group and guardian after graceful shutdown expires.
func (tree *processTree) forceTerminate(command *exec.Cmd) error {
	controlErr := tree.terminate(command)
	groupErr := killProcessGroup(tree.processGroupID, syscall.SIGKILL)
	processErr := command.Process.Kill()
	if errors.Is(processErr, os.ErrProcessDone) || errors.Is(processErr, syscall.ESRCH) {
		processErr = nil
	}
	return errors.Join(controlErr, groupErr, processErr)
}

// close releases pipe handles retained by the owner process.
func (tree *processTree) close() {
	for _, file := range []*os.File{tree.controlRead, tree.controlWrite, tree.statusRead, tree.statusWrite} {
		if file != nil {
			_ = file.Close()
		}
	}
}

// cleanup avoids signaling a recycled group ID after the guardian has reaped it.
func (tree *processTree) cleanup(*exec.Cmd) error { return nil }

// processPID returns the process leader PID after startup acknowledgement.
func (tree *processTree) processPID(command *exec.Cmd) int {
	if tree.processGroupID > 0 {
		return tree.processGroupID
	}
	return command.Process.Pid
}

// killProcessGroup signals all processes that remain in the process group.
func killProcessGroup(groupID int, signal syscall.Signal) error {
	if groupID <= 0 {
		return nil
	}
	err := syscall.Kill(-groupID, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
