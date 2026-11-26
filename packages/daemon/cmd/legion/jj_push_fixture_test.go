package main

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

// This file holds the fixture of the tests of the step that runs before every `jj git push`
// (jj_push_test.go, whose comment describes the two forms of Legion's jj a push runs through): the
// forms, the repository a role pushes from, the recording jj, and the checks of what a push
// leaves. The other jj_push_*_test.go files, jj_expand_test.go, jj_expand_read_test.go and
// jj_warnings_test.go use it too. As jj_push_test.go does, it names nothing the step's change adds.

// rolePushVariable is the table of every role's identity that the daemon writes into a role
// process's environment.
const rolePushVariable = "LEGION_ROLE_IDENTITIES"

// rolePushBoundVariable is the variable that this package's test binary, run as legion, reads as
// the step's bound, in bytes, of a word it builds of ids; the legion binary reads no such variable.
const rolePushBoundVariable = "LEGION_TEST_JJ_WORD_BOUND"

// rolePushRevisionBound is the step's own bound of a word of ids, in bytes, which a push keeps
// where its environment holds no rolePushBoundVariable: the bound under which the step keeps the
// word of a revision argument that it gives the push as the ids of its commits, which each line of
// those ids names. The tests of every jj_push_*_test.go file share it.
const rolePushRevisionBound = 100_000

// rolePushArgMaxVariable is the variable that this package's test binary, run as legion, reads as
// the step's limit, in bytes, of the push's command and of each metaedit's; the legion binary reads
// no such variable.
const rolePushArgMaxVariable = "LEGION_TEST_JJ_ARG_MAX"

// rolePushIdentity is the identity a role's process has, its JJ_USER and JJ_EMAIL.
type rolePushIdentity struct{ name, email string }

// The two roles these tests stand for, with the names and addresses of the GitHub Apps they work
// as: a name with brackets in it is what a deployment's identity is, and what a `--config
// user.name=…` must still carry whole.
var (
	rolePushImplementer = rolePushIdentity{"legion-implementer[bot]", "100000001+legion-implementer[bot]@users.noreply.github.com"}
	rolePushReviewer    = rolePushIdentity{"legion-reviewer[bot]", "100000002+legion-reviewer[bot]@users.noreply.github.com"}
	// rolePushNobody is no identity at all: jj makes a commit with an empty author under it.
	rolePushNobody = rolePushIdentity{}
)

// rolePushTable is the table of the two roles, as the daemon writes it: one JSON object, keys
// sorted.
const rolePushTable = `{"implementer":{"name":"legion-implementer[bot]","email":"100000001+legion-implementer[bot]@users.noreply.github.com"},"reviewer":{"name":"legion-reviewer[bot]","email":"100000002+legion-reviewer[bot]@users.noreply.github.com"}}`

// rolePushBookmark is the push most rows make: the role's bookmark, by name.
var rolePushBookmark = []string{"git", "push", "--bookmark", "legion/K-1"}

// rolePushMessage is a description whose trailer block names role.
func rolePushMessage(title, role string) string {
	return title + "\n\nLegion-Role: " + role + "\n"
}

// rolePushForm is a form of Legion's jj a test pushes through.
type rolePushForm string

const (
	rolePushPod  rolePushForm = "pod"
	rolePushTmux rolePushForm = "tmux"
)

var rolePushForms = []rolePushForm{rolePushPod, rolePushTmux}

// What a form changes, and what it does not (newRolePushWrapper, podDir, install). A role's `jj
// <args>` reaches the push step through the form's jj, first on PATH:
//   - pod: packages/daemon/docker/jj, a shell script. It expands the line as jj does (aliases,
//     ui.default-command) by its own reads of jj's configuration, refuses a git jj's configuration
//     names other than the git wrapper, filters the warnings its reads print (once_only), puts
//     `--config=git.executable-path="<git wrapper>"` in front of the line and after a push
//     (podForces), and hands a line it read as a push to `legion jj --real <next jj>`.
//   - tmux: workerbin.Install's bin/jj, `legion jj --after <bin> -- <args>`: legion jj expands the
//     line itself, in Go, and runs the next jj on PATH after bin.
// Each form then runs the same step, cmd/legion/jj.go's, on the push line it was handed: the trailer
// and author rules, the rewrite of a revision argument to the commits it named, the bounds of a
// word and of a command, the snapshot, the dry run and its reading, the retries. For a line that
// names no alias, default command or configuration layer of the role's, the step gets the same
// words in both forms but for the pod's git.executable-path words, global options the step skips
// (rolePushGlobalValues), whose bytes count toward the command's size as every word does, and the
// step gets its environment from the form, whose PATH differs. So a case that tests the expansion,
// the wrappers' refusals, their filtering of warnings, which jj a form runs, or a form's own
// command (help, a jj that jj starts) runs in both forms (rolePushEachForm), and a case of the
// step's own logic runs in one (rolePushStepLogic), with one case of each family of that logic
// still in both as the check that the forms agree.

// rolePushEachForm runs test under each form, each in a parallel subtest named for the form.
func rolePushEachForm(t *testing.T, test func(t *testing.T, form rolePushForm)) {
	for _, form := range rolePushForms {
		t.Run(string(form), func(t *testing.T) {
			t.Parallel()
			test(t, form)
		})
	}
}

// rolePushStepForm is the one form a case of the step's own logic runs in: the pod's, a Legion
// pod's, whose line also carries the git.executable-path words the step skips.
const rolePushStepForm = rolePushPod

// rolePushStepLogic runs test under rolePushStepForm alone, in a parallel subtest named for it, for
// a case of the step's own logic (above).
func rolePushStepLogic(t *testing.T, test func(t *testing.T, form rolePushForm)) {
	t.Run(string(rolePushStepForm), func(t *testing.T) {
		t.Parallel()
		test(t, rolePushStepForm)
	})
}

// rolePushForEach runs test under each form where both is true, the case that checks a family of
// the step's logic in both forms, and under rolePushStepForm alone where it is false.
func rolePushForEach(t *testing.T, both bool, test func(t *testing.T, form rolePushForm)) {
	if both {
		rolePushEachForm(t, test)
		return
	}
	rolePushStepLogic(t, test)
}

// rolePushRealJJ is the first jj on the test process's PATH that is a compiled program
// (lookpath.Kind): in a Legion pod the first jj, /opt/legion/bin/jj, is the image's wrapper script,
// which fixtureenv.MustGuard has already taken off PATH, and a script there would be passed over
// all the same.
func rolePushRealJJ(t testing.TB) string {
	t.Helper()
	found, _ := lookpath.Others(os.Getenv("PATH"), "jj", "")
	for _, jj := range found {
		if _, compiled, err := lookpath.Kind(jj.Path); err == nil && compiled {
			return jj.Path
		}
	}
	t.Fatal("no jj on PATH that is a compiled program: these tests run a real jj")
	return ""
}

