package main

import (
	"cmp"
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
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

// The tests in this file are those of the step that runs before every `jj git push` and sets the
// author of each commit the push sends from that commit's first Legion-Role trailer
// (dispatch://INFRA-4172). Each runs a role's push as a process, through one of the two forms of
// Legion's jj, on a real jj repository with a local bare remote:
//
//   - the pod's form: a copy of packages/daemon/docker/jj first on PATH, whose real=, wrapper= and,
//     where it has one, legion= lines are repointed at the real jj (or a recording jj ahead of it),
//     a git first on PATH that runs the real git, and this test binary run as legion
//     (trailerLegion);
//   - the tmux form: the bin directory that workerbin.Install writes for that legion, first on
//     PATH, with the real jj (or a recording jj ahead of it) after it.
//
// Every row runs under both forms. The file names nothing the step's change adds: the table's
// variable and those of the test binary's bounds of the step are written out, and every command's
// environment is built here. So it compiles and runs at the base too, where docker/jj hands no push
// to `legion jj` and workerbin.Install writes no jj; there the tests of a change fail, and those of
// what stays pass.

// rolePushVariable is the table of every role's identity that the daemon writes into a role
// process's environment.
const rolePushVariable = "LEGION_ROLE_IDENTITIES"

// rolePushBoundVariable is the variable that this package's test binary, run as legion, reads as
// the step's bound, in bytes, of a word it builds of ids; the legion binary reads no such variable.
const rolePushBoundVariable = "LEGION_TEST_JJ_WORD_BOUND"

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

// rolePushEachForm runs test under each form, each in a parallel subtest named for the form.
func rolePushEachForm(t *testing.T, test func(t *testing.T, form rolePushForm)) {
	for _, form := range rolePushForms {
		t.Run(string(form), func(t *testing.T) {
			t.Parallel()
			test(t, form)
		})
	}
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
// is_push, is_dry, is_log and is_set telling whether the command holds the word metaedit, push,
// --dry-run or log, or a revset that names remote_bookmarks (the step's read of the set). A
// behaviour that sets ran answers the command itself, with status.
const (
	// rolePushRecords runs every command on the real jj.
	rolePushRecords = ":"
	// rolePushFailsMetaedit fails every metaedit, as jj fails, before it changes anything.
	rolePushFailsMetaedit = `if [ -n "$is_metaedit" ]; then echo 'Error: legion test: this metaedit fails' >&2; status=1; ran=1; fi`
	// rolePushFailsSetRead fails the read of the commits the push sends.
	rolePushFailsSetRead = `if [ -n "$is_log" ] && [ -n "$is_set" ]; then echo 'Error: legion test: this read of the set fails' >&2; status=1; ran=1; fi`
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

// rolePushRecorder is a recording jj in front of real: it runs behaviour, then the real jj unless
// behaviour answered, and appends to log the time it started and ended, its status and its
// arguments, each field ended by \037 and each record by \036, which no argument holds.
func rolePushRecorder(real, log, behaviour string) string {
	return "#!/bin/sh\n" +
		"real='" + real + "'\n" +
		"start=$(date +%s%N)\n" +
		"ran= status=0 is_metaedit= is_push= is_dry= is_log= is_set=\n" +
		"for arg; do\n" +
		"  case $arg in\n" +
		"    metaedit) is_metaedit=1 ;;\n" +
		"    push) is_push=1 ;;\n" +
		"    --dry-run) is_dry=1 ;;\n" +
		"    log) is_log=1 ;;\n" +
		"    *remote_bookmarks*) is_set=1 ;;\n" +
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

// realPush is whether the call is a push that sends: no dry run.
func (c rolePushCall) realPush() bool { return c.has("push") && !c.has("--dry-run") }

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
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// podDir writes the pod's wrappers into dir, as the image's /opt/legion/bin holds them: a git that
// runs the real git, and packages/daemon/docker/jj with real= naming the form's next jj, wrapper=
// naming that git, and legion=, where the script has that line, naming this test binary run as
// legion. It answers dir.
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
	sh, err := exec.LookPath("sh")
	if err != nil {
		f.t.Fatal(err)
	}
	return trailerRun(f.t, f.repo, f.env(path, as), "", sh, append([]string{"-c", `exec jj "$@"`, "jj"}, args...)...)
}

// alone runs the real jj by itself, with no form of Legion's in front of it.
func (f *rolePushFixture) alone(as rolePushIdentity, args ...string) (code int, stdout, stderr string) {
	f.t.Helper()
	return trailerRun(f.t, f.repo, f.env(f.setupPath, as), "", f.real, args...)
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
	// waiting is whether @ holds a write no command has snapshotted, which the push's own
	// snapshot takes after the step: the printed new commit is then matched to the remote's by its
	// change.
	waiting bool
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
	// ids are the lines `legion jj: revision `<argument>` names other commits after the step; …
	// in <k> word(s) of its option, …` the push prints, in order: those of more than one word, and
	// those of one word too where oneWord is set. A row that leaves oneWord unset reads no line of
	// a revision the step gives as its ids in one word, which the step gives any argument that names
	// a commit it rewrote by that commit's id.
	ids     []rolePushIDs
	oneWord bool
	// pushed, when not nil, checks the words of the real push, which a recording jj ahead of the
	// real one saw (oneRealPush).
	pushed func(words []string)
}

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
}

// rolePushPrinted is one line `legion jj: <role>: <old> -> <new>`.
type rolePushPrinted struct{ role, old, new string }

var rolePushRewriteLine = regexp.MustCompile(`^legion jj: ([a-z]+): ([0-9a-f]{40}) -> ([0-9a-f]{40})$`)

// rolePushIDs is one line `legion jj: revision `<argument>` names other commits after the step;
// the push gets the <n> commit(s) … by their ids, in <k> word(s) of its option, each under <bound>
// bytes`: the argument, n, k and the bound.
type rolePushIDs struct {
	argument              string
	commits, words, bound int
}

var rolePushIDsLine = regexp.MustCompile(`^legion jj: revision ` + "`(.*)`" + ` names other commits after the step; the push gets the ([0-9]+) commit\(s\) it named before the step, as the step left them, by their ids, in ([0-9]+) word\(s\) of its option, each under ([0-9]+) bytes$`)

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
	rolePushTablePrefix      = "legion jj: " + rolePushVariable + " is "
	rolePushTableSuffix      = "; the push runs with no author step"
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
		switch {
		case strings.HasPrefix(line, rolePushCannotReadPrefix) && strings.HasSuffix(line, rolePushAsWrittenSuffix):
			lines.cannotRead = append(lines.cannotRead, line)
		case strings.HasPrefix(line, rolePushCannotSetPrefix) && strings.HasSuffix(line, rolePushAsWrittenSuffix):
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
// setup that makes the repository it pushes from and says what the push must leave.
type rolePushRow struct {
	name  string
	args  []string
	setup func(f *rolePushFixture) rolePushWant
}

// rolePushRun runs each row under each form, each in a repository of its own.
func rolePushRun(t *testing.T, rows []rolePushRow) {
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, "")
				want := row.setup(f)
				f.pushAndCheck(row.args, want)
			})
		})
	}
}

