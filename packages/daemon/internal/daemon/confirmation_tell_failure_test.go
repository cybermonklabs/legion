package daemon

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/supervise/supervisetest"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// A start's task names a decision a person made while nobody in the root's tree could hear it, and
// the root's architect takes it. The write recording an accepted take is the one that tells the
// decision (store.tellKeptDecisions), and it can fail — a database error, a lost connection —
// while the worker waits on the answer. The take is composed on a copy that memory takes only once
// that write lands (supervise's accept), so a failed write leaves no take: the worker is answered
// the failure and no outcome, and nothing is told. The take is recorded refused at its revision
// instead (decision 20 of dispatch://INFRA-2811), so once the store answers again the value is
// written again under the same number, as take-failed, and the worker's take at that newer
// revision is accepted with a text that names the decision, which that take tells, once: a later
// start of the root names it no more. The tree's rule is that a person's decision is never lost
// (the architect's decision on #2163, review round cap, 31 October 2026), and here it reaches the
// architect in the very task the failed take was of.
func TestAFailedConfirmationTellLeavesTheDecisionKeptAndToldAgain(t *testing.T) {
	values := supervisetest.NewTaskValues()
	log := &testlog.Buffer{}
	r := newHeldTaskRig(t, func(r *heldTaskRig) {
		r.sup.deps.TaskValues = values
		r.sup.deps.Log = slog.New(slog.NewJSONHandler(log, nil))
	})
	ref := "dispatch://" + heldTaskRoot + "/ask/ask-told"
	keepDecision(t, r.pool, r.records, heldTaskRoot, ref, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	r.runRow(r.enqueueStart())
	machine, _ := r.sup.Machine(r.token)
	pending := machine.Claim().Pending
	if pending == nil || !strings.Contains(pending.Task, ref) {
		t.Fatalf("the architect holds %+v, want the start's task naming %s", pending, ref)
	}
	asked, held := values.Value(r.token)
	if !held || asked.Number != pending.Number {
		t.Fatalf("the architect's value is %+v (held %v), want task %d written", asked, held, pending.Number)
	}

	// Every delete of a kept decision is counted, so a decision told twice shows; and while the
	// store refuses, the telling the accepted take owes is refused.
	r.execAll(`create table told_decisions (ref text not null)`,
		`create function count_the_tell() returns trigger language plpgsql as $$
			begin insert into told_decisions (ref) values (old.ref); return old; end $$`,
		`create trigger count_the_tell after delete on parked_decisions for each row execute function count_the_tell()`,
		`create function refuse_the_tell() returns trigger language plpgsql as $$
			begin raise exception 'the confirmation''s write did not commit'; end $$`,
		`create trigger refuse_the_tell before delete on parked_decisions for each row execute function refuse_the_tell()`)
	answer, err := r.takeAt(machine, asked)
	if err == nil || answer.Outcome != "" {
		t.Fatalf("the take whose confirmation write the store refused = %+v, %v; want no outcome and the write's failure", answer, err)
	}
	if told := r.told(ref); told != 0 || !strings.Contains(keptRefs(keptDecisions(t, r.pool, r.records, heldTaskRoot)), ref) {
		t.Fatalf("after the failed take %s was told %d times and the root keeps %s; want it kept and told never",
			ref, told, keptRefs(keptDecisions(t, r.pool, r.records, heldTaskRoot)))
	}
	r.execAll("drop trigger refuse_the_tell on parked_decisions", "drop function refuse_the_tell()")

	// The store answers again: the sweep's observation writes the value again under its number,
	// and the worker asks at that newer revision.
	if err := machine.Handle(r.ctx, aliveObservation(machine)); err != nil {
		t.Fatalf("the sweep's observation of the architect: %v", err)
	}
	machine.Wait()
	rewritten, _ := values.Value(r.token)
	if rewritten.Number != asked.Number || rewritten.Revision <= asked.Revision {
		t.Fatalf("the architect's value after the store answered is %+v, want task %d written again at a newer revision than %d",
			rewritten, asked.Number, asked.Revision)
	}
	if causes := writeCauses(t, log, rewritten.Number); len(causes) != 2 || causes[1] != supervise.TaskValueTakeFailed {
		t.Fatalf("the writes of task %d were logged with causes %q, want the queue's and then %q", rewritten.Number, causes, supervise.TaskValueTakeFailed)
	}
	answer, err = r.takeAt(machine, rewritten)
	if err != nil || answer.Outcome != supervise.TakeOutcomeAccepted || !strings.Contains(answer.Text, ref) {
		t.Fatalf("the take at the newer revision = %+v, %v; want it accepted with a text naming %s", answer, err, ref)
	}
	if told := r.told(ref); told != 1 {
		t.Fatalf("%s was told %d times by the accepted take, want once", ref, told)
	}

	// The root's next generation: its start task names what the root still keeps, which is not
	// the decision the take told.
	r.readmit()
	var task string
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		task, _, err = r.engine.ParkedDecisionTask(r.ctx, tx, r.root)
		return err
	}); err != nil {
		t.Fatalf("compose the next start's task: %v", err)
	}
	if strings.Contains(task, ref) {
		t.Fatalf("the root's next start task %q names %s again, which the accepted take told", task, ref)
	}
	if told := r.told(ref); told != 1 {
		t.Fatalf("%s was told %d times by the next start, want the take's one telling alone", ref, told)
	}
}

// execAll runs each statement on the rig's database, failing the test on the first that fails.
func (r *heldTaskRig) execAll(statements ...string) {
	r.t.Helper()
	for _, statement := range statements {
		if _, err := r.pool.Exec(r.ctx, statement); err != nil {
			r.t.Fatalf("%s: %v", statement, err)
		}
	}
}

// takeAt is the architect's worker taking value at its revision, as its plugin asks.
func (r *heldTaskRig) takeAt(machine *supervise.Machine, value supervise.TaskValue) (supervise.TakeAnswer, error) {
	c := machine.Claim()
	answer, err := machine.Take(r.ctx, supervise.RequestTake{Claim: r.token, Generation: c.Generation, Session: c.Session,
		Number: value.Number, Revision: supervise.TaskRevision{Epoch: value.Epoch, Revision: value.Revision}})
	machine.Wait()
	return answer, err
}

// told is how many times the store deleted ref as told (told_decisions).
func (r *heldTaskRig) told(ref string) int {
	r.t.Helper()
	var told int
	if err := r.pool.QueryRow(r.ctx, "select count(*) from told_decisions where ref = $1", ref).Scan(&told); err != nil {
		r.t.Fatalf("count the tellings of %s: %v", ref, err)
	}
	return told
}

// writeCauses is the cause of each value write of task the machine logged, in order.
func writeCauses(t *testing.T, log *testlog.Buffer, task uint64) []string {
	t.Helper()
	var causes []string
	for _, line := range strings.Split(log.String(), "\n") {
		var entry struct {
			Msg   string `json:"msg"`
			Task  uint64 `json:"task"`
			Cause string `json:"cause"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "supervise: a task value was written" && entry.Task == task {
			causes = append(causes, entry.Cause)
		}
	}
	return causes
}
