package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// writeHandoverRecords writes a record on each of subjects through client, as the claim route
// writes a fresh one (bus.Client.WriteTakeoverRecord).
func writeHandoverRecords(t *testing.T, client *bus.Client, subjects ...string) {
	t.Helper()
	for _, subject := range subjects {
		if err := client.WriteTakeoverRecord(t.Context(), subject, bus.TakeoverRecord{ClaimedAt: 1}); err != nil {
			t.Fatalf("write the record %s: %v", subject, err)
		}
	}
}

// testDispatchPrincipal is the identity a test names as Dispatch's (listenerDeps.dispatchPrincipal,
// ENVOY_DISPATCH_PRINCIPAL) and readSuccessors presents as verified: the one caller the successors
// route names the successors' principal to.
const testDispatchPrincipal = "system:serviceaccount:example-ns:example-dispatch"

// readSuccessors is GET /v1/sessions/<session>/successors through handler as Dispatch reads it,
// on testDispatchPrincipal, decoded, with the answer as it came.
func readSuccessors(t *testing.T, handler http.Handler, session string) (listenerwire.SessionSuccessors, *httptest.ResponseRecorder) {
	t.Helper()
	return readSuccessorsAs(t, handler, session, testDispatchPrincipal)
}

// readSuccessorsAs is readSuccessors on a request whose identity token proved identity
// (withVerifiedPrincipal), or on no credential for "".
func readSuccessorsAs(t *testing.T, handler http.Handler, session, identity string) (listenerwire.SessionSuccessors, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+session+"/successors", nil)
	if identity != "" {
		request = request.WithContext(withVerifiedPrincipal(request.Context(), identity))
	}
	handler.ServeHTTP(recorder, request)
	var answer listenerwire.SessionSuccessors
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the successors of %s %s: %v", session, recorder.Body.String(), err)
		}
	}
	return answer, recorder
}

// registerAs records each of sessionIDs as having last registered as principal, none for "", as
// the registration route records a session: admitted first, then written at the admission's
// revision (session.KnownSessions.AdmitRegistration, RecordRegistrationWithin).
func registerAs(t *testing.T, known *session.KnownSessions, principal string, sessionIDs ...string) {
	t.Helper()
	for _, sessionID := range sessionIDs {
		admission, err := known.AdmitRegistration(t.Context(), sessionID, principal)
		if err != nil {
			t.Fatalf("admit %s's registration as %q: %v", sessionID, principal, err)
		}
		if err := known.RecordRegistrationWithin(t.Context(), admission, session.KnownSession{SessionID: sessionID, Principal: principal}); err != nil {
			t.Fatalf("record %s's registration as %q: %v", sessionID, principal, err)
		}
	}
}

// The route registerV1Routes mounts answers the sessions the handover records of a session name
// as its successors, a record of a role under a project and one of a bare role alike, and not the
// session a plain takeover record of it was written for; a session no record names is answered
// the empty list, never null. A session whose principal no registration recorded is named by
// none, its records notwithstanding, since nothing says its successors are its principal's.
func TestTheSuccessorsRouteNamesTheSessionsThatReplacedASession(t *testing.T) {
	client := setupPublishTestClient(t)
	known := openKnownSessionsOn(t, client.Conn, time.Now)
	const replaced, unrecorded = "ses_successors_route_replaced", "ses_successors_route_unrecorded"
	registerAs(t, known, "", replaced, "ses_successors_route_a", "ses_successors_route_b")
	writeHandoverRecords(t, client,
		bus.HandoverSubject("ACME.reviewer", replaced, "ses_successors_route_b"),
		bus.HandoverSubject("plain-role", replaced, "ses_successors_route_a"),
		bus.TakeoverSubject("plain-role", replaced, "attempt1"),
		bus.HandoverSubject("plain-role", unrecorded, "ses_successors_route_a"),
	)
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client, known: known}, "test-machine", logging.New("test"))

	answer, recorder := readSuccessors(t, mux, replaced)
	requireStatus(t, recorder, http.StatusOK, "the successors of "+replaced)
	if want := []string{"ses_successors_route_a", "ses_successors_route_b"}; answer.SessionID != replaced || !slices.Equal(answer.Successors, want) {
		t.Errorf("the successors of %s = %+v, want %v", replaced, answer, want)
	}

	for _, nobody := range []string{"ses_successors_route_unreplaced", unrecorded} {
		answer, recorder = readSuccessors(t, mux, nobody)
		requireStatus(t, recorder, http.StatusOK, "the successors of "+nobody)
		if !strings.Contains(recorder.Body.String(), `"successors":[]`) || answer.SessionID != nobody {
			t.Errorf("the successors of %s = %s, want the empty list", nobody, recorder.Body.String())
		}
	}
}

