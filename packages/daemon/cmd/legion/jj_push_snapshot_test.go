package main

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The tests in this file are more of the step's (jj_push_test.go): those of its one snapshot of
// the working copy, which it takes before it changes a commit or a word of the push, or before its
// first read where git moved HEAD or a branch (jj_push_git_test.go), and which is then the push's
// one snapshot: the order of the step's commands around that snapshot and the real push, the
// role's own --ignore-working-copy, and a descendant of the working copy whose write no command
// has snapshotted, with the helpers they use. descendant serves jj_push_race_test.go too, and
// opsSince jj_push_test.go. As jj_push_test.go does, the file names nothing the step's change
// adds, so it compiles and runs at the base too.

// rolePushWaitingIgnoredForms are the waiting forms that send @ by legion/K-1 with the role's own
// --ignore-working-copy, before the subcommand pair and after it: the push takes no snapshot, and
// sends @ without the write.
var rolePushWaitingIgnoredForms = []rolePushWaitingForm{
	{name: "--bookmark on @ with --ignore-working-copy", args: []string{"--ignore-working-copy", "git", "push", "--bookmark", "legion/K-1"}, ref: "legion/K-1", bookmark: true},
	{name: "--bookmark on @ with --ignore-working-copy after git push", args: []string{"git", "push", "--ignore-working-copy", "--bookmark", "legion/K-1"}, ref: "legion/K-1", bookmark: true},
}

// descendant makes the state of the rows of a descendant of the working copy: P on main, which the
// reviewer made with the implementer's trailer; E on P and Y on E, which the implementer made with
// its own trailer, under be and by; Q on main, which the reviewer made with the implementer's
// trailer, under bq; each bookmark tracked, which origin holds at the seed; @ brought back to E by
// jj edit, as restoreWorkspace (internal/launcher/restore.go) brings a saved working copy back; and
// a write in E that no command has snapshotted. The implementer's mine() then names Y and E, which
// descendant checks, and once the step has rewritten P, P too, and Q too once it has rewritten Q,
// which heads(mine()) then names beside Y.
func (f *rolePushFixture) descendant() (p, e, y, q rolePushChange) {
	f.t.Helper()
	impl, rev := rolePushImplementer, rolePushReviewer
	for _, name := range []string{"be", "by", "bq"} {
		f.track(name)
	}
	q = f.commitOn(rev, impl, "main", rolePushMessage("Q", "implementer"))
	p = f.commitOn(rev, impl, "main", rolePushMessage("P", "implementer"))
	e = f.commitOn(impl, impl, p.commit, rolePushMessage("E", "implementer"))
	y = f.commitOn(impl, impl, e.commit, rolePushMessage("Y", "implementer"))
	f.bookmark("bq", q.commit)
	f.bookmark("be", e.commit)
	f.bookmark("by", y.commit)
	f.jj(impl, "edit", e.commit)
	if mine := strings.Fields(f.jj(impl, "--ignore-working-copy", "log", "--no-graph", "-r", "mine()", "-T", `commit_id ++ "\n"`)); !slices.Equal(mine, []string{y.commit, e.commit}) {
		f.t.Fatalf("the implementer's mine() names %v before the push, want Y and E, %s and %s, alone", mine, y.commit, e.commit)
	}
	f.writeFile(filepath.Join(f.repo, "waiting.txt"), rolePushWaitingContent)
	return p, e, y, q
}

