package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/redirects"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// The placeholders the recording carries (testdata/gke-metadata.json): the pool of a project named
// example-project, and an identity provider of the form a real cluster's has. No test here prints
// a token, the fake service's included: a failure names a token's place, never its value.
const (
	testWorkloadPool     = "example-project.svc.id.goog"
	testIdentityProvider = "https://container.googleapis.com/v1/projects/example-project/locations/example-location/clusters/example-cluster"
	userinfoEmailScope   = "https://www.googleapis.com/auth/userinfo.email"
	tokenPath            = metadataPrefix + "instance/service-accounts/default/token"
)

// flavor is the header every request under the API needs.
var flavor = map[string]string{"Metadata-Flavor": "Google"}

// recordedAnswer is one answer of GKE's metadata server as the recording holds it.
type recordedAnswer struct {
	Name           string            `json:"name"`
	Path           string            `json:"path"`
	RequestHeaders map[string]string `json:"requestHeaders"`
	Status         int               `json:"status"`
	Headers        map[string]string `json:"headers"`
	Body           string            `json:"body"`
}

func recordedAnswers(t *testing.T) []recordedAnswer {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "gke-metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recording struct {
		Answers []recordedAnswer `json:"answers"`
	}
	if err := json.Unmarshal(data, &recording); err != nil {
		t.Fatalf("the recording: %v", err)
	}
	if len(recording.Answers) == 0 {
		t.Fatal("the recording holds no answer")
	}
	return recording.Answers
}

// fakeSTS stands in for Google's security token service. It checks that each exchange is the one
// GKE's own server makes (the grant, the audience, the subject token's type and the token asked
// for) and that the subject token it is sent is what the token file holds at that moment, and
// answers a token of its own with expiresIn seconds left, or refuses the exchange while refusing.
type fakeSTS struct {
	t         *testing.T
	tokenFile string
	mu        sync.Mutex
	expiresIn int
	refusing  bool
	refused   int
	exchanges []stsExchange
	// description is the error_description a refusal answers; "" for the long default.
	description string
}

// stsExchange is one exchange the fake answered: the scope it was asked for, the subject token it
// was sent and the access token it answered with.
type stsExchange struct{ scope, subject, token string }

func (s *fakeSTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.t.Errorf("the exchange's form: %v", err)
		http.Error(w, "unreadable form", http.StatusBadRequest)
		return
	}
	for name, want := range map[string]string{
		"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
		"audience":             "identitynamespace:" + testWorkloadPool + ":" + testIdentityProvider,
		"subject_token_type":   "urn:ietf:params:oauth:token-type:jwt",
		"requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
	} {
		if got := r.PostForm.Get(name); got != want {
			s.t.Errorf("the exchange sent %s=%q, want %q", name, got, want)
		}
	}
	subject := r.PostForm.Get("subject_token")
	current, err := os.ReadFile(s.tokenFile)
	if err != nil {
		s.t.Errorf("read the token file: %v", err)
	}
	if subject != strings.TrimSpace(string(current)) {
		s.t.Error("the exchange sent a subject token other than what the token file holds")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if s.refusing {
		s.refused++
		w.WriteHeader(http.StatusBadRequest)
		// A long answer, of characters that take two bytes each, which the failure line cuts.
		description := strings.Repeat("é", 300)
		if s.description != "" {
			description = s.description
		}
		body, _ := json.Marshal(map[string]string{"error": "invalid_grant", "error_description": description})
		_, _ = w.Write(body)
		return
	}
	token := "fake-access-token-" + strconv.Itoa(len(s.exchanges)+1)
	s.exchanges = append(s.exchanges, stsExchange{scope: r.PostForm.Get("scope"), subject: subject, token: token})
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": token, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"token_type": "Bearer", "expires_in": s.expiresIn,
	})
}

func (s *fakeSTS) setExpiresIn(seconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiresIn = seconds
}

func (s *fakeSTS) refuse() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusing = true
}

// refuseWith refuses every exchange with description as the answer's error_description.
func (s *fakeSTS) refuseWith(description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusing, s.description = true, description
}

// answered is every exchange the fake answered with a token, in order.
func (s *fakeSTS) answered() []stsExchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.exchanges)
}

func (s *fakeSTS) refusals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// scopeOf is the scope the fake was asked for when it answered token, "" for a token it never
// answered.
func (s *fakeSTS) scopeOf(token string) string {
	for _, exchange := range s.answered() {
		if exchange.token == token {
			return exchange.scope
		}
	}
	return ""
}