// A message handed to a session that is itself replaced before it reads it is handed on to the
// session that replaced that one, its delivery unchanged, so the route names the whole chain:
// the session that replaced a session, the one that replaced that one, and so on, and the
// principal they all last registered as, which Dispatch holds a successor's reply to. It follows a
// session only while it last registered as the principal the asked-about session did: the
// person's session at the end of the chain, and the one only it leads to, are named by no read,
// and a chain that comes back round to the session asked about ends there. The case fails where
// the route names a session's own successors alone (heir and spare, not grandheir), or follows a
// session of another principal (stranger, beyond), or names another principal than theirs or
// one beside no successor (beyond, whom nobody replaced).
func TestTheSuccessorsRouteFollowsAChainOfOnePrincipal(t *testing.T) {
	client := setupPublishTestClient(t)
	known := openKnownSessionsOn(t, client.Conn, time.Now)
	const (
		first, heir, spare, grandheir = "ses_chain_first", "ses_chain_heir", "ses_chain_spare", "ses_chain_grandheir"
		stranger, beyond              = "ses_chain_stranger", "ses_chain_beyond"
	)
	registerAs(t, known, "example-fleet", first, heir, spare, grandheir, beyond)
	registerAs(t, known, "ada@example.com", stranger)
	writeHandoverRecords(t, client,
		bus.HandoverSubject("plain-role", first, heir),
		bus.HandoverSubject("plain-role", first, spare),
		bus.HandoverSubject("plain-role", heir, grandheir),
		bus.HandoverSubject("plain-role", grandheir, first),
		bus.HandoverSubject("plain-role", grandheir, stranger),
		bus.HandoverSubject("plain-role", stranger, beyond),
	)
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client, known: known, dispatchPrincipal: testDispatchPrincipal}, "test-machine", logging.New("test"))

	for session, want := range map[string][]string{
		first: {grandheir, heir, spare},
		heir:  {first, grandheir, spare},
	} {
		answer, recorder := readSuccessors(t, mux, session)
		requireStatus(t, recorder, http.StatusOK, "the successors of "+session)
		if !slices.Equal(answer.Successors, want) || answer.Principal != "example-fleet" {
			t.Errorf("the successors of %s = %v for %q, want %v for example-fleet", session, answer.Successors, answer.Principal, want)
		}
	}
	if answer, recorder := readSuccessors(t, mux, beyond); recorder.Code != http.StatusOK || len(answer.Successors) != 0 || answer.Principal != "" {
		t.Errorf("the successors of %s = %d %s, want none and no principal", beyond, recorder.Code, recorder.Body.String())
	}
}

// A copy handed on from one session to the next has no bound on its hops, so neither has the
// route: a session replaced ten times over, each successor of its principal, has all ten named,
// the last of them among them, and so may answer the deliveries of every session before it. The
// case fails where the route stops after a fixed number of hops and answers the sessions it
// reached as the whole list; the control is the session one before the end, which has one.
func TestTheSuccessorsRouteFollowsAChainOfAnyLength(t *testing.T) {
	client := setupPublishTestClient(t)
	known := openKnownSessionsOn(t, client.Conn, time.Now)
	chain := make([]string, 11)
	for i := range chain {
		chain[i] = fmt.Sprintf("ses_long_chain_%02d", i)
	}
	registerAs(t, known, "example-fleet", chain...)
	subjects := make([]string, 0, len(chain)-1)
	for i := 1; i < len(chain); i++ {
		subjects = append(subjects, bus.HandoverSubject("plain-role", chain[i-1], chain[i]))
	}
	writeHandoverRecords(t, client, subjects...)
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client, known: known}, "test-machine", logging.New("test"))

	for _, c := range []struct {
		name, session string
		want          []string
	}{
		{"case: the first session of the chain", chain[0], chain[1:]},
		{"control: the session one before the end", chain[len(chain)-2], chain[len(chain)-1:]},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer, recorder := readSuccessors(t, mux, c.session)
			requireStatus(t, recorder, http.StatusOK, "the successors of "+c.session)
			if !slices.Equal(answer.Successors, c.want) {
				t.Errorf("the successors of %s = %v, want %v", c.session, answer.Successors, c.want)
			}
		})
	}
}

