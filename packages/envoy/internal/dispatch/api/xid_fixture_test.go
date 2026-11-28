package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The xid fixture: the load harness's large spec with twenty open anchors on it, behind a traced
// pool. The window tests (xid_window_test.go, a plain-build file) measure on it, and so does the
// write-phase batch test (write_batch_test.go), which the race build runs too, so the fixture
// lives in a file both builds compile.

// xidProject is the fixture's project key.
const xidProject = "XID"

// xidAnchoredComments and xidAnchoredAsks are the fixture's open anchors: twenty, as on the
// harness's large specs once their comments have accumulated.
const (
	xidAnchoredComments = 12
	xidAnchoredAsks     = 8
)

// xidSpec is the fixture spec, storetest.XidSpec on the fixture's project: about 40 KB of tables
// and prose as the harness's large specs, and its requirement cells.
func xidSpec(seed uint64) (string, []string) { return storetest.XidSpec(xidProject, seed) }

// xidFixture is a server on a traced pool holding one issue whose spec is xidSpec with twenty open
// anchors on it, written by a session through the API as the harness's seed writes them.
type xidFixture struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	tracer   *storetest.XidTracer
	issueKey string
	specID   string
	// spec is the spec's text as the steps run on the fixture have left it (revise, accepted).
	spec   string
	quotes []string
	// spare are requirement cells no fixture anchor quotes, for the measured request's own;
	// takeSpare hands each out once.
	spare      []string
	spareTaken int
	comments   []string
	asks       []string
	// revision is the number the spec's revision token stands at (revise).
	revision int
	// verification is set once the issue's Verification document is uploaded
	// (uploadVerification).
	verification bool
}

// newXidFixture builds the fixture on a server with options, whose tracer it sets, and with spec
// as the issue's spec (xidSpec(1) when empty). Its database is a clone of a template seeded once
// per test binary for spec and the options the seeding reads (xidFixtureKey, seedXidTemplate),
// and the fields its steps address are the seed's (xidSeeds). Its server is the row's own, on the
// clone and with the row's options: its tracer, its document store, its listener.
//
// Each write arms its document's settlement and loads no room of it (docs.Service's
// publishLiveWrite). Under the hour's settle the settlement the seed's last write armed stays
// armed through the test, and no room of the spec is resident. Under twenty milliseconds' it
// runs, and loads the spec's room as it settles, which the requests after it meet resident; each
// caller of such a fixture waits for it before it measures (newBlockAskFixture's
// awaitSettledDocuments, runXidSteps with settled). The clone's server has made no write, so once
// its own boot's work has ended (awaitQuietDatabase), the fixture arms the spec's settlement on it
// as that last write armed it on the seed's server (docs.Service.ScheduleSettlement), and the
// measured request meets the spec as it would on the server that seeded it. Where twenty
// milliseconds' settlements ran, the template holds the spec settled, so the one armed here
// writes nothing.
//
// The log line says how long this test took to get its fixture - the clone (and the templates,
// for the first test of a key), the server and that wait - and from which template; the test
// that seeded the template also says how long the seeding took.
func newXidFixture(t *testing.T, options testServerOptions, spec string) *xidFixture {
	t.Helper()
	started := time.Now()
	fixture := &xidFixture{tracer: storetest.NewXidTracer()}
	if options.settle == 0 {
		options.settle = time.Hour
	}
	_, fixture.quotes = xidSpec(1)
	if spec == "" {
		spec, _ = xidSpec(1)
	}
	fixture.spec = spec
	seeding := testServerOptions{settle: options.settle, parentStatusRule: options.parentStatusRule}
	key := xidFixtureKey(spec, seeding)
	options.store = storetest.OpenFilled(t, storetest.Filled{Key: key, Fill: func(_ context.Context, database *store.Store) error {
		seedXidTemplate(t, key, database, seeding, spec, fixture.quotes)
		return nil
	}})
	seed := xidSeedOf(t, key)
	fixture.issueKey, fixture.specID = seed.issueKey, seed.specID
	fixture.comments, fixture.asks, fixture.spare = slices.Clone(seed.comments), slices.Clone(seed.asks), slices.Clone(seed.spare)
	options.tracer = fixture.tracer
	fixture.handler, fixture.database, fixture.deps = newTestServer(t, options)
	awaitQuietDatabase(t, fixture.database, false)
	fixture.deps.Docs.(*docs.Service).ScheduleSettlement(fixture.specID)
	t.Logf("xid fixture: spec of %d bytes, %d requirement cells, %d open anchored comments and %d open anchored asks, seeded in %s from template %q",
		len(spec), len(fixture.quotes), len(fixture.comments), len(fixture.asks), time.Since(started).Round(time.Millisecond), key)
	return fixture
}

