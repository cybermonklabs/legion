package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

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
// key and value alike, a key cased otherwise matching as git matches it, and then http.sslVerify
// on. Every other pair the caller names, a proxy, a CA, sslVerify off, a URL rewrite, an include,
// another credential helper, an ssh command or another hooks path, is dropped however it is cased
// or scoped. A count past the pairs the environment holds reads the pairs there, and a count that
// is no count reads none.
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
// names a proxy here, which would be handed the token gh's credential helper answers.
func TestGhChildGitReadsNoConfigurationOfTheCallers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	home := t.TempDir()
	proxyFile := "[http]\n\tproxy = http://127.0.0.1:1\n\tsslCAInfo = /tmp/example.pem\n"
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
		"GIT_CONFIG_PARAMETERS='http.proxy'='http://127.0.0.1:2'",
	}, gitConfigEnviron(append(sandbox.GitConfigPairs(), [2]string{"http.proxy", "http://127.0.0.1:3"}))...)
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
		{name: "a proxy", dir: repositoryWith(t, []string{"http.proxy", "http://127.0.0.1:1"}), want: "sets http.proxy"},
		{name: "a URL-scoped proxy", dir: repositoryWith(t, []string{"http.https://github.com/.proxy", "http://127.0.0.1:1"}), want: "sets http.https://github.com/.proxy"},
		{name: "a CA an include names", dir: repositoryWith(t, []string{"include.path", included}), want: "(file:" + included + ") sets http.sslcainfo"},
		{name: "another credential helper", dir: repositoryWith(t, []string{"credential.helper", "!/tmp/example-helper"}), want: "sets credential.helper to a helper other than"},
		{name: "another helper for github.com", dir: repositoryWith(t, append(legion, []string{"--add", "credential.https://github.com.helper", "store"})...), want: "sets credential.https://github.com.helper to a helper other than"},
		{name: "a configuration git cannot read", dir: broken, want: "cannot read the repository's git configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.dir)
			caller := append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, tc.caller...)
			err := ghRepoGitConfigRefusal(ghChildEnviron(caller, nil))
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
