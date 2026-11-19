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
	"time"

	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// The exit statuses the shared public leak check documents. Anything else is a setup failure: a
// status this package does not know is a check it does not understand, and a write it has not
// cleared.
//
// The first three are exported because they are the vocabulary a command that stands in for the
// check exits with as well — `legion leak-check`, which a pre-push hook runs — and a second copy
// of the three numbers is one an edit here would silently part from. StatusWantsStdin stays
// unexported: it is a step of the gh shape's own handshake (Guard.askAgain), never a status this
// package answers a caller with.
const (
	StatusClean        = 0 // the write is clean
	StatusSetupFailure = 2 // the check could not be made to run, or could not finish
	StatusRefused      = 3 // the write names something private
	checkWantsStdin    = 4 // the gh call reads standard input; buffer it and ask again
)

// whyNoCheck is the setup failure of a call that found no shared check at all.
const whyNoCheck = "the shared public leak check could not be found"

// whyOtherCheck is the setup failure of a call whose check is found and is not the deployment's
// checker (CheckerVar): its program hashes to another sha256, or cannot be read.
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
// (brokenCheck): a check that is named and not there, a file of the check's name on PATH that is
// no executable, and a pointer file that cannot be read or names nothing.
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
	if there := unusableOnPath(searchPath); there != "" {
		return "", brokenCheck{path: there, error: fmt.Errorf("%s names none, and %s on PATH is no executable file", CheckVar, there)}
	}
	config := value(env, "XDG_CONFIG_HOME")
	if config == "" {
		if home := value(env, "HOME"); home != "" {
			config = filepath.Join(home, ".config")
		}
	}
	if config == "" {
		return "", noneNamed{errors.New(CheckVar + " names none, " + checkName + " is on no PATH entry, and there is no configuration home to read a pointer file from")}
	}
	pointer := filepath.Join(config, pointerFile)
	content, err := os.ReadFile(pointer)
	if err != nil {
		err = fmt.Errorf("%s names none, %s is on no PATH entry, and the pointer file %s could not be read: %w", CheckVar, checkName, pointer, err)
		if _, statErr := os.Lstat(pointer); errors.Is(statErr, fs.ErrNotExist) {
			return "", noneNamed{err}
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
// path a variable, an option or a pointer file names is not there (found), a file of the check's
// name on PATH is no executable (unusableOnPath), or the pointer file cannot be read or names
// nothing. path is that path, or the pointer file's. Each refuses every write whatever the
// deployment runs.
type brokenCheck struct {
	path string
	error
}

func (b brokenCheck) Unwrap() error { return b.error }

// BrokenAt is the path err, an answer of Resolve, names where the setup is broken (brokenCheck).
// The tmux runtime tells a pane that path as CheckVar, so the pane's guard refuses its writes as
// the daemon's own guard would, though the pane cannot read the place that named it: the path is
// not there, or it is no program the guard can run, and either refuses the write.
func BrokenAt(err error) (string, bool) {
	var broken brokenCheck
	if errors.As(err, &broken) {
		return broken.path, true
	}
	return "", false
}

// sharedCheck is the shared check a write of what runs (Resolve), or "" and no error where none is
// named and the environment names no deployment checker (CheckerVar): a deployment that runs no
// shared leak check of its own (Setting.Configured) refuses no write for the check's absence, and
// the write gets the rest of the guard, the visibility read and the private-name pass. Under a
// deployment that names one, and wherever a check is named but cannot be resolved, the answer is
// the setup failure whyNoCheck, which refuses the write.
//
// Under a deployment checker the check found must be that checker too: the program beside it
// (checkProgram) must hash to CheckerVar, or the answer is the setup failure whyOtherCheck, naming
// both digests. So a role runs every write under the one check its deployment named by sha256,
// whatever PUBLIC_LEAK_CHECK, PATH or the pointer file found.
func (g *Guard) sharedCheck(what string) (string, error) {
	checker := value(g.base(), CheckerVar)
	check, err := g.Resolve()
	if err != nil {
		if NoneNamed(err) && checker == "" {
			return "", nil
		}
		return "", &SetupFailure{What: what, Why: whyNoCheck, Err: err}
	}
	if checker == "" {
		return check, nil
	}
	held, err := programSHA256(check)
	switch {
	case err != nil:
		return "", &SetupFailure{What: what, Why: whyOtherCheck,
			Err: fmt.Errorf("%s=%s, and the %s beside %s could not be hashed: %w", CheckerVar, checker, checkProgram, check, err)}
	case !strings.EqualFold(held, checker):
		return "", &SetupFailure{What: what, Why: whyOtherCheck,
			Err: fmt.Errorf("%s=%s, and the %s beside %s hashes to %s", CheckerVar, checker, checkProgram, check, held)}
	}
	return check, nil
}

// found is path, once a file is there: a name taken from a variable, an option or a pointer file
// is somebody's word for where the check is, and a stale one is better reported as the missing
// check it is than as a child process that would not start.
func found(path, source string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", brokenCheck{path: path, error: fmt.Errorf("%s names %s, which is not there: %w", source, path, err)}
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
	// and the one made through curl (Guard.curlVisibility).
	inherit func(name string) bool
	// stdin is the child's standard input, nil for none.
	stdin io.Reader
	// files are the child's descriptors from 3 up, as exec.Cmd.ExtraFiles takes them.
	files []*os.File
	// stderr, where it is not nil, takes the child's standard error apart from the standard
	// output run answers, which the GraphQL visibility read needs to read gh's body alone
	// (Guard.graphQLVisibility), the curl read to read curl's answer alone
	// (Guard.curlVisibility), the token read to quote gh's error without its output
	// (Guard.ghToken), and a git read of a push's records to quote git's error without the push's
	// content (Guard.git). Nil answers the two together.
	stderr io.Writer
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
// together unless c.stderr takes err. The error is a child that could not be run at all: one that
// did not start, one the end of ctx stopped, which is a *stoppedError, or one a signal ended,
// which has no status. A status is an answer, and no status is not.
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
	started := time.Now()
	err := procgroup.Err(cmd.Run())
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
