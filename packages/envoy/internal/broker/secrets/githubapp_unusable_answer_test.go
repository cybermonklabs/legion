package secrets_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
)

// tokenAnswerFault is how the answer to a token request breaks on its way back from GitHub. Each
// fault but answerCut500 comes after GitHub has minted the token, so GitHub holds a live token and
// the minter gets no answer it can use. The empty fault is the whole answer, as GitHub sends it.
type tokenAnswerFault string

const (
	// answerWhole is the stand-in's own answer, unbroken.
	answerWhole tokenAnswerFault = ""
	// answerLost closes the connection before any of the answer arrives.
	answerLost tokenAnswerFault = "lost"
	// answerCut sends the 201 and the answer up to just past the token, and closes the connection.
	answerCut tokenAnswerFault = "cut off"
	// answerStalled sends the 201 and the answer up to just past the token, and then sends nothing
	// more until the caller hangs up: what a caller whose own context ends during the read sees.
	answerStalled tokenAnswerFault = "stalled until the caller's context ends"
	// answerPastCap sends a 201 whose answer runs past the cap the minter reads a token answer to,
	// with the token first, where GitHub writes it.
	answerPastCap tokenAnswerFault = "past the cap"
	// answerPastCapTokenLast is that answer with the token only after the cap, which no read of
	// the cap can find.
	answerPastCapTokenLast tokenAnswerFault = "past the cap, its token after it"
	// answerUndecodable sends a 201 whose expires_at is no time, so the answer does not decode.
	answerUndecodable tokenAnswerFault = "undecodable"
	// answerNoExpiry sends a 201 with the token and no expires_at.
	answerNoExpiry tokenAnswerFault = "with no expiry"
	// answerNoToken sends a whole 201 that names no token.
	answerNoToken tokenAnswerFault = "whole, with no token"
	// answerCut500 mints nothing and sends a 500 whose body is cut off part of the way. The body
	// begins with a member named token, which no answer of GitHub's other than a 201 holds, so a
	// minter that read a token out of an answer whatever its status would send a revoke for it.
	answerCut500 tokenAnswerFault = "a 500 cut off"
	// answerUnreachable is no answer at all: the minter's transport fails the request as a refused
	// dial does, before anything reaches GitHub, so GitHub mints nothing. answerUnresolved fails it
	// as a name that did not resolve does, and answerUntrusted as a TLS handshake whose certificate
	// the broker does not trust does.
	answerUnreachable tokenAnswerFault = "unreachable"
	answerUnresolved  tokenAnswerFault = "a name that did not resolve"
	answerUntrusted   tokenAnswerFault = "a certificate the broker does not trust"
	// answerWriteFails has GitHub receive the whole request and mint its token, while the minter's
	// own write of that request reports an error, as a connection that fails as the last bytes
	// leave can; GitHub then closes the connection with no answer.
	answerWriteFails tokenAnswerFault = "received whole, though the write reported an error"
	// answerReset has GitHub receive the whole request, mint its token and reset the connection
	// with no answer, so the minter's read fails as a *net.OpError that is no dial's.
	answerReset tokenAnswerFault = "reset after it was received"
	// answerBadRecord has GitHub receive the whole request, mint its token and answer it, while
	// the minter's transport fails the call with a TLS record error, as a record that does not
	// parse after the handshake fails; answerAlert does so with a TLS alert the peer sent after
	// the handshake. Either is a TLS error that comes after the request went out.
	answerBadRecord tokenAnswerFault = "a TLS record error after the request went out"
	answerAlert     tokenAnswerFault = "a TLS alert after the request went out"
)

// pastTheCap is more than the minter reads of a token answer (secrets' accessTokensResponseLimit,
// 1 MiB and 64 KiB for each of 100 repositories, 7.25 MiB in all).
const pastTheCap = 8 << 20

