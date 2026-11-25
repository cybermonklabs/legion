package supervise

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// recordedHead is the head of the issue's pull request as a test's record holds it, which the
// machine reads through Deps.ReviewHead: a test moves it, and the machine's goroutines read it.
type recordedHead struct {
	mu   sync.Mutex
	head string
	err  error
	read int
}

func (r *recordedHead) set(head string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head = head
}

func (r *recordedHead) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *recordedHead) reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read
}

func (r *recordedHead) answer(context.Context, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.read++
	return r.head, r.err
}

// newReviewerHarness is a machine over a queued reviewer's claim whose record holds head, and
// whose launches give the reviewer head (testSpecs.setReviewHead) until the test moves both.
func newReviewerHarness(t *testing.T, head string) (*harness, *recordedHead) {
	t.Helper()
	h := newBareHarness(t)
	recorded := &recordedHead{head: head}
	h.deps.ReviewHead = recorded.answer
	h.specs.setReviewHead(head)
	c := queuedClaim()
	c.Token, c.Role = "legion-LEGION-209-reviewer", claim.RoleReviewer
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, recorded
}

// A reviewer works only in a clone of its own made at the head its launch read (the daemon's
// spawn spec, runtime.SpawnSpec.ReviewHead: the daemon's host makes the clone there on tmux, and
// the pod's role-init does in a pod), and nothing moves that clone while its process runs. So a
// task the reviewer is handed once the head its record holds has moved is not sent to that
// process: the process is replaced first (a relaunch of the same session), and the task goes to
// the process launched at the record's head once it is ready. A task at the head the process was
// launched at is sent at once, with no relaunch.
func TestAReviewersTaskAtAnotherHeadGoesToAProcessLaunchedAtIt(t *testing.T) {
	h, recorded := newReviewerHarness(t, "head-1")
	h.reach(StateIdle)
	h.wantPrompts(1)
	if resumes := h.calls("Resume"); len(resumes) != 0 {
		t.Fatalf("a task at the head the process was launched at relaunched it: %+v", resumes)
	}

	// The implementer pushes, and the reviewer's next round is handed to it.
	recorded.set("head-2")
	h.specs.setReviewHead("head-2")
	h.must(RequestDeliver{Claim: h.token, Task: "review round two"})
	h.m.Wait()
	h.wantPrompts(1)
	resumes := h.wantCalls("Resume", 1)
	if got := resumes[0].Spec.ReviewHead; got != "head-2" {
		t.Fatalf("the relaunch gave the reviewer head %q, want head-2", got)
	}
	h.wantState(StateLaunching)
	if p := h.pending(); p.Task != "review round two" || !p.DeliveredAt.IsZero() {
		t.Fatalf("the pending delivery is %+v, want the round's task, unsent", p)
	}

	h.relaunched()
	h.m.Wait()
	prompts := h.wantPrompts(2)
	if !strings.Contains(prompts[1].Message, "review round two") {
		t.Fatalf("the relaunched reviewer was sent %q, want the round's task", prompts[1].Message)
	}
	h.wantCalls("Resume", 1)
}