// rolePushArgsRow is one row of a test whose push's arguments name what its setup made: the setup
// makes the repository the push goes from, and answers the push's arguments and what the push
// must leave.
type rolePushArgsRow struct {
	name  string
	setup func(f *rolePushFixture) ([]string, rolePushWant)
}

// rolePushRunArgs runs each row under each form, each in a repository of its own, with a recording
// jj of behaviour ahead of the real one where behaviour is not "".
func rolePushRunArgs(t *testing.T, behaviour string, rows []rolePushArgsRow) {
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, behaviour)
				args, want := row.setup(f)
				f.pushAndCheck(args, want)
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
	f.check(want, code, stderr, divergent, edits)
}

// check checks what a push that exited code and printed stderr left against want. divergent is
// divergence before the push, and edits is metaedits before it: check reads edits only where want
// sets metaedits, and a caller whose want sets none passes 0.
func (f *rolePushFixture) check(want rolePushWant, code int, stderr string, divergent map[string]int, edits int) {
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
	ids := lines.ids
	if !want.oneWord {
		ids = slices.DeleteFunc(slices.Clone(ids), func(l rolePushIDs) bool { return l.words == 1 })
	}
	if !slices.Equal(ids, want.ids) {
		t.Errorf("the push printed %v of a revision it gives as ids, want %v (one-word lines read: %v); it printed:\n%s", lines.ids, want.ids, want.oneWord, stderr)
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
			t.Errorf("the push added %d metaedit operations, want %d, one for each role", got, want.metaedits)
		}
	}
	if want.noMetaedit {
		for _, call := range f.calls() {
			if call.has("metaedit") {
				t.Errorf("the recording jj ran %q, want no metaedit", call.args)
			}
		}
	}
	if want.pushed != nil {
		calls, push := f.oneRealPush()
		want.pushed(calls[push].args)
	}
}

