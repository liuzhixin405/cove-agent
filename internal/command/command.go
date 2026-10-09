package command

import (
	"context"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	ctxt "github.com/liuzhixin405/cove-agent/internal/context"
	"github.com/liuzhixin405/cove-agent/internal/mcp"
	"github.com/liuzhixin405/cove-agent/internal/memory"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/plugin"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/skills"
)

type CostTrackerView interface {
	Summary() string
}

type EngineView interface {
	Messages() []api.Message
	MessageCount() int
	LoadMessages([]api.Message)
	SetSystemOverride(prompt string)
	SystemPrompt() string
	CostTracker() CostTrackerView
}

type SessionStore interface {
	List() ([]session.Record, error)
	Load(id string) (*session.Record, error)
}

type PluginManager interface {
	Install(name string, url string) error
	Uninstall(name string) error
	Disable(name string) error
	Enable(name string) error
	AllPlugins() []plugin.Entry
}

type SkillManager interface {
	All() []skills.Skill
	Get(name string) (skills.Skill, bool)
}

type MemoryStore interface {
	All() []memory.Entry
	Save(name, content string) error
	Delete(name string) error
	Search(query string, topK int) []memory.EntryMatch
	Stats() memory.Stats
}

// StatusSource is the optional part of EngineView /status reads the live
// session from. /status used to read a copy (state.AppState) that nothing
// kept current: the session ID was never set on the paths people use.
type StatusSource interface {
	Model() string
	PermissionMode() permission.Mode
	SessionID() string
}

type PermissionManager interface {
	Mode() permission.Mode
	SetMode(mode permission.Mode)
}

type MCPPool interface {
	Connect(ctx context.Context, name string, cfg mcp.ServerConfig) error
	Disconnect(name string)
	DisconnectAll()
	AllServers() []*mcp.ManagedServer
	AllTools() []mcp.ToolRef
	AllResources() []mcp.ResourceRef
	ReadResource(ctx context.Context, serverName, uri string) (*mcp.ReadResourceResult, error)
}

type Input struct {
	// Raw is the whole line as typed ("/attach a b.png"); Args are its
	// fields after the command name.
	Raw               string
	Args              []string
	Cwd               string
	Config            *config.Config
	SaveConfig        func(*config.Config) error
	Engine            EngineView
	SessionStore      SessionStore
	PluginManager     PluginManager
	SkillManager      SkillManager
	MemoryStore       MemoryStore
	PermissionManager PermissionManager
	MCPPool           MCPPool
	ProjectContext    *ctxt.ProjectContext
}

type Output struct {
	Message string
	Data    string
}

type Command interface {
	Name() string
	Aliases() []string
	Description() string
	Help() string
	Execute(ctx context.Context, input Input) (Output, error)
}

// The registry is the one list of slash commands: what runs, what /help and
// completion show, and what a front end refuses while a task runs are all
// read from the commands registered in it. A command declares the rest of
// its metadata through these optional interfaces.

// EngineMutator is a command that, with these arguments, rewrites state a
// running task uses (the history, the working directory, the provider); a
// front end refuses it while a task runs.
type EngineMutator interface {
	MutatesEngine(args []string) bool
}

// ArgHinter offers completions for a command's first argument.
type ArgHinter interface {
	ArgHints() []string
}

// Categorized names the /help section a command is listed under.
type Categorized interface {
	Category() string
}

// Mutates reports whether c with args must not run while a task runs.
func Mutates(c Command, args []string) bool {
	m, ok := c.(EngineMutator)
	return ok && m.MutatesEngine(args)
}

type Registry struct {
	cmds  map[string]Command
	order []string
}

func NewRegistry() *Registry {
	return &Registry{cmds: make(map[string]Command)}
}

// Register adds c. A command already registered under the same name is
// replaced in place: a front end registers its own /resume or /config over
// the generic one, and there is still one entry per name.
func (r *Registry) Register(c Command) {
	if old, ok := r.cmds[c.Name()]; ok && old.Name() == c.Name() {
		for _, a := range old.Aliases() {
			if r.cmds[a] == old {
				delete(r.cmds, a)
			}
		}
	} else {
		r.order = append(r.order, c.Name())
	}
	r.cmds[c.Name()] = c
	for _, a := range c.Aliases() {
		r.cmds[a] = c
	}
}

func (r *Registry) Find(name string) (Command, bool) {
	c, ok := r.cmds[name]
	return c, ok
}

func (r *Registry) All() []Command {
	var res []Command
	seen := map[string]bool{}
	for _, n := range r.order {
		c := r.cmds[n]
		if !seen[c.Name()] {
			seen[c.Name()] = true
			res = append(res, c)
		}
	}
	return res
}
