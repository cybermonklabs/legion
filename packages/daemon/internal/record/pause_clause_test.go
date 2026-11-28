package record

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// pauseQueued is one row the pause clause's tests queue: the issue it is for, its payload, whether
// a pause of its project holds it, and the claim the state names for it when it is held.
type pauseQueued struct {
	name    string
	issue   string
	payload OutboxPayload
	held    bool
	claim   claim.Token
}

// pauseQueue is a row of every kind a project's pause holds, each on an issue of its own so no
// ordering rule or lane rule of one issue holds another's, beside the rows it does not hold: the
// controller's notice, a suspend, a pause's own stop, and two rows of another project on the same
// database.
var pauseQueue = []pauseQueued{
	{name: "a start", issue: "LEGION-301", held: true, claim: "legion-legion-legion-301-implementer",
		payload: SuperviseRequest{Op: "start", Tree: "LEGION-301", Role: claim.RoleImplementer, Generation: 1, Task: "implement it"}},
	{name: "an architect's notice", issue: "LEGION-302", held: true,
		payload: Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-320 is done"}},
	{name: "a merge forward's ask", issue: "LEGION-303", held: true, claim: "legion-legion-legion-303-implementer",
		payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Testing, PullRequest: 42, Why: ConflictCheckNotGreen}},
	{name: "a hook's words", issue: "LEGION-304", held: true, claim: "legion-legion-legion-304-implementer",
		payload: HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: 1, Prompt: "Read the thread."}},
	{name: "a role message", issue: "LEGION-305", held: true, claim: "legion-legion-legion-305-reviewer",
		payload: RoleMessage{Role: "legion-legion-legion-305-reviewer", Body: "Pull request #42: CI settled at c0de."}},
	{name: "an update ask", issue: "LEGION-306", held: true, claim: "legion-legion-legion-306-architect",
		payload: UpdateAsk{Role: claim.RoleArchitect, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Text: "Push your work."}},
	{name: "a merge-queue message", issue: "LEGION-307", held: true, claim: "legion-legion-legion-307-merger",
		payload: MergeQueueRemoval{Number: 42, Head: "c0de"}},
	{name: "a merge-holds change", issue: "LEGION-313", held: true, claim: "legion-legion-legion-313-merger",
		payload: MergeHoldsChanged{Project: "LEGION"}},
	{name: "a check's wake", issue: "LEGION-308", held: true, payload: CheckWakeDue{Look: 1, Reason: "the tester's own wake"}},
	{name: "a check's look bound", issue: "LEGION-309", held: true, payload: CheckLook{Look: 1}},
	{name: "a check's bound", issue: "LEGION-310", held: true, payload: CheckBound{}},
	{name: "the controller's notice", issue: "LEGION", payload: ControllerNotice{Kind: TickNotice}},
	{name: "a suspend", issue: "LEGION-311",
		payload: SuperviseRequest{Op: "suspend", Tree: "LEGION-311", Role: claim.RoleImplementer, Generation: 1, Reason: "parked"}},
	{name: "a pause's stop", issue: "LEGION-312",
		payload: SuperviseRequest{Op: "pause_stop", Tree: "LEGION-312", Role: claim.RoleTester, Generation: 1, Pause: 1}},
	{name: "another project's notice", issue: "INFRA-5",
		payload: Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "INFRA-6 is done"}},
	{name: "another project's start", issue: "INFRA-6",
		payload: SuperviseRequest{Op: "start", Tree: "INFRA-6", Role: claim.RoleImplementer, Generation: 1, Task: "implement it"}},
}

