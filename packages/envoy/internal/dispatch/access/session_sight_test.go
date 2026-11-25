package access

import (
	"context"
	"maps"
	"slices"
	"testing"
)

// TestASessionSeesWhatItsPersonAndItsCredentialSee holds SessionSeesProjectClause, through
// SessionsSeeProjects, to the rule over the seed's people (PUB public; PRIV private, naming bob
// and carol; OTHER private, naming the group crew of bob and dave): every person a session is tied
// to sees the project, and a listener record that lists the session's projects covers it - a
// restricted member's list for what it lists, every other list for each public project and the
// private ones it lists. A session tied to nobody with no list sees the public projects. Each row
// is its own control: it sees one project and not another.
//
// A second pass moves the listener record past its expires_at: a session whose list it held is
// then read as one whose listener sent none, so the session tied to nobody that listed PRIV sees
// PUB alone, and carol's session whose list named PUB alone sees what carol sees.
func TestASessionSeesWhatItsPersonAndItsCredentialSee(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into session_owners (session_id, login) values
			('ses_carol', 'carol@example.com'), ('ses_alice', 'alice@example.com'), ('ses_bob', 'bob@other.example'),
			('ses_dave', 'dave@example.com'), ('ses_newbie', 'newbie@example.com'),
			('ses_carol_and_alice', 'carol@example.com'), ('ses_carol_and_alice', 'alice@example.com'),
			('ses_carol_listed', 'carol@example.com'), ('ses_bob_listed', 'bob@other.example'),
			('ses_alice_listed', 'alice@example.com');
		insert into route_roster (answered, sessions, sent_at, expires_at) values (true,
			'[{"session_id":"ses_nobody_listed","projects":["PRIV"]},{"session_id":"ses_carol_listed","projects":["PUB"]},{"session_id":"ses_bob_listed","projects":["PRIV"]},{"session_id":"ses_alice_listed","projects":["PRIV"]},{"session_id":"ses_nobody_empty","projects":[]}]',
			now(), now() + interval '1 hour');
	`); err != nil {
		t.Fatalf("seed the sessions: %v", err)
	}
	sessions := []string{"ses_carol", "ses_alice", "ses_bob", "ses_dave", "ses_newbie", "ses_carol_and_alice",
		"ses_nobody", "ses_nobody_listed", "ses_nobody_empty", "ses_carol_listed", "ses_bob_listed", "ses_alice_listed"}
	projects := []string{"PUB", "PRIV", "OTHER", "NONE"}
	check := func(t *testing.T, want map[string][]string) {
		t.Helper()
		sight, err := SessionsSeeProjects(ctx, database.Pool, sessions, projects)
		if err != nil {
			t.Fatalf("SessionsSeeProjects: %v", err)
		}
		for _, session := range sessions {
			var got []string
			for _, project := range projects {
				if sight[project][session] {
					got = append(got, project)
				}
			}
			if !slices.Equal(got, want[session]) {
				t.Errorf("%s sees %v, want %v", session, got, want[session])
			}
		}
		if unknown := slices.Collect(maps.Keys(sight["NONE"])); len(unknown) != 0 {
			t.Errorf("a project no row holds is seen by %v, want nobody", unknown)
		}
	}
	check(t, map[string][]string{
		"ses_carol":           {"PUB", "PRIV"},
		"ses_alice":           {"PUB"},
		"ses_bob":             {"PRIV"},
		"ses_dave":            {"PUB", "OTHER"},
		"ses_newbie":          {"PUB"},
		"ses_carol_and_alice": {"PUB"},
		"ses_nobody":          {"PUB"},
		"ses_nobody_listed":   {"PUB", "PRIV"},
		"ses_nobody_empty":    {"PUB"},
		"ses_carol_listed":    {"PUB"},
		"ses_bob_listed":      {"PRIV"},
		"ses_alice_listed":    {"PUB"},
	})
	if one, err := SessionSeesProject(ctx, database.Pool, "ses_bob", "PUB"); err != nil || one {
		t.Errorf("SessionSeesProject(ses_bob, PUB) = %v, %v; want false: a restricted member's session sees no public project its person's lists do not name", one, err)
	}
	if one, err := SessionSeesProject(ctx, database.Pool, "ses_bob", "PRIV"); err != nil || !one {
		t.Errorf("SessionSeesProject(ses_bob, PRIV) = %v, %v; want true", one, err)
	}

	if _, err := database.Pool.Exec(ctx, `update route_roster set expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatalf("expire the listener record: %v", err)
	}
	t.Run("an expired listener record lists nothing", func(t *testing.T) {
		check(t, map[string][]string{
			"ses_carol":           {"PUB", "PRIV"},
			"ses_alice":           {"PUB"},
			"ses_bob":             {"PRIV"},
			"ses_dave":            {"PUB", "OTHER"},
			"ses_newbie":          {"PUB"},
			"ses_carol_and_alice": {"PUB"},
			"ses_nobody":          {"PUB"},
			"ses_nobody_listed":   {"PUB"},
			"ses_nobody_empty":    {"PUB"},
			"ses_carol_listed":    {"PUB", "PRIV"},
			"ses_bob_listed":      {"PRIV"},
			"ses_alice_listed":    {"PUB"},
		})
	})
}

