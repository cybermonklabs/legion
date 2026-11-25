package docs

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// handedCallerServer serves service's document sockets as the api package mounts them: every
// request reaches ServeHTTP carrying caller (WithSocketCaller), the caller the api package decided
// for it, until the test ends.
func handedCallerServer(t *testing.T, service *Service, caller SocketCaller) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.ServeHTTP(w, r.WithContext(WithSocketCaller(r.Context(), caller)))
	}))
	testcleanup.Add(t, server.Close)
	return server
}

// tokenSession is the actor the api package hands for a broker-issued token that names a person:
// the session the token names, with the owner, Proven and Act an API write with the same token
// records (bearerSessionActor in internal/dispatch/api).
func tokenSession() model.Actor {
	const session = "session-0123456789abcdef"
	return model.Actor{
		Kind:   "session",
		ID:     session,
		Owner:  new("alice@example.com"),
		Proven: true,
		Act:    &model.ActorAct{Session: session, SessionProven: true, Scopes: []string{model.ScopePerson}},
	}
}

// countedLogins is a service's identity that counts the lookups it is asked for.
type countedLogins struct {
	identity.Identity
	lookups atomic.Int64
}

func (c *countedLogins) Login(r *http.Request) (string, error) {
	c.lookups.Add(1)
	return c.Identity.Login(r)
}

// admittedCalls records, in order, each writability authorize hands a SocketCaller's Admitted.
type admittedCalls struct {
	mu    sync.Mutex
	calls []bool
}

func (a *admittedCalls) record(writable bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, writable)
}

func (a *admittedCalls) taken() []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

// registeredActors is the actor the room registered for each of its connections (state.connected).
func registeredActors(service *Service, artifactID string) []model.Actor {
	state := service.lockExistingState(artifactID)
	if state == nil {
		return nil
	}
	defer service.unlockState(artifactID, state)
	return slices.Collect(maps.Values(state.connected))
}