// metadataRig is the endpoint's server under test, served over HTTP, with the fake service it
// exchanges at, the token file it reads, the clock it reads a token's remaining seconds against
// and the lines it logs.
type metadataRig struct {
	server    *metadataServer
	url       string
	sts       *fakeSTS
	tokenFile string
	log       *lockedBuffer
	mu        sync.Mutex
	clock     time.Time
}

func newMetadataRig(t *testing.T) *metadataRig {
	t.Helper()
	g := &metadataRig{tokenFile: filepath.Join(t.TempDir(), WorkloadIdentityTokenFile), log: &lockedBuffer{}, clock: time.Now()}
	writeSubjectToken(t, g.tokenFile, "fake-subject-token-1")
	g.sts = &fakeSTS{t: t, tokenFile: g.tokenFile, expiresIn: 3600}
	sts := httptest.NewServer(g.sts)
	t.Cleanup(sts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g.server = newMetadataServer(ctx, MetadataConfig{
		WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider, tokenFile: g.tokenFile, tokenURL: sts.URL,
	}, func(format string, args ...any) { _, _ = fmt.Fprintf(g.log, format+"\n", args...) })
	g.server.now = g.now
	endpoint := httptest.NewServer(g.server)
	t.Cleanup(endpoint.Close)
	g.url = endpoint.URL
	return g
}

// writeSubjectToken writes the pod's projected token as the kubelet does, a new one in its place.
func writeSubjectToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (g *metadataRig) now() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.clock
}

func (g *metadataRig) advance(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.clock = g.clock.Add(d)
}

func (g *metadataRig) get(t *testing.T, path string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	return getMetadata(t, g.url, path, headers)
}

// noRedirects is a client that hands back a redirect as it is answered (redirects.Refused), since
// a 301 is one of the endpoint's answers, which a test compares with GKE's. It keeps no connection
// between requests, so no idle connection of the client's outlives the server a test closes.
var noRedirects = redirects.Refused(&http.Client{Transport: &http.Transport{DisableKeepAlives: true}})

// getMetadata asks the endpoint at base for path with headers and answers the response and its
// body.
func getMetadata(t *testing.T, base, path string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := noRedirects.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return response, string(body)
}

// metadataToken is the endpoint's answer to a token request.
type metadataToken struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// token asks for the default account's token with query (`?scopes=…`, or "") and answers it.
func (g *metadataRig) token(t *testing.T, query string) metadataToken {
	t.Helper()
	response, body := g.get(t, tokenPath+query, flavor)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a token request%s answered %d %q", query, response.StatusCode, body)
	}
	var token metadataToken
	if err := json.Unmarshal([]byte(body), &token); err != nil {
		t.Fatalf("a token request%s answered no token: %v", query, err)
	}
	return token
}

var (
	accessTokenField = regexp.MustCompile(`"access_token":"[^"]*"`)
	expiresInField   = regexp.MustCompile(`"expires_in":[0-9]+`)
)

// redacted is body with any access token in it replaced, so a failure prints none.
func redacted(body string) string {
	return accessTokenField.ReplaceAllString(body, `"access_token":"<token>"`)
}

// The two content types GKE's server answers a value or a listing under: its own for the text
// form, and JSON's for the form `?alt=json` asks for.
const (
	textType = "application/text"
	jsonType = "application/json"
)

// entriesOf is the entries of a listing or a list value in the form contentType names: one a line
// in the text form, and the strings of one JSON array in the form `?alt=json` asks for.
func entriesOf(t *testing.T, contentType, body string) []string {
	t.Helper()
	if contentType != jsonType {
		return strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	}
	var entries []string
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("%q is no JSON array of entries: %v", body, err)
	}
	return entries
}

// inForm is entries in the form contentType names (entriesOf), the JSON array as GKE's server
// writes it: on one line, with no line break after it.
func inForm(t *testing.T, contentType string, entries []string) string {
	t.Helper()
	if contentType != jsonType {
		return strings.Join(entries, "\n") + "\n"
	}
	return oneLineJSON(t, entries)
}

// jsonForm is what GKE's server answers with `?alt=json` where it answers text without it, by the
// rule the recording shows on the paths it holds in both forms: a value, which ends in no line
// break, is a JSON string, and a list value or a listing, one entry a line, is a JSON array of
// the entries.
func jsonForm(t *testing.T, text string) string {
	t.Helper()
	if strings.HasSuffix(text, "\n") {
		return inForm(t, jsonType, entriesOf(t, textType, text))
	}
	return oneLineJSON(t, text)
}

func oneLineJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Every path the endpoint serves is answered as GKE's metadata server answered it from a Legion
// issue pod (testdata/gke-metadata.json) — status, Metadata-Flavor, Content-Type, Location and
// body — but for the token, which is the one the fake service handed out for the scopes the
// request named, and its remaining seconds. A directory GKE lists entries of that the endpoint
// does not serve is answered with exactly the entries it does serve, in GKE's order and form,
// one a line or a JSON array: each entry it lists answers, and each it leaves out is a path it
// does not serve. `?alt=json`, in any case of the letters, answers a value, a list value and a
// listing as JSON and any other `alt` as text, while the token and `?recursive=true` stay JSON:
// as recorded where the recording holds the path with that query, and on every other path it
// holds as text, as the JSON of that text (jsonForm). Refusals, the redirects and the unknown
// paths are GKE's own, `identity` among them.
func TestMetadataAnswersEveryPathAsGKEsServerAnswersIt(t *testing.T) {
	t.Parallel()
	g := newMetadataRig(t)
	unserved := map[string][]string{
		"instance-dir":          {"attributes/", "gke/", "hostname", "id", "name", "zone"},
		"instance-dir-alt-json": {"attributes/", "gke/", "hostname", "id", "name", "zone"},
		"project-dir":           {"numeric-project-id"},
		"project-dir-alt-json":  {"numeric-project-id"},
		"default-dir":           {"identity"},
		"default-dir-alt-json":  {"identity"},
		"email-dir":             {"identity"},
	}
	answers := recordedAnswers(t)
	for _, want := range answers {
		t.Run(want.Name, func(t *testing.T) {
			response, body := g.get(t, want.Path, want.RequestHeaders)
			if response.StatusCode != want.Status {
				t.Errorf("status %d, want %d", response.StatusCode, want.Status)
			}
			for _, name := range []string{"Metadata-Flavor", "Content-Type", "Location"} {
				if got := response.Header.Get(name); got != want.Headers[name] {
					t.Errorf("%s %q, want %q", name, got, want.Headers[name])
				}
			}
			wantBody := want.Body
			if strings.Contains(want.Body, `"access_token"`) {
				wantBody = servedToken(t, g, want, body)
			}
			dir, query, _ := strings.Cut(want.Path, "?")
			contentType := want.Headers["Content-Type"]
			if left, ok := unserved[want.Name]; ok {
				recorded := entriesOf(t, contentType, want.Body)
				for _, entry := range left {
					if !slices.Contains(recorded, entry) {
						t.Errorf("the recording lists no %s; the endpoint leaves it out for nothing", entry)
					}
					if response, _ := g.get(t, dir+entry, flavor); response.StatusCode != http.StatusNotFound {
						t.Errorf("%s, which the listing leaves out, answered %d, want 404", dir+entry, response.StatusCode)
					}
				}
				wantBody = inForm(t, contentType, slices.DeleteFunc(recorded, func(entry string) bool { return slices.Contains(left, entry) }))
			}
			if body != wantBody {
				t.Errorf("body %q, want %q", redacted(body), redacted(wantBody))
			}
			if want.Status != http.StatusOK {
				return
			}
			// A listing, in either form, names only entries that answer. `?recursive=true` answers
			// a directory's contents, which is no listing.
			if strings.HasSuffix(dir, "/") && !strings.Contains(query, "recursive=true") {
				for _, entry := range entriesOf(t, response.Header.Get("Content-Type"), body) {
					if response, _ := g.get(t, dir+entry, flavor); response.StatusCode != http.StatusOK && response.StatusCode != http.StatusMovedPermanently {
						t.Errorf("%s, which the listing names, answered %d", dir+entry, response.StatusCode)
					}
				}
			}
			// The JSON form of each path recorded as text, the paths recorded in one form among
			// them: the two directories above the API, most listings and the account's other name.
			if contentType == textType && query == "" {
				response, asJSON := g.get(t, want.Path+"?alt=json", want.RequestHeaders)
				if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != jsonType ||
					response.Header.Get("Metadata-Flavor") != "Google" || asJSON != jsonForm(t, body) {
					t.Errorf("with ?alt=json answered %d %v %q, want 200 %s %q, the JSON of its text",
						response.StatusCode, response.Header, asJSON, jsonType, jsonForm(t, body))
				}
			}
		})
	}
	// The universe's other spelling, which GKE lists beside the one recorded, in both forms.
	for _, want := range answers {
		if want.Name != "universe-domain" && want.Name != "universe-domain-alt-json" {
			continue
		}
		path := strings.Replace(want.Path, "universe-domain", "universe_domain", 1)
		response, body := g.get(t, path, flavor)
		if response.StatusCode != want.Status || response.Header.Get("Metadata-Flavor") != want.Headers["Metadata-Flavor"] ||
			response.Header.Get("Content-Type") != want.Headers["Content-Type"] || body != want.Body {
			t.Errorf("%s answered %d %v %q, want %s's %d %v %q",
				path, response.StatusCode, response.Header, body, want.Name, want.Status, want.Headers, want.Body)
		}
	}
}

