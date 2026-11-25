package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The tests in this file are more of the step's (jj_push_test.go): those of what git did in the
// colocated repository that no jj command has imported yet, a commit that git made or a branch
// that git moved (spec v35 R1, Verification c-git-made-commit). Where the push takes a snapshot of
// its own, the step compares git's HEAD and branches with @'s first parent and jj's @git
// bookmarks, with reads that import nothing, and where they differ takes the push's one snapshot
// first, so that its first read sees the commit git made and it sets that commit's author as for
// any other. A workspace with no git working tree compares nothing. The helpers here read the
// comparison's commands (checkCompared) and the step's snapshot ahead of its first read of what
// the push sends (checkSnapshotFirst), run a push of a commit git made (rolePushGitMade), make a
// commit with git alone (gitCommitWrite) and rebase the branch work with git alone
// (gitRebaseWork); jj_push_test.go uses checkCompared, rolePushGitMade and gitCommitWrite, and
// jj_push_snapshot_test.go checkCompared. As jj_push_test.go does, the file names nothing the
// step's change adds, so it compiles and runs at the base too.

// rolePushComparisonRead is one of the step's jj reads of its comparison of git's HEAD and
// branches with jj's: its name, whether it reads what the comparison compares, and whether a
// command the recording jj ran is that read, told from every other command of the step's by its
// words.
type rolePushComparisonRead struct {
	name     string
	compares bool
	is       func(c rolePushCall) bool
}

// rolePushComparisonReads are the comparison's jj reads: the read of the workspace's root (`jj
// workspace root`), which decides whether there is a git working tree to compare, and the reads
// of what it compares, @'s first parent and jj's @git bookmarks.
var rolePushComparisonReads = []rolePushComparisonRead{
	{name: "jj workspace root", is: func(c rolePushCall) bool { return c.pair() == [2]string{"workspace", "root"} }},
	{name: "jj log -r first_parent(@)", compares: true, is: func(c rolePushCall) bool { return c.pair()[0] == "log" && c.has("first_parent(@)") }},
	{name: "jj bookmark list --all-remotes", compares: true, is: func(c rolePushCall) bool {
		return c.pair() == [2]string{"bookmark", "list"} && c.has("--all-remotes")
	}},
}

// rolePushAheadOfFirstRead is whether c, a command the recording jj ran, may run ahead of the
// step's first read of what the push sends: one of rolePushComparisonReads, or a read of the
// configuration (`jj config`), which the pane's legion jj and the pod's wrapper make before the
// step.
func rolePushAheadOfFirstRead(c rolePushCall) bool {
	return c.pair()[0] == "config" || slices.ContainsFunc(rolePushComparisonReads, func(read rolePushComparisonRead) bool { return read.is(c) })
}

// checkCompared checks calls, every command the recording jj ran for one push, against compares,
// whether the step compares git's HEAD and branches with jj's before that push: where it does, it
// reads @'s first parent or jj's @git bookmarks, or both, and where it does not, as before a push
// with the role's own --ignore-working-copy or from a workspace whose root holds no git working
// tree, it reads neither. Each read of rolePushComparisonReads that the step makes runs with
// --ignore-working-copy. It holds the step to no number of reads and no order of them.
func (f *rolePushFixture) checkCompared(calls []rolePushCall, compares bool) {
	f.t.Helper()
	steps := calls[:f.oneRealPush(calls)]
	var compared [][]string
	for _, call := range steps {
		for _, read := range rolePushComparisonReads {
			if !read.is(call) {
				continue
			}
			if !call.has("--ignore-working-copy") {
				f.t.Errorf("the step ran %q, want `%s` with --ignore-working-copy", call.args, read.name)
			}
			if read.compares {
				compared = append(compared, call.args)
			}
		}
	}
	switch {
	case compares && len(compared) == 0:
		f.t.Errorf("the step ran %v before the push, want its comparison's read of @'s first parent or of jj's @git bookmarks", steps)
	case !compares && len(compared) > 0:
		f.t.Errorf("the step ran %q, want no read of what its comparison compares", compared)
	}
}