// breakingGitHub is the airlock GitHub stand-in, installed on acme-org (1) and ada (2) and minting
// a token of its own for every token request (GitHubOptions.DistinctTokens), as GitHub does, with
// the answer to its nth token request broken as faults[n] says and every answer past the list
// whole. The stand-in mints each token before its answer breaks, but for answerCut500, so it holds
// every one as live as GitHub would, and its revoke route forgets the one it is given, unless
// refuseRevokes has GitHub answer every revoke 500. client is the transport a minter reaches it
// through, which fails each answerUnreachable, answerUnresolved or answerUntrusted request before
// it is sent and reports an error for the write of each answerWriteFails one; silentClient is the
// same but for its last step, a transport of its own that tells a request's trace nothing, as a
// transport a caller injects need not; partialClient is client behind a wrapper that forwards
// the standard transport's GetConn to the request's trace and not its GotConn, as a wrapper need
// not forward every hook; registeredClient sends through an *http.Transport that hands every
// http request to that same wrapper, a protocol registered on it. Every other route is the
// stand-in's own. minted is every token it minted, in order.
type breakingGitHub struct {
	service          *airlocktest.Service
	client           *http.Client
	silentClient     *http.Client
	partialClient    *http.Client
	registeredClient *http.Client
	faults           []tokenAnswerFault
	// received is told each time GitHub has received an answerWriteFails request whole.
	received chan struct{}

	mu       sync.Mutex
	asked    int
	minted   []string
	revokes  int
	refusing bool
}

func newBreakingGitHub(t *testing.T, refuseRevokes bool, faults ...tokenAnswerFault) *breakingGitHub {
	t.Helper()
	stand := airlocktest.NewGitHubStand(airlocktest.GitHubOptions{Installations: map[string]int64{"acme-org": 1, "ada": 2}, DistinctTokens: true})
	service, err := airlocktest.NewService(airlocktest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(service.Close)
	g := &breakingGitHub{service: service, faults: faults, refusing: refuseRevokes, received: make(chan struct{}, 8)}
	dialer := &net.Dialer{}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return writeFailing{Conn: connection, github: g}, nil
	}}
	t.Cleanup(transport.CloseIdleConnections)
	g.client = &http.Client{Transport: unreachableFirst{github: g, next: transport}}
	g.silentClient = &http.Client{Transport: unreachableFirst{github: g, next: silentTransport{}}}
	g.partialClient = &http.Client{Transport: partialTrace{next: g.client.Transport}}
	registered := &http.Transport{}
	registered.RegisterProtocol("http", partialTrace{next: g.client.Transport})
	g.registeredClient = &http.Client{Transport: registered}
	service.AnswerWith(func(w http.ResponseWriter, r *http.Request, seen airlocktest.Request) bool {
		if seen.Method == http.MethodDelete && seen.Path == "/installation/token" {
			g.mu.Lock()
			g.revokes++
			refusing := g.refusing
			g.mu.Unlock()
			if refusing {
				http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
				return true
			}
			return stand.Answer(w, r, seen)
		}
		if seen.Method != http.MethodPost || !strings.HasSuffix(seen.Path, "/access_tokens") {
			return stand.Answer(w, r, seen)
		}
		fault := g.next()
		if fault == answerCut500 {
			writeCut(w, "500 Internal Server Error", []byte(`{"token":"ghs_not_a_minted_token","message":"Server Error","status":"500"}`), 30)
			return true
		}
		answered := httptest.NewRecorder()
		stand.Answer(answered, r, seen)
		var members map[string]json.RawMessage
		if answered.Code != http.StatusCreated || json.Unmarshal(answered.Body.Bytes(), &members) != nil {
			http.Error(w, fmt.Sprintf("the stand-in answered the token request %d, want a minted token", answered.Code), http.StatusInternalServerError)
			return true
		}
		var token string
		_ = json.Unmarshal(members["token"], &token)
		g.mu.Lock()
		g.minted = append(g.minted, token)
		g.mu.Unlock()
		if fault == answerWriteFails {
			g.received <- struct{}{}
			fault = answerLost
		}
		breakAnswer(w, fault, members)
		return true
	})
	return g
}

