package shim_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// copyRig is an issue's working copies as a pod holds them, with the real git and jj: a shared
// clone, colocated with its git repository, and the issue's own workspace of it, LEGION_WORKSPACE,
// whose working copy stands on the task's head (a commit of the task over the clone's base). The
// shim runs on that workspace, and the jj first on its PATH records each command it is given and
// runs the real one.
type copyRig struct {
	t         *testing.T
	jj        string // the real jj
	home      string
	clone     string
	workspace string // LEGION_WORKSPACE
	fixDir    string // workspace.FixDir(workspace)
	record    string // the file the PATH's jj writes each command's arguments to, one line each
	head      string // the task's head: the commit under the workspace's working copy
	p         *peer
}

// The identity the daemon names on each adoption here, and the one every command of the test's
// own runs under, so an author tells the two apart.
const (
	adoptingUser  = "implementer"
	adoptingEmail = "implementer@example.test"
	setupUser     = "setup"
)

func newCopyRig(t *testing.T) *copyRig {
	t.Helper()
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("jj is required: %v", err)
	}
	state := t.TempDir()
	r := &copyRig{
		t:         t,
		jj:        jj,
		home:      t.TempDir(),
		clone:     filepath.Join(state, "repos", "github.com", "acme", "widgets"),
		workspace: filepath.Join(state, "workspaces", "acme", "widgets", "widgets-42"),
		record:    filepath.Join(t.TempDir(), "jj-commands"),
	}
	r.fixDir = workspace.FixDir(r.workspace)
	for _, dir := range []string{r.clone, filepath.Dir(r.workspace)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The init takes no -R and opens no workspace, so with no Dir jj would walk up from the test
	// process's directory to the closest .jj and write that repository.
	fixturegit.Isolated(t, r.home).With("JJ_USER="+setupUser, "JJ_EMAIL="+setupUser+"@example.test").
		Run(r.clone, append([]string{jj}, fixturegit.JJInit(r.clone, true)[1:]...)...)
	r.write(r.clone, "README.md", "widgets\n")
	r.jjIn(r.clone, "commit", "-m", "chore: the base")
	r.jjIn(r.clone, "workspace", "add", r.workspace, "--name", filepath.Base(r.workspace), "--revision", r.commitOf(r.clone, "@-"))
	r.write(r.workspace, "feature.txt", "the task's change\n")
	r.jjIn(r.workspace, "commit", "-m", "feat: the task's change")
	r.head = r.commitOf(r.workspace, "@-")
	r.startShim()
	return r
}

