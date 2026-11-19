// packages/envoy/internal/broker/airlockproxy/azure_echo_test.go
// A token endpoint that sends the real federated token back as an answer no client reads, which
// the transport's error quotes. The broker sends the endpoint that token in every exchange it
// makes: the refusal of the exchange, its line and the answer the command is given, must hold
// none of it.
package airlockproxy

import (
	"bufio"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/logmask"
)

// echoingTokenEndpoint is a token endpoint of the row's own at the bound host, at address, speaking
// HTTP/1.1 over TLS with the certificate the proxy trusts. While echo is false it answers an
// exchange as Microsoft does, with azureRealAccess; once echo is set it answers the
// client_assertion it was sent, the real federated token, as its status line, which no client
// reads as an answer. The rig dials address for azureEchoAddress (r.dialTo), and a row points the
// bound host at it through the resolver.
func echoingTokenEndpoint(t *testing.T, r *rig) *atomic.Bool {
	t.Helper()
	leaf, _, err := r.service.Authority().Leaf([]string{boundHost})
	if err != nil {
		t.Fatalf("the echoing endpoint's certificate: %v", err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*leaf}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatalf("bind the echoing endpoint: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	echo := &atomic.Bool{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(20 * time.Second))
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				body, _ := io.ReadAll(request.Body)
				form, _ := url.ParseQuery(string(body))
				if echo.Load() {
					io.WriteString(conn, "echo-"+form.Get("client_assertion")+"\r\n\r\n")
					return
				}
				answer := `{"token_type":"Bearer","expires_in":3599,"access_token":"` + azureRealAccess + `"}`
				io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: "+
					strconv.Itoa(len(answer))+"\r\n\r\n"+answer)
			}()
		}
	}()
	r.dialTo[net.JoinHostPort(azureEchoAddress.String(), "443")] = listener.Addr().String()
	return echo
}

// azureEchoAddress is the public address the bound host resolves to when a row sends its
// exchanges to the echoing endpoint.
var azureEchoAddress = netip.MustParseAddr("93.184.216.47")

// holdsARunOf reports whether text holds twelve bytes in a row of secret: a run long enough that
// the broker's own words ("federated token") are none of it.
func holdsARunOf(text, secret string) bool {
	for i := 0; i+12 <= len(secret); i++ {
		if strings.Contains(text, secret[i:i+12]) {
			return true
		}
	}
	return false
}

// noFederatedToken fails t when the answer or any line the proxy wrote holds a run of the real
// federated token, and unless the answer and a line's reason hold shown, what is written in place
// of the endpoint's bytes: the refusal line of a refusal, or the call line of a 502, which is no
// refusal and names no code.
func noFederatedToken(t *testing.T, r *rig, answer, shown string) {
	t.Helper()
	if holdsARunOf(answer, azureRealFederated) {
		t.Errorf("the answer the command was given holds the real federated token: %q", answer)
	}
	if !strings.Contains(answer, shown) {
		t.Errorf("the answer the command was given is %q; want %s where the endpoint's bytes were", answer, shown)
	}
	reasoned := false
	for _, record := range r.logs.Records() {
		if text := recordText(record); holdsARunOf(text, azureRealFederated) {
			t.Errorf("a line the proxy wrote holds the real federated token: %q", text)
		}
		record.Attrs(func(a slog.Attr) bool {
			reasoned = reasoned || a.Key == "reason" && strings.Contains(a.Value.String(), shown)
			return true
		})
	}
	if !reasoned {
		t.Errorf("no line gives as its reason that the endpoint's answer was %s: %v", shown, r.logs.Records())
	}
}

// tokenEndpointNamed is a token endpoint of the row's own at azureEchoAddress, whose certificate,
// signed by the authority the proxy trusts, names the hosts names and not the bound host: the
// proxy's transport refuses it in the handshake, before any request is sent.
func tokenEndpointNamed(t *testing.T, r *rig, names ...string) {
	t.Helper()
	leaf, _, err := r.service.Authority().Leaf(names)
	if err != nil {
		t.Fatalf("the endpoint's certificate: %v", err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*leaf}})
	if err != nil {
		t.Fatalf("bind the endpoint: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(20 * time.Second))
				conn.(*tls.Conn).Handshake()
			}()
		}
	}()
	r.dialTo[net.JoinHostPort(azureEchoAddress.String(), "443")] = listener.Addr().String()
}

