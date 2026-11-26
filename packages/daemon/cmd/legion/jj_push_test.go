package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests in this file are those of the step that runs before every `jj git push` and sets the
// author of each commit the push sends from that commit's first Legion-Role trailer, where its
// rules let it (dispatch://INFRA-4172). Each runs a role's push as a process, through one of the
// two forms of Legion's jj, on a real jj repository with a local bare remote:
//
//   - the pod's form: a copy of packages/daemon/docker/jj first on PATH, whose real=, wrapper= and,
//     where it has one, legion= lines are repointed at the real jj (or a recording jj ahead of it),
//     a git first on PATH that runs the real git, and this test binary run as legion
//     (trailerLegion);
//   - the tmux form: the bin directory that workerbin.Install writes for that legion, first on
//     PATH, with the real jj (or a recording jj ahead of it) after it.
//
// A case that tests a form (its expansion of the line, its wrapper's refusals and its filtering of
// warnings, which jj it runs, its own help) runs under both forms, as does each row whose line
// holds a configuration layer, an alias or a `--` of the role's, which the pod's wrapper reads
// itself, and one row or test of each family of the step's own logic, the check that the two
// forms agree. Every other case of the step's own logic runs under the pod's form alone; the
// fixture's comment above rolePushEachForm says, from the code, what a form changes and what it
// does not. TestAFailingReadOfThePushsConfigurationSendsNothing, of a read that only the pane's
// legion jj makes, runs under the tmux form alone. Neither file names anything the step's change
// adds: the fixture writes out the table's variable and those of the test binary's bounds of the
// step, and builds every command's environment. So both compile and run at the base too, where
// docker/jj hands no push to `legion jj` and workerbin.Install writes no jj; there the tests of a
// change fail, and those of what stays pass.

// rolePushAuthorRows are the ways a commit comes to have another role's author, and the states of
// the commits a push sends, each pushed by the implementer.
func rolePushAuthorRows() []rolePushRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	rows := []rolePushRow{
		// The authors family's check that the two forms agree (rolePushRow.bothForms).
		{name: "made by the other role's jj new", bothForms: true, args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
			f.bookmark("legion/K-1", c.commit)
			return rolePushSends("legion/K-1", c, "implementer", impl)
		}},
		// restoreWorkspace (internal/launcher/restore.go) brings a saved working copy back with
		// `jj edit`, with the author it was saved with.
		{name: "@ brought back by jj edit", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			f.jj(rev, "new", "main")
			made := f.read("@")
			f.jj(rev, "new")
			f.jj(impl, "edit", made.change)
			f.write()
			f.jj(impl, "describe", "-m", rolePushMessage("work", "implementer"))
			f.bookmark("legion/K-1", "@")
			return rolePushSends("legion/K-1", f.read("@"), "implementer", impl)
		}},
		// The adoption is that metaedit (shim/commands.go adoptionArgs).
		{name: "author set by the other role's metaedit", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			c := f.commitOn(impl, impl, "main", rolePushMessage("work", "implementer"))
			f.jj(rev, "metaedit", "--update-author", "-r", c.commit)
			c = f.read(c.change)
			f.bookmark("legion/K-1", c.commit)
			return rolePushSends("legion/K-1", c, "implementer", impl)
		}},
		{name: "an earlier session of the pushing role", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			c := f.commitOn(rev, impl, "main", "work\n\nOmp-Session: 019a0000-0000-7000-8000-000000000001\nLegion-Role: implementer\n")
			f.bookmark("legion/K-1", c.commit)
			return rolePushSends("legion/K-1", c, "implementer", impl)
		}},
		{name: "the other role's trailer, the pushing role's author", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			c := f.commitOn(impl, impl, "main", rolePushMessage("review work", "reviewer"))
			f.bookmark("legion/K-1", c.commit)
			return rolePushSends("legion/K-1", c, "reviewer", rev)
		}},
		// The implementer's metaedit runs first, in claim.Roles' order, and rebases the reviewer
		// commit above it, which the read before the reviewer's metaedit finds.
		{name: "a chain of both roles' commits", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			a := f.commitOn(impl, impl, "main", rolePushMessage("A", "reviewer"))
			b := f.commitOn(rev, rev, a.commit, rolePushMessage("B", "implementer"))
			c := f.commitOn(impl, impl, b.commit, rolePushMessage("C", "reviewer"))
			f.bookmark("legion/K-1", c.commit)
			return rolePushWant{
				refs: []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{
					{change: c.change, author: rev}, {change: b.change, author: impl}, {change: a.change, author: rev},
				}}},
				rewrites: []rolePushRewrite{
					{role: "implementer", author: impl, change: b.change, old: b.commit},
					{role: "reviewer", author: rev, change: a.change, old: a.commit},
					{role: "reviewer", author: rev, change: c.change, old: c.commit, rebased: true},
				},
				metaedits: 2,
			}
		}},
		// jj's trailers are the description's last paragraph, so the squashed commit's role is
		// the one its source's trailer names, and its author the destination's.
		{name: "a squash of two described commits", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			p := f.commitOn(rev, rev, "main", rolePushMessage("P", "reviewer"))
			s := f.commitOn(impl, impl, p.commit, rolePushMessage("S", "implementer"))
			f.jj(impl, "squash", "--from", s.commit, "--into", p.commit, "--config", `ui.editor="true"`)
			squashed := f.read(p.change)
			f.bookmark("legion/K-1", squashed.commit)
			return rolePushSends("legion/K-1", squashed, "implementer", impl)
		}},
		{name: "a trailer and an empty author", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			c := f.commitOn(rolePushNobody, rolePushNobody, "main", rolePushMessage("work", "implementer"))
			f.bookmark("legion/K-1", c.commit)
			return rolePushSends("legion/K-1", c, "implementer", impl)
		}},
		// The other role's commit, whose one trailer block holds two Legion-Role keys: jj reads
		// both, in order, and the step takes the first, the pushing role's.
		{name: "one trailer block with two Legion-Role keys, the first the pushing role's", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			c := f.commitOn(rev, rev, "main", "work\n\nLegion-Role: implementer\nLegion-Role: reviewer\n")
			f.bookmark("legion/K-1", c.commit)
			return rolePushSends("legion/K-1", c, "implementer", impl)
		}},
		{name: "the push's own --config user.email", bothForms: true, args: []string{"git", "push", "--bookmark", "legion/K-1", "--config", "user.name=Another Name", "--config", "user.email=another@example.invalid"},
			setup: func(f *rolePushFixture) rolePushWant {
				c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
				f.bookmark("legion/K-1", c.commit)
				return rolePushSends("legion/K-1", c, "implementer", impl)
			}},
		{name: "a divergent change, each commit on a tracked bookmark, --tracked", args: []string{"git", "push", "--tracked"}, setup: func(f *rolePushFixture) rolePushWant {
			f.track("legion/K-1")
			f.track("legion/K-2")
			x := f.commitOn(rev, rev, "main", "x\n")
			f.jj(rev, "new", "main")
			d1, d2 := f.diverge(x, rolePushMessage("D1", "implementer"), rolePushMessage("D2", "reviewer"))
			f.jj(impl, "metaedit", "--update-author", "-r", d2.commit)
			d2 = f.titled(x.change, "D2")
			f.bookmark("legion/K-1", d1.commit)
			f.bookmark("legion/K-2", d2.commit)
			return rolePushWant{
				refs: []rolePushRef{
					{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: x.change, author: impl}}},
					{ref: "refs/heads/legion/K-2", commits: []rolePushCommit{{change: x.change, author: rev}}},
				},
				rewrites: []rolePushRewrite{
					{role: "implementer", author: impl, change: x.change, old: d1.commit},
					{role: "reviewer", author: rev, change: x.change, old: d2.commit},
				},
			}
		}},
		{name: "one identity for both roles", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			one := rolePushIdentity{"legion[bot]", "100000003+legion[bot]@users.noreply.github.com"}
			table := `{"implementer":{"name":"legion[bot]","email":"100000003+legion[bot]@users.noreply.github.com"},"reviewer":{"name":"legion[bot]","email":"100000003+legion[bot]@users.noreply.github.com"}}`
			f.table = &table
			x := f.commitOn(one, one, "main", rolePushMessage("X", "implementer"))
			y := f.commitOn(rev, one, x.commit, rolePushMessage("Y", "reviewer"))
			f.bookmark("legion/K-1", y.commit)
			return rolePushWant{
				by: one,
				refs: []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{
					{change: y.change, author: one}, {change: x.change, author: one, commit: x.commit},
				}}},
				rewrites: []rolePushRewrite{{role: "reviewer", author: one, change: y.change, old: y.commit}},
				same:     map[string]rolePushIdentity{x.commit: one},
			}
		}},
	}
	// emptyWaiting is @ on main, made and described with role's trailer under no identity, so with
	// an empty author, under legion/K-1, with a write that no command has snapshotted. The dry run
	// refuses @, which has no author, so the step takes the push's one snapshot first, which fills
	// @'s author with the pushing role's identity, as jj alone's push would, and takes the write: the
	// step then sets the author of @ only where role is the other role, and the remote holds @ with
	// role's identity and the write.
	emptyWaiting := func(role string, author rolePushIdentity) func(f *rolePushFixture) rolePushWant {
		return func(f *rolePushFixture) rolePushWant {
			f.jj(rolePushNobody, "new", "main")
			f.jj(rolePushNobody, "describe", "-m", rolePushMessage("work in @", role))
			f.bookmark("legion/K-1", "@")
			c := f.read("@")
			f.writeFile(filepath.Join(f.repo, "waiting.txt"), rolePushWaitingContent)
			want := rolePushWant{refs: []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{
				{change: c.change, author: author, file: "waiting.txt", content: rolePushWaitingContent},
			}}}}
			if author != impl {
				want.rewrites = []rolePushRewrite{{role: role, author: author, change: c.change, old: c.commit, snapshotted: true}}
			}
			return want
		}
	}
	rows = append(rows,
		rolePushRow{name: "@ with the pushing role's trailer, an empty author and a waiting write", args: rolePushBookmark, setup: emptyWaiting("implementer", impl)},
		rolePushRow{name: "@ with the other role's trailer, an empty author and a waiting write", args: rolePushBookmark, setup: emptyWaiting("reviewer", rev)},
	)
	for _, rewrite := range []bool{true, false} {
		for _, form := range rolePushWaitingForms {
			rows = append(rows, rolePushWaitingRow(form, rewrite))
		}
	}
	return rows
}

