package processrunner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type outputStream struct {
	writer  io.Writer
	mu      sync.Mutex
	state   *execution
	file    *os.File
	limit   int
	written int
	secrets []string
	pending string
	retain  int
}

type outputLogs struct {
	stdout, stderr *outputStream
	pipes          *commandOutput
}

// openLogs creates optional disk logs while always enforcing bounded in-memory capture.
func openLogs(ctx context.Context, limit int, state *execution, environment []string) (*outputLogs, error) {
	secrets := []string{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && value != "" && strings.Contains(strings.ToUpper(name), "TOKEN") {
			secrets = append(secrets, value)
		}
		if ok && value != "" && (strings.Contains(strings.ToUpper(name), "PASSWORD") || strings.Contains(strings.ToUpper(name), "SECRET")) {
			secrets = append(secrets, value)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	logs := &outputLogs{}
	logs.stdout = &outputStream{state: state, limit: limit, secrets: secrets}
	logs.stderr = &outputStream{state: state, limit: limit, secrets: secrets}
	if len(secrets) > 0 {
		logs.stdout.retain, logs.stderr.retain = len(secrets[0])-1, len(secrets[0])-1
	}
	directory, _ := ctx.Value(logDirectoryKey{}).(string)
	if directory == "" {
		return logs, nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path, err := os.MkdirTemp(directory, "process-")
	if err != nil {
		return nil, err
	}
	for name, stream := range map[string]*outputStream{"stdout": logs.stdout, "stderr": logs.stderr} {
		stream.file, err = os.OpenFile(filepath.Join(path, name+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = logs.close()
			return nil, err
		}
	}
	state.metadataPath = filepath.Join(path, "process.json")
	if err := state.save(); err != nil {
		_ = logs.close()
		return nil, err
	}
	return logs, nil
}

// Write redacts secrets across write boundaries before bounded capture and logging.
func (stream *outputStream) Write(data []byte) (int, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.writer != nil {
		n, err := stream.writer.Write(data)
		if err != nil {
			return n, err
		}
		if n != len(data) {
			return n, io.ErrShortWrite
		}
	}
	stream.pending += string(data)
	for _, secret := range stream.secrets {
		stream.pending = strings.ReplaceAll(stream.pending, secret, "[REDACTED]")
	}
	count := max(0, len(stream.pending)-stream.retain)
	err := stream.emit([]byte(stream.pending[:count]))
	stream.pending = stream.pending[count:]
	return len(data), err
}

// emit drains output after caps are reached so child pipes cannot block on logging limits.
func (stream *outputStream) emit(data []byte) error {
	state := stream.state
	state.mu.Lock()
	now := time.Now().UTC()
	if len(data) > 0 {
		state.snapshot.OutputBytes += int64(len(data))
		state.snapshot.LastOutputAt = &now
	}
	available := max(0, state.captureLimit-len(state.output))
	state.output = append(state.output, data[:min(available, len(data))]...)
	if len(data) > available {
		state.snapshot.Truncated = true
	}
	state.mu.Unlock()
	if stream.file != nil {
		count := min(len(data), max(0, stream.limit-stream.written))
		if count < len(data) {
			state.mu.Lock()
			state.snapshot.Truncated = true
			state.mu.Unlock()
		}
		n, err := stream.file.Write(data[:count])
		stream.written += n
		if err != nil {
			return err
		}
	}
	state.mu.Lock()
	save := time.Since(state.lastSave) >= time.Second
	if save {
		state.lastSave = now
	}
	state.mu.Unlock()
	if save {
		return state.save()
	}
	return nil
}

// flush drains retained redaction tails after both process streams close.
func (logs *outputLogs) flush() error {
	var errs []error
	for _, stream := range []*outputStream{logs.stdout, logs.stderr} {
		if stream == nil {
			continue
		}
		stream.mu.Lock()
		errs = append(errs, stream.emit([]byte(stream.pending)))
		stream.pending = ""
		stream.mu.Unlock()
	}
	return errors.Join(errs...)
}

// close releases disk logs and persists the last known process state.
func (logs *outputLogs) close() error {
	err := logs.flush()
	for _, stream := range []*outputStream{logs.stdout, logs.stderr} {
		if stream != nil && stream.file != nil {
			err = errors.Join(err, stream.file.Close())
			stream.file = nil
		}
	}
	if logs.stdout != nil {
		err = errors.Join(err, logs.stdout.state.save())
	}
	return err
}

// save atomically publishes metadata for readers without recording command arguments.
func (state *execution) save() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.metadataPath == "" {
		return nil
	}
	data, err := json.Marshal(state.snapshot)
	if err != nil {
		return err
	}
	temporary := state.metadataPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, state.metadataPath)
}

// LogEntry returns bounded output alongside process metadata.
type LogEntry struct {
	ID string `json:"id"`
	Snapshot
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// ReadLogs reads invocation logs through a rooted filesystem to reject escaping symlinks.
func ReadLogs(directory string, tailBytes int) ([]LogEntry, error) {
	entries := []LogEntry{}
	if tailBytes < 1 || tailBytes > 65536 {
		return nil, errors.New("log tail must be between 1 and 65536 bytes")
	}
	root, err := os.OpenRoot(directory)
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := file.Readdirnames(64)
	_ = file.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasPrefix(name, "process-") {
			continue
		}
		entry, err := readLogEntry(root, name, tailBytes)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// readLogEntry bounds metadata and output reads for one invocation.
func readLogEntry(root *os.Root, name string, tailBytes int) (LogEntry, error) {
	entry := LogEntry{ID: name}
	file, err := root.Open(filepath.Join(name, "process.json"))
	if err != nil {
		return entry, err
	}
	err = json.NewDecoder(io.LimitReader(file, 16384)).Decode(&entry.Snapshot)
	_ = file.Close()
	if err != nil {
		return entry, err
	}
	for stream, target := range map[string]*string{"stdout": &entry.Stdout, "stderr": &entry.Stderr} {
		file, err := root.Open(filepath.Join(name, stream+".log"))
		if err != nil {
			return entry, err
		}
		info, err := file.Stat()
		if err == nil {
			_, err = file.Seek(max(0, info.Size()-int64(tailBytes)), io.SeekStart)
		}
		if err != nil {
			_ = file.Close()
			return entry, err
		}
		data, err := io.ReadAll(io.LimitReader(file, int64(tailBytes)))
		_ = file.Close()
		if err != nil {
			return entry, err
		}
		*target = string(data)
	}
	return entry, nil
}
