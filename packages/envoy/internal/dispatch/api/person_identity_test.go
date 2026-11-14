package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
)

// The identities of TestAPersonAndTheirProvedSessionAreOneNodeAndSeeOnlyTheirOwn: the role the
// proved session holds, and a login one character from the person's own (brokerTestPerson).
const (
	identityRole = "identity-reader"
	identityTwin = "alicf"
)

// identityRow is one row of the identity table the test carries: an identity as a request
// presents it, and the person it must resolve to, written down before any read.
type identityRow struct {
	identity  string
	presented string
	person    string
}

// TestAPersonAndTheirProvedSessionAreOneNodeAndSeeOnlyTheirOwn is the identity half of
// dispatch://INFRA-1149's c-person-graph, the rows its shell driver cannot present: a session
// the server proved a person's, which needs the secrets broker's DPoP-bound token, and a role
// that person's session held, which needs a claim store this pod's fake Envoy does not have.
// Here the session is proved through the server's own broker-token path, with an ES256 key the
// test makes (brokertokentest.NewCaller), and the role's holder comes from a claim store the
// outbox is handed (outbox.Deps.RoleClaims), so both rows are written through the real paths.
//
// The test carries its own table of identities, each row naming the person it must resolve to
// before anything is read, and reads every row back through the server:
//
//   - a login in mixed case, the same login with spaces around it, the raw actor id a record of
//     that person stored, the session the server proved theirs and the role that session held
//     all answer one person node, with the alias hanging off it;
//   - two logins one character apart, and two personal tokens of one deployment held by two
//     people, answer two person nodes;
//   - an actor that merely claims another person's session earns no alias;
//   - the role's held_by tuple names the proved session, the session hangs under the role and
//     the role's page under the role, and a delivery to the role ends at the role and on no
//     person's page;
//   - a delivery to the person's proved session reaches that person's page and count, and
//     neither the page nor the count of the person one character away.
func TestAPersonAndTheirProvedSessionAreOneNodeAndSeeOnlyTheirOwn(t *testing.T) {
	ctx := context.Background()
	table := []identityRow{
		{"a login in mixed case", "AlIcE", brokerTestPerson},
		{"the same login with spaces around it", "  alice  ", brokerTestPerson},
		{"a raw actor id of that person", "the author a record of hers stored", brokerTestPerson},
		{"a session the server proved theirs", brokerTestSession, brokerTestPerson},
		{"a role that person held", identityRole, brokerTestPerson},
		{"a login one character from hers", identityTwin, identityTwin},
		{"her personal token", "the token alice holds", brokerTestPerson},
		{"another person's personal token", "the token alicf holds", identityTwin},
	}

	rig := newIdentityRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-proving-alices-session"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	issue := rig.seedRoutedIssue(t)

	// The writes, each through the route a caller of that identity uses.
	proved := rig.created(t, "the proved session's comment", rig.serve(caller.presenting(t, http.MethodPost,
		"/api/v1/issues/"+issue+"/comments", token, map[string]any{"body": "Written by the session the broker proved."})))
	byPerson := rig.created(t, "the person's comment", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+issue+"/comments", map[string]any{"body": "Written by the person herself."}, brokerTestPerson))
	byTwin := rig.created(t, "the twin's comment", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+issue+"/comments", map[string]any{"body": "Written by the login one character away."}, identityTwin))
	// Each personal token writes as the session its body names, a session of its own holder's
	// agent: a personal token proves its person and no session.
	tokens := map[string]string{
		"the token alice holds": personalTokenOwnedBy(t, rig.database, brokerTestPerson, model.Scopes...),
		"the token alicf holds": personalTokenOwnedBy(t, rig.database, identityTwin, model.Scopes...),
	}
	tokenSessions := map[string]string{"the token alice holds": "ses-of-alices-token", "the token alicf holds": "ses-of-alicfs-token"}
	tokenComments := map[string]string{}
	for presented, credential := range tokens {
		tokenComments[presented] = rig.created(t, "a comment under "+presented, bearerTokenRequest(t, rig.handler, http.MethodPost,
			"/api/v1/issues/"+issue+"/comments", credential, map[string]any{
				"body": "Written under " + presented + ".", "actor": map[string]any{"kind": "session", "id": tokenSessions[presented]},
			}))
	}
	// The twin's personal token names the proved session as its own: a session it merely claims.
	claimed := rig.created(t, "the claim of another person's session", bearerTokenRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+issue+"/comments", tokens["the token alicf holds"], map[string]any{
			"body": "Written as a session alicf does not own.", "actor": map[string]any{"kind": "session", "id": brokerTestSession},
		}))
	rig.publish(t, issue)

	// Each row read back through the server.
	resolve := func(row identityRow) string {
		t.Helper()
		switch row.identity {
		case "a login in mixed case", "the same login with spaces around it", "a login one character from hers":
			return rig.pagePerson(t, row.presented, brokerTestPerson)
		case "a raw actor id of that person":
			// What the record stored, read back out of the comment as the server answers it, and
			// then read as the login it is.
			stored := rig.storedAuthor(t, byPerson)
			if stored.ID == "" {
				t.Fatalf("the person's comment stored no actor id: %+v", stored)
			}
			return rig.pagePerson(t, stored.ID, brokerTestPerson)
		case "a session the server proved theirs":
			return rig.aliasOf(t, row.presented)
		case "a role that person held":
			held := rig.currentOut(t, graphstore.KindRole, row.presented, graphstore.TupleHeldBy)
			if len(held) != 1 || held[0].ToKind != graphstore.KindSession {
				t.Fatalf("the role %s is held by %+v, want one session", row.presented, held)
			}
			return rig.aliasOf(t, held[0].ToID)
		default:
			// A personal token's comment is the session its body named, and the record keeps the
			// person the token proved as that author's owner: the comment as the server reads it
			// back names the person, whose page is then read.
			stored := rig.storedAuthor(t, tokenComments[row.presented])
			if stored.Kind != "session" || stored.ID != tokenSessions[row.presented] || stored.Owner == nil {
				t.Fatalf("the comment written under %s stored %+v, want its session and the person the token proved", row.presented, stored)
			}
			return rig.pagePerson(t, *stored.Owner, brokerTestPerson)
		}
	}
	resolved := map[string]string{}
	for _, row := range table {
		got := resolve(row)
		resolved[row.identity] = got
		if got != row.person {
			t.Errorf("%s (%q) resolves to person %q, and the table says %q", row.identity, row.presented, got, row.person)
		}
	}

	// One person node answers the first five rows, with the alias hanging off it, and the two
	// identities that must stay apart answer two.
	var people int
	if err := rig.database.Pool.QueryRow(ctx, `
		select count(*) from graph_nodes where kind = $1 and lower(btrim(id)) = $2
	`, graphstore.KindPerson, brokerTestPerson).Scan(&people); err != nil {
		t.Fatalf("count the person nodes of %s: %v", brokerTestPerson, err)
	}
	if people != 1 {
		t.Errorf("the store holds %d person nodes spelling %s, want the one every spelling of her resolves to", people, brokerTestPerson)
	}
	aliases := rig.currentIn(t, graphstore.KindPerson, brokerTestPerson, graphstore.TupleAliasOf)
	if !slices.ContainsFunc(aliases, func(alias graphstore.Tuple) bool {
		return alias.FromKind == graphstore.KindSession && alias.FromID == brokerTestSession
	}) {
		t.Errorf("the person node %s carries the aliases %+v, want the session the server proved hers", brokerTestPerson, aliases)
	}
	for _, apart := range [][2]string{
		{"a login in mixed case", "a login one character from hers"},
		{"her personal token", "another person's personal token"},
	} {
		if resolved[apart[0]] == resolved[apart[1]] {
			t.Errorf("%s and %s answer one person node %q, want two", apart[0], apart[1], resolved[apart[0]])
		}
	}
	for _, person := range []string{brokerTestPerson, identityTwin} {
		if _, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindPerson, person); err != nil || !held {
			t.Errorf("the store holds no person node %s (err %v)", person, err)
		}
	}

	// An actor that merely claims another person's session earns no alias: the proved session is
	// the person's alone, and the claimant has no session aliased to her at all.
	if sessionAliases := rig.currentOut(t, graphstore.KindSession, brokerTestSession, graphstore.TupleAliasOf); len(sessionAliases) != 1 ||
		sessionAliases[0].ToID != brokerTestPerson {
		t.Errorf("the proved session is an alias of %+v, want of %s alone after %s claimed it in comment %s",
			sessionAliases, brokerTestPerson, identityTwin, claimed)
	}
	if twinAliases := rig.currentIn(t, graphstore.KindPerson, identityTwin, graphstore.TupleAliasOf); len(twinAliases) != 0 {
		t.Errorf("%s, who only claimed a session, carries the aliases %+v", identityTwin, twinAliases)
	}

	// The role, its holder and its page, as the outbox's record states them, and a delivery to
	// the role ending at the role.
	if held := rig.currentOut(t, graphstore.KindRole, identityRole, graphstore.TupleHeldBy); len(held) != 1 || held[0].ToID != brokerTestSession {
		t.Errorf("the role is held by %+v, want the proved session %s", held, brokerTestSession)
	}
	if parts := rig.currentOut(t, graphstore.KindSession, brokerTestSession, graphstore.TuplePartOf); !slices.ContainsFunc(parts,
		func(part graphstore.Tuple) bool {
			return part.ToKind == graphstore.KindRole && part.ToID == identityRole
		}) {
		t.Errorf("the proved session is part of %+v, want the role it claimed", parts)
	}
	if parts := rig.currentOut(t, graphstore.KindPage, graphstore.RolePageID(identityRole), graphstore.TuplePartOf); len(parts) != 1 ||
		parts[0].ToKind != graphstore.KindRole || parts[0].ToID != identityRole {
		t.Errorf("the role's page hangs under %+v, want the role", parts)
	}
	for _, kind := range []string{graphstore.KindRole, graphstore.KindPage} {
		id := identityRole
		if kind == graphstore.KindPage {
			id = graphstore.RolePageID(identityRole)
		}
		if _, held, err := graphstore.ReadNode(ctx, rig.database.Pool, kind, id); err != nil || !held {
			t.Errorf("the store holds no %s node %s (err %v)", kind, id, err)
		}
	}
	delivered := rig.currentOut(t, commentNodeKind, byTwin, graphstore.TupleDeliveredTo)
	if len(delivered) == 0 {
		t.Fatalf("the twin's comment on an issue routed to the role was delivered nowhere, so the role's delivery proves nothing")
	}
	for _, to := range delivered {
		if to.ToKind != graphstore.KindRole || to.ToID != identityRole {
			t.Errorf("the twin's comment was delivered to %s %s, want the role and no person or session", to.ToKind, to.ToID)
		}
	}

	// A delivery to the person's proved session: her page and her count hold it, and the page and
	// the count of the person one character away hold nothing more than they did.
	before := map[string]personNodes{
		brokerTestPerson: rig.page(t, brokerTestPerson, brokerTestPerson),
		identityTwin:     rig.page(t, identityTwin, identityTwin),
	}
	if personNodeOn(before[brokerTestPerson], byTwin) || personNodeOn(before[brokerTestPerson], proved) {
		t.Errorf("her page holds a comment delivered to the role or written by her session, which ends at the role and at the session: %+v",
			before[brokerTestPerson].Rows)
	}
	mention := decodeMentionedComment(t, dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body": "For her agent.", "mentions": []map[string]any{{"target": "session:" + brokerTestSession}},
	}, "bob"))
	attempts := settledCommentDeliveries(t, rig.database, mention.ID)
	if !slices.ContainsFunc(attempts, func(attempt model.CommentDelivery) bool {
		return attempt.SessionID != nil && *attempt.SessionID == brokerTestSession && attempt.State == graphstore.DeliverySent
	}) {
		t.Fatalf("the mention of the proved session settled %+v, want one attempt sent to it", attempts)
	}
	rig.publish(t, issue)
	after := map[string]personNodes{
		brokerTestPerson: rig.page(t, brokerTestPerson, brokerTestPerson),
		identityTwin:     rig.page(t, identityTwin, identityTwin),
	}
	if !slices.ContainsFunc(after[brokerTestPerson].Rows, func(row personNode) bool {
		return row.ID == mention.ID && slices.Contains(row.Relations, graphstore.TupleDeliveredTo)
	}) {
		t.Errorf("her page after the delivery to her proved session is %+v, want the comment as delivered", after[brokerTestPerson].Rows)
	}
	if got, want := after[brokerTestPerson].Total, before[brokerTestPerson].Total+1; got != want {
		t.Errorf("her count reads %d after the delivery to her proved session, want %d", got, want)
	}
	if personNodeOn(after[identityTwin], mention.ID) {
		t.Errorf("the page of %s holds the delivery to %s's session: %+v", identityTwin, brokerTestPerson, after[identityTwin].Rows)
	}
	if got, want := after[identityTwin].Total, before[identityTwin].Total; got != want {
		t.Errorf("the count of %s reads %d after a delivery to %s's session, and %d before it", identityTwin, got, brokerTestPerson, want)
	}
}

