// result_entry_digest_test.go drives REAL compiled brokers on one database with a result grant
// made under another results entry than theirs: a grant records the digest of the profile
// DISPATCH_RESULTS_TOKEN and the private destination it was made under (result_grants.entry_digest),
// and a broker whose own differs serves it nothing (requests.Machine.ResultEntryDigest). The
// helpers are result_write_test.go's, result_entry_start_test.go's and main_test.go's.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
)

// entryRefusal is the words of the refusal a broker gives a result grant of another entry.
const entryRefusal = "another results profile or private destination"

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
//     GRANT_NOT_LIVE with Airlock-Forwarded: false, in words that name another results profile or
//     private destination; the command's write gets no answer from a service, the stand-in of
//     evil.ci-results.test receives nothing, and no access-token session carries B's key.
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
		"BROKER_EXCHANGE_RESULT_WORKFLOWS", "BROKER_AIRLOCK_PRIVATE_UPSTREAM"), routing...),
		"BROKER_PROFILES_FILE="+profilesFile(t, resultsProfilesBinding(`"`+otherResultsHost+`"`)),
		"BROKER_AIRLOCK_PRIVATE_UPSTREAM="+otherResultsHost+"=https://"+otherResultsHost))
	c := startEntryBroker(t, binary, append(withoutSettings(e.env, "BROKER_AIRLOCK_TEST_DIAL_ALLOW", "BROKER_AIRLOCK_PRIVATE_UPSTREAM"),
		"BROKER_AIRLOCK_PRIVATE_UPSTREAM="+resultsBoundHost+"=https://"+elsewhere.Addr(),
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
			if answer.StatusCode != http.StatusProxyAuthRequired || answer.Header.Get("Airlock-Forwarded") != "false" ||
				answer.Header.Get("Airlock-Code") != "GRANT_NOT_LIVE" || !strings.Contains(body, entryRefusal) {
				t.Errorf("A's placeholder in a CONNECT to %s = %d, Airlock-Forwarded %q, Airlock-Code %q (%q); want 407, false, GRANT_NOT_LIVE, saying %q",
					row.host, answer.StatusCode, answer.Header.Get("Airlock-Forwarded"), answer.Header.Get("Airlock-Code"), body, entryRefusal)
			}
			status, said := tryWrite(t, g, row.broker, row.host)
			seen := row.standIn.Requests()
			t.Logf("CONNECT %s: %d %s %q; the write: %d %q; the destination's stand-in received %d request(s)",
				row.host, answer.StatusCode, answer.Header.Get("Airlock-Code"), body, status, said, len(seen))
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
	stored(t, "A's run's write through D", answer, body)
	if received := len(e.dispatch.service.Requests()); received != 1 {
		t.Errorf("Dispatch's stand-in received %d write(s); want D's one", received)
	}
	if bound := tokensBoundTo(t, e.databaseURL, d.thumbprint); bound == 0 {
		t.Errorf("no access-token session carries D's key %s; want the token D minted for A's grant, which shows the count reads a pod's key", d.thumbprint)
	}
}
