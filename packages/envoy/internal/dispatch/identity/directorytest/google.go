// Package directorytest serves, on loopback, the three Google hosts a Workspace Directory check
// reaches: IAM Credentials' signJwt, Google's OAuth token endpoint and the Directory API's
// users.get. The impersonate package fixes the first two hosts, and identity.DirectoryReader the
// third, so the fake is reached through one http.Client that sends each of the three hosts to its
// own httptest server and refuses every other host: identity.NewDirectoryReader takes it as
// option.WithHTTPClient (Google.ClientOptions), and nothing a test runs reaches Google.
package directorytest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"
)

// The hosts the fake serves, as a Directory check names them.
const (
	SignerHost    = "iamcredentials.googleapis.com"
	TokenHost     = "oauth2.googleapis.com"
	DirectoryHost = "admin.googleapis.com"
)

// DirectoryScope is the one scope the Domain-wide Delegation grant gives the signer, and the fake's
// grant until Grant changes it.
const DirectoryScope = "https://www.googleapis.com/auth/admin.directory.user.readonly"

// Planted is in every body the fake writes for a refusal, a missing user or an answer a test gives
// it, beside the words Google writes: a reader's error that holds it quotes Google's answer.
const Planted = "planted-quokka-words"

// Answer is a reply a test gives one of the hosts in place of the one the fake would write: Status
// and Body as written, or, with Stall, no reply until the request's context ends.
type Answer struct {
	Status int
	Body   string
	Stall  bool
	// resource is the user SetUser gave, written for each read by the fields it asks for (partial).
	resource map[string]any
}

// User is a Directory user as users.get answers for it: 200 with these four fields, beside the
// resource's kind and etag, each written only when the read's fields parameter names it or the
// read names no fields, as Google's partial response writes a resource.
type User struct {
	ID           string
	PrimaryEmail string
	Suspended    bool
	Archived     bool
}

// Sign is one signJwt request: the service account it named, and the claims it asked IAM to sign.
type Sign struct {
	Principal string
	Issuer    string
	Subject   string
	Scope     string
	Audience  string
}

// Read is one users.get request: the user key it asked for, its fields parameter, and the
// delegated user and scope of the access token it carried ("" for a token the fake did not issue).
type Read struct {
	UserKey string
	Fields  string
	Subject string
	Scope   string
}

// Google is the three hosts. Its zero answers: signJwt signs any payload; the token endpoint
// trades a JWT it signed for an access token when every scope the JWT asks for is granted, and
// refuses it 401 unauthorized_client otherwise, as Google does for a scope the delegation does not
// grant; users.get answers each user SetUser gave it, with the fields its fields parameter names,
// and 404 for any other key, and 401 for a token it did not issue for the Directory scope.
type Google struct {
	client  *http.Client
	release chan struct{}

	mu        sync.Mutex
	signer    *Answer
	hold      chan struct{}
	scopes    map[string]*Answer
	granted   map[string]bool
	tokenLife time.Duration
	users     map[string]Answer
	signed    map[string]Sign
	issued    map[string]Sign
	signs     []Sign
	exchanges []Sign
	reads     []Read
	// open is how many requests each host is answering now.
	open map[string]int
}

// ServeGoogle starts the three hosts, stopped when t ends; a stalled reply is released first.
func ServeGoogle(t testing.TB) *Google {
	t.Helper()
	g := &Google{
		release:   make(chan struct{}),
		scopes:    map[string]*Answer{},
		granted:   map[string]bool{DirectoryScope: true},
		tokenLife: time.Hour,
		users:     map[string]Answer{},
		signed:    map[string]Sign{},
		issued:    map[string]Sign{},
		open:      map[string]int{},
	}
	servers := map[string]*httptest.Server{
		SignerHost:    httptest.NewServer(g.counted(SignerHost, g.serveSign)),
		TokenHost:     httptest.NewServer(g.counted(TokenHost, g.serveToken)),
		DirectoryHost: httptest.NewServer(g.counted(DirectoryHost, g.serveDirectory)),
	}
	// A transport of its own, with no proxy, so HTTPS_PROXY in the test's environment reroutes nothing.
	transport := &http.Transport{}
	routes := map[string]string{}
	for host, server := range servers {
		t.Cleanup(server.Close)
		routes[host] = strings.TrimPrefix(server.URL, "http://")
	}
	t.Cleanup(transport.CloseIdleConnections)
	// Cleanups run last first: the stalled replies are released before any server waits for them.
	t.Cleanup(func() { close(g.release) })
	g.client = &http.Client{Transport: route{routes: routes, base: transport}}
	return g
}

// ClientOptions reach the three hosts and no other, with no credential of their own.
func (g *Google) ClientOptions() []option.ClientOption {
	return []option.ClientOption{option.WithHTTPClient(g.client)}
}

// SetUser makes users.get answer key with u.
func (g *Google) SetUser(key string, u User) {
	g.AnswerUser(key, Answer{Status: http.StatusOK, resource: map[string]any{
		"kind": "admin#directory#user", "id": u.ID, "primaryEmail": u.PrimaryEmail, "suspended": u.Suspended,
		"archived": u.Archived, "etag": `"` + Planted + `"`,
	}})
}

