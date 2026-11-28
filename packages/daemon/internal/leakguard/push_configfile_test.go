package leakguard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// The guard reads the push's configuration as the push reads it, with no GIT_CONFIG, which only
// `git config` reads (Guard.pushConfiguration; R1 (e) of dispatch://INFRA-3524's spec), also for
// what bounds a push it guards (R10, heldRemotes). The clone's origin is the destination, which
// holds c1; a second remote's fetch refspec files that remote's refs under refs/remotes/origin/*,
// so origin/main names c2, a commit the destination does not hold that adds a line the rules
// refuse; and the branch work is c2 and one clean commit of its own. The push sets GIT_CONFIG to
// an empty file, under which `git config` lists no fetch refspec of the second remote, or the case
// would prove nothing. GIT_CONFIG keeps the push guarded and unread (knowsEveryInput), so the
// tracking refs bound it alone: read as the push reads the configuration, origin's refs are filed
// by another remote's fetch (filedByAnother) and bound nothing, c2 is read, and the push is
// refused; a guard that read the configuration under GIT_CONFIG would take origin/main as what
// the destination holds and pass the push of c2.
func TestAPushUnderGitConfigIsBoundByTheConfigurationThePushReads(t *testing.T) {
	root := t.TempDir()
	destination, other := filepath.Join(root, "destination.git"), filepath.Join(root, "other.git")
	seed, clone := filepath.Join(root, "seed"), filepath.Join(root, "clone")
	fixture := fixturegit.Isolated(t, t.TempDir())
	fixture.Run(root, fixturegit.InitBare(destination, "main")...)
	fixture.Run(root, fixturegit.InitBare(other, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(seed, "main")...)
	commitFiles(t, fixture, seed, map[string]string{"README.md": "one\n"}, "c1")
	fixture.Run(seed, "git", "push", "--quiet", destination, "main")
	commitFiles(t, fixture, seed, notesRefused(), "c2, which adds a line the rules refuse")
	fixture.Run(seed, "git", "push", "--quiet", other, "main")
	fixture.Run(root, "git", "clone", "--quiet", destination, clone)
	fixture.Run(clone, "git", "remote", "add", "other", other)
	fixture.Run(clone, "git", "config", "--replace-all", "remote.other.fetch", "+refs/heads/*:refs/remotes/origin/*")
	fixture.Run(clone, "git", "fetch", "--quiet", "other")
	fixture.Run(clone, "git", "checkout", "--quiet", "--no-track", "-b", "work", "refs/remotes/origin/main")
	work := commitFiles(t, fixture, clone, map[string]string{"work.md": "a line of the branch's own\n"}, "the branch's own commit")
	empty := filepath.Join(t.TempDir(), "empty.gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := fixture.With("GIT_CONFIG="+empty).Try(clone, "git", "config", "--get-all", "remote.other.fetch"); err == nil {
		t.Fatalf("under GIT_CONFIG=%s, git config listed remote.other.fetch %q: the case would prove nothing", empty, out)
	}
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, "GIT_CONFIG="+empty)
	err := guard.Push(bound(t), PushRequest{
		Dir:     clone,
		Remote:  "origin",
		URL:     destination,
		Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: work, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
	})
	wantRefused(t, err, notesReport)
}
