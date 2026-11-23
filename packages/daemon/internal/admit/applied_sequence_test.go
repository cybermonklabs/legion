package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A listing summary or a view of an issue admission does not record is older than an issue fact
// of it the daemon applied only when that fact was applied: an event, or a view intake let
// through. A reading that decides nothing is no such fact: the periodic turn's re-read of an issue
// a recorded issue waits on reads Dispatch's last sequence of the issue, and Dispatch raises that
// sequence for a comment, a document edit, an ask or a claim, none of which reaches admission as
// an issue fact. So after the turn read the issue at 6 with no issue fact of it applied, a summary
// or a view at 5 still records it: a listing the consumer has caught up with (the boot's or a
// recovery's), a summary Reconcile held that the consumer's position releases, and a view of an
// admission event. Before, the turn's reading raised the sequence each of the three was compared
// with, so each recorded nothing, and nothing listed the issue again.
//
// Control, in the same table: an event at 6 that recorded nothing (its repository unread) holds
// back each of the three at 5.
func TestOnlyAnAppliedIssueFactHoldsBackAnOlderListingOrView(t *testing.T) {
	const key = "LEGION-READ"
	summary := dispatch.IssueSummary{Key: key, ResolvedRepo: "acme/widgets", Title: "read", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 5}
	view := intake.ObservedIssue(dispatch.Issue{Key: key, ResolvedRepo: "acme/widgets", Title: "read", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 5})
	for _, at6 := range []struct {
		name    string
		records bool
		fact    intake.Fact
	}{
		{name: "the turn's re-read", records: true, fact: intake.ControllerTick{Observed: []intake.IssueStatus{
			{Key: key, Project: testProject, Status: "todo", Seq: 6, Labels: []string{dispatch.LegionLabel}, Rank: "A"}}}},
		{name: "an event that recorded nothing", records: false, fact: intake.DispatchIssue{Key: key, ResolvedRepo: "acme/unread", Seq: 6,
			Type: "issue.updated", Status: "todo", Title: "read", Rank: "A", HandedOver: handed}},
	} {
		for _, path := range []struct {
			name string
			// before runs ahead of the fact at 6, and after once it applied.
			before, after func(t *testing.T, pool *pgxpool.Pool, admission *Admission, mirror *blockers.Mirror)
		}{
			{name: "a listing the consumer has caught up with", after: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, _ *blockers.Mirror) {
				reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{summary}, 10, 10, false)
			}},
			{name: "a held summary at its release", before: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, _ *blockers.Mirror) {
				reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{summary}, 10, 0, false)
				if !admission.Held() {
					t.Fatal("Held() = false after a labelled todo summary behind the stream, want it held")
				}
			}, after: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, mirror *blockers.Mirror) {
				applyWith(t, pool, "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 10}, mirror, admission)
			}},
			{name: "a view of an admission event", after: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, mirror *blockers.Mirror) {
				applyWith(t, pool, "the-view", view, mirror, admission)
			}},
		} {
			t.Run(at6.name+", then "+path.name, func(t *testing.T) {
				pool := migratedPool(t)
				admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
				mirror := blockers.NewMirror(record.NewStore(), testProject)
				if path.before != nil {
					path.before(t, pool, admission, mirror)
				}
				applyWith(t, pool, "at-6", at6.fact, mirror, admission)
				if got := maybeIssue(t, pool, key); got != nil {
					t.Fatalf("%s recorded by %s = %#v, want none", key, at6.name, got)
				}
				path.after(t, pool, admission, mirror)
				got := maybeIssue(t, pool, key)
				switch {
				case at6.records && (got == nil || got.Repo != "acme/widgets" || got.LastDispatchSeq != 5):
					t.Fatalf("%s after %s at 6 and %s at 5 = %#v, want it recorded on acme/widgets at 5: no issue fact of it was applied at 6",
						key, at6.name, path.name, got)
				case !at6.records && got != nil:
					t.Fatalf("%s after %s at 6 and %s at 5 = %#v, want it unrecorded: an issue fact of it was applied at 6", key, at6.name, path.name, got)
				}
			})
		}
	}
}

// Reconcile answers the key of each summary it found older than an issue fact of its issue the
// daemon applied, so a recovery keeps that issue in the scope of its next pass, and it finds it so
// whether or not the consumer has reached its target: a stale summary is answered before it could
// be held, since release, which would apply a held one later, answers nothing. A summary no older
// than any applied fact is not answered.
func TestReconcileAnswersEachSummaryOlderThanAnAppliedFact(t *testing.T) {
	const stale, fresh = "LEGION-STALE", "LEGION-FRESH"
	for _, tc := range []struct {
		name             string
		target, ackFloor int64
	}{
		{name: "the consumer caught up", target: 10, ackFloor: 10},
		{name: "the consumer behind", target: 10, ackFloor: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			mirror := blockers.NewMirror(record.NewStore(), testProject)
			applyWith(t, pool, "at-6", intake.DispatchIssue{Key: stale, ResolvedRepo: "acme/unread", Seq: 6, Type: "issue.updated",
				Status: "todo", Title: "stale", Rank: "A", HandedOver: handed}, mirror, admission)
			refused := reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
				{Key: stale, ResolvedRepo: "acme/widgets", Title: "stale", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 5},
				{Key: fresh, ResolvedRepo: "acme/unread", Title: "fresh", Status: "todo", Rank: "B", HandedOver: handed, LastSeq: 5},
			}, tc.target, tc.ackFloor, false)
			if len(refused) != 1 || refused[0] != stale {
				t.Fatalf("Reconcile answered %q, want only %s, whose summary at 5 is older than its event at 6", refused, stale)
			}
			if got := maybeIssue(t, pool, stale); got != nil {
				t.Fatalf("%s recorded from its stale summary = %#v, want none", stale, got)
			}
			admission.mu.Lock()
			_, held := admission.pending[stale]
			admission.mu.Unlock()
			if held {
				t.Fatalf("%s is held for release, want it answered instead: release would refuse it and answer nothing", stale)
			}
		})
	}
}

// applyWith applies fact through the daemon's three handlers in the daemon's order: the copy of
// Dispatch's issues (mirror), the workflow, then admission.
func applyWith(t *testing.T, pool *pgxpool.Pool, id string, fact intake.Fact, mirror *blockers.Mirror, admission *Admission) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", id, fact, mirror, admission.engine, admission); err != nil {
		t.Fatalf("ApplyFact %s: %v", id, err)
	}
}
