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
	"strings"
	"testing"
)

// TestATunnelWritesOneLineNamingItsCallerAndAddress: a tunnel carries bytes the proxy never reads, so the
// line it writes when it opens one is the whole of what an operator can ask afterwards. It names
// who asked - the grant, its enrollment, and the session and runtime of the pod behind it - what
// was asked for, and the address the socket was opened to, which is the address the check read
// (address.go) and not a second lookup's.
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
// other half of the question a tunnel's line answers. Each row drives one of the three ways a dial
// ends badly - an address this proxy never dials, a name that resolves nowhere, and a service that
// could not be reached - on each of the three paths that dial: a CONNECT, a bound host's request,
// and a plain-HTTP request. Each line names the failure's class, so an operator reads a refusal of
// the broker's own rules apart from a service that was down.
func TestAFailedDialWritesOneLineNamingItsClass(t *testing.T) {
	const (
		unreachableHost = "unreachable.example.net"
		nowhereHost     = "nowhere.example.net"
	)
	// A rig whose names cover the three failures: an address the rules refuse, a name that
	// resolves to nothing, and a public name wired to no service, whose dial the rig refuses.
	newDialRig := func(t *testing.T) *rig {
		r := newRig(t, Options{})
		r.addresses[unreachableHost] = []netip.Addr{netip.MustParseAddr("93.184.216.44")}
		r.answers[nowhereHost] = [][]netip.Addr{{}}
		return r
	}
	for _, row := range []struct {
		name, host, port, failure, address string
	}{
		{"a CONNECT to an address the proxy never dials", "10.1.2.3", "443", dialRefused, "10.1.2.3"},
		{"a CONNECT to a name that resolves nowhere", nowhereHost, "443", dialUnresolved, ""},
		{"a CONNECT to a service that cannot be reached", unreachableHost, "443", dialUnreachable, "93.184.216.44:443"},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newDialRig(t)
			target := net.JoinHostPort(row.host, row.port)
			if answer, _, _ := r.connectRaw(target, basic(workerPlaceholder)); answer.StatusCode == http.StatusOK {
				t.Fatalf("CONNECT %s = 200; want it refused or failed", target)
			}
			line := r.logs.dialFailuresAtLeast(t, 1)[0]
			for field, want := range map[string]any{
				"grant": testGrantID, "enrollment": testEnrollID, "session": testSession, "runtime": testRuntime,
				"host": row.host, "port": row.port, "failure": row.failure, "address": row.address,
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

	t.Run("a bound host's request whose dial fails", func(t *testing.T) {
		r := newDialRig(t)
		// The CONNECT's check reads a public address wired to the service; the dial of the
		// request behind it reads a public address wired to nothing, so the dial fails inside
		// the proxy's own transport.
		r.answers[boundHost] = [][]netip.Addr{{netip.MustParseAddr("93.184.216.34")}, {netip.MustParseAddr("93.184.216.44")}}
		if answer, _ := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things",
			"Authorization", "Bearer "+workerPlaceholder); answer.StatusCode == http.StatusOK {
			t.Fatal("the call to a bound host whose dial fails answered 200; want a failure")
		}
		line := r.logs.dialFailuresAtLeast(t, 1)[0]
		if line.fields["failure"] != dialUnreachable || line.fields["host"] != boundHost {
			t.Fatalf("the failed dial's line reads %v; want %s for %s", line.fields, dialUnreachable, boundHost)
		}
		if line.fields["grant"] != testGrantID || line.fields["session"] != testSession || line.fields["runtime"] != testRuntime {
			t.Fatalf("the failed dial's line names no caller: %v", line.fields)
		}
	})

	t.Run("a plain-HTTP request to an address the proxy never dials", func(t *testing.T) {
		r := newDialRig(t)
		if answer, _ := r.get(r.client(workerPlaceholder), "http://10.1.2.3/status"); answer.StatusCode != http.StatusForbidden {
			t.Fatalf("plain http to a private address answered %d; want 403", answer.StatusCode)
		}
		line := r.logs.dialFailuresAtLeast(t, 1)[0]
		if line.fields["failure"] != dialRefused || line.fields["host"] != "10.1.2.3" || line.fields["port"] != "80" {
			t.Fatalf("the failed dial's line reads %v; want %s for 10.1.2.3 at port 80", line.fields, dialRefused)
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
// body - drives every path that writes one of these lines, and reads every record the proxy wrote,
// its message and each of its fields.
func TestNoProxyLineHoldsAValueOrPlaceholder(t *testing.T) {
	const (
		plantedHeader = "planted-header-value-nothing-may-log"
		plantedBody   = "planted-body-value-nothing-may-log"
	)
	r := newRig(t, Options{})
	r.answers["nowhere.example.net"] = [][]netip.Addr{{}}

	// A tunnel that opened, with the planted value in a header of the CONNECT itself.
	answer, conn, _ := r.connectRaw(net.JoinHostPort(echoHost, "443"),
		basic(workerPlaceholder), "X-Planted: "+plantedHeader+"\r\n")
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d; want 200", echoHost, answer.StatusCode)
	}
	defer conn.Close()
	// A refused address, a name that resolves nowhere, and a call to the bound host carrying
	// the placeholder in the header the entry allows and the planted value in its body.
	client := r.client(workerPlaceholder)
	r.connectRaw(net.JoinHostPort("10.1.2.3", "443"), basic(workerPlaceholder))
	r.connectRaw(net.JoinHostPort("nowhere.example.net", "443"), basic(workerPlaceholder))
	r.do(client, http.MethodPost, "https://"+boundHost+"/v1/things", plantedBody,
		"Authorization", "Bearer "+workerPlaceholder, "X-Planted", plantedHeader)
	r.logs.tunnelsAtLeast(t, 1)
	r.logs.dialFailuresAtLeast(t, 2)
	r.logs.callsAtLeast(t, 1)

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
