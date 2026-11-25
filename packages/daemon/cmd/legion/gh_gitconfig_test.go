package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A git read of `legion gh` that outlives its bound is stopped with everything it started: a git,
// or a wrapper of it, whose child holds the output pipe once git itself is stopped would keep the
// read waiting past its bound for as long as that child lives. The stand-in git starts a child
// that holds its standard output and records the child's pid, then never answers. The read's
// deadline passes by hand once that pid is recorded and is the child's, a running `sleep`
// (recordedHelper): a fixed bound of a second could stop the stand-in's shell before a loaded
// machine had let it record anything, and a recorded pid that were not the child's would let the
// last check pass with no child seen. The read must end, refused, with the test binary's deadline
// as the only bound on the wait, and the child must be gone with it.
func TestAGitReadThatOutlivesItsBoundIsStoppedWithWhatItStarted(t *testing.T) {
	for _, tc := range []struct {
		name, refused string
		read          func(context.Context) error
	}{
		{"the configuration read", "cannot read the repository's git configuration", func(ctx context.Context) error { return ghRepoGitConfigRefusal(ctx, os.Environ()) }},
		{"the remotes read", "git remote -v", func(ctx context.Context) error { _, err := gitLines(ctx, "remote", "-v"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, child := t.TempDir(), filepath.Join(t.TempDir(), "child.pid")
			const childSeconds, gitSeconds = "86397", "86396"
			script := "#!/bin/sh\n" + recordsChild(child, childSeconds) + "\nexec sleep " + gitSeconds + "\n"
			if err := testexec.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			killRecordedHelper(t, child, childSeconds)
			deadline := newPassedDeadline()
			t.Cleanup(deadline.pass)
			answered := make(chan error, 1)
			go func() { answered <- tc.read(deadline) }()
			var pid int
			testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, testBoundMargin), testwait.Poll, "the stand-in git to start its child and record its pid",
				func() bool { var running bool; pid, running = recordedHelper(child, childSeconds); return running })
			deadline.pass()
			err := testwait.Receive(t, tc.name+" whose git never answers and whose child holds its output to end once its deadline passed", answered)
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("%s = %v, want a refusal naming %q", tc.name, err, tc.refused)
			}
			testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, testBoundMargin), testwait.Poll,
				fmt.Sprintf("git's child (pid %d) to end with the read's stop: want the stop to reach git's whole process group", pid),
				func() bool { return !helperRunning(pid, childSeconds) })
		})
	}
}

// A git read of `legion gh` whose git exited while something it started still held the output is
// refused: the copy of what git printed stopped at exec's WaitDelay, so what was read may be cut
// short, and the configuration read would then pass a setting it never read, while gh's own git
// reads the configuration whole. The stand-in git starts a child that holds its standard output,
// records it and exits 0 at once; the row gives WaitDelay a value of its own (procgroup.
// WaitDelayBound), since it reads that delay's end, and the child holds the output past any delay.
func TestAGitReadCutShortByWhatItStartedIsRefused(t *testing.T) {
	previous := procgroup.WaitDelayBound
	procgroup.WaitDelayBound = 100 * time.Millisecond
	t.Cleanup(func() { procgroup.WaitDelayBound = previous })
	for _, tc := range []struct {
		name, refused string
		read          func() error
	}{
		{"the configuration read", "cannot read the repository's git configuration: git exited, but something it started still held its output", func() error { return ghRepoGitConfigRefusal(context.Background(), os.Environ()) }},
		{"the remotes read", "git remote -v exited, but something it started still held its output", func() error { _, err := gitLines(context.Background(), "remote", "-v"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, child := t.TempDir(), filepath.Join(t.TempDir(), "child.pid")
			const childSeconds = "86398"
			script := "#!/bin/sh\n" + recordsChild(child, childSeconds) + "\nexit 0\n"
			if err := testexec.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			killRecordedHelper(t, child, childSeconds)
			if err := tc.read(); err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("%s = %v, want a refusal naming %q", tc.name, err, tc.refused)
			}
		})
	}
}

// gitConfigEnviron is pairs as the GIT_CONFIG_COUNT block git reads as it reads `-c`.
func gitConfigEnviron(pairs [][2]string) []string {
	out := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs))}
	for i, pair := range pairs {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, pair[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, pair[1]))
	}
	return out
}

// gitConfigPairsOf is the pairs a GIT_CONFIG_COUNT block names, in order, failing the test on a
// block whose count and pairs disagree.
func gitConfigPairsOf(t *testing.T, block []string) [][2]string {
	t.Helper()
	env := map[string]string{}
	for _, pair := range block {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	count, err := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	if err != nil || len(block) != 1+2*count {
		t.Fatalf("the block %q counts %q pairs and holds %d entries", block, env["GIT_CONFIG_COUNT"], len(block))
	}
	pairs := make([][2]string, 0, count)
	for i := range count {
		pairs = append(pairs, [2]string{env["GIT_CONFIG_KEY_"+strconv.Itoa(i)], env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]})
	}
	return pairs
}

