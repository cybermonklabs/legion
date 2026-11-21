// Package ciphertokentest is the stands-in a test of internal/ciphertoken needs. Two are the
// broker-token check's: the secrets broker's introspection route, answering what the test says
// each token is, and a caller holding one key, whose proofs come from the producer a real caller
// uses (internal/cipher/helper's DPoP transport) rather than from a second signer written beside
// the test. Two are a GoogleAccount's: a Compute Engine metadata server, and IAM Service Account
// Credentials' generateIdToken. It also reads the golden file of the answers to a token narrowed
// to reading (NarrowedAnswers), which the broker's test and every reader's test hold to. It is an
// ordinary package rather than a _test.go file so the suites that drive each — internal/ciphertoken's
// own and internal/dispatch/api's for the check, internal/ciphertoken's own and
// internal/cipher/judge's for the account, and every reader of a narrowed token for the golden
// file — read one copy; Go cannot share test-only code across packages.
package ciphertokentest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/googleidentity"
)

// StubBroker is the secrets broker's introspection route: it answers what the test has said each
// token is, counts the introspections so a test can prove how many reached it, and keeps every
// Authorization it was presented so a test can prove Dispatch authenticated as itself.
type StubBroker struct {
	// Server is the stub's own server. Its URL is the broker URL a verifier is built on, and a
	// test closes it to prove what a broker that answers nothing is answered with.
	Server *httptest.Server

	mu      sync.Mutex
	answers map[string]ciphertoken.Introspection
	raw     map[string]string
	bearers []string

	introspections atomic.Int64
}

// NewStubBroker serves the stub until the test ends.
func NewStubBroker(t testing.TB) *StubBroker {
	t.Helper()
	broker := &StubBroker{answers: map[string]ciphertoken.Introspection{}, raw: map[string]string{}}
	broker.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != ciphertoken.IntrospectPath {
			http.Error(w, "not the introspection route", http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "unreadable form", http.StatusBadRequest)
			return
		}
		broker.introspections.Add(1)
		broker.mu.Lock()
		broker.bearers = append(broker.bearers, r.Header.Get("Authorization"))
		answer := broker.answers[r.PostForm.Get("token")]
		raw, isRaw := broker.raw[r.PostForm.Get("token")]
		broker.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if isRaw {
			_, _ = w.Write([]byte(raw))
			return
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(broker.Server.Close)
	return broker
}

// Says makes the broker answer answer for token. A token nothing says anything about is answered
// not live, as a token the broker never issued is.
func (b *StubBroker) Says(token string, answer ciphertoken.Introspection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answers[token] = answer
}

// SaysRaw makes the broker answer body, verbatim, for token: an answer this release's broker
// would never serialise, such as one an older broker wrote.
func (b *StubBroker) SaysRaw(token, body string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.raw[token] = body
}

// Count is how many introspections have reached the broker.
func (b *StubBroker) Count() int64 { return b.introspections.Load() }

// Bearers is every Authorization the broker was presented, in the order it was, as a copy: what
// a test reads to prove the caller authenticated as itself.
func (b *StubBroker) Bearers() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.bearers)
}

// capturedRequest keeps the request the helper's DPoP transport signed, instead of sending it.
type capturedRequest struct {
	mu   sync.Mutex
	last *http.Request
}

func (c *capturedRequest) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.last = r
	c.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

// Caller is one caller holding one key, producing the proofs its real client produces.
type Caller struct {
	client   *http.Client
	captured *capturedRequest
	jkt      string
}

// NewCaller mints a caller's key and the transport that proves it.
func NewCaller(t testing.TB) *Caller {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatalf("mint the caller's key: %v", err)
	}
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint the caller's key: %v", err)
	}
	captured := &capturedRequest{}
	return &Caller{client: &http.Client{Transport: cipherclient.NewDPoPTransport(key, captured)}, captured: captured, jkt: jkt}
}

// JKT is the thumbprint of the caller's key: what the broker binds a token to, and what a proof
// of this key is checked against.
func (c *Caller) JKT() string { return c.jkt }

