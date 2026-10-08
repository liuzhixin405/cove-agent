package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/remote"
	"github.com/liuzhixin405/cove-agent/internal/repl"
)

type remoteController struct {
	hub    *remote.Hub
	server *remote.Server
}

type remoteCommand struct {
	*feCmd
	fe         *frontend
	controller *remoteController
	ownerCalls chan remoteOwnerCall
	ownerWake  chan struct{}
	ownerDone  chan struct{}
	closeOnce  sync.Once
}

type remoteOwnerCall struct {
	run  func()
	done chan struct{}
}

func (fe *frontend) remoteCommands() []command.Command {
	return []command.Command{&remoteCommand{fe: fe, ownerCalls: make(chan remoteOwnerCall, 32), ownerWake: make(chan struct{}, 1), ownerDone: make(chan struct{}), feCmd: &feCmd{
		name: "remote", desc: "Opt-in authenticated remote supervision", category: catSystem,
		hints: []string{"start", "status", "stop"},
		help:  "/remote start [--bind 127.0.0.1:0] [--token-file NEW_PATH] [--public-origin https://HOST] [--allow-lan --tls-cert CERT --tls-key KEY] | status | stop",
	}}}
}

func (c *remoteCommand) Execute(ctx context.Context, in command.Input) (command.Output, error) {
	if c.fe == nil || c.fe.eng == nil || c.fe.tasks == nil {
		return command.Output{}, errors.New("remote requires an active interactive CLI session")
	}
	args := in.Args
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "start":
		if c.controller != nil {
			return command.Output{}, errors.New("remote already started")
		}
		var cfg remote.Config
		flags := flag.NewFlagSet("remote", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&cfg.Address, "bind", "127.0.0.1:0", "")
		flags.StringVar(&cfg.TokenFile, "token-file", "", "")
		flags.StringVar(&cfg.PublicOrigin, "public-origin", "", "")
		flags.BoolVar(&cfg.AllowLAN, "allow-lan", false, "")
		flags.StringVar(&cfg.TLSCert, "tls-cert", "", "")
		flags.StringVar(&cfg.TLSKey, "tls-key", "", "")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return command.Output{}, errors.New("invalid /remote start options; see /help remote")
		}
		hub := remote.NewHubWithWake(c.ownerWake)
		snapshot, err := c.fe.remoteSnapshot()
		if err != nil {
			return command.Output{}, err
		}
		hub.Publish(snapshot)
		server, err := remote.Start(cfg, hub)
		if err != nil {
			hub.Close()
			return command.Output{}, err
		}
		c.controller = &remoteController{hub: hub, server: server}
		return command.Output{Message: fmt.Sprintf("Remote: %s\nPrivate credential file: %s\nPause stops queued tasks only.", server.URL, server.TokenFile)}, nil
	case "status":
		if len(args) != 1 {
			return command.Output{}, errors.New("status takes no arguments")
		}
		if c.controller == nil {
			return command.Output{Message: "Remote disabled"}, nil
		}
		if err := c.controller.server.Err(); err != nil {
			return command.Output{}, err
		}
		return command.Output{Message: "Remote: " + c.controller.server.URL + "\nPrivate credential file: " + c.controller.server.TokenFile}, nil
	case "stop":
		if len(args) != 1 {
			return command.Output{}, errors.New("stop takes no arguments")
		}
		if c.controller == nil {
			return command.Output{Message: "Remote disabled"}, nil
		}
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err := c.controller.server.Stop(stopCtx)
		c.controller = nil
		return command.Output{Message: "Remote stopped"}, err
	default:
		return command.Output{}, errors.New("expected /remote start|status|stop")
	}
}

func (fe *frontend) remoteController() *remoteController {
	if cmd := fe.remoteCommand(); cmd != nil {
		return cmd.controller
	}
	return nil
}

func (fe *frontend) remoteCommand() *remoteCommand {
	if fe == nil || fe.reg == nil {
		return nil
	}
	cmd, ok := fe.reg.Find("remote")
	if !ok {
		return nil
	}
	remoteCmd, ok := cmd.(*remoteCommand)
	if !ok {
		return nil
	}
	return remoteCmd
}

func (fe *frontend) remoteWake() <-chan struct{} {
	if cmd := fe.remoteCommand(); cmd != nil {
		return cmd.ownerWake
	}
	return nil
}