// The git gh runs takes of its caller's GIT_CONFIG_COUNT pairs only those the runtime itself sets,
// key and value alike, a key cased otherwise matching as git matches it, and then
// ghForcedGitConfig: http.sslVerify on and submodule recursion off. Every other pair the caller
// names, a proxy, a CA, sslVerify off, a URL rewrite, an include, another credential helper, an
// ssh command, another hooks path, or submodule recursion on, is dropped however it is cased or
// scoped. A count past the pairs the environment holds reads the pairs there, and a count that is
// no count reads none.
func TestGhChildGitConfigKeepsOnlyTheRuntimesPairs(t *testing.T) {
	runtime := sandbox.GitConfigPairs()
	cased := slices.Clone(runtime)
	cased[2][0] = "CREDENTIAL.https://github.com.USEHTTPPATH"
	foreign := [][2]string{
		{"http.proxy", "http://127.0.0.1:1"},
		{"HTTP.https://github.com/.SSLVERIFY", "false"},
		{"http.sslCAInfo", "/tmp/example.pem"},
		{"url.https://example.invalid/.insteadOf", "https://github.com/"},
		{"include.path", "/tmp/example.gitconfig"},
		{"credential.https://github.com.helper", "!/tmp/example-helper"},
		{"core.sshCommand", "/tmp/example-ssh"},
		{"core.hooksPath", "/tmp/example-hooks"},
		{"submodule.recurse", "true"},
		{"FETCH.recurseSubmodules", "on-demand"},
		{"push.recurseSubmodules", "on-demand"},
		{"submodule.sub.fetchRecurseSubmodules", "true"},
	}
	interleaved := append(append(slices.Clone(foreign[:4]), runtime...), foreign[4:]...)
	withCount := func(count string, pairs [][2]string) []string {
		return append(gitConfigEnviron(pairs)[1:], "GIT_CONFIG_COUNT="+count)
	}
	for _, tc := range []struct {
		name    string
		environ []string
		want    [][2]string
	}{
		{"the runtime's pairs", gitConfigEnviron(runtime), runtime},
		{"the runtime's pairs among a caller's own", gitConfigEnviron(interleaved), runtime},
		{"a runtime key cased otherwise", gitConfigEnviron(cased), cased},
		{"a caller's pairs alone", gitConfigEnviron(foreign), nil},
		{"no block", nil, nil},
		{"a count past the pairs", withCount("1000000000", runtime), runtime},
		{"a count that is negative", withCount("-1", runtime), nil},
		{"a count that is no count", withCount("five", runtime), nil},
		{"a runtime key with its value changed", gitConfigEnviron([][2]string{{"core.hooksPath", "/tmp/example-hooks"}}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := append(slices.Clone(tc.want), ghForcedGitConfig...)
			if got := gitConfigPairsOf(t, ghChildGitConfig(tc.environ)); !slices.Equal(got, want) {
				t.Errorf("ghChildGitConfig = %q, want %q", got, want)
			}
		})
	}
}

// isolatedGit runs git with args in dir with no configuration file but the repository's own, and
// fails the test when it fails.
func isolatedGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %q: %v: %s", args, err, out)
	}
}

// repositoryWith is a new repository whose own configuration holds each of settings, `git config`
// arguments, in turn.
func repositoryWith(t *testing.T, settings ...[]string) string {
	t.Helper()
	dir := t.TempDir()
	isolatedGit(t, dir, fixturegit.InitWorkTree("", "")[1:]...)
	for _, args := range settings {
		isolatedGit(t, dir, append([]string{"config"}, args...)...)
	}
	return dir
}

