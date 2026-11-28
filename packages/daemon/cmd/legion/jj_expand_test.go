package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The tests in this file are those of a command that jj expands, through an alias, an alias of an
// alias or ui.default-command, run as a role runs it through one of the two forms of Legion's jj
// (jj_push_test.go, and the fixture in jj_push_fixture_test.go), on a real jj repository with a
// local bare remote. The pod's form expands the line in packages/daemon/docker/jj, and the tmux
// form in `legion jj --after` (jj_expand.go). Like jj_push_test.go, this file names nothing the
// change adds, so it compiles and runs at the base too; the tests that read the change's own Go
// names are in jj_expand_read_test.go.

// rolePushPodRefusalPrefix starts the line of the pod's wrapper's refusal of a line it cannot read
// (packages/daemon/docker/jj's refuse), which it prints before it exits 2.
const rolePushPodRefusalPrefix = "LEAK CHECK SETUP ERROR: the push: "

// configure appends toml to the configuration of the repository, the file `jj config path --repo`
// names: jj config set writes neither a table of an alias nor a [[--scope]].
func (f *rolePushFixture) configure(toml string) {
	f.t.Helper()
	f.configureIn("--repo", toml)
}

// configureIn appends toml to the configuration file that `jj config path <scope>` names, scope
// being --repo or --user (the user's, under the fixture's own HOME).
func (f *rolePushFixture) configureIn(scope, toml string) {
	f.t.Helper()
	path := strings.TrimSpace(f.jj(rolePushImplementer, "config", "path", scope))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := file.WriteString("\n" + toml + "\n"); err != nil {
		f.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		f.t.Fatal(err)
	}
}

// isConfigRead is whether the call is a read of legion jj's: a `jj config list` that names no key,
// its read of the aliases or its read of a push's configuration before the step, where each of the
// pod's wrapper's reads names one (`aliases.<name>`, `ui.default-command`).
func (c rolePushCall) isConfigRead() bool {
	return c.pair() == [2]string{"config", "list"} && !slices.ContainsFunc(c.args, func(arg string) bool {
		return strings.HasPrefix(arg, "aliases.") || arg == "ui.default-command"
	})
}

// workAndB makes C, a commit the other role made and the implementer described with its own
// trailer, on main, under the new bookmark b.
func (f *rolePushFixture) workAndB() rolePushChange {
	f.t.Helper()
	c := f.commitOn(rolePushReviewer, rolePushImplementer, "main", rolePushMessage("work", "implementer"))
	f.bookmark("b", c.commit)
	return c
}

// rolePushBAbsent is what a command that sends nothing leaves of C: no b on origin, and C as it
// was.
func rolePushBAbsent(c rolePushChange) rolePushWant {
	return rolePushWant{
		refs: []rolePushRef{{ref: "refs/heads/b", absent: true}},
		same: map[string]rolePushIdentity{c.commit: rolePushReviewer},
	}
}

// rolePushBAsItWas is what a push of b that runs no step leaves: b on origin at C as it was, with
// the other role's author.
func rolePushBAsItWas(c rolePushChange) rolePushWant {
	return rolePushWant{
		refs: []rolePushRef{{ref: "refs/heads/b", commits: []rolePushCommit{{change: c.change, author: rolePushReviewer, commit: c.commit}}}},
		same: map[string]rolePushIdentity{c.commit: rolePushReviewer},
	}
}

// rolePushExpandedRow is a row of a command that jj expands: setup makes the repository and its
// configuration, and answers the role's words and what the push must leave.
type rolePushExpandedRow struct {
	name  string
	setup func(f *rolePushFixture) (args []string, want rolePushWant)
}

// rolePushExpandsTo is a row whose configuration is toml, over C under b (workAndB), whose push of
// args sends C as the step left it, the real push running expanded, the line jj expands args into,
// with the step's words.
func rolePushExpandsTo(toml string, args, expanded []string) func(f *rolePushFixture) ([]string, rolePushWant) {
	return func(f *rolePushFixture) ([]string, rolePushWant) {
		f.configure(toml)
		c := f.workAndB()
		want := rolePushSends("b", c, "implementer", rolePushImplementer)
		want.pushed = f.pushedAfterSnapshot(expanded)
		return args, want
	}
}

