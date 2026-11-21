package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// pauseLine is one owner of the daemon's half of a pause whose line says what its transaction
// wrote: seed builds the fixture on a pause rig of its own and answers it with act, the owner's
// run on the store it is given, and wrote, which reads the run's durable write on the rig's own
// pool. match is SQL that only the transaction of the owner's line sends, which the cut meets, and
// line is the line's message.
type pauseLine struct {
	name, line, match string
	seed              func(t *testing.T) (r *pauseRig, act func(st *store.Store) error, wrote func() bool)
}

func pauseLines() []pauseLine {
	return []pauseLine{
		{name: "the catch-up after a pause", match: "insert into project_pause_stops",
			line: "pause: the catch-up asked the stop of live roles the pause's transaction did not name",
			seed: func(t *testing.T) (*pauseRig, func(*store.Store) error, func() bool) {
				r := newPauseRig(t)
				sup, _ := r.supervisor()
				r.pauses.supervisor = sup
				// A launch that passed the gate before the flag was set: live on its machine, and named
				// by no stop of the pause's transaction.
				launchedClaim(t, sup, pauseIssue, pauseIssue, claim.RoleImplementer)
				held := r.pauseLegion()
				return r, func(st *store.Store) error {
						r.pauses.store = st
						r.pauses.Paused(r.ctx, held, nil)
						return nil
					},
					func() bool { return len(r.stops(held.ID, claim.RoleImplementer)) == 1 }
			}},
		{name: "the boot's ask of a live role's stop", match: "insert into project_pause_stops",
			line: "pause: the boot asked the stop of the live roles the project's pause has not stopped",
			seed: func(t *testing.T) (*pauseRig, func(*store.Store) error, func() bool) {
				r := newPauseRig(t)
				held := r.pauseLegion()
				// A role an earlier daemon launched before the pause reached it.
				if err := r.st.PutClaim(r.ctx, r.claim(claim.RoleImplementer, supervise.StateIdle)); err != nil {
					t.Fatalf("seed the implementer's claim: %v", err)
				}
				return r, func(st *store.Store) error {
						r.pauses.store = st
						return r.pauses.askBootStops(r.ctx, held)
					},
					func() bool { return len(r.stops(held.ID, claim.RoleImplementer)) == 1 }
			}},
		{name: "the boot's ask again of the stops the pause waits on", match: "update outbox set next_at",
			line: "pause: the boot asks again, at once, the stops the project's pause still waits on",
			seed: func(t *testing.T) (*pauseRig, func(*store.Store) error, func() bool) {
				r := newPauseRig(t, claim.RoleImplementer)
				held := r.pauseLegion()
				// The implementer's stop, held for its turn: its row waits on its bounded retry.
				if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = now() + interval '10 minutes'
					where payload->>'op' = 'pause_stop'`); err != nil {
					t.Fatal(err)
				}
				return r, func(st *store.Store) error {
						r.pauses.store = st
						return r.pauses.askBootStops(r.ctx, held)
					},
					func() bool {
						return r.count(`select count(*) from outbox where payload->>'op' = 'pause_stop' and next_at > now()`) == 0
					}
			}},
		{name: "a stop row the executor ends", match: "update project_pause_stops",
			line: "outbox pause stop ended its stop row",
			seed: func(t *testing.T) (*pauseRig, func(*store.Store) error, func() bool) {
				r := newPauseRig(t, claim.RoleTester)
				held := r.pauseLegion()
				// No machine of the tester: its stop row ends failed.
				sup, _ := r.supervisor()
				runner := &outbox{pool: r.pool, records: record.NewStore(), supervisor: sup, project: "legion",
					dispatchProject: "LEGION", log: r.pauses.log, now: time.Now}
				row := mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "pause_stop", Tree: pauseIssue, Role: claim.RoleTester,
					Generation: 1, Pause: held.ID, Reason: "project LEGION is paused"}, time.Now())
				return r, func(st *store.Store) error {
						runner.trees = st
						return runner.execute(r.ctx, row)
					},
					func() bool {
						stops := r.stops(held.ID, claim.RoleTester)
						return len(stops) == 1 && stops[0] == (pauseStopRecord{end: store.PauseStopFailed})
					}
			}},
	}
}

// Each owner of the daemon's half of a pause whose line says what its transaction wrote writes the
// line as the hook of that commit. The owner runs once on a store whose COMMIT is cut before it
// reaches PostgreSQL: the transaction rolls back, its write is not there, and no line is. The
// owner runs again on the direct store and commits: the write is there, and the line once.
func TestAPauseLineOfACommittedWriteIsWrittenOnlyOnceItsTransactionCommits(t *testing.T) {
	for _, tc := range pauseLines() {
		t.Run(tc.name, func(t *testing.T) {
			r, act, wrote := tc.seed(t)
			if wrote() {
				t.Fatal("the owner's write is there before it ran: the case proves nothing")
			}
			lines := func() int { return len(r.lines(tc.line)) }
			if got := lines(); got != 0 {
				t.Fatalf("lines before the owner ran = %d, want 0: the case proves nothing", got)
			}
			direct := r.st
			faulted, fault := faultedStoreOf(t, direct)

			fired := fault.Cut(tc.match, testdb.CutBeforeCommit)
			err := act(faulted)
			testdb.Met(t, fired)
			if wrote() {
				t.Fatal("the rolled-back run's write is there: the cut came after the commit")
			}
			if got := lines(); got != 0 {
				t.Errorf("lines after the rolled-back run (%v) = %d, want 0; log:\n%s", err, got, r.logs.String())
			}

			if err := act(direct); err != nil {
				t.Fatalf("the owner run again on the direct store: %v", err)
			}
			if !wrote() {
				t.Fatal("the run again made no write: the owner does not reach its write")
			}
			if got := lines(); got != 1 {
				t.Errorf("lines after the committed run = %d, want 1; log:\n%s", got, r.logs.String())
			}
		})
	}
}