// oneRealPush is every command the recording jj ran, and the index among them of its real push;
// it fails the test where that jj ran no real push, or more than one.
func (f *rolePushFixture) oneRealPush() (calls []rolePushCall, push int) {
	f.t.Helper()
	calls = f.calls()
	push = slices.IndexFunc(calls, rolePushCall.realPush)
	if push < 0 || slices.ContainsFunc(calls[push+1:], rolePushCall.realPush) {
		f.t.Fatalf("the recording jj ran %v, want one real push", calls)
	}
	return calls, push
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
			if used[i] || p.role != w.role || (p.old != w.old && !w.rebased) {
				continue
			}
			if f.changeOf(p.old) != w.change || f.changeOf(p.new) != w.change {
				continue
			}
			if !want.waiting && !slices.Contains(landed[w.change], rolePushRemoteCommit{id: p.new, name: w.author.name, email: w.author.email}) {
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
// of role, and the step printed c's rewrite.
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
// that no command has snapshotted. It answers @ as the step reads it, and what the push leaves:
// the remote holds @'s change under form's ref with the write and the implementer's author, and
// the step printed its rewrite of @ when rewrite is true.
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
	want := rolePushWant{waiting: true, refs: []rolePushRef{{ref: "refs/heads/" + ref, commits: []rolePushCommit{
		{change: c.change, author: rolePushImplementer, file: "waiting.txt", content: rolePushWaitingContent},
	}}}}
	if rewrite {
		want.rewrites = []rolePushRewrite{{role: "implementer", author: rolePushImplementer, change: c.change, old: c.commit}}
	}
	return c, want
}

// rolePushAuthorRows are the ways a commit comes to have another role's author, and the states of
// the commits a push sends, each pushed by the implementer.
func rolePushAuthorRows() []rolePushRow {
	impl, rev := rolePushImplementer, rolePushReviewer
	rows := []rolePushRow{
		{name: "made by the other role's jj new", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
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
		{name: "the push's own --config user.email", args: []string{"git", "push", "--bookmark", "legion/K-1", "--config", "user.name=Another Name", "--config", "user.email=another@example.invalid"},
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
// for each commit it rewrote, and leaves no change divergent.
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
		{name: "--change <commit id>", args: []string{"git", "push", "--change", "{commit:@-}"}, setup: underChange},
		{name: "--named nm=<commit id>", args: []string{"git", "push", "--named", "nm={commit:@-}"}, setup: named},
		{name: "--revisions <commit id>", args: []string{"git", "push", "--revisions", "{commit:@-}"}, setup: onTracked},
		{name: "--change=<commit id>", args: []string{"git", "push", "--change={commit:@-}"}, setup: underChange},
		{name: "-c<commit id>", args: []string{"git", "push", "-c{commit:@-}"}, setup: underChange},
		{name: "--named=nm=<commit id>", args: []string{"git", "push", "--named=nm={commit:@-}"}, setup: named},
		{name: "-r<commit id>", args: []string{"git", "push", "-r{commit:@-}"}, setup: onTracked},
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
// rewritten commit in the real push, in every spelling; each commit-id row names @-, a commit
// other than @. A push to two remotes sends the same rewritten commit to both, leaves the commit
// one remote already holds as it is, and forces neither.
func TestEveryPushFormSendsTheRewrittenCommit(t *testing.T) {
	t.Parallel()
	rolePushRun(t, rolePushFormRows())
	for _, first := range rolePushTwoRemoteFirsts {
		t.Run("two remotes, "+first.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
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
	return []rolePushArgsRow{
		{name: "-c <C1> | <C2>, two commits on main", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c1, c2 := work(f, "main", "work 1"), work(f, "main", "work 2")
			want := rolePushSends("push-"+c1.change[:12], c1, "implementer", impl)
			sent := rolePushSends("push-"+c2.change[:12], c2, "implementer", impl)
			want.refs = append(want.refs, sent.refs...)
			want.rewrites = append(want.rewrites, sent.rewrites...)
			return []string{"git", "push", "-c", c1.commit + " | " + c2.commit}, want
		}},
		{name: "-c present(<the first 12 hex digits of C>)", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c := work(f, "main", "work")
			return []string{"git", "push", "-c", "present(" + c.commit[:12] + ")"}, rolePushSends("push-"+c.change[:12], c, "implementer", impl)
		}},
		// The step rewrites C alone, and its metaedit rebases D onto the new C: the D of the real
		// push is the rebased D, whose parent is the new C.
		{name: "--named nm=<D>-, D a commit on C", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c := work(f, "main", "C")
			d := work(f, c.commit, "D")
			return []string{"git", "push", "--named", "nm=" + d.commit + "-"}, rolePushSends("nm", c, "implementer", impl)
		}},
		// D is a number that starts the id of one commit of the repository, hidden ones included:
		// a commit of C's change that a describe hid. jj reads the argument with D a number, which
		// names C alone among the commits a metaedit can rewrite, so the step follows C, and the
		// real push gets the ids of the commits the argument named before the step, the commit the
		// step made of C in place of C.
		{name: "-r ancestors(<C>, <D>), D a number that starts the id of a hidden commit of C's change", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			f.track("legion/K-1")
			c := work(f, "main", "work")
			f.bookmark("legion/K-1", c.commit)
			c, depth := f.decimalPrefixed(c, impl, rolePushMessage("work", "implementer"), true)
			return []string{"git", "push", "-r", "ancestors(" + c.commit + ", " + depth + ")"}, rolePushSends("legion/K-1", c, "implementer", impl)
		}},
		// @ beside C1, a commit on main, with a write in @ that no command has snapshotted: the
		// push's own snapshot takes the write after the step's rewrite of @, and the remote gets
		// both changes with the implementer's author, @'s with the write.
		{name: "-c @ | <C1>, @ with a waiting write", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			c1 := work(f, "main", "work 1")
			_, want := f.waiting(rolePushWaitingChange, true)
			sent := rolePushSends("push-"+c1.change[:12], c1, "implementer", impl)
			want.refs = append(want.refs, sent.refs...)
			want.rewrites = append(want.rewrites, sent.rewrites...)
			return []string{"git", "push", "-c", "@ | " + c1.commit}, want
		}},
		// The commit id of @, with a write in @ that no command has snapshotted: the step's rewrite
		// of @ makes another working-copy commit, which the push's own snapshot rewrites once more
		// with the write, so the real push names it as @, and the remote gets @'s change with the
		// write and the implementer's author.
		{name: "-c <the commit id of @>, @ with a waiting write", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			w, want := f.waiting(rolePushWaitingChange, true)
			return []string{"git", "push", "-c", w.commit}, want
		}},
		// The commit id @ had before a describe of @ that took no snapshot hid it, with a write in @
		// that no command has snapshotted: the hidden commit stands for @, its change's one visible
		// commit, so the real push names it as @ after the step's rewrite of @, whose old id is the
		// one the describe made, and the remote gets @'s change with the write and the
		// implementer's author.
		{name: "-c <the commit id of a hidden predecessor of @>, @ with a waiting write", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
			hidden, want := f.waiting(rolePushWaitingChange, true)
			f.jj(impl, "--ignore-working-copy", "describe", "-m", rolePushMessage("work in @, described again", "implementer"))
			want.rewrites[0].old = f.read("@").commit
			return []string{"git", "push", "-c", hidden.commit}, want
		}},
	}
}

