package ghtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// env is the whole environment a call's gh runs with, configDir its GH_CONFIG_DIR: PATH of gh's
// and git's directories, HOME and the XDG directories at the answer's empty ones, the wrapper
// GH_PATH names, gh's switches (ghSettings), and nothing of the daemon's own environment.
func (a *Answer) env(configDir string) []string {
	env := make([]string, 0, 8+len(ghSettings))
	env = append(env,
		"PATH="+a.toolPath(),
		"HOME="+a.home,
		"XDG_CONFIG_HOME="+a.config,
		"XDG_CACHE_HOME="+a.cache,
		"XDG_DATA_HOME="+a.data,
		"XDG_STATE_HOME="+a.state,
		"GH_CONFIG_DIR="+configDir,
		"GH_PATH="+a.wrapper,
	)
	return append(env, ghSettings...)
}

// ghSettings are gh's own switches, which every gh of a call runs under: the call's own
// (Answer.env) and the leak guard's children (Answer.guardEnv). gh asks no question, checks for no
// update, and prints no spinner and no colour.
var ghSettings = []string{"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1"}

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
// runs it, between the directory the call was routed to and the gh about to be started
// (cmd/legion's runGh), and its error — a *leakguard.Refusal, about the text the call carries, a
// *leakguard.SetupFailure, a check that could not be made to run, or a *leakguard.NotFound, a
// repository whose visibility read GitHub answered 404 — refuses the call with the guard's own
// message, as `legion gh` refuses it: a check that did not run has cleared nothing, and neither
// has a visibility read GitHub answered 404. check is the check it runs, the one the host's
// LeakCheck answered for this call, so the guard resolves none of its own. An empty check is
// refused before the guard runs, with the setup failure naming it: handed none, the guard would
// resolve one itself (leakguard's Resolve), and the tool runs no check but the one its host
// answers.
//
// Under the deployment's manifest digest (Host.LeakCheckManifestSHA256) the guard runs a private
// copy of the check it held to that digest, and makes it in the answer's own 0700 directory
// (Answer.copies, leakguard.Options.PrivateCopyBase), never under leakguard.PrivateCopyBase: that
// is a role's private directory, which the worker image a Kubernetes daemon runs from holds
// root-owned and empty, with nothing bound over it in the daemon's container, so no copy could be
// made there and every call would be refused.
//
// Its private-name pass runs the deployment's rule file, the answer's copy Open wrote of the bytes
// boot read (Host.PrivateNameRules), which the guard reads where its environment names it
// (Answer.guardEnv, leakguard.RulesFileVar), as a role's `legion gh` runs the copy its runtime
// wrote of the same launch secret: a write the loop makes for a stopped agent is refused by the
// rules a running agent's is. Where the deployment names none, the guard runs the shared check
// alone and reads no visibility and nothing the call sends (leakguard.GhReadNoRules), as in a
// role.
//
// The whole argv is what it reads, the footer added and all, rather than the options a parser
// calls text: the shared check's own gh-guard reads full argv, and a private name is refusable
// wherever it stands. The repository is the one the call was routed to, and "" where it named no
// one repository, which the guard reads as a public destination and scans. What the call sends
// beside its argv is the parse's own (ghroute.Target.Sent), which knew each option's arity from
// the leaf's help, so the guard reads no argv again with tables of its own — tables that would
// read the `-Fixed in abc123` of a body as a file nobody named and refuse a call over it.
//
// Standard input is nil: this tool hands gh none (Answer.runGH), and there is nothing to buffer.
// A check that asks for it anyway — the exit-4 handshake — is answered rather than left waiting:
// the guard buffers an empty file and asks again (leakguard's askAgain, whose read is skipped for
// a nil Stdin), and the bytes it answers with are none, which is what gh is given.
//
// Of the three calls `legion gh` lets through unchecked, the tool lets none through: every call it
// makes gets the guard a write gets, a read included. `gh auth git-credential` is none of the
// tool's commands, refused by the allow list before any of this; the shared check's own
// visibility read is a child of this guard, run with the real gh and never through the wrapper
// (Answer.guardEnv), so no call of the model's can be it; and the exact `gh api rate_limit`,
// which the model can make, is guarded here, since nothing of the tool skips the guard for it.
func (a *Answer) leakGuard(ctx context.Context, check string, argv []string, target ghroute.Target, configDir string) error {
	if check == "" {
		return &leakguard.SetupFailure{
			What: leakguard.WhatGhCall,
			Why:  "the daemon handed the guard no shared public leak check for this call, and the gh tool runs no check but the one the daemon hands it",
			Err:  errors.New("the check's path is empty"),
		}
	}
	_, err := leakguard.New(leakguard.Options{Check: check, Env: a.guardEnv(configDir), PrivateCopyBase: a.copies}).GhCall(ctx, leakguard.GhRequest{
		Args:  argv,
		Stdin: nil,
		Repo:  targetRepo(target),
		Sent:  target.Sent,
	})
	return err
}