// The head a process was launched at is the claim's own record (Claim.ReviewLaunchHead), which
// the store keeps, so a machine a daemon restart rebuilt hands its live reviewer the next task in
// the process it found, where the record's head has not moved, and relaunches it where it has.
// A row that records no launch head for the process the claim records — one a release before this
// one launched — sends the reviewer's next task to a process launched at the record's head, and so
// does a row whose launch head is the record's but whose launch generation is not the claim's: a
// release before this one started the process the claim records, and left the columns this
// release wrote for the process before it. A record that holds no head (an issue with no pull
// request) holds no task back, and a record that cannot be read holds the task back, unsent and
// with no relaunch, until it answers.
func TestAReviewersTaskWaitsForAProcessWhoseHeadTheMachineKnows(t *testing.T) {
	t.Run("after a restart, at the same head", func(t *testing.T) {
		h, _ := newReviewerHarness(t, "head-1")
		h.reach(StateIdle)
		h.restart()
		h.must(RequestDeliver{Claim: h.token, Task: "review round two"})
		h.m.Wait()
		h.wantPrompts(2)
		h.wantCalls("Resume", 0)
	})
	t.Run("after a restart, at a head that moved", func(t *testing.T) {
		h, recorded := newReviewerHarness(t, "head-1")
		h.reach(StateIdle)
		h.restart()
		recorded.set("head-2")
		h.specs.setReviewHead("head-2")
		h.must(RequestDeliver{Claim: h.token, Task: "review round two"})
		h.m.Wait()
		h.wantPrompts(1)
		if got := h.wantCalls("Resume", 1)[0].Spec.ReviewHead; got != "head-2" {
			t.Fatalf("the relaunch gave the reviewer head %q, want head-2", got)
		}
		h.relaunched()
		h.m.Wait()
		h.wantPrompts(2)
	})
	t.Run("a row an earlier release wrote", func(t *testing.T) {
		h, _ := newReviewerHarness(t, "head-1")
		h.reach(StateIdle)
		h.m.Wait()
		earlier := h.store.load(h.token)
		earlier.ReviewLaunchHead, earlier.ReviewLaunchGeneration = "", 0
		if err := h.store.PutClaim(h.ctx, earlier); err != nil {
			t.Fatal(err)
		}
		h.restart()
		h.must(RequestDeliver{Claim: h.token, Task: "review round two"})
		h.m.Wait()
		h.wantPrompts(1)
		if got := h.wantCalls("Resume", 1)[0].Spec.ReviewHead; got != "head-1" {
			t.Fatalf("the relaunch gave the reviewer head %q, want head-1", got)
		}
		h.relaunched()
		h.m.Wait()
		h.wantPrompts(2)
	})
	t.Run("a process an earlier release started over this release's head", func(t *testing.T) {
		// This release launched the reviewer at head-1 and recorded that launch. A release before
		// this one then started the claim's process again, at the claim's next generation: it
		// reads and writes the row and leaves both columns, so the row still names head-1, at a
		// generation that is not the claim's. That launch made no clone at head-1.
		h, _ := newReviewerHarness(t, "head-1")
		h.reach(StateIdle)
		h.m.Wait()
		rolledBack := h.store.load(h.token)
		if rolledBack.ReviewLaunchHead != "head-1" || rolledBack.ReviewLaunchGeneration != rolledBack.Generation {
			t.Fatalf("the launch recorded head %q at generation %d, want head-1 at the claim's generation %d",
				rolledBack.ReviewLaunchHead, rolledBack.ReviewLaunchGeneration, rolledBack.Generation)
		}
		rolledBack.Generation++
		if err := h.store.PutClaim(h.ctx, rolledBack); err != nil {
			t.Fatal(err)
		}
		h.restart()
		h.must(RequestDeliver{Claim: h.token, Task: "review round two"})
		h.m.Wait()
		h.wantPrompts(1)
		if got := h.wantCalls("Resume", 1)[0].Spec.ReviewHead; got != "head-1" {
			t.Fatalf("the relaunch gave the reviewer head %q, want head-1", got)
		}
		h.relaunched()
		h.m.Wait()
		h.wantPrompts(2)
	})
	t.Run("no head recorded", func(t *testing.T) {
		h, _ := newReviewerHarness(t, "")
		h.reach(StateIdle)
		h.restart()
		h.must(RequestDeliver{Claim: h.token, Task: "review round two"})
		h.m.Wait()
		h.wantPrompts(2)
		h.wantCalls("Resume", 0)
	})
	t.Run("the record does not answer", func(t *testing.T) {
		h, recorded := newReviewerHarness(t, "head-1")
		h.reach(StateIdle)
		recorded.fail(errors.New("the store is down"))
		h.must(RequestDeliver{Claim: h.token, Task: "review round two"})
		h.m.Wait()
		h.wantPrompts(1)
		h.wantCalls("Resume", 0)
		h.wantState(StateIdle)
		recorded.fail(nil)
		h.must(IssueTurnEnded{Claim: h.token})
		h.m.Wait()
		h.wantPrompts(2)
	})
	t.Run("a relaunch whose launch still gives another head is raised once", func(t *testing.T) {
		h, recorded := newReviewerHarness(t, "")
		h.reach(StateIdle)
		recorded.set("head-2")
		h.must(RequestDeliver{Claim: h.token, Task: "review round two"})
		h.m.Wait()
		h.wantCalls("Resume", 1)
		h.relaunched()
		h.m.Wait()
		h.wantPrompts(1)
		h.wantCalls("Resume", 1)
		if lines := h.logs.lines("its launch gave it another", "head=head-2"); len(lines) != 1 {
			t.Fatalf("the refusal logged %q, want one line naming head-2", lines)
		}
		// The record moves on, and the spec builder now gives the reviewer that head.
		recorded.set("head-3")
		h.specs.setReviewHead("head-3")
		h.must(IssueTurnEnded{Claim: h.token})
		h.m.Wait()
		h.wantCalls("Resume", 2)
		h.relaunched()
		h.m.Wait()
		h.wantPrompts(2)
	})
}

