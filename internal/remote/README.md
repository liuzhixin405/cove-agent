# Opt-in active CLI supervision (protocol v1)

This package is not the standalone gomobile provider. It controls an active
interactive CLI task runner through an owning-thread queue. It starts nothing
automatically, reads no arbitrary files, exposes no shell endpoint, and never
logs credentials or model transcripts. No additional dependencies are needed.

## Active CLI wiring

1. In `frontend.install`, register every command from `fe.remoteCommands()` on
   the same registry as other frontend commands. Construction is nil-fe safe;
   execution requires matching non-nil `fe.eng` and `fe.tasks.eng`.
2. `runREPL` installs `LineReader.SetOwnerEventHook(fe.remoteWake, callback)`.
   The callback calls `fe.pollRemote()` on the owning REPL thread on wake and
   a 250 ms input-wait heartbeat, including fallback/piped stdin. The wake
   channel belongs to the registered command and survives stop/start, allowing
   permission callbacks to reach the owner even when the listener is disabled.
   The heartbeat refreshes task completion and reconnect snapshots. Unchanged
   snapshots do not advance `event_id` or `updated`.
   Do not call these frontend methods from HTTP, task, timer callback, or
   permission worker goroutines. Integrate with the actual REPL input event
   loop, not a ticker goroutine accessing frontend state.
3. `runREPL` defers `fe.stopRemote(context.Background())`. It closes the owner
   bridge with `fe.closeRemoteOwner()` before exit/restart waits for tasks,
   releasing callbacks and revoking pending/delivered approvals. Shutdown is
   bounded to five seconds and removes the credential file. `/remote status`
   surfaces server failures. A coordinator's later `closeWorkflows` may call
   stop again safely; another remote lifecycle hook is not required.
4. Keep processing remote wake/heartbeat while the user is typing or a local
   permission prompt is pending. A blocking read without an owner-thread event
   pump cannot deliver queued controls. The input-wait worker reads only the
   underlying byte source once; the owner alone accesses bufio state, rune
   editing, escape parsing and paste detection. It is joined by its read result
   before the next read, so idle input does not spawn successive blocked reads.

No new frontend fields or global frontend map are required: the registered
`*remoteCommand` owns its controller, owner request queue and wake channel.
HTTP threads enqueue Hub actions only. Permission goroutines use `onOwner` to
register and consume approvals; they never read the owner-only controller,
engine session or runner fields. Before serving permission requests, the owner
drains queued HTTP actions so cancellation precedes permit consumption.

## CLI and listener

```
/remote start
/remote status
/remote stop
/remote start --token-file C:\Users\YOU\remote.token --public-origin https://phone.example
/remote start --bind 192.168.1.10:8443 --allow-lan --tls-cert CERT.pem --tls-key KEY.pem
```

The default is an ephemeral `127.0.0.1` port, printed with the private credential
file path, never the token. A specified token file must be NEW: existing files
are never overwritten or imported. Each start generates a 256-bit token.
Windows uses a protected current-user-only DACL, applied through an explicitly
authorized handle after matching the original file identity; other platforms
use mode 0600. If securing the file fails, no token is written and startup fails.
Choose a user-owned directory; do not publish/sync the token file. Stop deletes
it. After a crash, delete the old credential file locally before reusing its
name. Filesystem privacy does not protect against the same OS user or admins.

Phone access uses the user's authenticated HTTPS tunnel, explicitly configured
with `--public-origin`. Forward to loopback. Preserve the configured public Host
for browser clients. Forwarded headers are not trusted. A proxy rewriting Host
to the local listener is usable by curl without Origin, but browser Origin must
match the actual forwarded Host. Do not disable these checks to fit a proxy.
Private-LAN literal IP binds require both `--allow-lan` and TLS cert/key (TLS
1.2 minimum); trust the certificate on the client. Wildcard, empty host, public
IP and hostname binds are refused even with TLS. No plaintext LAN credentials.

## HTTP API

All endpoints require `Authorization: Bearer TOKEN`. URL query parameters,
unknown Host, mismatched/unconfigured Origin, cross-site/same-site fetches,
CORS/preflight, unknown fields, non-JSON bodies and trailing JSON are refused.
No auth cookies, wildcard CORS or URL token authentication. Responses use
`Cache-Control: no-store`. Header/read/write timeouts are 3/5/5 seconds,
idle timeout 30 seconds, maximum headers 8 KiB, bodies 16 KiB, steer text 8 KiB.

- `GET /v1/status`: cached scope, running/paused state, bounded readable task
  preview, queue size, elapsed time, pending guidance and pending approval.
  `updated` is owner-thread publication time; `event_id` is monotonic in this
  server instance. It is a polling reconnect snapshot, not an SSE history.
- `POST /v1/actions`: `{id, scope, kind, text?, approval_id?}`. Copy `scope`
  verbatim from a fresh status response: session, project, task and version.
  Version is a decimal JSON STRING to avoid 64-bit client precision loss.
  Kinds: `steer`, `cancel`, `pause`, `approve`, `deny` only.
- `GET /v1/actions/ID`: `queued`, `applied`, or `rejected`, with event ID and
  error. HTTP 202 means queued, not executed. Approval `applied` means delivered
  to the permission hook, not that the tool executed or was finally authorized.

Use a new globally unique request ID for each intentional action. Repeated IDs
return 409, never replay actions, including rejected actions. On uncertain
network response, GET the original ID; do not retry with a new ID. If 404,
resubmit the SAME ID while the same server instance/scope is active. Do not
automatically repeat controls across server restart/session change. Queue cap
is 128, result/ID cap 4096 per server start; saturation returns 503 without
evicting replay protection. A drifted scope returns 409 at submission or a
rejected result at owner-thread consumption.

