package supervise

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// nextChangeAt is where the first task of a next change of the implementer's issue starts the
// working copy: the issue's branch, which the daemon moved to the head of main.
var nextChangeAt = runtime.StartAt{Bookmark: "legion/LEGION-209", Base: "main"}

// movedTo is what the shim answers a start that moved the working copy from the merged head O to
// main's head M.
var movedTo = runtime.StartedAt{Moved: true, From: strings.Repeat("0", 39) + "1", At: strings.Repeat("0", 39) + "2"}

// nextChangeTask is the first task of a next change, carrying its start at the branch.
func nextChangeTask() RequestDeliver {
	return RequestDeliver{Claim: testToken, Task: "implement the next change", Phase: phase.Implementing, Generation: 6,
		StartAt: nextChangeAt}
}

// startRefusal is the shim refusing the start `ok: false`, as that answer reaches the machine from
// the stream: an error that is runtime.ErrStartAtRefused and carries the shim's own words.
func startRefusal(reason string) error {
	return fmt.Errorf("worker-stream: %s refused start-at-branch: %s: %w", testToken, reason, runtime.ErrStartAtRefused)
}

// restoredOwedStart is a daemon restarted over a ready claim that holds the first task of a next
// change whose start of the working copy at the branch no earlier daemon answered
// (Delivery.StartAt, Started nil). It carries no news. The restart arms its question over the task
// (askFirst), as over every unconfirmed task it restores. The agent's connection is registered, and
// the agent is in a turn of its own, an Envoy message's, so it answers a prompt busy.
func restoredOwedStart(t *testing.T) *harness {
	t.Helper()
	restored := queuedClaim()
	restored.State, restored.Generation = StateReady, 3
	restored.Session, restored.SessionFile = session, sessionFile
	restored.Locator = &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: testToken, Incarnation: "4242:77",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
	restored.Pending = &Delivery{ID: "outbox:47", Task: "implement the next change", Phase: phase.Implementing, Generation: 6,
		QueuedAt: epoch, StartAt: StartAt{StartAt: nextChangeAt}}
	h := newBareHarness(t)
	if err := h.store.PutClaim(h.ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	h.conns.Register(testToken, h.conn)
	h.conn.SetStreaming(true)
	h.conn.RefusePrompt(busyAnswer)
	h.rt.AnswerStartAtBranch(movedTo)
	h.start(restored)
	return h
}

// After a restart, a next change's first task whose working copy no start has moved is never
// confirmed by a turn the agent started by itself. No earlier daemon's send of it got past its
// start, so the agent never read it, and its confirmation would retire a task nobody sent over a
// working copy still at the previous change. So the question the restart left (askFirst) never
// takes the agent's turn for the task's, at either place it could: the send the ready makes, where
// the agent says it is in a turn, and a turn's start seen before anything was sent. Nor does the
// start run under that turn, which it would change the files of. The task stays unconfirmed and
// unstarted, and the end of the agent's own turn starts the working copy and sends the task behind
// what moved, and that send's own turn confirms it.
func TestARestartNeverConfirmsAFirstTaskWhoseStartIsOwedByATurnOfTheAgentsOwn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first Event
	}{
		{name: "the ready's send finds the agent in a turn of its own", first: RequestReady{Claim: testToken, Generation: 3, Session: session}},
		{name: "a turn of the agent's own starts before anything is sent", first: StreamTurnStart{Claim: testToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := restoredOwedStart(t)

			h.must(tc.first)

			if stored, ok := h.store.delivery(testToken); !ok || !stored.ConfirmedAt.IsZero() {
				t.Fatalf("stored delivery after %s = %+v (held %t), want outbox:47 unconfirmed: no start and no prompt of it ran", tc.name, stored, ok)
			}
			h.wantCalls("StartAtBranch", 0)
			h.wantPrompts(0)
			h.wantState(StateWorking)

			h.conn.FailPrompt(nil)
			h.conn.SetStreaming(false)
			h.mustQuiet(StreamTurnEnd{Claim: testToken})

			h.wantCalls("StartAtBranch", 1)
			sent := h.wantPrompts(1)[0]
			if want := startedNote(nextChangeAt, movedTo) + "implement the next change"; sent.DeliveryID != h.pending().ID || sent.Message != want {
				t.Fatalf("the send at the end of the agent's own turn = %+v, want %q under its pending id", sent, want)
			}
			h.must(StreamTurnStart{Claim: testToken})
			if stored, ok := h.store.delivery(testToken); !ok || stored.ConfirmedAt.IsZero() {
				t.Fatalf("stored delivery after the turn its send started = %+v (held %t), want it confirmed", stored, ok)
			}
		})
	}
}

