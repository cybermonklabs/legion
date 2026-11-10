package ghtool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// env is the whole environment a call's gh runs with, configDir its GH_CONFIG_DIR: PATH of gh's
// and git's directories, HOME and the XDG directories at the answer's empty ones, the wrapper
// GH_PATH names, gh's prompts, update check, spinner and colour off, and nothing of the daemon's
// own environment.
func (a *Answer) env(configDir string) []string {
	path := a.toolPath()
	return []string{
		"PATH=" + path,
		"HOME=" + a.home,
		"XDG_CONFIG_HOME=" + a.config,
		"XDG_CACHE_HOME=" + a.cache,
		"XDG_DATA_HOME=" + a.data,
		"XDG_STATE_HOME=" + a.state,
		"GH_CONFIG_DIR=" + configDir,
		"GH_PATH=" + a.wrapper,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_SPINNER_DISABLED=1",
		"NO_COLOR=1",
	}
}

// toolPath is gh's directory, and the host's git's joined to it where the host holds a git of
// its own: the whole of a call's PATH, and the front of the leak guard's (Answer.guardEnv).
func (a *Answer) toolPath() string {
	path := filepath.Dir(a.host.GH)
	if a.host.Git != "" && filepath.Dir(a.host.Git) != path {
		path += string(filepath.ListSeparator) + filepath.Dir(a.host.Git)
	}
	return path
}

// leakGuard is the shared leak guard's read of the call, the one check a Legion write to a public
// GitHub repository passes before it writes (internal/leakguard). It runs where a pane's own gh
// runs it, between the owner the call was routed to and the gh about to be started
// (cmd/legion's runGh), and its error — a *leakguard.Refusal, about the text the call carries, or
// a *leakguard.SetupFailure, a check that could not be made to run — refuses the call with the
// guard's own message: a check that did not run has cleared nothing.
//
// The whole argv is what it reads, the footer added and all, rather than the options a parser
// calls text: the shared check's own gh-guard reads full argv, and a private name is refusable
// wherever it stands. The repository is the one the call was routed to, and "" where it named no
// one repository, which the guard reads as a public destination and scans. The files are the
// parse's own (ghroute.Target.Files), which knew each option's arity from the leaf's help, so the
// guard reads no argv again with a table of its own — a table that would read the `-Fixed in
// abc123` of a body as a file nobody named and refuse a call over it.
//
// Standard input is nil: this tool hands gh none (Answer.runGH), and there is nothing to buffer.
// A check that asks for it anyway — the exit-4 handshake — is answered rather than left waiting:
// the guard buffers an empty file and asks again (leakguard's askAgain, whose read is skipped for
// a nil Stdin), and the bytes it answers with are none, which is what gh is given.
//
// Neither call `legion gh` lets through unchecked arises here. `gh auth git-credential` is none
// of the tool's commands, refused by the allow list before any of this; and the shared check's
// own visibility read is a child of this guard, run with the real gh and never through the
// wrapper (Answer.guardEnv), so no call of the model's can be it.
func (a *Answer) leakGuard(ctx context.Context, argv []string, target ghroute.Target, configDir string) error {
	_, err := leakguard.New(leakguard.Options{Check: a.host.LeakCheck, Env: a.guardEnv(configDir)}).GhCall(ctx, leakguard.GhRequest{
		Args:  argv,
		Stdin: nil,
		Repo:  targetRepo(target),
		Files: target.Files,
	})
	return err
}

// targetRepo is the repository a call writes to, "<owner>/<name>", and "" where it names no one
// repository: a call naming none at all, and one naming an owner alone or two repositories of
// one owner, which is a destination the guard is better told nothing of than told wrongly.
func targetRepo(target ghroute.Target) string {
	if target.Owner == "" || target.Repo == "" {
		return ""
	}
	return target.Owner + "/" + target.Repo
}

// guardEnv is the environment the leak guard's children start from for a call routed to
// configDir: the daemon's own (Host.Environ, this process's where the host named none), with
// PATH and GH_CONFIG_DIR its own and three names of the daemon's taken out.
//
// The daemon's and not the call's, which is the environment gh itself runs under (Answer.env):
// that one holds two directories on its PATH, an empty HOME and empty XDG directories, which is
// no environment the shared check — a shell script that runs git, gh and a detection engine —
// could run in, and none its three places could be resolved from either (leakguard's Resolve,
// which reads PATH and the configuration home).
//
// What is changed in it is what the guard's two children need:
//
//   - PATH with the real gh's directory, and the host's git's, in front of the daemon's own. The
//     guard's visibility read is a plain `gh api … repos/<slug> --jq .visibility` resolved on
//     this PATH (leakguard's program), so the gh that answers it is the boot this tool runs and
//     never the answer's GH_PATH wrapper, which runs `legion gh` and would call a guard of its
//     own over the read. The daemon's own PATH stays behind them, so the check finds the shell,
//     the engine and everything else it runs.
//   - GH_CONFIG_DIR the routed owner's, the directory the call itself runs with, so that read
//     reaches GitHub as the agent's App — the one identity this call is made under — and sees
//     what that App sees of the repository.
//   - GH_TOKEN and GITHUB_TOKEN dropped, and GH_PATH with them. A token of the daemon's own
//     would answer in the App's place, so the visibility read would be made as whoever started
//     the daemon rather than as the agent; and a GH_PATH of the daemon's would name gh's git
//     credential helper, which is no helper of this answer's bundle.
func (a *Answer) guardEnv(configDir string) []string {
	base := a.host.Environ
	if base == nil {
		base = os.Environ()
	}
	env := make([]string, 0, len(base)+2)
	// The last entry of a name is the one exec reads, so the daemon's PATH is the last one seen.
	path := ""
	for _, pair := range base {
		switch name, value, _ := strings.Cut(pair, "="); name {
		case "PATH":
			path = value
		case "GH_CONFIG_DIR", "GH_PATH", "GH_TOKEN", "GITHUB_TOKEN":
		default:
			env = append(env, pair)
		}
	}
	if path != "" {
		path = string(filepath.ListSeparator) + path
	}
	return append(env, "PATH="+a.toolPath()+path, "GH_CONFIG_DIR="+configDir)
}