// rolePushDescendantRows are the pushes of a descendant of the working copy whose write no command
// has snapshotted (rolePushFixture.descendant, and a state of a row's own where it has nothing to
// rewrite), each pushed by the implementer.
func rolePushDescendantRows() []rolePushNamedRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	// written is c as origin holds it after the push: with the write, and the implementer's
	// author.
	written := func(c rolePushChange) rolePushCommit {
		return rolePushCommit{change: c.change, author: impl, file: "waiting.txt", content: rolePushWaitingContent}
	}
	// rewritten is c as origin holds it after the push through the form's jj, with the
	// implementer's author, and held as it holds it after jj alone's, as it was.
	rewritten := func(c rolePushChange) rolePushCommit { return rolePushCommit{change: c.change, author: impl} }
	held := func(c rolePushChange) rolePushCommit {
		return rolePushCommit{change: c.change, author: rev, commit: c.commit}
	}
	rewrite := func(c rolePushChange) rolePushRewrite {
		return rolePushRewrite{role: "implementer", author: impl, change: c.change, old: c.commit}
	}
	return []rolePushNamedRow{
		// -r sends each bookmark that points at a commit it names: be on E and by on Y, and not bq
		// on Q, which stays where track put it, the seed. mine() names P too once the step has
		// rewritten it, so the real push gets the ids of Y and of @.
		{name: "-r mine(), mine() naming E on be and Y on by, @ brought back to E with a waiting write", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			p, e, y, q := f.descendant()
			refs := func(under rolePushCommit) []rolePushRef {
				return []rolePushRef{
					{ref: "refs/heads/be", commits: []rolePushCommit{written(e), under}},
					{ref: "refs/heads/by", commits: []rolePushCommit{written(y), written(e), under}},
					{ref: "refs/heads/bq"},
				}
			}
			want := rolePushWant{
				refs: refs(rewritten(p)), rewrites: []rolePushRewrite{rewrite(p)}, same: map[string]rolePushIdentity{q.commit: rev},
				ids: []rolePushIDs{{argument: "mine()", commits: 2, words: 1, bound: rolePushRevisionBound}},
			}
			alone := rolePushWant{refs: refs(held(p)), same: map[string]rolePushIdentity{p.commit: rev, q.commit: rev}}
			return []string{"git", "push", "-r", "mine()"}, want, alone
		}},
		// -b bq sends Q, which the step rewrites with P: heads(mine()) names Q too after the step,
		// so the real push gets n as the id of Y. be and by stay where track put them, the seed.
		{name: "-b bq --named n=heads(mine()), heads(mine()) naming Y, @ brought back to E with a waiting write", alone: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			p, e, y, q := f.descendant()
			refs := func(underY, onQ rolePushCommit) []rolePushRef {
				return []rolePushRef{
					{ref: "refs/heads/n", commits: []rolePushCommit{written(y), written(e), underY}},
					{ref: "refs/heads/bq", commits: []rolePushCommit{onQ}},
					{ref: "refs/heads/be"}, {ref: "refs/heads/by"},
				}
			}
			want := rolePushWant{
				refs: refs(rewritten(p), rewritten(q)), rewrites: []rolePushRewrite{rewrite(p), rewrite(q)},
				ids: []rolePushIDs{rolePushOneIDLine("heads(mine())")},
			}
			alone := rolePushWant{refs: refs(held(p), held(q)), same: map[string]rolePushIdentity{p.commit: rev, q.commit: rev}}
			return []string{"git", "push", "-b", "bq", "--named", "n=heads(mine())"}, want, alone
		}},
		// E on main under be and Y on E under by, both the implementer's with its own trailer, and
		// H, Y's first commit, which a describe of Y hid: H stands for Y, its change's one visible
		// commit, and the step has no commit to rewrite. -c of H changes a word of the push all the
		// same, so the step takes the push's one snapshot first, which rebases Y onto E with the
		// write, and gives the push the id of that Y. jj alone pushes H itself, and so has no row.
		// The snapshot family's check that the two forms agree (rolePushNamedRow.bothForms).
		{name: "-c <a hidden commit id of Y>, nothing to rewrite, @ brought back to E with a waiting write", bothForms: true, setup: func(f *rolePushFixture) ([]string, rolePushWant, rolePushWant) {
			f.track("be")
			f.track("by")
			e := f.commitOn(impl, impl, "main", rolePushMessage("E", "implementer"))
			h := f.commitOn(impl, impl, e.commit, rolePushMessage("Y", "implementer"))
			f.jj(impl, "describe", "-r", h.commit, "-m", rolePushMessage("Y, described again", "implementer"))
			y := f.read(h.change)
			f.bookmark("be", e.commit)
			f.bookmark("by", y.commit)
			f.jj(impl, "edit", e.commit)
			f.writeFile(filepath.Join(f.repo, "waiting.txt"), rolePushWaitingContent)
			want := rolePushWant{
				refs: []rolePushRef{
					{ref: "refs/heads/push-" + y.change[:12], commits: []rolePushCommit{written(y), written(e)}},
					{ref: "refs/heads/be"}, {ref: "refs/heads/by"},
				},
				ids:        []rolePushIDs{rolePushOneIDLine(h.commit)},
				noMetaedit: true, snapshots: rolePushTakesOneSnapshot,
			}
			return []string{"git", "push", "-c", h.commit}, want, rolePushWant{}
		}},
	}
}