// The git gh runs, under the environment `legion gh` hands it, reads the repository's own
// configuration and its command line, and nothing a caller names otherwise: not the caller's
// ~/.gitconfig, $XDG_CONFIG_HOME/git/config or the file its GIT_CONFIG_GLOBAL names, not its
// GIT_CONFIG_PARAMETERS, and of its GIT_CONFIG_COUNT pairs only the runtime's. Each of those
// names a proxy here, which would be handed the token gh's credential helper answers, and turns
// submodule recursion on, which would fetch a submodule with its own configuration; the command
// line holds the runtime's pairs and ghForcedGitConfig alone, recursion off among them.
func TestGhChildGitReadsNoConfigurationOfTheCallers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	home := t.TempDir()
	proxyFile := "[http]\n\tproxy = http://127.0.0.1:1\n\tsslCAInfo = /tmp/example.pem\n[submodule]\n\trecurse = true\n[fetch]\n\trecurseSubmodules = true\n"
	for _, path := range []string{filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git", "config"), filepath.Join(home, "global.gitconfig")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(proxyFile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo := repositoryWith(t, []string{"user.name", "Legion test"})
	caller := append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "global.gitconfig"), "GIT_CONFIG_NOSYSTEM=",
		"GIT_CONFIG_PARAMETERS='http.proxy'='http://127.0.0.1:2' 'submodule.recurse'='true'",
	}, gitConfigEnviron(append(sandbox.GitConfigPairs(), [2]string{"http.proxy", "http://127.0.0.1:3"}, [2]string{"fetch.recurseSubmodules", "true"}))...)
	cmd := exec.Command("git", "config", "--show-scope", "--get-regexp", ".*")
	cmd.Dir = repo
	cmd.Env = ghChildEnviron(caller, nil)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config under gh's environment: %v", err)
	}
	var command []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		scope, entry, _ := strings.Cut(line, "\t")
		switch scope {
		case "local":
		case "command":
			command = append(command, entry)
		default:
			t.Errorf("gh's git read %q from %s scope, want the repository's own and the command line alone", entry, scope)
		}
		if strings.HasPrefix(entry, "http.proxy") || strings.HasPrefix(entry, "http.sslcainfo") {
			t.Errorf("gh's git read %q (%s)", entry, scope)
		}
	}
	var want []string
	for _, pair := range append(sandbox.GitConfigPairs(), ghForcedGitConfig...) {
		want = append(want, strings.TrimSpace(gitConfigKey(pair[0])+" "+pair[1]))
	}
	for i, entry := range command {
		command[i] = strings.TrimSpace(entry)
	}
	if !slices.Equal(command, want) {
		t.Errorf("gh's git read %q from its command line, want the runtime's pairs and http.sslVerify, %q", command, want)
	}
}

