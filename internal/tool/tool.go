package tool

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

type Input = map[string]any

type Result struct {
	Data        string            `json:"data"`
	IsError     bool              `json:"is_error"`
	ShouldRetry bool              `json:"should_retry"`
	Parts       []api.MessagePart `json:"parts,omitempty"`
}

type PermissionDecision struct {
	Decision PermissionResult
	Reason   string
}

type PermissionResult string

const (
	Allow  PermissionResult = "allow"
	Deny   PermissionResult = "deny"
	Ask    PermissionResult = "ask"
	Bypass PermissionResult = "bypass"
)

type Context struct {
	Cwd              string
	ToolUseID        string
	SessionID        string
	PermissionMode   string
	IsNonInteractive bool
	Debug            bool
	Runtime          *Runtime
	// OnProgress, when set, is invoked with incremental output chunks while a
	// long-running tool (e.g. bash/powershell) executes. It lets the UI show
	// live output and lets the stall monitor know the tool is still alive.
	OnProgress func(chunk string)
	// OnStderrProgress, when set, receives the chunks of the tool's stderr,
	// which otherwise go to OnProgress too. stdout and stderr are copied by
	// separate goroutines; a consumer that keeps per-stream state (a
	// sanitiser holding back a partial escape or UTF-8 sequence) needs them
	// apart, or the held bytes of one are completed by the other's chunk.
	OnStderrProgress func(chunk string)
	// SetWaiting, when set, tells the engine the tool is waiting on the
	// person (the question tool at AskUser), so the stall monitor does not
	// call the wait a hang. Call it with true before waiting, false after.
	SetWaiting func(waiting bool)
}

type Runtime struct {
	mu            sync.Mutex
	PlanMode      bool
	WorktreeDir   string
	WorktreeMain  string
	Tasks         map[string]*TaskRecord
	Teams         map[string]*TeamRecord
	Messages      []MessageRecord
	TaskCounter   int
	AgentRunner   any
	SkillManager  any
	SkillPrompts  map[string]string
	PluginManager any
	Cwd           string
	AskUser       func(prompt string) string
	// PlanExecuteFunc, when set, is invoked by the execute_plan tool.
	// It receives parallel flag and returns a formatted result summary.
	// ctx is the calling tool's context: cancelling the turn cancels the
	// sub-agents the plan executor is running.
	PlanExecuteFunc func(ctx context.Context, parallel bool) (string, error)

	// files is created lazily by Files(); guarded by mu.
	files *FileTracker
}

func (r *Runtime) Lock()   { r.mu.Lock() }
func (r *Runtime) Unlock() { r.mu.Unlock() }

// PlanMode and WorktreeDir are written by tool calls and read by others, and
// tool calls can execute concurrently (see the engine's parallel batch). They
// are guarded by the same mutex as the rest of Runtime, so go through these
// accessors rather than touching the fields directly.

// SetPlanMode records whether plan mode is active.
func (r *Runtime) SetPlanMode(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.PlanMode = on
}

// IsPlanMode reports whether plan mode is active.
func (r *Runtime) IsPlanMode() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.PlanMode
}

// SetWorktreeMain records the project directory the active worktree was
// entered from, so exit_worktree can run git there (git refuses to remove
// the worktree it is run from).
func (r *Runtime) SetWorktreeMain(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.WorktreeMain = dir
}

func (r *Runtime) GetWorktreeMain() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.WorktreeMain
}

// SetWorktreeDir records the active worktree path ("" when none).
func (r *Runtime) SetWorktreeDir(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.WorktreeDir = dir
}

// GetWorktreeDir returns the active worktree path, or "" when none.
func (r *Runtime) GetWorktreeDir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.WorktreeDir
}

type TaskRecord struct {
	ID          string
	Title       string
	Description string
	Status      string
	Priority    string // todowrite's priority (high, medium, low)
	Output      string
	Kind        string
	ParentID    string
	CreatedAt   string
	UpdatedAt   string
}

type TeamRecord struct {
	Name      string
	Members   []TeamMemberRecord
	Status    string
	CreatedAt string
}

type TeamMemberRecord struct {
	ID     string
	Agent  string
	Task   string
	Status string
	Output string
}

type MessageRecord struct {
	To        string
	Message   string
	CreatedAt string
	Delivered bool
}

type Def struct {
	Name              string
	Aliases           []string
	Description       string
	Prompt            string
	InputSchema       json.RawMessage
	IsReadOnly        bool
	IsConcurrencySafe bool
	// PlanSafe lets a tool that is not read-only run in plan mode because
	// what it changes stays inside the session (a todo list, a question to
	// the user, a task record) or goes through the permission check again
	// (a sub-agent's own tool calls). Plan mode allows read-only and
	// PlanSafe tools only, whatever a tool's CheckPermissions answers: a new
	// tool that returns Allowed is not a way out of plan mode.
	PlanSafe       bool
	UserFacingName string
}

type Tool interface {
	Def() Def
	Call(ctx context.Context, input Input, tctx Context) (Result, error)
	Validate(input Input) string
	CheckPermissions(input Input, tctx Context) PermissionDecision
}

type baseTool struct{ def Def }

func (b *baseTool) Def() Def                    { return b.def }
func (b *baseTool) Validate(input Input) string { return "" }
func (b *baseTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return PermissionDecision{Decision: Deny, Reason: "not implemented"}
}

func Decision(d PermissionResult, reason string) PermissionDecision {
	return PermissionDecision{Decision: d, Reason: reason}
}

func Allowed(reason string) PermissionDecision { return Decision(Allow, reason) }
func Denied(reason string) PermissionDecision  { return Decision(Deny, reason) }
func Asked(reason string) PermissionDecision   { return Decision(Ask, reason) }