// startShim starts the shim on the rig's workspace, with a jj first on PATH that records each
// command it is given and runs the real one, and opens its connection.
func (r *copyRig) startShim() {
	t := r.t
	t.Helper()
	// The shim's jj, first on PATH, where the shim looks. It hands the real jj the PATH past its own
	// directory, so a jj on PATH that is itself a wrapper never finds this one again.
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + r.record + "\nPATH=\"${PATH#*:}\" exec " + r.jj + " \"$@\"\n"
	if err := writeExecutable(filepath.Join(bin, "jj"), []byte(script)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(fixtureenv.Isolated(r.home), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+r.workspace)
	run(t, cfg, newClock())
	r.p = daemon.accept(t)
	r.p.open(t)
}

// newOrigin is the repository on GitHub that a pod clones, as a bare repository: its default
// branch main, the base, and the issue key's branch, legion/<key>, a commit of the task over it.
func newOrigin(t *testing.T, key string) string {
	t.Helper()
	dir := t.TempDir()
	origin, seed := filepath.Join(dir, "widgets.git"), filepath.Join(dir, "seed")
	git := fixturegit.Isolated(t, t.TempDir())
	git.Run(dir, fixturegit.InitBare(origin, "main")...)
	git.Run(dir, fixturegit.InitWorkTree(seed, "main")...)
	for _, file := range []struct{ name, body, message string }{
		{"README.md", "widgets\n", "chore: the base"},
		{"feature.txt", "the task's change\n", "feat: the task's change"},
	} {
		if err := os.WriteFile(filepath.Join(seed, file.name), []byte(file.body), 0o644); err != nil {
			t.Fatal(err)
		}
		git.Run(seed, "git", "add", file.name)
		git.Run(seed, "git", "commit", "--quiet", "-m", file.message)
	}
	git.Run(seed, "git", "push", "--quiet", origin, "HEAD~1:refs/heads/main", "HEAD:refs/heads/"+claim.IssueBranch(key))
	return origin
}

// newPodRig is a pod of the issue key as a node provisions it from origin: a clone that holds
// origin's default branch and the issue's branch and nothing else of origin, colocated with jj, and
// the issue's workspace of it at the issue's branch, with a shim on that workspace. jj is the real
// jj, which a test with more than one pod reads once, before any pod puts its recording jj on PATH.
func newPodRig(t *testing.T, jj, origin, key string) *copyRig {
	t.Helper()
	state := t.TempDir()
	r := &copyRig{
		t:         t,
		jj:        jj,
		home:      t.TempDir(),
		clone:     filepath.Join(state, "repos", "github.com", "acme", "widgets"),
		workspace: filepath.Join(state, "workspaces", "acme", "widgets", strings.ToLower(key)),
		record:    filepath.Join(t.TempDir(), "jj-commands"),
	}
	r.fixDir = workspace.FixDir(r.workspace)
	for _, dir := range []string{r.clone, filepath.Dir(r.workspace)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git := fixturegit.Isolated(t, r.home)
	git.Run(r.clone, fixturegit.InitWorkTree("", "main")...)
	git.Run(r.clone, "git", "remote", "add", "origin", origin)
	git.Run(r.clone, "git", "fetch", "--quiet", "origin", "main", claim.IssueBranch(key))
	git.With("JJ_USER="+setupUser, "JJ_EMAIL="+setupUser+"@example.test").Run(r.clone, append([]string{jj}, fixturegit.JJInit(r.clone, true)[1:]...)...)
	r.jjIn(r.clone, "workspace", "add", r.workspace, "--name", filepath.Base(r.workspace), "--revision", claim.IssueBranch(key)+"@origin")
	r.head = r.commitOf(r.workspace, "@-")
	r.startShim()
	return r
}

// shell runs command with sh in dir as an agent's shell runs a command a text gives it: the rig's
// workspace in LEGION_WORKSPACE, the real jj first on PATH, and the test's own identity. It answers
// what the command printed and its exit status.
func (r *copyRig) shell(dir, command string) (string, int) {
	r.t.Helper()
	run := exec.Command("sh", "-c", command)
	run.Dir = dir
	run.Env = append(fixtureenv.Isolated(r.home), "JJ_USER="+setupUser, "JJ_EMAIL="+setupUser+"@example.test",
		"LEGION_WORKSPACE="+r.workspace, "PATH="+filepath.Dir(r.jj)+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := run.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return strings.TrimSpace(string(output)), exit.ExitCode()
	}
	if err != nil {
		r.t.Fatalf("sh -c %s in %s: %v", command, dir, err)
	}
	return strings.TrimSpace(string(output)), 0
}

// filesOf is every file of rev's tree in dir's repository.
func (r *copyRig) filesOf(dir, rev string) []string {
	r.t.Helper()
	return strings.Fields(r.jjIn(dir, "file", "list", "-r", rev))
}

// try runs the real jj in dir on dir's own repository, as the test's own identity, and answers
// what it printed and how it ended. It runs in dir, since jj reads the repository of the
// directory it runs in whatever -R names.
func (r *copyRig) try(dir string, args ...string) (string, error) {
	r.t.Helper()
	command := exec.Command(r.jj, append(args, "-R", dir)...)
	command.Dir = dir
	command.Env = append(fixtureenv.Isolated(r.home), "JJ_USER="+setupUser, "JJ_EMAIL="+setupUser+"@example.test")
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// jjIn is try that fails the test when jj does.
func (r *copyRig) jjIn(dir string, args ...string) string {
	r.t.Helper()
	output, err := r.try(dir, args...)
	if err != nil {
		r.t.Fatalf("jj %v in %s: %v\n%s", args, dir, err, output)
	}
	return output
}

// commitOf is the commit id of rev in dir's working copies' repository.
func (r *copyRig) commitOf(dir, rev string) string {
	r.t.Helper()
	return r.jjIn(dir, "log", "-r", rev, "--no-graph", "-T", "commit_id")
}

// authorOf is the author's name of rev and its description, as one string.
func (r *copyRig) authorOf(dir, rev string) string {
	r.t.Helper()
	return r.jjIn(dir, "log", "-r", rev, "--no-graph", "-T", `author.name() ++ "|" ++ description`)
}

func (r *copyRig) write(dir, name, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// writeExecutable writes body to a new executable file at path. It holds syscall.ForkLock for
// reading while the file is open for writing, as the leak guard's writeCopyFile does: a child that
// another goroutine forks in that span holds the write descriptor until its own exec closes it,
// and the kernel refuses to exec a file open for writing ("text file busy").
func writeExecutable(path string, body []byte) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o700)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// commit writes name in dir's working copy and commits it there, as a role's step does, and
// answers the commit.
func (r *copyRig) commit(dir, name, message string) string {
	r.t.Helper()
	r.write(dir, name, message+"\n")
	r.jjIn(dir, "commit", "-m", message)
	return r.commitOf(dir, "@-")
}

// adopt sends the daemon's adoption with frame's fixFrom and moveTo and answers the shim's reply.
func (r *copyRig) adopt(id string, frame shimwire.AdoptWorkingCopy) shimwire.AdoptWorkingCopyResult {
	r.t.Helper()
	frame.ID, frame.JJUser, frame.JJEmail, frame.TimeoutMs = id, adoptingUser, adoptingEmail, 30000
	r.p.send(r.t, frame)
	got, ok := r.p.next(r.t).(shimwire.AdoptWorkingCopyResult)
	if !ok || got.ID != id {
		r.t.Fatalf("the shim answered %#v, want the result of %s", got, id)
	}
	return got
}

// commands is every jj command the shim has run so far, by its arguments.
func (r *copyRig) commands() []string {
	r.t.Helper()
	body, err := os.ReadFile(r.record)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// verbs is what each command did: its subcommand, with the workspace subcommand's own.
func verbs(commands []string) []string {
	var done []string
	for _, command := range commands {
		words := strings.Fields(command)
		verb := words[0]
		if verb == "workspace" && len(words) > 1 {
			verb += " " + words[1]
		}
		done = append(done, verb)
	}
	return done
}

// The adoption that precedes a fix's task makes the fix's working copy, beside the issue's
// workspace, on a fresh commit over the head the fix starts from, authored by the role the daemon
// names, and answers its directory. The issue's workspace is not adopted: its working copy stays
// the commit it was, authored as it was, for the step that runs beside the fix.
func TestAnAdoptionMakesTheFixCopyAtItsHead(t *testing.T) {
	r := newCopyRig(t)
	shared := `commit_id ++ " " ++ change_id ++ " " ++ author.name()`
	before := r.jjIn(r.workspace, "log", "-r", "@", "--no-graph", "-T", shared)

	got := r.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: r.head})
	if !got.OK || got.FixDir != r.fixDir || got.MovedTo != "" {
		t.Fatalf("the shim answered %#v, want ok with fixDir %s and no movedTo", got, r.fixDir)
	}
	if parent := r.commitOf(r.fixDir, "@-"); parent != r.head {
		t.Errorf("the fix's working copy stands on %s, want the head it starts from, %s", parent, r.head)
	}
	if author := r.authorOf(r.fixDir, "@"); author != adoptingUser+"|" {
		t.Errorf("the fix's working copy is %q, want an undescribed commit authored by %s", author, adoptingUser)
	}
	if after := r.jjIn(r.workspace, "log", "-r", "@", "--no-graph", "-T", shared); after != before {
		t.Errorf("the issue's workspace's working copy went from %q to %q, want it unchanged beside the fix", before, after)
	}
	add := "workspace add --name " + filepath.Base(r.fixDir) + " -r " + r.head + " " + r.fixDir + " -R " + r.workspace
	if ran := r.commands(); len(ran) != 2 || ran[0] != add || !slices.Equal(verbs(ran), []string{"workspace add", "metaedit"}) || !strings.HasSuffix(ran[1], "-R "+r.fixDir) {
		t.Errorf("the adoption ran jj\n%s\nwant the fix's workspace added (%s), then the adoption of that workspace alone", strings.Join(ran, "\n"), add)
	}
}

// A later fix of the same task finds the fix's working copy there, holding nothing over the head
// the fix starts from, and moves it onto that head, a fresh commit authored by the role, and adds no
// second workspace. The one read before the move is what tells it from the running fix's own copy
// (TestAnAdoptionOfTheRunningFixKeepsItsCopy).
func TestASecondFixMovesTheCopy(t *testing.T) {
	r := newCopyRig(t)
	if got := r.adopt("first", shimwire.AdoptWorkingCopy{FixFrom: r.head}); !got.OK || got.FixDir != r.fixDir {
		t.Fatalf("the first fix's adoption answered %#v, want ok with fixDir %s", got, r.fixDir)
	}
	pushed := r.commit(r.fixDir, "fix.txt", "fix: the first failure")
	before := len(r.commands())

	got := r.adopt("second", shimwire.AdoptWorkingCopy{FixFrom: pushed})
	if !got.OK || got.FixDir != r.fixDir {
		t.Fatalf("the second fix's adoption answered %#v, want ok with fixDir %s", got, r.fixDir)
	}
	if parent := r.commitOf(r.fixDir, "@-"); parent != pushed {
		t.Errorf("the fix's working copy stands on %s, want the second fix's head, %s", parent, pushed)
	}
	if author := r.authorOf(r.fixDir, "@"); author != adoptingUser+"|" {
		t.Errorf("the fix's working copy is %q, want an undescribed commit authored by %s", author, adoptingUser)
	}
	ran := r.commands()[before:]
	if want := []string{"workspace update-stale", "log", "new", "metaedit"}; !slices.Equal(verbs(ran), want) {
		t.Errorf("the second fix's adoption ran jj\n%s\nwant %v in the fix's working copy and no second workspace add", strings.Join(ran, "\n"), want)
	}
	for _, command := range ran {
		if !strings.HasSuffix(command, "-R "+r.fixDir) {
			t.Errorf("the second fix's adoption ran %q, want each command on the fix's working copy", command)
		}
	}
}

// The adoption before a fix's task runs again for the same fix whenever that task is sent again —
// after a refused prompt, a daemon's restart, the boot's resume, a pod release whose next pod brings
// the fix's working copy back — and it names the head the fix started from. By then the fix's
// working copy holds the fix's own work over that head: a commit, and an edit no jj command has
// recorded yet. The adoption keeps the copy where it stands and adopts it there, so the fix's
// commit is still under its working copy and both files are still in it. The control is
// TestASecondFixMovesTheCopy: a copy that holds nothing over the head a fix names is moved there.
func TestAnAdoptionOfTheRunningFixKeepsItsCopy(t *testing.T) {
	r := newCopyRig(t)
	if got := r.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: r.head}); !got.OK || got.FixDir != r.fixDir {
		t.Fatalf("the fix's adoption answered %#v, want ok with fixDir %s", got, r.fixDir)
	}
	fixed := r.commit(r.fixDir, "fix.txt", "fix: the failure")
	r.write(r.fixDir, "wip.txt", "not committed yet\n")

	got := r.adopt("again", shimwire.AdoptWorkingCopy{FixFrom: r.head})
	if !got.OK || got.FixDir != r.fixDir {
		t.Fatalf("the fix's adoption again answered %#v, want ok with fixDir %s", got, r.fixDir)
	}
	if parent := r.commitOf(r.fixDir, "@-"); parent != fixed {
		t.Errorf("the fix's working copy stands on %s after its adoption again, want the fix's own commit, %s", parent, fixed)
	}
	for name, body := range map[string]string{"fix.txt": "fix: the failure\n", "wip.txt": "not committed yet\n"} {
		if read, err := os.ReadFile(filepath.Join(r.fixDir, name)); err != nil || string(read) != body {
			t.Errorf("the fix's working copy holds %s as %q (%v) after its adoption again, want %q", name, read, err, body)
		}
	}
}

