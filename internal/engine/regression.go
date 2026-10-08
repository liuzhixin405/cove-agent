package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/checkpoint"
	"github.com/liuzhixin405/cove-agent/internal/delegate"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

type regressionTool struct{ engine *Engine }

func (t *regressionTool) Def() tool.Def {
	return tool.Def{
		Name: "regression_verify", UserFacingName: "Regression Verify",
		Description: "Verify Go regression tests: overlay selected production .go files with a pre-fix checkpoint, require real failing test events, then run the same tests on current code. Never restores workspace files. Test code is executed, not sandboxed.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"baseline":{"type":"string","description":"Pre-fix checkpoint hash from this project"},"files":{"type":"array","items":{"type":"string"},"description":"Existing production .go files to replace with old code, not test files"},"package":{"type":"string","description":"Project-relative Go package, e.g. ./internal/foo"},"run":{"type":"string","description":"Go test -run regex selecting regression tests"}},"required":["baseline","files","package","run"],"additionalProperties":false}`),
	}
}

func regressionFiles(input tool.Input) []string {
	var files []string
	switch values := input["files"].(type) {
	case []string:
		files = append(files, values...)
	case []any:
		for _, value := range values {
			name, ok := value.(string)
			if !ok {
				return nil
			}
			files = append(files, name)
		}
	}
	return files
}

func (t *regressionTool) Validate(input tool.Input) string {
	baseline, _ := input["baseline"].(string)
	if matched, _ := regexp.MatchString(`^[a-fA-F0-9]{7,64}$`, baseline); !matched {
		return "baseline must be a checkpoint hash"
	}
	files := regressionFiles(input)
	if len(files) == 0 || len(files) > 32 {
		return "select 1 to 32 production Go files"
	}
	for _, name := range files {
		clean := filepath.Clean(filepath.FromSlash(name))
		if filepath.IsAbs(name) || strings.Contains(name, ":") || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return "files must be project-relative production .go files"
		}
	}
	pkg, _ := input["package"].(string)
	if pkg != "." && !strings.HasPrefix(pkg, "./") {
		return "package must be project-relative"
	}
	for _, part := range strings.Split(pkg, "/") {
		if part == ".." || part == ".git" || strings.ContainsAny(part, "\\:\x00") {
			return "package must stay inside the project"
		}
	}
	pattern, _ := input["run"].(string)
	if strings.TrimSpace(pattern) == "" {
		return "run must select regression tests"
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return "invalid test regex: " + err.Error()
	}
	return ""
}

func (t *regressionTool) CheckPermissions(input tool.Input, tctx tool.Context) tool.PermissionDecision {
	return tool.Asked("runs project Go test code twice (baseline overlay and current implementation); not a sandbox")
}

func (t *regressionTool) Call(ctx context.Context, input tool.Input, tctx tool.Context) (tool.Result, error) {
	if why := t.Validate(input); why != "" {
		return tool.Result{Data: why, IsError: true}, nil
	}
	evidence, err := runRegression(ctx, input, tctx.Cwd)
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	if t.engine != nil {
		t.engine.recordRegressionEvidence(evidence)
	}
	data, err := json.Marshal(evidence)
	return tool.Result{Data: string(data), IsError: evidence.Status != "passed"}, err
}

type regressionOutput struct {
	sync.Mutex
	bytes.Buffer
	overflow bool
}

func (output *regressionOutput) Write(data []byte) (int, error) {
	output.Lock()
	defer output.Unlock()
	const limit = 2 * 1024 * 1024
	length := len(data)
	if len(data) > limit-output.Len() {
		data = data[:limit-output.Len()]
		output.overflow = true
	}
	_, err := output.Buffer.Write(data)
	return length, err
}

