package leakguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
)

// The exit statuses the shared public leak check documents. Anything else is a setup failure: a
// status this package does not know is a check it does not understand, and a write it has not
// cleared.
//
// The first three are exported because they are the vocabulary a command that stands in for the
// check exits with as well — `legion leak-check`, which a pre-push hook runs — and a second copy
// of the three numbers is one an edit here would silently part from. StatusWantsStdin stays
// unexported: it is a step of the gh shape's own handshake (Guard.askAgain), never a status this
// package answers a caller with. `legion leak-check` also exits StatusSetupFailure for a
// destination GitHub answers 404 for (a *NotFound), a write it has not cleared either.
const (
	StatusClean        = 0 // the write is clean
	StatusSetupFailure = 2 // a check could not run or finish, or a 404 for the destination
	StatusRefused      = 3 // the write names something private
	checkWantsStdin    = 4 // the gh call reads standard input; buffer it and ask again
)

// whyNoCheck is the setup failure of a call that found no shared check at all.
const whyNoCheck = "the shared public leak check could not be found"

// whyOtherCheck is the setup failure of a call whose check is found and is not the deployment's
// (CheckerVar): its manifest digest (Manifest) is another one, or cannot be read.
const whyOtherCheck = "the shared public leak check found is not the one the deployment names"

// CheckVar is the environment variable that names the shared check, the first place Resolve
// looks. It is exported because the daemon tells a pane the check it resolved under this name
// (runtime/tmux's panePairs), the Kubernetes runtime tells every role's container the copy the
// pod's launcher installed under it (runtime/sandbox's mainEnvironment, which owns the name
// against an operator's own value), the shim names that install under it for everything it
// starts where the environment names none (internal/shim's spawnOnce, through Installed), and a
// second copy of the string in any of them is one an edit here would silently part from.
const CheckVar = "PUBLIC_LEAK_CHECK"

// checkName is the shared check's file name, which is how it is found on PATH.
const checkName = "public-leak-check.sh"

// pointerFile is the path of the file that names the check, under the configuration home: the
// last place Resolve looks.
const pointerFile = "public-leak-check/path"

// Resolve is the shared public leak check's path, found the way the check itself says to find
// it: CheckVar, then public-leak-check.sh on PATH, then the pointer file under the
// configuration home, whose content is the path of the script. The guard's own Check option
// stands in front of all three, and is how a test hands the guard a stand-in. Where none of the
// three names a check at all — CheckVar unset, nothing of the check's name on any PATH entry, and
// no pointer file — the error is a noneNamed, which sharedCheck reads apart from a broken setup
// (brokenCheck): a check that is named and is no executable file, a file of the check's name on
// PATH that is no executable, and a pointer file that cannot be read or names nothing.
//
// A file of the check's name on PATH that does not run hides nothing behind it: the pointer file
// is still read, and a check it names is the answer. For a dangling link or a directory there,
// that is the check's own pre-push hook's order too, since its `command -v` finds nothing. For a
// file with no execute bit, the hook's `command -v` finds it and the hook refuses, where the guard
// runs the pointer's check: each either runs a check or refuses. The unusable PATH file is the
// answer only where nothing else names a check.
//
// Every call resolves again, and no answer is kept. The check is published often, and a guard
// that resolved one copy at start would keep running it: a text the current check refuses would
// pass, which is the one failure this package exists to prevent.
func (g *Guard) Resolve() (string, error) {
	if g.opts.Check != "" {
		return found(g.opts.Check, "the guard's own Check option")
	}
	env := g.base()
	if named := value(env, CheckVar); named != "" {
		return found(named, CheckVar)
	}
	searchPath := value(env, "PATH")
	if onPath, ok := lookpath.On(searchPath, checkName); ok {
		return onPath, nil
	}
	nothingElse := func(none error) error {
		if there := unusableOnPath(searchPath); there != "" {
			return brokenCheck{path: there, error: fmt.Errorf("%s names none, %s on PATH is no executable file, and no pointer file names a check", CheckVar, there)}
		}
		return noneNamed{none}
	}
	config := value(env, "XDG_CONFIG_HOME")
	if config == "" {
		if home := value(env, "HOME"); home != "" {
			config = filepath.Join(home, ".config")
		}
	}
	if config == "" {
		return "", nothingElse(errors.New(CheckVar + " names none, " + checkName + " is on no PATH entry, and there is no configuration home to read a pointer file from"))
	}
	pointer := filepath.Join(config, pointerFile)
	content, err := os.ReadFile(pointer)
	if err != nil {
		err = fmt.Errorf("%s names none, %s is on no PATH entry, and the pointer file %s could not be read: %w", CheckVar, checkName, pointer, err)
		if _, statErr := os.Lstat(pointer); errors.Is(statErr, fs.ErrNotExist) {
			return "", nothingElse(err)
		}
		return "", brokenCheck{path: pointer, error: err}
	}
	named := strings.TrimSpace(string(content))
	if named == "" {
		return "", brokenCheck{path: pointer, error: fmt.Errorf("the pointer file %s names no check", pointer)}
	}
	return found(named, "the pointer file "+pointer)
}