func (fe *frontend) pollRemote() {
	fe.pollRemoteActions()
	if cmd := fe.remoteCommand(); cmd != nil {
		for {
			select {
			case call := <-cmd.ownerCalls:
				select {
				case <-cmd.ownerDone:
				default:
					call.run()
				}
				close(call.done)
			default:
				goto drained
			}
		}
	}
drained:
	fe.pollRemoteActions()
}

func (fe *frontend) pollRemoteActions() {
	controller := fe.remoteController()
	if controller == nil {
		return
	}
	if controller.server != nil && controller.server.Err() != nil {
		controller.hub.Close()
		return
	}
	snapshot, err := fe.remoteSnapshot()
	if err != nil {
		controller.hub.Close()
		return
	}
	controller.hub.Publish(snapshot)
	controller.hub.Drain(func() remote.Snapshot {
		snapshot, err := fe.remoteSnapshot()
		if err != nil {
			return remote.Snapshot{}
		}
		return snapshot
	}, fe.applyRemote)
	if snapshot, err := fe.remoteSnapshot(); err == nil {
		controller.hub.Publish(snapshot)
	}
}

func (c *remoteCommand) onOwner(run func()) bool {
	call := remoteOwnerCall{run: run, done: make(chan struct{})}
	select {
	case <-c.ownerDone:
		return false
	case c.ownerCalls <- call:
	}
	select {
	case c.ownerWake <- struct{}{}:
	default:
	}
	select {
	case <-c.ownerDone:
		return false
	case <-call.done:
		return true
	}
}

func (fe *frontend) closeRemoteOwner() {
	if cmd := fe.remoteCommand(); cmd != nil {
		cmd.closeOnce.Do(func() { close(cmd.ownerDone) })
		if cmd.controller != nil {
			cmd.controller.hub.Close()
		}
	}
}

func (fe *frontend) revokeRemoteApproval() {
	if controller := fe.remoteController(); controller != nil {
		controller.hub.RevokeApproval()
	}
}

func (fe *frontend) installRemotePermissionPrompt() {
	cmd := fe.remoteCommand()
	if cmd == nil || fe.eng == nil {
		return
	}
	eng := fe.eng
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		return askToolPermissionExternal(eng, toolName, input, reason, func() (<-chan repl.ExternalAnswer, func()) {
			return cmd.permissionAnswer(eng, toolName, input, reason)
		})
	}
}

func (c *remoteCommand) permissionAnswer(eng *engine.Engine, toolName string, input map[string]any, reason string) (<-chan repl.ExternalAnswer, func()) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, nil
	}
	var hub *remote.Hub
	var pending remote.Pending
	var delivery <-chan *remote.Permit
	registered := c.onOwner(func() {
		if c.controller == nil || c.fe.eng != eng {
			return
		}
		snapshot, snapshotErr := c.fe.remoteSnapshot()
		if snapshotErr != nil || !snapshot.Running {
			return
		}
		hub = c.controller.hub
		hub.Publish(snapshot)
		ttl := permissionPromptTimeout
		if ttl > 5*time.Minute {
			ttl = 5 * time.Minute
		}
		pending, delivery, err = hub.PendingApproval(snapshot.Scope, toolName, encoded, permissionPromptDescription(input, reason), ttl)
	})
	if !registered {
		answer := make(chan repl.ExternalAnswer, 1)
		answer <- repl.ExternalAnswer{Answer: "n"}
		return answer, nil
	}
	if err != nil || delivery == nil {
		return nil, nil
	}
	answers := make(chan repl.ExternalAnswer, 1)
	stop := make(chan struct{})
	go func() {
		select {
		case permit := <-delivery:
			if permit == nil {
				answers <- repl.ExternalAnswer{Answer: "n"}
				return
			}
			answers <- repl.ExternalAnswer{Answer: "y", Validate: func() bool {
				currentInput, marshalErr := json.Marshal(input)
				if marshalErr != nil {
					return false
				}
				allowed := false
				consumed := c.onOwner(func() {
					if c.controller == nil || c.controller.hub != hub || c.fe.eng != eng {
						return
					}
					snapshot, snapshotErr := c.fe.remoteSnapshot()
					if snapshotErr == nil && snapshot.Running {
						hub.Publish(snapshot)
						allowed = permit.Consume(snapshot.Scope, toolName, currentInput)
					}
				})
				return consumed && allowed
			}}
		case <-c.ownerDone:
			answers <- repl.ExternalAnswer{Answer: "n"}
		case <-stop:
		}
	}()
	return answers, func() {
		close(stop)
		hub.CancelApproval(pending.ID)
	}
}

