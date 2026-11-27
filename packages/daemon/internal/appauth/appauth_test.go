package appauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

func testAppKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
}

func decodeJWTPart(t *testing.T, part string, into any) {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode JWT part: %v", err)
	}
	if err := json.Unmarshal(decoded, into); err != nil {
		t.Fatalf("decode JWT JSON: %v", err)
	}
}

func verifyAppJWT(t *testing.T, token string, publicKey *rsa.PublicKey, now time.Time, appID string) {
	t.Helper()
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	var claims struct {
		Issuer  string `json:"iss"`
		Issued  int64  `json:"iat"`
		Expires int64  `json:"exp"`
	}
	parts := splitJWT(t, token)
	decodeJWTPart(t, parts[0], &header)
	decodeJWTPart(t, parts[1], &claims)
	if header.Algorithm != "RS256" || header.Type != "JWT" {
		t.Fatalf("JWT header = %+v, want RS256 JWT", header)
	}
	if claims.Issuer != appID {
		t.Fatalf("JWT issuer = %q, want %q", claims.Issuer, appID)
	}
	if claims.Issued != now.Unix()-60 || claims.Expires != now.Unix()+540 {
		t.Fatalf("JWT claims = %+v, want iat %d and exp %d", claims, now.Unix()-60, now.Unix()+540)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode JWT signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("verify JWT signature: %v", err)
	}
}

func splitJWT(t *testing.T, token string) []string {
	t.Helper()
	var parts []string
	for _, part := range split(token, '.') {
		parts = append(parts, part)
	}
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	return parts
}

func split(value string, sep byte) []string {
	start := 0
	parts := make([]string, 0, 3)
	for i := 0; i < len(value); i++ {
		if value[i] == sep {
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	return append(parts, value[start:])
}

// mintedFor is GitHub's answer to a token exchange: the token, its expiry, the repositories
// GitHub says the installation holds — repos, which no request names and which the mint reads
// not at all — and the permissions it says the token holds, which every mint holds its answer
// to (narrowedTo). GitHub names in every answer each permission the token it minted holds, so a
// stand-in names back the permissions the request asked for (askedInExchange).
func mintedFor(token string, expires time.Time, permissions map[string]string, repos ...string) string {
	named := make([]string, 0, len(repos))
	for _, repo := range repos {
		named = append(named, fmt.Sprintf(`{"name":%q}`, repo))
	}
	// A map of strings always marshals, and json.Marshal sorts its keys.
	held, _ := json.Marshal(permissions)
	return fmt.Sprintf(`{"token":%q,"expires_at":%q,"repository_selection":"selected","repositories":[%s],"permissions":%s}`,
		token, expires.Format(time.RFC3339), strings.Join(named, ","), held)
}

// askedInExchange is the body of the token exchange r carries, both as the text it was sent as
// and as GitHub parses it: the permissions the token is to hold. A stand-in GitHub answers from
// it, as GitHub answers a token request from the body it was sent (mintedFor), and a test that
// asks whether a request named a repository at all reads the text.
func askedInExchange(t *testing.T, r *http.Request) (string, accessTokensBody) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read the exchange body: %v", err)
		return "", accessTokensBody{}
	}
	var asked accessTokensBody
	if err := json.Unmarshal(raw, &asked); err != nil {
		t.Errorf("the exchange body is not the JSON an access_tokens request is: %v", err)
	}
	return string(raw), asked
}

// grantedEverythingAsked is every permission readPermissions and the two writePermissions name,
// at the highest access any of them names: what an installation whose account has approved all
// of Legion's grants is granted.
var grantedEverythingAsked = map[string]string{
	"actions": "write", "attestations": "read", "checks": "write", "contents": "write", "issues": "write",
	"metadata": "read", "pull_requests": "write", "security_events": "write", "statuses": "write",
	"workflows": "write",
}

// installationGranting is GitHub's GET /app/installations/{id}: the installation and the
// permissions its account granted the App, which a mint reads before each exchange and cuts its
// request down to (exchangeGranted), with the null suspended_at GitHub answers for an
// installation no account suspended.
func installationGranting(id string, permissions map[string]string) string {
	return installationAnswer(id, permissions, "null")
}

// suspendedSince is when the stand-in's suspended installations were suspended.
const suspendedSince = "2026-11-14T08:00:00Z"

// installationSuspended is installationGranting for an installation its account suspended: the
// permissions stand as they were granted, and suspended_at is the time of the suspension, the
// one field a mint reads a suspension from (grantedPermissions).
func installationSuspended(id string, permissions map[string]string) string {
	return installationAnswer(id, permissions, `"`+suspendedSince+`"`)
}

// installationAnswer is the installation id granted permissions, with suspendedAt as the JSON
// value of its suspended_at.
func installationAnswer(id string, permissions map[string]string, suspendedAt string) string {
	// A map of strings always marshals, and json.Marshal sorts its keys.
	encoded, _ := json.Marshal(permissions)
	return fmt.Sprintf(`{"id":%s,"permissions":%s,"suspended_at":%s}`, id, encoded, suspendedAt)
}

func TestGenerateJWTSignsShippedClaims(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	token, err := GenerateJWT("12345", privatePEM, now)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	verifyAppJWT(t, token, &key.PublicKey, now, "12345")
}

// TestGenerateJWTExpiresInsideGitHubsCapWithMargin holds exp at most nine minutes past now and iat
// at or before now: GitHub refuses an exp more than ten minutes ahead of its clock, so a signer
// whose clock runs ahead of GitHub's fails at exactly ten minutes.
func TestGenerateJWTExpiresInsideGitHubsCapWithMargin(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 10, 27, 15, 42, 0, 0, time.UTC)
	token, err := GenerateJWT("12345", privatePEM, now)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}
	var claims struct {
		Issued  int64 `json:"iat"`
		Expires int64 `json:"exp"`
	}
	decodeJWTPart(t, splitJWT(t, token)[1], &claims)
	if lead := time.Duration(claims.Expires-now.Unix()) * time.Second; lead > 9*time.Minute {
		t.Fatalf("exp is %s past now, want at most 9m0s (GitHub refuses more than 10m ahead of its clock)", lead)
	}
	if claims.Issued > now.Unix() {
		t.Fatalf("iat %d is after now %d", claims.Issued, now.Unix())
	}
}

func TestAppRoleForEveryRole(t *testing.T) {
	for _, tc := range []struct {
		role claim.Role
		want AppRole
	}{
		{claim.RoleArchitect, Review},
		{claim.RolePlanner, Review},
		{claim.RoleImplementer, Implement},
		{claim.RoleTester, Review},
		{claim.RoleReviewer, Review},
		{claim.RoleMerger, Implement},
		{claim.RoleController, Review},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			if got := AppRoleFor(tc.role); got != tc.want {
				t.Errorf("AppRoleFor(%q) = %q, want %q", tc.role, got, tc.want)
			}
		})
	}
}

func TestTokensMintsRefreshesAndCachesIdentity(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	exchanges, discoveries, identities := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwt := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/app/installations":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			mu.Lock()
			discoveries++
			mu.Unlock()
			fmt.Fprint(w, `[{"id":77,"account":{"login":"Sjawhar"}}]`)
		case "/app/installations/77":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
		case "/app/installations/77/access_tokens":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			_, asked := askedInExchange(t, r)
			mu.Lock()
			exchanges++
			exchange := exchanges
			mu.Unlock()
			fmt.Fprint(w, mintedFor(fmt.Sprintf("installation-%d", exchange), now.Add(time.Hour), asked.Permissions, "legion"))
		case "/app":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			fmt.Fprint(w, `{"slug":"legion-implementer"}`)
		case "/users/legion-implementer[bot]":
			mu.Lock()
			identities++
			mu.Unlock()
			if got := r.Header.Get("Authorization"); got != "token installation-1" {
				t.Fatalf("bot identity authorization = %q, want installation token", got)
			}
			fmt.Fprint(w, `{"id":100000001}`)
		default:
			t.Fatalf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	first, err := tokens.Token(context.Background(), Implement, "sjawhar")
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if first.Token != "installation-1" || first.Identity.Name != "legion-implementer[bot]" || first.Identity.Email != "100000001+legion-implementer[bot]@users.noreply.github.com" {
		t.Fatalf("first lease = %+v", first)
	}
	if _, err := tokens.Token(context.Background(), Implement, "SJAWHAR"); err != nil {
		t.Fatalf("cached token: %v", err)
	}

	// The hour-long lease has fewer than refreshWindow's twenty minutes left: the next call mints.
	now = now.Add(40*time.Minute + time.Second)
	refreshed, err := tokens.Token(context.Background(), Implement, "sjawhar")
	if err != nil {
		t.Fatalf("refreshed token: %v", err)
	}
	if refreshed.Token != "installation-2" {
		t.Fatalf("refreshed token = %q, want installation-2", refreshed.Token)
	}
	mu.Lock()
	defer mu.Unlock()
	if exchanges != 2 || discoveries != 1 || identities != 1 {
		t.Fatalf("exchanges=%d discoveries=%d identities=%d, want 2, 1, 1", exchanges, discoveries, identities)
	}
}

