// packages/envoy/internal/broker/airlockproxy/field_cut_test.go
//
// The cut of each field the caller wrote, on every line of the airlock: every line the proxy
// writes fits one Cloud Logging entry whatever the caller sends, driven on the real proxy of the
// rig, field by field.
package airlockproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestEveryLineOfTheAirlockCutsEachFieldTheCallerWrote: a refusal's line, a call's line, a joined
// connection's line, a cut call's line, an upstream failure's line and net/http's own line on the
// proxy port each fit one Cloud Logging entry, 262,144 bytes, whatever the caller sends, because
// each text field the caller wrote is cut to its first 256 bytes where it enters the line
// (answer.ClipFields; ca-refusal-record, Verification v19). Each row sends, through one field the
// caller chose, text that writes that line at about 300 KB uncut, on the real proxy of the rig:
//
//   - of a refusal's line, the method, the host, the path and the X-Correlation-Id of a request
//     that is no proxy request, and the reason in both its forms that quote the host - a CONNECT
//     target that is no host and port (r.Host), and a name that does not resolve
//     (checkedAddresses' error), on a CONNECT and on a plain proxy request;
//   - of a call's line (callFields), the method of a call whose grant read did not happen and the
//     path of a call the proxy carried;
//   - of a joined connection's line (watchCarried, through callFields), the method of an upgrade
//     whose entry allows any method (Allow nil) and the path of one under the allowed prefix,
//     each joined and then closed by a grant that stopped carrying it;
//   - of a cut call's line (watchCarried, through callFields), the method and the path of a call
//     whose answer streams, under the same two entries, each cut by a grant that stopped
//     carrying it;
//   - of a tunnel's line (logTunnel, through dialFields), the host of a CONNECT whose name
//     resolves to an address the proxy may dial and a service answers on, so the tunnel opens;
//   - of a failed dial's line (logDialFailure, through dialFields), the host and the reason of a
//     CONNECT to a name that resolves nowhere, the reason being the proxy's own words around that
//     host;
//   - of the upstream failure's line (badGateway), the path and the host of a plain proxy request
//     whose service cannot be reached, and the error of one whose name answers the check and
//     nothing to the transport's own lookup, which quotes that host;
//   - of net/http's own line (slogWriter, the ErrorLog of both servers), the error of a TLS
//     handshake that failed on a bound connection, whose ClientHello offers only cipher suites
//     the proxy does not speak, which crypto/tls's error lists in hex with no quotation mark, so
//     the line holds it past the peer bytes it hides (logmask.HidePeerBytes).
//
// The cut is for the caller's text, never for the server's own. One row more panics a handler of
// the inner server (serveBound, through the clock the rig hands the proxy), which net/http
// recovers and logs with the stack of the goroutine that panicked. That line is the one record of
// where the panic happened, so it holds the first line of net/http's text as its error, cut as
// every other error is, and the stack after it whole in its stack field, which must name the
// handler that panicked; net/http bounds the stack it writes, so the line still fits the entry.
// No other row's line holds a stack.
//
// Each row writes its line through the broker's own JSON handler (logging.NewCloudHandler), reads
// its length, and reads the field back as the first answer.FieldBytes bytes of the text it held,
// which for a reason or an error is the proxy's words around the host, for net/http's error its
// words around the client's address and the cipher suites it offered, and for the panic's error,
// which is shorter, its whole first line: the client's address and the value the clock panicked
// with. Every row but the panic's holds more than answer.FieldBytes bytes, so each reads a field
// the cut shortened. The table records which field of which line it drove, and fails when it
// drove fewer than all of them.
//
// The named reverts: let one field pass answer.ClipFields uncut, slogWriter's among them; that
// field's row writes a field past answer.FieldBytes bytes and fails, and all but net/http's a line
// of about 300 KB. Or pass net/http's whole text through answer.ClipFields as its error with no
// stack field, as slogWriter did before Verification v19; the panic row then fails, since the
// first 256 bytes of that text end inside the runtime's frames, before any of the handler's.
func TestEveryLineOfTheAirlockCutsEachFieldTheCallerWrote(t *testing.T) {
	const entryLimit = 256 << 10
	// The caller's text: not one byte repeated, so its first 256 bytes are no other 256 of it.
	sent := strings.Repeat("abcdefghij", 30000)
	// public is an address the proxy may dial that no stand-in service answers on port 80, so a
	// plain proxy request to a name holding it reaches badGateway.
	public := netip.MustParseAddr("93.184.216.37")
	// notAProxyRequest is a request to the proxy port that is no proxy request, changed by edit,
	// which the port refuses before it reads any credential.
	notAProxyRequest := func(t *testing.T, r *rig, edit func(*http.Request)) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, "http://"+r.addr+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		edit(request)
		response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
		if err != nil {
			t.Fatalf("a request to the proxy port: %v", err)
		}
		bodyOf(t, response)
		return response
	}
	// bound is one call to the bound host through the proxy, as a command's client makes it.
	bound := func(r *rig, method, path string) *http.Response {
		response, _ := r.do(r.client(workerPlaceholder), method, "https://"+boundHost+path, "", "Authorization", "Bearer "+workerPlaceholder)
		return response
	}
	connect := func(target string) func(t *testing.T, r *rig) *http.Response {
		return func(_ *testing.T, r *rig) *http.Response {
			response, _, _ := r.connectRaw(target, basic(workerPlaceholder))
			return response
		}
	}
	// join upgrades to the service at that method and path, joins the two connections, and then
	// ends the grant behind them, which is what makes the watcher write the join's own line
	// (TestAJoinedConnectionIsClosedWhenItsGrantStopsCarryingIt drives the same close).
	join := func(method, path string) func(t *testing.T, r *rig) *http.Response {
		return func(t *testing.T, r *rig) *http.Response {
			_, conn, reader, upgraded := joinedThroughPath(t, r, method, path)
			r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
			waitClosed(t, conn, reader)
			return upgraded
		}
	}
	// cut streams the service's answer to a call at that method and path, and then ends the grant
	// behind it, which is what makes the watcher write the cut's own line
	// (TestACarriedStreamEndsWhenItsGrantStopsCarryingIt drives the same cut).
	cut := func(method, path string) func(t *testing.T, r *rig) *http.Response {
		return func(t *testing.T, r *rig) *http.Response {
			streamStandIn(r)
			request, err := http.NewRequest(method, "https://"+boundHost+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+workerPlaceholder)
			response, err := r.client(workerPlaceholder).Do(request)
			if err != nil {
				t.Fatalf("the streamed call through the proxy: %v", err)
			}
			defer response.Body.Close()
			reader := bufio.NewReader(response.Body)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatalf("the stream's first line: %v", err)
			}
			r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)
			_, _ = io.Copy(io.Discard, reader)
			return response
		}
	}
	// anyMethod is the rules file of the join's method row: an entry with no allow list, which
	// the airlock reads as any method and path (airlock.SecretRule.Allow), so the upgrade the row
	// sends is carried rather than refused and its method reaches the join's line.
	anyMethod := airlock.Rules{Secrets: []airlock.SecretRule{{
		Name: workerName, Hosts: []string{boundHost}, Headers: []string{"authorization"},
	}}}
	// suites is a ClientHello's cipher suites, none of which crypto/tls's server speaks: 4,000 ids
	// from 0x2000 on, where no cipher suite is registered. crypto/tls refuses such a ClientHello
	// listing every suite the client offered in hex, with no quotation mark (pickCipherSuite), so
	// net/http's line holds about 20 KB of the client's text that logmask.HidePeerBytes leaves as
	// it came, and only the cut bounds.
	suites := make([]uint16, 4000)
	for i := range suites {
		suites[i] = 0x2000 + uint16(i)
	}
	// handshakeError is what net/http's line holds before the cut. It names the client's own
	// address, which only the row's dial picks, so the row's send writes it.
	var handshakeError string
	// panicked is what the panic row's clock panics with, and panicError the first line of
	// net/http's text for it, which names the client's address as handshakeError does.
	const panicked = "the clock the panic row hands the proxy panicked"
	var panicError string
	// fields is every field of each line that holds the caller's text, each of which a row drives.
	fields := map[string][]string{
		answer.RefusalMessage: {"method", "host", "path", "correlation_id", "reason"},
		callMessage:           {"method", "path"},
		joinMessage:           {"method", "path"},
		cutMessage:            {"method", "path"},
		tunnelMessage:         {"host"},
		dialFailedMessage:     {"host", "reason"},
		upstreamMessage:       {"path", "host", "error"},
		netHTTPMessage:        {"error"},
	}
	drove := map[string]bool{}
	driven := 0
	for _, row := range []struct {
		name string
		// message is the line's, field the field the caller's text goes into, and held what that
		// field holds before the cut.
		message, field, held string
		// heldBy, where set, answers held once send has run, for a field whose text names what
		// only the row's own dial picks.
		heldBy func() string
		// frame, where set, is the function the line's stack must name: the handler that
		// panicked. A row that sets none writes a line with no stack.
		frame  string
		status int
		// options is the rig's; the zero value is the plain rig every other row drives.
		options Options
		send    func(t *testing.T, r *rig) *http.Response
	}{
		{name: "a refusal's method", message: answer.RefusalMessage, field: "method", held: sent, status: http.StatusBadRequest,
			send: func(t *testing.T, r *rig) *http.Response {
				return notAProxyRequest(t, r, func(request *http.Request) { request.Method = sent })
			}},
		{name: "a refusal's host", message: answer.RefusalMessage, field: "host", held: sent, status: http.StatusBadRequest,
			send: func(t *testing.T, r *rig) *http.Response {
				return notAProxyRequest(t, r, func(request *http.Request) { request.Host = sent })
			}},
		{name: "a refusal's path", message: answer.RefusalMessage, field: "path", held: "/" + sent, status: http.StatusBadRequest,
			send: func(t *testing.T, r *rig) *http.Response {
				return notAProxyRequest(t, r, func(request *http.Request) { request.URL.Path = "/" + sent })
			}},
		{name: "a refusal's X-Correlation-Id", message: answer.RefusalMessage, field: "correlation_id", held: sent, status: http.StatusBadRequest,
			send: func(t *testing.T, r *rig) *http.Response {
				return notAProxyRequest(t, r, func(request *http.Request) { request.Header.Set("X-Correlation-Id", sent) })
			}},
		{name: "a refusal's reason: a CONNECT target that is no host and port", message: answer.RefusalMessage, field: "reason",
			held: sent + "] is no host and port", status: http.StatusBadRequest, send: connect(sent + "]")},
		{name: "a refusal's reason: a CONNECT target that does not resolve", message: answer.RefusalMessage, field: "reason",
			held: "cannot resolve " + sent + ": no such host", status: http.StatusForbidden, send: connect(sent + ":443")},
		{name: "a refusal's reason: a plain proxy request to a host that does not resolve", message: answer.RefusalMessage, field: "reason",
			held: "cannot resolve " + sent + ": no such host", status: http.StatusForbidden,
			send: func(_ *testing.T, r *rig) *http.Response {
				response, _ := r.get(r.client(workerPlaceholder), "http://"+sent+"/")
				return response
			}},
		{name: "a call's method", message: callMessage, field: "method", held: sent, status: http.StatusServiceUnavailable,
			send: func(_ *testing.T, r *rig) *http.Response {
				r.grants.refuseValues(databaseDown)
				return bound(r, sent, "/v1/things")
			}},
		{name: "a call's path", message: callMessage, field: "path", held: "/v1/" + sent, status: http.StatusOK,
			send: func(_ *testing.T, r *rig) *http.Response {
				return bound(r, http.MethodGet, "/v1/"+sent)
			}},
		{name: "a joined connection's method", message: joinMessage, field: "method", held: sent, status: http.StatusSwitchingProtocols,
			options: Options{Watch: watchFast, Rules: anyMethod}, send: join(sent, upgradePath)},
		{name: "a joined connection's path", message: joinMessage, field: "path", held: "/v1/" + sent, status: http.StatusSwitchingProtocols,
			options: Options{Watch: watchFast}, send: join(http.MethodGet, "/v1/"+sent)},
		{name: "a cut call's method", message: cutMessage, field: "method", held: sent, status: http.StatusOK,
			options: Options{Watch: watchFast, Rules: anyMethod}, send: cut(sent, "/v1/stream")},
		{name: "a cut call's path", message: cutMessage, field: "path", held: "/v1/" + sent, status: http.StatusOK,
			options: Options{Watch: watchFast}, send: cut(http.MethodGet, "/v1/"+sent)},
		{name: "a tunnel's host: a CONNECT to a name the proxy may dial that a service answers", message: tunnelMessage, field: "host",
			held: sent, status: http.StatusOK,
			send: func(_ *testing.T, r *rig) *http.Response {
				// The name resolves to a public address wired to the echo service, so the
				// CONNECT opens a tunnel and the proxy writes that tunnel's one line.
				r.addresses[sent] = []netip.Addr{public}
				r.dialTo[net.JoinHostPort(public.String(), "443")] = r.dialTo[net.JoinHostPort(echoHost, "443")]
				response, _, _ := r.connectRaw(sent+":443", basic(workerPlaceholder))
				return response
			}},
		{name: "a failed dial's host: a CONNECT to a name that resolves nowhere", message: dialFailedMessage, field: "host",
			held: sent, status: http.StatusForbidden, send: connect(sent + ":443")},
		{name: "a failed dial's reason: a CONNECT to a name that resolves nowhere", message: dialFailedMessage, field: "reason",
			held: "cannot resolve " + sent + ": no such host", status: http.StatusForbidden, send: connect(sent + ":443")},
		{name: "an upstream failure's path: a plain proxy request whose service cannot be reached", message: upstreamMessage, field: "path",
			held: "/" + sent, status: http.StatusBadGateway,
			send: func(_ *testing.T, r *rig) *http.Response {
				// The rig dials nothing on port 80, so the request is a 502 of badGateway's.
				response, _ := r.get(r.client(workerPlaceholder), "http://"+echoHost+"/"+sent)
				return response
			}},
		{name: "an upstream failure's host: a plain proxy request to a name that resolves and answers nothing", message: upstreamMessage, field: "host",
			held: sent, status: http.StatusBadGateway,
			send: func(_ *testing.T, r *rig) *http.Response {
				// The name resolves to an address the proxy may dial and no service answers on.
				r.addresses[sent] = []netip.Addr{public}
				response, _ := r.get(r.client(workerPlaceholder), "http://"+sent+"/")
				return response
			}},
		{name: "an upstream failure's error: a name that answers the check and nothing the transport's own lookup", message: upstreamMessage, field: "error",
			held: sent + " resolves to no address", status: http.StatusBadGateway,
			send: func(_ *testing.T, r *rig) *http.Response {
				// One answer for servePlain's check and none for the transport's lookup, whose
				// error names the host the caller asked for.
				r.answers[sent] = [][]netip.Addr{{public}, {}}
				response, _ := r.get(r.client(workerPlaceholder), "http://"+sent+"/")
				return response
			}},
		{name: "Go's HTTP server's error: a TLS handshake on a bound connection whose ClientHello offers no cipher suite the proxy speaks", message: netHTTPMessage, field: "error",
			heldBy: func() string { return handshakeError }, status: http.StatusOK,
			send: func(t *testing.T, r *rig) *http.Response {
				response, conn, _ := r.connectRaw(boundHost+":443", basic(workerPlaceholder))
				// net/http names the client by the address its CONNECT came from, this end of the
				// connection, and crypto/tls lists every cipher suite the client offered.
				handshakeError = fmt.Sprintf("http: TLS handshake error from %s: tls: no cipher suite supported by both client and server; client offered: %x",
					conn.LocalAddr(), suites)
				conn.SetDeadline(time.Now().Add(20 * time.Second))
				if _, err := conn.Write(clientHelloOffering(suites)); err != nil {
					t.Fatalf("write the ClientHello: %v", err)
				}
				// The proxy answers with an alert and closes the connection.
				io.Copy(io.Discard, conn)
				return response
			}},
		{name: "Go's HTTP server's error: a handler of the inner server that panics, whose stack the line keeps whole", message: netHTTPMessage, field: "error",
			heldBy: func() string { return panicError }, frame: "github.com/sjawhar/envoy/internal/broker/airlockproxy.(*Server).serveBound(",
			status: http.StatusOK, options: Options{Now: func() time.Time { panic(panicked) }},
			send: func(t *testing.T, r *rig) *http.Response {
				response, conn, _ := r.connectRaw(boundHost+":443", basic(workerPlaceholder))
				// net/http names the client as the handshake row's line does, and the value the
				// handler panicked with beside it.
				panicError = fmt.Sprintf("http: panic serving %s: %s", conn.LocalAddr(), panicked)
				client := tls.Client(conn, &tls.Config{ServerName: boundHost, RootCAs: r.trust})
				// A call to the bound host: serveBound reads the grant, chooses the name the call is
				// for (choosePair) and asks for the grant's values, whose first read of the clock panics.
				call := "GET /v1/things HTTP/1.1\r\nHost: " + boundHost + "\r\nAuthorization: Bearer " + workerPlaceholder + "\r\n\r\n"
				if _, err := client.Write([]byte(call)); err != nil {
					t.Fatalf("write the call on the bound connection: %v", err)
				}
				if answered, err := http.ReadResponse(bufio.NewReader(client), nil); err == nil {
					t.Fatalf("the call whose handler panicked was answered %d; want the connection closed with no answer", answered.StatusCode)
				}
				return response
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			driven++
			drove[row.message+" "+row.field] = true
			r := newRig(t, row.options)
			response := row.send(t, r)
			id := response.Header.Get(answer.Header)
			if response.StatusCode != row.status || id == "" {
				t.Fatalf("the answer is %d with answer id %q, want %d with one", response.StatusCode, id, row.status)
			}
			// A call's line is written once its answer has been copied to the client, so it
			// can come a moment after the client has read the answer, and net/http's once the
			// handshake has failed or the handler has panicked. The upstream failure's line and
			// net/http's name no answer id, so each is read by its message alone: each row has a
			// rig of its own.
			var lines []string
			for deadline := time.Now().Add(10 * time.Second); len(lines) == 0 && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				for _, record := range r.logs.Records() {
					named, carries := false, false
					record.Attrs(func(a slog.Attr) bool {
						carries = carries || a.Key == "answer_id"
						named = named || (a.Key == "answer_id" && a.Value.String() == id)
						return true
					})
					if record.Message != row.message || (carries && !named) {
						continue
					}
					out := &logtest.Buffer{}
					if err := logging.NewCloudHandler(out).Handle(context.Background(), record); err != nil {
						t.Fatalf("write the line: %v", err)
					}
					lines = append(lines, out.String())
				}
			}
			if len(lines) != 1 {
				t.Fatalf("the answer wrote %d lines under %q naming its answer id, or none, want 1", len(lines), row.message)
			}
			if size := len(lines[0]); size >= entryLimit {
				t.Fatalf("the line is %d bytes, want under the %d bytes of one Cloud Logging entry", size, entryLimit)
			}
			var written map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &written); err != nil {
				t.Fatalf("the line is not one JSON object: %v", err)
			}
			// The stack first, so that a line that kept no stack fails on the frame it lost
			// rather than on the error that holds the stack's first bytes in its place.
			stack, _ := written["stack"].(string)
			_, stacked := written["stack"]
			switch {
			case row.frame == "" && stacked:
				t.Fatalf("the line has a stack of %d bytes, want none: no handler panicked", len(stack))
			case row.frame != "" && !strings.Contains(stack, row.frame):
				t.Fatalf("the line names no frame %s, the handler that panicked: its stack is %d bytes and its error %q",
					row.frame, len(stack), written["error"])
			}
			held := row.held
			if row.heldBy != nil {
				held = row.heldBy()
			}
			if row.frame == "" && len(held) <= answer.FieldBytes {
				t.Fatalf("the row's text is %d bytes, no more than the cut's %d, so the row tests no cut: %.80q", len(held), answer.FieldBytes, held)
			}
			want := held[:min(len(held), answer.FieldBytes)]
			if kept, _ := written[row.field].(string); kept != want {
				t.Fatalf("the line's %s is %d bytes, want the first %d of the %d it held: %.80q", row.field, len(kept), len(want), len(held), kept)
			}
		})
	}
	var missing []string
	for message, named := range fields {
		for _, field := range named {
			if !drove[message+" "+field] {
				missing = append(missing, message+" "+field)
			}
		}
	}
	if driven != 21 || len(missing) != 0 {
		t.Fatalf("the table drove %d rows, want 21; it never drove %v", driven, missing)
	}
}

// clientHelloOffering is one TLS record holding a TLS 1.2 ClientHello that offers suites, and
// null compression, with no session and no extension, as RFC 5246 section 7.4.1.2 lays one out:
// crypto/tls's client offers only the suites it speaks, so a row that offers others writes its
// own bytes.
func clientHelloOffering(suites []uint16) []byte {
	hello := []byte{3, 3}                      // client_version: TLS 1.2
	hello = append(hello, make([]byte, 32)...) // random
	hello = append(hello, 0)                   // session_id: none
	hello = binary.BigEndian.AppendUint16(hello, uint16(2*len(suites)))
	for _, suite := range suites {
		hello = binary.BigEndian.AppendUint16(hello, suite)
	}
	hello = append(hello, 1, 0) // compression_methods: null
	message := append([]byte{1, byte(len(hello) >> 16), byte(len(hello) >> 8), byte(len(hello))}, hello...)
	record := []byte{22, 3, 1} // a handshake record, TLS 1.0 on the wire as clients write it
	record = binary.BigEndian.AppendUint16(record, uint16(len(message)))
	return append(record, message...)
}