// AnswerUser makes users.get answer key with a.
func (g *Google) AnswerUser(key string, a Answer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.users[key] = a
}

// HoldSigner holds each signJwt request from now on as it arrives, until release is called, the
// request's context ends or the fake stops; a released request is then answered as AnswerSigner
// says. Signs counts a held request from its arrival, so a test sees how many reached the signer
// while none was answered.
func (g *Google) HoldSigner() (release func()) {
	held := make(chan struct{})
	g.mu.Lock()
	g.hold = held
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.hold == held {
				g.hold = nil
			}
			g.mu.Unlock()
			close(held)
		})
	}
}

// AnswerSigner makes signJwt answer every request with a; nil signs again.
func (g *Google) AnswerSigner(a *Answer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.signer = a
}

// AnswerScope makes the token endpoint answer every exchange for a JWT that asks for scope with a;
// nil trades by the grant again.
func (g *Google) AnswerScope(scope string, a *Answer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a == nil {
		delete(g.scopes, scope)
		return
	}
	g.scopes[scope] = a
}

// Grant replaces the scopes the Domain-wide Delegation grant gives.
func (g *Google) Grant(scopes ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.granted = map[string]bool{}
	for _, scope := range scopes {
		g.granted[scope] = true
	}
}

// TokenLife sets the expires_in of each access token issued from now on.
func (g *Google) TokenLife(life time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tokenLife = life
}

// Signs are the signJwt requests that reached the fake, in order, each counted as it arrived.
func (g *Google) Signs() []Sign {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Sign(nil), g.signs...)
}

// Exchanges are the claims of each JWT the token endpoint was asked to trade, in order.
func (g *Google) Exchanges() []Sign {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Sign(nil), g.exchanges...)
}

// Reads are the users.get requests the fake answered, in order.
func (g *Google) Reads() []Read {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Read(nil), g.reads...)
}

// Open is how many requests to host (SignerHost, TokenHost or DirectoryHost) the fake is still
// answering: a held or stalled one stays open until it is let go, or until the client that sent
// it hangs up, which ends the request's context.
func (g *Google) Open(host string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open[host]
}

// counted serves host with serve, counting the request open until serve returns. It reads the
// request's body to its end first: net/http notices a client that hung up, and ends the request's
// context, only once the body was read to its end.
func (g *Google) counted(host string, serve http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		g.mu.Lock()
		g.open[host]++
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			g.open[host]--
			g.mu.Unlock()
		}()
		serve(w, r)
	}
}

// route sends each of the three hosts' https requests to its server, and refuses any other.
type route struct {
	routes map[string]string
	base   http.RoundTripper
}

func (r route) RoundTrip(req *http.Request) (*http.Response, error) {
	address, ok := r.routes[req.URL.Host]
	if !ok || req.URL.Scheme != "https" {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("directorytest: no fake serves %s://%s", req.URL.Scheme, req.URL.Host)
	}
	out := req.Clone(req.Context())
	out.URL.Scheme = "http"
	out.URL.Host = address
	out.Host = ""
	return r.base.RoundTrip(out)
}

// stall holds a reply until the request's context ends or the fake stops.
func (g *Google) stall(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-g.release:
	}
}

func (g *Google) write(w http.ResponseWriter, r *http.Request, a Answer) {
	if a.Stall {
		g.stall(r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(a.Status)
	_, _ = w.Write([]byte(a.Body))
}

// googleError is the body Google's JSON APIs write for an error status, with Planted in its message.
func googleError(status int, message string) string {
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"code": status, "message": message + " " + Planted, "errors": []map[string]string{{"message": message, "reason": Planted}},
	}})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// oauthError is the body Google's token endpoint writes for a refused exchange.
