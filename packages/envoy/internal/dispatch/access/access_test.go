package access

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
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
		{"carol's token naming PRIV", personalToken(carol, "PRIV"), model.Viewer{Person: carol, Sees: []string{"PRIV"}, AnyPrivate: true, Lists: []string{"PRIV"}}},
		{"alice's token naming PRIV", personalToken(alice, "PRIV"), model.Viewer{Person: alice, Sees: []string{}, AnyPrivate: true, Lists: []string{"PRIV"}}},
		{"carol's token naming no project", personalToken(carol), model.Viewer{Person: carol, Open: true, Sees: []string{"PUB"}, AnyPrivate: true}},
		{"the shared agent token", model.Actor{Credential: &model.Credential{}}, model.Viewer{Open: true, Sees: []string{"PUB"}, AnyPrivate: true}},
		{"a service-account token naming PRIV", model.Actor{Service: new("system:serviceaccount:legion:worker"), Credential: &model.Credential{Projects: []string{"PRIV"}}},
			model.Viewer{Sees: []string{}, AnyPrivate: true, Lists: []string{"PRIV"}}},
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

// countingQuerier is a Querier that counts the statements it is asked to run.
type countingQuerier struct {
	Querier
	reads int
}

func (q *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q.reads++
	return q.Querier.QueryRow(ctx, sql, args...)
}

func (q *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.reads++
	return q.Querier.Query(ctx, sql, args...)
}

