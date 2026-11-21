package bus

import (
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testsocks5"
)

// An admission proof through a proxy dials with a record of its own: its failed dial never becomes
// the error the client's own next connect reports (socks5Dialer.LastError), and its error names the
// setting and the proxy, as a connect's does. The client names two servers through a proxy that
// routes only the second; it connects there, its last dial a success. The proof dials the first
// server first, which the proxy refuses.
//
// Control: give the proof's dialer the client's record (c.dialer.bounded in place of
// c.dialer.detached); the client's record then holds the proof's refused dial, and the test fails.
func TestAProofThroughAProxyLeavesTheClientsDialRecordAlone(t *testing.T) {
	server, err := url.Parse(testnats.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	const routed, unrouted = "hub.example.test:4222", "unrouted.example.test:4222"
	proxy := testsocks5.StartRouted(t, map[string]string{routed: server.Host})
	environment := map[string]string{SOCKS5ProxyEnvVar: proxy.Addr, AllowRemoteEnvVar: "1"}
	// The test server's URL with its account's user, naming host as its server.
	named := func(host string) string {
		u := *server
		u.Host = host
		return u.String()
	}
	client, err := Connect([]string{named(unrouted), named(routed)},
		WithEnvironment(func(name string) (string, bool) { value, ok := environment[name]; return value, ok }),
		WithoutStreamSubjects())
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	if err := client.dialer.LastError(); err != nil {
		t.Fatalf("the client's record holds %v after it connected, want its last dial's success", err)
	}

	err = client.ProveAdmission(t.Context())
	if err == nil {
		t.Fatalf("the proof passed, though the proxy routes %s nowhere", unrouted)
	}
	for _, want := range []string{"admission proof: nats://" + unrouted + ": ", "through the SOCKS5 proxy " + SOCKS5ProxyEnvVar + "=" + proxy.Addr} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the proof's error %q does not say %q", err, want)
		}
	}
	if err := client.dialer.LastError(); err != nil {
		t.Fatalf("the client's record holds %v, the proof's refused dial", err)
	}
}
