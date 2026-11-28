package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// leftRelease is one claim_release row a claim write queued: the issue the row is keyed by and the
// session its payload names.
type leftRelease struct {
	issue   string
	session string
}

// queuedLeftReleases is each claim_release row the outbox holds, in id order. Each payload is
// decoded as the daemon's executor decodes it (record.DecodeOutboxPayload), so a row this package
// writes in a shape the executor cannot read fails here rather than in the claim lane.
func queuedLeftReleases(t *testing.T, s *Store) []leftRelease {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `select id, kind, issue, payload from outbox where kind = $1 order by id`,
		string(record.OutboxKindClaimRelease))
	if err != nil {
		t.Fatalf("read the queued claim releases: %v", err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var r record.OutboxRow
		var kind string
		err := row.Scan(&r.ID, &kind, &r.Issue, &r.Payload)
		r.Kind = record.OutboxKind(kind)
		return r, err
	})
	if err != nil {
		t.Fatalf("read the queued claim releases: %v", err)
	}
	released := []leftRelease{}
	for _, row := range queued {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode the claim release of %s as the executor does: %v", row.Issue, err)
		}
		release, ok := payload.(record.ClaimRelease)
		if !ok || release.Session == "" {
			t.Fatalf("the claim release of %s carries %#v, want the session the write left behind", row.Issue, payload)
		}
		released = append(released, leftRelease{issue: row.Issue, session: release.Session})
	}
	return released
}

// sentLeftReleases is the claim lane's run of every queued claim release: the rows go, as a lane
// that ran each one deletes it.
func sentLeftReleases(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `delete from outbox where kind = $1`, string(record.OutboxKindClaimRelease)); err != nil {
		t.Fatalf("run the queued claim releases: %v", err)
	}
}

// releaseTell is one issue a store told its wake (ReleaseLeftSessionClaims), with the number of
// that issue's claim_release rows another connection read at that moment.
type releaseTell struct {
	issue   string
	visible int
}