// checkSnapshotFirst checks calls, every command the recording jj ran for one push whose step
// finds that git moved HEAD or a branch, for the step's snapshot, its command before the real push
// with no --ignore-working-copy, ahead of its first read of what the push sends: only commands
// that may run ahead of that read (rolePushAheadOfFirstRead) run before the snapshot.
func (f *rolePushFixture) checkSnapshotFirst(calls []rolePushCall) {
	f.t.Helper()
	for i, call := range calls {
		if call.has("--ignore-working-copy") || call.realPush() {
			continue
		}
		for _, before := range calls[:i] {
			if !rolePushAheadOfFirstRead(before) {
				f.t.Errorf("the step ran %q before its snapshot %q, want only the reads of its comparison first", before.args, call.args)
			}
		}
		return
	}
}

// rolePushGitMade runs a push of a commit that the real git made in the colocated working copy and
// that no jj command has imported yet, under each form and in each unimported row's words
// (rolePushUnimportedRows), with a recording jj ahead of the real one. gitMakes makes the commit
// with git alone, after any jj command of its own, and answers its id, the commits the remote
// must hold below it, which the push sends too, the tip first, and whether jj's import of it
// makes its change divergent, as that of a git commit --amend of a commit jj made does: git keeps
// the change-id header of the commit it amends, and jj leaves that commit visible. The step's
// comparison (checkCompared) finds that git's HEAD is not on @'s first parent, so the step takes
// its one snapshot before its first read of what the push sends (checkSnapshotFirst), and that
// snapshot imports the commit: the push sends that commit with the implementer's author, as its
// trailer names, the step prints its one line of that commit's rewrite and the line of the
// argument's ids, as the argument named that commit by its id, and no line of a dry run it cannot
// read. @'s first parent is then the commit that line names, and `jj log` and `jj st` exit 0 with
// no word of a stale working copy.
func rolePushGitMade(t *testing.T, gitMakes func(f *rolePushFixture) (made string, below []rolePushCommit, diverges bool)) {
	impl := rolePushImplementer
	for _, row := range rolePushUnimportedRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				made, below, diverges := gitMakes(f)
				if change := f.changeOf(made); change != "" {
					t.Fatalf("jj reads the git commit %s as change %s before the push, want a commit no jj command imported", made, change)
				}
				args := make([]string, len(row.args))
				for i, arg := range row.args {
					args[i] = strings.ReplaceAll(arg, rolePushGitCommit, made)
				}
				divergent := f.divergence()
				code, _, stderr := f.push(impl, args...)
				change := f.changeOf(made)
				ref := row.ref
				if ref == "" {
					ref = "push-" + change[:min(12, len(change))]
				}
				f.check(rolePushWant{
					refs:      []rolePushRef{{ref: "refs/heads/" + ref, commits: append([]rolePushCommit{{change: change, author: impl}}, below...)}},
					rewrites:  []rolePushRewrite{{role: "implementer", author: impl, change: change, old: made}},
					ids:       []rolePushIDs{rolePushOneIDLine(made)},
					diverges:  diverges,
					snapshots: rolePushTakesOneSnapshot,
				}, args, code, stderr, divergent, 0)
				calls := f.calls()
				f.checkCompared(calls, true)
				f.checkSnapshotFirst(calls)
				for _, printed := range rolePushStepLines(stderr).rewrites {
					if printed.old != made {
						continue
					}
					if parent := f.read("first_parent(@)").commit; parent != printed.new {
						t.Errorf("@'s first parent is %s after the push, want %s, the commit the step's line names for %s, the commit git made", parent, printed.new, made)
					}
				}
				for _, command := range []string{"log", "st"} {
					if code, _, stderr := f.alone(impl, command); code != 0 || strings.Contains(stderr, "stale") {
						t.Errorf("jj %s exited %d after the push, want 0 with no word of a stale working copy; it printed:\n%s", command, code, stderr)
					}
				}
			})
		})
	}
}