// Signed is the request this caller's client signed for method and url, presenting token under
// the DPoP scheme: a test sends it, or reads its Authorization and DPoP headers onto a request of
// its own. Each call signs a fresh proof, as a real client does, and nothing leaves the process —
// the transport keeps the request rather than making it.
func (c *Caller) Signed(t testing.TB, method, url, token string) *http.Request {
	t.Helper()
	signing, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build the request to sign: %v", err)
	}
	signing.Header.Set("Authorization", "DPoP "+token)
	response, err := c.client.Do(signing)
	if err != nil {
		t.Fatalf("sign the request: %v", err)
	}
	response.Body.Close()
	c.captured.mu.Lock()
	defer c.captured.mu.Unlock()
	return c.captured.last
}

// narrowedAnswersFile is internal/ciphertoken/testdata/narrowed-answers.json, read once for the
// test binary. It is found from this file's own path rather than from the process's working
// directory, so the test of every package reads the one file, wherever its binary runs.
var narrowedAnswersFile = sync.OnceValues(func() ([]byte, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return nil, errors.New("this test binary carries no path for brokertokentest.go")
	}
	return os.ReadFile(filepath.Join(filepath.Dir(file), "..", "testdata", "narrowed-answers.json"))
})

// NarrowedAnswers is the broker's answer for a token narrowed to reading, by the kind of trade
// that minted it, as the golden file internal/ciphertoken/testdata/narrowed-answers.json holds
// them: each bound to jkt, naming session and expiring at exp, filled in as the file's comment
// says. The broker's own test holds the answers it mints to the same file
// (internal/cipher/oauth, TestATradeThatAsksForReadKeepsItsIdentityAndHoldsRead). A member the
// file names and Introspection does not carry fails the read, so the file holds nothing a reader
// never reads.
func NarrowedAnswers(t testing.TB, jkt, session string, exp int64) map[string]ciphertoken.Introspection {
	t.Helper()
	raw, err := narrowedAnswersFile()
	if err != nil {
		t.Fatalf("read the narrowed answers: %v", err)
	}
	filled := strings.NewReplacer(`"${jkt}"`, strconv.Quote(jkt), `"${session}"`, strconv.Quote(session),
		`"${exp}"`, strconv.FormatInt(exp, 10)).Replace(string(raw))
	var golden struct {
		Comment string                               `json:"_comment"`
		Answers map[string]ciphertoken.Introspection `json:"answers"`
	}
	decoder := json.NewDecoder(strings.NewReader(filled))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&golden); err != nil {
		t.Fatalf("decode the narrowed answers: %v", err)
	}
	if len(golden.Answers) == 0 {
		t.Fatal("the narrowed answers file holds no answer")
	}
	return golden.Answers
}

// The two paths of a Compute Engine metadata server a GoogleAccount reads, as
// internal/googleidentity asks them: the identity token of the machine's own service account, and
// that account's OAuth access token, which a mint at generateIdToken spends.
const (
	MetadataIdentityPath = "/computeMetadata/v1/instance/service-accounts/default/identity"
	MetadataTokenPath    = "/computeMetadata/v1/instance/service-accounts/default/token"
)

// MetadataIdentityToken is what a MetadataStand answers on the identity path, MetadataAccessBearer
// the access token it answers on the token path, and IAMIdentityToken what an IAMStand answers a
// mint with until a test says otherwise. Each is its own, so a test reads off a bearer which route
// produced it, and none is a real credential.
const (
	MetadataIdentityToken = "metadata-identity-token"
	MetadataAccessBearer  = "metadata-access-token"
	IAMIdentityToken      = "iam-identity-token"
)

// MetadataStand is a Compute Engine metadata server at the host GCE_METADATA_HOST names. It answers
// the identity path with MetadataIdentityToken and the token path with MetadataAccessBearer, good
// for an hour, and keeps the path of every request it got, so a test can prove which of the two a
// reader asked. Every answer carries Metadata-Flavor: Google, without which
// internal/googleidentity's access-token read takes what answered for no metadata server at all.
type MetadataStand struct {
	// Server is the stand's own server, whose host GCE_METADATA_HOST names.
	Server *httptest.Server

	mu    sync.Mutex
	paths []string
}

