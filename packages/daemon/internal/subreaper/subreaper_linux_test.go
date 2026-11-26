package subreaper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// helperEnv names the case the test binary runs when it is re-executed as a helper. A helper is a
// process of its own because Serve makes its caller a subreaper and reaps every exited child the
// guard does not hold: in the test binary itself that would take the children of every other test
// of the package, and the orphans of the machine's other processes would never reach it.
const helperEnv = "LEGION_SUBREAPER_TEST_HELPER"

// helperBound bounds every wait a helper makes for a process to start, end or be reaped: far past
// what a loaded machine takes for a sleep of a fraction of a second, short enough that a helper
// whose reaper is broken prints what it saw rather than meeting the test binary's alarm.
const helperBound = 20 * time.Second

func TestMain(m *testing.M) {
	if helper := os.Getenv(helperEnv); helper != "" {
		os.Exit(runHelper(helper))
	}
	os.Exit(m.Run())
}

// runHelper runs one case and prints its `key=value` lines. Its exit status is 0 whenever the case
// ran to the end, so every verdict is the test's, read from those lines.
func runHelper(helper string) int {
	h := &helperRun{guard: NewGuard()}
	h.guard.passed = h.record
	var err error
	switch helper {
	case "orphan":
		err = h.orphan(true, nil)
	case "orphan-delayed-record":
		err = h.orphan(true, newRecordDelay())
	case "orphan-no-subreaper":
		err = h.orphan(false, nil)
	case "together":
		err = h.together(nil, false)
	case "together-delayed-record":
		err = h.together(newRecordDelay(), false)
	case "together-delayed-error":
		err = h.together(newRecordDelay(), true)
	case "no-spin":
		err = h.noSpin()
	case "retry":
		err = h.retry()
	default:
		err = fmt.Errorf("no helper case %q", helper)
	}
	if h.delay != nil {
		// Release the reaper before it is stopped: on an error path together and orphan return
		// without releasing it, and a reaper parked in record on the delay would make h.stop hang.
		h.delay.done()
	}
	if h.stop != nil {
		h.stop()
	}
	if err != nil {
		fmt.Println("helper-error=" + strings.ReplaceAll(err.Error(), "\n", " "))
	}
	return 0
}

