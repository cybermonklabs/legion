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

// The pod's identities: the service account the pod runs as, the name the metadata server
// answers for a pod that runs as a Workload Identity principal and no service account, the
// metadata server's own token for either, and the token IAM mints for the account. Each is an
// example-project name, since the repository is public.
const (
	brokerAccount = "broker@example-project.iam.gserviceaccount.com"
	principal     = "example-project.svc.id.goog"
	defaultToken  = "pod-default-token-5d2e"
	mintedToken   = "minted-directory-token-8a41"
)

// emailPath is the metadata server's path for the email of the account the pod runs as.
const emailPath = "/computeMetadata/v1/instance/service-accounts/default/email"

// mintPath is generateAccessToken's path, the account between its fixed parts.
var mintPath = regexp.MustCompile(`^/v1/projects/-/serviceAccounts/([^/]+):generateAccessToken$`)

// refusal is a Google refusal a stand-in answers: its status and its JSON body.
type refusal struct {
	status int
	body   string
}

// The refusals the reader's log line must name the reason of. The mint's is IAM's answer to an
// account that holds no Token Creator on itself; the Directory's is the Admin SDK's answer to a
// token it does not take, as it answers a GKE pod's own metadata-server token.
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

// google is a stand-in of the two Google endpoints the directory token reads: the metadata
// server, which answers email for the account the pod runs as, and IAM's generateAccessToken,
// which mints mintedToken for any account, ending expiresIn after the real clock's now (the
// token source's reuse reads the real clock, not a test's), or answers refused when it is set.
type google struct {
	metadata, iam *httptest.Server

	mu        sync.Mutex
	email     string
	refused   *refusal
	expiresIn time.Duration
	// noExpiry makes the mint answer a token with no expireTime.
	noExpiry bool
	// metadataStatus, when set, is what the metadata server answers instead of the email.
	metadataStatus int
	// flavors is the Metadata-Flavor header of each request to the metadata server, and paths
	// each one's path.
	flavors, metadataPaths []string
	mints                  []mintCall
}

func newGoogle(t *testing.T, email string) *google {
	t.Helper()
	g := &google{email: email, expiresIn: time.Hour}
	g.metadata = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.flavors = append(g.flavors, r.Header.Get("Metadata-Flavor"))
		g.metadataPaths = append(g.metadataPaths, r.URL.Path)
		switch {
		case g.metadataStatus != 0:
			w.WriteHeader(g.metadataStatus)
		case r.Method != http.MethodGet || r.URL.Path != emailPath || r.Header.Get("Metadata-Flavor") != "Google":
			http.NotFound(w, r)
		default:
			_, _ = io.WriteString(w, g.email)
		}
	}))
	t.Cleanup(g.metadata.Close)
	g.iam = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(g.iam.Close)
	return g
}

// source is the directory token over the stand-ins, with the metadata server's own token as the
// default credential that calls IAM.
func (g *google) source() oauth2.TokenSource {
	return groups.NewTokenSource(groups.TokenOptions{
		MetadataURL:       g.metadata.URL,
		IAMCredentialsURL: g.iam.URL,
		HTTP:              g.iam.Client(),
		Default:           oauth2.StaticTokenSource(&oauth2.Token{AccessToken: defaultToken, TokenType: "Bearer"}),
	})
}

func (g *google) minted() []mintCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.mints)
}

func (g *google) metadataRead() (flavors, paths []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.flavors), slices.Clone(g.metadataPaths)
}

// TestTheTokenIsTheAccountsOwnWithTheMemberReadScope pins the directory token: the account it
// mints for is the one the metadata server names, read with the metadata server's own header,
// and nothing else; the mint asks for the one member-read scope for an hour, with the metadata
// server's own token as its bearer; and the token answered is the minted one, ending when IAM
// says it ends.
func TestTheTokenIsTheAccountsOwnWithTheMemberReadScope(t *testing.T) {
	g := newGoogle(t, brokerAccount)
	token, err := g.source().Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token.AccessToken != mintedToken {
		t.Fatalf("Token = %q, want the token IAM minted", token.AccessToken)
	}
	if until := time.Until(token.Expiry); until < 59*time.Minute || until > time.Hour {
		t.Fatalf("the token ends in %v, want the hour IAM's expireTime names", until)
	}
	flavors, paths := g.metadataRead()
	if !slices.Equal(paths, []string{emailPath}) || !slices.Equal(flavors, []string{"Google"}) {
		t.Fatalf("the metadata server was asked %v with Metadata-Flavor %v; want %s once, with Google", paths, flavors, emailPath)
	}
	mints := g.minted()
	if len(mints) != 1 {
		t.Fatalf("IAM was asked %d mints, want 1", len(mints))
	}
	mint := mints[0]
	if mint.method != http.MethodPost || mint.account != brokerAccount {
		t.Fatalf("IAM was asked %s for account %q, want POST generateAccessToken for %s, the account the metadata server names", mint.method, mint.account, brokerAccount)
	}
	if !slices.Equal(mint.scope, []string{groups.Scope}) || mint.lifetime != "3600s" {
		t.Fatalf("the mint asked for scope %v and lifetime %q, want exactly [%s] and 3600s", mint.scope, mint.lifetime, groups.Scope)
	}
	if mint.bearer != "Bearer "+defaultToken || mint.contentType != "application/json" {
		t.Fatalf("the mint carried Authorization %q and Content-Type %q, want the metadata server's own token and application/json", mint.bearer, mint.contentType)
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
			g := newGoogle(t, brokerAccount)
			g.expiresIn = tc.expiresIn
			source := g.source()
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

// TestEachTokenRefusalNamesItsCause: an email that names no service account (a pod that runs as
// a Workload Identity principal, or a person) is ErrNoServiceAccount and mints nothing; a
// metadata server that answers no email, a mint IAM refuses and a mint answered with no end each
// refuse the token, the IAM refusal naming Google's reason, and none of them hands out a token.
func TestEachTokenRefusalNamesItsCause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setUp func(*google)
		is    error
		names []string
		mints int
	}{
		{"a Workload Identity principal", func(g *google) { g.email = principal }, groups.ErrNoServiceAccount, []string{"no service account", principal}, 0},
		{"a person", func(g *google) { g.email = "member@example.com" }, groups.ErrNoServiceAccount, []string{"no service account"}, 0},
		{"no email", func(g *google) { g.metadataStatus = http.StatusNotFound }, nil, []string{"metadata server", "404"}, 0},
		{"a refused mint", func(g *google) { g.refused = &mintDenied }, nil, []string{"403", "IAM_PERMISSION_DENIED"}, 1},
		{"a token with no end", func(g *google) { g.noExpiry = true }, nil, []string{"expireTime"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGoogle(t, brokerAccount)
			tc.setUp(g)
			token, err := g.source().Token()
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
