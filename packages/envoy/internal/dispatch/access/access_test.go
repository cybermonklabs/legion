package access

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// The people and projects of verification v15's seed, plus the group case R2 adds. PUB is public
// and PRIV private; OTHER is private and lists only the group crew, whose members are dave, a
// full member, and bob, the restricted member PRIV names directly.
const (
	alice  = "alice@example.com"  // a full member no access list names
	bob    = "bob@other.example"  // a restricted member PRIV names
	carol  = "carol@example.com"  // a full member PRIV names
	dave   = "dave@example.com"   // a full member of the group OTHER lists
	olga   = "olga@example.com"   // the owner of every seeded project
	newbie = "newbie@example.com" // a person with no people row, signed in only through the broker
)

// seedAccess writes the seed: the people with their member kinds, the three projects, the two
// access lists and the group.
func seedAccess(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into people (email, member_kind) values
			('alice@example.com', 'full'), ('bob@other.example', 'restricted'), ('carol@example.com', 'full'),
			('dave@example.com', 'full'), ('olga@example.com', 'full');
		insert into projects (key, name, default_assignee, visibility) values
			('PUB', 'Public', 'olga@example.com', 'public'),
			('PRIV', 'Private', 'olga@example.com', 'private'),
			('OTHER', 'Other', 'olga@example.com', 'private');
		insert into access_groups (name, owner) values ('crew', 'olga@example.com');
		insert into access_group_members (group_name, person) values ('crew', 'bob@other.example'), ('crew', 'dave@example.com');
		insert into project_access (project_key, principal_kind, principal, added_by) values
			('PRIV', 'person', 'bob@other.example', '{"kind":"user","id":"olga@example.com"}'),
			('PRIV', 'person', 'carol@example.com', '{"kind":"user","id":"olga@example.com"}'),
			('OTHER', 'group', 'crew', '{"kind":"user","id":"olga@example.com"}');
	`); err != nil {
		t.Fatalf("seed access: %v", err)
	}
	return database
}

// seededCaller is one caller of the seed and what the rule must answer for it.
type seededCaller struct {
	name  string
	actor model.Actor
	want  model.Viewer
}

func cookie(login string) model.Actor {
	return model.Actor{Kind: "user", ID: login, Credential: &model.Credential{Scopes: model.Scopes}}
}

func personalToken(login string, projects ...string) model.Actor {
	return model.Actor{Kind: "session", Owner: new(login), Credential: &model.Credential{Scopes: model.Scopes, TokenID: "token-of-" + login, Projects: projects}}
}

func seededCallers() []seededCaller {
	return []seededCaller{
		{"alice", cookie(alice), model.Viewer{Person: alice, Open: true, Sees: []string{"PUB"}, AnyPrivate: true}},
		{"bob", cookie(bob), model.Viewer{Person: bob, Restricted: true, Sees: []string{"PRIV"}, AnyPrivate: true}},
		{"carol", cookie(carol), model.Viewer{Person: carol, Open: true, Sees: []string{"PRIV", "PUB"}, AnyPrivate: true}},
		{"dave", cookie(dave), model.Viewer{Person: dave, Open: true, Sees: []string{"OTHER", "PUB"}, AnyPrivate: true}},
		{"carol's token naming PRIV", personalToken(carol, "PRIV"), model.Viewer{Person: carol, Listed: true, Sees: []string{"PRIV"}, AnyPrivate: true}},
		{"alice's token naming PRIV", personalToken(alice, "PRIV"), model.Viewer{Person: alice, Listed: true, Sees: []string{}, AnyPrivate: true}},
		{"carol's token naming no project", personalToken(carol), model.Viewer{Person: carol, Open: true, Sees: []string{"PUB"}, AnyPrivate: true}},
		{"the shared agent token", model.Actor{Credential: &model.Credential{}}, model.Viewer{Open: true, Sees: []string{"PUB"}, AnyPrivate: true}},
		{"a service-account token naming PRIV", model.Actor{Service: new("system:serviceaccount:legion:worker"), Credential: &model.Credential{Projects: []string{"PRIV"}}},
			model.Viewer{Listed: true, Sees: []string{}, AnyPrivate: true}},
		{"a broker token of a person with no people row", model.Actor{Kind: "session", ID: "ses_newbie", Owner: new(newbie), Credential: &model.Credential{Scopes: model.Scopes, Session: "ses_newbie"}},
			model.Viewer{Person: newbie, Session: "ses_newbie", Open: true, Sees: []string{"PUB"}, AnyPrivate: true}},
	}
}

// TestTheRuleOverTheSeededCallers holds Resolve to the rule for every caller of verification v15:
// a person sees a project its access list names or that is public to a full member; a credential
// sees its list within its person's, or the public part of its person's with no list; a
// credential with no person sees public projects.
func TestTheRuleOverTheSeededCallers(t *testing.T) {
	database := seedAccess(t)
	for _, caller := range seededCallers() {
		got, err := Resolve(context.Background(), database.Pool, caller.actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", caller.name, err)
		}
		if !viewersEqual(got, caller.want) {
			t.Errorf("%s: Resolve = %+v, want %+v", caller.name, got, caller.want)
		}
	}
}

// TestCredentialSeesAgreesWithTheSQLRule: Credential.Sees, Dispatch's rule over a resolved
// person, answers every seeded caller and project exactly as the SQL rule does.
func TestCredentialSeesAgreesWithTheSQLRule(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	private := map[string]bool{"PUB": false, "PRIV": true, "OTHER": true}
	for _, caller := range seededCallers() {
		viewer, err := Resolve(ctx, database.Pool, caller.actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", caller.name, err)
		}
		credential := Credential{Browser: caller.actor.Kind == "user"}
		if caller.actor.Credential != nil {
			credential.Listed = caller.actor.Credential.Projects
		}
		if viewer.Person != "" {
			credential.Person = true
			person, err := Resolve(ctx, database.Pool, cookie(viewer.Person))
			if err != nil {
				t.Fatalf("%s: Resolve its person: %v", caller.name, err)
			}
			// The broker's rule for a person with no people row is the full member's, which the
			// person's cookie alone does not answer: read it from the broker credential itself.
			if caller.actor.Credential != nil && caller.actor.Credential.Session != "" {
				person = viewer
			}
			credential.PersonSees = person.Sees
		}
		for project, isPrivate := range private {
			got, sql := credential.Sees(project, isPrivate), slices.Contains(viewer.Sees, project)
			if got != sql {
				t.Errorf("%s: Credential.Sees(%s) = %t, the SQL rule answers %t", caller.name, project, got, sql)
			}
			// Two forms that agree are both right only when they answer the rule: held to the
			// seed's stated answer, an empty SQL rule and an always-false Credential do not pass
			// by agreeing.
			if want := slices.Contains(caller.want.Sees, project); got != want {
				t.Errorf("%s: Credential.Sees(%s) = %t, the rule says %t", caller.name, project, got, want)
			}
		}
	}
}

// TestBrokerTokenAdmitsIsTheListRule: the bus's rule for a broker token is its list alone, and,
// with no list, every project that is not private.
func TestBrokerTokenAdmitsIsTheListRule(t *testing.T) {
	for _, test := range []struct {
		listed  []string
		project string
		private bool
		want    bool
	}{
		{[]string{"PRIV"}, "PRIV", true, true},
		{[]string{"PRIV"}, "PUB", false, false},
		{[]string{"PRIV", "PUB"}, "PUB", false, true},
		{nil, "PUB", false, true},
		{nil, "PRIV", true, false},
		{[]string{}, "PRIV", true, false},
	} {
		if got := BrokerTokenAdmits(test.listed, test.project, test.private); got != test.want {
			t.Errorf("BrokerTokenAdmits(%v, %s, private=%t) = %t, want %t", test.listed, test.project, test.private, got, test.want)
		}
	}
}

// TestARestrictedMemberIsNeverAdmittedByAGroup (v24 R2): bob, restricted, is a member of the group
// OTHER lists and sees nothing of OTHER, while dave, a full member of the same group, sees it.
func TestARestrictedMemberIsNeverAdmittedByAGroup(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	for _, test := range []struct {
		person string
		sees   bool
	}{{bob, false}, {dave, true}} {
		viewer, err := Resolve(ctx, database.Pool, cookie(test.person))
		if err != nil {
			t.Fatalf("Resolve(%s): %v", test.person, err)
		}
		if got := slices.Contains(viewer.Sees, "OTHER"); got != test.sees {
			t.Errorf("%s sees OTHER = %t, want %t: OTHER lists only the group crew, and %s is a member of it", test.person, got, test.sees, test.person)
		}
		var listed bool
		if err := database.Pool.QueryRow(ctx, `select `+PersonSeesProjectSQL("$1", "(select member_kind from people where email = $1)", "p")+` from projects p where p.key = 'OTHER'`,
			test.person).Scan(&listed); err != nil {
			t.Fatalf("PersonSeesProjectSQL(%s): %v", test.person, err)
		}
		if listed != test.sees {
			t.Errorf("PersonSeesProjectSQL(%s, OTHER) = %t, want %t", test.person, listed, test.sees)
		}
	}
}

// TestEverySQLFragmentStartsWithTheMarker: each fragment names itself after access.Marker, so a
// statement that binds one says so in pg_stat_statements and in every census that reads SQL.
func TestEverySQLFragmentStartsWithTheMarker(t *testing.T) {
	for name, fragment := range map[string]string{
		"PersonSeesProjectSQL": PersonSeesProjectSQL("$1", "$2", "p"),
		"VisibleProjectsSQL":   VisibleProjectsSQL(),
		"ProjectClause":        ProjectClause("p.key", "$1"),
		"PersonClause":         PersonClause("u.email", "$1"),
		"SessionClause":        SessionClause("s.id", "$1"),
		"RoleClause":           RoleClause("r.id", "$1"),
		"PartyClause":          PartyClause("n.parties", "$1"),
	} {
		if !strings.HasPrefix(fragment, Marker+name+" */") {
			t.Errorf("%s = %q, want it to start with %q", name, fragment[:min(len(fragment), 60)], Marker+name+" */")
		}
	}
}

// viewerArg is the jsonb a statement binds a viewer as, in the shape "Contracts / K2, the
// visibility argument" names (the author is no clause's concern).
func viewerArg(t *testing.T, viewer model.Viewer) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"person": nullable(viewer.Person), "session": nullable(viewer.Session), "open": viewer.Open,
		"all": viewer.All, "sees": viewer.Sees, "private": viewer.AnyPrivate,
	})
	if err != nil {
		t.Fatalf("encode viewer: %v", err)
	}
	return data
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// TestTheClausesOverTheSeededCallers: the clauses a read binds answer each seeded viewer as the
// rule says. A project row passes ProjectClause when the viewer sees it. A person, a session, a
// role and a direct conversation pass for an open viewer; for a restricted viewer or one that
// lists projects, a person passes when it sees a project the viewer sees or is the viewer, a
// session when it acted in such a project or is the viewer's own, a role when its holder session
// passes, and a conversation when the viewer is one of its parties.
func TestTheClausesOverTheSeededCallers(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects (session_id, project_key) values ('ses_priv', 'PRIV'), ('ses_pub', 'PUB');
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event) values
			('role', 'priv-architect', 'held_by', 'session', 'ses_priv', now(), '{}', 0),
			('role', 'pub-architect', 'held_by', 'session', 'ses_priv', now() - interval '1 hour', '{}', 0);
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted) values
			('role', 'pub-architect', 'held_by', 'session', 'ses_priv', now(), '{}', 0, false);
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event) values
			('role', 'pub-architect', 'held_by', 'session', 'ses_pub', now(), '{}', 0);
	`); err != nil {
		t.Fatalf("seed sessions and roles: %v", err)
	}
	callers := map[string]model.Actor{
		"alice": cookie(alice), "bob": cookie(bob), "carol's token naming PRIV": personalToken(carol, "PRIV"),
	}
	type probe struct {
		name, sql string
		arg       any
		want      map[string]bool
	}
	probes := []probe{
		{"ProjectClause PRIV", ProjectClause("$2", "$1"), "PRIV", map[string]bool{"alice": false, "bob": true, "carol's token naming PRIV": true}},
		{"ProjectClause PUB", ProjectClause("$2", "$1"), "PUB", map[string]bool{"alice": true, "bob": false, "carol's token naming PRIV": false}},
		{"PersonClause carol", PersonClause("$2", "$1"), carol, map[string]bool{"alice": true, "bob": true, "carol's token naming PRIV": true}},
		{"PersonClause alice", PersonClause("$2", "$1"), alice, map[string]bool{"alice": true, "bob": false, "carol's token naming PRIV": false}},
		{"PersonClause bob himself", PersonClause("$2", "$1"), bob, map[string]bool{"alice": true, "bob": true, "carol's token naming PRIV": true}},
		{"SessionClause ses_priv", SessionClause("$2", "$1"), "ses_priv", map[string]bool{"alice": true, "bob": true, "carol's token naming PRIV": true}},
		{"SessionClause ses_pub", SessionClause("$2", "$1"), "ses_pub", map[string]bool{"alice": true, "bob": false, "carol's token naming PRIV": false}},
		{"RoleClause priv-architect", RoleClause("$2", "$1"), "priv-architect", map[string]bool{"alice": true, "bob": true, "carol's token naming PRIV": true}},
		{"RoleClause pub-architect", RoleClause("$2", "$1"), "pub-architect", map[string]bool{"alice": true, "bob": false, "carol's token naming PRIV": false}},
		{"PartyClause bob and ses_pub", PartyClause("$2", "$1"), []string{"person:" + bob, "session:ses_pub"}, map[string]bool{"alice": true, "bob": true, "carol's token naming PRIV": false}},
		{"PartyClause carol and ses_priv", PartyClause("$2", "$1"), []string{"person:" + carol, "session:ses_priv"}, map[string]bool{"alice": true, "bob": false, "carol's token naming PRIV": true}},
	}
	for name, actor := range callers {
		viewer, err := Resolve(ctx, database.Pool, actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		for _, p := range probes {
			var got bool
			if err := database.Pool.QueryRow(ctx, `select coalesce(`+p.sql+`, false)`, viewerArg(t, viewer), p.arg).Scan(&got); err != nil {
				t.Fatalf("%s for %s: %v", p.name, name, err)
			}
			if got != p.want[name] {
				t.Errorf("%s for %s = %t, want %t", p.name, name, got, p.want[name])
			}
		}
	}
	// The server's own viewer passes every clause.
	for _, p := range probes {
		var got bool
		if err := database.Pool.QueryRow(ctx, `select coalesce(`+p.sql+`, false)`, viewerArg(t, *model.ServerViewer), p.arg).Scan(&got); err != nil {
			t.Fatalf("%s for the server: %v", p.name, err)
		}
		if !got {
			t.Errorf("%s for the server's own viewer = false, want true", p.name)
		}
	}
}

func viewersEqual(a, b model.Viewer) bool {
	return a.Person == b.Person && a.Session == b.Session && a.Restricted == b.Restricted && a.Listed == b.Listed &&
		a.Open == b.Open && a.All == b.All && a.AnyPrivate == b.AnyPrivate && slices.Equal(a.Sees, b.Sees)
}
