package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// aroundGit runs every command as its recordingRunner does, and a git command whose argv holds
// each of words with before called on it first: what GitHub does while a case's fetch runs.
type aroundGit struct {
	*recordingRunner
	words  []string
	before func(command *Command)
}

func (r aroundGit) Run(ctx context.Context, command Command) (Result, error) {
	if command.Argv[0] != "git" || slices.ContainsFunc(r.words, func(word string) bool { return !slices.Contains(command.Argv, word) }) {
		return r.recordingRunner.Run(ctx, command)
	}
	command.Argv = slices.Clone(command.Argv)
	r.before(&command)
	return r.recordingRunner.Run(ctx, command)
}

// gitSays is what git, run in dir under the fixtures' environment, prints of args, stdout and
// stderr together, whatever its exit: what git itself says, before a case's filter reads it.
func gitSays(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir, command.Env = dir, setupEnvironment()
	said, _ := command.CombinedOutput()
	return string(said)
}

// uploadPackNaming is a stand-in for git-upload-pack, for --upload-pack: a protocol-v2 server whose
// ls-refs answer, whatever it is asked, names HEAD at tip with its symbolic target target as the
// server writes it, and refs/heads/main at tip. git's ls-remote checks no name a v2 server sends.
func uploadPackNaming(t *testing.T, tip, target string) string {
	t.Helper()
	pkt := func(line string) string { return fmt.Sprintf("%04x%s\n", len(line)+5, line) }
	answer := pkt("version 2") + pkt("ls-refs") + "0000" + pkt(tip+" HEAD symref-target:"+target) + pkt(tip+" refs/heads/main") + "0000"
	script := filepath.Join(t.TempDir(), "upload-pack")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' '"+answer+"'\ncat >/dev/null\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// What the repository cache or GitHub wrote reaches what Fetch prints through git, and a stand-in
// at either address can write a credential there: a URL with user information, plain or
// percent-encoded, in a ref the cache streams in packed-refs, which fsck names as it refuses the
// name (fsck-userinfo, fsck-userinfo-encoded), and one in the branch GitHub's HEAD names, which
// git's ls-remote prints as a protocol-v2 server wrote it and a line logs when the fetch did not
// bring it (log-userinfo). Each goes through the fetch's one filter, which holds the URL rule, and
// fsck's stderr an error prints through the fail-closed rule too, so neither Fetch's error nor its
// log holds the credential. Each case first shows git itself says it; the controls show what still
// names the failure, the user information taken out or the words not printed. The cases fetch
// under tokens that share no run with the probe's user and password (testCacheToken,
// echoedFetchToken), so only the URL rule and the fail-closed rule can take them out. Captures
// workspace.git-words/<case>.
func TestWhatGitSaysOfGitHubsOrTheCachesWordsPrintsThroughTheFetchsFilter(t *testing.T) {
	const (
		sha             = "0123456789abcdef0123456789abcdef01234567"
		userinfo        = "https://not-a-secret-user:not-a-secret-password@example.com/x"
		userinfoEncoded = "https://not-a-secret-user%3Anot-a-secret-password%40example.com/x"
	)
	// fromCache is the cases' request: their provisioning token, and a stand-in cache streaming
	// body that takes cacheToken.
	fromCache := func(t *testing.T, body []byte, cacheToken string) FetchRequest {
		req := fetchRequest(t)
		req.Token = staticToken(echoedFetchToken)
		req.CacheURL, req.CacheToken = (&fakeCache{body: body, token: cacheToken}).serve(t), cacheToken
		return req
	}
	errorText := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	for _, tc := range []struct{ name, url, says string }{
		{"fsck-userinfo", userinfo, "packed refname is dangerous: refs/heads/https://example.com/x"},
		{"fsck-userinfo-encoded", userinfoEncoded, oauth2calltest.Said(userinfoEncoded).String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			packed := sha + " refs/heads/" + tc.url + "\n"
			twin := filepath.Join(t.TempDir(), "twin.git")
			runSetup(t, filepath.Dir(twin), "git", "init", "--bare", "--quiet", twin)
			if err := os.WriteFile(filepath.Join(twin, "packed-refs"), []byte(packed), 0o644); err != nil {
				t.Fatal(err)
			}
			if said := gitSays(t, twin, "--git-dir="+twin, "fsck", "--connectivity-only", "--no-dangling", "--no-progress"); !strings.Contains(said, redactPassword) {
				t.Fatalf("git fsck says %q of the ref, want its user information in it: the case proves nothing", said)
			}
			_, err := Fetch(context.Background(), newLocalRunner(t), fromCache(t, tarOf(t, [2]string{"HEAD", "ref: refs/heads/main\n"}, [2]string{"packed-refs", packed}), testCacheToken))
			said := errorText(err)
			requireNoFetchEcho(t, "workspace.git-words/"+tc.name, said)
			if !strings.Contains(said, "the repository cache's history is not whole: command failed") || !strings.Contains(said, tc.says) {
				t.Errorf("Fetch = %v, want fsck's refusal of the ref, %q in it", err, tc.says)
			}
		})
	}
	t.Run("log-userinfo", func(t *testing.T) {
		run := newLocalRunner(t)
		pushRemoteBranch(t, run.remote, "main", "history.txt")
		req := fetchRequest(t)
		req.Token = staticToken(echoedFetchToken)
		if _, err := Fetch(context.Background(), run, req); err != nil {
			t.Fatalf("Fetch filling the feed: %v", err)
		}
		tip := strings.TrimSpace(runSetup(t, run.remote, "git", "--git-dir="+run.remote, "rev-parse", "refs/heads/main"))
		standIn := uploadPackNaming(t, tip, "refs/heads/"+userinfo)
		if said := gitSays(t, run.remote, "ls-remote", "--symref", "--upload-pack="+standIn, run.remote, "HEAD"); !strings.Contains(said, redactPassword) {
			t.Fatalf("git ls-remote says %q of the stand-in's HEAD, want its user information in it: the case proves nothing", said)
		}
		var logged []string
		req.Log = func(line string) { logged = append(logged, line) }
		lsRemote := aroundGit{recordingRunner: run, words: []string{"ls-remote"}, before: func(command *Command) {
			command.Argv = slices.Insert(command.Argv, slices.Index(command.Argv, "ls-remote")+1, "--upload-pack="+standIn)
		}}
		if _, err := Fetch(context.Background(), lsRemote, req); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		said := strings.Join(logged, "\n")
		requireNoFetchEcho(t, "workspace.git-words/log-userinfo", said)
		if !strings.Contains(said, "GitHub's default branch of acme/widgets is now https://example.com/x, which this fetch did not bring") {
			t.Errorf("Fetch logged %q, want the branch GitHub's HEAD names with its user information taken out", logged)
		}
	})
}

// redactPassword is the password the cases' URLs carry, one of the check's markers.
const redactPassword = "not-a-secret-password"