// KnownIdentity is the bot identity a lease of the App already fetched, read from the Manager's
// cache alone: none is known for either App before a mint, the identity once a lease of that App
// fetched it, still none for the other App, and no read of it, before or after, reaches GitHub.
func TestKnownIdentityIsWhatALeaseFetchedAndAsksGitHubNothing(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var requests atomic.Int64
	server := githubtest.Serve(t, githubtest.Spec{Record: func(*http.Request) { requests.Add(1) }, Handle: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations":
			fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
		case "/app/installations/77":
			fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
		case "/app/installations/77/access_tokens":
			_, asked := askedInExchange(t, r)
			fmt.Fprint(w, mintedFor("installation-1", now.Add(time.Hour), asked.Permissions, "legion"))
		case "/app":
			fmt.Fprint(w, `{"slug":"legion-implementer"}`)
		case "/users/legion-implementer[bot]":
			fmt.Fprint(w, `{"id":271566630}`)
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	t.Cleanup(tokens.Close)

	for _, role := range append(slices.Clone(Roles), "unconfigured") {
		if identity, known := tokens.KnownIdentity(role); known {
			t.Errorf("KnownIdentity(%s) before any mint = %+v, want none known", role, identity)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("%d requests reached GitHub before any mint, want none: KnownIdentity asks GitHub nothing", got)
	}

	lease, err := tokens.Token(context.Background(), Implement, "acme")
	if err != nil {
		t.Fatalf("the implement App's lease on acme: %v", err)
	}
	minted := requests.Load()
	want := GitIdentity{Name: "legion-implementer[bot]", Email: "271566630+legion-implementer[bot]@users.noreply.github.com"}
	if identity, known := tokens.KnownIdentity(Implement); !known || identity != want || identity != lease.Identity {
		t.Errorf("KnownIdentity(implement) after its lease = %+v, %t; want %+v, the identity the lease fetched", identity, known, want)
	}
	if identity, known := tokens.KnownIdentity(Review); known {
		t.Errorf("KnownIdentity(review) after the implement App's lease alone = %+v, want none known: no lease of the review App fetched one", identity)
	}
	if got := requests.Load(); got != minted {
		t.Errorf("%d requests reached GitHub after the mint's %d, want none: KnownIdentity reads the cache alone", got-minted, minted)
	}
}

// An App removed from an owner after the daemon found its installation answers the next mint's
// exchange 404: the installation found is forgotten and looked for again, so the mint answers
// ErrNotInstalled, an answer about the owner, rather than an error a caller retries without end;
// and an App installed there again, under a new installation, mints on that one.
func TestAnInstallationRemovedAfterItWasFoundIsAnsweredNotInstalled(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	installation, discoveries := "77", 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := installation
		mu.Unlock()
		switch r.URL.Path {
		case "/app/installations":
			mu.Lock()
			discoveries++
			mu.Unlock()
			if current == "" {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprintf(w, `[{"id":%s,"account":{"login":"acme"}}]`, current)
		case "/app/installations/77", "/app/installations/88":
			// The mint reads what the installation is granted before it asks for a token, so a
			// removed installation answers this read 404 before the exchange is ever made.
			if !strings.HasSuffix(r.URL.Path, "/"+current) || current == "" {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			fmt.Fprint(w, installationGranting(current, grantedEverythingAsked))
		case "/app/installations/77/access_tokens", "/app/installations/88/access_tokens":
			if !strings.Contains(r.URL.Path, "/"+current+"/") || current == "" {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			_, asked := askedInExchange(t, r)
			fmt.Fprint(w, mintedFor("installation-"+current, now.Add(time.Hour), asked.Permissions, "legion"))
		case "/app":
			fmt.Fprint(w, `{"slug":"legion-reviewer"}`)
		case "/users/legion-reviewer[bot]":
			fmt.Fprint(w, `{"id":100000002}`)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	if _, err := tokens.Token(context.Background(), Review, "acme"); err != nil {
		t.Fatalf("first token: %v", err)
	}

	mu.Lock()
	installation = ""
	mu.Unlock()
	now = now.Add(time.Hour)
	_, err := tokens.Token(context.Background(), Review, "acme")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("a mint after the App was removed = %v, want ErrNotInstalled", err)
	}

	mu.Lock()
	installation = "88"
	mu.Unlock()
	lease, err := tokens.Token(context.Background(), Review, "acme")
	if err != nil || lease.Token != "installation-88" {
		t.Fatalf("a mint after the App was installed again = %+v, %v; want the new installation's token", lease, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if discoveries != 3 {
		t.Fatalf("installations were listed %d times, want 3: at the first mint, once the removed one answered 404, and once more for the reinstall", discoveries)
	}
}

// Forget drops both of an owner's cached leases for a role, the write token's and the read
// token's, in any case of the owner's name, so the next Token and ReadToken each mint again; a
// lease it did not drop is still served from the cache, and each lease records when it was minted.
func TestForgetMakesTheNextTokenAndReadTokenMintAgain(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations":
			fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
		case "/app/installations/77":
			fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
		case "/app/installations/77/access_tokens":
			_, asked := askedInExchange(t, r)
			mu.Lock()
			exchanges++
			n := exchanges
			mu.Unlock()
			fmt.Fprint(w, mintedFor(fmt.Sprintf("installation-%d", n), now.Add(time.Hour), asked.Permissions, "legion"))
		case "/app":
			fmt.Fprint(w, `{"slug":"legion-reviewer"}`)
		case "/users/legion-reviewer[bot]":
			fmt.Fprint(w, `{"id":100000002}`)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	ctx := context.Background()
	mint := func(read bool) Lease {
		t.Helper()
		lease, err := tokens.Token(ctx, Review, "acme")
		if read {
			lease, err = tokens.ReadToken(ctx, Review, "acme")
		}
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	write, read := mint(false), mint(true)
	if !write.Minted.Equal(now) || !read.Minted.Equal(now) {
		t.Fatalf("minted %s and %s, want both at %s", write.Minted, read.Minted, now)
	}
	if again := mint(false); again.Token != write.Token {
		t.Fatalf("a second Token = %s, want the cached %s", again.Token, write.Token)
	}
	tokens.Forget(Review, "ACME")
	if again := mint(false); again.Token == write.Token {
		t.Fatalf("Token after Forget = the dropped %s, want a fresh mint", again.Token)
	}
	if again := mint(true); again.Token == read.Token {
		t.Fatalf("ReadToken after Forget = the dropped %s, want a fresh mint", again.Token)
	}
	mu.Lock()
	defer mu.Unlock()
	if exchanges != 4 {
		t.Fatalf("%d exchanges, want 4: each lease once, then each again after Forget", exchanges)
	}
}

// ReadToken is the same App's installation token for an owner, narrowed to reading: its exchange
// posts the read permissions, it caches under its own key beside Token's (one mint each for one
// owner, a second ReadToken served from the cache), and it carries the same bot identity.
func TestReadTokenMintsANarrowedTokenCachedApartFromToken(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwt := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/app/installations":
			fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
		case "/app/installations/77":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
		case "/app/installations/77/access_tokens":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			raw, asked := askedInExchange(t, r)
			mu.Lock()
			bodies = append(bodies, raw)
			exchange := len(bodies)
			mu.Unlock()
			fmt.Fprint(w, mintedFor(fmt.Sprintf("installation-%d", exchange), now.Add(time.Hour), asked.Permissions, "legion"))
		case "/app":
			fmt.Fprint(w, `{"slug":"legion-implementer"}`)
		case "/users/legion-implementer[bot]":
			fmt.Fprint(w, `{"id":100000001}`)
		default:
			t.Fatalf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	read, err := tokens.ReadToken(context.Background(), Implement, "acme")
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	write, err := tokens.Token(context.Background(), Implement, "Acme")
	if err != nil {
		t.Fatalf("write token: %v", err)
	}
	again, err := tokens.ReadToken(context.Background(), Implement, "ACME")
	if err != nil {
		t.Fatalf("cached read token: %v", err)
	}
	if read.Token != "installation-1" || write.Token != "installation-2" || again.Token != "installation-1" {
		t.Fatalf("read=%q write=%q again=%q, want installation-1, installation-2, installation-1", read.Token, write.Token, again.Token)
	}
	if read.Identity != write.Identity || read.Identity.Name != "legion-implementer[bot]" {
		t.Fatalf("read identity = %+v, write identity = %+v, want the same bot", read.Identity, write.Identity)
	}
	mu.Lock()
	defer mu.Unlock()
	// json.Marshal of a map sorts its keys, so each body is this exact text: the permissions and
	// nothing else — the read set a reading lease carries, each at read and nothing writable, and
	// the implement App's write set.
	want := []string{
		`{"permissions":{"actions":"read","attestations":"read","checks":"read","contents":"read","issues":"read","metadata":"read","pull_requests":"read","statuses":"read"}}`,
		`{"permissions":{"actions":"write","attestations":"read","checks":"read","contents":"write","issues":"write","metadata":"read","pull_requests":"write","security_events":"read","statuses":"write","workflows":"write"}}`,
	}
	if len(bodies) != len(want) || bodies[0] != want[0] || bodies[1] != want[1] {
		t.Fatalf("exchange bodies = %q, want %q", bodies, want)
	}
}

// A write mint asks for what the installation is granted of the role's set and leaves the rest
// out: an account that has approved three of the implement App's ten permissions gets a token
// holding those three, in one exchange, where asking for all ten would make GitHub refuse the
// mint whole and cost the role its gh bundle. Each permission left out is logged once at warn,
// issues among them, so an installation whose account has not approved a grant is named.
func TestGrantedWriteSetAsksForTheThreeTheInstallationGrants(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{
		owners: []string{"cybermonklabs"},
		slug:   "legion-implementer",
		grants: map[string]string{"contents": "write", "metadata": "read", "checks": "read"},
	}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		// GitHub's own answer to a request naming a permission the installation is not granted:
		// no token at all, rather than a token without that permission.
		for permission := range body.Permissions {
			if github.grants[permission] == "" {
				return http.StatusUnprocessableEntity, `{"message":"The permissions requested are not granted to this installation."}`
			}
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	lease, err := tokens.Token(context.Background(), Implement, "cybermonklabs")
	if err != nil || lease.Token != "ghs_1" {
		t.Fatalf("Token = %q, %v; want the lease the three granted permissions mint", lease.Token, err)
	}

	asked := github.exchanges()
	if len(asked) != 1 {
		t.Fatalf("%d exchanges, want the one mint: the request is cut before it is sent, never asked again", len(asked))
	}
	want := map[string]string{"checks": "read", "contents": "write", "metadata": "read"}
	if !maps.Equal(asked[0].Permissions, want) {
		t.Errorf("the write request named permissions %v, want exactly %v", asked[0].Permissions, want)
	}
	if !slices.Equal(github.permissionReads(), []string{"101"}) {
		t.Errorf("the installation was read %v, want the one read before the exchange", github.permissionReads())
	}
	records := loggedRecords(t, &stderr)
	for _, left := range []string{"actions", "issues", "pull_requests", "statuses", "workflows"} {
		if _, named := asked[0].Permissions[left]; named {
			t.Errorf("the request named %s, which the installation is granted none of", left)
		}
		line := fmt.Sprintf("appauth: the implement App's installation on cybermonklabs is granted no %s, where its token asks for %s: write, so the token is minted without it; grant the App %s: write and approve it on that account", left, left, left)
		if count := shortfallLogged(records, line, Implement, "cybermonklabs", left, "write", ""); count != 1 {
			t.Errorf("the log holds %d warn records of %q with its fields, want exactly one:\n%s", count, line, stderr.String())
		}
	}
}

// A permission granted below the access the role's set asks for is asked for at the access
// granted: the token does what the account approved, and the mint says what it cannot do.
// Asking at the access the set names would make GitHub refuse the mint whole.
func TestGrantedAccessBelowTheSetIsAskedForAtWhatIsGranted(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{
		owners: []string{"acme"},
		slug:   "legion-reviewer",
		grants: map[string]string{"actions": "read", "checks": "write", "contents": "read", "metadata": "write", "pull_requests": "write"},
	}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	if _, err := tokens.Token(context.Background(), Review, "acme"); err != nil {
		t.Fatalf("Token: %v", err)
	}

	asked := github.exchanges()
	if len(asked) != 1 {
		t.Fatalf("%d exchanges, want the one mint", len(asked))
	}
	// contents and actions are granted read where the review set asks for write, so each is
	// asked for at read; metadata is granted write where the set asks for read, and is asked for
	// at the read the set names.
	want := map[string]string{"actions": "read", "checks": "write", "contents": "read", "metadata": "read", "pull_requests": "write"}
	if !maps.Equal(asked[0].Permissions, want) {
		t.Errorf("the write request named permissions %v, want exactly %v", asked[0].Permissions, want)
	}
	records := loggedRecords(t, &stderr)
	for _, permission := range []string{"actions", "contents"} {
		line := fmt.Sprintf("appauth: the review App's installation on acme is granted %s: read, where its token asks for %s: write, so the token is minted with %s: read; grant the App %s: write and approve it on that account", permission, permission, permission, permission)
		if count := shortfallLogged(records, line, Review, "acme", permission, "write", "read"); count != 1 {
			t.Errorf("the log holds %d warn records of %q with its fields, want exactly one:\n%s", count, line, stderr.String())
		}
	}
	for _, record := range records {
		if record.Permission == "metadata" || strings.Contains(record.Msg, "metadata") {
			t.Errorf("the log says something of metadata, which is granted more than the set asks for: %+v", record)
		}
	}
}

// GitHub's answer may name an access this daemon does not know, today or after GitHub adds one,
// and an access above the set's is no reason to ask for more than the set names. A word outside
// read, write and admin is granted none: the permission is left out of the request and logged at
// warn, rather than passed back to GitHub, which would refuse the mint whole and leave the
// role no gh files. An admin grant is asked for at the write the set names, never at admin.
func TestGrantedAnAccessThisDoesNotKnowIsGrantedNone(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{
		owners: []string{"acme"},
		slug:   "legion-implementer",
		grants: map[string]string{"contents": "maintain", "metadata": "read", "statuses": "", "workflows": "admin"},
	}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	if _, err := tokens.Token(context.Background(), Implement, "acme"); err != nil {
		t.Fatalf("Token: %v", err)
	}

	asked := github.exchanges()
	if len(asked) != 1 {
		t.Fatalf("%d exchanges, want the one mint", len(asked))
	}
	want := map[string]string{"metadata": "read", "workflows": "write"}
	if !maps.Equal(asked[0].Permissions, want) {
		t.Errorf("the write request named permissions %v, want exactly %v", asked[0].Permissions, want)
	}
	line := "appauth: the implement App's installation on acme is granted contents: maintain, where its token asks for contents: write, so the token is minted without it; grant the App contents: write and approve it on that account"
	if count := shortfallLogged(loggedRecords(t, &stderr), line, Implement, "acme", "contents", "write", "maintain"); count != 1 {
		t.Errorf("the log holds %d warn records of %q with its fields, want exactly one:\n%s", count, line, stderr.String())
	}
}

// An account approves each of an App's grants, so an installation may be granted no Actions, no
// checks, no commit statuses and no attestations at all. The read mint leaves them out of the
// request and asks for the four it is granted in one exchange, so every owner's read token is
// served rather than the first such owner's mint failing. What the token cannot read is logged
// once at warn per App role, owner and permission: a second mint of the same scope logs nothing
// again, since the grant is the installation's.
func TestGrantedReadSetLeavesOutActionsAndSaysSoOncePerOwner(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{
		owners: []string{"acme", "sjawhar"},
		slug:   "legion-reviewer",
		grants: map[string]string{"contents": "read", "issues": "read", "metadata": "read", "pull_requests": "read"},
	}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		// GitHub's own answer to a request naming a permission the installation is not granted:
		// no token at all, rather than a token without that permission.
		for permission := range body.Permissions {
			if github.grants[permission] == "" {
				return http.StatusUnprocessableEntity, `{"message":"The permissions requested are not granted to this installation."}`
			}
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	// Each owner's read token is asked for on its own, which is what a controller's bundle does
	// for it (ControllerCredential): one mint per owner, on that owner's own installation.
	for _, owner := range []string{"acme", "sjawhar"} {
		lease, err := tokens.ReadToken(context.Background(), Review, owner)
		if err != nil || lease.Token == "" {
			t.Fatalf("read token for %s = %q, %v; want the lease the four granted reads mint", owner, lease.Token, err)
		}
	}
	// Forget drops acme's lease, so the next read token of that scope mints again.
	tokens.Forget(Review, "ACME")
	if _, err := tokens.ReadToken(context.Background(), Review, "acme"); err != nil {
		t.Fatalf("read token after Forget: %v", err)
	}

	asked := github.exchanges()
	if len(asked) != 3 {
		t.Fatalf("%d exchanges, want one for each of the three mints: a cut request is never asked twice", len(asked))
	}
	want := map[string]string{"contents": "read", "issues": "read", "metadata": "read", "pull_requests": "read"}
	for i, body := range asked {
		if !maps.Equal(body.Permissions, want) {
			t.Errorf("exchange %d named permissions %v, want exactly %v", i+1, body.Permissions, want)
		}
	}
	records := loggedRecords(t, &stderr)
	for _, owner := range []string{"acme", "sjawhar"} {
		for _, left := range []string{"actions", "attestations", "checks", "statuses"} {
			line := fmt.Sprintf("appauth: the review App's installation on %s is granted no %s, where its token asks for %s: read, so the token is minted without it; grant the App %s: read and approve it on that account", owner, left, left, left)
			if count := shortfallLogged(records, line, Review, owner, left, "read", ""); count != 1 {
				t.Errorf("the log holds %d warn records of %q with its fields, want exactly one:\n%s", count, line, stderr.String())
			}
		}
	}
	if len(records) != 8 {
		t.Errorf("the log holds %d records, want one for each of the two owners and each of the four permissions:\n%s", len(records), stderr.String())
	}
}

// GitHub reads a token request with no permissions as every permission the installation holds,
// so a cut that leaves nothing refuses the mint where it is made: nothing reaches the token
// endpoint, and the refusal names the role, the owner and the set it asked for.
func TestGrantedNoneOfTheSetRefusesTheMintBeforeTheTokenEndpoint(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{
		owners: []string{"acme"},
		slug:   "legion-implementer",
		grants: map[string]string{"members": "read", "repository_projects": "write"},
	}
	github.answer = func(int, accessTokensBody) (int, string) {
		t.Error("GitHub was asked for a token whose every permission the installation is granted none of")
		return http.StatusOK, ""
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	lease, err := tokens.Token(context.Background(), Implement, "acme")
	if lease.Token != "" || err == nil {
		t.Fatalf("a mint on an installation granted none of the set = %q, %v; want no lease and a refusal", lease.Token, err)
	}
	for _, named := range []string{"implement", "acme", "contents: write", "workflows: write"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal does not name %q: %v", named, err)
		}
	}
	if asked := github.exchanges(); len(asked) != 0 {
		t.Errorf("%d exchanges, want none: a body with no permissions mints every permission the installation holds", len(asked))
	}
}

// The permissions read is classified as every other read of GitHub here is: its 5xx is GitHub's
// trouble, which a boot waits out, and its 404 is the installation gone, which sends the mint
// down the re-discovery path — the installation is forgotten, looked for once more, and a mint
// that meets the 404 again answers ErrNotInstalled.
func TestGrantedPermissionsReadIsTransientOn5xxAndGoneOn404(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		transient bool
		gone      bool
		reads     int
	}{
		{name: "a 500 is GitHub's trouble", status: http.StatusInternalServerError, transient: true, reads: 1},
		{name: "a 404 is the installation gone", status: http.StatusNotFound, gone: true, reads: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-implementer"}
			github.granted = func(string, int) (int, string) { return tc.status, `{"message":"no"}` }
			github.answer = func(int, accessTokensBody) (int, string) {
				t.Error("GitHub was asked for a token after the permissions read failed")
				return http.StatusOK, ""
			}
			tokens := keyModeManager(t, github, &now, &stderr)

			lease, err := tokens.Token(context.Background(), Implement, "acme")
			var trouble *TransientError
			if lease.Token != "" {
				t.Fatalf("a mint whose permissions read failed = %q, want no lease", lease.Token)
			}
			if got := errors.As(err, &trouble); got != tc.transient {
				t.Errorf("the failure = %v, transient %v, want transient %v", err, got, tc.transient)
			}
			if got := errors.Is(err, ErrNotInstalled); got != tc.gone {
				t.Errorf("the failure = %v, not installed %v, want %v", err, got, tc.gone)
			}
			if reads := github.permissionReads(); len(reads) != tc.reads {
				t.Errorf("the installation was read %v, want %d: a 404 is looked for once more and a 500 is not", reads, tc.reads)
			}
			if asked := github.exchanges(); len(asked) != 0 {
				t.Errorf("%d exchanges, want none: no token is asked for before the grants are read", len(asked))
			}
		})
	}
}

// A suspension is read from the installation's own suspended_at, which the read before every
// exchange answers, and never from the words of GitHub's 403 to the token request. The account
// here suspended the installation after GitHub listed it, so discovery lists it as any other
// and only that read can find it. Its token request would be answered 403 in words that are not
// GitHub's own today and name no suspension (forbiddenAnswer), as a message GitHub rewords
// would be: a mint that read those words would take the suspension for a plain refusal, which
// fails every bundle of the App. The mint is refused as a suspended installation's instead, for
// the write lease and the read lease alike, which is the one refusal a bundle leaves the
// owner's directory out over (leaveOutReason), and it asks GitHub for no token.
func TestASuspensionIsReadFromTheInstallationAndNeverFromTheWordsOfA403(t *testing.T) {
	for _, tc := range []struct {
		name string
		mint func(*Manager) (Lease, error)
	}{
		{"the write lease", func(m *Manager) (Lease, error) { return m.Token(context.Background(), Implement, "pausedlabs") }},
		{"the read lease", func(m *Manager) (Lease, error) { return m.ReadToken(context.Background(), Review, "pausedlabs") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"pausedlabs"}, slug: "legion-implementer"}
			github.forbids = func(owner string) bool { return owner == "pausedlabs" }
			tokens := keyModeManager(t, github, &now, &stderr)

			lease, err := tc.mint(tokens)
			if lease.Token != "" || !errors.Is(err, errInstallationSuspended) {
				t.Fatalf("a mint on a suspended installation = %q, %v; want no lease and errInstallationSuspended, whatever the 403 says", lease.Token, err)
			}
			var trouble *TransientError
			if errors.As(err, &trouble) {
				t.Errorf("the refusal %v is GitHub's trouble, want an answer about the installation", err)
			}
			if reason := leaveOutReason(err); reason != "suspended" {
				t.Errorf("leaveOutReason = %q, want suspended: a bundle leaves this owner's directory out", reason)
			}
			if !strings.Contains(err.Error(), suspendedSince) {
				t.Errorf("the refusal does not name when the installation was suspended (%s): %v", suspendedSince, err)
			}
			if reads := github.permissionReads(); !slices.Equal(reads, []string{"101"}) {
				t.Errorf("the installation was read %v, want once: a suspended installation is not looked for again", reads)
			}
			if asked := github.exchanges(); len(asked) != 0 {
				t.Errorf("%d token requests, want none: the read says the installation is suspended", len(asked))
			}
		})
	}
}

// A 422 carrying GitHub's rate limit says nothing about what the installation is granted: it is
// GitHub's trouble (TransientError), no lease is served, and nothing is said of a grant. GitHub
// documents its limits on a 429 and a 403, so the three ways a 422 can still carry one are each
// checked. The mint asked for the whole read set, which this installation is granted, so the
// answer is about the limit alone.
func TestARateLimited422IsGitHubsTroubleAndNotAGrant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		body    string
	}{
		{"the remaining header", map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"Validation failed"}`},
		{"a retry-after", map[string]string{"Retry-After": "60"}, `{"message":"Validation failed"}`},
		{"the secondary limit in the body", nil, `{"message":"You have exceeded a secondary rate limit."}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, privatePEM := testAppKey(t)
			now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
			var mu sync.Mutex
			exchanges := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/app/installations":
					fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
				case r.URL.Path == "/app/installations/77":
					fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
				case strings.HasSuffix(r.URL.Path, "/access_tokens"):
					mu.Lock()
					exchanges++
					mu.Unlock()
					for name, value := range tc.headers {
						w.Header().Set(name, value)
					}
					w.WriteHeader(http.StatusUnprocessableEntity)
					fmt.Fprint(w, tc.body)
				default:
					t.Errorf("unexpected GitHub path %s", r.URL.Path)
				}
			}))
			defer server.Close()

			var stderr bytes.Buffer
			tokens := New(config.GitHubApps{Review: config.GitHubApp{AppID: "67890", PrivateKey: privatePEM}},
				Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
					Stderr: &stderr, Log: slog.New(slog.NewJSONHandler(&stderr, nil))})
			defer tokens.Close()

			lease, err := tokens.ReadToken(context.Background(), Review, "acme")
			var trouble *TransientError
			if lease.Token != "" || !errors.As(err, &trouble) {
				t.Fatalf("read token under a rate limit = %q, %v; want no lease and a TransientError", lease.Token, err)
			}
			if errors.Is(err, ErrNarrowingRefused) {
				t.Errorf("a rate limit = %v, want it never read as the permissions the installation holds", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if exchanges != 1 {
				t.Errorf("%d exchanges, want the one request a rate limit is answered to", exchanges)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing said of a grant the answer says nothing about", stderr.String())
			}
		})
	}
}