func oauthError(code, description string) string {
	body, err := json.Marshal(map[string]string{"error": code, "error_description": description + " " + Planted})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// serveSign is IAM Credentials' projects.serviceAccounts.signJwt: it signs the payload as given
// and returns a JWT whose payload is that one, which the token endpoint reads back.
func (g *Google) serveSign(w http.ResponseWriter, r *http.Request) {
	principal, ok := strings.CutPrefix(r.URL.Path, "/v1/projects/-/serviceAccounts/")
	principal, signJWT := strings.CutSuffix(principal, ":signJwt")
	if !ok || !signJWT || r.Method != http.MethodPost {
		g.write(w, r, Answer{Status: http.StatusNotFound, Body: googleError(http.StatusNotFound, "Not found")})
		return
	}
	var request struct {
		Payload string `json:"payload"`
	}
	var claims struct {
		Iss   string `json:"iss"`
		Scope string `json:"scope"`
		Sub   string `json:"sub"`
		Aud   string `json:"aud"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || json.Unmarshal([]byte(request.Payload), &claims) != nil {
		g.write(w, r, Answer{Status: http.StatusBadRequest, Body: googleError(http.StatusBadRequest, "Invalid JSON payload")})
		return
	}
	sign := Sign{Principal: principal, Issuer: claims.Iss, Subject: claims.Sub, Scope: claims.Scope, Audience: claims.Aud}
	g.mu.Lock()
	g.signs = append(g.signs, sign)
	// The answer is the one set when the request arrived, or, for a held request, when it is let go.
	hold, answer := g.hold, g.signer
	g.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		case <-g.release:
			return
		}
		g.mu.Lock()
		answer = g.signer
		g.mu.Unlock()
	}
	g.mu.Lock()
	jwt := ""
	if answer == nil {
		jwt = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
			base64.RawURLEncoding.EncodeToString([]byte(request.Payload)) + "." +
			base64.RawURLEncoding.EncodeToString([]byte("signature-"+strconv.Itoa(len(g.signed)+1)))
		g.signed[jwt] = sign
	}
	g.mu.Unlock()
	if answer != nil {
		g.write(w, r, *answer)
		return
	}
	body, err := json.Marshal(map[string]string{"keyId": "fake-key", "signedJwt": jwt})
	if err != nil {
		panic(err)
	}
	g.write(w, r, Answer{Status: http.StatusOK, Body: string(body)})
}

// serveToken is the token endpoint's JWT bearer grant, in the legacy form the impersonate package
// sends (grant_type=assertion), for a JWT the fake signed and that names the token endpoint.
func (g *Google) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/token" || r.Method != http.MethodPost || r.ParseForm() != nil ||
		r.PostForm.Get("grant_type") != "assertion" || r.PostForm.Get("assertion_type") != "http://oauth.net/grant_type/jwt/1.0/bearer" {
		g.write(w, r, Answer{Status: http.StatusBadRequest, Body: oauthError("unsupported_grant_type", "Invalid grant_type")})
		return
	}
	g.mu.Lock()
	sign, signed := g.signed[r.PostForm.Get("assertion")]
	if signed {
		g.exchanges = append(g.exchanges, sign)
	}
	var answer *Answer
	granted := sign.Scope != ""
	for _, scope := range strings.Fields(sign.Scope) {
		if a := g.scopes[scope]; a != nil && answer == nil {
			answer = a
		}
		granted = granted && g.granted[scope]
	}
	life := g.tokenLife
	token := ""
	if signed && answer == nil && granted && sign.Audience == "https://"+TokenHost+"/token" {
		token = "ya29.fake-" + strconv.Itoa(len(g.issued)+1)
		g.issued[token] = sign
	}
	g.mu.Unlock()
	switch {
	case !signed:
		g.write(w, r, Answer{Status: http.StatusBadRequest, Body: oauthError("invalid_grant", "Invalid JWT Signature.")})
	case answer != nil:
		g.write(w, r, *answer)
	case token == "":
		g.write(w, r, Answer{Status: http.StatusUnauthorized, Body: oauthError("unauthorized_client",
			"Client is unauthorized to retrieve access tokens using this method, or client not authorized for any of the scopes requested.")})
	default:
		body, err := json.Marshal(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": int64(life / time.Second)})
		if err != nil {
			panic(err)
		}
		g.write(w, r, Answer{Status: http.StatusOK, Body: string(body)})
	}
}

// serveDirectory is the Directory API's users.get.
func (g *Google) serveDirectory(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.EscapedPath(), "/admin/directory/v1/users/")
	if ok {
		var err error
		key, err = url.PathUnescape(key)
		ok = err == nil && key != "" && !strings.Contains(key, "/") && r.Method == http.MethodGet
	}
	if !ok {
		g.write(w, r, Answer{Status: http.StatusNotFound, Body: googleError(http.StatusNotFound, "Not Found")})
		return
	}
	bearer, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	token, issued := g.issued[bearer]
	g.reads = append(g.reads, Read{UserKey: key, Fields: r.URL.Query().Get("fields"), Subject: token.Subject, Scope: token.Scope})
	answer, known := g.users[key]
	g.mu.Unlock()
	switch {
	case !hasBearer || !issued || !strings.Contains(" "+token.Scope+" ", " "+DirectoryScope+" "):
		g.write(w, r, Answer{Status: http.StatusUnauthorized, Body: googleError(http.StatusUnauthorized, "Request had invalid authentication credentials.")})
	case !known:
		g.write(w, r, Answer{Status: http.StatusNotFound, Body: googleError(http.StatusNotFound, "Resource Not Found: userKey")})
	case answer.resource == nil:
		g.write(w, r, answer)
	default:
		g.write(w, r, Answer{Status: answer.Status, Body: partial(answer.resource, r.URL.Query().Get("fields"))})
	}
}

// partial is resource as Google's partial response writes it for a fields parameter: the whole
// resource for no fields, and otherwise only the fields it names, comma separated. The fake reads
// top-level names alone, and leaves out a name the resource does not hold.
func partial(resource map[string]any, fields string) string {
	selected := resource
	if fields != "" {
		selected = map[string]any{}
		for _, name := range strings.Split(fields, ",") {
			if value, ok := resource[name]; ok {
				selected[name] = value
			}
		}
	}
	body, err := json.Marshal(selected)
	if err != nil {
		panic(err)
	}
	return string(body)
}
