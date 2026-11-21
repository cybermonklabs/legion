package main

import (
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests in this file are more of the step's (jj_push_test.go): pushes of a revision argument
// that a rewrite of the step can make name other commits, of a long history, and of a word of ids
// or a command past the step's bound or limit, or under that limit only as the step counts it, with
// the helpers that only they use. As jj_push_test.go does, the file names nothing the step's change
// adds, so it compiles and runs at the base too.

// rolePushRevisionBound is the bound, in bytes, under which the step keeps the word of a revision
// argument that it gives the push as the ids of its commits.
const rolePushRevisionBound = 100_000

// rolePushHistory is how many commits a long history has (rolePushFixture.history and
// mutableHistory): enough that the ids of its commits, joined by ` | `, are over
// rolePushRevisionBound bytes and under the 131,072 bytes of one argv word that Linux takes, and
// that `change_id(<change>)` of each, joined so, is over those 131,072 bytes.
const rolePushHistory = 3001

// offsets makes a divergent change of two commits on main, each a describe of one commit that the
// reviewer made (diverge), with first's message and with second's, and answers the change's id and
// the two commits. jj counts a change's commits newest first, so offset 1 of the change is the
// first and offset 0 the second, which offsets checks.
func (f *rolePushFixture) offsets(first, second string) (string, rolePushChange, rolePushChange) {
	f.t.Helper()
	x := f.commitOn(rolePushReviewer, rolePushReviewer, "main", "x\n")
	f.jj(rolePushReviewer, "new", "main")
	d1, d2 := f.diverge(x, first, second)
	for offset, c := range []rolePushChange{d2, d1} {
		if got := f.read(x.change + "/" + strconv.Itoa(offset)); got.commit != c.commit {
			f.t.Fatalf("%s/%d is %s, want %s", x.change, offset, got.commit, c.commit)
		}
	}
	return x.change, d1, d2
}

// rolePushOffsetSends is what a push of the two commits of a divergent change must leave
// (rolePushFixture.offsets), sent being the bookmark on D1, offset 1 before the step, and held
// the one on D2, offset 0: origin holds sent on the rewritten D1, with the implementer's author,
// and held on D2 as it was; and alone, what jj alone leaves, D1 as it was too.
func rolePushOffsetSends(sent, held string, d1, d2 rolePushChange) (want, alone rolePushWant) {
	impl, rev := rolePushImplementer, rolePushReviewer
	heldD2 := rolePushRef{ref: "refs/heads/" + held, commits: []rolePushCommit{{change: d2.change, author: rev, commit: d2.commit}}}
	want = rolePushSends(sent, d1, "implementer", impl)
	want.refs = append(want.refs, heldD2)
	want.same = map[string]rolePushIdentity{d2.commit: rev}
	alone = rolePushWant{
		refs: []rolePushRef{heldD2, {ref: "refs/heads/" + sent, commits: []rolePushCommit{{change: d1.change, author: rev, commit: d1.commit}}}},
		same: map[string]rolePushIdentity{d1.commit: rev, d2.commit: rev},
	}
	return want, alone
}

// mine makes the state of the rows of an author revset that the step's rewrite makes name more
// commits: M on main, which the implementer made with its own trailer, under the bookmark other;
// P on main, which the reviewer made with the implementer's trailer, under mid; A on P, made so
// too, under top; each bookmark tracked, which origin holds at the seed; and @ the reviewer's, on
// main. The implementer's mine() then names M alone, which mine checks, and once the step has
// rewritten P and A, those too.
func (f *rolePushFixture) mine() (m, p, a rolePushChange) {
	f.t.Helper()
	impl, rev := rolePushImplementer, rolePushReviewer
	for _, name := range []string{"other", "mid", "top"} {
		f.track(name)
	}
	m = f.commitOn(impl, impl, "main", rolePushMessage("M", "implementer"))
	p = f.commitOn(rev, impl, "main", rolePushMessage("P", "implementer"))
	a = f.commitOn(rev, impl, p.commit, rolePushMessage("A", "implementer"))
	f.jj(rev, "new", "main")
	f.bookmark("other", m.commit)
	f.bookmark("mid", p.commit)
	f.bookmark("top", a.commit)
	if got := f.read("mine()"); got.commit != m.commit {
		f.t.Fatalf("the implementer's mine() names %s before the push, want M, %s, alone", got.commit, m.commit)
	}
	return m, p, a
}

// rolePushMineSends is what a push of -b top and of an argument of mine() must leave
// (rolePushFixture.mine), ref being the bookmark the argument sends M under: origin holds top on
// the rewritten A and P, each with the implementer's author, ref on M as it was, and mid and other
// where track put them, but where ref is other; and alone, what jj alone leaves, A and P as they
// were.
func rolePushMineSends(ref string, m, p, a rolePushChange) (want, alone rolePushWant) {
	impl, rev := rolePushImplementer, rolePushReviewer
	held := []rolePushRef{{ref: "refs/heads/" + ref, commits: []rolePushCommit{{change: m.change, author: impl, commit: m.commit}}}, {ref: "refs/heads/mid"}}
	if ref != "other" {
		held = append(held, rolePushRef{ref: "refs/heads/other"})
	}
	want = rolePushWant{
		refs: append([]rolePushRef{{ref: "refs/heads/top", commits: []rolePushCommit{{change: a.change, author: impl}, {change: p.change, author: impl}}}}, held...),
		rewrites: []rolePushRewrite{
			{role: "implementer", author: impl, change: p.change, old: p.commit},
			{role: "implementer", author: impl, change: a.change, old: a.commit},
		},
		same: map[string]rolePushIdentity{m.commit: impl},
	}
	alone = rolePushWant{
		refs: append([]rolePushRef{{ref: "refs/heads/top", commits: []rolePushCommit{{change: a.change, author: rev, commit: a.commit}, {change: p.change, author: rev, commit: p.commit}}}}, held...),
		same: map[string]rolePushIdentity{m.commit: impl, p.commit: rev, a.commit: rev},
	}
	return want, alone
}

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

// onlyRefs checks that origin holds no ref but main and those of want's refs on origin that it
// holds.
func (f *rolePushFixture) onlyRefs(want rolePushWant) {
	f.t.Helper()
	named := map[string]bool{"refs/heads/main": true}
	for _, ref := range want.refs {
		if ref.remote == "" && !ref.absent {
			named[ref.ref] = true
		}
	}
	for _, ref := range strings.Fields(f.runner(f.setupPath, rolePushImplementer).Out(f.root, f.git, "--git-dir", f.bare, "for-each-ref", "--format=%(refname)")) {
		if !named[ref] {
			f.t.Errorf("origin holds %s afterwards, a ref the push must not make", ref)
		}
	}
}

// rolePushNamedRow is a push of a revision argument that a test may check against jj alone
// (rolePushRunNamed): one that a rewrite of the step can make name other commits than it named
// before the step, or one whose command would pass the step's limit. setup makes the state it
// runs on, and answers the push's arguments, what the push through the form's jj must leave, and
// what jj alone must leave on a twin state made the same way, which the row checks where alone is
// true.
type rolePushNamedRow struct {
	name  string
	alone bool
	setup func(f *rolePushFixture) (args []string, want, alone rolePushWant)
}

// rolePushNamedRows are the revision arguments that name other commits after a rewrite of the
// step: a change offset of a divergent change, which the step's rewrite of one of its commits
// renumbers, whether that commit is the argument's own or another argument's, on a short history
// and on one whose commits' ids, joined, are over the bound of a revision the step gives as ids,
// and an author revset whose author the rewrite gives other commits too, whether of the argument
// or of the push's other commits, each pushed by the implementer.
func rolePushNamedRows() []rolePushNamedRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	return []rolePushNamedRow{
		{name: "--named x=<change>/1 of a divergent change", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			change, d1, d2 := f.offsets(rolePushMessage("D1", "implementer"), rolePushMessage("D2", "reviewer"))
			want := rolePushSends("x", d1, "implementer", impl)
			want.same = map[string]rolePushIdentity{d2.commit: rev}
			alone := rolePushWant{
				refs: []rolePushRef{{ref: "refs/heads/x", commits: []rolePushCommit{{change: change, author: rev, commit: d1.commit}}}},
				same: map[string]rolePushIdentity{d1.commit: rev, d2.commit: rev},
			}
			return []string{"git", "push", "--named", "x=" + change + "/1"}, want, alone
		}},
		// -r sends each bookmark that points at a commit it names, and jj refuses to make a remote
		// bookmark for one that tracks nothing: b stays on origin where track put it, the seed.
		{name: "-r <change>/1 of a divergent change, a tracked bookmark on each commit", setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			f.track("a")
			f.track("b")
			change, d1, d2 := f.offsets(rolePushMessage("D1", "implementer"), rolePushMessage("D2", "reviewer"))
			f.bookmark("a", d1.commit)
			f.bookmark("b", d2.commit)
			want := rolePushSends("a", d1, "implementer", impl)
			want.refs = append(want.refs, rolePushRef{ref: "refs/heads/b"})
			want.same = map[string]rolePushIdentity{d2.commit: rev}
			return []string{"git", "push", "-r", change + "/1"}, want, rolePushWant{}
		}},
		// The step's rewrite of C gives it the implementer's author, which the argument names: C
		// gets no bookmark of its own, as with jj alone.
		{name: "-c author(<the implementer's email>) & mutable(), C the other role's under D", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			c := f.commitOn(rev, impl, "main", rolePushMessage("C", "implementer"))
			d := f.commitOn(impl, impl, c.commit, rolePushMessage("D", "implementer"))
			f.jj(rev, "new", "main")
			noC := rolePushRef{ref: "refs/heads/push-" + c.change[:12], absent: true}
			want := rolePushWant{
				refs: []rolePushRef{{ref: "refs/heads/push-" + d.change[:12], commits: []rolePushCommit{
					{change: d.change, author: impl}, {change: c.change, author: impl},
				}}, noC},
				rewrites: []rolePushRewrite{{role: "implementer", author: impl, change: c.change, old: c.commit}},
			}
			alone := rolePushWant{
				refs: []rolePushRef{{ref: "refs/heads/push-" + d.change[:12], commits: []rolePushCommit{
					{change: d.change, author: impl, commit: d.commit}, {change: c.change, author: rev, commit: c.commit},
				}}, noC},
			}
			return []string{"git", "push", "-c", `author(substring:"` + impl.email + `") & mutable()`}, want, alone
		}},
		// The step's rewrite of B and C takes the other role's author from both, which the
		// argument names: B still has "base" in its description, and C nothing the argument names.
		{name: `-c description(substring:"base") | (author(<the other role's email>) & mutable()), C on B`, alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			b := f.commitOn(rev, impl, "main", rolePushMessage("base", "implementer"))
			c := f.commitOn(rev, impl, b.commit, rolePushMessage("work", "implementer"))
			want := rolePushWant{
				refs: []rolePushRef{
					{ref: "refs/heads/push-" + b.change[:12], commits: []rolePushCommit{{change: b.change, author: impl}}},
					{ref: "refs/heads/push-" + c.change[:12], commits: []rolePushCommit{{change: c.change, author: impl}, {change: b.change, author: impl}}},
				},
				rewrites: []rolePushRewrite{
					{role: "implementer", author: impl, change: b.change, old: b.commit},
					{role: "implementer", author: impl, change: c.change, old: c.commit},
				},
			}
			heldB := rolePushCommit{change: b.change, author: rev, commit: b.commit}
			alone := rolePushWant{refs: []rolePushRef{
				{ref: "refs/heads/push-" + b.change[:12], commits: []rolePushCommit{heldB}},
				{ref: "refs/heads/push-" + c.change[:12], commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}, heldB}},
			}}
			return []string{"git", "push", "-c", `description(substring:"base") | (author(substring:"` + rev.email + `") & mutable())`}, want, alone
		}},
		// The row of -r <change>/1 on a long history: the ids of the commits ::<change>/1 names
		// before the step are over the bound, so the real push's revision holds another form of
		// them, and b stays where track put it, the history's last commit.
		{name: "-r ::<change>/1 of a divergent change on a history of more than 3,000 commits, a tracked bookmark on each commit", setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			f.history(rolePushHistory)
			f.track("a")
			f.track("b")
			change, d1, d2 := f.offsets(rolePushMessage("D1", "implementer"), rolePushMessage("D2", "reviewer"))
			f.bookmark("a", d1.commit)
			f.bookmark("b", d2.commit)
			ids := strings.Fields(f.jj(impl, "--ignore-working-copy", "log", "--no-graph", "-r", "::"+change+"/1", "-T", `commit_id ++ "\n"`))
			if joined := len(strings.Join(ids, " | ")); joined < rolePushRevisionBound {
				f.t.Fatalf("the %d ids of ::%s/1 joined are %d bytes, want %d or more", len(ids), change, joined, rolePushRevisionBound)
			}
			want := rolePushSends("a", d1, "implementer", impl)
			want.refs = append(want.refs, rolePushRef{ref: "refs/heads/b"})
			want.same = map[string]rolePushIdentity{d2.commit: rev}
			want.pushed = func(words []string) {
				if revision := words[len(words)-1]; len(revision) >= rolePushRevisionBound {
					f.t.Errorf("the real push's revision is %d bytes, want under %d", len(revision), rolePushRevisionBound)
				}
			}
			return []string{"git", "push", "-r", "::" + change + "/1"}, want, rolePushWant{}
		}},
		// The step's rewrite of D1 makes it the change's newest commit, offset 0, and D2 offset 1:
		// f and g, whose own commit the step rewrites in g's case alone, each go to the push as the
		// id of the commit they named before the step, as the step left it.
		{name: "--named f and g of offsets 0 and 1 of a divergent change, the step rewriting offset 1's commit", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			change, d1, d2 := f.offsets(rolePushMessage("D1", "implementer"), rolePushMessage("D2", "reviewer"))
			want, alone := rolePushOffsetSends("g", "f", d1, d2)
			return []string{"git", "push", "--named", "f=" + change + "/0", "--named", "g=" + change + "/1"}, want, alone
		}},
		// -r sends each bookmark that points at a commit it names: b, on D2, offset 0 before the
		// step, whose rewrite of D1, which -b a sends, makes D1 offset 0.
		{name: "-b a -r <offset 0 of a divergent change>, a on offset 1, b on offset 0, each tracked", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			f.track("a")
			f.track("b")
			change, d1, d2 := f.offsets(rolePushMessage("D1", "implementer"), rolePushMessage("D2", "reviewer"))
			f.bookmark("a", d1.commit)
			f.bookmark("b", d2.commit)
			want, alone := rolePushOffsetSends("a", "b", d1, d2)
			return []string{"git", "push", "-b", "a", "-r", change + "/0"}, want, alone
		}},
		// -r sends each bookmark that points at a commit it names: other, on M, and not mid, on P,
		// which mine() names only once the step has rewritten it.
		{name: "-b top -r mine(), mine() naming M on other alone, the step rewriting P on mid and A on top", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			m, p, a := f.mine()
			want, alone := rolePushMineSends("other", m, p, a)
			return []string{"git", "push", "-b", "top", "-r", "mine()"}, want, alone
		}},
		// --named names one commit: jj would refuse mine() once the step has rewritten P and A.
		{name: "-b top --named nm=mine(), mine() naming M alone, the step rewriting P on mid and A on top", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			m, p, a := f.mine()
			want, alone := rolePushMineSends("nm", m, p, a)
			return []string{"git", "push", "-b", "top", "--named", "nm=mine()"}, want, alone
		}},
	}
}

