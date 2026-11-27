package testnats

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/urljoin"
)

// TestConnectionsAtJoinsConnzOntoTheMonitoringURL: a monitoring endpoint's base URL
// (LEGION_TEST_NATS_MONITOR_URL, or a test's own server's) with a path and a query (path and
// query): the read of the server's client connections asks /connz joined onto that path with the
// query kept (urljoin.Join), and its errors for an answer that is not 200, one cut short and one
// that is not JSON each name that URL as oauth2call.AskedURL names it (its path, never its query).
// Where the read wrote /connz after the URL's text, /connz went inside the query and the stand-in
// was asked /prefix. A plain origin (origin) is asked /connz, as before.
func TestConnectionsAtJoinsConnzOntoTheMonitoringURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, asked, named string }{
		{"path and query", "/prefix?key=k3y-made-up", "/prefix/connz?key=k3y-made-up", "/prefix/connz"},
		{"origin", "", "/connz", "/connz"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			var respond func(http.ResponseWriter)
			monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, r.URL.RequestURI())
				answer := respond
				mu.Unlock()
				answer(w)
			}))
			t.Cleanup(monitor.Close)
			read := func(answer func(http.ResponseWriter)) ([]Connection, error) {
				mu.Lock()
				respond, asked = answer, nil
				mu.Unlock()
				connections, err := connectionsAt(monitor.URL + c.suffix)
				mu.Lock()
				defer mu.Unlock()
				if !slices.Equal(asked, []string{c.asked}) {
					t.Errorf("connectionsAt(%s) asked %q, want %q", monitor.URL+c.suffix, asked, []string{c.asked})
				}
				return connections, err
			}
			want := monitor.URL + c.named

			connections, err := read(func(w http.ResponseWriter) { fmt.Fprint(w, `{"connections":[{"cid":7,"name":"row"}]}`) })
			if err != nil || !slices.Equal(connections, []Connection{{CID: 7, Name: "row"}}) {
				t.Errorf("connectionsAt = %v, %v; want the one connection the stand-in listed", connections, err)
			}
			_, err = read(func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
			if err == nil || !strings.HasPrefix(err.Error(), "GET "+want+" answered ") {
				t.Errorf("connectionsAt of a 503 = %v; want it to name GET %s", err, want)
			}
			_, err = read(func(w http.ResponseWriter) {
				conn, buffered, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("hijack the stand-in's connection: %v", err)
					return
				}
				defer conn.Close()
				fmt.Fprint(buffered, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"connections\":")
				_ = buffered.Flush()
			})
			if err == nil || !strings.HasPrefix(err.Error(), "read "+want+": ") {
				t.Errorf("connectionsAt of an answer cut short = %v; want it to name %s", err, want)
			}
			_, err = read(func(w http.ResponseWriter) { fmt.Fprint(w, "not JSON") })
			if err == nil || !strings.HasPrefix(err.Error(), "decode "+want+": ") {
				t.Errorf("connectionsAt of an answer that is not JSON = %v; want it to name %s", err, want)
			}
		})
	}
}

// TestConnectionsAtRefusesAMonitoringURLWithNoHostQuotingNoneOfIt: a monitoring endpoint's base
// URL that is no absolute URL with a host is refused as urljoin.ErrBase before any request, in
// words that quote none of it, since the URL can carry a credential. Where the read wrote /connz
// after the URL's text, net/http's error for it quoted the URL whole.
func TestConnectionsAtRefusesAMonitoringURLWithNoHostQuotingNoneOfIt(t *testing.T) {
	_, err := connectionsAt("127.0.0.1:8222?key=k3y-made-up")
	if !errors.Is(err, urljoin.ErrBase) || strings.Contains(err.Error(), "k3y-made-up") || strings.Contains(err.Error(), "8222") {
		t.Errorf("connectionsAt of a URL with no scheme = %v; want urljoin.ErrBase, quoting none of the URL", err)
	}
}
