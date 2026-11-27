package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// checkoutDelay is how long the jj the cases below start sleeps before a checkout, and
// checkoutBudget the runner's slow-command budget under every other command of their provisions:
// the delay outlasts that budget by three seconds, so that checkout runs past it however fast this
// machine is. Every other provisioning command of the fixture's one-commit repository answers in
// milliseconds on an idle machine, well inside the budget. A loaded machine can start such a
// command's jj so late that the budget ends it, which says nothing of provisioning, so each case
// judges only a run in which no command took the budget for that alone (budgetSpans.startedLate).
const (
	checkoutDelay  = 8 * time.Second
	checkoutBudget = 5 * time.Second
)

// checkoutRunMargin is what each case below leaves of the test binary's deadline
// (testwait.Bounded): a run's checkout delay, one command held to the runner's budget, and the
// time a run the budget cuts takes to return and the row to fail in (cutRunMargin).
const checkoutRunMargin = checkoutDelay + checkoutBudget + cutRunMargin

// lateStart is how long the stand-in jj (slowCheckoutTools) may take, from the moment the runner
// started it to its first line, before a command of it that took the runner's budget is one the
// machine held there (budgetSpans.startedLate). An idle machine brings a shell to its first line
// within milliseconds of its start, and a second is what main's rows gave a command to start and
// report that it was running (startBound). Nothing the runner does before that start or after it
// moves the mark (markStart), so a run in which the product held a command to the budget reads as
// late only where the machine also brought that command's jj to its first line a second or more
// after its start, which an idle machine never does.
const lateStart = time.Second

// standInMark is the environment entry in which budgetSpans names, for each command, the file its
// stand-in jj writes its start mark to (markStart).
const standInMark = "LEGION_TEST_STANDIN_MARK"

// slowCheckoutTools is testTools with a jj that sleeps delay whenever its arguments hold `sparse
// reset`, so the checkout alone outlasts a runner budget shorter than delay. The sleeping script
// is first on the runner's jj entry and execs testTools' own jj wrapper with every argument it was
// given, so whatever that wrapper does for GIT_ALLOW_PROTOCOL still happens: the fixture's remote
// is a local path, which git reaches over its file transport alone. It reads the two words
// wherever they stand rather than as "$1 $2", because the runner puts its own
// --config=git.executable-path first on every jj it starts (execRunner.Run); no other jj
// provisioning runs holds them side by side (`jj sparse list`, `jj sparse set --clear`). Where its
// environment names a file under standInMark (budgetSpans), its first act writes its start mark
// there (markStart): when the runner started it, as the kernel counts it, and when it reached that
// line.
func slowCheckoutTools(t *testing.T, delay time.Duration) map[string]string {
	t.Helper()
	tools := testTools(t)
	wrapper := filepath.Join(t.TempDir(), "jj")
	mark := "[ -z \"${" + standInMark + "-}\" ] || { " + testexec.MarkStart(`"$`+standInMark+`"`) + "; }\n"
	script := "#!/bin/sh\n" + mark + "case \" $* \" in *' sparse reset '*) sleep " + strconv.FormatFloat(delay.Seconds(), 'f', -1, 64) + " ;; esac\nexec '" + tools["jj"] + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tools["jj"] = wrapper
	return tools
}

// slowCheckoutRunner is newLocalRunner whose jj sleeps delay before a checkout and whose
// slow-command budget is budget, what the runner gives a command carrying no bound of its own.
func slowCheckoutRunner(t *testing.T, delay, budget time.Duration) *recordingRunner {
	t.Helper()
	run := newLocalRunner(t)
	run.timeout = budget
	run.runner = NewRunner(budget, slowCheckoutTools(t, delay))
	return run
}

// budgetSpans is a Runner that runs every command through runner, each command but the checkout
// that carries runner's own budget (its Timeout) under budget in its place, and keeps how long the
// checkout took and each other command under budget that took as long as budget, each measured
// around the whole of its run, with the start mark its stand-in jj wrote (standInMark), where it
// wrote one. A command that took less than the budget ended before the budget could end it, and
// one whose jj started late was held there by the machine (startedLate): those are the facts a
// case judges a run by, read apart from the provision's answer, which is what the case reads.
type budgetSpans struct {
	runner Runner
	budget time.Duration
	marks  string

	mu       sync.Mutex
	commands int
	checkout time.Duration
	over     []overBudget
}

// overBudget is a command that took as long as budgetSpans' budget: its argv, how long it took,
// and the start mark its stand-in jj wrote, where marked.
type overBudget struct {
	argv   []string
	span   time.Duration
	mark   testexec.StartMark
	marked bool
}

