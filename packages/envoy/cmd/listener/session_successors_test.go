package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
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

// readSuccessors is GET /v1/sessions/<session>/successors through handler, decoded, with the
// answer as it came.
func readSuccessors(t *testing.T, handler http.Handler, session string) (listenerwire.SessionSuccessors, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/sessions/"+session+"/successors", nil))
	var answer listenerwire.SessionSuccessors
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the successors of %s %s: %v", session, recorder.Body.String(), err)
		}
	}
	return answer, recorder
}

// The route registerV1Routes mounts answers the sessions the handover records of a session name
// as its successors, a record of a role under a project and one of a bare role alike, and not the
// session a plain takeover record of it was written for; a session no record names is answered
// the empty list, never null.
func TestTheSuccessorsRouteNamesTheSessionsThatReplacedASession(t *testing.T) {
	client := setupPublishTestClient(t)
	const replaced = "ses_successors_route_replaced"
	writeHandoverRecords(t, client,
		bus.HandoverSubject("ACME.reviewer", replaced, "ses_successors_route_b"),
		bus.HandoverSubject("plain-role", replaced, "ses_successors_route_a"),
		bus.TakeoverSubject("plain-role", replaced, "attempt1"),
	)
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client}, "test-machine", logging.New("test"))

	answer, recorder := readSuccessors(t, mux, replaced)
	requireStatus(t, recorder, http.StatusOK, "the successors of "+replaced)
	if want := []string{"ses_successors_route_a", "ses_successors_route_b"}; answer.SessionID != replaced || !slices.Equal(answer.Successors, want) {
		t.Errorf("the successors of %s = %+v, want %v", replaced, answer, want)
	}

	const nobody = "ses_successors_route_unreplaced"
	answer, recorder = readSuccessors(t, mux, nobody)
	requireStatus(t, recorder, http.StatusOK, "the successors of "+nobody)
	if !strings.Contains(recorder.Body.String(), `"successors":[]`) || answer.SessionID != nobody {
		t.Errorf("the successors of %s = %s, want the empty list", nobody, recorder.Body.String())
	}
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
	writeHandoverRecords(t, client, bus.HandoverSubject("plain-role", replaced, "ses_successors_silent_b"))
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client}, "test-machine", logging.New("test"))

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