// rolePushChain is the configuration of a chain of n aliases, c01 to c<n>, each naming the next,
// the last `git push -b b`.
func rolePushChain(n int) string {
	var toml strings.Builder
	toml.WriteString("[aliases]\n")
	for i := 1; i < n; i++ {
		fmt.Fprintf(&toml, "c%02d = [\"c%02d\"]\n", i, i+1)
	}
	fmt.Fprintf(&toml, "c%02d = [\"git\", \"push\", \"-b\", \"b\"]\n", n)
	return toml.String()
}

// rolePushExpandedRows are the commands that jj expands into a push of C under b, each pushed by
// the implementer.
func rolePushExpandedRows() []rolePushExpandedRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	pushB := []string{"git", "push", "-b", "b"}
	return []rolePushExpandedRow{
		{name: "jj pp -b b, aliases.pp = ['p'] and aliases.p = ['git', 'push']", setup: rolePushExpandsTo("[aliases]\npp = [\"p\"]\np = [\"git\", \"push\"]", []string{"pp", "-b", "b"}, pushB)},
		// An option clap does not know in front of the command word: jj puts the default
		// command in front of the whole line.
		{name: "jj -b b, ui.default-command = ['git', 'push']", setup: rolePushExpandsTo("[ui]\ndefault-command = [\"git\", \"push\"]", []string{"-b", "b"}, pushB)},
		{name: "jj -b b, ui.default-command = 'p' and aliases.p = ['git', 'push']", setup: rolePushExpandsTo("[ui]\ndefault-command = \"p\"\n[aliases]\np = [\"git\", \"push\"]", []string{"-b", "b"}, pushB)},
		{name: "jj --, ui.default-command = ['git', 'push', '-b', 'b']", setup: rolePushExpandsTo("[ui]\ndefault-command = [\"git\", \"push\", \"-b\", \"b\"]", []string{"--"}, []string{"git", "push", "-b", "b", "--"})},
		// An argument in the alias's own words, held to its set (jj_push_revision_test.go): R,
		// which the implementer made with the reviewer's trailer, under the tracked bookmark r,
		// is all that mine() names, and none of it once the step has rewritten R, so the real
		// push gets -r with the id of the commit that replaced R.
		{name: "jj m, aliases.m = ['git', 'push', '-r', 'mine()'], the step rewriting a commit mine() names", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			f.configure("[aliases]\nm = [\"git\", \"push\", \"-r\", \"mine()\"]")
			f.track("r")
			r := f.commitOn(impl, impl, "main", rolePushMessage("R", "reviewer"))
			f.jj(rev, "new", "main")
			f.bookmark("r", r.commit)
			if got := f.read("mine()"); got.commit != r.commit {
				f.t.Fatalf("the implementer's mine() names %s before the push, want R, %s, alone", got.commit, r.commit)
			}
			want := rolePushSends("r", r, "reviewer", rev)
			want.ids = []rolePushIDs{rolePushOneIDLine("mine()")}
			want.pushed = func(words []string) {
				f.pushedAfterSnapshot([]string{"git", "push", "-r", f.read(r.change).commit})(words)
			}
			return []string{"m"}, want
		}},
		// An enabled table-form alias, which jj 0.45.1-sami and 0.46.0 run and the pod's wrapper
		// expands from its definition. jj 0.38.0 alone refuses it, and both forms push it with the
		// step there too, a limit of the expansion (docs/deployment.md).
		{name: "jj x -b b, [aliases.x] definition = ['git', 'push']", setup: rolePushExpandsTo("[aliases.x]\ndefinition = [\"git\", \"push\"]", []string{"x", "-b", "b"}, pushB)},
		{name: "jj q q git push -b b, aliases.q = ['--quiet']", setup: rolePushQuietTwice("[aliases]\nq = [\"--quiet\"]", []string{"q", "q", "git", "push", "-b", "b"})},
		// v's words hold a command word, q, whose own words are options alone: the second v comes
		// from outside the words of the first v and of q, so jj 0.45.1-sami expands it again.
		{name: "jj v v git push -b b, aliases.v = ['q'] and aliases.q = ['--quiet']", setup: rolePushQuietTwice("[aliases]\nv = [\"q\"]\nq = [\"--quiet\"]", []string{"v", "v", "git", "push", "-b", "b"})},
		// An alias of options alone leaves no command word, and jj 0.45.1-sami puts the default
		// command in front of the line, as the pod's wrapper does. jj 0.38.0 alone refuses the
		// line (`'jj' requires a subcommand`), and both forms push it with the step there too, a
		// limit of the expansion (docs/deployment.md).
		{name: "jj q, aliases.q = ['--quiet'] and ui.default-command = ['git', 'push', '-b', 'b']", setup: rolePushExpandsTo("[aliases]\nq = [\"--quiet\"]\n[ui]\ndefault-command = [\"git\", \"push\", \"-b\", \"b\"]", []string{"q"}, []string{"git", "push", "-b", "b", "--quiet"})},
		// The pod's wrapper follows 32 aliases at most, and refuses the line itself.
		{name: "a chain of 34 aliases ending in ['git', 'push', '-b', 'b']", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			if f.form == rolePushPod {
				f.configure(rolePushChain(34))
				want := rolePushBAbsent(f.workAndB())
				want.exit, want.noMetaedit = 2, true
				return []string{"c01"}, want
			}
			return rolePushExpandsTo(rolePushChain(34), []string{"c01"}, pushB)(f)
		}},
	}
}

