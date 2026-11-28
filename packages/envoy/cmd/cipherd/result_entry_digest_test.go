// result_entry_digest_test.go drives REAL compiled brokers on one database with a result grant
// made under another results entry than theirs: a grant records the digest of the profile
// DISPATCH_RESULTS_TOKEN and the private destination it was made under (result_grants.entry_digest),
// and a broker whose own differs serves it nothing (requests.MintedKind.EntryDigest). The
// helpers are result_write_test.go's, result_entry_start_test.go's and main_test.go's.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
)

// entryRefusal is the words of the refusal a broker gives a result grant of another entry, and
// entryRefusalUntrue the words of an ended grant, which that refusal must not say: the grant is
// live, and a broker of its own entry serves it.
const (
	entryRefusal       = "the grant is refused by this broker: it was made under another dpop block or private destination"
	entryRefusalUntrue = "expired"
)

// startEntryBroker starts binary with env, a broker with an airlock, and answers its proxy port and
// the thumbprint of the pod key it logged.
func startEntryBroker(t *testing.T, binary string, env []string) *resultsBroker {
	t.Helper()
	process := startBroker(t, brokerOptions{Binary: binary, Env: env})
	thumbprint, _ := process.AwaitRecord(t, dpopKeyLine, nil)["thumbprint"].(string)
	if thumbprint == "" {
		t.Fatalf("the broker's %q line names no thumbprint", dpopKeyLine)
	}
	return &resultsBroker{brokerProcess: process, proxyAddr: airlockProxyAddr(t, process), thumbprint: thumbprint}
}

// connectAt is one CONNECT to host:443 through b's proxy with password as the proxy password,
// answering the proxy's answer and its body.
func connectAt(t *testing.T, b *resultsBroker, host, password string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", b.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	target := host + ":443"
	credential := base64.StdEncoding.EncodeToString([]byte("agent:" + password))
	if _, err := conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nProxy-Authorization: Basic " + credential + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	answer, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read the answer to CONNECT %s: %v", target, err)
	}
	body, _ := io.ReadAll(answer.Body)
	return answer, string(body)
}

