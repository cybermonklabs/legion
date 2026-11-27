// packages/envoy/internal/cipher/proxy/intermediate_words_test.go
// What the line of a failed intermediate request prints ("proxy intermediate request failed"),
// for each way the request can fail: none of the credentials the request carried - the pod's own
// token, and the user information and query of the endpoint it was sent to - and no name of a
// certificate the pool's answer holds, since whoever answers in the pool's place has the request,
// token and all, and chooses every word and name it answers.
package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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

// TestAPoolRedirectIsTheAnswerAndPrintsNoCredential: a pool that answers 302, to another server
// on the same address whose URL carries the pod's token, is not followed: the redirect is the
// answer, refused by its status, the token is sent nowhere the Location names, and the line prints
// no credential. What leaves the pod for a party other than the pool is captured with the line:
// the Authorization the redirect's target was sent, none where it was sent no request. The twin
// follows it, as a client with no redirect policy does: the target is sent the pod's token, and
// net/http's error names the Location, token and all. Captures proxy.intermediate-redirect/run.
func TestAPoolRedirectIsTheAnswerAndPrintsNoCredential(t *testing.T) {
	redirectingPool := func(t *testing.T) (string, *abortingServer) {
		target := newAbortingServer(t)
		pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			location := target.URL + "/echo/" + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, `<a href="`+location+`">Found</a>`)
		}))
		t.Cleanup(pool.Close)
		return credentialedEndpoint(t, pool.URL), target
	}
	t.Run("run", func(t *testing.T) {
		endpoint, target := redirectingPool(t)
		line := firstFailure(t, bearerPool(endpoint))
		sent, _ := target.authorization.Load().(string)
		redacttest.RequireNoPiece(t, "proxy.intermediate-redirect/run", line+"\nthe redirect's target was sent Authorization: "+sent, wordsCredentials...)
		if asked := target.asked.Load(); asked != 0 {
			t.Errorf("the redirect's target was sent %d requests; want none", asked)
		}
		requireSays(t, line, "the pool answered 302 Found")
	})
	t.Run("twin", func(t *testing.T) {
		endpoint, target := redirectingPool(t)
		requireHolds(t, "net/http's own text", plainFailure(t, endpoint), wordsBearer)
		if got, _ := target.authorization.Load().(string); got != "Bearer "+wordsBearer {
			t.Errorf("the target of the followed redirect was sent Authorization %q; want the pod's token, which net/http keeps on the same host", got)
		}
	})
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

// refusingSource is a token source whose every Token is refused as x/oauth2 refuses one, with an
// OAuth error answer whose description quotes the credential it was given.
type refusingSource struct{}

func (refusingSource) Token() (*oauth2.Token, error) {
	return nil, &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest}, ErrorCode: "invalid_grant",
		ErrorDescription: "the refresh token " + wordsBearer + " is revoked"}
}

// TestAPodTokenRefusalPrintsNoneOfItsWords: a token source whose refusal quotes the credential it
// read prints through redact.OAuth2, which holds no record of a request for it and so prints its
// class alone. The twin is the refusal's own text, which quotes the credential. Captures
// proxy.intermediate-token/refused.
func TestAPodTokenRefusalPrintsNoneOfItsWords(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		pool := bearerPool(credentialedEndpoint(t, newAbortingServer(t).URL))
		pool.Token = refusingSource{}
		line := firstFailure(t, pool)
		redacttest.RequireNoPiece(t, "proxy.intermediate-token/refused", line, wordsCredentials...)
		requireSays(t, line, "the pod's own token for the pool: ")
	})
	t.Run("refused-twin", func(t *testing.T) {
		_, err := refusingSource{}.Token()
		requireHolds(t, "x/oauth2's own text", err.Error(), wordsBearer)
	})
}