// rolePushQuietTwice is a row whose configuration is toml, over C under b (workAndB), whose push
// of args jj 0.45.1-sami expands to `--quiet --quiet git push -b b` and pushes, and jj 0.38.0
// refuses as a recursive alias. Under the tmux form the step runs and the real push runs that
// line, which jj 0.45.1-sami takes and jj 0.38.0 refuses for its --quiet twice (`the argument
// '--quiet' cannot be used multiple times`), after one metaedit, a limit of the expansion
// (docs/deployment.md); the remote is then unchanged. The pod's wrapper follows a name once, and
// refuses the line itself, exit 2, before anything runs.
func rolePushQuietTwice(toml string, args []string) func(f *rolePushFixture) ([]string, rolePushWant) {
	return func(f *rolePushFixture) ([]string, rolePushWant) {
		f.configure(toml)
		c := f.workAndB()
		if f.form == rolePushPod {
			want := rolePushBAbsent(c)
			want.exit, want.noMetaedit = 2, true
			return args, want
		}
		expanded := []string{"--quiet", "--quiet", "git", "push", "-b", "b"}
		if code, _ := f.aloneSends(expanded); code != 0 {
			want := rolePushBAbsent(c)
			want.exit, want.same, want.others, want.metaedits = code, nil, true, 1
			return args, want
		}
		want := rolePushSends("b", c, "implementer", rolePushImplementer)
		want.pushed = f.pushedAfterSnapshot(expanded)
		return args, want
	}
}

// A command that jj expands into a push sends each commit as the step left it, on both forms: an
// alias of an alias; a line with no command word, `-b b` or `--`, which jj puts ui.default-command
// in front of, as a list or as one string naming an alias; an alias whose own words hold a
// revision argument that the step's rewrite makes name other commits, which the real push gets as
// the ids of the commits it named before the step; an enabled table-form alias; an alias of
// options alone named twice, by itself and through another alias; an alias of options alone in
// front of a push as the default command; and a chain of 34 aliases. Each sends the trailer's
// author, the step prints its rewrite, the recording jj sees the real push run the expanded line
// with the step's words, and no change is divergent. Where a release or a form refuses the
// expanded line, the row expects that refusal: jj 0.38.0 refuses `--quiet` twice, after the step,
// and the pod's wrapper refuses a name it would follow twice and a chain past 32 aliases, before
// anything runs.
func TestAnExpandedPushRunsTheStepOnTheExpandedLine(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushExpandedRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				args, want := row.setup(f)
				edits := 0
				if want.metaedits != 0 {
					edits = f.metaedits()
				}
				divergent := f.divergence()
				code, _, stderr := f.push(rolePushImplementer, args...)
				f.check(want, args, code, stderr, divergent, edits)
				switch {
				case want.exit == 2 && form == rolePushPod:
					if !strings.Contains(stderr, rolePushPodRefusalPrefix) {
						t.Errorf("the push printed\n%s\nwant the pod's wrapper's refusal, %q", stderr, rolePushPodRefusalPrefix)
					}
				case want.exit != 0:
					if !strings.Contains(stderr, "the argument '--quiet' cannot be used multiple times") {
						t.Errorf("the push printed\n%s\nwant jj's refusal of --quiet twice", stderr)
					}
				}
			})
		})
	}
}