// targetRepo is the repository a call writes to, "<owner>/<name>", and "" where it names no one
// repository: a call naming none at all, and one naming an owner alone, two repositories, or one
// repository by both of the names a configured rename gives it, which is a destination the guard
// is better told nothing of than told wrongly.
func targetRepo(target ghroute.Target) string {
	if target.Owner == "" || target.Repo == "" {
		return ""
	}
	return target.Owner + "/" + target.Repo
}

// guardEnv is the environment the leak guard's children start from for a call routed to
// configDir: the names of the daemon's own environment (Host.Environ, this process's where the
// host named none) that guardKeeps keeps, gh's switches (ghSettings), PATH and GH_CONFIG_DIR its
// own, the deployment's manifest digest as leakguard.CheckerVar where the deployment names a
// check (Host.LeakCheckManifestSHA256), and the answer's copy of the deployment's private-name
// rule file as leakguard.RulesFileVar where the deployment names one (Host.PrivateNameRules,
// Answer.rules). The guard reads its own environment for both names (leakguard's sharedCheck and
// Guard.rules), so this is the one place it is told them. It holds the check the host answers to
// the digest: a check of another digest refuses the call with the setup failure naming both
// digests. Where the deployment names none the name is not set, and the guard runs the check it
// is handed as it is. It runs the rule file's rules over a write to a public repository, and
// where the deployment names no rule file it runs none, as a role's guard does.
//
// The daemon's and not the call's, which is the environment gh itself runs under (Answer.env):
// that one holds two directories on its PATH, an empty HOME and empty XDG directories, which is
// no environment the shared check — a shell script that runs git, gh and a detection engine —
// could run in. The check itself is found elsewhere, before the guard runs (Host.LeakCheck).
//
// What is set in it is what the guard's two children need:
//
//   - PATH with the real gh's directory, and the host's git's, in front of the daemon's own. The
//     guard's visibility read is a plain `gh api … repos/<slug> --include --jq .visibility`
//     resolved on this PATH (leakguard's program), so the gh that answers it is the boot this
//     tool runs and never the answer's GH_PATH wrapper, which runs `legion gh` and would call a
//     guard of its own over the read. The daemon's own PATH stays behind them, so the check
//     finds the shell, the engine and everything else it runs.
//   - GH_CONFIG_DIR the directory the call itself runs with, the one its repository routes to
//     (Answer.route), so that read reaches GitHub as the agent's App — the one identity this call
//     is made under — and sees what that App sees of the repository.
//   - gh's switches, the ones the call's own gh runs under, so the gh the guard and the check
//     start asks nothing and prints plainly.
//
// Of the daemon's own environment it keeps an allow list (guardKeeps), and never the whole less
// some names. A token of the daemon's own (GH_TOKEN, GITHUB_TOKEN) would answer in the App's
// place, so the visibility read would be made as whoever started the daemon rather than as the
// agent; a GH_PATH of the daemon's would name gh's git credential helper, which is no helper of
// this answer's bundle; and every other credential or seed of the daemon's would reach a script
// this repository does not hold, and everything that script starts.
func (a *Answer) guardEnv(configDir string) []string {
	base := a.host.Environ
	if base == nil {
		base = os.Environ()
	}
	env := make([]string, 0, len(base)+len(ghSettings)+4)
	// The last entry of a name is the one exec reads, so the daemon's PATH is the last one seen.
	path := ""
	for _, pair := range base {
		switch name, value, _ := strings.Cut(pair, "="); {
		case name == "PATH":
			path = value
		case guardKeeps(name):
			env = append(env, pair)
		}
	}
	if path != "" {
		path = string(filepath.ListSeparator) + path
	}
	env = append(env, ghSettings...)
	if a.host.LeakCheckManifestSHA256 != "" {
		env = append(env, leakguard.CheckerVar+"="+a.host.LeakCheckManifestSHA256)
	}
	if a.rules != "" {
		env = append(env, leakguard.RulesFileVar+"="+a.rules)
	}
	return append(env, "PATH="+a.toolPath()+path, "GH_CONFIG_DIR="+configDir)
}

