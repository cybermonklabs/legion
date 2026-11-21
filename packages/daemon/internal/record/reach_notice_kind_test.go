package record

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// kindsTheReleaseBeforeRuns is every outbox kind the release before this change lists on a lane:
// the lanes of cybermonklabs/legion's main at 545b211633, this change's base. That release's outbox
// runs a row only through a lane that lists its kind (its UnfinishedLanes leaves out a kind it has
// no lane for, and each lane lists due rows of its own kinds alone), so a row of any other kind is
// one it never selects, decodes or retries. The list is written out because it is that release's,
// and this one's lanes changing does not change it.
var kindsTheReleaseBeforeRuns = []OutboxKind{
	"supervise", "issue_suspend", "issue_bundle", "issue_release", "linger_close", "workspace_remove",
	"gate_seed", "issue_branch", "check_wake", "check_look", "check_bound",
	"dispatch_status", "dispatch_message", "notice", "controller_notice", "role_message",
	"gated_steps_publish", "design_gates_publish",
	"phase_ended", "hook_prompt", "update_ask", "merge_queue_read", "merge_queue_removal",
	"claim_release",
}

// reachNotices is one of each notice this change adds, as its writers fill it: the controller's
// seven and the architect's one. The release before decodes none of them: it knows none of the
// kinds, and its decoder refuses the claim and unreachable fields (DisallowUnknownFields).
func reachNotices(at time.Time) []OutboxPayload {
	return []OutboxPayload{
		ControllerNotice{Kind: UnreachableClaimsNotice, Unreachable: []UnreachableClaim{
			{Claim: "legion-LEGION-836-implementer", Issue: "LEGION-836", Role: claim.RoleImplementer, State: "working", LastSeen: at},
		}},
		ControllerNotice{Kind: EnvoyNoLiveSessionNotice, Reason: "Envoy has listed no live session for 20 minutes"},
		ControllerNotice{Kind: RelaunchReadyNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "its background job has ended, so relaunch would now reach it"},
		ControllerNotice{Kind: RelaunchGuardHeldNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "the guard has refused its relaunch for 60 minutes; this is a case for a person"},
		ControllerNotice{Kind: RelaunchNotBackNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "it is still unreachable 20 minutes after its relaunch; this is a case for a person"},
		ControllerNotice{Kind: UnreachableExitNotice, Role: claim.RolePlanner, Claim: "legion-LEGION-836-planner",
			Reason: "its process exits at its Envoy role claim; this is a case for a person"},
		ControllerNotice{Kind: WorkerLockedOutNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "generation 7 has been refused the worker stream for 5 minutes; generation 6 is still connected"},
		Notice{Kind: ClaimOutOfReachNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "its session's last registration with Envoy is more than 20 minutes old"},
	}
}

// A notice the release before cannot decode is queued under an outbox kind that release lists on
// no lane, so a daemon of it — one a rollback runs, or the old half of a rolling restart — leaves
// the row where it stands, and this release's board lane delivers it, where it delivers every
// other notice. Every other notice keeps the kind that release runs, so a rollback still delivers
// them.
func TestANoticeTheReleaseBeforeCannotDecodeIsQueuedUnderAKindItNeverRuns(t *testing.T) {
	at := time.Date(2026, 11, 16, 8, 20, 0, 0, time.UTC)
	for _, payload := range reachNotices(at) {
		row, err := NewOutboxRow("LEGION-836", payload, at)
		if err != nil {
			t.Fatalf("NewOutboxRow(%#v): %v", payload, err)
		}
		if slices.Contains(kindsTheReleaseBeforeRuns, row.Kind) {
			t.Errorf("a %T of kind %s is queued as %s, a kind the release before runs and cannot decode it under", payload, noticeKindOf(payload), row.Kind)
		}
		if lane := LaneOf(row.Kind); lane != LaneBoard {
			t.Errorf("a %T of kind %s is queued as %s on lane %q, want the board lane every notice runs on", payload, noticeKindOf(payload), row.Kind, lane)
		}
		decoded, err := DecodeOutboxPayload(OutboxRow{ID: 7, Kind: row.Kind, Payload: row.Payload})
		if err != nil {
			t.Errorf("decode a %s row back: %v", row.Kind, err)
			continue
		}
		if !reflect.DeepEqual(decoded, payload) {
			t.Errorf("a %s row decodes to %#v, want %#v", row.Kind, decoded, payload)
		}
	}
	for _, tc := range []struct {
		payload OutboxPayload
		want    OutboxKind
	}{
		{Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Summary: "done"}, "notice"},
		{ControllerNotice{Kind: TickNotice}, "controller_notice"},
		{ControllerNotice{Kind: ProviderErrorsNotice, Role: claim.RoleImplementer, Reason: "6 turns in a row ended in a provider error"}, "controller_notice"},
	} {
		row, err := NewOutboxRow("LEGION-836", tc.payload, at)
		if err != nil {
			t.Fatalf("NewOutboxRow(%#v): %v", tc.payload, err)
		}
		if row.Kind != tc.want {
			t.Errorf("a %T of kind %s is queued as %s, want %s, the kind the release before delivers it under", tc.payload, noticeKindOf(tc.payload), row.Kind, tc.want)
		}
	}
}

