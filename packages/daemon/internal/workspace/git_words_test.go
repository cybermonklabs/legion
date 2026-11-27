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
// each of words with before called on it first and after, when set, once it has run: what GitHub,
// or the issue's volume, does while a case's fetch runs.
type aroundGit struct {
	*recordingRunner
	words  []string
	before func(command *Command)
	after  func(command Command)
}

func (r aroundGit) Run(ctx context.Context, command Command) (Result, error) {
	if command.Argv[0] != "git" || slices.ContainsFunc(r.words, func(word string) bool { return !slices.Contains(command.Argv, word) }) {
		return r.recordingRunner.Run(ctx, command)
	}
	command.Argv = slices.Clone(command.Argv)
	r.before(&command)
	result, err := r.recordingRunner.Run(ctx, command)
	if r.after != nil {
		r.after(command)
	}
	return result, err
}

// gitDirOf is the repository argv's --git-dir names.
func gitDirOf(t *testing.T, argv []string) string {
	t.Helper()
	for _, arg := range argv {
		if dir, ok := strings.CutPrefix(arg, "--git-dir="); ok {
			return dir
		}
	}
	t.Fatalf("%q names no --git-dir", argv)
	return ""
}

// unwritable takes the write permission away from dir until the returned func gives it back, as a
// volume that refuses a write does; the test's end gives it back too.
func unwritable(t *testing.T, dir string) func() {
	t.Helper()
	restore := func() { _ = os.Chmod(dir, 0o755) }
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	return restore
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

// What GitHub or the repository cache wrote reaches what Fetch prints through git, and a stand-in
// at either address can write a credential there: a URL with user information, plain or
// percent-encoded, in a ref the cache streams in packed-refs, which fsck names as it refuses the
// name (fsck-userinfo, fsck-userinfo-encoded); one in the branch GitHub's HEAD names, which git's
// ls-remote prints as a protocol-v2 server wrote it and a line logs when the fetch did not bring
// it (log-userinfo); and a token the fetch holds, as written and percent-encoded, in the branch
// GitHub's HEAD names, which the delta's refspecs name when the delta fails (delta-refspec) and
// HEAD's move names when it fails (follow-ref), in a HEAD naming no branch, which the fetch refuses
// naming it (head-not-branch), and in a ref the cache streamed that the fetch drops, whose removal
// names it when it fails (prune-ref). Each goes through the fetch's one filter, and a stderr an
// error prints through the fail-closed rule too, so neither Fetch's error nor its log holds a
// credential or a run of a token, as written or percent-decoded. Each case first shows git itself
// says the credential; the controls show what still names the failure, each credential's place
// redacted. The cases whose words hold the probe's user and password fetch under tokens that share
// no run with them (testCacheToken, echoedFetchToken), so only the URL rule and the fail-closed
// rule can take them out. Captures workspace.git-words/<case>.
func TestWhatGitSaysOfGitHubsOrTheCachesWordsPrintsThroughTheFetchsFilter(t *testing.T) {
	const (
		sha             = "0123456789abcdef0123456789abcdef01234567"
		userinfo        = "https://not-a-secret-user:not-a-secret-password@example.com/x"
		userinfoEncoded = "https://not-a-secret-user%3Anot-a-secret-password%40example.com/x"
	)
	encodedFetch := percentEncoded(echoedFetchToken, func(int) bool { return true }, false)
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
	// called is the argv of the first command run recorded that holds word, joined.
	called := func(t *testing.T, run *recordingRunner, word string) string {
		t.Helper()
		for _, call := range run.Calls() {
			if slices.Contains(call.Argv, word) {
				return strings.Join(call.Argv, " ")
			}
		}
		t.Fatalf("no command holds %q", word)
		return ""
	}
	for _, tc := range []struct{ name, url, says string }{
		{"fsck-userinfo", userinfo, "packed refname is dangerous: refs/heads/https://[a URL with credentials]/x"},
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
		if !strings.Contains(said, "GitHub's default branch of acme/widgets is now https://[a URL with credentials]/x, which this fetch did not bring") {
			t.Errorf("Fetch logged %q, want the branch GitHub's HEAD names with its authority, which holds its user information, as the mark of a URL with credentials", logged)
		}
	})
	t.Run("delta-refspec", func(t *testing.T) {
		run := newLocalRunner(t)
		pushRemoteBranch(t, run.remote, "main", "history.txt")
		cache := cacheOf(t, run.remote)
		head := "refs/heads/" + echoedFetchToken
		runSetup(t, run.remote, "git", "--git-dir="+run.remote, "update-ref", head, "refs/heads/main")
		runSetup(t, run.remote, "git", "--git-dir="+run.remote, "update-ref", "refs/heads/"+encodedFetch, "refs/heads/main")
		runSetup(t, run.remote, "git", "--git-dir="+run.remote, "symbolic-ref", "HEAD", head)
		req := fromCache(t, cacheTar(t, cache), echoedCacheToken)
		req.RecordedDefault, req.Clock = encodedFetch, newTestClock()
		// GitHub drops the branch its HEAD named between the fetch's read of HEAD and its delta.
		dropped := false
		delta := aroundGit{recordingRunner: run, words: []string{"fetch", GitHubURL(req.Repo)}, before: func(*Command) {
			if !dropped {
				runSetup(t, run.remote, "git", "--git-dir="+run.remote, "update-ref", "-d", head)
				dropped = true
			}
		}}
		_, err := Fetch(context.Background(), delta, req)
		if argv := called(t, run, "fetch"); !strings.Contains(argv, echoedFetchToken) {
			t.Fatalf("the delta ran %q, want its refspecs naming the branch GitHub's HEAD names: the case proves nothing", argv)
		}
		said := errorText(err)
		requireNoFetchEcho(t, "workspace.git-words/delta-refspec", said)
		if strings.Count(said, "+refs/heads/***:refs/heads/***") != 2 || !strings.Contains(said, "couldn't find remote ref refs/heads/***") {
			t.Errorf("Fetch = %v, want the delta's failure naming both its exact refspecs and git's refusal, each token's place redacted", err)
		}
	})
	t.Run("head-not-branch", func(t *testing.T) {
		run := newLocalRunner(t)
		pushRemoteBranch(t, run.remote, "main", "history.txt")
		cache := cacheOf(t, run.remote)
		tag := "refs/tags/" + echoedFetchToken + "/" + encodedFetch
		runSetup(t, run.remote, "git", "--git-dir="+run.remote, "update-ref", tag, "refs/heads/main")
		runSetup(t, run.remote, "git", "--git-dir="+run.remote, "symbolic-ref", "HEAD", tag)
		if said := gitSays(t, run.remote, "ls-remote", "--symref", run.remote, "HEAD"); !strings.Contains(said, echoedFetchToken) {
			t.Fatalf("git ls-remote says %q of HEAD, want the token in the tag it names: the case proves nothing", said)
		}
		_, err := Fetch(context.Background(), run, fromCache(t, cacheTar(t, cache), echoedCacheToken))
		said := errorText(err)
		requireNoFetchEcho(t, "workspace.git-words/head-not-branch", said)
		if !strings.Contains(said, `GitHub's HEAD of acme/widgets names "refs/tags/***/***", not a branch`) {
			t.Errorf("Fetch = %v, want the refusal of a HEAD naming no branch, naming it with each token's place redacted", err)
		}
	})
	t.Run("prune-ref", func(t *testing.T) {
		run := newLocalRunner(t)
		pushRemoteBranch(t, run.remote, "main", "history.txt")
		cache := cacheOf(t, run.remote)
		runSetup(t, cache, "git", "--git-dir="+cache, "update-ref", "refs/heads/"+echoedCacheToken+"-"+encodedFetch, "refs/heads/main")
		// The volume refuses the lock the removal takes, as a full or read-only volume does.
		var restore func()
		prune := aroundGit{recordingRunner: run, words: []string{"update-ref", "-d"},
			before: func(command *Command) {
				restore = unwritable(t, filepath.Join(gitDirOf(t, command.Argv), "refs", "heads"))
			},
			after: func(Command) { restore() }}
		_, err := Fetch(context.Background(), prune, fromCache(t, cacheTar(t, cache), echoedCacheToken))
		if argv := called(t, run, "update-ref"); !strings.Contains(argv, echoedCacheToken) {
			t.Fatalf("the removal ran %q, want it naming the ref the cache streamed: the case proves nothing", argv)
		}
		said := errorText(err)
		requireNoFetchEcho(t, "workspace.git-words/prune-ref", said)
		if !strings.Contains(said, "update-ref --no-deref -d refs/heads/***-***\n") || !strings.Contains(said, "cannot lock ref 'refs/heads/***-***'") {
			t.Errorf("Fetch = %v, want the failed removal naming the ref, and git's refusal, each token's place redacted", err)
		}
	})
	t.Run("follow-ref", func(t *testing.T) {
		run := newLocalRunner(t)
		pushRemoteBranch(t, run.remote, "main", "history.txt")
		cache := cacheOf(t, run.remote)
		head := "refs/heads/" + echoedFetchToken + "-" + encodedFetch
		runSetup(t, run.remote, "git", "--git-dir="+run.remote, "update-ref", head, "refs/heads/main")
		runSetup(t, run.remote, "git", "--git-dir="+run.remote, "symbolic-ref", "HEAD", head)
		// The volume refuses the lock HEAD's move takes.
		var restore func()
		follow := aroundGit{recordingRunner: run, words: []string{"symbolic-ref", head},
			before: func(command *Command) { restore = unwritable(t, gitDirOf(t, command.Argv)) },
			after:  func(Command) { restore() }}
		_, err := Fetch(context.Background(), follow, fromCache(t, cacheTar(t, cache), echoedCacheToken))
		if argv := called(t, run, head); !strings.Contains(argv, "symbolic-ref HEAD") {
			t.Fatalf("HEAD's move ran %q, want it naming the branch GitHub's HEAD names: the case proves nothing", argv)
		}
		said := errorText(err)
		requireNoFetchEcho(t, "workspace.git-words/follow-ref", said)
		if !strings.Contains(said, "symbolic-ref HEAD refs/heads/***-***\n") || !strings.Contains(said, "HEAD.lock': Permission denied") {
			t.Errorf("Fetch = %v, want HEAD's failed move naming the branch, and git's refusal, each token's place redacted", err)
		}
	})
}

// redactPassword is the password the cases' URLs carry, one of the check's markers.
const redactPassword = "not-a-secret-password"
