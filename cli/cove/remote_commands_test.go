package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/remote"
	"github.com/liuzhixin405/cove-agent/internal/repl"
)

func TestRemoteCommandsNilConstruction(t *testing.T) {
	var fe *frontend
	commands := fe.remoteCommands()
	if len(commands) != 1 || commands[0].Name() != "remote" || command.Mutates(commands[0], []string{"start"}) {
		t.Fatal("command metadata invalid")
	}
	if _, err := commands[0].Execute(context.Background(), command.Input{}); err == nil {
		t.Fatal("nil frontend accepted")
	}
}

func TestRemoteHTTPRealRunner(t *testing.T) {
	captureTurnOutput(t)
	eng := steerTestEngine(t)
	eng.SetAutoExtract(false)
	provider := &blockingProvider{firstStarted: make(chan struct{}), release: make(chan struct{})}
	eng.SetProvider(provider)
	runner := newREPLTaskRunner(eng)
	t.Cleanup(func() {
		runner.CancelForExit()
		if !runner.WaitIdleUntil(time.Now().Add(5 * time.Second)) {
			t.Error("runner did not stop")
		}
	})
	runner.Enqueue(api.Message{Role: "user", Content: "inspect only"})
	select {
	case <-provider.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not start")
	}
	fe := &frontend{eng: eng, tasks: runner, reg: command.NewRegistry()}
	cmd := fe.remoteCommands()[0].(*remoteCommand)
	hub := remote.NewHub()
	cmd.controller = &remoteController{hub: hub}
	fe.reg.Register(cmd)
	snapshot, err := fe.remoteSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	hub.Publish(snapshot)
	token := strings.Repeat("a", 64)
	handler, err := remote.NewHandler(hub, token, []string{"http://127.0.0.1:9999"})
	if err != nil {
		t.Fatal(err)
	}
	send := func(id, kind, text string) {
		action := remote.Action{ID: id, Scope: hub.Snapshot().Scope, Kind: kind, Text: text}
		body, _ := json.Marshal(action)
		request := httptest.NewRequest("POST", "http://127.0.0.1:9999/v1/actions", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 202 {
			t.Fatalf("%s: %d %s", kind, response.Code, response.Body)
		}
		fe.pollRemote()
		result, _ := hub.Result(id)
		if result.State != "applied" {
			t.Fatalf("%s: %+v", kind, result)
		}
	}
	send("steer", "steer", "do not write files")
	if text, count := eng.PendingSteer(); text != "do not write files" || count != 1 {
		t.Fatalf("real steer=%q count=%d", text, count)
	}
	if err := fe.applyRemote(remote.Action{ID: "stale", Scope: snapshot.Scope, Kind: "cancel"}); err != remote.ErrDrift {
		t.Fatalf("stale action: %v", err)
	}
	send("pause", "pause", "")
	if !runner.Snapshot().Paused {
		t.Fatal("queue not paused")
	}
	send("cancel", "cancel", "")
	if !runner.WaitIdleUntil(time.Now().Add(5 * time.Second)) {
		t.Fatal("HTTP cancel did not reach provider context")
	}
	fe.pollRemote()
	if hub.Snapshot().Running || !hub.Snapshot().Paused || len(provider.requests()) != 1 {
		t.Fatalf("unexpected final state=%+v requests=%d", hub.Snapshot(), len(provider.requests()))
	}
}

func TestRemoteCommandLifecycle(t *testing.T) {
	eng := steerTestEngine(t)
	fe := &frontend{eng: eng, tasks: newREPLTaskRunner(eng), reg: command.NewRegistry()}
	cmd := fe.remoteCommands()[0]
	fe.reg.Register(cmd)
	t.Cleanup(func() { fe.stopRemote(context.Background()) })
	output, err := cmd.Execute(context.Background(), command.Input{Args: []string{"start"}})
	if err != nil {
		t.Fatal(err)
	}
	if fe.remoteWake() == nil || !strings.Contains(output.Message, "私有凭据文件:") {
		t.Fatal("start did not connect lifecycle")
	}
	if _, err := cmd.Execute(context.Background(), command.Input{Args: []string{"start"}}); err == nil {
		t.Fatal("double start accepted")
	}
	if err := fe.stopRemote(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fe.remoteController() != nil || fe.remoteWake() == nil {
		t.Fatal("stopped controller retained")
	}
}

type remoteTestClient struct {
	t     *testing.T
	url   string
	token string
	http  *http.Client
}

func newRemoteTestClient(t *testing.T, session *e2eSession) *remoteTestClient {
	t.Helper()
	session.Type("/remote start")
	session.WaitFor("私有凭据文件:", e2eTimeout)
	var address, credential string
	for _, line := range strings.Split(session.Output(), "\n") {
		line = strings.TrimSpace(line)
		if at := strings.Index(line, "远程地址: http://"); at >= 0 {
			address = strings.TrimSpace(line[at+len("远程地址: "):])
		}
		if at := strings.Index(line, "私有凭据文件:"); at >= 0 {
			credential = strings.TrimSpace(line[at+len("私有凭据文件:"):])
		}
	}
	if address == "" || credential == "" {
		t.Fatal("remote endpoint or credential path missing")
	}
	secret, err := os.ReadFile(credential)
	if err != nil {
		t.Fatal("credential file unavailable")
	}
	return &remoteTestClient{t: t, url: address, token: strings.TrimSpace(string(secret)), http: &http.Client{Timeout: 3 * time.Second}}
}

func (client *remoteTestClient) request(method, path string, payload any, want int, out any) int {
	client.t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			client.t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, client.url+path, body)
	if err != nil {
		client.t.Fatal("cannot construct remote request")
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		client.t.Fatal("remote request failed")
	}
	defer response.Body.Close()
	if want != 0 && response.StatusCode != want {
		client.t.Fatalf("%s %s: status=%d want=%d", method, path, response.StatusCode, want)
	}
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			client.t.Fatal("remote response is not valid JSON")
		}
	}
	return response.StatusCode
}

