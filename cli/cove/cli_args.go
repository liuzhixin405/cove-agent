package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/dream"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// cliAction is a flag that does one thing and exits instead of starting a
// session.
type cliAction int

const (
	actionRun cliAction = iota
	actionVersion
	actionHelp
	actionDoctor
	actionConfig
	actionListSessions
	// actionDreamWorker is the hidden --dream-worker <sessions-dir>: one
	// memory consolidation, then exit (started detached at session end).
	actionDreamWorker
)

// cliOptions is the parsed command line.
type cliOptions struct {
	action      cliAction
	listAll     bool
	debug       bool
	dumpPrompt  bool
	noAuto      bool
	tui         bool
	noTUI       bool
	printMode   bool
	printPrompt string
	attachments []string
	resumeID    string
	profile     string
	// dreamWorkerDir is the sessions directory given to --dream-worker.
	dreamWorkerDir string
	// dreamProjectRoot is the hidden --dream-project <root> given before
	// --dream-worker: the project whose memory directory is consolidated too.
	dreamProjectRoot string
	// maxTurns is --max-turns: the -p turn's model-call cap (0 = no cap).
	// maxTurnsSet tells an explicit 0 from the flag being absent.
	maxTurns    int
	maxTurnsSet bool
}

// cliFlags lists every flag parseCLIArgs understands. -p stops short of
// taking one of these as its prompt, so "cove -p --image a.png 描述" keeps
// both the image and the prompt.
var cliFlags = map[string]bool{
	"-v": true, "--version": true, "-h": true, "--help": true,
	"--doctor": true, "--config": true, "--list-sessions": true,
	"--dump-system-prompt": true, "--no-auto": true, "-d": true, "--debug": true,
	"-p": true, "--print": true, "--image": true, "--file": true,
	"-r": true, "--resume": true, "--tui": true, "--no-tui": true,
	"--profile": true, "--max-turns": true,
}

// parseCLIArgs parses os.Args[1:].
//
// It used to be a loop in main that ignored whatever it did not recognise: a
// typo such as --no-tiu, or a bare word without -p, started cove with the
// defaults the user was trying to change, and -r only skipped its argument.
// Unknown flags and stray words are now errors. Action flags (--help,
// --doctor...) stop parsing where they stand, as before.
func parseCLIArgs(args []string) (cliOptions, error) {
	var opts cliOptions
	var words []string

	value := func(i int) (string, error) {
		if i+1 >= len(args) || cliFlags[args[i+1]] {
			return "", fmt.Errorf("参数 %s 需要一个值", args[i])
		}
		return args[i+1], nil
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-v", "--version":
			opts.action = actionVersion
			return opts, nil
		case "-h", "--help":
			opts.action = actionHelp
			return opts, nil
		case "--doctor":
			opts.action = actionDoctor
			return opts, nil
		case "--config":
			opts.action = actionConfig
			return opts, nil
		case "--list-sessions":
			// "--list-sessions all" lists every project's sessions; bare, only
			// the current directory's, matching /history.
			opts.action = actionListSessions
			opts.listAll = i+1 < len(args) && strings.EqualFold(args[i+1], "all")
			return opts, nil
		case dream.WorkerProjectFlag:
			// Hidden, like --dream-worker, which follows it.
			v, err := value(i)
			if err != nil {
				return opts, err
			}
			opts.dreamProjectRoot = v
			i++
		case dream.WorkerFlag:
			// Hidden (not in cliFlags, --help or the manual): only cove's own
			// exit path starts it.
			v, err := value(i)
			if err != nil {
				return opts, err
			}
			opts.action, opts.dreamWorkerDir = actionDreamWorker, v
			return opts, nil
		case "--dump-system-prompt":
			opts.dumpPrompt = true
		case "--no-auto":
			opts.noAuto = true
		case "-d", "--debug":
			opts.debug = true
		case "--tui":
			opts.tui = true
		case "--no-tui":
			opts.noTUI = true
		case "-p", "--print":
			opts.printMode = true
			// The prompt is optional (it can come from stdin), and it may itself
			// start with a dash; only an exact known flag is left alone.
			if i+1 < len(args) && !cliFlags[args[i+1]] {
				i++
				words = append(words, args[i])
			}
		case "--max-turns":
			v, err := value(i)
			if err != nil {
				return opts, err
			}
			i++
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return opts, fmt.Errorf("--max-turns 需要一个非负整数（0 表示不限制），收到 %q", v)
			}
			opts.maxTurns, opts.maxTurnsSet = n, true
		case "--image", "--file", "-r", "--resume", "--profile":
			v, err := value(i)
			if err != nil {
				return opts, err
			}
			i++
			switch arg {
			case "--image", "--file":
				opts.attachments = append(opts.attachments, v)
			case "-r", "--resume":
				opts.resumeID = v
			case "--profile":
				opts.profile = v
			}
		default:
			if strings.HasPrefix(arg, "-") && len(arg) > 1 {
				return opts, fmt.Errorf("未知参数: %s（cove --help 查看可用参数）", arg)
			}
			words = append(words, arg)
		}
	}

	if len(opts.attachments) > 0 && !opts.printMode {
		return opts, fmt.Errorf("--image/--file 只能与 -p 一起使用（交互模式请在输入中用 @路径 或 /attach 添加附件）")
	}
	if opts.maxTurnsSet && !opts.printMode {
		// The interactive shell asks at the cap instead (config
		// max_iterations sets its window).
		return opts, errors.New("--max-turns 只能与 -p 一起使用（交互模式达到上限时会询问是否继续，窗口大小由配置 max_iterations 决定）")
	}
	if len(words) > 0 {
		if !opts.printMode {
			return opts, fmt.Errorf("未知参数: %s（单次询问请用 cove -p \"提示\"）", strings.Join(words, " "))
		}
		// An unquoted prompt arrives as several words; it used to keep only
		// the first one.
		opts.printPrompt = strings.Join(words, " ")
	}
	return opts, nil
}

