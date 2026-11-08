package admit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// slotWait is the fixture of the waiting-for-slot notice: one Dispatch project's admission over a
// cap of one slot, the root whose architect hands its work to a person, and the roots that take
// that slot while it waits. The project key is a field rather than testProject because nothing of
// this notice reads it, and TestAnAnsweredRootWithNoSlotIsToldOnce runs the whole case under two
// keys to prove it; every other helper of this package records issues under testProject, so this
// one seeds its own.
type slotWait struct {
	t         *testing.T
	pool      *pgxpool.Pool
	project   string
	admission *Admission
	root      string
	seq       int64
	facts     int
}

// newSlotWait is the fixture with nothing recorded yet, under a cap of one slot and the design
// gate of steps (nil for the deployment's single step spec).
func newSlotWait(t *testing.T, project string, steps []record.DesignStep) *slotWait {
	t.Helper()
	return &slotWait{t: t, pool: migratedPool(t), project: project, root: project + "-208", seq: 100,
		admission: newAdmissionForProject(t, project, 1, steps, slog.New(slog.NewTextHandler(io.Discard, nil)))}
}

// next is the next Dispatch sequence this fixture writes. Every hand-back, mirror row and applied
// event takes one, so each is newer than everything before it, which is the fence a handout reads
// (record.Store.StartHandout).
func (w *slotWait) next() int64 {
	w.seq += 10
	return w.seq
}

// architect is the claim token of issue's root architect, built as workflow.ReconcileAwaiting
// builds it: the project token, then the token of the issue's architect role.
func (w *slotWait) architect(issue string) claim.Token {
	w.t.Helper()
	project, err := claim.ProjectToken(w.project)
	if err != nil {
		w.t.Fatalf("project token of %s: %v", w.project, err)
	}
	token, err := claim.NewToken(project, issue, claim.RoleArchitect)
	if err != nil {
		w.t.Fatalf("architect token of %s: %v", issue, err)
	}
	return token
}

// issueOf is a root of this project, its own tree, at phase and status.
func (w *slotWait) issueOf(key, rank, status string, at phase.Phase) record.Issue {
	return record.Issue{Key: key, Tree: key, Project: w.project, Title: key, Phase: at, Generation: 1,
		Status: status, Rank: rank, HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// openLifecycle opens key's tree lifecycle under the workflow's authority, as a promotion does.
func (w *slotWait) openLifecycle(key string) {
	w.t.Helper()
	project, err := claim.ProjectToken(w.project)
	if err != nil {
		w.t.Fatalf("project token of %s: %v", w.project, err)
	}
	inTx(w.t, w.pool, func(tx pgx.Tx) {
		if _, err := record.NewStore().OpenTreeLifecycle(context.Background(), tx, project, key, treelifecycle.AuthorityWorkflow); err != nil {
			w.t.Fatalf("open the tree lifecycle of %s: %v", key, err)
		}
	})
}

// slotted records key as a running root of this project holding an admission slot, its tree
// lifecycle open under the workflow (seedSlotted, for a project of this fixture's own).
func (w *slotWait) slotted(key, rank string) {
	w.t.Helper()
	putIssue(w.t, w.pool, w.issueOf(key, rank, "in_progress", phase.Admitted))
	inTx(w.t, w.pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		held, err := records.Slots(ctx, tx)
		if err != nil {
			w.t.Fatalf("list slots: %v", err)
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: len(held), AdmittedAt: fixedNow}); err != nil {
			w.t.Fatalf("put the slot of %s: %v", key, err)
		}
	})
	w.openLifecycle(key)
}

// waiting records key as a root of this project waiting in line for its first slot.
func (w *slotWait) waiting(key, rank string) {
	w.t.Helper()
	putIssue(w.t, w.pool, w.issueOf(key, rank, "todo", phase.Admitted))
}

// claimArchitect records issue's architect in state, holding a task it has taken: the one standing
// in which its root may hand work to a person and give its slot back (workflow's owed).
func (w *slotWait) claimArchitect(issue string, state supervise.ClaimState) {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session,
		session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $3, 'architect', 1, 'ses_architect', '', $4, 0, 0, 0, 0)`,
		string(w.architect(issue)), w.project, issue, string(state)); err != nil {
		w.t.Fatalf("record the architect claim of %s: %v", issue, err)
	}
	if _, err := w.pool.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at,
		generation, phase, confirmed_at) values ($1, 'outbox:7', 'Decompose it.', now(), 1, 'admitted', now())`,
		string(w.architect(issue))); err != nil {
		w.t.Fatalf("record the architect's task of %s: %v", issue, err)
	}
}

