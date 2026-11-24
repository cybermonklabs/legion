package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// checkoutDelay is how long the jj the cases below start sleeps before a checkout, and
// checkoutBudget the runner's slow-command budget under TestProvisionChecksOutPastTheRunnersBudget:
// the delay outlasts that budget by three seconds, so that checkout runs past it however fast this
// machine is. Every other provisioning command of the fixture's one-commit repository answers in
// milliseconds on an idle machine, but a loaded one can hold such a command as long as the budget,
// so that case judges only a run in which none took that long (budgetSpans).
const (
	checkoutDelay  = 8 * time.Second
	checkoutBudget = 5 * time.Second
)

// checkoutRunMargin is what TestProvisionChecksOutPastTheRunnersBudget leaves of the test binary's
// deadline (testwait.Bounded): a run's checkout delay, one command held to the runner's budget,
// and the time a run the budget cuts takes to return and the row to fail in (cutRunMargin).
const checkoutRunMargin = checkoutDelay + checkoutBudget + cutRunMargin

// slowCheckoutTools is testTools with a jj that sleeps delay whenever its arguments hold `sparse
// reset`, so the checkout alone outlasts a runner budget shorter than delay. The sleeping script
// is first on the runner's jj entry and execs testTools' own jj wrapper with every argument it was
// given, so whatever that wrapper does for GIT_ALLOW_PROTOCOL still happens: the fixture's remote
// is a local path, which git reaches over its file transport alone. It reads the two words
// wherever they stand rather than as "$1 $2", because the runner puts its own
// --config=git.executable-path first on every jj it starts (execRunner.Run); no other jj
// provisioning runs holds them side by side (`jj sparse list`, `jj sparse set --clear`).
func slowCheckoutTools(t *testing.T, delay time.Duration) map[string]string {
	t.Helper()
	tools := testTools(t)
	wrapper := filepath.Join(t.TempDir(), "jj")
	script := "#!/bin/sh\ncase \" $* \" in *' sparse reset '*) sleep " + strconv.FormatFloat(delay.Seconds(), 'f', -1, 64) + " ;; esac\nexec '" + tools["jj"] + "' \"$@\"\n"
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

// budgetSpans is a Runner that runs every command through runner and keeps how long the checkout
// took and the longest any other command carrying runner's own budget (its Timeout) took, each
// measured around the whole of its run. A command that took less than the budget ended before the
// budget could end it: that is the fact TestProvisionChecksOutPastTheRunnersBudget judges a run
// by, read apart from the checkout's answer, which is what the row reads.
type budgetSpans struct {
	runner Runner

	mu          sync.Mutex
	checkout    time.Duration
	longest     time.Duration
	longestArgv []string
}

func (s *budgetSpans) Timeout() time.Duration { return s.runner.Timeout() }

func (s *budgetSpans) Run(ctx context.Context, command Command) (Result, error) {
	started := time.Now()
	result, err := s.runner.Run(ctx, command)
	span := time.Since(started)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case isCheckout(command.Argv):
		s.checkout = span
	case command.Timeout == s.runner.Timeout() && span > s.longest:
		s.longest, s.longestArgv = span, command.Argv
	}
	return result, err
}

// spans is what s kept: the checkout's span, and the longest span of any other command under the
// runner's budget, with that command's argv.
func (s *budgetSpans) spans() (checkout, longest time.Duration, longestArgv []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkout, s.longest, s.longestArgv
}

// Each provisioning command is given the bound its own class carries: the checkout, the one that
// writes the issue's whole working copy, is given CheckoutTimeout, and a command carrying no
// bound of its own — `jj workspace update-stale`, which only a workspace that already exists runs,
// among them — the runner's slow-command budget. The shared clone's size-following steps share one
// CloneTimeout and are TestTheSharedClonesCloneIsBoundedByCloneTimeout's, so this case passes
// over them (isSharedCloneStep). A fresh workspace and one provisioned a second time are read
// together, so both paths through Provision are covered.
func TestProvisionBoundsEachCommandByItsOwnBudget(t *testing.T) {
	run := newLocalRunner(t)
	req := feedRequest(t, run, provisionRequest(t))
	checkouts, stale := 0, 0
	for _, round := range []string{"a fresh workspace", "a workspace that already exists"} {
		before := len(run.Calls())
		if _, err := Provision(context.Background(), run, req); err != nil {
			t.Fatalf("provision %s: %v", round, err)
		}
		for _, call := range run.Calls()[before:] {
			// The shared clone's own size-following steps carry what remains of CloneTimeout, and
			// are TestTheSharedClonesCloneIsBoundedByCloneTimeout's: they are recognised here the
			// way that case recognises them, and left to it.
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
// Every other command of the provision carries that budget, which a clock counts from each
// command's start, and a loaded machine can hold one of them that long: the provision then fails
// on that command's timeout and says nothing of the checkout's bound. Only a run in which each of
// them took less than the budget is judged (budgetSpans); a run of the other order provisions
// again, from a remote and a state of its own, until a run of the right order comes, within a
// minute. A run whose checkout took less than the budget proves nothing and fails at once.
func TestProvisionChecksOutPastTheRunnersBudget(t *testing.T) {
	within := testwait.Bounded(t, time.Minute, checkoutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		run := slowCheckoutRunner(t, checkoutDelay, checkoutBudget)
		spans := &budgetSpans{runner: run.runner}
		run.runner = spans
		pushRemoteBranch(t, run.remote, "main", "history.txt")
		working, err := Provision(context.Background(), run, provisionRequest(t))
		checkout, longest, argv := spans.spans()

		if longest >= checkoutBudget {
			if time.Now().Before(until) {
				t.Logf("run %d is not judged: %q took %s, as long as the runner's %s budget, which may have ended it (Provision = %v)", runs, argv, longest, checkoutBudget, err)
				continue
			}
			t.Fatalf("no run of the right order in %d runs over %s: in each, a command under the runner's %s budget took that long (in the last, %q took %s; Provision = %v)",
				runs, within, checkoutBudget, argv, longest, err)
		}
		if err != nil {
			t.Fatalf("run %d: Provision: %v", runs, err)
		}
		if checkout < checkoutBudget {
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
// ends the checkout there. The runner's own budget, under every other command of the provision,
// is the rest of the test binary's deadline: far from where any of them ends on a loaded machine,
// and far past the checkout's delay, so a checkout given that budget in place of its bound would
// finish, and the provision would succeed.
func TestProvisionFailsWhenTheCheckoutOutlivesItsBound(t *testing.T) {
	run := slowCheckoutRunner(t, checkoutDelay, testwait.Remaining(t))
	req := provisionRequest(t)
	req.CheckoutBound = time.Second
	working, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Provision(context.Background(), run, req)
	want := "check out workspace " + working.Dir + ": command timed out: jj sparse reset"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Provision whose checkout outlived its one-second bound = %v, want an error holding %q", err, want)
	}
}