// A repository whose own configuration, or a file it includes, sets an http ssl or proxy key,
// generic or URL-scoped, or a credential helper other than none or `legion git-credential`, is
// refused, naming the key and the file and never the value: git reads it before the command line,
// and a URL-scoped one beats every entry there. A Legion clone's own helpers, actions/checkout's
// extraheader, a key on the command line, a global file the environment never lets git read, and
// a directory that is no repository pass. A configuration git cannot read is refused.
func TestGhRepoGitConfigRefusalNamesWhatHandsTheTokenOn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	included := filepath.Join(t.TempDir(), "included.gitconfig")
	if err := os.WriteFile(included, []byte("[http]\n\tsslCAInfo = /tmp/example.pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), "global.gitconfig")
	if err := os.WriteFile(global, []byte("[http]\n\tproxy = http://127.0.0.1:9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legion := [][]string{
		{"credential.helper", ""}, {"--add", "credential.helper", workspace.GitHubCredentialHelper},
		{"credential.https://github.com.helper", ""}, {"--add", "credential.https://github.com.helper", workspace.GitHubCredentialHelper},
		{"credential.interactive", "false"}, {"credential.useHttpPath", "true"},
		{"http.https://github.com/.extraheader", "AUTHORIZATION: basic example"},
	}
	broken := repositoryWith(t)
	if err := os.WriteFile(filepath.Join(broken, ".git", "config"), []byte("[http\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valueless := repositoryWith(t)
	config, err := os.OpenFile(filepath.Join(valueless, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.WriteString("[http]\n\tsslVerify\n"); err != nil {
		t.Fatal(err)
	}
	if err := config.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		dir    string
		caller []string
		want   string // "" for a repository that passes
	}{
		{name: "a Legion clone", dir: repositoryWith(t, legion...)},
		{name: "no repository", dir: t.TempDir()},
		{name: "a caller's command-line proxy", dir: repositoryWith(t), caller: gitConfigEnviron([][2]string{{"http.proxy", "http://127.0.0.1:1"}})},
		{name: "a caller's global proxy", dir: repositoryWith(t), caller: []string{"GIT_CONFIG_GLOBAL=" + global}},
		{name: "a URL-scoped sslVerify off", dir: repositoryWith(t, []string{"http.https://github.com/acme/.sslVerify", "false"}), want: "sets http.https://github.com/acme/.sslverify"},
		{name: "a generic sslVerify off", dir: repositoryWith(t, []string{"http.sslVerify", "false"}), want: "sets http.sslverify"},
		{name: "a generic sslVerify 0", dir: repositoryWith(t, []string{"http.sslVerify", "0"}), want: "sets http.sslverify"},
		{name: "a generic sslVerify on", dir: repositoryWith(t, []string{"http.sslVerify", "true"})},
		{name: "a URL-scoped sslVerify yes", dir: repositoryWith(t, []string{"http.https://github.com/.sslVerify", "yes"})},
		{name: "an sslVerify with no value, which git reads as true", dir: valueless},
		{name: "a proxy", dir: repositoryWith(t, []string{"http.proxy", "http://127.0.0.1:1"}), want: "sets http.proxy"},
		{name: "a URL-scoped proxy", dir: repositoryWith(t, []string{"http.https://github.com/.proxy", "http://127.0.0.1:1"}), want: "sets http.https://github.com/.proxy"},
		{name: "an include of a file holding a CA", dir: repositoryWith(t, []string{"include.path", included}), want: "sets include.path, an include"},
		{name: "an include whose file is not there", dir: repositoryWith(t, []string{"include.path", "../absent.gitconfig"}), want: "sets include.path, an include"},
		{name: "another credential helper", dir: repositoryWith(t, []string{"credential.helper", "!/tmp/example-helper"}), want: "sets credential.helper to a helper other than"},
		{name: "another helper for github.com", dir: repositoryWith(t, append(legion, []string{"--add", "credential.https://github.com.helper", "store"})...), want: "sets credential.https://github.com.helper to a helper other than"},
		{name: "a configuration git cannot read", dir: broken, want: "cannot read the repository's git configuration"},
		{name: "an include on a branch gh's git can check out", dir: repositoryWith(t, []string{"includeIf.onbranch:feature.path", included}), want: "sets includeif.onbranch:feature.path, an include"},
		{name: "an include on a remote gh's git can add", dir: repositoryWith(t, []string{"includeIf.hasconfig:remote.*.url:https://github.com/acme/other**.path", included}), want: "sets includeif.hasconfig:remote.*.url:https://github.com/acme/other**.path, an include"},
		{name: "an include on a git directory this one is not", dir: repositoryWith(t, []string{"includeIf.gitdir:/nonexistent/.path", included}), want: "sets includeif.gitdir:/nonexistent/.path, an include"},
		{name: "an include on linked worktrees", dir: repositoryWith(t, []string{"includeIf.gitdir:**/worktrees/**.path", included}), want: "sets includeif.gitdir:**/worktrees/**.path, an include"},
		{name: "a configuration for each worktree", dir: repositoryWith(t, []string{"extensions.worktreeConfig", "true"}), want: "sets extensions.worktreeconfig, which gives each worktree"},
		{name: "no configuration for each worktree", dir: repositoryWith(t, []string{"extensions.worktreeConfig", "false"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.dir)
			caller := append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, tc.caller...)
			err := ghRepoGitConfigRefusal(context.Background(), ghChildEnviron(caller, nil))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("ghRepoGitConfigRefusal = %v, want none", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("ghRepoGitConfigRefusal = %v, want a refusal containing %q", err, tc.want)
			case err != nil && (strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "example-helper") || strings.Contains(err.Error(), "example.pem")):
				t.Fatalf("ghRepoGitConfigRefusal's refusal %q quotes the value", err)
			}
		})
	}
}

// With no git on PATH, ghRepoGitConfigRefusal reads nothing and refuses nothing: gh finds its git
// on the same PATH, so it runs none, and no configuration of one can send anything. With a git
// older than 2.26, which knows no --show-scope, it reads as with any other: a repository that
// sets sslVerify off is still refused, and a clean one passes. The old git here is a script that
// answers --show-scope as git 2.25 does and hands every other call to the git of the PATH the test
// started with, so a git wrapper on that PATH resolves the real git as it does outside the test.
// Handed on by its path under the script's own PATH, a wrapper that takes the first other git on
// PATH as the real one took the script, and the two ran each other until the read's timeout. The
// PATH is written into the script, since the refusal runs git with only the names gh keeps
// (ghChildEnviron). The read's bound, ghGitReadBound, is this binary's deadline here
// (boundByTheDeadline): at a box load of about 1,100 that script and the git it runs took past the
// product's 30 s, which the rows read as a refusal.
func TestGhRepoGitConfigRefusalWithNoGitOrAnOldGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	started := os.Getenv("PATH")
	off := repositoryWith(t, []string{"http.https://github.com/.sslVerify", "false"})
	clean := repositoryWith(t, []string{"user.name", "Legion test"})
	none := t.TempDir()
	old := t.TempDir()
	script := "#!/bin/sh\nfor arg; do\n  if [ \"$arg\" = --show-scope ]; then\n    echo \"error: unknown option \\`show-scope'\" >&2\n    exit 129\n  fi\ndone\n" +
		"PATH=" + shellWord(started) + "\nexec git \"$@\"\n"
	if err := testexec.WriteFile(filepath.Join(old, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, dir, want string }{
		{"no git on PATH, in a repository that sets sslVerify off", none, off, ""},
		{"a git older than 2.26, in a clean repository", old, clean, ""},
		{"a git older than 2.26, in a repository that sets sslVerify off", old, off, "sets http.https://github.com/.sslverify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			t.Chdir(tc.dir)
			err := ghRepoGitConfigRefusal(context.Background(), ghChildEnviron([]string{"PATH=" + tc.path, "HOME=" + t.TempDir()}, nil))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("ghRepoGitConfigRefusal = %v, want none", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("ghRepoGitConfigRefusal = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// withoutRecursionOff is env with ghForcedGitConfig's recursion pairs taken out of its
// GIT_CONFIG_COUNT block: the environment gh's git got before they were forced.
func withoutRecursionOff(env []string) []string {
	values := map[string]string{}
	var rest []string
	for _, pair := range env {
		name, value, _ := strings.Cut(pair, "=")
		if name == "GIT_CONFIG_COUNT" || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			values[name] = value
			continue
		}
		rest = append(rest, pair)
	}
	count, _ := strconv.Atoi(values["GIT_CONFIG_COUNT"])
	var pairs [][2]string
	for i := range count {
		key := values["GIT_CONFIG_KEY_"+strconv.Itoa(i)]
		switch gitConfigKey(key) {
		case "submodule.recurse", "fetch.recursesubmodules", "push.recursesubmodules":
			continue
		}
		pairs = append(pairs, [2]string{key, values["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]})
	}
	return append(rest, gitConfigEnviron(pairs)...)
}

// The fetch and the pull gh's git makes recurse into no submodule, whose own configuration
// (.git/modules/<name>/config) no check of `legion gh` reads. The superproject's submodule is
// populated, its own configuration sets a URL-scoped http.sslVerify=false, and the upstream's new
// commit moves its gitlink to a commit it lacks; the caller's proxy is a man-in-the-middle proxy
// whose CA nothing trusts, and the credential helper answers a token, as gh's own -c helper does.
// git's default fetch.recurseSubmodules is on-demand, so with no flag and no setting such a fetch
// went to the submodule and handed the proxy the token: the control, run with the environment
// before ghForcedGitConfig turned recursion off, must leak, so the rows can see one. Under the
// environment `legion gh` gives gh's git, no fetch reaches the submodule: not by default, not
// with submodule.recurse and fetch.recurseSubmodules true in the repository's own configuration,
// and not with fetchRecurseSubmodules true in the .gitmodules of HEAD or of the fetched commit.
func TestGhChildGitRecursesIntoNoSubmodule(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	proxy := startCaptureProxy(t)
	type fixture struct {
		local                       [][2]string
		headModules, fetchedModules string
	}
	// superproject is a superproject as fixture says, its helper, and the caller's environment.
	superproject := func(t *testing.T, f fixture) (dir, helper string, caller []string) {
		t.Helper()
		scratch := t.TempDir()
		helper = filepath.Join(scratch, "helper.sh")
		if err := testexec.WriteFile(helper, []byte("#!/bin/sh\n[ \"$1\" = get ] || exit 0\nprintf 'username=x-access-token\\npassword=ghs_submodule_fake\\n'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		iso := []string{"PATH=" + filepath.Dir(git) + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid", "GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid"}
		run := func(dir string, args ...string) {
			t.Helper()
			cmd := exec.Command(git, args...)
			cmd.Dir, cmd.Env = dir, iso
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %q: %v: %s", args, err, out)
			}
		}
		appendTo := func(path, text string) {
			t.Helper()
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(text); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		lib, dir, upstream, work := filepath.Join(scratch, "lib"), filepath.Join(scratch, "super"), filepath.Join(scratch, "upstream.git"), filepath.Join(scratch, "work")
		for _, d := range []string{lib, dir} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		run(lib, "init", "-q", "-b", "main")
		run(lib, "commit", "-q", "--allow-empty", "-m", "lib")
		run(dir, "init", "-q", "-b", "main")
		run(dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "sub")
		run(dir, "commit", "-q", "-m", "add sub")
		if f.headModules != "" {
			appendTo(filepath.Join(dir, ".gitmodules"), f.headModules)
			run(dir, "commit", "-q", "-am", "recurse into sub")
		}
		run(scratch, "clone", "-q", "--bare", dir, upstream)
		run(dir, "remote", "add", "origin", upstream)
		run(dir, "fetch", "-q", "origin")
		run(dir, "branch", "-q", "--set-upstream-to=origin/main")
		run(filepath.Join(dir, "sub"), "remote", "set-url", "origin", "https://github.com/acme/sub")
		run(filepath.Join(dir, "sub"), "config", "http.https://github.com/.sslVerify", "false")
		for _, pair := range f.local {
			run(dir, "config", pair[0], pair[1])
		}
		run(scratch, "clone", "-q", upstream, work)
		run(work, "update-index", "--cacheinfo", "160000,1111111111111111111111111111111111111111,sub")
		if f.fetchedModules != "" {
			appendTo(filepath.Join(work, ".gitmodules"), f.fetchedModules)
			run(work, "add", ".gitmodules")
		}
		run(work, "commit", "-q", "-m", "move sub")
		run(work, "push", "-q", "origin", "main")
		return dir, helper, append(append([]string{}, iso[:2]...), "HTTPS_PROXY="+proxy.url, "https_proxy="+proxy.url, "NO_PROXY=", "no_proxy=")
	}
	// sends runs command in dir as gh runs its git, with env and gh's -c credential helper, and
	// says whether the proxy read a credential, and what git printed.
	sends := func(dir, helper string, env, command []string) (bool, string) {
		proxy.take()
		cmd := exec.Command(git, append([]string{"-c", "credential.https://github.com.helper=", "-c", "credential.https://github.com.helper=" + helper, "-c", "credential.interactive=false"}, command...)...)
		cmd.Dir, cmd.Env = dir, env
		out, _ := cmd.CombinedOutput()
		for _, r := range proxy.take() {
			if r.auth != "" {
				return true, string(out)
			}
		}
		return false, string(out)
	}
	fetch := []string{"fetch", "origin", "--no-tags"}
	pull := []string{"pull", "--ff-only", "origin", "main"}
	recurse := "\tfetchRecurseSubmodules = true\n"

	dir, helper, caller := superproject(t, fixture{})
	env, err := withGitHubUseHTTPPath(ghChildEnviron(caller, nil))
	if err != nil {
		t.Fatal(err)
	}
	if leaked, out := sends(dir, helper, withoutRecursionOff(env), fetch); !leaked {
		t.Fatalf("the control fetch, with git's default recursion, sent the proxy no credential (%q): this fixture cannot show a submodule's fetch", out)
	}
	for _, tc := range []struct {
		name    string
		fixture fixture
		command []string
	}{
		{"a fetch", fixture{}, fetch},
		{"a pull", fixture{}, pull},
		{"a fetch, with submodule.recurse and fetch.recurseSubmodules true in the repository", fixture{local: [][2]string{{"submodule.recurse", "true"}, {"fetch.recurseSubmodules", "true"}}}, fetch},
		{"a pull, with submodule.recurse true in the repository", fixture{local: [][2]string{{"submodule.recurse", "true"}}}, pull},
		{"a fetch, with fetchRecurseSubmodules true in HEAD's .gitmodules", fixture{headModules: recurse}, fetch},
		{"a fetch, with fetchRecurseSubmodules true in the fetched commit's .gitmodules", fixture{fetchedModules: recurse}, fetch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, helper, caller := superproject(t, tc.fixture)
			env, err := withGitHubUseHTTPPath(ghChildEnviron(caller, nil))
			if err != nil {
				t.Fatal(err)
			}
			if leaked, out := sends(dir, helper, env, tc.command); leaked || strings.Contains(out, "Fetching submodule") {
				t.Errorf("gh's git %q sent the proxy a credential: %v, and printed %q; want no fetch of the submodule", tc.command, leaked, out)
			}
		})
	}
}

// `legion gh` refuses, before gh runs, what has gh's git recurse into a submodule or take
// configuration from the call: `pr checkout --recurse-submodules`, and any git flag a clone leaf
// (repo clone, repo fork, gist clone) passes on that ghCloneGitFlagTakes does not hold, spelled in
// full, abbreviated, joined, with `=`, or after a second `--`. It reads every flag passed on, not
// the first alone: the second review's `-- --depth 1 -c ...` is refused at its `-c`. The flags it
// holds, a directory first, pass, scripts/e2e's fixture clone among them, at gh 2.104.0 and 2.98.0.
func TestGhTargetOfRefusesRecursionAndUnreadCloneFlags(t *testing.T) {
	off, proxy := "http.https://github.com/.sslVerify=false", "http.https://github.com/.proxy=http://127.0.0.1:1"
	for _, version := range []string{"2.104.0", "2.98.0"} {
		help := savedGhHelpOf(t, version)
		for _, tc := range []struct {
			argv   []string
			refuse string // "" for a call that passes
		}{
			{[]string{"pr", "checkout", "1", "-R", "acme/widgets", "--recurse-submodules"}, `"--recurse-submodules" runs`},
			{[]string{"pr", "checkout", "1", "-R", "acme/widgets", "--recurse-submodules=false"}, `"--recurse-submodules=false" runs`},
			{[]string{"repo", "clone", "acme/widgets", "w2", "--", "-c", off, "-c", proxy}, `hands "-c" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--depth", "1", "-c", off}, `hands "-c" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--config", off}, `hands "--config" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--config=" + off}, `hands "--config=` + off + `" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--conf", off}, `hands "--conf" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--recurse-submodules"}, `hands "--recurse-submodules" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--recurse-submodules=lib"}, `hands "--recurse-submodules=lib" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--recursive"}, `hands "--recursive" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--template", "/tmp/example-template"}, `hands "--template" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--template=/tmp/example-template"}, `hands "--template=/tmp/example-template" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "-u", "/tmp/example-upload-pack"}, `hands "-u" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--upload-pack", "/tmp/example-upload-pack"}, `hands "--upload-pack" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--reference", "/tmp/example-reference"}, `hands "--reference" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--reference-if-able", "/tmp/example-reference"}, `hands "--reference-if-able" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--separate-git-dir", "/tmp/example-git-dir"}, `hands "--separate-git-dir" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "-qn"}, `hands "-qn" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "-bmain"}, `hands "-bmain" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "-b=main"}, `hands "-b=main" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--quiet=true"}, `hands "--quiet=true" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--depth"}, `hands "--depth" to git clone with no value`},
			{[]string{"repo", "clone", "acme/widgets", "widgets", "extra"}, `hands "extra" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "widgets", "--", "--recursive"}, `hands "--" to git clone`},
			{[]string{"repo", "fork", "acme/widgets", "--clone", "--", "--recursive"}, `hands "--recursive" to git clone`},
			{[]string{"repo", "fork", "acme/widgets", "--clone", "--", "-c", off}, `hands "-c" to git clone`},
			{[]string{"gist", "clone", "0123456789abcdef", "--", "-c", "core.hooksPath=/tmp/example-hooks"}, `hands "-c" to git clone`},
			{[]string{"pr", "checkout", "1", "-R", "acme/widgets"}, ""},
			{[]string{"repo", "clone", "acme/widgets", "widgets", "--", "--quiet", "--depth", "1", "--branch", "main"}, ""},
			{[]string{"repo", "clone", "acme/widgets", "--", "--depth=1", "--filter=blob:none", "--single-branch", "-b", "main", "--no-tags", "--origin", "up"}, ""},
			{[]string{"repo", "clone", "--", "acme/widgets", "widgets", "--bare", "-q", "--branch", "--recursive"}, ""},
			{[]string{"repo", "fork", "acme/widgets", "--clone", "--", "--depth", "1"}, ""},
			{[]string{"gist", "clone", "0123456789abcdef", "notes", "--", "--quiet"}, ""},
			{[]string{"pr", "checkout", "1", "-R", "acme/widgets", "--worktree", "/tmp/example-worktree"}, ""},
		} {
			_, err := ghTargetOf(tc.argv, nil, help, noGitRepository, ownersOwnDirectories)
			switch {
			case tc.refuse == "" && err != nil:
				t.Errorf("ghTargetOf(%q) at gh %s = %v, want no refusal", tc.argv, version, err)
			case tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)):
				t.Errorf("ghTargetOf(%q) at gh %s = %v, want a refusal containing %q", tc.argv, version, err, tc.refuse)
			}
		}
	}
}

// The configuration a git of gh's reads later, in another worktree or after a checkout, is the one
// ghRepoGitConfigRefusal read before gh ran, or the call is refused. Three routes the third side
// review measured, each built here so that the check's own directory reads clean but for the key
// that makes the later read differ, and each with its control: the route, run as gh's git runs it
// (`legion gh`'s environment, gh's -c credential helper, the caller's proxy a man-in-the-middle
// proxy whose CA nothing trusts), hands the proxy the token, so a refusal here is what stops it.
//
//   - W1: an includeIf.gitdir:**/worktrees/** in the repository's .git/config, inactive in the main
//     worktree, where the check reads; `pr checkout --worktree <linked worktree>` runs `git -C
//     <worktree> fetch`, where it applies.
//   - I1: an include.path of a tracked file the checked-out branch lacks and the base branch
//     holds; `pr merge --delete-branch` checks out the base branch, then pulls.
//   - extensions.worktreeConfig, with a URL-scoped sslVerify=false in a linked worktree's own
//     config.worktree, which the main worktree's read does not hold.
func TestGhRepoGitConfigRefusalSeesWhatALaterGitReads(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	proxy := startCaptureProxy(t)
	scratch := t.TempDir()
	helper := filepath.Join(scratch, "helper.sh")
	if err := testexec.WriteFile(helper, []byte("#!/bin/sh\n[ \"$1\" = get ] || exit 0\nprintf 'username=x-access-token\\npassword=ghs_later_read_fake\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	iso := []string{"PATH=" + filepath.Dir(git) + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid", "GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid"}
	must := func(dir string, args ...string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(git, args...)
		cmd.Dir, cmd.Env = dir, iso
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %q in %s: %v: %s", args, dir, err, out)
		}
	}
	caller := append(append([]string{}, iso[:2]...), "HTTPS_PROXY="+proxy.url, "https_proxy="+proxy.url, "NO_PROXY=", "no_proxy=",
		"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid", "GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid")
	env, err := withGitHubUseHTTPPath(ghChildEnviron(caller, nil))
	if err != nil {
		t.Fatal(err)
	}
	// asGh runs args in dir as gh's git runs them and says whether the proxy read a credential.
	asGh := func(dir string, args ...string) (bool, string) {
		proxy.take()
		cmd := exec.Command(git, append([]string{"-c", "credential.https://github.com.helper=", "-c", "credential.https://github.com.helper=" + helper, "-c", "credential.interactive=false"}, args...)...)
		cmd.Dir, cmd.Env = dir, env
		out, _ := cmd.CombinedOutput()
		for _, r := range proxy.take() {
			if r.auth != "" {
				return true, string(out)
			}
		}
		return false, string(out)
	}
	off := "[http \"https://github.com/\"]\n\tsslVerify = false\n"
	for _, tc := range []struct {
		name string
		// build makes the repository and answers the directory `legion gh` reads in and the
		// route, the git commands gh's git runs after it.
		build func(root string) (string, [][]string)
		want  string
	}{
		{"W1 an includeIf.gitdir on linked worktrees, then `git -C <worktree> fetch`", func(root string) (string, [][]string) {
			repo, wt := filepath.Join(root, "main"), filepath.Join(root, "wt")
			must(repo, "init", "-q", "-b", "main")
			must(repo, "commit", "-q", "--allow-empty", "-m", "c1")
			must(repo, "remote", "add", "origin", "https://github.com/acme/widgets")
			must(repo, "worktree", "add", "-q", "-b", "wtb", wt)
			evil := filepath.Join(root, "evil.gitconfig")
			if err := os.WriteFile(evil, []byte(off), 0o600); err != nil {
				t.Fatal(err)
			}
			must(repo, "config", "includeIf.gitdir:**/worktrees/**.path", evil)
			return repo, [][]string{{"-C", wt, "fetch", "origin", "refs/heads/main", "--no-tags"}}
		}, "sets includeif.gitdir:**/worktrees/**.path, an include"},
		{"I1 an include of a file only the base branch holds, then checkout and pull", func(root string) (string, [][]string) {
			repo := filepath.Join(root, "repo")
			must(repo, "init", "-q", "-b", "main")
			if err := os.WriteFile(filepath.Join(repo, "evil.gitconfig"), []byte(off), 0o600); err != nil {
				t.Fatal(err)
			}
			must(repo, "add", "evil.gitconfig")
			must(repo, "commit", "-q", "-m", "main holds the file")
			must(repo, "remote", "add", "origin", "https://github.com/acme/widgets")
			must(repo, "checkout", "-q", "-b", "feature")
			must(repo, "rm", "-q", "evil.gitconfig")
			must(repo, "commit", "-q", "-m", "feature lacks it")
			must(repo, "config", "include.path", "../evil.gitconfig")
			return repo, [][]string{{"checkout", "-q", "main"}, {"pull", "--ff-only", "origin", "main"}}
		}, "sets include.path, an include"},
		{"a linked worktree's own config.worktree, then `git -C <worktree> fetch`", func(root string) (string, [][]string) {
			repo, wt := filepath.Join(root, "main"), filepath.Join(root, "wt")
			must(repo, "init", "-q", "-b", "main")
			must(repo, "commit", "-q", "--allow-empty", "-m", "c1")
			must(repo, "remote", "add", "origin", "https://github.com/acme/widgets")
			must(repo, "worktree", "add", "-q", "-b", "wtb", wt)
			must(repo, "config", "core.repositoryFormatVersion", "1")
			must(repo, "config", "extensions.worktreeConfig", "true")
			must(wt, "config", "--worktree", "http.https://github.com/.sslVerify", "false")
			return repo, [][]string{{"-C", wt, "fetch", "origin", "refs/heads/main", "--no-tags"}}
		}, "sets extensions.worktreeconfig, which gives each worktree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The control, in a repository of its own: the route hands the proxy the token.
			control, route := tc.build(t.TempDir())
			leaked := false
			var said []string
			for _, args := range route {
				sent, out := asGh(control, args...)
				leaked = leaked || sent
				said = append(said, out)
			}
			if !leaked {
				t.Fatalf("the control route %q sent the proxy no credential (%q): this fixture cannot show the route", route, said)
			}
			dir, _ := tc.build(t.TempDir())
			t.Chdir(dir)
			err := ghRepoGitConfigRefusal(context.Background(), ghChildEnviron(caller, nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ghRepoGitConfigRefusal in %s = %v, want a refusal containing %q, so gh's git never runs the route", dir, err, tc.want)
			}
		})
	}
}