func TestTokensDeduplicatesConcurrentRequests(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	exchangeStarted := make(chan struct{})
	releaseExchange := make(chan struct{})
	var mu sync.Mutex
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwt := r.Header.Get("Authorization")
		if r.URL.Path == "/app/installations/77/access_tokens" {
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			_, asked := askedInExchange(t, r)
			mu.Lock()
			exchanges++
			mu.Unlock()
			close(exchangeStarted)
			<-releaseExchange
			fmt.Fprint(w, mintedFor("installation", now.Add(time.Hour), asked.Permissions, "legion"))
			return
		}
		switch r.URL.Path {
		case "/app":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			fmt.Fprint(w, `{"slug":"legion-implementer"}`)
		case "/app/installations/77":
			verifyAppJWT(t, jwt[len("Bearer "):], &key.PublicKey, now, "12345")
			fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
		case "/users/legion-implementer[bot]":
			fmt.Fprint(w, `{"id":100000001}`)
		default:
			t.Fatalf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM, Installations: map[string]string{"sjawhar": "77"}},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	results := make(chan Lease, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			lease, err := tokens.Token(context.Background(), Implement, "sjawhar")
			if err != nil {
				errs <- err
				return
			}
			results <- lease
		}()
	}
	<-exchangeStarted
	close(releaseExchange)
	first, second := <-results, <-results
	select {
	case err := <-errs:
		t.Fatalf("Token: %v", err)
	default:
	}
	if first.Token != "installation" || second.Token != first.Token {
		t.Fatalf("leases = %+v and %+v, want one installation token", first, second)
	}
	mu.Lock()
	defer mu.Unlock()
	if exchanges != 1 {
		t.Fatalf("token exchanges = %d, want 1", exchanges)
	}
}