// A document socket the api package hands a caller (WithSocketCaller) is that caller. Here the
// caller is a broker-issued token's session, with its owner, Proven and Act, on a request whose
// header names bob, a person the service's header identity would resolve: the room registers the
// connection as the handed session and credits the edit it sends to that whole actor, and the
// identity is asked nothing. authorize tells the caller's Admitted, once, whether it admitted the
// socket writable: true on an open issue's document, false on a closed issue's. With no caller
// handed, the header identity decides: carol's header registers carol, Proven, and a request
// naming nobody is refused with the docs package's own 401, as text/plain with nosniff, naming the
// process ProcessName reports.
//
// Controls: a requestCaller that asks the identity before it reads the handed caller registers bob
// rather than the session and counts a lookup; one that asks and then takes the handed caller still
// counts a lookup. An Admitted left uncalled, called twice, or called with anything but the socket's
// writability fails the first two cases. A fallback that stops asking the identity, or that writes
// its refusal as JSON, fails the last.
func TestADocumentSocketTakesTheCallerItIsHanded(t *testing.T) {
	t.Parallel()
	session := tokenSession()
	newCountingService := func(t *testing.T) (*Service, string, *countedLogins) {
		t.Helper()
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, "before")
		logins := &countedLogins{Identity: service.identity}
		service.identity = logins
		return service, artifactID, logins
	}

	t.Run("a caller handed on an open issue's document", func(t *testing.T) {
		service, artifactID, logins := newCountingService(t)
		var admitted admittedCalls
		server := handedCallerServer(t, service, SocketCaller{Actor: session, MayWrite: true, Admitted: admitted.record})
		peer := connectWithScope(t, server.URL, artifactID, "bob", "read-write", "before\n")
		if registered := registeredActors(service, artifactID); !reflect.DeepEqual(registered, []model.Actor{session}) {
			t.Fatalf("the room registered %+v, want the handed session %+v alone", registered, session)
		}
		if calls := admitted.taken(); !slices.Equal(calls, []bool{true}) {
			t.Fatalf("authorize called Admitted with %v, want once with true", calls)
		}
		editAsBrowser(t, service, artifactID, peer, "after\n")
		requireOwedAlone(t, service, artifactID, session)
		if lookups := logins.lookups.Load(); lookups != 0 {
			t.Fatalf("the document service asked its identity %d times for a socket handed its caller, want none", lookups)
		}
	})

	t.Run("a caller handed on a closed issue's document", func(t *testing.T) {
		service, artifactID, logins := newCountingService(t)
		writeIssueClosed(t, service.store, "DOC-1", true)
		var admitted admittedCalls
		server := handedCallerServer(t, service, SocketCaller{Actor: session, MayWrite: true, Admitted: admitted.record})
		connectWithScope(t, server.URL, artifactID, "bob", "readonly", "before\n")
		if registered := registeredActors(service, artifactID); !reflect.DeepEqual(registered, []model.Actor{session}) {
			t.Fatalf("the room registered %+v, want the handed session %+v alone", registered, session)
		}
		if calls := admitted.taken(); !slices.Equal(calls, []bool{false}) {
			t.Fatalf("authorize called Admitted with %v, want once with false", calls)
		}
		if lookups := logins.lookups.Load(); lookups != 0 {
			t.Fatalf("the document service asked its identity %d times for a socket handed its caller, want none", lookups)
		}
	})

	t.Run("no caller handed", func(t *testing.T) {
		service, artifactID, logins := newCountingService(t)
		server := newTestServer(t, service)
		connectWithScope(t, server.URL, artifactID, "carol", "read-write", "before\n")
		carol := model.Actor{Kind: "user", ID: "carol", Proven: true}
		if registered := registeredActors(service, artifactID); !reflect.DeepEqual(registered, []model.Actor{carol}) {
			t.Fatalf("the room registered %+v, want carol %+v, whom the header names", registered, carol)
		}
		if lookups := logins.lookups.Load(); lookups != 1 {
			t.Fatalf("the document service asked its identity %d times for one socket handed no caller, want once", lookups)
		}

		connection, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/doc/"+artifactID, nil)
		if err == nil {
			_ = connection.Close()
			t.Fatal("a socket whose request names nobody was opened, want it refused")
		}
		if response == nil {
			t.Fatalf("a socket whose request names nobody got no answer: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a socket whose request names nobody was answered %d, want 401", response.StatusCode)
		}
		if kind, sniff := response.Header.Get("Content-Type"), response.Header.Get("X-Content-Type-Options"); kind != "text/plain; charset=utf-8" || sniff != "nosniff" {
			t.Fatalf("the 401 is Content-Type %q with X-Content-Type-Options %q, want the docs package's text/plain refusal with nosniff", kind, sniff)
		}
		if named := response.Header.Get(ProcessHeader); named != service.ProcessName() {
			t.Fatalf("the 401 names process %q, want %q, the name ProcessName reports", named, service.ProcessName())
		}
	})
}

// A socket the api package hands with MayWrite false is read-only for its whole life, on a page
// the document service would admit writable (an open issue, a page every live process admits): its
// token message is answered readonly, and the edit it sends after that message is neither taken
// by the room nor stored. The control is the same socket handed MayWrite true: its token message
// is answered read-write, and its edit is stored.
//
// Controls: the read-only mark kept in authorize's own answer and left out of admission.writable,
// which the token message's answer reads (schemaAdmissions.writable), answers the token read-write
// and lets the edit in, and so does an authorize that reads no mayWrite at all: either fails the
// first case. A mark that made every handed socket read-only fails the second.
func TestADocumentSocketsReadOnlyMarkOutlivesItsTokenMessage(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		mayWrite bool
		scope    string
	}{
		{name: "handed a caller that may not write", mayWrite: false, scope: "readonly"},
		{name: "handed a caller that may write", mayWrite: true, scope: "read-write"},
	} {
		t.Run(c.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "before")
			server := handedCallerServer(t, service, SocketCaller{Actor: tokenSession(), MayWrite: c.mayWrite})
			peer := connectWithScope(t, server.URL, artifactID, "bob", c.scope, "before\n")
			if !c.mayWrite {
				requireKeystrokeDropped(t, service, peer, artifactID, storedHead(t, service.store, artifactID))
				return
			}
			typeOver(t, peer, "before", "typed by the session")
			waitForPersistedProofText(t, service.store, artifactID, "typed by the session\n")
		})
	}
}