// guardKeeps is whether the leak guard's children keep the name of the daemon's environment
// (Answer.guardEnv). The guard's gh reads carry the App's token through GH_CONFIG_DIR, so it keeps
// what a program carrying a GitHub token keeps (leakguard.TokenChildKeeps), each for what reads it:
//
//   - PATH, on which the guard finds the askpass program and every child's program (leakguard's
//     noAnswer and program), and the check its shell, git and engine; Answer.guardEnv puts the
//     real gh's directory in front of it;
//   - HOME, the home of the check's git and of gh;
//   - TMPDIR, where the check and the programs it starts make their temporary files;
//   - TERM, TZ, LANG and the LC_ names, the terminal, time zone and locale the check's shell,
//     its python and git run under;
//   - the proxy variables in both cases (HTTPS_PROXY, HTTP_PROXY, ALL_PROXY, NO_PROXY), the route
//     out a pane and a pod are handed: gh reaches GitHub through them (Go's net/http), and so do
//     the guard's curl read, its ls-remote and the check's git (libcurl).
//
// It keeps these besides, each with what reads it:
//
//   - XDG_CONFIG_HOME, XDG_CACHE_HOME and XDG_DATA_HOME, where gh keeps its configuration, cache
//     and data (go-gh pkg/config/config.go:276-327, which cmd/legion's ghChildKeeps cites);
//   - XDG_STATE_HOME, where gh keeps its state and the guard the public answers it read
//     (leakguard's visibilityCachePath).
//
// Everything else stays out, among it: the daemon's own credentials and seeds; PUBLIC_LEAK_CHECK
// (leakguard.CheckVar), since the guard is handed the check's path (Host.LeakCheck) and resolves
// none of its own; a leakguard.CheckerVar of the daemon's own environment, since the guard is told
// the deployment's digest from the host alone (Answer.guardEnv); a leakguard.RulesFileVar of the
// daemon's own environment, since the guard is told the deployment's rule file from the host
// alone, the bytes boot read (Host.PrivateNameRules, Answer.guardEnv); GH_TOKEN, GITHUB_TOKEN and
// GH_PATH (Answer.guardEnv); the trust stores (SSL_CERT_FILE, SSL_CERT_DIR, CURL_CA_BUNDLE), which
// beside a proxy would let that proxy read the App's token (leakguard.TokenChildKeeps), while the
// call's own gh runs with none (Answer.env), and a deployment whose proxy reads TLS installs its CA
// in the system's store, which gh reads by default (docs/deployment.md); GH_HOST, which the call's
// gh runs without and the check reads a call under (ghroutetest.PinnedCheckReads); the GIT_ names,
// by which the check's git would read the daemon's repository and configuration (GIT_DIR,
// GIT_CONFIG_PARAMETERS); PUBLIC_LEAK_CHECK_REPO_NAMES and PUBLIC_LEAK_CHECK_INTERNAL, which the
// guard sets for no check (leakguard's package doc); and LEGION_GH_OWNERS_DIR, which the guard
// reads only of a read `legion gh` refused, while this tool's reads run the real gh.
func guardKeeps(name string) bool {
	return leakguard.TokenChildKeeps(name) || guardNames[name]
}

// guardNames are the whole names guardKeeps keeps beside leakguard.TokenChildKeeps' own.
var guardNames = map[string]bool{
	"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true,
}

