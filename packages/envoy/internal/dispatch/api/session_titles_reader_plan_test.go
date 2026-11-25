package api

import (
	"context"
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The sessions the reader's session titles read is measured for, as delivery's own plan test
// (session_titles_plan_test.go) lays them out for the server's shape: one whose only title is on an
// event older than three untitled ones and newer than six, one that never stamped a title, and one
// the events table has never heard of.
const (
	readerTitledSession   = "ses_reader_release_sender"
	readerUntitledSession = "ses_reader_never_titled"
	readerTitleName       = "Release sender"
	readerOlderEvents     = 6
	readerNewerEvents     = 3
	readerUntitledEvents  = 4
)

var readerTitleSessions = []string{readerTitledSession, readerUntitledSession, "ses_reader_unknown"}

// readerTitlesStoppedRows is what a read that stops at each session's newest titled row it may
// read touches: the titled session's newer untitled rows and the titled row, and every event of
// the session that never stamped one. readerTitlesEveryRow is every event the listed sessions have.
const (
	readerTitlesStoppedRows = readerNewerEvents + 1 + readerUntitledEvents
	readerTitlesEveryRow    = readerOlderEvents + 1 + readerNewerEvents + readerUntitledEvents
)

// newReaderTitlesFixture writes the sessions' events on one issue of TEST, oldest first.
func newReaderTitlesFixture(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('TEST', 'Test', 'alice')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank)
			values ('TEST-1', 'TEST', 1, 'Test issue', 'todo', '{"kind":"user","id":"alice"}', 'U')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("write the session titles fixture: %v", err)
		}
	}
	at := time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC)
	seq := 0
	write := func(actor model.Actor) {
		t.Helper()
		seq++
		stamped, err := json.Marshal(actor)
		if err != nil {
			t.Fatalf("marshal the actor of %s: %v", actor.ID, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			values ('TEST-1', $1, 'message.created', $2::jsonb, '{}', false, $3)`,
			seq, stamped, at.Add(time.Duration(seq)*time.Minute)); err != nil {
			t.Fatalf("write event %d: %v", seq, err)
		}
	}
	untitled := model.Actor{Kind: "session", ID: readerTitledSession}
	for range readerOlderEvents {
		write(untitled)
	}
	write(model.Actor{Kind: "session", ID: readerTitledSession, Origin: &model.ActorOrigin{SessionTitle: readerTitleName}})
	for range readerNewerEvents {
		write(untitled)
	}
	for range readerUntitledEvents {
		write(model.Actor{Kind: "session", ID: readerUntitledSession})
	}
	return database
}

// recordingTitlesQuerier runs a statement on pool and keeps it, with its arguments, for the plan.
type recordingTitlesQuerier struct {
	pool *store.Pool
	sql  string
	args []any
}

func (r *recordingTitlesQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.sql, r.args = sql, args
	return r.pool.Query(ctx, sql, args...)
}

func (r *recordingTitlesQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.sql, r.args = sql, args
	return r.pool.QueryRow(ctx, sql, args...)
}

// titleReadNode is what this test reads of a plan node: how often it ran, the rows it answered
// and threw away on each run, and the index it reads.
type titleReadNode struct {
	IndexName string          `json:"Index Name,omitempty"`
	Rows      float64         `json:"Actual Rows"`
	Loops     float64         `json:"Actual Loops"`
	Filtered  float64         `json:"Rows Removed by Filter"`
	Plans     []titleReadNode `json:"Plans,omitempty"`
}

func (node titleReadNode) everyNode() []titleReadNode {
	nodes := []titleReadNode{node}
	for _, child := range node.Plans {
		nodes = append(nodes, child.everyNode()...)
	}
	return nodes
}

// TestTheReadersSessionTitlesReadStopsAtTheNewestTitleItMayRead measures the shape of the
// session titles read a reader's request sends (delivery.ResolveSessionTitles under
// readableEvents), which binds the event rule (eventReadableSQL) in the lateral. For a reader
// that sees the events' project it is the server's shape: one ordered read of
// events_session_actor_created_at_idx a session, stopping at the newest titled row, every probe
// the rule adds keyed. For a reader that sees none of them it walks each listed session's whole
// history and answers no title, which is the cost of the rule: a session's events are read to the
// end when none of its titles is one the reader may read.
func TestTheReadersSessionTitlesReadStopsAtTheNewestTitleItMayRead(t *testing.T) {
	database := newReaderTitlesFixture(t)
	const index = "events_session_actor_created_at_idx"
	// The plan's counts are averages over the loops, rounded, so the rows a node touched are
	// held to bounds rather than to an exact count: at most the stopped read's for a reader that
	// sees the titled row, and past it - into the titled session's older events - for one that
	// does not.
	for _, reader := range []struct {
		name        string
		viewer      *model.Viewer
		want        map[string]string
		least, most float64
	}{
		{"a reader that sees the project", &model.Viewer{Sees: []string{"TEST"}}, map[string]string{readerTitledSession: readerTitleName}, 0, readerTitlesStoppedRows},
		{"a reader that sees none of it", &model.Viewer{Sees: []string{"OTHER"}}, map[string]string{}, readerTitlesStoppedRows + 1, readerTitlesEveryRow + 1},
	} {
		t.Run(reader.name, func(t *testing.T) {
			recorder := &recordingTitlesQuerier{pool: database.Pool}
			titles, err := delivery.ResolveSessionTitles(context.Background(), recorder, readerTitleSessions,
				readableEvents(model.Actor{Viewer: reader.viewer}))
			if err != nil {
				t.Fatalf("resolve the titles: %v", err)
			}
			if !maps.Equal(titles, reader.want) {
				t.Errorf("the titles = %v, want %v", titles, reader.want)
			}

			plan := planWithoutSeqScans(t, database, recorder.sql, recorder.args...)
			if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
				t.Errorf("the reader's session titles read reads %v, which no index serves:\n%s", unkeyed, plan)
			}
			explaintest.NewIndexKeys(database.Pool).RequireIndexKeyed(t, plan, index, "the reader's session titles read", explaintest.CondRequired)

			root, executed, raw := analyzedTitleRead(t, database, recorder.sql, recorder.args...)
			reads, touched := 0, float64(0)
			for _, node := range root.everyNode() {
				if node.IndexName != index {
					continue
				}
				reads++
				touched += (node.Rows + node.Filtered) * node.Loops
				if node.Loops != float64(len(readerTitleSessions)) {
					t.Errorf("the read reads events %v times for %d sessions; want one ordered read a session:\n%s", node.Loops, len(readerTitleSessions), raw)
				}
			}
			if reads != 1 {
				t.Errorf("the read reads events through %s %d times; want the lateral's one read:\n%s", index, reads, raw)
			}
			if touched < reader.least || touched > reader.most {
				t.Errorf("the read touched %v rows of %s; want %v to %v of the %d the listed sessions have:\n%s",
					touched, index, reader.least, reader.most, readerTitlesEveryRow, raw)
			}
			t.Logf("%s: %v of %d events touched, %.3f ms executed", reader.name, touched, readerTitlesEveryRow, executed)
		})
	}
	// The server's own shape, for the comparison the log carries: the same read under the rule
	// every event passes. It asserts nothing (delivery's own plan test holds that read), so it runs
	// only where the log is shown.
	if !testing.Verbose() {
		return
	}
	recorder := &recordingTitlesQuerier{pool: database.Pool}
	if _, err := delivery.ResolveSessionTitles(context.Background(), recorder, readerTitleSessions, events.EveryEvent); err != nil {
		t.Fatalf("resolve the titles as the server: %v", err)
	}
	_, executed, _ := analyzedTitleRead(t, database, recorder.sql, recorder.args...)
	t.Logf("the server's own shape: %.3f ms executed", executed)
}

// analyzedTitleRead runs statement under `EXPLAIN (ANALYZE, FORMAT JSON)` with sequential scans
// off, in a transaction rolled back, and answers its root node, its execution time in
// milliseconds and its text. The fixture is a handful of rows, which a sequential scan reads
// fastest whatever the statement, so with it on the plan would say nothing about the statement.
func analyzedTitleRead(t *testing.T, database *store.Store, statement string, arguments ...any) (titleReadNode, float64, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	var plans []struct {
		Plan     titleReadNode `json:"Plan"`
		Executed float64       `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode the explain answer (%v): %s", err, raw)
	}
	return plans[0].Plan, plans[0].Executed, string(raw)
}