// An adoption naming the commit the task's working copy moves to starts the step on a fresh commit
// over it, in the issue's workspace, then adopts that workspace as an adoption with no move does,
// and answers the commit.
func TestAnAdoptionMovesTheTaskCopyToTheHead(t *testing.T) {
	r := newCopyRig(t)
	// Another working copy of the clone pushed a commit over the task's head.
	r.jjIn(r.clone, "new", r.head, "-m", "fix: pushed from another working copy")
	moved := r.commitOf(r.clone, "@")

	got := r.adopt("move", shimwire.AdoptWorkingCopy{MoveTo: moved})
	if !got.OK || got.MovedTo != moved || got.FixDir != "" {
		t.Fatalf("the shim answered %#v, want ok with movedTo %s and no fixDir", got, moved)
	}
	if parent := r.commitOf(r.workspace, "@-"); parent != moved {
		t.Errorf("the task's working copy stands on %s, want the commit it moved to, %s", parent, moved)
	}
	if author := r.authorOf(r.workspace, "@"); author != adoptingUser+"|" {
		t.Errorf("the task's working copy is %q, want an undescribed commit authored by %s", author, adoptingUser)
	}
	if ran := r.commands(); !slices.Equal(verbs(ran), []string{"log", "new", "log", "metaedit"}) {
		t.Errorf("the adoption ran jj\n%s\nwant the read of @-, the move, then the adoption as without one", strings.Join(ran, "\n"))
	}
}