// identityHarness is a ready implementer's claim whose daemon mints the role's App identity, so a
// phase task adopts the working copy, with deps changed by more before the machine is built.
func identityHarness(t *testing.T, more func(*Deps)) *harness {
	t.Helper()
	h := newHarness(t)
	h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}, nil
	}
	if more != nil {
		more(&h.deps)
	}
	h.restart()
	h.reach(StateReady)
	return h
}

// The first task of a next change starts the working copy at the moved branch before anything
// else of its send: before the adoption, and before the prompt, which goes out behind what the
// start did. The start's answer stays with the delivery, written to the store, so a later send of
// the task moves nothing again and still says what moved. A refusal holds the task, charged
// nothing, tells the owning architect once with the shim's words, and is tried again at the next
// sweep. A delivery with no start makes no such call.
func TestANextChangesFirstTaskStartsTheWorkingCopyAtTheBranchFirst(t *testing.T) {
	t.Run("a start the shim answers", func(t *testing.T) {
		h := identityHarness(t, nil)
		h.rt.AnswerStartAtBranch(movedTo)

		h.must(nextChangeTask())

		starts := h.wantCalls("StartAtBranch", 1)
		if got := starts[0].Locator; starts[0].StartAt != nextChangeAt || got.Claim != testToken || got.Incarnation != h.locator().Incarnation {
			t.Fatalf("the start was asked as %+v, want %+v at the claim's own process", starts[0], nextChangeAt)
		}
		methods := h.rt.Methods()
		if start, adopt := slices.Index(methods, "StartAtBranch"), slices.Index(methods, "AdoptWorkingCopy"); adopt < 0 || start > adopt {
			t.Fatalf("the runtime was asked %v, want the start before the adoption", methods)
		}
		if sent := h.wantPrompts(1)[0]; sent.Message != startedNote(nextChangeAt, movedTo)+"implement the next change" {
			t.Fatalf("sent %q, want the task behind what the start did", sent.Message)
		}
		kept := h.pending().StartAt
		if kept.Started == nil || *kept.Started != movedTo {
			t.Fatalf("the delivery keeps the start as %+v, want the shim's answer %+v", kept, movedTo)
		}
		if stored := h.store.load(h.token).Pending; stored == nil || stored.StartAt.Started == nil || *stored.StartAt.Started != movedTo {
			t.Fatalf("the store holds the delivery as %+v, want the start's answer written with it", stored)
		}
	})

	t.Run("a start the shim refuses", func(t *testing.T) {
		type told struct {
			delivery, reason string
		}
		var tellings []told
		h := identityHarness(t, func(deps *Deps) {
			deps.StartAtRefused = func(_ Claim, d Delivery, reason string) { tellings = append(tellings, told{d.ID, reason}) }
		})
		const conflict = "Bookmark legion/LEGION-209 is conflicted. The working copy did not move."
		h.rt.FailStartAtBranch(startRefusal(conflict))

		h.must(nextChangeTask())
		h.observe(runtime.Alive)

		h.wantCalls("StartAtBranch", 2)
		h.wantCalls("AdoptWorkingCopy", 0)
		h.wantPrompts(0)
		h.wantBudgets(Budgets{})
		if len(tellings) != 1 || tellings[0].delivery != h.pending().ID || !strings.Contains(tellings[0].reason, conflict) {
			t.Fatalf("told %+v, want the owning architect told once, of this task, with the shim's words", tellings)
		}
		if stored := h.store.load(h.token).Pending; stored == nil || !stored.StartAt.Told || stored.StartAt.Started != nil {
			t.Fatalf("the store holds the delivery as %+v, want its telling written and no start", stored)
		}

		h.rt.FailStartAtBranch(nil)
		h.rt.AnswerStartAtBranch(movedTo)
		h.observe(runtime.Alive)
		h.wantCalls("StartAtBranch", 3)
		if sent := h.wantPrompts(1)[0]; sent.Message != startedNote(nextChangeAt, movedTo)+"implement the next change" {
			t.Fatalf("sent %q, want the task behind what the start did once it went through", sent.Message)
		}
		if len(tellings) != 1 {
			t.Fatalf("told %+v, want the one telling of the refusal", tellings)
		}
	})

	t.Run("a delivery with no start", func(t *testing.T) {
		h := identityHarness(t, nil)
		h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 5})
		h.wantPrompts(1)
		h.wantCalls("AdoptWorkingCopy", 1)
		h.wantCalls("StartAtBranch", 0)
	})
}