// The recording jj's behaviours: shell that its script runs before the real jj, with is_metaedit,
// is_push, is_dry, is_log, is_set, is_ignoring and is_quiet telling whether the command holds the
// word metaedit, push, --dry-run or log, a revset that names remote_bookmarks (the step's read of
// the set), --ignore-working-copy, or --quiet. A behaviour that sets ran answers the command
// itself, with status.
const (
	// rolePushRecords runs every command on the real jj.
	rolePushRecords = ":"
	// rolePushFailsMetaedit fails every metaedit, as jj fails, before it changes anything.
	rolePushFailsMetaedit = `if [ -n "$is_metaedit" ]; then echo 'Error: legion test: this metaedit fails' >&2; status=1; ran=1; fi`
	// rolePushFailsSetRead fails the read of the commits the push sends.
	rolePushFailsSetRead = `if [ -n "$is_log" ] && [ -n "$is_set" ]; then echo 'Error: legion test: this read of the set fails' >&2; status=1; ran=1; fi`
	// rolePushFailsSnapshot fails every command that is no push and that holds no
	// --ignore-working-copy, the step's snapshot, as jj fails, before it changes anything.
	rolePushFailsSnapshot = `if [ -z "$is_push" ] && [ -z "$is_ignoring" ]; then echo 'Error: legion test: this snapshot fails' >&2; status=1; ran=1; fi`
	// rolePushSnapshotPrints prints rolePushSnapshotLine on standard error from every command that
	// takes a snapshot of the working copy and does not run with --quiet, every command that holds
	// neither --ignore-working-copy nor --quiet, the step's snapshot and a real push without that
	// flag alike, then runs it on the real jj: a stand-in for jj's line of the descendants of @ that
	// a snapshot rebased, which jj 0.38 and 0.45 alike print on standard error, and which --quiet
	// silences. The step's dry run holds --ignore-working-copy, and prints no such line.
	rolePushSnapshotPrints = `if [ -z "$is_ignoring" ] && [ -z "$is_quiet" ]; then echo '` + rolePushSnapshotLine + `' >&2; fi`
	// rolePushMetaeditFillsNothing answers every metaedit with success and changes nothing.
	rolePushMetaeditFillsNothing = `if [ -n "$is_metaedit" ]; then ran=1; fi`
	// rolePushUnreadableDryRun adds a line of neither shape to every block of a dry run.
	rolePushUnreadableDryRun = `if [ -n "$is_push" ] && [ -n "$is_dry" ]; then
  out=$(mktemp)
  "$real" "$@" 2>"$out"
  status=$?
  while IFS= read -r line; do
    printf '%s\n' "$line"
    case $line in "Changes to push to "*) printf '%s\n' '  legion test: a line of neither shape' ;; esac
  done <"$out" >&2
  rm -f "$out"
  ran=1
fi`
	// rolePushDryRunWithNoBlock makes every dry run exit as jj exits, having printed nothing.
	rolePushDryRunWithNoBlock = `if [ -n "$is_push" ] && [ -n "$is_dry" ]; then "$real" "$@" 2>/dev/null; status=$?; ran=1; fi`
	// rolePushShortensDryRun shortens each commit id of 12 hex digits on a dry run's standard
	// error, in a block's line of either release's shape and in jj's refusal of a commit with no
	// author alike, to its first digit: a stand-in for an id that jj prints short and that the
	// id of another commit of the repository's starts with too.
	rolePushShortensDryRun = `if [ -n "$is_push" ] && [ -n "$is_dry" ]; then
  out=$(mktemp)
  "$real" "$@" 2>"$out"
  status=$?
  sed -E 's/(^|[^0-9A-Za-z])([0-9a-f])[0-9a-f]{11}($|[^0-9A-Za-z])/\1\2\3/g' "$out" >&2
  rm -f "$out"
  ran=1
fi`
	// rolePushMeasuresMetaedit appends the environment of every metaedit, as execve(2) handed it
	// to the recording jj (/proc/<pid>/environ), then \036, which no variable holds, to the file
	// of the recording jj's path with `.environ` after it (metaeditSizes).
	rolePushMeasuresMetaedit = `if [ -n "$is_metaedit" ]; then { cat /proc/$$/environ; printf '\036'; } >>"$0.environ"; fi`
)

// rolePushUnreadableLine is the line rolePushUnreadableDryRun adds.
const rolePushUnreadableLine = "legion test: a line of neither shape"

// rolePushSnapshotLine is the line rolePushSnapshotPrints prints.
const rolePushSnapshotLine = "legion test: this snapshot prints on standard error"

// rolePushRecorder is a recording jj in front of real: it runs behaviour, then the real jj unless
// behaviour answered, and appends to log the time it started and ended, its status and its
// arguments, each field ended by \037 and each record by \036, which no argument holds.
func rolePushRecorder(real, log, behaviour string) string {
	return "#!/bin/sh\n" +
		"real='" + real + "'\n" +
		"start=$(date +%s%N)\n" +
		"ran= status=0 is_metaedit= is_push= is_dry= is_log= is_set= is_ignoring= is_quiet=\n" +
		"for arg; do\n" +
		"  case $arg in\n" +
		"    metaedit) is_metaedit=1 ;;\n" +
		"    push) is_push=1 ;;\n" +
		"    --dry-run) is_dry=1 ;;\n" +
		"    log) is_log=1 ;;\n" +
		"    *remote_bookmarks*) is_set=1 ;;\n" +
		"    --ignore-working-copy) is_ignoring=1 ;;\n" +
		"    --quiet) is_quiet=1 ;;\n" +
		"  esac\n" +
		"done\n" +
		behaviour + "\n" +
		"if [ -z \"$ran\" ]; then\n" +
		"  \"$real\" \"$@\"\n" +
		"  status=$?\n" +
		"fi\n" +
		"end=$(date +%s%N)\n" +
		"{ printf '%s\\037' \"$start\" \"$end\" \"$status\" \"$@\"; printf '\\036'; } >>'" + log + "'\n" +
		"exit \"$status\"\n"
}

// rolePushCall is one command the recording jj ran.
type rolePushCall struct {
	start, end int64
	status     int
	args       []string
}

func (c rolePushCall) has(word string) bool { return slices.Contains(c.args, word) }

// rolePushGlobalValues are jj's global options whose value may be the next word, which legion jj's
// reading of a push (readJJPush, jjGlobalValues) skips to find a command's subcommand pair.
var rolePushGlobalValues = []string{"-R", "--repository", "--at-operation", "--at-op", "--color", "--config", "--config-file", "--config-toml"}

// pair is the call's subcommand pair, as legion jj's reading of a push finds it in a line jj has
// no alias left to expand in: its first two words that are no option and no value of one of
// rolePushGlobalValues.
func (c rolePushCall) pair() [2]string {
	var pair [2]string
	n := 0
	for i := 0; i < len(c.args) && n < len(pair); i++ {
		switch arg := c.args[i]; {
		case slices.Contains(rolePushGlobalValues, arg):
			i++
		case !strings.HasPrefix(arg, "-"):
			pair[n] = arg
			n++
		}
	}
	return pair
}

// realPush is whether the call is a push that sends: a `jj git push`, by its subcommand pair, with
// no --dry-run. A command that starts a push, as `jj util exec -- jj git push …` does, is none.
func (c rolePushCall) realPush() bool {
	return c.pair() == [2]string{"git", "push"} && !c.has("--dry-run")
}

// rolePushChange is one commit: its change id and its commit id.
type rolePushChange struct{ change, commit string }

// rolePushFixture is one row's repository and the form of Legion's jj its push goes through.
type rolePushFixture struct {
	t      *testing.T
	form   rolePushForm
	real   string // the real jj
	git    string // the real git
	root   string
	home   string
	tmp    string
	legion string // this test binary, run as legion
	// next is the jj the form runs: the recording jj, or the real one when there is none.
	next string
	// formDir is the directory whose jj is the form's: the pod's directory of wrappers, or the
	// tmux form's bin.
	formDir string
	// podForces holds, for the git of each directory podDir wrote, the word that names it in each
	// layer the pod's jj writes into a line it runs: `--config=git.executable-path="<git>"`.
	podForces []string
	// after is the PATH after formDir: the recording jj and the real one for tmux, the system's
	// directories for the pod, whose wrapper runs its jj by its full path.
	after string
	path  string // the PATH of a role's push: formDir, then after
	// setupPath is the PATH of the commands that make the repository, which run the real jj by its
	// full path and push with the real git.
	setupPath string
	// timestamp is JJ_TIMESTAMP in every command's environment, or "" to leave it unset: a test
	// that sets it (rolePushFixedTimestamp) before its setup has every commit that jj makes for it,
	// in the setup and in the push alike, carry that one time, as commands within one second do.
	timestamp string
	// table is LEGION_ROLE_IDENTITIES in every command's environment, or nil to leave it unset.
	table *string
	// bound and argMax are rolePushBoundVariable and rolePushArgMaxVariable in every command's
	// environment, each in bytes, or 0 to leave it unset.
	bound, argMax int
	log           string // the recording jj's log, or "" when there is no recording jj
	bare          string // the remote origin
	repo          string // the clone, a colocated jj repository; its workspace is the default one
	seed          string // the commit id of the remote's main, under every ref the tests read
	files         int
}

