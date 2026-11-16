// packages/envoy/internal/broker/airlockproxy/h2_test.go
//
// HTTP/2 on a bound host. A client whose TLS handshake offers h2 and nothing else - every gRPC
// client's does - is answered in HTTP/2 on the connection the proxy decrypts, and each stream of
// that connection is one request to serveBound: read, decided and swapped as an HTTP/1.1 request
// is. TestMain already runs the whole suite with such a client; the rows here are what only
// HTTP/2 shows: which protocol a client offering both is answered in, a real gRPC client with its
// metadata, its streamed answers and its trailers, the streams of one connection each decided on
// its own, and a call its grant stops carrying while it runs.
//
// How a stream becomes the airlock.Request the decision reads - :authority as Host, the TLS name
// of the connection, :path as the path - is net/http's server's, and these rows read it back;
// the Lean model the decision is proved against has no wire format, so nothing proves it.
package airlockproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// negotiated is the protocol an answer's TLS agreed, "" for an answer that came over none.
func negotiated(answer *http.Response) string {
	if answer.TLS == nil {
		return ""
	}
	return answer.TLS.NegotiatedProtocol
}

// noPlaceholder fails the row when any header the service received names the placeholder.
func noPlaceholder(t *testing.T, header http.Header) {
	t.Helper()
	for name, values := range header {
		for _, value := range values {
			if strings.Contains(value, workerPlaceholder) {
				t.Errorf("the service received the placeholder in %s: %q", name, value)
			}
		}
	}
}

// TestAClientOfferingOnlyH2IsAnsweredInHTTP2: a client whose handshake offers h2 alone reaches the
// bound host in HTTP/2, and the swap reaches the stream's own headers - the service receives the
// real value in the header the entry names, and the placeholder nowhere. The named revert: the
// proxy offering http/1.1 alone (connectBound's NextProtos), which fails this client's handshake
// with "tls: no application protocol", as every gRPC client's failed before HTTP/2 was carried.
func TestAClientOfferingOnlyH2IsAnsweredInHTTP2(t *testing.T) {
	r := newRig(t, Options{})
	r.protocol = protocolH2
	answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the call is %d %q, want 200", answer.StatusCode, body)
	}
	if answer.Proto != "HTTP/2.0" || negotiated(answer) != protocolH2 {
		t.Fatalf("the call was answered in %s over %q, want HTTP/2.0 over h2", answer.Proto, negotiated(answer))
	}
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service received %d requests, want 1", len(seen))
	}
	if got := seen[0].Header.Get("Authorization"); got != "Bearer "+workerValue {
		t.Errorf("the service received Authorization %q, want the real value", got)
	}
	noPlaceholder(t, seen[0].Header)
}

// TestAClientOfferingBothProtocolsStaysOnHTTP11: the proxy offers http/1.1 before h2, and
// crypto/tls picks in the server's order, so a client that offers both - net/http's own, curl,
// git, each with h2 first - is answered in HTTP/1.1 as it was before HTTP/2 was carried. Only a
// client offering h2 alone moves to HTTP/2. The named revert: h2 first in connectBound's
// NextProtos, which answers this client in HTTP/2.
func TestAClientOfferingBothProtocolsStaysOnHTTP11(t *testing.T) {
	r := newRig(t, Options{})
	var both http.Protocols
	both.SetHTTP1(true)
	both.SetHTTP2(true)
	client := r.client(workerPlaceholder)
	client.Transport.(*http.Transport).Protocols = &both
	answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the call is %d %q, want 200", answer.StatusCode, body)
	}
	if answer.Proto != "HTTP/1.1" || negotiated(answer) != protocolHTTP1 {
		t.Fatalf("a client offering h2 and http/1.1 was answered in %s over %q, want HTTP/1.1 over http/1.1", answer.Proto, negotiated(answer))
	}
}

