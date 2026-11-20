package access

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestAnOpenViewerReadsNoOtherSessionWhileTheFillIsUnfinished: session_projects places every
// session in the projects it acted in only once `envoy-dispatch backfill-session-projects` has
// written session_projects_filled's row. Before that - after migration
// 20261116081157_session_projects_proven, and while `backfill-session-projects --reset` refills a
// deployment that already holds a private project - a session that acted in a private project may
// have no row placing it there, and the open arm, which refuses only a session a row places in a
// project the viewer does not see, would show it. So while the marker is absent and a project is
// private, an open viewer reads no session but its own, and no conversation that names another;
// once the marker is written it reads every session no row places in a project it does not see,
// one with no row by its listener record (SessionClause). With no private project nothing changes
// (R9).
func TestAnOpenViewerReadsNoOtherSessionWhileTheFillIsUnfinished(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	// ses_pub acted in PUB and its pair is recorded; ses_unrecorded acted in PRIV before the fill
	// reached its event, so no row places it anywhere yet, and the listener lists it with a
	// credential that names no project ("projects":[]: a record with no projects key names no list,
	// and passes no open viewer while a project is private, filled or not).
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects (session_id, project_key) values ('ses_pub', 'PUB');
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, '[{"session_id":"ses_unrecorded","projects":[]}]', now(), now() + interval '1 hour');
	`); err != nil {
		t.Fatalf("seed ses_pub's pair and ses_unrecorded's listener record: %v", err)
	}
	aliceSession := model.Actor{Kind: "session", ID: "ses_alice", Owner: new(alice), Credential: &model.Credential{Scopes: model.Scopes, Session: "ses_alice"}}
	resolve := func(name string, actor model.Actor) model.Viewer {
		t.Helper()
		viewer, err := Resolve(ctx, database.Pool, actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		if !viewer.Open {
			t.Fatalf("%s resolves as %+v, want an open viewer", name, viewer)
		}
		return viewer
	}
	answers := func(viewer model.Viewer, sql string, arg any) bool {
		t.Helper()
		var got bool
		if err := database.Pool.QueryRow(ctx, `select `+sql, viewerArg(t, viewer), arg).Scan(&got); err != nil {
			t.Fatalf("%.40s…: %v", sql, err)
		}
		return got
	}
	type probe struct {
		name string
		sql  string
		arg  any
	}
	others := []probe{
		{"SessionClause ses_pub", SessionClause("$2", "$1"), "ses_pub"},
		{"SessionClause ses_unrecorded", SessionClause("$2", "$1"), "ses_unrecorded"},
		{"PartyClause carol and ses_unrecorded", PartyClause("$2", "$1"), []string{"person:" + carol, "session:ses_unrecorded"}},
	}
	own := probe{"SessionClause ses_alice, her own", SessionClause("$2", "$1"), "ses_alice"}

	alicePerson, aliceAgent := resolve("alice", cookie(alice)), resolve("alice's session", aliceSession)
	if !alicePerson.AnyPrivate {
		t.Fatalf("alice resolves as %+v, want a deployment that holds a private project", alicePerson)
	}
	for _, p := range others {
		if answers(alicePerson, p.sql, p.arg) {
			t.Errorf("unfilled, with PRIV private: %s for alice = true, want false", p.name)
		}
	}
	if !answers(aliceAgent, own.sql, own.arg) {
		t.Errorf("unfilled, with PRIV private: %s = false, want true", own.name)
	}

	if _, err := database.Pool.Exec(ctx, `insert into session_projects_filled default values`); err != nil {
		t.Fatalf("write the fill's marker: %v", err)
	}
	for _, p := range others {
		if !answers(alicePerson, p.sql, p.arg) {
			t.Errorf("filled: %s for alice = false, want true", p.name)
		}
	}
	if !answers(aliceAgent, own.sql, own.arg) {
		t.Errorf("filled: %s = false, want true", own.name)
	}

	if _, err := database.Pool.Exec(ctx, `
		delete from session_projects_filled;
		update projects set visibility = 'public';
	`); err != nil {
		t.Fatalf("empty the marker and make every project public: %v", err)
	}
	alicePerson = resolve("alice, with no private project", cookie(alice))
	if alicePerson.AnyPrivate {
		t.Fatalf("alice resolves as %+v with every project public, want no private project", alicePerson)
	}
	for _, p := range others {
		if !answers(alicePerson, p.sql, p.arg) {
			t.Errorf("unfilled, with no private project: %s for alice = false, want true (R9)", p.name)
		}
	}
}

// TestTheFillMarkerIsReadOncePerStatement: the open arm's read of session_projects_filled names no
// column of the row it tests, so the statement reads it once (an InitPlan), not once per row. A
// read over fifty sessions, for an open viewer of a deployment that holds a private project, both
// with the marker and without it, scans session_projects_filled in one loop.
func TestTheFillMarkerIsReadOncePerStatement(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	alicePerson, err := Resolve(ctx, database.Pool, cookie(alice))
	if err != nil {
		t.Fatalf("alice: Resolve: %v", err)
	}
	for _, state := range []struct{ name, sql string }{
		{"unfilled", `delete from session_projects_filled`},
		{"filled", `insert into session_projects_filled default values`},
	} {
		if _, err := database.Pool.Exec(ctx, state.sql); err != nil {
			t.Fatalf("%s: %v", state.name, err)
		}
		if scans, total := planLoops(t, database.Pool, "session_projects_filled",
			`select count(*) from generate_series(1, 50) as n where `+SessionClause("'ses_' || n", "$1"),
			viewerArg(t, alicePerson)); total != 1 {
			t.Errorf("%s: the read over fifty sessions scans session_projects_filled in %d nodes, %d loops in all; want one loop", state.name, scans, total)
		}
	}
}

// planLoops explains sql, run with args, and answers how many of its plan's nodes scan relation and
// how many loops they ran in all. A plan that scans relation nowhere fails the test, since a count
// over it would measure nothing.
func planLoops(t *testing.T, q Querier, relation, sql string, args ...any) (scans, total int) {
	t.Helper()
	return relationLoops(t, relation, analyzedPlan(t, q, sql, args...))
}

// analyzedPlan is the raw answer of explaining sql, run with args through q, with what it measured
// (explain analyze): the one plan explaintest.OnePlan reads.
func analyzedPlan(t *testing.T, q Querier, sql string, args ...any) []byte {
	t.Helper()
	var raw []byte
	if err := q.QueryRow(context.Background(), `explain (analyze, costs off, timing off, format json) `+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain the read: %v", err)
	}
	return raw
}

// relationLoops is planLoops over raw, an analyzed plan: how many of its nodes scan relation and
// how many loops they ran in all, failing the test where no node scans it.
func relationLoops(t *testing.T, relation string, raw []byte) (scans, total int) {
	t.Helper()
	for _, scan := range explaintest.Scans(t, explaintest.OnePlan(t, raw)) {
		if scan.Relation == relation {
			scans++
			total += int(scan.Loops)
		}
	}
	if scans == 0 {
		var plan []map[string]any
		_ = json.Unmarshal(raw, &plan)
		encoded, _ := json.Marshal(plan)
		t.Fatalf("the plan reads %s nowhere, so this measures nothing:\n%s", relation, encoded)
	}
	return scans, total
}

// TestTheListenerRecordUsesItsPrimaryKey (M15): SessionClause is the statement's own SQL, so a
// read that binds it plans the listener record's read with the statement, where EXPLAIN shows it:
// a read over fifty sessions none of which has a pair, for an open viewer and for one that is not
// open, of a deployment that holds a private project and has filled session_projects, reads
// route_roster_session_projects through its primary key, keyed on id, and no node of it runs
// more than once, since the read names no column of the row the clause tests (an InitPlan): the
// open arm reads it once in all. A clause that called a database function would show no such
// read, since a function's body is not part of the plan (and a SQL function whose body reads a
// table is never inlined), and the test fails there reading the relation nowhere.
//
// Control: drop route_roster_session_projects_pkey in the test's own transaction; the plan reads
// the projection by a sequential scan, and the test fails naming the index it searches nowhere.
func TestTheListenerRecordUsesItsPrimaryKey(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects_filled default values;
		insert into route_roster (answered, sessions, sent_at, expires_at)
		select true, jsonb_agg(jsonb_build_object('session_id', 'ses_' || n, 'projects', jsonb_build_array(case when n % 2 = 0 then 'PUB' else 'PRIV' end))),
			now(), now() + interval '1 hour'
		from generate_series(1, 50) as n;
	`); err != nil {
		t.Fatalf("seed the listener's record of fifty sessions: %v", err)
	}
	for _, person := range []string{alice, bob} {
		viewer, err := Resolve(ctx, database.Pool, cookie(person))
		if err != nil {
			t.Fatalf("%s: Resolve: %v", person, err)
		}
		if !viewer.AnyPrivate {
			t.Fatalf("%s resolves as %+v, want a deployment that holds a private project", person, viewer)
		}
		subject := "the read of fifty sessions' listener records for " + person
		explained := func() []byte {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin %s's plan: %v", person, err)
			}
			// Rolled back however the read ends, so a failed plan leaves no transaction holding a
			// connection the test database's drop waits on.
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `set local enable_seqscan = off`); err != nil {
				t.Fatalf("prefer the listener-record index: %v", err)
			}
			raw := analyzedPlan(t, tx, `select count(*) from generate_series(1, 50) as n where `+SessionClause("'ses_' || n", "$1"), viewerArg(t, viewer))
			explaintest.NewIndexKeys(tx).RequireIndexKeyed(t, explaintest.OnePlan(t, raw), "route_roster_session_projects_pkey", subject, explaintest.CondRequired)
			return raw
		}
		raw := explained()
		scans, total := relationLoops(t, "route_roster_session_projects", raw)
		plan := explaintest.OnePlan(t, raw)
		for _, scan := range explaintest.Scans(t, plan) {
			if scan.Relation == "route_roster_session_projects" && scan.Loops > 1 {
				t.Errorf("%s reads route_roster_session_projects %v times in one node, want at most once a statement:\n%s", subject, scan.Loops, plan)
			}
		}
		if person == alice && total != 1 {
			t.Errorf("%s reads route_roster_session_projects in %d nodes, %d loops in all; want one loop:\n%s", subject, scans, total, plan)
		}
		if total < 1 {
			t.Errorf("%s ran no read of route_roster_session_projects, so the arm under test never ran:\n%s", subject, plan)
		}
	}
}