// releasingStore is a migrated store whose claim writes queue the release of a session they leave
// behind, as the store of a daemon with a workflow does (ReleaseLeftSessionClaims), with each issue
// it has told its wake since the last reset of told.
func releasingStore(t *testing.T) (s *Store, told *[]releaseTell) {
	t.Helper()
	s = migratedStore(t)
	told = &[]releaseTell{}
	s.ReleaseLeftSessionClaims(func(issue string) {
		var visible int
		if err := s.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1 and issue = $2`,
			string(record.OutboxKindClaimRelease), issue).Scan(&visible); err != nil {
			t.Errorf("read the claim releases of %s when the store told it: %v", issue, err)
		}
		*told = append(*told, releaseTell{issue: issue, visible: visible})
	})
	return s, told
}

// forgotten is c as the machine's forgetSession leaves it: the session it recorded moved to the
// record of the one it abandoned, and no session of its own.
func forgotten(c supervise.Claim) supervise.Claim {
	c.Abandoned = supervise.AbandonedSession{Session: c.Session, SessionFile: c.SessionFile, Why: "a fresh session", At: at(30)}
	c.Session, c.SessionFile = "", ""
	return c
}

// releasedByPause is c suspended as released, as a pause's stop and the idle release suspend a
// claim: its session kept for the launch that resumes it.
func releasedByPause(c supervise.Claim) supervise.Claim {
	c.SuspendedBy = supervise.SuspendedByReleased
	return c
}

// A claim write that moves a claim off a session it recorded leaves that session behind: no claims
// row names it after the write, and while the claim's issue runs, no move's row and no boot row
// (the outbox's releaseClaim) releases the Dispatch claim it holds. So the write queues, in its
// own transaction, one claim_release row keyed by the claim's issue whose payload names the left
// session, and once that transaction commits the store tells the issue (ReleaseLeftSessionClaims),
// where the daemon wakes the issue's claim lane. Every claim write goes through putClaim, so each
// of PutClaim, PutClaimAndDelivery, RetireDelivery, ReleaseHolds, AdmitClaim, PutClaimTx and
// PutClaimAndDeliveryTx queues it for a write that moves the session — a fresh session that clears
// it, as forgetSession does, and a new session registered over it — and none for a write that does
// not: a first registration that fills an empty session, the same session written again, a pause's
// stop and a retirement that keep it, and an operator's release that marks the abandoned session
// released.
//
// A store the daemon has not turned the switch on for queues nothing: only a daemon with a workflow
// runs the outbox that sends the row. Neither does a claim on no issue, the project controller's
// among them, whose row would be keyed by no issue, nor a claim of another project's issue, whose
// row only that project's daemon runs. Each move queues a row of its own, so two moves before the
// lane runs either queue two, and a transaction that rolls back queued nothing and tells nothing.
func TestEveryClaimWriteThatLeavesASessionBehindQueuesItsRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("each claim write", func(t *testing.T) {
		s, told := releasingStore(t)
		openHolderTree(t, s)
		put := func(c supervise.Claim) func() error { return func() error { return s.PutClaim(ctx, c) } }
		admit := func(c supervise.Claim) func() error {
			return func() error {
				_, err := s.AdmitClaim(ctx, c)
				return err
			}
		}
		delivered := func(c supervise.Claim, d supervise.Delivery) func() error {
			return func() error { return s.PutClaimAndDelivery(ctx, c, d) }
		}
		retired := func(c supervise.Claim, id string) func() error {
			return func() error { return s.RetireDelivery(ctx, c, id) }
		}
		releasedNames := func(c supervise.Claim, holds ...supervise.Hold) func() error {
			return func() error {
				_, err := s.ReleaseHolds(ctx, c, holds)
				return err
			}
		}
		// The pause's stop writes in a transaction of the caller's, which the wake waits on.
		putTx := func(c supervise.Claim) func() error {
			return func() error {
				return Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error { return s.PutClaimTx(ctx, tx, c) })
			}
		}
		deliveredTx := func(c supervise.Claim, d supervise.Delivery) func() error {
			return func() error {
				return Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error { return s.PutClaimAndDeliveryTx(ctx, tx, c, d) })
			}
		}
		task := func(id string) supervise.Delivery {
			return supervise.Delivery{ID: id, Task: "test it", QueuedAt: at(1), DeliveredAt: at(2), ConfirmedAt: at(3), Generation: 1}
		}
		releasedHold := forgotten(holderClaim(supervise.StateSuspended, "ses-5"))
		releasedHold.Abandoned.Released = at(40)

		for _, w := range []struct {
			name  string
			write func() error
			// left is the session the write leaves behind, "" for a write that leaves none.
			left string
		}{
			{"AdmitClaim of a claim that has not launched", admit(holderClaim(supervise.StateLaunching, "")), ""},
			{"PutClaim of the first registration, which fills the empty session", put(holderClaim(supervise.StateRegistered, "ses-1")), ""},
			{"PutClaim of the same session again", put(holderClaim(supervise.StateReady, "ses-1")), ""},
			{"PutClaim of a new session registered over the recorded one", put(holderClaim(supervise.StateRegistered, "ses-2")), "ses-1"},
			{"PutClaim of a fresh session, which forgets the recorded one", put(forgotten(holderClaim(supervise.StateQueued, "ses-2"))), "ses-2"},
			{"PutClaim of the registration that fills the forgotten session's place", put(holderClaim(supervise.StateRegistered, "ses-3")), ""},
			{"PutClaimAndDelivery of a confirmed task in the same session", delivered(holderClaim(supervise.StateWorking, "ses-3"), task("delivery-1")), ""},
			{"PutClaimAndDelivery of a fresh session written with its task", delivered(forgotten(holderClaim(supervise.StateQueued, "ses-3")), task("delivery-1")), "ses-3"},
			{"PutClaim of the registration over it", put(holderClaim(supervise.StateRegistered, "ses-4")), ""},
			{"RetireDelivery of a retirement that keeps the session", retired(holderClaim(supervise.StateRetired, "ses-4"), "delivery-1"), ""},
			{"PutClaimAndDelivery of a second task in the same session", delivered(holderClaim(supervise.StateWorking, "ses-4"), task("delivery-2")), ""},
			{"RetireDelivery of a retirement written with a fresh session", retired(forgotten(holderClaim(supervise.StateRetired, "ses-4")), "delivery-2"), "ses-4"},
			{"PutClaim of the registration over the retirement", put(holderClaim(supervise.StateRegistered, "ses-5")), ""},
			// An operator's release writes the claim through putClaim like any write, so one that
			// moved the session would leave it behind too.
			{"ReleaseHolds of a write that moves the session", releasedNames(forgotten(holderClaim(supervise.StateSuspended, "ses-5"))), "ses-5"},
			{"ReleaseHolds of the release that marks the abandoned session released", releasedNames(releasedHold,
				supervise.Hold{Kind: supervise.HoldSessionID, Name: "ses-5"}), ""},
			{"PutClaim of the registration after the release", put(holderClaim(supervise.StateRegistered, "ses-6")), ""},
			{"AdmitClaim that launches the claim again over the session it kept", admit(holderClaim(supervise.StateLaunching, "ses-6")), ""},
			{"AdmitClaim that launches the claim in a fresh session", admit(forgotten(holderClaim(supervise.StateLaunching, "ses-6"))), "ses-6"},
			{"PutClaim of the fresh session's registration", put(holderClaim(supervise.StateRegistered, "ses-7")), ""},
			{"PutClaimTx of a pause's stop, which keeps the session", putTx(releasedByPause(holderClaim(supervise.StateSuspended, "ses-7"))), ""},
			{"PutClaimTx of a fresh session", putTx(forgotten(holderClaim(supervise.StateSuspended, "ses-7"))), "ses-7"},
			{"PutClaim of the registration after it", put(holderClaim(supervise.StateRegistered, "ses-8")), ""},
			{"PutClaimAndDeliveryTx of a pause's stop with its task, which keeps the session",
				deliveredTx(releasedByPause(holderClaim(supervise.StateSuspended, "ses-8")), task("delivery-3")), ""},
			{"PutClaimAndDeliveryTx of a fresh session written with its task",
				deliveredTx(forgotten(holderClaim(supervise.StateSuspended, "ses-8")), task("delivery-3")), "ses-8"},
		} {
			*told = nil
			if err := w.write(); err != nil {
				t.Fatalf("%s: %v", w.name, err)
			}
			wantRows, wantTold := []leftRelease{}, []releaseTell(nil)
			if w.left != "" {
				wantRows = []leftRelease{{issue: holderIssue, session: w.left}}
				wantTold = []releaseTell{{issue: holderIssue, visible: 1}}
			}
			if got := queuedLeftReleases(t, s); !slices.Equal(got, wantRows) {
				t.Fatalf("%s queued the claim releases %+v, want %+v", w.name, got, wantRows)
			}
			if !slices.Equal(*told, wantTold) {
				t.Fatalf("%s told %+v, want %+v: the issue once, with its row committed, for a write that left a session", w.name, *told, wantTold)
			}
			sentLeftReleases(t, s)
		}
	})

	t.Run("a store the daemon has not turned the release on for", func(t *testing.T) {
		for _, releases := range []bool{false, true} {
			s := migratedStore(t)
			if releases {
				s.ReleaseLeftSessionClaims(func(string) {})
			}
			openHolderTree(t, s)
			if _, err := s.AdmitClaim(ctx, holderClaim(supervise.StateLaunching, "")); err != nil {
				t.Fatalf("admit the claim: %v", err)
			}
			for _, c := range []supervise.Claim{holderClaim(supervise.StateRegistered, "ses-1"), forgotten(holderClaim(supervise.StateQueued, "ses-1"))} {
				if err := s.PutClaim(ctx, c); err != nil {
					t.Fatalf("write the %s claim: %v", c.State, err)
				}
			}
			want := []leftRelease{}
			if releases {
				want = []leftRelease{{issue: holderIssue, session: "ses-1"}}
			}
			if got := queuedLeftReleases(t, s); !slices.Equal(got, want) {
				t.Fatalf("a registration and a fresh session on a store that releases: %t queued %+v, want %+v", releases, got, want)
			}
		}
	})

	t.Run("a claim on no issue", func(t *testing.T) {
		s, told := releasingStore(t)
		controller := supervise.Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController,
			Generation: 1, State: supervise.StateRegistered, Session: "ses-controller-1"}
		noIssue := supervise.Claim{Token: "legion-operator-tree-implementer", Project: "legion", Tree: "operator-tree",
			Role: claim.RoleImplementer, Generation: 1, State: supervise.StateRegistered, Session: "ses-operator-1"}
		for _, c := range []supervise.Claim{controller, noIssue} {
			moved := c
			moved.Session = c.Session + "-next"
			for _, write := range []supervise.Claim{c, moved, forgotten(moved)} {
				if err := s.PutClaim(ctx, write); err != nil {
					t.Fatalf("write %s with session %q: %v", write.Token, write.Session, err)
				}
			}
		}
		if got := queuedLeftReleases(t, s); len(got) != 0 {
			t.Fatalf("the moves of the controller's claim and of a claim on no issue queued %+v, want nothing", got)
		}
		if len(*told) != 0 {
			t.Fatalf("the moves of the controller's claim and of a claim on no issue told %+v, want nothing", *told)
		}
	})

	t.Run("a claim of another project's issue", func(t *testing.T) {
		s, told := releasingStore(t)
		spawn := func(state supervise.ClaimState, session string) supervise.Claim {
			return supervise.Claim{Token: "legion-legion-otherproject-9-architect", Project: "legion", Tree: "OTHERPROJECT-9", Issue: "OTHERPROJECT-9",
				Role: claim.RoleArchitect, Generation: 1, State: state, Session: session}
		}
		for _, c := range []supervise.Claim{
			spawn(supervise.StateRegistered, "ses-1"), spawn(supervise.StateRegistered, "ses-2"),
			forgotten(spawn(supervise.StateQueued, "ses-2")),
		} {
			if err := s.PutClaim(ctx, c); err != nil {
				t.Fatalf("write the claim of OTHERPROJECT-9 with session %q: %v", c.Session, err)
			}
		}
		if got := queuedLeftReleases(t, s); len(got) != 0 {
			t.Fatalf("the moves of the claim of another project's issue queued %+v, want nothing: only that project's daemon runs a row keyed by its issue", got)
		}
		if len(*told) != 0 {
			t.Fatalf("the moves of the claim of another project's issue told %+v, want nothing", *told)
		}
	})

	t.Run("two moves of one claim before the lane runs", func(t *testing.T) {
		s, told := releasingStore(t)
		openHolderTree(t, s)
		if _, err := s.AdmitClaim(ctx, holderClaim(supervise.StateLaunching, "")); err != nil {
			t.Fatalf("admit the claim: %v", err)
		}
		for _, c := range []supervise.Claim{
			holderClaim(supervise.StateRegistered, "ses-1"), holderClaim(supervise.StateRegistered, "ses-2"),
			forgotten(holderClaim(supervise.StateQueued, "ses-2")),
		} {
			if err := s.PutClaim(ctx, c); err != nil {
				t.Fatalf("write the claim with session %q: %v", c.Session, err)
			}
		}
		want := []leftRelease{{issue: holderIssue, session: "ses-1"}, {issue: holderIssue, session: "ses-2"}}
		if got := queuedLeftReleases(t, s); !slices.Equal(got, want) {
			t.Fatalf("two moves of one claim queued %+v, want a row for each session left behind, %+v", got, want)
		}
		if wantTold := []releaseTell{{issue: holderIssue, visible: 1}, {issue: holderIssue, visible: 2}}; !slices.Equal(*told, wantTold) {
			t.Fatalf("two moves of one claim told %+v, want %+v: the issue once for each commit", *told, wantTold)
		}
	})

	t.Run("a transaction that rolls back", func(t *testing.T) {
		s, told := releasingStore(t)
		openHolderTree(t, s)
		if _, err := s.AdmitClaim(ctx, holderClaim(supervise.StateLaunching, "")); err != nil {
			t.Fatalf("admit the claim: %v", err)
		}
		if err := s.PutClaim(ctx, holderClaim(supervise.StateRegistered, "ses-1")); err != nil {
			t.Fatalf("the registration: %v", err)
		}
		rolledBack := errors.New("the caller's transaction fails after the claim write")
		err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
			if err := s.PutClaimTx(ctx, tx, forgotten(holderClaim(supervise.StateSuspended, "ses-1"))); err != nil {
				return err
			}
			return rolledBack
		})
		if !errors.Is(err, rolledBack) {
			t.Fatalf("the rolled-back fresh session answered %v, want %v", err, rolledBack)
		}
		if got := queuedLeftReleases(t, s); len(got) != 0 {
			t.Fatalf("the rolled-back fresh session left the claim releases %+v, want none", got)
		}
		if len(*told) != 0 {
			t.Fatalf("the rolled-back fresh session told %+v, want nothing: it queued nothing that stands", *told)
		}
		if stored := onlyClaim(t, s); stored.Session != "ses-1" {
			t.Fatalf("after the rollback the claim names session %q, want ses-1, the session it kept", stored.Session)
		}
	})
}