// TestAHopByHopHeaderTheCommandNamesStopsAtTheProxy: a header the command's Connection header names
// belongs to its connection to the proxy (RFC 9110, section 7.6.1), so neither it nor the
// Connection header goes on to the service, which is what httputil.ReverseProxy does for every
// request it is handed. Toward a service that speaks HTTP/2, which has no Connection header, a
// forwarded one fails the call itself: Go's HTTP/2 client refuses to send it, and the command is
// answered 502. The command's own forwarding header goes on as it was sent, since the proxy
// changes no header the entry does not name. The named revert: the swap's headers written over
// the request ReverseProxy stripped (pr.Out.Header = header in serveBound's Rewrite), which
// forwards both hop-by-hop headers and answers this call 502.
func TestAHopByHopHeaderTheCommandNamesStopsAtTheProxy(t *testing.T) {
	r := newRig(t, Options{})
	// A client of HTTP/1.1, the only one that has a Connection header to send.
	r.protocol = protocolHTTP1
	answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things",
		"Authorization", "Bearer "+workerPlaceholder,
		"Connection", "X-Hop",
		"X-Hop", "a header of the command's connection to the proxy",
		"X-Forwarded-For", "192.0.2.7")
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the call is %d %q, want 200", answer.StatusCode, body)
	}
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service received %d requests, want 1", len(seen))
	}
	for _, name := range []string{"Connection", "X-Hop"} {
		if got, sent := seen[0].Header[name]; sent {
			t.Errorf("the service received %s %q, a header of the command's connection to the proxy", name, got)
		}
	}
	if got := seen[0].Header.Get("X-Forwarded-For"); got != "192.0.2.7" {
		t.Errorf("the service received X-Forwarded-For %q, want the command's own 192.0.2.7", got)
	}
	if got := seen[0].Header.Get("Authorization"); got != "Bearer "+workerValue {
		t.Errorf("the service received Authorization %q, want the real value", got)
	}
}

// grpcService is the gRPC service the stand-in below answers: gRPC's own health service, whose
// Check is a call with one answer and whose Watch is a call whose answers stream.
const grpcService = "grpc.health.v1.Health"

// grpcRules is the rules file of the gRPC rows: the one secret, bound to the bound host, in
// Authorization, and allowed on the health service's calls alone. Every gRPC call is a POST to
// /<service>/<method>.
func grpcRules() airlock.Rules {
	return airlock.Rules{Secrets: []airlock.SecretRule{{
		Name:    workerName,
		Hosts:   []string{boundHost},
		Headers: []string{"authorization"},
		Allow:   []airlock.Allow{{Method: http.MethodPost, PathPrefix: "/" + grpcService + "/"}},
	}}}
}

// grpcStand is a gRPC service behind the bound host, in TLS with a certificate of the stand-in
// authority, the one authority the proxy trusts upstream. It records the metadata of every call,
// and ended is sent the error a Watch of the "long" service ended with: one that streams until
// its call ends, which is how a row tells that the proxy ended the call at the service too.
type grpcStand struct {
	grpc_health_v1.UnimplementedHealthServer
	mu    sync.Mutex
	seen  []metadata.MD
	ended chan error
}

func (g *grpcStand) record(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = append(g.seen, md.Copy())
}

// calls is the metadata of every call the stand-in received, in order.
func (g *grpcStand) calls() []metadata.MD {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.seen)
}

// Check answers SERVING with a trailer of its own for the server as a whole (""), and NotFound for
// any service it is asked about, which grpc-go answers Trailers-Only: one HEADERS frame that
// carries the status and ends the stream.
func (g *grpcStand) Check(ctx context.Context, request *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	g.record(ctx)
	if request.GetService() != "" {
		return nil, status.Error(codes.NotFound, "the stand-in knows no service "+request.GetService())
	}
	if err := grpc.SetTrailer(ctx, metadata.Pairs("x-stand", "checked")); err != nil {
		return nil, err
	}
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

// Watch streams three answers and ends with a trailer of its own, or, for the "long" service, one
// answer and then nothing until its call ends.
func (g *grpcStand) Watch(request *grpc_health_v1.HealthCheckRequest, stream grpc.ServerStreamingServer[grpc_health_v1.HealthCheckResponse]) error {
	g.record(stream.Context())
	serving := &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}
	if request.GetService() == "long" {
		if err := stream.Send(serving); err != nil {
			return err
		}
		<-stream.Context().Done()
		g.ended <- stream.Context().Err()
		return stream.Context().Err()
	}
	stream.SetTrailer(metadata.Pairs("x-stand", "watched"))
	for range 3 {
		if err := stream.Send(serving); err != nil {
			return err
		}
	}
	return nil
}