// NewMetadataStand serves the stand until the test ends and points GCE_METADATA_HOST at it for the
// test's length (t.Setenv), so a test that calls it never calls t.Parallel.
func NewMetadataStand(t testing.TB) *MetadataStand {
	t.Helper()
	stand := &MetadataStand{}
	stand.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stand.mu.Lock()
		stand.paths = append(stand.paths, r.URL.Path)
		stand.mu.Unlock()
		w.Header().Set("Metadata-Flavor", "Google")
		switch r.URL.Path {
		case MetadataIdentityPath:
			_, _ = io.WriteString(w, MetadataIdentityToken)
		case MetadataTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": MetadataAccessBearer, "expires_in": 3600, "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stand.Server.Close)
	t.Setenv(googleidentity.MetadataHostVar, strings.TrimPrefix(stand.Server.URL, "http://"))
	return stand
}

// Paths is the path of every request the stand got, in the order it got them, as a copy.
func (s *MetadataStand) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.paths)
}

// Asked is how many requests the stand got for path.
func (s *MetadataStand) Asked(path string) int {
	asked := 0
	for _, got := range s.Paths() {
		if got == path {
			asked++
		}
	}
	return asked
}

// IAMRequest is one request an IAMStand got: its method, its path, its Authorization header, and
// its body as JSON decodes it, nil for a body that is no JSON object.
type IAMRequest struct {
	Method, Path, Authorization string
	Body                        map[string]any
}

// IAMStand is IAM Service Account Credentials' generateIdToken. It answers every request with what
// the test last set — a token, IAMIdentityToken until a test sets another, as Google answers a
// mint; a status with a body, as Google answers a refusal; or a redirect — holds that answer back
// while the test says to, and keeps every request it got.
type IAMStand struct {
	// Server is the stand's own server, whose URL is the origin a GoogleAccount's
	// IAMCredentialsURL names.
	Server *httptest.Server

	mu       sync.Mutex
	requests []IAMRequest
	status   int
	body     string
	location string
	// held, when it is not nil, holds each answer back until it is closed, holdFor has passed
	// or the caller has gone, whichever comes first.
	held    chan struct{}
	holdFor time.Duration
}

// NewIAMStand serves the stand until the test ends, answering IAMIdentityToken.
func NewIAMStand(t testing.TB) *IAMStand {
	t.Helper()
	stand := &IAMStand{}
	stand.AnswersToken(IAMIdentityToken)
	stand.Server = httptest.NewServer(http.HandlerFunc(stand.serve))
	t.Cleanup(stand.Server.Close)
	return stand
}

func (s *IAMStand) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		body = nil
	}
	s.mu.Lock()
	s.requests = append(s.requests, IAMRequest{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body})
	status, answer, location, held, holdFor := s.status, s.body, s.location, s.held, s.holdFor
	s.mu.Unlock()
	if held != nil {
		select {
		case <-held:
		case <-time.After(holdFor):
		case <-r.Context().Done():
			return
		}
	}
	if location != "" {
		http.Redirect(w, r, location, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, answer)
}

// AnswersToken makes every later answer a 200 carrying token, as generateIdToken answers a mint.
func (s *IAMStand) AnswersToken(token string) {
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		panic(err)
	}
	s.answer(http.StatusOK, string(body), "")
}

// AnswersStatus makes every later answer status with body, as Google answers a refusal.
func (s *IAMStand) AnswersStatus(status int, body string) { s.answer(status, body, "") }

// Redirects makes every later answer a 307 to location.
func (s *IAMStand) Redirects(location string) {
	s.answer(http.StatusTemporaryRedirect, "", location)
}

func (s *IAMStand) answer(status int, body, location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body, s.location = status, body, location
}

// Hold holds every later answer back for d, until the release it returns is called, or until the
// caller hangs up, whichever comes first: how a test has readers queue behind one mint, or spends
// a reader's bound. A held answer ends with its caller, so it never keeps the server from closing
// once the caller has gone.
func (s *IAMStand) Hold(d time.Duration) (release func()) {
	held := make(chan struct{})
	s.mu.Lock()
	s.held, s.holdFor = held, d
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(held) }) }
}

// Requests is every request the stand got, in the order it got them, as a copy.
func (s *IAMStand) Requests() []IAMRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}
