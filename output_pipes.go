package processrunner

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

const outputDrainTimeout = 2 * time.Second

// commandOutput owns pipes independently of exec.Cmd.Wait, allowing tree cleanup
// after process exit even when descendants retain the output descriptors.
type commandOutput struct {
	readers [2]*os.File
	writers [2]*os.File
	results chan error
	failed  chan error
}

func newCommandOutput(command *exec.Cmd) (*commandOutput, error) {
	output := &commandOutput{results: make(chan error, 2), failed: make(chan error, 2)}
	for i := range output.readers {
		reader, writer, err := os.Pipe()
		if err != nil {
			output.closeFiles()
			return nil, err
		}
		output.readers[i], output.writers[i] = reader, writer
	}
	command.Stdout, command.Stderr = output.writers[0], output.writers[1]
	return output, nil
}

// start must precede platform startup handshakes that may wait on child output.
func (output *commandOutput) start(logs *outputLogs) {
	for i, stream := range []*outputStream{logs.stdout, logs.stderr} {
		_ = output.writers[i].Close()
		go func(reader *os.File, destination io.Writer) {
			_, err := io.Copy(destination, reader)
			_ = reader.Close()
			if err != nil {
				output.failed <- err
			}
			output.results <- err
		}(output.readers[i], stream)
	}
}

func (output *commandOutput) failures() <-chan error {
	if output == nil {
		return nil
	}
	return output.failed
}

// wait bounds inherited-pipe draining after cleanup. Caller-owned writers must
// return from Write; closing a pipe cannot interrupt a blocked user callback.
func (output *commandOutput) wait(timeout time.Duration) error {
	if output == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	for range output.readers {
		select {
		case copyErr := <-output.results:
			err = errors.Join(err, copyErr)
		case <-timer.C:
			output.closeFiles()
			err = errors.Join(err, exec.ErrWaitDelay, <-output.results)
		}
	}
	return err
}

func (output *commandOutput) closeFiles() {
	for _, pair := range [][2]*os.File{output.readers, output.writers} {
		for _, file := range pair {
			if file != nil {
				_ = file.Close()
			}
		}
	}
}
