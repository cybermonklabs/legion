package refs

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheVisibilityArgumentCarriesTheViewer: VisibilityArg is the one conversion from a reader to
// the argument every predicate binds, "Contracts / K2, the visibility argument" of the
// private-projects plan: the acting credential as the author rule compares it, beside what the
// reader's viewer sees. A reader with no viewer is the closed viewer, which sees nothing and reads
// as though some project were private.
func TestTheVisibilityArgumentCarriesTheViewer(t *testing.T) {
	carol := "carol@example.com"
	for _, test := range []struct {
		name   string
		reader model.Actor
		want   string
	}{
		{
			name:   "a reader with no viewer",
			reader: model.Actor{Kind: "user", ID: "alice"},
			want:   `{"author":{"kind":"person","id":"alice"},"sees":[],"open":false,"all":false,"private":true}`,
		},
		{
			name:   "the shared bearer with no viewer",
			reader: model.Actor{},
			want:   `{"author":null,"sees":[],"open":false,"all":false,"private":true}`,
		},
		{
			name: "a restricted member",
			reader: model.Actor{Kind: "user", ID: "bob@outside.example", Viewer: &model.Viewer{
				Person: "bob@outside.example", Restricted: true, Sees: []string{"PRIV"}, AnyPrivate: true,
			}},
			want: `{"author":{"kind":"person","id":"bob@outside.example"},"person":"bob@outside.example",` +
				`"sees":["PRIV"],"open":false,"all":false,"private":true}`,
		},
		{
			name: "a personal token that lists a project",
			reader: model.Actor{Kind: "session", Owner: &carol, Viewer: &model.Viewer{
				Person: carol, Sees: []string{"PRIV"}, AnyPrivate: true, Lists: []string{"PRIV"},
			}},
			want: `{"author":{"kind":"person","id":"carol@example.com"},"person":"carol@example.com",` +
				`"sees":["PRIV"],"open":false,"all":false,"private":true,"lists":["PRIV"]}`,
		},
		{
			name: "a session of an open viewer that sees nothing yet",
			reader: model.Actor{Kind: "session", ID: "ses_1", Viewer: &model.Viewer{
				Session: "ses_1", Open: true,
			}},
			want: `{"author":null,"session":"ses_1",` +
				`"sees":[],"open":true,"all":false,"private":false}`,
		},
		{
			name:   "the server's own viewer",
			reader: model.ServerReader,
			want:   `{"author":null,"sees":[],"open":true,"all":true,"private":false}`,
		},
	} {
		got, ok := VisibilityArg(test.reader).([]byte)
		if !ok {
			t.Fatalf("%s: VisibilityArg answered %T, want the argument's JSON", test.name, VisibilityArg(test.reader))
		}
		if !json.Valid(got) || string(got) != test.want {
			t.Errorf("%s: VisibilityArg = %s\nwant %s", test.name, got, test.want)
		}
	}
}

// The projects and people of verification v16's seed (the private-projects live-verification.md):
// PUB is public and PRIV private; alice is a full member no access list names, bob a restricted
// member PRIV's list names, and carol a full member PRIV's list names, who owns both projects.
const (
	seededPublic  = "PUB"
	seededPrivate = "PRIV"
	seededAlice   = "alice@example.com"
	seededBob     = "bob@outside.example"
	seededCarol   = "carol@example.com"
)

// seededCaller is one caller of verification v16, as the refs reads see it: an actor whose
// viewer access.Resolve answers over the seed, or, for the closed caller, an actor with none.
type seededCaller struct {
	name   string
	actor  model.Actor
	closed bool
	server bool
}

