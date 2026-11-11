package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// infraSizeTrees is the population these measurements run over: the trees of the facts' own rig
// (legiontest.DefaultTrees, 42 of them), which is 252 issues and 210 of them with Legion phase
// work. That is INFRA's size today.
const infraSizeTrees = legiontest.DefaultTrees

// firstPayloadBound is the size a first payload may reach, compressed: 300 KB, the bound the
// root states (dispatch://INFRA-979/spec#b-r14, "the first payload is at most 300 KB
// compressed"). The board's body is one such payload.
const firstPayloadBound = 300 * 1024

// boardEntriesFloor is how many issues entries a seeded project's board must carry: the floor
// check k4 of dispatch://INFRA-2915 counts against.
const boardEntriesFloor = 20

// boardAtInfraSize seeds one project of INFRA's size, names its daemon and runs the facts
// consumer over it until it has written a row for every issue with Legion phase work. The
// consumer is stopped before this answers, so every measurement below reads stored rows that
// nothing is rewriting.
//
// The history ends now rather than at a fixed moment, so an open issue's hours are the hours the
// consumer's own clock measures.
func boardAtInfraSize(t *testing.T, options testServerOptions) (http.Handler, *store.Store, legiontest.Fixture) {
	t.Helper()
	handler, database, _ := newTestServer(t, options)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "INFRA", Trees: infraSizeTrees})
	nameLegionDaemon(t, handler, fixture.Project, "http://legion-infra.internal:13370")
	runLegionFactsConsumer(t, database, fixture.Project, len(fixture.PhaseWork))
	return handler, database, fixture
}

