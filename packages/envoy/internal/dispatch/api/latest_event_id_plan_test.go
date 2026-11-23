package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// eventIDStatements records, with its arguments, every statement the pool it traces sends that
// names the event its write appended last by store.LatestEventID, whether sent alone or on a
// write phase's batch (pgx.QueryTracer, pgx.BatchTracer).
type eventIDStatements struct {
	mu   sync.Mutex
	sent []eventIDStatement
}

// eventIDStatement is one statement eventIDStatements recorded.
type eventIDStatement struct {
	sql  string
	args []any
}

func (r *eventIDStatements) record(sql string, args []any) {
	if !strings.Contains(sql, store.LatestEventID) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, eventIDStatement{sql: sql, args: slices.Clone(args)})
}

func (r *eventIDStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.record(data.SQL, data.Args)
	return ctx
}

func (r *eventIDStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *eventIDStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (r *eventIDStatements) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	r.record(data.SQL, data.Args)
}

func (r *eventIDStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// take answers what r has recorded since the last take, and starts again.
func (r *eventIDStatements) take() []eventIDStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	sent := r.sent
	r.sent = nil
	return sent
}

// eventsScans is every node of statement's plan (planNode, search_index_test.go) that reads rows of
// events, as Postgres plans it on planned. The update of an event's own row is a node naming
// events too, which writes the rows the scan below it found and reads none itself, so it is left
// out.
func eventsScans(t *testing.T, planned *store.Pool, statement eventIDStatement) []planNode {
	t.Helper()
	var planJSON []byte
	if err := planned.QueryRow(context.Background(), "explain (format json) "+statement.sql, statement.args...).Scan(&planJSON); err != nil {
		t.Fatalf("explain %s: %v", statement.sql, err)
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode the plan of %s: %v (%s)", statement.sql, err, planJSON)
	}
	var scans []planNode
	var walk func(node planNode)
	walk = func(node planNode) {
		if node.Relation == "events" && node.NodeType != "ModifyTable" {
			scans = append(scans, node)
		}
		for _, child := range node.Plans {
			walk(child)
		}
	}
	walk(plans[0].Plan)
	return scans
}

// requireEventsReadByPrimaryKey fails unless every statement of sent that reads events reads it
// through events_pkey, and answers how many statements of sent read events and hold marker, so a
// caller can require the statements it is about were among them.
func requireEventsReadByPrimaryKey(t *testing.T, planned *store.Pool, sent []eventIDStatement, markers ...string) map[string]int {
	t.Helper()
	read := make(map[string]int, len(markers))
	for _, statement := range sent {
		scans := eventsScans(t, planned, statement)
		for _, scan := range scans {
			if scan.NodeType != "Index Scan" || scan.IndexName != "events_pkey" {
				t.Errorf("the statement reads events by a %s (index %q), not an index scan of events_pkey:\n%s",
					scan.NodeType, scan.IndexName, statement.sql)
			}
		}
		if len(scans) == 0 {
			continue
		}
		for _, marker := range markers {
			if strings.Contains(statement.sql, marker) {
				read[marker]++
			}
		}
	}
	return read
}

// A statement that finds the event its write appended by store.LatestEventID reads that one row
// through events' primary key. Written as `events.id = currval(...)`, the comparison was with a
// volatile function, which Postgres evaluates row by row and cannot use as an index key, so each
// such statement read every row of events while it held its owner's lock and its transaction id
// (INFRA-5126): opening an approval request read the table once (the ask.opened identity), and
// answering it read the table twice for each document it named (the review's digest and its
// sequence), four whole reads of events for a spec and its Verification document. In production
// that table is large enough that an approval took up to 64 seconds to answer.
//
// The rows here are too few for Postgres to choose an index scan as it would for a production
// table, so each recorded statement is planned on a connection that scans a table sequentially
// only where no index can serve (storetest.IndexScan): there, a statement whose comparison an
// index can take is an index scan, and one whose comparison none can take is still a sequential
// scan.
func TestTheStatementsThatFindTheirEventByItsIDReadEventsByItsPrimaryKey(t *testing.T) {
	recorder := &eventIDStatements{}
	database := storetest.Open(t)
	handler, _, _ := newTestServer(t, testServerOptions{store: database, tracer: recorder})
	planned := storetest.Planned(t, database.Pool, storetest.IndexScan)

	issue := createInteractionIssue(t, handler, "EVID", "Approve both by the key", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "# Verification\n\nOne.\n")
	recorder.take()

	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
		map[string]any{"actor": sessionActor(), "include_verification": true})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval of both: status=%d body=%s", requested.Code, requested.Body.String())
	}
	opened := requireEventsReadByPrimaryKey(t, planned, recorder.take(), "opened_event_id")
	if opened["opened_event_id"] == 0 {
		t.Fatalf("the approval request sent no statement reading events for the ask.opened identity: nothing here was checked")
	}

	approved := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{"state": "approved"}, "alice")
	if approved.Code != http.StatusCreated {
		t.Fatalf("approve both: status=%d body=%s", approved.Code, approved.Body.String())
	}
	reviewed := requireEventsReadByPrimaryKey(t, planned, recorder.take(), "content_sha256", "set event_seq")
	// One review of each document the request named, each with its digest and its sequence.
	if reviewed["content_sha256"] < 2 || reviewed["set event_seq"] < 2 {
		t.Fatalf("the approval sent %d digest and %d sequence statements reading events, want one of each per document (2): nothing here was checked",
			reviewed["content_sha256"], reviewed["set event_seq"])
	}
}