// gitCommitWrite writes a file of its own into the working copy (write), and with git alone, as
// as, adds it and runs `git commit` with commit; it answers HEAD's id after that commit.
func (f *rolePushFixture) gitCommitWrite(as rolePushIdentity, commit ...string) string {
	f.write()
	run := f.runner(f.setupPath, as)
	run.Run(f.repo, f.git, "add", "change-"+strconv.Itoa(f.files)+".txt")
	run.Run(f.repo, slices.Concat([]string{f.git, "commit"}, commit)...)
	return strings.TrimSpace(run.Out(f.repo, f.git, "rev-parse", "HEAD"))
}

// gitRebaseWork makes, with jj, base, a commit of the implementer's on main, and c, an unpushed
// commit on main that the reviewer made with the implementer's trailer, and puts the bookmark
// work on c; then with git alone, as the implementer, it rebases work onto base (`git rebase`,
// which leaves git's HEAD on work). It answers base, c and made, the commit git's work names after
// that rebase, which is a new commit.
func (f *rolePushFixture) gitRebaseWork() (base, c rolePushChange, made string) {
	f.t.Helper()
	impl := rolePushImplementer
	base = f.commitOn(impl, impl, "main", "base\n")
	c = f.commitOn(rolePushReviewer, impl, "main", rolePushMessage("work", "implementer"))
	f.bookmark("work", c.commit)
	run := f.runner(f.setupPath, impl)
	run.Run(f.repo, f.git, "rebase", "--quiet", base.commit, "work")
	made = strings.TrimSpace(run.Out(f.repo, f.git, "rev-parse", "work"))
	if made == c.commit {
		f.t.Fatalf("git rebase left work on %s, want a new commit", made)
	}
	return base, c, made
}

// A git rebase of a branch under @, git alone, of an unpushed commit that the reviewer made with
// the implementer's trailer, onto a commit of the implementer's (gitRebaseWork), then a push that
// names the rebased commit by its id: the remote gets that commit with the implementer's author,
// and the commit below it as it is (rolePushGitMade).
func TestAPushAfterAGitRebaseOfAnotherRolesCommitSetsTheTrailersAuthor(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	rolePushGitMade(t, func(f *rolePushFixture) (string, []rolePushCommit, bool) {
		base, _, made := f.gitRebaseWork()
		return made, []rolePushCommit{{change: base.change, author: impl, commit: base.commit}}, false
	})
}

// A git commit --amend, on jj's detached HEAD, of an unpushed commit that the reviewer made with
// the implementer's trailer, with a file of its own, then a push that names the amended commit by
// its id: the remote gets that commit with the implementer's author (rolePushGitMade). The
// amended commit keeps the change id of the one jj made, which stays visible, so jj's import
// alone makes that change divergent.
func TestAPushAfterAGitCommitOnTheDetachedHeadSetsTheTrailersAuthor(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	rolePushGitMade(t, func(f *rolePushFixture) (string, []rolePushCommit, bool) {
		c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
		made := f.gitCommitWrite(impl, "--quiet", "--amend", "--no-edit")
		if made == c.commit {
			f.t.Fatalf("git commit --amend left HEAD on %s, want a new commit", made)
		}
		return made, nil, true
	})
}

// A git rebase of the branch work, git alone, of an unpushed commit that the reviewer made with
// the implementer's trailer, onto a commit of the implementer's (gitRebaseWork), then a git
// checkout of the commit it rebased, detached, where jj had left git's HEAD: git moved work and
// left HEAD on @'s first parent. The step's comparison of git's branches with jj's @git bookmarks
// finds the branch git moved (spec v35 R1), so the step takes its one snapshot before its first
// read of what the push sends (checkSnapshotFirst), which imports the rebased commit: `jj git push
// -b work` sends it with the implementer's author, and the commit below it as it is.
func TestAPushAfterAGitRebaseThatMovesOnlyABranchSetsTheTrailersAuthor(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, rolePushRecords)
		base, c, made := f.gitRebaseWork()
		run := f.runner(f.setupPath, impl)
		run.Run(f.repo, f.git, "checkout", "--quiet", "--detach", c.commit)
		if head, parent := strings.TrimSpace(run.Out(f.repo, f.git, "rev-parse", "HEAD")), f.read("first_parent(@)").commit; head != parent {
			t.Fatalf("git's HEAD is on %s, want it on @'s first parent %s, where jj left it", head, parent)
		}
		if change := f.changeOf(made); change != "" {
			t.Fatalf("jj reads the git commit %s as change %s before the push, want a commit no jj command imported", made, change)
		}
		args := []string{"git", "push", "-b", "work"}
		divergent := f.divergence()
		code, _, stderr := f.push(impl, args...)
		change := f.changeOf(made)
		f.check(rolePushWant{
			refs:      []rolePushRef{{ref: "refs/heads/work", commits: []rolePushCommit{{change: change, author: impl}, {change: base.change, author: impl, commit: base.commit}}}},
			rewrites:  []rolePushRewrite{{role: "implementer", author: impl, change: change, old: made}},
			snapshots: rolePushTakesOneSnapshot,
		}, args, code, stderr, divergent, 0)
		calls := f.calls()
		f.checkCompared(calls, true)
		f.checkSnapshotFirst(calls)
	})
}

