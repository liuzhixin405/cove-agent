package race

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

type boundedOutput struct {
	mu   sync.Mutex
	data []byte
}

type processContainment struct {
	attach func() error
	close  func()
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	length := len(data)
	remaining := 64*1024 - len(b.data)
	if len(data) > remaining {
		data = data[:remaining]
	}
	b.data = append(b.data, data...)
	return length, nil
}

func contextStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "cancelled"
}

func Execute(ctx context.Context, directory string, env []string, argv []string) Execution {
	started := time.Now()
	result := Execution{Status: "start_error", ExitCode: -1, CostSource: "unverified"}
	if len(argv) == 0 {
		result.Error = "empty argv"
		return result
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = 2 * time.Second
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	containment, setupErr := containProcess(cmd)
	if setupErr != nil {
		result.Error = setupErr.Error()
		return result
	}
	defer containment.close()
	err := cmd.Start()
	if err == nil {
		if err = containment.attach(); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		} else {
			err = cmd.Wait()
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	result.Output = string(output.data)
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		result.Status = contextStatus(ctx)
	} else if err == nil {
		result.Status = "passed"
	} else if result.ExitCode != -1 {
		result.Status = "nonzero"
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