// TestResolveRefusesARecordedActor: the list and the broker flag Resolve reads live on the
// request's credential, so an actor that carries none is refused with ErrUnresolvedActor before
// any read, rather than answered as a credential that lists nothing. That is every seeded caller as
// a write records it (Actor.Recorded) and the person actingPerson answers for a token, which
// carries the token's act and no credential. The same callers as optionalActor resolves them, each
// with its credential, answer their seeded viewers.
func TestResolveRefusesARecordedActor(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	refused := []seededCaller{{name: "carol's token as actingPerson answers its person", actor: model.Actor{
		Kind: "user", ID: carol, Act: &model.ActorAct{TokenID: "token-of-" + carol, Scopes: model.Scopes},
	}}}
	for _, caller := range seededCallers() {
		refused = append(refused, seededCaller{name: caller.name + ", as a write records it", actor: caller.actor.Recorded()})
	}
	for _, caller := range refused {
		q := &countingQuerier{Querier: database.Pool}
		if got, err := Resolve(ctx, q, caller.actor); !errors.Is(err, ErrUnresolvedActor) || q.reads != 0 {
			t.Errorf("%s: Resolve = %+v, %v after %d reads, want ErrUnresolvedActor and no read", caller.name, got, err, q.reads)
		}
	}
	for _, caller := range seededCallers() {
		if got, err := Resolve(ctx, database.Pool, caller.actor); err != nil || !viewersEqual(got, caller.want) {
			t.Errorf("%s: Resolve = %+v, %v, want %+v", caller.name, got, err, caller.want)
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
		credential := Credential{Browser: caller.actor.Kind == "user" && caller.actor.Act == nil}
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
		"OpenClause":           OpenClause("$1"),
		"ListsClause":          ListsClause("p.key", "$1"),
		"IssueProjectClause":   IssueProjectClause("i.key", "$1"),
		"ItemOwnerClause":      ItemOwnerClause("c.issue_key", "c.artifact_id", "$1"),
		"MessageClause":        MessageClause("m", "$1"),
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

// The callers TestTheClausesOverTheSeededCallers reads the clauses for, by their names in
// seededCallers: alice, carol's token that lists no project and the shared token, three open
// viewers that do not see PRIV; carol in the browser, an open viewer that sees PRIV; bob, the
// restricted member PRIV names; and carol's token that lists PRIV.
const (
	byAlice         = "alice"
	byBob           = "bob"
	byCarol         = "carol"
	byCarolPriv     = "carol's token naming PRIV"
	byCarolUnlisted = "carol's token naming no project"
	byShared        = "the shared agent token"
)

// passes is a probe's answer for each caller: true for each caller it names, false for every other.
func passes(callers ...string) map[string]bool {
	answers := map[string]bool{}
	for _, caller := range callers {
		answers[caller] = true
	}
	return answers
}

// TestTheClausesOverTheSeededCallers: the clauses a read binds answer each seeded viewer as the
// rule says. A project row passes ProjectClause when the viewer sees it. A person passes for an
// open viewer. A session passes for the viewer's own and the server's; for any other viewer, never
// when it acted in a private project the viewer does not see (a session_projects pair: ses_priv and
// ses_both to the open viewers that do not see PRIV, ses_priv_other, which acted in PRIV and in
// OTHER, to every caller here), and otherwise for an open viewer, and for a restricted viewer or
// one that lists projects when it acted in a project the viewer sees. A session with no pair is
// judged by its listener record's projects: an open viewer reads it when the record names no
// project the viewer does not see (ses_new_priv, whose record names PRIV, reaches no open viewer
// that does not see PRIV), and a viewer that is not open when the record names a project it sees.
// While a project is private, a session with neither a pair nor a listener record (ses_neither)
// passes for no viewer but its own and the server's.
// A role of a private project the viewer does not see (legion-priv-architect, which nobody holds,
// and legion-priv-planner, which ses_pub holds) passes for no viewer but the server's. Any other
// role passes for an open viewer unless a session that holds it now does not pass (priv-architect,
// held by ses_priv), and for any other viewer when a session that holds it now passes. For a
// restricted viewer or one that lists projects, a person passes when it sees a project the viewer
// sees or is the viewer. A conversation of no project passes when the viewer's own person or
// session is a party, or the viewer is open, and every session party passes SessionClause: carol's
// token that lists no project does not read her own conversation with ses_priv, nor bob his with
// ses_pub.
//
// With no private project every session, role and conversation reads as before a project could be
// private (R9): an open viewer reads each one, and a viewer that is not open reads a session only
// for a pair in a project it sees and a role only through such a session holding it. The one
// exception is the conversation rule above, which holds a restricted member to the sessions of its
// projects whatever is private: bob does not read his conversation with ses_pub.
func TestTheClausesOverTheSeededCallers(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	// The pairs of a filled deployment: session_projects_filled holds its row, so the open arm reads
	// the pairs (TestAnOpenViewerReadsNoOtherSessionWhileTheFillIsUnfinished holds the unfilled one).
	// The listener's record (route_roster) lists the sessions with no history: ses_new_priv with a
	// token that lists PRIV, ses_new_pub with one that lists PUB, and ses_new_none with no list, as
	// envoy.Session leaves an empty one out. ses_neither has neither a pair nor a record.
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects_filled default values;
		insert into session_projects (session_id, project_key) values ('ses_priv', 'PRIV'), ('ses_pub', 'PUB'), ('ses_both', 'PUB'), ('ses_both', 'PRIV'),
			('ses_priv_other', 'PRIV'), ('ses_priv_other', 'OTHER');
		insert into route_roster (answered, sessions, sent_at, expires_at) values (true,
			'[{"session_id":"ses_new_priv","projects":["PRIV"]},{"session_id":"ses_new_pub","projects":["PUB"]},{"session_id":"ses_new_none"}]',
			now(), now() + interval '1 hour');
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event) values
			('role', 'priv-architect', 'held_by', 'session', 'ses_priv', now(), '{}', 0),
			('role', 'pub-architect', 'held_by', 'session', 'ses_priv', now() - interval '1 hour', '{}', 0);
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted) values
			('role', 'pub-architect', 'held_by', 'session', 'ses_priv', now(), '{}', 0, false);
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event) values
			('role', 'pub-architect', 'held_by', 'session', 'ses_pub', now(), '{}', 0),
			('role', 'legion-priv-planner', 'held_by', 'session', 'ses_pub', now(), '{}', 0);
	`); err != nil {
		t.Fatalf("seed sessions and roles: %v", err)
	}
	callers := map[string]model.Actor{}
	for _, caller := range seededCallers() {
		switch caller.name {
		case byAlice, byBob, byCarol, byCarolPriv, byCarolUnlisted, byShared:
			callers[caller.name] = caller.actor
		}
	}
	if len(callers) != 6 {
		t.Fatalf("seededCallers names %d of the six callers this test reads: %v", len(callers), slices.Collect(maps.Keys(callers)))
	}
	everyone := passes(byAlice, byBob, byCarol, byCarolPriv, byCarolUnlisted, byShared)
	// openAnd is a probe's answer with no private project: every open caller, and those named.
	openAnd := func(callers ...string) map[string]bool {
		return passes(append([]string{byAlice, byCarol, byCarolUnlisted, byShared}, callers...)...)
	}
	// Each probe's want is its answer for the seed, PRIV and OTHER private; public, for a probe
	// of a session, a role or a conversation, is its answer once every project is public.
	type probe struct {
		name, sql    string
		arg          any
		want, public map[string]bool
	}
	probes := []probe{
		{"ProjectClause PRIV", ProjectClause("$2", "$1"), "PRIV", passes(byBob, byCarol, byCarolPriv), nil},
		{"ProjectClause PUB", ProjectClause("$2", "$1"), "PUB", passes(byAlice, byCarol, byCarolUnlisted, byShared), nil},
		{"PersonClause carol", PersonClause("$2", "$1"), carol, everyone, nil},
		{"PersonClause alice", PersonClause("$2", "$1"), alice, passes(byAlice, byCarol, byCarolUnlisted, byShared), nil},
		{"PersonClause bob himself", PersonClause("$2", "$1"), bob, everyone, nil},
		{"SessionClause ses_priv", SessionClause("$2", "$1"), "ses_priv", passes(byBob, byCarol, byCarolPriv), openAnd(byBob, byCarolPriv)},
		{"SessionClause ses_pub", SessionClause("$2", "$1"), "ses_pub", passes(byAlice, byCarol, byCarolUnlisted, byShared), openAnd()},
		{"SessionClause ses_both", SessionClause("$2", "$1"), "ses_both", passes(byBob, byCarol, byCarolPriv), openAnd(byBob, byCarolPriv)},
		{"SessionClause ses_priv_other", SessionClause("$2", "$1"), "ses_priv_other", passes(), openAnd(byBob, byCarolPriv)},
		{"SessionClause ses_new_priv", SessionClause("$2", "$1"), "ses_new_priv", passes(byBob, byCarol, byCarolPriv), openAnd()},
		{"SessionClause ses_new_pub", SessionClause("$2", "$1"), "ses_new_pub", passes(byAlice, byCarol, byCarolUnlisted, byShared), openAnd()},
		{"SessionClause ses_new_none", SessionClause("$2", "$1"), "ses_new_none", passes(byAlice, byCarol, byCarolUnlisted, byShared), openAnd()},
		{"SessionClause ses_neither", SessionClause("$2", "$1"), "ses_neither", passes(), openAnd()},
		{"RoleClause priv-architect", RoleClause("$2", "$1"), "priv-architect", passes(byBob, byCarol, byCarolPriv), openAnd(byBob, byCarolPriv)},
		{"RoleClause pub-architect", RoleClause("$2", "$1"), "pub-architect", passes(byAlice, byCarol, byCarolUnlisted, byShared), openAnd()},
		{"RoleClause legion-priv-architect", RoleClause("$2", "$1"), "legion-priv-architect", passes(byCarol), openAnd()},
		{"RoleClause legion-priv-planner", RoleClause("$2", "$1"), "legion-priv-planner", passes(byCarol), openAnd()},
		{"RoleClause legion-pub-architect", RoleClause("$2", "$1"), "legion-pub-architect", passes(byAlice, byCarol, byCarolUnlisted, byShared), openAnd()},
		{"PartyClause bob and ses_pub", PartyClause("$2", "$1"), []string{"person:" + bob, "session:ses_pub"}, passes(byAlice, byCarol, byCarolUnlisted, byShared), openAnd()},
		{"PartyClause bob and ses_priv", PartyClause("$2", "$1"), []string{"person:" + bob, "session:ses_priv"}, passes(byBob, byCarol), openAnd(byBob)},
		{"PartyClause carol and ses_priv", PartyClause("$2", "$1"), []string{"person:" + carol, "session:ses_priv"}, passes(byCarol, byCarolPriv), openAnd(byCarolPriv)},
	}
	read := func(who string, viewer model.Viewer, p probe) bool {
		t.Helper()
		var got bool
		if err := database.Pool.QueryRow(ctx, `select coalesce(`+p.sql+`, false)`, viewerArg(t, viewer), p.arg).Scan(&got); err != nil {
			t.Fatalf("%s for %s: %v", p.name, who, err)
		}
		return got
	}
	resolve := func(name string) model.Viewer {
		t.Helper()
		viewer, err := Resolve(ctx, database.Pool, callers[name])
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		return viewer
	}
	for name := range callers {
		viewer := resolve(name)
		for _, p := range probes {
			if got := read(name, viewer, p); got != p.want[name] {
				t.Errorf("%s for %s = %t, want %t", p.name, name, got, p.want[name])
			}
		}
	}
	// The server's own viewer passes every clause.
	for _, p := range probes {
		if !read("the server", *model.ServerViewer, p) {
			t.Errorf("%s for the server's own viewer = false, want true", p.name)
		}
	}

	if _, err := database.Pool.Exec(ctx, `update projects set visibility = 'public'`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
	for name := range callers {
		viewer := resolve(name)
		if viewer.AnyPrivate {
			t.Fatalf("%s resolves as %+v with every project public, want no private project", name, viewer)
		}
		for _, p := range probes {
			if p.public == nil {
				continue
			}
			if got := read(name, viewer, p); got != p.public[name] {
				t.Errorf("with no private project: %s for %s = %t, want %t (R9)", p.name, name, got, p.public[name])
			}
		}
	}
}

// TestRoleProjectSQLReadsAsRoleProject: RoleClause reads a role's project in SQL (roleProjectSQL),
// and that read answers every name as contracts.RoleProject does, null where it answers NoProject:
// a Legion role's token is a project key once upper-cased, and any other name, a token with an
// upper-case letter, a leading digit, a character outside the ASCII ones or too many characters
// among them, works for no project.
func TestRoleProjectSQLReadsAsRoleProject(t *testing.T) {
	database := storetest.Open(t)
	for _, role := range []string{
		"legion-priv-architect", "legion-priv-planner-2", "legion-pub-x", "legion-infra-infra-12-architect",
		"legion-ab-x", "legion-a-x", "legion-abcdefghij-x", "legion-abcdefghijk-x", "legion-a1-x", "legion-1a-x",
		"legion-Priv-x", "legion-PRIV-x", "legion-ßk-x", "legion-priv-", "legion-priv", "legion--x", "legion-_-x",
		"priv-architect", "lead-reviewer", "Legion-priv-x", "xlegion-priv-x", "legion-priv-\nx", "",
	} {
		want := contracts.RoleProject(role)
		var got *string
		if err := database.Pool.QueryRow(context.Background(), `select `+roleProjectSQL("$1::text"), role).Scan(&got); err != nil {
			t.Fatalf("roleProjectSQL(%q): %v", role, err)
		}
		switch {
		case want == contracts.NoProject && got != nil:
			t.Errorf("roleProjectSQL(%q) = %q, want null: RoleProject reads it as no project", role, *got)
		case want != contracts.NoProject && (got == nil || *got != want):
			t.Errorf("roleProjectSQL(%q) = %v, want %q as RoleProject reads it", role, got, want)
		}
	}
}

func viewersEqual(a, b model.Viewer) bool {
	return a.Person == b.Person && a.Session == b.Session && a.Restricted == b.Restricted &&
		a.Open == b.Open && a.All == b.All && a.AnyPrivate == b.AnyPrivate && slices.Equal(a.Sees, b.Sees) && slices.Equal(a.Lists, b.Lists)
}