// recordDelay stops the reaper between its Wait4, which makes an orphan absent from /proc, and
// helperRun.record, which records the pid the pass reaped. It makes the order in the flaky
// together and orphan cases certain: an orphan can be gone before the test has seen the callback's
// record. done is idempotent (sync.Once), and runHelper calls it before it stops the reaper, so a
// helper that returns an error while the reaper is parked in record never leaves it stuck.
type recordDelay struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newRecordDelay() *recordDelay {
	return &recordDelay{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (d *recordDelay) wait() bool {
	select {
	case <-d.entered:
		return true
	case <-time.After(helperBound):
		return false
	}
}

func (d *recordDelay) done() {
	d.once.Do(func() { close(d.release) })
}

// helperRun is one helper's guard and every pass its reaper made.
type helperRun struct {
	guard *Guard
	stop  func()

	delay *recordDelay

	mu     sync.Mutex
	passes int
	reaped []int
}

func (h *helperRun) record(reaped []int) {
	if h.delay != nil && len(reaped) != 0 {
		h.delay.entered <- struct{}{}
		<-h.delay.release
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.passes++
	h.reaped = append(h.reaped, reaped...)
}

// counts are how many passes the reaper has made and how many times it reaped pid.
func (h *helperRun) counts(pid int) (passes, reapedPID int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, got := range h.reaped {
		if got == pid {
			reapedPID++
		}
	}
	return h.passes, reapedPID
}

func (h *helperRun) serve() error {
	stop, err := h.guard.Serve(func(format string, args ...any) { fmt.Printf("reaper-log="+format+"\n", args...) })
	h.stop = stop
	return err
}

// guarded runs script under /bin/sh through the guard, as a caller that waits for its own child
// does, and answers the first line it printed, its pid, and its wait's own error.
func (h *helperRun) guarded(script string) (first string, pid int, waited error) {
	var out bytes.Buffer
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stdout = &out
	pid, err := h.guard.Start(cmd)
	if err != nil {
		return "", 0, err
	}
	waited = cmd.Wait()
	h.guard.Done(pid)
	first, _, _ = strings.Cut(out.String(), "\n")
	return strings.TrimSpace(first), pid, waited
}

// orphan starts, through the guard, a shell that starts a sleep in the background and exits, so
// the sleep is orphaned while it still runs, and reports whose child the sleep became and, once it
// ends, whether it was reaped (gone from /proc) and by how many of the reaper's passes.
//
// delayed, the forced control, holds the reaper after Wait4 made the orphan absent from /proc and
// before record runs. The direct count below then reads 0, which is the same unpromised callback
// order that made the together case flake.
func (h *helperRun) orphan(subreaper bool, delay *recordDelay) error {
	h.delay = delay
	if subreaper {
		if err := h.serve(); err != nil {
			return err
		}
	}
	first, _, waited := h.guarded("/bin/sleep 1 </dev/null >/dev/null 2>&1 & echo $!")
	if waited != nil {
		return fmt.Errorf("the shell's own wait: %w", waited)
	}
	orphan, err := strconv.Atoi(first)
	if err != nil {
		return fmt.Errorf("the shell printed %q, not the sleep's pid", first)
	}
	parent, _, _, err := statOf(orphan)
	if err != nil {
		return err
	}
	fmt.Printf("orphan-parent-is-self=%t\n", parent == os.Getpid())
	if !subreaper {
		return nil
	}
	fmt.Printf("orphan=%s\n", awaitGone(orphan))
	if delay != nil {
		if !delay.wait() {
			return fmt.Errorf("the reaper made the orphan gone but did not enter its record in %s", helperBound)
		}
		delay.done()
	}
	reaps := h.awaitReaps(orphan, 1)
	fmt.Printf("orphan-reaps=%d\n", reaps)
	return nil
}

// together runs rounds in which a guarded child and the orphan it leaves end at the same moment,
// and reports, for every round, the guarded child's status as its own wait read it, how many of the
// reaper's passes reaped the orphan, and whether any reaped the guarded child. The orphan is
// started by a subshell that exits at once, so it comes to this process long before it ends and is
// never the guarded shell's child when it ends, which the shell's own waits would otherwise reap.
//
// delayed, the forced control, holds the reaper after Wait4 reaped the orphan and before record
// runs. The test releases that callback, then waits for its record: the callback's order after
// Wait4 is an implementation detail of Serve, not the test's result.
func (h *helperRun) together(delay *recordDelay, injectError bool) error {
	h.delay = delay
	if err := h.serve(); err != nil {
		return err
	}
	const rounds = 20
	failures := 0
	for round := range rounds {
		first, pid, waited := h.guarded("(/bin/sleep 0.2 </dev/null >/dev/null 2>&1 & echo $!); /bin/sleep 0.2; exit 5")
		var exit *exec.ExitError
		status := "not an exit"
		if errors.As(waited, &exit) {
			status = strconv.Itoa(exit.ExitCode())
		} else if waited != nil {
			status = waited.Error()
		}
		orphan, err := strconv.Atoi(first)
		if err != nil {
			return fmt.Errorf("round %d: the shell printed %q, not the sleep's pid", round, first)
		}
		gone := awaitGone(orphan)
		if delay != nil {
			if !delay.wait() {
				return fmt.Errorf("round %d: the reaper made the orphan gone but did not enter its record in %s", round, helperBound)
			}
			if injectError {
				// The reaper is parked in record on the delay's release. This returns the error
				// path's own way (without delay.done()), so the control proves runHelper releases
				// the delay before it stops the reaper; otherwise stop would hang and this error
				// line would never be printed.
				return fmt.Errorf("round %d: a forced helper error while the reaper is parked in its record", round)
			}
			delay.done()
		}
		orphanReaps := h.awaitReaps(orphan, 1)
		_, childReaps := h.counts(pid)
		if status != "5" || gone != "reaped" || orphanReaps != 1 || childReaps != 0 {
			failures++
			fmt.Printf("together-round-%d=status %s, orphan %s, orphan reaped by %d passes, guarded child reaped by %d passes\n",
				round, status, gone, orphanReaps, childReaps)
		}
	}
	fmt.Printf("together-rounds=%d together-failures=%d\n", rounds, failures)
	return nil
}

// noSpin holds a guarded child that has exited, unwaited, for a while, and reports how many passes
// the reaper made meanwhile; then it waits for the child, reports the status that wait read, and
// whether the reaper made a pass for the Done that followed.
func (h *helperRun) noSpin() error {
	if err := h.serve(); err != nil {
		return err
	}
	// The reaper's first pass, which Serve makes at once, is over before the child starts, so the
	// second below can hold at most the one pass the child's SIGCHLD makes.
	if !h.awaitPasses(1) {
		return fmt.Errorf("the reaper made no first pass in %s", helperBound)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	pid, err := h.guard.Start(cmd)
	if err != nil {
		return err
	}
	if state := awaitZombie(pid); state != "zombie" {
		return fmt.Errorf("the guarded child is %s, not a zombie, %s after its start", state, helperBound)
	}
	before, _ := h.counts(pid)
	time.Sleep(time.Second)
	held, reaps := h.counts(pid)
	fmt.Printf("passes-while-held=%d held-child-reaps=%d\n", held-before, reaps)
	waited := cmd.Wait()
	h.guard.Done(pid)
	var exit *exec.ExitError
	if errors.As(waited, &exit) {
		fmt.Printf("status=%d\n", exit.ExitCode())
	} else {
		fmt.Printf("status=%v\n", waited)
	}
	deadline := time.Now().Add(helperBound)
	for {
		if after, _ := h.counts(pid); after > held || time.Now().After(deadline) {
			fmt.Printf("pass-after-done=%t\n", after > held)
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// retry makes the reaper's passes fail (the guard lists a /proc that does not exist) while an
// orphan ends, so the pass its SIGCHLD makes fails, then lets passes succeed again with no event
// to ask for one, and reports whether the orphan was reaped all the same.
func (h *helperRun) retry() error {
	h.guard.mu.Lock()
	h.guard.proc = "/proc/no-such-directory"
	h.guard.mu.Unlock()
	if err := h.serve(); err != nil {
		return err
	}
	first, _, waited := h.guarded("/bin/sleep 0.3 </dev/null >/dev/null 2>&1 & echo $!")
	if waited != nil {
		return fmt.Errorf("the shell's own wait: %w", waited)
	}
	orphan, err := strconv.Atoi(first)
	if err != nil {
		return fmt.Errorf("the shell printed %q, not the sleep's pid", first)
	}
	if state := awaitZombie(orphan); state != "zombie" {
		return fmt.Errorf("the orphan is %s, not a zombie, %s after its start", state, helperBound)
	}
	// Long enough for the pass the orphan's SIGCHLD made to have failed.
	time.Sleep(200 * time.Millisecond)
	h.guard.mu.Lock()
	h.guard.proc = "/proc"
	h.guard.mu.Unlock()
	fmt.Printf("orphan=%s\n", awaitGone(orphan))
	return nil
}

// awaitPasses waits, bounded, for the reaper to have made n passes.
func (h *helperRun) awaitPasses(n int) bool {
	deadline := time.Now().Add(helperBound)
	for {
		if passes, _ := h.counts(0); passes >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitReaps waits, bounded, for h.record to name pid at least n times. Wait4 makes pid absent
// from /proc before Serve calls that callback, so awaitGone is not the condition that proves the
// pass's report is ready.
func (h *helperRun) awaitReaps(pid, n int) int {
	deadline := time.Now().Add(helperBound)
	for {
		_, reaps := h.counts(pid)
		if reaps >= n || time.Now().After(deadline) {
			return reaps
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// statOf reads pid's parent, process group and state from /proc.
func statOf(pid int) (parent, group int, state string, err error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, "", err
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, 0, "", fmt.Errorf("an unreadable stat for %d: %q", pid, stat)
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 3 {
		return 0, 0, "", fmt.Errorf("an unreadable stat for %d: %q", pid, stat)
	}
	parent, _ = strconv.Atoi(fields[1])
	group, _ = strconv.Atoi(fields[2])
	return parent, group, fields[0], nil
}

// awaitGone waits for pid to leave /proc: "reaped" once it has, "zombie" when it is still there,
// exited and unreaped, at the bound, and "running" when it has not ended by then.
func awaitGone(pid int) string {
	deadline := time.Now().Add(helperBound)
	for {
		_, _, state, err := statOf(pid)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return "reaped"
		case time.Now().After(deadline) && state == "Z":
			return "zombie"
		case time.Now().After(deadline):
			return "running"
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitZombie waits for pid, a child that exits at once, to be a zombie.
func awaitZombie(pid int) string {
	deadline := time.Now().Add(helperBound)
	for {
		_, _, state, err := statOf(pid)
		switch {
		case err != nil:
			return err.Error()
		case state == "Z":
			return "zombie"
		case time.Now().After(deadline):
			return "state " + state
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runCase runs the test binary as the helper of name and answers its `key=value` lines and its
// whole output for a failure's message.
func runCase(t *testing.T, name string) (map[string]string, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*helperBound)
	defer cancel()
	helper := exec.CommandContext(ctx, self, "-test.run=^$")
	helper.Env = append(os.Environ(), helperEnv+"="+name)
	out, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s helper: %v\n%s", name, err, out)
	}
	report := map[string]string{}
	for line := range strings.Lines(string(out)) {
		for field := range strings.FieldsSeq(line) {
			if key, value, ok := strings.Cut(field, "="); ok && !strings.Contains(line, "-round-") {
				report[key] = value
			}
		}
	}
	if report["helper-error"] != "" || strings.Contains(string(out), "helper-error=") {
		t.Fatalf("the %s helper failed:\n%s", name, out)
	}
	return report, string(out)
}

// A process that serves its guard takes in the orphan a child of its own leaves, and reaps it once
// it ends, once. The delayed-record control stops the reaper after Wait4 made the orphan absent
// from /proc and before the callback records it, so the count read right after awaitGone would
// read 0: orphan reads it through awaitReaps instead. The no-subreaper control runs the same child
// without Serve: the orphan then goes to another process, which is what makes the first answer the
// subreaper's doing.
func TestAnOrphanComesToTheGuardsProcessAndIsReapedThere(t *testing.T) {
	for _, helper := range []string{"orphan", "orphan-delayed-record"} {
		report, out := runCase(t, helper)
		if report["orphan-parent-is-self"] != "true" {
			t.Errorf("%s: the orphan's parent is not the process that serves the guard\n%s", helper, out)
		}
		if report["orphan"] != "reaped" || report["orphan-reaps"] != "1" {
			t.Errorf("%s: the orphan is %q and was reaped by %q passes, want reaped by one\n%s", helper, report["orphan"], report["orphan-reaps"], out)
		}
	}
	control, out := runCase(t, "orphan-no-subreaper")
	if control["orphan-parent-is-self"] != "false" {
		t.Errorf("without Serve the orphan's parent is still the helper, so the case above shows nothing of Serve's\n%s", out)
	}
}

// A guarded child and the orphan it leaves behind end at the same moment, twenty times over: each
// time the guarded child's own wait reads its exit 5, the reaper never reaps the guarded child, and
// it reaps the orphan exactly once. The delayed record control stops the reaper after Wait4 made
// the orphan absent from /proc and before the callback records it, which forces the order that
// flaked in CI. The test must wait for that record, rather than treating disappearance from /proc
// as proof that it has happened.
func TestAGuardedChildAndAnOrphanEndingTogetherAreEachReapedOnce(t *testing.T) {
	for _, helper := range []string{"together", "together-delayed-record"} {
		report, out := runCase(t, helper)
		if report["together-rounds"] != "20" || report["together-failures"] != "0" {
			t.Errorf("%s: rounds %q, failures %q, want 20 rounds and none failed\n%s", helper, report["together-rounds"], report["together-failures"], out)
		}
	}
}

// A helper that returns an error while the reaper is parked in its record (the delayed round of
// the together case, forced by together-delayed-error) reaches the test with its error line, and
// within a short bound: runHelper releases the delay before it stops the reaper, so h.stop does not
// hang on the parked reaper. A runHelper that stopped the reaper without releasing it would hang
// until runCase's own context killed the helper, and the error line would be lost.
func TestAHelperErrorWithTheReaperParkedReachesTheTest(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperBound)
	defer cancel()
	helper := exec.CommandContext(ctx, self, "-test.run=^$")
	helper.Env = append(os.Environ(), helperEnv+"=together-delayed-error")
	started := time.Now()
	out, err := helper.CombinedOutput()
	elapsed := time.Since(started)
	if ctx.Err() != nil {
		t.Fatalf("the helper did not exit on its own in %s, so h.stop hung on the parked reaper: %v\n%s", helperBound, err, out)
	}
	if err != nil {
		t.Fatalf("the helper: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "helper-error=") {
		t.Fatalf("the helper printed no helper-error line, so its forced error never reached the test\n%s", out)
	}
	if elapsed > helperBound/2 {
		t.Errorf("the helper took %s to report its error, want well under %s: the reaper's release is not prompt\n%s", elapsed, helperBound, out)
	}
}

// A guarded child that has exited and that its wait has not read yet is passed over, and costs the
// reaper no more than the pass its SIGCHLD makes: over a second of it the reaper makes at most that
// one pass, where a reaper that looped until no exited child was left would make thousands. The
// wait then reads the child's own exit 7, and its Done makes one more pass, which shows the
// counter counts.
func TestTheReaperDoesNotSpinOnAnExitedGuardedChild(t *testing.T) {
	report, out := runCase(t, "no-spin")
	passes, err := strconv.Atoi(report["passes-while-held"])
	if err != nil || passes > 1 {
		t.Errorf("the reaper made %q passes while a guarded child it must pass over had exited, want at most 1\n%s", report["passes-while-held"], out)
	}
	if report["held-child-reaps"] != "0" {
		t.Errorf("the reaper reaped the guarded child %q times, want none\n%s", report["held-child-reaps"], out)
	}
	if report["status"] != "7" {
		t.Errorf("the guarded child's own wait read %q, want its exit 7\n%s", report["status"], out)
	}
	if report["pass-after-done"] != "true" {
		t.Errorf("no pass followed the guarded child's Done, so the count above may count nothing\n%s", out)
	}
}

// A pass that fails is tried again on a clock of its own: the passes fail while an orphan ends, so
// the pass its SIGCHLD makes fails too, and once passes can succeed again no event comes to ask
// for one. The orphan must still be reaped, where a reaper that waited for the next event would
// leave it a zombie for good.
func TestAFailedPassIsTriedAgainWithNoFurtherEvent(t *testing.T) {
	report, out := runCase(t, "retry")
	if report["orphan"] != "reaped" {
		t.Errorf("the orphan is %q once passes could succeed again, want reaped by a retried pass\n%s", report["orphan"], out)
	}
	if !strings.Contains(out, "trying again in") {
		t.Errorf("no pass failed, so the case shows nothing of the retry\n%s", out)
	}
}

// Once a guarded child's own wait has reaped it, and before that wait's Done, the kernel may give
// its pid to a new child started through the same guard. The earlier Done must leave the new
// child held, or a pass could take the new child's exit from its own wait. The pid is held twice
// through hold, the record Start makes, once for each child, and released as Done releases it:
// a hold that marked the pid rather than counting it leaves it held 0 times after the first Done.
func TestAnEarlierDoneLeavesALaterChildOfTheSamePidHeld(t *testing.T) {
	g := NewGuard()
	const pid = 4242
	g.mu.Lock()
	g.hold(pid) // the first child, whose wait has reaped it but not yet called Done
	g.hold(pid) // the second child, given the same pid
	g.mu.Unlock()
	g.Done(pid)
	g.mu.Lock()
	held := g.pids[pid]
	g.mu.Unlock()
	if held != 1 {
		t.Fatalf("after the first child's Done the pid is held %d times, want 1, the second child's", held)
	}
	g.Done(pid)
	g.mu.Lock()
	_, left := g.pids[pid]
	g.mu.Unlock()
	if left {
		t.Fatal("after both Dones the pid is still recorded")
	}
}
