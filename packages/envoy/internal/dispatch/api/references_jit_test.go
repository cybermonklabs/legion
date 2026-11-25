package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The issue references read runs with JIT off whatever its session starts with (inClosureRead):
// stock Postgres starts every session at jit = on, under which JIT's compilation alone once took
// the read past its bound. GET /api/v1/issues/{key}/references is served by a handler whose store
// is a pool of one connection whose sessions start at jit = on, so the session the route reads in
// and the session read after it are the one connection's, and a tracer on that pool records each
// statement the connection sends, in order (jitOnRoutes). The route answers 200; the statements of
// its closure, each starting with refs.ClosureTag, run in one transaction, whose first statement
// after its begin is the closure's set local jit = off, before any other statement of the closure;
// and after the read the same session reads jit on, so the setting was the transaction's alone.
// The negative controls: inClosureRead without its set local jit = off, under which the
// transaction's first statement after its begin is the budget's set_config of statement_timeout;
// and the route running refs.Closure on the pool rather than inside inClosureRead, under which the
// closure's statements run in no transaction.
func TestTheIssueReferencesReadRunsWithJITOff(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	routes, jitOn, sent := jitOnRoutes(t, database, deps)
	if response := issueReferencesRequest(t, routes, issue.Key, ""); response.Code != http.StatusOK {
		t.Fatalf("references of %s on a session that starts at jit = on: status=%d body=%s, want 200", issue.Key, response.Code, response.Body.String())
	}
	requireSessionJITOn(t, jitOn, jitOffTransactionBackend(t, sent.all(), closureJITOff))
}

// The references list read runs with JIT off whatever its session starts with
// (inReferencesListRead): its statement over graph_edges took 136.5 ms a call with JIT on and 21.6
// to 27.8 ms with JIT off. GET /api/v1/references is served as the issue references read is above,
// on one traced connection whose sessions start at jit = on (jitOnRoutes). A comment on one issue
// cites another, and the route reads ?to= the cited issue and ?from= the comment, each of which
// lists the comment's mention. Each read answers 200 with an edge; the statements of its list -
// its statement over graph_edges, and on the ?to= read its read of the excerpts stored on document
// mentions - run in one transaction, whose first statement after its begin is the list read's set
// local jit = off, before any other statement of the list; and after the read the same session
// reads jit on, so the setting was the transaction's alone. The negative controls:
// inReferencesListRead without its set local jit = off, under which the transaction's first
// statement after its begin is the statement over graph_edges; and the route reading refs.Edges
// on the pool, under which that statement runs in no transaction.
func TestTheReferencesListReadRunsWithJITOff(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	cited := createReferenceAPIIssue(t, handler, "CORE")
	citing := createReferenceAPIIssue(t, handler, "OPS")
	comment := createReferenceAPIComment(t, handler, citing.Key, "see dispatch://"+cited.Key)
	routes, jitOn, sent := jitOnRoutes(t, database, deps)
	for _, read := range []struct {
		name  string
		query url.Values
	}{
		{"to", url.Values{"to": {"dispatch://" + cited.Key}}},
		{"from", url.Values{"from": {"dispatch://" + citing.Key + "/comment/" + comment.ID}}},
	} {
		t.Run(read.name, func(t *testing.T) {
			before := len(sent.all())
			response := graphRequest(t, routes, read.query)
			if response.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/references?%s on a session that starts at jit = on: status=%d body=%s, want 200", read.query.Encode(), response.Code, response.Body.String())
			}
			if edges := decodeBody[model.GraphReferences](t, response).Edges; len(edges) == 0 {
				t.Fatalf("GET /api/v1/references?%s lists no edge, want the comment's mention: body=%s", read.query.Encode(), response.Body.String())
			}
			requireSessionJITOn(t, jitOn, jitOffTransactionBackend(t, sent.all()[before:], listJITOff))
		})
	}
}

// jitOnRoutes serves deps's routes on a store of one connection whose sessions start at jit = on,
// so a read the routes serve and a statement on the store after it run in the one session, and a
// tracer on that store records each statement the connection sends, in order. It answers the
// routes, the store and the tracer.
func jitOnRoutes(t *testing.T, database *store.Store, deps Deps) (http.Handler, *store.Store, *backendStatements) {
	t.Helper()
	sent := &backendStatements{}
	jitOn := storetest.Reopened(t, database, func(config *pgxpool.Config) {
		config.MaxConns = 1
		config.MinConns = 0
		config.MinIdleConns = 0
		config.ConnConfig.RuntimeParams["jit"] = "on"
		config.ConnConfig.Tracer = sent
	})
	deps.Store = jitOn
	routes := http.NewServeMux()
	// Registered after the one-connection store, so this server's wait runs before that store
	// closes.
	t.Cleanup(Register(routes, t.Context(), deps).Wait)
	return routes, jitOn, sent
}

// requireSessionJITOn requires that the session of jitOn's one connection after a read is the
// read's own, backend readBackend, and that it reads jit on, the value its sessions start with, so
// that the read's setting was its transaction's alone.
func requireSessionJITOn(t *testing.T, jitOn *store.Store, readBackend uint32) {
	t.Helper()
	var jit string
	var backend int32
	if err := jitOn.Pool.QueryRow(context.Background(), "select current_setting('jit'), pg_backend_pid()").Scan(&jit, &backend); err != nil {
		t.Fatalf("read jit on the pool after the read: %v", err)
	}
	if uint32(backend) != readBackend {
		t.Fatalf("the session after the read is backend %d, want the read's own, backend %d", backend, readBackend)
	}
	if jit != "on" {
		t.Errorf("jit on the read's own session after the read reads %q, want on, the value its sessions start with", jit)
	}
}