// The principal the successors last registered as names a person's address or a service, which no
// session needs to read of another; Dispatch alone holds a successor's reply to it. So the route
// names it only to Dispatch's own verified identity (listenerDeps.dispatchPrincipal), and answers
// every other caller the same successors with no principal: a request with no credential, one
// whose identity token proved another principal, and Dispatch's own identity on a listener that
// names no Dispatch principal. Each case fails where the route names the principal to any caller;
// the control is Dispatch's read, which names it.
func TestTheSuccessorsRouteNamesThePrincipalToDispatchAlone(t *testing.T) {
	client := setupPublishTestClient(t)
	known := openKnownSessionsOn(t, client.Conn, time.Now)
	const replaced, heir = "ses_successors_principal_replaced", "ses_successors_principal_heir"
	registerAs(t, known, "person:ada@example.com", replaced, heir)
	writeHandoverRecords(t, client, bus.HandoverSubject("plain-role", replaced, heir))
	named := http.NewServeMux()
	registerV1Routes(named, &listenerDeps{client: client, known: known, dispatchPrincipal: testDispatchPrincipal}, "test-machine", logging.New("test"))
	unnamed := http.NewServeMux()
	registerV1Routes(unnamed, &listenerDeps{client: client, known: known}, "test-machine", logging.New("test"))

	for _, row := range []struct {
		name      string
		mux       http.Handler
		identity  string
		principal string
	}{
		{"case: a request with no credential", named, "", ""},
		{"case: an identity token that proved another principal", named, "system:serviceaccount:example-ns:example-worker", ""},
		{"case: Dispatch's identity on a listener that names no Dispatch principal", unnamed, testDispatchPrincipal, ""},
		{"control: Dispatch's own identity", named, testDispatchPrincipal, "person:ada@example.com"},
	} {
		t.Run(row.name, func(t *testing.T) {
			answer, recorder := readSuccessorsAs(t, row.mux, replaced, row.identity)
			requireStatus(t, recorder, http.StatusOK, "the successors of "+replaced)
			if !slices.Equal(answer.Successors, []string{heir}) || answer.Principal != row.principal {
				t.Errorf("the successors of %s = %v for %q; want [%s] for %q", replaced, answer.Successors, answer.Principal, heir, row.principal)
			}
		})
	}
}

// A listener whose known-session bucket is not open can read no principal any session registered
// as until an attempt of its own store's opens the bucket, which on a listener whose grant does not
// carry it is not before the grant does, so its successors route names no successor, 200 with the
// empty list, as its claim route refuses every handover: Dispatch then refuses another session's
// reply 403 at once, as it did before successors, where a 503 would have it answer
// ENVOY_UNAVAILABLE to every such reply for as long as the bucket stays shut. The case fails where
// the route answers that read 503; the control is the same read through the store whose bucket is
// open, which names the successor.
func TestTheSuccessorsRouteNamesNoneWhileTheKnownSessionBucketIsNotOpen(t *testing.T) {
	client := setupPublishTestClient(t)
	known := openKnownSessionsOn(t, client.Conn, time.Now)
	const replaced, heir = "ses_successors_shut_replaced", "ses_successors_shut_heir"
	registerAs(t, known, "example-fleet", replaced, heir)
	writeHandoverRecords(t, client, bus.HandoverSubject("plain-role", replaced, heir))
	shut := http.NewServeMux()
	registerV1Routes(shut, &listenerDeps{client: client, known: &session.KnownSessions{}, dispatchPrincipal: testDispatchPrincipal}, "test-machine", logging.New("test"))
	open := http.NewServeMux()
	registerV1Routes(open, &listenerDeps{client: client, known: known, dispatchPrincipal: testDispatchPrincipal}, "test-machine", logging.New("test"))

	t.Run("case: the store whose bucket is not open names no successor", func(t *testing.T) {
		answer, recorder := readSuccessors(t, shut, replaced)
		requireStatus(t, recorder, http.StatusOK, "the successors of "+replaced+" while the bucket is not open")
		if !strings.Contains(recorder.Body.String(), `"successors":[]`) || answer.Principal != "" {
			t.Errorf("the successors of %s = %s; want the empty list and no principal", replaced, recorder.Body.String())
		}
	})
	t.Run("control: the store whose bucket is open names the successor", func(t *testing.T) {
		answer, recorder := readSuccessors(t, open, replaced)
		requireStatus(t, recorder, http.StatusOK, "the successors of "+replaced)
		if !slices.Equal(answer.Successors, []string{heir}) {
			t.Errorf("the successors of %s = %v; want [%s]", replaced, answer.Successors, heir)
		}
	})
}