// servedToken checks a token answer against the recorded one and answers the recorded body with
// the token the endpoint served in the placeholder's place and its remaining seconds in GKE's: the
// token is the one the fake service handed out for the scopes the request named, an hour's token
// read within a minute of its exchange.
func servedToken(t *testing.T, g *metadataRig, want recordedAnswer, body string) string {
	t.Helper()
	var served metadataToken
	if err := json.Unmarshal([]byte(body), &served); err != nil {
		t.Fatalf("no token answered: %v", err)
	}
	request, err := url.Parse(want.Path)
	if err != nil {
		t.Fatal(err)
	}
	scopes := strings.Split(request.Query().Get("scopes"), ",")
	if scopes[0] == "" {
		scopes = []string{cloudPlatformScope}
	}
	slices.Sort(scopes)
	if got, scope := g.sts.scopeOf(served.AccessToken), strings.Join(scopes, " "); got != scope {
		t.Errorf("the token served is not the one the service handed out for scope %q", scope)
	}
	if served.ExpiresIn < 3600-60 || served.ExpiresIn > 3600+60 {
		t.Errorf("expires_in %d, want an hour's token's seconds left", served.ExpiresIn)
	}
	wantBody := strings.Replace(want.Body, `"<token>"`, strconv.Quote(served.AccessToken), 1)
	return expiresInField.ReplaceAllString(wantBody, `"expires_in":`+strconv.Itoa(served.ExpiresIn))
}

// The endpoint gets its token as GKE's own server does: it exchanges the pod's projected token at
// the security token service for the pool's audience (the fake checks every exchange), reading
// the token file at each exchange, so a token the kubelet has rotated is the one sent next.
func TestMetadataExchangesWhatTheTokenFileHoldsAtEachExchange(t *testing.T) {
	t.Parallel()
	g := newMetadataRig(t)
	// A token with less than five minutes left is exchanged again at the next request.
	g.sts.setExpiresIn(299)
	g.token(t, "")
	writeSubjectToken(t, g.tokenFile, "fake-subject-token-2\n")
	g.token(t, "")
	exchanges := g.sts.answered()
	if len(exchanges) != 2 {
		t.Fatalf("%d exchanges, want 2", len(exchanges))
	}
	if exchanges[0].subject != "fake-subject-token-1" || exchanges[1].subject != "fake-subject-token-2" {
		t.Error("the exchanges did not send the token file's contents of their moment, the first and then the rotated one")
	}
	for i, exchange := range exchanges {
		if exchange.scope != cloudPlatformScope {
			t.Errorf("exchange %d asked for scope %q, want %q, the scope of a request that names none", i, exchange.scope, cloudPlatformScope)
		}
	}
}

// expires_in is the seconds the token handed out has left, read against the clock, so it falls as
// the token ages while the endpoint hands out the one it holds.
func TestMetadataExpiresInIsTheSecondsTheCachedTokenHasLeft(t *testing.T) {
	t.Parallel()
	g := newMetadataRig(t)
	first := g.token(t, "")
	g.advance(10 * time.Minute)
	later := g.token(t, "")
	if later.AccessToken != first.AccessToken {
		t.Error("the endpoint exchanged again for a token with fifty minutes left")
	}
	if first.ExpiresIn < 3600-60 || first.ExpiresIn > 3600+60 {
		t.Errorf("expires_in %d at the exchange, want the hour the service gave", first.ExpiresIn)
	}
	if later.ExpiresIn != first.ExpiresIn-600 {
		t.Errorf("expires_in %d ten minutes after %d, want %d", later.ExpiresIn, first.ExpiresIn, first.ExpiresIn-600)
	}
	if first.TokenType != "Bearer" {
		t.Errorf("token_type %q, want Bearer", first.TokenType)
	}
	if n := len(g.sts.answered()); n != 1 {
		t.Errorf("%d exchanges, want 1", n)
	}
}