// A socket handed a caller that may write still meets the conditions every socket meets: on a
// closed issue's document, and on a page the live fleet does not admit (a live process of the next
// build creates what the page cannot represent), it opens read-only, its token message is
// answered readonly, and its edit is neither taken by the room nor stored.
//
// Control: an authorize that let a handed caller's MayWrite stand for the whole answer, skipping
// the issue-open or the fleet condition for it, opens the socket read-write in the case whose
// condition it skips, and that case fails on the scope.
func TestADocumentSocketKeepsTodaysWriteConditionsForAToken(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		arrange func(t *testing.T, service *Service)
	}{
		{name: "the document's issue is closed", arrange: func(t *testing.T, service *Service) {
			writeIssueClosed(t, service.store, "DOC-1", true)
		}},
		{name: "a live process creates what the page cannot represent", arrange: func(t *testing.T, service *Service) {
			version := pmdoc.SchemaVersion()
			fleetProcess(t, service, version+1, version+1, store.OwnerLockProtocol, 0)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "before")
			c.arrange(t, service)
			server := handedCallerServer(t, service, SocketCaller{Actor: tokenSession(), MayWrite: true})
			peer := connectWithScope(t, server.URL, artifactID, "bob", "readonly", "before\n")
			requireKeystrokeDropped(t, service, peer, artifactID, storedHead(t, service.store, artifactID))
		})
	}
}

