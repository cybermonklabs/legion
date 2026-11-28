// packages/envoy/internal/cipher/proxy/log_masking_test.go
//
// What of a request reaches the broker's log. The airlock exists so that a secret is never
// written down, and a log line is the first place one would leak: these rows plant made-up values
// where a command really puts them, and the placeholder itself wherever a line reads a command's
// or a service's own text, and read every line the broker would write about them.
package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/logmask"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestNoProxyLineHoldsAValueOrPlaceholder: no line the broker writes about a request holds a value
// a command sent or the placeholder. The rows plant made-up values where a command really puts
// them - the proxy password, a request header and a body - and the placeholder itself in every
// place a line reads the command's own text: the X-Correlation-Id of every request it sends, the
// host of a CONNECT's authority and of a CONNECT whose authority is its Host header, the port of
// such a Host header, the host and the zone of a plain-HTTP URL, the Host header of a plain-HTTP
// request and of a request on a bound connection, the path of a refused call and of a carried
// one, the method of a request that is no proxy request, the names a forwarded https service's
// certificate holds - the placeholder's host and a host made of the real value - which the
// upstream line and its 502 write as a fixed phrase, the trailer line a forwarded service and a
// bound one send, which the error httputil logs when it cannot read it quotes, and bytes a bound
// service sends after a whole answer, which net/http's transport quotes on the standard logger.
// Between them they write every line the proxy writes about a request: a tunnel's, a forwarded
// plain-HTTP request's, each class of failed dial, an upstream failure's, httputil's own, the
// transport's, a refusal of the outer server and of a bound call, and a carried call's.
//
// Each row reads what the broker writes: every line of the proxy's logger as the broker writes it
// (logCapture's written: the broker's JSON handler through its placeholder mask), and everything
// written to the standard logger, routed as cmd/cipherd routes it (routeAsTheBroker). No line may
// hold the placeholder in any case - the host is written lowercased, and lowercasing is no
// redaction - nor a planted value, nor any run of eight bytes of the real value the swap put in.
// The controls show each plant reached a record as the command wrote it, so a mask that let it
// through would fail the row, and that the line the broker writes holds logmask.Mark in its place.
// The two halves are subtests with a rig each, so a failure in one leaves the other's checks to
// run: the lines of the requests a command sends, and the lines the bytes a bound service sends
// reach.
func TestNoProxyLineHoldsAValueOrPlaceholder(t *testing.T) {
	t.Run("the lines of the requests a command sends", noLineOfACommandsRequestsHoldsAValueOrPlaceholder)
	t.Run("the lines the bytes a bound service sends reach", noLineABoundServicesBytesReachHoldsAValueOrPlaceholder)
}

// The values the rows plant where a command really puts them, which no line may hold.
const (
	plantedHeader = "planted-header-value-nothing-may-log"
	plantedBody   = "planted-body-value-nothing-may-log"
)

// routeAsTheBroker points the default logger at the broker's own JSON handler through its
// placeholder mask, and the log package at that logger, with logmask.SetDefault, as cmd/cipherd's
// setUpLogging does, until t ends; the lines are written into the buffer it answers. A line any
// package writes to the standard logger then passes what it passes in the broker.
func routeAsTheBroker(t *testing.T) *logtest.Buffer {
	t.Helper()
	out := &logtest.Buffer{}
	logtest.Keep(t)
	logmask.SetDefault(logging.NewCloudHandler(logmask.Writer(out)))
	return out
}

// recordText is a record as one text: its message and each of its fields as the proxy wrote them,
// before any handler, so a control can find in it what the mask had to mask.
func recordText(record slog.Record) string {
	text := record.Message
	record.Attrs(func(a slog.Attr) bool {
		text += " " + a.Key + "=" + fmt.Sprint(a.Value.Any())
		return true
	})
	return text
}

