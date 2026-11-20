package brokertoken

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redirects"
)

// GoogleAccountCacheTTL is how long a GoogleAccount reuses the token it last read. The metadata
// server mints an identity token that lives an hour, generateIdToken mints a service account's for
// an hour too, and the broker refuses a caller's token that lives longer than an hour from its
// issue (machine.MaxTokenLoginLifetime), so a token this fresh is never near expiry; the window is
// AccountTokenCacheTTL's for the reason that one is what it is, that a credential read once per
// call would be read once per request.
//
// It is a window rather than an expiry read from the token because nothing here parses a claim:
// internal/googleidentity reads the bytes and leaves every claim to the broker, and a reader that
// started decoding the token to find its 'exp' would be deciding how long to trust a credential
// from the credential itself.
const GoogleAccountCacheTTL = 30 * time.Second

// GoogleAccountMintTimeout is the one bound over a mint (GoogleAccount.ServiceAccount): the read
// of this pod's own access token and the generateIdToken call that spends it. It is
// internal/googleidentity's own bound for one metadata read, so neither of the reader's two routes
// holds a caller longer than the other, and a mint, which Google answers in well under a second,
// that takes this long is a fault rather than a slow network.
const GoogleAccountMintTimeout = 10 * time.Second

// googleIAMCredentialsURL is IAM Service Account Credentials, whose generateIdToken mints a
// service account's identity token for a caller that may act as that account.
const googleIAMCredentialsURL = "https://iamcredentials.googleapis.com"

// maxMintAnswer bounds what one answer of generateIdToken is read of, and maxMintRefusal how much
// of a refusal's body its error carries: an identity token is a few KiB, and the reason Google
// gives for a refusal fits in a few hundred bytes.
const (
	maxMintAnswer  = 1 << 20
	maxMintRefusal = 512
)

// GoogleAccount is the Account of a caller holding no Kubernetes token at all, whose identity is a
// Google identity token, read one of two ways.
//
// With ServiceAccount empty the token is this machine's own, which its metadata server mints for
// the service account the machine runs as (internal/googleidentity): a listener on a Compute
// Engine VM, whose calling service the broker resolves from the token's 'email' claim
// (BROKER_API_ACCOUNTS' Google half), so what a deployment names here is the audience alone; and
// the broker's model judge on a pod with a service account of its own.
//
// With ServiceAccount set the token is that account's, minted at IAM Service Account Credentials'
// generateIdToken with includeEmail, spending the OAuth access token the metadata server answers
// for this pod. That is the route Google gives an identity that has no identity token of its own,
// such as a pod that runs as a bare Workload Identity principal: its metadata server answers an
// access token and no identity token. The principal needs
// roles/iam.serviceAccountOpenIdTokenCreator on the account. On this route the metadata server's
// identity path is never asked, and no failure falls back to it: a deployment that named an
// account meant its calls to carry that account's token, and the pod's own would be a different
// caller.
//
// The access token comes from internal/googleidentity's metadata read rather than from Application
// Default Credentials, because their token read takes no context and retries past any bound this
// reader sets. The mint is this file's own call rather than internal/brokerclient's generateIDToken
// because internal/brokerclient and internal/googleidentity are paths release.yaml's
// LEGION_VERSION_ARGS names: changing either to share the call would release the Legion daemon for
// no change in its behaviour.
//
// Either token rotates as a projected one does, so it is asked for at call time and never once at
// boot.
type GoogleAccount struct {
	// Audience is the 'aud' the token is minted for: the broker's own audience, as the broker's
	// verifier of its callers' Google tokens requires it (BROKER_GOOGLE_OIDC_AUDIENCE), or the
	// model gateway's, for the broker's judge (BROKER_JUDGE_MODEL_AUDIENCE).
	Audience string
	// ServiceAccount is the email of the service account whose identity token Read mints at
	// generateIdToken, and empty for this machine's own, read from the metadata server's identity
	// path. It is set before the first Read and never after.
	ServiceAccount string
	// IAMCredentialsURL is the origin generateIdToken is called at, Google's own
	// (https://iamcredentials.googleapis.com) when empty. A test names its stand-in here.
	IAMCredentialsURL string
	// HTTP is the client the mint is called on, http.DefaultClient when nil. Either is used as a
	// copy that refuses redirects (redirects.Refused says why): the call carries this pod's own
	// access token, which no 3xx may take anywhere.
	HTTP *http.Client
	// MintTimeout is the one bound over a mint, GoogleAccountMintTimeout for every reader
	// NewGoogleAccount builds. Zero reads as that constant, never as no bound. A test that
	// proves the bound holds shortens it.
	MintTimeout time.Duration
	// TTL is how long Read reuses what it read, GoogleAccountCacheTTL for every reader
	// NewGoogleAccount builds. A test that proves a second read reaches the metadata server
	// lapses the cache by zeroing it.
	TTL time.Duration

	mu     sync.Mutex
	cached string
	readAt time.Time
	// flight is the one read in progress, which every reader that finds nothing fresh enough
	// joins rather than opening a second one.
	flight singleflight.Group
	// access is this pod's own access token, which accessOnce makes on the first mint and every
	// later mint reuses: oauth2.ReuseTokenSource answers the token it holds until that token
	// nears its expiry, so a mint every TTL asks the metadata server for an access token about
	// once an hour rather than once per mint.
	accessOnce sync.Once
	access     oauth2.TokenSource
}