// newBudgetSpans is a budgetSpans over runner whose commands write their start marks in a
// directory of t's own.
func newBudgetSpans(t *testing.T, runner Runner, budget time.Duration) *budgetSpans {
	t.Helper()
	return &budgetSpans{runner: runner, budget: budget, marks: t.TempDir()}
}

func (s *budgetSpans) Timeout() time.Duration { return s.runner.Timeout() }

func (s *budgetSpans) Run(ctx context.Context, command Command) (Result, error) {
	checkout := isCheckout(command.Argv)
	held := !checkout && command.Timeout == s.runner.Timeout()
	if held {
		command.Timeout = s.budget
	}
	s.mu.Lock()
	s.commands++
	mark := filepath.Join(s.marks, strconv.Itoa(s.commands))
	s.mu.Unlock()
	command.Env = append(slices.Clip(command.Env), standInMark+"="+mark)
	started := time.Now()
	result, err := s.runner.Run(ctx, command)
	span := time.Since(started)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case checkout:
		s.checkout = span
	case held && span >= s.budget:
		read, readErr := testexec.ReadStartMark(mark)
		s.over = append(s.over, overBudget{argv: command.Argv, span: span, mark: read, marked: readErr == nil})
	}
	return result, err
}

// checkoutSpan is how long the checkout took.
func (s *budgetSpans) checkoutSpan() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkout
}

// startedLate answers a command that took the budget, and whether every command that did had a
// stand-in jj that reached its first line lateStart or more after the runner started it: a run of
// which that is so, and in which some command took the budget, is one the machine held, not the
// product. A command that took the budget with its jj on time, or with no mark of its own (a git,
// or a jj the budget ended before its first line), leaves the run judged, so a provisioning command
// the product holds to the budget fails the run.
func (s *budgetSpans) startedLate() (overBudget, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.over) == 0 {
		return overBudget{}, false
	}
	for _, over := range s.over {
		if !over.marked || over.mark.Late() < lateStart {
			return overBudget{}, false
		}
	}
	return s.over[0], true
}

// notJudged is whether run number runs, which Provision answered err, is one its case does not
// judge (startedLate), and logs it as such. Once until has passed, within after the case began,
// such a run fails the case instead: it has had no run of the right order.
func (s *budgetSpans) notJudged(t *testing.T, runs int, within time.Duration, until time.Time, err error) bool {
	t.Helper()
	late, ok := s.startedLate()
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		t.Logf("run %d is not judged: %q took %s, as long as the runner's %s budget, and its jj reached its first line %s after the runner started it (Provision = %v)", runs, late.argv, late.span, s.budget, late.mark.Late(), err)
		return true
	}
	t.Fatalf("no run of the right order in %d runs over %s: in each, a command under the runner's %s budget took that long, its jj reaching its first line %s or more after the runner started it (in the last, %q took %s, its jj %s late; Provision = %v)",
		runs, within, s.budget, lateStart, late.argv, late.span, late.mark.Late(), err)
	return false
}

// Each provisioning command is given the bound its own class carries: the checkout, the one that
// writes the issue's whole working copy, is given CheckoutTimeout, and a command carrying no
// bound of its own — `jj workspace update-stale`, which only a workspace that already exists runs,
// among them — the runner's slow-command budget. The shared clone's size-following steps share one
// CloneTimeout (TestProvisionClonesThroughTemporarySiblingWithCredentialReset on the tmux runtime,
// TestTheFillGivesTheCachesVerificationAndTheDeltaTheirOwnBounds in an issue pod), so this case
// passes over them (isSharedCloneStep). A fresh workspace and one provisioned a second time are read
// together, so both paths through Provision are covered.
func TestProvisionBoundsEachCommandByItsOwnBudget(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	checkouts, stale := 0, 0
	for _, round := range []string{"a fresh workspace", "a workspace that already exists"} {
		before := len(run.Calls())
		if _, err := Provision(context.Background(), run, req); err != nil {
			t.Fatalf("provision %s: %v", round, err)
		}
		for _, call := range run.Calls()[before:] {
			// The shared clone's own size-following steps carry what remains of CloneTimeout: they
			// are recognised here by isSharedCloneStep, and left to the tests named above.
			if isSharedCloneStep(call.Argv) {
				continue
			}
			if isCheckout(call.Argv) {
				checkouts++
				if call.Timeout != CheckoutTimeout {
					t.Errorf("%s: the checkout %q was given %s, want CheckoutTimeout %s", round, call.Argv, call.Timeout, CheckoutTimeout)
				}
				continue
			}
			if commandWith(call.Argv, "jj", "workspace", "update-stale") {
				stale++
			}
			if call.Timeout != testTimeout {
				t.Errorf("%s: %q was given %s, want the runner's budget %s", round, call.Argv, call.Timeout, testTimeout)
			}
		}
	}
	// The fresh round checks out; the second finds the workspace checked out already (CheckedOut)
	// and runs `jj workspace update-stale` instead, so each is seen once over the two rounds.
	if checkouts != 1 {
		t.Errorf("provisioning ran %d checkouts over the two rounds, want the fresh workspace's one", checkouts)
	}
	if stale != 1 {
		t.Errorf("provisioning ran %d `jj workspace update-stale` over the two rounds, want the existing workspace's one", stale)
	}
}