// identityRig is the server the identity test drives: the package's test server with the
// broker-token check wired to a stub broker, as newBrokerTokenRig wires it, and pointed at a
// stub Envoy listener that holds the proved session live and takes every send to it.
type identityRig struct {
	brokerTokenRig
}

func newIdentityRig(t *testing.T) *identityRig {
	t.Helper()
	const sessions = `[{"session_id":"` + brokerTestSession + `","title":"alice's agent","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/"+brokerTestSession:
			writeSessionRecord(t, w, sessions, brokerTestSession)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"identity-envelope","recipient":"` + brokerTestSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	broker := brokertokentest.NewStubBroker(t)
	handler, database, deps := newTestServer(t, testServerOptions{
		envoyURL: listener.URL,
		brokerTokens: func(database *store.Store) *brokertoken.Verifier {
			verifier, err := brokertoken.New(brokertoken.Options{
				BrokerURL: broker.Server.URL,
				Account:   brokertoken.NewAccountToken(accountTokenFile(t, credentialTestAccountToken)),
				Origin:    testServerURL,
				Replay:    store.NewDPoPProofs(database.Pool),
				Address:   auth.ForwardedAddress,
			})
			if err != nil {
				t.Fatalf("build the broker token verifier: %v", err)
			}
			return verifier
		},
	})
	return &identityRig{brokerTokenRig{handler: handler, deps: deps, database: database, broker: broker}}
}

