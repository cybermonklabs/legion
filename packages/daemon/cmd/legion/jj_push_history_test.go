package main

import (
	"strconv"
	"strings"
	"testing"
)

// The tests in this file are more of the step's (jj_push_test.go): a push of a long history, with
// the helpers that make a history above the seed (fastImport, history, mutableHistory) and read a
// long one's push (rolePushCountRewrites). A long push runs through push, whose limit is what is
// left of the test binary's deadline (trailerRunLimit). The tests of jj_push_revision_test.go and
// jj_push_bound_test.go use those helpers too, and no other file does. As jj_push_test.go does,
// the file names nothing the step's change adds, so it compiles and runs at the base too.

// rolePushHistory is how many commits a long history has (rolePushFixture.history and
// mutableHistory): enough that the ids of its commits, joined by ` | `, are over
// rolePushRevisionBound bytes and under the 131,072 bytes of one argv word that Linux takes, and
// that `change_id(<change>)` of each, joined so, is over those 131,072 bytes.
const rolePushHistory = 3001

// fastImport makes n empty commits on ref above the seed, with git fast-import in the colocated
// repository, each authored by author (Legion test where it is the zero identity) with message,
// its first line followed by the commit's number, and answers the last.
func (f *rolePushFixture) fastImport(ref string, n int, author rolePushIdentity, message string) string {
	f.t.Helper()
	title, rest, _ := strings.Cut(message, "\n")
	var stream strings.Builder
	for i := 1; i <= n; i++ {
		text := title + " " + strconv.Itoa(i) + "\n" + rest
		stamp := strconv.Itoa(1_700_000_000+i) + " +0000\n"
		stream.WriteString("commit " + ref + "\nmark :" + strconv.Itoa(i) + "\n")
		if author != rolePushNobody {
			stream.WriteString("author " + author.name + " <" + author.email + "> " + stamp)
		}
		stream.WriteString("committer Legion test <legion-test@example.invalid> " + stamp)
		stream.WriteString("data " + strconv.Itoa(len(text)) + "\n" + text)
		if i == 1 {
			stream.WriteString("from " + f.seed + "\n")
		}
		stream.WriteString("\n")
	}
	if code, _, stderr := trailerRun(f.t, f.repo, f.env(f.setupPath, rolePushImplementer), stream.String(), f.git, "fast-import", "--quiet"); code != 0 {
		f.t.Fatalf("git fast-import exited %d:\n%s", code, stderr)
	}
	return strings.TrimSpace(f.runner(f.setupPath, rolePushImplementer).Out(f.repo, f.git, "rev-parse", ref))
}

// history puts n empty commits on the remote's main above the seed (fastImport): the real git
// pushes them, and jj imports them, so main and main@origin are on the last of them, which is then
// the seed that every ref the tests read is above.
func (f *rolePushFixture) history(n int) {
	f.t.Helper()
	last := f.fastImport("refs/heads/main", n, rolePushNobody, "history\n")
	f.runner(f.setupPath, rolePushImplementer).Run(f.repo, f.git, "push", "--quiet", "origin", "main")
	f.jj(rolePushImplementer, "git", "fetch")
	if main := f.read("main").commit; main != last {
		f.t.Fatalf("main is %s after the import of the history, want %s, its last commit", main, last)
	}
	f.seed = last
}

// mutableHistory puts legion/K-1 on the last of n commits above the seed that no remote holds,
// each of which the reviewer authored with the implementer's trailer (fastImport), which jj
// imports.
func (f *rolePushFixture) mutableHistory(n int) {
	f.t.Helper()
	last := f.fastImport("refs/heads/legion/K-1", n, rolePushReviewer, rolePushMessage("work", "implementer"))
	f.jj(rolePushImplementer, "git", "import")
	if tip := f.read("legion/K-1").commit; tip != last {
		f.t.Fatalf("legion/K-1 is %s after the import of the history, want %s, its last commit", tip, last)
	}
}

// checkHistory checks that origin's legion/K-1 holds the n commits of a long history above the
// seed (mutableHistory, rolePushHistory), and that its tip, every 500th commit below it and its
// first have author.
func (f *rolePushFixture) checkHistory(author rolePushIdentity, stderr string) {
	f.t.Helper()
	got, ok := f.remoteCommits(f.bare, "refs/heads/legion/K-1")
	if !ok || len(got) != rolePushHistory {
		f.t.Errorf("origin holds %d commits of legion/K-1 above the seed (%v), want %d; the push printed:\n%s", len(got), ok, rolePushHistory, stderr)
		return
	}
	for i, c := range got {
		if (i%500 == 0 || i == len(got)-1) && (c.name != author.name || c.email != author.email) {
			f.t.Errorf("origin's legion/K-1 commit %d, %s, has author %s <%s>, want %s <%s>", i, c.id, c.name, c.email, author.name, author.email)
		}
	}
}

// rolePushCountRewrites reads a push's standard error, stderr: how many of its lines are rewrites
// (rolePushRewriteLine), and every other line, printed, which check reads.
func rolePushCountRewrites(stderr string) (rewrites int, printed string) {
	var rest []string
	for line := range strings.SplitSeq(stderr, "\n") {
		if rolePushRewriteLine.MatchString(line) {
			rewrites++
		} else {
			rest = append(rest, line)
		}
	}
	return rewrites, strings.Join(rest, "\n")
}

// A push of a history that no remote holds and that is longer than one revset of the step's reads
// can name, more than 3,000 commits, each of which the reviewer authored with the implementer's
// trailer (rolePushFixture.mutableHistory): each read of the step that names those commits, or
// their changes, goes to as many reads as keep each revset under 100,000 bytes, so the step
// rewrites them all and the push exits 0. Origin's tip commit, every 500th commit below it and the
// first have the implementer's author, the step prints one rewrite for each commit, and no change
// is divergent. jj alone, on a twin history made the same way, sends them with the reviewer's
// author.
func TestAPushOfALongHistorySetsEveryAuthor(t *testing.T) {
	t.Parallel()
	// jj alone runs no form of Legion's, so one twin stands for both forms.
	twin := newRolePushFixture(t, rolePushPod, "")
	twin.mutableHistory(rolePushHistory)
	if code, _, stderr := twin.alone(rolePushImplementer, rolePushBookmark...); code != 0 {
		t.Fatalf("jj alone exited %d:\n%s", code, stderr)
	}
	twin.checkHistory(rolePushReviewer, "")
	rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, "")
		f.mutableHistory(rolePushHistory)
		divergent := f.divergence()
		code, _, stderr := f.push(rolePushImplementer, rolePushBookmark...)
		rewrites, printed := rolePushCountRewrites(stderr)
		f.check(rolePushWant{others: true}, rolePushBookmark, code, printed, divergent, 0)
		f.checkHistory(rolePushImplementer, printed)
		if rewrites != rolePushHistory {
			t.Errorf("the push printed %d rewrites, want %d; it printed beside them:\n%s", rewrites, rolePushHistory, printed)
		}
	})
}