// checkNoLineHolds fails t for each text of sinks - the bytes a sink was written - that holds the
// placeholder in any case, a planted value, or a run of eight bytes of the real value.
func checkNoLineHolds(t *testing.T, sinks map[string]string) {
	t.Helper()
	lowered := strings.ToLower(workerPlaceholder)
	for sink, text := range sinks {
		if strings.Contains(strings.ToLower(text), lowered) {
			t.Errorf("%s holds the placeholder, in some case: %q", sink, text)
		}
		for _, planted := range []string{plantedHeader, plantedBody} {
			if strings.Contains(text, planted) {
				t.Errorf("%s holds the planted value %q: %q", sink, planted, text)
			}
		}
		for i := 0; i+8 <= len(workerValue); i++ {
			if run := workerValue[i : i+8]; strings.Contains(text, run) {
				t.Errorf("%s holds %q of the real value: %q", sink, run, text)
			}
		}
	}
}

// writtenWhere is whether a line written under message holds what holds says it must.
func writtenWhere(lines []map[string]any, message string, holds func(map[string]any) bool) bool {
	_, found := logtest.LineWhere(lines, func(line map[string]any) bool { return line["message"] == message && holds(line) })
	return found
}

// noLineOfACommandsRequestsHoldsAValueOrPlaceholder is the first half: one rig, the requests a
// command sends with the planted values and the placeholder in its own text, and a forwarded
// service that quotes the placeholder back in its certificate and in a trailer line.
func noLineOfACommandsRequestsHoldsAValueOrPlaceholder(t *testing.T) {
	const (
		zoned       = "2001:db8::1"
		plainHost   = "plain.example.net"
		certHost    = "cert.example.net"
		trailerHost = "trailer.example.net"
	)
	stdlog := routeAsTheBroker(t)
	r := newRig(t, Options{})
	unwired := netip.MustParseAddr("93.184.216.44")
	placeholderHost := workerPlaceholder + ".example.net"
	r.answers["nowhere.example.net"] = [][]netip.Addr{{}}
	r.addresses["unreachable.example.net"] = []netip.Addr{unwired}
	// The placeholder's host resolves, lowercased, to a public address wired to no service, so
	// a CONNECT to it and a plain-HTTP request to it each reach a dial that fails.
	r.addresses[strings.ToLower(placeholderHost)] = []netip.Addr{unwired}
	// A plain service the proxy forwards to, so a forwarded request writes its tunnel line.
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "the plain service answered")
	}))
	t.Cleanup(service.Close)
	r.addresses[plainHost] = []netip.Addr{netip.MustParseAddr("93.184.216.37")}
	r.dialTo[net.JoinHostPort(plainHost, "80")] = service.Listener.Addr().String()
	correlation := "X-Correlation-Id: " + workerPlaceholder + "\r\n"

	// A tunnel that opened, with the planted value in a header of the CONNECT itself.
	connected, conn, _ := r.connectRaw(net.JoinHostPort(echoHost, "443"),
		basic(workerPlaceholder), "X-Planted: "+plantedHeader+"\r\n", correlation)
	if connected.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d; want 200", echoHost, connected.StatusCode)
	}
	defer conn.Close()
	// A refused address, a name that resolves nowhere, a service that cannot be reached, and a
	// host that is the placeholder, as the CONNECT's authority and as the Host header of a
	// CONNECT whose target is a path, which net/http reads its authority from.
	r.connectRaw(net.JoinHostPort("10.1.2.3", "443"), basic(workerPlaceholder), correlation)
	r.connectRaw(net.JoinHostPort("nowhere.example.net", "443"), basic(workerPlaceholder), correlation)
	r.connectRaw(net.JoinHostPort("unreachable.example.net", "443"), basic(workerPlaceholder), correlation)
	r.connectRaw(net.JoinHostPort(placeholderHost, "443"), basic(workerPlaceholder), correlation)
	r.requestRaw(http.MethodConnect, "/x", net.JoinHostPort(placeholderHost, "443"), basic(workerPlaceholder), correlation)
	// The placeholder as a port, in such a Host header, which net/http does not read as a URL.
	if answer, body := r.requestRaw(http.MethodConnect, "/x", echoHost+":"+workerPlaceholder, basic(workerPlaceholder), correlation); answer.StatusCode == http.StatusOK {
		t.Fatalf("a CONNECT whose port is the placeholder answered 200 %q; want it refused", body)
	}
	// A carried call to the bound host with the placeholder in the header the entry allows and
	// the planted value in its body; a call whose Host header is the placeholder's host, which
	// the decision refuses; a call whose path holds the placeholder, which no rule allows; and one
	// whose path holds it under a prefix the rule allows, which is refused for the placeholder it
	// still carries after the swap.
	client := r.client(workerPlaceholder)
	r.do(client, http.MethodPost, "https://"+boundHost+"/v1/things", plantedBody,
		"Authorization", "Bearer "+workerPlaceholder, "X-Planted", plantedHeader)
	r.do(client, http.MethodGet, "https://"+boundHost+"/v1/things", "",
		"Authorization", "Bearer "+workerPlaceholder, "Host", placeholderHost, "X-Correlation-Id", workerPlaceholder)
	r.do(client, http.MethodGet, "https://"+boundHost+"/v2/"+workerPlaceholder, "",
		"Authorization", "Bearer "+workerPlaceholder, "X-Correlation-Id", workerPlaceholder)
	r.do(client, http.MethodGet, "https://"+boundHost+"/v1/"+workerPlaceholder, "",
		"Authorization", "Bearer "+workerPlaceholder, "X-Correlation-Id", workerPlaceholder)
	// The placeholder as a plain-HTTP host, in its own case, as the zone of a plain-HTTP
	// address literal, and as the Host header of a plain-HTTP request the proxy forwards.
	r.plainRaw(placeholderHost, "/x", basic(workerPlaceholder), correlation)
	r.plainRaw("["+zoned+"%25"+workerPlaceholder+"]", "/x", basic(workerPlaceholder), correlation)
	if answer, body := r.requestRaw(http.MethodGet, "http://"+plainHost+"/status", placeholderHost, basic(workerPlaceholder), correlation); answer.StatusCode != http.StatusOK {
		t.Fatalf("the forwarded plain-HTTP request answered %d %q; want 200", answer.StatusCode, body)
	}
	// A forwarded https request to a service whose certificate names the placeholder's host in
	// its own case and a host made of the real value the swap puts in: the proxy's transport
	// refuses the certificate, crypto/x509's error lists the names it holds with no quotation
	// mark, and the upstream line and the 502 write a fixed phrase in its place.
	certLeaf, _, err := r.service.Authority().Leaf([]string{placeholderHost, workerValue + ".example.net"})
	if err != nil {
		t.Fatalf("the certificate naming the placeholder and the real value: %v", err)
	}
	certService := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "the service whose certificate names another host answered")
	}))
	certService.TLS = &tls.Config{Certificates: []tls.Certificate{*certLeaf}}
	certService.StartTLS()
	t.Cleanup(certService.Close)
	r.addresses[certHost] = []netip.Addr{netip.MustParseAddr("93.184.216.40")}
	r.dialTo[net.JoinHostPort(certHost, "443")] = certService.Listener.Addr().String()
	certAnswer, certBody := r.requestRaw(http.MethodGet, "https://"+certHost+"/x", certHost, basic(workerPlaceholder), correlation)
	if certAnswer.StatusCode != http.StatusBadGateway || !strings.Contains(certBody, certificateNotForHost) {
		t.Fatalf("the forward to a service whose certificate names another host answered %d %q; want 502 saying %q", certAnswer.StatusCode, certBody, certificateNotForHost)
	}
	// A forwarded plain-HTTP request to a service whose chunked answer ends in a trailer line
	// that is the placeholder and no header at all: the transport cannot read it, and httputil
	// logs the error, which quotes the line.
	trailers, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the trailer service: %v", err)
	}
	t.Cleanup(func() { trailers.Close() })
	go func() {
		for {
			conn, err := trailers.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				io.WriteString(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n"+workerPlaceholder+"\r\n\r\n")
			}()
		}
	}()
	r.addresses[trailerHost] = []netip.Addr{netip.MustParseAddr("93.184.216.41")}
	r.dialTo[net.JoinHostPort(trailerHost, "80")] = trailers.Addr().String()
	trailed, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	trailed.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.WriteString(trailed, "GET http://"+trailerHost+"/x HTTP/1.1\r\nHost: "+trailerHost+"\r\n"+basic(workerPlaceholder)+correlation+"Connection: close\r\n\r\n"); err != nil {
		t.Fatalf("write the request to the trailer service: %v", err)
	}
	// The answer is cut short when the copy fails, so it is read to its end and not parsed.
	io.Copy(io.Discard, trailed)
	trailed.Close()
	copyFailed := func() bool {
		return strings.Contains(stdlog.String(), "during body copy") || slices.ContainsFunc(r.logs.lines(netHTTPMessage), func(line logLine) bool {
			return strings.Contains(fmt.Sprint(line.fields["error"]), "during body copy")
		})
	}
	for deadline := time.Now().Add(10 * time.Second); !copyFailed(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no line says the copy of the trailer service's answer failed")
		}
	}
	// The placeholder as the method of a request that is no proxy request at all.
	r.requestRaw(workerPlaceholder, "/x", "proxy.example.net", correlation)
	r.logs.tunnelsAtLeast(t, 2)
	r.logs.dialFailuresAtLeast(t, 7)
	r.logs.callsAtLeast(t, 1)

	// The control: each place the placeholder was planted reached a record as the command wrote
	// it, lowercased where the proxy lowercases a host, so a line that kept that text as it came
	// would hold it.
	planted := 0
	for _, record := range r.logs.Records() {
		if strings.Contains(strings.ToLower(recordText(record)), strings.ToLower(workerPlaceholder)) {
			planted++
		}
	}
	if planted < 8 {
		t.Errorf("%d records the proxy wrote hold the placeholder as the command wrote it; want one for each place it was planted, at least 8", planted)
	}
	// And each line the broker writes of them holds the mark where the placeholder was.
	written := r.logs.writtenLines(t)
	for _, control := range []struct {
		what, message, field, want string
	}{
		{"a failed dial's host", dialFailedMessage, "host", logmask.Mark + ".example.net"},
		{"a failed dial's host, the zone gone", dialFailedMessage, "host", zoned},
		{"a refusal's correlation id", answer.RefusalMessage, "correlation_id", logmask.Mark},
		{"a refusal's method", answer.RefusalMessage, "method", logmask.Mark},
		{"a refusal's path", answer.RefusalMessage, "path", "/v2/" + logmask.Mark},
		{"a refused call's path its rule allows", answer.RefusalMessage, "path", "/v1/" + logmask.Mark},
	} {
		if !writtenWhere(written, control.message, func(line map[string]any) bool { return line[control.field] == control.want }) {
			t.Errorf("no line the broker writes names %s %q: %q", control.what, control.want, r.logs.written.String())
		}
	}
	if !writtenWhere(written, upstreamMessage, func(line map[string]any) bool {
		return line["host"] == certHost && strings.Contains(fmt.Sprint(line["error"]), certificateNotForHost)
	}) {
		t.Errorf("no upstream line for %s says %q: %q", certHost, certificateNotForHost, r.logs.written.String())
	}
	if !writtenWhere(written, netHTTPMessage, func(line map[string]any) bool {
		text := fmt.Sprint(line["error"])
		return strings.Contains(text, "during body copy") && strings.Contains(text, logmask.PeerBytesShown)
	}) {
		t.Errorf("no net/http line says the body copy failed with the trailer written as %s: %q", logmask.PeerBytesShown, r.logs.written.String())
	}

	sinks := map[string]string{
		"the proxy's lines as the broker writes them":                       r.logs.written.String(),
		"the standard logger":                                               stdlog.String(),
		"the answer to the forward to a service whose certificate names it": certBody,
	}
	// A planted value and the real value are no placeholder, and no mask hides them: no record
	// the proxy wrote may hold one either.
	for i, record := range r.logs.Records() {
		if text := recordText(record); !strings.Contains(strings.ToLower(text), strings.ToLower(workerPlaceholder)) {
			sinks[fmt.Sprintf("record %d the proxy wrote", i)] = text
		}
	}
	checkNoLineHolds(t, sinks)

	// The control: the planted values really were sent, so a line that held one would have had
	// it to hold. The service saw the body and the header, and the swap put the real value in
	// the request it forwarded.
	seen := r.requests()
	posted := slices.IndexFunc(seen, func(request seenRequest) bool { return request.Body == plantedBody })
	if posted < 0 {
		t.Fatalf("the service never saw the call carrying the planted body: %v", seen)
	}
	if seen[posted].Header.Get("X-Planted") != plantedHeader {
		t.Fatalf("the service saw X-Planted %q; want the planted header", seen[posted].Header.Get("X-Planted"))
	}
	if got, want := seen[posted].Header.Get("Authorization"), "Bearer "+workerValue; got != want {
		t.Fatalf("the service saw Authorization %q; want %q, the real value the swap put in", got, want)
	}
}