// grpcThrough puts the gRPC stand-in behind the bound host and answers a gRPC client that reaches
// it through the proxy as a command's does: a CONNECT carrying the grant's password, and gRPC's
// own TLS inside it, which offers h2 alone. The CONNECT is the row's own dialer, written as
// grpc-go's proxy dialer writes it, because grpc-go reads HTTPS_PROXY through net/http's
// ProxyFromEnvironment, which reads the environment once per process.
func grpcThrough(t *testing.T, r *rig) (*grpcStand, grpc_health_v1.HealthClient) {
	t.Helper()
	certificate, _, err := r.service.Authority().Leaf([]string{boundHost})
	if err != nil {
		t.Fatalf("the gRPC stand-in's certificate: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the gRPC stand-in: %v", err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(certificate)))
	stand := &grpcStand{ended: make(chan error, 1)}
	grpc_health_v1.RegisterHealthServer(server, stand)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	r.dialTo[net.JoinHostPort(boundHost, "443")] = listener.Addr().String()

	client, err := grpc.NewClient("passthrough:///"+net.JoinHostPort(boundHost, "443"),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: r.trust})),
		grpc.WithNoProxy(),
		grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
			return connectThrough(ctx, r.addr, target, workerPlaceholder)
		}))
	if err != nil {
		t.Fatalf("the gRPC client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return stand, grpc_health_v1.NewHealthClient(client)
}

// connectThrough opens a tunnel to target through the proxy at addr with password as its proxy
// password, and answers the tunnel once the proxy has answered 200.
func connectThrough(ctx context.Context, addr, target, password string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n"+basic(password)+"\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	answer, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if answer.StatusCode != http.StatusOK {
		conn.Close()
		return nil, errors.New("the proxy answered the CONNECT " + answer.Status)
	}
	return &readerConn{Conn: conn, reader: reader}, nil
}

// readerConn reads a connection through the bufio.Reader its CONNECT's answer was read with, so a
// byte the proxy wrote behind that answer is not lost.
type readerConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *readerConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

// withPlaceholder is a call's context carrying the placeholder in its authorization metadata, as
// a gRPC client given the placeholder as its access token sends it, and bounded so a call the
// proxy never answers fails the row rather than hanging it.
func withPlaceholder(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+workerPlaceholder)
}