func (client *remoteTestClient) pending(previous string) remote.Snapshot {
	client.t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for time.Now().Before(deadline) {
		var snapshot remote.Snapshot
		client.request("GET", "/v1/status", nil, 200, &snapshot)
		if snapshot.Pending != nil && snapshot.Pending.ID != previous {
			return snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	client.t.Fatal("real REPL did not publish a pending approval while stdin was silent")
	return remote.Snapshot{}
}

func (client *remoteTestClient) result(id, state string) remote.Result {
	client.t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for time.Now().Before(deadline) {
		var result remote.Result
		client.request("GET", "/v1/actions/"+id, nil, 200, &result)
		if result.State == state {
			return result
		}
		if result.State != "queued" {
			client.t.Fatalf("result=%+v want=%s", result, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	client.t.Fatalf("remote action %s did not reach %s without keyboard input", id, state)
	return remote.Result{}
}

func TestRemoteRealREPLApprovalWithoutTyping(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir remote_first"}`}}},
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir remote_second"}`}}},
		fakeStep{Content: "remote approvals completed with no keyboard answers"},
	)
	_, project := e2eHome(t, model)
	session := startREPL(t)
	client := newRemoteTestClient(t, session)
	session.Type("create the two requested directories")
	first := client.pending("")
	if first.Pending.Tool != "bash" || first.Pending.Digest == "" || first.Pending.Scope != first.Scope {
		t.Fatal("real permission pending is not bound to the exact tool and scope")
	}
	if _, err := os.Stat(filepath.Join(project, "remote_first")); !os.IsNotExist(err) {
		t.Fatal("tool executed before approval")
	}
	action := remote.Action{ID: "approve-first", Scope: first.Scope, Kind: "approve", ApprovalID: first.Pending.ID}
	statuses := make(chan int, 16)
	for count := 0; count < cap(statuses); count++ {
		go func() { statuses <- client.request("POST", "/v1/actions", action, 0, nil) }()
	}
	accepted := 0
	for count := 0; count < cap(statuses); count++ {
		switch status := <-statuses; status {
		case 202:
			accepted++
		case 409:
		default:
			t.Fatalf("concurrent action status=%d", status)
		}
	}
	if accepted != 1 {
		t.Fatalf("same request ID queued %d times", accepted)
	}
	client.result(action.ID, "applied")
	client.request("POST", "/v1/actions", action, 409, nil)
	second := client.pending(first.Pending.ID)
	if _, err := os.Stat(filepath.Join(project, "remote_first")); err != nil {
		t.Fatal("remote approval did not execute the first real tool")
	}
	if _, err := os.Stat(filepath.Join(project, "remote_second")); !os.IsNotExist(err) {
		t.Fatal("first approval leaked into a second prompt")
	}
	stale := remote.Action{ID: "old-button", Scope: second.Scope, Kind: "approve", ApprovalID: first.Pending.ID}
	client.request("POST", "/v1/actions", stale, 202, nil)
	if got := client.result(stale.ID, "rejected"); got.Error != remote.ErrDrift.Error() {
		t.Fatalf("stale result=%+v", got)
	}
	client.request("POST", "/v1/actions", remote.Action{ID: "approve-second", Scope: second.Scope, Kind: "approve", ApprovalID: second.Pending.ID}, 202, nil)
	client.result("approve-second", "applied")
	session.WaitFor("remote approvals completed with no keyboard answers", e2eTimeout)
	if _, err := os.Stat(filepath.Join(project, "remote_second")); err != nil {
		t.Fatal("second real tool was not executed")
	}
	deadline := time.Now().Add(e2eTimeout)
	var idle remote.Snapshot
	for time.Now().Before(deadline) {
		client.request("GET", "/v1/status", nil, 200, &idle)
		if !idle.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if idle.Running || idle.Pending != nil {
		t.Fatal("idle snapshot did not refresh while stdin was silent")
	}
	time.Sleep(600 * time.Millisecond)
	var unchanged remote.Snapshot
	client.request("GET", "/v1/status", nil, 200, &unchanged)
	if unchanged.EventID != idle.EventID {
		t.Fatal("idle polling generated meaningless events")
	}
	session.Exit()
	if len(model.Requests()) != 3 {
		t.Fatalf("fake model calls=%d want=3", len(model.Requests()))
	}
	t.Log("negative proof: replay=409; old button rejected; second directory absent until its own approval; no stdin answers")
}

func TestRemoteRealREPLPendingFailsClosed(t *testing.T) {
	for _, reason := range []string{"cancel", "stop", "expiry", "exit"} {
		t.Run(reason, func(t *testing.T) {
			oldTimeout := permissionPromptTimeout
			if reason == "expiry" {
				permissionPromptTimeout = 500 * time.Millisecond
			}
			t.Cleanup(func() { permissionPromptTimeout = oldTimeout })
			model := newFakeModel(t,
				fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir remote_forbidden"}`}}},
				fakeStep{Content: "denied operation completed without writing a directory"},
			)
			_, project := e2eHome(t, model)
			session := startREPL(t)
			client := newRemoteTestClient(t, session)
			session.Type("create a directory only if approved")
			pending := client.pending("")
			switch reason {
			case "cancel":
				client.request("POST", "/v1/actions", remote.Action{ID: "cancel-task", Scope: pending.Scope, Kind: "cancel"}, 202, nil)
				client.result("cancel-task", "applied")
			case "stop":
				session.Type("/remote stop")
				session.WaitFor("远程监督已停止", e2eTimeout)
				session.WaitFor("denied operation completed", e2eTimeout)
			case "expiry":
				session.WaitFor("denied operation completed", e2eTimeout)
				// The answer streams before the task runner settles; the
				// scope version changes once more when it does, and a
				// status read taken in between made the action a 409.
				session.WaitIdle(e2eTimeout)
				// The published snapshot lags the live state by up to one
				// owner poll (250 ms); an action built from a stale scope
				// is a 409. Read until the snapshot says idle too.
				var current remote.Snapshot
				for attempt := 0; ; attempt++ {
					client.request("GET", "/v1/status", nil, 200, &current)
					if !current.Running || attempt > 40 {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				client.request("POST", "/v1/actions", remote.Action{ID: "expired", Scope: current.Scope, Kind: "approve", ApprovalID: pending.Pending.ID}, 202, nil)
				client.result("expired", "rejected")
			case "exit":
				session.Exit()
			}
			if _, err := os.Stat(filepath.Join(project, "remote_forbidden")); !os.IsNotExist(err) {
				t.Fatalf("%s executed the denied tool", reason)
			}
			session.Exit()
			t.Logf("negative proof: %s left real tool side effect absent", reason)
		})
	}
}

func TestRemoteOwnerBridgeClosesWithoutReader(t *testing.T) {
	fe := &frontend{reg: command.NewRegistry()}
	cmd := fe.remoteCommands()[0].(*remoteCommand)
	fe.reg.Register(cmd)
	done := make(chan bool, 1)
	go func() { done <- cmd.onOwner(func() { t.Error("closed owner executed callback") }) }()
	fe.closeRemoteOwner()
	select {
	case result := <-done:
		if result {
			t.Fatal("closed owner accepted callback")
		}
	case <-time.After(time.Second):
		t.Fatal("owner bridge did not release its caller")
	}
}

func TestRemoteQueuedCancelPrecedesPermitConsumption(t *testing.T) {
	captureTurnOutput(t)
	eng := steerTestEngine(t)
	eng.SetAutoExtract(false)
	provider := &blockingProvider{firstStarted: make(chan struct{}), release: make(chan struct{})}
	eng.SetProvider(provider)
	runner := newREPLTaskRunner(eng)
	t.Cleanup(func() {
		runner.CancelForExit()
		runner.WaitIdleUntil(time.Now().Add(5 * time.Second))
	})
	runner.Enqueue(api.Message{Role: "user", Content: "wait for approval"})
	select {
	case <-provider.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("real runner did not start")
	}
	fe := &frontend{eng: eng, tasks: runner, reg: command.NewRegistry()}
	cmd := fe.remoteCommands()[0].(*remoteCommand)
	hub := remote.NewHubWithWake(cmd.ownerWake)
	cmd.controller = &remoteController{hub: hub}
	fe.reg.Register(cmd)
	snapshot, err := fe.remoteSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	hub.Publish(snapshot)
	input := json.RawMessage(`{"command":"mkdir forbidden"}`)
	pending, delivery, err := hub.PendingApproval(snapshot.Scope, "bash", input, "mkdir forbidden", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Submit(remote.Action{ID: "approve", Scope: snapshot.Scope, Kind: "approve", ApprovalID: pending.ID}); err != nil {
		t.Fatal(err)
	}
	fe.pollRemote()
	permit := <-delivery
	if _, err := hub.Submit(remote.Action{ID: "cancel-before-consume", Scope: snapshot.Scope, Kind: "cancel"}); err != nil {
		t.Fatal(err)
	}
	consumed := false
	call := remoteOwnerCall{done: make(chan struct{}), run: func() {
		consumed = permit.Consume(snapshot.Scope, "bash", input)
	}}
	cmd.ownerCalls <- call
	fe.pollRemote()
	<-call.done
	if consumed {
		t.Fatal("owner consumed a permit before handling an already queued cancellation")
	}
	if result, _ := hub.Result("cancel-before-consume"); result.State != "applied" {
		t.Fatalf("cancel result=%+v", result)
	}
	if !runner.WaitIdleUntil(time.Now().Add(5 * time.Second)) {
		t.Fatal("real runner ignored cancellation")
	}
}

// A local steer (the user typing a line while the prompt waits), a remote
// pause or a queue change moves the scope version. That only means the
// remote side can no longer approve this prompt; it used to be delivered as
// a remote "n" and the local prompt was refused under the user's hands.
func TestScopeDriftDoesNotDenyTheLocalPermissionPrompt(t *testing.T) {
	captureTurnOutput(t)
	eng := steerTestEngine(t)
	eng.SetAutoExtract(false)
	provider := &blockingProvider{firstStarted: make(chan struct{}), release: make(chan struct{})}
	eng.SetProvider(provider)
	runner := newREPLTaskRunner(eng)
	t.Cleanup(func() {
		runner.CancelForExit()
		runner.WaitIdleUntil(time.Now().Add(5 * time.Second))
	})
	runner.Enqueue(api.Message{Role: "user", Content: "wait for approval"})
	select {
	case <-provider.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("real runner did not start")
	}
	fe := &frontend{eng: eng, tasks: runner, reg: command.NewRegistry()}
	cmd := fe.remoteCommands()[0].(*remoteCommand)
	hub := remote.NewHubWithWake(cmd.ownerWake)
	cmd.controller = &remoteController{hub: hub}
	fe.reg.Register(cmd)
	snapshot, err := fe.remoteSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	hub.Publish(snapshot)
	// permissionAnswer registers the pending on the owner thread, which the
	// test drives by polling, so it runs on its own goroutine.
	type registration struct {
		answers <-chan repl.ExternalAnswer
		cleanup func()
	}
	registered := make(chan registration, 1)
	go func() {
		answers, cleanup := cmd.permissionAnswer(eng, "bash", map[string]any{"command": "mkdir forbidden"}, "mkdir forbidden")
		registered <- registration{answers, cleanup}
	}()
	var reg registration
	deadline := time.Now().Add(5 * time.Second)
	for reg.answers == nil {
		fe.pollRemote()
		select {
		case reg = <-registered:
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("remote answer channel not registered")
			}
		}
	}
	if reg.answers == nil {
		t.Fatal("remote answer channel not registered")
	}
	answers := reg.answers
	defer reg.cleanup()
	if hub.Snapshot().Pending == nil {
		t.Fatal("pending approval was not published")
	}
	// The user types a line while the prompt is open: steer, scope drifts.
	runner.SubmitWithFeedback(api.Message{Role: "user", Content: "be careful"})
	fe.pollRemote()
	if hub.Snapshot().Pending != nil {
		t.Fatal("drifted pending approval still published")
	}
	select {
	case answer := <-answers:
		t.Fatalf("scope drift answered the local prompt with %q", answer.Answer)
	case <-time.After(300 * time.Millisecond):
	}
}