// NewGoogleAccount is a reader of this machine's Google identity token for audience, which Read
// asks the metadata server for at call time, caching what it reads for GoogleAccountCacheTTL. A
// caller that sets ServiceAccount before the first Read makes it a reader of that account's
// identity token instead, minted within GoogleAccountMintTimeout.
func NewGoogleAccount(audience string) *GoogleAccount {
	return &GoogleAccount{Audience: audience, TTL: GoogleAccountCacheTTL, MintTimeout: GoogleAccountMintTimeout}
}

// Read is the bearer a call presents: the identity token for Audience, this machine's own or
// ServiceAccount's, asked for again once the copy it cached is older than TTL. A metadata server
// that cannot be reached, or that answers no token, is an error naming the audience and no
// request at all, never an empty bearer for the broker to refuse: a machine identity this program
// cannot read is this deployment's own misconfiguration, as an unreadable projected token is. A
// mint that fails is an error too, which names the account and the audience before what failed,
// since the judge writes it into the audit row a person reads, and never names a token.
//
// The read runs on a context of its own rather than on the request's: one request going away must
// not leave the next one without the machine's identity, and the answer is cached for every
// caller. Its bound is internal/googleidentity's own 10-second timeout for the metadata server's
// identity token, and MintTimeout for a mint, one bound over both its access-token read and its
// call to generateIdToken.
//
// Readers that arrive while a read is in flight take its answer — the token, or the error it
// failed with — instead of each asking in turn, and the lock is released around the read rather
// than held across it. This is what keeps a metadata server or a generateIdToken that has stopped
// answering from costing the Nth queued caller N of the read's bounds: on a listener this read is
// on the per-request authentication path (introspect.go). The cost is that concurrent readers no
// longer get a failure each of their own, which is what the followers would have paid the bound
// for.
func (g *GoogleAccount) Read() (string, error) {
	if token, fresh := g.fresh(); fresh {
		return token, nil
	}
	shared, err, _ := g.flight.Do(g.Audience, func() (any, error) {
		// The flight's own look at the cache: a reader that waited out this flight's predecessor
		// would otherwise ask again for a token just read.
		if token, fresh := g.fresh(); fresh {
			return token, nil
		}
		token, err := g.read()
		if err != nil {
			return "", err
		}
		g.mu.Lock()
		g.cached, g.readAt = token, time.Now()
		g.mu.Unlock()
		return token, nil
	})
	if err != nil {
		return "", err
	}
	return shared.(string), nil
}