// Each commit that a role pushes, and that has a Legion-Role trailer naming a role of the table,
// reaches the remote with that role's identity as its author, whatever role made the change, set
// its author last or pushes it, in each state rolePushAuthorRows makes. The step prints one line
// for each commit it rewrote, and leaves no change divergent. @ with an empty author and a write
// that no command has snapshotted gets the pushing role's identity from the step's one snapshot,
// as from jj alone's push, and the other role's from the step's rewrite after it, where its
// trailer names that role. Where the step took its snapshot, the real push runs with
// --ignore-working-copy as its first word, and takes no snapshot of its own; elsewhere it carries
// that flag as the role wrote it (a recording jj ahead of the real one sees it).
func TestThePushSetsEachCommitsAuthorFromItsRoleTrailer(t *testing.T) {
	t.Parallel()
	rolePushRun(t, rolePushAuthorRows())
}

// rolePushFormRows are the forms of a push, each sending @-, a commit the other role made and the
// implementer described.
func rolePushFormRows() []rolePushRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	work := func(f *rolePushFixture) rolePushChange {
		return f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
	}
	onBookmark := func(f *rolePushFixture) rolePushWant {
		c := work(f)
		f.bookmark("legion/K-1", c.commit)
		return rolePushSends("legion/K-1", c, "implementer", impl)
	}
	// onTracked is onBookmark of a bookmark origin already holds: the push with no argument and -r
	// send a bookmark that tracks a remote one, and jj alone refuses to make one for -r.
	onTracked := func(f *rolePushFixture) rolePushWant {
		f.track("legion/K-1")
		return onBookmark(f)
	}
	underChange := func(f *rolePushFixture) rolePushWant {
		c := work(f)
		return rolePushSends("push-"+c.change[:12], c, "implementer", impl)
	}
	named := func(f *rolePushFixture) rolePushWant {
		return rolePushSends("nm", work(f), "implementer", impl)
	}
	// two is a second bookmark of the other role's beside the implementer's, both new or both
	// tracked.
	two := func(tracked bool) func(f *rolePushFixture) rolePushWant {
		return func(f *rolePushFixture) rolePushWant {
			if tracked {
				f.track("legion/K-1")
				f.track("legion/K-2")
			}
			other := f.commitOn(impl, impl, "main", rolePushMessage("review work", "reviewer"))
			f.bookmark("legion/K-2", other.commit)
			c := work(f)
			f.bookmark("legion/K-1", c.commit)
			want := rolePushSends("legion/K-1", c, "implementer", impl)
			sent := rolePushSends("legion/K-2", other, "reviewer", rev)
			want.refs = append(want.refs, sent.refs...)
			want.rewrites = append(want.rewrites, sent.rewrites...)
			return want
		}
	}
	// byID is setup's want with the one line of an argument that names the commit by its id: the
	// step's rewrite hid that commit, so the real push gets the id of the one that replaced it.
	byID := func(setup func(f *rolePushFixture) rolePushWant) func(f *rolePushFixture) rolePushWant {
		return func(f *rolePushFixture) rolePushWant {
			want := setup(f)
			want.ids = []rolePushIDs{rolePushOneIDLine(want.rewrites[0].old)}
			return want
		}
	}
	return []rolePushRow{
		{name: "--bookmark", args: rolePushBookmark, setup: onBookmark},
		{name: "--all, and a new bookmark of the other role", args: []string{"git", "push", "--all"}, setup: two(false)},
		{name: "--tracked, and a tracked bookmark of the other role", args: []string{"git", "push", "--tracked"}, setup: two(true)},
		{name: "no argument, a tracked bookmark", args: []string{"git", "push"}, setup: onTracked},
		// jj's releases differ on a push with no argument of a bookmark nothing tracks: the row
		// expects what the jj under test does alone.
		{name: "no argument, a new bookmark", args: []string{"git", "push"}, setup: func(f *rolePushFixture) rolePushWant {
			want := onBookmark(f)
			code, remotes := f.aloneSends([]string{"git", "push"})
			if code == 0 && len(remotes) > 0 {
				return want
			}
			c := want.rewrites[0]
			return rolePushWant{
				exit: code, cannotRead: rolePushMayNotRead,
				refs: []rolePushRef{{ref: "refs/heads/legion/K-1", absent: true}},
				same: map[string]rolePushIdentity{c.old: rev},
			}
		}},
		{name: "--change <change id>", args: []string{"git", "push", "--change", "{change:@-}"}, setup: underChange},
		// The revisions family's check that the two forms agree (rolePushRow.bothForms).
		{name: "--change <commit id>", bothForms: true, args: []string{"git", "push", "--change", "{commit:@-}"}, setup: byID(underChange)},
		{name: "--named nm=<commit id>", args: []string{"git", "push", "--named", "nm={commit:@-}"}, setup: byID(named)},
		{name: "--revisions <commit id>", args: []string{"git", "push", "--revisions", "{commit:@-}"}, setup: byID(onTracked)},
		{name: "--change=<commit id>", args: []string{"git", "push", "--change={commit:@-}"}, setup: byID(underChange)},
		{name: "-c<commit id>", args: []string{"git", "push", "-c{commit:@-}"}, setup: byID(underChange)},
		{name: "--named=nm=<commit id>", args: []string{"git", "push", "--named=nm={commit:@-}"}, setup: byID(named)},
		{name: "-r<commit id>", args: []string{"git", "push", "-r{commit:@-}"}, setup: byID(onTracked)},
		// The push's own --config in front of `git push`, which jj keeps where no --config
		// follows the pair, defines the alias that -r names: the step's dry run reads it too.
		{name: "-r <an alias of the push's own --config in front of git push>", bothForms: true, args: []string{"--config", `revset-aliases.sendme="@-"`, "git", "push", "-r", "sendme"}, setup: onTracked},
		// A `--` ends the options: the step's dry run puts its own words in front of it.
		{name: "--bookmark, then --", bothForms: true, args: []string{"git", "push", "--bookmark", "legion/K-1", "--"}, setup: onBookmark},
		// A tag makes its commit immutable, so the step leaves it as it is and the push goes on. A
		// jj with no --tag refuses the push itself, and the row expects that refusal.
		{name: "--tag", args: []string{"git", "push", "--tag", "v1"}, setup: func(f *rolePushFixture) rolePushWant {
			c := work(f)
			f.jj(impl, "tag", "set", "v1", "-r", c.commit)
			if code, _ := f.aloneSends([]string{"git", "push", "--tag", "v1"}); code != 0 {
				return rolePushWant{
					exit: code, cannotRead: rolePushMayNotRead,
					refs: []rolePushRef{{ref: "refs/tags/v1", absent: true}},
					same: map[string]rolePushIdentity{c.commit: rev},
				}
			}
			return rolePushWant{
				refs: []rolePushRef{{ref: "refs/tags/v1", commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}}}},
				same: map[string]rolePushIdentity{c.commit: rev},
			}
		}},
	}
}

// rolePushTwoRemoteFirsts are the first pushes of the two-remote row: to fork alone, and, where
// the jj under test takes two --remote, to both.
var rolePushTwoRemoteFirsts = []struct {
	name string
	args []string
}{
	{name: "--remote fork", args: []string{"git", "push", "--bookmark", "legion/K-1", "--remote", "fork"}},
	{name: "--remote origin --remote fork", args: []string{"git", "push", "--bookmark", "legion/K-1", "--remote", "origin", "--remote", "fork"}},
}

// rolePushTwoRemoteSecond is the two-remote row's second push.
var rolePushTwoRemoteSecond = []string{"git", "push", "--bookmark", "legion/K-1", "--remote", "origin"}

// Each form of a push sends the commits as the step left them: the remote's commits have their
// trailers' authors, and no change is divergent. A commit id the step's rewrite hid is the
// rewritten commit in the real push, in every spelling, and the step prints the one line of that
// argument's id; each commit-id row names @-, a commit other than @. A -r of an alias that the
// push's own --config in front of `git push` defines sends the rewritten commit too, as the step's
// dry run reads that --config as the push does, and so does a push that ends with `--`, in front
// of which the dry run puts its own words. Where the step took its snapshot, the real push runs
// with --ignore-working-copy as its first word, and elsewhere with that flag as the role wrote it
// (a recording jj ahead of the real one sees it). A push to two remotes sends the same rewritten
// commit to both, leaves the commit one remote already holds as it is, and forces neither.
func TestEveryPushFormSendsTheRewrittenCommit(t *testing.T) {
	t.Parallel()
	rolePushRun(t, rolePushFormRows())
	for _, first := range rolePushTwoRemoteFirsts {
		t.Run("two remotes, "+first.name, func(t *testing.T) {
			t.Parallel()
			rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
				rolePushTwoRemotes(t, newRolePushFixture(t, form, ""), first.args)
			})
		})
	}
}

// rolePushTwoRemotes pushes legion/K-1, whose older commit origin holds and whose newer commit no
// remote holds, with first, then to origin alone. jj's releases differ on two --remote, so the
// first push expects what the jj under test does alone.
func rolePushTwoRemotes(t *testing.T, f *rolePushFixture, first []string) {
	impl, rev := rolePushImplementer, rolePushReviewer
	f.addRemote("fork")
	a := f.commitOn(rev, impl, "main", rolePushMessage("A", "implementer"))
	f.bookmark("legion/K-1", a.commit)
	f.jj(impl, "git", "push", "--bookmark", "legion/K-1")
	b := f.commitOn(rev, impl, a.commit, rolePushMessage("B", "implementer"))
	f.bookmark("legion/K-1", b.commit)
	f.jj(impl, "bookmark", "track", "legion/K-1", "--remote=fork")
	held := rolePushCommit{change: a.change, author: rev, commit: a.commit}
	code, remotes := f.aloneSends(first)
	want := rolePushWant{exit: code}
	if code != 0 {
		want.cannotRead = rolePushMayNotRead
	} else {
		want.rewrites = []rolePushRewrite{{role: "implementer", author: impl, change: b.change, old: b.commit}}
	}
	for _, remote := range []string{"origin", "fork"} {
		ref := rolePushRef{remote: remote, ref: "refs/heads/legion/K-1"}
		switch {
		case code == 0 && slices.Contains(remotes, remote):
			ref.commits = []rolePushCommit{{change: b.change, author: impl}, held}
		case remote == "origin":
			ref.commits = []rolePushCommit{held}
		default:
			ref.absent = true
		}
		want.refs = append(want.refs, ref)
	}
	f.pushAndCheck(first, want)
	if t.Failed() {
		return
	}
	sent, _ := f.remoteCommits(f.remote("fork"), "refs/heads/legion/K-1")
	newer := rolePushCommit{change: b.change, author: impl}
	if len(sent) == 2 {
		newer.commit = sent[0].id
	}
	second := rolePushWant{refs: []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{newer, held}}}}
	if code != 0 {
		second.rewrites = []rolePushRewrite{{role: "implementer", author: impl, change: b.change, old: b.commit}}
	}
	f.pushAndCheck(rolePushTwoRemoteSecond, second)
}

