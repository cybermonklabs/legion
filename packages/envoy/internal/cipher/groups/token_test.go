package groups_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/groups"
)

// The identities: the reader account a deployment names, the pod's own token that calls IAM, and
// the token IAM mints for the reader account. Each is an example-project name, since the
// repository is public.
const (
	readerAccount = "group-reader@example-project.iam.gserviceaccount.com"
	defaultToken  = "pod-default-token-5d2e"
	mintedToken   = "minted-directory-token-8a41"
)

// mintPath is generateAccessToken's path, the account between its fixed parts.
var mintPath = regexp.MustCompile(`^/v1/projects/-/serviceAccounts/([^/]+):generateAccessToken$`)

// refusal is a Google refusal a stand-in answers: its status and its JSON body.
type refusal struct {
	status int
	body   string
}

// The refusals the reader's log line must name the reason of. The mint's is IAM's answer to a
// caller that holds no Token Creator on the reader account; the Directory's is the Admin SDK's
// answer to a token it does not take, as it answers a GKE pod's own metadata-server token.
var (
	mintDenied               = refusal{http.StatusForbidden, `{"error":{"code":403,"message":"Permission 'iam.serviceAccounts.getAccessToken' denied on resource (or it may not exist).","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"IAM_PERMISSION_DENIED","domain":"iam.googleapis.com","metadata":{"permission":"iam.serviceAccounts.getAccessToken"}}]}}`}
	directoryUnauthenticated = refusal{http.StatusUnauthorized, `{"error":{"code":401,"message":"Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential.","errors":[{"message":"Invalid Credentials","domain":"global","reason":"authError","location":"Authorization","locationType":"header"}],"status":"UNAUTHENTICATED"}}`}
)

// mintCall is one generateAccessToken request as the IAM stand-in read it.
type mintCall struct {
	method, account, bearer, contentType string
	scope                                []string
	lifetime                             string
}

// iam is a stand-in of IAM's generateAccessToken: it mints mintedToken for any account, ending
// expiresIn after the real clock's now (the token source's reuse reads the real clock, not a
// test's), or answers refused when it is set, and records each request.
type iam struct {
	server *httptest.Server

	mu        sync.Mutex
	refused   *refusal
	expiresIn time.Duration
	// noExpiry makes the mint answer a token with no expireTime.
	noExpiry bool
	mints    []mintCall
}

