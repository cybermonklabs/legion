package access

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAPairRecordedTwiceReadsAsOnce: session_projects has no unique key, so two first writes of
// one session in one project, open at once, store the pair twice
// (events.TestTwoFirstRecordsOfAPairWithCrossedIssueLocksBothCommit). Its one reader is
// SessionClause, an exists, which RoleClause and PartyClause read through. Each answers every
// viewer the same with the pair stored twice as with it stored once.
func TestAPairRecordedTwiceReadsAsOnce(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	// The pairs of a filled deployment: while a project is private, SessionClause reads the pairs,
	// for an open viewer and one that is not open alike, only once session_projects_filled holds
	// its row (TestARestrictedViewerReadsNoOtherSessionWhileTheFillIsUnfinished).
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects_filled default values;
		insert into issues (key, project_key, number, title, created_by, rank) values
			('PRIV-1', 'PRIV', 1, 'One', '{"kind":"user","id":"olga@example.com"}', 'U'),
			('PRIV-2', 'PRIV', 2, 'Two', '{"kind":"user","id":"olga@example.com"}', 'V');
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event) values
			('role', 'priv-architect', 'held_by', 'session', 'ses_twice', now(), '{}', 0);
	`); err != nil {
		t.Fatalf("seed issues and a role: %v", err)
	}

	// The crossed writes: the first holds PRIV-1 and its record; the second writes PRIV-2 and
	// commits; the first then takes PRIV-2 and commits. The session is one its credential proved,
	// the only kind the live insert records.
	broker := events.NewBroker()
	session := model.Actor{Kind: "session", ID: "ses_twice", Act: &model.ActorAct{Session: "ses_twice"}}
	issueEvent := func(key string) model.Event {
		return model.Event{IssueKey: new(key), Type: "issue.updated", Actor: session, Payload: map[string]any{}}
	}
	first, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the first write: %v", err)
	}
	defer first.Rollback(ctx)
	if _, err := broker.Append(ctx, first, issueEvent("PRIV-1")); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	second, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the second write: %v", err)
	}
	if _, err := broker.Append(context.Background(), second, issueEvent("PRIV-2")); err != nil {
		t.Fatalf("the second write: %v", err)
	}
	if err := second.Commit(context.Background()); err != nil {
		t.Fatalf("commit the second write: %v", err)
	}
	if _, err := broker.Append(ctx, first, issueEvent("PRIV-2")); err != nil {
		t.Fatalf("the first write on PRIV-2: %v", err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first write: %v", err)
	}

	readers := []struct{ name, sql, arg string }{
		{"SessionClause", SessionClause("$2", "$1"), "ses_twice"},
		{"RoleClause", RoleClause("$2", "$1"), "priv-architect"},
		{"PartyClause", PartyClause("array['session:' || $2]", "$1"), "ses_twice"},
	}
	viewers := map[string]model.Actor{
		"bob, who sees PRIV":                 cookie(bob),
		"carol's token naming PRIV":          personalToken(carol, "PRIV"),
		"alice's token naming PRIV, sees no": personalToken(alice, "PRIV"),
	}
	answers := func(t *testing.T) map[string]bool {
		t.Helper()
		got := map[string]bool{}
		for name, actor := range viewers {
			viewer, err := Resolve(ctx, database.Pool, actor)
			if err != nil {
				t.Fatalf("%s: Resolve: %v", name, err)
			}
			for _, reader := range readers {
				var passes bool
				if err := database.Pool.QueryRow(ctx, `select coalesce(`+reader.sql+`, false)`, viewerArg(t, viewer), reader.arg).Scan(&passes); err != nil {
					t.Fatalf("%s for %s: %v", reader.name, name, err)
				}
				got[reader.name+" for "+name] = passes
			}
		}
		return got
	}
	rows := func() int {
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from session_projects where session_id = 'ses_twice' and project_key = 'PRIV'`).Scan(&count); err != nil {
			t.Fatalf("count the pair's rows: %v", err)
		}
		return count
	}

	if n := rows(); n != 2 {
		t.Fatalf("(ses_twice, PRIV) rows after the crossed writes = %d, want 2", n)
	}
	twice := answers(t)
	if _, err := database.Pool.Exec(ctx, `
		delete from session_projects where ctid in (
			select ctid from session_projects where session_id = 'ses_twice' and project_key = 'PRIV' limit 1)
	`); err != nil {
		t.Fatalf("keep one row of the pair: %v", err)
	}
	if n := rows(); n != 1 {
		t.Fatalf("(ses_twice, PRIV) rows = %d, want 1", n)
	}
	once := answers(t)
	for key, want := range once {
		if twice[key] != want {
			t.Errorf("%s = %t with the pair stored twice, %t with it stored once", key, twice[key], want)
		}
	}
	// The answers are the rule's, not two equal blanks: a viewer that sees PRIV passes the
	// session, and one that sees nothing does not.
	if !once["SessionClause for bob, who sees PRIV"] || once["SessionClause for alice's token naming PRIV, sees no"] {
		t.Errorf("SessionClause answers %v, want bob passed and alice's PRIV token refused", once)
	}
}
