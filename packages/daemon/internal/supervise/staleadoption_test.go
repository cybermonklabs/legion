package supervise

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// staleEditsCommit is a commit the shim names when bringing a stale working copy up to date left
// the edits that no jj command had recorded in it, and laterEditsCommit one a later adoption names.
const (
	staleEditsCommit = "c45ff7e33d4899a2b99db849241fe128115021b0"
	laterEditsCommit = "9b1e0d6f2a7c4e3b8d5f1a0c6e2b7d4f9a3c8e1b"
)

// staleAdoptionLine is the line the machine writes when it records such a commit and sends the
// task naming it, as an operator reads it.
const staleAdoptionLine = "supervise: the adoption brought a stale working copy up to date and left the edits no jj command had recorded in a commit; the task names it"

// namesTheEdits is whether a prompt names the commit that holds the edits a stale working copy's
// update left and says they are there and no longer on disk, as the role reads it.
func namesTheEdits(message string) bool {
	return strings.Contains(message, "Before this task, the working copy was stale.") &&
		strings.Contains(message, "left the edits that no jj command had recorded in commit "+staleEditsCommit) &&
		strings.Contains(message, "they are no longer on disk") &&
		strings.Contains(message, "jj show "+staleEditsCommit)
}

// wantLeftEdits fails unless the claim records commits as the commits a stale working copy's
// update left the edits in, told by the confirmation of delivery by, or untold where by is "", as
// the machine holds it and as the store holds it. No commits is a claim that records none.
func (h *harness) wantLeftEdits(commits []string, by, when string) {
	h.t.Helper()
	for where, got := range map[string]LeftEdits{"the machine": h.claim().LeftEdits, "the store": h.store.load(h.token).LeftEdits} {
		if !slices.Equal(got.Commits, commits) || got.Told != (by != "") || got.ToldBy != by {
			h.t.Fatalf("%s: %s records %+v, want the commits %v told=%v by %q", when, where, got, commits, by != "", by)
		}
	}
}

// phaseOwnerOf is an adopting harness whose issue holds every phase task of run and of any later
// run (Deps.PhaseHolds), so a test that raises run has the next send drop a task of an earlier one.
func phaseOwnerOf(t *testing.T, run *uint64) *harness {
	t.Helper()
	h := newHarness(t)
	h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}, nil
	}
	h.deps.PhaseHolds = func(_ context.Context, _ string, _ phase.Phase, generation uint64) (bool, error) {
		return generation >= *run, nil
	}
	h.restart()
	h.reach(StateReady)
	return h
}