// A move to the commit the task's working copy already stands on reads it and moves nothing: the
// one read, then the adoption as without a move, and no `jj new`.
func TestAMoveToWhereItStandsRunsNoJJNew(t *testing.T) {
	r := newCopyRig(t)
	got := r.adopt("stay", shimwire.AdoptWorkingCopy{MoveTo: r.head})
	if !got.OK || got.MovedTo != r.head {
		t.Fatalf("the shim answered %#v, want ok with movedTo %s", got, r.head)
	}
	if parent := r.commitOf(r.workspace, "@-"); parent != r.head {
		t.Errorf("the task's working copy stands on %s, want %s, where it stood", parent, r.head)
	}
	ran := r.commands()
	if want := []string{"log", "log", "metaedit"}; !slices.Equal(verbs(ran), want) {
		t.Errorf("the adoption ran jj\n%s\nwant %v: one read of @-, no jj new", strings.Join(ran, "\n"), want)
	}
	if len(ran) > 0 && ran[0] != "log -r @- --no-graph -T commit_id -R "+r.workspace {
		t.Errorf("the adoption's first command is %q, want the read of the working copy's parent", ran[0])
	}
}

// Every workspace of the clone shares its store, so a commit a step makes in the issue's
// workspace while the fix runs is there in the fix's working copy by its id, for the fix to take.
func TestACommitOfTheSharedCopyIsSeenInTheFixCopy(t *testing.T) {
	r := newCopyRig(t)
	if got := r.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: r.head}); !got.OK || got.FixDir != r.fixDir {
		t.Fatalf("the fix's adoption answered %#v, want ok with fixDir %s", got, r.fixDir)
	}
	red := r.commit(r.workspace, "red_test.txt", "test: the red test")
	if got := r.jjIn(r.fixDir, "log", "-r", red, "--no-graph", "-T", "commit_id ++ \" \" ++ description.first_line()"); got != red+" test: the red test" {
		t.Errorf("the fix's working copy reads %s as %q, want the shared copy's commit", red, got)
	}
}