// unusableOnPath is the first file of the check's name on an entry of searchPath that lookpath.On
// passed over — a dangling link, a file with no execute bit, a directory — or "" where no entry
// holds anything of that name. lookpath.On reads the entries it searches, absolute and non-empty,
// so this reads the same ones. Such a file is somebody's install of the check that does not run,
// which is a broken setup and not a check named nowhere.
func unusableOnPath(searchPath string) string {
	for _, dir := range filepath.SplitList(searchPath) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, checkName)
		if _, err := os.Lstat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// noneNamed is Resolve's answer where no place it looks names a check at all, as against a broken
// setup (brokenCheck), which is broken whatever the deployment runs.
type noneNamed struct{ error }

func (n noneNamed) Unwrap() error { return n.error }

// NoneNamed is whether err, an answer of Resolve, is that no place names a check at all: the one
// answer under which a deployment that names no checker runs a write with no shared check
// (sharedCheck). The tmux runtime reads it to say nothing at a launch where that is so.
func NoneNamed(err error) bool {
	var none noneNamed
	return errors.As(err, &none)
}

// brokenCheck is Resolve's answer where something names or holds a check that does not run: the
// path a variable, an option or a pointer file names is not there or is no executable file
// (found), a file of the check's name on PATH is no executable and nothing else names a check
// (unusableOnPath), or the pointer file cannot be read or names nothing. path is that path, or the
// pointer file's. Each refuses every write whatever the deployment runs.
type brokenCheck struct {
	path string
	error
}

func (b brokenCheck) Unwrap() error { return b.error }

// BrokenAt is the path err, an answer of Resolve, names where the setup is broken (brokenCheck).
// The tmux runtime tells a pane that path as CheckVar, so the pane's guard refuses its writes as
// the daemon's own guard would, though the pane cannot read the place that named it: the path is
// not there, or it is no executable file (found), and either refuses every write, a push included.
func BrokenAt(err error) (string, bool) {
	var broken brokenCheck
	if errors.As(err, &broken) {
		return broken.path, true
	}
	return "", false
}

// sharedRun is the shared check one write runs, as sharedCheck answers it. path is what the write's
// shape runs: the launcher of the private copy under a deployment's manifest digest, the check
// Resolve found under none, and "" where no shared check runs. program is the
// public_leak_check.py the digest covered, nil under no digest. dir is the private copy's
// directory, "" where there is none.
type sharedRun struct {
	path    string
	program []byte
	dir     string
}

// remove removes the private copy, where there is one. Every entry point defers it as soon as
// sharedCheck answers, so the copy goes whatever the check answered: cleared, refused, a setup
// failure, or a run the caller's bound stopped. A copy a killed process leaves is in the role's
// own private directory, which the role's launcher clears before it starts the role's next
// generation (internal/launcher's writeFiles). The role can write that directory, so removal
// follows no link and waits on no fifo (rolefile.RemoveTree).
func (r sharedRun) remove() {
	if r.dir != "" {
		_ = rolefile.RemoveTree(r.dir)
	}
}