// TestASessionOfNoPrivateDeploymentSeesEveryProjectButAnOutsidersSessionDoesNot is R9 for the
// session's sight: with every project public, every session of a full member and every session
// tied to nobody sees every project, as every destination reached them before; a restricted
// member's session still sees only the projects its person's lists name.
func TestASessionOfNoPrivateDeploymentSeesEveryProjectButAnOutsidersSessionDoesNot(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		update projects set visibility = 'public';
		insert into session_owners (session_id, login) values ('ses_alice', 'alice@example.com'), ('ses_bob', 'bob@other.example');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sight, err := SessionsSeeProjects(ctx, database.Pool, []string{"ses_alice", "ses_nobody", "ses_bob"}, []string{"PUB", "PRIV", "OTHER"})
	if err != nil {
		t.Fatalf("SessionsSeeProjects: %v", err)
	}
	for _, project := range []string{"PUB", "PRIV", "OTHER"} {
		for _, session := range []string{"ses_alice", "ses_nobody"} {
			if !sight[project][session] {
				t.Errorf("%s does not see %s with every project public, want it to (R9)", session, project)
			}
		}
	}
	if got := []bool{sight["PUB"]["ses_bob"], sight["PRIV"]["ses_bob"], sight["OTHER"]["ses_bob"]}; !slices.Equal(got, []bool{false, true, false}) {
		t.Errorf("bob's session sees PUB, PRIV, OTHER = %v, want only PRIV, the one project bob's lists name", got)
	}
}

// TestTheSessionSightClauseAnswersTrueOrFalseNeverNull: a missing session, a missing project key and
// a project of no visibility each answer a boolean, as the clause and as its negation, so a rule
// written as "no destination the session may not see" fails closed
// (TestEveryClauseAnswersTrueOrFalseNeverNull holds the viewer clauses so).
func TestTheSessionSightClauseAnswersTrueOrFalseNeverNull(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into session_owners (session_id, login) values ('ses_carol', 'carol@example.com');
		insert into route_roster (answered, sessions, sent_at, expires_at) values (true,
			'[{"session_id":"ses_listed","projects":["PRIV"]}]', now(), now() + interval '1 hour');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for name, rows := range map[string]string{
		"a missing session over a real project":    `(select null::text as id) s, (select key, visibility, owner from projects where key = 'PRIV') p`,
		"a tied session over a missing project":    `(select 'ses_carol'::text as id) s, (select null::text as key, null::text as visibility, null::text as owner) p`,
		"a listed session over a missing project":  `(select 'ses_listed'::text as id) s, (select null::text as key, null::text as visibility, null::text as owner) p`,
		"an untied session over a missing project": `(select 'ses_nobody'::text as id) s, (select null::text as key, null::text as visibility, null::text as owner) p`,
	} {
		clause := SessionSeesProjectClause("s.id", "p")
		for _, form := range []string{clause, "not " + clause} {
			var answer *bool
			if err := database.Pool.QueryRow(ctx, `select `+form+` from `+rows).Scan(&answer); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if answer == nil {
				t.Errorf("%s answers NULL as `%.24s…`", name, form)
			}
		}
	}
}