// A commit id inside a revision argument's revset, each of `C1 | C2`, a prefix of C in present(),
// the D of `D-`, the C of `ancestors(C, D)`, C1 beside @, and the commit id of @ on its own, is the
// rewritten commit in the real push, which gets the ids of the commits the argument named before
// the step, each commit the step rewrote as the commit it made of it and @ as @: the remote's
// commits have their trailers' authors, the step prints each rewrite, and no change is divergent.
// The D of `ancestors(C, D)` is a number that starts the id of a hidden commit whose change has
// one visible commit, and jj reads it as a number. A write in @ that no command has snapshotted
// reaches the remote, and so it does where the commit id is one @ had before a rewrite of @ hid
// it. `::C` has a test of its own, TestTheRealPushsArgumentCarriesNoIDOfACommitTheStepLeftAlone.
func TestACommitIDInsideARevsetSendsTheRewrittenCommit(t *testing.T) {
	t.Parallel()
	rolePushRunArgs(t, "", rolePushRevsetRows())
}

// rolePushFullID is a full commit id.
var rolePushFullID = regexp.MustCompile(`[0-9a-f]{40}`)

// `jj git push -r ::<C>`, a tracked bookmark on C (-r sends a bookmark that points at the commit,
// and jj refuses to make a remote bookmark for one that tracks nothing), on a history of five
// commits under C that origin holds (rolePushFixture.history), with the step's bound of a word of
// ids lowered to rolePushOneWordBound bytes, over the 132 bytes of the argument's primary form
// and under the 341 of the ids of the eight commits it names: the real push's argument carries no
// id of a commit the step left alone. jj reads the argument among the commits the step can
// rewrite, C alone, so a recording jj ahead of the real one sees the real push's argument, a word
// under the bound, hold the ids of C's change and of no other commit, the history's, the seed's
// and the root's among them: where the ids of what it names pass the bound, its length grows with
// the commits the step rewrote, not with C's history. The remote gets the rewritten C with its
// trailer's author, the step prints the rewrite, and no change is divergent.
func TestTheRealPushsArgumentCarriesNoIDOfACommitTheStepLeftAlone(t *testing.T) {
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
			}
			return []string{"git", "push", "-r", "::" + c.commit}, want
		}},
	})
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
	// asWritten is args and want, and what the real push's words must be: args, every byte.
	asWritten := func(f *rolePushFixture, args []string, want rolePushWant) ([]string, rolePushWant) {
		want.pushed = func(words []string) {
			if !slices.Equal(words, args) {
				f.t.Errorf("the real push ran with %q, want the role's own words %q", words, args)
			}
		}
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
// with the role's own words, every byte, and the remote gets the rewritten commit with its
// trailer's author, @'s with the write, and no change is divergent.
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
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, "")
		message := rolePushMessage("work", "implementer")
		c := f.commitOn(rev, impl, "main", message)
		c, count := f.decimalPrefixed(c, impl, message, false)
		f.pushAndCheck([]string{"git", "push", "-c", "latest(" + c.commit + ", " + count + ")"}, rolePushSends("push-"+c.change[:12], c, "implementer", impl))
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
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, "")
				c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
				f.pushAndCheck([]string{"git", "push", "-c", quote + c.commit + quote}, rolePushSends("push-"+c.change[:12], c, "implementer", impl))
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
// the trailer's author. That retry, of a divergent change's commit, runs in a later second than
// the refused push's rewrite; TestASameSecondRetrySendsTheCommitTheRefusedPushMade runs it in the
// same second.
func TestARefusedPushLeavesNoBookmarkAndItsRetrySends(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushRefusedRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
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
				want.rewrites = nil
				divergent := f.divergence()
				code, _, stderr := f.push(rolePushImplementer, args...)
				f.check(want, code, stderr, divergent, 0)
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
// again. The step takes that commit, prints its rewrite of D1, and sends it with the
// implementer's author, and makes no change more divergent.
func TestASameSecondRetrySendsTheCommitTheRefusedPushMade(t *testing.T) {
	t.Parallel()
	row := rolePushDivergentRefusedRow()
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
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
		divergent := f.divergence()
		code, _, stderr := f.push(rolePushImplementer, args...)
		f.check(want, code, stderr, divergent, 0)
	})
}