// sharedCheck is the shared check a write of what runs (Resolve), or a sharedRun with no path and
// no error where none is named and the environment names no deployment checker (CheckerVar): a
// deployment that runs no shared leak check of its own (Setting.Configured) refuses no write for
// the check's absence, and the write gets the rest of the guard, the visibility read and the
// private-name pass. Under a deployment that names one, and wherever a check is named but cannot
// be resolved, the answer is the setup failure whyNoCheck, which refuses the write.
//
// Under a deployment's manifest digest the check found must be that check too: the manifest digest
// of its four files (readHeldCheck: the launcher script, the program, the must-pass lines and the
// README beside it) must be CheckerVar, or the answer is the setup failure whyOtherCheck, naming
// both digests. The write then runs those very bytes, written into a copy only this role can reach
// (privateCopy), and never the directory they were read from: a pod's install is on the issue's
// scratch, which every role of the pod can write, since they all run as one user, so a file
// replaced there between the read and the run would otherwise be the check that judges the write.
// So a role runs every write under the one check its deployment named, whatever
// PUBLIC_LEAK_CHECK, PATH or the pointer file found, and a launcher script edited beside an
// unchanged program is another check.
//
// With no digest told there is nothing to hold the check to, and the write runs the check where
// Resolve found it, as it did before the deployment's setting existed: a tmux pane's check in its
// operator's own home, or a pod of a deployment that names none.
func (g *Guard) sharedCheck(what string) (sharedRun, error) {
	checker := value(g.base(), CheckerVar)
	check, err := g.Resolve()
	if err != nil {
		if NoneNamed(err) && checker == "" {
			return sharedRun{}, nil
		}
		return sharedRun{}, &SetupFailure{What: what, Why: whyNoCheck, Err: err}
	}
	if checker == "" {
		return sharedRun{path: check}, nil
	}
	held, err := readHeldCheck(check)
	switch {
	case err != nil:
		return sharedRun{}, &SetupFailure{What: what, Why: whyOtherCheck,
			Err: fmt.Errorf("%s=%s, and the manifest of the check at %s could not be read: %w", CheckerVar, checker, check, err)}
	case !strings.EqualFold(held.digest, checker):
		return sharedRun{}, &SetupFailure{What: what, Why: whyOtherCheck,
			Err: fmt.Errorf("%s=%s, and the check at %s has the manifest digest %s", CheckerVar, checker, check, held.digest)}
	}
	dir, err := held.privateCopy(PrivateCopyBase, privateCopyFallback)
	if err != nil {
		return sharedRun{}, &SetupFailure{What: what, Why: "the shared public leak check could not be copied where only this role can reach it",
			Err: fmt.Errorf("the check at %s, of the manifest digest %s=%s: %w", check, CheckerVar, checker, err)}
	}
	return sharedRun{path: filepath.Join(dir, checkName), program: held.files[checkProgram], dir: dir}, nil
}

// PrivateCopyBase is where a guard under a deployment's digest makes the copy of the check it runs
// (privateCopy): in an issue pod, a role's own memory-backed private directory, which
// `legion launcher role-init` binds back at this path inside that role's own mount namespace alone
// (internal/launcher's FixedRoot and RolePrivateDir), so no other role of the pod reaches it,
// though every role runs as one user; the controller's pod mounts its one role's there. It is
// spelled here because internal/launcher imports this package, and internal/launcher's layout test
// holds it to the layout's names.
const PrivateCopyBase = "/var/run/legion/private"

// privateCopyFallback is where the copy is made where PrivateCopyBase does not exist: /tmp itself,
// which role-init also gives each role of an issue pod as a directory of its own. Neither TMPDIR
// nor XDG_STATE_HOME is read for it: a pod tells its roles a TMPDIR and a state home on the issue's
// scratch (internal/runtime/sandbox's roleEnvironment), which every role writes, and which is where
// the install itself is. On a machine with neither directory of a pod — a tmux pane's — /tmp is
// shared, and the copy's 0700 directory keeps out every other user, though not another process of
// the same one.
const privateCopyFallback = "/tmp"

// copyBase is the directory the private copy is made under: base where a directory is there, and
// fallback only where nothing is at base at all. Anything else at base, or a base that cannot be
// read, is an error: a role whose private directory is broken makes no copy where other roles may
// reach it.
func copyBase(base, fallback string) (string, error) {
	info, err := os.Stat(base)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fallback, nil
	case err != nil:
		return "", err
	case !info.IsDir():
		return "", fmt.Errorf("%s is no directory (%s)", base, info.Mode())
	}
	return base, nil
}