// rolePushRunNamed runs each row under each form: where alone is true, jj alone on a twin state
// first, which must leave what the row's alone says and no other ref beside main on origin; then
// the push through the form's jj, with a recording jj ahead of the real one, which must leave what
// the row's want says and no other ref beside main on origin.
func rolePushRunNamed(t *testing.T, rows []rolePushNamedRow) {
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				if row.alone {
					twin := newRolePushFixture(t, form, "")
					args, _, alone := row.setup(twin)
					divergent := twin.divergence()
					code, _, stderr := twin.alone(rolePushImplementer, args...)
					twin.check(alone, code, stderr, divergent, 0)
					twin.onlyRefs(alone)
				}
				f := newRolePushFixture(t, form, rolePushRecords)
				args, want, _ := row.setup(f)
				f.pushAndCheck(args, want)
				f.onlyRefs(want)
			})
		})
	}
}

// A revision argument that a rewrite of the step can make name other commits than it named before
// the step reaches the real push naming exactly the commits it named before, each one the step
// rewrote as rewritten (rolePushNamedRows), whether the rewrite is of the argument's own commits or
// of another argument's: a change offset `<change>/<n>`, which counts a change's commits newest
// first, the step's own among them, and an author revset whose author the step's rewrite gives
// another commit. The push exits 0, origin holds the refs the row names and no other beside main,
// each commit with its trailer's author, the step prints each rewrite, and no change is divergent;
// jj alone, on a twin state made the same way, sends what the row says it does. So `--named f=<X>/0
// --named g=<X>/1` puts f on the commit offset 0 named before the step, `-b a -r <X>/0` still sends
// b, on that commit, `-b top -r mine()` sends no bookmark of a commit that mine() names only after
// the step, and `-b top --named nm=mine()` exits 0. `-r ::<change>/1` on a history of more than
// 3,000 commits sends the rewritten commit alone, and the real push's revision, which a recording
// jj ahead of the real one sees, is under the bound of 100,000 bytes, which the ids of the commits
// it names are over. A -c of a change offset run twice has a test of its own,
// TestAChangeOffsetPushRunAgainLeavesOriginAsItWas.
func TestARevisionArgumentSendsTheCommitsItNamedBeforeTheStep(t *testing.T) {
	t.Parallel()
	rolePushRunNamed(t, rolePushNamedRows())
}