// rolePushRevsetRows are the pushes whose revision argument names a commit by its commit id, inside
// a revset, as the commit id of @, or as the commit id of @ that a rewrite of @ hid, each of
// commits that the other role's jj new made and the implementer described, each pushed by the
// implementer.
func rolePushRevsetRows() []rolePushArgsRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	work := func(f *rolePushFixture, parent, title string) rolePushChange {
		return f.commitOn(rev, impl, parent, rolePushMessage(title, "implementer"))
	}
	// line is the one line of argument, which the real push gets as the ids of the n commits it
	// named before the step, in one word.
	line := func(argument string, n int) []rolePushIDs {
		return []rolePushIDs{{argument: argument, commits: n, words: 1, bound: rolePushRevisionBound}}
	}
	return []rolePushArgsRow{
		{name: "-c <C1> | <C2>, two commits on main", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c1, c2 := work(f, "main", "work 1"), work(f, "main", "work 2")
			want := rolePushSends("push-"+c1.change[:12], c1, "implementer", impl)
			sent := rolePushSends("push-"+c2.change[:12], c2, "implementer", impl)
			want.refs = append(want.refs, sent.refs...)
			want.rewrites = append(want.rewrites, sent.rewrites...)
			want.ids = line(c1.commit+" | "+c2.commit, 2)
			return []string{"git", "push", "-c", c1.commit + " | " + c2.commit}, want
		}},
		{name: "-c present(<the first 12 hex digits of C>)", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c := work(f, "main", "work")
			want := rolePushSends("push-"+c.change[:12], c, "implementer", impl)
			want.ids = line("present("+c.commit[:12]+")", 1)
			return []string{"git", "push", "-c", "present(" + c.commit[:12] + ")"}, want
		}},
		// The step rewrites C alone, and its metaedit rebases D onto the new C: the D of the real
		// push is the rebased D, whose parent is the new C.
		{name: "--named nm=<D>-, D a commit on C", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c := work(f, "main", "C")
			d := work(f, c.commit, "D")
			want := rolePushSends("nm", c, "implementer", impl)
			want.ids = line(d.commit+"-", 1)
			return []string{"git", "push", "--named", "nm=" + d.commit + "-"}, want
		}},
		// D is a number that starts the id of one commit of the repository, hidden ones included:
		// a commit of C's change that a describe hid. jj reads the argument with D a number, which
		// names C alone among the commits a metaedit can rewrite, so the step follows C, and the
		// real push gets the ids of the commits the argument named before the step, the commit the
		// step made of C in place of C, and each ancestor of C that D, as a depth, reaches.
		{name: "-r ancestors(<C>, <D>), D a number that starts the id of a hidden commit of C's change", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			f.track("legion/K-1")
			c := work(f, "main", "work")
			f.bookmark("legion/K-1", c.commit)
			c, depth := f.decimalPrefixed(c, impl, rolePushMessage("work", "implementer"), true)
			argument := "ancestors(" + c.commit + ", " + depth + ")"
			want := rolePushSends("legion/K-1", c, "implementer", impl)
			want.ids = line(argument, len(strings.Fields(f.jj(impl, "--ignore-working-copy", "log", "--no-graph", "-r", argument, "-T", `commit_id ++ "\n"`))))
			return []string{"git", "push", "-r", argument}, want
		}},
		// @ beside C1, a commit on main, with a write in @ that no command has snapshotted: the
		// step's one snapshot takes the write before its rewrite of @, and the remote gets both
		// changes with the implementer's author, @'s with the write.
		{name: "-c @ | <C1>, @ with a waiting write", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c1 := work(f, "main", "work 1")
			_, want := f.waiting(rolePushWaitingChange, true)
			sent := rolePushSends("push-"+c1.change[:12], c1, "implementer", impl)
			want.refs = append(want.refs, sent.refs...)
			want.rewrites = append(want.rewrites, sent.rewrites...)
			want.ids = line("@ | "+c1.commit, 2)
			return []string{"git", "push", "-c", "@ | " + c1.commit}, want
		}},
		// The commit id of @, with a write in @ that no command has snapshotted: the step's one
		// snapshot takes the write and hides the commit the id names, which stands for @, its
		// change's one visible commit, so the real push names it as @ after the step's rewrite of @,
		// and the remote gets @'s change with the write and the implementer's author.
		{name: "-c <the commit id of @>, @ with a waiting write", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			w, want := f.waiting(rolePushWaitingChange, true)
			want.ids = line(w.commit, 1)
			return []string{"git", "push", "-c", w.commit}, want
		}},
		// The commit id @ had before a describe of @ that took no snapshot hid it, with a write in @
		// that no command has snapshotted: the hidden commit stands for @, its change's one visible
		// commit, before the step's one snapshot and after it, so the real push names it as @ after
		// the step's rewrite of @, and the remote gets @'s change with the write and the
		// implementer's author.
		{name: "-c <the commit id of a hidden predecessor of @>, @ with a waiting write", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			hidden, want := f.waiting(rolePushWaitingChange, true)
			f.jj(impl, "--ignore-working-copy", "describe", "-m", rolePushMessage("work in @, described again", "implementer"))
			want.ids = line(hidden.commit, 1)
			return []string{"git", "push", "-c", hidden.commit}, want
		}},
	}
}

// A commit id inside a revision argument's revset, each of `C1 | C2`, a prefix of C in present(),
// the D of `D-`, the C of `ancestors(C, D)`, C1 beside @, and the commit id of @ on its own, is the
// rewritten commit in the real push, which gets the ids of the commits the argument named before
// the step, each commit the step rewrote as the commit it made of it and @ as @: the remote's
// commits have their trailers' authors, the step prints each rewrite and the one line of the
// argument's ids, which counts those commits in one word, and no change is divergent. The D of
// `ancestors(C, D)` is a number that starts the id of a hidden commit whose change has one visible
// commit, and jj reads it as a number. A write in @ that no command has snapshotted reaches the
// remote, and so it does where the commit id is one @ had before a rewrite of @ hid it. `::C` has
// a test of its own, TestARevisionWhoseIDsPassALoweredBoundGetsItsPrimaryForm.
func TestACommitIDInsideARevsetSendsTheRewrittenCommit(t *testing.T) {
	t.Parallel()
	rolePushRunArgs(t, "", rolePushRevsetRows())
}

// rolePushFollowedRows are the pushes whose revision argument names a commit the step rewrites by
// what jj itself moves to the commit the rewrite makes: a bookmark, a change id, and @ with a write
// no command has snapshotted (rolePushWaitingRow), each of a commit that the other role's jj new
// made and the implementer described, each pushed by the implementer.
func rolePushFollowedRows() []rolePushArgsRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	work := func(f *rolePushFixture) rolePushChange {
		return f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
	}
	// asWritten is args and want, and what the real push's words must be (pushedAfterSnapshot).
	asWritten := func(f *rolePushFixture, args []string, want rolePushWant) ([]string, rolePushWant) {
		want.pushed = f.pushedAfterSnapshot(args)
		return args, want
	}
	waiting := rolePushWaitingRow(rolePushWaitingChange, true)
	return []rolePushArgsRow{
		{name: "-c <bookmark>, the bookmark on C", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c := work(f)
			f.bookmark("mybookmark", c.commit)
			return asWritten(f, []string{"git", "push", "-c", "mybookmark"}, rolePushSends("push-"+c.change[:12], c, "implementer", impl))
		}},
		{name: "-c <the change id of C>", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c := work(f)
			return asWritten(f, []string{"git", "push", "-c", c.change}, rolePushSends("push-"+c.change[:12], c, "implementer", impl))
		}},
		{name: waiting.name, setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			return asWritten(f, waiting.args, waiting.setup(f))
		}},
	}
}

// A revision argument that names a commit the step rewrites by what jj itself moves to the commit
// the rewrite makes, a bookmark, a change id, or @ with a write no command has snapshotted, names
// that commit after the step as it is: a recording jj ahead of the real one sees the real push run
// with the role's own words, every byte, after the --ignore-working-copy that the step's snapshot
// gives it, and the remote gets the rewritten commit with its trailer's author, @'s with the
// write, and no change is divergent.
func TestARevisionJJMovesToTheRewrittenCommitGoesToThePushAsWritten(t *testing.T) {
	t.Parallel()
	rolePushRunArgs(t, rolePushRecords, rolePushFollowedRows())
}

// `jj git push -c latest(<C>, <D>)`, where D is a number that the id of C, the commit the push
// sends, alone starts with, so jj could also read D as a commit id. The argument names C, a commit
// the step rewrites, so the real push sends the rewritten C: the remote's commit has its trailer's
// author, and -c makes no bookmark on the old, hidden C, so no change is divergent.
func TestACommitIDBesideARefusedNumberSendsTheRewrittenCommit(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, "")
		message := rolePushMessage("work", "implementer")
		c := f.commitOn(rev, impl, "main", message)
		c, count := f.decimalPrefixed(c, impl, message, false)
		want := rolePushSends("push-"+c.change[:12], c, "implementer", impl)
		want.ids = []rolePushIDs{{argument: "latest(" + c.commit + ", " + count + ")", commits: 1, words: 1, bound: rolePushRevisionBound}}
		f.pushAndCheck([]string{"git", "push", "-c", "latest(" + c.commit + ", " + count + ")"}, want)
	})
}

// jj reads a string literal standing alone in a revset as a symbol, as it reads the bare word: a
// quoted commit id, `"<C>"` or `'<C>'`, names the commit C. `jj git push -c "<C>"` of a commit the
// step rewrites therefore names a commit the step rewrote, and the real push gets the new C: the
// remote's commit has its trailer's author, and -c makes no bookmark on the old, hidden C, so no
// change is divergent.
func TestAQuotedCommitIDSendsTheRewrittenCommit(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	for _, quote := range []string{`"`, `'`} {
		t.Run(quote, func(t *testing.T) {
			t.Parallel()
			rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, "")
				c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
				want := rolePushSends("push-"+c.change[:12], c, "implementer", impl)
				want.ids = []rolePushIDs{{argument: quote + c.commit + quote, commits: 1, words: 1, bound: rolePushRevisionBound}}
				f.pushAndCheck([]string{"git", "push", "-c", quote + c.commit + quote}, want)
			})
		})
	}
}

// rolePushRefusedRows are the pushes that make a bookmark of their own, -c and --named, of a
// commit id: of @-, and of @- whose change is divergent (rolePushDivergentRefusedRow).
func rolePushRefusedRows() []rolePushRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	work := func(f *rolePushFixture) rolePushChange {
		return f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
	}
	return []rolePushRow{
		{name: "-c <commit id>", args: []string{"git", "push", "-c", "{commit:@-}"}, setup: func(f *rolePushFixture) rolePushWant {
			c := work(f)
			return rolePushSends("push-"+c.change[:12], c, "implementer", impl)
		}},
		{name: "--named nm=<commit id>", args: []string{"git", "push", "--named", "nm={commit:@-}"}, setup: func(f *rolePushFixture) rolePushWant {
			return rolePushSends("nm", work(f), "implementer", impl)
		}},
		rolePushDivergentRefusedRow(),
	}
}

