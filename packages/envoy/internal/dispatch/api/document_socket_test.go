package api

// document_socket_test.go holds the document socket's admission by the api package
// (document_socket.go): a broker-issued token opens a socket as the session it names, recorded as
// an API write with the same token records it; a cookie that resolves opens one whatever bearer
// rides beside it, and that bearer is never read; and a token's socket may write only where the
// edits route lets the same token write. Each row runs on newDocumentSocketTokenRig, the one server
// where a request carries a cookie and a token side by side. Every credential the socket refuses
// is document_socket_door_test.go's.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/testwait"
)

// countedServiceVerifier is a service-account token verifier (Deps.OIDC) that counts the tokens
// it is asked to verify.
type countedServiceVerifier struct {
	ServiceTokenVerifier
	calls atomic.Int64
}

func (v *countedServiceVerifier) Verify(ctx context.Context, raw string) (oidc.Claims, error) {
	v.calls.Add(1)
	return v.ServiceTokenVerifier.Verify(ctx, raw)
}

// serve carries one request through the rig's own server in process: the server its sockets are
// opened on, whose document service holds their rooms.
func (rig *documentSocketTokenRig) serve(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	rig.server.Config.Handler.ServeHTTP(response, request)
	return response
}

// asPerson sends method and path, with body as JSON, to the rig's server, signed in as the rig's
// person by her cookie.
func (rig *documentSocketTokenRig) asPerson(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	request := brokerRequest(t, method, path, body, "", "")
	request.AddCookie(rig.cookie)
	return rig.serve(request)
}

// issueSpec files an issue of project, titled title, with spec as its spec, as the rig's person on
// the rig's own server, and answers the spec's id.
func (rig *documentSocketTokenRig) issueSpec(t *testing.T, project, title, spec string) string {
	t.Helper()
	created := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]string{"project": project, "title": title, "spec": spec})
	if created.Code != http.StatusCreated {
		t.Fatalf("file %q in %s: status=%d body=%s", title, project, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created).PrimaryArtifactID
}

// presentingAt is what caller sends to open room's socket with token under the DPoP scheme: the
// token and a fresh proof of caller's key for the socket's own target.
func presentingAt(t *testing.T, caller *brokerCaller, room, token string) http.Header {
	t.Helper()
	return presentedHeaders(caller.presenting(t, http.MethodGet, "/ws/doc/"+room, token, nil))
}

// tokenMessageScope sends peer's room the token message the dashboard's editor sends once its
// socket opens, and answers the scope the room grants it: read-write or readonly. docstest's
// Authenticate waits under a bound of its own, so it is handed one no run reaches, and the answer
// is waited for here instead, whose one bound is the test binary's deadline (testwait.Receive).
func tokenMessageScope(t *testing.T, peer *syncedPeer) string {
	t.Helper()
	type answer struct {
		scope string
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		scope, err := peer.connection().Authenticate(strconv.Itoa(pmdoc.SchemaVersion()), math.MaxInt64)
		answered <- answer{scope, err}
	}()
	reply := testwait.Receive(t, "the room's answer to the socket's token message", answered)
	if reply.err != nil {
		t.Fatalf("the socket's token message: %v", reply.err)
	}
	return reply.scope
}

// requireEditStored types text into peer's document, as a keystroke does, and waits until the
// store holds it in room's stored updates. Its one bound is the test binary's deadline
// (testwait.UntilPolling).
func requireEditStored(t *testing.T, database *store.Store, peer *syncedPeer, room, text string) {
	t.Helper()
	peer.appendParagraph(t, text)
	testwait.UntilPolling(t, 20*time.Millisecond, fmt.Sprintf("the edit %q to be stored (the stored document)", text), func() (string, bool) {
		stored := storedDocumentText(t, database, room)
		return stored, strings.Contains(stored, text)
	})
}

