package processrunner

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Tool identifies a configured executable and its read-only version command.
type Tool struct {
	Name, Binary string
	VersionArgs  []string
}

// ToolHealth reports actual executable readiness without exposing filesystem paths or arguments.
type ToolHealth struct {
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Version string `json:"version"`
	Error   string `json:"error"`
}

// CheckTools performs bounded version checks in parallel through the common process runner.
func CheckTools(ctx context.Context, tools []Tool) []ToolHealth {
	results := make([]ToolHealth, len(tools))
	var wait sync.WaitGroup
	for index, tool := range tools {
		wait.Add(1)
		go func(index int, tool Tool) { defer wait.Done(); results[index] = checkTool(ctx, tool) }(index, tool)
	}
	wait.Wait()
	return results
}

// checkTool validates executable lookup and version execution within three seconds.
func checkTool(ctx context.Context, tool Tool) ToolHealth {
	health := ToolHealth{Name: tool.Name}
	path, err := exec.LookPath(tool.Binary)
	if err != nil {
		health.Error = "executable is missing or not executable"
		return health
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := (ProcessRunner{}).Run(ctx, Invocation{Name: tool.Name, Binary: path, Args: tool.VersionArgs, CaptureBytes: 4096})
	if err != nil {
		health.Error = "version check failed or timed out"
		return health
	}
	health.Ready = true
	health.Version = strings.TrimSpace(string(result.Output))
	return health
}