// rolePushDivergentRefusedRow is the push of -c with the commit id of @-, D1, one of the two
// commits of a divergent change, each with the reviewer as author and D1 with the implementer's
// trailer. The refused push's rewrite hides D1, whose change then has two visible commits: the
// retry names a hidden commit of a divergent change, which stands for itself, and the step
// rewrites it again.
func rolePushDivergentRefusedRow() rolePushRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	return rolePushRow{name: "-c <commit id> of a divergent change", args: []string{"git", "push", "-c", "{commit:@-}"}, setup: func(f *rolePushFixture) rolePushWant {
		x := f.commitOn(rev, rev, "main", "x\n")
		f.jj(rev, "new", "main")
		d1, _ := f.diverge(x, rolePushMessage("D1", "implementer"), rolePushMessage("D2", "reviewer"))
		f.jj(impl, "new", d1.commit)
		want := rolePushSends("push-"+x.change[:12], d1, "implementer", impl)
		want.diverges = true
		return want
	}}
}

// rolePushFixedTimestamp is the one JJ_TIMESTAMP of a test that fixes it
// (rolePushFixture.timestamp).
const rolePushFixedTimestamp = "2026-11-20T10:00:00+00:00"

// refusedPush pushes args through the form's jj as the implementer to an origin whose
// pre-receive hook refuses every push, fails the test where the push exits 0, removes the hook,
// and answers what the push printed on standard error.
func (f *rolePushFixture) refusedPush(args []string) string {
	f.t.Helper()
	hook := filepath.Join(f.bare, "hooks", "pre-receive")
	f.script(filepath.Dir(hook), "pre-receive", "#!/bin/sh\necho 'legion test: this remote refuses every push' >&2\nexit 1\n")
	code, _, stderr := f.push(rolePushImplementer, args...)
	if code == 0 {
		f.t.Fatalf("the push the remote refuses exited 0:\n%s", stderr)
	}
	if err := os.Remove(hook); err != nil {
		f.t.Fatal(err)
	}
	return stderr
}

// A real push that the remote refuses, of -c or --named with a commit id, leaves no bookmark of
// its own: `jj bookmark list --all` names neither push-<change> nor nm. Once the remote takes it,
// the same command, with the commit id the role first gave, sends the trailer's author and leaves
// no change divergent; where that commit's change is divergent, it sends the rewritten commit with
// the trailer's author. The refused push's rewrite hid that commit, so the retry's step prints the
// one line of that commit id, of one commit in one word. That retry, of a divergent change's
// commit, runs in a later second than the refused push's rewrite;
// TestASameSecondRetrySendsTheCommitTheRefusedPushMade runs it in the same second.
func TestARefusedPushLeavesNoBookmarkAndItsRetrySends(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushRefusedRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, "")
				want := row.setup(f)
				args := f.resolve(row.args)
				name := strings.TrimPrefix(want.refs[0].ref, "refs/heads/")
				f.refusedPush(args)
				for line := range strings.SplitSeq(f.jj(rolePushImplementer, "bookmark", "list", "--all"), "\n") {
					if bookmark, _, _ := strings.Cut(line, ":"); bookmark == name || strings.HasPrefix(bookmark, name+" ") {
						t.Errorf("jj bookmark list --all names %s after the refused push: %q", name, line)
					}
				}
				if want.diverges {
					// The retry runs in a later second than the refused push's rewrite: the
					// step's rewrite of the hidden commit makes a new commit, which the change
					// has beside the two.
					rolePushNextSecond()
				}
				want.others = true
				want.ids = []rolePushIDs{rolePushOneIDLine(want.rewrites[0].old)}
				want.rewrites = nil
				divergent := f.divergence()
				code, _, stderr := f.push(rolePushImplementer, args...)
				f.check(want, args, code, stderr, divergent, 0)
			})
		})
	}
}

// rolePushNextSecond waits until the wall clock is in the next second: a commit's timestamps are
// whole seconds.
func rolePushNextSecond() {
	now := time.Now()
	time.Sleep(now.Truncate(time.Second).Add(time.Second).Sub(now))
}

// The push of rolePushDivergentRefusedRow, refused, and its retry in the second of the refused
// push's rewrite (one fixed JJ_TIMESTAMP): the step's rewrite of D1, the hidden commit the retry
// names, would make the commit the refused push's rewrite made of D1, which jj refuses to make
// again. The step takes that commit, prints its rewrite of D1 and the one line of D1's id, and
// sends it with the implementer's author, and makes no change more divergent.
func TestASameSecondRetrySendsTheCommitTheRefusedPushMade(t *testing.T) {
	t.Parallel()
	row := rolePushDivergentRefusedRow()
	rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, "")
		f.timestamp = rolePushFixedTimestamp
		want := row.setup(f)
		args := f.resolve(row.args)
		d1 := want.rewrites[0].old
		printed := rolePushStepLines(f.refusedPush(args)).rewrites
		if len(printed) != 1 || printed[0].old != d1 {
			t.Fatalf("the refused push printed the rewrites %v, want one, of %s", printed, d1)
		}
		want.refs[0].commits[0].commit = printed[0].new
		want.diverges = false
		want.ids = []rolePushIDs{rolePushOneIDLine(d1)}
		divergent := f.divergence()
		code, _, stderr := f.push(rolePushImplementer, args...)
		f.check(want, args, code, stderr, divergent, 0)
	})
}

// A retry after a `jj op restore` to the operation before the refused push, in the second of that
// push's rewrite (one fixed JJ_TIMESTAMP): the restore puts the bookmark back on the commit the
// role made, C, and hides the commit the refused push's rewrite made of it, which the step's
// rewrite of C would make again, and jj refuses to make a commit that exists. No commit of the
// push stands for that one, C being visible, so the push stops before the real push runs: it
// prints jj's own error, which names `Newly-created commit <made> already exists`, and exits with
// jj's status, the one jj alone exits with for the same metaedit in the same second. It prints no
// line `legion jj: could not set the author: …`, the recording jj sees no real push, origin still
// holds no legion/K-1, as the refused push left it, the bookmark stays on C with its author as it
// is, and no change is made divergent. The retry adds no operation but one `snapshot working copy`
// at most, the step's snapshot before that metaedit: jj refused the metaedit whole, and no push
// ran.
func TestARetryAfterARestoreStopsThePushWithJJsError(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, rolePushRecords)
		f.timestamp = rolePushFixedTimestamp
		c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
		f.bookmark("legion/K-1", c.commit)
		before := f.op()
		printed := rolePushStepLines(f.refusedPush(rolePushBookmark)).rewrites
		if len(printed) != 1 || printed[0].old != c.commit {
			t.Fatalf("the refused push printed the rewrites %v, want one, of %s", printed, c.commit)
		}
		made := printed[0].new
		f.forgetCalls()
		f.jj(impl, "op", "restore", before)
		if on := f.read("legion/K-1").commit; on != c.commit {
			t.Fatalf("legion/K-1 is on %s after jj op restore, want %s, the commit the role made", on, c.commit)
		}
		if hidden := f.jj(impl, "--ignore-working-copy", "log", "-r", made, "--no-graph", "-T", "hidden"); hidden != "true" {
			t.Fatalf("%s, the refused push's rewrite, is hidden %q after jj op restore, want true", made, hidden)
		}
		refusal := "Newly-created commit " + made + " already exists"
		aloneCode, _, aloneErr := f.alone(impl, "--ignore-working-copy", "metaedit", "--update-author", "-r", c.commit)
		if aloneCode == 0 || !strings.Contains(aloneErr, refusal) {
			t.Fatalf("jj alone's metaedit --update-author of %s exited %d and printed:\n%s\nwant jj's refusal %q", c.commit, aloneCode, aloneErr, refusal)
		}
		retry := f.op()
		divergent := f.divergence()
		code, _, stderr := f.push(impl, rolePushBookmark...)
		f.check(rolePushWant{
			exit: aloneCode,
			refs: []rolePushRef{{ref: "refs/heads/legion/K-1", absent: true}},
			same: map[string]rolePushIdentity{c.commit: rev},
		}, rolePushBookmark, code, stderr, divergent, 0)
		if !strings.Contains(stderr, aloneErr) {
			t.Errorf("the push printed\n%s\nwant jj's own error, as jj alone prints it:\n%s", stderr, aloneErr)
		}
		if calls := f.calls(); slices.ContainsFunc(calls, rolePushCall.realPush) {
			t.Errorf("the recording jj ran %v, want no real push", calls)
		}
		if on := f.read("legion/K-1").commit; on != c.commit {
			t.Errorf("legion/K-1 is on %s after the push, want %s, where the restore left it", on, c.commit)
		}
		if ops := f.opsSince(retry); len(ops) > 1 || len(ops) == 1 && ops[0] != "snapshot working copy" {
			t.Errorf("the retry added the operations %q, newest first, want one snapshot of the working copy at most and nothing else", ops)
		}
	})
}

// rolePushGitCommit is the word of an unimported row's arguments that stands for the commit id of
// the commit the real git made.
const rolePushGitCommit = "{git commit}"

// rolePushUnimportedRows are the pushes that name, by its commit id, a commit the real git made in
// the colocated working copy and that no jj command has imported yet: each with the bookmark name
// it sends the commit under, "" for -c's own.
var rolePushUnimportedRows = []struct {
	name, ref string
	args      []string
}{
	{name: "--named nm=<commit id>", ref: "nm", args: []string{"git", "push", "--named", "nm=" + rolePushGitCommit}},
	{name: "-c <commit id>", args: []string{"git", "push", "-c", rolePushGitCommit}},
}

// A push that names, by its commit id, a commit that a git commit in the colocated working copy
// made and that jj has not imported yet sends it with the author its trailer names, the
// implementer's in place of git's own. The git commit moved git's HEAD off @'s first parent, so
// the step's comparison of git's HEAD with jj's finds that git moved it (spec v35 R1), and the
// step takes the push's one snapshot first, which imports the commit: the step then reads it, and
// prints no line of a dry run it cannot read (rolePushGitMade).
func TestAPushOfACommitJJHasNotImportedSendsIt(t *testing.T) {
	t.Parallel()
	rolePushGitMade(t, false, func(f *rolePushFixture) (string, []rolePushCommit, bool) {
		return f.gitCommitWrite(rolePushImplementer, "--quiet", "-m", rolePushMessage("work", "implementer")), nil, false
	})
}