// The fix takes a commit made under the issue's working copy by copying it under its own working
// copy (`jj duplicate <commit> --insert-before @`, which the fix's task names), so the copy is the
// fix copy's `@-` and nothing of the issue's workspace is rewritten: its working copy stays fresh,
// and the next adoption of it, moving it to the fix's head, succeeds.
func TestADuplicatedCommitLeavesTheSharedCopyFresh(t *testing.T) {
	r := newCopyRig(t)
	if got := r.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: r.head}); !got.OK || got.FixDir != r.fixDir {
		t.Fatalf("the fix's adoption answered %#v, want ok with fixDir %s", got, r.fixDir)
	}
	red := r.commit(r.workspace, "red_test.txt", "test: the red test")
	fixed := r.commit(r.fixDir, "fix.txt", "fix: the failure")
	r.jjIn(r.fixDir, "duplicate", red, "--insert-before", "@")
	copied := r.commitOf(r.fixDir, "@-")
	if got := r.jjIn(r.fixDir, "log", "-r", copied, "--no-graph", "-T", `description.first_line() ++ " " ++ parents.map(|p| p.commit_id()).join(",")`); got != "test: the red test "+fixed {
		t.Fatalf("the fix copy's @- after the duplicate is %q, want the copy of the red test over the fix's commit %s", got, fixed)
	}
	if status, err := r.try(r.workspace, "status"); err != nil || strings.Contains(status, "stale") {
		t.Fatalf("jj status in the issue's workspace after the copy = %v:\n%s\nwant a fresh working copy", err, status)
	}

	got := r.adopt("move", shimwire.AdoptWorkingCopy{MoveTo: copied})
	if !got.OK || got.MovedTo != copied {
		t.Fatalf("the next adoption of the issue's workspace answered %#v, want ok with movedTo %s", got, copied)
	}
	if parent := r.commitOf(r.workspace, "@-"); parent != copied {
		t.Errorf("the task's working copy stands on %s, want the fix's head, %s", parent, copied)
	}
}

