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
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
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
	"actions": "write", "checks": "write", "contents": "write", "metadata": "read",
	"pull_requests": "write", "security_events": "write", "statuses": "write", "workflows": "write",
}

// installationGranting is GitHub's GET /app/installations/{id}: the installation and the
// permissions its account granted the App, which a mint reads before each exchange and cuts its
// request down to (exchangeGranted).
func installationGranting(id string, permissions map[string]string) string {
	// A map of strings always marshals, and json.Marshal sorts its keys.
	encoded, _ := json.Marshal(permissions)
	return fmt.Sprintf(`{"id":%s,"permissions":%s}`, id, encoded)
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
			fmt.Fprint(w, `{"id":271566630}`)
		default:
			t.Fatalf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	first, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if first.Token != "installation-1" || first.Identity.Name != "legion-implementer[bot]" || first.Identity.Email != "271566630+legion-implementer[bot]@users.noreply.github.com" {
		t.Fatalf("first lease = %+v", first)
	}
	if _, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("SJAWHAR/legion")); err != nil {
		t.Fatalf("cached token: %v", err)
	}

	// The hour-long lease has fewer than refreshWindow's twenty minutes left: the next call mints.
	now = now.Add(40*time.Minute + time.Second)
	refreshed, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion"))
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
			fmt.Fprint(w, `{"id":271566631}`)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	if _, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/legion")); err != nil {
		t.Fatalf("first token: %v", err)
	}

	mu.Lock()
	installation = ""
	mu.Unlock()
	now = now.Add(time.Hour)
	_, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/legion"))
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("a mint after the App was removed = %v, want ErrNotInstalled", err)
	}

	mu.Lock()
	installation = "88"
	mu.Unlock()
	lease, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/legion"))
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
			fmt.Fprint(w, `{"id":271566631}`)
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
		lease, err := tokens.Token(ctx, Review, ghrepo.MustParse("acme/legion"))
		if read {
			lease, err = tokens.ReadToken(ctx, Review, "acme", []string{"legion"})
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
			fmt.Fprint(w, `{"id":271566630}`)
		default:
			t.Fatalf("unexpected GitHub path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	read, err := tokens.ReadToken(context.Background(), Implement, "acme", []string{"legion"})
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	write, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("Acme/legion"))
	if err != nil {
		t.Fatalf("write token: %v", err)
	}
	again, err := tokens.ReadToken(context.Background(), Implement, "ACME", []string{"Legion"})
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
	// nothing else — the four read ones a reading lease carries, actions among them and nothing
	// writable, and the implement App's write set.
	want := []string{
		`{"permissions":{"actions":"read","contents":"read","metadata":"read","pull_requests":"read"}}`,
		`{"permissions":{"actions":"write","checks":"read","contents":"write","metadata":"read","pull_requests":"write","security_events":"read","statuses":"write","workflows":"write"}}`,
	}
	if len(bodies) != len(want) || bodies[0] != want[0] || bodies[1] != want[1] {
		t.Fatalf("exchange bodies = %q, want %q", bodies, want)
	}
}