// GitHub's trouble is transient and its answers are not: a request GitHub never answered or whose
// body stopped mid-read, a 5xx, or a rate limit (a 429, or a 403 that says it is one) may pass when
// it is made again, and a boot waits it out; any other status, a body that is not the JSON asked
// for, or an owner the App is not installed on is refused at once. The boot's retry reads nothing
// else.
func TestGitHubsTroubleIsTransientAndItsAnswersAreNot(t *testing.T) {
	_, privatePEM := testAppKey(t)
	status := func(code int, header ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			for i := 0; i+1 < len(header); i += 2 {
				w.Header().Set(header[i], header[i+1])
			}
			http.Error(w, "{}", code)
		}
	}
	// secondaryLimit is GitHub's secondary rate limit answered with its message alone.
	secondaryLimit := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4321")
		http.Error(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`, http.StatusForbidden)
	}
	installed := func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`) }
	for _, tc := range []struct {
		name      string
		discovery http.HandlerFunc
		exchange  http.HandlerFunc // nil answers with a token
		transient bool
	}{
		{name: "discovery 502", discovery: status(http.StatusBadGateway), transient: true},
		{name: "discovery 429", discovery: status(http.StatusTooManyRequests), transient: true},
		{name: "discovery 403 out of rate limit", discovery: status(http.StatusForbidden, "X-RateLimit-Remaining", "0"), transient: true},
		{name: "discovery 403 with retry-after", discovery: status(http.StatusForbidden, "Retry-After", "60"), transient: true},
		{name: "discovery 403", discovery: status(http.StatusForbidden, "X-RateLimit-Remaining", "4999")},
		{name: "discovery 403 for a secondary rate limit", discovery: secondaryLimit, transient: true},
		{name: "discovery 401", discovery: status(http.StatusUnauthorized)},
		{name: "discovery never answers", discovery: func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, transient: true},
		{name: "discovery body cut off mid-read", discovery: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			fmt.Fprint(w, `[{"id":77,`)
			w.(http.Flusher).Flush()
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
		}, transient: true},
		{name: "discovery body not JSON", discovery: func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `<html>`) }},
		{name: "not installed on the owner", discovery: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `[{"id":77,"account":{"login":"someone-else"}}]`)
		}},
		{name: "exchange 500", discovery: installed, exchange: status(http.StatusInternalServerError), transient: true},
		{name: "exchange 422", discovery: installed, exchange: status(http.StatusUnprocessableEntity)},
		{name: "exchange 403 for a secondary rate limit", discovery: installed, exchange: secondaryLimit, transient: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app/installations":
					tc.discovery(w, r)
				case "/app/installations/77":
					fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
				case "/app/installations/77/access_tokens":
					if tc.exchange != nil {
						tc.exchange(w, r)
						return
					}
					_, asked := askedInExchange(t, r)
					fmt.Fprint(w, mintedFor("installation-1", time.Now().Add(time.Hour), asked.Permissions, "legion"))
				default:
					t.Errorf("unexpected GitHub path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}},
				Options{BaseURL: server.URL, HTTPClient: server.Client()})
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			_, err := tokens.Token(ctx, Implement, "acme")
			if err == nil {
				t.Fatal("Token succeeded, want a failure")
			}
			var transient *TransientError
			if got := errors.As(err, &transient); got != tc.transient {
				t.Fatalf("Token = %v, transient %v, want transient %v", err, got, tc.transient)
			}
		})
	}
}

