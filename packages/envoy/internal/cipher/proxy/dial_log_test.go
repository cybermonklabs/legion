// packages/envoy/internal/cipher/proxy/dial_log_test.go
//
// What the proxy writes down about where it went. Until these rows the proxy logged one line for
// each call to a bound host and one for each refusal, and nothing at all for a tunnel it opened or
// a dial that failed: a command could hold a tunnel to any address the rules say nothing about and
// leave no trace of it, so an operator asking afterwards "did any session reach that address"
// could not answer from the log.
//
// Each row drives the real proxy through the rig of proxy_test.go and reads the records the
// proxy's own logger wrote, as the call and refusal rows do.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// TestATunnelWritesOneLineNamingItsCallerAndAddress: a tunnel carries bytes the proxy never reads, so the
// line it writes when it opens one is the whole of what an operator can ask afterwards. It names
// who asked - the grant, its enrollment, the session the grant belongs to and the runtime it runs
// as, which is the enrollment's runtime id - what was asked for, and the address the socket was
// opened to, which is the address the check read (address.go) and not a second lookup's. The port
// is the number the command asked for, whatever spelling it wrote that number in. A plain-HTTP
// request the proxy forwards is carried the same way, nothing decided and nothing put into it, so
// the socket each one opens writes the same line, and so does a plain-HTTP upgrade, whose bytes
// cross both ways once the service has answered 101. A forwarded request gets a socket of its
// own, so its socket's line names its own caller, even when two callers' requests go to one
// service at once and that service speaks HTTP/2, whose one connection would carry many requests.
func TestATunnelWritesOneLineNamingItsCallerAndAddress(t *testing.T) {
	// names fails t unless line names the rig's caller, the host and port asked for, and the
	// address the socket was opened to.
	names := func(t *testing.T, line logLine, host, port, address string) {
		t.Helper()
		for field, want := range map[string]any{
			"grant": testGrantID, "enrollment": testEnrollID, "session": testSession, "runtime": testRuntime,
			"issue": testIssue, "host": host, "port": port, "address": address,
		} {
			if got := line.fields[field]; got != want {
				t.Errorf("the tunnel line's %s = %v; want %v (whole line: %v)", field, got, want, line.fields)
			}
		}
	}

	r := newRig(t, Options{})
	answer, conn, _ := r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d; want 200", echoHost, answer.StatusCode)
	}
	defer conn.Close()
	lines := r.logs.tunnelsAtLeast(t, 1)
	if len(lines) != 1 {
		t.Fatalf("the proxy wrote %d tunnel lines for one tunnel; want one", len(lines))
	}
	names(t, lines[0], echoHost, "443", net.JoinHostPort(r.addresses[echoHost][0].String(), "443"))

	// A bound host is no tunnel: the proxy opens its TLS and reads every request on it, and each
	// of those requests writes its own call line. So it writes no tunnel line.
	before := len(r.logs.tunnels())
	if answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things",
		"Authorization", "Bearer "+workerPlaceholder); answer.StatusCode != http.StatusOK {
		t.Fatalf("the call to the bound host answered %d %q; want 200", answer.StatusCode, body)
	}
	r.logs.callsAtLeast(t, 1)
	if after := len(r.logs.tunnels()); after != before {
		t.Fatalf("a bound host's CONNECT wrote %d tunnel line(s); want none: it is read, not tunnelled", after-before)
	}

	// A port written with leading zeros is the number it names: the tunnel is opened to that
	// port and its line names it as the number, so a query for the port finds it.
	t.Run("a CONNECT whose port has leading zeros", func(t *testing.T) {
		r := newRig(t, Options{})
		answer, conn, _ := r.connectRaw(echoHost+":0443", basic(workerPlaceholder))
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT %s:0443 = %d; want 200", echoHost, answer.StatusCode)
		}
		defer conn.Close()
		lines := r.logs.tunnelsAtLeast(t, 1)
		if len(lines) != 1 {
			t.Fatalf("the proxy wrote %d tunnel lines for one tunnel; want one", len(lines))
		}
		names(t, lines[0], echoHost, "443", net.JoinHostPort(r.addresses[echoHost][0].String(), "443"))
	})

	// Each plain-HTTP request the proxy forwards opens a socket of its own and writes its line
	// before the answer goes back, so a row that has its answer reads the line its socket wrote.
	t.Run("a plain-HTTP request the proxy forwards", func(t *testing.T) {
		const plainHost = "plain.example.net"
		r := newRig(t, Options{})
		service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "the plain service answered")
		}))
		t.Cleanup(service.Close)
		r.addresses[plainHost] = []netip.Addr{netip.MustParseAddr("93.184.216.37")}
		r.dialTo[net.JoinHostPort(plainHost, "80")] = service.Listener.Addr().String()
		client := r.client(workerPlaceholder)
		for i := 1; i <= 2; i++ {
			if answer, body := r.get(client, "http://"+plainHost+"/status"); answer.StatusCode != http.StatusOK {
				t.Fatalf("request %d: plain http to %s answered %d %q; want 200", i, plainHost, answer.StatusCode, body)
			}
			lines := r.logs.tunnels()
			if len(lines) != i {
				t.Fatalf("after %d forwarded request(s) the proxy wrote %d tunnel line(s); want one for each", i, len(lines))
			}
			names(t, lines[i-1], plainHost, "80", "93.184.216.37:80")
		}
	})

	t.Run("a plain-HTTP upgrade the proxy joins", func(t *testing.T) {
		const upgradeHost = "upgrade.example.net"
		r := newRig(t, Options{})
		r.addresses[upgradeHost] = []netip.Addr{netip.MustParseAddr("93.184.216.38")}
		r.dialTo[net.JoinHostPort(upgradeHost, "80")] = startUpgradeEcho(t)
		conn, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatalf("dial the proxy: %v", err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(20 * time.Second))
		request := "GET http://" + upgradeHost + "/join HTTP/1.1\r\nHost: " + upgradeHost + "\r\n" +
			"Connection: Upgrade\r\nUpgrade: " + upgradeProtocol + "\r\n" + basic(workerPlaceholder) + "\r\n"
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatalf("write the upgrade: %v", err)
		}
		reader := bufio.NewReader(conn)
		answer, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil || answer.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("the upgrade answered %v, %v; want 101", answer, err)
		}
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatalf("write through the joined connection: %v", err)
		}
		echoed := make([]byte, 4)
		if _, err := io.ReadFull(reader, echoed); err != nil || string(echoed) != "ping" {
			t.Fatalf("the joined connection answered %q, %v; want the bytes sent back", echoed, err)
		}
		lines := r.logs.tunnels()
		if len(lines) != 1 {
			t.Fatalf("the proxy wrote %d tunnel lines for one upgrade; want one", len(lines))
		}
		names(t, lines[0], upgradeHost, "80", "93.184.216.38:80")
	})

	// Two callers' requests, sent at once as absolute https URLs to a service that offers HTTP/2:
	// every request reaches the service on a socket of its own, over HTTP/1.1, and each socket
	// was dialled for the caller whose request it carried, so its line names that caller.
	t.Run("forwards from two callers at once to a service that speaks HTTP/2", func(t *testing.T) {
		const (
			h2Host    = "h2.example.net"
			perCaller = 60
		)
		r := newRig(t, Options{})
		other := testConnection()
		other.GrantID, other.EnrollmentID = "grant-11111111", "enrollment-11111111"
		other.SessionID, other.RuntimeID = "session-11111111", "runtime-11111111"
		otherPlaceholder := requests.PlaceholderPrefix + "ZzYyXxWwVvUuTtSsRrQqPpOoNnMmLlKk"
		r.grants.setConnection(otherPlaceholder, other)
		placeholders := map[string]string{testGrantID: workerPlaceholder, other.GrantID: otherPlaceholder}

		// The service, with a certificate the proxy trusts upstream, offering h2 before
		// http/1.1 as a service that speaks HTTP/2 does. It holds each request a moment, so the
		// requests overlap, and records the socket each arrived on, the caller that sent it and
		// the protocol it came in.
		leaf, _, err := r.service.Authority().Leaf([]string{h2Host})
		if err != nil {
			t.Fatalf("the h2 service's certificate: %v", err)
		}
		type arrival struct {
			socket, caller string
			major          int
		}
		var arrivedMu sync.Mutex
		var arrived []arrival
		service := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			arrivedMu.Lock()
			arrived = append(arrived, arrival{request.RemoteAddr, request.Header.Get("X-Caller"), request.ProtoMajor})
			arrivedMu.Unlock()
			time.Sleep(20 * time.Millisecond)
			io.WriteString(w, "the h2 service answered")
		}))
		service.EnableHTTP2 = true
		service.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}, NextProtos: []string{"h2", "http/1.1"}}
		service.StartTLS()
		t.Cleanup(service.Close)
		r.addresses[h2Host] = []netip.Addr{netip.MustParseAddr("93.184.216.39")}
		r.dialTo[net.JoinHostPort(h2Host, "443")] = service.Listener.Addr().String()

		// send is one command's request on a connection of its own, written as no HTTP client
		// writes an https URL to a proxy but any command can. It answers rather than fails the
		// row, since it runs beside the others.
		send := func(grant string) error {
			conn, err := net.Dial("tcp", r.addr)
			if err != nil {
				return err
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(20 * time.Second))
			request := "GET https://" + h2Host + "/x HTTP/1.1\r\nHost: " + h2Host + "\r\n" + basic(placeholders[grant]) +
				"X-Caller: " + grant + "\r\nConnection: close\r\n\r\n"
			if _, err := conn.Write([]byte(request)); err != nil {
				return err
			}
			answer, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
			if err != nil {
				return err
			}
			defer answer.Body.Close()
			if _, err := io.ReadAll(answer.Body); err != nil {
				return err
			}
			if answer.StatusCode != http.StatusOK {
				return fmt.Errorf("answered %d", answer.StatusCode)
			}
			return nil
		}
		var sent sync.WaitGroup
		failed := make(chan error, 2*perCaller)
		for grant := range placeholders {
			for range perCaller {
				sent.Go(func() {
					if err := send(grant); err != nil {
						failed <- fmt.Errorf("%s: %w", grant, err)
					}
				})
			}
		}
		sent.Wait()
		close(failed)
		for err := range failed {
			t.Errorf("a forwarded request failed: %v", err)
		}

		arrivedMu.Lock()
		got := slices.Clone(arrived)
		arrivedMu.Unlock()
		if len(got) != 2*perCaller {
			t.Fatalf("the service was sent %d requests; want %d", len(got), 2*perCaller)
		}
		perSocket := map[string]int{}
		crossed, overHTTP2 := 0, 0
		for _, each := range got {
			perSocket[each.socket]++
			if dialledFor := r.socketCaller(each.socket); dialledFor != each.caller {
				crossed++
			}
			if each.major != 1 {
				overHTTP2++
			}
		}
		shared := 0
		for _, requests := range perSocket {
			if requests > 1 {
				shared++
			}
		}
		if crossed != 0 || shared != 0 || overHTTP2 != 0 || r.dials.Load() != int64(len(got)) {
			t.Errorf("of %d requests, %d went on a socket dialled for the other caller, %d sockets carried more than one, %d went over HTTP/2, and the proxy dialled %d sockets; want none, none, none and one each",
				len(got), crossed, shared, overHTTP2, r.dials.Load())
		}
		lines := r.logs.tunnels()
		named := map[any]int{}
		for _, line := range lines {
			named[line.fields["grant"]]++
		}
		if len(lines) != 2*perCaller || named[testGrantID] != perCaller || named[other.GrantID] != perCaller {
			t.Errorf("the proxy wrote %d tunnel lines, naming the callers %v; want %d for each caller", len(lines), named, perCaller)
		}
	})
}