// TestAGRPCClientIsCarriedWithItsMetadataStreamsAndTrailers: a real grpc-go client reaches a gRPC
// service on the bound host through the proxy - a call with one answer, a call the service fails,
// and a call whose answers stream - and the service receives the real value in each call's
// authorization metadata and the placeholder in none, while the client receives each answer, the
// status, and the trailers the service set. The failing call is made fifty times: grpc-go answers
// it Trailers-Only, one HEADERS frame that carries the status and ends the stream, and a proxy
// that flushes that frame before it knows the answer has no body sends it without ending the
// stream, which the client reads as "server closed the stream without sending trailers" and not
// as the service's status; the flush races the end of the answer, so one call alone may miss it.
// The named reverts: http/1.1 alone in connectBound's NextProtos, which fails every call's
// handshake; and statusWriter flushing an answer with no body (whole), which turns some of the
// failing calls into Internal.
func TestAGRPCClientIsCarriedWithItsMetadataStreamsAndTrailers(t *testing.T) {
	r := newRig(t, Options{Rules: grpcRules()})
	stand, health := grpcThrough(t, r)
	ctx := withPlaceholder(t)

	var trailer metadata.MD
	checked, err := health.Check(ctx, &grpc_health_v1.HealthCheckRequest{}, grpc.Trailer(&trailer))
	if err != nil {
		t.Fatalf("the Check through the proxy: %v", err)
	}
	if checked.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("the Check answered %v, want SERVING", checked.GetStatus())
	}
	if got := trailer.Get("x-stand"); !slices.Equal(got, []string{"checked"}) {
		t.Errorf("the Check's trailer x-stand is %q, want the service's own [checked]", got)
	}

	const failing = 50
	for i := range failing {
		_, err := health.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "unknown"})
		if status.Code(err) != codes.NotFound || !strings.Contains(status.Convert(err).Message(), "knows no service unknown") {
			t.Fatalf("failing Check %d of %d answered %v, want the service's own NotFound", i+1, failing, err)
		}
	}

	watch, err := health.Watch(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("the Watch through the proxy: %v", err)
	}
	answers := 0
	for {
		_, err := watch.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the Watch's answer %d: %v", answers+1, err)
		}
		answers++
	}
	if answers != 3 {
		t.Errorf("the Watch streamed %d answers, want the service's 3", answers)
	}
	if got := watch.Trailer().Get("x-stand"); !slices.Equal(got, []string{"watched"}) {
		t.Errorf("the Watch's trailer x-stand is %q, want the service's own [watched]", got)
	}

	calls := stand.calls()
	if len(calls) != failing+2 {
		t.Fatalf("the service received %d calls, want %d", len(calls), failing+2)
	}
	for i, md := range calls {
		if got := md.Get("authorization"); !slices.Equal(got, []string{"Bearer " + workerValue}) {
			t.Errorf("call %d reached the service with authorization %q, want the real value", i+1, got)
		}
		for name, values := range md {
			for _, value := range values {
				if strings.Contains(value, workerPlaceholder) {
					t.Errorf("call %d reached the service with the placeholder in %s", i+1, name)
				}
			}
		}
	}
	for _, line := range r.logs.callsAtLeast(t, failing+2) {
		if !strings.HasPrefix(line.fields["path"].(string), "/"+grpcService+"/") || line.fields["allowed"] != true {
			t.Errorf("the proxy logged %v for a gRPC call, want an allowed call on %s", line.fields, grpcService)
		}
	}
}

// TestAGRPCStreamEndsWhenItsGrantStopsCarryingIt: a gRPC call whose answers stream holds its
// stream open for as long as the service streams, and no request is read on that stream again, so
// the proxy reads the grant for it every watch interval and ends the stream once the grant stops
// carrying it, at the client and at the service both, as it closes a WebSocket. The named revert:
// no watcher for a call that is not an upgrade, which leaves the stream running until the row's
// deadline.
func TestAGRPCStreamEndsWhenItsGrantStopsCarryingIt(t *testing.T) {
	r := newRig(t, Options{Rules: grpcRules(), Watch: watchFast})
	stand, health := grpcThrough(t, r)
	ctx, cancel := context.WithTimeout(withPlaceholder(t), 2*cutWait)
	defer cancel()
	watch, err := health.Watch(ctx, &grpc_health_v1.HealthCheckRequest{Service: "long"})
	if err != nil {
		t.Fatalf("the Watch through the proxy: %v", err)
	}
	if _, err := watch.Recv(); err != nil {
		t.Fatalf("the Watch's first answer: %v", err)
	}

	r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
	ended := time.Now()
	_, err = watch.Recv()
	if took := time.Since(ended); err == nil || status.Code(err) == codes.DeadlineExceeded || took > cutWait {
		t.Fatalf("the Watch went on %v after its grant ended and then answered %v; want it ended within %v, the watch interval being %v", took, err, cutWait, watchFast)
	}
	select {
	case err := <-stand.ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the service's call ended with %v, want it cancelled", err)
		}
	case <-time.After(cutWait):
		t.Fatal("the service's call went on after the proxy ended the client's")
	}
	cuts := r.logs.cuts()
	if len(cuts) != 1 {
		t.Fatalf("the proxy logged %d lines for the end of the stream, want 1", len(cuts))
	}
	if got := cuts[0].fields["reason"]; got != requests.ErrGrantNotLive.Error() {
		t.Errorf("the end of the stream logged the reason %v, want %q, the read's own words", got, requests.ErrGrantNotLive)
	}
}