// ranAsWritten checks that the recording jj ran calls, reads `jj config list` reads first and then
// the role's words, args, byte for byte, once, and nothing else: no command of the step.
func (f *rolePushFixture) ranAsWritten(calls []rolePushCall, reads int, args []string) {
	f.t.Helper()
	if len(calls) != reads+1 || !slices.Equal(calls[len(calls)-1].args, args) || slices.ContainsFunc(calls[:len(calls)-1], func(c rolePushCall) bool { return !c.isConfigRead() }) {
		f.t.Errorf("the recording jj ran %v, want %d `jj config list` read(s), then the role's words %q, and nothing else", calls, reads, args)
	}
}

// noStepLine checks that stderr holds no line of legion jj's.
func (f *rolePushFixture) noStepLine(stderr string) {
	f.t.Helper()
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.HasPrefix(line, "legion jj:") {
			f.t.Errorf("the command printed %q, want no line of legion jj's", line)
		}
	}
}

// rolePushHelpDefault is a default command that is a push, which a word of help or version must
// not run.
const rolePushHelpDefault = "[ui]\ndefault-command = [\"git\", \"push\", \"-b\", \"b\"]"

// rolePushAsWrittenRow is a command a role runs under the tmux form, with a recording jj ahead of
// the real one: setup makes the repository and answers C, which b is on.
type rolePushAsWrittenRow struct {
	name string
	args []string
	// toml is the repository's configuration.
	toml string
	// reads is the number of `jj config list` reads the recording jj must see before the
	// command.
	reads int
	// extra are variables after the role's environment.
	extra []string
	// elsewhere is whether the command runs from the directory above the repository, with -R
	// naming it.
	elsewhere bool
	// noTable is whether the role's process has no table.
	noTable bool
	// sends is whether jj alone sends b, at C as it was.
	sends bool
}

// rolePushAsWrittenRows are the commands that run as the role wrote them: an alias that is no push
// and a built-in command, each with jj's own output; a push written as `git push`; help and version
// with a push as the default command; shell completion; an alias that jj reads from no file in the
// working directory's configuration; and a process with no table.
var rolePushAsWrittenRows = []rolePushAsWrittenRow{
	{name: "jj l, aliases.l = ['log', '-r', '@']", args: []string{"l"}, toml: "[aliases]\nl = [\"log\", \"-r\", \"@\"]", reads: 1},
	{name: "jj log", args: []string{"log"}},
	{name: "jj --help", args: []string{"--help"}, toml: rolePushHelpDefault},
	{name: "jj -h", args: []string{"-h"}, toml: rolePushHelpDefault},
	{name: "jj --version", args: []string{"--version"}, toml: rolePushHelpDefault},
	{name: "jj -V", args: []string{"-V"}, toml: rolePushHelpDefault},
	{name: "jj -hV", args: []string{"-hV"}, toml: rolePushHelpDefault},
	{name: "COMPLETE=bash jj p", args: []string{"p"}, toml: "[aliases]\np = [\"git\", \"push\"]", extra: []string{"COMPLETE=bash"}},
	{name: "jj q -b b, aliases.q = ['git', 'push'] only in the repository -R names, run from another directory", args: []string{"-R", "repo", "q", "-b", "b"}, toml: "[aliases]\nq = [\"git\", \"push\"]", reads: 1, elsewhere: true},
	{name: `jj --config 'aliases.q=["git","push"]' q -b b`, args: []string{"--config", `aliases.q=["git","push"]`, "q", "-b", "b"}, reads: 1},
	{name: "no table, jj p -b b", args: []string{"p", "-b", "b"}, toml: "[aliases]\np = [\"git\", \"push\"]", noTable: true, sends: true},
}