// privateCopy writes the four files h holds into a new directory under copyBase's answer of base
// and fallback, and answers that directory. The directory is made 0700 (os.MkdirTemp), and each
// file is created anew (writeCopyFile): the launcher, which the guard runs, 0500, and the three it
// only reads 0400. A failure removes what it made as sharedRun.remove removes the copy, following
// no link and waiting on no fifo (rolefile.RemoveTree).
func (h heldCheck) privateCopy(base, fallback string) (string, error) {
	under, err := copyBase(base, fallback)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(under, "leak-check-")
	if err != nil {
		return "", err
	}
	for _, name := range ManifestFiles {
		mode := os.FileMode(0o400)
		if name == checkName {
			mode = 0o500
		}
		if err := writeCopyFile(filepath.Join(dir, name), h.files[name], mode); err != nil {
			_ = rolefile.RemoveTree(dir)
			return "", fmt.Errorf("%s: %w", name, err)
		}
	}
	return dir, nil
}

// writeCopyFile writes body to path as a new file of mode exactly: O_EXCL and O_NOFOLLOW, so
// nothing already at path, a link included, is opened or written through, and the mode set again
// on the open file, since the umask may have taken bits off the create's. syscall.ForkLock is held
// for reading while the file is open for writing, which keeps every fork of this process out of
// that span: a child forked then would hold the write descriptor until its own exec closed it, and
// the kernel refuses to exec a file open for writing (ETXTBSY), which the launcher is the moment
// this answers.
func writeCopyFile(path string, body []byte, mode os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// found is path, once it is absolute and an executable regular file is there: a name taken from a
// variable, an option or a pointer file is somebody's word for where the check is. A relative one
// names no one file: the guard would read it against its own working directory, a bare name is
// looked up on PATH when it is run, and a push runs it in the repository's directory, so the file
// the digest held need not be the file that runs. It is refused, as is a stale one, or one that
// names a directory or a file with no execute bit, each reported as the broken setup it is rather
// than as a child process that would not start; and every shape refuses it alike, a push included,
// which runs the check through bash and would otherwise run a file the others cannot.
func found(path, source string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", brokenCheck{path: path, error: fmt.Errorf("%s names %s, which is no absolute path", source, path)}
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return "", brokenCheck{path: path, error: fmt.Errorf("%s names %s, which is not there: %w", source, path, err)}
	case !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0:
		return "", brokenCheck{path: path, error: fmt.Errorf("%s names %s, which is no executable file (%s)", source, path, info.Mode())}
	}
	return path, nil
}

// promptsNothing is the part of a child's environment that makes a credential prompt fail closed.
// The shared check runs git against the destination remote, and a git that cannot find a
// credential asks for one: on a pod there is no terminal to ask, and the check would stall until
// its bound ran out instead of answering. GIT_TERMINAL_PROMPT=0 refuses git's own prompt, and the
// two askpass programs point at one that exits without an answer, which both git and ssh read as
// "no credential". SSH_ASKPASS_REQUIRE=force is what makes ssh use its askpass where it has a
// terminal of its own.
func promptsNothing(noAnswer string) []string {
	return []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=" + noAnswer,
		"SSH_ASKPASS=" + noAnswer,
		"SSH_ASKPASS_REQUIRE=force",
	}
}

// noAnswer is the askpass program a child is pointed at: `false`, resolved on the base
// environment's PATH, and /bin/false where that PATH holds none. Either way it is a program that
// answers no credential; a path that names nothing at all fails the child's credential read too,
// which is the same closed door.
func noAnswer(env []string) string {
	if path, ok := lookpath.On(value(env, "PATH"), "false"); ok {
		return path
	}
	return "/bin/false"
}

