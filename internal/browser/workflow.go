package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/safeurl"
)

const (
	MaxWorkflowBytes   = 64 * 1024
	MaxWorkflowSteps   = 32
	MaxScreenshotBytes = 4 * 1024 * 1024
	StatusPass         = "pass"
	StatusFail         = "fail"
	StatusUnverified   = "unverified"
)

var (
	ErrAssertionFailed = errors.New("browser assertion failed")
	errWorkflowSafety  = errors.New("browser workflow safety policy rejected page or request")
	errWorkflowNetwork = errors.New("browser workflow network unavailable")
)

// Workflow is a bounded, declarative browser scenario. Fixtures are non-secret
// user-supplied inputs; no executable JavaScript is accepted.
type Workflow struct {
	URL           string            `json:"url"`
	TimeoutMS     int               `json:"timeout_ms,omitempty"`
	StepTimeoutMS int               `json:"step_timeout_ms,omitempty"`
	Fixtures      map[string]string `json:"fixtures,omitempty"`
	Steps         []WorkflowStep    `json:"steps"`
}

type WorkflowStep struct {
	Action   string `json:"action"`
	Selector string `json:"selector,omitempty"`
	URL      string `json:"url,omitempty"`
	Text     string `json:"text,omitempty"`
	Fixture  string `json:"fixture,omitempty"`
}

// RunOptions contains trusted host settings, never settings from workflow JSON.
type RunOptions struct {
	EvidenceDir string
	ChromePath  string
}

type StepEvidence struct {
	Viewport string `json:"viewport"`
	Index    int    `json:"index"`
	Action   string `json:"action"`
	Status   string `json:"status"`
}

type Artifact struct {
	Name     string `json:"name"`
	Viewport string `json:"viewport"`
	SHA256   string `json:"sha256"`
	Bytes    int    `json:"bytes"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// Report deliberately excludes fixture values, selectors, page text and URLs.
type Report struct {
	Version     int            `json:"version"`
	Status      string         `json:"status"`
	Reason      string         `json:"reason,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	DurationMS  int64          `json:"duration_ms"`
	Assertions  int            `json:"assertions"`
	Steps       []StepEvidence `json:"steps"`
	Artifacts   []Artifact     `json:"artifacts"`
	EvidenceDir string         `json:"evidence_dir"`
}

