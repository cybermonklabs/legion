package metrics

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// roleEventsLogSize is the event log the role events' plan is taken over: a million events, the
// size at which legion's readSessionIssues recorded the planner trading the issues' index for the
// session partial index, and above the 831,000 of ten times INFRA's rate. It is the log, beside
// the three issues' 150 events, whose statistics the test states (roleEventsStatistics).
const roleEventsLogSize = 1_000_000

// roleEventsStatistics is the planner's statistics of events and of its indexes over a log of
// roleEventsLogSize events beside the three issues' 150, as PostgreSQL read them back when this
// test wrote that log itself: the file says at which commit, by which command, and which of that
// command's statements it keeps.
var roleEventsStatistics = filepath.Join("testdata", "role_events_statistics.sql")

// TestTheRoleEventsReadReadsTheIssuesRowsByIssue: the read of the first event each session wrote
// on each issue of a scope (roleEventsStatement) reads those issues' rows through events' unique
// (issue_key, seq), events_issue_key_seq_key, and never the partial index on a session's id
// (events_session_actor_created_at_idx) nor the whole table, under the plan with its values and
// under the generic plan a statement pgx caches can settle on, over a log of a million events.
//
// The log is three issues of fifty events each, half of them by role sessions, beside a million
// events every one of which a session wrote on another issue: the shape in which a statement that
// tests the issues and the session beside each other in one condition can read the session
// index, since the planner holds no statistics on actor's fields.
//
// The planner plans from a table's statistics and the pages its file holds, not from its rows, so
// the test writes the three issues' events beside 5,000 of the million's, each on a page of its
// own, and states the million's statistics over them (stateRoleEventsStatistics), as
// dispatch://INFRA-6335/spec (R2) lets a plan test do over the fewest rows at which the planner
// still makes each plan the test proves; writing the million took 23 s in CI. Run at 1a37a9ffe738
// on PostgreSQL 18.7, the test that wrote the million and this one made the same plans: the same
// nodes, through the same index, estimating the same rows.
func TestTheRoleEventsReadReadsTheIssuesRowsByIssue(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	exec := func(what, sql string, args ...any) int64 {
		t.Helper()
		tag, err := database.Pool.Exec(ctx, sql, args...)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return tag.RowsAffected()
	}
	exec("write the project", `insert into projects (key, name, default_assignee) values ('INFRA', 'INFRA', 'alice')`)
	// The planner takes events' pages from its file, not from the statistics the test states, and
	// weighs the issues' index scan against a bitmap scan by them: with the events on 2,045 pages
	// the generic plan reads the index through a bitmap scan, and from 4,086 through an index scan,
	// as on the million's 20,412. At fillfactor 10 an insert fills a page to a tenth of its 8 kB,
	// and each filler event is more than that with its 900-byte body, so each lies on a page of
	// its own. Autovacuum is kept off events, so that no analyze or vacuum of its own replaces
	// the statistics the test states.
	exec("lay the filler events one to a page", `alter table events set (fillfactor = 10, autovacuum_enabled = off)`)
	const fillerEvents, perIssue = 5_000, 100
	seeded := time.Now()
	exec("write the issues", `
		insert into issues (key, project_key, number, title, status, rank, created_by)
		select 'INFRA-' || n, 'INFRA', n, 'An issue', 'todo', lpad(n::text, 8, '0'), '{"kind":"user","id":"alice"}'::jsonb
		from generate_series(1, $1::int + 3) n`, fillerEvents/perIssue)
	logged := exec("write the filler events", `
		insert into events (issue_key, seq, type, actor, payload, notify, published_at, created_at)
		select 'INFRA-' || (4 + (n - 1) / $2::int), 1 + (n - 1) % $2::int, 'comment.created',
		       jsonb_build_object('kind', 'session', 'id', 's-' || (n % 5000)),
		       jsonb_build_object('body', repeat('x', 900)), false, now(), now() - make_interval(secs => n)
		from generate_series(1, $1::int) n`, fillerEvents, perIssue)
	logged += exec("write the three issues' events", `
		insert into events (issue_key, seq, type, actor, payload, notify, published_at, created_at)
		select 'INFRA-' || issue, n, 'comment.created',
		       case when n % 2 = 0
		            then jsonb_build_object('kind', 'session', 'id', 's-INFRA-' || issue || '-' || n,
		                   'origin', jsonb_build_object('session_title', 'Legion implementer · INFRA-' || issue))
		            else '{"kind":"user","id":"alice"}'::jsonb end,
		       '{}'::jsonb, false, now(), now() - make_interval(mins => n)
		from generate_series(1, 3) issue, generate_series(1, 50) n`)
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plan: %v", err)
	}
	defer connection.Release()
	stateRoleEventsStatistics(t, ctx, connection)
	t.Logf("seeded %d events over %d issues and stated a log of %d in %s", logged, fillerEvents/perIssue+3,
		roleEventsLogSize+150, time.Since(seeded).Round(time.Millisecond))

	keys := []string{"INFRA-1", "INFRA-2", "INFRA-3"}
	var literal string
	if err := database.Pool.QueryRow(ctx, `select quote_literal($1::text[]::text)`, keys).Scan(&literal); err != nil {
		t.Fatalf("quote the keys: %v", err)
	}

	valued := func() json.RawMessage {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the plan's transaction: %v", err)
		}
		defer tx.Rollback(ctx)
		var raw []byte
		if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+roleEventsStatement, keys).Scan(&raw); err != nil {
			t.Fatalf("explain the role events' read: %v", err)
		}
		return explaintest.OnePlan(t, raw)
	}
	for _, plan := range []struct {
		mode string
		plan json.RawMessage
	}{
		{"with its values", valued()},
		{"generic", explaintest.AnalyzedGenericPlan(t, ctx, connection, "role_events", roleEventsStatement, []string{"text[]"}, []string{literal + "::text[]"})},
	} {
		var root struct {
			Total float64 `json:"Actual Total Time"`
		}
		if err := json.Unmarshal(plan.plan, &root); err != nil {
			t.Fatalf("read the plan's time: %v", err)
		}
		t.Logf("the plan %s ran in %.3f ms:\n%s", plan.mode, root.Total, explaintest.Outline(t, plan.plan))
		if explaintest.SeqScansRelation(t, plan.plan, "events") {
			t.Errorf("the plan %s reads the whole event log:\n%s", plan.mode, explaintest.Outline(t, plan.plan))
		}
		byIssue := false
		for _, scan := range explaintest.IndexScans(t, plan.plan) {
			t.Logf("  it reads %s, keyed by %s", scan, scan.Cond)
			switch scan.Index {
			case "events_session_actor_created_at_idx":
				t.Errorf("the plan %s reads the session partial index, every session event of the log:\n%s",
					plan.mode, explaintest.Outline(t, plan.plan))
			case "events_issue_key_seq_key":
				byIssue = byIssue || strings.Contains(scan.Cond, "issue_key")
			}
		}
		if !byIssue {
			t.Errorf("the plan %s reads the issues' events through no scan of events_issue_key_seq_key keyed by issue_key:\n%s",
				plan.mode, explaintest.Outline(t, plan.plan))
		}
	}

	population := &Population{Issues: map[string]*Issue{}}
	for _, key := range keys {
		population.Issues[key] = &Issue{Key: key, FirstRoleEvent: map[string]time.Time{}}
	}
	if err := population.readRoleEvents(ctx, database.Pool, keys); err != nil {
		t.Fatalf("read the role events: %v", err)
	}
	for _, key := range keys {
		if _, worked := population.Issues[key].FirstRoleEvent["implementer"]; !worked {
			t.Errorf("%s holds no first implementer event, want the first of its twenty-five: %v", key, population.Issues[key].FirstRoleEvent)
		}
	}
	t.Logf("each of %v holds its implementer's first event", keys)
}