// A retry after a `jj op restore` to the operation before the refused push, in the second of that
// push's rewrite (one fixed JJ_TIMESTAMP): the restore puts the bookmark back on the commit the
// role made, C, and hides the commit the refused push's rewrite made of it, which the step's
// rewrite of C would make again, and jj refuses to make a commit that exists. The step prints one
// line that names jj's error and changes nothing more, and the push sends C as jj alone does,
// with its author as it is, and makes no change divergent.
func TestARetryAfterARestoreSendsTheCommitAsJJDoes(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
		f := newRolePushFixture(t, form, "")
		f.timestamp = rolePushFixedTimestamp
		c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
		f.bookmark("legion/K-1", c.commit)
		before := f.op()
		printed := rolePushStepLines(f.refusedPush(rolePushBookmark)).rewrites
		if len(printed) != 1 || printed[0].old != c.commit {
			t.Fatalf("the refused push printed the rewrites %v, want one, of %s", printed, c.commit)
		}
		made := printed[0].new
		f.jj(impl, "op", "restore", before)
		if on := f.read("legion/K-1").commit; on != c.commit {
			t.Fatalf("legion/K-1 is on %s after jj op restore, want %s, the commit the role made", on, c.commit)
		}
		if hidden := f.jj(impl, "--ignore-working-copy", "log", "-r", made, "--no-graph", "-T", "hidden"); hidden != "true" {
			t.Fatalf("%s, the refused push's rewrite, is hidden %q after jj op restore, want true", made, hidden)
		}
		f.pushAndCheck(rolePushBookmark, rolePushWant{
			refs:      []rolePushRef{{ref: "refs/heads/legion/K-1", commits: []rolePushCommit{{change: c.change, author: rev, commit: c.commit}}}},
			same:      map[string]rolePushIdentity{c.commit: rev},
			cannotSet: 1, cannotSetNames: "Newly-created commit " + made + " already exists",
		})
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
// made and that jj has not imported yet sends it as jj alone does. The step's commands run with
// --ignore-working-copy, so none of them imports the commit from git or can read it: the step
// prints its one line of a dry run it cannot read, which names the commit, and changes nothing,
// and the real push imports the commit and sends it as it is.
func TestAPushOfACommitJJHasNotImportedSendsIt(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushUnimportedRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, "")
				f.write()
				run := f.runner(f.setupPath, rolePushImplementer)
				run.Run(f.repo, f.git, "add", "change-"+strconv.Itoa(f.files)+".txt")
				run.Run(f.repo, f.git, "commit", "--quiet", "-m", rolePushMessage("work", "implementer"))
				commit := strings.TrimSpace(run.Out(f.repo, f.git, "rev-parse", "HEAD"))
				if change := f.changeOf(commit); change != "" {
					t.Fatalf("jj reads the git commit %s as change %s before the push, want a commit no jj command imported", commit, change)
				}
				args := make([]string, len(row.args))
				for i, arg := range row.args {
					args[i] = strings.ReplaceAll(arg, rolePushGitCommit, commit)
				}
				divergent := f.divergence()
				code, _, stderr := f.push(rolePushImplementer, args...)
				change := f.changeOf(commit)
				ref := row.ref
				if ref == "" {
					ref = "push-" + change[:min(12, len(change))]
				}
				f.check(rolePushWant{
					refs:       []rolePushRef{{ref: "refs/heads/" + ref, commits: []rolePushCommit{{change: change, commit: commit}}}},
					cannotRead: rolePushCannotReadIt, cannotReadNames: commit,
				}, code, stderr, divergent, 0)
			})
		})
	}
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
		{name: "no Legion-Role trailer", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
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
			return rolePushSends("push-"+p.change[:12], p, "implementer", impl)
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
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
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
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
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
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
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
		}, got, stderr, divergent, 0)
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
// behaviour, and the dry runs that a flag or the repository's configuration would silence.
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
		{rolePushRow{name: "--quiet on the push", args: []string{"git", "push", "--quiet", "--bookmark", "legion/K-1"}, setup: reads}, ""},
		{rolePushRow{name: "ui.quiet = true in the repository's configuration", args: rolePushBookmark, setup: func(f *rolePushFixture) rolePushWant {
			f.jj(impl, "config", "set", "--repo", "ui.quiet", "true")
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
	// and sends that commit, as jj alone does, which makes it visible again beside the other.
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
		}
	}},
	// The step's loop over jj's refusal of a commit with no author fills the commit the role named
	// before the dry run it cannot read, and that rewrite hides it: the real push gets the commit
	// after the rewrite.
	{name: "a commit id of a commit the step fills before that dry run", setup: func(f *rolePushFixture) ([]string, rolePushWant) {
		c := f.commitOn(rolePushNobody, rolePushNobody, "main", rolePushMessage("work", "implementer"))
		want := rolePushSends("nm", c, "implementer", rolePushImplementer)
		want.cannotRead, want.cannotReadNames = rolePushCannotReadIt, rolePushUnreadableLine
		return []string{"git", "push", "--named", "nm=" + c.commit}, want
	}},
}

