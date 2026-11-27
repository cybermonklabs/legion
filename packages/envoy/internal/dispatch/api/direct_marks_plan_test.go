package api

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestTheDirectMarksReadTwoProbesASession holds directMarks, the read of a person's read marks and
// Clears that GET /api/v1/me/agents/state, the conversation window and the Inbox's Agent rows share
// (unreadDirectRepliesCTE, myAgentMarks), to the cost its index skip is written for: a person
// holding ten thousand marks on five sessions - a read mark and a Clear stated a thousand times on
// each - is answered by reading a few rows of graph_tuples a session, under EXPLAIN (ANALYZE,
// BUFFERS), however many marks stand behind them, and the answer is each session's newest mark of
// each kind. Each of the three ways a read names its sessions is held to it: every session, one
// session, and every session through the narrowed read with no session named.
func TestTheDirectMarksReadTwoProbesASession(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	const sessions, marks = 5, 10000
	// Mark n is a read mark where n is even and a Clear where it is odd, into session s<n mod 5>,
	// each a second after the one before: two and five share no factor, so every session holds a
	// thousand of each kind, and its newest of each kind is its latest.
	start := time.Now().UTC().Add(-marks * time.Second).Truncate(time.Second)
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select $1, 'alice', case when n % 2 = 0 then $2 else $3 end, $4, 's' || (n % $5),
		       $6::timestamptz + make_interval(secs => n), '{"kind":"user","id":"alice"}', n, true
		from generate_series(1, $7::int) as n
	`, graphstore.KindPerson, graphstore.TupleReadThrough, graphstore.TupleCleared, graphstore.KindSession,
		sessions, start, marks); err != nil {
		t.Fatalf("seed alice's marks: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `analyze graph_tuples`); err != nil {
		t.Fatalf("analyze graph_tuples: %v", err)
	}
	// What each session's marks are, from the seed's own arithmetic: the newest read mark and
	// the newest Clear of each session are its last even and its last odd n.
	newest := func(session int, even bool) time.Time {
		last := marks
		for last%sessions != session || (last%2 == 0) != even {
			last--
		}
		return start.Add(time.Duration(last) * time.Second)
	}
	want := func(only ...int) []string {
		var rows []string
		for session := range sessions {
			if len(only) > 0 && !slices.Contains(only, session) {
				continue
			}
			read, cleared := newest(session, true), newest(session, false)
			later := read
			if cleared.After(read) {
				later = cleared
			}
			rows = append(rows, fmt.Sprintf("s%d read %s cleared %s at %s", session,
				read.Format(time.RFC3339), cleared.Format(time.RFC3339), later.Format(time.RFC3339)))
		}
		return rows
	}

	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	one := "s3"
	for _, read := range []struct {
		name     string
		narrowed bool
		args     []any
		sessions int
		want     []string
	}{
		{name: "every session", args: []any{"alice", "alice"}, sessions: sessions, want: want()},
		{name: "one session", narrowed: true, args: []any{"alice", "alice", &one}, sessions: 1, want: want(3)},
		{name: "the narrowed read naming none", narrowed: true, args: []any{"alice", "alice", (*string)(nil)}, sessions: sessions, want: want()},
	} {
		t.Run(read.name, func(t *testing.T) {
			statement := `select session_id, read_through, cleared_before, at from (` + directMarks(read.narrowed) + `) marks order by session_id`
			rows, err := database.Pool.Query(ctx, statement, read.args...)
			if err != nil {
				t.Fatalf("read the marks: %v", err)
			}
			var got []string
			for rows.Next() {
				var session string
				var readThrough, cleared, at time.Time
				if err := rows.Scan(&session, &readThrough, &cleared, &at); err != nil {
					t.Fatalf("scan a mark: %v", err)
				}
				got = append(got, fmt.Sprintf("%s read %s cleared %s at %s", session,
					readThrough.UTC().Format(time.RFC3339), cleared.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339)))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read the marks: %v", err)
			}
			if !slices.Equal(got, read.want) {
				t.Errorf("the marks read are\n  %v\nwant each session's newest of each kind\n  %v", got, read.want)
			}

			plan, text := explainDismissedScaleStatement(t, connection.Conn(), read.name, statement, read.args)
			t.Logf("the marks of %s, out of %d:\n%s", read.name, marks, text)
			if explaintest.SeqScansRelation(t, plan, "graph_tuples") {
				t.Errorf("the read scans graph_tuples:\n%s", text)
			}
			// Two kinds, each a probe for the first session, one a session for the next and one
			// past the last, and one for each session's newest tuple: at most four rows a session
			// of each kind, where a read of the marks whole answers every one of them.
			readRows := 0.0
			for _, scan := range explaintest.Scans(t, plan) {
				if scan.Relation != "graph_tuples" {
					continue
				}
				if scan.Index != "graph_tuples_current" {
					t.Errorf("the read takes graph_tuples through %q, want graph_tuples_current:\n%s", scan.Index, text)
				}
				readRows += scan.Loops * scan.Rows
			}
			if bound := float64(2 * 4 * read.sessions); readRows > bound {
				t.Errorf("the read of %d sessions answered %.0f rows of graph_tuples out of %d marks, want at most %.0f:\n%s",
					read.sessions, readRows, marks, bound, text)
			}
		})
	}
}