// noLineABoundServicesBytesReachHoldsAValueOrPlaceholder is the second half: a proxy whose bound
// host is a service of the row's own that sends back the placeholder and the real value the swap
// put in, in the ways a library quotes a peer's bytes.
func noLineABoundServicesBytesReachHoldsAValueOrPlaceholder(t *testing.T) {
	stdlog := routeAsTheBroker(t)
	// A proxy whose bound host is a service of the row's own, speaking HTTP/1.1 over TLS
	// with the certificate the proxy trusts, that answers six ways. Its trailer answer ends in
	// a trailer line that is the placeholder, which httputil logs serveBound's copy of failing
	// (its ErrorLog, Server.errorLog). Its four answers that end in bytes after a whole answer, in
	// the same write - the placeholder, the request's own path, which holds the placeholder, the
	// Authorization header it was sent, which holds the real value the swap put in, and that
	// header inside a JSON fragment - leave those bytes on a connection the transport keeps for
	// the next request, and net/http's transport quotes them with log.Printf, which no ErrorLog
	// reaches and the standard logger the broker routes does (routeAsTheBroker). Its last answer is a status
	// line made of that real value, which the transport refuses quoting it, and the upstream line
	// logs that error and the 502 the command is answered holds it (badGateway). Every run of
	// bytes a library quotes from a peer is written as logmask.PeerBytesShown (logmask.HidePeerBytes), since the
	// placeholder mask cannot see a real value. A forwarded plain-HTTP service that writes before
	// it reads the request can leave the same bytes on Server.forward's connection by a race,
	// which the same standard logger covers.
	bound := newRig(t, Options{})
	leaf, _, err := bound.service.Authority().Leaf([]string{boundHost})
	if err != nil {
		t.Fatalf("the bound service's certificate: %v", err)
	}
	answers, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*leaf}})
	if err != nil {
		t.Fatalf("bind the bound service: %v", err)
	}
	t.Cleanup(func() { answers.Close() })
	var heldMu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		heldMu.Lock()
		defer heldMu.Unlock()
		for _, conn := range held {
			conn.Close()
		}
	})
	// unsolicitedClosed hears once from each connection whose answer ended in bytes after it, when
	// the proxy's transport closes that connection: net/http's transport writes the unsolicited
	// line before it closes the connection (readLoopPeekFailLocked), so a row that has heard from
	// a connection reads its line. Each of those answers is one write, so its bytes after the
	// answer are on the connection when the transport reads the answer's end.
	unsolicitedClosed := make(chan string, 8)
	go func() {
		for {
			conn, err := answers.Accept()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, conn)
			heldMu.Unlock()
			go func() {
				reader := bufio.NewReader(conn)
				unsolicited := ""
				for {
					request, err := http.ReadRequest(reader)
					if err != nil {
						conn.Close()
						if unsolicited != "" {
							unsolicitedClosed <- unsolicited
						}
						return
					}
					io.Copy(io.Discard, request.Body)
					const two = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
					switch path := request.URL.Path; path {
					case "/v1/trailer":
						io.WriteString(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n"+workerPlaceholder+"\r\n\r\n")
					case "/v1/unsolicited":
						io.WriteString(conn, two+workerPlaceholder)
						unsolicited = path
					case "/v1/echo-value":
						io.WriteString(conn, two+"echo "+request.Header.Get("Authorization")+"\r\n")
						unsolicited = path
					case "/v1/echo-json":
						// The value inside a JSON fragment, whose quotation marks the transport's
						// %q writes escaped: a reading of the quoted run that stopped at the first
						// escaped quotation mark would show the value after it.
						io.WriteString(conn, two+`{"authorization":"`+request.Header.Get("Authorization")+`"}`+"\r\n")
						unsolicited = path
					case "/v1/echo-status":
						io.WriteString(conn, "echo-"+strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")+"\r\n\r\n")
						conn.Close()
						return
					default:
						io.WriteString(conn, two+"echo "+path+"\r\n")
						unsolicited = path
					}
				}
			}()
		}
	}()
	bound.dialTo[net.JoinHostPort(boundHost, "443")] = answers.Addr().String()
	boundClient := bound.client(workerPlaceholder)
	// The last path holds the placeholder, which the proxy refuses before anything goes upstream
	// (leftoverPlaceholder): its refusal line is the record of it the control below reads.
	paths := []string{"/v1/trailer", "/v1/unsolicited", "/v1/echo-path", "/v1/echo-value", "/v1/echo-json", "/v1/echo-status", "/v1/" + workerPlaceholder}
	// The four answers that end in bytes after them. The proxy's transport puts a connection back
	// idle once it has read an answer to its end, and only then peeks for bytes after it
	// (persistConn.readLoop): bytes it finds while no request waits on the connection are the ones
	// it writes the unsolicited line about, but a next request that has taken the connection up by
	// then reads them as its own answer, a malformed one, and no line is written. A runner whose
	// CPUs are all busy can hold the transport's goroutine between the two for as long as that
	// next request takes, so each of these requests waits to hear its connection closed before the
	// next one is sent; the deadline only bounds a run that would otherwise hang.
	endsInBytes := map[string]bool{"/v1/unsolicited": true, "/v1/echo-path": true, "/v1/echo-value": true, "/v1/echo-json": true}
	answered := map[string]string{}
	for _, path := range paths {
		request, err := http.NewRequest(http.MethodGet, "https://"+boundHost+path, nil)
		if err != nil {
			t.Fatalf("build the request for %s: %v", path, err)
		}
		request.Header.Set("Authorization", "Bearer "+workerPlaceholder)
		// The trailer answer is cut short when the copy fails, so an answer is read to its end
		// and not parsed.
		if answer, err := boundClient.Do(request); err == nil {
			body, _ := io.ReadAll(answer.Body)
			answer.Body.Close()
			answered[path] = string(body)
		}
		if !endsInBytes[path] {
			continue
		}
		select {
		case closed := <-unsolicitedClosed:
			if closed != path {
				t.Fatalf("the connection the proxy's transport closed after the answer to %s ended in bytes after it is the one whose last answer was to %s (standard logger: %q)", path, closed, stdlog.String())
			}
		case <-time.After(2 * time.Minute):
			t.Fatalf("the proxy's transport did not close the connection whose answer to %s ended in bytes after it (standard logger: %q)", path, stdlog.String())
		}
	}

	// The bound copy's failure is on the bound proxy's own log, or - with no ErrorLog on
	// serveBound's ReverseProxy - on the standard logger, where the control below finds it
	// absent. httputil logs it before the handler returns, so it is written by the time the
	// command's answer has ended.
	written := bound.logs.writtenLines(t)
	copyFailed := func(line map[string]any) bool {
		text := fmt.Sprint(line["error"])
		return strings.Contains(text, "during body copy") && strings.Contains(text, logmask.PeerBytesShown)
	}
	if !writtenWhere(written, netHTTPMessage, copyFailed) {
		t.Errorf("no net/http line of the bound proxy says its body copy failed with the trailer written as %s (standard logger: %q): %q", logmask.PeerBytesShown, stdlog.String(), bound.logs.written.String())
	}
	// The 502 the command was answered for the status line made of the real value holds the
	// fixed phrase where the service's bytes were, and none of the value; the bound proxy's
	// upstream line says the answer was malformed, with the same phrase.
	if status := answered["/v1/echo-status"]; !strings.Contains(status, logmask.PeerBytesShown) {
		t.Errorf("the answer to the status line made of the real value is %q; want %s where the service's bytes were", status, logmask.PeerBytesShown)
	}
	if !writtenWhere(written, upstreamMessage, func(line map[string]any) bool {
		return strings.Contains(fmt.Sprint(line["error"]), "malformed HTTP response "+logmask.PeerBytesShown)
	}) {
		t.Errorf("no upstream line of the bound proxy says the answer was malformed with its status line written as %s: %q", logmask.PeerBytesShown, bound.logs.written.String())
	}
	// The control for the standard logger: the transport did write the four unsolicited answers
	// to it, each with the fixed phrase where the service's bytes were; the JSON handler writes
	// the quotation marks escaped.
	stdText := stdlog.String()
	if shown := strings.Count(stdText, `starting with \"[service bytes not shown]\"`); shown < 4 {
		t.Errorf("the standard logger writes %d unsolicited answers as %s; want four: %q", shown, logmask.PeerBytesShown, stdText)
	}
	// The control for the placeholder: the bound proxy wrote it down as the command sent it, in
	// the request path of the refused call that holds it, so a line that kept it would hold it.
	if !slices.ContainsFunc(bound.logs.Records(), func(record slog.Record) bool {
		return strings.Contains(recordText(record), workerPlaceholder)
	}) {
		t.Errorf("no record of the bound proxy holds the placeholder as it was sent: %v", bound.logs.Records())
	}

	sinks := map[string]string{
		"the bound proxy's lines as the broker writes them":    bound.logs.written.String(),
		"the standard logger":                                  stdText,
		"the answer to the status line made of the real value": answered["/v1/echo-status"],
	}
	for i, record := range bound.logs.Records() {
		if text := recordText(record); !strings.Contains(strings.ToLower(text), strings.ToLower(workerPlaceholder)) {
			sinks[fmt.Sprintf("record %d the bound proxy wrote", i)] = text
		}
	}
	checkNoLineHolds(t, sinks)
}