// A command that is no push runs as the role wrote it, and one whose word is a built-in command
// reads nothing but before a push the step runs on. Under the tmux form, with a recording jj ahead
// of the real one: `jj l` with an alias that is no push prints what jj alone prints, and the
// recording jj sees one `jj config list`, then the role's words byte for byte, and no command of
// the step; `jj log` runs no `jj config list`; `jj git push --bookmark legion/K-1` runs exactly
// one, and first, the read of the push's configuration that legion jj makes before the step's
// first command (`jj --ignore-working-copy --color=never config list -T '""'`, with none of the
// push's -R and --config words, as it holds none), and no read of the aliases; `jj --help`, `-h`,
// `--version`, `-V` and `-hV` with a push as the default command, and `jj p` under COMPLETE=bash,
// read nothing, run the role's words alone and send nothing, as jj alone; `jj -R repo q -b b` from
// the directory above the repository, whose own configuration alone defines q, and `jj --config
// 'aliases.q=…' q -b b` end in jj's own refusal, as jj alone prints it, and send nothing; a
// process with no table reads nothing and runs `jj p -b b` as jj alone does, which sends b as it
// was. The pod's form of these is the pod's wrapper's (packages/daemon/docker/jj), held by
// internal/leakguard's tests.
func TestACommandThatIsNoPushRunsAsTheRoleWroteIt(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushAsWrittenRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newRolePushFixture(t, rolePushTmux, rolePushRecords)
			if row.toml != "" {
				f.configure(row.toml)
			}
			c := f.workAndB()
			dir := f.repo
			if row.elsewhere {
				dir = f.root
			}
			want := rolePushBAbsent(c)
			if row.sends {
				want = rolePushBAsItWas(c)
			}
			if row.noTable {
				f.table = nil
			} else {
				aloneCode, aloneOut, aloneErr := f.aloneIn(dir, rolePushImplementer, row.extra, row.args...)
				if aloneCode != 0 {
					want.exit = aloneCode
				}
				code, stdout, stderr := f.runIn(f.path, dir, rolePushImplementer, row.extra, row.args...)
				if code != aloneCode || stdout != aloneOut || stderr != aloneErr {
					t.Errorf("the command exited %d and printed\n%s\n%s\nwant %d and\n%s\n%s\nas jj alone", code, stdout, stderr, aloneCode, aloneOut, aloneErr)
				}
				f.check(want, row.args, code, stderr, f.divergence(), 0)
				f.ranAsWritten(f.calls(), row.reads, row.args)
				return
			}
			code, _, stderr := f.runIn(f.path, dir, rolePushImplementer, row.extra, row.args...)
			f.check(want, row.args, code, stderr, f.divergence(), 0)
			f.noStepLine(stderr)
			f.ranAsWritten(f.calls(), row.reads, row.args)
		})
	}
	t.Run("jj git push --bookmark legion/K-1", func(t *testing.T) {
		t.Parallel()
		f := newRolePushFixture(t, rolePushTmux, rolePushRecords)
		c := f.commitOn(rolePushReviewer, rolePushImplementer, "main", rolePushMessage("work", "implementer"))
		f.bookmark("legion/K-1", c.commit)
		f.pushAndCheck(rolePushBookmark, rolePushSends("legion/K-1", c, "implementer", rolePushImplementer))
		calls := f.calls()
		read := []string{"--ignore-working-copy", "--color=never", "config", "list", "-T", `""`}
		reads := 0
		for _, call := range calls {
			if call.isConfigRead() {
				reads++
			}
		}
		if reads != 1 || !slices.Equal(calls[0].args, read) {
			t.Errorf("the recording jj ran %v, want one `jj config list` before a push written as git push, first, with the words %q: the read of the push's configuration, and no read of the aliases", calls, read)
		}
	})
}