// xidFixtureKey is the key of the template seedXidTemplate seeds for spec under seeding, the
// options its writes read (storetest.Filled): a hash of the spec, which fixture C's transport ask
// block changes; the settle delay, which decides whether the seed's settlements run (twenty
// milliseconds) or stay owed (the hour); and the parent-status rule, under which the issue is
// created. The other options a caller passes are its row's server's alone, which no write of the
// seed reads: the tracer; persistence, the document store, which stores and loads as the default
// store does until TestXidWindowDocumentRebuild makes it fail one load after the fixture is built
// (invalidLoadOnceStore); and envoyURL, the listener, which none of the seed's writes calls, since
// none mentions a session and the issue has no route.
func xidFixtureKey(spec string, seeding testServerOptions) string {
	sum := sha256.Sum256([]byte(spec))
	return fmt.Sprintf("api xid fixture: spec %x, settle %s, parent status rule %t", sum[:8], seeding.settle, seeding.parentStatusRule)
}

// xidSeed is what a template's seed wrote that the steps on a fixture address: the issue, its
// spec, the anchored comments and asks on it, and spare cells none of them quotes.
type xidSeed struct {
	issueKey, specID string
	comments, asks   []string
	spare            []string
}

// xidSeeds is the seed of each template newXidFixture clones, by the template's key
// (xidFixtureKey): written once per test binary by the template's Fill (seedXidTemplate), as
// readCorpusBuilt is for the priorities corpus, and read by every fixture cloned from it, which
// storetest.OpenFilled hands over only after that Fill has returned. The lock keeps the map whole
// while one template's Fill writes it and another fixture reads it.
var xidSeeds = struct {
	sync.Mutex
	byKey map[string]xidSeed
}{byKey: map[string]xidSeed{}}

// xidSeedOf is the seed of the template of key.
func xidSeedOf(t *testing.T, key string) xidSeed {
	t.Helper()
	xidSeeds.Lock()
	defer xidSeeds.Unlock()
	seed, seeded := xidSeeds.byKey[key]
	if !seeded {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): template %q was cloned, but no seed of this test binary wrote it", key)
	}
	return seed
}