// Every GitHub call appauth makes keeps GitHub's answer where only the daemon's start reads it
// (dispatch://INFRA-2597/spec R4): each answer other than 2xx carries an *AnswerError holding the
// request's method and path, GitHub's status, body and rate-limit headers, inside the
// TransientError where one is made, and the error's message is the one it was before. The start
// reads it on its own calls, the App logins read and the key proof (internal/daemon's bootLimit);
// a mint's calls carry it too, though the start mints no token. The answer is a field and no link
// of the chain, so errors.As finds no *githubrest.Answer in any of these errors: every other
// reader of one (internal/daemon's defaultBranchLimit and the outbox's githubrest.RetryAfter among
// them) reads a mint's error as it did before.
//
// One row per call: the installation discovery, the permissions read and the exchange, each
// refused for a spent rate limit; the exchange's 422 that carries the limit's headers, which is
// GitHub's trouble and no rate limit githubrest names (RateLimited false); the logins' GET /app;
// the bot user; token mode's viewer, App and repository reads; key mode's installation read of a
// repository (InstalledOn); and the answers that are no rate limit, the 404s of the exchange and of
// the permissions read, and token mode's 401 of the repository list.
func TestEveryGitHubCallOfTheStartsMintKeepsGitHubsAnswer(t *testing.T) {
	_, privatePEM := testAppKey(t)
	reset := strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)
	spent := githubrest.LimitHeaders{Limit: "15000", Remaining: "0", Reset: reset, Resource: "core"}
	const limitBody = `{"message":"API rate limit exceeded for installation ID 77."}`
	widgets := ghrepo.MustParse("acme/widgets")
	keyMode := func(server *httptest.Server) *Manager {
		return New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}},
			Options{BaseURL: server.URL, HTTPClient: server.Client(), Stderr: io.Discard})
	}
	broker := newFakeBroker(t, []string{"acme"}, time.Now().Add(time.Hour))
	tokenMode := func(server *httptest.Server) *Manager {
		return New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", TokenCommand: broker.command}},
			Options{BaseURL: server.URL, HTTPClient: server.Client(), Stderr: io.Discard})
	}
	mint := func(tokens *Manager) error {
		_, err := tokens.Token(context.Background(), Implement, "acme")
		return err
	}
	login := func(tokens *Manager) error {
		_, err := tokens.AppLogin(context.Background(), Implement)
		return err
	}
	installedOn := func(tokens *Manager) error {
		_, err := tokens.InstalledOn(context.Background(), Implement, widgets)
		return err
	}
	for _, tc := range []struct {
		name      string
		manager   func(*httptest.Server) *Manager
		call      func(*Manager) error
		method    string
		path      string
		status    int
		limit     githubrest.LimitHeaders
		body      string
		message   string
		transient bool
	}{
		{name: "the installation discovery", manager: keyMode, call: mint, method: http.MethodGet, path: "/app/installations",
			status: http.StatusForbidden, limit: spent, body: limitBody, transient: true,
			message: "GitHub App installation discovery failed (403)"},
		{name: "the permissions read", manager: keyMode, call: mint, method: http.MethodGet, path: "/app/installations/77",
			status: http.StatusForbidden, limit: spent, body: limitBody, transient: true,
			message: "GitHub App installation permissions read failed (403): " + limitBody},
		{name: "the exchange", manager: keyMode, call: mint, method: http.MethodPost, path: "/app/installations/77/access_tokens",
			status: http.StatusForbidden, limit: spent, body: limitBody, transient: true,
			message: "GitHub App token exchange failed (403): " + limitBody},
		{name: "the exchange's 422 with the limit's headers", manager: keyMode, call: mint, method: http.MethodPost, path: "/app/installations/77/access_tokens",
			status: http.StatusUnprocessableEntity, limit: spent, body: limitBody, transient: true,
			message: "GitHub App token exchange failed (422): " + limitBody},
		{name: "the logins read", manager: keyMode, call: login, method: http.MethodGet, path: "/app",
			status: http.StatusForbidden, limit: spent, body: limitBody, transient: true,
			message: "read the implement App's login: GitHub App identity lookup failed (403)"},
		{name: "the bot user", manager: keyMode, call: mint, method: http.MethodGet, path: "/users/legion-implementer[bot]",
			status: http.StatusForbidden, limit: spent, body: limitBody, transient: true,
			message: "GitHub App bot identity lookup failed (403)"},
		{name: "token mode's viewer", manager: tokenMode, call: login, method: http.MethodPost, path: "/graphql",
			status: http.StatusForbidden, limit: spent, body: limitBody, transient: true,
			message: "read the implement App's login: read the implement App's bot login (GraphQL viewer) failed (403)"},
		{name: "token mode's App", manager: tokenMode, call: login, method: http.MethodGet, path: "/apps/legion-implementer",
			status: http.StatusForbidden, limit: spent, body: limitBody, transient: true,
			message: "read the implement App's login: read the implement App legion-implementer (GET /apps/legion-implementer) failed (403)"},
		{name: "token mode's repository list", manager: tokenMode, call: installedOn, method: http.MethodGet, path: "/installation/repositories",
			status: http.StatusForbidden, limit: spent, body: limitBody, transient: true,
			message: "read whether the implement App is installed on acme/widgets failed (403)"},
		{name: "key mode's installation read of a repository", manager: keyMode, call: installedOn, method: http.MethodGet, path: "/repos/acme/widgets/installation",
			status: http.StatusForbidden, limit: spent, body: limitBody, transient: true,
			message: "read whether the implement App is installed on acme/widgets failed (403)"},
		{name: "the exchange's 404", manager: keyMode, call: mint, method: http.MethodPost, path: "/app/installations/77/access_tokens",
			status: http.StatusNotFound, body: `{"message":"Not Found"}`,
			message: `github_app_not_installed: implement's installation 77 on acme answers the installation is gone: GitHub App token exchange failed (404): {"message":"Not Found"}`},
		{name: "the permissions read's 404", manager: keyMode, call: mint, method: http.MethodGet, path: "/app/installations/77",
			status: http.StatusNotFound, body: `{"message":"Not Found"}`,
			message: `github_app_not_installed: implement's installation 77 on acme answers the installation is gone: GitHub App installation permissions read failed (404): {"message":"Not Found"}`},
		{name: "token mode's 401 of the repository list", manager: tokenMode, call: installedOn, method: http.MethodGet, path: "/installation/repositories",
			status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`,
			message: "read whether the implement App is installed on acme/widgets failed (401)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				if r.Method == tc.method && r.URL.Path == tc.path {
					for header, value := range map[string]string{"X-RateLimit-Limit": tc.limit.Limit, "X-RateLimit-Remaining": tc.limit.Remaining,
						"X-RateLimit-Reset": tc.limit.Reset, "X-RateLimit-Resource": tc.limit.Resource} {
						if value != "" {
							w.Header().Set(header, value)
						}
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				switch {
				case r.URL.Path == "/app/installations":
					fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
				case r.URL.Path == "/app/installations/77":
					fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
				case r.URL.Path == "/app/installations/77/access_tokens":
					_, asked := askedInExchange(t, r)
					fmt.Fprint(w, mintedFor("ghs_installation", time.Now().Add(time.Hour), asked.Permissions, "legion"))
				case r.URL.Path == "/app":
					fmt.Fprint(w, `{"slug":"legion-implementer"}`)
				case r.URL.Path == "/users/legion-implementer[bot]":
					fmt.Fprint(w, `{"id":100000001}`)
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					fmt.Fprint(w, `{"data":{"viewer":{"login":"legion-implementer[bot]"}}}`)
				case r.URL.Path == "/apps/legion-implementer":
					fmt.Fprint(w, `{"id":12345,"slug":"legion-implementer"}`)
				case r.URL.Path == "/installation/repositories":
					fmt.Fprint(w, `{"total_count":1,"repositories":[{"full_name":"acme/widgets"}]}`)
				case r.URL.Path == "/repos/acme/widgets/installation":
					fmt.Fprint(w, `{"id":77}`)
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})

			err := tc.call(tc.manager(server))

			if err == nil || err.Error() != tc.message {
				t.Fatalf("the call = %v, want today's message %q", err, tc.message)
			}
			var trouble *TransientError
			if got := errors.As(err, &trouble); got != tc.transient {
				t.Fatalf("the call = %v, transient %v, want transient %v as before", err, got, tc.transient)
			}
			var hidden *githubrest.Answer
			if errors.As(err, &hidden) {
				t.Fatalf("errors.As found GitHub's answer %+v in %v, want it out of the chain, so no reader after the start reads it", hidden, err)
			}
			var kept *AnswerError
			if !errors.As(err, &kept) {
				t.Fatalf("the call = %v, want an *AnswerError holding GitHub's answer", err)
			}
			if tc.transient && !errors.As(trouble.Err, &kept) {
				t.Fatalf("the TransientError = %v, want the *AnswerError inside it", trouble)
			}
			answer := kept.Answer
			if answer == nil || answer.Method != tc.method || answer.Path != tc.path || answer.Status != tc.status || answer.Body != tc.body || answer.Limit != tc.limit {
				t.Fatalf("the answer kept = %+v, want %s %s answered %d with %s and headers %+v", answer, tc.method, tc.path, tc.status, tc.body, tc.limit)
			}
			if wantLimited := tc.status == http.StatusForbidden; answer.RateLimited != wantLimited {
				t.Errorf("the answer kept reads as a rate limit: %v, want %v, as githubrest reads it", answer.RateLimited, wantLimited)
			}
		})
	}
}

// keyModeGitHub is a stand-in GitHub for a mint the daemon signs its own JWT for: one
// installation for each owner it names, what each installation is granted (grants, nil for
// grantedEverythingAsked), the App's slug and bot user, and every access_tokens body, both as
// GitHub parsed it (bodies) and as the text it was sent as (raw), which is what a test asking
// whether a request named a repository must read. answer decides each exchange from the body it
// carried and how many came before it, and granted, when it is set, answers the permissions
// read of an installation instead of its grants.
//
// refuses, when it is set, is the owners whose every token exchange is answered 422 before
// answer is asked, as an installation that is granted none of what the request asks for is: a
// bundle holds one lease per owner and asks each installation for the same permissions
// (appauth's bundleDirs), so which owner the exchange was addressed to is the whole of what a
// refusal now turns on, and a stand-in that read the permissions asked for instead could not
// tell one owner's refusal from another's. The exchange is recorded before it is refused, as
// GitHub's own refusal of one is a request it answered.
//
// suspended is the owners whose installation GitHub lists with a suspended_at, every other one
// listed with it null, as GitHub lists each installation. forbids, when it is set, is the owners
// whose installation is suspended while it is listed as any other: an installation suspended
// between the listing and the mint, or one named in the configuration. The read of an
// installation of either kind answers the time it was suspended at (installationSuspended),
// before granted is asked, and its exchange is answered 403 (forbiddenAnswer), as GitHub refuses
// every token of a suspended installation.
// answers, when it is set, answers an owner's exchange itself where it reports that it did:
// GitHub's trouble and its rate limits, which a test sends one owner alone.
type keyModeGitHub struct {
	mu        sync.Mutex
	owners    []string
	slug      string
	grants    map[string]string
	bodies    []accessTokensBody
	raw       []string
	reads     []string
	answer    func(exchange int, body accessTokensBody) (int, string)
	granted   func(installation string, read int) (int, string)
	refuses   func(owner string) bool
	suspended map[string]bool
	forbids   func(owner string) bool
	answers   func(w http.ResponseWriter, owner string) bool
}

// forbiddenAnswer is the stand-in's 403 to a token request on a suspended installation. Its
// words are not the ones GitHub answers today ("This installation has been suspended") and
// name no suspension: GitHub may reword that message, so nothing a mint decides may rest on it,
// and a mint that read the message for a suspension would find none here.
const forbiddenAnswer = `{"message":"Access to this installation is on hold","documentation_url":"https://docs.github.com/rest/apps/apps#create-an-installation-access-token-for-an-app","status":"403"}`

// ownerOf is the account the installation id names, as /app/installations lists them.
func (g *keyModeGitHub) ownerOf(installation string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, owner := range g.owners {
		if installation == strconv.Itoa(101+i) {
			return strings.ToLower(owner)
		}
	}
	return ""
}

func (g *keyModeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations":
			g.mu.Lock()
			owners, suspended := slices.Clone(g.owners), g.suspended
			g.mu.Unlock()
			listed := make([]string, 0, len(owners))
			for i, owner := range owners {
				since := "null"
				if suspended[strings.ToLower(owner)] {
					since = `"` + suspendedSince + `"`
				}
				listed = append(listed, fmt.Sprintf(`{"id":%d,"account":{"login":%q},"suspended_at":%s}`, 101+i, owner, since))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(listed, ","))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			text, body := askedInExchange(t, r)
			g.mu.Lock()
			g.bodies = append(g.bodies, body)
			g.raw = append(g.raw, text)
			exchange, refuses, suspended, forbids, answers := len(g.bodies), g.refuses, g.suspended, g.forbids, g.answers
			g.mu.Unlock()
			owner := g.ownerOf(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens"))
			if answers != nil && answers(w, owner) {
				return
			}
			if suspended[owner] || (forbids != nil && forbids(owner)) {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, forbiddenAnswer)
				return
			}
			if refuses != nil && refuses(owner) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprint(w, refusedNarrowingAnswer)
				return
			}
			status, answer := g.answer(exchange, body)
			w.WriteHeader(status)
			fmt.Fprint(w, answer)
		case strings.HasPrefix(r.URL.Path, "/app/installations/"):
			installation := strings.TrimPrefix(r.URL.Path, "/app/installations/")
			g.mu.Lock()
			g.reads = append(g.reads, installation)
			read, grants, granted, suspended, forbids := len(g.reads), g.grants, g.granted, g.suspended, g.forbids
			g.mu.Unlock()
			if grants == nil {
				grants = grantedEverythingAsked
			}
			if owner := g.ownerOf(installation); suspended[owner] || (forbids != nil && forbids(owner)) {
				fmt.Fprint(w, installationSuspended(installation, grants))
				return
			}
			if granted != nil {
				status, answer := granted(installation, read)
				w.WriteHeader(status)
				fmt.Fprint(w, answer)
				return
			}
			fmt.Fprint(w, installationGranting(installation, grants))
		case r.URL.Path == "/app":
			fmt.Fprintf(w, `{"slug":%q}`, g.slug)
		case r.URL.Path == "/users/"+g.slug+"[bot]":
			fmt.Fprint(w, `{"id":100000001}`)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}})
}

// installOn gives the App an installation on owner, as an account installing it does: the next
// read of /app/installations lists it, and nothing else about the stand-in changes.
func (g *keyModeGitHub) installOn(owner string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.owners = append(g.owners, owner)
}

// exchanges is every access_tokens body GitHub was sent, in the order it got them.
func (g *keyModeGitHub) exchanges() []accessTokensBody {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.bodies)
}

// exchangeTexts is every access_tokens body GitHub was sent as the text it was sent as, in the
// order it got them: what a request named, rather than what the daemon's own struct holds of it.
func (g *keyModeGitHub) exchangeTexts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.raw)
}

// permissionReads is the installation each read of what an installation is granted named, in the
// order GitHub got them.
func (g *keyModeGitHub) permissionReads() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.reads)
}

// keyModeManager is a Manager holding a key for each App, minting against github, with its
// stderr and its log both kept in stderr rather than written to the test's own: the log as one
// JSON record a line (loggedRecords). A key-mode mint writes nothing to stderr itself, so what
// the buffer holds is what the Manager logged.
func keyModeManager(t *testing.T, github *keyModeGitHub, now *time.Time, stderr *bytes.Buffer) *Manager {
	t.Helper()
	_, privatePEM := testAppKey(t)
	server := github.serve(t)
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return *now },
		Stderr: stderr, Log: slog.New(slog.NewJSONHandler(stderr, nil))})
	t.Cleanup(tokens.Close)
	return tokens
}

// loggedRecord is one record a Manager logged, as keyModeManager's JSON handler writes it: its
// level and its message, and the fields a grant shortfall carries (sayShortfalls).
type loggedRecord struct {
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	App        string `json:"app"`
	Owner      string `json:"owner"`
	Permission string `json:"permission"`
	Asked      string `json:"asked"`
	Granted    string `json:"granted"`
}

// loggedRecords is every record said holds, in the order they were logged.
func loggedRecords(t *testing.T, said *bytes.Buffer) []loggedRecord {
	t.Helper()
	var records []loggedRecord
	for line := range strings.Lines(said.String()) {
		var record loggedRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the log holds %q, which is no JSON record: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// shortfallLogged is how many of records are the warn record a grant shortfall is logged as:
// message line, and the App, the owner, the permission, the access asked and the access granted
// (empty where none is) as its fields. A test writes line out in full, so the wording is held
// to the text an operator reads.
func shortfallLogged(records []loggedRecord, line string, app AppRole, owner, permission, asked, granted string) int {
	want := loggedRecord{Level: "WARN", Msg: line, App: string(app), Owner: owner, Permission: permission, Asked: asked, Granted: granted}
	count := 0
	for _, record := range records {
		if record == want {
			count++
		}
	}
	return count
}

// Each lease asks GitHub for exactly its set and names no repository at all: what the token
// reaches is what the account chose when it installed the App, and what the token may do there
// is these permissions. Each App's write lease asks for that App's own set, issues: write among
// it, since the implementer's code, and the tester running that code under the review App, post
// on an issue of the repository they work in, and attestations: read among it, since a role
// reads the attestations of an artifact. The review App's set also asks for statuses: read, since
// its roles read the commit statuses of a head and post none. Either App's read lease asks for
// the read set a controller and a provisioning token need of a repository they only read —
// contents, metadata, pull requests, issues, Actions, checks, commit statuses and attestations —
// each at read and nothing writable. The installation holds more, such as projects and packages,
// and a token that asks for none of it cannot be made to use them.
func TestEachLeaseAsksForItsSetIssuesIncluded(t *testing.T) {
	reads := map[string]string{
		"actions": "read", "attestations": "read", "checks": "read", "contents": "read", "issues": "read",
		"metadata": "read", "pull_requests": "read", "statuses": "read",
	}
	for _, tc := range []struct {
		name string
		role AppRole
		slug string
		read bool
		want map[string]string
	}{
		{"implement write", Implement, "legion-implementer", false, map[string]string{
			"actions": "write", "attestations": "read", "checks": "read", "contents": "write", "issues": "write",
			"metadata": "read", "pull_requests": "write", "security_events": "read", "statuses": "write",
			"workflows": "write",
		}},
		{"review write", Review, "legion-reviewer", false, map[string]string{
			"actions": "write", "attestations": "read", "checks": "write", "contents": "write", "issues": "write",
			"metadata": "read", "pull_requests": "write", "security_events": "write", "statuses": "read",
		}},
		{"implement read", Implement, "legion-implementer", true, reads},
		{"review read", Review, "legion-reviewer", true, reads},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"acme"}, slug: tc.slug}
			github.answer = func(exchange int, body accessTokensBody) (int, string) {
				return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
			}
			tokens := keyModeManager(t, github, &now, &stderr)

			var err error
			if tc.read {
				_, err = tokens.ReadToken(context.Background(), tc.role, "acme")
			} else {
				_, err = tokens.Token(context.Background(), tc.role, "acme")
			}
			if err != nil {
				t.Fatalf("the %s lease: %v", tc.name, err)
			}

			asked := github.exchanges()
			if len(asked) != 1 {
				t.Fatalf("%d exchanges, want the one mint", len(asked))
			}
			if !maps.Equal(asked[0].Permissions, tc.want) {
				t.Errorf("the %s request named permissions %v, want exactly %v", tc.name, asked[0].Permissions, tc.want)
			}
			if text := github.exchangeTexts()[0]; strings.Contains(text, "repositor") {
				t.Errorf("the request body is %s, want the permissions and no repository", text)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing: the installation is granted every permission the set asks for", stderr.String())
			}
		})
	}
}

// No request this daemon sends names a repository, for either App, for a write lease or a read
// one: the App's installation is the whole of what a token reaches (INFRA-2543). A body naming
// `repositories` or `repository_ids` would narrow the token GitHub mints, which is the narrowing
// INFRA-2543 took away, so every body sent is read for both, and for any key but the
// permissions.
func TestNoMintEverNamesARepositoryToGitHub(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"acme", "sjawhar"}, slug: "legion-implementer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	for _, role := range Roles {
		for _, owner := range []string{"acme", "sjawhar"} {
			if _, err := tokens.Token(context.Background(), role, owner); err != nil {
				t.Fatalf("the %s App's write mint for %s: %v", role, owner, err)
			}
			if _, err := tokens.ReadToken(context.Background(), role, owner); err != nil {
				t.Fatalf("the %s App's read mint for %s: %v", role, owner, err)
			}
		}
	}

	texts := github.exchangeTexts()
	if len(texts) != 8 {
		t.Fatalf("%d exchanges, want the two Apps' write and read mints for each of the two owners", len(texts))
	}
	for i, text := range texts {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &keys); err != nil {
			t.Errorf("exchange %d carried %s, which is no JSON object: %v", i+1, text, err)
			continue
		}
		if _, named := keys["permissions"]; !named || len(keys) != 1 {
			t.Errorf("exchange %d carried %s, want an object whose one key is permissions", i+1, text)
		}
		if strings.Contains(text, "repositories") || strings.Contains(text, "repository_ids") {
			t.Errorf("exchange %d carried %s, want a body that names no repository", i+1, text)
		}
	}
}

// GitHub's answer names the repositories the installation holds, which the account chose when it
// installed the App and which no request of this daemon's narrows: the answer is read for its
// permissions alone, so a selection of every repository, and repositories no request could have
// named, are served like any other lease and cached like any other lease.
func TestAnAnswerNamingTheInstallationsRepositoriesIsServed(t *testing.T) {
	expires := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC).Format(time.RFC3339)
	permissions := `{"actions":"write","checks":"read","contents":"write","metadata":"read","pull_requests":"write","statuses":"write","workflows":"write"}`
	for _, tc := range []struct {
		name   string
		answer string
	}{
		{"a selection of every repository", `{"token":"ghs_all","expires_at":"` + expires +
			`","repository_selection":"all","repositories":[],"permissions":` + permissions + `}`},
		{"repositories no request named", `{"token":"ghs_all","expires_at":"` + expires +
			`","repository_selection":"selected","repositories":[{"name":"legion"},{"name":"secrets"}],"permissions":` + permissions + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"sjawhar"}, slug: "legion-implementer"}
			github.answer = func(int, accessTokensBody) (int, string) { return http.StatusOK, tc.answer }
			tokens := keyModeManager(t, github, &now, &stderr)

			lease, err := tokens.Token(context.Background(), Implement, "sjawhar")
			if err != nil || lease.Token != "ghs_all" {
				t.Fatalf("the lease = %q, %v; want the token GitHub minted", lease.Token, err)
			}
			if _, err := tokens.Token(context.Background(), Implement, "sjawhar"); err != nil {
				t.Fatalf("the second call: %v", err)
			}
			if asked := github.exchanges(); len(asked) != 1 {
				t.Errorf("%d exchanges, want the second call served from the cache", len(asked))
			}
		})
	}
}