// `jj git push -c <change>/0` of a divergent change, two describes of one commit that the reviewer
// made (rolePushFixture.offsets), D1 with the reviewer's trailer and D2, offset 0, with the
// implementer's, run twice through the form's jj, with a recording jj ahead of the real one. The
// first push exits 0, origin holds push-<change> with D2 rewritten, with the implementer's author,
// and no other ref beside main, D1 keeps the reviewer's author, the step prints the rewrite, and
// no change is more divergent. The same push run again exits 0, without `Bookmark already exists`,
// and leaves origin as the first push left it.
func TestAChangeOffsetPushRunAgainLeavesOriginAsItWas(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, rolePushRecords)
		change, d1, d2 := f.offsets(rolePushMessage("D1", "reviewer"), rolePushMessage("D2", "implementer"))
		args := []string{"git", "push", "-c", change + "/0"}
		want := rolePushSends("push-"+change[:12], d2, "implementer", impl)
		want.same = map[string]rolePushIdentity{d1.commit: rev}
		f.pushAndCheck(args, want)
		f.onlyRefs(want)
		if t.Failed() {
			return
		}
		again := rolePushWant{refs: slices.Clone(want.refs)}
		for i, ref := range again.refs {
			sent, _ := f.remoteCommits(f.bare, ref.ref)
			again.refs[i].commits = slices.Clone(ref.commits)
			for j := range again.refs[i].commits {
				again.refs[i].commits[j].commit = sent[j].id
			}
		}
		divergent := f.divergence()
		code, _, stderr := f.push(impl, args...)
		if strings.Contains(stderr, "Bookmark already exists") {
			t.Errorf("the push run again printed `Bookmark already exists`:\n%s", stderr)
		}
		f.check(again, code, stderr, divergent, 0)
		f.onlyRefs(again)
	})
}