// route reads the repositories argv targets, and their owners, as a pane's `legion gh` reads
// them (ghroute.TargetOf, from the real gh's own help, read under the call's environment with the
// bundle's default directory, as a pane reads it), routes them by the pick a pane's makes, over
// the answer's own bundle (ghroute.DirFor), and answers the target, the owner the call runs as
// and the gh directory it runs with. That directory is the one every repository and owner the
// command names routes to: the owner's own, whose lease reaches every repository its
// installation holds, except for a repository a configured rename moved, which is answered from
// the directory of the owner holding it now. The owner is the one that directory is named for
// (ghconfig.Dir). The bundle's default, the directory the issue's own repository routes to and
// the call's GH_CONFIG_DIR, serves a command naming none, and a read naming only owners the
// bundle holds no directory for and no repository a configured rename moves (R4 of
// dispatch://INFRA-3399): a call TargetOf reads as one that writes nothing, or a GraphQL query
// holding neither `mutation` nor `subscription`. The working directory is the answer's empty one,
// which has no git remotes, so none is ever read for such a read. A command TargetOf refuses is
// refused: among them one whose repositories route to two directories, every other one naming an
// owner the bundle holds no directory for (an owner the role's App is not installed on), and one
// naming a repository a configured rename moved to such an owner.
func (a *Answer) route(ctx context.Context, argv []string) (ghroute.Target, string, string, error) {
	env := a.env(ghconfig.Dir(a.owners, a.bundle.Default))
	vars := make(map[string]string, len(env))
	for _, pair := range env {
		name, value, _ := strings.Cut(pair, "=")
		vars[name] = value
	}
	noRemotes := func() ([]ghroute.Remote, error) { return nil, nil }
	dirFor := func(owner, name string) (string, error) { return ghroute.DirFor(a.owners, owner, name) }
	target, err := ghroute.TargetOf(argv, vars, a.help(ctx, env), noRemotes, dirFor)
	if err != nil {
		return ghroute.Target{}, "", "", err
	}
	dir := target.Dir
	if dir == "" {
		dir = ghconfig.Dir(a.owners, a.bundle.Default)
	}
	return target, filepath.Base(dir), dir, nil
}

// help is the real gh's help of a command path, `gh <path> --help`, read under env as a pane's
// `legion gh` reads it (ghroute.ReadHelp): bounded, its process group stopped past the bound or at
// ctx's end, run under leakguard.RunByRealGhVar, and refused when gh answers it with an error.
func (a *Answer) help(ctx context.Context, env []string) func(path []string) (string, error) {
	return func(path []string) (string, error) { return ghroute.ReadHelp(ctx, a.host.GH, env, path) }
}

// runGH runs gh with argv under the call's environment, configDir its GH_CONFIG_DIR, in the
// answer's empty working directory with no stdin, bounded as every program of the loop's tools is
// (RunCapped).
func (a *Answer) runGH(ctx context.Context, argv []string, configDir string, bounds config.AnswerBounds) Result {
	cmd := exec.CommandContext(ctx, a.host.GH, argv...)
	cmd.Env, cmd.Dir = a.env(configDir), a.cwd
	return RunCapped(ctx, cmd, "gh", bounds)
}

// RunCapped runs cmd, made with ctx and its environment, working directory and stdin already set,
// as the loop's every tool that starts a program runs it: its stdout and stderr interleaved and cut
// at bounds.GHOutputChars (CappedOutput), and stopped, its whole process group, at ctx's end (the
// gh_seconds bound). name is the program as the result's notes name it. A program that could not
// start did not run, and its exit is -1. A program a signal ended ran and answered no status
// (procgroup.Ended): what it printed stands, with the bound's note where ctx's end stopped it and
// the signal's words where something else did, and its exit is -1.
func RunCapped(ctx context.Context, cmd *exec.Cmd, name string, bounds config.AnswerBounds) Result {
	procgroup.Configure(cmd)
	out := NewCappedOutput(bounds.GHOutputChars)
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	end := procgroup.Ended(err)
	if err := procgroup.Err(err); err != nil && end.Signal == "" {
		return Result{Output: fmt.Sprintf("%s could not run: %v", name, err), Exit: -1}
	}
	output := out.String()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		output += fmt.Sprintf("\n[stopped: %s ran past its %s bound (answers.bounds.gh_seconds)]", name, bounds.GHCall)
	case end.Signal != "":
		output += fmt.Sprintf("\n[%s was ended by %s]", name, end.Signal)
	}
	return Result{Output: output, Exit: cmd.ProcessState.ExitCode(), Ran: true}
}

// CappedOutput keeps the first limit characters written to it and counts the rest. A character is
// a UTF-8 sequence, counted by its leading byte, so a sequence split across writes counts once.
// The answer loop's every tool cuts what its model reads with it, so a cut reads the same whatever
// tool made it (internal/dispatchtool).
type CappedOutput struct {
	limit int
	kept  []byte
	chars int
}

// NewCappedOutput keeps the first limit characters written to it.
func NewCappedOutput(limit int) *CappedOutput { return &CappedOutput{limit: limit} }

var _ io.Writer = (*CappedOutput)(nil)

func (c *CappedOutput) Write(p []byte) (int, error) {
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
func (c *CappedOutput) String() string {
	if c.chars <= c.limit {
		return string(c.kept)
	}
	return fmt.Sprintf("%s\n[cut: %d more characters]", c.kept, c.chars-c.limit)
}
