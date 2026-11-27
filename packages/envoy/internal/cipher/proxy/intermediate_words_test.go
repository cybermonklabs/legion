// packages/envoy/internal/cipher/proxy/intermediate_words_test.go
// What the line of a failed intermediate request prints ("proxy intermediate request failed"),
// for each way the request can fail: none of the credentials the request carried - the pod's own
// token, and the user information and query of the endpoint it was sent to - and no name of a
// certificate the pool's answer holds, since whoever answers in the pool's place has the request,
// token and all, and chooses every word and name it answers.
package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// wordsBearer is the token a words row's pod presents, and wordsQueryKey the value of the query
// parameter its endpoint carries: two credentials of their own, beside the probe's user
// information (redacttest.User, redacttest.Password), so a row names which one printed.
const (
	wordsBearer   = "pod-bearer-not-a-secret-token"
	wordsQueryKey = "pool-key-RDX-7f3a"
)

// wordsCredentials is every credential a words row's request can carry.
var wordsCredentials = []string{wordsBearer, redacttest.User, redacttest.Password, wordsQueryKey}

// credentialedEndpoint is base (http://host:port) with the probe's user information, the path
// /ca and a query that carries wordsQueryKey: a BROKER_PROXY_TEST_CA_URL that holds credentials.
func credentialedEndpoint(t *testing.T, base string) string {
	t.Helper()
	endpoint, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.User = url.UserPassword(redacttest.User, redacttest.Password)
	endpoint.Path, endpoint.RawQuery = "/ca", "key="+wordsQueryKey
	return endpoint.String()
}

// askedOf is the URL a twin asks at endpoint: its /ca path joined with /v1/x, its query kept.
func askedOf(endpoint string) string { return strings.Replace(endpoint, "/ca?", "/ca/v1/x?", 1) }

// bearerPool is the pool at endpoint whose pod presents wordsBearer, and basicPool the one whose
// pod presents no token, as the local stack's does, so net/http sends the endpoint's user
// information as basic authentication.
func bearerPool(endpoint string) Pool {
	return Pool{Name: testPoolName, Endpoint: endpoint, Token: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: wordsBearer})}
}

func basicPool(endpoint string) Pool { return Pool{Name: testPoolName, Endpoint: endpoint} }

// wordsRig is a pool authority for s.airlock.test on pool, on a clock of its own, not yet running.
func wordsRig(pool Pool) *poolRig {
	clock := &steppedClock{now: time.Now().Truncate(time.Second)}
	log := &syncBuffer{}
	authority := NewPoolAuthority(AuthorityOptions{Clock: clock, Hosts: []string{"s.airlock.test"}, Log: slog.New(slog.NewJSONHandler(log, nil))})
	return &poolRig{clock: clock, authority: authority, log: log, pool: pool}
}

// failedLine waits for r to have logged more than before failed requests, and answers its log as
// written then: what the line prints. The loop then sleeps on r's clock, which nobody moves, until
// the row ends.
func failedLine(t *testing.T, r *poolRig, before int) string {
	t.Helper()
	testwait.Until(t, "a failed request to be logged", func() (string, bool) {
		return r.log.String(), len(r.log.lines("proxy intermediate request failed")) > before
	})
	return r.log.String()
}

// firstFailure is the log of a words rig on pool once it has logged its first failed request.
func firstFailure(t *testing.T, pool Pool) string {
	t.Helper()
	r := wordsRig(pool)
	r.run(t, IntermediateRenewBefore)
	return failedLine(t, r, 0)
}

// rawPool answers each request, on a connection of its own, with answer(request) written on the
// connection as it is, then closes it; it answers its base URL.
func rawPool(t *testing.T, answer func(*http.Request) string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the raw pool: %v", err)
	}
	var serving sync.WaitGroup
	t.Cleanup(func() {
		_ = listener.Close()
		serving.Wait()
	})
	serving.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			serving.Go(func() {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, request.Body)
				_, _ = io.WriteString(conn, answer(request))
			})
		}
	})
	return "http://" + listener.Addr().String()
}

// abortingServer closes each connection it is sent a request on without an answer, counting the
// requests and keeping the Authorization of the last.
type abortingServer struct {
	*httptest.Server
	asked         atomic.Int32
	authorization atomic.Value
}

func newAbortingServer(t *testing.T) *abortingServer {
	t.Helper()
	s := &abortingServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		s.asked.Add(1)
		s.authorization.Store(r.Header.Get("Authorization"))
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(s.Close)
	return s
}

// plainAsk is the twin of a pod's request: a POST to endpoint (askedOf) carrying wordsBearer, on a
// client that follows redirects, as main's request went out.
func plainAsk(t *testing.T, endpoint string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, askedOf(endpoint), strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+wordsBearer)
	return (&http.Client{}).Do(request)
}

// plainFailure is plainAsk's error text, which the twin's request must fail with.
func plainFailure(t *testing.T, endpoint string) string {
	t.Helper()
	response, err := plainAsk(t, endpoint)
	if err == nil {
		response.Body.Close()
		t.Fatalf("the twin's request was answered %s", response.Status)
	}
	return err.Error()
}

// requireHolds fails the twin unless text holds a run of credential: the leak its row guards
// against is real, and through the shape the row names.
func requireHolds(t *testing.T, what, text, credential string) {
	t.Helper()
	if redacttest.Piece(text, credential) == "" {
		t.Fatalf("%s holds no run of %q, a credential the request carried, so its row cannot see a leak: %q", what, credential, text)
	}
}