// TestNetHTTPsLineIsReadAsTheStandardLoggersIs: the line net/http writes through the proxy's
// ErrorLog (slogWriter) reads a write as the broker's standard logger does (logmask.LibraryLines):
// the bytes it quotes from a peer hidden, its first line as its error, the rest as its stack, and
// no stack for a write that ends in a blank line.
func TestNetHTTPsLineIsReadAsTheStandardLoggersIs(t *testing.T) {
	for _, c := range []struct {
		name, write, first, stack string
	}{
		{"one line", "http: TLS handshake error from 10.0.0.1:5678: EOF\n", "http: TLS handshake error from 10.0.0.1:5678: EOF", ""},
		{"a line and a blank one", "http: TLS handshake error\n\n", "http: TLS handshake error", ""},
		{"a panic and its stack", "http: panic serving 10.0.0.1:5678: boom\ngoroutine 1 [running]:\nmain.f()\n", "http: panic serving 10.0.0.1:5678: boom", "goroutine 1 [running]:\nmain.f()"},
		{"a quoted run", `httputil: ReverseProxy read error during body copy: malformed MIME header line: "` + workerValue + `"` + "\n",
			"httputil: ReverseProxy read error during body copy: malformed MIME header line: " + logmask.PeerBytesShown, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var records logtest.Recorder
			if _, err := (slogWriter{log: slog.New(&records)}).Write([]byte(c.write)); err != nil {
				t.Fatalf("write: %v", err)
			}
			fields := map[string]any{}
			for _, record := range records.Records() {
				record.Attrs(func(a slog.Attr) bool {
					fields[a.Key] = a.Value.Any()
					return true
				})
			}
			if first, stack := logmask.LibraryLines([]byte(c.write)); first != c.first || stack != c.stack {
				t.Errorf("LibraryLines = %q, %q; want %q, %q", first, stack, c.first, c.stack)
			}
			stack, stacked := fields["stack"]
			if fields["error"] != c.first || stacked != (c.stack != "") || stacked && stack != c.stack {
				t.Errorf("net/http's line holds %v; want its error %q and its stack %q, none where it is empty", fields, c.first, c.stack)
			}
		})
	}
}
