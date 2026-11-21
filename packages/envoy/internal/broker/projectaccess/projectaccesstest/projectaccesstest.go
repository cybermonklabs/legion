// Package projectaccesstest is a stand-in for Dispatch's two project-access routes, the ones the
// broker's projectaccess client asks: whether a person may see a project, and what kind of member
// a person is and whether Dispatch holds them. It answers from rows a test gives it, holds every
// caller to the credential Dispatch holds the broker to — a read token whose service is
// AccessService, presented under the DPoP scheme with a proof of the key it is bound to, which it
// checks as Dispatch does (brokertoken.Verifier, against its canonical URL) — counts the calls it
// answers, keeps what each presented, and can be made to fail, so a test can show what the broker
// does when Dispatch cannot be reached. Exchange is a stand-in for the broker's own token endpoint,
// whose tokens the stand-in takes. It is an ordinary package rather than a _test.go file so every
// broker package's tests, and the real binary's, can serve it.
//
// It spells the wire on its own, field by field, rather than encoding the client's own type: a
// client that read a field under a name Dispatch does not send would then disagree with this
// stand-in, where one declaration shared by both would agree with itself and prove nothing.
package projectaccesstest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"authelia.com/provider/oauth2/storage"
	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// The paths of Dispatch's two project-access routes, as projectaccess asks them.
const (
	projectPath = "/api/v1/access/projects/"
	personPath  = "/api/v1/access/person"
)

// AccessService is the one service the stand-in admits, as Dispatch admits the services its
// DISPATCH_ACCESS_ACCOUNTS names: the broker's own Kubernetes account, which its read entry names
// as the read token's service.
const AccessService = "system:serviceaccount:dispatch:envoy-broker"

// AccessSession is the session the read token of a stand-in's own names, as a read entry fixes one.
const AccessSession = "broker-access"

// Row is one project as Dispatch holds it: whether it is public or private, its owner, and the
// people its access list names.
type Row struct {
	// Visibility is "public" or "private".
	Visibility string
	// Owner is the owner's lowercase login, who sees the project whatever else is true.
	Owner string
	// Listed are the lowercase logins the project's access list names.
	Listed []string
}

// Server is the stand-in: URL is the address it is reached at, Origin the canonical URL every
// proof must name, Token the read token it holds live for its own key, and Account that token as
// the credential the broker's client presents.
type Server struct {
	// URL is the stand-in's base URL: what BROKER_PROJECT_ACCESS_URL names, or, for a stand-in
	// NewServerAt built, what BROKER_PROJECT_ACCESS_DIAL_URL names.
	URL string
	// Origin is the canonical URL a proof is checked against, as DISPATCH_SERVER_URL is Dispatch's:
	// URL itself, or the name NewServerAt was given.
	Origin string
	// Token is the read token the stand-in holds live, bound to Account's key and naming
	// AccessService. Every other token is refused unless its introspection says it is live
	// (Exchange's, or a real broker's after IntrospectAt).
	Token string
	// Account is Token with its key, as the broker's projectaccess client presents a credential.
	Account *Account

	rows   map[string]Row
	people map[string]string
	// broker answers the introspections of the stand-in's own verifier: Token, and every token
	// an Exchange of this stand-in minted.
	broker   *brokertokentest.StubBroker
	verifier atomic.Pointer[brokertoken.Verifier]

	mu            sync.Mutex
	calls         int
	down          bool
	held          map[string]bool
	presentations []Presentation
	// until is the not_held_until the person route answers each person with while it answers them
	// not held, as an IAP deployment's Dispatch does; a person with none is answered with none.
	until map[string]time.Time
	// asOf is the as_of of the person route's last answer: each answer's is the wall clock at
	// Postgres's microsecond, moved past the last one's when the clock has not moved, as the times
	// of Dispatch's reads only move forward.
	asOf time.Time
}

// Presentation is what one request to the stand-in presented: its Authorization header and its
// DPoP proof, as they arrived.
type Presentation struct {
	Authorization string
	Proof         string
}

// Account is a read token as a test hands it to projectaccess.New: one token, bound to one key,
// that never rotates.
type Account struct {
	token string
	key   *ecdsa.PrivateKey
}

// StaticAccount is token bound to a key of its own, for a stand-in that checks no credential: the
// stand-in NewServer builds refuses it, since it holds token live for no key.
func StaticAccount(token string) *Account {
	return &Account{token: token, key: newKey()}
}

// Token answers the token.
func (a *Account) Token(context.Context) (string, error) { return a.token, nil }

// Key answers the key the token is bound to.
func (a *Account) Key() *ecdsa.PrivateKey { return a.key }