func runRegressionProcess(ctx context.Context, dir, pkg, pattern, overlay string) delegate.RegressionRun {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := []string{"test", "-json", "-count=1", "-timeout=60s", "-run=" + pattern}
	if overlay != "" {
		args = append(args, "-overlay="+overlay)
	}
	args = append(args, pkg)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir, cmd.WaitDelay = dir, 3*time.Second
	var output regressionOutput
	var diagnostics regressionOutput
	cmd.Stdout, cmd.Stderr = &output, &diagnostics
	err := cmd.Run()
	result := delegate.RegressionRun{Output: output.String(), Diagnostics: diagnostics.String()}
	if err != nil {
		result.ExitCode = -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
		} else {
			result.Error = err.Error()
		}
	}
	if ctx.Err() != nil {
		result.Error = ctx.Err().Error()
	}
	if output.overflow || diagnostics.overflow {
		result.Error = "test output exceeded 2 MiB evidence limit"
	}
	return result
}

func runRegression(ctx context.Context, input tool.Input, dir string) (delegate.RegressionEvidence, error) {
	if err := ctx.Err(); err != nil {
		return delegate.RegressionEvidence{}, err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return delegate.RegressionEvidence{}, err
	}
	manager, err := checkpoint.New(dir)
	if err != nil {
		return delegate.RegressionEvidence{}, err
	}
	baseline, _ := input["baseline"].(string)
	files := regressionFiles(input)
	baseline, contents, err := manager.ReadFiles(baseline, files)
	if err != nil {
		return delegate.RegressionEvidence{}, fmt.Errorf("regression baseline in %s: %w", dir, err)
	}
	for name := range contents {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || !info.Mode().IsRegular() {
			return delegate.RegressionEvidence{}, fmt.Errorf("overlay needs an existing regular file: %s", name)
		}
	}
	current, err := manager.Create("before independent regression verification")
	if err != nil {
		return delegate.RegressionEvidence{}, err
	}
	defer manager.WaitMaintenance()
	temp, err := os.MkdirTemp("", "cove-regression-")
	if err != nil {
		return delegate.RegressionEvidence{}, err
	}
	defer os.RemoveAll(temp)
	replacements := make(map[string]string)
	for name, data := range contents {
		path := filepath.Join(temp, fmt.Sprintf("source-%d.go", len(replacements)))
		if err := os.WriteFile(path, data, 0600); err != nil {
			return delegate.RegressionEvidence{}, err
		}
		absolute, err := filepath.Abs(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return delegate.RegressionEvidence{}, err
		}
		replacements[absolute] = path
	}
	data, err := json.Marshal(struct{ Replace map[string]string }{replacements})
	if err != nil {
		return delegate.RegressionEvidence{}, err
	}
	overlay := filepath.Join(temp, "overlay.json")
	if err := os.WriteFile(overlay, data, 0600); err != nil {
		return delegate.RegressionEvidence{}, err
	}
	pkg, _ := input["package"].(string)
	pattern, _ := input["run"].(string)
	before := runRegressionProcess(ctx, dir, pkg, pattern, overlay)
	after := delegate.RegressionRun{Error: "not run after interrupted baseline verification"}
	if before.Error == "" && ctx.Err() == nil {
		after = runRegressionProcess(ctx, dir, pkg, pattern, "")
	}
	evidence := delegate.CompareRegression(before, after)
	evidence.Baseline, evidence.Current = baseline, current
	evidence.Files, evidence.Package, evidence.Pattern = append([]string(nil), files...), pkg, pattern
	if ctx.Err() == nil {
		latest, err := manager.Create("after independent regression verification")
		if err != nil || latest != current {
			evidence.Status, evidence.Reason = "unverified", "workspace changed during verification or its state could not be checked"
		}
	} else {
		evidence.Status, evidence.Reason = "unverified", ctx.Err().Error()
	}
	evidence.Before.Output = truncateTail(evidence.Before.Output, 1000)
	evidence.After.Output = truncateTail(evidence.After.Output, 1000)
	evidence.Before.Diagnostics = truncateTail(evidence.Before.Diagnostics, 1000)
	evidence.After.Diagnostics = truncateTail(evidence.After.Diagnostics, 1000)
	return evidence, nil
}