// streamStandIn makes the stand-in service answer every call with a line every few milliseconds,
// each flushed, until the call ends, and answers the channel the error it ended with is sent on.
func streamStandIn(r *rig) chan error {
	ended := make(chan error, 1)
	r.answerWith(func(w http.ResponseWriter, request *http.Request, _ airlocktest.Request) bool {
		w.WriteHeader(http.StatusOK)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			_, _ = io.WriteString(w, "a line of the stream\n")
			_ = http.NewResponseController(w).Flush()
			select {
			case <-request.Context().Done():
				ended <- request.Context().Err()
				return true
			case <-ticker.C:
			}
		}
	})
	return ended
}

// TestACarriedStreamEndsWhenItsGrantStopsCarryingIt: an answer that streams - a server-sent event
// stream, a long download, a gRPC call - is carried for as long as the service sends it, so the
// grant is read for it every watch interval, as for a WebSocket the proxy joined, and the call is
// ended at both ends once the grant stops carrying it, with one line saying why. The row runs in
// both of TestMain's runs, so the call is an HTTP/1.1 request in one and an HTTP/2 stream in the
// other. The named revert: no watcher for a call that is not an upgrade, which leaves the answer
// streaming until the row's deadline.
func TestACarriedStreamEndsWhenItsGrantStopsCarryingIt(t *testing.T) {
	r := newRig(t, Options{Watch: watchFast})
	ended := streamStandIn(r)
	client := r.client(workerPlaceholder)
	client.Timeout = 0
	ctx, cancel := context.WithTimeout(context.Background(), 2*cutWait)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+boundHost+"/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+workerPlaceholder)
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("the streamed call through the proxy: %v", err)
	}
	defer answer.Body.Close()
	reader := bufio.NewReader(answer.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "a line of the stream\n" {
		t.Fatalf("the stream's first line is %q (%v), want the service's", line, err)
	}

	r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
	cut := time.Now()
	_, err = io.Copy(io.Discard, reader)
	if took := time.Since(cut); err == nil || errors.Is(err, context.DeadlineExceeded) || took > cutWait {
		t.Fatalf("the stream went on %v after its grant ended and then ended with %v; want it cut within %v, the watch interval being %v", took, err, cutWait, watchFast)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the service's answer ended with %v, want it cancelled", err)
		}
	case <-time.After(cutWait):
		t.Fatal("the service went on streaming after the proxy cut the command's side")
	}
	cuts := r.logs.cuts()
	if len(cuts) != 1 {
		t.Fatalf("the proxy logged %d lines for the cut, want 1", len(cuts))
	}
	if got := cuts[0].fields["reason"]; got != requests.ErrGrantNotLive.Error() {
		t.Errorf("the cut logged the reason %v, want %q, the read's own words", got, requests.ErrGrantNotLive)
	}
	if got := cuts[0].fields["status"]; got != int64(http.StatusProxyAuthRequired) {
		t.Errorf("the cut logged the status %v, want 407, the status every later call of the grant is refused with", got)
	}
}