// jitOffRead is a read a route runs with JIT off, as jitOffTransactionBackend finds it among the
// statements a pool's connections sent: name, the read as a failure names it; statement, whether
// one statement's SQL is the read's, and none, what a failure says of a route that sent none of
// them; jitOff, the setting the read's transaction sends first; and outside, what a statement of
// the read that ran in no transaction says of the route.
type jitOffRead struct {
	name, none, jitOff, outside string
	statement                   func(sql string) bool
}

// closureJITOff is the issue references read (inClosureRead): every statement of refs.Closure, and
// the setting of its transaction, starts with refs.ClosureTag.
var closureJITOff = jitOffRead{
	name:      "the closure",
	none:      "none starting with refs.ClosureTag",
	jitOff:    refs.ClosureTag + "set local jit = off",
	outside:   "the route ran refs.Closure outside inClosureRead's transaction",
	statement: func(sql string) bool { return strings.HasPrefix(sql, refs.ClosureTag) },
}

// listJITOff is the references list read (inReferencesListRead), found by the heads of its two
// statements of its own: readEdgeRows's over graph_edges and StoredExcerpts's of the excerpts
// stored on document mentions, which refs does not export, so they are written again here. The
// statements of the loaders refs.Edges runs for the far ends of its edges have no head of their
// own and run after the first. No statement of the read carries a tag, and nor does its setting.
var listJITOff = jitOffRead{
	name:    "the list read",
	none:    "neither its statement over graph_edges nor its read of stored excerpts",
	jitOff:  "set local jit = off",
	outside: "the route read its edges or their stored excerpts outside inReferencesListRead's transaction",
	statement: func(sql string) bool {
		return strings.HasPrefix(sql, "select kind, other_kind, other_id, created_at, source_seq, at_block from (") ||
			strings.HasPrefix(strings.TrimSpace(sql), "select r.from_id, coalesce(r.excerpt_block_id, ''), r.excerpt_text from refs r")
	},
}

// jitOffTransactionBackend requires of sent, every statement a pool's connections sent in order,
// that the statements of read ran on one backend in one transaction, and that the first statement
// of that transaction after its begin is read's setting of jit off. It answers that backend.
func jitOffTransactionBackend(t *testing.T, sent []backendStatement, read jitOffRead) uint32 {
	t.Helper()
	isRead := func(statement backendStatement) bool { return read.statement(statement.sql) }
	first := slices.IndexFunc(sent, isRead)
	if first < 0 {
		t.Fatalf("the route sent no statement of %s, %s:\n%s", read.name, read.none, backendStatementLines(sent))
	}
	backend := sent[first].backend
	begun, readBegun := -1, -1
	for index, statement := range sent {
		ofRead := isRead(statement)
		if statement.backend != backend {
			if ofRead {
				t.Fatalf("statement %d, of %s, ran on backend %d, and %s's first on backend %d:\n%s", index, read.name, statement.backend, read.name, backend, backendStatementLines(sent))
			}
			continue
		}
		switch {
		case strings.HasPrefix(statement.sql, "begin"):
			begun = index
		case statement.sql == "commit" || statement.sql == "rollback":
			begun = -1
		case !ofRead:
		case begun < 0:
			t.Fatalf("statement %d, of %s, ran in no transaction: %s, where nothing sets jit off for it:\n%s", index, read.name, read.outside, backendStatementLines(sent))
		case readBegun < 0:
			readBegun = begun
		case begun != readBegun:
			t.Fatalf("statement %d, of %s, ran in the transaction begun at statement %d, and %s's first in the one begun at statement %d, want one transaction:\n%s", index, read.name, begun, read.name, readBegun, backendStatementLines(sent))
		}
	}
	next := readBegun + 1
	for sent[next].backend != backend {
		next++
	}
	if sent[next].sql != read.jitOff {
		t.Fatalf("statement %d, the first of %s's transaction after its begin at statement %d, is %s, want %q, before any other statement of %s:\n%s", next, read.name, readBegun, shownSQL(sent[next].sql), read.jitOff, read.name, backendStatementLines(sent))
	}
	return backend
}

// backendStatement is one statement a connection sent: the backend it ran on and its SQL.
type backendStatement struct {
	backend uint32
	sql     string
}

// backendStatementLines is sent one to a line, each with its index and backend and its SQL
// (shownSQL), for a failure to show.
func backendStatementLines(sent []backendStatement) string {
	lines := make([]string, len(sent))
	for index, statement := range sent {
		lines[index] = fmt.Sprintf("%d backend %d: %s", index, statement.backend, shownSQL(statement.sql))
	}
	return shownLines(lines)
}

// shownSQL is sql quoted and cut to 120 bytes, for a failure to show.
func shownSQL(sql string) string {
	quoted := strconv.Quote(sql)
	if len(quoted) > 120 {
		quoted = quoted[:120] + "..."
	}
	return quoted
}

// backendStatements records each statement the connections of the pool it traces send, with the
// backend each ran on, in the order they started (pgx.QueryTracer).
type backendStatements struct {
	mu   sync.Mutex
	sent []backendStatement
}

func (b *backendStatements) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, backendStatement{backend: conn.PgConn().PID(), sql: data.SQL})
	return ctx
}

func (b *backendStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// all is every statement b has recorded, in the order they started.
func (b *backendStatements) all() []backendStatement {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.sent)
}