// rolePushUntouchedRows are the commits a push leaves as they are, each beside a commit of the
// push's that the step may rewrite (pushed), so that the step runs.
func rolePushUntouchedRows() []rolePushRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	// pushed puts legion/K-1 on a new commit on parent that the other role made and the
	// implementer described, and answers the remote's ref after the push: that commit, over below.
	pushed := func(f *rolePushFixture, parent string, below ...rolePushCommit) rolePushRef {
		p := f.commitOn(rev, impl, parent, rolePushMessage("work", "implementer"))
		f.bookmark("legion/K-1", p.commit)
		return rolePushRef{ref: "refs/heads/legion/K-1", commits: append([]rolePushCommit{{change: p.change}}, below...)}
	}
	keeps := func(ref rolePushRef, kept map[string]rolePushIdentity) rolePushWant {
		return rolePushWant{refs: []rolePushRef{ref}, same: kept, others: true}
	}
	// divergent is a divergent change of two commits, each with the implementer's trailer and
	// the reviewer as author.
	divergent := func(f *rolePushFixture) (rolePushChange, rolePushChange) {
		x := f.commitOn(rev, rev, "main", "x\n")
		f.jj(rev, "new", "main")
		return f.diverge(x, rolePushMessage("D1", "implementer"), rolePushMessage("D2", "implementer"))
	}
	inPush := func(f *rolePushFixture, d1 rolePushChange) rolePushRef {
		f.bookmark("legion/K-1", d1.commit)
		return rolePushRef{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: d1.change}}}
	}
	return []rolePushRow{
		// The family's check that the two forms agree (rolePushRow.bothForms).
		{name: "no Legion-Role trailer", bothForms: true, args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			k := f.commitOn(rev, impl, "main", "work with no trailer\n")
			f.bookmark("legion/K-1", k.commit)
			return keeps(rolePushRef{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: k.change, author: rev, commit: k.commit}}},
				map[string]rolePushIdentity{k.commit: rev})
		}},
		{name: "a trailer that names a role with no identity", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			k := f.commitOn(rev, impl, "main", rolePushMessage("test work", "tester"))
			f.bookmark("legion/K-1", k.commit)
			return keeps(rolePushRef{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: k.change, author: rev, commit: k.commit}}},
				map[string]rolePushIdentity{k.commit: rev})
		}},
		{name: "a commit that a bookmark of the push's remote holds", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			a := f.commitOn(rev, impl, "main", rolePushMessage("held", "implementer"))
			f.bookmark("legion/K-1", a.commit)
			f.jj(impl, "git", "push", "--bookmark", "legion/K-1")
			return keeps(pushed(f, a.commit, rolePushCommit{change: a.change, author: rev, commit: a.commit}), map[string]rolePushIdentity{a.commit: rev})
		}},
		{name: "a commit that a bookmark of a second remote holds", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			f.addRemote("fork")
			a := f.commitOn(rev, impl, "main", rolePushMessage("held", "implementer"))
			f.bookmark("legion/K-1", a.commit)
			f.jj(impl, "git", "push", "--bookmark", "legion/K-1", "--remote", "fork")
			f.jj(impl, "bookmark", "track", "legion/K-1", "--remote=origin")
			return keeps(pushed(f, a.commit, rolePushCommit{change: a.change, author: rev, commit: a.commit}), map[string]rolePushIdentity{a.commit: rev})
		}},
		{name: "another bookmark the push does not send", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			o := f.commitOn(rev, impl, "main", rolePushMessage("other work", "implementer"))
			f.bookmark("other", o.commit)
			return keeps(pushed(f, "main"), map[string]rolePushIdentity{o.commit: rev})
		}},
		{name: "another workspace's @", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			ref := pushed(f, "main")
			second := filepath.Join(f.root, "second")
			f.jj(rev, "workspace", "add", "--name", "second", "-r", "main", second)
			f.writeFile(filepath.Join(second, "second.txt"), "the second workspace's work\n")
			f.jjIn(second, rev, "describe", "-m", rolePushMessage("second's work", "implementer"))
			w := f.read("second@")
			return keeps(ref, map[string]rolePushIdentity{w.commit: rev})
		}},
		{name: "a divergent change the push does not send", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			d1, d2 := divergent(f)
			return keeps(pushed(f, "main"), map[string]rolePushIdentity{d1.commit: rev, d2.commit: rev})
		}},
		{name: "a divergent change, the other commit on another bookmark", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			d1, d2 := divergent(f)
			f.bookmark("other", d2.commit)
			return keeps(inPush(f, d1), map[string]rolePushIdentity{d2.commit: rev})
		}},
		{name: "a divergent change, the other commit under a remote bookmark", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			d1, d2 := divergent(f)
			f.bookmark("other", d2.commit)
			f.jj(impl, "git", "push", "--bookmark", "other")
			return keeps(inPush(f, d1), map[string]rolePushIdentity{d2.commit: rev})
		}},
		{name: "a divergent change, the other commit on the default branch", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			d1, d2 := divergent(f)
			f.bookmark("main", d2.commit)
			f.jj(impl, "git", "push", "--bookmark", "main")
			return keeps(inPush(f, d1), map[string]rolePushIdentity{d2.commit: rev})
		}},
		{name: "a tagged commit in the push", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			tagged := f.commitOn(rev, impl, "main", rolePushMessage("tagged work", "implementer"))
			f.jj(impl, "tag", "set", "v1", "-r", tagged.commit)
			return keeps(pushed(f, tagged.commit, rolePushCommit{change: tagged.change, author: rev, commit: tagged.commit}), map[string]rolePushIdentity{tagged.commit: rev})
		}},
	}
}

// No commit outside the rule changes: a commit with no Legion-Role trailer or one naming a role
// with no identity, a commit a remote holds, a commit the push does not send (another bookmark's,
// another workspace's @, a divergent change's), and a commit jj does not let a command rewrite
// (a tag's) each keep their author and their commit id, and the push goes on.
func TestThePushChangesNoCommitOutsideIt(t *testing.T) {
	t.Parallel()
	rolePushRun(t, rolePushUntouchedRows())
}

// rolePushRedescribes is how many times, at most, a row's setup describes a commit again for a
// commit id of the shape it wants.
const rolePushRedescribes = 256

// redescribe describes c again as describer, with message's title followed by a count and the
// rest of message kept, until done holds of its commit id and of the commit ids of c that the
// describes hid, and answers c then. It fails the test past rolePushRedescribes describes.
func (f *rolePushFixture) redescribe(c rolePushChange, describer rolePushIdentity, message string, done func(commit string, hid []string) bool) rolePushChange {
	f.t.Helper()
	title, rest, _ := strings.Cut(message, "\n")
	var hid []string
	for count := 1; !done(c.commit, hid); count++ {
		if count > rolePushRedescribes {
			f.t.Fatalf("%d describes of change %s left its commit %s, whose id is not of the shape the row wants", rolePushRedescribes, c.change, c.commit)
		}
		hid = append(hid, c.commit)
		f.jj(describer, "describe", "-r", c.commit, "-m", title+" "+strconv.Itoa(count)+"\n"+rest)
		c = f.read(c.change)
	}
	return c
}

// shortIDPushed makes P, the commit a short-id row pushes: a commit on main that maker's jj new
// makes and describer's jj commit describes with message (commitOn), described again by describer
// until the first hex digit of its commit id is that of a commit of P's change that a describe
// hid, and of no other visible commit. It sets the repository's revsets.short-prefixes to none()
// first: jj resolves a prefix among the commits of that set (by default revsets.log, which holds
// P), and among every commit, hidden ones included, only where no single commit of that set has
// an id that starts with it. So jj, as for a commit that set does not hold, finds P's digit
// ambiguous as a prefix.
func (f *rolePushFixture) shortIDPushed(maker, describer rolePushIdentity, message string) rolePushChange {
	f.t.Helper()
	f.jj(rolePushImplementer, "config", "set", "--repo", "revsets.short-prefixes", "none()")
	return f.redescribe(f.commitOn(maker, describer, "main", message), describer, message, func(commit string, hid []string) bool {
		first := commit[:1]
		if !slices.ContainsFunc(hid, func(h string) bool { return strings.HasPrefix(h, first) }) {
			return false
		}
		for _, visible := range strings.Fields(f.jj(rolePushImplementer, "--ignore-working-copy", "log", "-r", "all()", "--no-graph", "-T", `commit_id ++ "\n"`)) {
			if visible != commit && strings.HasPrefix(visible, first) {
				return false
			}
		}
		return true
	})
}

// decimalPrefixed describes c again as describer (redescribe), in a repository whose
// revsets.short-prefixes is none(), so that jj resolves a commit id prefix among every commit,
// hidden ones included (shortIDPushed), until the id of a commit of c's change starts with decimal
// digits, the first not 0, that jj resolves to that commit alone: the id of the commit the last
// describe hid where hidden is true, else c's own. It answers c then, and those digits, as many as
// lead the id, up to 12.
func (f *rolePushFixture) decimalPrefixed(c rolePushChange, describer rolePushIdentity, message string, hidden bool) (rolePushChange, string) {
	f.t.Helper()
	f.jj(rolePushImplementer, "config", "set", "--repo", "revsets.short-prefixes", "none()")
	run := f.runner(f.setupPath, rolePushImplementer)
	var digits string
	c = f.redescribe(c, describer, message, func(commit string, hid []string) bool {
		id := commit
		if hidden {
			if len(hid) == 0 {
				return false
			}
			id = hid[len(hid)-1]
		}
		n := 0
		for n < 12 && '0' <= id[n] && id[n] <= '9' {
			n++
		}
		if n == 0 || id[0] == '0' {
			return false
		}
		digits = id[:n]
		out, err := run.Try(f.repo, f.real, "--ignore-working-copy", "log", "-r", digits, "--no-graph", "-T", "commit_id")
		return err == nil && strings.TrimSpace(out) == id
	})
	return c, digits
}

// rolePushShortIDName is what the step's one line of a dry run it cannot read names where the id
// that rolePushShortensDryRun printed for commit starts the ids of more than one commit it read.
func rolePushShortIDName(commit string) string { return "commit id " + commit[:1] + " " }

// rolePushShortIDRows are the pushes of P, a commit whose id as rolePushShortensDryRun prints it
// starts the id of a hidden commit too (shortIDPushed), each pushed by the implementer.
func rolePushShortIDRows() []rolePushRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	message := rolePushMessage("work", "implementer")
	return []rolePushRow{
		{name: "--bookmark", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			p := f.shortIDPushed(rev, impl, message)
			f.bookmark("legion/K-1", p.commit)
			return rolePushSends("legion/K-1", p, "implementer", impl)
		}},
		{name: "-c <commit id>", args: []string{"git", "push", "-c", "{commit:@-}"}, setup: func(f *rolePushFixture) rolePushWant {
			p := f.shortIDPushed(rev, impl, message)
			want := rolePushSends("push-"+p.change[:12], p, "implementer", impl)
			want.ids = []rolePushIDs{rolePushOneIDLine(p.commit)}
			return want
		}},
		{name: "--bookmark, another bookmark's commit of the same digit", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			p := f.shortIDPushed(rev, impl, message)
			f.bookmark("legion/K-1", p.commit)
			other := rolePushMessage("other work", "implementer")
			o := f.redescribe(f.commitOn(rev, impl, "main", other), impl, other, func(commit string, _ []string) bool { return commit[0] == p.commit[0] })
			f.bookmark("other", o.commit)
			return rolePushWant{
				refs:       []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: p.change, author: rev, commit: p.commit}}}},
				same:       map[string]rolePushIdentity{p.commit: rev, o.commit: rev},
				cannotRead: rolePushCannotReadIt, cannotReadNames: rolePushShortIDName(p.commit), noMetaedit: true,
			}
		}},
	}
}