// route reads the repository argv targets, and its owner, as a pane's `legion gh` reads them
// (ghroute.TargetOf, from the real gh's own help, read under the call's environment with the
// issue owner's directory, as a pane reads it), and answers the target, the owner the call runs
// as and the gh directory it runs with: the repository's own, `<owner>/<repo>`, whose token
// reaches that one repository, where the command names one repository and the bundle holds its
// directory, and the owner's otherwise (repositoryDir). The bundle's default, the issue's owner,
// serves a command naming none. The working directory is the answer's empty one, which has no
// git remotes. A command TargetOf refuses, and one naming an owner the bundle holds no directory
// for, are refused.
func (a *Answer) route(ctx context.Context, argv []string) (ghroute.Target, string, string, error) {
	env := a.env(ghconfig.Dir(a.owners, a.bundle.Default))
	vars := make(map[string]string, len(env))
	for _, pair := range env {
		name, value, _ := strings.Cut(pair, "=")
		vars[name] = value
	}
	noRemotes := func() ([]ghroute.Remote, error) { return nil, nil }
	target, err := ghroute.TargetOf(argv, vars, a.help(ctx, env), noRemotes)
	if err != nil {
		return ghroute.Target{}, "", "", err
	}
	owner := target.Owner
	if owner == "" {
		return target, a.bundle.Default, ghconfig.Dir(a.owners, a.bundle.Default), nil
	}
	if _, ok := a.bundle.Dirs[owner]; !ok {
		return ghroute.Target{}, "", "", fmt.Errorf("%q is not an owner of this project: the agent's credentials hold no gh directory for it", owner)
	}
	return target, owner, a.repositoryDir(owner, target.Repo), nil
}

// repositoryDir is the gh directory a call targeting owner's repository repo runs with: the
// repository's own, `<owner>/<repo>`, where the command named one repository and the bundle
// holds its directory, and the owner's own otherwise — a command naming no one repository, and
// one whose repository the bundle holds no directory for, which is every repository of an owner
// the role writes to one of. The name is the one the bundle's directory is named for
// (ghroute.RepositoryName, which the reader already keyed the target by).
func (a *Answer) repositoryDir(owner, repo string) string {
	if name := owner + "/" + ghroute.RepositoryName(repo); repo != "" && ghconfig.IsDirName(name) {
		if _, ok := a.bundle.Dirs[name]; ok {
			return ghconfig.Dir(a.owners, name)
		}
	}
	return ghconfig.Dir(a.owners, owner)
}

// help is the real gh's help of a command path, `gh <path> --help`, run as a call runs, refused
// when gh answers it with an error, as ghroute.ReadHelp refuses it for a pane.
func (a *Answer) help(ctx context.Context, env []string) func(path []string) (string, error) {
	return func(path []string) (string, error) {
		cmd := exec.CommandContext(ctx, a.host.GH, append(slices.Clone(path), "--help")...)
		cmd.Env, cmd.Dir = env, a.cwd
		procgroup.Configure(cmd)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("gh answered its help with %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		if stdout.Len() == 0 {
			return "", errors.New("gh printed no help")
		}
		return stdout.String(), nil
	}
}

// runGH runs gh with argv under the call's environment, configDir its GH_CONFIG_DIR, in the
// answer's empty working directory with no stdin, its stdout and stderr interleaved and cut at
// bounds.GHOutputChars, and stopped, its whole process group, at ctx's end (the gh_seconds bound).
func (a *Answer) runGH(ctx context.Context, argv []string, configDir string, bounds config.AnswerBounds) Result {
	cmd := exec.CommandContext(ctx, a.host.GH, argv...)
	cmd.Env, cmd.Dir = a.env(configDir), a.cwd
	procgroup.Configure(cmd)
	out := &cappedOutput{limit: bounds.GHOutputChars}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if err := procgroup.Err(err); err != nil {
		return Result{Output: fmt.Sprintf("gh could not run: %v", err), Exit: -1}
	}
	output := out.String()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		output += fmt.Sprintf("\n[stopped: gh ran past its %s bound (answers.bounds.gh_seconds)]", bounds.GHCall)
	}
	return Result{Output: output, Exit: cmd.ProcessState.ExitCode(), Ran: true}
}

// cappedOutput keeps the first limit characters written to it and counts the rest. A character is
// a UTF-8 sequence, counted by its leading byte, so a sequence split across writes counts once.
type cappedOutput struct {
	limit int
	kept  []byte
	chars int
}

var _ io.Writer = (*cappedOutput)(nil)

func (c *cappedOutput) Write(p []byte) (int, error) {
	for _, b := range p {
		if b&0xC0 != 0x80 {
			c.chars++
		}
		if c.chars <= c.limit {
			c.kept = append(c.kept, b)
		}
	}
	return len(p), nil
}

// String is what was kept, and a note of how many characters were cut after it.
func (c *cappedOutput) String() string {
	if c.chars <= c.limit {
		return string(c.kept)
	}
	return fmt.Sprintf("%s\n[cut: %d more characters]", c.kept, c.chars-c.limit)
}