// requireSays fails the row unless the error of a failed request's line in log holds want: the
// line still says what failed.
func requireSays(t *testing.T, log, want string) {
	t.Helper()
	for line := range strings.SplitSeq(log, "\n") {
		var fields map[string]any
		if json.Unmarshal([]byte(line), &fields) == nil && fields["msg"] == "proxy intermediate request failed" {
			if why, _ := fields["error"].(string); strings.Contains(why, want) {
				return
			}
		}
	}
	t.Errorf("no failed request's line says %q:\n%s", want, log)
}

// TestAPoolRequestThatGotNoAnswerPrintsNoCredentialItCarried: a request that got no answer it
// could read prints as redact.Unanswered names it, by the URL asked with no user information or
// query, and a failure net/http quotes the answering party's words in by its class. Each case's
// twin is net/http's own text of the same failure, which holds a credential the request carried.
// Captures proxy.intermediate-unanswered/<case>.
func TestAPoolRequestThatGotNoAnswerPrintsNoCredentialItCarried(t *testing.T) {
	for _, c := range []struct {
		name string
		// endpoint is the stand-in's endpoint, credentials and all, where net/http's own text of
		// the failure (plainFailure) holds holds.
		endpoint func(t *testing.T) string
		holds    string
		says     string
	}{
		{
			name:     "dropped",
			endpoint: func(t *testing.T) string { return credentialedEndpoint(t, newAbortingServer(t).URL) },
			holds:    wordsQueryKey, // net/http's error names the URL with its query
			says:     "/ca/v1/" + testPoolName + `/certificates": EOF`,
		},
		{
			name: "malformed-status",
			endpoint: func(t *testing.T) string {
				// A status line with no space, which net/http quotes whole as a malformed response.
				return credentialedEndpoint(t, rawPool(t, func(r *http.Request) string {
					return "HTTP/1.1-" + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + "\r\n\r\n"
				}))
			},
			holds: wordsBearer,
			says:  "/ca/v1/" + testPoolName + `/certificates": the request failed`,
		},
		{
			name: "location-unparsed",
			endpoint: func(t *testing.T) string {
				return credentialedEndpoint(t, rawPool(t, func(r *http.Request) string {
					return "HTTP/1.1 302 Found\r\nLocation: /%zz/" + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + "\r\nContent-Length: 0\r\n\r\n"
				}))
			},
			holds: wordsBearer,
			says:  "the answer was a redirect whose Location does not parse",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			line := firstFailure(t, bearerPool(c.endpoint(t)))
			redacttest.RequireNoPiece(t, "proxy.intermediate-unanswered/"+c.name, line, wordsCredentials...)
			requireSays(t, line, c.says)
		})
		t.Run(c.name+"-twin", func(t *testing.T) {
			requireHolds(t, "net/http's own text", plainFailure(t, c.endpoint(t)), c.holds)
		})
	}
}

// TestThePoolRequestGoesToTheResourceJoinedOntoTheEndpoint: the certificates resource is joined
// onto the endpoint's path (urljoin.Join), the endpoint's query kept ahead of the request's own and
// its fragment dropped, and each segment of the pool's name escaped, where main appended the path
// to the endpoint's text, so a query or a fragment took it in and the pool was asked another path.
// Control: an endpoint with no path or query is asked /v1/<pool>/certificates, as before.
func TestThePoolRequestGoesToTheResourceJoinedOntoTheEndpoint(t *testing.T) {
	for _, c := range []struct {
		name, endpoint, pool string
		path, query          string // the path asked, and the start of its query
	}{
		{"query", "/ca?key=k1", testPoolName, "/ca/v1/" + testPoolName + "/certificates", "key=k1&certificateId="},
		{"query-trailing-slash", "/ca/?key=k1", testPoolName, "/ca/v1/" + testPoolName + "/certificates", "key=k1&certificateId="},
		{"fragment", "/ca#part", testPoolName, "/ca/v1/" + testPoolName + "/certificates", "certificateId="},
		{"escaped-name", "", "projects/p?x=1/locations/l/caPools/c", "/v1/projects/p%3Fx=1/locations/l/caPools/c/certificates", "certificateId="},
		{"no-path", "", testPoolName, "/v1/" + testPoolName + "/certificates", "certificateId="},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan *url.URL, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case asked <- r.URL:
				default:
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(server.Close)
			pool := basicPool(server.URL + c.endpoint)
			pool.Name = c.pool
			firstFailure(t, pool)
			got := <-asked
			if got.EscapedPath() != c.path || !strings.HasPrefix(got.RawQuery, c.query) {
				t.Errorf("the pool was asked %s?%s; want %s?%s...", got.EscapedPath(), got.RawQuery, c.path, c.query)
			}
		})
	}
}

// TestAPoolEndpointThatDoesNotParsePrintsNoneOfIt: an endpoint the join cannot use, one with user
// information and a query, fails in fixed words that name its setting, never the URL. The twin is
// net/url's error for the URL main built, which quotes it whole, password and all. Captures
// proxy.intermediate-endpoint/unparsed.
func TestAPoolEndpointThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	const endpoint = "http://" + redacttest.User + ":" + redacttest.Password + "@pool.invalid/%zz?key=" + wordsQueryKey
	t.Run("unparsed", func(t *testing.T) {
		line := firstFailure(t, bearerPool(endpoint))
		redacttest.RequireNoPiece(t, "proxy.intermediate-endpoint/unparsed", line, wordsCredentials...)
		requireSays(t, line, "the pool's endpoint (BROKER_PROXY_TEST_CA_URL) is not an absolute URL with a host (the URL is not printed)")
	})
	t.Run("unparsed-twin", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodPost, endpoint+"/v1/"+testPoolName+"/certificates", nil)
		if err == nil {
			t.Fatal("the endpoint parses")
		}
		requireHolds(t, "net/url's own text", err.Error(), redacttest.Password)
	})
}