// A write mint asks for what the installation is granted of the role's set and leaves the rest
// out: an account that has approved three of the implement App's seven permissions gets a token
// holding those three, in one exchange, where asking for all seven would make GitHub refuse the
// mint whole and cost the role its gh bundle.
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

	lease, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("cybermonklabs/legion"))
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
	for _, left := range []string{"actions", "pull_requests", "statuses", "workflows"} {
		if _, named := asked[0].Permissions[left]; named {
			t.Errorf("the request named %s, which the installation is granted none of", left)
		}
		line := fmt.Sprintf("appauth: the implement App's installation on cybermonklabs is granted no %s, where its token asks for %s: write, so the token is minted without it; grant the App %s: write and approve it on that account", left, left, left)
		if count := strings.Count(stderr.String(), line); count != 1 {
			t.Errorf("stderr holds %d of %q, want exactly one:\n%s", count, line, stderr.String())
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

	if _, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/legion")); err != nil {
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
	for _, permission := range []string{"actions", "contents"} {
		line := fmt.Sprintf("appauth: the review App's installation on acme is granted %s: read, where its token asks for %s: write, so the token is minted with %s: read; grant the App %s: write and approve it on that account", permission, permission, permission, permission)
		if count := strings.Count(stderr.String(), line); count != 1 {
			t.Errorf("stderr holds %d of %q, want exactly one:\n%s", count, line, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "metadata") {
		t.Errorf("stderr says something of metadata, which is granted more than the set asks for:\n%s", stderr.String())
	}
}

// GitHub's answer may name an access this daemon does not know, today or after GitHub adds one,
// and an access above the set's is no reason to ask for more than the set names. A word outside
// read, write and admin is granted none: the permission is left out of the request and said on
// stderr, rather than passed back to GitHub, which would refuse the mint whole and leave the
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

	if _, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("acme/legion")); err != nil {
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
	if count := strings.Count(stderr.String(), line); count != 1 {
		t.Errorf("stderr holds %d of %q, want exactly one:\n%s", count, line, stderr.String())
	}
}

// An account approves each of an App's grants, so an installation may be granted no actions at
// all. The read mint leaves actions out of the request and asks for the other three in one
// exchange, so every owner's read token is served rather than the first such owner's mint
// failing. What the token cannot read is said once per App role and owner: a second mint of the
// same scope says nothing again, since the grant is the installation's.
func TestGrantedReadSetLeavesOutActionsAndSaysSoOncePerOwner(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	github := &keyModeGitHub{
		owners: []string{"acme", "sjawhar"},
		slug:   "legion-reviewer",
		grants: map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"},
	}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		if body.Permissions["actions"] != "" {
			return http.StatusUnprocessableEntity, `{"message":"The permissions requested are not granted to this installation."}`
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	// Each owner's read token is asked for on its own, which is what a controller's bundle does
	// for it (ControllerCredential): one mint per owner, on that owner's own installation.
	for _, owner := range []string{"acme", "sjawhar"} {
		lease, err := tokens.ReadToken(context.Background(), Review, owner, []string{"legion"})
		if err != nil || lease.Token == "" {
			t.Fatalf("read token for %s = %q, %v; want the lease the three granted reads mint", owner, lease.Token, err)
		}
	}
	// Forget drops acme's lease, so the next read token of that scope mints again.
	tokens.Forget(Review, "ACME")
	if _, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"legion"}); err != nil {
		t.Fatalf("read token after Forget: %v", err)
	}

	asked := github.exchanges()
	if len(asked) != 3 {
		t.Fatalf("%d exchanges, want one for each of the three mints: a cut request is never asked twice", len(asked))
	}
	want := map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}
	for i, body := range asked {
		if !maps.Equal(body.Permissions, want) {
			t.Errorf("exchange %d named permissions %v, want exactly %v", i+1, body.Permissions, want)
		}
	}
	for _, owner := range []string{"acme", "sjawhar"} {
		line := fmt.Sprintf("appauth: the review App's installation on %s is granted no actions, where its token asks for actions: read, so the token is minted without it; grant the App actions: read and approve it on that account", owner)
		if count := strings.Count(stderr.String(), line); count != 1 {
			t.Errorf("stderr holds %d of %q, want exactly one:\n%s", count, line, stderr.String())
		}
	}
	if lines := strings.Count(stderr.String(), "\n"); lines != 2 {
		t.Errorf("stderr holds %d lines, want one for each of the two owners:\n%s", lines, stderr.String())
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
		grants: map[string]string{"issues": "write", "members": "read"},
	}
	github.answer = func(int, accessTokensBody) (int, string) {
		t.Error("GitHub was asked for a token whose every permission the installation is granted none of")
		return http.StatusOK, ""
	}
	tokens := keyModeManager(t, github, &now, &stderr)

	lease, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("acme/legion"))
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

			lease, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("acme/legion"))
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

