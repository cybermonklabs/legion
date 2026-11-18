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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// TestATunnelWritesOneLineNamingItsCallerAndAddress: a tunnel carries bytes the proxy never reads, so the
// line it writes when it opens one is the whole of what an operator can ask afterwards. It names
// who asked - the grant, its enrollment, the session the grant belongs to and the runtime it runs
// as, which is the enrollment's runtime id - what was asked for, and the address the socket was
// opened to, which is the address the check read (address.go) and not a second lookup's.
func TestATunnelWritesOneLineNamingItsCallerAndAddress(t *testing.T) {
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
	for field, want := range map[string]any{
		"grant":      testGrantID,
		"enrollment": testEnrollID,
		"session":    testSession,
		"runtime":    testRuntime,
		"issue":      testIssue,
		"host":       echoHost,
		"port":       "443",
		"address":    net.JoinHostPort(r.addresses[echoHost][0].String(), "443"),
	} {
		if got := lines[0].fields[field]; got != want {
			t.Errorf("the tunnel line's %s = %v; want %v (whole line: %v)", field, got, want, lines[0].fields)
		}
	}

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
}

// TestAFailedDialWritesOneLineNamingItsClass: a dial that never happened and one that failed are the
// other half of the question a tunnel's line answers. The rows drive each of the three ways a dial
// ends badly - an address this proxy never dials, a name that resolves nowhere, and a service that
// could not be reached - on each of the three paths that dial: a CONNECT, a bound host's request,
// and a plain-HTTP request, nine rows in all. Each line names its caller and the failure's class,
// so an operator reads a refusal of the broker's own rules apart from a service that was down, and
// each failure writes exactly one: the line is written before the answer goes back, so a row that
// has its answer reads the whole of what its failure wrote.
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
	// is forwarded, and dialled by that same transport once the check passes.
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
	for _, row := range []struct {
		name                         string
		send                         func(*rig) int
		status                       int
		host, port, failure, address string
	}{
		{"a CONNECT to an address the proxy never dials", connect("10.1.2.3"), http.StatusForbidden,
			"10.1.2.3", "443", dialRefused, "10.1.2.3"},
		{"a CONNECT to a name that resolves nowhere", connect(nowhereHost), http.StatusForbidden,
			nowhereHost, "443", dialUnresolved, ""},
		{"a CONNECT to a service that cannot be reached", connect(unreachableHost), http.StatusBadGateway,
			unreachableHost, "443", dialUnreachable, "93.184.216.44:443"},
		{"a bound host's request whose dial reads an address the proxy never dials", bound([]netip.Addr{private}), http.StatusBadGateway,
			boundHost, "443", dialRefused, "10.1.2.3"},
		{"a bound host's request whose dial reads no address", bound([]netip.Addr{}), http.StatusBadGateway,
			boundHost, "443", dialUnresolved, ""},
		{"a bound host's request whose dial reaches no service", bound([]netip.Addr{unwired}), http.StatusBadGateway,
			boundHost, "443", dialUnreachable, "93.184.216.44:443"},
		{"a plain-HTTP request to an address the proxy never dials", plain("10.1.2.3"), http.StatusForbidden,
			"10.1.2.3", "80", dialRefused, "10.1.2.3"},
		{"a plain-HTTP request to a name that resolves nowhere", plain(nowhereHost), http.StatusForbidden,
			nowhereHost, "80", dialUnresolved, ""},
		{"a plain-HTTP request to a service that cannot be reached", plain(unreachableHost), http.StatusBadGateway,
			unreachableHost, "80", dialUnreachable, "93.184.216.44:80"},
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
// body - and the placeholder itself in the one place a line reads the command's own text, the
// host it asks for. It drives a tunnel that opens and each class of failed dial on a CONNECT -
// an address the proxy never dials, a name that resolves nowhere and a service that cannot be
// reached - and a call to a bound host, and reads every record the proxy wrote, its message and
// each of its fields.
func TestNoProxyLineHoldsAValueOrPlaceholder(t *testing.T) {
	const (
		plantedHeader = "planted-header-value-nothing-may-log"
		plantedBody   = "planted-body-value-nothing-may-log"
	)
	r := newRig(t, Options{})
	r.answers["nowhere.example.net"] = [][]netip.Addr{{}}
	r.addresses["unreachable.example.net"] = []netip.Addr{netip.MustParseAddr("93.184.216.44")}

	// A tunnel that opened, with the planted value in a header of the CONNECT itself.
	answer, conn, _ := r.connectRaw(net.JoinHostPort(echoHost, "443"),
		basic(workerPlaceholder), "X-Planted: "+plantedHeader+"\r\n")
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d; want 200", echoHost, answer.StatusCode)
	}
	defer conn.Close()
	// A refused address, a name that resolves nowhere, a service that cannot be reached, a host
	// that is the placeholder, and a call to the bound host carrying the placeholder in the
	// header the entry allows and the planted value in its body.
	client := r.client(workerPlaceholder)
	r.connectRaw(net.JoinHostPort("10.1.2.3", "443"), basic(workerPlaceholder))
	r.connectRaw(net.JoinHostPort("nowhere.example.net", "443"), basic(workerPlaceholder))
	r.connectRaw(net.JoinHostPort("unreachable.example.net", "443"), basic(workerPlaceholder))
	placeholderHost := workerPlaceholder + ".example.net"
	r.connectRaw(net.JoinHostPort(placeholderHost, "443"), basic(workerPlaceholder))
	r.do(client, http.MethodPost, "https://"+boundHost+"/v1/things", plantedBody,
		"Authorization", "Bearer "+workerPlaceholder, "X-Planted", plantedHeader)
	r.logs.tunnelsAtLeast(t, 1)
	failures := r.logs.dialFailuresAtLeast(t, 4)
	r.logs.callsAtLeast(t, 1)
	// The control for the host: the CONNECT that named the placeholder as its host did reach a
	// line, as the host lowercased, so a line that kept the host as it came would hold it.
	if !slices.ContainsFunc(failures, func(line logLine) bool { return line.fields["host"] == strings.ToLower(placeholderHost) }) {
		t.Fatalf("no failed-dial line names the host %q, lowercased: %v", placeholderHost, failures)
	}

	never := []string{workerPlaceholder, workerValue, plantedHeader, plantedBody}
	for _, record := range r.logs.Records() {
		written := record.Message
		record.Attrs(func(a slog.Attr) bool {
			written += " " + a.Key + "=" + fmt.Sprint(a.Value.Any())
			return true
		})
		for _, secret := range never {
			if strings.Contains(written, secret) {
				t.Fatalf("a line the proxy wrote holds a planted value or a placeholder: %q", written)
			}
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
