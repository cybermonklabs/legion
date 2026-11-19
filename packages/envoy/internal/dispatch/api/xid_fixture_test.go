package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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
	// mayHoldNoXid passes a measured request that holds no xid at all
	// (storetest.XidReport.MayHoldNoXid): one that may write nothing.
	mayHoldNoXid bool
}

// newXidFixture builds the fixture on a server with options, whose tracer it sets, and with spec
// as the issue's spec (xidSpec(1) when empty).
func newXidFixture(t *testing.T, options testServerOptions, spec string) *xidFixture {
	t.Helper()
	fixture := &xidFixture{tracer: storetest.NewXidTracer()}
	if options.settle == 0 {
		options.settle = time.Hour
	}
	options.tracer = fixture.tracer
	fixture.handler, fixture.database, fixture.deps = newTestServer(t, options)
	_, fixture.quotes = xidSpec(1)
	if spec == "" {
		spec, _ = xidSpec(1)
	}
	fixture.spec = spec
	started := time.Now()
	issue := createInteractionIssue(t, fixture.handler, xidProject, "Large spec under review", spec)
	fixture.issueKey, fixture.specID = issue.Key, issue.PrimaryArtifactID

	anchors := xidAnchoredComments + xidAnchoredAsks
	step := len(fixture.quotes) / (anchors + 4)
	anchored := map[int]bool{}
	for i := range anchors {
		index := i * step
		anchored[index] = true
		quote := fixture.quotes[index]
		if i < xidAnchoredComments {
			created := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/comments", map[string]any{
				"body":   fmt.Sprintf("Seed comment %d: does %q still hold after the migration?", i, quote),
				"anchor": map[string]any{"artifact": "spec", "quote": quote}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("seed anchored comment %d: status=%d body=%s", i, created.Code, created.Body.String())
			}
			fixture.comments = append(fixture.comments, decodeBody[model.Comment](t, created).ID)
			continue
		}
		created := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/asks", map[string]any{
			"question": fmt.Sprintf("Seed ask %d: who owns %q?", i, quote),
			"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
			"anchor":   map[string]any{"artifact": "spec", "quote": quote}, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("seed anchored ask %d: status=%d body=%s", i, created.Code, created.Body.String())
		}
		fixture.asks = append(fixture.asks, decodeBody[model.Ask](t, created).ID)
	}
	for index := len(fixture.quotes) - 1; index >= 0 && len(fixture.spare) < 8; index-- {
		if !anchored[index] {
			fixture.spare = append(fixture.spare, fixture.quotes[index])
		}
	}
	t.Logf("xid fixture: spec of %d bytes, %d requirement cells, %d open anchored comments and %d open anchored asks, seeded in %s",
		len(spec), len(fixture.quotes), len(fixture.comments), len(fixture.asks), time.Since(started).Round(time.Millisecond))
	return fixture
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