// A 422 carrying GitHub's rate limit says nothing about what the installation is granted: it is
// GitHub's trouble (TransientError), no lease is served, and nothing is said of a grant. GitHub
// documents its limits on a 429 and a 403, so the three ways a 422 can still carry one are each
// checked. The mint asked for the four reads whole, which this installation is granted, so the
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
				Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }, Stderr: &stderr})
			defer tokens.Close()

			lease, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"legion"})
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
			fmt.Fprint(w, `{"id":271566630}`)
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
			lease, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion"))
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

			_, err := tokens.Token(ctx, Implement, ghrepo.MustParse("acme/legion"))
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

// keyModeGitHub is a stand-in GitHub for a mint the daemon signs its own JWT for: one
// installation for each owner it names, what each installation is granted (grants, nil for
// grantedEverythingAsked), the App's slug and bot user, and every access_tokens body, both as
// GitHub parsed it (bodies) and as the text it was sent as (raw), which is what a test asking
// whether a request named a repository must read. answer decides each exchange from the body it
// carried and how many came before it, and granted, when it is set, answers the permissions
// read of an installation instead of its grants.
type keyModeGitHub struct {
	mu      sync.Mutex
	owners  []string
	slug    string
	grants  map[string]string
	bodies  []accessTokensBody
	raw     []string
	reads   []string
	answer  func(exchange int, body accessTokensBody) (int, string)
	granted func(installation string, read int) (int, string)
}

func (g *keyModeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations":
			listed := make([]string, 0, len(g.owners))
			for i, owner := range g.owners {
				listed = append(listed, fmt.Sprintf(`{"id":%d,"account":{"login":%q}}`, 101+i, owner))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(listed, ","))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			text, body := askedInExchange(t, r)
			g.mu.Lock()
			g.bodies = append(g.bodies, body)
			g.raw = append(g.raw, text)
			exchange := len(g.bodies)
			g.mu.Unlock()
			status, answer := g.answer(exchange, body)
			w.WriteHeader(status)
			fmt.Fprint(w, answer)
		case strings.HasPrefix(r.URL.Path, "/app/installations/"):
			installation := strings.TrimPrefix(r.URL.Path, "/app/installations/")
			g.mu.Lock()
			g.reads = append(g.reads, installation)
			read, grants, granted := len(g.reads), g.grants, g.granted
			g.mu.Unlock()
			if granted != nil {
				status, answer := granted(installation, read)
				w.WriteHeader(status)
				fmt.Fprint(w, answer)
				return
			}
			if grants == nil {
				grants = grantedEverythingAsked
			}
			fmt.Fprint(w, installationGranting(installation, grants))
		case r.URL.Path == "/app":
			fmt.Fprintf(w, `{"slug":%q}`, g.slug)
		case r.URL.Path == "/users/"+g.slug+"[bot]":
			fmt.Fprint(w, `{"id":271566630}`)
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
	}})
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
// stderr kept rather than written to the test's own.
func keyModeManager(t *testing.T, github *keyModeGitHub, now *time.Time, stderr *bytes.Buffer) *Manager {
	t.Helper()
	_, privatePEM := testAppKey(t)
	server := github.serve(t)
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return *now }, Stderr: stderr})
	t.Cleanup(tokens.Close)
	return tokens
}

// A write mint asks for exactly the permissions that App's roles use, each App's set its own,
// and names no repository at all: what the token reaches is what the account chose when it
// installed the App, and what the token may do there is these permissions (INFRA-2543). The
// installation holds more — issues, projects, packages — and a token that asks for none of it
// cannot be made to use them.
func TestTokenAsksForNoRepositoryAndTheAppsWritePermissions(t *testing.T) {
	for _, tc := range []struct {
		role AppRole
		slug string
		want map[string]string
	}{
		{Implement, "legion-implementer", map[string]string{
			"actions": "write", "checks": "read", "contents": "write", "metadata": "read",
			"pull_requests": "write", "security_events": "read", "statuses": "write", "workflows": "write",
		}},
		{Review, "legion-reviewer", map[string]string{
			"actions": "write", "checks": "write", "contents": "write", "metadata": "read",
			"pull_requests": "write", "security_events": "write",
		}},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"sjawhar"}, slug: tc.slug}
			github.answer = func(exchange int, body accessTokensBody) (int, string) {
				return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
			}
			tokens := keyModeManager(t, github, &now, &stderr)

			if _, err := tokens.Token(context.Background(), tc.role, ghrepo.MustParse("sjawhar/legion")); err != nil {
				t.Fatalf("Token: %v", err)
			}

			asked := github.exchanges()
			if len(asked) != 1 {
				t.Fatalf("%d exchanges, want the one mint", len(asked))
			}
			if !maps.Equal(asked[0].Permissions, tc.want) {
				t.Errorf("the %s App's write request named permissions %v, want exactly %v", tc.role, asked[0].Permissions, tc.want)
			}
			if text := github.exchangeTexts()[0]; strings.Contains(text, "repositor") {
				t.Errorf("the request body is %s, want the permissions and no repository", text)
			}
		})
	}
}