// TestAPoolAnswerPrintsNoCredentialItEchoes: an answer that echoes the request - in its status
// line, in a trailer of its body, in Google's error status or message, in a body of another shape
// - prints its status by the standard phrase and its words as the rule reads them, with every
// credential the request carried redacted, and a body that could not be read by its class. Each
// twin reads the same answer as main did, which holds the credential. Control: the words beside
// the echo still print. Captures proxy.intermediate-answer/<case>.
func TestAPoolAnswerPrintsNoCredentialItEchoes(t *testing.T) {
	echoed := func(r *http.Request) string {
		user, password, _ := r.BasicAuth()
		return "Authorization " + r.Header.Get("Authorization") + " user " + user + " password " + password + " key " + r.URL.Query().Get("key")
	}
	served := func(t *testing.T, handler http.HandlerFunc) string {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		return credentialedEndpoint(t, server.URL)
	}
	googleAnswer := func(status int, refusal func(r *http.Request) (string, string)) func(*testing.T) string {
		return func(t *testing.T) string {
			return served(t, func(w http.ResponseWriter, r *http.Request) {
				code, message := refusal(r)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": status, "status": code, "message": message}})
			})
		}
	}
	for _, c := range []struct {
		name     string
		endpoint func(t *testing.T) string
		basic    bool   // the pod presents no token, so the endpoint's user information goes as basic authentication
		holds    string // the credential the twin's reading of the answer holds
		says     string
	}{
		{
			name: "status-line",
			endpoint: func(t *testing.T) string {
				return credentialedEndpoint(t, rawPool(t, func(r *http.Request) string {
					return "HTTP/1.1 403 " + r.Header.Get("Authorization") + "\r\nContent-Length: 0\r\n\r\n"
				}))
			},
			holds: wordsBearer,
			says:  "the pool answered 403 Forbidden",
		},
		{
			name: "body-unreadable",
			endpoint: func(t *testing.T) string {
				return credentialedEndpoint(t, rawPool(t, func(r *http.Request) string {
					return "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n" + r.Header.Get("Authorization") + "\r\n\r\n"
				}))
			},
			holds: wordsBearer,
			says:  "read the pool's answer: the request failed",
		},
		{
			name: "google-message",
			endpoint: googleAnswer(http.StatusForbidden, func(r *http.Request) (string, string) {
				return "PERMISSION_DENIED", "Permission denied on resource; the request carried " + echoed(r)
			}),
			holds: wordsBearer,
			says:  "the pool answered 403 Forbidden: PERMISSION_DENIED: Permission denied on resource; the request carried Authorization ",
		},
		{
			name: "google-status",
			endpoint: googleAnswer(http.StatusUnauthorized, func(r *http.Request) (string, string) {
				return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "Request had invalid authentication credentials."
			}),
			holds: wordsBearer,
			says:  ": Request had invalid authentication credentials.",
		},
		{
			name: "other-shape",
			endpoint: func(t *testing.T) string {
				return served(t, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, "upstream refused the request; it carried "+echoed(r))
				})
			},
			basic: true,
			holds: redacttest.Password,
			says:  "the pool answered 502 Bad Gateway: upstream refused the request; it carried Authorization ",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := bearerPool(c.endpoint(t))
			if c.basic {
				pool = basicPool(pool.Endpoint)
			}
			line := firstFailure(t, pool)
			redacttest.RequireNoPiece(t, "proxy.intermediate-answer/"+c.name, line, wordsCredentials...)
			requireSays(t, line, c.says)
		})
		t.Run(c.name+"-twin", func(t *testing.T) {
			endpoint := c.endpoint(t)
			var response *http.Response
			var err error
			if c.basic {
				response, err = (&http.Client{}).Post(askedOf(endpoint), "application/json", strings.NewReader("{}"))
			} else {
				response, err = plainAsk(t, endpoint)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				requireHolds(t, "the read of the answer's body as main read it", err.Error(), c.holds)
				return
			}
			requireHolds(t, "the answer as main read it", response.Status+" "+string(body), c.holds)
		})
	}
}

// answerFunc is what a namedPool answers a CSR's key with at now: the certificate and its chain,
// each DER.
type answerFunc func(key any, now time.Time) (cert []byte, chain [][]byte)