// newKey is a P-256 key, as every broker process makes its own.
func newKey() *ecdsa.PrivateKey {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(fmt.Sprintf("projectaccesstest: make a key: %v", err))
	}
	return key
}

// NewServer serves rows (by project key) and people (lowercase login to member kind: "full" or
// "restricted"; a login it does not hold is "none") until t ends, at its own URL, which is its
// canonical one. The maps are copied, so a test that goes on changing its own changes nothing
// here.
func NewServer(t testing.TB, rows map[string]Row, people map[string]string) *Server {
	t.Helper()
	return NewServerAt(t, "", rows, people)
}

// NewServerAt is NewServer reached at an address of its own but checking every proof against
// origin, as a Dispatch whose DISPATCH_SERVER_URL is origin is checked when the broker reaches it
// at an internal address. An empty origin is the stand-in's own URL.
func NewServerAt(t testing.TB, origin string, rows map[string]Row, people map[string]string) *Server {
	t.Helper()
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatalf("mint the stand-in's token: %v", err)
	}
	s := &Server{Token: "dispatch-access-" + hex.EncodeToString(token[:]), rows: map[string]Row{}, people: map[string]string{}, held: map[string]bool{}, until: map[string]time.Time{}}
	for key, row := range rows {
		row.Listed = slices.Clone(row.Listed)
		s.rows[key] = row
	}
	for login, kind := range people {
		s.people[login] = kind
	}
	s.Account = &Account{token: s.Token, key: newKey()}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(server.Close)
	s.URL = server.URL
	s.Origin = origin
	if s.Origin == "" {
		s.Origin = s.URL
	}
	s.broker = brokertokentest.NewStubBroker(t)
	jkt, err := proof.Thumbprint(&s.Account.key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint the stand-in's key: %v", err)
	}
	s.broker.Says(s.Token, readToken(jkt))
	s.verifier.Store(newVerifier(t, s.broker.Server.URL, staticBearer("dispatchs-own-account-token"), s.Origin))
	return s
}

// IntrospectAt makes the stand-in ask a real broker at brokerURL whether each presented token is
// live, presenting account, as Dispatch asks the broker DISPATCH_AGENT_SECRETS_URL names with its
// own projected token: a compiled broker's own read token is then one the stand-in takes, and its
// own Token no longer is.
func (s *Server) IntrospectAt(t testing.TB, brokerURL string, account brokertoken.Account) {
	t.Helper()
	s.verifier.Store(newVerifier(t, brokerURL, account, s.Origin))
}

// newVerifier is the check Dispatch makes of a broker-issued token, asking the broker at brokerURL
// with account, holding every proof to origin, and serving a read-only token, as cmd/dispatch's
// brokerTokenVerifier builds it; spent proofs are kept in memory, and the bucket of introspections
// is wide enough that no test's burst meets it.
func newVerifier(t testing.TB, brokerURL string, account brokertoken.Account, origin string) *brokertoken.Verifier {
	t.Helper()
	verifier, err := brokertoken.New(brokertoken.Options{
		BrokerURL:      brokerURL,
		Account:        account,
		Origin:         strings.TrimSuffix(origin, "/"),
		Replay:         storage.NewMemoryStore(),
		Address:        func(r *http.Request) string { return r.RemoteAddr },
		AddressLimit:   ratelimit.Limit{Every: time.Millisecond, Burst: 10000},
		ServesReadOnly: true,
	})
	if err != nil {
		t.Fatalf("build the stand-in's broker-token check: %v", err)
	}
	return verifier
}

// staticBearer is an introspection credential that never rotates: a brokertoken.Account.
type staticBearer string

func (b staticBearer) Read() (string, error) { return string(b), nil }

// readToken is the broker's answer for a read token of the broker's own read entry bound to jkt:
// the scope read alone, the project scope none, AccessService and AccessSession, no person and no
// project.
func readToken(jkt string) brokertoken.Introspection {
	return brokertoken.Introspection{
		Active: true, Session: AccessSession, Service: AccessService, ProjectScope: brokertoken.ProjectScopeNone,
		Scope: brokertoken.ScopeRead, ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: brokertoken.ExchangeClientID, Thumbprint: jkt,
	}
}

// Calls is how many requests the stand-in has answered, refusals and the ones it failed while
// down included.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Presentations is what each request the stand-in got presented, in the order they came, as a
// copy.
func (s *Server) Presentations() []Presentation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.presentations)
}

// SetDown makes every later request fail 503 (true) or be answered again (false), which is what a
// Dispatch the broker cannot reach looks like to it.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// SetHeld makes the person route answer person (a lowercase login) held, or not held, as Dispatch
// does once it records a hold of them or their next sign-in. Every person starts not held.
func (s *Server) SetHeld(person string, held bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[person] = held
}

