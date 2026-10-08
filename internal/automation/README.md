# Opt-in Maintenance

Version 1 persists project-keyed specifications and inbox results outside the
original project, under `COVE_CONFIG_DIR/automations` (normally
`~/.cove/automations`). State replacement uses fsatomic and transactions use
filelock. Add/import never starts a worker. There is no cloud scheduler,
implicit background daemon, lifecycle hook, apply, merge or push operation.

## CLI Integration

The coordinator must register every command returned by
`fe.automationCommands()` in `frontend.install`, alongside its other frontend
commands. This method only builds metadata/closures; storage and runners are
initialized on Execute, so a nil frontend is safe for documentation tests.

Before ordinary argument parsing in main, call:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
if handled, code := RunAutomationCLI(ctx, os.Args[1:], os.Stdout, os.Stderr); handled {
    os.Exit(code)
}
```

Do not add automation flags to the ordinary parser if using this early dispatch.
Normal arguments are not intercepted. Workers use existing `cove -p`,
`--max-turns`, `--no-auto`, a private config directory, and a positive budget
share per attempt. The private config strips profiles, MCP, experimental tools,
embedding calls and automatic verification/self-review. Explicit verification
argv is executed and persisted by the maintenance executor instead.

## Operator Workflow

Save a specification such as this outside the original project:

```json
{
  "version": 1,
  "spec": {
    "id": "maintenance",
    "prompt": "Fix one small failing test and explain the changes",
    "enabled": true,
    "every_seconds": 3600,
    "event": "tests-failed",
    "timeout_seconds": 300,
    "budget_usd": 1,
    "max_turns": 8,
    "retries": 1,
    "verify": [["go", "test", "./internal/automation"]]
  }
}
```

```text
/automations add C:\Temp\maintenance.json
/automations list
/automations run maintenance
/automations tick
/automations event tests-failed build-42
/inbox list
/inbox show <result-id>
/inbox patch <result-id> C:\Temp\result.patch
/inbox review <result-id> accepted
/automations remove maintenance
```

`/automations add <file> session` binds the task to the active session; standalone
ticks/events intentionally skip session-bound tasks. `every_seconds: 0` disables
scheduling, not manual/event execution. `enabled: false` disables all execution.
An interval must be at least 60 seconds. Replace a spec by removing it and
adding a new spec; this retains previous inbox results. Remove cannot stop a
running task. Ctrl+C cancels the current foreground worker.

After coordinator wiring, standalone commands (for example from Windows Task
Scheduler, using a full executable path) are:

```text
cove --automation add D:\project C:\Temp\maintenance.json
cove --automation tick D:\project
cove --automation event D:\project tests-failed build-42
cove --automation-inbox list D:\project
cove --automation-inbox review D:\project <result-id> rejected
```

The scheduler must invoke tick periodically; tick does one sequential due scan
and exits. It coalesces missed intervals into one run, rather than catching up
all missed periods. An external event producer invokes event with its stable,
unique occurrence key. A key is consumed even when that run fails or crashes.
No events are monitored implicitly.

## Execution And Recovery

- The project must be the root of a Git repository with a committed HEAD.
  Non-Git projects are refused before starting the model or writing project files.
- Jobs start from committed HEAD, not the user's dirty or untracked files.
  Each attempt uses a fresh detached temporary worktree. Patch capture includes
  binary changes and nonignored untracked files, relative to the recorded HEAD,
  even if the model creates a local commit. No result is automatically applied.
- The timeout is shared across attempts and verification (maximum one hour).
  Patch capture and worktree cleanup each have an additional 15-second bound
  per attempt; the two-hour claim lease exceeds the whole execution bound.
  There is one active job per project, coordinated by a cross-process job lock.
- Up to two retries are allowed only after completed failures, with a fixed
  share `budget_usd / (retries + 1)` per attempt. Unused shares are not recycled.
  Budget enforcement inherits cove's estimated cost tracker: an already in-flight
  provider response can overshoot a cap. This is not a provider-side spending
  guarantee. Logs/patches are bounded at 4 MiB per command; overflow is a failure,
  not a successful silently truncated patch.
- States are running, succeeded, failed, interrupted and uncertain, independent
  of pending/accepted/rejected review. Cancellation/timeouts are interrupted,
  never silently retried. Expired claims become uncertain on the next command or
  Recover call. Unknown outcomes block further runs until explicitly reviewed;
  they are not treated as safe failures. Inbox state is retained after removal.
- A crash during terminal persistence leaves the prior running claim, ultimately
  uncertain. Inspect artifacts and review before scheduling again. Crash-created
  worktrees may need manual cleanup; cleanup failures report the retained path.
- Worktrees isolate normal file edits, not hostile code. Git administration data
  is shared, and shell tools/build scripts/model providers can access the host
  and network. Windows jobs and Unix process groups bound normal descendant
  processes, not a security sandbox. Run only trusted projects and commands;
  do not assume filesystem/network/process escape prevention.
- Results contain outputs and patches that may contain secrets; state uses
  owner-only file permissions where supported. Private worker configs contain
  the provider credentials during execution and are deleted on normal return;
  a host crash may leave temporary configs requiring manual cleanup.

## Embedding API

`Open`, `Store.Add`, `Read`, `Remove`, `Review`, `Recover`, `Executor.Run`,
`Executor.RunDue`, `Executor.Trigger`, `Runner`, and `CommandRunner` are available
for explicit integrations. Consumers must opt into a runner and invoke APIs;
the package does not launch anything on import or store initialization.