// rolePushLowBound is the step's bound of a word of ids, in bytes, that two rows of
// TestARevisionPastALoweredBoundGetsItsIDsInWordsOfItsOption set (rolePushBoundVariable): over the
// 92 bytes of two commit ids joined by ` | ` behind `--change=`, and over each revset of the step's
// reads of one commit, the longest of which, `(mutable() | (::(<C>) & mutable())) ~
// ::remote_bookmarks()`, is 95 bytes; and under the 126 bytes of three commit ids joined so, and so
// under the shortest primary form of an argument that names three commits the step rewrites.
const rolePushLowBound = 100

// rolePushOneWordBound is the step's bound of a word of ids, in bytes, that a row of
// TestARevisionPastALoweredBoundGetsItsIDsInWordsOfItsOption and
// TestTheRealPushsArgumentCarriesNoIDOfACommitTheStepLeftAlone set (rolePushBoundVariable): over
// the 126 bytes of three commit ids joined by ` | `, and over the 132 bytes of the primary form of
// `::<C>` of one rewritten commit C, `(::<C>) ~ (<C>) | <C'>`; and under the 341 bytes of the ids
// of eight commits joined so.
const rolePushOneWordBound = 200

// rolePushLowArgMax is the step's limit of a command, in bytes, that the rows of
// TestAPushPastTheLimitOfACommandSetsNoAuthor set (rolePushArgMaxVariable): under the environment
// alone of any push of these tests, which holds the table, a PATH of temporary directories and the
// role's identity.
const rolePushLowArgMax = 1000

// rolePushGapBound is the step's bound of a word of ids, in bytes, that a row of
// TestAPushUnderTheLimitWithEachArgumentAtItsIDsSetsEveryAuthor sets (rolePushBoundVariable), and
// how many bytes over the push's environment its limit of a command is (rolePushArgMaxVariable).
// That is over what the push's command takes beside that environment with the argument as the
// role wrote it or as the ids of its three commits, each some 130 bytes: with the push's other
// words, the dry run's flags, the jj it runs and the variables that the form's jj and legion jj
// add, some 800 bytes. A word of the bound in place of the argument takes more than the bound
// alone, so the command with it would be over the limit.
const rolePushGapBound = 4096

// rolePushIDsHistory is how many commits the history of
// TestARevisionPastTheBoundGetsItsIDsInWordsOfItsOption has: enough that their ids, joined by
// ` | `, are over rolePushRevisionBound bytes, 103,197 of them, which two words keep under it, and
// so that the shortest primary form of an argument naming them all, which holds each id twice, is
// over the bound too.
const rolePushIDsHistory = 2400

