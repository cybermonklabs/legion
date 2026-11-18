// packages/envoy/internal/broker/airlockproxy/dial_log_test.go
//
// What the proxy writes down about where it went. Until these rows the proxy logged one line for
// each call to a bound host and one for each refusal, and nothing at all for a tunnel it opened or
// a dial that failed: a command could hold a tunnel to any address the rules say nothing about and
// leave no trace of it, so an operator asking afterwards "did any session reach that address"
// could not answer from the log.
//
// Each row drives the real proxy through the rig of proxy_test.go and reads the records the
// proxy's own logger wrote, as the call and refusal rows do.
package airlockproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/answer"
)

// TestATunnelWritesOneLineNamingItsCallerAndAddress: a tunnel carries bytes the proxy never reads, so the
// line it writes when it opens one is the whole of what an operator can ask afterwards. It names
// who asked - the grant, its enrollment, the session the grant belongs to and the runtime it runs
// as, which is the enrollment's runtime id - what was asked for, and the address the socket was
// opened to, which is the address the check read (address.go) and not a second lookup's. The port
// is the number the command asked for, whatever spelling it wrote that number in. A plain-HTTP
// request the proxy forwards is carried the same way, nothing decided and nothing put into it, so
// the socket each one opens writes the same line, and so does a plain-HTTP upgrade, whose bytes
// cross both ways once the service has answered 101.
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