// next is the fault of the token request asked now, counting the ones the transport failed.
func (g *breakingGitHub) next() tokenAnswerFault {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked++
	if g.asked <= len(g.faults) {
		return g.faults[g.asked-1]
	}
	return answerWhole
}

// peek is the fault the next token request will get, without counting it.
func (g *breakingGitHub) peek() tokenAnswerFault {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.asked < len(g.faults) {
		return g.faults[g.asked]
	}
	return answerWhole
}

// unreachableFirst is the minter's transport: a token request whose fault is answerUnreachable,
// answerUnresolved or answerUntrusted fails as a refused dial, a name that did not resolve or a
// certificate the broker does not trust does, and reaches nothing; one whose fault is
// answerBadRecord or answerAlert reaches GitHub, which mints and answers, and then fails with a
// TLS record error or alert; every other request goes on to next.
type unreachableFirst struct {
	github *breakingGitHub
	next   http.RoundTripper
}

func (u unreachableFirst) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/access_tokens") {
		return u.next.RoundTrip(request)
	}
	var failed error
	switch fault := u.github.peek(); fault {
	case answerUnreachable:
		failed = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	case answerUnresolved:
		failed = &net.DNSError{Err: "no such host", Name: "api.github.com", IsNotFound: true}
	case answerUntrusted:
		failed = &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	case answerBadRecord, answerAlert:
		// The stand-in takes this fault itself (next), mints and answers whole; the answer
		// never reaches the minter.
		response, err := u.next.RoundTrip(request)
		if err == nil {
			_ = response.Body.Close()
		}
		if fault == answerBadRecord {
			return nil, tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}
		}
		return nil, &net.OpError{Op: "remote error", Err: tls.AlertError(20)}
	default:
		return u.next.RoundTrip(request)
	}
	u.github.next()
	if request.Body != nil {
		_ = request.Body.Close()
	}
	return nil, failed
}

// silentTransport sends each request over a connection of its own, one request to a connection,
// and tells the request's trace nothing: no GotConn, no WroteRequest, since it writes a copy of
// the request whose context holds no trace. A transport a caller injects into the minter
// (GitHubApp.HTTP) owes the trace nothing, so the minter cannot read a silent trace as a request
// that never left.
type silentTransport struct{}

func (silentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	connection, err := (&net.Dialer{}).DialContext(request.Context(), "tcp", request.URL.Host)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(request.Context(), func() { _ = connection.Close() })
	if err := request.Clone(context.Background()).Write(connection); err != nil {
		stop()
		_ = connection.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), request)
	if err != nil {
		stop()
		_ = connection.Close()
		return nil, err
	}
	response.Body = closingBody{ReadCloser: response.Body, connection: connection, stop: stop}
	return response, nil
}

// partialTrace sends each request through next under a trace of its own that forwards the
// caller's GetConn and not its GotConn: a wrapper owes the caller's trace none of its hooks.
type partialTrace struct{ next http.RoundTripper }

func (p partialTrace) RoundTrip(request *http.Request) (*http.Response, error) {
	forwarded := &httptrace.ClientTrace{}
	if trace := httptrace.ContextClientTrace(request.Context()); trace != nil {
		forwarded.GetConn = trace.GetConn
	}
	return p.next.RoundTrip(request.WithContext(httptrace.WithClientTrace(withoutTrace{request.Context()}, forwarded)))
}

// withoutTrace is a context that hides the trace its parent carries, and nothing else.
type withoutTrace struct{ context.Context }

func (c withoutTrace) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		if _, isTrace := value.(*httptrace.ClientTrace); isTrace {
			return nil
		}
		return value
	}
	return nil
}

// closingBody closes its connection once the answer's body is closed.
type closingBody struct {
	io.ReadCloser
	connection net.Conn
	stop       func() bool
}