// rolePushUnexpandableRows are the aliases that the pane cannot expand, each with the could-not
// line it prints under the tmux form, and whether the pod's wrapper refuses it.
var rolePushUnexpandableRows = []struct {
	name, toml string
	args       []string
	line       string
	podRefuses bool
}{
	{name: "aliases.x = 'git push' (a string)", toml: "[aliases]\nx = \"git push\"", args: []string{"x", "-b", "b"}, podRefuses: true,
		line: `legion jj: could not expand aliases.x: it is "git push", which is no list of strings; the command runs as written`},
	{name: "aliases.x = [1, 2]", toml: "[aliases]\nx = [1, 2]", args: []string{"x", "-b", "b"}, podRefuses: true,
		line: "legion jj: could not expand aliases.x: it is [1,2], which is no list of strings; the command runs as written"},
	{name: "aliases.a = ['b'] and aliases.b = ['a'] (a cycle)", toml: "[aliases]\na = [\"b\"]\nb = [\"a\"]", args: []string{"a", "-b", "b"}, podRefuses: true,
		line: "legion jj: could not expand aliases.a: jj would expand it again inside its own expansion, a cycle jj refuses; the command runs as written"},
	{name: "[aliases.x] definition = ['git', 'push', '-b', 'b'] and enabled = false", toml: "[aliases.x]\ndefinition = [\"git\", \"push\", \"-b\", \"b\"]\nenabled = false", args: []string{"x"},
		line: "legion jj: could not expand aliases.x: its enabled is false, which jj 0.45.1-sami ignores and jj 0.46.0 obeys; the command runs as written"},
}

// An alias the pane cannot expand runs as the role wrote it, with no step: a string, a list of
// numbers and a cycle, which jj itself refuses, and a table-form alias with enabled = false, which
// jj 0.45.1-sami runs anyway and 0.38.0 refuses. Under the tmux form, legion jj prints one line
// `legion jj: could not expand <what>: <why>; the command runs as written`, then jj prints and
// exits as jj alone does with the same words, and the recording jj sees one `jj config list` and
// then the role's words. Under the pod form the pod's wrapper refuses the first three itself, exit
// 2, and leaves the fourth to jj. Where jj alone sends the fourth (its dry run exits 0), both forms
// send b as it was, with the other role's author, a limit of the expansion (docs/deployment.md);
// else nothing reaches the remote.
func TestAnAliasThePaneCannotExpandRunsAsWritten(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushUnexpandableRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				f.configure(row.toml)
				c := f.workAndB()
				want := rolePushBAbsent(c)
				want.noMetaedit = true
				aloneCode, aloneErr := 0, ""
				if code, _, _ := f.alone(rolePushImplementer, append(slices.Clone(row.args), "--dry-run", "--ignore-working-copy")...); code == 0 {
					want = rolePushBAsItWas(c)
					want.noMetaedit = true
				} else {
					aloneCode, _, aloneErr = f.alone(rolePushImplementer, row.args...)
					want.exit = aloneCode
				}
				if form == rolePushPod && row.podRefuses {
					want = rolePushBAbsent(c)
					want.exit, want.noMetaedit = 2, true
				}
				code, _, stderr := f.push(rolePushImplementer, row.args...)
				first, rest, _ := strings.Cut(stderr, "\n")
				switch {
				case form == rolePushTmux:
					if first != row.line {
						t.Errorf("the push's first line is %q, want %q", first, row.line)
					}
					if want.exit != 0 && rest != aloneErr {
						t.Errorf("the push printed after its line\n%s\nwant what jj alone prints\n%s", rest, aloneErr)
					}
					f.check(want, row.args, code, rest, f.divergence(), 0)
					f.ranAsWritten(f.calls(), 1, row.args)
				case row.podRefuses:
					if !strings.HasPrefix(stderr, rolePushPodRefusalPrefix) {
						t.Errorf("the push printed\n%s\nwant the pod's wrapper's refusal, %q", stderr, rolePushPodRefusalPrefix)
					}
					f.check(want, row.args, code, stderr, f.divergence(), 0)
				default:
					f.noStepLine(stderr)
					f.check(want, row.args, code, stderr, f.divergence(), 0)
				}
			})
		})
	}
}