// sessionLoader is the part of *session.Store that resuming needs.
type sessionLoader interface {
	Load(id string) (*session.Record, error)
}

// resumeStartupSession hands the session named by -r/--resume to resume
// (the engine's ResumeSession, which continues it under its own ID) and
// returns it, with a warning when it belongs to a project other than cwd (as
// /resume does). -r used to be parsed and then dropped, so every
// "cove -r <id>" started an empty conversation.
//
// A missing session is an error and resume is not called: the user asked for
// that conversation, and silently starting a fresh one would look like it had
// been lost.
func resumeStartupSession(store sessionLoader, id, cwd string, resume func(*session.Record)) (*session.Record, string, error) {
	if store == nil {
		return nil, "", errors.New("会话存储不可用")
	}
	// Users copy the file name out of ~/.cove/sessions (<id>.jsonl, or a
	// legacy <id>.json). "index" names the sessions index and is refused.
	id, err := session.ParseSessionID(id)
	if err != nil {
		return nil, "", err
	}
	r, err := store.Load(id)
	if err != nil {
		return nil, "", err
	}
	// Computed before resume, which fills in a legacy record's Cwd.
	warning := session.ProjectMismatchWarning(r, cwd)
	resume(r)
	return r, warning, nil
}

// maxPipedStdin caps what -p reads from a pipe, the same ceiling the headless
// line reader uses for one line.
const maxPipedStdin = 8 * 1024 * 1024

// stdinFirstDataTimeout is how long -p waits for a pipe's first byte when
// nobody is watching (stdinFirstDataWait). Some launchers (IDEs, CI runners,
// other agents) hand a child an open stdin that never gets data or EOF;
// without a limit "cove -p" would hang there forever.
const stdinFirstDataTimeout = 3 * time.Second

// errStdinNoData is readPipedStdin's report that nothing arrived within the
// timeout; the caller warns and goes on without stdin.
var errStdinNoData = errors.New("no data on stdin")