// setArchitect moves issue's architect claim to state.
func (w *slotWait) setArchitect(issue string, state supervise.ClaimState) {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(), `update claims set state = $2 where token = $1`,
		string(w.architect(issue)), string(state)); err != nil {
		w.t.Fatalf("set the architect state of %s: %v", issue, err)
	}
}

// handBack records the root's architect handing question ask-q to a person: the question's mirror
// row, still with them, and the hand-back itself, which is where the wait begins.
func (w *slotWait) handBack() {
	w.t.Helper()
	at := w.next()
	inTx(w.t, w.pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		if err := records.PutRootAsk(ctx, tx, record.RootAsk{Issue: w.root, Ask: "ask-q", Kind: record.AskQuestion,
			Open: true, Seq: at, WaitingOn: "human", WaitingOnSeq: at, ByTree: true}); err != nil {
			w.t.Fatalf("record the question of %s: %v", w.root, err)
		}
		if err := records.Declare(ctx, tx, w.root, record.EventMark{Seq: at}); err != nil {
			w.t.Fatalf("declare the hand-back of %s: %v", w.root, err)
		}
	})
}

// pass runs one admission pass, the controller's tick, which is the pass that runs on no fact of
// its own.
func (w *slotWait) pass(label string) {
	w.t.Helper()
	w.facts++
	if _, err := intake.ApplyFact(context.Background(), w.pool, "controller", fmt.Sprintf("%s-%d", label, w.facts),
		intake.ControllerTick{}, engineStub{}, w.admission); err != nil {
		w.t.Fatalf("admission pass %s: %v", label, err)
	}
}

// apply applies fact through the workflow engine and admission, as the daemon's handlers run.
func (w *slotWait) apply(label string, fact intake.Fact) {
	w.t.Helper()
	w.facts++
	if _, err := intake.ApplyFact(context.Background(), w.pool, "dispatch", fmt.Sprintf("%s-%d", label, w.facts),
		fact, w.admission.engine, w.admission); err != nil {
		w.t.Fatalf("apply %s: %v", label, err)
	}
}

// notices is every waiting-for-slot notice the outbox holds for the root, oldest first.
func (w *slotWait) notices() []record.Notice {
	w.t.Helper()
	return gateNotices(w.t, w.pool, w.root, workflow.WaitingForSlotNoticeKind)
}

// told is whether the root's wait carries the mark that says its architect was told
// (record.Awaiting.SlotNoticeAt).
func (w *slotWait) told() bool {
	w.t.Helper()
	got := issue(w.t, w.pool, w.root)
	return got.Awaiting != nil && got.Awaiting.SlotNoticeAt != nil
}

// architectStartsOf is how many architect start rows the outbox holds for the root.
func (w *slotWait) architectStartsOf() int {
	w.t.Helper()
	starts := 0
	for _, got := range effects(w.t, w.pool) {
		if request, ok := got.payload.(record.SuperviseRequest); ok && got.issue == w.root &&
			request.Role == claim.RoleArchitect && request.Op == "start" {
			starts++
		}
	}
	return starts
}

// runArchitectStarts stands in for the outbox running the root's queued architect starts and the
// architect taking the task they carry: the rows go, the claim works and the delivery is
// confirmed, which is what ends the architect's debt (workflow's owed).
func (w *slotWait) runArchitectStarts() {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(), `delete from outbox where kind = 'supervise' and issue = $1
		and payload->>'role' = 'architect' and payload->>'op' = 'start'`, w.root); err != nil {
		w.t.Fatalf("run the architect starts of %s: %v", w.root, err)
	}
	w.setArchitect(w.root, supervise.StateWorking)
	if _, err := w.pool.Exec(context.Background(), `update pending_task_deliveries set confirmed_at = now() where claim_token = $1`,
		string(w.architect(w.root))); err != nil {
		w.t.Fatalf("confirm the architect task of %s: %v", w.root, err)
	}
}