// jj prints each commit id of a dry run, a block's and that of its refusal of a commit with no
// author, as its first 12 hex digits, which the id of another commit of the repository's, a hidden
// one or another issue's, may start with too. A recording jj stands in for such a pair: it
// shortens each of those ids to its first digit (rolePushShortensDryRun), and the setup gives the
// pushed commit a digit that a hidden commit's id starts with too and no other visible commit's,
// in a repository where jj resolves that digit as a prefix among every commit (shortIDPushed). The
// step reads each such id among the commits the push can send, never as a prefix jj resolves: the
// commit, pushed by --bookmark or by -c with its commit id, reaches the remote with its trailer's
// author, and the step prints its rewrite. Where another bookmark, which the push does not send,
// holds a commit of the same digit, the step prints one line that names the short id and changes
// nothing, and the push runs as written and exits 0, as jj alone does. A commit with no author,
// which the step fills, is TestAFilledCommitsShortIDIsReadAmongTheCommitsThePushCanSend.
func TestADryRunsShortCommitIDIsReadAmongTheCommitsThePushCanSend(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushShortIDRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushShortensDryRun)
				f.pushAndCheck(row.args, row.setup(f))
			})
		})
	}
}

// A push by --bookmark of P, a commit with an empty author and the implementer's trailer, in the
// repository of shortIDPushed and through the recording jj that shortens each commit id of a dry
// run to its first digit (rolePushShortensDryRun). The step reads jj's refusal of P among the
// commits it can fill and fills P; its last dry run then prints the id of the commit the fill
// made, which the step reads among the targets of bookmarks and tags and the commits the push's
// revisions name (fullHeads). That id is new in each run, so its first digit may be that of main's
// commit: the setup deletes the local bookmark main before the push, so that legion/K-1 is the one
// bookmark, the repository has no tag and the push no revision argument, and the short id starts
// the id of one commit of that read in every run, whatever the run's ids are. main stays on the
// remote, and trunk() reads main@origin. Fixing the ids instead would not do: jj reads
// JJ_RANDOMNESS_SEED in each command, so one seed gives each command the same change ids and makes
// the fixture's changes divergent, and which digit fixed ids give is a fact of the bytes a jj
// release writes for each commit, not of the step. The commit the step filled reaches the remote
// with the trailer's author, the step prints its rewrite, and it prints no line of a dry run it
// cannot read.
func TestAFilledCommitsShortIDIsReadAmongTheCommitsThePushCanSend(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, rolePushShortensDryRun)
		p := f.shortIDPushed(rolePushNobody, rolePushNobody, rolePushMessage("work", "implementer"))
		f.bookmark("legion/K-1", p.commit)
		f.jj(impl, "bookmark", "delete", "main")
		if read := f.read("bookmarks() | tags()"); read.commit != p.commit {
			t.Fatalf("bookmarks() | tags() holds %s before the push, want %s alone", read.commit, p.commit)
		}
		f.pushAndCheck(rolePushBookmark, rolePushSends("legion/K-1", p, "implementer", impl))
	})
}

// A bookmark that jj itself finds conflicted is jj's own refusal of the push, as at the base: the
// push exits with jj's status and prints jj's error, and nothing changes.
func TestAConflictedBookmarkIsJJsOwnRefusal(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, "")
		a := f.commitOn(rev, impl, "main", rolePushMessage("A", "implementer"))
		b := f.commitOn(rev, impl, "main", rolePushMessage("B", "implementer"))
		op := f.op()
		f.jj(impl, "--at-op", op, "bookmark", "create", "legion/K-1", "-r", a.commit)
		f.jj(impl, "--at-op", op, "bookmark", "create", "legion/K-1", "-r", b.commit)
		f.jj(impl, "bookmark", "list")
		code, _, alone := f.alone(impl, rolePushBookmark...)
		if code == 0 {
			t.Fatalf("jj alone pushed the conflicted bookmark:\n%s", alone)
		}
		divergent := f.divergence()
		got, _, stderr := f.push(impl, rolePushBookmark...)
		f.check(rolePushWant{
			exit: code, cannotRead: rolePushMayNotRead,
			refs: []rolePushRef{{ref: "refs/heads/legion/K-1", absent: true}},
			same: map[string]rolePushIdentity{a.commit: rev, b.commit: rev},
		}, rolePushBookmark, got, stderr, divergent, 0)
		rest := stderr
		for line := range strings.SplitSeq(strings.TrimSpace(alone), "\n") {
			at := strings.Index(rest, line)
			if at < 0 {
				t.Fatalf("the push printed\n%s\nwant jj's own refusal, %q among it, in order", stderr, line)
			}
			rest = rest[at+len(line):]
		}
	})
}

// rolePushNoStepRows are the pushes that run no step: a dry run, help, and a push at an operation,
// in either spelling.
var rolePushNoStepRows = []struct {
	name string
	args []string
}{
	{name: "--dry-run", args: []string{"git", "push", "--dry-run", "--bookmark", "legion/K-1"}},
	{name: "--help", args: []string{"git", "push", "--help"}},
	{name: "-h", args: []string{"git", "push", "-h"}},
	{name: "--at-op <op>", args: []string{"--at-op", "{op:@}", "git", "push", "--bookmark", "legion/K-1"}},
	{name: "--at-operation=<op>", args: []string{"--at-operation={op:@}", "git", "push", "--bookmark", "legion/K-1"}},
}

// `jj git push --dry-run`, `--help`, `-h` and a push with `--at-op` run no step: no commit
// changes, and each adds the operations the same command adds through the real jj alone, in a
// repository made the same way, and no more.
func TestADryRunHelpOrAtOperationRunsNoStep(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	setup := func(f *rolePushFixture) rolePushChange {
		c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
		f.bookmark("legion/K-1", c.commit)
		return c
	}
	for _, row := range rolePushNoStepRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				twin := newRolePushFixture(t, form, "")
				twinChange := setup(twin)
				before := twin.ops()
				aloneCode, aloneOut, _ := twin.alone(impl, twin.resolve(row.args)...)
				aloneAdded := twin.ops() - before
				aloneSent, _ := twin.remoteCommits(twin.bare, "refs/heads/legion/K-1")

				f := newRolePushFixture(t, form, "")
				c := setup(f)
				before = f.ops()
				code, out, stderr := f.push(impl, f.resolve(row.args)...)
				if added := f.ops() - before; added != aloneAdded {
					t.Errorf("the push added %d operations, want %d, as jj alone adds", added, aloneAdded)
				}
				if code != aloneCode || out != aloneOut {
					t.Errorf("the push exited %d and printed %q, want %d and %q, as jj alone does", code, out, aloneCode, aloneOut)
				}
				if lines := rolePushStepLines(stderr); len(lines.rewrites)+len(lines.ids)+len(lines.cannotRead)+len(lines.cannotSet)+len(lines.table)+len(lines.unknown) != 0 {
					t.Errorf("the push printed a line of the step's, want none:\n%s", stderr)
				}
				f.checkSame(c.commit, rev)
				sent, _ := f.remoteCommits(f.bare, "refs/heads/legion/K-1")
				if len(sent) != len(aloneSent) || len(sent) == 1 && (sent[0].id != c.commit || aloneSent[0].id != twinChange.commit) {
					t.Errorf("origin holds %v of legion/K-1, want what jj alone leaves, %v, of the commit as it was", sent, aloneSent)
				}
			})
		})
	}
}

// rolePushCannotReadRows are the dry runs the step cannot read, each with its recording jj's
// behaviour, and the dry runs that a flag, the repository's configuration or the push's own
// --config-file would silence.
func rolePushCannotReadRows() []struct {
	rolePushRow
	behaviour string
} {
	impl, rev := rolePushImplementer, rolePushReviewer
	asWritten := func(names string) func(f *rolePushFixture) rolePushWant {
		return func(f *rolePushFixture) rolePushWant {
			c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
			f.bookmark("legion/K-1", c.commit)
			return rolePushWant{
				refs:       []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}}}},
				same:       map[string]rolePushIdentity{c.commit: rev},
				cannotRead: rolePushCannotReadIt, cannotReadNames: names, noMetaedit: true,
			}
		}
	}
	reads := func(f *rolePushFixture) rolePushWant {
		c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
		f.bookmark("legion/K-1", c.commit)
		return rolePushWant{refs: []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: c.change, author: impl}}}}, others: true}
	}
	return []struct {
		rolePushRow
		behaviour string
	}{
		{rolePushRow{name: "a line of neither shape in the block", args: rolePushBookmark, setup: asWritten(rolePushUnreadableLine)}, rolePushUnreadableDryRun},
		{rolePushRow{name: "a dry run that exits 0 with no block", args: rolePushBookmark, setup: asWritten("")}, rolePushDryRunWithNoBlock},
		// What silences jj the pod's wrapper reads too (its own reads of the configuration, and
		// --quiet before or after the command): these rows run in both forms (rolePushRow.bothForms).
		{rolePushRow{name: "--quiet on the push", bothForms: true, args: []string{"git", "push", "--quiet", "--bookmark", "legion/K-1"}, setup: reads}, ""},
		{rolePushRow{name: "ui.quiet = true in the repository's configuration", bothForms: true, args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			f.jj(impl, "config", "set", "--repo", "ui.quiet", "true")
			return reads(f)
		}}, ""},
		// The push's own --config-file sets ui.quiet = true after its own --config, both in front
		// of `git push`: the dry run's --config goes after both. The file is in .jj, which jj never
		// snapshots, and the push runs in the repository, so the row names it by a relative path.
		{rolePushRow{name: "ui.quiet = true in the push's own --config-file, after its own --config", bothForms: true, args: []string{"--config", "ui.color=never", "--config-file", ".jj/quiet.toml", "git", "push", "--bookmark", "legion/K-1"}, setup: func(f *rolePushFixture) rolePushWant {
			f.writeFile(filepath.Join(f.repo, ".jj", "quiet.toml"), "ui.quiet = true\n")
			return reads(f)
		}}, ""},
		// The bound on the step's loop over jj's refusal of a commit with no author: a metaedit
		// that fills nothing must not make the step read the same refusal for ever.
		{rolePushRow{name: "a metaedit that fills no author", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			c := f.commitOn(rolePushNobody, rolePushNobody, "main", rolePushMessage("work", "implementer"))
			f.bookmark("legion/K-1", c.commit)
			return rolePushWant{
				exit:       1,
				refs:       []rolePushRef{{ref: "refs/heads/legion/K-1", absent: true}},
				same:       map[string]rolePushIdentity{c.commit: rolePushNobody},
				cannotRead: rolePushCannotReadIt, cannotReadNames: c.commit[:12],
			}
		}}, rolePushMetaeditFillsNothing},
	}
}

