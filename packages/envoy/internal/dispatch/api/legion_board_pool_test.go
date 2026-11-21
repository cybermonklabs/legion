package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedClimbingLegionBoard is a project whose board runs every read the board makes: its daemon is
// named and read once, and a chain whose walk climbs onto an issue the daemon reports nothing of,
// so the board reads that issue's phase work (readPhaseWork) after its snapshot transaction
// (readSampled) and its issue rows (readIssues).
func seedClimbingLegionBoard(t *testing.T, handler http.Handler, database *store.Store, project string) {
	t.Helper()
	seedLegionProjects(t, handler, project)
	nameLegionDaemon(t, handler, project, "http://legion-pool.internal:13370")
	now := time.Now().UTC()
	top := seedLegionIssue(t, database, project, 870, "The top, which the daemon reports", "a", nil, "")
	middle := seedLegionIssue(t, database, project, 871, "The tracking issue the daemon reports nothing of", "b", nil, top)
	bottom := seedLegionIssue(t, database, project, 872, "The bottom, which the daemon reports", "c", nil, middle)
	for _, key := range []string{top, bottom} {
		seedLegionObservation(t, database, project, key, legion.FieldPhase, `"implementing"`, now.Add(-time.Hour))
	}
	seedLegionRead(t, database, project, now.Add(-time.Minute), "")
}

// A board answer is one pinned read (servedAsRead, apiRoute.pinsConnection): the caller's
// identity, the project, its Legion setting, the repeatable-read transaction that reads the
// header and the cards from one snapshot (readSampled), the issue rows and the phase work all run
// on the one request-pool connection its first statement took. The transaction is opened on the
// request pool (Store.Pool.BeginTx), which under the pin runs it on that connection rather than
// taking a second one while the request holds the first, the nested acquire this branch removes.
// Served without the pin - every statement taking a connection of its own, as main serves every
// read - the same answer takes one acquire per statement and fails here.
func TestALegionBoardRequestTakesOneRequestPoolConnection(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedClimbingLegionBoard(t, handler, database, "INFRA")
	awaitQuietRequestPool(t, database.Pool)

	before := requestPoolStats(t, database.Pool).Acquire.Count
	board := legionBoardOf(t, handler, "INFRA")
	acquires := requestPoolStats(t, database.Pool).Acquire.Count - before

	if acquires != 1 {
		t.Errorf("one board answer took %d request-pool connections, want 1: its pinned connection", acquires)
	}
	if held := database.Pool.Stat().AcquiredConns(); held != 0 {
		t.Errorf("the board answer left %d request-pool connections taken once it answered, want 0", held)
	}
	if board.Project != "INFRA" || !board.Header.ReadOK {
		t.Errorf("the board answered project %q with header %+v, want INFRA's good read", board.Project, board.Header)
	}
}

// A Legion board view's recompute (readLegionBoardView) reads through the live pool the hub hands
// every view's read (liveRead), never the request pool: with every connection of the request pool
// held - the requests of a burst - a subscribed board still gets its snapshot, and the live pool's
// acquires grow by the read's. Pointed back at the request pool, as main's read was written, the
// read waits out the request pool's bound and is retried, and no snapshot comes.
func TestALegionBoardViewReadsThroughTheLivePoolWhileTheRequestPoolIsFullyAcquired(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{
		requestPoolSize: 4, acquireBound: partitionAcquireBound,
	})
	seedClimbingLegionBoard(t, handler, database, "INFRA")
	rig := hubOn(t, handler, database, deps, newNoonHubClock(), nil)
	live := settledAcquires(t, database, store.LivePool)

	holdEveryConnection(t, database.Pool)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	snapshot := conn.snapshot(t, 5*time.Second, "b")

	if project := legionBodyProject(t, snapshot.Body); project != "INFRA" {
		t.Errorf("the snapshot is of project %q, want INFRA", project)
	}
	if got := poolAcquires(database, store.LivePool); got <= live {
		t.Errorf("live-pool acquires = %d, want more than the %d before the subscribe: the board's read runs on the live pool", got, live)
	}
}