// newRolePushWrapper makes the form's jj for a test, with a recording jj between it and the real
// one when behaviour is not "", and no repository.
func newRolePushWrapper(t *testing.T, form rolePushForm, behaviour string) *rolePushFixture {
	t.Helper()
	root := t.TempDir()
	table := rolePushTable
	f := &rolePushFixture{
		t: t, form: form, real: rolePushRealJJ(t), git: fixturegit.CompiledGit(t), root: root,
		home: filepath.Join(root, "home"), tmp: filepath.Join(root, "tmp"), table: &table,
		bare: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "repo"),
	}
	for _, dir := range []string{f.home, f.tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.setupPath = filepath.Dir(f.git) + ":/usr/bin:/bin"
	f.next = f.real
	nextDirs := filepath.Dir(f.real)
	if behaviour != "" {
		f.log = filepath.Join(root, "jj-calls")
		recording := filepath.Join(root, "recording")
		f.next = f.script(recording, "jj", rolePushRecorder(f.real, f.log, behaviour))
		nextDirs = recording + ":" + nextDirs
	}
	f.legion = trailerLegion(t)
	switch form {
	case rolePushPod:
		f.formDir = f.podDir(filepath.Join(root, "pod-bin"))
		f.after = "/usr/bin:/bin"
	case rolePushTmux:
		f.after = nextDirs + ":" + filepath.Dir(f.git) + ":/usr/bin:/bin"
		f.formDir = f.install(f.after)
	}
	f.path = f.formDir + ":" + f.after
	return f
}

// newRolePushFixture is newRolePushWrapper with the repository a role pushes from: a colocated
// clone of a local bare remote, made as provisioning makes one, with main tracked.
func newRolePushFixture(t *testing.T, form rolePushForm, behaviour string) *rolePushFixture {
	t.Helper()
	f := newRolePushWrapper(t, form, behaviour)
	run := f.runner(f.setupPath, rolePushImplementer)
	run.Run(f.root, append([]string{f.git}, fixturegit.InitBare(f.bare, "main")[1:]...)...)
	seed := filepath.Join(f.root, "seed")
	run.Run(f.root, append([]string{f.git}, fixturegit.InitWorkTree(seed, "main")[1:]...)...)
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run.Run(seed, f.git, "add", "README")
	run.Run(seed, f.git, "commit", "--quiet", "-m", "seed")
	run.Run(seed, f.git, "push", "--quiet", f.bare, "main")
	f.seed = strings.TrimSpace(run.Out(seed, f.git, "rev-parse", "HEAD"))
	run.Run(f.root, f.git, "clone", "--quiet", f.bare, f.repo)
	run.Run(f.repo, append([]string{f.real}, fixturegit.JJInit(f.repo, true)[1:]...)...)
	f.jj(rolePushImplementer, "bookmark", "track", "main", "--remote=origin")
	return f
}

// script writes an executable script named name into dir and answers its path.
func (f *rolePushFixture) script(dir, name, body string) string {
	f.t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := execfile.Write(path, []byte(body), 0o700); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// podDir writes the pod's wrappers into dir, as the image's /opt/legion/bin holds them: a git that
// runs the real git, and packages/daemon/docker/jj with real= naming the form's next jj, wrapper=
// naming that git, and legion=, where the script has that line, naming this test binary run as
// legion. It adds that git's layer word to podForces, and answers dir.
func (f *rolePushFixture) podDir(dir string) string {
	f.t.Helper()
	git := f.script(dir, "git", "#!/bin/sh\nexec '"+f.git+"' \"$@\"\n")
	read, err := os.ReadFile(filepath.Join("..", "..", "docker", "jj"))
	if err != nil {
		f.t.Fatal(err)
	}
	lines := strings.Split(string(read), "\n")
	repointed := map[string]bool{}
	for i, line := range lines {
		switch line {
		case "real=/usr/local/bin/jj":
			lines[i] = "real='" + f.next + "'"
		case "wrapper=/opt/legion/bin/git":
			lines[i] = "wrapper='" + git + "'"
		case "legion=/opt/legion/bin/legion":
			lines[i] = "legion='" + f.legion + "'"
		default:
			continue
		}
		repointed[strings.SplitN(line, "=", 2)[0]] = true
	}
	if !repointed["real"] || !repointed["wrapper"] {
		f.t.Fatalf("packages/daemon/docker/jj holds no real= or wrapper= line to repoint: %v", repointed)
	}
	f.podForces = append(f.podForces, `--config=git.executable-path="`+git+`"`)
	f.script(dir, "jj", strings.Join(lines, "\n"))
	return dir
}

// install runs workerbin.Install for this test's legion with path as the PATH its agents inherit,
// under a new state directory, and answers its bin.
func (f *rolePushFixture) install(path string) string {
	f.t.Helper()
	state, err := os.MkdirTemp(f.root, "state-")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := workerbin.Install(state, f.legion, path); err != nil {
		f.t.Fatalf("workerbin.Install: %v", err)
	}
	return workerbin.LauncherDir(state)
}

// env is the whole environment of one of a role's commands: the PATH given, a home, a temporary
// directory and a jj configuration of the test's own, no git configuration of the host's, the
// role's identity, the table, and the row's bounds of the step and fixed timestamp where it set
// them. Nothing is inherited, so no variable of the pod's reaches it.
func (f *rolePushFixture) env(path string, as rolePushIdentity) []string {
	env := []string{
		"PATH=" + path, "TMPDIR=" + f.tmp,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file",
		"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid",
		"GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid",
		"JJ_USER=" + as.name, "JJ_EMAIL=" + as.email,
	}
	env = append(env, fixtureenv.Home(f.home)...)
	if f.table != nil {
		env = append(env, rolePushVariable+"="+*f.table)
	}
	if f.bound != 0 {
		env = append(env, rolePushBoundVariable+"="+strconv.Itoa(f.bound))
	}
	if f.argMax != 0 {
		env = append(env, rolePushArgMaxVariable+"="+strconv.Itoa(f.argMax))
	}
	if f.timestamp != "" {
		env = append(env, "JJ_TIMESTAMP="+f.timestamp)
	}
	return env
}

// runner runs commands under env of path and as.
func (f *rolePushFixture) runner(path string, as rolePushIdentity) fixturegit.Runner {
	return fixturegit.Runner{TB: f.t, Env: f.env(path, as)}
}

// jj runs the real jj in the repository as the role whose identity as is, failing the test when it
// fails, and answers what it printed on standard output.
func (f *rolePushFixture) jj(as rolePushIdentity, args ...string) string {
	f.t.Helper()
	return f.jjIn(f.repo, as, args...)
}

// jjIn is jj in dir.
func (f *rolePushFixture) jjIn(dir string, as rolePushIdentity, args ...string) string {
	f.t.Helper()
	return f.runner(f.setupPath, as).Out(dir, append([]string{f.real}, args...)...)
}

// push runs `jj <args>` as a role's shell runs it, by name, through the form's jj first on PATH.
func (f *rolePushFixture) push(as rolePushIdentity, args ...string) (code int, stdout, stderr string) {
	f.t.Helper()
	return f.pushOn(f.path, as, args...)
}

// pushOn is push with path as the PATH.
func (f *rolePushFixture) pushOn(path string, as rolePushIdentity, args ...string) (code int, stdout, stderr string) {
	f.t.Helper()
	return f.runIn(path, f.repo, as, nil, args...)
}

// runIn runs `jj <args>` as a role's shell runs it, by name through the first jj on path, as the
// role whose identity as is, in dir, with extra variables after the role's environment.
func (f *rolePushFixture) runIn(path, dir string, as rolePushIdentity, extra []string, args ...string) (code int, stdout, stderr string) {
	f.t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		f.t.Fatal(err)
	}
	return trailerRun(f.t, dir, append(f.env(path, as), extra...), "", sh, append([]string{"-c", `exec jj "$@"`, "jj"}, args...)...)
}

// alone runs the real jj by itself, with no form of Legion's in front of it.
func (f *rolePushFixture) alone(as rolePushIdentity, args ...string) (code int, stdout, stderr string) {
	f.t.Helper()
	return f.aloneIn(f.repo, as, nil, args...)
}