// seedXidTemplate writes the fixture's issue and its twenty anchors into database, the template of
// key, through the API of a server of its own, as the harness's seed writes them, and keeps the
// seed in xidSeeds. The server runs on seeding, the options its writes read, with no tracer. It
// then waits for the work those writes left running (awaitQuietDatabase), each settlement too where
// the fixture settles in twenty milliseconds, so the template holds the settled spec and the block
// ask its settlement opens; under the hour's settle the spec's settlement stays owed, as on a
// server that seeded the fixture for its own test. The server stops before seedXidTemplate returns,
// so no session of its holds the template once Fill has returned (newStoppableTestServer). Its
// document service's Shutdown settles each document it holds that owes a settlement, so the seed
// first lets go of the spec's room (docs.Service.Evict), which stops the armed settlement, and
// fails where the stop changed what the template owes.
func seedXidTemplate(t *testing.T, key string, database *store.Store, seeding testServerOptions, spec string, quotes []string) {
	t.Helper()
	started := time.Now()
	seeding.store = database
	handler, deps, stop := newStoppableTestServer(t, seeding)
	defer stop()
	issue := createInteractionIssue(t, handler, xidProject, "Large spec under review", spec)
	seed := xidSeed{issueKey: issue.Key, specID: issue.PrimaryArtifactID}

	anchors := xidAnchoredComments + xidAnchoredAsks
	step := len(quotes) / (anchors + 4)
	anchored := map[int]bool{}
	for i := range anchors {
		index := i * step
		anchored[index] = true
		quote := quotes[index]
		if i < xidAnchoredComments {
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+seed.issueKey+"/comments", map[string]any{
				"body":   fmt.Sprintf("Seed comment %d: does %q still hold after the migration?", i, quote),
				"anchor": map[string]any{"artifact": "spec", "quote": quote}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("seed anchored comment %d: status=%d body=%s", i, created.Code, created.Body.String())
			}
			seed.comments = append(seed.comments, decodeBody[model.Comment](t, created).ID)
			continue
		}
		created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+seed.issueKey+"/asks", map[string]any{
			"question": fmt.Sprintf("Seed ask %d: who owns %q?", i, quote),
			"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
			"anchor":   map[string]any{"artifact": "spec", "quote": quote}, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("seed anchored ask %d: status=%d body=%s", i, created.Code, created.Body.String())
		}
		seed.asks = append(seed.asks, decodeBody[model.Ask](t, created).ID)
	}
	for index := len(quotes) - 1; index >= 0 && len(seed.spare) < 8; index-- {
		if !anchored[index] {
			seed.spare = append(seed.spare, quotes[index])
		}
	}
	awaitQuietDatabase(t, database, seeding.settle < time.Hour)
	owed := xidSettlementsOwed(t, database)
	if err := deps.Docs.(*docs.Service).Evict(testwait.Context(t), seed.specID); err != nil {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): let go of the seeded spec's room before the seed's server stops: %v", err)
	}
	stop()
	if left := xidSettlementsOwed(t, database); left != owed {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the template owed %d settlements once its seeding's work had ended and %d once the seed's server stopped; the stop must leave it as the seeding did", owed, left)
	}
	xidSeeds.Lock()
	xidSeeds.byKey[key] = seed
	xidSeeds.Unlock()
	t.Logf("xid fixture: seeding template %q took %s", key, time.Since(started).Round(time.Millisecond))
}

// xidSettlementsOwed counts the documents of database that owe a settlement.
func xidSettlementsOwed(t *testing.T, database *store.Store) int {
	t.Helper()
	var owed int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_settlements_pending`).Scan(&owed); err != nil {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): count the documents that owe a settlement: %v", err)
	}
	return owed
}

// takeSpare hands out a spare cell no step on f has anchored yet, so each anchored write of a run
// lands on a cell of its own, as on a fixture of its own.
func (f *xidFixture) takeSpare(t *testing.T) string {
	t.Helper()
	if f.spareTaken == len(f.spare) {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): all %d spare cells of the fixture are taken", len(f.spare))
	}
	quote := f.spare[f.spareTaken]
	f.spareTaken++
	return quote
}

// xidSuggestion is a session's open suggestion on a spare cell: the comment, the cell it quotes
// and the text its accept leaves there.
type xidSuggestion struct {
	model.Comment
	quote, replaceWith string
}

// suggest opens a session's anchored suggestion on a spare requirement cell.
func (f *xidFixture) suggest(t *testing.T) xidSuggestion {
	t.Helper()
	quote := f.takeSpare(t)
	replaceWith := strings.Replace(quote, "holds", "stands", 1)
	created := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
		"body": "This requirement no longer holds as worded.", "anchor": map[string]any{"artifact": "spec", "quote": quote},
		"suggestion": map[string]string{"replace_with": replaceWith}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("open suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	return xidSuggestion{Comment: decodeBody[model.Comment](t, created), quote: quote, replaceWith: replaceWith}
}