// A connection the api package hands as a broker-issued token's session is read as that session
// by each reader of the room's connections (state.connected), through what that reader does:
//
//   - connectedCreditLocked credits the session with the edit it sent, so the document owes the
//     session alone and the version the settlement writes lists it;
//   - recordEditNotice counts people alone, so that version opens no edit notice, held or due;
//   - soleConnectedActor names the session, the room's one connection, as the author of the ask
//     block it wrote (observeAskBlocks), which the append records and the settled ask keeps;
//   - connectedActors names it, so a lost write names it among the document's participants
//     (lostWriteParticipants);
//   - connectedMarkAuthors counts people alone, so an unrecorded mark naming the session, in
//     either spelling, ages and is swept a TTL after it was first seen though the session is
//     connected throughout.
//
// Controls: a session registered as anyone else, or not registered, fails the credit, the ask's
// author and the participants. A reader that counted a session as a person fails on what it then
// does: recordEditNotice opens a notice, and the sweep keeps the marks as an open composer's.
func TestATokenConnectionIsReadAsTheSessionItIs(t *testing.T) {
	t.Parallel()
	session := tokenSession()
	caller := SocketCaller{Actor: session, MayWrite: true}

	t.Run("credited with its edit, and named by no edit notice", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
		settleCurrentGeneration(t, service, artifactID)
		if rows := editNoticeRows(t, service.store, artifactID, false); rows != 0 {
			t.Fatalf("the seed left %d edit notice rows, want none before the session edits", rows)
		}
		before := latestVersionNumber(t, service, artifactID)
		server := handedCallerServer(t, service, caller)
		peer := connectWithScope(t, server.URL, artifactID, session.ID, "read-write", "First.\n\nSecond.\n")
		editAsBrowser(t, service, artifactID, peer, "First, by the session.\n\nSecond.\n")
		requireOwedAlone(t, service, artifactID, session)
		settleCurrentGeneration(t, service, artifactID)
		if latest := latestVersionNumber(t, service, artifactID); latest != before+1 {
			t.Fatalf("the settlement wrote version %d, want %d", latest, before+1)
		}
		if authors := latestVersionAuthors(t, service, artifactID); !reflect.DeepEqual(authors, []model.Actor{session}) {
			t.Fatalf("the settled version lists %+v, want the session %+v alone", authors, session)
		}
		if rows := editNoticeRows(t, service.store, artifactID, false); rows != 0 {
			t.Fatalf("the session's settled edit left %d edit notice rows, want none: a notice tells of a person's edit", rows)
		}
	})

	t.Run("named the author of the ask block it wrote alone", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, "First.\n")
		settleCurrentGeneration(t, service, artifactID)
		server := handedCallerServer(t, service, caller)
		peer := connectWithScope(t, server.URL, artifactID, session.ID, "read-write", "First.\n")
		editAsBrowser(t, service, artifactID, peer, "First.\n\n:::ask{#session-ask urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n")
		waitForLandedAppends(t, service, artifactID)
		if recorded := recordedAskAuthors(t, service.store, artifactID); !reflect.DeepEqual(recorded, map[string]model.Actor{"session-ask": session}) {
			t.Fatalf("doc_ask_block_authors after the session's append = %+v, want session-ask recorded as the session %+v", recorded, session)
		}
		settleCurrentGeneration(t, service, artifactID)
		if author := blockAskAuthor(t, service, artifactID, "session-ask"); !reflect.DeepEqual(author, session) {
			t.Fatalf("the settled ask's author = %+v, want the session %+v, the room's one connection when it wrote the block", author, session)
		}
	})

	t.Run("named among a lost write's participants", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, lostEditDocument)
		server := handedCallerServer(t, service, caller)
		connectWithScope(t, server.URL, artifactID, session.ID, "read-write", lostEditDocument)
		edit := startEditInFlight(t, service, artifactID, model.EditOp{
			Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph edited.",
		})
		// The concurrent change goes into the room as the package's other lost-write rows put it
		// there (editInFlight.browserWrites): what this case reads is whom the refusal names, and
		// those are the connections the room registered, the session's socket alone.
		edit.browserWrites(t, withoutTopLevelBlock("Alpha paragraph."))
		_, err := service.SnapshotVersion(edit.ctx, artifactID, lostEditAgent)
		var lost *ErrEditLost
		if !errors.As(err, &lost) {
			t.Fatalf("snapshot after a concurrent change deleted the agent's paragraph = %v, want the agent's edit refused as lost", err)
		}
		if !reflect.DeepEqual(lost.Participants, []model.Actor{session}) {
			t.Fatalf("the lost write names the participants %+v, want the connected session %+v", lost.Participants, session)
		}
		if !strings.Contains(lost.Error(), ActorRef(session)) {
			t.Fatalf("the refusal %q does not name the session %s", lost.Error(), ActorRef(session))
		}
		edit.ledger.Discard()
		if err := edit.tx.Rollback(context.Background()); err != nil {
			t.Fatalf("roll back the refused edit: %v", err)
		}
	})

	t.Run("left out of the authors whose marks the sweep keeps", func(t *testing.T) {
		service, artifactID, age := newUnrecordedMarkService(t)
		seedServiceText(t, service, artifactID, "The quick brown fox")
		server := handedCallerServer(t, service, caller)
		connectWithScope(t, server.URL, artifactID, session.ID, "read-write", "The quick brown fox\n")
		browserMarkWithAttrs(t, service, artifactID, "proofComment", "quick", pmdoc.Attrs{"id": "bare", "by": session.ID})
		browserMarkWithAttrs(t, service, artifactID, "proofComment", "brown", pmdoc.Attrs{"id": "reference", "by": ActorRef(session)})
		settleCurrentGeneration(t, service, artifactID)
		age(markSweepTTL)
		settleCurrentGeneration(t, service, artifactID)
		for _, id := range []string{"bare", "reference"} {
			if markIsInTheDocument(t, service, artifactID, id) {
				t.Errorf("the unrecorded mark %s naming the connected session outlived its TTL: the sweep kept it as an open composer's", id)
			}
		}
	})
}