// child is one process this package runs: the shared check in one of its shapes, a read of a
// repository's visibility (gh's REST read; where that is refused for a rate limit, gh's GraphQL
// read and git's ls-remote; where gh failed on its connection, gh's token answer, curl's REST read
// and git's ls-remote; and where `legion gh` refuses it for an owner the project does not serve,
// git's ls-remote), or a git command a push's records are read with.
type child struct {
	// argv is the command and its arguments, argv[0] naming the program.
	argv []string
	// dir is the directory the child runs in, empty for this process's own.
	dir string
	// env is what the child's environment carries beyond the guard's own (Guard.env).
	env []string
	// inherit, where it is not nil, is which names of the base environment the child keeps;
	// the guard's own names and env go on top either way. Nil keeps the whole base, which is
	// what every child keeps but the visibility read made with no credential (Guard.lsRemote)
	// and the read through curl (Guard.CurlGet).
	inherit func(name string) bool
	// stdin is the child's standard input, nil for none.
	stdin io.Reader
	// files are the child's descriptors from 3 up, as exec.Cmd.ExtraFiles takes them.
	files []*os.File
	// stderr, where it is not nil, takes the child's standard error apart from the standard
	// output run answers, which the GraphQL visibility read needs to read gh's body alone
	// (Guard.graphQLVisibility), the read through curl to read curl's answer alone
	// (Guard.CurlGet), the token read to quote gh's error without its output
	// (Guard.ghToken), and a git read of a push's records to quote git's error without the push's
	// content (Guard.git). Nil answers the two together.
	stderr io.Writer
	// stdout, where it is not nil, reads the child's standard output as the child writes it,
	// from a pipe the child holds the other end of, and run answers none of it: a push's diff
	// and the blobs the record writer reads are read this way (Guard.gitRead), so that no copy
	// of the whole output is held. It reads to the end of the pipe; an error it answers closes
	// the pipe, which ends the child's writes, and is run's error.
	stdout func(io.Reader) error
}

// program is where name is, read from the base environment's PATH rather than from this
// process's: the environment a guard is given decides which bash, git and gh its children are,
// and exec resolves a bare name against the parent's own PATH instead. A name that is already a
// path, and one that PATH holds nothing of, is left as it is. The lookup is made for every
// child: a program that appeared on PATH since the last one is the program this child must run.
func program(base []string, name string) string {
	if strings.ContainsRune(name, filepath.Separator) {
		return name
	}
	if path, ok := lookpath.On(value(base, "PATH"), name); ok {
		return path
	}
	return name
}

// run runs c under ctx and answers its exit status and everything it printed, out and err
// together unless c.stderr takes err or c.stdout reads out. The error is a child that could not be
// run at all: one that did not start, one the end of ctx stopped, which is a *stoppedError, or one
// a signal ended, which has no status; and, after those, an error c.stdout answered. A status is
// an answer, and no status is not.
//
// c.stdout reads from an *os.File pipe, never through exec's own copying goroutine: exec waits
// for that goroutine at most procgroup.WaitDelay once the child has exited, and a reader still
// scanning the end of a diff past that wait would lose what it had not read yet and go on
// writing after run returned. With the pipe, the reader is this goroutine, and run waits for the
// child only once the reader has read to the end.
func (g *Guard) run(ctx context.Context, c child) (int, string, error) {
	// One read of the base environment for the child, which both the program lookup and the
	// child's own environment are built from.
	base := g.base()
	cmd := exec.CommandContext(ctx, program(base, c.argv[0]), c.argv[1:]...)
	cmd.Dir = c.dir
	cmd.Env = g.env(base, c.inherit, c.env...)
	cmd.Stdin = c.stdin
	cmd.ExtraFiles = c.files
	// procgroup before any other SysProcAttr field, as its doc requires: it replaces the whole
	// struct. It also makes the bound reach a helper the child started, which for the shared
	// check is the git it runs against the remote.
	killed := procgroup.Configure(cmd)
	var printed bytes.Buffer
	cmd.Stdout, cmd.Stderr = &printed, &printed
	if c.stderr != nil {
		cmd.Stderr = c.stderr
	}
	var out, in *os.File
	if c.stdout != nil {
		var err error
		if out, in, err = os.Pipe(); err != nil {
			return 0, "", err
		}
		defer out.Close()
		cmd.Stdout = in
	}
	started := time.Now()
	err := cmd.Start()
	if in != nil {
		// The child holds its own copy of the write end; this one closed, the reader sees the
		// end of the pipe once the child and anything it started have closed theirs.
		in.Close()
	}
	var readErr error
	if err == nil {
		if out != nil {
			readErr = c.stdout(out)
			// A reader that stopped early leaves the child writing into a pipe nobody reads:
			// closed, its next write fails and it ends, rather than blocking the wait below.
			out.Close()
		}
		err = cmd.Wait()
	}
	err = procgroup.Err(err)
	switch {
	case killed():
		stopped := &stoppedError{
			program:   filepath.Base(c.argv[0]),
			ran:       time.Since(started),
			cancelled: errors.Is(ctx.Err(), context.Canceled),
		}
		if deadline, ok := ctx.Deadline(); ok {
			stopped.bound = deadline.Sub(started)
		}
		return 0, printed.String(), stopped
	case readErr != nil:
		return 0, printed.String(), readErr
	case err != nil:
		return 0, printed.String(), err
	}
	return cmd.ProcessState.ExitCode(), printed.String(), nil
}