// upgradeProtocol is the protocol startUpgradeEcho switches to: a name of the row's own, since
// what crosses after the 101 is no protocol the proxy reads.
const upgradeProtocol = "airlock-test-echo"

// startUpgradeEcho is a plain-HTTP service that answers every request 101, switching to
// upgradeProtocol, and then writes back whatever it is sent: the service behind a plain-HTTP
// upgrade, whose bytes cross both ways once it has answered.
func startUpgradeEcho(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the upgrade service: %v", err)
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
				reader := bufio.NewReader(conn)
				if _, err := http.ReadRequest(reader); err != nil {
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+upgradeProtocol+"\r\n\r\n"); err != nil {
					return
				}
				io.Copy(conn, reader)
			}()
		}
	}()
	return listener.Addr().String()
}

// TestAFailedDialWritesOneLineNamingItsClass: a dial that never happened and one that failed are the
// other half of the question a tunnel's line answers. The rows drive each of the three ways a dial
// ends badly - an address this proxy never dials, a name that resolves nowhere, and a service that
// could not be reached - on each of the three paths that dial: a CONNECT, a bound host's request,
// and a plain-HTTP request, nine rows, and two more about the port a plain-HTTP request names:
// one written with leading zeros, and the port an https URL names by its scheme alone. Each line
// names its caller, the failure's class, the address as ip:port - the one refused, or each one
// dialled - and how many addresses the host resolved to, so an operator reads a refusal of the
// broker's own rules apart from a service that was down. Each failure writes exactly one: the
// line is written before the answer goes back, so a row that has its answer reads the whole of
// what its failure wrote.
func TestAFailedDialWritesOneLineNamingItsClass(t *testing.T) {
	const (
		unreachableHost = "unreachable.example.net"
		nowhereHost     = "nowhere.example.net"
	)
	private := netip.MustParseAddr("10.1.2.3")
	unwired := netip.MustParseAddr("93.184.216.44")
	// A rig whose names cover the three failures: an address the rules refuse, a name that
	// resolves to nothing, and a public name wired to no service, whose dial the rig refuses.
	newDialRig := func(t *testing.T) *rig {
		r := newRig(t, Options{})
		r.addresses[unreachableHost] = []netip.Addr{unwired}
		r.answers[nowhereHost] = [][]netip.Addr{{}}
		return r
	}
	// The three paths, each answering the status the command was given. A CONNECT's check is
	// where its dial fails or is refused. A bound host's CONNECT is checked against the address
	// wired to the service, and the request behind it is dialled by the proxy's own transport,
	// which reads the name again and is given atDial. A plain-HTTP request is checked before it
	// is forwarded, and dialled by a transport of the proxy's once the check passes.
	connect := func(host string) func(*rig) int {
		return func(r *rig) int {
			answer, _, _ := r.connectRaw(net.JoinHostPort(host, "443"), basic(workerPlaceholder))
			return answer.StatusCode
		}
	}
	bound := func(atDial []netip.Addr) func(*rig) int {
		return func(r *rig) int {
			r.answers[boundHost] = [][]netip.Addr{r.addresses[boundHost], atDial}
			answer, _ := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things",
				"Authorization", "Bearer "+workerPlaceholder)
			return answer.StatusCode
		}
	}
	plain := func(host string) func(*rig) int {
		return func(r *rig) int {
			answer, _ := r.get(r.client(workerPlaceholder), "http://"+host+"/status")
			return answer.StatusCode
		}
	}
	// absolute is a proxy request for a whole URL written on a socket of the row's own, for a
	// URL net/http's client would not send that way: a port with leading zeros, or an https URL,
	// which the client sends as a CONNECT.
	absolute := func(url, host string) func(*rig) int {
		return func(r *rig) int {
			answer, _ := r.requestRaw(http.MethodGet, url, host, basic(workerPlaceholder))
			return answer.StatusCode
		}
	}
	for _, row := range []struct {
		name                         string
		send                         func(*rig) int
		status                       int
		host, port, failure, address string
		resolved                     int64
	}{
		{"a CONNECT to an address the proxy never dials", connect("10.1.2.3"), http.StatusForbidden,
			"10.1.2.3", "443", dialRefused, "10.1.2.3:443", 1},
		{"a CONNECT to a name that resolves nowhere", connect(nowhereHost), http.StatusForbidden,
			nowhereHost, "443", dialUnresolved, "", 0},
		{"a CONNECT to a service that cannot be reached", connect(unreachableHost), http.StatusBadGateway,
			unreachableHost, "443", dialUnreachable, "93.184.216.44:443", 1},
		{"a bound host's request whose dial reads an address the proxy never dials", bound([]netip.Addr{private}), http.StatusBadGateway,
			boundHost, "443", dialRefused, "10.1.2.3:443", 1},
		{"a bound host's request whose dial reads no address", bound([]netip.Addr{}), http.StatusBadGateway,
			boundHost, "443", dialUnresolved, "", 0},
		{"a bound host's request whose dial reaches no service", bound([]netip.Addr{unwired}), http.StatusBadGateway,
			boundHost, "443", dialUnreachable, "93.184.216.44:443", 1},
		{"a plain-HTTP request to an address the proxy never dials", plain("10.1.2.3"), http.StatusForbidden,
			"10.1.2.3", "80", dialRefused, "10.1.2.3:80", 1},
		{"a plain-HTTP request to a name that resolves nowhere", plain(nowhereHost), http.StatusForbidden,
			nowhereHost, "80", dialUnresolved, "", 0},
		{"a plain-HTTP request to a service that cannot be reached", plain(unreachableHost), http.StatusBadGateway,
			unreachableHost, "80", dialUnreachable, "93.184.216.44:80", 1},
		{"a plain-HTTP request whose port has leading zeros", absolute("http://10.1.2.3:0080/status", "10.1.2.3:0080"), http.StatusForbidden,
			"10.1.2.3", "80", dialRefused, "10.1.2.3:80", 1},
		{"a proxy request for an https URL that names no port", absolute("https://10.1.2.3/status", "10.1.2.3"), http.StatusForbidden,
			"10.1.2.3", "443", dialRefused, "10.1.2.3:443", 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newDialRig(t)
			if status := row.send(r); status != row.status {
				t.Fatalf("the command was answered %d; want %d", status, row.status)
			}
			lines := r.logs.dialFailures()
			if len(lines) != 1 {
				t.Fatalf("the proxy wrote %d failed-dial lines for one failure; want one: %v", len(lines), lines)
			}
			line := lines[0]
			for field, want := range map[string]any{
				"grant": testGrantID, "enrollment": testEnrollID, "session": testSession, "runtime": testRuntime,
				"issue": testIssue, "host": row.host, "port": row.port, "failure": row.failure, "address": row.address,
				"resolved": row.resolved,
			} {
				if got := line.fields[field]; got != want {
					t.Errorf("the failed dial's %s = %v; want %v (whole line: %v)", field, got, want, line.fields)
				}
			}
			if reason, _ := line.fields["reason"].(string); reason == "" {
				t.Errorf("the failed dial's line carries no reason: %v", line.fields)
			}
		})
	}

	// A name with more addresses than one field holds: the line lists as many whole addresses as
	// fit, in the order they were dialled, and says how many the name had. A cut through the last
	// one would leave half an address, which can read as another address.
	t.Run("a name whose addresses do not all fit in the line", func(t *testing.T) {
		const manyHost = "many.example.net"
		r := newDialRig(t)
		var dialled []string
		for i := range 40 {
			addr := netip.AddrFrom4([4]byte{93, 184, 217, byte(100 + i)})
			r.addresses[manyHost] = append(r.addresses[manyHost], addr)
			dialled = append(dialled, net.JoinHostPort(addr.String(), "443"))
		}
		if status := connect(manyHost)(r); status != http.StatusBadGateway {
			t.Fatalf("CONNECT %s = %d; want 502", manyHost, status)
		}
		lines := r.logs.dialFailures()
		if len(lines) != 1 {
			t.Fatalf("the proxy wrote %d failed-dial lines for one failure; want one: %v", len(lines), lines)
		}
		line := lines[0]
		address, _ := line.fields["address"].(string)
		listed := strings.Split(address, " ")
		if len(address) > answer.FieldBytes || len(listed) == 0 || len(listed) >= len(dialled) || !slices.Equal(listed, dialled[:len(listed)]) {
			t.Fatalf("the line lists %q; want whole addresses only, the first of the %d dialled, within %d bytes", address, len(dialled), answer.FieldBytes)
		}
		if line.fields["resolved"] != int64(len(dialled)) {
			t.Fatalf("the line's resolved = %v; want %d, every address the name had", line.fields["resolved"], len(dialled))
		}
	})

	// A bound host's connection outlives the grant read its CONNECT made: each request on it
	// reads the grant again, and a dial made for that request names the caller that read
	// answered, as the request's own call line does, and not the one the CONNECT was read for.
	t.Run("a bound host's request whose grant moved after its CONNECT", func(t *testing.T) {
		r := newDialRig(t)
		r.answers[boundHost] = [][]netip.Addr{r.addresses[boundHost], {unwired}}
		answer, conn, _ := r.connectRaw(net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT %s = %d; want 200", boundHost, answer.StatusCode)
		}
		moved := testConnection()
		moved.EnrollmentID, moved.SessionID, moved.RuntimeID = "enrollment-moved", "session-moved", "runtime-moved"
		r.grants.setConnection(workerPlaceholder, moved)
		client := tls.Client(conn, &tls.Config{RootCAs: r.trust, ServerName: boundHost})
		client.SetDeadline(time.Now().Add(20 * time.Second))
		request := "GET /v1/things HTTP/1.1\r\nHost: " + boundHost + "\r\nAuthorization: Bearer " + workerPlaceholder + "\r\nConnection: close\r\n\r\n"
		if _, err := client.Write([]byte(request)); err != nil {
			t.Fatalf("write the request on the decrypted connection: %v", err)
		}
		served, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodGet})
		if err != nil || served.StatusCode != http.StatusBadGateway {
			t.Fatalf("the request answered %v, %v; want 502", served, err)
		}
		lines := r.logs.dialFailures()
		if len(lines) != 1 {
			t.Fatalf("the proxy wrote %d failed-dial lines for one failure; want one: %v", len(lines), lines)
		}
		for field, want := range map[string]any{
			"grant": testGrantID, "enrollment": moved.EnrollmentID, "session": moved.SessionID, "runtime": moved.RuntimeID,
		} {
			if got := lines[0].fields[field]; got != want {
				t.Errorf("the failed dial's %s = %v; want %v, the grant the request was read for (whole line: %v)", field, got, want, lines[0].fields)
			}
		}
	})

	// The controls: a tunnel that opened and a call to a bound host that was carried write no
	// failure line at all, so the lines above are about the failures and not about every dial.
	t.Run("control: a tunnel that opened and a call that was carried write no failure line", func(t *testing.T) {
		r := newDialRig(t)
		answer, conn, _ := r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(workerPlaceholder))
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT %s = %d; want 200", echoHost, answer.StatusCode)
		}
		defer conn.Close()
		if answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things",
			"Authorization", "Bearer "+workerPlaceholder); answer.StatusCode != http.StatusOK {
			t.Fatalf("the call to the bound host answered %d %q; want 200", answer.StatusCode, body)
		}
		r.logs.tunnelsAtLeast(t, 1)
		r.logs.callsAtLeast(t, 1)
		if failures := r.logs.dialFailures(); len(failures) != 0 {
			t.Fatalf("the proxy wrote %d failed-dial line(s) for a tunnel that opened and a call it carried; want none: %v", len(failures), failures)
		}
	})
}