// read asks for the token once, by the route ServiceAccount picks.
func (g *GoogleAccount) read() (string, error) {
	if g.ServiceAccount != "" {
		return g.mint()
	}
	token, err := googleidentity.Token(context.Background(), g.Audience)
	if err != nil {
		return "", fmt.Errorf("brokertoken: read this machine's Google identity token for %s: %w", g.Audience, err)
	}
	return token, nil
}

// fresh is the cached token while it is younger than TTL, and false once it is not.
func (g *GoogleAccount) fresh() (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cached != "" && time.Since(g.readAt) < g.TTL {
		return g.cached, true
	}
	return "", false
}

// mint is ServiceAccount's identity token for Audience, from one generateIdToken call that spends
// this pod's own access token, the read and the call both inside one MintTimeout. Every error
// opens with the account and the audience, then says what failed, and none holds the access token
// or an identity token: a refusal's body is quoted, cut to maxMintRefusal bytes, as the rule for
// remote text reads it with the request's credentials redacted (redact.RemoteText), and the body
// of a 2xx answer, which carries the token, never is.
func (g *GoogleAccount) mint() (string, error) {
	token, err := g.generateIDToken()
	if err != nil {
		return "", fmt.Errorf("brokertoken: mint the Google identity token of %s for %s at IAM Service Account Credentials: %w", g.ServiceAccount, g.Audience, err)
	}
	return token, nil
}

// generateIDToken is mint's work, its errors not yet naming the account and the audience.
func (g *GoogleAccount) generateIDToken() (string, error) {
	bound := g.MintTimeout
	if bound <= 0 {
		bound = GoogleAccountMintTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	access, err := g.accessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("read this pod's own access token from the metadata server: %w", err)
	}
	endpoint := strings.TrimSuffix(cmp.Or(g.IAMCredentialsURL, googleIAMCredentialsURL), "/") +
		"/v1/projects/-/serviceAccounts/" + url.PathEscape(g.ServiceAccount) + ":generateIdToken"
	body, err := json.Marshal(map[string]any{"audience": g.Audience, "includeEmail": true})
	if err != nil {
		return "", fmt.Errorf("build the generateIdToken request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build the generateIdToken request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := redirects.Refused(cmp.Or(g.HTTP, http.DefaultClient)).Do(req)
	if err != nil {
		return "", fmt.Errorf("call generateIdToken: %w", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxMintAnswer))
	if err != nil {
		return "", fmt.Errorf("read generateIdToken's answer: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		reason := bytes.TrimSpace(answer)
		reason = reason[:min(len(reason), maxMintRefusal)]
		said := redact.NewRemoteText(string(bytes.ToValidUTF8(reason, nil))).WithoutTokens(redact.Carried(resp)...)
		return "", fmt.Errorf("generateIdToken answered HTTP %d: %s", resp.StatusCode, said)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(answer, &minted) != nil || minted.Token == "" {
		return "", fmt.Errorf("generateIdToken answered HTTP %d with no token", resp.StatusCode)
	}
	return minted.Token, nil
}

// accessToken is this pod's own access token from the reader's one access source, or ctx's error
// once ctx ends first. A TokenSource's Token takes no context, so it runs on a goroutine of its
// own that this read stops waiting for at ctx's end; that goroutine ends within
// internal/googleidentity's own 10-second bound, and a token it reads then is kept by the source
// for the next mint.
func (g *GoogleAccount) accessToken(ctx context.Context) (string, error) {
	g.accessOnce.Do(func() {
		g.access = oauth2.ReuseTokenSource(nil, redact.TokenSource(googleidentity.AccessTokenSource()))
	})
	source := g.access
	type answer struct {
		token *oauth2.Token
		err   error
	}
	// One slot, so a read nothing waits for any more does not block on its send.
	answered := make(chan answer, 1)
	go func() {
		token, err := redact.OAuth2(source.Token())
		answered <- answer{token, err}
	}()
	select {
	case a := <-answered:
		if a.err != nil {
			return "", a.err
		}
		return a.token.AccessToken, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