// aloneIn is alone in dir, with extra variables after the role's environment.
func (f *rolePushFixture) aloneIn(dir string, as rolePushIdentity, extra []string, args ...string) (code int, stdout, stderr string) {
	f.t.Helper()
	return trailerRun(f.t, dir, append(f.env(f.setupPath, as), extra...), "", f.real, args...)
}

// aloneSends is what the real jj by itself says the push of args would do: its dry run's status,
// and the remotes of the blocks it printed. A row whose push jj's releases treat differently
// expects what the jj under test does alone.
func (f *rolePushFixture) aloneSends(args []string) (code int, remotes []string) {
	f.t.Helper()
	code, _, stderr := f.alone(rolePushImplementer, append(slices.Clone(args), "--dry-run", "--ignore-working-copy")...)
	for line := range strings.SplitSeq(stderr, "\n") {
		if remote, ok := strings.CutPrefix(line, "Changes to push to "); ok {
			remotes = append(remotes, strings.TrimSuffix(remote, ":"))
		}
	}
	return code, remotes
}

// write writes a file of its own into the working copy, with no jj command after it.
func (f *rolePushFixture) write() {
	f.files++
	f.writeFile(filepath.Join(f.repo, "change-"+strconv.Itoa(f.files)+".txt"), "change "+strconv.Itoa(f.files)+"\n")
}