// stateRoleEventsStatistics states roleEventsStatistics on connection, in place of an analyze of
// the events the test wrote (a statistic the file leaves out, such as created_at's common values,
// would keep the analyze's value beside the million's), and fails t unless PostgreSQL takes every
// statistic the file states (a statement it takes in part answers false). The planner reads the
// reltuples of events, and of a partial index on it, as a density over relpages, which it applies
// to the pages the relation's file holds when it plans (estimate_rel_size). So each relation's
// relpages is then stated again as the pages its file holds, over which the planner estimates the
// million's rows.
func stateRoleEventsStatistics(t *testing.T, ctx context.Context, connection *pgxpool.Conn) {
	t.Helper()
	statistics, err := os.ReadFile(roleEventsStatistics)
	if err != nil {
		t.Fatalf("read the event log's statistics: %v", err)
	}
	results, err := connection.Conn().PgConn().Exec(ctx, string(statistics)).ReadAll()
	if err != nil {
		t.Fatalf("state the event log's statistics: %v", err)
	}
	for i, result := range results {
		if len(result.Rows) != 1 || string(result.Rows[0][0]) != "t" {
			t.Fatalf("statement %d of %s states its statistics in part or not at all: %q", i+1, roleEventsStatistics, result.Rows)
		}
	}
	var stated bool
	if err := connection.QueryRow(ctx, `
		select bool_and(pg_restore_relation_stats('schemaname', 'public', 'relname', relname::text,
		         'relpages', (pg_relation_size(oid) / current_setting('block_size')::int)::integer))
		from pg_class
		where oid = 'events'::regclass or oid in (select indexrelid from pg_index where indrelid = 'events'::regclass)`).Scan(&stated); err != nil || !stated {
		t.Fatalf("state the pages events and its indexes hold (stated: %t): %v", stated, err)
	}
	var pages int
	var rows float64
	if err := connection.QueryRow(ctx, `select relpages, reltuples from pg_class where oid = 'events'::regclass`).Scan(&pages, &rows); err != nil {
		t.Fatalf("read the statistics of events: %v", err)
	}
	t.Logf("stated the %d statements of %s: events holds %d pages, over which the planner estimates %.0f rows",
		len(results), roleEventsStatistics, pages, rows)
}