// checkOptionWords checks that words, the arguments of a command the recording jj ran (what), give
// the revision argument of option in count words, each under bound bytes with option where option
// is attached to its value (it ends with `=`), and answers the ids those words hold, which must be
// n full commit ids, each once.
func (f *rolePushFixture) checkOptionWords(what string, words []string, option string, count, n, bound int) []string {
	f.t.Helper()
	attached := strings.HasSuffix(option, "=")
	var values []string
	for k, word := range words {
		switch value, ok := strings.CutPrefix(word, option); {
		case attached && ok:
			values = append(values, value)
		case !attached && word == option && k+1 < len(words):
			values = append(values, words[k+1])
		}
	}
	if len(values) != count {
		f.t.Errorf("%s gives %d words of %s, want %d: %.300q", what, len(values), option, count, words)
	}
	var ids, bad []string
	seen := map[string]bool{}
	for _, value := range values {
		word := value
		if attached {
			word = option + value
		}
		if len(word) >= bound {
			f.t.Errorf("%s gives a word of %s of %d bytes, want under %d", what, option, len(word), bound)
		}
		for _, id := range strings.Split(value, " | ") {
			if len(id) != 40 || !rolePushFullID.MatchString(id) || seen[id] {
				bad = append(bad, id)
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(bad) > 0 {
		f.t.Errorf("%s gives %d terms in its words of %s that are no full commit id or one given before, the first %.100q", what, len(bad), option, bad[0])
	}
	if len(ids) != n {
		f.t.Errorf("%s gives %d ids in its words of %s, want %d", what, len(ids), option, n)
	}
	return ids
}

// checkIDsHistory checks what a push of option, -r or -c, leaves of the history of
// TestARevisionPastTheBoundGetsItsIDsInWordsOfItsOption: origin holds each of its commits with
// author, under legion/K-1 for -r and under a bookmark push-<change> of each for -c, and no other
// ref beside main. It names the first commit with another author and how many have one.
func (f *rolePushFixture) checkIDsHistory(option string, author rolePushIdentity) {
	f.t.Helper()
	run := f.runner(f.setupPath, rolePushImplementer)
	var others []string
	refs := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(run.Out(f.root, f.git, "--git-dir", f.bare, "for-each-ref", "--format=%(refname)%1f%(authorname)%1f%(authoremail)")), "\n") {
		fields := strings.Split(line, "\x1f")
		if len(fields) != 3 || fields[0] == "refs/heads/main" {
			continue
		}
		refs[fields[0]] = true
		if email := strings.TrimSuffix(strings.TrimPrefix(fields[2], "<"), ">"); fields[1] != author.name || email != author.email {
			others = append(others, fields[0]+" by "+fields[1]+" "+fields[2])
		}
	}
	want := map[string]bool{"refs/heads/legion/K-1": true}
	if option == "-c" {
		want = map[string]bool{}
		for _, change := range strings.Fields(f.jj(rolePushImplementer, "--ignore-working-copy", "log", "--no-graph", "-r", f.seed+"..legion/K-1", "-T", `change_id ++ "\n"`)) {
			want["refs/heads/push-"+change[:12]] = true
		}
	}
	if !maps.Equal(refs, want) {
		f.t.Errorf("origin holds %d refs beside main, want %d, those of the push of %s", len(refs), len(want), option)
	}
	if option == "-r" {
		got, _ := f.remoteCommits(f.bare, "refs/heads/legion/K-1")
		if len(got) != rolePushIDsHistory {
			f.t.Errorf("origin holds %d commits of legion/K-1 above the seed, want %d", len(got), rolePushIDsHistory)
		}
		for _, c := range got {
			if c.name != author.name || c.email != author.email {
				others = append(others, c.id+" by "+c.name+" <"+c.email+">")
			}
		}
	}
	if len(others) > 0 {
		f.t.Errorf("origin holds %d commits with another author than %s <%s>, the first %s", len(others), author.name, author.email, others[0])
	}
}

// `jj git push -r <S>..<C>` and `-c <S>..<C>`, S the seed and C the tip of a history of 2,400
// commits that no remote holds, each of which the reviewer authored with the implementer's trailer
// (rolePushFixture.mutableHistory), legion/K-1 on C and, for -r, tracked. The step rewrites every
// commit, and the argument then names the old, hidden commits; neither its primary form nor the
// ids of its 2,400 commits in one word is under the bound of 100,000 bytes. The real push gets
// those ids in two words of the argument's option, `-r <ids> -r <ids>` (a recording jj ahead of
// the real one sees them), and no command the step runs has a word of 100,000 bytes or more. The
// push exits 0, origin holds each commit with the implementer's author, under legion/K-1 for -r
// and under a bookmark of -c's own for each for -c, and no other ref beside main, the step prints
// one rewrite for each commit and one line of the ids in two words, and no change is divergent.
// jj alone, on a twin history made the same way, sends the commits with the reviewer's author.
func TestARevisionPastTheBoundGetsItsIDsInWordsOfItsOption(t *testing.T) {
	t.Parallel()
	for _, option := range []string{"-r", "-c"} {
		t.Run(option, func(t *testing.T) {
			t.Parallel()
			setup := func(f *rolePushFixture) []string {
				if option == "-r" {
					f.track("legion/K-1")
				}
				f.mutableHistory(rolePushIDsHistory)
				return []string{"git", "push", option, f.seed + ".." + f.read("legion/K-1").commit}
			}
			// jj alone runs no form of Legion's, so one twin stands for both forms.
			twin := newRolePushFixture(t, rolePushPod, "")
			args := setup(twin)
			if code, _, stderr := trailerRunWithin(t, rolePushLongLimit, twin.repo, twin.env(twin.setupPath, rolePushImplementer), strings.NewReader(""), twin.real, args...); code != 0 {
				t.Fatalf("jj alone exited %d:\n%s", code, stderr)
			}
			twin.checkIDsHistory(option, rolePushReviewer)
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				args := setup(f)
				divergent := f.divergence()
				code, stderr := f.pushLong(rolePushImplementer, args...)
				rewrites, printed := rolePushCountRewrites(stderr)
				f.check(rolePushWant{others: true, ids: []rolePushIDs{{argument: args[3], commits: rolePushIDsHistory, words: 2, bound: rolePushRevisionBound}}}, code, printed, divergent, 0)
				f.checkIDsHistory(option, rolePushImplementer)
				if rewrites != rolePushIDsHistory {
					t.Errorf("the push printed %d rewrites, want %d; it printed beside them:\n%s", rewrites, rolePushIDsHistory, printed)
				}
				calls, push := f.oneRealPush()
				for _, call := range calls {
					for _, word := range call.args {
						if len(word) >= rolePushRevisionBound {
							t.Errorf("the recording jj ran %.120q… with a word of %d bytes, want each under %d", strings.Join(call.args, " "), len(word), rolePushRevisionBound)
						}
					}
				}
				f.checkOptionWords("the real push", calls[push].args, option, 2, rolePushIDsHistory, rolePushRevisionBound)
			})
		})
	}
}

