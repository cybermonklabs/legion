package main

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The tests in this file are more of the step's (jj_push_test.go): pushes of a revision argument
// that a rewrite of the step can make name other commits than it named before the step, or that a
// --config which jj drops for the push would make name other commits, with the helpers that only
// they use. As jj_push_test.go does, the file names nothing the step's change adds, so it compiles
// and runs at the base too.

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
// (rolePushFixture.mine), ref being the bookmark the argument sends M under, or "" where it sends
// none: origin holds top on the rewritten A and P, each with the implementer's author, ref on M as
// it was, and mid and other where track put them, but where ref is other; and alone, what jj alone
// leaves, A and P as they were.
func rolePushMineSends(ref string, m, p, a rolePushChange) (want, alone rolePushWant) {
	impl, rev := rolePushImplementer, rolePushReviewer
	held := []rolePushRef{{ref: "refs/heads/mid"}}
	if ref != "" {
		held = append(held, rolePushRef{ref: "refs/heads/" + ref, commits: []rolePushCommit{{change: m.change, author: impl, commit: m.commit}}})
	}
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

// rolePushNamedRows are the revision arguments that name other commits after a rewrite of the
// step: a change offset of a divergent change, which the step's rewrite of one of its commits
// renumbers, whether that commit is the argument's own or another argument's, on a short history
// and on one whose commits' ids, joined, are over the bound of a revision the step gives as ids,
// and an author revset whose author the rewrite gives other commits too, whether of the argument
// or of the push's other commits, one of which names no commit before the step; and an alias that
// names another commit under a --config that jj drops for the push, one in front of `git push`
// beside one after it; each pushed by the implementer.
func rolePushNamedRows() []rolePushNamedRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	return []rolePushNamedRow{
		{name: "--named x=<change>/1 of a divergent change", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			change, d1, d2 := f.offsets(rolePushMessage("D1", "implementer"), rolePushMessage("D2", "reviewer"))
			want := rolePushSends("x", d1, "implementer", impl)
			want.same = map[string]rolePushIdentity{d2.commit: rev}
			want.ids = []rolePushIDs{rolePushOneIDLine(change + "/1")}
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
			want.ids = []rolePushIDs{rolePushOneIDLine(change + "/1")}
			return []string{"git", "push", "-r", change + "/1"}, want, rolePushWant{}
		}},
		// The step's rewrite of C gives it the implementer's author, which the argument names: C
		// gets no bookmark of its own, as with jj alone.
		{name: "-c author(<the implementer's email>) & mutable(), C the other role's under D", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			c := f.commitOn(rev, impl, "main", rolePushMessage("C", "implementer"))
			d := f.commitOn(impl, impl, c.commit, rolePushMessage("D", "implementer"))
			f.jj(rev, "new", "main")
			argument := `author(substring:"` + impl.email + `") & mutable()`
			noC := rolePushRef{ref: "refs/heads/push-" + c.change[:12], absent: true}
			want := rolePushWant{
				refs: []rolePushRef{{ref: "refs/heads/push-" + d.change[:12], commits: []rolePushCommit{
					{change: d.change, author: impl}, {change: c.change, author: impl},
				}}, noC},
				rewrites: []rolePushRewrite{{role: "implementer", author: impl, change: c.change, old: c.commit}},
				ids:      []rolePushIDs{rolePushOneIDLine(argument)},
			}
			alone := rolePushWant{
				refs: []rolePushRef{{ref: "refs/heads/push-" + d.change[:12], commits: []rolePushCommit{
					{change: d.change, author: impl, commit: d.commit}, {change: c.change, author: rev, commit: c.commit},
				}}, noC},
			}
			return []string{"git", "push", "-c", argument}, want, alone
		}},
		// The step's rewrite of B and C takes the other role's author from both, which the
		// argument names: B still has "base" in its description, and C nothing the argument names.
		{name: `-c description(substring:"base") | (author(<the other role's email>) & mutable()), C on B`, alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			b := f.commitOn(rev, impl, "main", rolePushMessage("base", "implementer"))
			c := f.commitOn(rev, impl, b.commit, rolePushMessage("work", "implementer"))
			argument := `description(substring:"base") | (author(substring:"` + rev.email + `") & mutable())`
			want := rolePushWant{
				refs: []rolePushRef{
					{ref: "refs/heads/push-" + b.change[:12], commits: []rolePushCommit{{change: b.change, author: impl}}},
					{ref: "refs/heads/push-" + c.change[:12], commits: []rolePushCommit{{change: c.change, author: impl}, {change: b.change, author: impl}}},
				},
				rewrites: []rolePushRewrite{
					{role: "implementer", author: impl, change: b.change, old: b.commit},
					{role: "implementer", author: impl, change: c.change, old: c.commit},
				},
				ids: []rolePushIDs{{argument: argument, commits: 2, words: 1, bound: rolePushRevisionBound}},
			}
			heldB := rolePushCommit{change: b.change, author: rev, commit: b.commit}
			alone := rolePushWant{refs: []rolePushRef{
				{ref: "refs/heads/push-" + b.change[:12], commits: []rolePushCommit{heldB}},
				{ref: "refs/heads/push-" + c.change[:12], commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}, heldB}},
			}}
			return []string{"git", "push", "-c", argument}, want, alone
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
			want.ids = []rolePushIDs{rolePushOneIDLine(change + "/0"), rolePushOneIDLine(change + "/1")}
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
			want.ids = []rolePushIDs{rolePushOneIDLine(change + "/0")}
			return []string{"git", "push", "-b", "a", "-r", change + "/0"}, want, alone
		}},
		// -r sends each bookmark that points at a commit it names: other, on M, and not mid, on P,
		// which mine() names only once the step has rewritten it.
		{name: "-b top -r mine(), mine() naming M on other alone, the step rewriting P on mid and A on top", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			m, p, a := f.mine()
			want, alone := rolePushMineSends("other", m, p, a)
			want.ids = []rolePushIDs{rolePushOneIDLine("mine()")}
			return []string{"git", "push", "-b", "top", "-r", "mine()"}, want, alone
		}},
		// --named names one commit: jj would refuse mine() once the step has rewritten P and A.
		{name: "-b top --named nm=mine(), mine() naming M alone, the step rewriting P on mid and A on top", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			m, p, a := f.mine()
			want, alone := rolePushMineSends("nm", m, p, a)
			want.ids = []rolePushIDs{rolePushOneIDLine("mine()")}
			return []string{"git", "push", "-b", "top", "--named", "nm=mine()"}, want, alone
		}},
		// The argument names no commit before the step, and P once the step has rewritten it: the
		// real push gets -r none() in its place, which sends no bookmark, as jj alone sends none of
		// it, and the step prints the one line that says so.
		{name: `-b top -r author(<the implementer's email>) & description(substring:"P"), naming no commit before the step and P after it`, alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			m, p, a := f.mine()
			argument := `author(substring:"` + impl.email + `") & description(substring:"P")`
			want, alone := rolePushMineSends("", m, p, a)
			want.ids = []rolePushIDs{{argument: argument}}
			want.pushed = func(words []string) {
				if got := words[len(words)-2:]; !slices.Equal(got, []string{"-r", "none()"}) {
					f.t.Errorf("the real push ends with %q, want -r none()", got)
				}
			}
			return []string{"git", "push", "-b", "top", "-r", argument}, want, alone
		}},
		// none() names no commit before the step and after it, so the real push gets it as the role
		// wrote it, with no line, beside legion/K-1 on the commit the step rewrote: the role's own
		// words, every byte, after the --ignore-working-copy that the step's snapshot gives it.
		{name: "-b legion/K-1 -r none(), the step rewriting legion/K-1's commit", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
			f.bookmark("legion/K-1", c.commit)
			args := []string{"git", "push", "-b", "legion/K-1", "-r", "none()"}
			want := rolePushSends("legion/K-1", c, "implementer", impl)
			want.pushed = f.pushedAfterSnapshot(args)
			alone := rolePushWant{
				refs: []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}}}},
				same: map[string]rolePushIdentity{c.commit: rev},
			}
			return args, want, alone
		}},
		// The repository's configuration sets sendme to C's id, and the push's own --config in
		// front of `git push` sets it to D's, beside a --config after the pair: jj keeps the one
		// after the pair alone, so the push reads sendme as the repository's, C, which -r sends
		// under c, and leaves d, on D, where track put it, the seed. The step reads sendme so too:
		// it rewrites C alone, and since jj resolves sendme to the hidden C after that rewrite,
		// the real push gets the id of the commit that replaced it.
		{name: "-r sendme, the repository's alias of C, beside a --config in front of git push that sets it to D and one after git push", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			f.track("c")
			f.track("d")
			c := f.commitOn(rev, impl, "main", rolePushMessage("C", "implementer"))
			d := f.commitOn(rev, impl, "main", rolePushMessage("D", "implementer"))
			f.bookmark("c", c.commit)
			f.bookmark("d", d.commit)
			f.jj(impl, "config", "set", "--repo", "revset-aliases.sendme", `"`+c.commit+`"`)
			atSeed := rolePushRef{ref: "refs/heads/d"}
			want := rolePushSends("c", c, "implementer", impl)
			want.refs = append(want.refs, atSeed)
			want.same = map[string]rolePushIdentity{d.commit: rev}
			want.ids = []rolePushIDs{rolePushOneIDLine("sendme")}
			alone := rolePushWant{
				refs: []rolePushRef{{ref: "refs/heads/c", commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}}}, atSeed},
				same: map[string]rolePushIdentity{c.commit: rev, d.commit: rev},
			}
			return []string{"--config", `revset-aliases.sendme="` + d.commit + `"`, "git", "push", "-r", "sendme", "--config", "ui.color=never"}, want, alone
		}},
	}
}