// SetNotHeldUntil makes the person route answer person (a lowercase login), while it answers them
// not held, with not_held_until, as an IAP deployment's Dispatch answers every not-held person
// with the last active Workspace Directory answer plus 24 hours, or with as_of itself once that
// passed; the zero time answers none again, as Dispatch does outside IAP. A held answer names no
// deadline whatever this says, as Dispatch's names none.
func (s *Server) SetNotHeldUntil(person string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until.IsZero() {
		delete(s.until, person)
		return
	}
	s.until[person] = until
}

// standing is whether person is held, the as_of of this answer, later than every as_of the
// stand-in answered before, and the not_held_until it answers a not-held person with (the zero
// time for none).
func (s *Server) standing(person string) (bool, time.Time, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	if !asOf.After(s.asOf) {
		asOf = s.asOf.Add(time.Microsecond)
	}
	s.asOf = asOf
	if s.held[person] {
		return true, asOf, time.Time{}
	}
	return false, asOf, s.until[person]
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls++
	s.presentations = append(s.presentations, Presentation{Authorization: r.Header.Get("Authorization"), Proof: r.Header.Get("DPoP")})
	down := s.down
	s.mu.Unlock()
	if down {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"code": "UNAVAILABLE", "error": "the stand-in is down"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": "METHOD_NOT_ALLOWED", "error": r.Method})
		return
	}
	if !s.admitted(w, r) {
		return
	}
	person := r.URL.Query().Get("person")
	switch {
	case r.URL.Path == personPath:
		if person == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "INVALID_QUERY", "error": "person is required"})
			return
		}
		held, asOf, until := s.standing(person)
		answer := map[string]any{"person": person, "member_kind": s.kind(person), "held": held, "as_of": asOf.Format(time.RFC3339Nano)}
		if !until.IsZero() {
			answer["not_held_until"] = until.UTC().Format(time.RFC3339Nano)
		}
		writeJSON(w, http.StatusOK, answer)
	case strings.HasPrefix(r.URL.Path, projectPath):
		key := strings.TrimPrefix(r.URL.Path, projectPath)
		row, held := s.rows[key]
		if !held || strings.Contains(key, "/") {
			writeJSON(w, http.StatusNotFound, map[string]any{"code": "PROJECT_NOT_FOUND", "error": "no such project"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"project":        key,
			"visibility":     row.Visibility,
			"owner":          row.Owner,
			"person":         person,
			"member_kind":    s.kind(person),
			"visible":        s.visible(row, person),
			"slack_channels": []string{},
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"code": "NOT_FOUND", "error": r.URL.Path})
	}
}

// admitted holds r to what Dispatch holds the broker's call to, and answers the refusal when it
// does not meet it: a live read token under the DPoP scheme, its proof of the token's key signed
// for this method and Origin's URL of this path (brokertoken.Verifier), naming AccessService. A
// token that does not verify is 401 and another service's is 403, as Dispatch answers them; a
// broker the check could not ask is 503.
func (s *Server) admitted(w http.ResponseWriter, r *http.Request) bool {
	caller, err := s.verifier.Load().Verify(r.Context(), r)
	switch {
	case err == nil && caller.ReadOnly && caller.Service == AccessService && caller.Person == "":
		return true
	case err == nil:
		writeJSON(w, http.StatusForbidden, map[string]any{"code": "ACCESS_ACCOUNT_REQUIRED", "error": "the stand-in takes the read token of " + AccessService + " alone"})
	case errors.Is(err, brokertoken.ErrNotActive), errors.Is(err, brokertoken.ErrKeyMismatch), errors.Is(err, brokertoken.ErrProofInvalid),
		errors.Is(err, brokertoken.ErrNotPresented), errors.Is(err, brokertoken.ErrNotASession):
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": "BROKER_TOKEN_REFUSED", "error": err.Error()})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"code": "BROKER_UNAVAILABLE", "error": err.Error()})
	}
	return false
}

// kind is a login's member kind, "none" for a login the stand-in does not hold or no login at all.
func (s *Server) kind(person string) string {
	if kind, held := s.people[person]; held && person != "" {
		return kind
	}
	return "none"
}

