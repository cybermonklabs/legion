package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// ghRepoGitConfigRefusal refuses the call when the configuration of the repository the git gh runs
// works in (the current directory's, as gh's own git finds it, with env, the environment gh gets)
// names something that would let that git's token reach anyone but github.com: any http.*ssl* or
// http.*proxy* key, generic or URL-scoped, but an http.*sslVerify that git reads as true, which
// only keeps the check on; and any credential helper but none or `legion git-credential`
// (workspace.GitHubCredentialHelper), the one a Legion clone names. git reads these before the
// command line, but a URL-scoped `http.https://github.com/<owner>/.sslVerify=false` beats any
// command-line entry, and was measured to hand a man-in-the-middle proxy the credential helper's
// token (git 2.43). It reads what git reads (`git config --show-origin --null --get-regexp`),
// leaves the command line's own entries alone (their origin is "command line:", which no file's
// is), and names the key and the file, never the value. It needs no --show-scope, which git 2.26
// added, so an older git reads the same; git has printed origins since 2.8. Outside a repository
// there is nothing to read. With no git on PATH, nothing is read and nothing refused: gh finds its
// git on the same PATH, so it runs none, and no configuration of one can send anything. Any other
// read git cannot make is refused, since gh's git would then read a configuration this one could
// not see.
//
// It refuses as well every way the configuration a later git of gh's reads can differ from the one
// this read sees, since this read is made once, here, and gh's git runs later, elsewhere in the
// repository or after a checkout:
//
//   - any include, `include.path` or `includeIf.<condition>.path`, whatever its condition and
//     whether or not its file is there: an include reads a file whose content, and whether it is
//     read at all, can change during the call. Measured with git 2.43, each naming a file with a
//     URL-scoped http.sslVerify=false and inactive or empty where this read ran, then active for
//     the git gh runs after, handed a man-in-the-middle proxy the token: includeIf.onbranch, once
//     that git checked out the branch; includeIf.hasconfig:remote.*.url, once it added the remote;
//     includeIf.gitdir:**/worktrees/**, for `pr checkout --worktree`'s `git -C <worktree> fetch`;
//     and include.path of a tracked file the branch lacks, for `pr merge --delete-branch`'s pull
//     after it checks out the base branch. git lists an include's key whether or not it applies.
//   - extensions.worktreeConfig on, which gives each worktree a configuration of its own
//     (.git/worktrees/<name>/config.worktree) that a git run in another worktree reads and this
//     read does not.
//
// With both refused, every worktree of the repository reads the configuration this read saw, so
// `gh pr checkout --worktree` and `gh issue develop --worktree`, which reuse only a linked worktree
// of the current repository (cli/cli v2.104.0 pkg/cmd/pr/shared/worktree.go:61-68: a path in
// another repository is an error) or create one, need no read of their own. No Legion clone, pod
// image or end-to-end fixture sets either.
//
// Its limit is ghConfigShape's: it reads the configuration before gh's git does, so a pane that
// writes it between the two wins that race, and that pane can already read the token itself.
//
// The read runs under ctx, runGh's beforeGh, in a process group of its own that its bound
// (ghroute.GitReadBound, the bound the remotes read takes too) or the end of ctx stops whole
// (procgroup.Configure): a git, or a wrapper of it, whose child holds the output pipe would
// otherwise keep the call waiting past the bound for as long as that child lives, since a stop of
// git alone leaves the pipe open. The terminal's signals do not reach that group, so a SIGINT, a
// SIGTERM or a SIGHUP reaches it through ctx (runGh's rule); runGh reads such a stop as the
// signal's, not as this refusal. A git that exits while a child still holds the pipe is waited
// for no longer than procgroup's WaitDelay, and its read is then refused, since what was copied
// may be cut short and a setting it misses would let gh's git send the token.
func ghRepoGitConfigRefusal(ctx context.Context, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, programbound.Of(&ghroute.GitReadBound))
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--show-origin", "--null", "--get-regexp", `^(http|credential)\.|^include\.path$|^includeif\..*\.path$|^extensions\.worktreeconfig$`)
	procgroup.Configure(cmd)
	cmd.Env = append(slices.Clone(env), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		return fmt.Errorf("cannot read the repository's git configuration: git exited, but something it started still held its output %s later, so what was read may be cut short, and gh's git would read it whole, so it sends nothing", procgroup.WaitDelayNow())
	}
	var exit *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 1 && stdout.Len() == 0:
		return nil
	case err != nil:
		return fmt.Errorf("cannot read the repository's git configuration (%v: %s), which gh's git would read, so it sends nothing", err, strings.TrimSpace(stderr.String()))
	}
	// Each entry is origin NUL key, then LF and the value where the key has one, NUL.
	fields := strings.Split(strings.TrimSuffix(stdout.String(), "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		origin := fields[i]
		key, value, hasValue := strings.Cut(fields[i+1], "\n")
		if origin == "command line:" {
			continue
		}
		key = gitConfigKey(key)
		name := key[strings.LastIndex(key, ".")+1:]
		switch {
		case (strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "includeif.")) && name == "path":
			return fmt.Errorf("the repository's git configuration (%s) sets %s, an include whose file, and whether git reads it, can change between this call's read and the git gh runs after it, so it sends nothing; unset it with `git config --unset-all %s`", origin, key, key)
		case key == "extensions.worktreeconfig" && gitConfigTrue(value, hasValue):
			return fmt.Errorf("the repository's git configuration (%s) sets %s, which gives each worktree a configuration of its own that a git gh runs in another worktree reads and this call does not, so it sends nothing; unset it with `git config --unset-all %s`", origin, key, key)
		case strings.HasPrefix(key, "http.") && name == "sslverify" && gitConfigTrue(value, hasValue):
		case strings.HasPrefix(key, "http.") && (strings.HasPrefix(name, "ssl") || strings.HasPrefix(name, "proxy")):
			return fmt.Errorf("the repository's git configuration (%s) sets %s, which would let the git gh runs send its token past a certificate check or to a proxy this call cannot see, so it sends nothing; unset it with `git config --unset-all %s`", origin, key, key)
		case strings.HasPrefix(key, "credential.") && name == "helper" && value != "" && value != workspace.GitHubCredentialHelper:
			return fmt.Errorf("the repository's git configuration (%s) sets %s to a helper other than `legion git-credential`, which git would hand the token gh's git gets, so it sends nothing; unset it with `git config --unset-all %s`", origin, key, key)
		}
	}
	return nil
}

// gitConfigTrue is whether git reads a boolean's value as true: a key with no value at all, or
// true, yes or on in any case, or a nonzero integer (git's git_parse_maybe_bool). Anything else,
// false, no, off, 0, the empty string and a word git would refuse, is not.
func gitConfigTrue(value string, hasValue bool) bool {
	if !hasValue {
		return true
	}
	switch strings.ToLower(value) {
	case "true", "yes", "on":
		return true
	}
	n, err := strconv.Atoi(value)
	return err == nil && n != 0
}