// An alias that a [[--scope]] defines for `jj config` alone, which jj runs for no other command, is
// read as a push on both forms, as the pod's wrapper read it at the base: `jj s`, which jj alone
// refuses as an unrecognized subcommand, sends C under b as the step left it, the real push
// running the alias's words with the step's, a limit of the expansion (docs/deployment.md).
func TestAScopedAliasIsReadAsThePodReadsIt(t *testing.T) {
	t.Parallel()
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, rolePushRecords)
		f.configure("[[--scope]]\n--when.commands = [\"config\"]\n[--scope.aliases]\ns = [\"git\", \"push\", \"-b\", \"b\"]")
		c := f.workAndB()
		if code, _, stderr := f.alone(rolePushImplementer, "s"); code == 0 || !strings.Contains(stderr, "unrecognized subcommand 's'") {
			t.Fatalf("jj alone exited %d on `jj s`, want its refusal of an unrecognized subcommand:\n%s", code, stderr)
		}
		want := rolePushSends("b", c, "implementer", rolePushImplementer)
		want.pushed = f.pushedAfterSnapshot([]string{"git", "push", "-b", "b"})
		f.pushAndCheck([]string{"s"}, want)
	})
}

// rolePushCapturedWarning is the warning that the jj under test prints of an alias named for a
// built-in command, as testdata/jj-config-list/<release>/builtin.txt captured it from that release
// (`log`), naming name in its place.
func rolePushCapturedWarning(t *testing.T, real, name string) string {
	t.Helper()
	version, err := exec.Command(real, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(version))
	if len(fields) < 2 {
		t.Fatalf("jj --version printed %q", version)
	}
	releases, err := os.ReadDir("testdata/jj-config-list")
	if err != nil {
		t.Fatal(err)
	}
	for _, release := range releases {
		if !strings.HasPrefix(fields[1], release.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata/jj-config-list", release.Name(), "builtin.txt"))
		if err != nil {
			t.Fatal(err)
		}
		_, stderr, _ := strings.Cut(string(data), "\n--- stderr\n")
		warning, _, _ := strings.Cut(stderr, "\n")
		if !strings.Contains(warning, "'log'") {
			t.Fatalf("%s/builtin.txt's warning %q names no 'log'", release.Name(), warning)
		}
		return strings.Replace(warning, "'log'", "'"+name+"'", 1)
	}
	t.Fatalf("testdata/jj-config-list holds no capture of jj %s", fields[1])
	return ""
}

// A command word that jj takes for a built-in command is never expanded, whatever the pane's list
// of built-in names holds: with aliases.fakebuiltin = ['git', 'push'], and a recording jj that adds
// to the standard error of each `jj config list` the warning the jj under test prints of an alias
// named for a built-in command (testdata/jj-config-list), naming fakebuiltin, `jj fakebuiltin -b b`
// under the tmux form runs the role's words after the one read, with no step, so the real jj, which
// does expand it, sends b as it was. The pod's wrapper reads no such warning.
func TestACommandWordJJTakesForBuiltinIsNeverExpanded(t *testing.T) {
	t.Parallel()
	warning := rolePushCapturedWarning(t, rolePushRealJJ(t), "fakebuiltin")
	if strings.ContainsAny(warning, "\"$`\\") {
		t.Fatalf("the captured warning %q holds a character the recording jj's shell would read", warning)
	}
	behaviour := `case " $* " in *" config list "*) "$real" "$@"; status=$?; echo "` + warning + `" >&2; ran=1 ;; esac`
	f := newRolePushFixture(t, rolePushTmux, behaviour)
	f.configure("[aliases]\nfakebuiltin = [\"git\", \"push\"]")
	c := f.workAndB()
	args := []string{"fakebuiltin", "-b", "b"}
	code, _, stderr := f.push(rolePushImplementer, args...)
	f.check(rolePushBAsItWas(c), args, code, stderr, f.divergence(), 0)
	f.noStepLine(stderr)
	f.ranAsWritten(f.calls(), 1, args)
}

// rolePushHelpRows are lines that clap reads as help, each run over C under b with a push as the
// default command: `jj -hV`, which the pane runs as written and the pod's wrapper hands legion jj
// as `git push -b b -hV` (podPushHelp), and `git push` with a cluster of short flags that starts
// with -h.
var rolePushHelpRows = []struct {
	name        string
	args        []string
	podPushHelp bool
}{
	{name: "jj -hV, ui.default-command = ['git', 'push', '-b', 'b']", args: []string{"-hV"}, podPushHelp: true},
	{name: "jj git push -b b -hV", args: []string{"git", "push", "-b", "b", "-hV"}},
	{name: "jj git push -hb b", args: []string{"git", "push", "-hb", "b"}},
}

// A word of one dash that starts with -h or -V is help or version to clap wherever it stands, and
// runs no step: measured with jj 0.45.1-sami and 0.38.0, `jj git push -b b -hV` and `jj git push
// -hb b` print the push's help and push nothing, and `jj -hV` with a push as the default command
// prints jj's help and pushes nothing. On both forms each exits as jj alone does, the recording jj
// sees no metaedit, legion jj prints no line, and the remote holds no b, and each prints what jj
// alone prints, but for `jj -hV` in a pod: the pod's wrapper puts the default command in front of
// it, and jj prints the push's help.
func TestAHelpOrVersionClusterRunsNoStep(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushHelpRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				f.configure(rolePushHelpDefault)
				c := f.workAndB()
				aloneArgs := row.args
				if form == rolePushPod && row.podPushHelp {
					aloneArgs = append([]string{"git", "push", "-b", "b"}, row.args...)
				}
				aloneCode, aloneOut, _ := f.alone(rolePushImplementer, aloneArgs...)
				code, stdout, stderr := f.push(rolePushImplementer, row.args...)
				if code != aloneCode || stdout != aloneOut {
					t.Errorf("the command exited %d and printed\n%s\nwant %d and\n%s\nas jj alone prints for %q", code, stdout, aloneCode, aloneOut, aloneArgs)
				}
				want := rolePushBAbsent(c)
				want.exit, want.noMetaedit = aloneCode, true
				f.check(want, row.args, code, stderr, f.divergence(), 0)
				f.noStepLine(stderr)
			})
		})
	}
}