// seedRoutedIssue makes the project the broker's token is scoped to and an issue in it routed to
// the role the proved session holds, as the person, and answers the issue's key.
func (r *identityRig) seedRoutedIssue(t *testing.T) string {
	t.Helper()
	if created := r.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	}); created.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := r.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": brokerTestProject, "title": "Who is who", "spec": "# Spec\n\nA passage.",
	})
	if issue.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", issue.Code, issue.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key
	if routed := r.asPerson(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"route": "role:" + identityRole}); routed.Code != http.StatusOK {
		t.Fatalf("route the issue to the role: status=%d body=%s", routed.Code, routed.Body.String())
	}
	return key
}

// created answers the id of the comment a write created, failing on any other answer.
func (r *identityRig) created(t *testing.T, what string, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("write %s: status=%d body=%s", what, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, response).ID
}

// storedAuthor is the author a comment's record stored, as GET /api/v1/comments/{id} answers it.
func (r *identityRig) storedAuthor(t *testing.T, comment string) model.Actor {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/comments/"+comment, nil, brokerTestPerson)
	if response.Code != http.StatusOK {
		t.Fatalf("read comment %s: status=%d body=%s", comment, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Comment struct {
			Author model.Actor `json:"author"`
		} `json:"comment"`
	}](t, response).Comment.Author
}