// The prompt of a phase task is fixed before its adoption runs, and an adoption that brought a
// stale working copy up to date names the commit in which the update left the edits that no jj
// command had recorded (runtime.Adoption). The send then prompts with no text it fixed before: the
// machine records the commit on the claim (LeftEdits), moves the pending task to a new id so the
// shim's dedupe by id forwards the new text, persists both, logs it, and sends the task at once
// with the sentence that names the commit after its text. The sentence is composed from the
// claim's record at every send until a turn takes a prompt that carried it, so every later send
// carries it — after a refused prompt, after a daemon restart — though each later adoption, of a
// working copy up to date by then, names no commit. A refusal that names a commit is recorded on
// the claim the same way. An adoption that names none leaves the claim, the task and its id as
// they were.
func TestAStaleAdoptionsCommitIsNamedInTheTaskItsRoleGets(t *testing.T) {
	task := RequestDeliver{Claim: testToken, ID: "outbox:41", Task: "implement the plan", Phase: phase.Implementing, Generation: 1}
	names := runtime.Adoption{EditsCommits: []string{staleEditsCommit}}
	named := task.Task + "\n\n" + staleEditsSentence([]string{staleEditsCommit})
	// wantNamed fails unless the pending task is the task as it was queued, under a new id, as the
	// store holds it, and the claim records the commit untold, and answers the task.
	wantNamed := func(t *testing.T, h *harness) Delivery {
		t.Helper()
		p := h.pending()
		stored, ok := h.store.delivery(testToken)
		if p.ID == task.ID || p.Task != task.Task || !ok || stored.ID != p.ID || stored.Task != p.Task {
			t.Fatalf("the pending task is %+v, stored %+v (%v); want the task as it was queued, persisted under a new id", p, stored, ok)
		}
		h.wantLeftEdits([]string{staleEditsCommit}, "", "after the adoption that named it")
		if lines := h.records.of(staleAdoptionLine); len(lines) != 1 {
			t.Fatalf("%q was written %d times, want once", staleAdoptionLine, len(lines))
		}
		return p
	}

	t.Run("an adoption that names a commit sends the task naming it, under a new id", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.AnswerAdoptWorkingCopy(names)
		h.must(task)
		sent := h.wantPrompts(1)[0]
		p := wantNamed(t, h)
		if sent.DeliveryID != p.ID || sent.Message != named {
			t.Fatalf("the agent was sent %+v, want %q under the task's new id %s", sent, named, p.ID)
		}
		h.wantBudgets(Budgets{})
	})

	t.Run("an adoption that names none sends the task as it was queued", func(t *testing.T) {
		h := adoptingHarness(t)
		h.must(task)
		sent := h.wantPrompts(1)[0]
		if sent.DeliveryID != task.ID || sent.Message != task.Task || namesTheEdits(sent.Message) {
			t.Fatalf("the agent was sent %+v, want the task %q as it was queued, under its id %s", sent, task.Task, task.ID)
		}
		if p := h.pending(); p.ID != task.ID || p.Task != task.Task {
			t.Fatalf("the pending task is %+v, want it as it was queued", p)
		}
		h.wantLeftEdits(nil, "", "after an adoption that named no commit")
		if lines := h.records.of(staleAdoptionLine); len(lines) != 0 {
			t.Fatalf("%q was written %d times, want never", staleAdoptionLine, len(lines))
		}
	})

	t.Run("a refused prompt's task is sent again naming the commit, whose next adoption names none", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.AnswerAdoptWorkingCopy(names)
		h.conn.RefusePrompt("the model provider refused the request")
		h.must(task)
		if refused := h.wantPrompts(1)[0]; !namesTheEdits(refused.Message) {
			t.Fatalf("the refused prompt was %q, want the task naming %s", refused.Message, staleEditsCommit)
		}
		wantNamed(t, h)

		h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{})
		h.conn.FailPrompt(nil)
		h.observe(runtime.Alive) // the sweep's re-send
		if again := h.wantPrompts(2)[1]; !namesTheEdits(again.Message) {
			t.Fatalf("the re-send was %q, want the task still naming %s", again.Message, staleEditsCommit)
		}
	})

	t.Run("after a daemon restart the task is sent naming the commit", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.AnswerAdoptWorkingCopy(names)
		h.conn.FailPrompt(errBoom) // the prompt is lost on the way, so the task waits unsent
		h.must(task)
		wantNamed(t, h)

		h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{})
		h.conn.FailPrompt(nil)
		h.restart()
		h.observe(runtime.Alive)
		prompts := h.prompts()
		if len(prompts) != 2 || !namesTheEdits(prompts[1].Message) {
			t.Fatalf("the prompts after the restart are %+v, want the task sent again naming %s", prompts, staleEditsCommit)
		}
	})

	t.Run("a refused adoption that names a commit is recorded on the claim, and the next prompt carries it", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.AnswerAdoptWorkingCopy(names)
		h.rt.FailAdoptWorkingCopy(adoptRefusal("Command failed (exit 1): jj metaedit"))
		h.must(task)
		h.wantPrompts(0)
		if p := wantNamed(t, h); p.SendFailures != 1 {
			t.Fatalf("the pending task is %+v, want the refused adoption counted on it", p)
		}
		if lines := h.records.of(adoptRefusedLine); len(lines) != 1 {
			t.Fatalf("%q was written %d times, want once", adoptRefusedLine, len(lines))
		}

		h.rt.FailAdoptWorkingCopy(nil)
		h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{})
		h.observe(runtime.Alive) // the sweep's re-send
		if sent := h.wantPrompts(1)[0]; !namesTheEdits(sent.Message) || sent.DeliveryID != h.pending().ID {
			t.Fatalf("the agent was sent %+v, want the task naming %s", sent, staleEditsCommit)
		}
	})
}