// requireEditDropped types text into peer's document, as a keystroke does, and fails t unless the
// room dropped it: the room answers one connection's frames in order, so its whole document, asked
// for on the same connection after the edit, is answered once the room has handled that edit, and
// it must not hold the text. Nor may room's stored updates.
func requireEditDropped(t *testing.T, database *store.Store, peer *syncedPeer, room, text string) {
	t.Helper()
	connection := peer.connection()
	for len(connection.Answers) > 0 {
		<-connection.Answers
	}
	peer.appendParagraph(t, text)
	if err := connection.AskForDocument(); err != nil {
		t.Fatalf("ask the room for its document after the edit: %v", err)
	}
	state := testwait.Receive(t, "the room's answer to a request for its document sent after the edit", connection.Answers)
	answered := crdt.New()
	if err := crdt.ApplyUpdateV1(answered, state, nil); err != nil {
		t.Fatalf("decode the room's document: %v", err)
	}
	if held := renderDocument(t, answered); strings.Contains(held, text) {
		t.Fatalf("the room took the edit %q from a read-only socket: it holds %q", text, held)
	}
	if stored := storedDocumentText(t, database, room); strings.Contains(stored, text) {
		t.Fatalf("the edit %q from a read-only socket was stored: %q", text, stored)
	}
}