// stoppedError is a child that the end of the caller's context stopped before it exited: its
// bound ran out, or the caller cancelled it. It carries how long the child ran and how long its
// bound gave it, because "it did not finish" alone does not tell a person reading the line
// whether the child was given too little time or hung.
type stoppedError struct {
	// program is the program argv[0] names, without its directory: bash, gh or git.
	program string
	// ran is how long the child ran before the stop.
	ran time.Duration
	// bound is how long the context's deadline gave the child from its start, and zero where the
	// context has no deadline.
	bound time.Duration
	// cancelled is whether a cancel stopped the child rather than its deadline.
	cancelled bool
}

func (s *stoppedError) Error() string {
	return s.said(s.program)
}

// said is the stop as one sentence about name, in whole seconds: what ran, for how long, and the
// bound or the cancel that stopped it. It is the one place the wording lives, so the error of a
// child and the setup failure of the shared check say the same thing. A context with no deadline
// can end only by a cancel, so it is said as one.
func (s *stoppedError) said(name string) string {
	if s.cancelled || s.bound == 0 {
		return fmt.Sprintf("%s ran %d s and was stopped by a cancel", name, wholeSeconds(s.ran))
	}
	return fmt.Sprintf("%s ran %d s and was stopped by its bound of %d s", name, wholeSeconds(s.ran), wholeSeconds(s.bound))
}

// wholeSeconds is d rounded to the nearest second, as a count of seconds.
func wholeSeconds(d time.Duration) int64 {
	return int64(d.Round(time.Second) / time.Second)
}

// shared runs one shape of the shared check and reads its status: clean passes, 3 refuses with
// the check's own report, and anything else is a setup failure naming the status. It is the one
// place the check's statuses are read, so a shape that meets an undocumented one refuses the
// write rather than reading it as a pass.
func (g *Guard) shared(ctx context.Context, what string, c child) error {
	status, printed, err := g.run(ctx, c)
	if err != nil {
		return sharedNotRun(what, err, printed)
	}
	return readStatus(what, status, printed)
}

// sharedNotRun is the setup failure of a shape of the shared check that run answered no status
// for. A check the caller's context stopped is said in the failure's own sentence, which names
// the check, the time it ran and its bound, with nothing beneath it: the stop's own error would
// say the same again, with the program's file name for its subject. Any other error stays
// beneath the sentence that the check could not be run, with what the check printed before a
// signal ended it (procgroup.Said).
func sharedNotRun(what string, err error, printed string) error {
	var stopped *stoppedError
	if errors.As(err, &stopped) {
		return &SetupFailure{What: what, Why: stopped.said("the shared public leak check")}
	}
	return &SetupFailure{What: what, Why: "the shared public leak check could not be run", Err: procgroup.Said(err, printed)}
}

// readStatus is what a status of the shared check means, for a shape that has no further status
// of its own: nil for a clean one, a refusal for 3, and a setup failure for everything else.
func readStatus(what string, status int, printed string) error {
	switch status {
	case StatusClean:
		return nil
	case StatusRefused:
		return &Refusal{What: what, By: bySharedCheck, Lines: reportLines(printed)}
	case StatusSetupFailure:
		return &SetupFailure{What: what, Why: "the shared public leak check could not finish (exit 2)", Err: printedError(printed)}
	default:
		return &SetupFailure{
			What: what,
			Why:  fmt.Sprintf("the shared public leak check exited %d, which it does not document", status),
			Err:  printedError(printed),
		}
	}
}

// reportLines is what the shared check printed, as the lines of a refusal's report. The check's
// own account of what it refused is the only one there is — this package has not read the write
// itself at that point — so it is carried as the check wrote it, blank lines dropped.
func reportLines(printed string) []string {
	var lines []string
	for line := range strings.Lines(printed) {
		if trimmed := strings.TrimRight(line, "\r\n"); strings.TrimSpace(trimmed) != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

// printedError is what the check printed, as the error under a setup failure, and nil where it
// printed nothing.
func printedError(printed string) error {
	if trimmed := strings.TrimSpace(printed); trimmed != "" {
		return errors.New(trimmed)
	}
	return nil
}
