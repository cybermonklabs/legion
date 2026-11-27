package legion

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestAnObservationWaitsForTheWriterBeforeItSoNoCursorSkipsIt: the intake applies each project's
// messages on that project's own runner, so the writers of two projects insert into
// legion_observations at once. A write of BRAVO that comes while a write of ALPHA holds its
// transaction open after its insert waits on the observations' lock (ObservationsLockKey) until
// ALPHA commits, so it cannot commit an id above ALPHA's while ALPHA's row is still open: a reader
// that pages the table by id (legionfacts' readObservationPage) never passes ALPHA's row before
// that row lands. ALPHA writes as a batch of state events does (recordEvents) and BRAVO as a whole
// read does (recordObservations), the intake's two writers.
//
// Without the lock BRAVO commits at once while ALPHA's row is open: a reader at that moment reads
// BRAVO's row alone, and its cursor passes ALPHA's id, which commits after it.
func TestAnObservationWaitsForTheWriterBeforeItSoNoCursorSkipsIt(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := t.Context()
	seedProject(t, database, "ALPHA")
	seedProject(t, database, "BRAVO")
	var before int64
	if err := database.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from legion_observations`).Scan(&before); err != nil {
		t.Fatalf("read the newest observation: %v", err)
	}
	// written is the project of each observation committed after before, in id order: what a
	// reader that pages the table by id reads.
	written := func() []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `select project from legion_observations where id > $1 order by id`, before)
		if err != nil {
			t.Fatalf("read the observations after %d: %v", before, err)
		}
		projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the observations after %d: %v", before, err)
		}
		return projects
	}
	at := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	phase := func(issue, value string) []Observation {
		return []Observation{{IssueKey: issue, Field: FieldPhase, Value: json.RawMessage(`"` + value + `"`)}}
	}

	alpha, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin ALPHA's batch: %v", err)
	}
	defer func() { _ = alpha.Rollback(context.Background()) }()
	if _, err := recordEvents(ctx, alpha, "ALPHA", at, phase("ALPHA-1", "implementing")); err != nil {
		t.Fatalf("ALPHA's batch: %v", err)
	}

	bravoDone := make(chan error, 1)
	go func() {
		bravoDone <- func() error {
			bravo, err := database.Pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = bravo.Rollback(context.Background()) }()
			if _, err := recordObservations(ctx, bravo, "BRAVO", at, phase("BRAVO-1", "testing")); err != nil {
				return err
			}
			return bravo.Commit(ctx)
		}()
	}()

	// BRAVO's write either waits on the observations' lock while ALPHA's transaction is open, or
	// has committed past it.
	type moment struct {
		committed bool
		err       error
		read      []string
	}
	seen := testwait.Until(t, "BRAVO's write to wait on the observations' lock while ALPHA's transaction is open", func() (moment, bool) {
		select {
		case err := <-bravoDone:
			return moment{committed: true, err: err, read: written()}, true
		default:
		}
		var waiting int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from pg_locks
			where locktype = 'advisory' and not granted and classid::bigint = $1 and objid = 0 and objsubid = 2
				and database = (select oid from pg_database where datname = current_database())
		`, ObservationsLockKey).Scan(&waiting); err != nil {
			t.Fatalf("count the waits on the observations' lock: %v", err)
		}
		return moment{}, waiting == 1
	})
	if seen.committed {
		t.Fatalf("BRAVO's write ended (error %v) while ALPHA's transaction was open: a reader at that moment read %v, and its cursor passed ALPHA's id, which commits after it",
			seen.err, seen.read)
	}
	if read := written(); len(read) != 0 {
		t.Fatalf("a reader read %v while ALPHA's transaction was open and BRAVO's write waited, want nothing", read)
	}

	if err := alpha.Commit(ctx); err != nil {
		t.Fatalf("commit ALPHA's batch: %v", err)
	}
	if err := testwait.Receive(t, "BRAVO's write to commit once ALPHA's has", bravoDone); err != nil {
		t.Fatalf("BRAVO's write: %v", err)
	}
	if read := written(); !slices.Equal(read, []string{"ALPHA", "BRAVO"}) {
		t.Fatalf("a reader read %v in id order, want ALPHA's row below BRAVO's", read)
	}
}