// A revision argument of three commits on main that the reviewer made with the implementer's
// trailer, with the step's bound of a word of ids lowered: `-c <C1> | <C2> | <C3>`, and
// `--change=<H1> | <H2> | <H3>`, each H the commit a describe of C hid, which stands for C, with the
// bound at rolePushLowBound bytes, under the ids of the three joined; and `-c <C1> | <C2> | <C3>`
// with the bound at rolePushOneWordBound bytes, over them. The step rewrites the three, and the
// real push gets their ids in words of the argument's option as the role spelled it, each under
// the bound (a recording jj ahead of the real one sees them): in two words under the lower bound,
// `-c <ids> -c <id>` and `--change=<ids> --change=<id>`, and in one word, `-c <ids>`, under the
// higher; each dry run of the --change= row gets the ids of the commits that stand for the three
// then in two such words too. The push exits 0, origin holds each commit under a bookmark of -c's
// own with the implementer's author, the step prints each rewrite and one line that counts the
// three commits and the words of their ids, and no change is divergent.
func TestARevisionPastALoweredBoundGetsItsIDsInWordsOfItsOption(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	work := func(f *rolePushFixture, bound int) []rolePushChange {
		f.bound = bound
		cs, _, _ := f.reviewed(3)
		return cs
	}
	// split is the push of argument by option, which sends cs in count words under f's bound, and
	// what it leaves; dry is whether each dry run gives the argument in words of option too.
	split := func(f *rolePushFixture, option, argument string, cs []rolePushChange, count int, dry bool) ([]string, rolePushWant) {
		want := rolePushWant{ids: []rolePushIDs{{argument: argument, commits: len(cs), words: count, bound: f.bound}}, oneWord: true}
		changes := map[string]bool{}
		for _, c := range cs {
			sent := rolePushSends("push-"+c.change[:12], c, "implementer", impl)
			want.refs = append(want.refs, sent.refs...)
			want.rewrites = append(want.rewrites, sent.rewrites...)
			changes[c.change] = true
		}
		check := func(what string, words []string) {
			got := map[string]bool{}
			for _, id := range f.checkOptionWords(what, words, option, count, len(cs), f.bound) {
				got[f.changeOf(id)] = true
			}
			if !maps.Equal(got, changes) {
				f.t.Errorf("%s gives the argument commits of changes %v, want one of each of %v", what, got, changes)
			}
		}
		want.pushed = func(words []string) {
			check("the real push", words)
			for _, call := range f.calls() {
				if dry && call.has("push") && call.has("--dry-run") {
					check("a dry run", call.args)
				}
			}
		}
		if strings.HasSuffix(option, "=") {
			return []string{"git", "push", option + argument}, want
		}
		return []string{"git", "push", option, argument}, want
	}
	ids := func(cs []rolePushChange) string { return strings.Join(rolePushCommitIDs(cs), " | ") }
	rolePushRunArgs(t, rolePushRecords, []rolePushArgsRow{
		{name: "-c <C1> | <C2> | <C3>", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			cs := work(f, rolePushLowBound)
			return split(f, "-c", ids(cs), cs, 2, false)
		}},
		{name: "--change=<H1> | <H2> | <H3>, each H the commit a describe of C hid", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			cs := work(f, rolePushLowBound)
			hidden := ids(cs)
			for i, c := range cs {
				f.jj(impl, "describe", c.commit, "-m", rolePushMessage("work "+strconv.Itoa(i+1)+", described again", "implementer"))
				cs[i] = f.read(c.change)
			}
			return split(f, "--change=", hidden, cs, 2, true)
		}},
		{name: "-c <C1> | <C2> | <C3>, the bound over their ids", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			cs := work(f, rolePushOneWordBound)
			return split(f, "-c", ids(cs), cs, 1, false)
		}},
	})
}

// rolePushPastTheLimit is what the line `legion jj: could not set the author: …; the push runs as
// written` says of a push whose command, with revision argument as the ids of its n commits, would
// not be under limit bytes.
func rolePushPastTheLimit(argument string, n, limit int) string {
	return "revision `" + argument + "` would go to the push as the ids of its " + strconv.Itoa(n) + " commits, and the push's arguments and environment would not be under " + strconv.Itoa(limit) + " bytes"
}

// reviewed makes n commits on main, each of which the reviewer made with the implementer's
// trailer, and answers them and what a push of each under a bookmark of -c's own must leave:
// want, where the step rewrites each, with the implementer's author (rolePushSends), and alone,
// where nothing rewrites them, as jj alone leaves them, each as it was, with the reviewer's
// author.
func (f *rolePushFixture) reviewed(n int) (cs []rolePushChange, want, alone rolePushWant) {
	f.t.Helper()
	impl, rev := rolePushImplementer, rolePushReviewer
	cs = make([]rolePushChange, n)
	alone.same = map[string]rolePushIdentity{}
	for i := range cs {
		c := f.commitOn(rev, impl, "main", rolePushMessage("work "+strconv.Itoa(i+1), "implementer"))
		cs[i] = c
		sent := rolePushSends("push-"+c.change[:12], c, "implementer", impl)
		want.refs = append(want.refs, sent.refs...)
		want.rewrites = append(want.rewrites, sent.rewrites...)
		alone.refs = append(alone.refs, rolePushRef{ref: "refs/heads/push-" + c.change[:12], commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}}})
		alone.same[c.commit] = rev
	}
	return cs, want, alone
}

// rolePushCommitIDs is the commit id of each of cs, in order.
func rolePushCommitIDs(cs []rolePushChange) []string {
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.commit
	}
	return ids
}