// tryWrite is a command's result write of g to host through b's proxy, answering the status
// (0 when the proxy took no request, as a client whose CONNECT was refused gets an error) and the
// error's words.
func tryWrite(t *testing.T, g resultGrant, b *resultsBroker, host string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "https://"+host+resultWritePath, strings.NewReader(`{"verdict":"pass"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "DPoP "+g.placeholder)
	answer, err := g.client(t, b).Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer answer.Body.Close()
	body, _ := io.ReadAll(answer.Body)
	return answer.StatusCode, string(body)
}

// tokensBoundTo is how many access-token sessions of the database at databaseURL carry
// thumbprint, the key a minted results token is bound to.
func tokensBoundTo(t *testing.T, databaseURL, thumbprint string) int {
	t.Helper()
	if thumbprint == "" {
		t.Fatal("count the tokens of no thumbprint")
	}
	conn, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var bound int
	if err := conn.QueryRow(t.Context(), `select count(*) from oauth_access_tokens where strpos(request::text, $1) > 0`, thumbprint).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	return bound
}

// TestABrokerOfAnotherResultsEntryServesNoGrant: broker A, with a result entry, grants run 7 a
// placeholder. Each broker below shares A's database and passes its own start check.
//
//   - B holds the profile DISPATCH_RESULTS_TOKEN bound to evil.ci-results.test and the private
//     destination of that host: A's placeholder through B to that host is answered 407
//     GRANT_NOT_LIVE with Cipher-Proxy-Forwarded: false, in words that say the grant is refused by
//     this broker for another dpop block or private destination, and not that it expired; the
//     command's write gets no answer from a service, the stand-in of evil.ci-results.test receives
//     nothing, and no access-token session carries B's key.
//   - C holds A's profile and a destination of another URL, another loopback address and port: it
//     is refused the same way, and the stand-in at that URL receives nothing.
//   - Control: D, of A's profile and destination, stores A's write at Dispatch's stand-in (201),
//     and the token it minted is found by the same count of D's key.
func TestABrokerOfAnotherResultsEntryServesNoGrant(t *testing.T) {
	e := newResultsEnvironment(t)
	standIn := func(addr, host string) *proxyrulestest.Service {
		t.Helper()
		service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Addr: addr, Hosts: []string{host}, Authority: e.dispatch.service.Authority()})
		if err != nil {
			t.Fatalf("start the stand-in of %s: %v", host, err)
		}
		t.Cleanup(service.Close)
		service.AnswerWith(func(w http.ResponseWriter, _ *http.Request, _ proxyrulestest.Request) bool {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"stored":true}`)
			return true
		})
		return service
	}
	// Each stand-in on a loopback address of its own: BROKER_AIRLOCK_TEST_RESOLVE refuses one
	// address given two ports, and Dispatch's stand-in has 127.0.0.1.
	evil := standIn("127.0.0.2:0", otherResultsHost)
	elsewhere := standIn("127.0.0.3:0", "127.0.0.3")
	a := e.start(t)
	g := e.grant(t, a, "7")
	binary := buildBroker(t)

	routing := []string{
		"BROKER_AIRLOCK_TEST_RESOLVE=" + resultsBoundHost + "=" + e.dispatch.service.Addr() + " " + otherResultsHost + "=" + evil.Addr(),
		"BROKER_AIRLOCK_TEST_DIAL_ALLOW=" + resultsBoundHost + ":443 " + otherResultsHost + ":443",
	}
	b := startEntryBroker(t, binary, append(append(withoutSettings(e.env, "BROKER_PROFILES_FILE", "BROKER_AIRLOCK_TEST_RESOLVE", "BROKER_AIRLOCK_TEST_DIAL_ALLOW",
		"BROKER_EXCHANGE_RESULT_WORKFLOWS", "BROKER_PROXY_PRIVATE_UPSTREAM"), routing...),
		"BROKER_PROFILES_FILE="+profilesFile(t, resultsProfilesBinding(`"`+otherResultsHost+`"`)),
		"BROKER_PROXY_PRIVATE_UPSTREAM="+otherResultsHost+"=https://"+otherResultsHost))
	c := startEntryBroker(t, binary, append(withoutSettings(e.env, "BROKER_AIRLOCK_TEST_DIAL_ALLOW", "BROKER_PROXY_PRIVATE_UPSTREAM"),
		"BROKER_PROXY_PRIVATE_UPSTREAM="+resultsBoundHost+"=https://"+elsewhere.Addr(),
		"BROKER_AIRLOCK_TEST_DIAL_ALLOW="+elsewhere.Addr()))

	for _, row := range []struct {
		name    string
		broker  *resultsBroker
		host    string
		standIn *proxyrulestest.Service
	}{
		{"B, the profile and the destination of " + otherResultsHost, b, otherResultsHost, evil},
		{"C, A's profile and a destination of another loopback address and port", c, resultsBoundHost, elsewhere},
	} {
		t.Run(row.name, func(t *testing.T) {
			answer, body := connectAt(t, row.broker, row.host, g.placeholder)
			if answer.StatusCode != http.StatusProxyAuthRequired || answer.Header.Get("Cipher-Proxy-Forwarded") != "false" ||
				answer.Header.Get("Cipher-Proxy-Code") != "GRANT_NOT_LIVE" || !strings.Contains(body, entryRefusal) || strings.Contains(body, entryRefusalUntrue) {
				t.Errorf("A's placeholder in a CONNECT to %s = %d, Cipher-Proxy-Forwarded %q, Cipher-Proxy-Code %q (%q); want 407, false, GRANT_NOT_LIVE, saying %q and not %q",
					row.host, answer.StatusCode, answer.Header.Get("Cipher-Proxy-Forwarded"), answer.Header.Get("Cipher-Proxy-Code"), body, entryRefusal, entryRefusalUntrue)
			}
			status, said := tryWrite(t, g, row.broker, row.host)
			seen := row.standIn.Requests()
			t.Logf("CONNECT %s: %d %s %q; the write: %d %q; the destination's stand-in received %d request(s)",
				row.host, answer.StatusCode, answer.Header.Get("Cipher-Proxy-Code"), body, status, said, len(seen))
			if status != 0 {
				t.Errorf("A's run's write to %s = %d %q; want no answer from any service", row.host, status, said)
			}
			if len(seen) != 0 {
				t.Errorf("the stand-in of the broker's destination received %d request(s), the first with Authorization %q; want none",
					len(seen), seen[0].Header.Get("Authorization"))
			}
			if bound := tokensBoundTo(t, e.databaseURL, row.broker.thumbprint); bound != 0 {
				t.Errorf("%d access-token session(s) carry the broker's key %s; want none, since it minted nothing for A's grant", bound, row.broker.thumbprint)
			}
		})
	}

	// Control: D, of A's own profile and destination, serves A's grant, and its token is counted.
	d := e.start(t)
	answer, body := g.write(t, g.client(t, d))
	assertResultStored(t, "A's run's write through D", answer, body)
	if received := len(e.dispatch.service.Requests()); received != 1 {
		t.Errorf("Dispatch's stand-in received %d write(s); want D's one", received)
	}
	if bound := tokensBoundTo(t, e.databaseURL, d.thumbprint); bound == 0 {
		t.Errorf("no access-token session carries D's key %s; want the token D minted for A's grant, which shows the count reads a pod's key", d.thumbprint)
	}
}