func (f *rolePushFixture) writeFile(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// read is the one commit rev names, read with no snapshot.
func (f *rolePushFixture) read(rev string) rolePushChange {
	f.t.Helper()
	fields := strings.Fields(f.jj(rolePushImplementer, "--ignore-working-copy", "log", "-r", rev, "--no-graph", "-T", `change_id ++ " " ++ commit_id ++ "\n"`))
	if len(fields) != 2 {
		f.t.Fatalf("jj log -r %s read %q, want one commit", rev, fields)
	}
	return rolePushChange{change: fields[0], commit: fields[1]}
}

// changeOf is the change id of commit, or "" when jj finds no such commit.
func (f *rolePushFixture) changeOf(commit string) string {
	f.t.Helper()
	out, err := f.runner(f.setupPath, rolePushImplementer).Try(f.repo, f.real, "--ignore-working-copy", "log", "-r", commit, "--no-graph", "-T", "change_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// commitOn makes a commit on parent as a role's shell does: maker's `jj new`, which makes it its
// author, a file of its own, and describer's `jj commit -m message`, which leaves @ a new empty
// commit on it.
func (f *rolePushFixture) commitOn(maker, describer rolePushIdentity, parent, message string) rolePushChange {
	f.t.Helper()
	f.jj(maker, "new", parent)
	f.write()
	f.jj(describer, "commit", "-m", message)
	return f.read("@-")
}

// bookmark points name at rev, making it where there is none.
func (f *rolePushFixture) bookmark(name, rev string) {
	f.t.Helper()
	f.jj(rolePushImplementer, "bookmark", "set", name, "-r", rev)
}

// track makes name a bookmark that origin holds and that jj tracks, at main.
func (f *rolePushFixture) track(name string) {
	f.t.Helper()
	f.bookmark(name, "main")
	f.jj(rolePushImplementer, "git", "push", "--bookmark", name)
}

// op is the id of the repository's current operation.
func (f *rolePushFixture) op() string {
	f.t.Helper()
	return strings.TrimSpace(f.jj(rolePushImplementer, "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", "id"))
}

// ops is the number of operations in the repository's log.
func (f *rolePushFixture) ops() int {
	f.t.Helper()
	return strings.Count(f.jj(rolePushImplementer, "--ignore-working-copy", "op", "log", "--no-graph", "-T", `"o\n"`), "\n")
}

// metaedits is the number of `jj metaedit` operations in the repository's log.
func (f *rolePushFixture) metaedits() int {
	f.t.Helper()
	count := 0
	for line := range strings.SplitSeq(f.jj(rolePushImplementer, "--ignore-working-copy", "op", "log", "--no-graph", "-T", `description ++ "\n"`), "\n") {
		if strings.HasPrefix(line, "edit commit metadata") {
			count++
		}
	}
	return count
}

// divergence is the number of visible commits of each divergent change.
func (f *rolePushFixture) divergence() map[string]int {
	f.t.Helper()
	counts := map[string]int{}
	for _, change := range strings.Fields(f.jj(rolePushImplementer, "--ignore-working-copy", "log", "-r", "all()", "--no-graph", "-T", `if(divergent, change_id ++ "\n")`)) {
		counts[change]++
	}
	return counts
}

// diverge describes c's one commit twice from one operation, as two roles' commands that run at
// the same moment do, and answers the two commits of the divergent change that leaves, in the
// order of the two descriptions. The first line of each description names its commit.
func (f *rolePushFixture) diverge(c rolePushChange, first, second string) (rolePushChange, rolePushChange) {
	f.t.Helper()
	op := f.op()
	f.jj(rolePushReviewer, "--at-op", op, "describe", "-r", c.commit, "-m", first)
	f.jj(rolePushReviewer, "--at-op", op, "describe", "-r", c.commit, "-m", second)
	f.jj(rolePushReviewer, "log", "-r", "@", "--no-graph", "-T", "commit_id")
	return f.titled(c.change, first), f.titled(c.change, second)
}

// titled is the visible commit of change whose description starts with the first line of message.
func (f *rolePushFixture) titled(change, message string) rolePushChange {
	f.t.Helper()
	title, _, _ := strings.Cut(message, "\n")
	for line := range strings.SplitSeq(f.jj(rolePushImplementer, "--ignore-working-copy", "log", "-r", "change_id("+change+")", "--no-graph", "-T", `commit_id ++ " " ++ description.first_line() ++ "\n"`), "\n") {
		if commit, first, ok := strings.Cut(line, " "); ok && first == title {
			return rolePushChange{change: change, commit: commit}
		}
	}
	f.t.Fatalf("change %s has no visible commit titled %q", change, title)
	return rolePushChange{}
}

// rolePushPlaceholder is a word of a row's arguments that names what the setup made:
// {commit:<rev>} and {change:<rev>} the commit id and the change id of rev, {op:@} the current
// operation.
var rolePushPlaceholder = regexp.MustCompile(`\{(commit|change|op):([^}]*)\}`)

// resolve is args with each placeholder replaced.
func (f *rolePushFixture) resolve(args []string) []string {
	f.t.Helper()
	resolved := make([]string, len(args))
	for i, arg := range args {
		resolved[i] = rolePushPlaceholder.ReplaceAllStringFunc(arg, func(word string) string {
			parts := rolePushPlaceholder.FindStringSubmatch(word)
			switch parts[1] {
			case "commit":
				return f.read(parts[2]).commit
			case "change":
				return f.read(parts[2]).change
			}
			return f.op()
		})
	}
	return resolved
}

// calls is every command the recording jj ran, in the order they ended.
func (f *rolePushFixture) calls() []rolePushCall {
	f.t.Helper()
	data, err := os.ReadFile(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var calls []rolePushCall
	for record := range strings.SplitSeq(strings.TrimSuffix(string(data), "\036"), "\036") {
		fields := strings.Split(strings.TrimSuffix(record, "\037"), "\037")
		if len(fields) < 3 {
			f.t.Fatalf("the recording jj wrote %q", record)
		}
		start, errStart := strconv.ParseInt(fields[0], 10, 64)
		end, errEnd := strconv.ParseInt(fields[1], 10, 64)
		status, errStatus := strconv.Atoi(fields[2])
		if err := errors.Join(errStart, errEnd, errStatus); err != nil {
			f.t.Fatalf("the recording jj wrote %q: %v", record, err)
		}
		calls = append(calls, rolePushCall{start: start, end: end, status: status, args: fields[3:]})
	}
	return calls
}

// forgetCalls empties the recording jj's log, so that calls reads only the commands that run after
// it: a test that pushes again through the form's jj, after a push the remote refused or after one
// that sent, checks the second push as one real push of its own.
func (f *rolePushFixture) forgetCalls() {
	f.t.Helper()
	if err := os.Remove(f.log); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
}

// remote is the bare repository of the remote named name; origin when name is "".
func (f *rolePushFixture) remote(name string) string {
	if name == "" || name == "origin" {
		return f.bare
	}
	return filepath.Join(f.root, name+".git")
}

// addRemote makes a second bare remote, name, and adds it to the repository.
func (f *rolePushFixture) addRemote(name string) {
	f.t.Helper()
	bare := f.remote(name)
	f.runner(f.setupPath, rolePushImplementer).Run(f.root, append([]string{f.git}, fixturegit.InitBare(bare, "main")[1:]...)...)
	f.jj(rolePushImplementer, "git", "remote", "add", name, bare)
}

// rolePushRemoteCommit is one commit a remote holds, as git reads it there.
type rolePushRemoteCommit struct{ id, name, email string }

// remoteCommits is each commit of ref in bare above the seed, the tip first, as git reads them in
// the remote; ok is whether bare holds ref at all.
func (f *rolePushFixture) remoteCommits(bare, ref string) (commits []rolePushRemoteCommit, ok bool) {
	f.t.Helper()
	run := f.runner(f.setupPath, rolePushImplementer)
	if _, err := run.Try(f.root, f.git, "--git-dir", bare, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return nil, false
	}
	for line := range strings.SplitSeq(strings.TrimSpace(run.Out(f.root, f.git, "--git-dir", bare, "log", "--format=%H%x1f%an%x1f%ae", ref, "--not", f.seed)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x1f")
		commits = append(commits, rolePushRemoteCommit{id: fields[0], name: fields[1], email: fields[2]})
	}
	return commits, true
}

// rolePushWant is what a row's push must leave.
type rolePushWant struct {
	by   rolePushIdentity // the role that pushes; the implementer when zero
	exit int              // the push's exit status
	refs []rolePushRef    // what remotes hold afterwards
	// rewrites are the commits the step rewrites, one printed line each.
	rewrites []rolePushRewrite
	// others is whether the step may also print rewrites of commits that the row does not name,
	// which a test of what stays does not read.
	others bool
	// same is each commit id that stays visible, with the author it keeps.
	same map[string]rolePushIdentity
	// diverges is whether the push may add a visible commit to a divergent change.
	diverges bool
	// metaedits, when not zero, is the number of metaedit operations the push adds.
	metaedits int
	// table is whether the step prints the one line of a table it cannot use.
	table bool
	// cannotRead is how many lines `legion jj: could not read what the push sends: …` the push may
	// print, and cannotReadNames what such a line holds.
	cannotRead      rolePushCannotRead
	cannotReadNames string
	// cannotSet is how many lines `legion jj: could not set the author: …` the push prints, and
	// cannotSetNames what each holds.
	cannotSet      int
	cannotSetNames string
	// noMetaedit is whether the recording jj must see no metaedit.
	noMetaedit bool
	// ids are the lines `legion jj: revision `<argument>` names other commits after the step; …`
	// the push prints, in order, and no other: each line of an argument the step gives as the ids
	// of its commits, or as none() (rolePushIDs).
	ids []rolePushIDs
	// snapshots is how many snapshots of the working copy the step takes: the commands of the
	// step's that a recording jj ahead of the real one sees run with no --ignore-working-copy
	// before the real push. check reads the real push's --ignore-working-copy (checkIgnoring), and
	// the ending of each line of a step that gave up, by what it sees there, whatever snapshots says.
	snapshots rolePushSnapshots
	// pushed, when not nil, checks the words of the real push, which a recording jj ahead of the
	// real one saw (oneRealPush).
	pushed func(words []string)
}

// rolePushSnapshots is how many snapshots of the working copy a row expects the step to take.
type rolePushSnapshots int

const (
	rolePushSnapshotsUnread  rolePushSnapshots = iota // not read
	rolePushTakesNoSnapshot                           // none
	rolePushTakesOneSnapshot                          // exactly one
)

// rolePushCannotRead is how many lines of a dry run the step cannot read a row expects.
type rolePushCannotRead int

const (
	rolePushReadsAll     rolePushCannotRead = iota // none
	rolePushMayNotRead                             // none or one: jj's own refusal of the push
	rolePushCannotReadIt                           // exactly one
)

// rolePushRef is a ref a remote holds afterwards.
type rolePushRef struct {
	remote string // the remote; origin when ""
	ref    string // the full ref name
	absent bool   // the remote holds no such ref
	// commits are the ref's commits above the seed, the tip first.
	commits []rolePushCommit
}

// rolePushCommit is a commit a remote holds.
type rolePushCommit struct {
	change string
	// author is its author; the zero identity leaves it unread.
	author rolePushIdentity
	// commit is its commit id, where it must be that one commit.
	commit string
	// file and content are a file its tree holds.
	file, content string
	// lacks is a file its tree does not hold, where it is not "".
	lacks string
}

// rolePushRewrite is a commit the step rewrites, with the line it prints for it.
type rolePushRewrite struct {
	role   string
	author rolePushIdentity // the identity of the role, which the new commit has
	change string
	old    string // its commit id before the push
	// rebased is whether an earlier metaedit of the same step rebases it first, so the old id
	// printed may be a later commit of its change.
	rebased bool
	// snapshotted is whether it is @ with a write no command has snapshotted, which the step's
	// one snapshot takes before its rewrite, so the old id printed is the commit that snapshot
	// made of it, a later commit of its change.
	snapshotted bool
}

// rolePushPrinted is one line `legion jj: <role>: <old> -> <new>`.
type rolePushPrinted struct{ role, old, new string }

var rolePushRewriteLine = regexp.MustCompile(`^legion jj: ([a-z]+): ([0-9a-f]{40}) -> ([0-9a-f]{40})$`)

// rolePushFullID is a full commit id.
var rolePushFullID = regexp.MustCompile(`[0-9a-f]{40}`)

// rolePushIDs is one line `legion jj: revision `<argument>` names other commits after the step;
// the push gets the <n> commit(s) … by their ids, in <k> word(s) of its option, each under <bound>
// bytes`: the argument, n, k and the bound; or one line `legion jj: revision `<argument>` names
// other commits after the step; it named no commit before the step, so the push gets none()`: the
// argument alone, with no number.
type rolePushIDs struct {
	argument              string
	commits, words, bound int
}

// rolePushOneIDLine is the line of argument whose real push gets the id of the one commit it named
// before the step, in one word under rolePushRevisionBound bytes.
func rolePushOneIDLine(argument string) rolePushIDs {
	return rolePushIDs{argument: argument, commits: 1, words: 1, bound: rolePushRevisionBound}
}

var (
	rolePushIDsLine  = regexp.MustCompile(`^legion jj: revision ` + "`(.*)`" + ` names other commits after the step; the push gets the ([0-9]+) commit\(s\) it named before the step, as the step left them, by their ids, in ([0-9]+) word\(s\) of its option, each under ([0-9]+) bytes$`)
	rolePushNoneLine = regexp.MustCompile(`^legion jj: revision ` + "`(.*)`" + ` names other commits after the step; it named no commit before the step, so the push gets none\(\)$`)
)

// rolePushNumbers is each of parts as a number; each is digits alone (rolePushIDsLine).
func rolePushNumbers(parts ...string) []int {
	numbers := make([]int, len(parts))
	for i, part := range parts {
		numbers[i], _ = strconv.Atoi(part)
	}
	return numbers
}

const (
	rolePushCannotReadPrefix = "legion jj: could not read what the push sends: "
	rolePushCannotSetPrefix  = "legion jj: could not set the author: "
	rolePushAsWrittenSuffix  = "; the push runs as written"
	// rolePushAsWrittenIgnoringSuffix ends the line of a step that took its snapshot before it
	// gave up: the push runs as written with --ignore-working-copy.
	rolePushAsWrittenIgnoringSuffix = rolePushAsWrittenSuffix + ", with --ignore-working-copy"
	rolePushTablePrefix             = "legion jj: " + rolePushVariable + " is "
	rolePushTableSuffix             = "; the push runs with no author step"
)

// rolePushLines is each line of legion jj's on a push's standard error, by kind.
type rolePushLines struct {
	rewrites   []rolePushPrinted
	ids        []rolePushIDs
	cannotRead []string
	cannotSet  []string
	table      []string
	unknown    []string
}

func rolePushStepLines(stderr string) rolePushLines {
	var lines rolePushLines
	for line := range strings.SplitSeq(stderr, "\n") {
		if !strings.HasPrefix(line, "legion jj:") {
			continue
		}
		if parts := rolePushRewriteLine.FindStringSubmatch(line); parts != nil {
			lines.rewrites = append(lines.rewrites, rolePushPrinted{role: parts[1], old: parts[2], new: parts[3]})
			continue
		}
		if parts := rolePushIDsLine.FindStringSubmatch(line); parts != nil {
			n := rolePushNumbers(parts[2:]...)
			lines.ids = append(lines.ids, rolePushIDs{argument: parts[1], commits: n[0], words: n[1], bound: n[2]})
			continue
		}
		if parts := rolePushNoneLine.FindStringSubmatch(line); parts != nil {
			lines.ids = append(lines.ids, rolePushIDs{argument: parts[1]})
			continue
		}
		asWritten := strings.HasSuffix(line, rolePushAsWrittenSuffix) || strings.HasSuffix(line, rolePushAsWrittenIgnoringSuffix)
		switch {
		case strings.HasPrefix(line, rolePushCannotReadPrefix) && asWritten:
			lines.cannotRead = append(lines.cannotRead, line)
		case strings.HasPrefix(line, rolePushCannotSetPrefix) && asWritten:
			lines.cannotSet = append(lines.cannotSet, line)
		case strings.HasPrefix(line, rolePushTablePrefix) && strings.HasSuffix(line, rolePushTableSuffix):
			lines.table = append(lines.table, line)
		default:
			lines.unknown = append(lines.unknown, line)
		}
	}
	return lines
}

// rolePushRow is one row of a test: the push's arguments, with placeholders (resolve), and the
// setup that makes the repository it pushes from and says what the push must leave. bothForms runs
// it in each form (rolePushForEach): a row whose line holds a configuration layer, an alias or a
// `--` of the role's, which the pod's wrapper reads itself, or the row that checks its family of
// the step's logic in both forms.
type rolePushRow struct {
	name      string
	args      []string
	setup     func(f *rolePushFixture) rolePushWant
	bothForms bool
}

// rolePushRun runs each row in its forms (bothForms), each in a repository of its own, with a
// recording jj ahead of the real one (rolePushRecords), so that check reads each real push's
// --ignore-working-copy (checkIgnoring).
func rolePushRun(t *testing.T, rows []rolePushRow) {
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushForEach(t, row.bothForms, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				want := row.setup(f)
				f.pushAndCheck(row.args, want)
			})
		})
	}
}

// rolePushArgsRow is one row of a test whose push's arguments name what its setup made: the setup
// makes the repository the push goes from, and answers the push's arguments and what the push
// must leave. bothForms is rolePushRow's.
type rolePushArgsRow struct {
	name      string
	setup     func(f *rolePushFixture) ([]string, rolePushWant)
	bothForms bool
}

// rolePushRunArgs runs each row in its forms (bothForms), each in a repository of its own, with a
// recording jj of behaviour ahead of the real one where behaviour is not "".
func rolePushRunArgs(t *testing.T, behaviour string, rows []rolePushArgsRow) {
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushForEach(t, row.bothForms, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, behaviour)
				args, want := row.setup(f)
				f.pushAndCheck(args, want)
			})
		})
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