// A checkout that outlasts the runner's slow-command budget finishes, and the workspace it wrote
// holds the whole tree: the checkout is bounded by CheckoutTimeout, an hour, and not by that
// budget.
//
// Every other command of the provision carries that budget, which a clock counts from before each
// command starts, and a loaded machine can start a command's jj so late that the budget ends it:
// the provision then fails on that command's timeout and says nothing of the checkout's bound.
// Such a run is not judged (budgetSpans.startedLate), and the case provisions again, from a remote
// and a state of its own, until a run of the right order comes, within a minute. A command that
// took the budget for any other reason fails the run, as it failed main's one run, and so does a
// run whose checkout took less than the budget, which proves nothing.
func TestProvisionChecksOutPastTheRunnersBudget(t *testing.T) {
	within := testwait.Bounded(t, time.Minute, checkoutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		run := slowCheckoutRunner(t, checkoutDelay, checkoutBudget)
		spans := newBudgetSpans(t, run.runner, checkoutBudget)
		run.runner = spans
		pushRemoteBranch(t, run.remote, "main", "history.txt")
		working, err := Provision(context.Background(), run, provisionRequest(t))

		if spans.notJudged(t, runs, within, until, err) {
			continue
		}
		if err != nil {
			t.Fatalf("run %d: Provision: %v", runs, err)
		}
		if checkout := spans.checkoutSpan(); checkout < checkoutBudget {
			t.Fatalf("run %d: the checkout took %s, inside the runner's %s budget, so this run proves nothing", runs, checkout, checkoutBudget)
		}
		requireCheckedOut(t, working)
		// The file list requireCheckedOut read is read again here: what this case proves beyond it
		// is that the checkout wrote each of those files to disk, and the list is unchanged between
		// the two reads — nothing writes to the workspace in between.
		files := strings.Fields(runSetup(t, working.Dir, "jj", "file", "list", "-R", working.Dir))
		for _, file := range files {
			if _, err := os.Stat(filepath.Join(working.Dir, file)); err != nil {
				t.Errorf("the checkout left %s unwritten: %v", file, err)
			}
		}
		return
	}
}

// A checkout that outlives its own bound fails the provision, naming the checkout: the checkout
// is bounded by that bound and by no other, so a request that gives it one second (CheckoutBound)
// ends the checkout there. Every other command of the provision that carries the runner's
// slow-command budget runs under five seconds in its place, main's budget (budgetSpans), so one
// that takes that long fails the case. The runner's own budget, which a checkout given it in place
// of its bound would carry, is the rest of the test binary's deadline, far past the checkout's
// delay, so such a checkout would finish, and the provision would succeed. A run in which another
// command took the five seconds only because the machine started its jj late is not judged, as in
// TestProvisionChecksOutPastTheRunnersBudget, and the case provisions again, within a minute.
func TestProvisionFailsWhenTheCheckoutOutlivesItsBound(t *testing.T) {
	within := testwait.Bounded(t, time.Minute, checkoutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		run := slowCheckoutRunner(t, checkoutDelay, testwait.Remaining(t))
		spans := newBudgetSpans(t, run.runner, checkoutBudget)
		run.runner = spans
		req := provisionRequest(t)
		req.CheckoutBound = time.Second
		working, err := Location(req.StateDir, req.Repo, req.Issue)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Provision(context.Background(), run, req)

		if spans.notJudged(t, runs, within, until, err) {
			continue
		}
		want := "check out workspace " + working.Dir + ": command timed out: jj sparse reset"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("run %d: Provision whose checkout outlived its one-second bound = %v, want an error holding %q", runs, err, want)
		}
		return
	}
}