// page is one person's page as the reader signed in as login reads it.
func (r *identityRig) page(t *testing.T, person, login string) personNodes {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/people/"+url.PathEscape(person)+"/nodes", nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("read the page of %q as %s: status=%d body=%s", person, login, response.Code, response.Body.String())
	}
	return decodeBody[personNodes](t, response)
}

// pagePerson is the person a login names, as the person page read answers it.
func (r *identityRig) pagePerson(t *testing.T, login, reader string) string {
	t.Helper()
	return r.page(t, login, reader).Person
}

// aliasOf is the one person a session is an alias of.
func (r *identityRig) aliasOf(t *testing.T, session string) string {
	t.Helper()
	aliases := r.currentOut(t, graphstore.KindSession, session, graphstore.TupleAliasOf)
	if len(aliases) != 1 || aliases[0].ToKind != graphstore.KindPerson {
		t.Fatalf("the session %s is an alias of %+v, want one person", session, aliases)
	}
	return aliases[0].ToID
}

func (r *identityRig) currentOut(t *testing.T, kind, id, tuple string) []graphstore.Tuple {
	t.Helper()
	tuples, err := graphstore.CurrentOut(context.Background(), r.database.Pool, kind, id, []string{tuple})
	if err != nil {
		t.Fatalf("read the %s tuples out of %s %s: %v", tuple, kind, id, err)
	}
	return tuples
}