// seededCallers is every caller of verification v16, bob's token that lists PRIV, the closed
// viewer a read with no viewer reads as, and the server's own viewer (model.ServerViewer). Each
// caller carries the credential optionalActor resolves for it, a browser's included, since
// access.Resolve reads the credential. A full member's token that lists PRIV sees every public
// project its person sees and PRIV where its person sees PRIV (access.CredentialCovers); bob's,
// a restricted member's, sees PRIV alone.
func seededCallers() []seededCaller {
	alice, bob, carol := seededAlice, seededBob, seededCarol
	cookie := &model.Credential{Scopes: model.Scopes}
	return []seededCaller{
		{name: "alice", actor: model.Actor{Kind: "user", ID: seededAlice, Credential: cookie}},
		{name: "bob", actor: model.Actor{Kind: "user", ID: seededBob, Credential: cookie}},
		{name: "carol", actor: model.Actor{Kind: "user", ID: seededCarol, Credential: cookie}},
		{name: "carol's token that lists PRIV", actor: model.Actor{Kind: "session", Owner: &carol,
			Credential: &model.Credential{TokenID: "carol-priv", Projects: []string{seededPrivate}}}},
		{name: "alice's token that lists PRIV", actor: model.Actor{Kind: "session", Owner: &alice,
			Credential: &model.Credential{TokenID: "alice-priv", Projects: []string{seededPrivate}}}},
		{name: "bob's token that lists PRIV", actor: model.Actor{Kind: "session", Owner: &bob,
			Credential: &model.Credential{TokenID: "bob-priv", Projects: []string{seededPrivate}}}},
		{name: "carol's token that lists no project", actor: model.Actor{Kind: "session", Owner: &carol,
			Credential: &model.Credential{TokenID: "carol-none"}}},
		{name: "the shared token", actor: model.Actor{Credential: &model.Credential{}}},
		{name: "the closed viewer", actor: model.Actor{Kind: "user", ID: seededAlice}, closed: true},
		{name: "the server", actor: model.Actor{}, server: true},
	}
}

// reader is the caller as a read binds it: its credential dropped and its viewer resolved over
// what q holds now (access.Resolve), none for the closed caller, model.ServerViewer for the server.
func (caller seededCaller) reader(t *testing.T, q access.Querier) model.Actor {
	t.Helper()
	switch {
	case caller.closed:
		return caller.actor.Reader()
	case caller.server:
		return model.ServerReader
	}
	viewer, err := access.Resolve(context.Background(), q, caller.actor)
	if err != nil {
		t.Fatalf("resolve what %s sees: %v", caller.name, err)
	}
	reader := caller.actor.Reader()
	reader.Viewer = &viewer
	return reader
}

