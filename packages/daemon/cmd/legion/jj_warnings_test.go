package main

import (
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The tests in this file are those of what jj prints on standard error for a command run through
// each form of Legion's jj, as a role runs it, through the shared fixture
// (jj_push_fixture_test.go): the warnings jj prints once and those it prints on every command,
// which the pod's wrapper's reads, the pane's reads and the step's commands must print as often as
// jj alone prints them. Like jj_push_test.go and jj_expand_test.go, this file names nothing the
// change adds, so it compiles and runs at the base too; it reads jj's colours by a pattern of its
// own (rolePushSGR). The tests of the filter itself, which read the change's own Go names, are in
// jj_warnings_read_test.go.

// rolePushCopiedWarning starts the warning that jj prints for the first command in a copy of a
// repository that has a configuration of its own, and for no command after it
// (lib/src/secure_config.rs).
const rolePushCopiedWarning = "Your repo appears to have been copied"

// rolePushOverridesWarning starts the warning that jj prints on every command of an alias that has
// the name of a built-in command (cli/src/cli_util.rs).
const rolePushOverridesWarning = "Cannot define an alias that overrides the built-in command"

// rolePushSGR is an escape sequence of jj's colours, which a line carries where ui.color is
// always.
var rolePushSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// aloneLine is the line jj alone runs for args, the line the form runs: under the pod's form, the
// git wrapper's first layer in front of args, a podForces word then `--config-file=/dev/null`, as
// the pod's jj puts that layer on every line it runs; under the tmux form, args as written.
func (f *rolePushFixture) aloneLine(args []string) []string {
	if f.form == rolePushPod {
		return slices.Concat([]string{f.podForces[0], "--config-file=/dev/null"}, args)
	}
	return args
}

// A warning that jj prints once reaches the role once: in a copy, made with `cp -a`, of a
// repository with a configuration of its own, which jj's secure configuration needs to tell that
// the repository was copied, the first command through the form's jj prints "Your repo appears to
// have been copied" once, as jj alone prints it for the line the form runs (aloneLine), as its
// first command in another such copy, and a second command in the copy prints it no more. `jj st`
// and `jj status`, each its own second command: `st` is jj's alias of `status`, which the pod's
// wrapper and the pane's legion jj each read first, and that read is the first command in the
// copy, which takes the warning; `status` is a built-in command, which neither reads. `jj git push
// -b b`, a push of C, a commit the other role made and the implementer described with its own
// trailer, then `jj st`: the push sends C with the implementer's author, so the step runs, and its
// first command in the copy takes the warning where the pod's wrapper's read of
// git.executable-path has not taken it first. `jj -R <copy> git push -b b` from the directory
// above the copy, then `jj -R <copy> st` there: the same push, of a repository that the working
// directory is not in. jj alone makes its push in a repository of its own, made the same way, with
// a remote of its own.
func TestAWarningJJPrintsOnceReachesTheRoleOnce(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	for _, row := range []struct {
		name string
		// args are the first command's words and next the second's, each "<copy>" in them the
		// copy's path.
		args, next []string
		push       bool
		// outside is whether each command runs from the directory above the copy.
		outside bool
	}{
		{name: "jj st", args: []string{"st"}, next: []string{"st"}},
		{name: "jj status", args: []string{"status"}, next: []string{"status"}},
		{name: "jj git push -b b", args: []string{"git", "push", "-b", "b"}, next: []string{"st"}, push: true},
		{name: "jj -R <copy> git push -b b from outside the copy", args: []string{"-R", "<copy>", "git", "push", "-b", "b"}, next: []string{"-R", "<copy>", "st"}, push: true, outside: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				// made makes the repository a fixture copies: its own configuration, and, for a
				// push, C under b.
				made := func(f *rolePushFixture) rolePushChange {
					f.configure("[ui]\neditor = \"true\"")
					if row.push {
						return f.workAndB()
					}
					return rolePushChange{}
				}
				// copyOf copies f's repository to name under f's root, and answers the copy and
				// the directory the row's commands run from.
				copyOf := func(f *rolePushFixture, name string) (dir, where string) {
					t.Helper()
					dir = filepath.Join(f.root, name)
					f.runner(f.setupPath, impl).Run(f.root, "cp", "-a", f.repo, dir)
					if row.outside {
						return dir, f.root
					}
					return dir, dir
				}
				// in is args with each "<copy>" the path of dir.
				in := func(args []string, dir string) []string {
					words := slices.Clone(args)
					for i, word := range words {
						if word == "<copy>" {
							words[i] = dir
						}
					}
					return words
				}
				f := newRolePushFixture(t, form, rolePushRecords)
				c := made(f)
				twin := f
				var divergent map[string]int
				if row.push {
					twin = newRolePushFixture(t, form, "")
					made(twin)
					divergent = f.divergence()
				}
				aloneDir, aloneWhere := copyOf(twin, "alone")
				line := twin.aloneLine(in(row.args, aloneDir))
				code, _, stderr := twin.aloneIn(aloneWhere, impl, nil, line...)
				if n := strings.Count(stderr, rolePushCopiedWarning); code != 0 || n != 1 {
					t.Fatalf("jj alone's first %q in a copy exited %d and printed %q %d times, want 0 and once:\n%s", line, code, rolePushCopiedWarning, n, stderr)
				}
				dir, where := copyOf(f, "copy")
				args, next := in(row.args, dir), in(row.next, dir)
				code, _, stderr = f.runIn(f.path, where, impl, nil, args...)
				nextCode, _, nextErr := f.runIn(f.path, where, impl, nil, next...)
				for i, run := range []struct {
					args   []string
					code   int
					stderr string
				}{{args, code, stderr}, {next, nextCode, nextErr}} {
					n := strings.Count(run.stderr, rolePushCopiedWarning)
					t.Logf("command %d in the copy, %q, printed %q %d times", i+1, run.args, rolePushCopiedWarning, n)
					if want := 1 - i; run.code != 0 || n != want {
						t.Errorf("command %d in the copy, %q, exited %d and printed %q %d times, want 0 and %d times, as jj alone prints it:\n%s", i+1, run.args, run.code, rolePushCopiedWarning, n, want, run.stderr)
					}
				}
				if row.push {
					// The checks read the copy, which the push went from.
					f.repo = dir
					f.check(rolePushSends("b", c, "implementer", impl), args, code, stderr, divergent, 0)
				}
			})
		})
	}
}

