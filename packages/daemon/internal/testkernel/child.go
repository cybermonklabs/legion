package testkernel

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A Child is the child a row's command starts in the background and leaves in the command's
// process group (procgroup.Configure), holding the command's output pipe past the command's exit.
// It tells whether a stop of that group reached it by the moment the run returned, and, where one
// did, whether os/exec still read that pipe then and when the stop's trap ran (After).
//
// The child ignores SIGPIPE, and its SIGTERM trap writes one byte to the pipe and records what the
// write found, and when the trap ran (testexec.MarkNow). Wait closes the pipe's read end once
// WaitDelay has run out, before it returns (os/exec's awaitGoroutines), and the budget's own stop,
// procgroup.Configure's Cancel, runs only before that wait has begun: the read end is open then,
// and stays open until the child has left on the SIGTERM or the grace's SIGKILL has ended it. So:
//   - EPIPE: no reader was left, so the stop came after the run.
//   - the byte went through: os/exec still read the pipe, so the stop was the budget's own, inside
//     Wait.
//   - no record, and the child gone: the grace's SIGKILL, which only the budget's own stop sends,
//     ended it before its trap wrote.
//
// That no SIGTERM reached the child by the run's return is a fact of the run too, not a record
// missing by some time. The child writes a beat, the time since boot (testexec.MarkNow), at each
// turn of its loop, and dash runs a pending trap at the end of each command, the trap exits, and a
// signal sent before the child reads the time is taken on that read's return: so a beat read past
// the run's return, and one more after it, mean no SIGTERM was sent by then. /proc/<pid>/status
// cannot say it: gVisor's has no SigPnd or ShdPnd, and a signal the shell has taken but whose trap
// has not yet run is pending in neither. The command exits only once the child has set its traps
// (its ready mark), so a run whose budget passed while the command's leader still ran has no ready
// mark. The child runs until the row releases it (Release), a stop or the row's kill ends it, or
// the test process is gone, never on a timer of its own.
type Child struct {
	dir      string
	released sync.Once
}

// childScript is the child's shell text: $1 is the directory of its records, and $2 the test
// process, whose end ends it too.
var childScript = `dir=$1 owner=$2
echo $$ >"$dir/pid"
onterm() { if printf x; then w=open; else w=closed; fi; ` + testexec.MarkNow(`"$dir/termed.at"`) + `; echo "$w" >"$dir/termed"; exit 0; }
trap '' PIPE
trap onterm TERM
: >"$dir/ready"
while [ ! -e "$dir/release" ] && kill -0 "$owner" 2>/dev/null; do ` + testexec.MarkNow(`"$dir/beat"`) + `; sleep 0.01; done
`

// NewChild is a Child whose records are in a directory of t's own. It is released at t's end
// whatever the row did (Release).
func NewChild(t testing.TB) *Child {
	t.Helper()
	c := &Child{dir: t.TempDir()}
	t.Cleanup(func() { c.Release(t) })
	return c
}

// ChildCommand is the shell text of a command, run as `sh -c <text> sh <Child.Args>...`, that runs
// answer, a command of the row's, then starts the child in the background, in the command's process
// group, and exits 0 once the child has set its traps, or once the test process is gone.
func ChildCommand(answer string) string {
	return answer + "; " + startLines(`"$1"`, `"$2"`, `"$3"`) + "; exit 0"
}

// Start is lines of shell for a command's own script that goes on past the child's start, as
// ChildCommand's does not: they start the child in the background, in the process group of the
// shell that runs them, and go on once the child has set its traps, or once the test process is
// gone. The child's script, the directory of its records and the test process's pid are written
// into the lines, each one quoted shell word, rather than read from the command's arguments.
func (c *Child) Start() string {
	args := c.Args()
	return startLines(shellprefix.Literal(args[0]), shellprefix.Literal(args[1]), args[2])
}

// startLines starts the child whose script, records directory and test process are the shell
// words script, dir and owner, and waits for its ready mark while the test process lives.
func startLines(script, dir, owner string) string {
	return `sh -c ` + script + ` child ` + dir + ` ` + owner + ` & while [ ! -e ` + dir + `/ready ] && kill -0 ` + owner + ` 2>/dev/null; do sleep 0.01; done`
}

// Args are the arguments, after its $0, of the command ChildCommand writes: the child's script, the
// directory of its records, and the test process's pid.
func (c *Child) Args() []string {
	return []string{childScript, c.dir, strconv.Itoa(os.Getpid())}
}