// TestADialWhoseCommandLeftWritesOneLine: a command can give up while the proxy is still looking
// its host up or dialling it - it resets its socket, and the request's context ends. A lookup or a
// dial given up that way says nothing about the service or the resolver, so its line is classed
// cancelled, not unreachable or unresolved. A dial one of whose addresses failed on its own before
// the command left keeps that failure's class. A dial that opened all the same, because it won
// that race, is a socket the proxy opened for the caller, so it writes the tunnel's line although
// the command is gone before its 200 could reach it.
func TestADialWhoseCommandLeftWritesOneLine(t *testing.T) {
	target := net.JoinHostPort(echoHost, "443")
	// until is a hold: it tells the row once that the proxy has reached it, and then waits for
	// the request's context to end, which is the command leaving.
	until := func(held chan struct{}) func(context.Context) {
		var once sync.Once
		return func(ctx context.Context) {
			once.Do(func() { close(held) })
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
		}
	}
	// leaveFor writes a CONNECT to target on a socket of its own, waits until the proxy is inside
	// the lookup or dial the row holds, and resets the socket, as a command that gives up does;
	// leave is that for echoHost.
	leaveFor := func(t *testing.T, r *rig, held <-chan struct{}, target string) {
		t.Helper()
		conn, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatalf("dial the proxy: %v", err)
		}
		request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n" + basic(workerPlaceholder) + "\r\n"
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatalf("write the CONNECT: %v", err)
		}
		select {
		case <-held:
		case <-time.After(10 * time.Second):
			t.Fatal("the proxy never reached the lookup or the dial the row holds")
		}
		if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
			t.Fatalf("set the socket to reset on close: %v", err)
		}
		conn.Close()
	}
	leave := func(t *testing.T, r *rig, held <-chan struct{}) {
		t.Helper()
		leaveFor(t, r, held, target)
	}
	// names fails t unless line names the rig's caller, echoHost and its port, and address.
	names := func(t *testing.T, line logLine, address string) {
		t.Helper()
		for field, want := range map[string]any{
			"grant": testGrantID, "enrollment": testEnrollID, "session": testSession, "runtime": testRuntime,
			"host": echoHost, "port": "443", "address": address,
		} {
			if got := line.fields[field]; got != want {
				t.Errorf("the line's %s = %v; want %v (whole line: %v)", field, got, want, line.fields)
			}
		}
	}
	dialled := func(r *rig) string { return net.JoinHostPort(r.addresses[echoHost][0].String(), "443") }

	t.Run("a dial that opened after its command left", func(t *testing.T) {
		r := newRig(t, Options{})
		held := make(chan struct{})
		wait := until(held)
		r.holdDial = func(ctx context.Context) context.Context {
			wait(ctx)
			// The dial opens all the same, as one that won the race with the command's going.
			return context.WithoutCancel(ctx)
		}
		leave(t, r, held)
		lines := r.logs.tunnelsAtLeast(t, 1)
		if len(lines) != 1 {
			t.Fatalf("the proxy wrote %d tunnel lines for one socket it opened; want one", len(lines))
		}
		names(t, lines[0], dialled(r))
		if failures := r.logs.dialFailures(); len(failures) != 0 {
			t.Fatalf("a dial that opened wrote %d failed-dial line(s); want none: %v", len(failures), failures)
		}
	})

	t.Run("a dial its command abandoned", func(t *testing.T) {
		r := newRig(t, Options{})
		held := make(chan struct{})
		wait := until(held)
		r.holdDial = func(ctx context.Context) context.Context {
			wait(ctx)
			return ctx
		}
		leave(t, r, held)
		lines := r.logs.dialFailuresAtLeast(t, 1)
		if len(lines) != 1 {
			t.Fatalf("the proxy wrote %d failed-dial lines for one dial; want one: %v", len(lines), lines)
		}
		names(t, lines[0], dialled(r))
		if lines[0].fields["failure"] != dialCancelled || lines[0].fields["resolved"] != int64(1) {
			t.Fatalf("the abandoned dial's line reads %v; want %s, with the one address resolved", lines[0].fields, dialCancelled)
		}
		if tunnels := r.logs.tunnels(); len(tunnels) != 0 {
			t.Fatalf("an abandoned dial wrote %d tunnel line(s); want none", len(tunnels))
		}
	})

	t.Run("a lookup its command abandoned", func(t *testing.T) {
		r := newRig(t, Options{})
		held := make(chan struct{})
		r.holdResolve = until(held)
		leave(t, r, held)
		lines := r.logs.dialFailuresAtLeast(t, 1)
		if len(lines) != 1 {
			t.Fatalf("the proxy wrote %d failed-dial lines for one lookup; want one: %v", len(lines), lines)
		}
		names(t, lines[0], "")
		if lines[0].fields["failure"] != dialCancelled || lines[0].fields["resolved"] != int64(0) {
			t.Fatalf("the abandoned lookup's line reads %v; want %s, with no address resolved", lines[0].fields, dialCancelled)
		}
		if r.dials.Load() != 0 {
			t.Fatalf("the proxy dialled %v for a lookup its command abandoned; want nothing", r.dialedAddresses())
		}
	})

	// A host with two addresses, the first of which fails its dial before the command leaves,
	// and the command leaves while the second is being dialled. The first failure is the
	// service's own and says it could not be reached, so the line keeps that class rather than
	// cancelled, which is for a dial whose every attempt was given up.
	t.Run("a dial whose first address failed before its command left", func(t *testing.T) {
		const twiceHost = "twice.example.net"
		r := newRig(t, Options{})
		r.addresses[twiceHost] = []netip.Addr{netip.MustParseAddr("93.184.216.44"), r.addresses[echoHost][0]}
		held := make(chan struct{})
		wait := until(held)
		var dials atomic.Int64
		r.holdDial = func(ctx context.Context) context.Context {
			// The first address is wired to nothing and fails at once; the second is held.
			if dials.Add(1) == 2 {
				wait(ctx)
			}
			return ctx
		}
		leaveFor(t, r, held, net.JoinHostPort(twiceHost, "443"))
		lines := r.logs.dialFailuresAtLeast(t, 1)
		if len(lines) != 1 {
			t.Fatalf("the proxy wrote %d failed-dial lines for one dial; want one: %v", len(lines), lines)
		}
		for field, want := range map[string]any{
			"host": twiceHost, "failure": dialUnreachable, "resolved": int64(2),
			"address": "93.184.216.44:443 " + net.JoinHostPort(r.addresses[echoHost][0].String(), "443"),
		} {
			if got := lines[0].fields[field]; got != want {
				t.Errorf("the dial's line's %s = %v; want %v (whole line: %v)", field, got, want, lines[0].fields)
			}
		}
	})

	// The same with a first address that times out on its own: the dialer's own Timeout, whose
	// error is context.DeadlineExceeded, as the production dialer's is (New). A timeout is the
	// service's failure, not the command's going, so the line keeps unreachable.
	t.Run("a dial whose first address timed out before its command left", func(t *testing.T) {
		const slowHost = "slow.example.net"
		slow := netip.MustParseAddr("93.184.216.45")
		r := newRig(t, Options{})
		r.addresses[slowHost] = []netip.Addr{slow, r.addresses[echoHost][0]}
		first := net.JoinHostPort(slow.String(), "443")
		r.dialTo[first] = r.dialTo[net.JoinHostPort(echoHost, "443")]
		r.dialTimeout = map[string]time.Duration{first: time.Nanosecond}
		held := make(chan struct{})
		wait := until(held)
		var dials atomic.Int64
		r.holdDial = func(ctx context.Context) context.Context {
			// The first address times out at once; the second is held.
			if dials.Add(1) == 2 {
				wait(ctx)
			}
			return ctx
		}
		leaveFor(t, r, held, net.JoinHostPort(slowHost, "443"))
		lines := r.logs.dialFailuresAtLeast(t, 1)
		if len(lines) != 1 {
			t.Fatalf("the proxy wrote %d failed-dial lines for one dial; want one: %v", len(lines), lines)
		}
		for field, want := range map[string]any{
			"host": slowHost, "failure": dialUnreachable, "resolved": int64(2),
			"address": first + " " + net.JoinHostPort(r.addresses[echoHost][0].String(), "443"),
		} {
			if got := lines[0].fields[field]; got != want {
				t.Errorf("the dial's line's %s = %v; want %v (whole line: %v)", field, got, want, lines[0].fields)
			}
		}
		// The control: the first dial did time out, so the row is about a timeout and not
		// about a dial that failed some other way.
		if reason, _ := lines[0].fields["reason"].(string); !strings.Contains(reason, "i/o timeout") {
			t.Errorf("the dial's reason %q names no timeout; want the first address's i/o timeout", reason)
		}
	})
}

