package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The verdict of a keyed write's answer is the agents' client's (packages/envoy-client/src/
// dispatch-http.ts, answerEvent): every answer of Dispatch's keyed layer names the request's key
// back, so an answer that names none is no verdict whatever its status; a 2xx naming it is the
// write landed; KEY_COMMITTED is a commit whose answer was lost; a replay, a handler's stored 4xx
// and KEY_REUSED are Dispatch's verdict on this request; and an outage, a 401, KEY_IN_FLIGHT and no
// answer at all settle nothing.
func TestTheKeyedVerdictReadsEachAnswerAsTheAgentsClientDoes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		status           int
		code             string
		echoed, replayed bool
		want             Verdict
	}{
		{name: "no answer", want: Unsettled},
		{name: "a 200 naming the key", status: 200, echoed: true, want: Landed},
		{name: "a replayed 200", status: 200, echoed: true, replayed: true, want: Landed},
		{name: "a 200 naming no key", status: 200, want: Unsettled},
		{name: "a redirect", status: 302, echoed: true, want: Unsettled},
		{name: "a 401 naming the key", status: 401, code: "UNAUTHORIZED", echoed: true, want: Unsettled},
		{name: "a 502", status: 502, echoed: true, want: Unsettled},
		{name: "a 503 naming no key", status: 503, want: Unsettled},
		{name: "a 408", status: 408, echoed: true, want: Unsettled},
		{name: "a 429", status: 429, echoed: true, want: Unsettled},
		{name: "a key in flight", status: 409, code: IdempotencyKeyInFlight, echoed: true, want: Unsettled},
		{name: "a key committed", status: 409, code: IdempotencyKeyCommitted, echoed: true, want: Committed},
		{name: "a key committed naming no key", status: 409, code: IdempotencyKeyCommitted, want: Unsettled},
		{name: "a key reused", status: 422, code: IdempotencyKeyReused, echoed: true, want: Refused},
		{name: "a handler's ISSUE_CHANGED", status: 409, code: "ISSUE_CHANGED", echoed: true, want: Refused},
		{name: "a replayed FOREIGN_CHILD_OPEN", status: 409, code: ForeignChildOpen, echoed: true, replayed: true, want: Refused},
		{name: "a 403 from a layer before the key", status: 403, code: "PROJECT_SCOPE", want: Unsettled},
		{name: "a 403 the handler stored", status: 403, code: "PROJECT_SCOPE", echoed: true, want: Refused},
	} {
		if got := KeyedVerdict(tc.status, tc.code, tc.echoed, tc.replayed); got != tc.want {
			t.Errorf("%s: KeyedVerdict(%d, %q, echoed %v, replayed %v) = %d, want %d", tc.name, tc.status, tc.code, tc.echoed, tc.replayed, got, tc.want)
		}
	}
}

// An acceptance write carries its key, and the client reads whether each answer names it back and
// was replayed: a 2xx naming it is the issue the write left; a 2xx naming none is no answer of the
// keyed layer, an *Error whose verdict is Unsettled; a replayed refusal is Refused, with its code.
func TestSetAcceptanceStatusSendsItsKeyAndReadsItsEcho(t *testing.T) {
	t.Parallel()
	var echo, replay bool
	var status int
	var sent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Get(IdempotencyKeyHeader)
		if echo {
			w.Header().Set(IdempotencyKeyHeader, sent)
		}
		if replay {
			w.Header().Set(IdempotencyReplayedHeader, "?1")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"key":"LEGION-4","status":"done","last_seq":13}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"ISSUE_CHANGED","error":"issue changed"}`))
	}))
	defer server.Close()
	client := New(server.URL, BearerTransport(dispatchToken, nil))
	write := AcceptanceWrite{Key: "legion:close_root:LEGION-4:k1", Issue: "LEGION-4", Status: "done", Sequence: 12}

	echo, status = true, http.StatusOK
	written, err := client.SetAcceptanceStatus(context.Background(), write)
	if err != nil || written.Status != "done" || sent != write.Key || VerdictOf(err) != Landed {
		t.Fatalf("the answer naming the key = %#v, %v, sent key %q; want done, landed, under %q", written, err, sent, write.Key)
	}
	echo = false
	if _, err = client.SetAcceptanceStatus(context.Background(), write); VerdictOf(err) != Unsettled {
		t.Fatalf("a 200 naming no key = %v, verdict %d; want it unsettled", err, VerdictOf(err))
	}
	echo, replay, status = true, true, http.StatusConflict
	_, err = client.SetAcceptanceStatus(context.Background(), write)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "ISSUE_CHANGED" || !refusal.Echoed || !refusal.Replayed || VerdictOf(err) != Refused {
		t.Fatalf("a replayed refusal = %#v; want ISSUE_CHANGED, echoed and replayed, refused", err)
	}
}
