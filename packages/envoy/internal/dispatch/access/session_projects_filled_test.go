package access

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
	// credential that names no project.
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects (session_id, project_key) values ('ses_pub', 'PUB');
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, '[{"session_id":"ses_unrecorded"}]', now(), now() + interval '1 hour');
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
	var plan []map[string]any
	if err := q.QueryRow(context.Background(), `explain (analyze, costs off, timing off, format json) `+sql, args...).Scan(&plan); err != nil {
		t.Fatalf("explain the read: %v", err)
	}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == relation {
			scans++
			loops, _ := node["Actual Loops"].(float64)
			total += int(loops)
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if child, ok := child.(map[string]any); ok {
				walk(child)
			}
		}
	}
	for _, root := range plan {
		if node, ok := root["Plan"].(map[string]any); ok {
			walk(node)
		}
	}
	if scans == 0 {
		encoded, _ := json.Marshal(plan)
		t.Fatalf("the plan reads %s nowhere, so this measures nothing:\n%s", relation, encoded)
	}
	return scans, total
}

// TestTheListenerRecordIsReadOncePerStatement: SessionClause reads the listener's record of a
// session with no pair from the route_roster row (listenerProjects), a read that names no column
// of the row it tests, so a statement reads the row once (an InitPlan), not once per session. A
// read over fifty sessions the row lists, none with a pair, for an open viewer and for a
// restricted one of a deployment that holds a private project, scans route_roster in one loop.
func TestTheListenerRecordIsReadOncePerStatement(t *testing.T) {
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
		if scans, total := planLoops(t, database.Pool, "route_roster",
			`select count(*) from generate_series(1, 50) as n where `+SessionClause("'ses_' || n", "$1"),
			viewerArg(t, viewer)); total != 1 {
			t.Errorf("%s: the read over fifty sessions scans route_roster in %d nodes, %d loops in all; want one loop", person, scans, total)
		}
	}
}