// A read mint asks for the four reads a role and a controller need of a repository that is not
// their own issue's — nothing writable — and names no repository either, for either App: the
// lease reaches every repository the owner's installation holds, and reading is the whole of
// what holds it.
func TestReadTokenAsksForTheFourReadsAndNoRepository(t *testing.T) {
	for _, tc := range []struct {
		role AppRole
		slug string
	}{
		{Implement, "legion-implementer"},
		{Review, "legion-reviewer"},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"acme"}, slug: tc.slug}
			github.answer = func(exchange int, body accessTokensBody) (int, string) {
				return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "legion")
			}
			tokens := keyModeManager(t, github, &now, &stderr)

			if _, err := tokens.ReadToken(context.Background(), tc.role, "acme", []string{"legion"}); err != nil {
				t.Fatalf("ReadToken: %v", err)
			}

			asked := github.exchanges()
			if len(asked) != 1 {
				t.Fatalf("%d exchanges, want the one mint", len(asked))
			}
			want := map[string]string{"actions": "read", "contents": "read", "metadata": "read", "pull_requests": "read"}
			if !maps.Equal(asked[0].Permissions, want) {
				t.Errorf("the %s App's read request named permissions %v, want exactly %v", tc.role, asked[0].Permissions, want)
			}
			if text := github.exchangeTexts()[0]; strings.Contains(text, "repositor") {
				t.Errorf("the request body is %s, want the four reads and no repository", text)
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
			if _, err := tokens.Token(context.Background(), role, ghrepo.MustParse(owner+"/legion")); err != nil {
				t.Fatalf("the %s App's write mint for %s: %v", role, owner, err)
			}
			if _, err := tokens.ReadToken(context.Background(), role, owner, []string{"legion"}); err != nil {
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

			lease, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion"))
			if err != nil || lease.Token != "ghs_all" {
				t.Fatalf("the lease = %q, %v; want the token GitHub minted", lease.Token, err)
			}
			if _, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion")); err != nil {
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
		{"a permission the request did not ask for", `{"issues":"write","contents":"read","metadata":"read","pull_requests":"read","actions":"read"}`, "issues: write"},
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

			lease, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"legion"})
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

			lease, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"legion"})
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

