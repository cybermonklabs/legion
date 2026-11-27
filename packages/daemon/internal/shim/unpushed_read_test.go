package shim_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// The idle release takes no pod whose workspace read finds work that is not on the remote, so the
// read must count only that. The release before measured the workspace with the
// save's revset, which kept every visible head no remote bookmark reached; a clone that keeps
// unreachable commits (git.abandon-unreachable-commits = false) keeps every commit of a branch the
// remote deleted after its merge as such a head, and the quiet workspaces of issues whose roles
// push their work read tens of megabytes of "unpushed" work that way. Each subtest starts from that
// shape: another issue's branch the pod's clone fetched and the remote then deleted, and the
// remote's main moved past the issue's branch and fetched, both of them on the remote's side. The
// first holds that the shape alone reads as nothing unpushed; the other two add one commit, and one
// change in the working copy, that the remote does not hold, and hold that the read counts each.
// The last adds a side head that no working copy and no bookmark reaches, and holds that the read
// does not count it.
func TestTheUnpushedReadCountsOnlyWorkNotOnTheRemote(t *testing.T) {
	for _, tc := range []struct {
		name    string
		add     func(f *workFixture)
		commits int
		changed bool
	}{
		{name: "only_remote_work", add: func(*workFixture) {}},
		{name: "one_local_commit", commits: 1, add: func(f *workFixture) {
			f.write("local.txt")
			f.jj(f.ws, "commit", "-R", f.ws, "-m", "work the remote does not hold")
		}},
		// A change in the working copy makes the working-copy commit hold work, so the read lists
		// that commit and counts it, beside Changed (as TestWorkspaceWorkReadsTheIssuePodLayout's
		// fixture 4 reads it).
		{name: "one_working_copy_change", commits: 1, changed: true, add: func(f *workFixture) {
			f.write("change.txt")
		}},
		// The exception the spec's R5 names: a commit that no working copy and no local bookmark
		// reaches, a side head a role left with `jj new <base>` over its own change, is not
		// counted, though no remote branch or tag holds it. Counting every visible head off the
		// remote would count the deleted branches above, which is the cause the first subtest
		// holds out, so the read leaves such a head out and a release can take it.
		{name: "side_head", add: func(f *workFixture) {
			head := f.sideHead()
			f.requireVisibleHead(head, "the side head jj new left")
			if got := f.jjRead(head + " & ::(remote_bookmarks() | tags())"); got != "" {
				f.t.Fatalf("a remote branch or tag holds the side head %s: it must be work no remote holds", head)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkFixture(t)
			deleted := f.mergedBranchTheRemoteDeleted()
			f.mainMovedOnTheRemote()
			tc.add(f)

			got := f.read()
			if !got.OK {
				t.Fatalf("the shim refused the read: %s", got.Error)
			}
			if got.Count != tc.commits || got.Changed != tc.changed {
				t.Fatalf("the read found %d commit(s) and changed=%t (%v), want %d and %t: work on the remote, %s among it, is not unpushed",
					got.Count, got.Changed, got.Commits, tc.commits, tc.changed, deleted)
			}
			for _, commit := range got.Commits {
				if commit == deleted {
					t.Fatalf("the read counted %s, the head of a branch the remote deleted after its merge", deleted)
				}
			}
		})
	}
}

// mergedBranchTheRemoteDeleted is another issue's branch as the pod's clone meets it: a second
// clone pushes it with a file of its own, the pod's clone fetches it, and the remote deletes it,
// as GitHub deletes a pull request's branch after its merge. The pod's clone fetches again and
// keeps the branch's commit as a visible head with no bookmark, which is what the save's revset
// counted. It answers that commit.
func (f *workFixture) mergedBranchTheRemoteDeleted() string {
	f.t.Helper()
	other := filepath.Join(f.root, "other-issue")
	f.git(f.root, "clone", "--quiet", f.remote, other)
	f.git(other, "checkout", "--quiet", "-b", "legion/OTHER-1")
	f.writeIn(other, strings.Repeat("merged-", 4)+".txt")
	f.git(other, "add", "-A")
	f.git(other, "commit", "--quiet", "-m", "another issue's merged work")
	head := f.git(other, "rev-parse", "HEAD")
	f.git(other, "push", "--quiet", "origin", "legion/OTHER-1")
	f.jj(f.clone, "git", "fetch", "-R", f.clone)
	f.git(other, "push", "--quiet", "origin", "--delete", "legion/OTHER-1")
	f.jj(f.clone, "git", "fetch", "-R", f.clone)
	f.requireVisibleHead(head, "the head of the branch the remote deleted")
	return head
}

// mainMovedOnTheRemote moves the remote's main past the issue's branch and has the pod's clone
// fetch it: main's newer commits are on the remote, under main@origin, and are no work of the
// issue's.
func (f *workFixture) mainMovedOnTheRemote() {
	f.t.Helper()
	main := filepath.Join(f.root, "main-mover")
	f.git(f.root, "clone", "--quiet", f.remote, main)
	f.writeIn(main, "newer-on-main.txt")
	f.git(main, "add", "-A")
	f.git(main, "commit", "--quiet", "-m", "main's newer commit")
	f.git(main, "push", "--quiet", "origin", "main")
	f.jj(f.clone, "git", "fetch", "-R", f.clone)
}
