package main

import (
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The tests in this file are more of the step's (jj_push_test.go): pushes of a word of ids or a
// command past the step's bound or limit, or under that limit only as the step counts it, with the
// bounds and limits they set and the helpers that only they use. As jj_push_test.go does, the file
// names nothing the step's change adds, so it compiles and runs at the base too.

// rolePushLowBound is the step's bound of a word of ids, in bytes, that two rows of
// TestARevisionPastALoweredBoundGetsItsIDsInWordsOfItsOption set (rolePushBoundVariable): over the
// 92 bytes of two commit ids joined by ` | ` behind `--change=`, and over each revset of the step's
// reads of one commit, the longest of which, `(mutable() | (::(<C>) & mutable())) ~
// ::remote_bookmarks()`, is 95 bytes; and under the 126 bytes of three commit ids joined so, and so
// under the shortest primary form of an argument that names three commits the step rewrites.
const rolePushLowBound = 100

// rolePushOneWordBound is the step's bound of a word of ids, in bytes, that a row of
// TestARevisionPastALoweredBoundGetsItsIDsInWordsOfItsOption and
// TestARevisionWhoseIDsPassALoweredBoundGetsItsPrimaryForm set (rolePushBoundVariable): over the
// 126 bytes of three commit ids joined by ` | `, and over the 132 bytes of the primary form of
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
			// jj alone runs no form of Legion's, so its twin needs one fixture, the pod's.
			twin := newRolePushFixture(t, rolePushPod, "")
			args := setup(twin)
			if code, _, stderr := trailerRunInput(t, twin.repo, twin.env(twin.setupPath, rolePushImplementer), strings.NewReader(""), twin.real, args...); code != 0 {
				t.Fatalf("jj alone exited %d:\n%s", code, stderr)
			}
			twin.checkIDsHistory(option, rolePushReviewer)
			// The step's own logic: rolePushStepLogic. The lowered bound's first row below checks the
			// word bound in both forms.
			rolePushStepLogic(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				args := setup(f)
				divergent := f.divergence()
				code, _, stderr := f.push(rolePushImplementer, args...)
				rewrites, printed := rolePushCountRewrites(stderr)
				f.check(rolePushWant{others: true, ids: []rolePushIDs{{argument: args[3], commits: rolePushIDsHistory, words: 2, bound: rolePushRevisionBound}}}, args, code, printed, divergent, 0)
				f.checkIDsHistory(option, rolePushImplementer)
				if rewrites != rolePushIDsHistory {
					t.Errorf("the push printed %d rewrites, want %d; it printed beside them:\n%s", rewrites, rolePushIDsHistory, printed)
				}
				calls := f.calls()
				push := f.oneRealPush(calls)
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
		want := rolePushWant{ids: []rolePushIDs{{argument: argument, commits: len(cs), words: count, bound: f.bound}}}
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
		// The word bound's check that the two forms agree (rolePushArgsRow.bothForms).
		{name: "-c <C1> | <C2> | <C3>", bothForms: true, setup: func(f *rolePushFixture) ([]string, rolePushWant) {
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

// `jj git push -r ::<C>`, a tracked bookmark on C (-r sends a bookmark that points at the commit,
// and jj refuses to make a remote bookmark for one that tracks nothing), on a history of five
// commits under C that origin holds (rolePushFixture.history), with the step's bound of a word of
// ids lowered to rolePushOneWordBound bytes: under the 341 bytes of the ids of the eight commits
// the argument names, C, the history's, the seed's and the root's, which so fit no one word, and
// over the 132 bytes of the argument's primary form, `(::<C>) ~ (<C>) | <C'>`, C' the commit the
// step made of C. The real push gets that primary form, which a recording jj ahead of the real one
// sees: a word under the bound that holds the ids of C's change and of no commit the step left
// alone, since jj reads the argument among the commits the step can rewrite, C alone. So where the
// ids of what an argument names pass the bound, its word grows with the commits the step
// rewrote, not with C's history. The remote gets the rewritten C with its trailer's author, the
// step prints the rewrite, and no change is divergent.
func TestARevisionWhoseIDsPassALoweredBoundGetsItsPrimaryForm(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	rolePushRunArgs(t, rolePushRecords, []rolePushArgsRow{
		{name: "-r ::<C>, a tracked bookmark on C", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			f.history(5)
			f.bound = rolePushOneWordBound
			f.track("legion/K-1")
			c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
			f.bookmark("legion/K-1", c.commit)
			want := rolePushSends("legion/K-1", c, "implementer", impl)
			want.pushed = func(words []string) {
				revision := words[len(words)-1]
				if len(revision) >= rolePushOneWordBound {
					f.t.Errorf("the real push's revision %q is %d bytes, want under %d", revision, len(revision), rolePushOneWordBound)
				}
				for _, id := range rolePushFullID.FindAllString(revision, -1) {
					if change := f.changeOf(id); change != c.change {
						f.t.Errorf("the real push's revision %q holds %s, a commit of change %q that the step left alone; want ids of C's change %s alone", revision, id, change, c.change)
					}
				}
				if primary := "(::" + c.commit + ") ~ (" + c.commit + ") | " + f.read(c.change).commit; revision != primary {
					f.t.Errorf("the real push's revision is %q, want its primary form %q", revision, primary)
				}
			}
			return []string{"git", "push", "-r", "::" + c.commit}, want
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
// author, and before it takes its snapshot of the working copy where no fill took it, that the
// push's command would then pass the limit. It sets no other author, prints one line `legion jj:
// could not set the author: revision `<argument>` would go to the push as the ids of its <n>
// commits, …` that names the limit, and the push runs as written but where the fill ran, exit 0,
// with no change divergent and origin holding no ref beside main but those of -c. Where the fill
// ran, after the step's one snapshot, the real push runs with --ignore-working-copy as its first
// word and takes no snapshot of its own, and that line ends by saying so:
//
//   - `-c "<C1> | <C2>"`, C1 and C2 two commits that the reviewer made with the implementer's
//     trailer: the step prints no rewrite, runs no metaedit and takes no snapshot (a recording jj
//     ahead of the real one sees no metaedit and no command of the step's with no
//     --ignore-working-copy), the real push gets the role's own words, every byte, beside the pod's
//     jj's own layers (rolePushFixture.written), and origin holds C1 and C2 as they were, with the
//     reviewer's author, as jj alone leaves them on a twin state made the same way;
//   - `-c "<E> | <C>"`, E a commit with an empty author and the implementer's trailer, which the
//     step fills before it decides, after its one snapshot, and C one that the reviewer made with
//     the implementer's trailer: the step prints the rewrite of E alone and runs one metaedit,
//     that fill, and the real push gets the ids of E as the fill left it, with the implementer's
//     author, in place of the hidden E, and of C as it was, with the reviewer's author (one line);
//   - `-c <D> -c mine()`, D a commit that the reviewer made with the implementer's trailer on E,
//     one with an empty author, which the step fills before it decides, after its one snapshot,
//     and mine() naming M alone, a commit the implementer made with its own trailer: the step
//     prints the rewrite of E alone and runs one metaedit, that fill, which rebases D, and gives
//     each argument as the id of the commit it named before the step, as the fill left it (one
//     line each). The real push sends the rebased D, with the reviewer's author, on E as the fill
//     left it, with the implementer's, and M as it was, and makes no bookmark for E, which mine()
//     names after the fill.
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
		want.noMetaedit, want.snapshots = true, rolePushTakesNoSnapshot
		want.pushed = func(words []string) {
			if !slices.Equal(f.written(words), args) {
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
				ids:       []rolePushIDs{rolePushOneIDLine(d.commit), rolePushOneIDLine("mine()")},
				snapshots: rolePushTakesOneSnapshot,
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
				ids:       []rolePushIDs{{argument: args[3], commits: 2, words: 1, bound: rolePushRevisionBound}},
				snapshots: rolePushTakesOneSnapshot,
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
	// The limit of a command's check that the two forms agree: the pod's line carries the
	// git.executable-path words and each form's environment its own PATH, all of it in the size.
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, rolePushMeasuresMetaedit)
		f.mutableHistory(rolePushPartsHistory)
		f.argMax = rolePushSize(f.env(f.path, impl)...) + rolePushPartsRoom
		divergent := f.divergence()
		args := []string{"git", "push", "-b", "legion/K-1"}
		code, _, stderr := f.push(impl, args...)
		rewrites, printed := rolePushCountRewrites(stderr)
		f.check(rolePushWant{others: true}, args, code, printed, divergent, 0)
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