// GitHub names in its answer the permissions it minted the token with, and the mint holds that
// answer against the request: a permission the request did not ask for, an access above the one
// it asked at, or a word this daemon cannot order is a token wider than the one asked for, and
// is refused rather than served. The refusal names what was asked and the permission the answer
// named, never the token.
func TestTokenRefusesATokenWiderThanThePermissionsAsked(t *testing.T) {
	expires := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC).Format(time.RFC3339)
	for _, tc := range []struct {
		name        string
		permissions string
		named       string
	}{
		{"a permission the request did not ask for", `{"administration":"write","contents":"read","metadata":"read","pull_requests":"read","actions":"read"}`, "administration: write"},
		{"an access above the one asked for", `{"contents":"write","metadata":"read","pull_requests":"read","actions":"read"}`, "contents: write"},
		{"an access this daemon cannot order", `{"contents":"maintain","metadata":"read","pull_requests":"read","actions":"read"}`, "contents: maintain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-reviewer"}
			github.answer = func(int, accessTokensBody) (int, string) {
				return http.StatusOK, `{"token":"ghs_wide","expires_at":"` + expires +
					`","repository_selection":"selected","repositories":[{"name":"legion"}],"permissions":` + tc.permissions + `}`
			}
			tokens := keyModeManager(t, github, &now, &stderr)

			lease, err := tokens.ReadToken(context.Background(), Review, "acme")
			if err == nil || lease.Token != "" {
				t.Fatalf("a token wider than the permissions asked = %q, %v; want no lease and a refusal", lease.Token, err)
			}
			if strings.Contains(err.Error(), "ghs_wide") {
				t.Errorf("the refusal carries the token it refused: %v", err)
			}
			if !strings.Contains(err.Error(), tc.named) {
				t.Errorf("the refusal does not name %q, what the answer held: %v", tc.named, err)
			}
			if !strings.Contains(err.Error(), "contents: read") {
				t.Errorf("the refusal does not name the permissions asked for: %v", err)
			}
		})
	}
}