// TestACallWaitingForItsAnswerIsRefusedWhenItsGrantEnds: a call whose service has not answered yet
// - a long poll - is ended by the watcher as a streamed answer is, and since nothing of the
// service's answer has reached the command, the command is answered the refusal every call of
// that grant now gets, in its words, rather than a 502 that would read as a service out of reach.
// The named revert: no watcher for a call that is not an upgrade, which leaves the command waiting
// until its client gives up.
func TestACallWaitingForItsAnswerIsRefusedWhenItsGrantEnds(t *testing.T) {
	r := newRig(t, Options{Watch: watchFast})
	ended := make(chan error, 1)
	r.answerWith(func(_ http.ResponseWriter, request *http.Request, _ airlocktest.Request) bool {
		<-request.Context().Done()
		ended <- request.Context().Err()
		return true
	})
	type result struct {
		answer *http.Response
		body   string
		err    error
	}
	done := make(chan result, 1)
	client := r.client(workerPlaceholder)
	go func() {
		request, err := http.NewRequest(http.MethodGet, "https://"+boundHost+"/v1/poll", nil)
		if err != nil {
			done <- result{err: err}
			return
		}
		request.Header.Set("Authorization", "Bearer "+workerPlaceholder)
		response, err := client.Do(request)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		done <- result{answer: response, body: string(body), err: err}
	}()
	for deadline := time.Now().Add(cutWait); len(r.requests()) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the call never reached the service")
		}
	}

	r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
	var got result
	select {
	case got = <-done:
	case <-time.After(cutWait):
		t.Fatalf("the call was still waiting %v after its grant ended; want it refused within it, the watch interval being %v", cutWait, watchFast)
	}
	if got.err != nil {
		t.Fatalf("the call ended with %v, want the proxy's refusal", got.err)
	}
	if got.answer.StatusCode != http.StatusProxyAuthRequired || !strings.Contains(got.body, requests.ErrGrantNotLive.Error()) {
		t.Fatalf("the call was answered %d %q, want 407 in the grant read's own words", got.answer.StatusCode, got.body)
	}
	if id := got.answer.Header.Get(answer.Header); id == "" || !strings.Contains(got.body, "(answer "+id+")") {
		t.Errorf("the refusal carries answer id %q and the body %q, want the body to end with that id", id, got.body)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the service's call ended with %v, want it cancelled", err)
		}
	case <-time.After(cutWait):
		t.Fatal("the service's call went on after the proxy refused the command's")
	}
	if cuts := r.logs.cuts(); len(cuts) != 1 {
		t.Fatalf("the proxy logged %d lines for the cut, want 1", len(cuts))
	}
	if lines := r.logs.lines(upstreamMessage); len(lines) != 0 {
		t.Errorf("the proxy logged the cut as a service out of reach: %v", lines)
	}
}

// h2Conn is one HTTP/2 connection to the bound host through the proxy, written frame by frame,
// which is how a row sends a stream an HTTP/2 client library would not: a CONNECT, a :protocol, a
// second authority, a :scheme of http.
type h2Conn struct {
	t       *testing.T
	conn    *tls.Conn
	framer  *http2.Framer
	block   bytes.Buffer
	encoder *hpack.Encoder
	next    uint32
}

// h2Through opens that connection: a CONNECT with the grant's password, a TLS handshake offering
// h2 alone, the client preface and an empty SETTINGS frame.
func h2Through(t *testing.T, r *rig) *h2Conn {
	t.Helper()
	answer, conn, reader := r.connectRaw(net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the CONNECT is %d, want 200", answer.StatusCode)
	}
	decrypted := tls.Client(&readerConn{Conn: conn, reader: reader}, &tls.Config{ServerName: boundHost, RootCAs: r.trust, NextProtos: []string{protocolH2}})
	if err := decrypted.Handshake(); err != nil {
		t.Fatalf("the TLS handshake offering h2 alone: %v", err)
	}
	t.Cleanup(func() { decrypted.Close() })
	if got := decrypted.ConnectionState().NegotiatedProtocol; got != protocolH2 {
		t.Fatalf("the handshake agreed %q, want h2", got)
	}
	if _, err := io.WriteString(decrypted, http2.ClientPreface); err != nil {
		t.Fatalf("write the client preface: %v", err)
	}
	c := &h2Conn{t: t, conn: decrypted, framer: http2.NewFramer(decrypted, decrypted), next: 1}
	c.framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	c.encoder = hpack.NewEncoder(&c.block)
	if err := c.framer.WriteSettings(); err != nil {
		t.Fatalf("write SETTINGS: %v", err)
	}
	return c
}

// h2Answer is what one stream was answered: its status and its body, or the code the stream was
// reset with.
type h2Answer struct {
	status string
	body   string
	reset  bool
	code   http2.ErrCode
}