// runLegionFactsConsumer runs the facts consumer (legionfacts.Run, the loop the server runs)
// until the project holds want stored rows, then stops it and waits for it to return. Every
// measurement after it reads rows that loop wrote and that no loop is rewriting.
func runLegionFactsConsumer(t *testing.T, database *store.Store, project string, want int) {
	t.Helper()
	leases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool for the facts consumer: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		legionfacts.Run(ctx, legionfacts.Deps{
			Pool:    database.Pool,
			Horizon: events.NewHorizon(database.Pool),
			Leases:  leases,
		})
	}()
	defer func() {
		stop()
		<-stopped
	}()
	started := time.Now()
	for {
		rows, err := legionfacts.Read(t.Context(), database.Pool, project)
		if err != nil {
			t.Fatalf("read the stored facts of %s: %v", project, err)
		}
		if len(rows) >= want {
			t.Logf("the facts consumer wrote %d rows of %s in %s", len(rows), project, time.Since(started).Round(time.Millisecond))
			return
		}
		if time.Since(started) > time.Minute {
			t.Fatalf("the facts consumer wrote %d rows of %s in %s, want %d", len(rows), project, time.Since(started).Round(time.Millisecond), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// gzipped is how many bytes body takes under gzip at its default level, which is the level a
// proxy in front of this server compresses a response at.
func gzipped(t *testing.T, body []byte) int {
	t.Helper()
	var packed bytes.Buffer
	writer := gzip.NewWriter(&packed)
	if _, err := writer.Write(body); err != nil {
		t.Fatalf("compress the body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("compress the body: %v", err)
	}
	return packed.Len()
}

// TestTheBoardsBodyAtInfrasSizeFitsTheFirstPayloadBound: the board of a project of INFRA's size
// is one first payload, and a first payload is at most 300 KB compressed
// (dispatch://INFRA-979/spec#b-r14). This reads the body the GET route answers over a project the
// facts consumer has run over, and measures it twice: the bytes the handler wrote, and those
// bytes under gzip, which is what a browser behind a compressing proxy receives.
//
// The raw size is recorded beside the compressed one because nothing in this process compresses
// the body: the Go server writes the bytes of the first number, and the second is what a proxy
// that compresses makes of them.
func TestTheBoardsBodyAtInfrasSizeFitsTheFirstPayloadBound(t *testing.T) {
	handler, _, fixture := boardAtInfraSize(t, testServerOptions{})

	response := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/projects/"+fixture.Project+"/legion/board", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the board of %s: status=%d", fixture.Project, response.Code)
	}
	body := response.Body.Bytes()
	raw, packed := len(body), gzipped(t, body)

	board := decodeBody[legionBoardResponse](t, response)
	if len(board.Issues) != len(fixture.PhaseWork) {
		t.Errorf("the board carries %d issues entries, want the %d issues with Legion phase work",
			len(board.Issues), len(fixture.PhaseWork))
	}
	if len(board.Issues) < boardEntriesFloor {
		t.Errorf("the board carries %d issues entries, below the %d a seeded project's board must carry",
			len(board.Issues), boardEntriesFloor)
	}
	if packed > firstPayloadBound {
		t.Errorf("the board's body is %d bytes compressed, above the %d byte first-payload bound",
			packed, firstPayloadBound)
	}
	t.Logf("project %s: %d issues, %d with Legion phase work; the board's body is %d bytes raw and %d bytes gzipped (%.1f KB and %.1f KB), against the 300 KB first payload bound, which leaves %d bytes",
		fixture.Project, len(fixture.Keys), len(fixture.PhaseWork), raw, packed,
		float64(raw)/1024, float64(packed)/1024, firstPayloadBound-packed)
	t.Logf("the body carries %d issues entries and %d cards", len(board.Issues), countBoardCards(board))
}

// countBoardCards is how many cards the board's ten columns hold between them.
func countBoardCards(board legionBoardResponse) int {
	cards := 0
	for _, column := range board.Columns {
		cards += len(column.Cards)
	}
	return cards
}

// boardComputeRuns is how many whole board reads TestWhatOneBoardComputeCostsAtInfrasSize
// measures. Twenty runs give a median a reader can stand on: the first read of a fresh database
// pays for plans and a cold page cache, and one run after it says nothing about the spread.
const boardComputeRuns = 20

// TestWhatOneBoardComputeCostsAtInfrasSize measures one whole board read and compute at a
// population of INFRA's size, which is what a page's first answer and every live recompute of a
// subscribed board costs.
//
// The measured work is the GET route's own: the read of the daemon's observations, the read of
// the project's stored facts and the cursor's age, the tree walk, the compute of the ten columns
// and the encoding of the body. The hub's recompute runs the same read and compute
// (readLegionBoardView) and encodes the same body, so the number here is that recompute's as
// well, less the delta it takes against what each subscriber holds.
//
// The consumer is stopped before the first run (boardAtInfraSize), so every run reads stored
// rows that nothing is rewriting, and the runs measure the read and not a race with a writer.
func TestWhatOneBoardComputeCostsAtInfrasSize(t *testing.T) {
	handler, _, fixture := boardAtInfraSize(t, testServerOptions{})
	target := "/api/v1/projects/" + fixture.Project + "/legion/board"

	// One read before the measured ones, which is also the read the counts below are taken
	// from: it pays for the statement plans and the first touch of each page, which a board a
	// reader opens never pays again.
	startedFirst := time.Now()
	first := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	cold := time.Since(startedFirst)
	if first.Code != http.StatusOK {
		t.Fatalf("GET the board of %s: status=%d", fixture.Project, first.Code)
	}
	board := decodeBody[legionBoardResponse](t, first)
	if len(board.Issues) != len(fixture.PhaseWork) {
		t.Fatalf("the board carries %d issues entries, want the %d issues with Legion phase work: "+
			"a board of another size is another measurement", len(board.Issues), len(fixture.PhaseWork))
	}

	taken := make([]time.Duration, 0, boardComputeRuns)
	var total time.Duration
	for range boardComputeRuns {
		started := time.Now()
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		elapsed := time.Since(started)
		if response.Code != http.StatusOK {
			t.Fatalf("GET the board of %s: status=%d", fixture.Project, response.Code)
		}
		taken = append(taken, elapsed)
		total += elapsed
	}
	slices.Sort(taken)
	median := taken[len(taken)/2]

	t.Logf("one board read and compute over %d issues, %d of them with Legion phase work, answering %d issues entries and %d cards: %d runs, median %s, fastest %s, slowest %s, mean %s",
		len(fixture.Keys), len(fixture.PhaseWork), len(board.Issues), countBoardCards(board),
		boardComputeRuns, median.Round(time.Microsecond), taken[0].Round(time.Microsecond),
		taken[len(taken)-1].Round(time.Microsecond), (total / boardComputeRuns).Round(time.Microsecond))
	t.Logf("the first read, which pays for the statement plans and the first touch of every page, took %s",
		cold.Round(time.Microsecond))
}