func (b closingBody) Close() error {
	b.stop()
	err := b.ReadCloser.Close()
	_ = b.connection.Close()
	return err
}

// writeFailing is a connection to the stand-in whose write of the last bytes of an answerWriteFails
// token request - its JSON body, the one body any request of this test carries - reports an error,
// once GitHub has received the request whole and minted for it.
type writeFailing struct {
	net.Conn
	github *breakingGitHub
}

func (c writeFailing) Write(p []byte) (int, error) {
	failing := bytes.HasSuffix(p, []byte("}")) && c.github.peek() == answerWriteFails
	n, err := c.Conn.Write(p)
	if !failing || err != nil || n != len(p) {
		return n, err
	}
	select {
	case <-c.github.received:
		return n, io.ErrUnexpectedEOF
	case <-time.After(5 * time.Second):
		return n, fmt.Errorf("the stand-in never received the request")
	}
}

// breakAnswer writes the stand-in's minted answer, members, broken as fault says.
func breakAnswer(w http.ResponseWriter, fault tokenAnswerFault, members map[string]json.RawMessage) {
	whole := orderedAnswer(members, "token", "expires_at", "permissions", "repository_selection", "repositories")
	padding := []byte(`"padding":"` + strings.Repeat("x", pastTheCap) + `"`)
	// The token member whole and the start of the next one: the answer GitHub began, cut off.
	pastToken := len(`{"token":`) + len(members["token"]) + len(`,"expires_at"`)
	switch fault {
	case answerWhole, answerBadRecord, answerAlert:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(whole)
	case answerLost:
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	case answerReset:
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		if tcp, isTCP := connection.(*net.TCPConn); isTCP {
			_ = tcp.SetLinger(0)
		}
		_ = connection.Close()
	case answerCut:
		writeCut(w, "201 Created", whole, pastToken)
	case answerStalled:
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		fmt.Fprintf(buffered, "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(whole))
		_, _ = buffered.Write(whole[:pastToken])
		_ = buffered.Flush()
		// Nothing more until the caller hangs up, or ten seconds: a caller whose context ends
		// closes the connection, which ends this read.
		_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, _ = io.Copy(io.Discard, connection)
		_ = connection.Close()
	case answerPastCap:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(append(append(append(whole[:len(whole)-1:len(whole)-1], ','), padding...), '}'))
	case answerPastCapTokenLast:
		w.WriteHeader(http.StatusCreated)
		rest := orderedAnswer(members, "expires_at", "permissions", "repository_selection", "repositories", "token")
		_, _ = w.Write(append(append(append([]byte{'{'}, padding...), ','), rest[1:]...))
	case answerUndecodable:
		members["expires_at"] = json.RawMessage(`"soon"`)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(orderedAnswer(members, "token", "expires_at", "permissions", "repository_selection", "repositories"))
	case answerNoExpiry:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(orderedAnswer(members, "token", "permissions", "repository_selection", "repositories"))
	case answerNoToken:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(orderedAnswer(members, "expires_at", "permissions", "repository_selection", "repositories"))
	}
}

// writeCut sends an answer of status whose body is announced whole and cut off after its first
// cut bytes, then closes the connection.
func writeCut(w http.ResponseWriter, status string, body []byte, cut int) {
	connection, buffered, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	fmt.Fprintf(buffered, "HTTP/1.1 %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", status, len(body))
	_, _ = buffered.Write(body[:cut])
	_ = buffered.Flush()
	_ = connection.Close()
}

// orderedAnswer is a JSON object of members' entries named in order, in that order, leaving out a
// name members does not hold: GitHub writes the token first, which a map's encoding does not.
func orderedAnswer(members map[string]json.RawMessage, order ...string) []byte {
	var answer bytes.Buffer
	answer.WriteByte('{')
	for _, name := range order {
		value, held := members[name]
		if !held {
			continue
		}
		if answer.Len() > 1 {
			answer.WriteByte(',')
		}
		fmt.Fprintf(&answer, "%q:%s", name, value)
	}
	answer.WriteByte('}')
	return answer.Bytes()
}

// tokens is every token the stand-in minted, one for each token request it served.
func (g *breakingGitHub) tokens() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.minted...)
}