// A warning that jj prints on every command prints as often as jj alone prints it for the line the
// form runs: with aliases.log = ['log', '-r', '@'] in the user's configuration (the file `jj config
// path --user` names under the fixture's own HOME), `jj st`, `jj status` and `jj git push -b b`,
// a push of C, a commit the other role made and the implementer described with its own trailer,
// print "Cannot define an alias that overrides the built-in command" as many times as jj alone
// prints it in a repository made the same way, for the line the form runs (aloneLine): with the
// git wrapper's first layer in front of the line under the pod's form, which puts that layer on
// every line it runs, and on which jj prints such a warning twice (warn_if_args_mismatch), and on
// the line as written under the tmux form. The push sends C with the implementer's author after
// the step's one snapshot, so the step's snapshot is among the commands that print nothing of that
// warning, and so, under the pod's form, is the wrapper's read of git.executable-path; under both
// forms, the read of `st`. Each of the three prints it so with ui.color = "always" in the
// repository's configuration as well, under which jj prints the warning in its colours on every
// command, a read's --color=never notwithstanding, since jj prints it before it reads --color. The
// test sets ui.color after the setup's reads, and unsets the alias and ui.color after the push, so
// that its own reads of what the push left, which read jj's standard error with its standard
// output, read no warning and no colour.
func TestAWarningJJPrintsOnEveryCommandPrintsAsOftenAsJJAlonePrintsIt(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	const toml = "[aliases]\nlog = [\"log\", \"-r\", \"@\"]"
	for _, row := range []struct {
		name   string
		args   []string
		push   bool
		colour bool
	}{
		{name: "jj st", args: []string{"st"}},
		{name: "jj status", args: []string{"status"}},
		{name: "jj git push -b b", args: []string{"git", "push", "-b", "b"}, push: true},
		{name: "jj st with ui.color always", args: []string{"st"}, colour: true},
		{name: "jj status with ui.color always", args: []string{"status"}, colour: true},
		{name: "jj git push -b b with ui.color always", args: []string{"git", "push", "-b", "b"}, push: true, colour: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				colour := func(f *rolePushFixture) {
					if row.colour {
						f.configure("[ui]\ncolor = \"always\"")
					}
				}
				twin := newRolePushFixture(t, form, "")
				twin.configureIn("--user", toml)
				twin.workAndB()
				colour(twin)
				line := twin.aloneLine(row.args)
				code, _, aloneErr := twin.alone(impl, line...)
				alone := strings.Count(aloneErr, rolePushOverridesWarning)
				if code != 0 || alone == 0 {
					t.Fatalf("jj alone exited %d on %q and printed %q %d times, want 0 and at least once:\n%s", code, line, rolePushOverridesWarning, alone, aloneErr)
				}
				if row.colour && !rolePushSGR.MatchString(aloneErr) {
					t.Fatalf("jj alone printed no colours on %q with ui.color = \"always\":\n%s", line, aloneErr)
				}
				f := newRolePushFixture(t, form, rolePushRecords)
				f.configureIn("--user", toml)
				c := f.workAndB()
				var divergent map[string]int
				if row.push {
					divergent = f.divergence()
				}
				colour(f)
				code, _, stderr := f.push(impl, row.args...)
				n := strings.Count(stderr, rolePushOverridesWarning)
				t.Logf("jj alone printed %q %d times for %q, and the form's jj %d times for %q", rolePushOverridesWarning, alone, line, n, row.args)
				if n != alone {
					t.Errorf("the command printed %q %d times, want %d, as jj alone prints it for %q:\n%s", rolePushOverridesWarning, n, alone, line, stderr)
				}
				if !row.push {
					if code != 0 {
						t.Errorf("the command exited %d, want 0:\n%s", code, stderr)
					}
					return
				}
				want := rolePushSends("b", c, "implementer", impl)
				want.snapshots = rolePushTakesOneSnapshot
				f.jj(impl, "config", "unset", "--user", "aliases.log")
				if row.colour {
					f.jj(impl, "config", "unset", "--repo", "ui.color")
				}
				f.check(want, row.args, code, stderr, divergent, 0)
			})
		})
	}
}