// podGate is the daemon's rule for a reviewer whose issue pod runs another image than the
// configured one (internal/daemon's reviewerPods), as a test drives it: while the pod runs
// another image, the reviewer's task waits (Deps.DeliveryWaits) and each launch's recheck refuses
// the reviewer's start (internal/daemon's reviewerGatedStore). checked counts those rechecks.
type podGate struct {
	*memStore
	gate    sync.Mutex
	other   bool
	checked int
}

func (g *podGate) CheckLaunch(ctx context.Context, c Claim) error {
	g.gate.Lock()
	defer g.gate.Unlock()
	g.checked++
	if g.other {
		return errors.New("the reviewer starts only in a pod on the configured image: the pod's update round replaces it first")
	}
	return g.memStore.CheckLaunch(ctx, c)
}

func (g *podGate) holdsTask(Claim) bool {
	g.gate.Lock()
	defer g.gate.Unlock()
	return g.other
}

// replaced is the pod's update round replacing the pod with one on the configured image.
func (g *podGate) replaced() {
	g.gate.Lock()
	defer g.gate.Unlock()
	g.other = false
}

func (g *podGate) checks() int {
	g.gate.Lock()
	defer g.gate.Unlock()
	return g.checked
}

// A message the daemon holds for a reviewer (ReviewHeadHolds) waits, as the reviewer's task does,
// while the reviewer's issue pod runs another image than the configured one: the reviewer is
// handed nothing there, and its launch into that pod is refused. So the message raises no
// relaunch at the record's head while it waits. A relaunch raised there would be refused at each
// attempt of the outbox, with one line for the raise and one for the refusal each time. Once the
// pod's round has replaced the pod, the next attempt raises the relaunch.
func TestAMessageHeldForAReviewerWhosePodWaitsRaisesNoRelaunch(t *testing.T) {
	h, recorded := newReviewerHarness(t, "head-1")
	h.reach(StateIdle)
	pod := &podGate{memStore: h.store, other: true}
	h.m.deps.Store = pod
	h.m.deps.DeliveryWaits = pod.holdsTask
	recorded.set("head-2")
	h.specs.setReviewHead("head-2")

	for attempt := 1; attempt <= 3; attempt++ {
		holds, err := h.m.ReviewHeadHolds(h.ctx)
		h.m.Wait()
		if err != nil || holds {
			t.Fatalf("attempt %d: ReviewHeadHolds = %t, %v; want the message held, with no error", attempt, holds, err)
		}
	}
	if n := pod.checks(); n != 0 {
		t.Errorf("the pod's gate was asked %d times, want no launch while the pod waits", n)
	}
	if lines := h.logs.lines("started again at the record's head"); len(lines) != 0 {
		t.Errorf("a relaunch was raised while the pod waits: %q", lines)
	}
	if lines := h.logs.lines("the relaunch at the reviewer's head"); len(lines) != 0 {
		t.Errorf("a refused relaunch was logged while the pod waits: %q", lines)
	}
	h.wantCalls("Resume", 0)
	h.wantState(StateIdle)

	pod.replaced()
	if holds, err := h.m.ReviewHeadHolds(h.ctx); err != nil || holds {
		t.Fatalf("ReviewHeadHolds = %t, %v; want the message held while the relaunch runs", holds, err)
	}
	h.m.Wait()
	if got := h.wantCalls("Resume", 1)[0].Spec.ReviewHead; got != "head-2" {
		t.Fatalf("the relaunch gave the reviewer head %q, want head-2", got)
	}
}

// Only a reviewer's task asks for the head: every other role works the issue's shared workspace,
// which every push reaches, and its task reads no record of a head.
func TestOnlyAReviewersTaskReadsTheHead(t *testing.T) {
	h := newBareHarness(t)
	recorded := &recordedHead{head: "head-1"}
	h.deps.ReviewHead = recorded.answer
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateIdle)
	recorded.set("head-2")
	h.must(RequestDeliver{Claim: h.token, Task: "implement the review's findings"})
	h.m.Wait()
	h.wantPrompts(2)
	h.wantCalls("Resume", 0)
	if n := recorded.reads(); n != 0 {
		t.Fatalf("the implementer's sends read the head %d times, want none", n)
	}
}