// rolePushNamedRow is a push that a test may check against jj alone (rolePushRunNamed): one of a
// revision argument that a rewrite of the step can make name other commits than it named before
// the step (jj_push_revision_test.go), one whose command would pass the step's limit
// (jj_push_bound_test.go), or one of a descendant of the working copy. setup makes the state it
// runs on, and answers the push's arguments, what the push through the form's jj must leave, and
// what jj alone must leave on a twin state made the same way, which the row checks where alone is
// true. bothForms is rolePushRow's.
type rolePushNamedRow struct {
	name      string
	alone     bool
	setup     func(f *rolePushFixture) (args []string, want, alone rolePushWant)
	bothForms bool
}

// rolePushRunNamed runs each row in its forms (bothForms): where alone is true, jj alone on a twin
// state first, which must leave what the row's alone says and no other ref beside main on origin;
// then the push through the form's jj, with a recording jj ahead of the real one, which must leave
// what the row's want says and no other ref beside main on origin.
func rolePushRunNamed(t *testing.T, rows []rolePushNamedRow) {
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushForEach(t, row.bothForms, func(t *testing.T, form rolePushForm) {
				if row.alone {
					twin := newRolePushFixture(t, form, "")
					args, _, alone := row.setup(twin)
					divergent := twin.divergence()
					code, _, stderr := twin.alone(rolePushImplementer, args...)
					twin.check(alone, args, code, stderr, divergent, 0)
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

// pushAndCheck pushes args through the form's jj and checks what the push left against want.
func (f *rolePushFixture) pushAndCheck(args []string, want rolePushWant) {
	f.t.Helper()
	by := want.by
	if by == rolePushNobody {
		by = rolePushImplementer
	}
	resolved := f.resolve(args)
	divergent, edits := f.divergence(), 0
	if want.metaedits != 0 {
		edits = f.metaedits()
	}
	code, _, stderr := f.push(by, resolved...)
	f.check(want, resolved, code, stderr, divergent, edits)
}

// check checks what a push of args, the role's words, that exited code and printed stderr left
// against want. divergent is divergence before the push, and edits is metaedits before it: check
// reads edits only where want sets metaedits, and a caller whose want sets none passes 0. Where a
// recording jj ahead of the real one ran a real push, check reads that push's
// --ignore-working-copy against args and the snapshots the step took before it (checkIgnoring),
// under the pod's form the git wrapper's first layer that push carries (checkPodLayer), and the
// ending of each line of a step that gave up against those snapshots, whatever want says; a push
// whose step failed before any real push has none to read. It reads the recording jj's log once,
// after the push.
func (f *rolePushFixture) check(want rolePushWant, args []string, code int, stderr string, divergent map[string]int, edits int) {
	t := f.t
	t.Helper()
	if code != want.exit {
		t.Fatalf("the push exited %d, want %d; it printed:\n%s", code, want.exit, stderr)
	}
	landed := map[string][]rolePushRemoteCommit{}
	for _, ref := range want.refs {
		f.checkRef(ref, landed, stderr)
	}
	lines := rolePushStepLines(stderr)
	for _, line := range lines.unknown {
		t.Errorf("the push printed %q, a line of legion jj's of no known kind", line)
	}
	if want.table && len(lines.table) != 1 || !want.table && len(lines.table) != 0 {
		t.Errorf("the push printed %q about the table, want %v for one line; it printed:\n%s", lines.table, want.table, stderr)
	}
	switch {
	case want.cannotRead == rolePushReadsAll && len(lines.cannotRead) != 0,
		want.cannotRead == rolePushMayNotRead && len(lines.cannotRead) > 1,
		want.cannotRead == rolePushCannotReadIt && len(lines.cannotRead) != 1:
		t.Errorf("the push printed %q of a dry run the step could not read, want %s; it printed:\n%s", lines.cannotRead, [...]string{"none", "none or one", "one"}[want.cannotRead], stderr)
	}
	for _, line := range lines.cannotRead {
		if !strings.Contains(line, want.cannotReadNames) {
			t.Errorf("the push printed %q, want the line to name %q", line, want.cannotReadNames)
		}
	}
	if len(lines.cannotSet) != want.cannotSet {
		t.Errorf("the push printed %q of an author the step could not set, want %d such lines; it printed:\n%s", lines.cannotSet, want.cannotSet, stderr)
	}
	for _, line := range lines.cannotSet {
		if !strings.Contains(line, want.cannotSetNames) {
			t.Errorf("the push printed %q, want the line to name %q", line, want.cannotSetNames)
		}
	}
	if !slices.Equal(lines.ids, want.ids) {
		t.Errorf("the push printed %v of a revision it gives as ids, want exactly %v; it printed:\n%s", lines.ids, want.ids, stderr)
	}
	f.checkRewrites(want, lines.rewrites, landed, stderr)
	for commit, author := range want.same {
		f.checkSame(commit, author)
	}
	if !want.diverges {
		if after := f.divergence(); !maps.Equal(divergent, after) {
			t.Errorf("the visible commits of each divergent change were %v before the push and %v after it, want no change made divergent", divergent, after)
		}
	}
	if want.metaedits != 0 {
		if got := f.metaedits() - edits; got != want.metaedits {
			t.Errorf("the push added %d metaedit operations, want %d", got, want.metaedits)
		}
	}
	calls := f.calls()
	if want.noMetaedit {
		for _, call := range calls {
			if call.has("metaedit") {
				t.Errorf("the recording jj ran %q, want no metaedit", call.args)
			}
		}
	}
	if want.snapshots != rolePushSnapshotsUnread || want.pushed != nil || slices.ContainsFunc(calls, rolePushCall.realPush) {
		push := f.oneRealPush(calls)
		var taken [][]string
		for _, call := range calls[:push] {
			if !call.has("--ignore-working-copy") {
				taken = append(taken, call.args)
			}
		}
		if want.snapshots != rolePushSnapshotsUnread {
			if n := int(want.snapshots - rolePushTakesNoSnapshot); len(taken) != n {
				t.Errorf("the step ran %q with no --ignore-working-copy, want %d such command(s), each a snapshot of the working copy", taken, n)
			}
		}
		// The line of a step that gave up says --ignore-working-copy where the step took its
		// snapshot, and only there.
		suffix := rolePushAsWrittenSuffix
		if len(taken) > 0 {
			suffix = rolePushAsWrittenIgnoringSuffix
		}
		for _, line := range slices.Concat(lines.cannotRead, lines.cannotSet) {
			if !strings.HasSuffix(line, suffix) {
				t.Errorf("the push printed %q, want it to end %q", line, suffix)
			}
		}
		f.checkIgnoring(args, calls[push].args, len(taken))
		if f.form == rolePushPod {
			f.checkPodLayer(calls[push].args)
		}
		if want.pushed != nil {
			want.pushed(calls[push].args)
		}
	}
}

// checkIgnoring checks pushed, the words of the real push, against args, the role's words, after
// snapshots commands of the step's that took a snapshot of the working copy: after the step's
// snapshot the push's first word is --ignore-working-copy, which it carries once, so that it takes
// no snapshot of its own; where the step took none, the push carries that flag as many times as
// the role wrote it.
func (f *rolePushFixture) checkIgnoring(args, pushed []string, snapshots int) {
	f.t.Helper()
	count := func(words []string) int {
		n := 0
		for _, word := range words {
			if word == "--ignore-working-copy" {
				n++
			}
		}
		return n
	}
	switch got, written := count(pushed), count(args); {
	case snapshots > 0 && (len(pushed) == 0 || pushed[0] != "--ignore-working-copy" || got != 1):
		f.t.Errorf("the real push ran with %q after the step's snapshot, want --ignore-working-copy as its first word and nowhere else", pushed)
	case snapshots == 0 && got != written:
		f.t.Errorf("the real push ran with %q, which holds --ignore-working-copy %d times, after no snapshot of the step's; want it %d times, as the role wrote it in %q", pushed, got, written, args)
	}
}

// pushedAfterSnapshot is a check of the words of a real push (rolePushWant.pushed) that runs after
// the step's snapshot: the --ignore-working-copy that the snapshot gives it, then args, the role's
// own words, every byte, beside the pod's jj's own layers (written).
func (f *rolePushFixture) pushedAfterSnapshot(args []string) func(words []string) {
	return func(words []string) {
		if written := slices.Concat([]string{"--ignore-working-copy"}, args); !slices.Equal(f.written(words), written) {
			f.t.Errorf("the real push ran with %q, want the step's --ignore-working-copy, then the role's own words: %q", words, written)
		}
	}
}

// written is words, the arguments of a command the recording jj ran, without the git wrapper's two
// layers that the pod's jj writes into a line it runs (packages/daemon/docker/jj): the first, a
// podForces word then `--config-file=/dev/null`, and the last, `--config-file=/dev/null` then a
// podForces word. Of the two layers' four words it takes those alone: a podForces word with no
// `--config-file=/dev/null` on the side its layer has it stays, as does every other word. Under the
// tmux form, which writes no layer, words come back as they are.
func (f *rolePushFixture) written(words []string) []string {
	kept := make([]string, 0, len(words))
	first := true
	for i := 0; i < len(words); i++ {
		word := words[i]
		if !slices.Contains(f.podForces, word) {
			kept = append(kept, word)
			continue
		}
		was := first
		first = false
		switch n := len(kept); {
		case was && i+1 < len(words) && words[i+1] == "--config-file=/dev/null":
			i++
		case !was && n > 0 && kept[n-1] == "--config-file=/dev/null":
			kept = kept[:n-1]
		default:
			kept = append(kept, word)
		}
	}
	return kept
}

// checkPodLayer checks that pushed, the words of a real push under the pod's form, carry the git
// wrapper's first layer as the pod's jj writes it, after the step's --ignore-working-copy where
// the push has it: a podForces word, then `--config-file=/dev/null`.
func (f *rolePushFixture) checkPodLayer(pushed []string) {
	f.t.Helper()
	words := pushed
	if len(words) > 0 && words[0] == "--ignore-working-copy" {
		words = words[1:]
	}
	if len(words) < 2 || !slices.Contains(f.podForces, words[0]) || words[1] != "--config-file=/dev/null" {
		f.t.Errorf("the real push ran with %q, want the pod's jj's first layer, one of %q then --config-file=/dev/null, in front of the role's words", pushed, f.podForces)
	}
}

// oneRealPush is the index among calls, the commands the recording jj ran, of its real push; it
// fails the test where calls hold no real push, or more than one.
func (f *rolePushFixture) oneRealPush(calls []rolePushCall) int {
	f.t.Helper()
	push := slices.IndexFunc(calls, rolePushCall.realPush)
	if push < 0 || slices.ContainsFunc(calls[push+1:], rolePushCall.realPush) {
		f.t.Fatalf("the recording jj ran %v, want one real push", calls)
	}
	return push
}

// checkRef checks what a remote holds of ref, and adds each commit it holds to landed, by change.
func (f *rolePushFixture) checkRef(ref rolePushRef, landed map[string][]rolePushRemoteCommit, stderr string) {
	t := f.t
	t.Helper()
	bare := f.remote(ref.remote)
	got, ok := f.remoteCommits(bare, ref.ref)
	if ref.absent {
		if ok {
			t.Errorf("%s holds %s afterwards, %v, want none; the push printed:\n%s", filepath.Base(bare), ref.ref, got, stderr)
		}
		return
	}
	if !ok {
		t.Errorf("%s holds no %s afterwards; the push printed:\n%s", filepath.Base(bare), ref.ref, stderr)
		return
	}
	if len(got) != len(ref.commits) {
		t.Errorf("%s %s holds %d commits above the seed, %v, want %d; the push printed:\n%s", filepath.Base(bare), ref.ref, len(got), got, len(ref.commits), stderr)
		return
	}
	run := f.runner(f.setupPath, rolePushImplementer)
	for i, w := range ref.commits {
		g := got[i]
		change := f.changeOf(g.id)
		if change != w.change {
			t.Errorf("%s %s's commit %d is %s of change %q, want a commit of %s", filepath.Base(bare), ref.ref, i, g.id, change, w.change)
		}
		if w.author != rolePushNobody && (g.name != w.author.name || g.email != w.author.email) {
			t.Errorf("%s %s's commit %d, %s, has author %s <%s>, want %s <%s>; the push printed:\n%s", filepath.Base(bare), ref.ref, i, g.id, g.name, g.email, w.author.name, w.author.email, stderr)
		}
		if w.commit != "" && g.id != w.commit {
			t.Errorf("%s %s's commit %d is %s, want %s, unchanged", filepath.Base(bare), ref.ref, i, g.id, w.commit)
		}
		if w.file != "" {
			if out, err := run.Try(f.root, f.git, "--git-dir", bare, "show", g.id+":"+w.file); err != nil || out != w.content {
				t.Errorf("%s %s's commit %d holds %s = %q (%v), want %q", filepath.Base(bare), ref.ref, i, w.file, out, err, w.content)
			}
		}
		if w.lacks != "" {
			if out, err := run.Try(f.root, f.git, "--git-dir", bare, "show", g.id+":"+w.lacks); err == nil {
				t.Errorf("%s %s's commit %d holds %s = %q, want no such file", filepath.Base(bare), ref.ref, i, w.lacks, out)
			}
		}
		landed[change] = append(landed[change], g)
	}
}

// checkRewrites checks the printed rewrites against want's, each line matched once.
func (f *rolePushFixture) checkRewrites(want rolePushWant, printed []rolePushPrinted, landed map[string][]rolePushRemoteCommit, stderr string) {
	t := f.t
	t.Helper()
	if !want.others && len(printed) != len(want.rewrites) {
		t.Errorf("the push printed %d rewrites, %v, want %d, one for each of %v; it printed:\n%s", len(printed), printed, len(want.rewrites), want.rewrites, stderr)
	}
	used := make([]bool, len(printed))
	for _, w := range want.rewrites {
		found := false
		for i, p := range printed {
			if used[i] || p.role != w.role || (p.old != w.old && !w.rebased && !w.snapshotted) {
				continue
			}
			if f.changeOf(p.old) != w.change || f.changeOf(p.new) != w.change {
				continue
			}
			if !slices.Contains(landed[w.change], rolePushRemoteCommit{id: p.new, name: w.author.name, email: w.author.email}) {
				continue
			}
			used[i], found = true, true
			break
		}
		if !found {
			t.Errorf("the push printed no line `legion jj: %s: %s -> <the commit of change %s the remote got>`; it printed:\n%s", w.role, w.old, w.change, stderr)
		}
	}
	for _, p := range printed {
		if _, kept := want.same[p.old]; kept {
			t.Errorf("the push rewrote %s, a commit it must leave as it is", p.old)
		}
	}
}

// checkSame checks that commit is still visible with author.
func (f *rolePushFixture) checkSame(commit string, author rolePushIdentity) {
	f.t.Helper()
	got := f.jj(rolePushImplementer, "--ignore-working-copy", "log", "-r", commit, "--no-graph", "-T", `hidden ++ "\n" ++ author.name() ++ "\n" ++ author.email() ++ "\n"`)
	if want := "false\n" + author.name + "\n" + author.email + "\n"; got != want {
		f.t.Errorf("commit %s afterwards: hidden, author name and email %q, want it visible with author %s <%s>", commit, got, author.name, author.email)
	}
}

// rolePushSends is what a push of one commit c to ref leaves: the remote holds c with the author
// of role, and the step printed c's rewrite. A row whose argument names c by a commit id that the
// step's rewrite hid lists the one line of that argument's ids itself (rolePushWant.ids).
func rolePushSends(ref string, c rolePushChange, role string, author rolePushIdentity) rolePushWant {
	return rolePushWant{
		refs:     []rolePushRef{{ref: "refs/heads/" + ref, commits: []rolePushCommit{{change: c.change, author: author}}}},
		rewrites: []rolePushRewrite{{role: role, author: author, change: c.change, old: c.commit}},
	}
}

// rolePushWaitingForm is a push that sends @ with a write no command has snapshotted: its
// arguments, the bookmark name it sends @ under ("" for -c's own), whether the setup puts
// legion/K-1 on @, and whether that bookmark is a tracked one.
type rolePushWaitingForm struct {
	name              string
	args              []string
	ref               string
	bookmark, tracked bool
}

// rolePushWaitingChange is the waiting form that sends @ by itself, under a bookmark of -c's own.
var rolePushWaitingChange = rolePushWaitingForm{name: "-c @", args: []string{"git", "push", "-c", "@"}}

var rolePushWaitingForms = []rolePushWaitingForm{
	{name: "--bookmark on @", args: rolePushBookmark, ref: "legion/K-1", bookmark: true},
	rolePushWaitingChange,
	{name: "--named x=@", args: []string{"git", "push", "--named", "x=@"}, ref: "x"},
	// -r sends a bookmark that points at the commit, and jj refuses to make a remote bookmark for
	// one that tracks nothing.
	{name: "-r @ with a bookmark on @", args: []string{"git", "push", "-r", "@"}, ref: "legion/K-1", bookmark: true, tracked: true},
	{name: "no argument with the tracked bookmark on @", args: []string{"git", "push"}, ref: "legion/K-1", bookmark: true, tracked: true},
}

// rolePushWaitingContent is the write that waits in @.
const rolePushWaitingContent = "a write no command has snapshotted\n"

// rolePushWaitingRow is the row of a waiting form: @ made by the other role's jj new, or by the
// implementer's when rewrite is false, described by the implementer, and a write on disk that no
// command has snapshotted (rolePushFixture.waiting).
func rolePushWaitingRow(form rolePushWaitingForm, rewrite bool) rolePushRow {
	row := "@ by the other role with a waiting write, " + form.name
	if !rewrite {
		row = "@ by its trailer's role with a waiting write, " + form.name
	}
	return rolePushRow{name: row, args: form.args, setup: func(f *rolePushFixture) rolePushWant {
		_, want := f.waiting(form, rewrite)
		return want
	}}
}

// waiting makes @ for a push of form: a commit on main that the other role's jj new made, or the
// implementer's when rewrite is false, and that the implementer described, with a write on disk
// that no command has snapshotted. It answers @ as the step reads it before its snapshot, and what
// the push leaves: the remote holds @'s change under form's ref with the write and the
// implementer's author, and the step printed its rewrite of @, the commit its snapshot made of @,
// when rewrite is true. A form with --ignore-working-copy takes no snapshot
// (rolePushWaitingIgnoredForms): the remote holds @'s change without the write, and the step's
// rewrite is of @ as it read it.
func (f *rolePushFixture) waiting(form rolePushWaitingForm, rewrite bool) (rolePushChange, rolePushWant) {
	f.t.Helper()
	maker := rolePushReviewer
	if !rewrite {
		maker = rolePushImplementer
	}
	if form.tracked {
		f.track("legion/K-1")
	}
	f.jj(maker, "new", "main")
	f.jj(rolePushImplementer, "describe", "-m", rolePushMessage("work in @", "implementer"))
	if form.bookmark {
		f.bookmark("legion/K-1", "@")
	}
	c := f.read("@")
	f.writeFile(filepath.Join(f.repo, "waiting.txt"), rolePushWaitingContent)
	ref := form.ref
	if ref == "" {
		ref = "push-" + c.change[:12]
	}
	sent := rolePushCommit{change: c.change, author: rolePushImplementer, file: "waiting.txt", content: rolePushWaitingContent}
	ignores := slices.Contains(form.args, "--ignore-working-copy")
	if ignores {
		sent = rolePushCommit{change: c.change, author: rolePushImplementer, lacks: "waiting.txt"}
	}
	want := rolePushWant{refs: []rolePushRef{{ref: "refs/heads/" + ref, commits: []rolePushCommit{sent}}}}
	if rewrite {
		want.rewrites = []rolePushRewrite{{role: "implementer", author: rolePushImplementer, change: c.change, old: c.commit, snapshotted: !ignores}}
	}
	return c, want
}

// rolePushHostJJ is a host's jj wrapper that runs jj by PATH, which comes back into the first jj
// on PATH; it gives up after three runs in one chain, so a chain that never ends fails the row
// rather than the test's time.
const rolePushHostJJ = `#!/bin/sh
depth=${HOST_JJ_DEPTH:-0}
if [ "$depth" -ge 3 ]; then
  echo "legion test: the host's jj ran $depth times in one chain" >&2
  exit 97
fi
HOST_JJ_DEPTH=$((depth + 1))
export HOST_JJ_DEPTH
exec jj "$@"
`

// rolePushHostPath is the PATH of a host's jj script that calls jj by PATH, after the form's jj.
func rolePushHostPath(f *rolePushFixture) string {
	host := filepath.Join(f.root, "host")
	f.script(host, "jj", rolePushHostJJ)
	if f.form == rolePushTmux {
		return f.install(host+":"+f.after) + ":" + host + ":" + f.after
	}
	return f.formDir + ":" + host + ":" + f.after
}
