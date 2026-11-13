package evals

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// testBuild is the build every test's readings write records on its day rows.
const testBuild = "test-build"

var (
	alice = model.Actor{Kind: "user", ID: "alice"}
	// implementer is a Legion implementer's session acting with bob's agent token, as a worker
	// session's write reaches the registry.
	implementer = model.Actor{
		Kind: "session", ID: "session-implementer", Owner: new("bob"),
		Origin: &model.ActorOrigin{SessionTitle: "Legion implementer · EVAL-7"},
	}
)

// rig is one test's registry over a database of its own, with the document service and event
// broker a server would hand it, and one project, EVAL, that holds no eval and no document.
type rig struct {
	database  *store.Store
	documents *docs.Service
	registry  *Store
	project   string
}

func newRig(t *testing.T) rig {
	t.Helper()
	database := storetest.Open(t)
	broker := events.NewBroker()
	// Settlement is an hour away, so no background settlement writes while a test reads.
	documents := docs.New(docs.Deps{Store: database, Events: broker, Settle: time.Hour})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	if _, err := database.Pool.Exec(t.Context(), `insert into projects (key, name) values ('EVAL', 'Evaluation')`); err != nil {
		t.Fatalf("make project EVAL: %v", err)
	}
	return rig{database: database, documents: documents, registry: New(database, documents, broker), project: "EVAL"}
}

// row is one reading as a caller's JSON body carries it.
func row(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode a reading: %v", err)
	}
	return encoded
}

// at is a time on 2026-11-<day> UTC.
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC)
}

// count answers one count(*) statement.
func (r rig) count(t *testing.T, statement string, args ...any) int {
	t.Helper()
	var n int
	if err := r.database.Pool.QueryRow(t.Context(), statement, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", statement, err)
	}
	return n
}

// write logs rows under slug as caller and fails the test unless the write is stored.
func (r rig) write(t *testing.T, slug string, caller model.Actor, rows ...json.RawMessage) Written {
	t.Helper()
	written, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: slug, Rows: rows, Caller: caller, BuiltBy: testBuild})
	if err != nil {
		t.Fatalf("write %d readings under %s: %v", len(rows), slug, err)
	}
	return written
}

// document answers the text and the blocks of the project's evals-md, failing the test when the
// project has none.
func (r rig) document(t *testing.T) (string, []model.ArtifactBlock) {
	t.Helper()
	var id string
	if err := r.database.Pool.QueryRow(t.Context(), `
		select id::text from artifacts where project_key = $1 and issue_key is null and slug = $2
	`, r.project, DocumentSlug).Scan(&id); err != nil {
		t.Fatalf("find the %s document of %s: %v", DocumentSlug, r.project, err)
	}
	text, blocks, err := r.documents.TextWithBlocks(t.Context(), id)
	if err != nil {
		t.Fatalf("read the %s document of %s: %v", DocumentSlug, r.project, err)
	}
	return text, blocks
}

// headings counts the lines of text that are the `##` heading of slug.
func headings(text, slug string) int {
	n := 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "## "+slug {
			n++
		}
	}
	return n
}

// day answers the day row of evalID's series on day for repo, failing the test when there is none.
func (r rig) day(t *testing.T, evalID, series, repo string, day time.Time) DayRow {
	t.Helper()
	found, err := scanDay(r.database.Pool.QueryRow(t.Context(), `
		select `+dayColumns+` from eval_days where eval_id = $1 and series = $2 and repo = $3 and day = $4::date
	`, evalID, series, repo, day))
	if err != nil {
		t.Fatalf("read the day row of %s %s repo %q on %s: %v", evalID, series, repo, day.Format(time.DateOnly), err)
	}
	return found
}
