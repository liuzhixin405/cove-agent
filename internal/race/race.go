package race

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

const Version = 1

type Spec struct {
	Version                 int        `json:"version"`
	Prompts                 []string   `json:"prompts"`
	Verify                  [][]string `json:"verify"`
	TotalBudgetUSD          float64    `json:"total_budget_usd"`
	CandidateBudgetUSD      float64    `json:"candidate_budget_usd"`
	TotalTimeoutSeconds     int        `json:"total_timeout_seconds"`
	CandidateTimeoutSeconds int        `json:"candidate_timeout_seconds"`
	VerifyTimeoutSeconds    int        `json:"verify_timeout_seconds"`
}

func (s Spec) Validate() error {
	if s.Version != Version {
		return errors.New("unsupported race spec version")
	}
	if len(s.Prompts) != 2 || strings.TrimSpace(s.Prompts[0]) == "" || strings.TrimSpace(s.Prompts[1]) == "" || strings.TrimSpace(s.Prompts[0]) == strings.TrimSpace(s.Prompts[1]) {
		return errors.New("exactly two distinct nonempty solution prompts are required")
	}
	for _, value := range []float64{s.TotalBudgetUSD, s.CandidateBudgetUSD} {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("positive finite total and per-candidate budgets are required")
		}
	}
	for _, value := range []int{s.TotalTimeoutSeconds, s.CandidateTimeoutSeconds, s.VerifyTimeoutSeconds} {
		if value <= 0 || value > 86400 {
			return errors.New("timeouts must be between 1 and 86400 seconds")
		}
	}
	if len(s.Verify) == 0 {
		return errors.New("at least one shared verifier argv is required")
	}
	for _, argv := range s.Verify {
		if len(argv) == 0 || argv[0] == "" {
			return errors.New("empty verifier command")
		}
		for _, arg := range argv {
			if strings.ContainsRune(arg, '\x00') {
				return errors.New("NUL in verifier argument")
			}
		}
	}
	return nil
}

func ReadSpec(path string) (Spec, error) {
	file, err := os.Open(path)
	if err != nil {
		return Spec{}, err
	}
	defer file.Close()
	var spec Spec
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return spec, errors.New("trailing or oversized spec data")
	}
	return spec, spec.Validate()
}