// Every caller names the repositories its lease is cached under, and those names reach GitHub
// not at all (TestNoMintEverNamesARepositoryToGitHub): they key the lease, its command run and
// its refusal memory. A call that names none, or names one with nothing in it, is a caller's
// mistake, so every door into a mint refuses it, and refuses it before any exchange is sent.
func TestNoMintWithoutRepositoriesEverReachesGitHub(t *testing.T) {
	for _, tc := range []struct {
		name string
		mint func(tokens *Manager) error
	}{
		{"a write token for the zero repository", func(tokens *Manager) error {
			_, err := tokens.Token(context.Background(), Implement, ghrepo.Repository{})
			return err
		}},
		{"a read token for no repository", func(tokens *Manager) error {
			_, err := tokens.ReadToken(context.Background(), Implement, "sjawhar", nil)
			return err
		}},
		{"a read token for an empty list", func(tokens *Manager) error {
			_, err := tokens.ReadToken(context.Background(), Implement, "sjawhar", []string{})
			return err
		}},
		{"a read token for a repository with no name", func(tokens *Manager) error {
			_, err := tokens.ReadToken(context.Background(), Implement, "sjawhar", []string{" "})
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
				t.Fatal("the mint succeeded, want a call naming no repository refused")
			}
			if asked := github.exchanges(); len(asked) != 0 {
				t.Errorf("%d exchanges, want none: a call naming no repository is refused before anything is minted", len(asked))
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

	lease, err := tokens.Token(context.Background(), Implement, ghrepo.MustParse("sjawhar/legion"))
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
		lease, err := tokens.ReadToken(context.Background(), Review, owner, []string{"legion"})
		if err != nil {
			t.Fatalf("ReadToken for %s: %v", owner, err)
		}
		return lease.Token
	}
	write := func() string {
		t.Helper()
		lease, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/legion"))
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
		lease, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"legion"})
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
		lease, err := tokens.Token(context.Background(), Review, ghrepo.MustParse("acme/legion"))
		if err != nil || lease.Token == "" {
			t.Fatalf("mint %d of the scope the installation grants = %q, %v", i+1, lease.Token, err)
		}
	}
	if asked := github.exchanges(); len(asked) != 3 {
		t.Errorf("%d exchanges, want one more: a scope that answers is remembered nowhere and served from its lease", len(asked))
	}
}

// GitHub's answer to a token exchange lists one object for every repository the installation
// holds, whatever the request named, and a Legion installation holds hundreds, so the bound a
// mint reads its answer under is 16 MiB: about 3,000 repositories at the 4.9 to 5.3 KB one
// object measures. An answer of 3,000 objects of 5,300 bytes is read whole and its lease served,
// though the mint names one repository; one past the bound is refused naming the bound, and the
// refusal carries neither the token nor anything else the body held.
func TestAnAnswerOfThreeThousandRepositoriesIsReadAndOnePastTheBoundIsRefused(t *testing.T) {
	expires := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC).Format(time.RFC3339)
	// Each repository object is of the size GitHub's answer carries, the padding standing for
	// the fields a repository object holds that this daemon never reads.
	const repositoryObject = 5300
	answerOf := func(repositories int) string {
		var answer strings.Builder
		fmt.Fprintf(&answer, `{"token":"ghs_many","expires_at":%q,"repository_selection":"selected","permissions":{"actions":"read","contents":"read","metadata":"read","pull_requests":"read"},"repositories":[`, expires)
		for i := range repositories {
			if i > 0 {
				answer.WriteByte(',')
			}
			object := fmt.Sprintf(`{"id":%d,"name":"repo-%04d","full_name":"acme/repo-%04d","description":"`, i, i, i)
			answer.WriteString(object)
			answer.WriteString(strings.Repeat("x", repositoryObject-len(object)-len(`"}`)))
			answer.WriteString(`"}`)
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
		{"an answer past the bound", 3200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := answerOf(tc.repositories)
			if past := len(answer) > 16<<20; past == tc.served {
				t.Fatalf("the answer of %d repositories is %d bytes against a bound of %d", tc.repositories, len(answer), 16<<20)
			}
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			var stderr bytes.Buffer
			github := &keyModeGitHub{owners: []string{"acme"}, slug: "legion-reviewer"}
			github.answer = func(int, accessTokensBody) (int, string) { return http.StatusOK, answer }
			tokens := keyModeManager(t, github, &now, &stderr)

			lease, err := tokens.ReadToken(context.Background(), Review, "acme", []string{"legion"})
			if tc.served {
				if err != nil || lease.Token != "ghs_many" {
					t.Fatalf("the lease of an answer of %d repositories = %q, %v; want the token GitHub minted", tc.repositories, lease.Token, err)
				}
				return
			}
			if err == nil || lease.Token != "" {
				t.Fatalf("an answer past the bound = %q, %v; want no lease and a refusal", lease.Token, err)
			}
			if !strings.Contains(err.Error(), "16777216") {
				t.Errorf("the refusal does not name the bound it met: %v", err)
			}
			if strings.Contains(err.Error(), "ghs_many") || strings.Contains(err.Error(), "repo-") {
				t.Errorf("the refusal carries what the body held: %v", err)
			}
		})
	}
}