// TestAPortThatNamesNoNumberIsRefusedBeforeAnyDial: a port is read once, as the number it names,
// before anything checks it, dials it or writes it down. A port that names no number from 1 to
// 65535 - 0, one past 16 bits, a service name, a number with a sign - is refused 400 before
// anything is dialled: the dialer reads such text by rules of its own, a service name as its
// number and a sign as nothing, so a bound host's 443 written that way would be tunnelled rather
// than read, and a port the proxy cannot read as a number is one its line could not name. That
// holds for a CONNECT's authority, for the Host header a CONNECT with a path for its target is
// read by, and for a plain-HTTP URL. The refusal's line names no port, since the port is the
// command's own text.
func TestAPortThatNamesNoNumberIsRefusedBeforeAnyDial(t *testing.T) {
	for _, row := range []struct {
		name, method, target, host, code string
	}{
		{"a CONNECT to port 0", http.MethodConnect, echoHost + ":0", echoHost + ":0", codeConnectTarget},
		{"a CONNECT to a port past 16 bits", http.MethodConnect, echoHost + ":65536", echoHost + ":65536", codeConnectTarget},
		{"a CONNECT whose Host header names a service for its port", http.MethodConnect, "/x", echoHost + ":https", codeConnectTarget},
		{"a CONNECT whose Host header gives a bound host's port a sign", http.MethodConnect, "/x", boundHost + ":+443", codeConnectTarget},
		{"a plain-HTTP request to port 0", http.MethodGet, "http://" + echoHost + ":0/x", echoHost + ":0", codeProxyTarget},
		{"a plain-HTTP request to a port past 16 bits", http.MethodGet, "http://" + echoHost + ":65536/x", echoHost + ":65536", codeProxyTarget},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t, Options{})
			answer, body := r.requestRaw(row.method, row.target, row.host, basic(workerPlaceholder))
			if answer.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s %s (Host %s) answered %d %q; want 400", row.method, row.target, row.host, answer.StatusCode, body)
			}
			refusals := r.logs.refusals()
			if len(refusals) != 1 || refusals[0].fields["code"] != row.code {
				t.Fatalf("the refusal recorded %v; want one record coded %s", refusals, row.code)
			}
			if reason := fmt.Sprint(refusals[0].fields["reason"]); strings.Contains(reason, row.host) {
				t.Fatalf("the refusal's reason %q quotes the authority the command wrote, port and all", reason)
			}
			if r.dials.Load() != 0 || len(r.logs.tunnels()) != 0 || len(r.logs.dialFailures()) != 0 {
				t.Fatalf("the proxy dialled %v and wrote %v, %v; want no dial and no dial line", r.dialedAddresses(), r.logs.tunnels(), r.logs.dialFailures())
			}
		})
	}
}