// pendingAuthorsOf is the author the document service recorded for each edit of room no version
// holds yet (doc_pending_authors), in the order the edits were stored: who it credits with what its
// connections sent.
func pendingAuthorsOf(t *testing.T, database *store.Store, room string) []model.Actor {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select actor from doc_pending_authors where artifact_id = $1 order by written_through
	`, room)
	if err != nil {
		t.Fatalf("read the pending authors of %s: %v", room, err)
	}
	encoded, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read the pending authors of %s: %v", room, err)
	}
	authors := make([]model.Actor, 0, len(encoded))
	for _, actor := range encoded {
		var author model.Actor
		if err := json.Unmarshal(actor, &author); err != nil {
			t.Fatalf("decode a pending author of %s: %v", room, err)
		}
		authors = append(authors, author)
	}
	return authors
}

// A broker-issued token opens a document socket with no cookie, as the session it names, and the
// connection is recorded as an API write with the same token records its writer. Three tokens may
// write a document of project BRK: one an operator's entry scopes to BRK (liveNamedToken), a dev
// box session's acting as its person (personScopedTokenAnswer), and an account entry's naming its
// person and no project. Each writes the document first through POST
// /api/v1/artifacts/{id}/edits on the same server, and the version that write stores names its
// author: the session, its owner, Proven, and the act naming the session the broker proved. Then
// the socket opens with the same token and a proof of its own: 101, its token message is answered
// read-write, an edit it sends is stored, and the author the document service records for that
// edit (doc_pending_authors) is that very actor, which graphstore reads as the token's person
// (ActingCredential).
//
// Controls: the mount with no decideBrokerToken layer, the document service asking identity.Login
// alone, refuses the socket 401 and the row fails at its open; the connection's actor handed as a
// bare session with no owner, Proven or act fails on the stored author.
func TestADocumentSocketAdmitsABrokerTokenAsItsSession(t *testing.T) {
	t.Parallel()
	rig := newDocumentSocketTokenRig(t, time.Hour, nil)
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]string{"key": brokerTestProject, "name": "Broker"}); created.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", brokerTestProject, created.Code, created.Body.String())
	}
	// Each case's issue is written in words no other case's shares, so the near-copy gate on
	// POST /api/v1/issues reads no case's issue as a copy of an earlier one's.
	for index, token := range []struct {
		name                 string
		title, first, second string
		answer               func(jkt string) ciphertoken.Introspection
	}{
		{"a token of the document's project", "Harbor buoy maintenance", "Buoys bob near the jetty.", "Lanterns glow at dusk.", liveNamedToken},
		{"a token acting as its person", "Alpine trail markers", "Cairns guide hikers upward.", "Snowfields glare at noon.", personScopedTokenAnswer},
		{"a named token naming its person and no project", "Desert cactus census", "Saguaros store rainwater.", "Coyotes howl past midnight.",
			func(jkt string) ciphertoken.Introspection {
				answer := liveNamedToken(jkt)
				answer.Projects = nil
				return answer
			}},
	} {
		t.Run(token.name, func(t *testing.T) {
			room := rig.issueSpec(t, brokerTestProject, token.title, token.first)
			caller := newBrokerCaller(t)
			value := fmt.Sprintf("a-token-that-opens-its-own-document-%d", index)
			rig.tokens.broker.Says(value, token.answer(caller.jkt))

			edited := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/artifacts/"+room+"/edits", value, map[string]any{
				"ops": []map[string]string{{"op": "replace", "find": token.first, "with": token.second}},
			}))
			if edited.Code != http.StatusOK {
				t.Fatalf("the API write with the token: status=%d body=%s", edited.Code, edited.Body.String())
			}
			waitForArtifactVersion(t, rig.tokens.handler, room, 2)
			var encoded []byte
			if err := rig.tokens.database.Pool.QueryRow(context.Background(), `
				select authors from artifact_versions where artifact_id = $1 and number = 2
			`, room).Scan(&encoded); err != nil {
				t.Fatalf("read the API write's version: %v", err)
			}
			var written []model.Actor
			if err := json.Unmarshal(encoded, &written); err != nil || len(written) != 1 {
				t.Fatalf("the API write's version lists the authors %s (%v), want its writer alone", encoded, err)
			}
			requireSessionProven(t, "the API write's author", written[0], true)

			peer := rig.openDocument(t, room, false, presentingAt(t, caller, room, value))
			if scope := tokenMessageScope(t, peer); scope != "read-write" {
				t.Fatalf("the token's socket is answered %q, want read-write: the token may write the document", scope)
			}
			requireEditStored(t, rig.tokens.database, peer, room, "Typed through the token's own socket.")
			stored := pendingAuthorsOf(t, rig.tokens.database, room)
			if len(stored) != 1 || !reflect.DeepEqual(stored[0], written[0]) {
				t.Fatalf("the socket's edit is recorded as %+v, want the one author %+v the API write with the same token recorded", stored, written[0])
			}
			encoded, _ = graphstore.ActingCredential(stored[0]).([]byte)
			var proved struct{ Kind, ID string }
			if err := json.Unmarshal(encoded, &proved); err != nil || proved.Kind != "person" || proved.ID != brokerTestPerson {
				t.Fatalf("graphstore reads the socket's edit as authored by %s (%v), want the token's person %s", encoded, err, brokerTestPerson)
			}
		})
	}
}

// A request that presents a broker-issued token is decided by the token, and a cookie beside it is
// not read, as on the API. A sound token acting as its person, beside the cookie of another person
// (the rig's alice@d.example, where the token names alice), opens the socket read-write as the
// token's session: the edit it sends is recorded as that session, its owner the token's person. A
// token the broker does not hold live, beside a cookie that opens the same room alone
// (openedDoor), is refused 401 BROKER_TOKEN_NOT_ACTIVE, as JSON, and no socket opens.
//
// Control: the cookie read before the presented token opens the first socket as the cookie's
// person, and the row fails on the recorded author.
func TestADocumentSocketTakesAPresentedTokenOverACookie(t *testing.T) {
	t.Parallel()
	rig := newDocumentSocketTokenRig(t, time.Hour, nil)
	caller := newBrokerCaller(t)

	t.Run("a sound token beside the cookie of another person", func(t *testing.T) {
		const token = "a-sound-token-beside-a-cookie"
		rig.tokens.broker.Says(token, personScopedTokenAnswer(caller.jkt))
		peer := rig.openDocument(t, rig.artifactID, true, presentingAt(t, caller, rig.artifactID, token))
		if scope := tokenMessageScope(t, peer); scope != "read-write" {
			t.Fatalf("the socket is answered %q, want read-write", scope)
		}
		requireEditStored(t, rig.tokens.database, peer, rig.artifactID, "Typed by the token's session beside a cookie.")
		stored := pendingAuthorsOf(t, rig.tokens.database, rig.artifactID)
		if len(stored) != 1 {
			t.Fatalf("the socket's edit is recorded as %+v, want one author", stored)
		}
		requireSessionProven(t, "the author of the socket's edit", stored[0], true)
		if stored[0].Owner == nil || *stored[0].Owner != brokerTestPerson {
			t.Fatalf("the socket's edit is recorded for the owner %v, want the token's person %s", stored[0].Owner, brokerTestPerson)
		}
	})

	t.Run("a token the broker does not hold live beside a sound cookie", func(t *testing.T) {
		door := openedDoor(t, rig.server, rig.artifactID, rig.cookie)
		headers := door.presenting(t, caller, "a-token-the-broker-never-issued")
		headers.Set("Cookie", rig.cookie.Name+"="+rig.cookie.Value)
		answer := door.knock(t, headers)
		if answer.opened {
			t.Fatalf("the socket opened (%d) on the cookie beside a token the broker does not hold live, want 401 BROKER_TOKEN_NOT_ACTIVE", answer.status)
		}
		var refusal refusalAnswer
		if err := json.Unmarshal([]byte(answer.body), &refusal); err != nil || answer.status != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_ACTIVE" {
			t.Fatalf("answered %d %s, want 401 BROKER_TOKEN_NOT_ACTIVE as JSON: the token decides", answer.status, strings.TrimSpace(answer.body))
		}
	})
}

// A token's socket is named on its access-log line as an API request with the same token is
// named. A person's token's line names the session it acts as and the token's person beside it:
// caller_kind=session, caller=<session>, credential=broker-token:<person>, as an API write logs. A
// service's token and an account entry's token naming no person open a read-only socket, and
// their line names the service, as an API read with the token logs: caller_kind=broker-token,
// caller=service:<service>, and no credential field. Each socket opens with the token alone and
// closes, and its line is the one access-log line its path gains.
//
// Controls: the token's caller noted as a person (accesslog.KindUser) fails the first case; the
// no-person line written as a session's (bearerSessionActor's note left standing) fails the other
// two.
func TestADocumentSocketsAccessLogNamesItsTokenCaller(t *testing.T) {
	rig := newDocumentSocketTokenRig(t, time.Hour, nil)
	logged := captureLog(t)
	caller := newBrokerCaller(t)
	path := "path=/ws/doc/" + rig.artifactID
	socketLines := func(t *testing.T) []string {
		t.Helper()
		var lines []string
		for _, line := range accessLines(t, logged) {
			if strings.Contains(line, path) {
				lines = append(lines, line)
			}
		}
		return lines
	}
	for index, token := range []struct {
		name   string
		answer func(jkt string) ciphertoken.Introspection
		want   []string
		person bool
	}{
		{"a token that names a person", liveNamedToken,
			[]string{"caller_kind=session", "caller=" + brokerTestSession + " ", "credential=broker-token:" + brokerTestPerson}, true},
		{"a service's token", serviceOnlyToken,
			[]string{"caller_kind=broker-token", "caller=service:" + brokerTestService + " "}, false},
		{"an account entry's token naming no person", personlessAccountToken,
			[]string{"caller_kind=broker-token", "caller=service:" + brokerTestAccount + " "}, false},
	} {
		t.Run(token.name, func(t *testing.T) {
			value := fmt.Sprintf("a-token-the-access-log-names-%d", index)
			rig.tokens.broker.Says(value, token.answer(caller.jkt))
			before := len(socketLines(t))
			peer := rig.openDocument(t, rig.artifactID, false, presentingAt(t, caller, rig.artifactID, value))
			peer.closeAndWait(t)
			lines := socketLines(t)
			if len(lines) != before+1 {
				t.Fatalf("the socket's request wrote %d access-log lines, want one:\n%s", len(lines)-before, strings.Join(lines[before:], "\n"))
			}
			line := lines[before]
			for _, want := range token.want {
				if !strings.Contains(line, want) {
					t.Errorf("the socket's access-log line %q does not carry %q", line, want)
				}
			}
			if !token.person && strings.Contains(line, "credential=") {
				t.Errorf("the socket's access-log line %q names a credential, want the service the token names as its caller and nothing else", line)
			}
		})
	}
}

// A cookie that resolves opens the document socket as its person, read-write, whatever bearer that
// is no DPoP presentation rides beside it, and the bearer is never read. Beside the cookie in turn:
// the shared bearer, a personal token that writes, a read-only personal token, and a service-account
// token, a JWT on a server that verifies them. Each socket's token message is answered read-write,
// and its edit is stored and recorded as the cookie's person. Across all four no statement the
// server's pool runs looks a personal token up (agentTokenLookup among rig.statements), and the
// service-account verifier is asked nothing. The records are this server's own: the API on the
// same server, sent the personal token and the service-account token, looks the one up and
// verifies the other, and both records show it.
//
// Control: the mount resolving a non-DPoP bearer, through optionalActor or through
// refuseReadOnlyToken mounted on every request as on GET /ws/live, makes a statement that reads
// agent_tokens and agent_read_tokens, and the row fails on that lookup (the read-only personal
// token's socket is refused 403 READ_ONLY_TOKEN there too).
func TestADocumentSocketNeverReadsABearerThatIsNotDPoP(t *testing.T) {
	t.Parallel()
	rig := newDocumentSocketTokenRig(t, time.Hour, nil, verifyingServiceAccounts)
	writer := mintPersonalToken(t, rig.tokens.handler, brokerTestPerson, "a writer", []string{model.ScopePerson}).Token
	reader := readOnlyPersonalTokens(t, rig.tokens)[0].token
	serviceAccount := rig.serviceTokens.mint(t, serviceTokenAudience)
	person := model.Actor{Kind: "user", ID: documentSocketPerson, Proven: true}

	sockets := rig.statements.count()
	for index, bearer := range []struct{ name, authorization string }{
		{"the shared bearer", "Bearer agent-token"},
		{"a personal token that writes", "Bearer " + writer},
		{"a read-only personal token", "Bearer " + reader},
		{"a service-account token on a server that verifies them", "Bearer " + serviceAccount},
	} {
		t.Run(bearer.name, func(t *testing.T) {
			peer := rig.openDocument(t, rig.artifactID, true, http.Header{"Authorization": {bearer.authorization}})
			if scope := tokenMessageScope(t, peer); scope != "read-write" {
				t.Fatalf("the cookie's socket is answered %q, want read-write", scope)
			}
			requireEditStored(t, rig.tokens.database, peer, rig.artifactID, fmt.Sprintf("Typed by the cookie's person beside bearer %d.", index))
			for _, author := range pendingAuthorsOf(t, rig.tokens.database, rig.artifactID) {
				if !reflect.DeepEqual(author, person) {
					t.Fatalf("an edit is recorded as %+v, want the cookie's person %+v", author, person)
				}
			}
			peer.closeAndWait(t)
		})
	}
	if holdsStatement(rig.statements.since(sockets), agentTokenLookup) {
		t.Errorf("the document sockets looked a personal token up, want no lookup: no bearer beside a cookie is read")
	}
	if verified := rig.serviceVerifies.calls.Load(); verified != 0 {
		t.Errorf("the document sockets asked the service-account verifier %d times, want none", verified)
	}

	api := rig.statements.count()
	for _, authorization := range []string{"Bearer " + writer, "Bearer " + serviceAccount} {
		if whoami := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, authorization, "")); whoami.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/whoami on the same server: status=%d body=%s", whoami.Code, whoami.Body.String())
		}
	}
	if looked := holdsStatement(rig.statements.since(api), agentTokenLookup); !looked || rig.serviceVerifies.calls.Load() == 0 {
		t.Fatalf("the API on the same server read the two bearers with a personal-token lookup %t and %d verifications, want a lookup and at least one verification: the records do not see this server's reads",
			looked, rig.serviceVerifies.calls.Load())
	}
}

// A token that names no person opens a read-only socket, whichever shape it is: a service's token
// (serviceOnlyToken) and an account entry's token naming no person and no project
// (personlessAccountToken), which the edits route lets write under its account. On an open issue's
// spec, a document no project limit or spec-writer rule keeps either from, each socket's token
// message is answered readonly and an edit it sends is neither taken nor stored.
//
// Controls: every broker token's socket admitted writable fails both cases, since their edit is
// stored; the account entry's token given the edits route's answer (actsAsItsAccount) fails the
// second.
func TestADocumentSocketOpensReadOnlyForATokenThatNamesNoPerson(t *testing.T) {
	t.Parallel()
	rig := newDocumentSocketTokenRig(t, time.Hour, nil)
	caller := newBrokerCaller(t)
	for index, token := range []struct {
		name   string
		answer func(jkt string) ciphertoken.Introspection
	}{
		{"a service's token", serviceOnlyToken},
		{"an account entry's token naming no person", personlessAccountToken},
	} {
		t.Run(token.name, func(t *testing.T) {
			value := fmt.Sprintf("a-token-naming-no-person-%d", index)
			rig.tokens.broker.Says(value, token.answer(caller.jkt))
			peer := rig.openDocument(t, rig.artifactID, false, presentingAt(t, caller, rig.artifactID, value))
			if scope := tokenMessageScope(t, peer); scope != "readonly" {
				t.Fatalf("the socket is answered %q, want readonly: the token names no person", scope)
			}
			requireEditDropped(t, rig.tokens.database, peer, rig.artifactID, fmt.Sprintf("Typed under a token naming no person, %d.", index))
		})
	}
}

// A token an operator's entry scoped to project BRK opens a read-only socket on a document of
// another project, TEST, as the edits route refuses it that write: its token message is answered
// readonly and its edit is neither taken nor stored.
//
// Controls: every broker token's socket admitted writable, or the project check left out of the
// write decision, fails the row, since the edit is stored.
func TestADocumentSocketOpensReadOnlyForATokenOfAnotherProject(t *testing.T) {
	t.Parallel()
	rig := newDocumentSocketTokenRig(t, time.Hour, nil)
	caller := newBrokerCaller(t)
	const token = "a-token-of-another-project"
	rig.tokens.broker.Says(token, liveNamedToken(caller.jkt))
	peer := rig.openDocument(t, rig.artifactID, false, presentingAt(t, caller, rig.artifactID, token))
	if scope := tokenMessageScope(t, peer); scope != "readonly" {
		t.Fatalf("the socket is answered %q, want readonly: the token may write %s and the document is of TEST", scope, brokerTestProject)
	}
	requireEditDropped(t, rig.tokens.database, peer, rig.artifactID, "Typed under another project's token.")
}

// A token scoped to project BRK opens a read-only socket on a document of no project, as the edits
// route fails closed on it. The document is stored directly: a session owns it (session_id set, no
// issue and no project, the shape the artifacts_one_owner check of migration 0071 allows), since a
// session's conversation holds only files and pictures through the API (owner.go), and its text
// is the rig's spec's, its stored updates copied, since an editor writes into a document that
// holds a block. Its token message is answered readonly, and its edit is neither taken nor stored.
//
// Controls: every broker token's socket admitted writable, or a project token's write allowed on a
// document of no project, fails the row, since the edit is stored.
func TestADocumentSocketOpensReadOnlyForAProjectTokenOnADocumentOfNoProject(t *testing.T) {
	t.Parallel()
	rig := newDocumentSocketTokenRig(t, time.Hour, nil)
	var room string
	if err := rig.tokens.database.Pool.QueryRow(context.Background(), `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ($1, 'notes', 'notes.md', 'doc', jsonb_build_object('kind', 'session', 'id', $1::text))
		returning id::text
	`, conversationSession).Scan(&room); err != nil {
		t.Fatalf("store a document of no project: %v", err)
	}
	if _, err := rig.tokens.database.Pool.Exec(context.Background(), `
		insert into doc_updates (artifact_id, version, update, content_changed)
		select $1::uuid, version, update, content_changed from doc_updates where artifact_id = $2::uuid
	`, room, rig.artifactID); err != nil {
		t.Fatalf("store the document's text: %v", err)
	}
	caller := newBrokerCaller(t)
	const token = "a-project-token-on-a-document-of-no-project"
	rig.tokens.broker.Says(token, liveNamedToken(caller.jkt))
	peer := rig.openDocument(t, room, false, presentingAt(t, caller, room, token))
	if scope := tokenMessageScope(t, peer); scope != "readonly" {
		t.Fatalf("the socket is answered %q, want readonly: the token may write %s and the document is of no project", scope, brokerTestProject)
	}
	requireEditDropped(t, rig.tokens.database, peer, room, "Typed under a project token on a document of no project.")
}

// On an issue Legion runs, in a project a role-holder publication marked, the spec is written only
// by a person or by an architect session of the issue's chain (specWriterRefusesDocument), and a
// token's socket may write it only where that rule lets its token write. The issue and its role
// holders are the spec-writer rig's (specWriterServer.tree, publishRoles): R, labelled legion, whose
// architect and implementer are sessions of their own. A token naming R's implementer session, of
// R's project, opens R's spec readonly and its edit is neither taken nor stored; the token naming
// R's architect session opens it read-write and its edit is stored.
//
// Controls: every broker token's socket admitted writable, or the spec-writer rule left out of the
// write decision, fails the first case, since the implementer's edit is stored.
func TestADocumentSocketOpensReadOnlyWhereTheSpecWriterRuleRefusesItsToken(t *testing.T) {
	t.Parallel()
	rig := newDocumentSocketTokenRig(t, time.Hour, nil)
	tree := (&specWriterServer{handler: rig.tokens.handler, database: rig.tokens.database, served: rig.tokens.served}).tree(t, specWriterRigShape{})
	tree.publishRoles(t)
	spec := tree.r.documents["spec.md"].id
	// The spec was written through the broker rig's server, whose document service settles it; the
	// sockets below open on the rig's own server once nothing of it is left to settle.
	awaitSettled(t, rig.tokens.database, spec)
	caller := newBrokerCaller(t)
	for index, session := range []struct {
		name, session, scope string
		stored               bool
	}{
		{"the token of a session that is no architect of the issue", tree.rImplementer, "readonly", false},
		{"the token of the issue's architect session", tree.rArchitect, "read-write", true},
	} {
		t.Run(session.name, func(t *testing.T) {
			value := fmt.Sprintf("a-token-of-%s", session.session)
			answer := liveNamedToken(caller.jkt)
			answer.Session, answer.Projects = session.session, []string{tree.project}
			rig.tokens.broker.Says(value, answer)
			peer := rig.openDocument(t, spec, false, presentingAt(t, caller, spec, value))
			if scope := tokenMessageScope(t, peer); scope != session.scope {
				t.Fatalf("the socket of %s on %s's spec is answered %q, want %s", session.session, tree.r.key, scope, session.scope)
			}
			text := fmt.Sprintf("Typed into the spec under a session's token, %d.", index)
			if session.stored {
				requireEditStored(t, rig.tokens.database, peer, spec, text)
				return
			}
			requireEditDropped(t, rig.tokens.database, peer, spec, text)
		})
	}
}
