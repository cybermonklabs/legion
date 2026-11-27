package repocache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// TestNarrowingTheCloneNamesNoRefOfIt: whatever answers the cache's fetch in GitHub's place names
// a branch with the provisioning token the fetch carried, and the fetch writes it into the clone
// as a ref. The narrowing reads the clone's HEAD, lists its refs and deletes every ref but the one
// HEAD names, so each of its refusals can name such a ref: a HEAD that names no branch
// (head-not-a-branch), a HEAD whose branch the clone no longer holds (head-missing), and what git
// wrote of a ref it could not list (listed) or lock (deleted). None of them holds a run of the
// token: the cache names no ref of its own, as its tar stream and its walk name no path, and the
// two git commands on the refs run under the fetch's filter (workspace.CacheGitHolding and
// CacheGitStdinHolding). Serve prints each refusal ("fetch failed; serving the clone as it was: ").
// The twin shows the clone's refs hold the token, and that the same deletion with nothing held
// prints what git wrote of the ref. Captures repocache.narrow-words/<case>.
func TestNarrowingTheCloneNamesNoRefOfIt(t *testing.T) {
	const branch = "echo-" + echoedProvisioningToken
	const ref = "refs/heads/" + branch
	// lock is the lock file a ref update takes, written here so git's own deletion of ref fails
	// naming it, as a fetch of this clone that is still running would make it fail.
	lock := func(t *testing.T, c *cache) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.dir, "refs", "heads", branch+".lock"), []byte("a lock of this ref\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deletion := "delete " + ref + "\n"

	t.Run("twin", func(t *testing.T) {
		c, run := narrowingCache(t, ref)
		listed, err := workspace.CacheGit(context.Background(), run, c.dir, "for-each-ref", "--format=%(refname)")
		if err != nil || !strings.Contains(listed.Stdout, echoedProvisioningToken) {
			t.Fatalf("the clone's refs are %q (%v), none holding the token, so the rows cannot see a leak", listed.Stdout, err)
		}
		lock(t, c)
		result, err := run.Run(context.Background(), workspace.Command{
			Argv: []string{"git", "--git-dir=" + c.dir, "update-ref", "--no-deref", "--stdin"}, Stdin: deletion, Dir: c.dir, Timeout: run.Timeout(),
		})
		if err != nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, echoedProvisioningToken) {
			t.Fatalf("the deletion git refused wrote %q (exit %d, %v), which holds no echo of the token, so the rows cannot see a leak", result.Stderr, result.ExitCode, err)
		}
	})

	t.Run("head-not-a-branch", func(t *testing.T) {
		c, _ := narrowingCache(t, ref)
		// A clone of an earlier release, whose HEAD the cache's fetch did not write: Fetch points
		// HEAD at refs/heads/<name> alone.
		if err := os.WriteFile(filepath.Join(c.dir, "HEAD"), []byte("ref: refs/tags/"+branch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		err := c.narrow(context.Background(), false, false)

		if err == nil {
			t.Fatal("the narrowing took a HEAD that names no branch")
		}
		oauth2calltest.RequireNoPiece(t, "repocache.narrow-words/head-not-a-branch", err.Error(), echoedProvisioningToken)
	})

	t.Run("head-missing", func(t *testing.T) {
		c, _ := narrowingCache(t, "")
		if err := os.WriteFile(filepath.Join(c.dir, "HEAD"), []byte("ref: "+ref+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		err := c.narrow(context.Background(), false, false)

		if !errors.Is(err, errCacheHeadMissing) {
			t.Fatalf("narrowing a clone whose HEAD names a ref it does not hold = %v, want the missing-head refusal", err)
		}
		oauth2calltest.RequireNoPiece(t, "repocache.narrow-words/head-missing", err.Error(), echoedProvisioningToken)
	})

	t.Run("listed", func(t *testing.T) {
		c, run := narrowingCache(t, ref)
		run.instead("for-each-ref", func(context.Context, workspace.Command) (workspace.Result, error) {
			return workspace.Result{ExitCode: 128, Stderr: "fatal: unable to read ref " + ref + "\n"}, nil
		})

		err := c.narrow(context.Background(), false, false)

		if err == nil || !strings.Contains(err.Error(), "list the cache's refs") {
			t.Fatalf("the narrowing whose ref listing failed = %v, want the listing's refusal", err)
		}
		oauth2calltest.RequireNoPiece(t, "repocache.narrow-words/listed", err.Error(), echoedProvisioningToken)
	})

	t.Run("deleted", func(t *testing.T) {
		c, _ := narrowingCache(t, ref)
		lock(t, c)

		err := c.narrow(context.Background(), false, false)

		if err == nil || !strings.Contains(err.Error(), "delete the cache's extra refs") {
			t.Fatalf("the narrowing whose ref deletion failed = %v, want the deletion's refusal", err)
		}
		oauth2calltest.RequireNoPiece(t, "repocache.narrow-words/deleted", err.Error(), echoedProvisioningToken)
	})
}

// narrowingCache is a cache that has filled and narrowed its clone of a remote whose default
// branch is main, with ref written into the clone afterwards, as a fetch of a repository whose
// HEAD moved leaves a ref the narrowing then removes; "" writes none. Its log is dropped: each row
// reads what the narrowing answers, which Serve prints.
func narrowingCache(t *testing.T, ref string) (*cache, *testRunner) {
	t.Helper()
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Log = func(string) {}
	c := cacheOn(t, opts)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("the refresh that fills the clone: %v", err)
	}
	if ref != "" {
		git(t, c.dir, "--git-dir="+c.dir, "update-ref", ref, "HEAD")
	}
	return c, run
}