// stdinFirstDataWait is how long -p waits for the first byte of stdin (0 =
// until data or EOF), given stdin's file mode and whether stderr is a
// terminal.
//
// The 3-second limit used to apply to every pipe, and silently: in
// `go test ./... 2>&1 | cove -p "..."` the tests take longer than that
// before printing anything, so the output was dropped and the model
// answered a prompt without it. A pipe whose writer is slow cannot be told
// from a launcher's pipe that will never be written (both are open, both
// are empty), so the choice rests on who is there:
//   - a regular file (cove -p < file) always reaches EOF: no limit;
//   - stderr on a terminal means a person ran the pipeline in a shell, can
//     see the "waiting" notice main prints and can press Ctrl+C: no limit;
//   - otherwise (IDE, CI, another agent — the hanging-launcher case) the
//     limit stays, and main now warns on stderr when it hits.
func stdinFirstDataWait(mode os.FileMode, stderrTerminal bool) time.Duration {
	if mode.IsRegular() || stderrTerminal {
		return 0
	}
	return stdinFirstDataTimeout
}

// utf8BOM is the byte-order mark Windows PowerShell 5 writes at the start of
// text it pipes into a native program.
var utf8BOM = string([]byte{0xEF, 0xBB, 0xBF})

// readPipedStdin reads r to EOF, keeping at most limit bytes (truncated
// reports whether more arrived). When nothing at all arrives within
// firstDataTimeout (0 = no limit) it gives up and returns errStdinNoData,
// leaving the blocked read behind; that only happens in a one-shot -p run
// that is about to exit.
func readPipedStdin(r io.Reader, firstDataTimeout time.Duration, limit int) (string, bool, error) {
	type chunk struct {
		data []byte
		err  error
	}
	first := make(chan chunk, 1)
	go func() {
		buf := make([]byte, 64*1024)
		n, err := r.Read(buf)
		first <- chunk{buf[:n], err}
	}()

	var timeout <-chan time.Time
	if firstDataTimeout > 0 {
		timeout = time.After(firstDataTimeout)
	}
	var c chunk
	select {
	case c = <-first:
	case <-timeout:
		return "", false, errStdinNoData
	}

	var sb strings.Builder
	sb.Write(c.data)
	if c.err != nil && !errors.Is(c.err, io.EOF) {
		return "", false, c.err
	}
	if c.err == nil {
		// Read one byte past the limit to learn whether anything was cut.
		rest, err := io.ReadAll(io.LimitReader(r, int64(limit-sb.Len()+1)))
		if err != nil {
			return "", false, err
		}
		sb.Write(rest)
	}
	// Windows PowerShell adds a byte-order mark to what it pipes in.
	s := strings.TrimPrefix(sb.String(), utf8BOM)
	if len(s) > limit {
		// Cut on a rune boundary: a piped Chinese log split mid-rune would
		// ship invalid UTF-8 to the provider.
		return textutil.ClipBytes(s, limit, ""), true, nil
	}
	return s, false, nil
}

// combinePromptAndStdin builds the -p message from the prompt argument and
// what was piped in, so `cat app.log | cove -p "解释"` sends both. Either may
// be empty.
func combinePromptAndStdin(prompt, stdin string) string {
	prompt = strings.TrimSpace(prompt)
	stdin = strings.TrimSpace(stdin)
	switch {
	case stdin == "":
		return prompt
	case prompt == "":
		return stdin
	default:
		return prompt + "\n\n" + stdin
	}
}

// stdinIsPiped reports whether stdin is a pipe or file that -p should read,
// rather than a terminal (or a character device such as NUL or /dev/null).
func stdinIsPiped() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return false
	}
	return !stdinIsMSYSPty()
}

// isMSYSPtyPipeName reports whether name is the pipe Git Bash's mintty (or
// Cygwin) uses as a terminal, e.g. \msys-1888ae32e00d56aa-pty0-from-master.
// Such a stdin is a pipe to Windows but a keyboard to the user.
func isMSYSPtyPipeName(name string) bool {
	name = strings.TrimPrefix(name, `\Device\NamedPipe`)
	parts := strings.Split(name, "-")
	if len(parts) < 5 {
		return false
	}
	if parts[0] != `\msys` && parts[0] != `\cygwin` {
		return false
	}
	return parts[1] != "" && strings.HasPrefix(parts[2], "pty") &&
		(parts[3] == "from" || parts[3] == "to") && parts[4] == "master"
}