// waitAnsweredWithNoSlot takes the root through one whole wait while the cap has no free slot:
// its architect hands a question to a person, the pass moves it into awaiting_person and gives
// its slot to filler, a person's act answers the wait, and one more pass decides the answered
// root with the cap full. It is what each stage of the first test repeats.
func (w *slotWait) waitAnsweredWithNoSlot(label, filler string, by record.Answer, detail string) {
	w.t.Helper()
	w.handBack()
	w.pass(label + "-hands-back")
	got := issue(w.t, w.pool, w.root)
	if got.Phase != phase.AwaitingPerson || got.Answered() {
		w.t.Fatalf("%s: the root after its architect handed a question back = %#v, want it waiting on a person", label, got)
	}
	if held := slots(w.t, w.pool); len(held) != 1 || held[0].Issue != filler {
		w.t.Fatalf("%s: slots = %#v, want the cap's one slot taken by %s while the root waits", label, held, filler)
	}
	answerWait(w.t, w.pool, w.root, by, detail)
	w.pass(label + "-answered")
}

// A person's answer of a root's question, on a tree whose design gate has never opened, with
// every slot of the cap taken, tells that root's architect once: its work came back, it needs a
// slot, there is none, and the daemon starts it again by itself. The telling is a notice and a
// mark beside the answer, and the mark is what holds the daemon to one notice per wait however
// many passes run before a slot frees.
//
// The whole case runs under two Dispatch project keys, with nothing else between the runs: the
// notice's text and the place it names are the same under both, because nothing of this notice
// reads a project key.
func TestAnAnsweredRootWithNoSlotIsToldOnce(t *testing.T) {
	said := map[string]string{}
	for _, project := range []string{"LEGION", "WIDGETS"} {
		t.Run(project, func(t *testing.T) {
			w := newSlotWait(t, project, nil)
			behind := project + "-400"
			// The root holds the cap's one slot and its architect is at work; two roots wait in
			// line behind it, the first of which takes the slot the root gives back.
			w.slotted(w.root, "U")
			w.waiting(project+"-300", "V")
			w.waiting(behind, "W")
			w.claimArchitect(w.root, supervise.StateWorking)

			// One wait, answered, with the cap full: one notice, and the mark beside the answer.
			w.waitAnsweredWithNoSlot("first", project+"-300", record.AnsweredByAsk, "ask-q")
			told := w.notices()
			if len(told) != 1 {
				t.Fatalf("waiting-for-slot notices = %#v, want exactly one", told)
			}
			if !w.told() {
				t.Fatalf("the answered wait of %s carries no mark, want the notice recorded on it", w.root)
			}
			if got := w.line(); len(got) != 2 || got[0] != w.root || got[1] != behind {
				t.Fatalf("the waiting line = %#v, want the answered root first and %s behind it", got, behind)
			}
			for _, says := range []string{
				"A person answered your ask ask-q.",
				"Your issue needs an admission slot, and the admission cap has no free slot now.",
				"Your issue stands at place 1 of 2 in the admission line.",
				"The daemon starts you again by itself at the next free slot.",
				"The place above is the place at the moment of this notice.",
				"The issue's own record carries the live place in its `waitingLine` field.",
			} {
				if !strings.Contains(told[0].Reason, says) {
					t.Fatalf("the notice reads %q, want it to say %q", told[0].Reason, says)
				}
			}
			if told[0].Role != claim.RoleArchitect {
				t.Fatalf("the notice goes to the %s, want the architect", told[0].Role)
			}
			said[project] = told[0].Reason
			t.Logf("the notice the pass wrote under %s: %s", project, told[0].Reason)

			// Ten further passes decide the same root again and write nothing: the mark, not the
			// wait, is what says the architect has been told.
			for i := range 10 {
				w.pass(fmt.Sprintf("again-%d", i))
			}
			if got := w.notices(); len(got) != 1 {
				t.Fatalf("waiting-for-slot notices after ten further passes = %d, want the one", len(got))
			}

			// A notice the outbox finishes undelivered leaves the wait marked: the mark says a
			// notice was written, never that an architect read one.
			w.finishNotices()
			if !w.told() {
				t.Fatalf("the wait of %s lost its mark when the outbox finished the notice", w.root)
			}
			w.pass("after-the-outbox-finished-it")
			if got := w.notices(); len(got) != 0 {
				t.Fatalf("waiting-for-slot notices after the outbox finished the first = %#v, want none written again", got)
			}

			// A slot frees: the architect is resumed where it stands, and nothing else is said.
			w.finish(project + "-300")
			w.pass("slot-freed")
			resumed := issue(t, w.pool, w.root)
			if resumed.Phase != phase.Admitted || resumed.Awaiting != nil {
				t.Fatalf("the root after a slot freed = %#v, want it admitted again with its wait ended", resumed)
			}
			if starts := w.architectStartsOf(); starts != 1 {
				t.Fatalf("architect starts after the slot freed = %d, want the one resume task", starts)
			}
			if got := w.notices(); len(got) != 0 {
				t.Fatalf("waiting-for-slot notices after the resume = %#v, want none", got)
			}

			// A second wait of the same root, after that resume, is a wait of its own and is told.
			w.runArchitectStarts()
			w.waitAnsweredWithNoSlot("second", behind, record.AnsweredByAsk, "ask-q")
			if got := w.notices(); len(got) != 1 {
				t.Fatalf("waiting-for-slot notices of the root's second wait = %#v, want one", got)
			}

			// A wait the linger clears leaves no mark behind, so the root's next wait is told.
			w.finishNotices()
			w.park("parked", "backlog")
			if parked := issue(t, w.pool, w.root); parked.Awaiting != nil {
				t.Fatalf("the root a linger took out of the workflow = %#v, want its wait and its mark gone", parked)
			}
			w.waiting(project+"-500", "X")
			w.readmit("after-the-linger", behind)
			w.waitAnsweredWithNoSlot("after-the-linger", project+"-500", record.AnsweredByAsk, "ask-q")
			if got := w.notices(); len(got) != 1 {
				t.Fatalf("waiting-for-slot notices of the wait after a linger = %#v, want one", got)
			}

			// And so does a wait the operator's hold on the tree clears.
			w.finishNotices()
			w.holdTreeByOperator()
			w.park("operator-parked", "backlog")
			if parked := issue(t, w.pool, w.root); parked.Awaiting != nil || parked.LingerUntil != nil {
				t.Fatalf("the root that left under the operator's tree = %#v, want its wait and its mark gone, not lingering", parked)
			}
			w.releaseTreeToWorkflow()
			w.waiting(project+"-600", "Y")
			w.readmit("after-the-operator", project+"-500")
			w.waitAnsweredWithNoSlot("after-the-operator", project+"-600", record.AnsweredByAsk, "ask-q")
			if got := w.notices(); len(got) != 1 {
				t.Fatalf("waiting-for-slot notices of the wait after the operator's hold = %#v, want one", got)
			}
		})
	}
	if said["LEGION"] != said["WIDGETS"] {
		t.Fatalf("the notice under LEGION reads\n%q\nand under WIDGETS\n%q\nwant the same words and the same place under either key",
			said["LEGION"], said["WIDGETS"])
	}
}