// TestAnAzureExchangeNamesTheGrantItsRequestRead: a request on a decrypted connection reads its
// grant again, and every dial made for it names that grant, as the request's own line does. A
// token request's exchange is such a dial - the proxy itself calls the token endpoint for it -
// so when the grant has moved since the CONNECT, the exchange's failed dial names the grant the
// token request read and not the one the CONNECT was read for.
func TestAnAzureExchangeNamesTheGrantItsRequestRead(t *testing.T) {
	r, _ := newAzureRig(t, Options{})
	// The CONNECT's check reads the address wired to the service; the exchange's own dial reads
	// a public address wired to nothing, so the exchange fails at its dial.
	r.answers[boundHost] = [][]netip.Addr{r.addresses[boundHost], {netip.MustParseAddr("93.184.216.44")}}
	answer, conn, _ := r.connectRaw(net.JoinHostPort(boundHost, "443"), basic(azureFederatedPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d; want 200", boundHost, answer.StatusCode)
	}
	defer conn.Close()
	moved := r.grants.connections[azureFederatedPlaceholder]
	moved.EnrollmentID, moved.SessionID, moved.RuntimeID = "enrollment-moved", "session-moved", "runtime-moved"
	r.grants.setConnection(azureFederatedPlaceholder, moved)
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {azureClientID},
		"scope":                 {azureScope},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {azureFederatedPlaceholder},
	}.Encode()
	client := tls.Client(conn, &tls.Config{RootCAs: r.trust, ServerName: boundHost})
	client.SetDeadline(time.Now().Add(20 * time.Second))
	request := "POST " + azureTokenPath + " HTTP/1.1\r\nHost: " + boundHost +
		"\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: " + fmt.Sprint(len(form)) +
		"\r\nConnection: close\r\n\r\n" + form
	if _, err := client.Write([]byte(request)); err != nil {
		t.Fatalf("write the token request on the decrypted connection: %v", err)
	}
	served, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodPost})
	if err != nil || served.StatusCode != http.StatusBadGateway {
		t.Fatalf("the token request answered %v, %v; want 502", served, err)
	}
	lines := r.logs.dialFailures()
	if len(lines) != 1 {
		t.Fatalf("the proxy wrote %d failed-dial lines for one exchange; want one: %v", len(lines), lines)
	}
	for field, want := range map[string]any{
		"grant": testGrantID, "enrollment": moved.EnrollmentID, "session": moved.SessionID, "runtime": moved.RuntimeID,
		"host": boundHost, "failure": dialUnreachable,
	} {
		if got := lines[0].fields[field]; got != want {
			t.Errorf("the exchange's failed dial's %s = %v; want %v (whole line: %v)", field, got, want, lines[0].fields)
		}
	}
}