// TestNoProxyLineHoldsAValueOrPlaceholder: the airlock exists so that a secret
// is never written down, and a log line is the first place one would leak. This row plants
// made-up values where a command really puts them - the proxy password, a request header and a
// body - and the placeholder itself in every place a line reads the command's own text: the
// X-Correlation-Id of every request it sends, the host of a CONNECT's authority and of a CONNECT
// whose authority is its Host header, the port of such a Host header, the host and the zone of a
// plain-HTTP URL, the Host header of a plain-HTTP request and of a request on a bound connection,
// the path of a refused call, and the method of a request that is no proxy request. Between them
// they write every line the proxy writes about a request: a tunnel's, a forwarded plain-HTTP
// request's, each class of failed dial, a refusal of the outer server and of a bound call, and a
// carried call's. It reads every record the proxy wrote, its message and each of its fields, and
// no line may hold the placeholder in any case: the host is written lowercased, and lowercasing
// is no redaction, so every line writes placeholderMark where its text held one (maskedHandler).
func TestNoProxyLineHoldsAValueOrPlaceholder(t *testing.T) {
	const (
		plantedHeader = "planted-header-value-nothing-may-log"
		plantedBody   = "planted-body-value-nothing-may-log"
		zoned         = "2001:db8::1"
		plainHost     = "plain.example.net"
	)
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
	answer, conn, _ := r.connectRaw(net.JoinHostPort(echoHost, "443"),
		basic(workerPlaceholder), "X-Planted: "+plantedHeader+"\r\n", correlation)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d; want 200", echoHost, answer.StatusCode)
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
	// the decision refuses; and a call whose path holds the placeholder, which no rule allows.
	client := r.client(workerPlaceholder)
	r.do(client, http.MethodPost, "https://"+boundHost+"/v1/things", plantedBody,
		"Authorization", "Bearer "+workerPlaceholder, "X-Planted", plantedHeader, "X-Correlation-Id", workerPlaceholder)
	r.do(client, http.MethodGet, "https://"+boundHost+"/v1/things", "",
		"Authorization", "Bearer "+workerPlaceholder, "Host", placeholderHost, "X-Correlation-Id", workerPlaceholder)
	r.do(client, http.MethodGet, "https://"+boundHost+"/v2/"+workerPlaceholder, "",
		"Authorization", "Bearer "+workerPlaceholder, "X-Correlation-Id", workerPlaceholder)
	// The placeholder as a plain-HTTP host, in its own case, as the zone of a plain-HTTP
	// address literal, and as the Host header of a plain-HTTP request the proxy forwards.
	r.plainRaw(placeholderHost, "/x", basic(workerPlaceholder), correlation)
	r.plainRaw("["+zoned+"%25"+workerPlaceholder+"]", "/x", basic(workerPlaceholder), correlation)
	if answer, body := r.requestRaw(http.MethodGet, "http://"+plainHost+"/status", placeholderHost, basic(workerPlaceholder), correlation); answer.StatusCode != http.StatusOK {
		t.Fatalf("the forwarded plain-HTTP request answered %d %q; want 200", answer.StatusCode, body)
	}
	// The placeholder as the method of a request that is no proxy request at all.
	r.requestRaw(workerPlaceholder, "/x", "proxy.example.net", correlation)
	r.logs.tunnelsAtLeast(t, 2)
	failures := r.logs.dialFailuresAtLeast(t, 7)
	r.logs.callsAtLeast(t, 1)

	never := []string{workerPlaceholder, workerValue, plantedHeader, plantedBody}
	lowered := strings.ToLower(workerPlaceholder)
	for _, record := range r.logs.Records() {
		written := record.Message
		record.Attrs(func(a slog.Attr) bool {
			written += " " + a.Key + "=" + fmt.Sprint(a.Value.Any())
			return true
		})
		for _, secret := range never {
			if strings.Contains(written, secret) {
				t.Errorf("a line the proxy wrote holds a planted value or a placeholder: %q", written)
			}
		}
		if strings.Contains(strings.ToLower(written), lowered) {
			t.Errorf("a line the proxy wrote holds the placeholder, in some case: %q", written)
		}
	}
	// The controls: each place the placeholder was planted reached a line, which wrote the mark
	// where the placeholder was, so a line that kept that text as it came would hold it.
	refusals := r.logs.refusals()
	for _, control := range []struct {
		what, field, want string
		lines             []logLine
	}{
		{"a failed dial's host", "host", placeholderMark + ".example.net", failures},
		{"a failed dial's host, the zone gone", "host", zoned, failures},
		{"a refusal's correlation id", "correlation_id", placeholderMark, refusals},
		{"a refusal's method", "method", placeholderMark, refusals},
		{"a refusal's path", "path", "/v2/" + placeholderMark, refusals},
	} {
		if !slices.ContainsFunc(control.lines, func(line logLine) bool { return line.fields[control.field] == control.want }) {
			t.Errorf("no line names %s %q: %v", control.what, control.want, control.lines)
		}
	}

	// The control: the planted values really were sent, so a line that held one would have had
	// it to hold. The service saw the body and the header, and the swap put the real value in
	// the request it forwarded.
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service was sent %d requests; want one", len(seen))
	}
	if seen[0].Header.Get("X-Planted") != plantedHeader || seen[0].Body != plantedBody {
		t.Fatalf("the service saw X-Planted %q and body %q; want the planted header and body", seen[0].Header.Get("X-Planted"), seen[0].Body)
	}
	if got, want := seen[0].Header.Get("Authorization"), "Bearer "+workerValue; got != want {
		t.Fatalf("the service saw Authorization %q; want %q, the real value the swap put in", got, want)
	}
}

// TestADialWhoseCommandLeftWritesOneLine: a command can give up while the proxy is still looking
// its host up or dialling it - it resets its socket, and the request's context ends. A lookup or a
// dial given up that way says nothing about the service or the resolver, so its line is classed
// cancelled, not unreachable or unresolved. A dial that opened all the same, because it won that
// race, is a socket the proxy opened for the caller, so it writes the tunnel's line although the
// command is gone before its 200 could reach it.
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
	// leave writes a CONNECT to echoHost on a socket of its own, waits until the proxy is inside
	// the lookup or dial the row holds, and resets the socket, as a command that gives up does.
	leave := func(t *testing.T, r *rig, held <-chan struct{}) {
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