// A token is handed out again until its last five minutes and exchanged again within them, so no
// client is handed one that runs out while it uses it: one with six minutes left is handed out
// again, one with less than five is not. The oauth2 library's own margin, ten seconds, would hand
// out the latter.
func TestMetadataRefreshesATokenBeforeItsLastFiveMinutes(t *testing.T) {
	t.Parallel()
	g := newMetadataRig(t)
	g.sts.setExpiresIn(360)
	first := g.token(t, "")
	if again := g.token(t, ""); again.AccessToken != first.AccessToken {
		t.Error("a token with six minutes left was exchanged again")
	}
	g.sts.setExpiresIn(299)
	short := g.token(t, "?scopes="+userinfoEmailScope)
	g.sts.setExpiresIn(3600)
	refreshed := g.token(t, "?scopes="+userinfoEmailScope)
	if refreshed.AccessToken == short.AccessToken {
		t.Error("a token with less than five minutes left was handed out again")
	}
	if kept := g.token(t, "?scopes="+userinfoEmailScope); kept.AccessToken != refreshed.AccessToken {
		t.Error("the refreshed token, an hour's, was exchanged again")
	}
	if n := len(g.sts.answered()); n != 3 {
		t.Errorf("%d exchanges, want 3", n)
	}
}

// `?scopes=` is the scope the service is asked for, one token source per scope set whatever the
// order the request names them in, and cloud-platform for a request that names none.
func TestMetadataAsksTheServiceForTheScopesARequestNames(t *testing.T) {
	t.Parallel()
	g := newMetadataRig(t)
	const storage = "https://www.googleapis.com/auth/devstorage.read_only"
	two := g.token(t, "?scopes="+userinfoEmailScope+","+cloudPlatformScope)
	if again := g.token(t, "?scopes="+cloudPlatformScope+","+userinfoEmailScope); again.AccessToken != two.AccessToken {
		t.Error("the same two scopes named in the other order were exchanged again")
	}
	g.token(t, "?scopes="+storage)
	g.token(t, "")
	var scopes []string
	for _, exchange := range g.sts.answered() {
		scopes = append(scopes, exchange.scope)
	}
	if want := []string{cloudPlatformScope + " " + userinfoEmailScope, storage, cloudPlatformScope}; !slices.Equal(scopes, want) {
		t.Errorf("the service was asked for %q, want %q", scopes, want)
	}
}

// At most sixteen token sources are kept, one per scope set, so a client naming ever new scope
// sets cannot grow the launcher's memory.
func TestMetadataKeepsAtMostSixteenTokenSources(t *testing.T) {
	t.Parallel()
	g := newMetadataRig(t)
	for i := range 20 {
		g.token(t, "?scopes=https://www.googleapis.com/auth/example-"+strconv.Itoa(i))
	}
	g.server.mu.Lock()
	kept := len(g.server.sources)
	g.server.mu.Unlock()
	if kept != 16 {
		t.Errorf("%d token sources kept, want 16", kept)
	}
	if n := len(g.sts.answered()); n != 20 {
		t.Errorf("%d exchanges, want 20", n)
	}
}

// A token request without `Metadata-Flavor: Google`, or one with X-Forwarded-For, is refused as
// GKE's server refuses it and before any exchange, so a request a proxy forwards gets no token.
func TestMetadataRefusesARequestWithoutTheFlavorOrForwardedBeforeAnyExchange(t *testing.T) {
	t.Parallel()
	g := newMetadataRig(t)
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"without the flavor", nil, "Missing required header \"Metadata-Flavor\": \"Google\"\n"},
		{"forwarded", map[string]string{"Metadata-Flavor": "Google", "X-Forwarded-For": "10.0.0.1"}, "Request cannot contain header: X-Forwarded-For\n"},
	} {
		response, body := g.get(t, tokenPath, tc.headers)
		if response.StatusCode != http.StatusForbidden || body != tc.want || response.Header.Get("Metadata-Flavor") != "" ||
			response.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("a token request %s answered %d %v %q, want 403 %q", tc.name, response.StatusCode, response.Header, redacted(body), tc.want)
		}
	}
	if n := len(g.sts.answered()); n != 0 {
		t.Errorf("%d exchanges for refused requests, want none", n)
	}
}

