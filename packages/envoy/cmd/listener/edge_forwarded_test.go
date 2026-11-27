package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"authelia.com/provider/oauth2/handler/rfc9449"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// The edge listener behind Google's global external Application Load Balancer. Every request
// reaches it from a Google Front End proxy in 35.191.0.0/16 or 130.211.0.0/22, with the caller's
// address as the second-to-last entry of the one X-Forwarded-For line the balancer writes, and
// the balancer sends a body-less request that got a 502, 503 or 504 once more, with the same
// headers, its DPoP proof included.

const (
	// edgeLBProject is the project the edge of these rows serves.
	edgeLBProject = "PRIV"
	// edgeLBAddress is the balancer's own address, the last entry Google appends.
	edgeLBAddress = "34.120.0.1"
	// edgeLBToken is the collaborator's live token, of edgeLBProject.
	edgeLBToken = "access-token-of-the-collaborator"
)

// forwardedFor is the X-Forwarded-For line Google's balancer writes for a client at address.
func forwardedFor(address string) string { return address + ", " + edgeLBAddress }

// edgeLBPresentation is one request a caller signed: its Authorization and its DPoP proof, which a
// retry of the request sends again unchanged.
type edgeLBPresentation struct {
	method, path, body   string
	authorization, proof string
}

// presentEdgeLB is the request caller's client signs for method and path under token.
func presentEdgeLB(t *testing.T, caller *ciphertokentest.Caller, method, path, token, body string) edgeLBPresentation {
	t.Helper()
	signed := caller.Signed(t, method, listenerBrokerOrigin+path, token)
	return edgeLBPresentation{method: method, path: path, body: body,
		authorization: signed.Header.Get("Authorization"), proof: signed.Header.Get("DPoP")}
}

// through is the presentation as it reaches the listener over a connection from peer, carrying
// each of forwarded as one X-Forwarded-For line.
func (p edgeLBPresentation) through(peer string, forwarded ...string) *http.Request {
	request := httptest.NewRequest(p.method, p.path, strings.NewReader(p.body))
	request.Host = "127.0.0.1:9020"
	request.RemoteAddr = peer
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", p.authorization)
	request.Header.Set("DPoP", p.proof)
	for _, line := range forwarded {
		request.Header.Add("X-Forwarded-For", line)
	}
	return request
}

// edgeLBBroker is a stub broker that calls edgeLBToken the collaborator's live token, of
// edgeLBProject and bound to caller's key.
func edgeLBBroker(t *testing.T, caller *ciphertokentest.Caller) *ciphertokentest.StubBroker {
	t.Helper()
	broker := ciphertokentest.NewStubBroker(t)
	answer := sessionAnswer("ses_collaborator", "outside@example.com", caller.JKT())
	answer.Projects = []string{edgeLBProject}
	broker.Says(edgeLBToken, answer)
	return broker
}

// edgeLBTokens is the broker-token credential over the broker at brokerURL and replay
// (listenerBrokerTokensOver), with two introspections an address and a clock that does not move:
// no bucket refills between two requests however slowly the box runs them, so which request a
// bucket refuses is decided by the requests alone.
func edgeLBTokens(t *testing.T, brokerURL string, replay rfc9449.DPoPReplayStorage) *brokerTokens {
	t.Helper()
	frozen := time.Now()
	return listenerBrokerTokensOver(t, brokerURL, replay, func(options *ciphertoken.Options) {
		options.AddressLimit = ratelimit.Limit{Every: time.Hour, Burst: 2}
		options.Now = func() time.Time { return frozen }
	})
}

// edgeLBAuth is the edge's admission of edgeLBProject over tokens.
func edgeLBAuth(tokens *brokerTokens, logger *logging.Logger) listenerAPIAuth {
	return listenerAPIAuth{brokers: tokens, edgeProjects: []string{edgeLBProject}, logger: logger}
}

// noContent is a route that serves every request it is handed.
var noContent = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

// edgeLBSender sends GET /v1/sessions as caller through handler, the edge's admission over a
// stub broker that knows edgeLBToken alone.
type edgeLBSender struct {
	t       *testing.T
	caller  *ciphertokentest.Caller
	broker  *ciphertokentest.StubBroker
	handler http.Handler
	junk    int
}

// newEdgeLBSender is an edge over edgeLBTokens and an in-memory spent-proof store, logging to
// logged (nil discards the log).
func newEdgeLBSender(t *testing.T, logged io.Writer) *edgeLBSender {
	t.Helper()
	if logged == nil {
		logged = io.Discard
	}
	caller := ciphertokentest.NewCaller(t)
	broker := edgeLBBroker(t, caller)
	handler := apiAuth(edgeLBAuth(edgeLBTokens(t, broker.Server.URL, &spentProofsInMemory{}), logging.NewWithWriter("test-machine", logged)), noContent)
	return &edgeLBSender{t: t, caller: caller, broker: broker, handler: handler}
}