// stream sends one stream of these header fields, its request ended with them, and reads the
// connection until that stream ends, answered or reset.
func (c *h2Conn) stream(fields []hpack.HeaderField) h2Answer {
	c.t.Helper()
	id := c.next
	c.next += 2
	c.block.Reset()
	for _, field := range fields {
		if err := c.encoder.WriteField(field); err != nil {
			c.t.Fatalf("encode %s: %v", field.Name, err)
		}
	}
	if err := c.framer.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: c.block.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		c.t.Fatalf("write stream %d: %v", id, err)
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		c.t.Fatalf("set the read deadline: %v", err)
	}
	var got h2Answer
	for {
		frame, err := c.framer.ReadFrame()
		if err != nil {
			c.t.Fatalf("read the connection for stream %d: %v", id, err)
		}
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := c.framer.WriteSettingsAck(); err != nil {
					c.t.Fatalf("acknowledge SETTINGS: %v", err)
				}
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				if err := c.framer.WritePing(true, f.Data); err != nil {
					c.t.Fatalf("answer PING: %v", err)
				}
			}
		case *http2.GoAwayFrame:
			c.t.Fatalf("the proxy closed the connection (GOAWAY %v %q) on stream %d; want each stream answered on its own", f.ErrCode, f.DebugData(), id)
		case *http2.MetaHeadersFrame:
			if f.StreamID == id {
				got.status = f.PseudoValue("status")
				if f.StreamEnded() {
					return got
				}
			}
		case *http2.DataFrame:
			if f.StreamID == id {
				got.body += string(f.Data())
				// The bytes back to the proxy's flow-control windows, the connection's and
				// the stream's, so a long row never stalls on a window the bodies used up.
				if n := uint32(len(f.Data())); n > 0 {
					_ = c.framer.WriteWindowUpdate(0, n)
					_ = c.framer.WriteWindowUpdate(id, n)
				}
				if f.StreamEnded() {
					return got
				}
			}
		case *http2.RSTStreamFrame:
			if f.StreamID == id {
				got.reset, got.code = true, f.ErrCode
				return got
			}
		}
	}
}

// h2Fields is a request's header fields: the four pseudo-header fields of an ordinary request,
// and then the rest.
func h2Fields(method, scheme, authority, path string, rest ...hpack.HeaderField) []hpack.HeaderField {
	return append([]hpack.HeaderField{
		{Name: ":method", Value: method},
		{Name: ":scheme", Value: scheme},
		{Name: ":authority", Value: authority},
		{Name: ":path", Value: path},
	}, rest...)
}

