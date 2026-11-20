package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// told is one notice the workflow queued, whether for the issue's architect or for the
// controller, as the rig reads it: its kind and the `detail` of the failure record beside it,
// which is where the sentence the failure wrote stands (record.NoticeFailure; a notice with no
// record reads "").
type told struct{ kind, detail string }

// notices are the notices the workflow has queued, in the order it queued them.
func (r *freshSessionRig) notices() []told {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), "select id, kind, issue, payload from outbox where kind in ('notice', 'controller_notice') order by id")
	if err != nil {
		r.t.Fatalf("read the notice rows: %v", err)
	}
	defer rows.Close()
	var notices []told
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			r.t.Fatalf("scan a notice row: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.t.Fatalf("decode notice row %d: %v", row.ID, err)
		}
		switch notice := payload.(type) {
		case record.Notice:
			notices = append(notices, told{kind: string(notice.Kind), detail: failureDetail(notice.Failure)})
		case record.ControllerNotice:
			notices = append(notices, told{kind: string(notice.Kind) + " (controller)", detail: failureDetail(notice.Failure)})
		default:
			r.t.Fatalf("outbox row %d is no notice: %#v", row.ID, payload)
		}
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the notice rows: %v", err)
	}
	return notices
}

// failureDetail is the sentence a notice's failure record carries, and "" for a notice with no
// record.
func failureDetail(failure *record.NoticeFailure) string {
	if failure == nil {
		return ""
	}
	return failure.Detail
}

// A refused turn whose provider error cannot clear fails the claim, and the workflow holds the
// issue for its architect: held from the phase it was in, with the architect's and the
// controller's held notices and the worker-died notice, each carrying the failure's sentence. Two
// reports decide such a turn, so each is driven through the daemon's own workflow here: the prompt
// result of the turn the phase task's prompt ran, and the turn-outcome report, with its status, of
// a turn no daemon prompt ran — the phase reminder's, which follows a turn that ended well. The
// supervise tests of the same decisions have no workflow, so this is where the hold is held.
func TestARefusal400HoldsTheIssueForItsArchitect(t *testing.T) {
	const failure = "a provider error that cannot clear ended this claim's turn: a provider error (HTTP 400)"
	for name, refuse := range map[string]func(*freshSessionRig){
		"the prompt result of the phase task's turn": func(r *freshSessionRig) {
			// The ready sent the phase task; its prompt is acknowledged once the send is over.
			r.machine.Wait()
			delivery := r.machine.Claim().Pending.ID
			r.must(supervise.StreamTurnStart{Claim: r.implementer})
			r.must(supervise.StreamTurnEnd{Claim: r.implementer})
			r.must(supervise.StreamPromptResult{Claim: r.implementer, DeliveryID: delivery, AgentInvoked: true,
				Error: "invalid_request_error", HTTPStatus: 400})
		},
		"the report of the phase reminder's turn": func(r *freshSessionRig) {
			r.machine.Wait()
			delivery := r.machine.Claim().Pending.ID
			r.must(supervise.StreamTurnStart{Claim: r.implementer})
			r.must(supervise.StreamTurnEnd{Claim: r.implementer})
			r.must(supervise.StreamPromptResult{Claim: r.implementer, DeliveryID: delivery, AgentInvoked: true})
			r.machine.Wait()
			if got := r.machine.Claim(); got.State != supervise.StateIdle || got.Pending != nil {
				r.t.Fatalf("after its task's turn the implementer is %s holding %+v, want it idle and owing nothing", got.State, got.Pending)
			}
			generation := r.machine.Claim().Generation
			r.must(supervise.RequestTurnOutcome{Claim: r.implementer, Generation: generation, Sequence: 1})
			r.must(supervise.StreamTurnStart{Claim: r.implementer})
			r.must(supervise.RequestTurnOutcome{Claim: r.implementer, Generation: generation, Sequence: 2, Refused: true,
				Message: "API Error: 400 invalid_request_error: prompt is too long", HTTPStatus: 400})
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newFreshSessionRig(t)

			refuse(rig)
			rig.machine.Wait()

			if got := rig.machine.Claim(); got.State != supervise.StateFailed || got.FailureCause != supervise.CauseProviderError || got.Failure != failure {
				t.Fatalf("the implementer is %s (%q, %q), want it failed with %q (%q)", got.State, got.FailureCause, got.Failure, failure, supervise.CauseProviderError)
			}
			current, from := rig.recordedPhase()
			if current != phase.Held || from != phase.Implementing {
				t.Fatalf("the issue is in %s held from %q, want it held from implementing", current, from)
			}
			var kinds []string
			for _, notice := range rig.notices() {
				kinds = append(kinds, notice.kind)
				if !strings.Contains(notice.detail, failure) {
					t.Errorf("the %s notice's failure detail says %q, want the failure's sentence in it", notice.kind, notice.detail)
				}
			}
			if want := []string{"held", "held (controller)", "worker-died"}; !slices.Equal(kinds, want) {
				t.Fatalf("the failure queued %v, want %v", kinds, want)
			}
		})
	}
}