// line is this project's waiting line, in its order, read as admission reads it.
func (w *slotWait) line() []string {
	w.t.Helper()
	var got []string
	inTx(w.t, w.pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		issues, err := records.Issues(ctx, tx)
		if err != nil {
			w.t.Fatalf("list issues: %v", err)
		}
		held, err := records.Slots(ctx, tx)
		if err != nil {
			w.t.Fatalf("list slots: %v", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, records, w.project, issues, w.admission.engine.GatedSteps())
		if err != nil {
			w.t.Fatalf("read the line facts: %v", err)
		}
		_, _, line, err := blockers.JudgeLine(ctx, tx, records, w.project, issues, held, facts, nil)
		if err != nil {
			w.t.Fatalf("judge the waiting line: %v", err)
		}
		for _, entry := range line {
			got = append(got, entry.Issue.Key)
		}
	})
	return got
}

// finishNotices deletes every waiting-for-slot notice of the root from the outbox, as the
// outbox's own finish of a row does (record.Store.FinishOutbox), delivered or given up on.
func (w *slotWait) finishNotices() {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(), `delete from outbox where kind = 'notice' and issue = $1
		and payload->>'kind' = $2`, w.root, string(workflow.WaitingForSlotNoticeKind)); err != nil {
		w.t.Fatalf("finish the waiting-for-slot notices of %s: %v", w.root, err)
	}
}