The runner adapter rechecks scope and mutates under `replTaskRunner.mu`.
Steer uses real `Engine.Steer` only while running, never queues a new task.
Cancel requests context cancellation; it does not undo previous side effects
or guarantee a tool that ignores cancellation has stopped. Pause persists the
queue's paused state; it does NOT freeze an executing tool. Resume/retry is a
local `/tasks` decision. The engine's synchronized PendingSteer getter is used;
Messages/provider/cost getters are not read from HTTP goroutines.

## PowerShell curl client (no secret in argv or URL)

Run locally in a private terminal. Replace URL/path with those printed by start;
do not paste the token into chat, logs, screenshots or shell history.

```powershell
$base = 'http://127.0.0.1:PORT'
$tokenFile = 'C:\Users\YOU\remote.token'
$status = ('Authorization: Bearer ' + (Get-Content -Raw $tokenFile).Trim()) | curl.exe --silent --show-error --fail --header @- "$base/v1/status"
$snapshot = $status | ConvertFrom-Json
$id = [guid]::NewGuid().ToString()
$action = @{ id=$id; scope=$snapshot.scope; kind='steer'; text='Do not modify files; inspect only.' }
$action | ConvertTo-Json -Depth 5 -Compress | Set-Content -Encoding ascii action.json
('Authorization: Bearer ' + (Get-Content -Raw $tokenFile).Trim()) | curl.exe --silent --show-error --fail --header @- --header 'Content-Type: application/json' --data-binary '@action.json' "$base/v1/actions"
('Authorization: Bearer ' + (Get-Content -Raw $tokenFile).Trim()) | curl.exe --silent --show-error --fail --header @- "$base/v1/actions/$id"
```

For cancel/pause refresh status, create a new ID and change kind; omit text.
For approve/deny also include `approval_id=$snapshot.pending.id`, refreshing
status first. Use a properly trusted HTTPS URL for LAN/tunnel, never `-k`.
The example's supplementary input is ASCII; non-ASCII clients must write UTF-8
JSON. The pipe sends the Authorization header directly to curl stdin and does
not display it or put it in the curl process command line.

## Exact pending approval handoff

`Hub.PendingApproval(scope, canonicalToolName, exactInputJSON, safeSummary, ttl)`
returns the published `Pending` and a buffered `<-chan *Permit`. TTL must be
positive and at most five minutes. Only one pending operation is supported;
replacement cancels the old pending and revokes already-delivered permits.
Use a safe summary, not a full sensitive tool payload. Digest covers canonical
tool name and normalized JSON with numbers preserved. IDs are random.

`fe.installRemotePermissionPrompt()` installs the real `eng.PermissionPrompt`
used by the engine DAsk path, after the existing tool/plan/policy refusal
checks. It retains `askToolPermission` and the local `AskWith` relay. Remote
approval never changes mode, persists rules, or bypasses a deny decision.

`AskSpec.External` registers an external answer channel and cleanup inside the
same `AskWith` serialization lock. `ExternalAnswer.Validate` runs only when
that answer is selected. A local answer/cancellation received during validation
wins; cleanup completes before another prompt can register. Local y/a/p/n,
questions, and limit prompts keep their existing behavior.

The permission callback marshals the actual tool input, requests publication on
the owner, and waits without holding runner.mu. Remote TTL is the smaller of
the local prompt timeout and five minutes. Local answers, timeout, drift,
replacement, cancel and service stop revoke the pending ID; nil delivery means
denial. With no active listener, the prompt remains local-only.

At external-answer validation, the callback marshals the current input again.
The owner verifies the same engine and Hub, re-reads current
session/project/task/version under runner.mu, publishes drift, and calls
`permit.Consume(currentScope, actualTool, actualJSON)`. Consume is single-use
even on a failed comparison. The prompting engine task is suspended throughout
this validation, and owner-controlled session/project replacement cannot run
inside the owner callback. This is not a filesystem/external-resource
compare-and-swap or a new tool-execution transaction; another process can
change those resources. The engine's bool permission API does not expose the
task context to the callback; cancellation uses its existing local relay and
remote cancellation hook, with TTL bounding unattended pending approval.

Unsupported in v1: headless/gomobile
control, multiple simultaneous approvals, arbitrary commands/file access,
remote queue resume, automatic action retries, event-history replay, browser
UI, exposure without authenticated tunnel/TLS, undo of tool side effects.

## Focused verification

```
go test ./internal/remote
go test ./cli/cove -run '^TestRemote' -count=1
go test -race ./internal/repl ./internal/remote ./cli/cove -run '^Test(Remote|HTTP|Bind|Credential|Approval|Delivered|Drift|Concurrent|OwnerInput|AskWith|UnchangedSnapshot)' -count=1
```

Tests use real `runREPL`, piped stdin, a loopback HTTP listener, actual tool
side effects in temporary projects, and a local fake OpenAI server. Sixteen
clients submitting the same request ID queue it once; replay, old buttons,
cancel, stop, expiry and exit cannot execute an unapproved tool. Other tests
cover canonical input, all scope fields, single-use permit consumption, raw
partial/escape/paste preservation and local prompt serialization. No paid API
calls or credential output are used. Temporarily disconnecting the production
owner hook made `TestRemoteRealREPLApprovalWithoutTyping` fail because silent
stdin could not publish pending approval; restoring it passed under `-race`.