// The architect's decision on INFRA-3916 (dispatch://INFRA-3916/comment/12605fd9-4887-4404-8a1d-91ba4fa2dda3):
// the role is told where a stale adoption left its edits in the next task it takes, whatever
// became of the task whose adoption left them. That task's adoption names the commit, and no
// prompt of it reaches the agent — the prompt is lost on the way — before the task is gone: the
// run moves on and the sweep's send drops it (sendPending), the workflow stops its phase
// (RetireStoppedTask), or the run moves on across a daemon restart, whose machine is built anew
// from the store. The claim is given the task of the next run, whose own adoption names no
// commit, and the prompt the agent gets names the commit and says the edits are there. Once a
// turn takes that prompt, the task after it names nothing. With the sentence held on the task
// alone, it went with the task, and the next task named no commit.
func TestAStaleAdoptionsCommitIsNamedInTheNextTaskWhenItsOwnNeverReachesTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		// drop takes the task of run 1 from the claim, unsent: it may raise the run the issue is
		// in.
		drop func(h *harness, run *uint64)
	}{
		{name: "the run moves on, and the sweep's send drops the task", drop: func(h *harness, run *uint64) {
			*run = 2
			h.observe(runtime.Alive)
		}},
		{name: "the workflow stops the task's phase", drop: func(h *harness, _ *uint64) {
			if retired, err := h.m.RetireStoppedTask(h.ctx, phase.Implementing, 1, 0); err != nil || !retired {
				h.t.Fatalf("the retirement of the stopped phase's task = %t, %v; want it retired", retired, err)
			}
		}},
		{name: "the run moves on across a daemon restart, and the sweep's send drops the task", drop: func(h *harness, run *uint64) {
			*run = 2
			h.restart()
			h.observe(runtime.Alive)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := uint64(1)
			h := phaseOwnerOf(t, &run)
			h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{EditsCommits: []string{staleEditsCommit}})
			h.conn.FailPrompt(errBoom) // the prompt is lost on the way: none of the task reaches the agent
			h.must(RequestDeliver{Claim: testToken, ID: "outbox:41", Task: "implement the plan", Phase: phase.Implementing, Generation: 1})
			h.wantPrompts(1)
			tc.drop(h, &run)
			if p := h.claim().Pending; p != nil {
				t.Fatalf("the claim holds %+v, want the task of run 1 gone, unsent", p)
			}

			h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{})
			h.conn.FailPrompt(nil)
			next := RequestDeliver{Claim: testToken, ID: "outbox:42", Task: "implement the plan of run 2", Phase: phase.Implementing, Generation: 2}
			h.must(next)
			sent := h.wantPrompts(2)[1]
			if sent.DeliveryID != next.ID || !strings.HasPrefix(sent.Message, next.Task) || !namesTheEdits(sent.Message) {
				t.Fatalf("the agent was sent %+v, want the task of run 2 under its id %s, naming %s", sent, next.ID, staleEditsCommit)
			}
			h.must(StreamTurnStart{Claim: testToken})
			h.must(StreamTurnEnd{Claim: testToken})

			after := RequestDeliver{Claim: testToken, ID: "outbox:43", Task: "address the review", Phase: phase.Implementing, Generation: 2}
			h.must(after)
			if last := h.wantPrompts(3)[2]; last.DeliveryID != after.ID || last.Message != after.Task {
				t.Fatalf("the task after the turn that took the commit's prompt was sent as %+v, want %q alone under %s", last, after.Task, after.ID)
			}
		})
	}
}

// The commits a stale working copy's update left the edits in are the claim's record
// (Claim.LeftEdits), told at the confirmation of a prompt that carried their sentence and by
// nothing else (confirmCarrying). The record outlives the task whose adoption left them across a
// daemon restart, whose machine reads it back from the store, and the turn of the next task tells
// it. A later adoption that leaves more before it is told adds its commit after the first, and
// the next prompt names both. A confirmation taken back takes the telling back with it
// (moveTelling), so the next prompt names the commit again, and the turn that takes that prompt
// tells it.
func TestAStaleAdoptionsCommitsAreTheClaimsUntilATurnTakesAPromptThatNamesThem(t *testing.T) {
	task := RequestDeliver{Claim: testToken, ID: "outbox:41", Task: "implement the plan", Phase: phase.Implementing, Generation: 1}

	t.Run("the record survives a daemon restart after its task is dropped, and the next task's turn tells it", func(t *testing.T) {
		run := uint64(1)
		h := phaseOwnerOf(t, &run)
		h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{EditsCommits: []string{staleEditsCommit}})
		h.conn.FailPrompt(errBoom)
		h.must(task)
		h.wantLeftEdits([]string{staleEditsCommit}, "", "after the adoption that named it")

		run = 2
		h.restart()
		h.wantLeftEdits([]string{staleEditsCommit}, "", "after the restart")
		h.observe(runtime.Alive) // the sweep, whose send drops the task of the run the issue left
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the claim holds %+v, want the task of run 1 dropped", p)
		}
		h.wantLeftEdits([]string{staleEditsCommit}, "", "after the drop")

		h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{})
		h.conn.FailPrompt(nil)
		h.must(RequestDeliver{Claim: testToken, ID: "outbox:42", Task: "implement the plan of run 2", Phase: phase.Implementing, Generation: 2})
		sent := h.wantPrompts(2)[1]
		if !namesTheEdits(sent.Message) {
			t.Fatalf("the task of run 2 was sent as %q, want it naming %s", sent.Message, staleEditsCommit)
		}
		h.wantLeftEdits([]string{staleEditsCommit}, "", "at the acknowledgement")
		h.must(StreamTurnStart{Claim: testToken})
		h.wantLeftEdits([]string{staleEditsCommit}, sent.DeliveryID, "once the task's turn began")
	})

	t.Run("a later adoption before the record is told adds its commit after the first", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{EditsCommits: []string{staleEditsCommit}})
		h.conn.RefusePrompt("the model provider refused the request")
		h.must(task)

		h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{EditsCommits: []string{laterEditsCommit}})
		h.conn.FailPrompt(nil)
		h.observe(runtime.Alive) // the sweep's re-send, whose adoption finds the working copy stale again
		both := []string{staleEditsCommit, laterEditsCommit}
		again := h.wantPrompts(2)[1]
		if want := task.Task + "\n\n" + staleEditsSentence(both); again.Message != want || again.DeliveryID != h.pending().ID {
			t.Fatalf("the re-send was %+v, want %q under the task's id", again, want)
		}
		h.wantLeftEdits(both, "", "after the second adoption")
		h.must(StreamTurnStart{Claim: testToken})
		h.wantLeftEdits(both, again.DeliveryID, "once the task's turn began")
	})

	t.Run("a confirmation taken back tells the record again", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{EditsCommits: []string{staleEditsCommit}})
		h.must(task)
		first := h.wantPrompts(1)[0]
		h.rt.AnswerAdoptWorkingCopy(runtime.Adoption{})
		h.must(StreamTurnStart{Claim: testToken})
		h.wantLeftEdits([]string{staleEditsCommit}, first.DeliveryID, "once the task's turn began")

		// Oh My Pi answers that prompt busy after all: the turn that confirmed it was another input's.
		h.must(StreamLateRefusal{Claim: testToken, DeliveryID: first.DeliveryID, Error: busyWords, Outcome: runtime.OutcomeAgentBusy})
		if p := h.pending(); !p.ConfirmedAt.IsZero() {
			t.Fatalf("pending after the late busy answer = %+v, want the confirmation taken back", p)
		}
		h.wantLeftEdits([]string{staleEditsCommit}, "", "after the confirmation that told it was taken back")

		h.mustQuiet(StreamTurnEnd{Claim: testToken})
		second := h.wantPrompts(2)[1]
		if !namesTheEdits(second.Message) {
			t.Fatalf("the task after the busy answer was sent as %q, want it naming %s again", second.Message, staleEditsCommit)
		}
		h.must(StreamTurnStart{Claim: testToken})
		h.wantLeftEdits([]string{staleEditsCommit}, second.DeliveryID, "once the task's own turn began")
	})
}