// A fix that rewrites a commit under the issue's working copy from its own (`jj rebase -r`) leaves
// that working copy stale, and jj refuses every command there until `jj workspace update-stale`
// runs. So where the fix's working copy exists, an adoption of the issue's workspace brings it up
// to date first, and the next move succeeds. The control takes the update-stale out: the
// adoption's first read, run alone, fails on the stale working copy.
func TestARewriteFromTheFixCopyIsBroughtUpToDate(t *testing.T) {
	rewritten := func(t *testing.T) (*copyRig, string) {
		t.Helper()
		r := newCopyRig(t)
		if got := r.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: r.head}); !got.OK || got.FixDir != r.fixDir {
			t.Fatalf("the fix's adoption answered %#v, want ok with fixDir %s", got, r.fixDir)
		}
		red := r.commit(r.workspace, "red_test.txt", "test: the red test")
		fixed := r.commit(r.fixDir, "fix.txt", "fix: the failure")
		r.jjIn(r.fixDir, "rebase", "-r", red, "--onto", fixed)
		return r, r.commitOf(r.fixDir, "children("+fixed+") ~ @")
	}
	t.Run("the next adoption brings the working copy up to date and moves it", func(t *testing.T) {
		r, moved := rewritten(t)
		before := len(r.commands())
		got := r.adopt("move", shimwire.AdoptWorkingCopy{MoveTo: moved})
		if !got.OK || got.MovedTo != moved {
			t.Fatalf("the next adoption of the issue's workspace answered %#v, want ok with movedTo %s", got, moved)
		}
		if parent := r.commitOf(r.workspace, "@-"); parent != moved {
			t.Errorf("the task's working copy stands on %s, want the rewritten commit, %s", parent, moved)
		}
		ran := r.commands()[before:]
		if len(ran) == 0 || ran[0] != "workspace update-stale -R "+r.workspace {
			t.Errorf("the adoption ran jj\n%s\nwant the update-stale of the issue's workspace first", strings.Join(ran, "\n"))
		}
	})
	t.Run("without the update-stale, jj refuses the stale working copy", func(t *testing.T) {
		r, _ := rewritten(t)
		output, err := r.try(r.workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
		if err == nil || !strings.Contains(output, "stale") {
			t.Fatalf("the adoption's first read with no update-stale ended %v:\n%s\nwant jj's refusal of a stale working copy", err, output)
		}
	})
}