func newIAM(t *testing.T) *iam {
	t.Helper()
	g := &iam{expiresIn: time.Hour}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scope    []string `json:"scope"`
			Lifetime string   `json:"lifetime"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		call := mintCall{method: r.Method, bearer: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), scope: body.Scope, lifetime: body.Lifetime}
		if match := mintPath.FindStringSubmatch(r.URL.Path); match != nil {
			call.account = match[1]
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.mints = append(g.mints, call)
		if g.refused != nil {
			w.WriteHeader(g.refused.status)
			_, _ = io.WriteString(w, g.refused.body)
			return
		}
		answer := map[string]string{"accessToken": mintedToken}
		if !g.noExpiry {
			answer["expireTime"] = time.Now().Add(g.expiresIn).UTC().Format(time.RFC3339)
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(g.server.Close)
	return g
}

// podToken is the pod's own credential, the bearer every mint carries.
func podToken() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: defaultToken, TokenType: "Bearer"})
}

// source is the directory token for account over the stand-in, minted with the pod's own token.
func (g *iam) source(account string) oauth2.TokenSource {
	return g.sourceWith(account, podToken())
}

func (g *iam) sourceWith(account string, own oauth2.TokenSource) oauth2.TokenSource {
	return groups.NewTokenSource(groups.TokenOptions{Account: account, IAMCredentialsURL: g.server.URL, HTTP: g.server.Client(), Default: own})
}

func (g *iam) refuse(answer *refusal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refused = answer
}

func (g *iam) minted() []mintCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.mints)
}

// noToken is a pod credential that gives no token, as application default credentials do on a
// machine with none.
type noToken struct{}

func (noToken) Token() (*oauth2.Token, error) {
	return nil, errors.New("google: could not find default credentials, read from " + defaultToken)
}

// TestTheTokenIsTheReaderAccountsWithTheMemberReadScope pins the directory token: the mint is for
// the reader account the deployment names, it asks for the one member-read scope for an hour,
// with the pod's own token as its bearer, and the token answered is the minted one, ending when
// IAM says it ends.
func TestTheTokenIsTheReaderAccountsWithTheMemberReadScope(t *testing.T) {
	g := newIAM(t)
	token, err := g.source(readerAccount).Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token.AccessToken != mintedToken {
		t.Fatalf("Token = %q, want the token IAM minted", token.AccessToken)
	}
	if until := time.Until(token.Expiry); until < 59*time.Minute || until > time.Hour {
		t.Fatalf("the token ends in %v, want the hour IAM's expireTime names", until)
	}
	mints := g.minted()
	if len(mints) != 1 {
		t.Fatalf("IAM was asked %d mints, want 1", len(mints))
	}
	mint := mints[0]
	if mint.method != http.MethodPost || mint.account != readerAccount {
		t.Fatalf("IAM was asked %s for account %q, want POST generateAccessToken for %s, the reader account", mint.method, mint.account, readerAccount)
	}
	if !slices.Equal(mint.scope, []string{groups.Scope}) || mint.lifetime != "3600s" {
		t.Fatalf("the mint asked for scope %v and lifetime %q, want exactly [%s] and 3600s", mint.scope, mint.lifetime, groups.Scope)
	}
	if mint.bearer != "Bearer "+defaultToken || mint.contentType != "application/json" {
		t.Fatalf("the mint carried Authorization %q and Content-Type %q, want the pod's own token and application/json", mint.bearer, mint.contentType)
	}
}

// TestTheMintedTokenIsReusedUntilItEnds: a token that has most of its hour left is handed out
// again with no second mint, and one about to end is minted again, so a read never sends the
// Directory a token past the end IAM gave it.
func TestTheMintedTokenIsReusedUntilItEnds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresIn time.Duration
		mints     int
	}{
		{"an hour left", time.Hour, 1},
		{"about to end", 5 * time.Second, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newIAM(t)
			g.expiresIn = tc.expiresIn
			source := g.source(readerAccount)
			for range 2 {
				if _, err := source.Token(); err != nil {
					t.Fatalf("Token: %v", err)
				}
			}
			if got := len(g.minted()); got != tc.mints {
				t.Fatalf("two tokens with %v left took %d mints, want %d", tc.expiresIn, got, tc.mints)
			}
		})
	}
}

// TestEachTokenRefusalNamesItsCause: no reader account is ErrNoDirectoryAccount and mints
// nothing; a pod credential that gives no token mints nothing; a mint IAM refuses names Google's
// reason; and a mint answered with no end is refused, so no token outlives its hour. None of them
// hands out a token, and no refusal quotes one.
func TestEachTokenRefusalNamesItsCause(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account string
		own     oauth2.TokenSource
		setUp   func(*iam)
		is      error
		names   []string
		mints   int
	}{
		{"no reader account", "", podToken(), func(*iam) {}, groups.ErrNoDirectoryAccount, []string{"no directory account"}, 0},
		{"no pod credential", readerAccount, noToken{}, func(*iam) {}, nil, []string{"own Google credentials gave no token"}, 0},
		{"a refused mint", readerAccount, podToken(), func(g *iam) { g.refuse(&mintDenied) }, nil, []string{readerAccount, "403", "IAM_PERMISSION_DENIED"}, 1},
		{"a token with no end", readerAccount, podToken(), func(g *iam) { g.noExpiry = true }, nil, []string{"expireTime"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newIAM(t)
			tc.setUp(g)
			token, err := g.sourceWith(tc.account, tc.own).Token()
			if err == nil {
				t.Fatalf("Token = %v, want a refusal", token)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("Token's error %v is not %v", err, tc.is)
			}
			for _, name := range tc.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("Token's error %q does not name %q", err, name)
				}
			}
			for _, secret := range []string{defaultToken, mintedToken} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("Token's error %q quotes a token", err)
				}
			}
			if got := len(g.minted()); got != tc.mints {
				t.Fatalf("IAM was asked %d mints, want %d", got, tc.mints)
			}
		})
	}
}