// visible is Dispatch's rule as the spec states it: a person sees a project its access list names
// them on (its owner too), or a public project when they are not a restricted member; a query
// naming no person sees the public projects alone.
func (s *Server) visible(row Row, person string) bool {
	if person == "" {
		return row.Visibility == "public"
	}
	if person == row.Owner || slices.Contains(row.Listed, person) {
		return true
	}
	return row.Visibility == "public" && s.kind(person) != "restricted"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Exchange is a stand-in for the broker's own token endpoint, where the broker trades its projected
// token for its read token: each trade is answered a fresh token bound to the key its DPoP proof
// names, which the stand-in it belongs to then holds live as the broker's read token, and keeps
// what the trade presented. A test sets how long its tokens live, refuses its trades, or holds
// them back.
type Exchange struct {
	// URL is the token endpoint's base: what a ReadToken trades at, or connects to.
	URL string

	server *Server
	mu     sync.Mutex
	trades []Trade
	// expiresIn is each token's life in seconds; refusal, when set, is the OAuth error every
	// trade is answered with instead; held, when set, is closed to let trades held back go on.
	expiresIn int
	refusal   *refusal
	held      chan struct{}
	// waiting is how many trades are held back now.
	waiting int
}

type refusal struct {
	status int
	code   string
}

// Trade is what one trade presented: the subject token and the htu its proof signed.
type Trade struct {
	SubjectToken string
	ProofURL     string
	Session      string
}

// NewExchange serves a token endpoint whose tokens s takes, minting tokens that live an hour.
func (s *Server) NewExchange(t testing.TB) *Exchange {
	t.Helper()
	e := &Exchange{server: s, expiresIn: 3600}
	server := httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(server.Close)
	e.URL = server.URL
	return e
}

// SetExpiresIn makes every later token live seconds.
func (e *Exchange) SetExpiresIn(seconds int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expiresIn = seconds
}

// Refuse makes every later trade answer status with the OAuth error code, as the broker refuses a
// projected token no read entry lists (invalid_grant) or answers 429.
func (e *Exchange) Refuse(status int, code string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refusal = &refusal{status: status, code: code}
}

// Hold holds every later trade back until the release it answers is called, or its caller hangs
// up: how a test shows a trade is bounded by the deadline of the request that needs it.
func (e *Exchange) Hold() (release func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	held := make(chan struct{})
	e.held = held
	var once sync.Once
	return func() { once.Do(func() { close(held) }) }
}

// Waiting is how many trades the endpoint holds back now (Hold): a trade that reached it and has
// not been answered.
func (e *Exchange) Waiting() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.waiting
}

// Trades is what each trade presented, in the order they came, as a copy.
func (e *Exchange) Trades() []Trade {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.trades)
}

func (e *Exchange) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != brokerclient.TokenPath {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_description": err.Error()})
		return
	}
	e.mu.Lock()
	held, refused, expiresIn := e.held, e.refusal, e.expiresIn
	e.mu.Unlock()
	if held != nil {
		e.mu.Lock()
		e.waiting++
		e.mu.Unlock()
		select {
		case <-held:
		case <-r.Context().Done():
		}
		e.mu.Lock()
		e.waiting--
		e.mu.Unlock()
		if r.Context().Err() != nil {
			return
		}
	}
	jkt, htu, err := proofOf(r.Header.Get("DPoP"))
	e.mu.Lock()
	e.trades = append(e.trades, Trade{SubjectToken: r.PostForm.Get("subject_token"), ProofURL: htu, Session: r.PostForm.Get(oauthwire.FormSession)})
	n := len(e.trades)
	e.mu.Unlock()
	switch {
	case refused != nil:
		writeJSON(w, refused.status, map[string]any{"error": refused.code, "error_description": "the stand-in refuses this trade"})
		return
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_dpop_proof", "error_description": err.Error()})
		return
	case r.PostForm.Get("grant_type") != brokerclient.GrantTypeTokenExchange || r.PostForm.Get("subject_token_type") != oauthwire.SubjectTokenTypeKubernetes || r.PostForm.Get("subject_token") == "":
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_description": "not a trade of a projected token"})
		return
	}
	token := fmt.Sprintf("read-token-%d-%s", n, jkt[:8])
	e.server.broker.Says(token, readToken(jkt))
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token, "token_type": "DPoP", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": expiresIn,
	})
}

// proofOf verifies a DPoP proof's signature by the key it embeds and answers that key's thumbprint
// and the htu it signed.
func proofOf(compact string) (jkt, htu string, err error) {
	signed, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(signed.Signatures) != 1 || signed.Signatures[0].Protected.JSONWebKey == nil {
		return "", "", fmt.Errorf("the trade carries no proof of a key: %v", err)
	}
	key := signed.Signatures[0].Protected.JSONWebKey
	payload, err := signed.Verify(key)
	if err != nil {
		return "", "", fmt.Errorf("the proof does not verify: %w", err)
	}
	var claims struct {
		HTU string `json:"htu"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", fmt.Errorf("the proof's claims do not read: %w", err)
	}
	public, ok := key.Key.(*ecdsa.PublicKey)
	if !ok {
		return "", "", errors.New("the proof's key is no P-256 key")
	}
	if jkt, err = proof.Thumbprint(public); err != nil {
		return "", "", err
	}
	return jkt, claims.HTU, nil
}
