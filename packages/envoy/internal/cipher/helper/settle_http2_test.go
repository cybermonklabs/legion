// packages/envoy/internal/cipher/helper/settle_http2_test.go
//go:build linux

package helper

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
)

// These two rows hold the paths of requestReach.settle that no HTTP/1 row reaches. Over HTTP/1
// the client's round trip waits for its write loop before it returns an error, so WroteRequest has
// fired and `sent` decides the outcome; and Go's HTTP/1 server reads a request body before it
// answers, so it never answers a body that does not end. These need HTTP/2, where the round trip
// can return for an ended context while the write of the request body is still going, and where
// the server can answer before it reads the body. Each points the Broker at an HTTP/2 stand-in
// whose handler holds a request whose body is still being written (a never-closed pipe,
// ContentLength -1), so WroteRequest never fires.

// neverEndingBody replaces each request's body with a pipe that writes an opening and is never
// closed, with ContentLength -1, so the transport's write of the request body never ends:
// WroteRequest cannot fire, and the only trace facts left are GotConn and, where the server
// answers, GotFirstResponseByte.
type neverEndingBody struct{ base http.RoundTripper }

func (t neverEndingBody) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte(`{"runtime_id":"box-1"`)) }() // an opening, never closed
	clone.Body = pr
	clone.ContentLength = -1
	clone.GetBody = nil
	return t.base.RoundTrip(clone)
}

// pointBrokerAtHTTP2 starts an HTTP/2 httptest server running handler, points r's Broker at it
// with the never-ending-body transport, and returns a channel carrying the protocol the server
// spoke, so a row can prove the request reached it over HTTP/2.0.
func pointBrokerAtHTTP2(t *testing.T, r *rig, handler http.HandlerFunc) <-chan string {
	t.Helper()
	proto := make(chan string, 1)
	h2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case proto <- req.Proto:
		default:
		}
		handler(w, req)
	}))
	h2.EnableHTTP2 = true
	h2.StartTLS()
	t.Cleanup(h2.Close)
	r.srv.Broker.URL = cipheraddr.New(h2.URL)
	r.srv.Broker.HTTP = &http.Client{Transport: neverEndingBody{base: h2.Client().Transport}}
	return proto
}

// wantHTTP2 reads the protocol the stand-in spoke and fails the row unless it was HTTP/2.0, so a
// row that quietly fell back to HTTP/1 — where the clause under test is never reached — fails
// rather than passes for the wrong reason.
func wantHTTP2(t *testing.T, proto <-chan string) {
	t.Helper()
	select {
	case p := <-proto:
		if p != "HTTP/2.0" {
			t.Fatalf("the stand-in spoke %s, not HTTP/2.0; this row proves nothing about the HTTP/2-only clause", p)
		}
		t.Logf("the stand-in spoke %s", p)
	case <-time.After(5 * time.Second):
		t.Fatal("the stand-in never saw the request; the clause under test was not reached")
	}
}

// TestAnEnrollWhoseBodyIsStillBeingWrittenAtTheBoundIsUnknown holds settle's `connected && ended`
// clause: the stand-in reads the body and never answers, so when the op's bound ends the request,
// WroteRequest has not fired and the broker has not answered — `sent` is false, only GotConn
// fired, and the context ended. Nothing but that clause makes the outcome unknown, so
// enroll-session-key must answer ENROLL_OUTCOME_UNKNOWN with settle's cause, before the bound plus
// 1 s, where brokerCall's timer would answer. With the clause removed (mutation C,
// `false && ended`), the row answers ENROLL_FAILED; with the op's context left running past the
// bound, the timer answers instead, at the bound plus 1 s and with its own cause.
func TestAnEnrollWhoseBodyIsStillBeingWrittenAtTheBoundIsUnknown(t *testing.T) {
	r := newRig(t, "")
	r.login(t) // log in against the ordinary fake first, then point the Broker at the stand-in

	proto := pointBrokerAtHTTP2(t, r, func(w http.ResponseWriter, req *http.Request) {
		// Read the body — which never ends — until the client's bound cancels the request and
		// resets the stream; never answer.
		_, _ = io.Copy(io.Discard, req.Body)
	})
	const bound = time.Second
	r.srv.BrokerCallBound = bound

	start := time.Now()
	resp, err := Call(r.sock, Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"}, BrokerCallWait)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("enroll-session-key: %v", err)
	}
	wantHTTP2(t, proto)
	wantSettled(t, "enroll whose body is still being written at the bound", resp, CodeEnrollUnknown, elapsed, bound+time.Second)
}

// TestAnEnrollWhoseAnswerBeginsBeforeItsBodyIsWrittenIsUnknown holds settle through
// GotFirstResponseByte alone: the stand-in answers 200 at once, without reading the body, with a
// body that is not JSON. WroteRequest never fires (the request body never ends) and the context
// does not end, so GotFirstResponseByte is the only fact that says the request was sent, and the
// failure is a decode error. enroll-session-key must answer ENROLL_OUTCOME_UNKNOWN with settle's
// cause, before the bound: an answer at the bound or later could be the `connected && ended`
// clause's or brokerCall's timer's, not GotFirstResponseByte's. With traceReach's
// GotFirstResponseByte line removed, the row answers ENROLL_FAILED.
func TestAnEnrollWhoseAnswerBeginsBeforeItsBodyIsWrittenIsUnknown(t *testing.T) {
	r := newRig(t, "")
	r.login(t)

	proto := pointBrokerAtHTTP2(t, r, func(w http.ResponseWriter, req *http.Request) {
		// Answer at once, without reading the body, with a body that will not decode.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{"))
	})
	const bound = 5 * time.Second
	r.srv.BrokerCallBound = bound

	start := time.Now()
	resp, err := Call(r.sock, Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"}, BrokerCallWait)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("enroll-session-key: %v", err)
	}
	wantHTTP2(t, proto)
	wantSettled(t, "enroll whose answer begins before its body is written", resp, CodeEnrollUnknown, elapsed, bound)
}