type Execution struct {
	Status     string   `json:"status"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
	Output     string   `json:"output,omitempty"`
	Error      string   `json:"error,omitempty"`
	CostUSD    *float64 `json:"cost_usd"`
	CostSource string   `json:"cost_source"`
}

type Request struct {
	Directory string
	Prompt    string
	BudgetUSD float64
}

type Runner interface {
	Run(context.Context, Request) Execution
}

type RunnerFunc func(context.Context, Request) Execution

func (f RunnerFunc) Run(ctx context.Context, request Request) Execution { return f(ctx, request) }

type Verification struct {
	Argv      []string  `json:"argv"`
	Execution Execution `json:"execution"`
}

type Candidate struct {
	ID           string         `json:"id"`
	Prompt       string         `json:"prompt"`
	BudgetUSD    float64        `json:"budget_usd"`
	Generation   Execution      `json:"generation"`
	Verification []Verification `json:"verification"`
	Correct      bool           `json:"correct"`
	DurationMS   int64          `json:"duration_ms"`
	PatchHash    string         `json:"patch_sha256"`
	PatchBytes   int            `json:"patch_bytes"`
	Error        string         `json:"error,omitempty"`
	CleanupError string         `json:"cleanup_error,omitempty"`
}

type Report struct {
	Version    int                `json:"version"`
	ID         string             `json:"id"`
	Baseline   workspace.Baseline `json:"baseline"`
	Spec       Spec               `json:"spec"`
	Started    time.Time          `json:"started"`
	DurationMS int64              `json:"duration_ms"`
	Status     string             `json:"status"`
	Candidates [2]Candidate       `json:"candidates"`
	Selected   string             `json:"selected,omitempty"`
	SelectedAt *time.Time         `json:"selected_at,omitempty"`
}

type Service struct {
	Directory string
	Runner    Runner
	OnSelect  func()
	// OnFinish, when set, receives the final report once a run ends
	// (completed, timeout or cancelled), after it was saved. The report used
	// to be dropped by Start, so a race finished with nobody told.
	OnFinish func(*Report)
	mu       sync.Mutex
	active   map[string]context.CancelFunc
	selectMu sync.Mutex
	workers  sync.WaitGroup
	shutdown context.Context
	stop     context.CancelFunc
	closed   bool
}

func token(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return false
		}
	}
	return true
}

func (s *Service) storage() (*os.Root, error) {
	if s.Directory == "" {
		return nil, errors.New("race report directory is required")
	}
	if err := os.MkdirAll(s.Directory, 0700); err != nil {
		return nil, err
	}
	return os.OpenRoot(s.Directory)
}

func (s *Service) save(report *Report) error {
	root, err := s.storage()
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.WriteFileRoot(root, filepath.Join(report.ID, "report.json"), data, 0600)
}

func (s *Service) Load(id string) (*Report, error) {
	if !token(id) {
		return nil, errors.New("invalid run ID")
	}
	root, err := s.storage()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// The report is rewritten atomically when the run ends; a reader that
	// arrives in that instant sees no file although the run's directory
	// exists. Such a report is retried briefly; a run that never existed
	// (no directory) is not.
	var file *os.File
	for attempt := 0; ; attempt++ {
		file, err = root.Open(filepath.Join(id, "report.json"))
		if err == nil {
			break
		}
		if _, dirErr := root.Stat(id); dirErr != nil || attempt >= 10 {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer file.Close()
	var report Report
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return nil, err
	}
	if report.Version != Version || report.ID != id || report.Candidates[0].ID != "a" || report.Candidates[1].ID != "b" {
		return nil, errors.New("invalid or unsupported race report")
	}
	return &report, nil
}

func (s *Service) List() ([]*Report, error) {
	root, err := s.storage()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	reports := make([]*Report, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !token(entry.Name()) {
			continue
		}
		report, err := s.Load(entry.Name())
		if err != nil {
			// A directory left by a crash between Mkdir and the first save
			// (or created by hand) must not hide every other run forever.
			continue
		}
		reports = append(reports, report)
	}
	return reports, nil
}

// outsideProject reports whether store (the reports directory) is not root or
// inside it. Both paths are absolute and symlink-resolved. On Windows a
// project on D: and the default ~/.cove on C: make filepath.Rel fail, which
// used to be read as "inside" and refused every cross-volume /race run.
func outsideProject(root, store string) bool {
	if runtime.GOOS == "windows" {
		root, store = strings.ToLower(root), strings.ToLower(store)
	}
	if filepath.VolumeName(root) != filepath.VolumeName(store) {
		return true
	}
	rel, err := filepath.Rel(root, store)
	if err != nil {
		return false
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Service) Cancel(id string) error {
	if !token(id) {
		return errors.New("invalid run ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cancel, ok := s.active[id]
	if !ok {
		return errors.New("unknown or inactive run ID")
	}
	cancel()
	return nil
}

func (s *Service) Run(ctx context.Context, project string, spec Spec) (*Report, error) {
	return s.run(ctx, project, spec, nil)
}

func (s *Service) Start(ctx context.Context, project string, spec Spec) (string, error) {
	type started struct {
		id  string
		err error
	}
	ready := make(chan started, 1)
	go func() {
		notified := false
		_, err := s.run(ctx, project, spec, func(id string) { notified = true; ready <- started{id: id} })
		if !notified {
			ready <- started{err: err}
		}
	}()
	result := <-ready
	return result.id, result.err
}

func (s *Service) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.stop != nil {
		s.stop()
	}
	s.mu.Unlock()
	s.workers.Wait()
	return nil
}

func (s *Service) run(ctx context.Context, project string, spec Spec, ready func(string)) (*Report, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("race service is closed")
	}
	if s.shutdown == nil {
		s.shutdown, s.stop = context.WithCancel(context.Background())
	}
	shutdown := s.shutdown
	s.workers.Add(1)
	s.mu.Unlock()
	defer s.workers.Done()
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TotalTimeoutSeconds)*time.Second)
	defer cancel()
	unhook := context.AfterFunc(shutdown, cancel)
	defer unhook()
	ctx = runCtx
	if s.Runner == nil {
		return nil, errors.New("race runner is required")
	}
	baseline, err := workspace.Capture(ctx, project)
	if err != nil {
		return nil, err
	}
	absStore, err := workspace.ResolvePath(s.Directory)
	if err != nil {
		return nil, err
	}
	if !outsideProject(baseline.Root, absStore) {
		return nil, errors.New("race reports must live outside the source project in the config directory")
	}
	root, err := s.storage()
	if err != nil {
		return nil, err
	}
	id := strings.ToLower(rand.Text())
	err = root.Mkdir(id, 0700)
	root.Close()
	if err != nil {
		return nil, err
	}
	started := time.Now()
	report := &Report{Version: Version, ID: id, Baseline: baseline, Spec: spec, Started: started.UTC(), Status: "running"}
	for index, name := range []string{"a", "b"} {
		report.Candidates[index] = Candidate{ID: name, Prompt: spec.Prompts[index], BudgetUSD: math.Min(spec.CandidateBudgetUSD, spec.TotalBudgetUSD/2), Generation: Execution{Status: "queued", ExitCode: -1, CostSource: "unverified"}}
	}
	if err := s.save(report); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.active == nil {
		s.active = make(map[string]context.CancelFunc)
	}
	s.active[id] = cancel
	s.mu.Unlock()
	if ready != nil {
		ready(id)
	}
	defer func() { s.mu.Lock(); delete(s.active, id); s.mu.Unlock() }()
	var group sync.WaitGroup
	for index := range report.Candidates {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			s.candidate(runCtx, baseline, id, spec, &report.Candidates[index])
		}(index)
	}
	group.Wait()
	report.DurationMS = time.Since(started).Milliseconds()
	report.Status = "completed"
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		report.Status = "timeout"
	} else if runCtx.Err() != nil {
		report.Status = "cancelled"
	}
	saveErr := s.save(report)
	if s.OnFinish != nil {
		s.OnFinish(report)
	}
	if saveErr != nil {
		return report, saveErr
	}
	return report, nil
}

func (s *Service) candidate(ctx context.Context, baseline workspace.Baseline, id string, spec Spec, candidate *Candidate) {
	started := time.Now()
	defer func() { candidate.DurationMS = time.Since(started).Milliseconds() }()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(spec.CandidateTimeoutSeconds)*time.Second)
	defer cancel()
	defer func() {
		if ctx.Err() != nil && (candidate.Generation.Status == "queued" || candidate.Generation.Status == "start_error") {
			candidate.Generation.Status = contextStatus(ctx)
		}
	}()
	parent, err := os.MkdirTemp("", "cove-race-")
	if err != nil {
		candidate.Generation.Status, candidate.Error = "start_error", err.Error()
		return
	}
	defer os.RemoveAll(parent)
	work := filepath.Join(parent, candidate.ID)
	if err := baseline.Add(ctx, work); err != nil {
		candidate.Generation.Status, candidate.Error = "start_error", err.Error()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		_ = baseline.Remove(cleanupCtx, work)
		return
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if err := baseline.Remove(cleanupCtx, work); err != nil {
			candidate.CleanupError = err.Error()
		}
	}()
	candidate.Generation = s.Runner.Run(ctx, Request{Directory: work, Prompt: candidate.Prompt, BudgetUSD: candidate.BudgetUSD})
	if candidate.Generation.CostUSD == nil {
		candidate.Generation.CostSource = "unverified"
	}
	if ctx.Err() != nil {
		candidate.Generation.Status = contextStatus(ctx)
	}
	if candidate.Generation.Status != "passed" {
		return
	}
	patch, err := workspace.Patch(ctx, work, baseline.Commit)
	if err != nil {
		candidate.Error = err.Error()
		return
	}
	candidate.Correct = true
	for _, argv := range spec.Verify {
		verifyCtx, verifyCancel := context.WithTimeout(ctx, time.Duration(spec.VerifyTimeoutSeconds)*time.Second)
		result := Execute(verifyCtx, work, nil, argv)
		verifyCancel()
		candidate.Verification = append(candidate.Verification, Verification{Argv: append([]string(nil), argv...), Execution: result})
		if result.Status != "passed" {
			candidate.Correct = false
		}
	}
	after, err := workspace.Patch(ctx, work, baseline.Commit)
	if err != nil || !bytes.Equal(patch, after) {
		candidate.Correct = false
		candidate.Error = "candidate changed during verification or patch capture failed"
	}
	if candidate.Generation.CostUSD != nil && (*candidate.Generation.CostUSD > candidate.BudgetUSD || *candidate.Generation.CostUSD < 0 || math.IsNaN(*candidate.Generation.CostUSD) || math.IsInf(*candidate.Generation.CostUSD, 0)) {
		candidate.Correct = false
		candidate.Error = "runner exceeded allocated budget or returned invalid cost"
	}
	if len(patch) == 0 {
		candidate.Correct = false
		candidate.Error = "candidate has no patch"
	}
	candidate.PatchHash, candidate.PatchBytes = workspace.Hash(patch), len(patch)
	root, err := s.storage()
	if err != nil {
		candidate.Correct = false
		candidate.Error = err.Error()
		return
	}
	defer root.Close()
	if err := fsatomic.WriteFileRoot(root, filepath.Join(id, candidate.ID+".patch"), patch, 0600); err != nil {
		candidate.Correct = false
		candidate.Error = err.Error()
	}
}

func (s *Service) Select(ctx context.Context, project, id, name string) (*Report, error) {
	s.selectMu.Lock()
	defer s.selectMu.Unlock()
	if name != "a" && name != "b" {
		return nil, errors.New("candidate must be a or b")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("race service is closed")
	}
	if s.shutdown == nil {
		s.shutdown, s.stop = context.WithCancel(context.Background())
	}
	shutdown := s.shutdown
	s.workers.Add(1)
	s.mu.Unlock()
	defer s.workers.Done()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	unhook := context.AfterFunc(shutdown, cancel)
	defer unhook()
	store, err := s.storage()
	if err != nil {
		return nil, err
	}
	defer store.Close()
	lock, err := store.OpenFile("selection.lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("selection lock held or unavailable (crash leftovers require operator review): %w", err)
	}
	if err := lock.Close(); err != nil {
		_ = store.Remove("selection.lock")
		return nil, err
	}
	defer store.Remove("selection.lock")
	report, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	// A run that hit the total timeout or was cancelled may still hold a
	// candidate that finished generation and every verifier before the cut;
	// its evidence (patch, hash, verification argv/exit codes) is complete and
	// is checked below exactly as for a completed run. Only a run still in
	// progress (or already applied) is refused here.
	if report.Status == "running" || report.Selected != "" || (report.Status != "completed" && report.Status != "timeout" && report.Status != "cancelled") {
		return nil, errors.New("run is incomplete or already selected")
	}
	baseline, err := workspace.Capture(ctx, project)
	if err != nil {
		return nil, err
	}
	if baseline != report.Baseline {
		return nil, errors.New("project scope, branch, or base commit drift")
	}
	index := 0
	if name == "b" {
		index = 1
	}
	candidate := report.Candidates[index]
	if !candidate.Correct || candidate.Error != "" || candidate.CleanupError != "" || candidate.Generation.Status != "passed" || len(candidate.Verification) != len(report.Spec.Verify) {
		return nil, errors.New("candidate did not pass shared verification")
	}
	for index, verify := range candidate.Verification {
		if verify.Execution.Status != "passed" || verify.Execution.ExitCode != 0 || !slices.Equal(verify.Argv, report.Spec.Verify[index]) {
			return nil, errors.New("failed or mismatched shared verification")
		}
	}
	root, err := s.storage()
	if err != nil {
		return nil, err
	}
	patch, err := root.ReadFile(filepath.Join(id, name+".patch"))
	root.Close()
	if err != nil {
		return nil, err
	}
	if len(patch) != candidate.PatchBytes {
		return nil, errors.New("patch size mismatch")
	}
	if err := report.Baseline.Apply(ctx, patch, candidate.PatchHash); err != nil {
		return nil, err
	}
	if s.OnSelect != nil {
		s.OnSelect()
	}
	now := time.Now().UTC()
	report.Selected, report.SelectedAt = name, &now
	if err := s.save(report); err != nil {
		return report, fmt.Errorf("patch applied, but selection audit could not be saved: %w", err)
	}
	return report, nil
}