// A row's kind and its notice's kind agree, or the row does not decode: a notice of this change
// under the ordinary notice kinds is a row the release before would select and fail on, so no
// writer can store one there, and no ordinary notice stands under the kinds the release before
// never runs.
func TestANoticeDecodesOnlyUnderTheKindItIsQueuedAs(t *testing.T) {
	at := time.Date(2026, 11, 16, 8, 20, 0, 0, time.UTC)
	for _, payload := range reachNotices(at) {
		row, err := NewOutboxRow("LEGION-836", payload, at)
		if err != nil {
			t.Fatalf("NewOutboxRow(%#v): %v", payload, err)
		}
		ordinary := OutboxKindNotice
		if _, ok := payload.(ControllerNotice); ok {
			ordinary = OutboxKindControllerNotice
		}
		if _, err := DecodeOutboxPayload(OutboxRow{ID: 7, Kind: ordinary, Payload: row.Payload}); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("a %s notice under %s decoded with %v, want the kinds' mismatch refused", noticeKindOf(payload), ordinary, err)
		}
	}
	tick, err := json.Marshal(ControllerNotice{Kind: TickNotice})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeOutboxPayload(OutboxRow{ID: 7, Kind: OutboxKindReachControllerNotice, Payload: tick}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a tick under %s decoded with %v, want the kinds' mismatch refused", OutboxKindReachControllerNotice, err)
	}
}

// The reads that decide whether a controller notice still waits unsent find the notices of this
// change under their own kind, so a second unreachable-claims list is not queued behind the first
// and a claim's own notice is one per claim, as the reads do for every other kind; and a pending
// tick is still found under its own.
func TestAControllerNoticeIsPendingUnderTheKindItIsQueuedAs(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	inTx(t, st, func(tx pgx.Tx) {
		for _, queued := range []struct {
			issue   string
			payload OutboxPayload
		}{
			{"LEGION", ControllerNotice{Kind: UnreachableClaimsNotice, Unreachable: []UnreachableClaim{{Claim: "legion-legion-209-implementer"}}}},
			{"LEGION-209", ControllerNotice{Kind: RelaunchReadyNotice, Role: claim.RoleImplementer, Claim: "legion-legion-209-implementer", Reason: "r"}},
			{"LEGION", ControllerNotice{Kind: TickNotice}},
		} {
			row, err := NewOutboxRow(queued.issue, queued.payload, now)
			must(t, err)
			must(t, records.Enqueue(ctx, tx, row))
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		for _, tc := range []struct {
			kind  NoticeKind
			issue string
			role  claim.Role
			want  bool
		}{
			{UnreachableClaimsNotice, "", "", true},
			{RelaunchReadyNotice, "LEGION-209", claim.RoleImplementer, true},
			{RelaunchReadyNotice, "LEGION-210", claim.RoleImplementer, false},
			{TickNotice, "", "", true},
		} {
			pending, err := records.ControllerNoticePending(ctx, tx, "LEGION", tc.kind, tc.issue, tc.role)
			must(t, err)
			if pending != tc.want {
				t.Errorf("ControllerNoticePending(%s, %q, %q) = %t, want %t", tc.kind, tc.issue, tc.role, pending, tc.want)
			}
		}
		pending, err := records.ControllerNoticesPending(ctx, tx, "LEGION", RelaunchReadyNotice, []string{"LEGION-209", "LEGION-210"}, claim.RoleImplementer)
		must(t, err)
		if _, ok := pending["LEGION-209"]; !ok || len(pending) != 1 {
			t.Errorf("ControllerNoticesPending(relaunch-ready, LEGION-209 and LEGION-210) = %v, want LEGION-209 alone", pending)
		}
	})
}

// The architect's notice of a claim out of reach is one of its tree's notices like any other: a
// later notice of the tree waits behind it (EarlierNotices), and a ready of the architect releases
// it from its backoff with the rest (WaitingNotices).
func TestTheArchitectsReachNoticeIsAmongItsTreesNotices(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var reach, later OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issueFixture("LEGION-209")))
		var err error
		reach, err = NewOutboxRow("LEGION-209", Notice{Kind: ClaimOutOfReachNotice, Role: claim.RoleImplementer,
			Claim: "legion-legion-209-implementer", Reason: "r"}, now.Add(time.Minute))
		must(t, err)
		later, err = NewOutboxRow("LEGION-209", Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Summary: "done"}, now)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, reach))
		must(t, records.Enqueue(ctx, tx, later))
	})
	inTx(t, st, func(tx pgx.Tx) {
		var laterID int64
		must(t, tx.QueryRow(ctx, "select max(id) from outbox where issue = 'LEGION-209'").Scan(&laterID))
		earlier, err := records.EarlierNotices(ctx, tx, "LEGION-209", laterID)
		must(t, err)
		if len(earlier) != 1 || earlier[0].Kind != reach.Kind {
			t.Errorf("the notices LEGION-209's later notice waits behind = %v, want its %s row", outboxKeys(earlier), reach.Kind)
		}
		waiting, err := records.WaitingNotices(ctx, tx, "LEGION", now)
		must(t, err)
		if len(waiting) != 1 || waiting[0].Kind != reach.Kind {
			t.Errorf("LEGION's waiting notices = %v, want its %s row, due in a minute", outboxKeys(waiting), reach.Kind)
		}
	})
}

// noticeKindOf is the notice kind a payload carries, for a failure's words.
func noticeKindOf(payload OutboxPayload) NoticeKind {
	switch value := payload.(type) {
	case Notice:
		return value.Kind
	case ControllerNotice:
		return value.Kind
	}
	return ""
}