// A push that names a descendant of @ sends it as the step left it, with a write in @ that no
// command has snapshotted: where the step has a commit or a word to change, it takes the push's one
// snapshot first, and the real push then runs with --ignore-working-copy and takes no snapshot of
// its own, so every commit id the step reads, Y's among them, names the commit the real push sees
// (rolePushDescendantRows). Over P, the other role's commit with the implementer's trailer, E on P
// under be and Y on E under by, the implementer's, and Q on main under bq, the other role's with
// the implementer's trailer, with @ brought back to E by jj edit and a write in E, `-r mine()`
// sends be and by, and `-b bq --named n=heads(mine())` puts n on Y, which the step gives the push
// as its id, each commit of E's and Y's with the write. The push exits 0, origin holds the refs the
// row names and no other beside main, P and Q, where sent, with the implementer's author, the step
// prints each rewrite and the line of the ids, and no change is divergent; jj alone, on a twin
// state made the same way, sends the same refs with the write, P and Q as they were. Over E on main
// and Y on E alone, with nothing to rewrite, `-c <H>`, H the commit of Y that a describe hid, sends
// Y as the step's one snapshot rebased it, with the write: the step takes that snapshot (a
// recording jj ahead of the real one sees one command of the step's with no
// --ignore-working-copy), runs no metaedit, prints the line of H's id, and no change is divergent.
func TestADescendantOfTheWorkingCopyGoesWithItsWaitingWrite(t *testing.T) {
	t.Parallel()
	rolePushRunNamed(t, rolePushDescendantRows())
}

// opsSince is the description of each operation after op, an operation id, in the repository's
// log, the newest first.
func (f *rolePushFixture) opsSince(op string) []string {
	f.t.Helper()
	var descriptions []string
	for line := range strings.SplitSeq(f.jj(rolePushImplementer, "--ignore-working-copy", "op", "log", "--no-graph", "-T", `id ++ " " ++ description.first_line() ++ "\n"`), "\n") {
		id, description, _ := strings.Cut(line, " ")
		if id == op {
			return descriptions
		}
		descriptions = append(descriptions, description)
	}
	f.t.Fatalf("the operation log holds no operation %s", op)
	return nil
}