// rolePushCannotReadIDRows are pushes of a commit id whose dry run the step cannot read (the
// recording jj of rolePushUnreadableDryRun adds a line of neither shape to each block).
var rolePushCannotReadIDRows = []rolePushArgsRow{
	// No rewrite of the step's hid the commit, so the real push gets its id as the role wrote it
	// and sends that commit, as jj alone does, which makes it visible again beside the other. The
	// step, which gives the push no word of its own, takes no snapshot.
	{name: "a commit id hidden before the push, its change with one visible commit", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
		impl, rev := rolePushImplementer, rolePushReviewer
		hidden := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
		f.jj(impl, "describe", "-r", hidden.commit, "-m", rolePushMessage("work, described again", "implementer"))
		visible := f.read(hidden.change)
		return []string{"git", "push", "--named", "nm=" + hidden.commit}, rolePushWant{
			refs:       []rolePushRef{{ref: "refs/heads/nm", commits: []rolePushCommit{{change: hidden.change, author: rev, commit: hidden.commit}}}},
			same:       map[string]rolePushIdentity{hidden.commit: rev, visible.commit: rev},
			diverges:   true,
			cannotRead: rolePushCannotReadIt, cannotReadNames: rolePushUnreadableLine, noMetaedit: true,
			snapshots: rolePushTakesNoSnapshot,
		}
	}},
	// The step's loop over jj's refusal of a commit with no author fills the commit the role named
	// before the dry run it cannot read, after the step's snapshot, and that rewrite hides it: the
	// real push gets the commit after the rewrite, and the step prints the one line of that commit
	// id.
	{name: "a commit id of a commit the step fills before that dry run", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
		c := f.commitOn(rolePushNobody, rolePushNobody, "main", rolePushMessage("work", "implementer"))
		want := rolePushSends("nm", c, "implementer", rolePushImplementer)
		want.cannotRead, want.cannotReadNames = rolePushCannotReadIt, rolePushUnreadableLine
		want.ids = []rolePushIDs{rolePushOneIDLine(c.commit)}
		return []string{"git", "push", "--named", "nm=" + c.commit}, want
	}},
}

// A dry run the step cannot read, one with a line of neither shape in its block or one that exits
// 0 with no block (a recording jj ahead of the real one prints each), makes the step print one
// line and change nothing, and the real push runs as written. A commit id of a commit hidden
// before the push goes to it as the role wrote it, and the step takes no snapshot; one whose
// commit the step's loop over a commit with no author rewrote before that dry run gets the commit
// after the rewrite. That rewrite comes after the step's snapshot, so the step's line ends by
// saying that the push runs with --ignore-working-copy, and the real push runs with that flag as
// its first word and takes no snapshot of its own. A metaedit that fills no author ends the step's
// loop over jj's refusal of a commit with no author the same way, after the step's snapshot: one
// line naming the commit, and the real push with --ignore-working-copy, which jj refuses. A
// --quiet on the push, ui.quiet in the repository's configuration and ui.quiet in the push's own
// --config-file after its own --config do not silence the step's dry run: the remote's commit has
// the trailer's author.
func TestADryRunTheStepCannotReadChangesNothing(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushCannotReadRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushForEach(t, row.bothForms, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, row.behaviour)
				want := row.setup(f)
				f.pushAndCheck(row.args, want)
				if row.behaviour == rolePushMetaeditFillsNothing {
					_, _, stderr := f.alone(rolePushImplementer, "git", "push", "--dry-run", "--bookmark", "legion/K-1")
					if refusal, _, _ := strings.Cut(stderr, "\n"); !strings.Contains(refusal, "no author") {
						t.Fatalf("jj alone printed %q, want its refusal of a commit with no author", stderr)
					}
				}
			})
		})
	}
	rolePushRunArgs(t, rolePushUnreadableDryRun, rolePushCannotReadIDRows)
}

// A dry run of `jj git push -c` prints `Creating bookmark push-<change> for revision <change>`
// on standard error before jj's refusal of the push, here of a commit with no description. The
// step cannot read such a dry run, and its one line names what it could not read: jj's error,
// never the line before it, which it reads. The push then runs as written, and jj refuses it.
func TestTheLineOfADryRunTheStepCannotReadNamesJJsError(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ name, revision string }{
		{name: "-c with a commit id", revision: "commit id"},
		{name: "-c @-", revision: "@-"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, "")
				f.jj(rolePushReviewer, "new", "main")
				f.write()
				f.jj(rolePushImplementer, "new")
				c := f.read("@-")
				revision := row.revision
				if revision == "commit id" {
					revision = c.commit
				}
				f.pushAndCheck([]string{"git", "push", "-c", revision}, rolePushWant{
					exit:            1,
					same:            map[string]rolePushIdentity{c.commit: rolePushReviewer},
					cannotRead:      rolePushCannotReadIt,
					cannotReadNames: "Error: Won't push commit " + c.commit[:12] + " since it has no description",
				})
			})
		})
	}
}

// rolePushTableRows are the values of LEGION_ROLE_IDENTITIES that the step cannot use; nil leaves
// it unset.
var rolePushTableRows = []struct {
	name  string
	value *string
}{
	{name: "unset"},
	{name: "empty", value: new("")},
	{name: "not JSON", value: new("implementer=legion-implementer[bot]")},
	{name: "an unknown role", value: new(`{"implementer":{"name":"legion-implementer[bot]","email":"100000001+legion-implementer[bot]@users.noreply.github.com"},"wizard":{"name":"wizard","email":"wizard@example.invalid"}}`)},
	{name: "an empty name", value: new(`{"implementer":{"name":"","email":"100000001+legion-implementer[bot]@users.noreply.github.com"}}`)},
	{name: "a name with a line break", value: new(`{"implementer":{"name":"legion-implementer[bot]\nLegion-Role: reviewer","email":"100000001+legion-implementer[bot]@users.noreply.github.com"}}`)},
}

// A process whose table is unset, as a controller's is, changes nothing and prints nothing. One
// whose table is set and cannot be used (empty, not JSON, naming a role outside claim.Roles, or
// holding an empty name or one with a line break) prints one line, runs no metaedit, and the push
// runs as written.
func TestARoleIdentityTableThatDoesNotParseChangesNothing(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	for _, row := range rolePushTableRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, "")
				c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
				f.bookmark("legion/K-1", c.commit)
				f.table = row.value
				f.pushAndCheck(rolePushBookmark, rolePushWant{
					refs:  []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}}}},
					same:  map[string]rolePushIdentity{c.commit: rev},
					table: row.value != nil,
				})
			})
		})
	}
}

// A push with no commit to change, of a clean working copy, adds the one operation that the same
// push through the real jj alone adds in a repository made the same way, and the recording jj sees
// no metaedit and a real push with no --ignore-working-copy, as the role wrote none. The step
// compares git's HEAD and branches with jj's, which git left as jj's commands set them, with
// reads that carry --ignore-working-copy (checkCompared), and takes no snapshot: a snapshot of a
// clean working copy adds no operation, so only the recording jj, which sees no command of the
// step's without --ignore-working-copy, tells a step that took one.
func TestAPushWithNothingToChangeAddsNoOperation(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	setup := func(f *rolePushFixture) rolePushChange {
		c := f.commitOn(impl, impl, "main", rolePushMessage("work", "implementer"))
		f.bookmark("legion/K-1", c.commit)
		return c
	}
	rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
		twin := newRolePushFixture(t, form, "")
		setup(twin)
		before := twin.ops()
		if code, _, stderr := twin.alone(impl, rolePushBookmark...); code != 0 {
			t.Fatalf("jj alone exited %d:\n%s", code, stderr)
		}
		aloneAdded := twin.ops() - before

		f := newRolePushFixture(t, form, rolePushRecords)
		c := setup(f)
		before = f.ops()
		divergent := f.divergence()
		code, _, stderr := f.push(impl, rolePushBookmark...)
		f.check(rolePushWant{
			refs:       []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: c.change, author: impl, commit: c.commit}}}},
			noMetaedit: true,
			snapshots:  rolePushTakesNoSnapshot,
		}, rolePushBookmark, code, stderr, divergent, 0)
		f.checkCompared(f.calls(), true)
		if added := f.ops() - before; added != aloneAdded {
			t.Errorf("the push added %d operations, want %d, as jj alone adds", added, aloneAdded)
		}
	})
}

// rolePushFailingRows are the step's jj commands that fail, each a recording jj's behaviour and the
// error it prints.
var rolePushFailingRows = []struct {
	name, behaviour, error string
	// bothForms runs the row in each form, the family's check that the two forms agree.
	bothForms bool
}{
	{name: "a metaedit that fails", behaviour: rolePushFailsMetaedit, error: "Error: legion test: this metaedit fails", bothForms: true},
	{name: "a read of the set that fails", behaviour: rolePushFailsSetRead, error: "Error: legion test: this read of the set fails"},
	{name: "a snapshot that fails", behaviour: rolePushFailsSnapshot, error: "Error: legion test: this snapshot fails"},
}

// rolePushFailingPush pushes legion/K-1, on a commit that the reviewer made with the implementer's
// trailer, under form with a recording jj of behaviour ahead of the real one, and checks that the
// push exits 1 after jj's error, failure, and that nothing reaches the remote.
func rolePushFailingPush(t *testing.T, form rolePushForm, behaviour, failure string) {
	f := newRolePushFixture(t, form, behaviour)
	c := f.commitOn(rolePushReviewer, rolePushImplementer, "main", rolePushMessage("work", "implementer"))
	f.bookmark("legion/K-1", c.commit)
	divergent := f.divergence()
	code, _, stderr := f.push(rolePushImplementer, rolePushBookmark...)
	f.check(rolePushWant{exit: 1, refs: []rolePushRef{{ref: "refs/heads/legion/K-1", absent: true}}}, rolePushBookmark, code, stderr, divergent, 0)
	if !strings.Contains(stderr, failure) {
		t.Errorf("the push printed\n%s\nwant jj's error %q", stderr, failure)
	}
}

// When a jj command of the step fails, its metaedit, its read of the set the push sends or its
// snapshot of the working copy, the push exits with its status after jj's error, and nothing
// reaches the remote (rolePushFailingPush).
func TestAFailingStepSendsNothing(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushFailingRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushForEach(t, row.bothForms, func(t *testing.T, form rolePushForm) {
				rolePushFailingPush(t, form, row.behaviour, row.error)
			})
		})
	}
}