// An answer naming no permission at all is refused for the reason the whole hold exists: a token
// GitHub describes as holding nothing holds whatever the installation is granted, and nothing in
// the answer says otherwise. The mint refuses what it cannot hold to the request rather than
// serving it, and the refusal carries no token.
func TestTokenRefusesATokenWhoseAnswerNamesNoPermissions(t *testing.T) {
	expires := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC).Format(time.RFC3339)
	for _, tc := range []struct {
		name   string
		answer string
	}{
		{"no permissions object at all", `{"token":"ghs_wide","expires_at":"` + expires +
			`","repository_selection":"selected","repositories":[{"name":"legion"}]}`},
		{"an empty permissions object", `{"token":"ghs_wide","expires_at":"` + expires +
			`","repository_selection":"selected","repositories":[{"name":"legion"}],"permissions":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-reviewer"}
			github.answer = func(int, accessTokensBody) (int, string) { return http.StatusOK, tc.answer }
			tokens := keyModeManager(t, github, &now, &stderr)

			lease, err := tokens.ReadToken(context.Background(), Review, "acme")
			if err == nil || lease.Token != "" {
				t.Fatalf("a token whose answer names no permission = %q, %v; want no lease and a refusal", lease.Token, err)
			}
			if strings.Contains(err.Error(), "ghs_wide") {
				t.Errorf("the refusal carries the token it refused: %v", err)
			}
			if !strings.Contains(err.Error(), "contents: read") {
				t.Errorf("the refusal does not name the permissions asked for: %v", err)
			}
		})
	}
}

// A mint names the account it mints on, and nothing else says which installation GitHub is to
// use: a call that names no account would hand its caller a token for an account it never asked
// about. So every door into a mint refuses a call that names none, and refuses it before any
// exchange is sent.
func TestNoMintWithoutAnOwnerEverReachesGitHub(t *testing.T) {
	for _, tc := range []struct {
		name string
		mint func(tokens *Manager) error
	}{
		{"a write token for no owner", func(tokens *Manager) error {
			_, err := tokens.Token(context.Background(), Implement, "")
			return err
		}},
		{"a read token for no owner", func(tokens *Manager) error {
			_, err := tokens.ReadToken(context.Background(), Implement, "")
			return err
		}},
		{"a write token for an owner of blanks", func(tokens *Manager) error {
			_, err := tokens.Token(context.Background(), Implement, "   ")
			return err
		}},
		{"a read token for an owner of blanks", func(tokens *Manager) error {
			_, err := tokens.ReadToken(context.Background(), Implement, " ")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"sjawhar"}, slug: "legion-implementer"}
			github.answer = func(int, accessTokensBody) (int, string) {
				return http.StatusOK, mintedFor("ghs_wide", now.Add(time.Hour), nil)
			}
			tokens := keyModeManager(t, github, &now, &stderr)

			if err := tc.mint(tokens); err == nil {
				t.Fatal("the mint succeeded, want a call naming no account refused")
			}
			if asked := github.exchanges(); len(asked) != 0 {
				t.Errorf("%d exchanges, want none: a mint with no account has no installation to ask", len(asked))
			}
		})
	}
}

// GitHub answers 422 to a permission the installation is not granted and mints nothing rather
// than leaving that one out, so a write mint reports the refusal naming the permissions it asked
// for and an operator reads which grant is missing. A mint asks for what the installation was
// granted a moment before (exchangeGranted), so this is a grant that changed between the two
// calls; the write lease is never softened, since a token minted without a permission of the set
// would take what it covers — `gh run rerun` for actions — from every role in silence.
func TestA422ForAPermissionTheInstallationIsNotGrantedIsTheNarrowingRefusal(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"sjawhar"}, slug: "legion-implementer"}
	github.answer = func(int, accessTokensBody) (int, string) {
		return http.StatusUnprocessableEntity, `{"message":"The permissions requested are not granted to this installation."}`
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	lease, err := tokens.Token(context.Background(), Implement, "sjawhar")
	var trouble *TransientError
	if lease.Token != "" || !errors.Is(err, ErrNarrowingRefused) || errors.As(err, &trouble) {
		t.Fatalf("a 422 to a write mint = %q, %v; want no lease and the narrowing refusal", lease.Token, err)
	}
	for _, permission := range []string{"actions: write", "checks: read", "contents: write", "metadata: read", "pull_requests: write", "statuses: write", "workflows: write"} {
		if !strings.Contains(err.Error(), permission) {
			t.Errorf("the refusal does not name %q, what the request carried: %v", permission, err)
		}
	}
	if asked := github.exchanges(); len(asked) != 1 {
		t.Errorf("%d exchanges, want the one: a refused write mint is never asked again", len(asked))
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing: a 422 says nothing of a grant, which the mint read before it asked", stderr.String())
	}
}

// The read lease and the write lease of one role and owner are two leases, and another owner's
// are two more: each is minted and served under its own scope, and none is ever served to the
// caller that asked for another, since a reading token is no token to push with and no account's
// token acts on another account. Forget knows the owner alone, and drops both of that owner's
// leases and no other owner's.
func TestTheReadAndWriteScopesOfEachOwnerAreCachedApart(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"acme", "sjawhar"}, slug: "legion-reviewer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	read := func(owner string) string {
		t.Helper()
		lease, err := tokens.ReadToken(context.Background(), Review, owner)
		if err != nil {
			t.Fatalf("ReadToken for %s: %v", owner, err)
		}
		return lease.Token
	}
	write := func() string {
		t.Helper()
		lease, err := tokens.Token(context.Background(), Review, "acme")
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		return lease.Token
	}

	acme, sjawhar, written := read("acme"), read("sjawhar"), write()
	if acme == sjawhar || acme == written || sjawhar == written {
		t.Fatalf("acme=%q sjawhar=%q write=%q, want three leases of their own", acme, sjawhar, written)
	}
	if read("acme") != acme || read("sjawhar") != sjawhar || write() != written {
		t.Error("a second call minted again where its own scope's lease was cached")
	}
	if asked := github.exchanges(); len(asked) != 3 {
		t.Fatalf("%d exchanges, want one for each of the three scopes", len(asked))
	}

	tokens.Forget(Review, "ACME")
	if read("acme") == acme || write() == written {
		t.Error("Forget left a lease of this owner cached")
	}
	if read("sjawhar") != sjawhar {
		t.Error("Forget dropped another owner's lease")
	}
	if asked := github.exchanges(); len(asked) != 5 {
		t.Errorf("%d exchanges, want this owner's two scopes minted again and the other owner's served from its lease", len(asked))
	}
}

// A narrowing GitHub refuses is its definitive answer about what the account grants the App's
// installation, so the Manager remembers that refusal per scope and serves it from there while
// narrowingRefusalWindow holds: the second mint of the refused scope answers the same refusal
// and GitHub is asked nothing — no exchange, and not even the grant read that precedes one —
// where every refresh of every role of every live pod would otherwise ask again, and a rate
// limit on those asks would fail every bundle of the project. Past the window the next mint asks
// GitHub once more. A scope that answers is remembered not at all: its own lease is what the
// callers after it are served.
func TestANarrowingRefusalIsServedFromMemoryInsideItsWindow(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-reviewer"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		// The read lease's scope is the refused one: its request asks for contents at read,
		// where the write lease of the same owner asks for it at write.
		if body.Permissions["contents"] == "read" {
			return http.StatusUnprocessableEntity, `{"message":"The permissions requested are not granted to this installation."}`
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)
	refused := func(which string) error {
		t.Helper()
		lease, err := tokens.ReadToken(context.Background(), Review, "acme")
		if !errors.Is(err, ErrNarrowingRefused) || lease.Token != "" {
			t.Fatalf("the %s mint of the refused scope = %q, %v; want no lease and ErrNarrowingRefused", which, lease.Token, err)
		}
		return err
	}

	first := refused("first")
	if asked := github.exchanges(); len(asked) != 1 {
		t.Fatalf("%d exchanges, want the one GitHub refused", len(asked))
	}
	now = now.Add(narrowingRefusalWindow - time.Minute)
	if again := refused("second"); again.Error() != first.Error() {
		t.Errorf("the second mint answered %v, want the refusal GitHub gave: %v", again, first)
	}
	if asked := github.exchanges(); len(asked) != 1 {
		t.Errorf("%d exchanges, want the refusal served from the Manager's own memory inside the window", len(asked))
	}
	if reads := github.permissionReads(); len(reads) != 1 {
		t.Errorf("%d grant reads, want the one the refused exchange made: a remembered refusal reads nothing", len(reads))
	}

	now = now.Add(2 * time.Minute)
	refused("third")
	if asked := github.exchanges(); len(asked) != 2 {
		t.Errorf("%d exchanges, want GitHub asked again once the window had passed", len(asked))
	}

	for i := range 2 {
		lease, err := tokens.Token(context.Background(), Review, "acme")
		if err != nil || lease.Token == "" {
			t.Fatalf("mint %d of the scope the installation grants = %q, %v", i+1, lease.Token, err)
		}
	}
	if asked := github.exchanges(); len(asked) != 3 {
		t.Errorf("%d exchanges, want one more: a scope that answers is remembered nowhere and served from its lease", len(asked))
	}
}

// GitHub's answer to a token exchange lists one object for every repository the installation
// holds, and a Legion installation holds hundreds, so the bound a mint reads its answer under is
// 16 MiB (exchangeAnswerBound): about 3,000 repositories at the 4.9 to 5.3 KB one object
// measures. An answer of 3,000 is read whole and its lease served; one past the bound is refused
// naming the bound, and the refusal carries neither the token nor anything else the body held.
func TestAnAnswerOfThreeThousandRepositoriesIsReadAndOnePastTheBoundIsRefused(t *testing.T) {
	expires := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC).Format(time.RFC3339)
	// Each repository object is of the size GitHub's answer carries, the padding standing for
	// the fields a repository object holds that this daemon never reads.
	answerOf := func(repositories int) string {
		var answer strings.Builder
		fmt.Fprintf(&answer, `{"token":"ghs_many","expires_at":%q,"repository_selection":"selected","permissions":{"actions":"read","contents":"read","metadata":"read","pull_requests":"read"},"repositories":[`, expires)
		for i := range repositories {
			if i > 0 {
				answer.WriteByte(',')
			}
			fmt.Fprintf(&answer, `{"id":%d,"name":"repo-%04d","full_name":"acme/repo-%04d","description":%q}`,
				i, i, i, strings.Repeat("x", 4900))
		}
		answer.WriteString("]}")
		return answer.String()
	}
	for _, tc := range []struct {
		name         string
		repositories int
		served       bool
	}{
		{"three thousand repositories", 3000, true},
		{"an answer past the bound", 3600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := answerOf(tc.repositories)
			if past := len(answer) > exchangeAnswerBound; past == tc.served {
				t.Fatalf("the answer of %d repositories is %d bytes against a bound of %d", tc.repositories, len(answer), exchangeAnswerBound)
			}
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-reviewer"}
			github.answer = func(int, accessTokensBody) (int, string) { return http.StatusOK, answer }
			tokens := keyModeManager(t, github, &now, &stderr)

			lease, err := tokens.ReadToken(context.Background(), Review, "acme")
			if tc.served {
				if err != nil || lease.Token != "ghs_many" {
					t.Fatalf("the lease of an answer of %d repositories = %q, %v; want the token GitHub minted", tc.repositories, lease.Token, err)
				}
				return
			}
			if err == nil || lease.Token != "" {
				t.Fatalf("an answer past the bound = %q, %v; want no lease and a refusal", lease.Token, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(exchangeAnswerBound)) {
				t.Errorf("the refusal does not name the bound it met: %v", err)
			}
			if strings.Contains(err.Error(), "ghs_many") || strings.Contains(err.Error(), "repo-0000") {
				t.Errorf("the refusal carries what the body held: %v", err)
			}
		})
	}
}

// Two reads of one App's installations that overlap in time make one read of GitHub, as two
// reads of one App's identity do: every gh bundle opens with this read, and the bundles of a
// project's pods are built side by side, so an overlapping pair asking GitHub the same question
// twice costs a page walk under a fresh JWT for nothing. It is no cache: a read made once the
// first has ended reads GitHub again, which is what a bundle's directories must rest on
// (INFRA-2543 R2).
func TestOverlappingInstallationReadsOfOneAppShareOneRead(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	var reads atomic.Int64
	reading, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
			return
		}
		if reads.Add(1) == 1 {
			close(reading)
			<-release
		}
		fmt.Fprint(w, `[{"id":77,"account":{"login":"Acme"}}]`)
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	answers := make(chan []string, 2)
	read := func() {
		owners, err := tokens.InstalledOwners(context.Background(), Implement)
		if err != nil {
			t.Errorf("InstalledOwners: %v", err)
		}
		answers <- owners
	}
	go read()
	<-reading
	// The second caller joins while the first is still inside GitHub's answer. Were it not
	// sharing that read, its own request would reach the handler here and count a second read,
	// since the first is held until release is closed.
	go read()
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range 2 {
		if owners := <-answers; !slices.Equal(owners, []string{"acme"}) {
			t.Fatalf("the owners read = %v, want acme", owners)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("two overlapping reads asked GitHub %d times, want once", got)
	}

	// Nothing is kept: the next read, made once the first has ended, is GitHub's own again.
	if _, err := tokens.InstalledOwners(context.Background(), Implement); err != nil {
		t.Fatalf("the read after the shared one: %v", err)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("the read after the shared one left GitHub asked %d times, want it read again", got)
	}
}

// The installation read the overlapping callers of one App share belongs to none of them: the
// first caller's context ending ends that caller's own wait and not the read, so a caller that
// joined the read — a launch beside an API request whose client went away — is answered with the
// owners GitHub lists rather than with `context canceled`, and GitHub is asked once
// (InstalledOwners, readSharedInstallations).
func TestTheSharedInstallationReadOutlivesItsFirstCallersCancellation(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var reads atomic.Int64
	reading, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
			return
		}
		if reads.Add(1) == 1 {
			close(reading)
		}
		// GitHub answers once release is closed, unless the request that asked was cancelled
		// first, which is what the read sharing the first caller's context made of it.
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `[{"id":77,"account":{"login":"Acme"},"suspended_at":null},{"id":78,"account":{"login":"sjawhar"},"suspended_at":null}]`)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	defer tokens.Close()

	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstErr := make(chan error, 1)
	go func() {
		_, err := tokens.InstalledOwners(first, Implement)
		firstErr <- err
	}()
	select {
	case <-reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the first caller never asked GitHub")
	}
	type answer struct {
		owners []string
		err    error
	}
	second := make(chan answer, 1)
	go func() {
		owners, err := tokens.InstalledOwners(context.Background(), Implement)
		second <- answer{owners, err}
	}()
	// The second caller joins the read the first started while GitHub holds its answer.
	time.Sleep(50 * time.Millisecond)

	cancelFirst()
	select {
	case err := <-firstErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled first caller = %v, want its own context's end", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled first caller is still waiting")
	}
	close(release)
	select {
	case got := <-second:
		if got.err != nil || !slices.Equal(got.owners, []string{"acme", "sjawhar"}) {
			t.Fatalf("the caller that joined the read = %v, %v; want acme and sjawhar: the first caller's cancellation is not the read's", got.owners, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the caller that joined the read was never answered")
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("GitHub was asked %d times, want once: the second caller shares the first's read", got)
	}
}

// A fresh installation list supersedes leases of an older installation on the same account,
// for both roles of a shared App and both permission scopes. An unchanged account keeps its lease.
func TestListInstallationsDropsLeasesOfAReplacedInstallation(t *testing.T) {
	now := time.Now()
	_, key := testAppKey(t)
	github := &keyModeGitHub{owners: []string{"acme", "globex"}, slug: "example-app"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "widgets")
	}
	server := github.serve(t)
	app := config.GitHubApp{AppID: "12345", PrivateKey: key}
	tokens := New(config.GitHubApps{Implement: app, Review: app}, Options{BaseURL: server.URL, HTTPClient: server.Client()})
	defer tokens.Close()
	ctx := context.Background()
	read := func(role AppRole, owner string, narrow bool) Lease {
		t.Helper()
		mint := tokens.Token
		if narrow {
			mint = tokens.ReadToken
		}
		lease, err := mint(ctx, role, owner)
		if err != nil {
			t.Fatalf("mint %s's token on %s: %v", role, owner, err)
		}
		return lease
	}
	for _, role := range Roles {
		for _, narrow := range []bool{false, true} {
			if lease := read(role, "acme", narrow); lease.Installation != "101" {
				t.Fatalf("the original lease names installation %q, want 101", lease.Installation)
			}
		}
	}
	unchanged := read(Implement, "globex", true)
	github.mu.Lock()
	github.owners = []string{"newlabs", "globex", "acme"}
	github.mu.Unlock()
	listed, err := tokens.ListInstallations(ctx, Implement)
	if err != nil || len(listed.Installations) != 3 {
		t.Fatalf("the fresh installation list = %+v, %v, want three installations", listed, err)
	}
	for _, role := range Roles {
		for _, narrow := range []bool{false, true} {
			if lease := read(role, "acme", narrow); lease.Installation != "103" {
				t.Errorf("the %s lease after reinstall names installation %q, want 103", role, lease.Installation)
			}
		}
	}
	if lease := read(Implement, "globex", true); lease.Token != unchanged.Token {
		t.Error("the list discarded the unchanged account's lease")
	}
}

// An exchange started before a fresh list must not publish its old installation's lease after
// the list has replaced that installation. The same mint call instead reaches the new one.
func TestAnInflightMintCannotRestoreAReplacedInstallation(t *testing.T) {
	now := time.Now()
	started, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	finish := func() { released.Do(func() { close(release) }) }
	github := &keyModeGitHub{owners: []string{"acme"}, slug: "example-app"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		if exchange == 1 {
			close(started)
			<-release
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "widgets")
	}
	var stderr bytes.Buffer
	tokens := keyModeManager(t, github, &now, &stderr)
	t.Cleanup(finish)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type answer struct {
		lease Lease
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		lease, err := tokens.ReadToken(ctx, Implement, "acme")
		done <- answer{lease, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the original installation's exchange did not start")
	}
	github.mu.Lock()
	github.owners = []string{"newlabs", "acme"}
	github.mu.Unlock()
	if _, err := tokens.ListInstallations(ctx, Implement); err != nil {
		t.Fatalf("list the new installation while the old mint is held: %v", err)
	}
	finish()
	select {
	case got := <-done:
		if got.err != nil || got.lease.Installation != "102" {
			t.Fatalf("the mint returned installation %q, %v, want the new installation 102", got.lease.Installation, got.err)
		}
	case <-ctx.Done():
		t.Fatal("the mint did not finish after the original exchange was released")
	}
}