// TestAnExchangeWhoseEndpointsCertificateNamesTheTokenWritesNoneOfIt: the command's exchange goes
// to a token endpoint whose certificate names other hosts than the bound one, the real federated
// token among them. crypto/x509's error lists every name it holds, with no quotation mark for
// logmask.HidePeerBytes to find, so the refusal of the exchange and its line write
// certificateNotForHost in its place (upstreamReason), as an upstream failure's line does, and
// hold none of the token.
func TestAnExchangeWhoseEndpointsCertificateNamesTheTokenWritesNoneOfIt(t *testing.T) {
	r, _ := newAzureRig(t, Options{})
	tokenEndpointNamed(t, r, azureRealFederated, "other.example.net")
	r.answersMu.Lock()
	r.answers[boundHost] = [][]netip.Addr{r.addresses[boundHost], {azureEchoAddress}}
	r.answersMu.Unlock()
	answer, body := tokenRequest(t, r, r.client(azureFederatedPlaceholder), azureFederatedPlaceholder)
	if answer.StatusCode != http.StatusBadGateway {
		t.Fatalf("the exchange answered %d %q; want 502", answer.StatusCode, body)
	}
	noFederatedToken(t, r, body, certificateNotForHost)
}

// TestAnExchangeWhoseEndpointEchoesTheTokenWritesNoneOfIt: the command's exchange, which this pod
// makes at the endpoint with the real federated token (exchangeToken), is answered with that
// token as a status line. The command is refused 502, and neither that answer nor any line holds
// the token.
func TestAnExchangeWhoseEndpointEchoesTheTokenWritesNoneOfIt(t *testing.T) {
	r, _ := newAzureRig(t, Options{})
	echo := echoingTokenEndpoint(t, r)
	echo.Store(true)
	// The CONNECT's check reads the address wired to the stand-in service; the exchange's own
	// dial reads the echoing endpoint's.
	r.answersMu.Lock()
	r.answers[boundHost] = [][]netip.Addr{r.addresses[boundHost], {azureEchoAddress}}
	r.answersMu.Unlock()
	answer, body := tokenRequest(t, r, r.client(azureFederatedPlaceholder), azureFederatedPlaceholder)
	if answer.StatusCode != http.StatusBadGateway {
		t.Fatalf("the exchange answered %d %q; want 502", answer.StatusCode, body)
	}
	noFederatedToken(t, r, body, logmask.PeerBytesShown)
}

// TestASecondPodsExchangeWhoseEndpointEchoesTheTokenWritesNoneOfIt: the exchange a pod makes for
// itself, for an access placeholder another pod minted (azureAccess), is answered with the real
// federated token as a status line. The call is refused, and neither its answer nor any line
// holds the token.
func TestASecondPodsExchangeWhoseEndpointEchoesTheTokenWritesNoneOfIt(t *testing.T) {
	r, _ := newAzureRig(t, Options{})
	echo := echoingTokenEndpoint(t, r)
	// The first pod's exchange goes to the echoing endpoint while it answers as Microsoft does.
	r.answersMu.Lock()
	r.answers[boundHost] = [][]netip.Addr{r.addresses[boundHost], {azureEchoAddress}}
	r.answersMu.Unlock()
	answer, body := tokenRequest(t, r, r.client(azureFederatedPlaceholder), azureFederatedPlaceholder)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the first pod's exchange answered %d %q; want 200", answer.StatusCode, body)
	}
	access := placeholderTokenOf(t, body)
	echo.Store(true)
	second := secondPod(t, r)
	conn, err := net.Dial("tcp", second)
	if err != nil {
		t.Fatalf("dial the second pod: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	target := net.JoinHostPort(boundHost, "443")
	if _, err := conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n" + basic(azureFederatedPlaceholder) + "\r\n")); err != nil {
		t.Fatalf("write the CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	if connected, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect}); err != nil || connected.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s on the second pod answered %v, %v; want 200", target, connected, err)
	}
	client := tls.Client(conn, &tls.Config{RootCAs: r.trust, ServerName: boundHost})
	request := "GET /v1/subscriptions HTTP/1.1\r\nHost: " + boundHost + "\r\nAuthorization: Bearer " + access + "\r\nConnection: close\r\n\r\n"
	if _, err := client.Write([]byte(request)); err != nil {
		t.Fatalf("write the call on the decrypted connection: %v", err)
	}
	served, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodGet})
	if err != nil || served.StatusCode == http.StatusOK {
		t.Fatalf("the call whose exchange was answered with the token answered %v, %v; want a refusal", served, err)
	}
	refused, _ := io.ReadAll(served.Body)
	noFederatedToken(t, r, string(refused), logmask.PeerBytesShown)
}