// While LEGION is paused, no row of a kind its pause holds is listed by DueRows or claimed by
// ClaimRow, and the refused claim leaves the row as it was: no lease, no attempt spent, its place
// kept. The controller's notice, a suspend, the pause's own stop and every row of another project
// on the same database are listed and claimed as before. PauseHeldRows lists exactly the held
// rows with the claim each is for, the resume reads the same list in its own transaction and
// marks each of those rows, and no other, with its pause (MarkPauseHeldRows), and once the pause
// has ended every held row is listed and claimed again.
func TestAPausedProjectsHeldRowsAreNotClaimable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	st := migratedStore(t)
	records := NewStore()

	ids := make([]int64, len(pauseQueue))
	inTx(t, st, func(tx pgx.Tx) {
		for i, queued := range pauseQueue {
			row, err := NewFencedRow(ctx, tx, records, queued.issue, queued.payload, now)
			must(t, err)
			ids[i], err = records.EnqueueID(ctx, tx, row)
			must(t, err)
		}
	})
	idsWhere := func(keep func(pauseQueued) bool) []int64 {
		var out []int64
		for i, queued := range pauseQueue {
			if keep(queued) {
				out = append(out, ids[i])
			}
		}
		return out
	}
	due := func(project string) []int64 {
		t.Helper()
		var out []int64
		inTx(t, st, func(tx pgx.Tx) {
			rows, err := records.DueRows(ctx, tx, project, "", nil, now)
			must(t, err)
			for _, row := range rows {
				out = append(out, row.ID)
			}
		})
		slices.Sort(out)
		return out
	}
	claimRow := func(id int64) bool {
		t.Helper()
		var ok bool
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			_, ok, err = records.ClaimRow(ctx, tx, id, now, time.Minute)
			must(t, err)
		})
		return ok
	}

	pause, asked, err := st.Pause(ctx, store.PauseRequest{Project: "LEGION", By: store.PauseActor{Kind: store.PauseByOperator},
		Reason: "the model provider is down", At: now,
		StopRow: func(store.Pause, store.PauseStopClaim) (store.PauseRow, error) {
			return store.PauseRow{}, errors.New("this project has no claim to stop")
		}})
	must(t, err)
	if len(asked) != 0 {
		t.Fatalf("the pause asked to stop %+v, want nothing: the project has no claim", asked)
	}

	legionFree := idsWhere(func(q pauseQueued) bool { return !q.held && q.issue != "INFRA-5" && q.issue != "INFRA-6" })
	if got := due("LEGION"); !reflect.DeepEqual(got, legionFree) {
		t.Fatalf("DueRows of LEGION while it is paused = %v, want the rows the pause does not hold %v", got, legionFree)
	}
	infra := idsWhere(func(q pauseQueued) bool { return q.issue == "INFRA-5" || q.issue == "INFRA-6" })
	if got := due("INFRA"); !reflect.DeepEqual(got, infra) {
		t.Fatalf("DueRows of INFRA while LEGION is paused = %v, want every row of it %v", got, infra)
	}

	var heldRows []PauseHeldRow
	inTx(t, st, func(tx pgx.Tx) {
		heldRows, err = records.PauseHeldRows(ctx, tx, "LEGION", now)
		must(t, err)
	})
	var want []int64
	for i, queued := range pauseQueue {
		if !queued.held {
			continue
		}
		want = append(want, ids[i])
		found := false
		for _, row := range heldRows {
			if row.ID == ids[i] {
				found = true
				if row.Claim != queued.claim || row.Issue != queued.issue || row.WaitedSeconds != 0 {
					t.Errorf("the held row of %s = claim %q, issue %s, waited %ds; want claim %q, issue %s, waited 0s",
						queued.name, row.Claim, row.Issue, row.WaitedSeconds, queued.claim, queued.issue)
				}
			}
		}
		if !found {
			t.Errorf("PauseHeldRows does not list %s, which the pause holds", queued.name)
		}
	}
	if len(heldRows) != len(want) {
		t.Fatalf("PauseHeldRows lists %d rows, want the %d the pause holds", len(heldRows), len(want))
	}
	inTx(t, st, func(tx pgx.Tx) {
		other, err := records.PauseHeldRows(ctx, tx, "INFRA", now)
		must(t, err)
		if len(other) != 0 {
			t.Fatalf("PauseHeldRows of INFRA = %+v, want none: INFRA is not paused", other)
		}
	})

	for i, queued := range pauseQueue {
		ok := claimRow(ids[i])
		if ok == queued.held {
			t.Fatalf("ClaimRow of %s while LEGION is paused = %t, want %t", queued.name, ok, !queued.held)
		}
		if !queued.held {
			continue
		}
		var attempts int
		var lease string
		var leaseUntil *time.Time
		var nextAt time.Time
		if err := st.Pool().QueryRow(ctx, `select attempts, lease_token, lease_until, next_at from outbox where id = $1`, ids[i]).
			Scan(&attempts, &lease, &leaseUntil, &nextAt); err != nil {
			t.Fatalf("read %s: %v", queued.name, err)
		}
		if attempts != 0 || lease != "" || leaseUntil != nil || !nextAt.Equal(now) {
			t.Fatalf("%s after its refused claim = attempts %d, lease %q until %v, next %s; want the row as it was queued",
				queued.name, attempts, lease, leaseUntil, nextAt)
		}
	}
	t.Logf("while LEGION is paused, %d held rows are refused and %d rows are claimed", len(want), len(pauseQueue)-len(want))

	var released []PauseHeldRow
	resumed, err := st.Resume(ctx, store.ResumeRequest{Project: "LEGION", By: store.PauseActor{Kind: store.PauseByOperator}, At: now,
		MarkHeld: func(ctx context.Context, tx pgx.Tx, held store.Pause) error {
			var err error
			released, err = records.MarkPauseHeldRows(ctx, tx, "LEGION", held.ID, now)
			return err
		}})
	must(t, err)
	if resumed.Pause.ID != pause.ID || len(released) != len(want) {
		t.Fatalf("the resume ended pause %d and read %d held rows, want pause %d and the %d held rows", resumed.Pause.ID, len(released), pause.ID, len(want))
	}

	// The resume marked each row its pause held with that pause, and no other row, so a held row
	// that runs after the resume names the pause that held it (OutboxRow.HeldByPause).
	for i, queued := range pauseQueue {
		var marked int64
		if err := st.Pool().QueryRow(ctx, `select coalesce((select held_by_pause from outbox where id = $1), 0)`, ids[i]).Scan(&marked); err != nil {
			t.Fatalf("read the mark of %s: %v", queued.name, err)
		}
		if wantMark := map[bool]int64{true: pause.ID}[queued.held]; marked != wantMark {
			t.Errorf("%s after the resume is marked with pause %d, want %d", queued.name, marked, wantMark)
		}
	}

	// The rows the pause did not hold are under their leases now; the held ones are due again.
	if got := due("LEGION"); !reflect.DeepEqual(got, want) {
		t.Fatalf("DueRows of LEGION after the resume = %v, want every row the pause held %v", got, want)
	}
	for i, queued := range pauseQueue {
		if queued.held && !claimRow(ids[i]) {
			t.Fatalf("ClaimRow of %s after the resume claimed nothing, want it claimed", queued.name)
		}
	}
	inTx(t, st, func(tx pgx.Tx) {
		left, err := records.PauseHeldRows(ctx, tx, "LEGION", now)
		must(t, err)
		if len(left) != 0 {
			t.Fatalf("PauseHeldRows after the resume = %+v, want none", left)
		}
	})
}

// The store records who acted on a pause in the same two words the controller's notice of it
// carries, so the pause service hands one to the other unchanged.
func TestThePauseActorsOfTheStoreAndTheNoticeAreOneVocabulary(t *testing.T) {
	if string(store.PauseByController) != PauseByController || string(store.PauseByOperator) != PauseByOperator {
		t.Fatalf("the store's actors %q and %q, the notice's %q and %q: want the same two words",
			store.PauseByController, store.PauseByOperator, PauseByController, PauseByOperator)
	}
}