func (r *identityRig) currentIn(t *testing.T, kind, id, tuple string) []graphstore.Tuple {
	t.Helper()
	tuples, err := graphstore.CurrentIn(context.Background(), r.database.Pool, kind, id, []string{tuple})
	if err != nil {
		t.Fatalf("read the %s tuples into %s %s: %v", tuple, kind, id, err)
	}
	return tuples
}

// publish runs the real outbox until every event of the issue is published, with the claim store
// it reads a role's holder from naming the proved session: the record of what each batch
// delivered is the writer of the role's held_by tuple, the session's part_of it, and every
// delivered_to tuple.
func (r *identityRig) publish(t *testing.T, issue string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	holder := provedSessionHoldsTheRole{}
	go func() {
		defer close(stopped)
		outbox.New(outbox.Deps{
			Store: r.database, Publisher: acceptingPublisher{}, Broker: events.NewBroker(),
			Listen:     r.database.Pool.Listener,
			RoleClaims: holder, Sessions: holder,
		}).Run(ctx)
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var unpublished int
		if err := r.database.Pool.QueryRow(context.Background(), `
			select count(*) from events where issue_key = $1 and published_at is null
		`, issue).Scan(&unpublished); err != nil {
			t.Fatalf("count the unpublished events of %s: %v", issue, err)
		}
		if unpublished == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox left %d events of %s unpublished", unpublished, issue)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// provedSessionHoldsTheRole is the roles claim store and session lookup the outbox resolves a
// role route through here: every role is claimed by the session the broker's token proved, whose
// record was written just now, so the forward resolves to that live session.
type provedSessionHoldsTheRole struct{}

func (provedSessionHoldsTheRole) RoleClaim(context.Context, string) (envoystore.RoleClaim, error) {
	return envoystore.RoleClaim{HolderSessionID: brokerTestSession}, nil
}

func (provedSessionHoldsTheRole) Holder(context.Context, string) (session.SessionEntry, error) {
	return session.SessionEntry{SelfSubscribed: true, UpdatedAt: time.Now().UnixMilli()}, nil
}

func (provedSessionHoldsTheRole) EntryState(entry session.SessionEntry) session.State {
	return session.StateOf(entry, time.Now(), session.LiveWindow, session.GoneHorizon)
}

func (provedSessionHoldsTheRole) LastSeen(string) int64 { return time.Now().UnixMilli() }

// bearerTokenRequest sends one request under a personal token, the credential a person's own
// agent presents.
func bearerTokenRequest(t *testing.T, handler http.Handler, method, target, credential string, body any) *httptest.ResponseRecorder {
	t.Helper()
	request := brokerRequest(t, method, target, body, "Bearer "+credential, "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