// When the read of the push's configuration that the pane's legion jj makes before the step's
// first command fails, the push exits with its status after jj's error, and nothing reaches the
// remote (rolePushFailingPush): under the tmux form alone, with a recording jj ahead of the real
// one that fails every `jj config list`. The pod's form leaves that read to the pod's wrapper's own
// read of git.executable-path, whose failure refuses the push (internal/leakguard's
// TestTheJJWrapperReadsTheConfigurationThePushWouldUse).
func TestAFailingReadOfThePushsConfigurationSendsNothing(t *testing.T) {
	t.Parallel()
	const failure = "Error: legion test: this read of the configuration fails"
	rolePushFailingPush(t, rolePushTmux, `case " $* " in *" config list "*) echo '`+failure+`' >&2; status=1; ran=1 ;; esac`, failure)
}

// rolePushPanesRows are the PATHs on which a pane's jj comes back to itself: its directory twice, a
// host's jj that runs jj by PATH after it, and a second Legion directory in front of it, as a
// controller started from a pane has. A row whose alias is not "" sets aliases.p to it in the
// repository's configuration and pushes with `jj p --bookmark legion/K-1`, a push that jj expands.
// The pod's wrapper runs its jj by its full path, so under the pod's form a host's jj after it is
// never reached, and the second directory is another of the image's: a bin that workerbin.Install
// writes in front of the pod's wrappers would hold a git of its own, which the pod's jj refuses as
// no leak check's.
var rolePushPanesRows = []struct {
	name  string
	path  func(f *rolePushFixture) string
	alias string
}{
	{name: "its directory twice on PATH", path: func(f *rolePushFixture) string { return f.formDir + ":" + f.path }},
	{name: "a host jj script that calls jj by PATH", path: rolePushHostPath},
	{name: "two Legion bin directories", path: func(f *rolePushFixture) string {
		if f.form == rolePushTmux {
			return f.install(f.path) + ":" + f.path
		}
		return f.podDir(filepath.Join(f.root, "second-pod-bin")) + ":" + f.path
	}},
	{name: "an alias push through a host jj script that calls jj by PATH", path: rolePushHostPath, alias: `["git", "push"]`},
}

// A pane's jj never runs itself again: with its directory twice on PATH, with a host's jj script
// after it that calls jj by PATH, and behind a second Legion bin directory, the step runs once (one
// metaedit, one push and one snapshot at most, and no dry run after the metaedit, which the
// recording jj sees) and the remote gets the trailer's author. The step's dry runs of this push
// come before its metaedit, since no commit the push sends lacks an author, so a dry run after it
// is a second run of the step: one on the real push takes no snapshot, since the push's words carry
// the step's --ignore-working-copy, and runs no metaedit, since the commit has its author by then.
// A push that jj expands from an alias, through that host's jj, runs the step once too. Under the
// tmux form legion jj makes one read of the configuration (rolePushCall.isConfigRead) on every
// row: of the aliases, for the push written as an alias, and, for a push written as `git push`,
// of the push's configuration before the step's first command. The host's jj brings that read
// back into the pane's jj, and that jj passes it on with no read of its own. Under the pod's form
// the pod's wrapper expands the line, with reads that each name a key, and legion jj makes no
// read.
func TestThePanesJJNeverRunsItselfAgain(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	for _, row := range rolePushPanesRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				args, reads := rolePushBookmark, 0
				if form == rolePushTmux {
					reads = 1
				}
				if row.alias != "" {
					f.jj(impl, "config", "set", "--repo", "aliases.p", row.alias)
					args = []string{"p", "--bookmark", "legion/K-1"}
				}
				c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
				f.bookmark("legion/K-1", c.commit)
				path := row.path(f)
				divergent := f.divergence()
				code, _, stderr := f.pushOn(path, impl, args...)
				f.check(rolePushSends("legion/K-1", c, "implementer", impl), args, code, stderr, divergent, 0)
				calls := f.calls()
				var snapshot, metaedit, push, read int
				var again [][]string
				for _, call := range calls {
					switch {
					case call.isConfigRead():
						read++
					case call.has("metaedit"):
						metaedit++
					case call.has("--dry-run"):
						if metaedit > 0 {
							again = append(again, call.args)
						}
					case call.realPush():
						push++
					case !call.has("--ignore-working-copy"):
						snapshot++
					}
				}
				if metaedit != 1 || push != 1 || snapshot > 1 {
					t.Errorf("the recording jj saw %d metaedits, %d pushes and %d snapshots, want one metaedit, one push and one snapshot at most: the step runs once\n%v", metaedit, push, snapshot, calls)
				}
				if len(again) != 0 {
					t.Errorf("the recording jj saw the dry runs %q after the step's metaedit, want none: the step ran again on the real push, which the loop guard skips where wrappedJJStepped is the digest of the push's own words, the step's --ignore-working-copy among them (jjStepped)\n%v", again, calls)
				}
				if read != reads {
					t.Errorf("the recording jj saw %d reads of the configuration by legion jj, want %d\n%v", read, reads, calls)
				}
			})
		})
	}
}

// rolePushStartedRows are the pushes that a jj command starts, each sending @-, a commit the other
// role made and the implementer described: `jj util exec` runs the inner jj by PATH, which finds
// the form's jj first on PATH.
func rolePushStartedRows() []rolePushRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	return []rolePushRow{
		// The inner jj is the form's own: a case of the forms, so in both.
		{name: "jj util exec -- jj git push --bookmark", bothForms: true, args: []string{"util", "exec", "--", "jj", "git", "push", "--bookmark", "legion/K-1"}, setup: func(f *rolePushFixture) rolePushWant {
			c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
			f.bookmark("legion/K-1", c.commit)
			return rolePushSends("legion/K-1", c, "implementer", impl)
		}},
	}
}

// A push that a jj command starts is a push of its own, and the step runs before it. Under the
// tmux form the inner jj of `jj util exec -- jj git push …` is the pane's jj, and its environment
// holds the loop guard's chain that the outer command's legion jj set, which lists the real jj: a
// re-entry to pickAfter, as a push that a host's jj wrapper brings back is. The remote gets the
// trailer's author, and the step prints the rewrite. The recording jj ahead of the real one sees
// the outer `jj util exec` too, which is no real push (rolePushCall.realPush), and the inner push
// with --ignore-working-copy as its first word, after the step's snapshot.
func TestAPushThatAJJCommandStartsRunsTheStep(t *testing.T) {
	t.Parallel()
	rolePushRun(t, rolePushStartedRows())
}

// rolePushOption is one option of the `Options:` section of `jj git push --help`: its short and
// long names, either of which may be "".
type rolePushOption struct{ short, long string }

func (o rolePushOption) String() string { return strings.Trim(o.short+", "+o.long, ", ") }

// sendsNoCommit is whether the option is one that names no commit to send: it shows, refuses,
// deletes, allows what jj would refuse, passes a push option to git, or picks the remote.
func (o rolePushOption) sendsNoCommit() bool {
	switch o.long {
	case "--dry-run", "--help", "--deleted", "--option", "--remote":
		return true
	}
	return strings.HasPrefix(o.long, "--allow-")
}

// drivenBy is whether word, one of a row's arguments, uses the option: a name, a long name with
// `=`, or a short name with its value attached.
func (o rolePushOption) drivenBy(word string) bool {
	return o.long != "" && (word == o.long || strings.HasPrefix(word, o.long+"=")) ||
		o.short != "" && strings.HasPrefix(word, o.short)
}

// rolePushHelpOptions reads the options of the `Options:` section of help, jj git push's, up to
// `Global Options:`: each line indented as an option is, that starts with `-`.
func rolePushHelpOptions(help string) []rolePushOption {
	var options []rolePushOption
	in := false
	for line := range strings.SplitSeq(help, "\n") {
		switch {
		case line == "Options:":
			in = true
			continue
		case line != "" && !strings.HasPrefix(line, " "):
			in = false
		}
		trimmed := strings.TrimLeft(line, " ")
		if !in || !strings.HasPrefix(trimmed, "-") || len(line)-len(trimmed) > 6 {
			continue
		}
		names, _, _ := strings.Cut(trimmed, " <")
		var option rolePushOption
		for name := range strings.SplitSeq(names, ", ") {
			if strings.HasPrefix(name, "--") {
				option.long = name
			} else {
				option.short = name
			}
		}
		options = append(options, option)
	}
	return options
}

// rolePushEveryArgument is every argument of every row of these tests' pushes that the row writes
// out, ahead of its setup (rolePushCannotReadIDRows' setups answer theirs, each a --named, and
// rolePushRevsetRows', rolePushFollowedRows' and rolePushNamedRows' theirs, each a -c, a -r or a
// --named).
func rolePushEveryArgument() []string {
	var words []string
	for _, rows := range [][]rolePushRow{rolePushAuthorRows(), rolePushFormRows(), rolePushRefusedRows(), rolePushUntouchedRows(), rolePushStartedRows(), rolePushShortIDRows()} {
		for _, row := range rows {
			words = append(words, row.args...)
		}
	}
	for _, row := range rolePushCannotReadRows() {
		words = append(words, row.args...)
	}
	for _, row := range rolePushTwoRemoteFirsts {
		words = append(words, row.args...)
	}
	for _, row := range rolePushNoStepRows {
		words = append(words, row.args...)
	}
	for _, row := range rolePushUnimportedRows {
		words = append(words, row.args...)
	}
	words = append(words, rolePushTwoRemoteSecond...)
	return append(words, rolePushBookmark...)
}

// Every option of `jj git push` that names what the push sends is driven by a row of these tests:
// the `Options:` section of the help of the jj under test, read through each form, holds no
// option that no row uses and that is not one that sends no commit. So a form a later jj adds is a
// failing test until a row drives it.
func TestEveryPushOptionIsDriven(t *testing.T) {
	t.Parallel()
	words := rolePushEveryArgument()
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushWrapper(t, form, "")
		code, help, stderr := trailerRun(t, f.root, f.env(f.path, rolePushImplementer), "", "/bin/sh", "-c", `exec jj git push --help`)
		if code != 0 {
			t.Fatalf("jj git push --help exited %d:\n%s", code, stderr)
		}
		options := rolePushHelpOptions(help)
		if !slices.ContainsFunc(options, func(o rolePushOption) bool { return o.long == "--bookmark" }) {
			t.Fatalf("read %v from the Options of jj git push --help, want --bookmark among them:\n%s", options, help)
		}
		for _, option := range options {
			if option.sendsNoCommit() {
				continue
			}
			if !slices.ContainsFunc(words, option.drivenBy) {
				t.Errorf("jj git push %s: no row of these tests drives it, and it is not an option that sends no commit", option)
			}
		}
	})
}