// A read of the records the bus does not answer is the bus-unavailable 503 with Retry-After: 1,
// never a list: an empty one would read as a session nobody replaced. The bus here swallows the
// stream read alone (the relay drops what follows the STREAM.INFO request), and the control is
// the same read before that, which the same connection answers.
func TestTheSuccessorsRouteAnswersAnUnansweredReadWith503(t *testing.T) {
	relay := startListenerRelay(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	const replaced = "ses_successors_silent_replaced"
	known := openKnownSessionsOn(t, client.Conn, time.Now)
	registerAs(t, known, "", replaced, "ses_successors_silent_b")
	writeHandoverRecords(t, client, bus.HandoverSubject("plain-role", replaced, "ses_successors_silent_b"))
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client, known: known}, "test-machine", logging.New("test"))

	answer, recorder := readSuccessors(t, mux, replaced)
	requireStatus(t, recorder, http.StatusOK, "control: the successors of "+replaced+" while the bus answers")
	if !slices.Equal(answer.Successors, []string{"ses_successors_silent_b"}) {
		t.Fatalf("control: the successors of %s = %v, want [ses_successors_silent_b]", replaced, answer.Successors)
	}

	relay.arm("$JS.API.STREAM.INFO.", 1)
	_, recorder = readSuccessors(t, mux, replaced)
	requireStatus(t, recorder, http.StatusServiceUnavailable, "the successors of "+replaced+" while the bus is silent")
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, busUnansweredPrefix) || strings.Contains(body, `"successors"`) {
		t.Errorf("the 503's body is %s, want the bus-unavailable answer and no list", body)
	}
}

// At the edge a successor is another session, so the route names one only where the token's
// projects show that session, confirmed by its stored record, as a row of the session list is: a
// PRIV reader is told of the PRIV session that replaced a session and not of the INFRA one. The
// control is the same read through the routes with no edge hold, which names both.
func TestAnEdgeNamesOnlyTheSuccessorsOfTheTokensProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const reader, priv, infra, replaced = "ses_edge_successors_reader", "ses_edge_successors_priv", "ses_edge_successors_infra", "ses_edge_successors_replaced"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	requireStatus(t, rig.subscribe(t, rig.namedToken(priv, "PRIV"), priv), http.StatusOK, "the PRIV successor's registration")
	rig.placeElsewhere(t, infra, "INFRA")
	writeHandoverRecords(t, rig.harness.client,
		bus.HandoverSubject("legion-priv-priv-2-tester", replaced, priv),
		bus.HandoverSubject("legion-infra-infra-2-tester", replaced, infra),
	)
	// The three sessions last registered as one principal, as the claim route requires of a
	// handover, recorded once the edge's own registration of the PRIV one has been.
	rig.deps.known = openKnownSessionsOn(t, rig.harness.client.Conn, time.Now)
	registerAs(t, rig.deps.known, "example-fleet", replaced, priv, infra)

	if control, recorder := readSuccessors(t, rig.mux, replaced); recorder.Code != http.StatusOK || !slices.Equal(control.Successors, []string{infra, priv}) {
		t.Fatalf("control: the successors of %s with no edge hold: status %d %s, want [%s %s]", replaced, recorder.Code, recorder.Body.String(), infra, priv)
	}
	recorder := rig.call(t, http.MethodGet, token, "/v1/sessions/"+replaced+"/successors", "")
	requireStatus(t, recorder, http.StatusOK, "the successors of "+replaced+" at the edge")
	var answer listenerwire.SessionSuccessors
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	if !slices.Equal(answer.Successors, []string{priv}) {
		t.Errorf("the successors of %s at the edge = %v, want only the PRIV session [%s]", replaced, answer.Successors, priv)
	}
}
