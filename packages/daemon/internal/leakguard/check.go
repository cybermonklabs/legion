package leakguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
// stands in front of all three, and is how a test hands the guard a stand-in.
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
	if onPath, ok := lookpath.On(value(env, "PATH"), checkName); ok {
		return onPath, nil
	}
	config := value(env, "XDG_CONFIG_HOME")
	if config == "" {
		if home := value(env, "HOME"); home != "" {
			config = filepath.Join(home, ".config")
		}
	}
	if config == "" {
		return "", errors.New(CheckVar + " names none, " + checkName + " is on no PATH entry, and there is no configuration home to read a pointer file from")
	}
	pointer := filepath.Join(config, pointerFile)
	content, err := os.ReadFile(pointer)
	if err != nil {
		return "", fmt.Errorf("%s names none, %s is on no PATH entry, and the pointer file %s could not be read: %w", CheckVar, checkName, pointer, err)
	}
	named := strings.TrimSpace(string(content))
	if named == "" {
		return "", fmt.Errorf("the pointer file %s names no check", pointer)
	}
	return found(named, "the pointer file "+pointer)
}

// found is path, once a file is there: a name taken from a variable, an option or a pointer file
// is somebody's word for where the check is, and a stale one is better reported as the missing
// check it is than as a child process that would not start.
func found(path, source string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%s names %s, which is not there: %w", source, path, err)
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

// child is one process this package runs: the shared check in one of its shapes, the gh read of a
// repository's visibility, or a git command a push's records are read with.
type child struct {
	// argv is the command and its arguments, argv[0] naming the program.
	argv []string
	// dir is the directory the child runs in, empty for this process's own.
	dir string
	// env is what the child's environment carries beyond the guard's own (Guard.env).
	env []string
	// stdin is the child's standard input, nil for none.
	stdin io.Reader
	// files are the child's descriptors from 3 up, as exec.Cmd.ExtraFiles takes them.
	files []*os.File
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
// together. The error is a child that could not be run at all, the caller's bound running out
// among them: a status is an answer, and no status is not.
func (g *Guard) run(ctx context.Context, c child) (int, string, error) {
	// One read of the base environment for the child, which both the program lookup and the
	// child's own environment are built from.
	base := g.base()
	cmd := exec.CommandContext(ctx, program(base, c.argv[0]), c.argv[1:]...)
	cmd.Dir = c.dir
	cmd.Env = g.env(base, c.env...)
	cmd.Stdin = c.stdin
	cmd.ExtraFiles = c.files
	// procgroup before any other SysProcAttr field, as its doc requires: it replaces the whole
	// struct. It also makes the bound reach a helper the child started, which for the shared
	// check is the git it runs against the remote.
	killed := procgroup.Configure(cmd)
	var printed bytes.Buffer
	cmd.Stdout, cmd.Stderr = &printed, &printed
	err := procgroup.Err(cmd.Run())
	switch {
	case killed():
		return 0, printed.String(), fmt.Errorf("%s did not finish within the caller's bound", filepath.Base(c.argv[0]))
	case err != nil:
		return 0, printed.String(), err
	}
	return cmd.ProcessState.ExitCode(), printed.String(), nil
}

// shared runs one shape of the shared check and reads its status: clean passes, 3 refuses with
// the check's own report, and anything else is a setup failure naming the status. It is the one
// place the check's statuses are read, so a shape that meets an undocumented one refuses the
// write rather than reading it as a pass.
func (g *Guard) shared(ctx context.Context, what string, c child) error {
	status, printed, err := g.run(ctx, c)
	if err != nil {
		return &SetupFailure{What: what, Why: "the shared public leak check could not be run", Err: err}
	}
	return readStatus(what, status, printed)
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