// A failed exchange answers 500 with a short text that names no token, and is logged once a
// minute at most, whatever the number of requests that fail: the line names neither the subject
// token nor any access token, and quotes the service's answer cut to 256 bytes at a character's
// start, so one long answer cannot flood the log.
func TestMetadataAnswersAFailedExchangeWith500AndLogsItOnceAMinute(t *testing.T) {
	t.Parallel()
	g := newMetadataRig(t)
	g.sts.refuse()
	failed := func() {
		t.Helper()
		if response, body := g.get(t, tokenPath, flavor); response.StatusCode != http.StatusInternalServerError || body != "token exchange failed\n" {
			t.Errorf("a failed exchange answered %d %q, want 500 %q", response.StatusCode, redacted(body), "token exchange failed\n")
		}
	}
	const line = "launcher: metadata: token exchange failed "
	failed()
	failed()
	if got := strings.Count(g.log.String(), line); got != 1 {
		t.Errorf("%d lines for two failures within a minute, want 1", got)
	}
	prefix := line + `scopes="` + cloudPlatformScope + `" reason=`
	_, quoted, found := strings.Cut(strings.TrimSuffix(g.log.String(), "\n"), prefix)
	if !found {
		t.Fatal("the failure line names no scope and reason")
	}
	reason, err := strconv.Unquote(quoted)
	switch {
	case err != nil:
		t.Errorf("the reason is not one quoted string: %v", err)
	case len(reason) > maxFailureReason || len(reason) < maxFailureReason-1 || !utf8.ValidString(reason):
		t.Errorf("the reason is %d bytes (valid UTF-8: %v), want the service's answer cut to %d bytes at a character's start",
			len(reason), utf8.ValidString(reason), maxFailureReason)
	case !strings.Contains(reason, "status code 400") || !strings.HasSuffix(reason, "é"):
		t.Errorf("the reason %q is not the start of the service's answer", reason)
	}
	if n := g.sts.refusals(); n != 2 {
		t.Errorf("%d exchanges tried, want one per request", n)
	}
	g.advance(exchangeFailureLogInterval)
	failed()
	if got := strings.Count(g.log.String(), line); got != 2 {
		t.Errorf("%d lines once a minute has passed, want 2", got)
	}
	if strings.Contains(g.log.String(), "fake-subject-token") || strings.Contains(g.log.String(), "fake-access-token") {
		t.Error("the log names a token")
	}
}

// Each path the endpoint does not serve is answered as GKE answers an unknown path and logged once,
// so the container's log names every path a tool asked for in vain; the paths remembered are
// bounded, so a client cannot grow the launcher's memory with ever new ones.
func TestMetadataLogsEachPathItDoesNotServeOnce(t *testing.T) {
	t.Parallel()
	g := newMetadataRig(t)
	for _, tc := range []struct{ path, logged string }{
		{metadataPrefix + "instance/service-accounts/default/identity?audience=https://example.com", metadataPrefix + "instance/service-accounts/default/identity"},
		{metadataPrefix + "instance/zone", metadataPrefix + "instance/zone"},
		{metadataPrefix + "project/numeric-project-id", metadataPrefix + "project/numeric-project-id"},
		{metadataPrefix + "instance/?recursive=true", metadataPrefix + "instance/?recursive=true"},
		{"/no-such-path", "/no-such-path"},
	} {
		for range 2 {
			if response, body := g.get(t, tc.path, flavor); response.StatusCode != http.StatusNotFound || body != "Not Found\n" ||
				response.Header.Get("Content-Type") != "text/plain; charset=utf-8" || response.Header.Get("Metadata-Flavor") != "" {
				t.Errorf("%s answered %d %v %q, want GKE's 404", tc.path, response.StatusCode, response.Header, body)
			}
		}
		if got := strings.Count(g.log.String(), "launcher: metadata: not served path="+tc.logged+"\n"); got != 1 {
			t.Errorf("%s logged %d times, want once: %s", tc.logged, got, g.log.String())
		}
	}
	for i := range maxNotServedPaths {
		g.get(t, metadataPrefix+"instance/no-such-path-"+strconv.Itoa(i), flavor)
	}
	g.server.mu.Lock()
	remembered := len(g.server.notServed)
	g.server.mu.Unlock()
	if remembered != maxNotServedPaths {
		t.Errorf("%d paths remembered, want %d", remembered, maxNotServedPaths)
	}
	if got := strings.Count(g.log.String(), "launcher: metadata: not served path="); got != maxNotServedPaths {
		t.Errorf("%d not-served lines, want %d", got, maxNotServedPaths)
	}
}