// An update round replaces the pod and saves nothing of its disk, so the work of a fix and of a step
// beside it reaches the next pod only through the branches each pushed at the round's ask. Two
// clones of one origin stand for the pod before the round and the pod after it. In the first, the
// fix's copy is made at the task's head X, since origin holds no branch of the fix yet; the fix
// commits fix.txt and pushes its own branch F with the commands its update text gives it, and the
// tester commits red_test.txt in the shared copy and pushes its branch B with BesidePushCommand.
// The first clone is removed. The second holds origin's default branch and the issue's branch and
// nothing of F or B, as a node provisions it; the adoption with FixFrom X and FixBranch F reads F on
// origin, fetches it, and makes the fix's copy on F's head, which holds fix.txt. The fix's take
// (BesideTakeCommands) copies the tester's commit under its working copy, and the tester's fetch of
// B in the second clone finds its commit. Each assertion reads files, never commit ids, since a copy
// has an id of its own. Each command the texts give is run as an agent's shell runs it, and its
// exit status on this jj is held to 0: a push of a new branch, a fetch of a branch origin does not
// hold, and a copy of no commit among them. The controls, each on a pod of its own: the adoption
// with no FixBranch makes the copy at X with no fix.txt, and the take with its fetch left out copies
// no commit of B, so the test fails for the cause it guards.
func TestAFixInAFreshPodTakesItsCommitsFromItsBranch(t *testing.T) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("jj is required: %v", err)
	}
	const key = "ACME-42"
	named := func(name string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return name
	}
	fixBranch := named(claim.BesideBranch(key, 1, "a", 1, claim.RoleImplementer))
	testerBranch := named(claim.BesideBranch(key, 1, "a", 1, claim.RoleTester))
	prefix := named(claim.BesideFixPrefix(key, 1, "a", 1))
	taskBranch := claim.IssueBranch(key)
	origin := newOrigin(t, key)
	// exits0 runs command in dir of the pod r and holds its exit status to 0, logging what it printed.
	exits0 := func(t *testing.T, r *copyRig, dir, command string) {
		t.Helper()
		output, status := r.shell(dir, command)
		t.Logf("exit %d: %s\n%s", status, command, output)
		if status != 0 {
			t.Fatalf("%s exited %d on this jj, want 0:\n%s", command, status, output)
		}
	}
	has := func(files []string, name string) bool { return slices.Contains(files, name) }

	// The pod before the update round.
	first := newPodRig(t, jj, origin, key)
	head := first.head
	got := first.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: head, FixBranch: fixBranch})
	if !got.OK || got.FixDir != first.fixDir {
		t.Fatalf("the first pod's adoption answered %#v, want ok with fixDir %s", got, first.fixDir)
	}
	if parent := first.commitOf(first.fixDir, "@-"); parent != head {
		t.Fatalf("with no branch of the fix on origin, the fix's copy stands on %s, want the head %s", parent, head)
	}
	if ran := first.commands(); slices.ContainsFunc(ran, func(command string) bool { return strings.HasPrefix(command, "git fetch") }) {
		t.Errorf("the first pod's adoption ran jj\n%s\nwant no fetch of a branch origin does not hold", strings.Join(ran, "\n"))
	}
	first.commit(first.fixDir, "fix.txt", "fix: the failure")
	exits0(t, first, first.fixDir, `jj -R "$LEGION_WORKSPACE-fix" bookmark set `+fixBranch+` -r @-`)
	exits0(t, first, first.fixDir, `jj -R "$LEGION_WORKSPACE-fix" git push --remote origin --bookmark `+fixBranch)
	first.commit(first.workspace, "red_test.txt", "test: the red test")
	exits0(t, first, first.workspace, `jj -R "$LEGION_WORKSPACE" git fetch --remote origin --branch `+testerBranch)
	exits0(t, first, first.workspace, workspace.BesidePushCommand(testerBranch))
	git := fixturegit.Isolated(t, t.TempDir())
	for _, branch := range []string{fixBranch, testerBranch} {
		if _, err := git.Try(origin, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
			t.Fatalf("origin holds no %s after its push: %v", branch, err)
		}
	}
	for _, dir := range []string{first.fixDir, first.workspace, first.clone} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the fix's copy in a fresh pod stands on its pushed work and takes the tester's", func(t *testing.T) {
		second := newPodRig(t, jj, origin, key)
		if second.head != head {
			t.Fatalf("the second pod's workspace stands on %s, want the issue's branch's head %s", second.head, head)
		}
		if held := second.jjIn(second.workspace, "log", "-r", `remote_bookmarks(glob:"legion-beside/*")`, "--no-graph", "-T", `commit_id ++ "\n"`); held != "" {
			t.Fatalf("the fresh pod's clone already holds a branch beside the fix:\n%s", held)
		}
		got := second.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: head, FixBranch: fixBranch})
		if !got.OK || got.FixDir != second.fixDir {
			t.Fatalf("the fresh pod's adoption answered %#v, want ok with fixDir %s", got, second.fixDir)
		}
		if files := second.filesOf(second.fixDir, "@-"); !has(files, "fix.txt") {
			t.Fatalf("the fix's copy in the fresh pod stands on a commit with %v, want the fix's own fix.txt", files)
		}
		fetch := "git fetch --remote origin --branch " + fixBranch + " -R " + second.workspace
		add := "workspace add --name " + filepath.Base(second.fixDir) + " -r " + fixBranch + "@origin " + second.fixDir + " -R " + second.workspace
		if ran := second.commands(); len(ran) != 3 || ran[0] != fetch || ran[1] != add || !strings.HasPrefix(ran[2], "metaedit ") {
			t.Errorf("the fresh pod's adoption ran jj\n%s\nwant %s, then %s, then the adoption of the fix's copy", strings.Join(ran, "\n"), fetch, add)
		}
		for _, command := range workspace.BesideTakeCommands(second.fixDir, prefix, fixBranch, taskBranch) {
			exits0(t, second, second.fixDir, command)
		}
		if files := second.filesOf(second.fixDir, "@-"); !has(files, "red_test.txt") || !has(files, "fix.txt") {
			t.Errorf("after the take the commit under the fix's working copy holds %v, want the tester's red_test.txt over the fix's fix.txt", files)
		}
		exits0(t, second, second.workspace, `jj -R "$LEGION_WORKSPACE" git fetch --remote origin --branch `+testerBranch)
		if body := second.jjIn(second.workspace, "file", "show", "-r", testerBranch+"@origin", "red_test.txt"); body != "test: the red test" {
			t.Errorf("the tester's branch in the fresh pod holds red_test.txt as %q, want its red test", body)
		}
	})
	t.Run("control: with no fix branch the copy stands at the head, without the fix's work", func(t *testing.T) {
		third := newPodRig(t, jj, origin, key)
		got := third.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: head})
		if !got.OK || got.FixDir != third.fixDir {
			t.Fatalf("the adoption answered %#v, want ok with fixDir %s", got, third.fixDir)
		}
		if parent := third.commitOf(third.fixDir, "@-"); parent != head {
			t.Errorf("the fix's copy stands on %s, want the head %s", parent, head)
		}
		if files := third.filesOf(third.fixDir, "@-"); has(files, "fix.txt") {
			t.Errorf("the fix's copy made with no fix branch holds %v, want no fix.txt", files)
		}
	})
	t.Run("control: the take with its fetch left out copies no commit of the tester's branch", func(t *testing.T) {
		fourth := newPodRig(t, jj, origin, key)
		if got := fourth.adopt("fix", shimwire.AdoptWorkingCopy{FixFrom: head, FixBranch: fixBranch}); !got.OK || got.FixDir != fourth.fixDir {
			t.Fatalf("the adoption answered %#v, want ok with fixDir %s", got, fourth.fixDir)
		}
		take := workspace.BesideTakeCommands(fourth.fixDir, prefix, fixBranch, taskBranch)
		exits0(t, fourth, fourth.fixDir, take[len(take)-1])
		if files := fourth.filesOf(fourth.fixDir, "@-"); has(files, "red_test.txt") || !has(files, "fix.txt") {
			t.Errorf("the take with no fetch leaves the fix's copy on a commit with %v, want fix.txt and no red_test.txt", files)
		}
	})
}