// send is one request under token, or under a token the broker never issued, a fresh one each
// call, when token is empty, from peer with each of forwarded as one X-Forwarded-For line. It
// answers the response.
func (s *edgeLBSender) send(token, peer string, forwarded ...string) *httptest.ResponseRecorder {
	s.t.Helper()
	if token == "" {
		s.junk++
		token = fmt.Sprintf("junk-token-%d", s.junk)
	}
	recorder := httptest.NewRecorder()
	s.handler.ServeHTTP(recorder, presentEdgeLB(s.t, s.caller, http.MethodGet, "/v1/sessions", token, "").through(peer, forwarded...))
	return recorder
}

// A stranger behind Google's proxies spends the introspection bucket of its own address, the
// second-to-last X-Forwarded-For entry, and no other caller's: tokens the broker never issued,
// sent through one proxy, leave the collaborator behind the same proxy its own bucket, and the
// stranger's bucket follows it to a proxy of the other range. An IPv6 caller is keyed by its /64,
// as the broker's own edge keys it, so one host cannot take a fresh bucket from each of its
// addresses, and an IPv4 address written as IPv6 is that IPv4 address, the proxy's and the
// caller's alike.
func TestEdgeForwardedAddressKeysEachCallersOwnBucket(t *testing.T) {
	sender := newEdgeLBSender(t, nil)
	for _, step := range []struct {
		what        string
		token, peer string
		forwarded   string
		want        int
	}{
		{what: "the stranger's first junk token", peer: "35.191.7.7:40001", forwarded: "198.51.100.1", want: http.StatusUnauthorized},
		{what: "the stranger's second junk token", peer: "35.191.7.7:40002", forwarded: "198.51.100.1", want: http.StatusUnauthorized},
		{what: "the stranger's third junk token, past its bucket", peer: "35.191.7.7:40003", forwarded: "198.51.100.1", want: http.StatusTooManyRequests},
		{what: "the collaborator through the same proxy", token: edgeLBToken, peer: "35.191.7.7:40004", forwarded: "203.0.113.9", want: http.StatusNoContent},
		{what: "the stranger through a proxy of the other range", peer: "130.211.1.2:40005", forwarded: "198.51.100.1", want: http.StatusTooManyRequests},
		{what: "an IPv6 stranger's first junk token", peer: "35.191.7.8:40006", forwarded: "2001:db8:1:2::1", want: http.StatusUnauthorized},
		{what: "its second, from another address of its /64", peer: "35.191.7.8:40007", forwarded: "2001:db8:1:2::2", want: http.StatusUnauthorized},
		{what: "its third, from a third address of its /64", peer: "35.191.7.8:40008", forwarded: "2001:db8:1:2::ff", want: http.StatusTooManyRequests},
		{what: "a caller of the next /64 through the same proxy", peer: "35.191.7.8:40009", forwarded: "2001:db8:1:3::1", want: http.StatusUnauthorized},
		{what: "the first stranger written as IPv6, through a proxy written as IPv6", peer: "[::ffff:35.191.7.9]:40010", forwarded: "::ffff:198.51.100.1", want: http.StatusTooManyRequests},
		{what: "the first stranger through a proxy whose address carries a zone", peer: "[::ffff:35.191.7.10%eth0]:40011", forwarded: "198.51.100.1", want: http.StatusTooManyRequests},
		{what: "the first stranger through Google's IPv6 proxy range", peer: "[2600:2d00:1:1::5]:40012", forwarded: "198.51.100.1", want: http.StatusTooManyRequests},
		{what: "a peer just outside that range, forwarded for the first stranger", peer: "[2600:2d00:1:2::5]:40013", forwarded: "198.51.100.1", want: http.StatusUnauthorized},
	} {
		if got := sender.send(step.token, step.peer, forwardedFor(step.forwarded)).Code; got != step.want {
			t.Errorf("%s (peer %s, forwarded for %s): status %d, want %d", step.what, step.peer, step.forwarded, got, step.want)
		}
	}
}

