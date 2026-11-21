package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// A member the hourly confirmation restricts loses what a restricted member does not read at once,
// not at their next sign-in: the confirmation's write of people.member_kind announces itself on
// store.AccessChangedChannel as it commits, which every process's room sync and live delivery hub
// hear, as they hear an access list's change.

// kindChangeEmail is a Workspace member's address on a domain the Workspace does not list, so an
// account that leaves the Workspace keeps it as a restricted member while some project names it.
const kindChangeEmail = "alice@second.example"

// restrictableRig is the server main serves a member of example.com's Workspace signed in with
// Google under outside sign-in (identity.OutsideRule), at kindChangeEmail: cookie identity
// confirming the membership with a fake Google at least hourly, the API, the live socket and the
// document socket Register mounts, and the document service's room sync on a listener of its own.
// PUB is a public project whose list does not name the member, and LISTED one whose list does.
type restrictableRig struct {
	database *store.Store
	pool     *oidctest.Issuer
	mux      *http.ServeMux
	server   *httptest.Server
	sockets  *servedSockets
	cookie   *http.Cookie
}

// newRestrictableRig signs the member in as a full member, as the sign-in callback does, with the
// sockets' heartbeat set to heartbeat.
func newRestrictableRig(t *testing.T, heartbeat time.Duration) *restrictableRig {
	t.Helper()
	ctx := context.Background()
	rig := &restrictableRig{pool: oidctest.New(t), database: storetest.Open(t)}
	rig.pool.EnableCodeFlow(rig.pool.PublishKey(t, "signing-key"), "dispatch-client", "dispatch-secret")
	// A domain-mode flow, as main builds it for DISPATCH_SIGNIN_DOMAIN, reads email and hd.
	flow := oidc.NewCodeFlow(rig.pool.URL(), "dispatch-client", "dispatch-secret", "example.com")
	const redirect = "https://dispatch.example/auth/callback"
	rig.pool.SignInAs(map[string]any{"sub": "member-subject", "email": kindChangeEmail, "email_verified": true, "hd": "example.com"})
	authURL, err := flow.AuthURL(ctx, redirect, "state", "nonce", oidc.AuthOptions{DomainHint: true})
	if err != nil {
		t.Fatalf("start the sign-in: %v", err)
	}
	code, _ := rig.pool.Authorize(t, authURL)
	signIn, err := flow.Exchange(ctx, redirect, code, "nonce")
	if err != nil {
		t.Fatalf("exchange the sign-in code: %v", err)
	}
	people := store.NewPgPeopleStore(rig.database.Pool, testSigningKey, nil)
	sessions := store.NewPgSessionStore(rig.database.Pool)
	if err := people.SignInSubject(ctx, kindChangeEmail, "member-subject", signIn.RefreshToken, "", model.MemberFull); err != nil {
		t.Fatalf("record the sign-in: %v", err)
	}
	generation, err := sessions.EnsureSession(ctx, kindChangeEmail)
	if err != nil {
		t.Fatalf("establish the session: %v", err)
	}
	if rig.cookie, err = http.ParseSetCookie(auth.IssueSessionCookie(kindChangeEmail, generation, testSigningKey, false)); err != nil {
		t.Fatalf("parse the session cookie: %v", err)
	}
	broker := events.NewBroker()
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, owner) values
			('PUB', 'Public', 'carol@example.com', 'carol@example.com'),
			('LISTED', 'Listed', 'carol@example.com', 'carol@example.com')
	`); err != nil {
		t.Fatalf("seed PUB and LISTED: %v", err)
	}
	listOn(t, rig.database, broker, "LISTED", kindChangeEmail)
	person := identity.CookieIdentity{
		SigningKey: testSigningKey,
		Sessions:   sessions,
		Membership: &identity.Membership{People: people, SignIn: flow, Rule: identity.OutsideRule{
			DomainRule: identity.DomainRule{Domain: "example.com"},
			People: identity.ListerFunc(func(ctx context.Context, email string) (bool, error) {
				return access.Listed(ctx, rig.database.Pool, email)
			}),
		}},
	}
	documents := docs.New(docs.Deps{Store: rig.database, Events: broker, Identity: person, Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	runDocumentRoomSync(t, rig.database, documents)
	deps, err := NewDeps(DepsInput{
		Store: rig.database, Identity: person, AgentTokens: sharedAgentTokens(t, "agent-token"), Docs: documents, Events: broker,
		StreamHeartbeat: heartbeat,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	rig.mux = http.NewServeMux()
	t.Cleanup(Register(rig.mux, t.Context(), deps).Wait)
	rig.sockets = &servedSockets{finished: make(map[string]chan struct{})}
	rig.server = httptest.NewServer(rig.sockets.serve(rig.mux.ServeHTTP))
	t.Cleanup(rig.server.Close)
	return rig
}

// leaveTheWorkspace has Google move the member's account out of the Workspace, so its next refresh
// carries no hd, and dates their last confirmation two hours back, so the next request or
// heartbeat that resolves their cookie runs the hourly confirmation: the outside rule keeps them
// signed in as a restricted member, since LISTED's list names them.
func (rig *restrictableRig) leaveTheWorkspace(t *testing.T) {
	t.Helper()
	rig.pool.UpdateGrants(func(claims map[string]any) { delete(claims, "hd") })
	if _, err := rig.database.Pool.Exec(context.Background(), `update people set confirmed_at = now() - interval '2 hours' where email = $1`, kindChangeEmail); err != nil {
		t.Fatalf("date the confirmation two hours back: %v", err)
	}
}

// requireStillSignedInRestricted requires the member's cookie to answer GET /api/v1/whoami 200 as
// a restricted member, and people to record them so.
func (rig *restrictableRig) requireStillSignedInRestricted(t *testing.T) {
	t.Helper()
	if kind := storedKind(t, rig.database, kindChangeEmail); kind != string(model.MemberRestricted) {
		t.Fatalf("people.member_kind of %s after the confirmation = %q, want restricted", kindChangeEmail, kind)
	}
	whoami, err := http.NewRequest(http.MethodGet, rig.server.URL+"/api/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	whoami.AddCookie(rig.cookie)
	answered, err := http.DefaultClient.Do(whoami)
	if err != nil {
		t.Fatalf("GET /api/v1/whoami: %v", err)
	}
	body, _ := io.ReadAll(answered.Body)
	_ = answered.Body.Close()
	if answered.StatusCode != http.StatusOK || !strings.Contains(string(body), `"member_kind":"restricted"`) {
		t.Fatalf("GET /api/v1/whoami with the cookie after the confirmation: %d %s, want 200 as a restricted member, still signed in", answered.StatusCode, body)
	}
}

// A full member holds a document socket open on a document of PUB, a public project whose list
// does not name them; LISTED's list does. Google then moves their account out of the Workspace,
// and the hourly confirmation that the socket's heartbeat runs (whileCallerResolves, by the cookie
// alone) keeps them signed in as a restricted member, who reads LISTED and not PUB. Their cookie
// still resolves, so no heartbeat closes the socket; the confirmation's announcement does, with
// the close code of a socket whose person no longer reads its room (4412), and the reconnect is
// refused.
//
// Control: the confirmation writes the member kind and announces nothing. The socket stays open
// past the window, and the test fails saying so.
func TestADocumentSocketClosesOnceTheHourlyConfirmationRestrictsItsPerson(t *testing.T) {
	const heartbeat = 250 * time.Millisecond
	rig := newRestrictableRig(t, heartbeat)
	encoded, err := json.Marshal(map[string]string{"project": "PUB", "title": "A public document", "spec": "The spec."})
	if err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRequest(http.MethodPost, "/api/v1/issues", bytes.NewReader(encoded))
	create.AddCookie(rig.cookie)
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	rig.mux.ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue of PUB as the full member: status=%d body=%s", created.Code, created.Body.String())
	}
	artifactID := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created).PrimaryArtifactID
	peer := &syncedPeer{
		wsURL: "ws" + strings.TrimPrefix(rig.server.URL, "http") + "/ws/doc/" + artifactID, sockets: rig.sockets,
		headers: http.Header{"Cookie": []string{rig.cookie.Name + "=" + rig.cookie.Value}}, artifactID: artifactID, doc: crdt.New(),
	}
	peer.connect(t)
	t.Cleanup(peer.close)

	// While they are a full member the socket outlives several heartbeats and stays writable.
	time.Sleep(4 * heartbeat)
	peer.appendParagraph(t, "typed while a full member")
	peer.barrier(t)

	rig.leaveTheWorkspace(t)
	lapsed := time.Now()
	const window = 10 * time.Second
	select {
	case <-peer.connection().Ended:
		t.Logf("the document socket closed %s after the confirmation lapsed (heartbeat %s)", time.Since(lapsed).Round(time.Millisecond), heartbeat)
	case <-time.After(window):
		t.Fatalf("the document socket of PUB stayed open %s after the hourly confirmation lapsed (member_kind %q, %d refreshes): the confirmation that restricted its person closed nothing",
			window, storedKind(t, rig.database, kindChangeEmail), rig.pool.Refreshes())
	}
	if code := peer.connection().CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Errorf("the document socket closed with %d, want %d, the close of a socket whose person no longer reads its room", code, contracts.DocumentAdmissionRevokedCloseCode)
	}
	rig.requireStillSignedInRestricted(t)
	connection, response, err := gws.DefaultDialer.Dial(peer.wsURL, peer.headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("the restricted member's reconnect to PUB's document: response=%#v err=%v, want it refused", response, err)
	}
}

// A full member's live socket carries an events subscription, which reads its reader again only at
// an event that can move what it sees (movesSight); a change of member kind appends none. So once
// the hourly confirmation, which the socket's heartbeat runs, restricts them, the heartbeat finds
// their member kind changed and closes the socket with 1008, as it does for a token whose list
// narrowed: the page opens a new one, which reads LISTED and not PUB. Until then the socket outlives
// several heartbeats.
//
// Control: the heartbeat compares no member kind. The socket stays open, carrying the full
// member's reader, and the test fails waiting for the close.
func TestALiveSocketClosesOnceTheHourlyConfirmationRestrictsItsPerson(t *testing.T) {
	rig := newRestrictableRig(t, time.Second)
	header := http.Header{"Cookie": []string{rig.cookie.Name + "=" + rig.cookie.Value}}
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(rig.server.URL, "http")+"/ws/live", header).servedBy(rig.database).read()
	subscribeEvents(t, client, "events", nil)
	time.Sleep(3 * time.Second)
	if refreshes := rig.pool.Refreshes(); refreshes != 0 {
		t.Fatalf("the fake Google was asked %d times within the hour, want 0", refreshes)
	}

	rig.leaveTheWorkspace(t)
	if closed := client.closeError(t, 10*time.Second); closed.Code != gws.ClosePolicyViolation {
		t.Fatalf("the live socket closed with %d %q once the hourly confirmation restricted its person, want 1008", closed.Code, closed.Text)
	}
	rig.requireStillSignedInRestricted(t)
}

// A live connection whose open-time resolution failed while the handler's own succeeded (a pool
// with no connection free, a broker that did not answer) has no person to hold to a member kind and
// no grant to compare, so it would outlive any change of its person's kind. It closes at its first
// heartbeat instead, and the page opens a new one. The request presents a broker-issued token, the
// one caller whose open asks the socket's own resolution (resolves) rather than resolving the
// request (withCallerResolved); that resolution fails once, at the open, and answers a person, with
// a grant equal to the open's none, every time after, so nothing but the failed open can close the
// connection.
//
// Control: the open's error dropped for a broker token (admitted, _, _ = resolves(check)). The
// connection then reads no member kind, its grant compares nil with nil, and it outlives the
// window. The earlier form of this test presented no credential, so its open took the request's
// resolution, which failed for want of an identity provider, and it passed under this control.
func TestALiveConnectionWhoseOpenResolutionFailedClosesAtItsFirstHeartbeat(t *testing.T) {
	var resolutions atomic.Int32
	resolves := func(*http.Request) (model.Actor, bool, error) {
		if resolutions.Add(1) == 1 {
			return model.Actor{}, false, errors.New("no connection was free")
		}
		return model.Actor{Kind: "user", ID: kindChangeEmail}, false, nil
	}
	served := directServer(Deps{StreamHeartbeat: 20 * time.Millisecond})
	ended := make(chan bool, 1)
	connection := served.whileCallerResolves(resolves, true, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			ended <- true
		case <-time.After(2 * time.Second):
			ended <- false
		}
	})
	request := httptest.NewRequest(http.MethodGet, "/ws/live", nil)
	request.Header.Set("Authorization", "DPoP "+strings.Repeat("b", 43))
	if _, presented := ciphertoken.Presented(request); !presented {
		t.Fatal("control: the request presents no broker token, so its open would not ask resolves")
	}
	connection.ServeHTTP(httptest.NewRecorder(), request)
	if !<-ended {
		t.Fatalf("a live connection whose open-time resolution failed was still open 2s later, after %d resolutions: nothing held it to its person's member kind", resolutions.Load())
	}
}

// The open of a live connection that presents no broker token resolves its caller once, on the
// request, for the handler and the heartbeat both (withCallerResolved), and does not ask the
// socket's own resolution, which the test above fails. A failure of that one resolution leaves the
// connection as little to hold to, so it closes at its first heartbeat too: here a cookie whose
// identity provider fails the open's read and answers every later one, and a personal token the
// open's read finds no row of. The heartbeat's own resolution answers the person every time, with
// no grant, so nothing but the failed open can close the connection.
//
// Control: the open's error dropped for these callers (admitted, _, _ = s.optionalActor). The
// connection then reads no member kind, compares nil grants, and outlives the window.
func TestALiveConnectionWhoseCookieOrTokenOpenFailedClosesAtItsFirstHeartbeat(t *testing.T) {
	resolves := func(*http.Request) (model.Actor, bool, error) {
		return model.Actor{Kind: "user", ID: kindChangeEmail}, false, nil
	}
	database := storetest.Open(t)
	for _, c := range []struct {
		name    string
		deps    Deps
		request func() *http.Request
	}{
		{
			name: "a cookie whose identity provider failed the open's read",
			deps: Deps{Identity: &failingOnceIdentity{login: kindChangeEmail}},
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/ws/live", nil)
			},
		},
		{
			name: "a personal token the open's read found no row of",
			deps: Deps{Store: database},
			request: func() *http.Request {
				request := httptest.NewRequest(http.MethodGet, "/ws/live", nil)
				request.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 12))
				return request
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			deps := c.deps
			deps.StreamHeartbeat = 20 * time.Millisecond
			served := directServer(deps)
			ended := make(chan bool, 1)
			connection := served.whileCallerResolves(resolves, true, func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
					ended <- true
				case <-time.After(2 * time.Second):
					ended <- false
				}
			})
			connection.ServeHTTP(httptest.NewRecorder(), c.request())
			if !<-ended {
				t.Fatalf("a live connection whose open-time resolution failed for %s was still open 2s later: nothing held it to its person's member kind", c.name)
			}
		})
	}
}

// failingOnceIdentity is an identity provider that fails its first read, as one that did not
// answer in time does, and answers login every time after.
type failingOnceIdentity struct {
	calls atomic.Int32
	login string
}

func (f *failingOnceIdentity) Login(*http.Request) (string, error) {
	if f.calls.Add(1) == 1 {
		return "", errors.New("the identity provider did not answer")
	}
	return f.login, nil
}

// A full member holds a live delivery view, a timeline, which shows every pull request whose issue
// its reader sees: PUB's, a public project whose list does not name them, among them; PRIV's list
// names them. The hourly confirmation then restricts them (ConfirmSubject, the write it makes for
// an account that left the Workspace), and a restricted member reads only the projects that name
// them. From the confirmation's commit the hub reads dave again, so acme/widgets#7, a pull request
// of PUB's issue written after the confirmation, reaches his page in no frame. acme/secret#8, a
// pull request of PRIV's issue written after it, does: the read that names it has read
// acme/widgets#7's row as well, and would name it too were it made for the full member dave was.
// The test ends on that frame, and fails when no frame names it.
//
// Control: the confirmation announces nothing. The hub keeps the full member's reader it had, and
// the next delta sends dave acme/widgets#7, which the test names.
func TestADeliveryViewStopsCarryingAProjectOnceTheHourlyConfirmationRestrictsItsReader(t *testing.T) {
	const dave = "dave@example.com"
	ctx := context.Background()
	seed := seedPrivateProject(t)
	people := store.NewPgPeopleStore(seed.database.Pool, testSigningKey, nil)
	if err := people.SignInSubject(ctx, dave, "dave-subject", "refresh-dave", "", model.MemberFull); err != nil {
		t.Fatalf("record dave's sign-in: %v", err)
	}
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/people/"+dave, nil, http.StatusOK)
	seedHubSettings(t, seed.database)
	clock := newNoonHubClock()
	now := clock.Now()
	rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)
	resolve := func(ctx context.Context) (model.Actor, error) {
		return seed.server.readerFor(ctx, model.BrowserActor(dave))
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: resolve})
	conn.snapshot(t, 5*time.Second, "t")
	rig.waitListening(t)
	// The connection's own re-check runs before the confirmation below, so only the confirmation's
	// announcement can read dave again.
	rig.waitFor(t, 0, 5*time.Second, "the re-check after the listener connected", func(note deliveryHubNote) bool {
		return note.what == "readers rechecked"
	})

	if err := people.ConfirmSubject(ctx, dave, "dave-subject", "refresh-dave", "refresh-dave-confirmed", "", model.MemberRestricted); err != nil {
		t.Fatalf("dave's hourly confirmation as a restricted member: %v", err)
	}
	public := hubPR(7, now.Add(-time.Hour))
	public.IssueKey = new(seed.issue[seedPublic])
	upsertPR(t, seed.database, public)
	secret := hubPR(8, now.Add(-time.Hour))
	secret.Repo, secret.URL = "acme/secret", "https://github.com/acme/secret/pull/8"
	secret.IssueKey = new(seed.issue[seedPrivate])
	upsertPR(t, seed.database, secret)

	// Every frame dave's page is sent, snapshot or delta, names no pull request of PUB, up to the
	// first that names acme/secret#8, written after acme/widgets#7.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case frame := <-conn.frames:
			ids, raw := framePullRequests(frame)
			if slices.Contains(ids, "acme/widgets#7") {
				t.Fatalf("dave, restricted by his hourly confirmation, was sent acme/widgets#7, a pull request of PUB: %.300s", raw)
			}
			if slices.Contains(ids, "acme/secret#8") {
				return
			}
		case <-deadline:
			t.Fatal("no frame dave's page was sent within 15s named acme/secret#8, a pull request of PRIV, whose list names him")
		}
	}
}

// listOn names person on project key's access list in a transaction of its own, as its owner's
// write does.
func listOn(t *testing.T, database *store.Store, broker *events.Broker, key, person string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := access.AddPrincipal(ctx, tx, broker, key, access.PrincipalPerson, person, model.Actor{Kind: "user", ID: "carol@example.com"}); err != nil {
		t.Fatalf("list %s on %s: %v", person, key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// storedKind is the member kind people records for email.
func storedKind(t *testing.T, database *store.Store, email string) string {
	t.Helper()
	var kind string
	if err := database.Pool.QueryRow(context.Background(), `select member_kind from people where email = $1`, email).Scan(&kind); err != nil {
		t.Fatalf("read the member kind of %s: %v", email, err)
	}
	return kind
}