// namedPool is a stand-in pool that answers each CSR with answer's func, on clock's time. answer
// is read again for each request, so a row can change it between two.
func namedPool(t *testing.T, clock *steppedClock, answer *atomic.Value) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PEMCSR string `json:"pemCsr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "no JSON body", http.StatusBadRequest)
			return
		}
		block, _ := pem.Decode([]byte(body.PEMCSR))
		if block == nil {
			http.Error(w, "no CSR", http.StatusBadRequest)
			return
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			http.Error(w, "no CSR", http.StatusBadRequest)
			return
		}
		cert, chain := answer.Load().(answerFunc)(csr.PublicKey, clock.Now())
		chainPEM := make([]string, len(chain))
		for i, der := range chain {
			chainPEM[i] = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"pemCertificate":      string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})),
			"pemCertificateChain": chainPEM,
		})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// issuedBy answers the pool's own shape for s.airlock.test (boundsTemplate: 7 days, path length
// 0), changed by shape, for the CSR's key, issued by issuer, with chain after it.
func issuedBy(t *testing.T, issuer testCertificate, shape func(*x509.Certificate), chain ...*x509.Certificate) answerFunc {
	return func(key any, now time.Time) ([]byte, [][]byte) {
		template := boundsTemplate(boundsAnswer{lifetime: 7 * 24 * time.Hour, names: []string{"s.airlock.test"}}, now)
		if shape != nil {
			shape(template)
		}
		der, err := x509.CreateCertificate(rand.Reader, template, issuer.cert, key, issuer.key)
		if err != nil {
			t.Errorf("issue the answer: %v", err)
		}
		raw := make([][]byte, len(chain))
		for i, c := range chain {
			raw[i] = c.Raw
		}
		return der, raw
	}
}

// TestAPoolAnswersCertificateNamesPrintNone: an answer the pod refuses for its chain - a chain
// whose last certificate is no self-signed root, one x509 finds no authority for, one under
// another root than the pod's first answer, one whose name constraints permit a name no profile
// binds - names each certificate by its place, a root by its SHA-256 and x509's failure by
// redact's words for it, never a name a certificate holds, which whoever answered in the pool's
// place chose (redacttest.CertificateName). Each twin reads the same answer as main's refusal
// did, and that text holds the name. Captures proxy.intermediate-names/<case>.
func TestAPoolAnswersCertificateNamesPrintNone(t *testing.T) {
	named := redacttest.CertificateName
	for _, c := range []struct {
		name string
		// first is the answer the pod takes first (nil for none), and refused the one it refuses
		// then; twin is the text main printed of refused's certificate and chain.
		first, refused func(t *testing.T) answerFunc
		twin           func(t *testing.T, cert *x509.Certificate, chain []*x509.Certificate) string
		says           string
	}{
		{
			name: "chain-end",
			refused: func(t *testing.T) answerFunc {
				middle := shapedIntermediate(t, testRoot(t, "root"), nil, time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), func(c *x509.Certificate) {
					c.Subject.CommonName, c.MaxPathLenZero, c.MaxPathLen = named, false, 1
				})
				return issuedBy(t, middle, nil, middle.cert)
			},
			twin: func(_ *testing.T, _ *x509.Certificate, chain []*x509.Certificate) string {
				return chain[len(chain)-1].Subject.String()
			},
			says: "the last certificate of its chain (certificate 2, the intermediate being 1) is not a self-signed root",
		},
		{
			name: "unknown-authority",
			refused: func(t *testing.T) answerFunc {
				return issuedBy(t, testRoot(t, named), nil, testRoot(t, named).cert)
			},
			twin: func(t *testing.T, cert *x509.Certificate, chain []*x509.Certificate) string {
				roots := x509.NewCertPool()
				roots.AddCert(chain[len(chain)-1])
				_, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
				if err == nil {
					t.Fatal("the namesake root verifies the answer")
				}
				return err.Error()
			},
			says: "it does not chain to its root: x509: certificate signed by unknown authority",
		},
		{
			name: "another-root",
			first: func(t *testing.T) answerFunc {
				root := testRoot(t, "first "+named)
				return issuedBy(t, root, nil, root.cert)
			},
			refused: func(t *testing.T) answerFunc {
				root := testRoot(t, named)
				return issuedBy(t, root, nil, root.cert)
			},
			twin: func(_ *testing.T, _ *x509.Certificate, chain []*x509.Certificate) string {
				return chain[len(chain)-1].Subject.String()
			},
			says: "it chains to another root (SHA-256 ",
		},
		{
			name: "name-constraints",
			refused: func(t *testing.T) answerFunc {
				root := testRoot(t, "root")
				return issuedBy(t, root, func(c *x509.Certificate) { c.PermittedDNSDomains = append(c.PermittedDNSDomains, named) }, root.cert)
			},
			twin: func(_ *testing.T, cert *x509.Certificate, _ []*x509.Certificate) string {
				return strings.Join(lowerNames(cert.PermittedDNSDomains), " ")
			},
			says: "its name constraints permit 1 other name(s); want exactly the bound hosts' names [s.airlock.test]",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var answer atomic.Value
			r := wordsRig(Pool{Name: testPoolName})
			r.pool.Endpoint = namedPool(t, r.clock, &answer)
			before := 0
			if c.first != nil {
				answer.Store(c.first(t))
				r.run(t, IntermediateRenewBefore)
				testwait.Until(t, "the first answer to be taken", func() (string, bool) {
					return r.log.String(), r.authority.Certificate() != nil
				})
				answer.Store(c.refused(t))
				before = len(r.log.lines("proxy intermediate request failed"))
				r.clock.set(r.authority.Certificate().NotAfter.Add(-time.Hour))
			} else {
				answer.Store(c.refused(t))
				r.run(t, IntermediateRenewBefore)
			}
			line := failedLine(t, r, before)
			redacttest.RequireNoPiece(t, "proxy.intermediate-names/"+c.name, line, named)
			requireSays(t, line, c.says)
		})
		t.Run(c.name+"-twin", func(t *testing.T) {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			der, chainDER := c.refused(t)(&key.PublicKey, time.Now())
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			chain := make([]*x509.Certificate, len(chainDER))
			for i, raw := range chainDER {
				if chain[i], err = x509.ParseCertificate(raw); err != nil {
					t.Fatal(err)
				}
			}
			if text := c.twin(t, cert, chain); redacttest.Piece(text, named) == "" {
				t.Fatalf("the text main printed holds no run of the certificate's name, so its row cannot see a leak: %q", text)
			}
		})
	}
}