// A revision argument that a rewrite of the step can make name other commits than it named before
// the step reaches the real push naming exactly the commits it named before, each one the step
// rewrote as rewritten (rolePushNamedRows), whether the rewrite is of the argument's own commits or
// of another argument's: a change offset `<change>/<n>`, which counts a change's commits newest
// first, the step's own among them, and an author revset whose author the step's rewrite gives
// another commit. The push exits 0, origin holds the refs the row names and no other beside main,
// each commit with its trailer's author, the step prints each rewrite and the one line of each
// argument it gives as ids, and no change is divergent; jj alone, on a twin state made the same
// way, sends what the row says it does. So `--named f=<X>/0 --named g=<X>/1` puts f on the commit
// offset 0 named before the step, `-b a -r <X>/0` still sends b, on that commit, `-b top -r
// mine()` sends no bookmark of a commit that mine() names only after the step, and `-b top
// --named nm=mine()` exits 0. `-b top -r 'author(<the implementer's email>) &
// description(substring:"P")'`, which names no commit before the step and P after it, reaches the
// real push as `-r none()`, and the step prints the one line that says so, while `-b legion/K-1
// -r none()`, of an argument that names no commit before the step or after it, reaches the real
// push as the role wrote it, with no line. `-r ::<change>/1` on a history of more than 3,000
// commits sends the rewritten commit alone, and the real push's revision, which a recording jj
// ahead of the real one sees, is under the bound of 100,000 bytes, which the ids of the commits it
// names are over. `-r sendme`, which the repository's configuration sets to C's id and a --config
// in front of `git push` to D's, beside a --config after `git push`, sends C, rewritten, as jj
// alone sends C, and leaves D as it was. A -c of a change offset run twice has a test of its own,
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
// and leaves origin as the first push left it; the recording jj's log is read from after the first
// push, so that each push's check reads its own real push.
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
		f.forgetCalls()
		code, _, stderr := f.push(impl, args...)
		if strings.Contains(stderr, "Bookmark already exists") {
			t.Errorf("the push run again printed `Bookmark already exists`:\n%s", stderr)
		}
		f.check(again, args, code, stderr, divergent, 0)
		f.onlyRefs(again)
	})
}