// The pushes of a revision argument of commits on main that the step would rewrite, with the
// step's limit of a command lowered to rolePushLowArgMax bytes, under the push's command with the
// argument as the role wrote it and as the ids of its commits, the larger of which the step counts
// for each argument. The step decides before it rewrites anything but the fill of a commit with no
// author that the push's command would then pass the limit. It sets no other author, prints one
// line `legion jj: could not set the author: revision `<argument>` would go to the push as the ids
// of its <n> commits, …` that names the limit, and the push runs as written but where the fill
// ran, exit 0, with no change divergent and origin holding no ref beside main but those of -c:
//
//   - `-c "<C1> | <C2>"`, C1 and C2 two commits that the reviewer made with the implementer's
//     trailer: the step prints no rewrite and runs no metaedit (a recording jj ahead of the real
//     one sees none), the real push gets the role's own words, every byte, and origin holds C1
//     and C2 as they were, with the reviewer's author, as jj alone leaves them on a twin state
//     made the same way;
//   - `-c "<E> | <C>"`, E a commit with an empty author and the implementer's trailer, which the
//     step fills before it decides, and C one that the reviewer made with the implementer's
//     trailer: the step prints the rewrite of E alone and runs one metaedit, that fill, and the
//     real push sends E as the fill left it, with the implementer's author, in place of the hidden
//     E, and C as it was, with the reviewer's author;
//   - `-c <D> -c mine()`, D a commit that the reviewer made with the implementer's trailer on E,
//     one with an empty author, which the step fills before it decides, and mine() naming M alone,
//     a commit the implementer made with its own trailer: the step prints the rewrite of E alone
//     and runs one metaedit, that fill, which rebases D, and gives each argument as the id of the
//     commit it named before the step, as the fill left it (one line each). The real push sends
//     the rebased D, with the reviewer's author, on E as the fill left it, with the
//     implementer's, and M as it was, and makes no bookmark for E, which mine() names after the
//     fill.
func TestAPushPastTheLimitOfACommandSetsNoAuthor(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	// asWritten makes n commits on main that the reviewer made with the implementer's trailer
	// (rolePushFixture.reviewed), under the limit f sets, and answers the push `-c "<C1> | … |
	// <Cn>"`, what that push must leave, and what jj alone leaves.
	asWritten := func(f *rolePushFixture, n int) ([]string, rolePushWant, rolePushWant) {
		cs, _, alone := f.reviewed(n)
		args := []string{"git", "push", "-c", strings.Join(rolePushCommitIDs(cs), " | ")}
		want := alone
		want.cannotSet, want.cannotSetNames = 1, rolePushPastTheLimit(args[3], n, f.argMax)
		want.noMetaedit = true
		want.pushed = func(words []string) {
			if !slices.Equal(words, args) {
				f.t.Errorf("the real push ran with %q, want the role's own words %q", words, args)
			}
		}
		return args, want, alone
	}
	rolePushRunNamed(t, []rolePushNamedRow{
		{name: "-c <C1> | <C2>", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			f.argMax = rolePushLowArgMax
			return asWritten(f, 2)
		}},
		{name: "-c <D> -c mine(), D on E with an empty author, mine() naming M alone", setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			f.argMax = rolePushLowArgMax
			e := f.commitOn(rolePushNobody, rolePushNobody, "main", rolePushMessage("E", "implementer"))
			d := f.commitOn(rev, impl, e.commit, rolePushMessage("D", "implementer"))
			m := f.commitOn(impl, impl, "main", rolePushMessage("M", "implementer"))
			f.jj(rev, "new", "main")
			if got := f.read("mine()"); got.commit != m.commit {
				f.t.Fatalf("the implementer's mine() names %s before the push, want M, %s, alone", got.commit, m.commit)
			}
			args := []string{"git", "push", "-c", d.commit, "-c", "mine()"}
			want := rolePushWant{
				refs: []rolePushRef{
					{ref: "refs/heads/push-" + d.change[:12], commits: []rolePushCommit{{change: d.change, author: rev}, {change: e.change, author: impl}}},
					{ref: "refs/heads/push-" + m.change[:12], commits: []rolePushCommit{{change: m.change, author: impl, commit: m.commit}}},
					{ref: "refs/heads/push-" + e.change[:12], absent: true},
				},
				rewrites:  []rolePushRewrite{{role: "implementer", author: impl, change: e.change, old: e.commit}},
				same:      map[string]rolePushIdentity{m.commit: impl},
				metaedits: 1,
				cannotSet: 1, cannotSetNames: rolePushPastTheLimit(d.commit, 1, f.argMax),
				ids: []rolePushIDs{
					{argument: d.commit, commits: 1, words: 1, bound: rolePushRevisionBound},
					{argument: "mine()", commits: 1, words: 1, bound: rolePushRevisionBound},
				},
				oneWord: true,
			}
			return args, want, rolePushWant{}
		}},
		{name: "-c <E> | <C>, E with an empty author", setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			f.argMax = rolePushLowArgMax
			e := f.commitOn(rolePushNobody, rolePushNobody, "main", rolePushMessage("work 1", "implementer"))
			c := f.commitOn(rev, impl, "main", rolePushMessage("work 2", "implementer"))
			args := []string{"git", "push", "-c", e.commit + " | " + c.commit}
			want := rolePushWant{
				refs: []rolePushRef{
					{ref: "refs/heads/push-" + e.change[:12], commits: []rolePushCommit{{change: e.change, author: impl}}},
					{ref: "refs/heads/push-" + c.change[:12], commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}}},
				},
				rewrites:  []rolePushRewrite{{role: "implementer", author: impl, change: e.change, old: e.commit}},
				same:      map[string]rolePushIdentity{c.commit: rev},
				metaedits: 1,
				cannotSet: 1, cannotSetNames: rolePushPastTheLimit(args[3], 2, f.argMax),
			}
			return args, want, rolePushWant{}
		}},
	})
}

// The pushes of revision arguments of commits on main that the reviewer made with the implementer's
// trailer, whose command with each argument as the role wrote it or as the ids of its commits, the
// larger of which the step counts for each, is under the step's limit of a command, but would not
// be with a word of the step's bound of a word of ids in place of each argument:
//
//   - 21 arguments, `-c <C1> … -c <C21>`, each one commit's id, with the limit and the bound left
//     at 2,097,152 and 100,000 bytes: 21 words of the bound are over the limit;
//   - `-c "<C1> | <C2> | <C3>"`, with the bound lowered to rolePushGapBound bytes and the limit to
//     that many bytes over the push's environment.
//
// The step rewrites every commit, and the real push gets each argument as the ids of the commits
// it named before the step, as the step left them, in one word of its option each. The push exits
// 0, origin holds each commit under a bookmark of -c's own with the implementer's author, the step
// prints each rewrite and one line of each argument's ids, no line of an author it could not set,
// and no change is divergent. jj alone, on a twin state of the 21 made the same way, sends each
// commit with the reviewer's author.
func TestAPushUnderTheLimitWithEachArgumentAtItsIDsSetsEveryAuthor(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	// arguments makes n commits on main that the reviewer made with the implementer's trailer
	// (rolePushFixture.reviewed), and answers the push of each of words, a function of their ids,
	// under -c, what that push must leave, the ids of the commits each of words names in one word
	// under bound bytes, and what jj alone leaves.
	arguments := func(f *rolePushFixture, n, bound int, words func(ids []string) []string) ([]string, rolePushWant, rolePushWant) {
		cs, want, alone := f.reviewed(n)
		want.oneWord = true
		args := []string{"git", "push"}
		for _, word := range words(rolePushCommitIDs(cs)) {
			args = append(args, "-c", word)
			want.ids = append(want.ids, rolePushIDs{argument: word, commits: strings.Count(word, " | ") + 1, words: 1, bound: bound})
		}
		return args, want, alone
	}
	rolePushRunNamed(t, []rolePushNamedRow{
		{name: "21 -c arguments, each one commit's id", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			return arguments(f, 21, rolePushRevisionBound, func(ids []string) []string { return ids })
		}},
		{name: "-c <C1> | <C2> | <C3>, the bound of a word lowered", setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			f.bound = rolePushGapBound
			f.argMax = rolePushSize(f.env(f.path, impl)...) + rolePushGapBound
			return arguments(f, 3, rolePushGapBound, func(ids []string) []string { return []string{strings.Join(ids, " | ")} })
		}},
	})
}

