package supervise

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// staleEditsCommit is a commit the shim names when bringing a stale working copy up to date left
// the edits that no jj command had recorded in it.
const staleEditsCommit = "c45ff7e33d4899a2b99db849241fe128115021b0"

// staleAdoptionLine is the line the machine writes when it puts such a commit in the task, as an
// operator reads it.
const staleAdoptionLine = "supervise: the adoption brought a stale working copy up to date and left the edits no jj command had recorded in a commit; the task names it and is sent at once"

// namesTheEdits is whether a prompt names the commit that holds the edits a stale working copy's
// update left and says they are there and no longer on disk, as the role reads it.
func namesTheEdits(message string) bool {
	return strings.Contains(message, "Before this task, the working copy was stale.") &&
		strings.Contains(message, "left the edits that no jj command had recorded in commit "+staleEditsCommit) &&
		strings.Contains(message, "they are no longer on disk") &&
		strings.Contains(message, "jj show "+staleEditsCommit)
}

// The prompt of a phase task is fixed before its adoption runs, and an adoption that brought a
// stale working copy up to date names the commit in which the update left the edits that no jj
// command had recorded (runtime.Adoption). The send then prompts with no text it fixed before:
// the machine writes the commit into the pending task, under a new id so the shim's dedupe by id
// forwards the new text, persists it, logs it, and sends that task at once. The sentence is the
// task's own text from then on, so every later send carries it — after a refused prompt, after a
// daemon restart — though each later adoption, of a working copy up to date by then, names no
// commit. A refusal that names a commit is recorded on the task the same way. An adoption that
// names none leaves the task and its id as they were.
func TestAStaleAdoptionsCommitIsNamedInTheTaskItsRoleGets(t *testing.T) {
	task := RequestDeliver{Claim: testToken, ID: "outbox:41", Task: "implement the plan", Phase: phase.Implementing, Generation: 1}
	names := runtime.Adoption{EditsCommits: []string{staleEditsCommit}}
	// wantNamed fails unless the pending task names the commit under a new id, as the store holds
	// it, and answers it.
	wantNamed := func(t *testing.T, h *harness) Delivery {
		t.Helper()
		p := h.pending()
		stored, ok := h.store.delivery(testToken)
		if p.ID == task.ID || !strings.HasPrefix(p.Task, task.Task) || !namesTheEdits(p.Task) || !ok || stored.ID != p.ID || stored.Task != p.Task {
			t.Fatalf("the pending task is %+v, stored %+v (%v); want the task naming %s, persisted, under a new id", p, stored, ok, staleEditsCommit)
		}
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
		if sent.DeliveryID != p.ID || sent.Message != p.Task {
			t.Fatalf("the agent was sent %+v, want the task naming the commit under its new id %s", sent, p.ID)
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

	t.Run("a refused adoption that names a commit is recorded on the task the next prompt carries", func(t *testing.T) {
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