// DecodeWorkflow rejects unknown fields and oversized or trailing JSON.
func DecodeWorkflow(reader io.Reader) (Workflow, error) {
	var workflow Workflow
	data, err := io.ReadAll(io.LimitReader(reader, MaxWorkflowBytes+1))
	if err != nil || len(data) > MaxWorkflowBytes {
		return workflow, errors.New("workflow exceeds size limit or cannot be read")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&workflow); err != nil {
		return Workflow{}, errors.New("invalid workflow JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Workflow{}, errors.New("workflow must contain one JSON object")
	}
	return workflow, nil
}

func (b *Browser) validateWorkflow(ctx context.Context, workflow Workflow) error {
	if len(workflow.Steps) == 0 || len(workflow.Steps) > MaxWorkflowSteps {
		return errors.New("workflow requires 1..32 steps")
	}
	if workflow.TimeoutMS < 0 || workflow.TimeoutMS > 120000 || workflow.StepTimeoutMS < 0 || workflow.StepTimeoutMS > 10000 {
		return errors.New("workflow duration exceeds limits")
	}
	if len(workflow.Fixtures) > 32 {
		return errors.New("too many fixtures")
	}
	for key, value := range workflow.Fixtures {
		if len(key) > 128 || len(value) > 4096 {
			return errors.New("fixture exceeds size limit")
		}
	}
	encoded, err := json.Marshal(workflow)
	if err != nil || len(encoded) > MaxWorkflowBytes {
		return errors.New("workflow exceeds size limit")
	}
	if err := b.workflowURL(ctx, workflow.URL); err != nil {
		return err
	}
	assertions := 0
	for _, step := range workflow.Steps {
		if len(step.Selector) > 1024 || len(step.Text) > 4096 {
			return errors.New("step exceeds size limit")
		}
		switch step.Action {
		case "navigate", "assert_url":
			if err := b.workflowURL(ctx, step.URL); err != nil {
				return err
			}
		case "click", "fill", "assert_text", "assert_visible":
			if strings.TrimSpace(step.Selector) == "" {
				return errors.New("step requires CSS selector")
			}
		default:
			return errors.New("unsupported workflow action")
		}
		if step.Action == "fill" {
			if _, exists := workflow.Fixtures[step.Fixture]; !exists || step.Text != "" {
				return errors.New("fill requires a fixture key, not inline text")
			}
		}
		if step.Action == "assert_text" && step.Text == "" {
			return errors.New("text assertion must not be empty")
		}
		if strings.HasPrefix(step.Action, "assert_") {
			assertions++
		}
	}
	if assertions == 0 {
		return errors.New("explicit assertions required")
	}
	return nil
}

func (b *Browser) workflowURL(ctx context.Context, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("invalid workflow URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errWorkflowSafety
	}
	_, err = b.workflowAddresses(ctx, parsed.Hostname())
	return err
}

func (b *Browser) workflowAddresses(ctx context.Context, host string) ([]net.IP, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	lower := strings.ToLower(host)
	if b.allowLocalhost && lower == "localhost" {
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") || lower == "metadata.google.internal" || strings.HasSuffix(lower, ".internal") || strings.HasSuffix(lower, ".local") {
		return nil, errWorkflowSafety
	}
	if literal := net.ParseIP(host); literal != nil {
		if b.allowLocalhost && literal.IsLoopback() {
			return []net.IP{literal}, nil
		}
		if safeurl.IsPrivateHost(host) {
			return nil, errWorkflowSafety
		}
		return []net.IP{literal}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIP(lookupCtx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errWorkflowNetwork
	}
	for _, address := range addresses {
		if safeurl.IsPrivateIP(address) {
			return nil, errWorkflowSafety
		}
	}
	return addresses, nil
}

// Run executes both desktop and mobile scenarios and persists bounded evidence.
// Missing Chrome, cancellation and infrastructure failures are never passes.
func (b *Browser) Run(ctx context.Context, workflow Workflow, options RunOptions) (Report, error) {
	report := Report{Version: 1, Status: StatusUnverified, StartedAt: time.Now().UTC(), Steps: []StepEvidence{}, Artifacts: []Artifact{}}
	if options.EvidenceDir == "" {
		return report, errors.New("evidence directory required")
	}
	if err := os.MkdirAll(options.EvidenceDir, 0700); err != nil {
		return report, err
	}
	directory, err := os.MkdirTemp(options.EvidenceDir, "browser-")
	if err != nil {
		return report, err
	}
	report.EvidenceDir, err = filepath.Abs(directory)
	if err != nil {
		return report, err
	}
	if err := privateWorkflowDirectory(directory); err != nil {
		_ = os.Remove(directory)
		report.EvidenceDir = ""
		return report, errors.New("cannot secure browser evidence directory")
	}
	timeout := time.Duration(workflow.TimeoutMS) * time.Millisecond
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 60 * time.Second
	}
	if b.timeout < timeout {
		timeout = b.timeout
	}
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runErr := b.validateWorkflow(taskCtx, workflow)
	if taskCtx.Err() != nil {
		runErr, report.Reason = taskCtx.Err(), "cancelled_or_timeout"
	} else if errors.Is(runErr, errWorkflowNetwork) {
		report.Reason = "network_unavailable"
	} else if runErr != nil {
		report.Status, report.Reason = StatusFail, "invalid_workflow"
	} else {
		runErr = b.runWorkflow(taskCtx, workflow, options, &report)
	}
	report.DurationMS = time.Since(report.StartedAt).Milliseconds()
	data, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		var root *os.Root
		root, err = os.OpenRoot(report.EvidenceDir)
		if err == nil {
			err = fsatomic.WriteFileRoot(root, "report.json", data, 0600)
			_ = root.Close()
		}
	}
	if err != nil {
		report.Status, report.Reason = StatusUnverified, "evidence_persist_failed"
		return report, fmt.Errorf("persist browser evidence: %w", err)
	}
	return report, runErr
}