// A start the shim answered is kept, so the task's next send, after a prompt the transport lost,
// moves nothing again and still begins with what moved.
func TestStartAtBranchIsMadeOnceForATaskSentAgain(t *testing.T) {
	h := identityHarness(t, nil)
	h.rt.AnswerStartAtBranch(movedTo)
	h.conn.FailPrompt(errBoom)
	h.must(nextChangeTask())
	h.wantCalls("StartAtBranch", 1)
	h.wantPrompts(1)

	h.conn.FailPrompt(nil)
	h.observe(runtime.Alive)
	h.wantCalls("StartAtBranch", 1)
	prompts := h.wantPrompts(2)
	if prompts[1].Message != startedNote(nextChangeAt, movedTo)+"implement the next change" {
		t.Fatalf("sent %q again, want the task behind what the start did, with no second start", prompts[1].Message)
	}
}

// Every role of an issue works one jj workspace, so the start of the working copy at the branch
// waits while another claim of the issue is in a turn, as an adoption does: the architect's turn
// holds the implementer's first task of the next change, and its end lets the start and the task
// go, once.
func TestStartAtBranchWaitsForAnotherClaimsTurn(t *testing.T) {
	architectInTurn := true
	h := identityHarness(t, func(deps *Deps) {
		deps.IssueInTurn = func(issue string, except claim.Token) (claim.Token, bool) {
			if except != testToken || !architectInTurn {
				return "", false
			}
			return rootToken, true
		}
	})
	h.rt.AnswerStartAtBranch(movedTo)

	h.must(nextChangeTask())
	h.wantCalls("StartAtBranch", 0)
	h.wantPrompts(0)

	architectInTurn = false
	h.must(IssueTurnEnded{Claim: testToken})
	h.wantCalls("StartAtBranch", 1)
	h.wantPrompts(1)

	h.must(IssueTurnEnded{Claim: testToken})
	h.observe(runtime.Alive)
	h.wantCalls("StartAtBranch", 1)
	h.wantPrompts(1)
}

// A daemon with no GitHub Apps adopts nothing (adopts), but the branch moved all the same, so the
// start is still made before the send, and the task goes behind it.
func TestStartAtBranchRunsWhereNoAppAdopts(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.rt.AnswerStartAtBranch(movedTo)

	h.must(nextChangeTask())

	h.wantCalls("StartAtBranch", 1)
	h.wantCalls("AdoptWorkingCopy", 0)
	if sent := h.wantPrompts(1)[0]; sent.Message != startedNote(nextChangeAt, movedTo)+"implement the next change" {
		t.Fatalf("sent %q, want the task behind what the start did", sent.Message)
	}
}
