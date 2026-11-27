package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// TestAnOpenViewerReadsNoOtherSessionWhileTheFillIsUnfinished: session_projects places every
// session in the projects it acted in only once the backfill a later slice adds has written
// session_projects_filled's row. Before that - after migration
// 20261116081157_session_projects_proven, and while that backfill's reset refills a
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
	// credential that names PUB ("projects":["PUB"]: a record with no projects key names no list,
	// and passes no open viewer while a project is private, filled or not). The record names a
	// project because PartyClause holds a session party to the content rule too
	// (SessionContentClause), which passes a listed session only when its list names a project or
	// it wrote in one; [] would pass SessionClause and fail the conversation for that reason alone.
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects (session_id, project_key) values ('ses_pub', 'PUB');
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, '[{"session_id":"ses_unrecorded","projects":["PUB"]}]', now(), now() + interval '1 hour');
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

// TestARestrictedViewerReadsNoOtherSessionWhileTheFillIsUnfinished: the reset of the backfill a
// later slice adds empties session_projects and its marker and refills the pairs one by one, so in the
// middle of a refill a session can hold its pair in a project the viewer sees and not yet its pair
// in a private project the viewer does not see. The arm of a viewer that is not open trusted the
// pairs as the open arm, before it waited for the marker, did not: so while a project is private
// and the marker is absent, bob, a restricted member who sees PRIV alone, and bob's token that
// lists PRIV each read no session but bob's own - not ses_refilled, whose PRIV pair is back and
// whose OTHER pair is not, nor ses_listed, which has no pair and a listener record naming PRIV (the
// arm of a restricted viewer that reads a listed record). Once the marker is written both read
// them again, and with no private project both read them by their pairs and records alone, marker
// or none (R9).
//
// Control: SessionClause passing the marker for an open viewer alone, as it did, and bob reads
// ses_refilled and ses_listed while the refill is unfinished.
func TestARestrictedViewerReadsNoOtherSessionWhileTheFillIsUnfinished(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects (session_id, project_key) values ('ses_refilled', 'PRIV');
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, '[{"session_id":"ses_listed","projects":["PRIV"]}]', now(), now() + interval '1 hour');
	`); err != nil {
		t.Fatalf("seed ses_refilled's pair and ses_listed's listener record: %v", err)
	}
	bobSession := model.Actor{Kind: "session", ID: "ses_bob", Owner: new(bob), Credential: &model.Credential{Scopes: model.Scopes, Session: "ses_bob"}}
	resolve := func(name string, actor model.Actor) model.Viewer {
		t.Helper()
		viewer, err := Resolve(ctx, database.Pool, actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		if viewer.Open {
			t.Fatalf("%s resolves as %+v, want a viewer that is not open", name, viewer)
		}
		return viewer
	}
	passes := func(viewer model.Viewer, session string) bool {
		t.Helper()
		var got bool
		if err := database.Pool.QueryRow(ctx, `select `+SessionClause("$2", "$1"), viewerArg(t, viewer), session).Scan(&got); err != nil {
			t.Fatalf("SessionClause %s: %v", session, err)
		}
		return got
	}
	others := []string{"ses_refilled", "ses_listed"}
	restricted := func(state string) map[string]model.Viewer {
		t.Helper()
		return map[string]model.Viewer{
			"bob":                         resolve(state+": bob", cookie(bob)),
			"bob's token that lists PRIV": resolve(state+": bob's token that lists PRIV", personalToken(bob, "PRIV")),
		}
	}

	bobAgent := resolve("bob's session", bobSession)
	for name, viewer := range restricted("unfilled") {
		if !viewer.AnyPrivate {
			t.Fatalf("%s resolves as %+v, want a deployment that holds a private project", name, viewer)
		}
		for _, session := range others {
			if passes(viewer, session) {
				t.Errorf("unfilled, with PRIV private: SessionClause %s for %s = true, want false", session, name)
			}
		}
	}
	if !passes(bobAgent, "ses_bob") {
		t.Error("unfilled, with PRIV private: SessionClause ses_bob, his own, = false, want true")
	}

	if _, err := database.Pool.Exec(ctx, `insert into session_projects_filled default values`); err != nil {
		t.Fatalf("write the fill's marker: %v", err)
	}
	for name, viewer := range restricted("filled") {
		for _, session := range others {
			if !passes(viewer, session) {
				t.Errorf("filled: SessionClause %s for %s = false, want true", session, name)
			}
		}
	}

	if _, err := database.Pool.Exec(ctx, `
		delete from session_projects_filled;
		update projects set visibility = 'public';
	`); err != nil {
		t.Fatalf("empty the marker and make every project public: %v", err)
	}
	for name, viewer := range restricted("with no private project") {
		if viewer.AnyPrivate {
			t.Fatalf("%s resolves as %+v with every project public, want no private project", name, viewer)
		}
		if !passes(viewer, "ses_refilled") {
			t.Errorf("unfilled, with no private project: SessionClause ses_refilled for %s = false, want true by its PRIV pair (R9)", name)
		}
		if passes(viewer, "ses_listed") {
			t.Errorf("unfilled, with no private project: SessionClause ses_listed for %s = true, want false: with no private project a viewer that is not open reads a session by its pairs alone (R9)", name)
		}
	}
}

// TestTheFillMarkerIsReadOncePerStatement: SessionClause's read of session_projects_filled names no
// column of the row it tests, so the statement reads it once (an InitPlan), not once per row. A
// read over fifty sessions, for an open viewer (alice) and for one that is not open (bob) of a
// deployment that holds a private project, both with the marker and without it, scans
// session_projects_filled in one loop.
func TestTheFillMarkerIsReadOncePerStatement(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	for _, person := range []string{alice, bob} {
		viewer, err := Resolve(ctx, database.Pool, cookie(person))
		if err != nil {
			t.Fatalf("%s: Resolve: %v", person, err)
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
				viewerArg(t, viewer)); total != 1 {
				t.Errorf("%s, %s: the read over fifty sessions scans session_projects_filled in %d nodes, %d loops in all; want one loop", person, state.name, scans, total)
			}
		}
	}
}

// planLoops explains sql, run with args, and answers how many of its plan's nodes scan relation and
// how many loops they ran in all, reading the plan through explaintest. A plan that scans relation
// nowhere fails the test, since a count over it would measure nothing.
func planLoops(t *testing.T, q Querier, relation, sql string, args ...any) (scans, total int) {
	t.Helper()
	var raw []byte
	if err := q.QueryRow(context.Background(), `explain (analyze, costs off, timing off, format json) `+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain the read: %v", err)
	}
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

// TestTheListenerRecordUsesItsPrimaryKey: SessionClause asks session_clause_visible about every row
// its own SQL does not answer, and Postgres plans and runs that function's statement once a call,
// never in the plan of the statement that binds the clause, so what a call reads is read as it
// runs (functionCalls, auto_explain's plans of nested statements). For a read over fifty
// sessions none of which has a pair, by an open viewer and by one that is not open, of a
// deployment that holds a private project and has filled session_projects, the clause calls it
// once a session, and each call reads route_roster_session_projects through its primary key,
// keyed on id, in no node more than once. With no private project an open viewer's read calls it
// not at all (R9). The function sets nothing for its calls (no SET clause, so no call saves and
// restores a search_path): it names each table by its schema.
//
// Controls: drop route_roster_session_projects_pkey on the connection the read runs on, and each
// call reads the projection by a sequential scan, which the test names as a search of no
// route_roster_session_projects_pkey; take the open viewer's arm out of SessionClause's own SQL,
// and the read with no private project makes fifty calls, which the test names.
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
	var settings []string
	if err := database.Pool.QueryRow(ctx, `
		select coalesce(proconfig, '{}') from pg_proc where proname = 'session_clause_visible'`).Scan(&settings); err != nil {
		t.Fatalf("read session_clause_visible's settings: %v", err)
	}
	if len(settings) > 0 {
		t.Errorf("session_clause_visible sets %v on each of its calls, want none: it names its tables by their schema", settings)
	}
	const projection = "route_roster_session_projects"
	read := `select count(*) from generate_series(1, 50) as n where ` + SessionClause("'ses_' || n", "$1")
	keys := explaintest.NewIndexKeys(database.Pool)
	resolve := func(person string) model.Viewer {
		t.Helper()
		viewer, err := Resolve(ctx, database.Pool, cookie(person))
		if err != nil {
			t.Fatalf("%s: Resolve: %v", person, err)
		}
		return viewer
	}
	for _, person := range []string{alice, bob} {
		viewer := resolve(person)
		if !viewer.AnyPrivate {
			t.Fatalf("%s resolves as %+v, want a deployment that holds a private project", person, viewer)
		}
		calls := functionCalls(t, database, "route_roster_session_projects", read, viewerArg(t, viewer))
		if len(calls) != 50 {
			t.Errorf("%s's read over fifty sessions made %d calls of session_clause_visible, want one a session", person, len(calls))
		}
		for index, call := range calls {
			subject := fmt.Sprintf("call %d of session_clause_visible for %s", index+1, person)
			keys.RequireIndexKeyed(t, call, projection+"_pkey", subject, explaintest.CondRequired)
			for _, scan := range explaintest.Scans(t, call) {
				if scan.Relation == projection && scan.Loops > 1 {
					t.Errorf("%s reads %s %v times in one node, want once:\n%s", subject, projection, scan.Loops, call)
				}
			}
		}
	}
	if _, err := database.Pool.Exec(ctx, `update projects set visibility = 'public'`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
	if viewer := resolve(alice); viewer.AnyPrivate || !viewer.Open {
		t.Fatalf("alice resolves as %+v with every project public, want an open viewer of no private project", viewer)
	} else if calls := functionCalls(t, database, "route_roster_session_projects", read, viewerArg(t, viewer)); len(calls) != 0 {
		t.Errorf("with no private project, alice's read over fifty sessions made %d calls of session_clause_visible, want none (R9)", len(calls))
	}
}

// functionCalls runs sql with args on a connection of its own, with sequential scans off, on which
// auto_explain sends the client, as a notice, the analyzed plan of every statement the connection
// runs, those a function runs included, and answers the plan of each statement whose query text
// names reads: each call of session_clause_visible for route_roster_session_projects, which only a
// clause's function reads. A connection that sent no plan of sql itself fails the test, since its
// silence about the calls would then say nothing. Loading auto_explain and setting it are a
// superuser's: a login that may not (SQLSTATE 42501, insufficient_privilege), such as the
// NOSUPERUSER role of a Legion pod's test server (legion-testpg), skips the test naming the login
// and the statement, and fails it on GitHub Actions, whose login is a superuser
// (testdb.SkipOutsideCI).
func functionCalls(t *testing.T, database *store.Store, reads, sql string, args ...any) []json.RawMessage {
	t.Helper()
	ctx := context.Background()
	config := database.Pool.Config().ConnConfig.Copy()
	var logged []string
	config.OnNotice = func(_ *pgconn.PgConn, notice *pgconn.Notice) { logged = append(logged, notice.Message) }
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a connection for the plans: %v", err)
	}
	defer connection.Close(ctx)
	for _, setting := range []string{
		`load 'auto_explain'`,
		`set auto_explain.log_min_duration = 0`,
		`set auto_explain.log_nested_statements = on`,
		`set auto_explain.log_analyze = on`,
		`set auto_explain.log_timing = off`,
		`set auto_explain.log_format = json`,
		`set auto_explain.log_level = notice`,
		`set client_min_messages = notice`,
		`set enable_seqscan = off`,
	} {
		if _, err := connection.Exec(ctx, setting); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				testdb.SkipOutsideCI(t, "%s: the test server's login %q may not (SQLSTATE 42501); the calls this test counts are the plans auto_explain reports, which only a superuser's session can turn on: %v",
					setting, config.User, err)
			}
			t.Fatalf("%s: %v", setting, err)
		}
	}
	if _, err := connection.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("run the read: %v", err)
	}
	var calls []json.RawMessage
	ranItself := false
	for _, message := range logged {
		start := strings.Index(message, "{")
		if start < 0 {
			continue
		}
		var entry struct {
			Query string          `json:"Query Text"`
			Plan  json.RawMessage `json:"Plan"`
		}
		if err := json.Unmarshal([]byte(message[start:]), &entry); err != nil {
			t.Fatalf("read the plan auto_explain sent: %v\n%s", err, message)
		}
		switch {
		case entry.Query == sql:
			ranItself = true
		case strings.Contains(entry.Query, reads):
			calls = append(calls, entry.Plan)
		}
	}
	if !ranItself {
		t.Fatalf("auto_explain sent no plan of the read itself, so the calls it names mean nothing: %q", logged)
	}
	return calls
}