// seedVerificationProjects writes verification v16's projects, people and access lists in tx.
func seedVerificationProjects(t *testing.T, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into people (email, member_kind) values ($1, 'full'), ($2, 'restricted'), ($3, 'full');
	`, seededAlice, seededBob, seededCarol); err != nil {
		t.Fatalf("seed the people: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		insert into projects (key, name, default_assignee, visibility) values
			($1, 'Public', $3, 'public'), ($2, 'Private', $3, 'private')
	`, seededPublic, seededPrivate, seededCarol); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		insert into project_access (project_key, principal_kind, principal, added_by) values
			($1, 'person', $2, '{"kind":"user","id":"carol@example.com"}'),
			($1, 'person', $3, '{"kind":"user","id":"carol@example.com"}')
	`, seededPrivate, seededBob, seededCarol); err != nil {
		t.Fatalf("seed PRIV's access list: %v", err)
	}
}

// writeSeedNodes writes nodes through the store's own writer, at the sentinel event.
func writeSeedNodes(t *testing.T, tx pgx.Tx, nodes []graphstore.Node, tuples []graphstore.Tuple) {
	t.Helper()
	for index := range nodes {
		node := &nodes[index]
		node.Visibility = graphstore.VisibilityProject
		node.State = graphstore.StateLive
		if node.Supertag == "" {
			node.Supertag = node.Kind
		}
		if node.MadeAt.IsZero() {
			node.MadeAt = time.Now()
		}
		if node.MadeBy == (model.Actor{}) {
			node.MadeBy = model.Actor{Kind: "user", ID: seededCarol}
		}
	}
	if err := graphstore.Write(context.Background(), tx, nodes, tuples); err != nil {
		t.Fatalf("write the seed's nodes: %v", err)
	}
}

// serverReader is the reader of every test of this package that reads the graph for no particular
// viewer: the server's own (model.ServerReader), which proves no author and sees every project and
// every party, which is what a read with no actor was before a reader carried a viewer.
var serverReader = model.ServerReader

// planReader is a reader a plan or a timing binds, by the name a failure gives it.
type planReader struct {
	name   string
	reader model.Actor
}

// planReaders are the readers a plan binds: the server's own, and two that are not, both of a
// deployment that holds a private project and both seeing project. The server's viewer passes
// every rule at its `all` flag, so a custom plan bound to it alone folds every project and party
// arm away and never shows the read a member's statement runs: alice is an open member, whose
// session and party arms read session_projects, and carol's token lists project, so her arms read
// the sessions and people of the projects she sees.
func planReaders(project string) []planReader {
	alice, carol := "alice@example.com", "carol@example.com"
	return []planReader{
		{"the server", serverReader},
		{"alice, an open member", model.Actor{Kind: "user", ID: alice, Viewer: &model.Viewer{
			Person: alice, Open: true, Sees: []string{project}, AnyPrivate: true,
		}}},
		{"carol's token listing " + project, model.Actor{Kind: "session", Owner: &carol, Viewer: &model.Viewer{
			Person: carol, Sees: []string{project}, AnyPrivate: true, Lists: []string{project},
		}}},
	}
}

// scopedNode is one node the project clause test reads, and the callers that see it.
type scopedNode struct {
	kind, id string
	// held says the store holds a row of the node; a component, and the address of a node
	// nothing wrote, hold none.
	held bool
	sees []string
}

// TestTheProjectClauseHidesAPrivateNode: for every node kind the registry holds and every caller
// of verification v16, each of the four predicates the registry renders - over a node's address
// with its kind a literal (VisibleKind) or a column (VisibleEnd), and over a row a read already
// holds (VisibleKindRow, VisibleRow) - answers the kind's rule: a node of PRIV only to the people
// and credentials that see PRIV, a node of PUB only to those that see PUB, a person, a session, a
// role, a page and a conversation as the access package's clauses decide, the block schema's nodes
// to everyone, and a node of a project kind the store holds no row of to no caller while a project
// is private.
func TestTheProjectClauseHidesAPrivateNode(t *testing.T) {
	tx := beginSeed(t)
	seedVerificationProjects(t, tx)
	seeded := seedScopedNodes(t, tx)
	ask, comment, message, conversation := seeded.ask, seeded.comment, seeded.message, seeded.conversation
	pubAsk, pubComment, pubMessage := seeded.pubAsk, seeded.pubComment, seeded.pubMessage

	// Who sees what, by the caller names seededCallers gives. The server sees every node.
	var (
		seesPrivate = []string{"bob", "carol", "carol's token that lists PRIV", "bob's token that lists PRIV"}
		seesPublic  = []string{"alice", "carol", "carol's token that lists PRIV", "alice's token that lists PRIV",
			"carol's token that lists no project", "the shared token"}
		open     = seesPublic
		everyone = []string{"alice", "bob", "carol", "carol's token that lists PRIV", "alice's token that lists PRIV",
			"bob's token that lists PRIV", "carol's token that lists no project", "the shared token", "the closed viewer"}
	)
	scoped := []scopedNode{
		{kind: "issue", id: "PRIV-1", held: true, sees: seesPrivate},
		{kind: "artifact", id: "PRIV/notes-md", held: true, sees: seesPrivate},
		{kind: "ask", id: ask, held: true, sees: seesPrivate},
		{kind: "comment", id: comment, held: true, sees: seesPrivate},
		{kind: "message", id: message, held: true, sees: seesPrivate},
		{kind: "block", id: "PRIV/notes-md#b1", held: true, sees: seesPrivate},
		{kind: "ask_block", id: "ask/" + ask + "#b1", held: true, sees: seesPrivate},
		{kind: "comment_block", id: "comment/" + comment + "#b1", held: true, sees: seesPrivate},
		{kind: "message_block", id: "message/" + message + "#b1", held: true, sees: seesPrivate},
		{kind: "component", id: "PRIV/storage", sees: seesPrivate},
		{kind: "issue", id: "PUB-1", held: true, sees: seesPublic},
		{kind: "artifact", id: "PUB/notes-md", held: true, sees: seesPublic},
		{kind: "ask", id: pubAsk, held: true, sees: seesPublic},
		{kind: "comment", id: pubComment, held: true, sees: seesPublic},
		{kind: "message", id: pubMessage, held: true, sees: seesPublic},
		{kind: "block", id: "PUB/notes-md#b1", held: true, sees: seesPublic},
		{kind: "ask_block", id: "ask/" + pubAsk + "#b1", held: true, sees: seesPublic},
		{kind: "comment_block", id: "comment/" + pubComment + "#b1", held: true, sees: seesPublic},
		{kind: "message_block", id: "message/" + pubMessage + "#b1", held: true, sees: seesPublic},
		{kind: "component", id: "PUB/storage", sees: seesPublic},
		// A conversation of no project: an open viewer, which sees every session party, and a
		// party or a credential of a person party only when it sees each session party too
		// (access.PartyClause): bob's token that lists PRIV does not see ses_pub, of PUB.
		{kind: "message", id: conversation, held: true, sees: open},
		// Nothing wrote these: a node of a project kind the store holds no row of is visible
		// only to an open viewer while no project is private.
		{kind: "issue", id: "PRIV-404"},
		{kind: "ask_block", id: "ask/" + uuid.NewString() + "#b1"},
		// A person: an open viewer, the person themself and their own token, and a viewer that
		// sees a project the person sees.
		{kind: graphstore.KindPerson, id: seededBob, held: true, sees: append(slices.Clone(open), "bob", "bob's token that lists PRIV")},
		{kind: graphstore.KindPerson, id: seededAlice, held: true, sees: open},
		// A session and its page: a viewer that sees a project the session acted in, and an open
		// viewer unless the session acted in a project it does not see (access.SessionClause), so
		// a session of PRIV reaches only those that see PRIV. A role the session holds, and its
		// page: a viewer that may read that session, an open viewer included only then
		// (access.RoleClause), so the role of PRIV's session reaches only those that see PRIV.
		{kind: graphstore.KindSession, id: "ses_priv", held: true, sees: seesPrivate},
		{kind: graphstore.KindRole, id: "r_priv", held: true, sees: seesPrivate},
		{kind: graphstore.KindPage, id: graphstore.SessionPageID("ses_priv"), held: true, sees: seesPrivate},
		{kind: graphstore.KindPage, id: graphstore.RolePageID("r_priv"), held: true, sees: seesPrivate},
		{kind: graphstore.KindSession, id: "ses_pub", held: true, sees: open},
		{kind: graphstore.KindRole, id: "r_pub", held: true, sees: open},
		{kind: graphstore.KindPage, id: graphstore.SessionPageID("ses_pub"), held: true, sees: open},
		{kind: graphstore.KindPage, id: graphstore.RolePageID("r_pub"), held: true, sees: open},
		// The block schema's own nodes, which the migration seeds: everyone.
		{kind: "supertag", id: "issue", held: true, sees: everyone},
		{kind: "attribute", id: "issue/status", sees: everyone},
	}
	covered := map[string]bool{}
	for _, node := range scoped {
		covered[node.kind] = true
	}
	for _, kind := range VisibilityKinds() {
		if !covered[kind] {
			t.Errorf("the registry holds the node kind %q and this test reads no node of it", kind)
		}
	}

	for _, caller := range seededCallers() {
		seen := VisibilityArg(caller.reader(t, tx))
		for _, node := range scoped {
			want := caller.server || slices.Contains(node.sees, caller.name)
			for form, got := range scopedPredicates(t, tx, node, seen) {
				if got != want {
					t.Errorf("%s of %s %s answers %t to %s; want %t", form, node.kind, node.id, got, caller.name, want)
				}
			}
		}
	}
}

// scopedSeed is the ids of the nodes seedScopedNodes writes under fresh uuids: an ask, a comment
// and a message of each project, and a conversation of no project.
type scopedSeed struct {
	ask, comment, message, conversation string
	pubAsk, pubComment, pubMessage      string
}

// seedScopedNodes writes, in tx, a node of every scope the registry states, in PRIV and in PUB
// where the scope has a project: an issue, a document and its block, an ask, a comment and a
// message, a block of each of the three items, a conversation of no project between carol and the
// session ses_pub, the people bob and alice, the sessions ses_priv and ses_pub with the roles each
// holds (r_priv and r_pub) and the page of each, and the projects each session acted in.
func seedScopedNodes(t *testing.T, tx pgx.Tx) scopedSeed {
	t.Helper()
	seeded := scopedSeed{
		ask: uuid.NewString(), comment: uuid.NewString(), message: uuid.NewString(), conversation: uuid.NewString(),
		pubAsk: uuid.NewString(), pubComment: uuid.NewString(), pubMessage: uuid.NewString(),
	}
	nodes := []graphstore.Node{
		{Kind: "issue", ID: "PRIV-1", Project: seededPrivate},
		{Kind: "artifact", ID: "PRIV/notes-md", Project: seededPrivate},
		{Kind: "ask", ID: seeded.ask, Project: seededPrivate},
		{Kind: "comment", ID: seeded.comment, Project: seededPrivate},
		{Kind: "message", ID: seeded.message, Project: seededPrivate},
		{Kind: "block", ID: "PRIV/notes-md#b1", Project: seededPrivate},
		{Kind: "ask_block", ID: "ask/" + seeded.ask + "#b1"},
		{Kind: "comment_block", ID: "comment/" + seeded.comment + "#b1"},
		{Kind: "message_block", ID: "message/" + seeded.message + "#b1"},
		{Kind: "issue", ID: "PUB-1", Project: seededPublic},
		{Kind: "artifact", ID: "PUB/notes-md", Project: seededPublic},
		{Kind: "ask", ID: seeded.pubAsk, Project: seededPublic},
		{Kind: "comment", ID: seeded.pubComment, Project: seededPublic},
		{Kind: "message", ID: seeded.pubMessage, Project: seededPublic},
		{Kind: "block", ID: "PUB/notes-md#b1", Project: seededPublic},
		{Kind: "ask_block", ID: "ask/" + seeded.pubAsk + "#b1"},
		{Kind: "comment_block", ID: "comment/" + seeded.pubComment + "#b1"},
		{Kind: "message_block", ID: "message/" + seeded.pubMessage + "#b1"},
		{Kind: "message", ID: seeded.conversation, Parties: []string{"person:" + seededCarol, "session:ses_pub"}},
		{Kind: graphstore.KindPerson, ID: seededBob},
		{Kind: graphstore.KindPerson, ID: seededAlice},
		{Kind: graphstore.KindSession, ID: "ses_priv"},
		{Kind: graphstore.KindSession, ID: "ses_pub"},
		{Kind: graphstore.KindRole, ID: "r_priv"},
		{Kind: graphstore.KindRole, ID: "r_pub"},
		{Kind: graphstore.KindPage, ID: graphstore.SessionPageID("ses_priv")},
		{Kind: graphstore.KindPage, ID: graphstore.RolePageID("r_priv")},
		{Kind: graphstore.KindPage, ID: graphstore.SessionPageID("ses_pub")},
		{Kind: graphstore.KindPage, ID: graphstore.RolePageID("r_pub")},
	}
	held := func(role, session string) graphstore.Tuple {
		return graphstore.Tuple{FromKind: graphstore.KindRole, FromID: role, Kind: graphstore.TupleHeldBy,
			ToKind: graphstore.KindSession, ToID: session, At: time.Now(), Who: model.Actor{Kind: "session", ID: session}, Asserted: true}
	}
	writeSeedNodes(t, tx, nodes, []graphstore.Tuple{held("r_priv", "ses_priv"), held("r_pub", "ses_pub")})
	// The pairs of a filled deployment: session_projects_filled holds its row, so an open viewer's
	// session arm reads the pairs (access.TestAnOpenViewerReadsNoOtherSessionWhileTheFillIsUnfinished).
	if _, err := tx.Exec(context.Background(), `insert into session_projects (session_id, project_key) values ('ses_priv', $1), ('ses_pub', $2)`,
		seededPrivate, seededPublic); err != nil {
		t.Fatalf("record the projects each session acted in: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `insert into session_projects_filled default values`); err != nil {
		t.Fatalf("record that the sessions' projects are filled: %v", err)
	}
	return seeded
}

// scopedPredicates answers each predicate the registry renders over node for the visibility
// argument seen, by the name of the predicate. Every predicate answers true or false, never
// unknown: a reader negates them and coalesces them.
func scopedPredicates(t *testing.T, tx pgx.Tx, node scopedNode, seen any) map[string]bool {
	t.Helper()
	var byLiteral, byColumn, held bool
	var kindRow, ownRow *bool
	if err := tx.QueryRow(context.Background(), `select `+
		VisibleKind(node.kind, "$1", "$2")+`, `+VisibleEnd("$3::text", "$1", true, "$2")+`,
		exists (select 1 from graph_nodes n where n.kind = $3 and n.id = $1),
		(select `+VisibleKindRow(node.kind, "n", "$2")+` from graph_nodes n where n.kind = $3 and n.id = $1),
		(select `+VisibleRow("n", "$2")+` from graph_nodes n where n.kind = $3 and n.id = $1)`,
		node.id, seen, node.kind).Scan(&byLiteral, &byColumn, &held, &kindRow, &ownRow); err != nil {
		t.Fatalf("read the predicates of %s %s: %v", node.kind, node.id, err)
	}
	if held != node.held {
		t.Fatalf("the store holds a row of %s %s: %t, want %t", node.kind, node.id, held, node.held)
	}
	answers := map[string]bool{"VisibleKind": byLiteral, "VisibleEnd": byColumn}
	if held {
		if kindRow == nil || ownRow == nil {
			t.Fatalf("a predicate over the row of %s %s answers unknown (VisibleKindRow %v, VisibleRow %v)", node.kind, node.id, kindRow, ownRow)
		}
		answers["VisibleKindRow"], answers["VisibleRow"] = *kindRow, *ownRow
	}
	return answers
}

// beginSeed opens a database of the test's own and a transaction on it, rolled back at the end.
func beginSeed(t *testing.T) pgx.Tx {
	t.Helper()
	database := storetest.Open(t)
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the seed: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}
