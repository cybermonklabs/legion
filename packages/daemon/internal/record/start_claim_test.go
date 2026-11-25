package record

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// keyAToken and keyBToken are the claims of two agents of the implementer on LEGION-208, as
// `implementer:a reviewer:b implementer:b` names them: key a is named implementer, key b reviewer.
const (
	keyAToken claim.Token = "legion-legion-legion-208-implementer"
	keyBToken claim.Token = "legion-legion-legion-208-reviewer"
)

// enqueueStarts queues each of starts on LEGION-208, in order, in one transaction on pool, and
// answers their row ids.
func enqueueStarts(t *testing.T, pool store.TxPool, starts ...SuperviseRequest) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(starts))
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, start := range starts {
			row, err := NewOutboxRow("LEGION-208", start, time.Now())
			if err != nil {
				return err
			}
			if err := NewStore().Enqueue(ctx, tx, row); err != nil {
				return err
			}
			var id int64
			if err := tx.QueryRow(ctx, "select max(id) from outbox").Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	}); err != nil {
		t.Fatalf("queue the starts: %v", err)
	}
	return ids
}

// queuedIDs is the id of each queued row of run, in order.
func queuedIDs(run RoleRun) []int64 {
	ids := make([]int64, 0, len(run.Queued))
	for _, queued := range run.Queued {
		ids = append(ids, queued.ID)
	}
	return ids
}

// Every phase worker's start names the claim it was queued for, and two claims can hold one role:
// in `implementer:a reviewer:b implementer:b` key a and key b each start as the implementer. A
// start that names key b is key b's queued run, and a start that names key a is key a's, however
// the read is keyed: here both claims are read at the implementer's role in one batch, key b first.
// A start the release before wrote names no claim and is the run of the claim the read asks for at
// its issue, role and generation, as before. Before, a named start was matched to the one token the
// batch kept for its role, the last asked, so key b's own start was read as no run of key b's.
func TestARoleRunHoldsTheStartsThatNameItsClaim(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	start := SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing}
	named := func(token claim.Token, entry int64) SuperviseRequest {
		s := start
		s.Claim, s.Entry, s.Task = token, entry, "Implement it."
		return s
	}
	ids := enqueueStarts(t, st.Pool(), named(keyBToken, 3), named(keyAToken, 4), start)
	keyB := RoleRunKey{Token: keyBToken, Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1}
	keyA := RoleRunKey{Token: keyAToken, Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1}
	inTx(t, st, func(tx pgx.Tx) {
		runs, err := NewStore().RoleRuns(ctx, tx, []RoleRunKey{keyB, keyA})
		must(t, err)
		if got, want := queuedIDs(runs[keyBToken]), ids[:1]; !slices.Equal(got, want) {
			t.Errorf("key b's queued rows = %v, want %v, the start that names it", got, want)
		}
		if got, want := queuedIDs(runs[keyAToken]), ids[1:]; !slices.Equal(got, want) {
			t.Errorf("key a's queued rows = %v, want %v: the start that names it, and the start that names none, read at its role", got, want)
		}
		alone, err := NewStore().RoleRuns(ctx, tx, []RoleRunKey{keyA})
		must(t, err)
		if got, want := queuedIDs(alone[keyAToken]), ids[1:]; !slices.Equal(got, want) {
			t.Errorf("key a's queued rows read alone = %v, want %v: key b's start is no run of key a's", got, want)
		}
	})
}

// A start waiting for its slot carries the claim it names into the waiting line's read
// (OwedStart.Claim), which admission's repair weighs it against; a start the release before wrote
// names none.
func TestAnOwedStartCarriesTheClaimItNames(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	start := SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing,
		Task: "Implement it.", NeedsSlot: true}
	keyed := start
	keyed.Claim = keyBToken
	enqueueStarts(t, st.Pool(), keyed)
	inTx(t, st, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ('supervise', 'LEGION-208',
			'{"op": "start", "tree": "LEGION-208", "role": "tester", "generation": 1, "phase": "testing", "task": "Test it.", "needsSlot": true}',
			0, now(), '')`)
		must(t, err)
	})
	inTx(t, st, func(tx pgx.Tx) {
		owed, err := NewStore().OwedStarts(ctx, tx, "LEGION")
		must(t, err)
		if len(owed) != 2 || owed[0].Role != claim.RoleImplementer || owed[0].Claim != keyBToken || owed[1].Role != claim.RoleTester || owed[1].Claim != "" {
			t.Fatalf("the owed starts = %+v, want the implementer's naming %s, then the tester's of the release before naming none", owed, keyBToken)
		}
	})
}

// A start a pause holds is listed under the claim it names, the claim it was queued for: key b's
// start of the implementer's step is key b's, though the implementer's own token is key a's. A start
// the release before wrote names none and is listed under the claim of the worker on its role's
// step (WorkerClaimOf), here the implementer's own token, since no phases row names another.
func TestAPauseHeldStartIsListedUnderTheClaimItNames(t *testing.T) {
	row := OutboxRow{Kind: OutboxKindSupervise, Issue: "LEGION-208"}
	if got := pauseHeldClaim("legion", row, string(claim.RoleImplementer), keyBToken, nil); got != keyBToken {
		t.Errorf("the held start naming key b is listed under %q, want %q", got, keyBToken)
	}
	if got := pauseHeldClaim("legion", row, string(claim.RoleImplementer), "", nil); got != keyAToken {
		t.Errorf("the held start naming no claim is listed under %q, want the implementer's own token %q", got, keyAToken)
	}
}

// The starts that wait for a pod are named by the claim each starts, by the rule the outbox's start
// acts on (QueuedClaim): the claim a row names, and for a row that names none the claim WorkerClaim
// gives its role's step. Each claim is named once, in the order of the rows: a row that names the
// implementer's own claim, a row of the release before that names none and so starts that same
// claim, and the same row queued twice are one start. Key b's start, which names key b's claim,
// stays apart from them. A start of a role no token spells is refused naming it.
func TestTheStartsThatWaitForAPodAreEachTheClaimTheyStartOnce(t *testing.T) {
	starts := []pods.WaitingStart{
		{Role: claim.RoleImplementer, Claim: keyAToken},
		{Role: claim.RoleImplementer},
		{Role: claim.RoleImplementer, Claim: keyBToken},
		{Role: claim.RoleImplementer, Claim: keyBToken},
		{Role: claim.RoleTester},
	}
	got, err := StartClaims("legion", "LEGION-208", starts, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []WaitingClaim{
		{Role: claim.RoleImplementer, Token: keyAToken},
		{Role: claim.RoleImplementer, Token: keyBToken},
		{Role: claim.RoleTester, Token: "legion-legion-legion-208-tester"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("the starts = %v, want %v", got, want)
	}
	if _, err := StartClaims("legion", "LEGION-208", []pods.WaitingStart{{Role: "Checker"}}, nil); err == nil || !strings.Contains(err.Error(), "Checker") {
		t.Errorf("the start of Checker = %v, want a refusal naming it", err)
	}
}
