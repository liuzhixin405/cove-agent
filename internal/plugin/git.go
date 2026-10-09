package plugin

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/race"
	"github.com/liuzhixin405/cove-agent/internal/shell"
)

// runGitCommand runs a bounded git invocation for plugin and marketplace work.
//
// git must never wait for input here. A private or mistyped HTTPS repository
// answers 401, and git then asked for a username on the terminal cove's UI
// owns, or the Windows credential manager opened a login window, and the
// install hung. shell.Env turns off git's terminal prompt; GCM_INTERACTIVE
// does the same for Git Credential Manager. Credentials that are already
// stored still work.
func runGitCommand(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result := race.Execute(ctx, "", append(shell.Env(os.Environ()), "GCM_INTERACTIVE=never"), append([]string{"git"}, args...))
	output := []byte(result.Output)
	if err := ctx.Err(); err != nil {
		return output, err
	}
	if result.Status != "passed" {
		return output, fmt.Errorf("git %s: %s", result.Status, result.Error)
	}
	return output, nil
}

// cloneRepo shallow-clones url into dir.
//
// core.symlinks=false is written into the new repository before checkout (and
// so also applies to later pulls): a plugin is untrusted content whose skills
// and commands are read into the model's prompt, and a symlink named
// skills/x/SKILL.md pointing at ~/.ssh/id_rsa would have that file read and
// sent to the provider. Links are checked out as plain files holding the link
// text instead. "--" keeps a URL starting with "-" from being read as an option.
func cloneRepo(url, dir string) error {
	return cloneRepoContext(context.Background(), url, dir)
}

func cloneRepoContext(ctx context.Context, url, dir string) error {
	if out, err := runGitCommand(ctx, "clone", "--depth=1", "--quiet", "--config", "core.symlinks=false", "--", url, dir); err != nil {
		return fmt.Errorf("git clone failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