// The endpoint is a microVM pod's container's alone, in either of the launcher's forms, and named
// whole: a gVisor pod reaches the node's metadata server, which the endpoint would shadow, and the
// pool and the identity provider together make the exchange's audience. A microVM issue pod's
// roles container and a microVM controller pod's single role each serve it; either form without
// --microvm, or with the endpoint named in part, is refused.
func TestMetadataIsRefusedOutsideAMicroVMPodOrNamedInPart(t *testing.T) {
	t.Parallel()
	issuePod := func() Config {
		return Config{
			Connect: "tcp://127.0.0.1:1", Sandbox: "legion-legion-legion-208", PodUID: "pod-1", RolesDir: "/roles", Roles: []string{"tester"},
			MicroVM: true, Metadata: &MetadataConfig{WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider},
		}
	}
	controllerPod := func() Config {
		return Config{
			Connect: "tcp://127.0.0.1:1", Sandbox: "legion-legion-controller", PodUID: "pod-1", Token: "launcher-token", Role: "controller", PrivateDir: "/private",
			MicroVM: true, Metadata: &MetadataConfig{WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider},
		}
	}
	if err := issuePod().validate(); err != nil {
		t.Fatalf("a microVM issue pod's endpoint was refused: %v", err)
	}
	if err := controllerPod().validate(); err != nil {
		t.Fatalf("a microVM controller pod's endpoint was refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		pod    func() Config
		change func(*Config)
		want   string
	}{
		{"the pool alone", issuePod, func(c *Config) { c.Metadata.IdentityProvider = "" }, "launcher: the metadata endpoint is named in part: no --metadata-identity-provider"},
		{"the identity provider alone", issuePod, func(c *Config) { c.Metadata.WorkloadPool = "" }, "launcher: the metadata endpoint is named in part: no --metadata-workload-pool"},
		{"a gVisor pod", issuePod, func(c *Config) { c.MicroVM = false }, "launcher: the metadata endpoint serves a microVM's pod: it needs --microvm"},
		{"the controller's pool alone", controllerPod, func(c *Config) { c.Metadata.IdentityProvider = "" }, "launcher: the metadata endpoint is named in part: no --metadata-identity-provider"},
		{"a gVisor controller pod", controllerPod, func(c *Config) { c.MicroVM = false }, "launcher: the metadata endpoint serves a microVM's pod: it needs --microvm"},
	} {
		cfg := tc.pod()
		tc.change(&cfg)
		if err := cfg.validate(); err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// metadataEnv names the endpoint's address over whatever a generation's environment carried where
// its launcher serves it, and leaves that environment exactly as it is where it does not.
func TestMetadataEnvNamesTheAddressOnlyWhereItsLauncherServes(t *testing.T) {
	t.Parallel()
	const address = "127.0.0.1:40001"
	carried := []string{"PATH=/bin", MetadataHostVariable + "=169.254.169.254", MetadataIPVariable + "=169.254.169.254"}
	served := metadataEnv(slices.Clone(carried), address)
	for _, name := range []string{MetadataHostVariable, MetadataIPVariable} {
		if got := envValue(served, name); got != address {
			t.Errorf("served: %s=%q, want %q", name, got, address)
		}
	}
	if got := envValue(served, "PATH"); got != "/bin" {
		t.Errorf("served: PATH=%q, want the carried /bin", got)
	}
	for _, env := range [][]string{carried, {"PATH=/bin"}} {
		if got := metadataEnv(slices.Clone(env), ""); !slices.Equal(got, env) {
			t.Errorf("not served: %q, want %q as it was", got, env)
		}
	}
}

// A pod's endpoint listens at MetadataAddress, the one its launcher's configuration names when it
// names none; only a test names an address of its own, so no test holds the pod's port.
func TestMetadataListensAtTheConstantAddressUnlessATestNamesItsOwn(t *testing.T) {
	t.Parallel()
	if got := (MetadataConfig{WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider}).listenAddress(); got != MetadataAddress {
		t.Errorf("a pod's endpoint listens at %q, want %q", got, MetadataAddress)
	}
	if got := (MetadataConfig{address: "127.0.0.1:0"}).listenAddress(); got != "127.0.0.1:0" {
		t.Errorf("a test's endpoint listens at %q, want the address it named", got)
	}
}

// A pod's endpoint, whose configuration names no bound, gives a client ten seconds to send a
// request, its headers and any body, and a connection the client keeps a minute to send its next
// one. Only a test names bounds of its own, short enough to wait out (the two tests below).
func TestMetadataGivesARequestTenSecondsAndAKeptConnectionAMinute(t *testing.T) {
	t.Parallel()
	server := (MetadataConfig{WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider}).httpServer(nil)
	if server.ReadHeaderTimeout != 10*time.Second || server.ReadTimeout != 10*time.Second || server.IdleTimeout != time.Minute {
		t.Errorf("a pod's endpoint gives a request's headers %s, the request %s and a kept connection %s, want 10s, 10s and 1m0s",
			server.ReadHeaderTimeout, server.ReadTimeout, server.IdleTimeout)
	}
}

// closeWait is how long a test waits for the endpoint to close a connection: far longer than the
// bound the test named, and less than either of a pod's own, so an endpoint that kept a pod's
// bound, or none, fails the test.
const closeWait = 5 * time.Second

// serveBoundedMetadata serves the endpoint as a pod's launcher does (serveMetadata), on a port of
// its own and with the bounds named, 0 being a pod's own, until the test ends, and answers its
// address.
func serveBoundedMetadata(t *testing.T, readTimeout, idleTimeout time.Duration) string {
	t.Helper()
	holdMetadataVariables(t, "")
	log := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	stopped, address := serveMetadata(ctx, Config{RolesDir: t.TempDir(), Metadata: &MetadataConfig{
		WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider,
		address: "127.0.0.1:0", readTimeout: readTimeout, idleTimeout: idleTimeout,
	}}, func(format string, args ...any) { _, _ = fmt.Fprintf(log, format+"\n", args...) })
	if address == "" {
		cancel()
		t.Fatalf("the endpoint did not listen: %s", log.String())
	}
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return address
}

// sendMetadata opens a connection to the endpoint at address, which the test's end closes and
// which waits closeWait at most for what it reads, and sends it sent.
func sendMetadata(t *testing.T, address, sent string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(closeWait)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, sent); err != nil {
		t.Fatal(err)
	}
	return conn
}

// The endpoint gives a client its bound to send a request and no longer, so a process in the pod
// holds no connection of the launcher's open by sending nothing more: a connection that sent part
// of a request line, or a line and part of its headers, is closed with nothing served (Go's
// server answers the first as a bad request and the second not at all), and one whose request
// names a body it never sends is answered, since the endpoint reads no body, and closed.
func TestMetadataClosesAConnectionWhoseRequestDoesNotArriveInTime(t *testing.T) {
	// Not parallel: it unsets environment variables of the whole process (serveBoundedMetadata).
	address := serveBoundedMetadata(t, 200*time.Millisecond, 0)
	const projectID = metadataPrefix + "project/project-id"
	for _, tc := range []struct{ name, sent, served string }{
		{"part of a request line", "GET " + projectID, ""},
		{"part of the headers", "GET " + projectID + " HTTP/1.1\r\nHost: metadata\r\nMetadata-Flavor: Google\r\n", ""},
		{
			"a body named and never sent",
			"POST " + projectID + " HTTP/1.1\r\nHost: metadata\r\nMetadata-Flavor: Google\r\nContent-Length: 8\r\n\r\n",
			"\r\n\r\nexample-project",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answered, err := io.ReadAll(sendMetadata(t, address, tc.sent))
			if err != nil {
				t.Fatalf("the endpoint did not close the connection within %s: %v", closeWait, err)
			}
			got := string(answered)
			if ok := strings.HasPrefix(got, "HTTP/1.1 200 OK\r\n"); ok != (tc.served != "") || !strings.HasSuffix(got, tc.served) {
				t.Errorf("the connection was answered %q before it closed, want a 200 only for a whole request, ending %q", got, tc.served)
			}
		})
	}
}

// A connection a client keeps after its answer is closed once it has sent no next request for the
// endpoint's idle bound, which is not the bound of a request: that one is a pod's own ten seconds
// here, longer than the test waits.
func TestMetadataClosesAKeptConnectionThatSendsNoNextRequest(t *testing.T) {
	// Not parallel: it unsets environment variables of the whole process (serveBoundedMetadata).
	address := serveBoundedMetadata(t, 0, 200*time.Millisecond)
	conn := sendMetadata(t, address, "GET "+metadataPrefix+"project/project-id HTTP/1.1\r\nHost: metadata\r\nMetadata-Flavor: Google\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("the request was not answered: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "example-project" || response.Close {
		t.Fatalf("the request was answered %q (%v, closing: %v), want example-project on a connection the endpoint keeps", body, err, response.Close)
	}
	if rest, err := io.ReadAll(reader); err != nil || len(rest) != 0 {
		t.Errorf("the endpoint did not close the kept connection within %s: it sent %q, and the read ended %v", closeWait, rest, err)
	}
}

// holdMetadataVariables puts both of the endpoint's variables in the process's environment at
// carried, or out of it where carried is "", for the rest of the test: a launcher that serves the
// endpoint names its address in its own environment, the test process's, and t.Setenv puts it
// back as the test found it.
func holdMetadataVariables(t *testing.T, carried string) {
	t.Helper()
	if carried == "" {
		fixtureenv.Unset(t, MetadataHostVariable, MetadataIPVariable)
		return
	}
	for _, name := range []string{MetadataHostVariable, MetadataIPVariable} {
		t.Setenv(name, carried)
	}
}