// finish records key as finished, which gives its slot back on the next pass.
func (w *slotWait) finish(key string) {
	w.t.Helper()
	done := issue(w.t, w.pool, key)
	done.Phase = phase.Done
	putIssue(w.t, w.pool, done)
}

// park is a person moving the root out of the workflow at status, which clears its wait: into the
// linger (workflow's beginLinger), or, while the operator holds a tree under its key, out from
// under the workflow alone (workflow's leaveUnderOperator).
func (w *slotWait) park(label, status string) {
	w.t.Helper()
	w.apply(label, intake.DispatchIssue{Key: w.root, ResolvedRepo: "acme/widgets", Seq: w.next(), Type: "issue.updated",
		Status: status, Title: w.root, Rank: "U", HandedOver: true})
}

// readmit sets the root back to todo with its label, which admits a new generation of it, frees
// the slot holder's slot and runs the pass that promotes the root into it, its architect at work.
func (w *slotWait) readmit(label, holder string) {
	w.t.Helper()
	w.apply(label+"-todo", intake.DispatchIssue{Key: w.root, ResolvedRepo: "acme/widgets", Seq: w.next(), Type: "issue.updated",
		Status: "todo", Title: w.root, Rank: "U", HandedOver: true})
	w.finish(holder)
	w.pass(label + "-promoted")
	back := issue(w.t, w.pool, w.root)
	if back.Phase != phase.Admitted {
		w.t.Fatalf("%s: the re-admitted root = %#v, want it admitted again", label, back)
	}
	if held := slots(w.t, w.pool); len(held) != 1 || held[0].Issue != w.root {
		w.t.Fatalf("%s: slots = %#v, want the re-admitted root holding the cap's one slot", label, held)
	}
	w.runArchitectStarts()
}

// holdTreeByOperator records the operator holding a tree under the root's key, as `legion` does
// when an operator runs one there: the root's own leave then takes the workflow's record out and
// leaves the operator's tree alone (workflow's leaveUnderOperator).
func (w *slotWait) holdTreeByOperator() {
	w.t.Helper()
	project, err := claim.ProjectToken(w.project)
	if err != nil {
		w.t.Fatalf("project token of %s: %v", w.project, err)
	}
	if _, err := w.pool.Exec(context.Background(), `update tree_lifecycles set authority = 'operator' where project = $1 and tree = $2`,
		project, w.root); err != nil {
		w.t.Fatalf("hold the tree of %s under the operator: %v", w.root, err)
	}
}

// releaseTreeToWorkflow is the operator's tree ending: the lifecycle is the workflow's again, so
// the root's re-admission opens it rather than waiting on the operator's lifetime.
func (w *slotWait) releaseTreeToWorkflow() {
	w.t.Helper()
	project, err := claim.ProjectToken(w.project)
	if err != nil {
		w.t.Fatalf("project token of %s: %v", w.project, err)
	}
	if _, err := w.pool.Exec(context.Background(), `delete from tree_lifecycles where project = $1 and tree = $2`,
		project, w.root); err != nil {
		w.t.Fatalf("release the tree of %s to the workflow: %v", w.root, err)
	}
}

// answeredAndWaiting records key as a deploy of this release finds a root whose wait a person
// has already answered: in awaiting_person with the answer on it and nothing in the column the
// mark lives in, its tree lifecycle open under the workflow, as a tree whose architect has run
// must be.
func (w *slotWait) answeredAndWaiting(key, rank string, waiting record.Awaiting) {
	w.t.Helper()
	at := fixedNow
	waiting.AnsweredAt = &at
	if waiting.Since.Seq == 0 {
		waiting.Since = record.EventMark{Seq: w.next()}
	}
	got := w.issueOf(key, rank, "in_progress", phase.AwaitingPerson)
	got.Awaiting = &waiting
	putIssue(w.t, w.pool, got)
	w.openLifecycle(key)
}