// A dry run the step cannot read, one with a line of neither shape in its block or one that exits
// 0 with no block (a recording jj ahead of the real one prints each), makes the step print one
// line and change nothing, and the real push runs as written. A commit id of a commit hidden
// before the push goes to it as the role wrote it, and one whose commit the step's loop over a
// commit with no author rewrote before that dry run gets the commit after the rewrite. A metaedit
// that fills no author ends the step's loop over jj's refusal of a commit with no author the same
// way: one line naming the commit, and the real push, which jj refuses. A --quiet on the push and
// ui.quiet in the repository's configuration do not silence the step's dry run: the remote's
// commit has the trailer's author.
func TestADryRunTheStepCannotReadChangesNothing(t *testing.T) {
	t.Parallel()
	for _, row := range rolePushCannotReadRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
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
// no metaedit.
func TestAPushWithNothingToChangeAddsNoOperation(t *testing.T) {
	t.Parallel()
	impl := rolePushImplementer
	setup := func(f *rolePushFixture) rolePushChange {
		c := f.commitOn(impl, impl, "main", rolePushMessage("work", "implementer"))
		f.bookmark("legion/K-1", c.commit)
		return c
	}
	rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
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
		}, code, stderr, divergent, 0)
		if added := f.ops() - before; added != aloneAdded {
			t.Errorf("the push added %d operations, want %d, as jj alone adds", added, aloneAdded)
		}
	})
}