// revoked is how many revokes the stand-in was sent, answered or refused.
func (g *breakingGitHub) revoked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.revokes
}

// live is the tokens the stand-in still holds as live: a read made with each is answered as by a
// credential it minted (200 for a repository the token reaches, 404 for one it does not), where a
// token it forgot at its revoke is answered 401, as one it never minted is.
func (g *breakingGitHub) live(t *testing.T, tokens []string) []string {
	t.Helper()
	var live []string
	for _, token := range tokens {
		if reachesWith(t, g.service, token) != http.StatusUnauthorized {
			live = append(live, token)
		}
	}
	return live
}

// The words of the three lines a token nobody holds writes when it stays live.
const (
	saysNotRevokedUnusable   = "did not revoke a token it minted whose answer the broker could not use"
	saysNotRevokedUnreleased = "did not revoke a token a release minted and handed to nobody"
	saysUnrevokable          = "cannot revoke, since the answer it holds does not name it"
)

// warned is how many lines of log say says, and how many of those are written at WARN, as a
// text handler writes the level and then the message. operate.md documents each line a token
// nobody holds writes, at WARN, so a line that says it at another level is not the line a reader
// of the guide searches the log for.
func warned(log, says string) (lines, warnings int) {
	warn := regexp.MustCompile(`level=WARN msg="[^"]*` + regexp.QuoteMeta(says))
	for line := range strings.Lines(log) {
		if strings.Contains(line, says) {
			lines++
			if warn.MatchString(line) {
				warnings++
			}
		}
	}
	return lines, warnings
}