// A command that is no push and comes back into the pane's jj with the same words, through a
// host's jj script after it that calls jj by PATH, runs there at once: under the tmux form, `jj l`
// with an alias that is no push prints and exits as jj alone does, and the recording jj sees one
// read of the aliases and then `l`; `jj x -b b` with aliases.x = "git push" prints its could-not
// line once, then jj's own refusal as jj alone prints it, and the recording jj sees one read and
// then the role's words. The pod's wrapper runs its jj by its full path, so a host's jj after it
// is never reached there.
func TestACommandThroughAHostJJScriptIsReadOnce(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, toml, line string
		args             []string
	}{
		{name: "jj l, aliases.l = ['log', '-r', '@', '--no-graph']", toml: "[aliases]\nl = [\"log\", \"-r\", \"@\", \"--no-graph\"]", args: []string{"l"}},
		{name: "jj x -b b, aliases.x = 'git push'", toml: "[aliases]\nx = \"git push\"", args: []string{"x", "-b", "b"},
			line: `legion jj: could not expand aliases.x: it is "git push", which is no list of strings; the command runs as written`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newRolePushFixture(t, rolePushTmux, rolePushRecords)
			f.configure(row.toml)
			c := f.workAndB()
			aloneCode, aloneOut, aloneErr := f.alone(rolePushImplementer, row.args...)
			code, stdout, stderr := f.pushOn(rolePushHostPath(f), rolePushImplementer, row.args...)
			wantErr := aloneErr
			if row.line != "" {
				wantErr = row.line + "\n" + aloneErr
			}
			if code != aloneCode || stdout != aloneOut || stderr != wantErr {
				t.Errorf("the command exited %d and printed\n%s\n%s\nwant %d and\n%s\n%s", code, stdout, stderr, aloneCode, aloneOut, wantErr)
			}
			want := rolePushBAbsent(c)
			want.exit, want.noMetaedit = aloneCode, true
			f.check(want, row.args, code, strings.TrimPrefix(stderr, row.line), f.divergence(), 0)
			f.ranAsWritten(f.calls(), 1, row.args)
		})
	}
}