// A command whose reads find no key prints no warning that jj alone does not print: a bare `jj`,
// where no configuration sets ui.default-command, and `jj nosuch`, which no configuration names an
// alias, print each line that starts `Warning:`, read with jj's colours left out (rolePushSGR), as
// many times as jj alone prints it for the line the form runs (aloneLine), and exit as jj alone
// exits. The pod's wrapper reads ui.default-command for the one and aliases.nosuch for the other,
// keys no configuration sets, of which jj notes `No matching config key for`; the pane's legion jj
// reads every alias and ui.default-command in one read that names no key.
func TestACommandWhoseReadsFindNoKeyPrintsNoWarningJJAloneDoesNotPrint(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	warnings := func(stderr string) map[string]int {
		counts := map[string]int{}
		for line := range strings.SplitSeq(rolePushSGR.ReplaceAllString(stderr, ""), "\n") {
			if strings.HasPrefix(line, "Warning:") {
				counts[line]++
			}
		}
		return counts
	}
	for _, row := range []struct {
		name string
		args []string
	}{
		{name: "jj", args: nil},
		{name: "jj nosuch", args: []string{"nosuch"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, "")
				line := f.aloneLine(row.args)
				aloneCode, _, aloneErr := f.alone(impl, line...)
				code, _, stderr := f.push(impl, row.args...)
				alone, got := warnings(aloneErr), warnings(stderr)
				t.Logf("jj alone exited %d and printed the warnings %v for %q, and the form's jj exited %d and printed %v for %q", aloneCode, alone, line, code, got, row.args)
				if code != aloneCode || !maps.Equal(got, alone) {
					t.Errorf("the command exited %d and printed the warnings %v, want %d and %v, as jj alone does for %q:\n%s", code, got, aloneCode, alone, line, stderr)
				}
			})
		})
	}
}
