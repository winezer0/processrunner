//go:build windows

package processrunner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const processExitCode = 1

type processTree struct {
	job     windows.Handle
	exited  chan struct{}
	exitErr error
}

// newProcessTree prepares a kill-on-close job before the command can execute.
func newProcessTree(command *exec.Cmd, _ Invocation) (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create Windows job object: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure Windows job object: %w", err)
	}
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	return &processTree{job: job}, nil
}

// attach binds the suspended process to its job before resuming its initial thread.
func (tree *processTree) attach(command *exec.Cmd) error {
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false,
		uint32(command.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("open command process: %w", err)
	}
	owned := true
	defer func() {
		if owned {
			_ = windows.CloseHandle(process)
		}
	}()
	if err := windows.AssignProcessToJobObject(tree.job, process); err != nil {
		return fmt.Errorf("assign command process to Windows job: %w", err)
	}
	if err := resumeProcessThread(uint32(command.Process.Pid)); err != nil {
		return fmt.Errorf("resume command process: %w", err)
	}
	// Observe kernel process exit independently of exec.Wait's pipe draining.
	// Descendants may retain stdout/stderr, so waiting for Wait first deadlocks cleanup.
	tree.exited = make(chan struct{})
	owned = false
	go func() {
		defer close(tree.exited)
		defer windows.CloseHandle(process)
		_, err := windows.WaitForSingleObject(process, windows.INFINITE)
		tree.exitErr = errors.Join(err, tree.terminate(command))
	}()
	return nil
}

// resumeProcessThread resumes the initial thread after the process enters its job.
func resumeProcessThread(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != processID {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return openErr
		}
		previousCount, resumeErr := windows.ResumeThread(thread)
		closeErr := windows.CloseHandle(thread)
		if resumeErr == nil && previousCount == 0 {
			resumeErr = errors.New("process thread was not suspended")
		}
		return errors.Join(resumeErr, closeErr)
	}
	if !errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
		return err
	}
	return errors.New("command process has no initial thread")
}

// terminate kills every process that remains in the owned job.
func (tree *processTree) terminate(*exec.Cmd) error {
	return windows.TerminateJobObject(tree.job, processExitCode)
}

// cleanup stops descendants retained in the job after the direct command exits.
func (tree *processTree) cleanup(command *exec.Cmd) error {
	if tree.exited != nil {
		<-tree.exited
		return tree.exitErr
	}
	return tree.terminate(command)
}

// forceTerminate kills the job and also the direct process if job assignment failed.
func (tree *processTree) forceTerminate(command *exec.Cmd) error {
	jobErr := tree.terminate(command)
	processErr := command.Process.Kill()
	if errors.Is(processErr, os.ErrProcessDone) || (jobErr == nil && errors.Is(processErr, windows.ERROR_ACCESS_DENIED)) {
		processErr = nil
	}
	return errors.Join(jobErr, processErr)
}

// close releases the job handle and its kill-on-close ownership.
func (tree *processTree) close() {
	if tree.exited != nil {
		<-tree.exited
	}
	_ = windows.CloseHandle(tree.job)
}

// processPID returns the Windows command process identifier.
func (*processTree) processPID(command *exec.Cmd) int {
	return command.Process.Pid
}
