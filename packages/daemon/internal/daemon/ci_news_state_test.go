package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// ciStateRig is ciRig with ciIssue on a plan whose one task, a, takes a fix and stands at its
// review, the reviewer's step at entry 4, ciHeadX holding k alone, at Dispatch's sequence of
// serial 1, and no prompt pending, and the workflow's engine that applies Dispatch's state events
// of the issue (its checkState).
type ciStateRig struct {
	*ciRig
	engine *workflow.Engine
	serial int64
}

func newCIStateRig(t *testing.T) *ciStateRig {
	t.Helper()
	r := newCIRig(t, phase.Reviewing)
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		records := r.runner.records
		v := record.PlanVersion{Issue: ciIssue, Generation: 1, Version: 1, ArtifactID: "plan-" + ciIssue, Source: record.PlanSourceOwner,
			GivenBy: "owner", GivenAt: time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)}
		tasks := []record.PlanTask{{Issue: ciIssue, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.", State: record.TaskWaiting, FixesCI: true}}
		steps := []record.PlanStep{
			{Issue: ciIssue, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: ciIssue, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"}}
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		if err := records.MoveTask(ctx, tx, ciIssue, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning); err != nil {
			return err
		}
		seq := checkstatetest.Seq(1)
		if _, err := records.PutCheckSet(ctx, tx, ciIssue, "acme/widgets", ciHeadX, []string{"k"}, seq); err != nil {
			return err
		}
		_, err := records.PutCheckState(ctx, tx, record.CheckState{Issue: ciIssue, Repo: "acme/widgets", Head: ciHeadX, Check: "k",
			State: "yes", Roles: []string{"tester"}, Seq: seq})
		return err
	})
	r.entry(claim.RoleReviewer, 4)
	engine := workflow.New(r.runner.records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	return &ciStateRig{ciRig: r, engine: engine, serial: 1}
}

// setOf applies Dispatch's set event of ciIssue at ciHeadX holding checks, at the serial after the
// rig's last, through intake.ApplyFact and the engine, as the live consumer applies it.
func (s *ciStateRig) setOf(checks ...string) {
	s.t.Helper()
	s.serial++
	checkstatetest.Apply(s.t, s.pool, ciIssue, "acme/widgets", ciHeadX, s.serial, []intake.DispatchCheckState{{Checks: append([]string{}, checks...)}}, s.engine)
}

// stored is the payload of each ci_news row the outbox holds, as stored, in id order.
func (s *ciStateRig) stored() []record.OutboxRow {
	s.t.Helper()
	rows, err := s.pool.Query(context.Background(), `select id, kind, issue, payload from outbox where kind = 'ci_news' order by id`)
	if err != nil {
		s.t.Fatalf("read the CI news rows: %v", err)
	}
	collected, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var o record.OutboxRow
		err := row.Scan(&o.ID, &o.Kind, &o.Issue, &o.Payload)
		return o, err
	})
	if err != nil {
		s.t.Fatalf("read the CI news rows: %v", err)
	}
	return collected
}

// emptySetOf fails t unless payload, encoded, names a present set of no checks: the member
// `checks` is `[]`.
func emptySetOf(t *testing.T, what string, payload []byte) {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		t.Fatalf("read %s's members: %v", what, err)
	}
	if got, ok := members["checks"]; !ok || string(got) != "[]" {
		t.Fatalf("%s names checks %q (present %t), want a present empty set", what, got, ok)
	}
}

// The removal of the head's last check reaches its current reader as a present set of no checks:
// Dispatch's set event that takes k out of ciHeadX, applied through the workflow's real handler,
// queues one prompt to read Dispatch for the reviewer, whose stored payload names `checks: []`,
// which the strict decoder takes as a present empty set, and which encodes so again; the outbox
// delivers it once to the reviewer, under the row's key. The same empty set again changes nothing
// and prompts nobody. Its red is the empty set dropped by the producer (the workflow's checkState):
// the removal then queues no prompt, the replacement lost.
func TestRemovalOfTheLastCheckDeliversAnEmptySet(t *testing.T) {
	t.Parallel()
	t.Run("the removal is a present empty set, delivered once", func(t *testing.T) {
		t.Parallel()
		s := newCIStateRig(t)
		s.setOf()
		rows := s.stored()
		if len(rows) != 1 {
			t.Fatalf("the prompts after the removal of k = %d rows, want one", len(rows))
		}
		emptySetOf(t, "the stored prompt", rows[0].Payload)
		decoded, err := record.DecodeOutboxPayload(rows[0])
		if err != nil {
			t.Fatalf("decode the stored prompt strictly: %v", err)
		}
		prompt, ok := decoded.(record.CINews)
		if !ok || prompt.Purpose != record.CINewsState || prompt.Checks == nil || len(*prompt.Checks) != 0 || len(prompt.States) != 0 ||
			prompt.Reader == nil || *prompt.Reader != (record.CIReader{Phase: phase.Reviewing, Entry: 4}) || prompt.Head != ciHeadX ||
			prompt.Role != string(mustClaimToken(t, ciIssue, claim.RoleReviewer)) {
			t.Fatalf("the decoded prompt = %+v, want the reviewer's prompt at %s with a present empty set and no snapshot", decoded, ciHeadX)
		}
		encoded, err := json.Marshal(prompt)
		if err != nil {
			t.Fatalf("encode the decoded prompt: %v", err)
		}
		emptySetOf(t, "the prompt encoded again", encoded)
		if again, err := record.DecodeOutboxPayload(record.OutboxRow{ID: rows[0].ID, Kind: record.OutboxKindCINews, Issue: ciIssue, Payload: encoded}); err != nil ||
			again.(record.CINews).Checks == nil || len(*again.(record.CINews).Checks) != 0 {
			t.Fatalf("the prompt encoded again decodes = %+v, %v; want a present empty set", again, err)
		}
		s.attempt(time.Minute)
		if _, delivered := s.publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != ciTopic(t, claim.RoleReviewer) ||
			delivered[0].key != record.OutboxKey(rows[0].ID) || s.left(record.OutboxKindCINews) != 0 {
			t.Fatalf("the delivery = %+v with %d rows left, want one publish to the reviewer under the row's key and the row finished",
				delivered, s.left(record.OutboxKindCINews))
		}

		s.setOf()
		if rows := s.stored(); len(rows) != 0 {
			t.Fatalf("the prompts after the same empty set again = %d rows, want none", len(rows))
		}
	})
}