// TestATokenWhoseAnswerTheMinterCannotUseIsGivenBack: GitHub mints a token when it answers a token
// request 201, before the answer reaches the minter, so an answer that is lost, cut off, past the
// minter's cap, undecodable, without an expiry or without a token leaves a live token at GitHub
// that nobody was handed. The stand-in mints a token of its own for every request, so every token
// is read on its own: each token whose answer names it is revoked before the request is sent again
// or the mint answers, under a bound of its own that the caller's context does not end, so a
// release that rides out a cut-off answer hands over the second token and leaves the first one
// dead, a release that fails at its second installation revokes the token of its first, which it
// would have handed over, and a check or a release whose own context ends during the read still
// revokes the token. A check (MintRequest.Check) mints before anybody approved anything, so it
// sends a token request whose answer was lost after the request reached GitHub once, leaving it
// one token it cannot revoke rather than one for each attempt, but sends again a request that
// never reached GitHub, such as one whose connection was refused, since that minted nothing; a
// release sends either again, and each answer it loses leaves its token live, which nothing can
// name. A cut-off answer of any status but 201 minted nothing, and nothing is revoked for it. A
// revoke GitHub refuses is logged, and its token stays live. Every unusable answer is the
// upstream's trouble rather than a refusal, so the broker's narrowing check lets the request
// through to its approver, and no error or log line holds a token. An answer whose part the
// minter holds does not name its token is logged as a token nothing can revoke.
func TestATokenWhoseAnswerTheMinterCannotUseIsGivenBack(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	value := githubAppValue(t, "123456", key)
	lean := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"lean"}, ReadOnly: true}
	// wide names 100 repositories of 93 characters each, so its token request's body is far larger
	// than the transport's write buffer and goes to the connection in writes of its own, the last of
	// which is the one answerWriteFails reports an error for.
	wide := &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}
	for i := range 100 {
		wide.Repositories = append(wide.Repositories, fmt.Sprintf("%02d-%s", i, strings.Repeat("r", 90)))
	}

	for _, tc := range []struct {
		name  string
		check bool
		// narrowing is the run's narrowing, and nil for an owner-wide release of both accounts.
		narrowing *secrets.GitHubNarrowing
		faults    []tokenAnswerFault
		// refuseRevokes has GitHub answer every revoke 500.
		refuseRevokes bool
		// silent has the minter reach GitHub through a transport that tells the trace nothing.
		silent bool
		// partial has the minter reach GitHub through a wrapper that forwards GetConn alone.
		partial bool
		// registered has the minter reach GitHub through an *http.Transport whose http protocol
		// is registered to that wrapper.
		registered bool
		// deadline ends the caller's context that long after the run starts, and zero leaves it
		// the test's own.
		deadline time.Duration
		// requests is the token requests GitHub served, each of which minted a token, but for the
		// 500s of answerCut500, which mint none.
		requests int
		// live is how many of those tokens are still live after the run: a lost answer's, which
		// nothing can name, an answer's whose part the minter holds names no token, one whose
		// revoke GitHub refused, and the one token a release hands over.
		live int
		// released is whether the run hands over a value, and verdict whether a check answers
		// that the release would have succeeded.
		released, verdict bool
		// noRevoke is whether the run must send GitHub no revoke at all.
		noRevoke bool
		// says is every line the run must log, and unrevokable whether it logs that GitHub holds a
		// token nothing can revoke: a 201 whose part the minter holds names no token. A
		// connection lost before any answer carries no 201, so nothing says a token was minted.
		says        []string
		unrevokable bool
	}{
		{name: "an answer lost, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerLost, answerLost, answerLost}, requests: 1, live: 1},
		{name: "an answer lost, to a release", narrowing: lean, faults: []tokenAnswerFault{answerLost, answerLost, answerLost}, requests: 3, live: 3},
		{name: "an answer cut off, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerCut, answerCut, answerCut}, requests: 1},
		{name: "an answer cut off, to a release", narrowing: lean, faults: []tokenAnswerFault{answerCut, answerCut, answerCut}, requests: 3},
		{name: "an answer cut off and then a whole one, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerCut}, requests: 1},
		{name: "an answer cut off and then a whole one, to a release", narrowing: lean, faults: []tokenAnswerFault{answerCut}, requests: 2, live: 1, released: true},
		{name: "an answer the caller's context ends during, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerStalled}, deadline: 300 * time.Millisecond, requests: 1},
		{name: "an answer the caller's context ends during, to a release", narrowing: lean, faults: []tokenAnswerFault{answerStalled}, deadline: 300 * time.Millisecond, requests: 1},
		{name: "a release of two accounts whose caller's context ends during the second answer", faults: []tokenAnswerFault{answerWhole, answerStalled}, deadline: 300 * time.Millisecond, requests: 2},
		{name: "an answer past the cap, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerPastCap}, requests: 1},
		{name: "an answer past the cap, to a release", narrowing: lean, faults: []tokenAnswerFault{answerPastCap}, requests: 1},
		{name: "an answer past the cap with its token after it, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerPastCapTokenLast}, requests: 1, live: 1, unrevokable: true},
		{name: "an answer past the cap with its token after it, to a release", narrowing: lean, faults: []tokenAnswerFault{answerPastCapTokenLast}, requests: 1, live: 1, unrevokable: true},
		{name: "an undecodable answer, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerUndecodable}, requests: 1},
		{name: "an undecodable answer, to a release", narrowing: lean, faults: []tokenAnswerFault{answerUndecodable}, requests: 1},
		{name: "an answer with no expiry, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerNoExpiry}, requests: 1},
		{name: "an answer with no expiry, to a release", narrowing: lean, faults: []tokenAnswerFault{answerNoExpiry}, requests: 1},
		{name: "a whole answer with no token, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerNoToken}, requests: 1, live: 1, unrevokable: true},
		{name: "a whole answer with no token, to a release", narrowing: lean, faults: []tokenAnswerFault{answerNoToken}, requests: 1, live: 1, unrevokable: true},
		{name: "a 500 cut off, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerCut500, answerCut500, answerCut500}, noRevoke: true},
		{name: "a 500 cut off, to a release", narrowing: lean, faults: []tokenAnswerFault{answerCut500, answerCut500, answerCut500}, noRevoke: true},
		{name: "a token request that never reached GitHub, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerUnreachable}, requests: 1, verdict: true},
		{name: "a token request that never reached GitHub, to a release", narrowing: lean, faults: []tokenAnswerFault{answerUnreachable}, requests: 1, live: 1, released: true},
		{name: "a token request whose name did not resolve, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerUnresolved}, requests: 1, verdict: true},
		{name: "a token request whose TLS handshake the broker refused, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerUntrusted}, requests: 1, verdict: true},
		{name: "a token request GitHub received whole though its write reported an error, to a check", check: true, narrowing: wide,
			faults: []tokenAnswerFault{answerWriteFails, answerWriteFails, answerWriteFails}, requests: 1, live: 1},
		{name: "a token request GitHub received whole though its write reported an error, to a release", narrowing: wide,
			faults: []tokenAnswerFault{answerWriteFails, answerWriteFails, answerWriteFails}, requests: 3, live: 3},
		{name: "a connection GitHub reset after it received the request, to a check", check: true, narrowing: lean,
			faults: []tokenAnswerFault{answerReset, answerReset, answerReset}, requests: 1, live: 1},
		{name: "a connection GitHub reset after it received the request, to a release", narrowing: lean,
			faults: []tokenAnswerFault{answerReset, answerReset, answerReset}, requests: 3, live: 3},
		{name: "a TLS record error after the request went out, to a check", check: true, narrowing: lean,
			faults: []tokenAnswerFault{answerBadRecord, answerBadRecord, answerBadRecord}, requests: 1, live: 1},
		{name: "a TLS alert after the request went out, to a check", check: true, narrowing: lean,
			faults: []tokenAnswerFault{answerAlert, answerAlert, answerAlert}, requests: 1, live: 1},
		{name: "a TLS record error after the request went out, to a release", narrowing: lean,
			faults: []tokenAnswerFault{answerBadRecord, answerBadRecord, answerBadRecord}, requests: 3, live: 3},
		{name: "an answer lost through a transport that reports no connection, to a check", check: true, narrowing: lean, silent: true,
			faults: []tokenAnswerFault{answerLost, answerLost, answerLost}, requests: 1, live: 1},
		{name: "an answer lost through a transport that reports no connection, to a release", narrowing: lean, silent: true,
			faults: []tokenAnswerFault{answerLost, answerLost, answerLost}, requests: 3, live: 3},
		{name: "a token request that never reached GitHub through a transport that reports no connection, to a check", check: true, narrowing: lean, silent: true,
			faults: []tokenAnswerFault{answerUnreachable}, requests: 1, verdict: true},
		{name: "an answer lost through a wrapper that forwards GetConn and not GotConn, to a check", check: true, narrowing: lean, partial: true,
			faults: []tokenAnswerFault{answerLost, answerLost, answerLost}, requests: 1, live: 1},
		{name: "an answer lost through an *http.Transport whose protocol is registered to that wrapper, to a check", check: true, narrowing: lean, registered: true,
			faults: []tokenAnswerFault{answerLost, answerLost, answerLost}, requests: 1, live: 1},
		{name: "an undecodable answer whose revoke GitHub refuses, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerUndecodable}, refuseRevokes: true, requests: 1, live: 1, says: []string{saysNotRevokedUnusable}},
		{name: "an undecodable answer whose revoke GitHub refuses, to a release", narrowing: lean, faults: []tokenAnswerFault{answerUndecodable}, refuseRevokes: true, requests: 1, live: 1, says: []string{saysNotRevokedUnusable}},
		{name: "a release of two accounts whose second answer is undecodable", faults: []tokenAnswerFault{answerWhole, answerUndecodable}, requests: 2},
		{name: "a release of two accounts whose second answer is undecodable and whose revokes GitHub refuses", faults: []tokenAnswerFault{answerWhole, answerUndecodable}, refuseRevokes: true, requests: 2, live: 2,
			says: []string{saysNotRevokedUnusable, saysNotRevokedUnreleased}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			github := newBreakingGitHub(t, tc.refuseRevokes, tc.faults...)
			retry, _, _ := testRetry(atTheBottom)
			client := github.client
			if tc.silent {
				client = github.silentClient
			}
			if tc.partial {
				client = github.partialClient
			}
			if tc.registered {
				client = github.registeredClient
			}
			app := secrets.GitHubApp{Base: github.service.URL(), HTTP: client, Retry: retry}

			ctx := t.Context()
			if tc.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.deadline)
				defer cancel()
			}
			owners := []string{"ada"}
			if tc.narrowing == nil {
				owners = []string{"acme-org", "ada"}
			}
			minted, err := app.Mint(ctx, secrets.MintRequest{Value: value, GitHubOwners: owners, GitHubNarrowing: tc.narrowing, Check: tc.check})
			switch {
			case tc.released:
				if err != nil || minted.Value == "" {
					t.Fatalf("the release answered %v, handing over %d bytes; want a value", err, len(minted.Value))
				}
			case tc.verdict:
				if err != nil || minted.Value != "" {
					t.Fatalf("the check answered %q, %v; want nil and nothing handed over: the release would have succeeded", minted.Value, err)
				}
			case minted.Value != "" || secrets.MintErrorClass(err) != "upstream":
				t.Fatalf("the run answered %q, %v (class %s); want nothing handed over and the upstream's trouble, which lets the request through to its approver",
					minted.Value, err, secrets.MintErrorClass(err))
			}
			tokens := github.tokens()
			if len(tokens) != tc.requests {
				t.Fatalf("GitHub minted %d tokens for the run, want %d", len(tokens), tc.requests)
			}
			for _, token := range append(tokens, "ghs_not_a_minted_token") {
				if (err != nil && strings.Contains(err.Error(), token)) || strings.Contains(logged.String(), token) {
					t.Fatalf("the error %v or the log %q holds a token", err, logged.String())
				}
			}
			live := github.live(t, tokens)
			if len(live) != tc.live {
				t.Fatalf("%d of the %d tokens GitHub minted are still live after the run (%q), want %d; log %q", len(live), len(tokens), live, tc.live, logged.String())
			}
			if tc.noRevoke && github.revoked() != 0 {
				t.Fatalf("the run sent GitHub %d revokes for answers that minted nothing, want none", github.revoked())
			}
			if tc.released {
				var handed mintedGitHubApp
				if err := json.Unmarshal([]byte(minted.Value), &handed); err != nil {
					t.Fatalf("the release's value: %v", err)
				}
				if got := handed.Installations["ada"].Token; len(live) != 1 || got != live[0] || got != tokens[len(tokens)-1] {
					t.Fatalf("the release handed over the token of request %d of %d; want the last one, the only one left live", indexOf(tokens, got)+1, len(tokens))
				}
			}
			for _, line := range tc.says {
				if lines, warnings := warned(logged.String(), line); lines == 0 || warnings != lines {
					t.Fatalf("log = %q; want a line saying %q, at WARN: %d lines say it, %d of them at WARN", logged.String(), line, lines, warnings)
				}
			}
			lines, warnings := warned(logged.String(), saysUnrevokable)
			if said := lines > 0; said != tc.unrevokable || warnings != lines {
				t.Fatalf("log = %q; a line saying the token cannot be revoked: %t, want %t; %d lines say it, %d of them at WARN", logged.String(), said, tc.unrevokable, lines, warnings)
			}
		})
	}
}

// indexOf is where token stands in tokens, and -1 where it is none of them.
func indexOf(tokens []string, token string) int {
	for i, minted := range tokens {
		if minted == token {
			return i
		}
	}
	return -1
}