// A peer outside Google's ranges is keyed by its own connection, as every request was before the
// edge read a forwarded address, and its X-Forwarded-For chooses nothing: naming the
// collaborator's address spends the peer's own bucket and not the collaborator's, naming another
// address does not give it a fresh one, and two lines or one entry are not refused, since nothing
// is read from them.
func TestEdgeForwardedAddressFromAnyOtherPeerChoosesNothing(t *testing.T) {
	sender := newEdgeLBSender(t, nil)
	for _, step := range []struct {
		what        string
		token, peer string
		forwarded   []string
		want        int
	}{
		{what: "a spoofer naming the collaborator's address", peer: "192.0.2.10:40001", forwarded: []string{forwardedFor("203.0.113.9")}, want: http.StatusUnauthorized},
		{what: "its second junk token, naming it again", peer: "192.0.2.10:40002", forwarded: []string{forwardedFor("203.0.113.9")}, want: http.StatusUnauthorized},
		{what: "its third, naming an address of its choice", peer: "192.0.2.10:40003", forwarded: []string{forwardedFor("198.51.100.77")}, want: http.StatusTooManyRequests},
		{what: "the collaborator behind Google's proxy, whose address the spoofer named", token: edgeLBToken, peer: "35.191.7.7:40004", forwarded: []string{forwardedFor("203.0.113.9")}, want: http.StatusNoContent},
		{what: "another peer's two lines", token: edgeLBToken, peer: "192.0.2.11:40005", forwarded: []string{"203.0.113.50", forwardedFor("203.0.113.51")}, want: http.StatusNoContent},
		{what: "another peer's line of one entry", token: edgeLBToken, peer: "192.0.2.12:40006", forwarded: []string{"203.0.113.52"}, want: http.StatusNoContent},
	} {
		if got := sender.send(step.token, step.peer, step.forwarded...).Code; got != step.want {
			t.Errorf("%s (peer %s, X-Forwarded-For %q): status %d, want %d", step.what, step.peer, step.forwarded, got, step.want)
		}
	}
}

// A request from Google's proxies whose X-Forwarded-For is not the one line Google writes - none,
// two lines, one entry, or an entry there that is no address - is refused 400 before anything of
// it is judged: the broker is asked nothing and no bucket is spent, since such a request names no
// caller to charge, and reading the wrong entry would hand the choice of a bucket to the caller.
func TestEdgeForwardedAddressRefusesWhatGoogleDidNotForward(t *testing.T) {
	var logged bytes.Buffer
	sender := newEdgeLBSender(t, &logged)
	for _, tc := range []struct {
		name      string
		forwarded []string
	}{
		{name: "no X-Forwarded-For"},
		{name: "two lines", forwarded: []string{"198.51.100.9", forwardedFor("203.0.113.9")}},
		{name: "one entry", forwarded: []string{"203.0.113.9"}},
		{name: "an empty entry", forwarded: []string{" , " + edgeLBAddress}},
		{name: "an entry that is no address", forwarded: []string{"unknown, " + edgeLBAddress}},
	} {
		logged.Reset()
		asked := sender.broker.Count()
		recorder := sender.send(edgeLBToken, "35.191.7.7:40001", tc.forwarded...)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "X-Forwarded-For") {
			t.Errorf("%s: status %d body %s, want 400 naming X-Forwarded-For", tc.name, recorder.Code, recorder.Body.String())
		}
		if got := sender.broker.Count(); got != asked {
			t.Errorf("%s: the broker was asked %d times about a request the edge refused", tc.name, got-asked)
		}
		if !strings.Contains(logged.String(), "listener: edge request without the address Google's load balancer forwards") {
			t.Errorf("%s: the refusal logged no line saying why: %s", tc.name, logged.String())
		}
	}
	// The control: the same request with Google's one line is served, in the form Google writes it,
	// CLIENT_IP,FORWARDING_RULE_IP with no space.
	if recorder := sender.send(edgeLBToken, "35.191.7.7:40002", "203.0.113.9,"+edgeLBAddress); recorder.Code != http.StatusNoContent {
		t.Errorf("Google's own line: status %d body %s, want 204", recorder.Code, recorder.Body.String())
	}
}

// A listener that is no edge reads no forwarded address, whoever its peer is: a request from
// Google's ranges is keyed on its connection, its X-Forwarded-For chooses nothing, and a header of
// two lines is no refusal.
func TestEdgeForwardedAddressIsReadByTheEdgeAlone(t *testing.T) {
	sender := newEdgeLBSender(t, nil)
	sender.handler = apiAuth(listenerAPIAuth{brokers: edgeLBTokens(t, sender.broker.Server.URL, &spentProofsInMemory{}),
		logger: logging.NewWithWriter("test-machine", io.Discard)}, noContent)
	for _, step := range []struct {
		what        string
		token, peer string
		forwarded   []string
		want        int
	}{
		{what: "two lines from Google's range", token: edgeLBToken, peer: "35.191.7.7:40001", forwarded: []string{"198.51.100.9", forwardedFor("203.0.113.9")}, want: http.StatusNoContent},
		{what: "a junk token forwarded for one address", peer: "35.191.7.8:40002", forwarded: []string{forwardedFor("198.51.100.1")}, want: http.StatusUnauthorized},
		{what: "a second, forwarded for it again", peer: "35.191.7.8:40003", forwarded: []string{forwardedFor("198.51.100.1")}, want: http.StatusUnauthorized},
		{what: "a third from the same proxy, forwarded for another address", peer: "35.191.7.8:40004", forwarded: []string{forwardedFor("198.51.100.2")}, want: http.StatusTooManyRequests},
	} {
		if got := sender.send(step.token, step.peer, step.forwarded...).Code; got != step.want {
			t.Errorf("%s (peer %s, X-Forwarded-For %q): status %d, want %d", step.what, step.peer, step.forwarded, got, step.want)
		}
	}
}