// TestAnAzureAccessExchangeNamesTheGrantItsRequestRead: the other dial an Azure name makes for a
// request. A call carrying an access placeholder reaches a pod that holds no token for it, and
// that pod exchanges for itself (azureAccess) before it forwards the call. When the grant has
// moved since the CONNECT, that exchange's failed dial names the grant the call read too.
func TestAnAzureAccessExchangeNamesTheGrantItsRequestRead(t *testing.T) {
	r, _ := newAzureRig(t, Options{})
	_, body := tokenRequest(t, r, r.client(azureFederatedPlaceholder), azureFederatedPlaceholder)
	access := placeholderTokenOf(t, body)
	second := secondPod(t, r)
	// The second pod's CONNECT check reads the address wired to the service; its exchange's
	// dial reads a public address wired to nothing, so the exchange fails at its dial.
	r.answersMu.Lock()
	r.answers[boundHost] = [][]netip.Addr{r.addresses[boundHost], {netip.MustParseAddr("93.184.216.44")}}
	r.answersMu.Unlock()
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
	if answer, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect}); err != nil || answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s on the second pod answered %v, %v; want 200", target, answer, err)
	}
	r.grants.mu.Lock()
	moved := r.grants.connections[azureFederatedPlaceholder]
	r.grants.mu.Unlock()
	moved.EnrollmentID, moved.SessionID, moved.RuntimeID = "enrollment-moved", "session-moved", "runtime-moved"
	r.grants.setConnection(azureFederatedPlaceholder, moved)
	client := tls.Client(conn, &tls.Config{RootCAs: r.trust, ServerName: boundHost})
	request := "GET /v1/subscriptions HTTP/1.1\r\nHost: " + boundHost + "\r\nAuthorization: Bearer " + access + "\r\nConnection: close\r\n\r\n"
	if _, err := client.Write([]byte(request)); err != nil {
		t.Fatalf("write the call on the decrypted connection: %v", err)
	}
	served, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodGet})
	if err != nil || served.StatusCode == http.StatusOK {
		t.Fatalf("the call whose exchange cannot be dialled answered %v, %v; want a refusal", served, err)
	}
	lines := r.logs.dialFailures()
	if len(lines) != 1 {
		t.Fatalf("the proxy wrote %d failed-dial lines for one exchange; want one: %v", len(lines), lines)
	}
	for field, want := range map[string]any{
		"grant": testGrantID, "enrollment": moved.EnrollmentID, "session": moved.SessionID, "runtime": moved.RuntimeID,
		"host": boundHost, "failure": dialUnreachable,
	} {
		if got := lines[0].fields[field]; got != want {
			t.Errorf("the access exchange's failed dial's %s = %v; want %v (whole line: %v)", field, got, want, lines[0].fields)
		}
	}
}
