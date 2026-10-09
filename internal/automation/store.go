package automation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/filelock"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

const Version = 1

type Spec struct {
	ID             string     `json:"id"`
	Prompt         string     `json:"prompt"`
	Enabled        bool       `json:"enabled"`
	SessionID      string     `json:"session_id,omitempty"`
	EverySeconds   int        `json:"every_seconds,omitempty"`
	Event          string     `json:"event,omitempty"`
	TimeoutSeconds int        `json:"timeout_seconds"`
	BudgetUSD      float64    `json:"budget_usd"`
	MaxTurns       int        `json:"max_turns"`
	Retries        int        `json:"retries"`
	Verify         [][]string `json:"verify,omitempty"`
	NextRun        time.Time  `json:"next_run,omitempty"`
}

type Check struct {
	Command  []string `json:"command"`
	ExitCode int      `json:"exit_code"`
	Output   string   `json:"output"`
}

type Result struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"task_id"`
	SessionID  string    `json:"session_id,omitempty"`
	Trigger    string    `json:"trigger"`
	EventKey   string    `json:"event_key,omitempty"`
	State      string    `json:"state"`
	Review     string    `json:"review"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished,omitempty"`
	LeaseUntil time.Time `json:"lease_until"`
	Attempts   int       `json:"attempts"`
	Base       string    `json:"base,omitempty"`
	Input      string    `json:"input,omitempty"`
	Patch      string    `json:"patch,omitempty"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	Checks     []Check   `json:"checks,omitempty"`
}

type Snapshot struct {
	Version int      `json:"version"`
	Project string   `json:"project"`
	Specs   []Spec   `json:"specs"`
	Results []Result `json:"results"`
}

type Store struct {
	dir     string
	project string
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)

func Open(stateRoot, project string) (*Store, error) {
	abs, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("automation project must be a directory")
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	root, err := ExternalPath(abs, stateRoot)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(abs))
	dir := filepath.Join(root, hex.EncodeToString(sum[:16]))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, project: abs}, nil
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(abs)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", err
		}
		suffix = append(suffix, filepath.Base(abs))
		abs = parent
	}
}

func ExternalPath(project, path string) (string, error) {
	root, err := canonicalPath(project)
	if err != nil {
		return "", err
	}
	resolved, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	comparison := resolved
	if runtime.GOOS == "windows" {
		root, comparison = strings.ToLower(root), strings.ToLower(comparison)
	}
	rel, err := filepath.Rel(root, comparison)
	if err != nil {
		if filepath.VolumeName(root) != filepath.VolumeName(comparison) {
			return resolved, nil
		}
		return "", err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return "", errors.New("automation path must be outside the original project")
	}
	return resolved, nil
}

func (s *Store) Path() string { return filepath.Join(s.dir, "state.json") }

func validate(spec Spec) error {
	if !validID.MatchString(spec.ID) || spec.Prompt == "" {
		return errors.New("id must be 1-80 letters/digits/_/-; prompt is required")
	}
	if len(spec.Prompt) > 65536 || len(spec.Verify) > 10 || len(spec.Event) > 120 || spec.EverySeconds > 31536000 {
		return errors.New("prompt max 64 KiB, verify max 10 commands, event max 120 bytes, interval max one year")
	}
	if spec.TimeoutSeconds < 1 || spec.TimeoutSeconds > 3600 || spec.MaxTurns < 1 || spec.MaxTurns > 100 || spec.Retries < 0 || spec.Retries > 2 {
		return errors.New("timeout_seconds must be 1..3600, max_turns 1..100, retries 0..2")
	}
	if math.IsNaN(spec.BudgetUSD) || math.IsInf(spec.BudgetUSD, 0) || spec.BudgetUSD <= 0 || spec.BudgetUSD > 100 {
		return errors.New("budget_usd must be finite and in (0,100]")
	}
	if spec.EverySeconds < 0 || (spec.EverySeconds > 0 && spec.EverySeconds < 60) {
		return errors.New("every_seconds must be 0 (manual/event) or at least 60")
	}
	for _, argv := range spec.Verify {
		if len(argv) == 0 || len(argv) > 64 || argv[0] == "" {
			return errors.New("verify requires nonempty command argv arrays")
		}
	}
	return nil
}

func (s *Store) update(fn func(*Snapshot) error) error {
	release, err := filelock.Acquire(filepath.Join(s.dir, "state.lock"), filelock.DefaultWait, 2*time.Minute)
	if err != nil {
		return err
	}
	defer release()
	state, err := s.readSnapshot()
	if err != nil {
		return err
	}
	if err := fn(&state); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(s.Path(), data, 0600)
}

func (s *Store) readSnapshot() (Snapshot, error) {
	state := Snapshot{Version: Version, Project: s.project}
	data, err := os.ReadFile(s.Path())
	if err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return Snapshot{}, fmt.Errorf("invalid automation state: %w", err)
		}
		if state.Version != Version || state.Project != s.project {
			return Snapshot{}, errors.New("unsupported automation state version or project mismatch")
		}
	} else if !os.IsNotExist(err) {
		return Snapshot{}, err
	}
	return state, nil
}

func (s *Store) Read() (Snapshot, error) {
	release, err := filelock.Acquire(filepath.Join(s.dir, "state.lock"), filelock.DefaultWait, 2*time.Minute)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	return s.readSnapshot()
}

func (s *Store) Add(spec Spec, now time.Time) error {
	if err := validate(spec); err != nil {
		return err
	}
	if spec.EverySeconds > 0 {
		spec.NextRun = now.Add(time.Duration(spec.EverySeconds) * time.Second)
	}
	return s.update(func(state *Snapshot) error {
		for _, existing := range state.Specs {
			if existing.ID == spec.ID {
				return fmt.Errorf("automation %q already exists; remove before replacing", spec.ID)
			}
		}
		state.Specs = append(state.Specs, spec)
		return nil
	})
}

func (s *Store) Remove(id string) error {
	now := time.Now().UTC()
	return s.update(func(state *Snapshot) error {
		for index := range state.Results {
			result := &state.Results[index]
			if result.TaskID != id || result.State != "running" {
				continue
			}
			if now.Before(result.LeaseUntil) {
				return errors.New("cannot remove a running automation")
			}
			// An expired claim (worker crashed) is uncertain, not running; it
			// used to block removal until some run/tick command converted it.
			result.State, result.Error = "uncertain", "worker lease expired; outcome unknown, not retried"
		}
		for index, spec := range state.Specs {
			if spec.ID == id {
				state.Specs = append(state.Specs[:index], state.Specs[index+1:]...)
				return nil
			}
		}
		return fmt.Errorf("unknown automation %q", id)
	})
}

func (s *Store) Review(id, decision string) error {
	if decision != "accepted" && decision != "rejected" {
		return errors.New("review must be accepted or rejected; review never applies or merges")
	}
	return s.update(func(state *Snapshot) error {
		for index := range state.Results {
			if state.Results[index].ID == id {
				if state.Results[index].State == "running" {
					if time.Now().UTC().Before(state.Results[index].LeaseUntil) {
						return errors.New("cannot review a running result")
					}
					state.Results[index].State = "uncertain"
					state.Results[index].Error = "worker lease expired; outcome unknown, not retried"
				}
				state.Results[index].Review = decision
				return nil
			}
		}
		return fmt.Errorf("unknown result %q", id)
	})
}