func (fe *frontend) stopRemote(ctx context.Context) error {
	if fe == nil || fe.reg == nil {
		return nil
	}
	cmd, ok := fe.reg.Find("remote")
	if !ok {
		return nil
	}
	remoteCmd, ok := cmd.(*remoteCommand)
	if !ok || remoteCmd.controller == nil {
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var err error
	if remoteCmd.controller.server != nil {
		err = remoteCmd.controller.server.Stop(stopCtx)
	} else {
		remoteCmd.controller.hub.Close()
	}
	remoteCmd.controller = nil
	return err
}

func (fe *frontend) remoteSnapshot() (remote.Snapshot, error) {
	if fe == nil || fe.eng == nil || fe.tasks == nil || fe.tasks.eng != fe.eng {
		return remote.Snapshot{}, remote.ErrUnavailable
	}
	project, err := os.Getwd()
	if err != nil {
		return remote.Snapshot{}, err
	}
	session := fe.eng.SessionID()
	fe.tasks.mu.Lock()
	defer fe.tasks.mu.Unlock()
	return fe.tasks.remoteSnapshotLocked(session, project), nil
}

func (r *replTaskRunner) remoteSnapshotLocked(session, project string) remote.Snapshot {
	pending := ""
	if r.eng != nil {
		pending, _ = r.eng.PendingSteer()
	}
	state, _ := json.Marshal(struct {
		Current                  any
		Start                    time.Time
		Running, Paused, Closing bool
		Queue                    any
		Pending                  string
	}{r.current, r.currentStart, r.running, r.paused, r.closing, r.queue, pending})
	digest := sha256.Sum256(state)
	taskDigest := sha256.Sum256([]byte(r.currentStart.String()))
	task := ""
	if r.running {
		task = hex.EncodeToString(taskDigest[:16])
	}
	scope := remote.Scope{Session: session, Project: project, Task: task, Version: binary.BigEndian.Uint64(digest[:8])}
	evidence := fmt.Sprintf("running=%t paused=%t queued=%d elapsed=%s", r.running, r.paused, len(r.queue), time.Since(r.currentStart).Truncate(time.Second))
	if !r.running {
		evidence = fmt.Sprintf("running=false paused=%t queued=%d", r.paused, len(r.queue))
	}
	if r.running {
		evidence += "\nTask: " + taskPreview(r.current)
	}
	if pending != "" {
		evidence += "\nPending guidance: " + taskPreviewMessage(pending)
	}
	if r.persistenceError != "" {
		evidence += "\nQueue persistence failed"
	}
	return remote.Snapshot{Scope: scope, Running: r.running, Paused: r.paused, Evidence: evidence}
}

func taskPreviewMessage(text string) string {
	words := strings.Join(strings.Fields(text), " ")
	runes := []rune(words)
	if len(runes) > 120 {
		words = string(runes[:120])
	}
	return words
}

func (fe *frontend) applyRemote(action remote.Action) error {
	project, err := os.Getwd()
	if err != nil {
		return err
	}
	if fe == nil || fe.tasks == nil || fe.eng == nil || fe.tasks.eng != fe.eng {
		return remote.ErrUnavailable
	}
	session := fe.eng.SessionID()
	r := fe.tasks
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing || action.Scope != r.remoteSnapshotLocked(session, project).Scope {
		return remote.ErrDrift
	}
	switch action.Kind {
	case "steer":
		if !r.running || r.eng == nil || strings.TrimSpace(action.Text) == "" {
			return errors.New("steer requires a running task and nonempty text")
		}
		r.eng.Steer(action.Text)
		_, count := r.eng.PendingSteer()
		repl.SetSteerCount(count)
	case "cancel":
		if !r.running || r.cancel == nil {
			return errors.New("no cancellable task")
		}
		r.cancel()
		fe.revokeRemoteApproval()
		denyPendingPermissionPrompt()
	case "pause":
		r.paused = true
		if !r.persistQueueLocked() {
			return errors.New("queue persistence failed; queue remains paused")
		}
	default:
		return errors.New("unsupported runner action")
	}
	return nil
}