// rolePushPartsHistory is how many commits the history of
// TestAMetaeditPastTheLimitOfACommandRunsInParts has: the `-r <id>` of each takes 60 bytes of a
// metaedit's command, 18,000 bytes for them all, several times the room rolePushPartsRoom leaves.
const rolePushPartsHistory = 300

// rolePushPartsRoom is how many bytes over the push's environment the step's limit of a command in
// TestAMetaeditPastTheLimitOfACommandRunsInParts is: over what a metaedit of the step's adds to
// that environment, its words but its `-r <id>` and the variables legion jj and the form's jj set,
// some hundreds of bytes, and so room for dozens of commits in each part.
const rolePushPartsRoom = 4096

// metaeditSizes is the bytes that Linux counted against its limit (execve(2)) for each metaedit
// that the recording jj of rolePushMeasuresMetaedit ran, in order: each string of its arguments,
// the recording jj's path first, and of its environment, with the NUL that ends it and the 8-byte
// pointer to it.
func (f *rolePushFixture) metaeditSizes() []int {
	f.t.Helper()
	data, err := os.ReadFile(f.next + ".environ")
	if err != nil {
		f.t.Fatal(err)
	}
	environs := strings.Split(strings.TrimSuffix(string(data), "\x1e"), "\x1e")
	var sizes []int
	for _, call := range f.calls() {
		if !call.has("metaedit") {
			continue
		}
		if len(sizes) == len(environs) {
			f.t.Fatalf("the recording jj ran more metaedits than the %d environments it wrote", len(environs))
		}
		environ := environs[len(sizes)]
		sizes = append(sizes, rolePushSize(append([]string{f.next}, call.args...)...)+len(environ)+8*strings.Count(environ, "\x00"))
	}
	if len(sizes) != len(environs) {
		f.t.Fatalf("the recording jj ran %d metaedits and wrote %d environments", len(sizes), len(environs))
	}
	return sizes
}

// rolePushSize is the bytes that Linux counts against its limit (execve(2)) for words, strings of
// a command's arguments or environment: each with the NUL that ends it and the 8-byte pointer to
// it.
func rolePushSize(words ...string) int {
	size := 0
	for _, word := range words {
		size += len(word) + 1 + 8
	}
	return size
}

// `jj git push -b legion/K-1` of a history of 300 commits that no remote holds, each of which the
// reviewer authored with the implementer's trailer (mutableHistory), with the step's limit of a
// command lowered (rolePushArgMaxVariable) to rolePushPartsRoom bytes over the push's environment,
// which one metaedit of the 300 commits would pass. The push has no revision argument, so the
// step's decision on the push's command does not stop it: it runs the implementer's metaedit in
// two parts or more, the command of each, its arguments and its environment, under the limit (a
// recording jj ahead of the real one measures each, metaeditSizes), and sets every author. The
// push exits 0, origin's legion/K-1 holds the 300 commits, each with the implementer's author, the
// step prints one rewrite for each commit and no other line, and no change is divergent.
func TestAMetaeditPastTheLimitOfACommandRunsInParts(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, rolePushMeasuresMetaedit)
		f.mutableHistory(rolePushPartsHistory)
		f.argMax = rolePushSize(f.env(f.path, impl)...) + rolePushPartsRoom
		divergent := f.divergence()
		code, stderr := f.pushLong(impl, "git", "push", "-b", "legion/K-1")
		rewrites, printed := rolePushCountRewrites(stderr)
		f.check(rolePushWant{others: true}, code, printed, divergent, 0)
		if rewrites != rolePushPartsHistory {
			t.Errorf("the push printed %d rewrites, want %d; it printed beside them:\n%s", rewrites, rolePushPartsHistory, printed)
		}
		got, _ := f.remoteCommits(f.bare, "refs/heads/legion/K-1")
		var others []string
		for _, c := range got {
			if c.name != impl.name || c.email != impl.email {
				others = append(others, c.id+" by "+c.name+" <"+c.email+">")
			}
		}
		if len(got) != rolePushPartsHistory || len(others) > 0 {
			t.Errorf("origin holds %d commits of legion/K-1 above the seed, %d with another author than the implementer's (%q), want %d, none", len(got), len(others), others, rolePushPartsHistory)
		}
		sizes := f.metaeditSizes()
		if len(sizes) < 2 {
			t.Errorf("the step ran %d metaedits, want the implementer's in two parts or more", len(sizes))
		}
		for i, size := range sizes {
			if size >= f.argMax {
				t.Errorf("metaedit %d of %d took %d bytes of arguments and environment, want under the step's limit, %d", i+1, len(sizes), size, f.argMax)
			}
		}
	})
}

// rolePushLongLimit is how long a push of a long history may run (pushLong): jj's own metaedit of
// more than 3,000 commits takes half a minute on an idle machine, which a busy one stretches past
// trailerRunLimit's minute.
const rolePushLongLimit = 10 * time.Minute

// pushLong is push with rolePushLongLimit in place of trailerRunLimit (trailerRunWithin): the push
// runs in a process group of its own, which the limit kills whole, and a push that cannot start,
// or that runs past the limit, fails the test.
func (f *rolePushFixture) pushLong(as rolePushIdentity, args ...string) (code int, stderr string) {
	f.t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		f.t.Fatal(err)
	}
	code, _, stderr = trailerRunWithin(f.t, rolePushLongLimit, f.repo, f.env(f.path, as), strings.NewReader(""), sh, append([]string{"-c", `exec jj "$@"`, "jj"}, args...)...)
	return code, stderr
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
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, "")
		f.mutableHistory(rolePushHistory)
		divergent := f.divergence()
		code, stderr := f.pushLong(rolePushImplementer, rolePushBookmark...)
		rewrites, printed := rolePushCountRewrites(stderr)
		f.check(rolePushWant{others: true}, code, printed, divergent, 0)
		f.checkHistory(rolePushImplementer, printed)
		if rewrites != rolePushHistory {
			t.Errorf("the push printed %d rewrites, want %d; it printed beside them:\n%s", rewrites, rolePushHistory, printed)
		}
	})
}