// The step's jj commands run one after another, and the real push starts after the last of them
// ends (a recording jj ahead of the real one writes when each starts and ends). With a write in @
// that no command has snapshotted and a rewrite to make, every command the recording jj sees
// before the real push carries --ignore-working-copy, so a step command that would snapshot fails
// the row, and the remote gets the write.
func TestTheStepsCommandsRunOneAfterAnother(t *testing.T) {
	t.Parallel()
	// --bookmark on @, -c @ and the push with no argument: the forms that name @ by a bookmark, by
	// itself and by the default revset.
	for _, waiting := range rolePushWaitingForms {
		if !slices.Contains([]string{"--bookmark on @", "-c @", "no argument with the tracked bookmark on @"}, waiting.name) {
			continue
		}
		row := rolePushWaitingRow(waiting, true)
		t.Run(waiting.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				f.pushAndCheck(row.args, row.setup(f))
				calls, pushes := f.oneRealPush()
				push := calls[pushes]
				steps := slices.Delete(slices.Clone(calls), pushes, pushes+1)
				if !slices.ContainsFunc(steps, func(c rolePushCall) bool { return c.has("metaedit") }) {
					t.Errorf("the recording jj ran %v before the push, want the step's metaedit among them", steps)
				}
				slices.SortFunc(steps, func(a, b rolePushCall) int { return cmp.Compare(a.start, b.start) })
				for i, call := range steps {
					if !call.has("--ignore-working-copy") {
						t.Errorf("the step ran %q with no --ignore-working-copy, which takes a snapshot", call.args)
					}
					if i > 0 && call.start < steps[i-1].end {
						t.Errorf("the step ran %q from %d before %q ended at %d", call.args, call.start, steps[i-1].args, steps[i-1].end)
					}
					if push.start < call.end {
						t.Errorf("the real push started at %d, before the step's %q ended at %d", push.start, call.args, call.end)
					}
				}
			})
		})
	}
}