// The step's jj commands run one after another, and the real push starts after the last of them
// ends (a recording jj ahead of the real one writes when each starts and ends), each push with a
// write in @ that no command has snapshotted. With a commit to rewrite, @ by the other role in each
// waiting form, the recording jj sees one command of the step's with no --ignore-working-copy, its
// snapshot, and none after the step's first metaedit, and the real push runs with
// --ignore-working-copy as its first word, once, so that the step's snapshot is the push's one
// snapshot (check); the repository's operation log holds that one snapshot, older than each
// metaedit of the step; and the remote gets the write in the commit the step printed. What that
// snapshot prints on standard error reaches the role once, as jj alone's push prints it, though
// the stand-in that prints it runs at every command that takes a snapshot, a real push without
// --ignore-working-copy among them; with the role's own --quiet, that snapshot runs with --quiet
// too, and a line that --quiet silences, as jj's of the descendants of @ it rebased, never reaches
// the role. A snapshot of an @ outside the push, with an empty author, runs with the push's own
// --config user.email, which so becomes @'s author's email, as jj alone's push would leave it.
// With nothing to rewrite, @ by its trailer's role, every command of the step's carries
// --ignore-working-copy, the real push carries none, as the role wrote none, and the one snapshot
// is the real push's own. Where the push takes a snapshot of its own, the step compares git's
// HEAD and branches with jj's, which git left as jj's commands set them, with reads that carry
// --ignore-working-copy (checkCompared); where it takes none, the step compares nothing. With the
// role's own --ignore-working-copy, before the subcommand pair or after it, and a commit to
// rewrite, every command of the step's carries --ignore-working-copy, the real push carries that
// flag once, as the role wrote it, and the push adds no snapshot: the remote gets @ with the
// implementer's author and without the write, as jj alone, on a twin state made the same way,
// sends @ without the write, with its author as it was, and adds no snapshot either.
func TestTheStepsCommandsRunOneAfterAnother(t *testing.T) {
	t.Parallel()
	// Each row is a push of a state that setup makes, which answers the push's arguments and what
	// it leaves, or, where setup is nil, of a waiting form (rolePushFixture.waiting); whether the
	// step has a commit to rewrite; the recording jj's behaviour, rolePushRecords where it is "";
	// and, where after is not nil, more that the row checks of what the push left and printed.
	// bothForms runs the row in each form (rolePushForEach): the row with a --config of the role's,
	// which the pod's wrapper reads too, is this test's check that the two forms agree.
	type row struct {
		name      string
		form      rolePushWaitingForm
		rewrite   bool
		behaviour string
		setup     func(f *rolePushFixture) ([]string, rolePushWant)
		after     func(f *rolePushFixture, stderr string)
		bothForms bool
	}
	var rows []row
	for _, waiting := range rolePushWaitingForms {
		rows = append(rows, row{name: waiting.name, form: waiting, rewrite: true})
	}
	for _, waiting := range rolePushWaitingIgnoredForms {
		rows = append(rows, row{name: waiting.name + ", a commit to rewrite", form: waiting, rewrite: true})
	}
	const another = "another@example.invalid"
	quiet := rolePushWaitingForm{name: "--bookmark on @ with --quiet", args: []string{"--quiet", "git", "push", "--bookmark", "legion/K-1"}, ref: "legion/K-1", bookmark: true}
	rows = append(rows,
		row{name: rolePushWaitingForms[0].name + ", a snapshot that prints on standard error", form: rolePushWaitingForms[0], rewrite: true, behaviour: rolePushSnapshotPrints,
			after: func(f *rolePushFixture, stderr string) {
				if n := strings.Count(stderr, rolePushSnapshotLine); n != 1 {
					f.t.Errorf("the push printed %q %d times, want once, as its snapshot printed it:\n%s", rolePushSnapshotLine, n, stderr)
				}
			}},
		row{name: quiet.name + ", a snapshot that prints on standard error", form: quiet, rewrite: true, behaviour: rolePushSnapshotPrints, bothForms: true,
			after: func(f *rolePushFixture, stderr string) {
				if n := strings.Count(stderr, rolePushSnapshotLine); n != 0 {
					f.t.Errorf("the push printed %q %d times, want none, as its snapshot prints it under the role's --quiet:\n%s", rolePushSnapshotLine, n, stderr)
				}
			}},
		row{name: "--bookmark on a commit to rewrite, @ outside the push with an empty author and a waiting write, the push's own --config user.email=<X>", rewrite: true, bothForms: true,
			setup: func(f *rolePushFixture) ([]string, rolePushWant) {
				c := f.commitOn(rolePushReviewer, rolePushImplementer, "main", rolePushMessage("work", "implementer"))
				f.bookmark("legion/K-1", c.commit)
				f.jj(rolePushNobody, "new", "main")
				f.writeFile(filepath.Join(f.repo, "waiting.txt"), rolePushWaitingContent)
				return []string{"git", "push", "--bookmark", "legion/K-1", "--config", "user.email=" + another}, rolePushSends("legion/K-1", c, "implementer", rolePushImplementer)
			},
			after: func(f *rolePushFixture, _ string) {
				if got := f.jj(rolePushImplementer, "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", "author.email()"); got != another {
					f.t.Errorf("@'s author's email is %q after the push, want %q, the push's own --config user.email", got, another)
				}
			}},
	)
	for _, waiting := range rolePushWaitingForms {
		rows = append(rows, row{name: rolePushWaitingRow(waiting, false).name, form: waiting})
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			// ignores is whether the push takes no snapshot of its own.
			ignores := slices.Contains(row.form.args, "--ignore-working-copy")
			setup := row.setup
			if setup == nil {
				setup = func(f *rolePushFixture) ([]string, rolePushWant) {
					_, want := f.waiting(row.form, row.rewrite)
					return row.form.args, want
				}
			}
			rolePushForEach(t, row.bothForms, func(t *testing.T, form rolePushForm) {
				if ignores {
					twin := newRolePushFixture(t, form, "")
					c, _ := twin.waiting(row.form, row.rewrite)
					before, divergent := twin.op(), twin.divergence()
					code, _, stderr := twin.alone(rolePushImplementer, row.form.args...)
					twin.check(rolePushWant{
						refs: []rolePushRef{{ref: "refs/heads/" + row.form.ref, commits: []rolePushCommit{{change: c.change, author: rolePushReviewer, commit: c.commit, lacks: "waiting.txt"}}}},
						same: map[string]rolePushIdentity{c.commit: rolePushReviewer},
					}, row.form.args, code, stderr, divergent, 0)
					if ops := twin.opsSince(before); slices.Contains(ops, "snapshot working copy") {
						t.Errorf("jj alone added the operations %q, newest first, want no snapshot of the working copy", ops)
					}
				}
				f := newRolePushFixture(t, form, cmp.Or(row.behaviour, rolePushRecords))
				args, want := setup(f)
				want.snapshots = rolePushTakesNoSnapshot
				if row.rewrite && !ignores {
					want.snapshots = rolePushTakesOneSnapshot
				}
				before, divergent := f.op(), f.divergence()
				code, _, stderr := f.push(rolePushImplementer, args...)
				f.check(want, args, code, stderr, divergent, 0)
				if row.after != nil {
					row.after(f, stderr)
				}
				calls := f.calls()
				f.checkCompared(calls, !ignores)
				pushes := f.oneRealPush(calls)
				push := calls[pushes]
				steps := slices.Delete(slices.Clone(calls), pushes, pushes+1)
				isMetaedit := func(c rolePushCall) bool { return c.has("metaedit") }
				if slices.ContainsFunc(steps, isMetaedit) != row.rewrite {
					t.Errorf("the recording jj ran %v before the push, want the step's metaedit among them: %v", steps, row.rewrite)
				}
				slices.SortFunc(steps, func(a, b rolePushCall) int { return cmp.Compare(a.start, b.start) })
				for i, call := range steps {
					if !call.has("--ignore-working-copy") && slices.ContainsFunc(steps[:i], isMetaedit) {
						t.Errorf("the step ran %q with no --ignore-working-copy, a snapshot after its first metaedit", call.args)
					}
					if i > 0 && call.start < steps[i-1].end {
						t.Errorf("the step ran %q from %d before %q ended at %d", call.args, call.start, steps[i-1].args, steps[i-1].end)
					}
					if push.start < call.end {
						t.Errorf("the real push started at %d, before the step's %q ended at %d", push.start, call.args, call.end)
					}
				}
				// Newest first: an operation after the snapshot's index is older than it.
				ops := f.opsSince(before)
				taken := slices.Index(ops, "snapshot working copy")
				switch {
				case ignores:
					if taken >= 0 {
						t.Errorf("the push added the operations %q, newest first, want no snapshot of the working copy", ops)
					}
				case taken < 0 || slices.Contains(ops[taken+1:], "snapshot working copy"):
					t.Errorf("the push added the operations %q, newest first, want one snapshot of the working copy", ops)
				case slices.ContainsFunc(ops[taken+1:], func(op string) bool { return strings.HasPrefix(op, "edit commit metadata") }):
					t.Errorf("the push added the operations %q, newest first, want its snapshot older than each metaedit", ops)
				}
			})
		})
	}
}