// TestTheEntryDigestHoldsTheDestinationsSchemeHostAndPort: resultEntryDigest is the SHA-256 of
// the profile DISPATCH_RESULTS_TOKEN's fingerprint, a newline, and the scheme, "://" and host with
// its port of the destination of the profile's host (config.MintedEntryDigest), so two brokers of
// one profile whose destinations differ in the scheme alone, the port alone or the host alone are
// two entries and refuse each other's grants. The real-broker test above moves address and port
// together, so it holds neither part alone. The URL's root path is no part of the entry, and a
// broker with no destination, with destinations of other hosts alone, or with no results profile
// has none.
func TestTheEntryDigestHoldsTheDestinationsSchemeHostAndPort(t *testing.T) {
	profiles, err := policy.ParseProfiles([]byte(`{"profiles": [{"name": "DISPATCH_RESULTS_TOKEN", "proxy": {"hosts": ["dispatch.ci-results.test"], "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/projects/"}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	destination := func(url string) []config.PrivateUpstream {
		t.Helper()
		upstreams, err := config.ParsePrivateUpstreams("other.ci-results.test=http://10.0.0.9:8766 dispatch.ci-results.test=" + url)
		if err != nil {
			t.Fatalf("ParsePrivateUpstreams(%s): %v", url, err)
		}
		return upstreams
	}
	const base = "https://10.0.0.5:8443"
	sum := sha256.Sum256([]byte(profiles.Digest(oauthwire.AirlockResultsName) + "\n" + base))
	digest := resultEntryDigest(profiles, destination(base))
	if want := hex.EncodeToString(sum[:]); digest != want {
		t.Fatalf("resultEntryDigest(%s) = %s; want %s, the SHA-256 of the profile's fingerprint, a newline and %s", base, digest, want, base)
	}
	for _, row := range []struct{ name, url string }{
		{"the scheme alone", "http://10.0.0.5:8443"},
		{"the port alone", "https://10.0.0.5:8444"},
		{"the port written out against none", "https://10.0.0.5"},
		{"the host alone", "https://10.0.0.6:8443"},
	} {
		if other := resultEntryDigest(profiles, destination(row.url)); other == digest {
			t.Errorf("a destination that differs from %s in %s (%s) has the same digest; want another entry", base, row.name, row.url)
		}
	}
	if same := resultEntryDigest(profiles, destination(base+"/")); same != digest {
		t.Errorf("the destination %s/ has the digest %s; want %s, as its root path is no part of the entry", base, same, digest)
	}
	if none := resultEntryDigest(profiles, nil); none != "" {
		t.Errorf("a broker with no destination has the digest %q; want none", none)
	}
	if none := resultEntryDigest(profiles, destination(base)[:1]); none != "" {
		t.Errorf("a broker whose one destination is of another host has the digest %q; want none", none)
	}
	if none := resultEntryDigest(policy.Profiles{}, destination(base)); none != "" {
		t.Errorf("a broker with no results profile has the digest %q; want none", none)
	}
}