// Release ends the child, once however often it is called: it writes the mark the child's loop
// stops at, and kills the child in case the mark does not reach it. A second kill could find the
// child's pid taken by a process of another's.
func (c *Child) Release(t testing.TB) {
	t.Helper()
	c.released.Do(func() {
		if err := os.WriteFile(c.path("release"), nil, 0o644); err != nil {
			t.Errorf("release the child: %v", err)
		}
		if pid := pidIn(c.path("pid")); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// Stop is what a Child saw of a stop of its group by its run's return (Child.After): no SIGTERM
// (None); or one whose trap ran at At, a time since boot, and whose write found the command's
// output pipe closed (AfterRun) or open (InWait); and, for a run After cannot judge, why
// (Unjudged). A stop that found the pipe open came inside Wait, where the budget's own stop comes,
// so After cannot judge it and names why. A row that knows the earliest moment its budget could
// pass judges an InWait stop whose trap ran before that moment: it was no stop of the budget's.
type Stop struct {
	None, AfterRun, InWait bool
	At                     time.Duration
	Unjudged               string
}

// Why After cannot judge a run.
const (
	stoppedBeforeReady = "the command was stopped before its child had set its traps, which the command waits for before it exits: its budget passed while its leader still ran"
	stoppedInWait      = "the child the command left in its group took a SIGTERM while os/exec still read the command's output pipe (its write went through), so the stop was the budget's own, inside Wait: the machine reaped the command's leader past its budget"
	killedFirst        = "the child the command left in its group ended before its SIGTERM trap recorded anything: the grace's SIGKILL, which only the budget's own stop sends, ended it"
	notSeen            = "by the end of the row's bound the child the command left in its group had neither recorded a SIGTERM, nor ended, nor run on past the run's return, so the row cannot tell whether a stop reached it"
)

// After reads what the child of a run that returned at returned, a time since boot
// (testexec.SinceBoot), saw of a stop, from its ready mark, the record its SIGTERM trap writes, and
// its beat, as Child's doc says. It waits until the child has recorded a SIGTERM, has ended, or has
// written a beat past returned and one more after it, or until until passes, which it reports as a
// run it cannot judge. It calls t.Fatalf, so it must be called from the goroutine running the test.
func (c *Child) After(t testing.TB, returned time.Duration, until time.Time) Stop {
	t.Helper()
	if _, err := os.Stat(c.path("ready")); err != nil {
		return Stop{Unjudged: stoppedBeforeReady}
	}
	pid := pidIn(c.path("pid"))
	var seen Stop
	recorded := func() bool {
		raw, err := os.ReadFile(c.path("termed"))
		if err != nil || !strings.HasSuffix(string(raw), "\n") {
			return false
		}
		at, err := testexec.ReadNowMark(c.path("termed.at"))
		if err != nil {
			t.Fatalf("read when the child's SIGTERM trap ran, which it records before what its write found: %v", err)
		}
		switch found := strings.TrimSpace(string(raw)); found {
		case "closed":
			seen = Stop{AfterRun: true, At: at}
		case "open":
			seen = Stop{InWait: true, At: at, Unjudged: stoppedInWait}
		default:
			t.Fatalf("the child's SIGTERM trap recorded %q, want open or closed", found)
		}
		return true
	}
	var past time.Duration
	if !testwait.Holds(time.Until(until), testwait.Poll, func() bool {
		if recorded() {
			return true
		}
		if ProcessEnded(t, pid) {
			// It may have recorded a SIGTERM and left between the two reads.
			if !recorded() {
				seen = Stop{Unjudged: killedFirst}
			}
			return true
		}
		at, err := testexec.ReadNowMark(c.path("beat"))
		if err != nil || at <= returned {
			return false
		}
		if past == 0 {
			past = at
			return false
		}
		seen.None = at > past
		return seen.None
	}) {
		return Stop{Unjudged: notSeen}
	}
	return seen
}

// path is the child's record named name.
func (c *Child) path(name string) string { return filepath.Join(c.dir, name) }

// pidIn is the pid a shell wrote to path, once the whole line is there, and 0 before.
func pidIn(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(raw), "\n") {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// ProcessEnded is whether the process pid is gone, or has exited and waits only to be reaped: its
// state, the third field of /proc/<pid>/stat after the command in parentheses, reads Z. A child a
// command left behind is reaped by whatever it was reparented to, not by the run, and kill(pid, 0)
// answers nil for it until then. A process kill still finds is never read as ended from a /proc
// entry that cannot be read: the test fails there, naming /proc, since it has no other way to ask.
// kill is asked again after such a read, because a process that went between the two leaves no
// entry, and has ended. It calls t.Fatalf, so it must be called from the goroutine running the
// test.
func ProcessEnded(t testing.TB, pid int) bool {
	t.Helper()
	if pid <= 0 {
		return false
	}
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	body, err := os.ReadFile(stat)
	if err != nil {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		t.Fatalf("%s cannot be read (%v) for process %d, which kill still finds: this row reads from /proc whether a process that answers kill has exited", stat, err, pid)
	}
	_, after, ok := strings.Cut(string(body), ") ")
	return ok && strings.HasPrefix(after, "Z")
}