// The sentence that names the commits is composed into each prompt from the claim's record
// (Delivery.message), after the text of whatever the claim is sent while the record tells, and it
// marks the prompt as carrying it (sentences.leftEdits): a reviewer's task keeps the two sentences
// of its review round last, whether it names its round or is given them, and the daemon's own
// delivery of a relaunch's record carries it too, so no prompt that a turn can confirm leaves it
// out. A record a turn has told adds nothing.
func TestTheSentenceOfAStaleAdoptionsCommitsFollowsEachPromptWhileItTells(t *testing.T) {
	const task = "Continue. Phase: reviewing. Reason: as requested."
	round := reviewround.Sentences("5ab0c1d2e3f405162738495a6b7c8d9e0f1a2b3c", 2)
	sentence := staleEditsSentence([]string{staleEditsCommit})
	left := LeftEdits{Commits: []string{staleEditsCommit}, Delivery: "outbox:41"}
	told := left
	told.Told, told.ToldBy = true, "outbox:41~2"
	for _, tc := range []struct {
		name     string
		role     claim.Role
		left     LeftEdits
		delivery Delivery
		want     string
		carried  bool
	}{
		{name: "an implementer's task", role: claim.RoleImplementer, left: left,
			delivery: Delivery{ID: "outbox:1", Task: task}, want: task + "\n\n" + sentence, carried: true},
		{name: "a reviewer's task that names its round", role: claim.RoleReviewer, left: left,
			delivery: Delivery{ID: "outbox:2", Task: task + round, RoundNamed: true}, want: task + "\n\n" + sentence + round, carried: true},
		{name: "a reviewer's task the daemon gives its round", role: claim.RoleReviewer, left: left,
			delivery: Delivery{ID: "OPERATORID", Task: task}, want: task + "\n\n" + sentence + reviewround.Sentences("", 0), carried: true},
		{name: "the relaunch's record", role: claim.RoleReviewer, left: left,
			delivery: Delivery{ID: relaunchTaskID(), Task: "the record"}, want: "the record\n\n" + sentence, carried: true},
		{name: "a task after the record was told", role: claim.RoleImplementer, left: told,
			delivery: Delivery{ID: "outbox:3", Task: task}, want: task},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, carried := tc.delivery.message(Claim{Role: tc.role, LeftEdits: tc.left})
			if got != tc.want || carried.leftEdits != tc.carried {
				t.Fatalf("the %s is sent %q carrying the record = %v, want %q carrying it = %v", tc.role, got, carried.leftEdits, tc.want, tc.carried)
			}
		})
	}
}