// A push from a workspace with no git working tree, such as an issue workspace under jj 0.38,
// which colocates none (here `jj workspace add` with git.colocate = false, which makes none under
// jj 0.38, 0.45 and 0.46 alike), compares nothing: the step reads the workspace's root, finds no
// .git there, and reads nothing the comparison compares, of jj's or of git's; and the push goes on
// as before, the step's one snapshot taken where it has a commit to rewrite, and the remote gets
// the commit that the reviewer made with the implementer's trailer with the implementer's author.
// The same push from the colocated default workspace does compare: the step reads what the
// comparison compares, git's HEAD or branches among them. The git first on the push's PATH, which
// the step's git reads run, is the form's own, the pod's directory's (which the pod's jj requires
// first on PATH) or the pane's bin/git, and the test makes it a git that records its arguments
// (trailerLogging, read back by trailerCalls) and runs the real git, as the pod's does; no commit
// goes through it.
func TestAPushFromAWorkspaceWithNoGitWorkingTreeComparesNothing(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	for _, row := range []struct {
		name      string
		colocated bool
	}{
		{name: "a workspace with no git working tree"},
		{name: "the colocated default workspace", colocated: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				dir, workspace := f.repo, "default"
				if !row.colocated {
					dir, workspace = filepath.Join(f.root, "second"), "second"
					f.jj(rev, "--config", "git.colocate=false", "workspace", "add", "--name", workspace, "-r", "main", dir)
					if _, err := os.Lstat(filepath.Join(dir, ".git")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("jj workspace add left %s (%v), want a workspace with no git working tree", filepath.Join(dir, ".git"), err)
					}
				}
				f.jjIn(dir, rev, "new", "main")
				f.writeFile(filepath.Join(dir, "work.txt"), "work\n")
				f.jjIn(dir, impl, "commit", "-m", rolePushMessage("work", "implementer"))
				c := f.read(workspace + "@-")
				f.bookmark("legion/K-1", c.commit)
				log := filepath.Join(f.root, "git-calls")
				// The log starts empty, so that trailerCalls reads it where no git ran: as one run
				// with no argument, which is no git read.
				f.writeFile(log, "")
				writeTrailerScript(t, filepath.Join(f.formDir, "git"), trailerLogging(log)+"exec '"+f.git+"' \"$@\"\n")
				divergent := f.divergence()
				code, _, stderr := f.runIn(f.path, dir, impl, nil, rolePushBookmark...)
				want := rolePushSends("legion/K-1", c, "implementer", impl)
				want.snapshots = rolePushTakesOneSnapshot
				f.check(want, rolePushBookmark, code, stderr, divergent, 0)
				f.checkCompared(f.calls(), row.colocated)
				var gitReads [][]string
				for _, args := range trailerCalls(t, log) {
					if slices.Contains(args, "rev-parse") || slices.Contains(args, "for-each-ref") {
						gitReads = append(gitReads, args)
					}
				}
				switch {
				case !row.colocated && len(gitReads) != 0:
					t.Errorf("the push ran git %q, want no git read from a workspace with no git working tree", gitReads)
				case row.colocated && len(gitReads) == 0:
					t.Errorf("the push ran no git read, want git's HEAD or its branches read in the colocated workspace")
				}
			})
		})
	}
}