// The column the mark lives in is empty on every deploy of this release, which is the point of
// adding it with no backfill: the first admission pass after the daemon starts reads every root
// already answered and waiting as one it has told nobody about. It tells each architect that can
// hear it, once, and the pass after that tells nobody again.
//
// The root whose architect's claim has registered but not yet taken its Envoy role is the half
// of it that waits: there is no session to read a notice, so the pass writes none and leaves the
// wait unmarked for the pass after that architect takes its role.
func TestTheFirstPassAfterAStartTellsTheRootsAlreadyWaiting(t *testing.T) {
	w := newSlotWait(t, testProject, nil)
	heard, unheard := w.root, testProject+"-209"
	w.slotted(testProject+"-900", "A")
	w.answeredAndWaiting(heard, "B", record.Awaiting{Ask: "ask-q", AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"})
	w.answeredAndWaiting(unheard, "C", record.Awaiting{Ask: "ask-r", AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-r"})
	w.claimArchitect(heard, supervise.StateIdle)
	w.claimArchitect(unheard, supervise.StateRegistered)

	w.pass("the-first-pass-after-the-start")
	told := w.notices()
	if len(told) != 1 {
		t.Fatalf("waiting-for-slot notices of %s after the first pass = %#v, want the one its architect can hear", heard, told)
	}
	if !strings.Contains(told[0].Reason, "A person answered your ask ask-q.") {
		t.Fatalf("the notice reads %q, want it to name the act that answered the wait", told[0].Reason)
	}
	if silent := gateNotices(t, w.pool, unheard, workflow.WaitingForSlotNoticeKind); len(silent) != 0 {
		t.Fatalf("waiting-for-slot notices of %s = %#v, want none: its architect has not taken its role", unheard, silent)
	}
	if !w.told() {
		t.Fatalf("the wait of %s carries no mark after its architect was told", heard)
	}
	if got := issue(t, w.pool, unheard); got.Awaiting == nil || got.Awaiting.SlotNoticeAt != nil {
		t.Fatalf("the wait of %s = %#v, want it unmarked: nothing was written for it", unheard, got.Awaiting)
	}

	w.pass("the-second-pass")
	if got := w.notices(); len(got) != 1 {
		t.Fatalf("waiting-for-slot notices of %s after the second pass = %d, want the one the first pass wrote", heard, len(got))
	}
	if silent := gateNotices(t, w.pool, unheard, workflow.WaitingForSlotNoticeKind); len(silent) != 0 {
		t.Fatalf("waiting-for-slot notices of %s after the second pass = %#v, want none", unheard, silent)
	}
}

// toldCase is one row of TestEveryAnswerThatLeavesARootWaitingIsTold: the deployment's design
// gate (steps; nil is its single step spec), the answered wait the root stands in, the gate rows
// it holds, its architect claim's state ("" records no claim row at all), whether its `legion`
// label is gone or its Dispatch claim is a person's — either makes a resume unsafe
// (workflow.Unresumable) — and whether the pass tells its architect. resumes is the one row the
// cap never reaches, because a root whose gate has opened needs no slot: the pass resumes it,
// and that assertion is also what proves these gate rows reach the pass at all.
type toldCase struct {
	name       string
	steps      []record.DesignStep
	waiting    record.Awaiting
	gates      []record.DesignGate
	state      supervise.ClaimState
	unlabelled bool
	claimed    string
	told       bool
	resumes    bool
}

// Every way a root's wait is answered while the cap can give it no slot tells its architect, and
// every reason the daemon has to say nothing says nothing. The two halves are one table because
// the rule is one condition read in one place (admit's tellWaitingForSlot): a root that needs a
// slot, has none, and has an architect that can hear the notice.
func TestEveryAnswerThatLeavesARootWaitingIsTold(t *testing.T) {
	approved := func(version int) *int { return &version }
	cases := []toldCase{
		{
			name:    "a question answered before any step opened",
			waiting: record.Awaiting{Ask: "ask-q", AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"},
			state:   supervise.StateWorking, told: true,
		},
		{
			name:    "a changes request on a gate that never opened",
			waiting: record.Awaiting{GateVersion: 4, Artifact: "artifact-root", AnsweredBy: record.AnsweredByChanges, AnsweredDetail: "version 4"},
			gates:   []record.DesignGate{{Step: record.DesignStepSpec, ArtifactID: "artifact-root", LatestVersion: 4}},
			state:   supervise.StateWorking, told: true,
		},
		{
			name:    "a wait answered by a retraction",
			waiting: record.Awaiting{Ask: "ask-q", AnsweredBy: record.AnsweredByRetraction, AnsweredDetail: "ask-q"},
			state:   supervise.StateWorking, told: true,
		},
		{
			name:    "a wait answered by a session's close",
			waiting: record.Awaiting{Ask: "ask-q", AnsweredBy: record.AnsweredByClosed, AnsweredDetail: "session-elsewhere"},
			state:   supervise.StateWorking, told: true,
		},
		{
			name:    "two gated steps with one approved and the other never opened",
			steps:   []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec},
			waiting: record.Awaiting{Ask: "ask-q", AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"},
			gates: []record.DesignGate{
				{Step: record.DesignStepCriteria, ArtifactID: "artifact-criteria", LatestVersion: 2, ApprovedVersion: approved(2), EverOpened: true},
				{Step: record.DesignStepSpec, ArtifactID: "artifact-root", LatestVersion: 4},
			},
			state: supervise.StateWorking, told: true,
		},
		{
			name:    "one gated step whose approval opens the gate",
			waiting: record.Awaiting{GateVersion: 4, Artifact: "artifact-root", AnsweredBy: record.AnsweredByApproval, AnsweredDetail: "version 4"},
			gates:   []record.DesignGate{{Step: record.DesignStepSpec, ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: approved(4), EverOpened: true}},
			state:   supervise.StateWorking,
			resumes: true,
		},
		{
			name:    "no architect claim row of the generation",
			waiting: record.Awaiting{Ask: "ask-q", AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"},
		},
		{
			name:       "an answered root whose legion label is gone",
			waiting:    record.Awaiting{Ask: "ask-q", AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"},
			state:      supervise.StateWorking,
			unlabelled: true,
		},
		{
			name:    "an answered root whose Dispatch claim a person holds",
			waiting: record.Awaiting{Ask: "ask-q", AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"},
			state:   supervise.StateWorking,
			claimed: "user:bob",
		},
	}
	// One row for each state that is not ready, working or idle: a claim in any of them has not
	// taken its Envoy role (supervise.TookRole), so no session would read the notice.
	for _, state := range []supervise.ClaimState{supervise.StateQueued, supervise.StateLaunchUncertain, supervise.StateLaunching,
		supervise.StateShimConnected, supervise.StateRegistered, supervise.StateSuspended, supervise.StateFailed, supervise.StateRetired} {
		cases = append(cases, toldCase{
			name:    "an architect claim in " + string(state),
			waiting: record.Awaiting{Ask: "ask-q", AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"},
			state:   state,
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newSlotWait(t, testProject, tc.steps)
			w.slotted(testProject+"-900", "A")
			w.answeredAndWaiting(w.root, "B", tc.waiting)
			if tc.unlabelled {
				got := issue(t, w.pool, w.root)
				got.HandedOver = false
				putIssue(t, w.pool, got)
			}
			for _, gate := range tc.gates {
				gate.Issue = w.root
				seedGate(t, w.pool, gate)
			}
			if tc.state != "" {
				w.claimArchitect(w.root, tc.state)
			}
			if tc.claimed != "" {
				seedDispatchClaim(t, w.pool, w.root, tc.claimed)
			}

			w.pass("one-pass")

			got := w.notices()
			if tc.told {
				if len(got) != 1 {
					t.Fatalf("waiting-for-slot notices = %#v, want the one this answer earns", got)
				}
				if !w.told() {
					t.Fatalf("the answered wait of %s carries no mark after its architect was told", w.root)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("waiting-for-slot notices = %#v, want none", got)
			}
			if w.told() {
				t.Fatalf("the wait of %s is marked told, and no notice was written for it", w.root)
			}
			if stands := issue(t, w.pool, w.root); tc.resumes != (stands.Phase == phase.Admitted && stands.Awaiting == nil) {
				t.Fatalf("the root after the pass = %#v, want resumed = %t", stands, tc.resumes)
			}
		})
	}
}