// rolePushFailingRows are the step's jj commands that fail, each a recording jj's behaviour and the
// error it prints.
var rolePushFailingRows = []struct{ name, behaviour, error string }{
	{name: "a metaedit that fails", behaviour: rolePushFailsMetaedit, error: "Error: legion test: this metaedit fails"},
	{name: "a read of the set that fails", behaviour: rolePushFailsSetRead, error: "Error: legion test: this read of the set fails"},
}

// When a jj command of the step fails, the push exits with its status after jj's error, and
// nothing reaches the remote.
func TestAFailingStepSendsNothing(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	for _, row := range rolePushFailingRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, row.behaviour)
				c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
				f.bookmark("legion/K-1", c.commit)
				divergent := f.divergence()
				code, _, stderr := f.push(impl, rolePushBookmark...)
				f.check(rolePushWant{exit: 1, refs: []rolePushRef{{ref: "refs/heads/legion/K-1", absent: true}}}, code, stderr, divergent, 0)
				if !strings.Contains(stderr, row.error) {
					t.Errorf("the push printed\n%s\nwant jj's error %q", stderr, row.error)
				}
			})
		})
	}
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

// rolePushPanesRows are the PATHs on which a pane's jj comes back to itself: its directory twice, a
// host's jj that runs jj by PATH after it, and a second Legion directory in front of it, as a
// controller started from a pane has. The pod's wrapper runs its jj by its full path, so under the
// pod's form a host's jj after it is never reached, and the second directory is another of the
// image's: a bin that workerbin.Install writes in front of the pod's wrappers would hold a git of
// its own, which the pod's jj refuses as no leak check's.
var rolePushPanesRows = []struct {
	name string
	path func(f *rolePushFixture) string
}{
	{name: "its directory twice on PATH", path: func(f *rolePushFixture) string { return f.formDir + ":" + f.path }},
	{name: "a host jj script that calls jj by PATH", path: func(f *rolePushFixture) string {
		host := filepath.Join(f.root, "host")
		f.script(host, "jj", rolePushHostJJ)
		if f.form == rolePushTmux {
			return f.install(host+":"+f.after) + ":" + host + ":" + f.after
		}
		return f.formDir + ":" + host + ":" + f.after
	}},
	{name: "two Legion bin directories", path: func(f *rolePushFixture) string {
		if f.form == rolePushTmux {
			return f.install(f.path) + ":" + f.path
		}
		return f.podDir(filepath.Join(f.root, "second-pod-bin")) + ":" + f.path
	}},
}

// A pane's jj never runs itself again: with its directory twice on PATH, with a host's jj script
// after it that calls jj by PATH, and behind a second Legion bin directory, the step runs once (one
// dry run, one metaedit, one push, which the recording jj sees) and the remote gets the trailer's
// author.
func TestThePanesJJNeverRunsItselfAgain(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	for _, row := range rolePushPanesRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
				f.bookmark("legion/K-1", c.commit)
				path := row.path(f)
				divergent := f.divergence()
				code, _, stderr := f.pushOn(path, impl, rolePushBookmark...)
				f.check(rolePushSends("legion/K-1", c, "implementer", impl), code, stderr, divergent, 0)
				var dry, metaedit, push int
				for _, call := range f.calls() {
					switch {
					case call.has("metaedit"):
						metaedit++
					case call.has("push") && call.has("--dry-run"):
						dry++
					case call.realPush():
						push++
					}
				}
				if dry != 1 || metaedit != 1 || push != 1 {
					t.Errorf("the recording jj saw %d dry runs, %d metaedits and %d pushes, want one of each: the step runs once\n%v", dry, metaedit, push, f.calls())
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
		{name: "jj util exec -- jj git push --bookmark", args: []string{"util", "exec", "--", "jj", "git", "push", "--bookmark", "legion/K-1"}, setup: func(f *rolePushFixture) rolePushWant {
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
// trailer's author, and the step prints the rewrite.
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
