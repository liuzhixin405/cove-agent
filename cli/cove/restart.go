package main

import (
	"fmt"
	"os"
	"strings"
)

// restartExitCode is what a cove started by a Windows restart supervisor
// exits with to ask for another restart (restart_windows.go). EX_TEMPFAIL,
// so a plain exit is never mistaken for it.
const restartExitCode = 75

// restartArgsEnv names the file a supervised cove writes its next command
// line to before exiting with restartExitCode.
const restartArgsEnv = "COVE_RESTART_ARGS_FILE"

// restartArgs is the command line the restarted cove runs with: the original
// one, without what only made sense at the first start — -r/--resume (the
// session to continue is sessionID now), --image/--file (attached to the
// first message, which was already sent) — plus -r
// sessionID when there is a saved conversation to continue.
func restartArgs(orig []string, sessionID string) []string {
	var out []string
	for i := 0; i < len(orig); i++ {
		switch orig[i] {
		case "-r", "--resume", "--image", "--file":
			i++ // and its value
			continue
		}
		out = append(out, orig[i])
	}
	if sessionID != "" {
		out = append(out, "-r", sessionID)
	}
	return out
}

// restartExecutable is the binary to start again. On Linux a binary replaced
// while running (an upgrade, the usual reason to restart) is reported as
// "<path> (deleted)"; the path itself now holds the new binary.
func restartExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(exe, " (deleted)"), nil
}

// restartCove replaces this process with a fresh cove continuing sessionID
// ("" starts an empty session). The caller has already saved the session and
// done the exit work; restartCove does not return.
func restartCove(sessionID string) {
	exe, err := restartExecutable()
	if err == nil {
		var code int
		code, err = restartSelf(exe, restartArgs(os.Args[1:], sessionID))
		if err == nil {
			os.Exit(code)
		}
	}
	fmt.Fprintf(os.Stderr, "重启失败: %v\n请手动启动 cove", err)
	if sessionID != "" {
		fmt.Fprintf(os.Stderr, "，用 cove -r %s 继续刚才的会话", sessionID)
	}
	fmt.Fprintln(os.Stderr, "。")
	os.Exit(1)
}