// TestEachStreamOfOneH2ConnectionIsDecidedOnItsOwn: one HTTP/2 connection carries many requests,
// each with an authority of its own, and each stream is decided as the request it is - so on one
// connection, in order: a stream to the bound host is carried; a stream naming another authority
// is refused and the service sees nothing of it, whatever host field it carries beside its
// :authority; a stream whose path has a dot segment is refused; a CONNECT stream is refused before
// anything reads the grant; an extended CONNECT, which carries :protocol, is reset by net/http's
// server, which has extended CONNECT turned off; a stream whose :scheme is http is carried to the
// CONNECT target over TLS, as every stream is - net/http's server reads such a stream as one of
// https, and the proxy forwards every stream to the target it decrypted for, so the value never
// leaves in the clear; a stream to the bound host after all of those is still carried; and once
// the operator withholds the name, and then once the grant ends, the next stream on the same
// connection is refused. The entry allows every method and path, so what refuses the CONNECT is
// the proxy's own refusal of one, not the entry's methods.
//
// The named reverts: http/1.1 alone in connectBound's NextProtos, which fails the handshake; the
// CONNECT refusal taken out of serveBound, which carries the CONNECT stream to the service; and the
// decision's Host read from the request's host header field (r.Header.Get("Host")) rather than
// from :authority (r.Host), which carries the stream naming another authority to the bound host.
func TestEachStreamOfOneH2ConnectionIsDecidedOnItsOwn(t *testing.T) {
	r := newRig(t, Options{Rules: airlock.Rules{Secrets: []airlock.SecretRule{{
		Name: workerName, Hosts: []string{boundHost}, Headers: []string{"authorization"},
	}}}})
	c := h2Through(t, r)
	authorization := hpack.HeaderField{Name: "authorization", Value: "Bearer " + workerPlaceholder}
	// The grant as it reads once its operator has withheld the one name bound to the host,
	// holding one other name so that the read answers a grant rather than refusing.
	withheld := testConnection()
	withheld.Names = map[string]requests.AirlockGrantName{"OTHER_TOKEN": {Slots: map[string]string{requests.SlotValue: requests.PlaceholderPrefix + "ZzYyXxWwVvUuTtSsRrQqPpOoNnMmLlKk"}}}
	for _, row := range []struct {
		name   string
		before func()
		fields []hpack.HeaderField
		// status is the answer's, "" for a stream that is reset; reason is a part of the
		// refusal's body; carried is whether the service received the stream.
		status  string
		reason  string
		carried bool
	}{
		{name: "a stream to the bound host", fields: h2Fields(http.MethodGet, "https", boundHost, "/v1/things", authorization),
			status: "200", carried: true},
		{name: "a stream naming another authority, with a host field naming the bound host",
			fields: h2Fields(http.MethodGet, "https", "other.example.org", "/v1/things", authorization, hpack.HeaderField{Name: "host", Value: boundHost}),
			status: "403", reason: "host header mismatch"},
		{name: "a stream whose path has a dot segment", fields: h2Fields(http.MethodGet, "https", boundHost, "/v1/../admin", authorization),
			status: "403", reason: "path has a dot segment"},
		{name: "a CONNECT stream", fields: []hpack.HeaderField{{Name: ":method", Value: http.MethodConnect}, {Name: ":authority", Value: boundHost + ":443"}, authorization},
			status: "403", reason: "a CONNECT inside a connection the proxy decrypted"},
		{name: "an extended CONNECT stream, which carries :protocol",
			fields: []hpack.HeaderField{{Name: ":method", Value: http.MethodConnect}, {Name: ":protocol", Value: "websocket"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: boundHost}, {Name: ":path", Value: "/v1/socket"}, authorization}},
		{name: "a stream whose :scheme is http", fields: h2Fields(http.MethodGet, "http", boundHost, "/v1/things", authorization),
			status: "200", carried: true},
		{name: "a stream to the bound host after those", fields: h2Fields(http.MethodGet, "https", boundHost, "/v1/things", authorization),
			status: "200", carried: true},
		{name: "a stream once the operator withheld the name", before: func() { r.grants.setConnection(workerPlaceholder, withheld) },
			fields: h2Fields(http.MethodGet, "https", boundHost, "/v1/things", authorization), status: "403", reason: "no name of this grant is bound to " + boundHost},
		{name: "a stream once the grant ended", before: func() { r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive) },
			fields: h2Fields(http.MethodGet, "https", boundHost, "/v1/things", authorization), status: "407", reason: requests.ErrGrantNotLive.Error()},
	} {
		if row.before != nil {
			row.before()
		}
		before := len(r.requests())
		got := c.stream(row.fields)
		switch {
		case row.status == "" && !got.reset:
			t.Errorf("%s: answered %s %q, want the stream reset", row.name, got.status, got.body)
		case row.status != "" && (got.reset || got.status != row.status || !strings.Contains(got.body, row.reason)):
			t.Errorf("%s: answered %s %q (reset %v, %v), want %s naming %q", row.name, got.status, got.body, got.reset, got.code, row.status, row.reason)
		}
		seen := r.requests()
		switch {
		case row.carried && len(seen) != before+1:
			t.Errorf("%s: the service received %d requests for it, want 1", row.name, len(seen)-before)
		case row.carried:
			if got := seen[before].Header.Get("Authorization"); got != "Bearer "+workerValue {
				t.Errorf("%s: the service received Authorization %q, want the real value", row.name, got)
			}
		case len(seen) != before:
			t.Errorf("%s: the service received %v, want nothing of a stream the proxy refused", row.name, seen[before:])
		}
	}
}
