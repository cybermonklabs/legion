// Package githubapp mints GitHub App installation tokens and proves the App
// can read a repository before Dispatch stores it as an architecture source.
//
// The RS256 App JWT is signed by internal/githubappjwt, which the secrets
// broker's github-app minter shares.
package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/githubrate"
	"github.com/sjawhar/envoy/httpoutage"
	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/githubappjwt"
)

// Typed access-check failures. Handlers surface these verbatim to the human
// configuring a source; anything else is a genuine upstream failure.
var (
	// ErrNoAppKey is the "no app credentials yet" boot state: the server has no
	// GitHub App private key, so no installation call can be signed.
	ErrNoAppKey = errors.New("the app has no private key configured")
	// ErrNoInstallation is GitHub's 404 for a repository's App installation.
	ErrNoInstallation = errors.New("the GitHub App is not installed")
	// ErrNoContentsRead is an installation whose Contents permission is missing
	// or "none". Read and write both satisfy read.
	ErrNoContentsRead = errors.New("the installation lacks Contents: read")
	// ErrNoActionsRead is an installation whose Actions permission is missing or
	// "none" (LEGION-567's reconcile needs it to list workflow runs and jobs).
	ErrNoActionsRead = errors.New("the installation lacks Actions: read")
	// ErrNoPullRequestsRead is an installation whose Pull requests permission is
	// missing or "none" (LEGION-567's reconcile needs it to read merged PRs).
	ErrNoPullRequestsRead = errors.New("the installation lacks Pull requests: read")
	// ErrNoBranch is GitHub's 422 for a configured branch that does not exist,
	// or its 404 for a repository it cannot find or the installation cannot see;
	// Branch also answers it for a name that is not exactly a branch's (see Branch).
	ErrNoBranch = errors.New("the branch does not exist")
	// ErrTreeTruncated is GitHub's truncated=true on a tree read: the
	// directory exceeds the tree API caps (100k entries / 7 MB), so the
	// listing is not trustworthy and the sync must fail whole.
	ErrTreeTruncated = errors.New("the directory tree is too large for the GitHub tree API")
	// ErrRepositoryUnreadable is GitHub's 403 or 404 on a repository read the installation may
	// not make, or on a repository or commit it cannot see: the answer the results read takes
	// as unknown rather than as a commit it placed elsewhere (CheckRepositoryResponse, which
	// CommitOnBranch, DefaultBranch and delivery's own pull-request reads all classify their
	// answers by).
	ErrRepositoryUnreadable = errors.New("the GitHub App cannot read the repository")
)

// responseLimit caps a GitHub API response body; a blob response is read
// through blobResponseLimit instead, since base64 with GitHub's line wrapping
// inflates notes.MaxFileSize bytes past responseLimit.
const (
	responseLimit     = 1 << 20
	blobResponseLimit = 2 << 20
)

// The Accept values request sends: GitHub's JSON for every call but Ref, which asks for a
// commit's SHA alone.
const (
	mediaTypeJSON = "application/vnd.github+json"
	mediaTypeSHA  = "application/vnd.github.sha"
)

// commitSHA is the whole of Ref's answer: one full commit SHA, lowercase hex.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// RequestTimeout bounds one API call -- the request and reading its body -- unless the caller's
// own context ends sooner, which always wins. GitHub terminates a request it has spent about
// ten seconds processing and answers its own error (documented for both the REST and the
// GraphQL API), so a deadline at ten seconds races GitHub's own: an answer that was about to
// arrive, success or GitHub's own timeout, is cut off and reported as a bare client timeout
// instead. This is that bound plus room for the connection and for transferring a response at
// responseLimit.
// A var rather than a const so a test can shrink it; nothing outside a test writes it.
var RequestTimeout = 20 * time.Second

// limitRecordTimeout bounds one write of a limit GitHub answered to the shared ledger. The write
// is one statement, which a healthy database answers in milliseconds, as the outbox's own
// records are bounded (outbox's recordTimeout).
const limitRecordTimeout = 5 * time.Second

// limitRecordContext is the context a limit GitHub answered is written to the shared ledger
// under: the caller's values without its deadline or its cancel, bounded by limitRecordTimeout.
// GitHub's answer is a fact about the App's budget whatever time its caller has left. A caller
// out of time -- a CI reconcile pass at its own bound -- would otherwise cut the write off, and
// every process would go on asking GitHub inside the limit GitHub had just named.
//
// It keeps the caller's values, so a keyed request's commit step (store.WithBeforeCommit, which
// api's request keys install) still runs in the ledger write: while the request's attempt holds
// its key, the write marks that key committed, as ratelimit's Record says. An attempt that has
// lost its key, past api's keyedHandlerLimit, has the mark refused, and the write of the limit
// with it.
func limitRecordContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), limitRecordTimeout)
}

// ResponseTooLargeError reports a response that did not fit inside a caller's configured limit.
type ResponseTooLargeError struct {
	Limit int64
}

func (err *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("GitHub response body exceeds the %d-byte limit", err.Limit)
}

const defaultBase = "https://api.github.com"

// tokenExpirySlack retires a cached installation token this long before
// GitHub's expiry so a token handed to a caller never dies mid-request.
const tokenExpirySlack = 5 * time.Minute

// Installation is the slice of GitHub's repository-installation response
// dispatch inspects. AccountLogin (the installation's own account -- a user or an org) is set
// only by ListInstallations (GET /app/installations), which is account-less in its own answer
// shape; RepositoryToken's own installation()/DeliveryPermissions() lookups never set it, since
// they already know the account from the owner/repo they resolved. AppID is the App's numeric id
// as the installation object names it, 0 where it names none. Permissions is the part of the
// installation's permissions Dispatch reads; Granted is every permission GitHub named for it, with
// its access, as GitHub named it (the rate reading logs them), and is empty, never nil, where
// GitHub named none.
type Installation struct {
	ID           int64
	AppID        int64
	AppSlug      string
	Permissions  auth.AppPerms
	Granted      map[string]string
	AccountLogin string
}

// Source is a successful access check: the installation that covers the
// repository and the slug of the App it belongs to.
type Source struct {
	InstallationID int64
	AppSlug        string
}

type cachedToken struct {
	token   string
	expires time.Time
	// permissions is what GitHub's access_tokens answer said this token holds. The client mints
	// with no narrowing, so it is what the installation had granted the App at the mint.
	permissions auth.AppPerms
}

// mintedToken is what the client remembers of a token it minted: the installation that scopes
// the token's calls, until GitHub's expiry for it.
type mintedToken struct {
	installation int64
	expires      time.Time
}

// ttlCache is a small time-bounded cache keyed by K, shared by ListInstallations (one fixed key),
// ListInstallationRepositoriesByID (one key per installation id) and RepositoryToken (one key per
// "owner/repo"). A singleflight.Group collapses concurrent cold-cache fetches for the same key
// into one network call: two goroutines racing a cold cache for the same installation (plausible
// under the cross-installation errgroup's own concurrency, or two browser reads of one repository
// through the GitHub proxy) share one fetch's answer instead of each making its own.
type ttlCache[K comparable, V any] struct {
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	items map[K]ttlCacheEntry[V]
	group singleflight.Group
}

type ttlCacheEntry[V any] struct {
	value V
	at    time.Time
}

func newTTLCache[K comparable, V any](ttl time.Duration, now func() time.Time) *ttlCache[K, V] {
	return &ttlCache[K, V]{ttl: ttl, now: now, items: map[K]ttlCacheEntry[V]{}}
}

// fresh returns key's cached value when younger than ttl.
func (c *ttlCache[K, V]) fresh(key K) (V, bool) {
	c.mu.Lock()
	entry, ok := c.items[key]
	c.mu.Unlock()
	if ok && c.now().Sub(entry.at) < c.ttl {
		return entry.value, true
	}
	var zero V
	return zero, false
}

// get returns key's cached value when fresh; otherwise it calls fetch exactly once across every
// concurrent caller for this key (singleflight, keyed by key's string form) and caches the
// result for the rest.
//
// The one fetch serves every waiting caller, so it runs on ctx without its cancellation or
// deadline: whichever caller started it may go away (a closed browser tab, an ended sync) while
// others still wait on the same answer, and one caller's cancellation must not fail them. Each
// request the fetch sends is still bounded by RequestTimeout. Each caller stops waiting when its
// own ctx ends, with ctx's error.
func (c *ttlCache[K, V]) get(ctx context.Context, key K, fetch func(context.Context) (V, error)) (V, error) {
	var zero V
	if value, ok := c.fresh(key); ok {
		return value, nil
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	shared := context.WithoutCancel(ctx)
	results := c.group.DoChan(fmt.Sprint(key), func() (any, error) {
		// Re-check: a sibling caller may have already refreshed this key while this one waited
		// to acquire the singleflight call.
		if value, ok := c.fresh(key); ok {
			return value, nil
		}
		fetched, err := fetch(shared)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.items[key] = ttlCacheEntry[V]{value: fetched, at: c.now()}
		c.mu.Unlock()
		return fetched, nil
	})
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case result := <-results:
		if result.Err != nil {
			return zero, result.Err
		}
		return result.Val.(V), nil
	}
}

// forget drops key's cached value, so the next get fetches it again.
func (c *ttlCache[K, V]) forget(key K) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

// Limits is the GitHub rate limits every process obeys together (ratelimit.Store, over
// github_rate_limits): GitHub budgets the App and each installation, not a process, so a limit
// one process meets must stop the others from asking too. Scopes are AppScope,
// InstallationScope's and GraphQLScope.
type Limits interface {
	// Wait is how long the limit in force on scope has left, zero when none is, and when that limit
	// was last recorded: the same for one limit however much of it has passed, changed by every
	// recording that makes a new limit or extends one.
	Wait(ctx context.Context, scope string) (wait time.Duration, recorded time.Time, err error)
	// Record records that GitHub asked for wait before the next request under scope.
	Record(ctx context.Context, scope string, wait time.Duration) error
	// Strike records a secondary limit under scope, whose wait doubles with each one met before
	// an hour of quiet (ratelimit.StrikeReset), as GitHub asks of a client that keeps meeting
	// one, and answers the row it wrote. A primary limit is Record's: GitHub's own reset ends it
	// whatever met it, and nothing is gained by holding the scope longer.
	Strike(ctx context.Context, scope string, wait time.Duration) (ratelimit.Limit, error)
}

// AppScope is the scope of every call the App JWT signs: installation lookups, token mints and the
// App webhook's delivery endpoints, which GitHub budgets as the App's.
const AppScope = "app"

// InstallationScope is the scope of every call made with installation id's token, which GitHub
// budgets as that installation's. It is exported so a caller outside this package -- a poller
// that budgets its own passes against an installation's limit -- can name the scope its
// requests are counted under.
func InstallationScope(id int64) string { return "installation:" + strconv.FormatInt(id, 10) }

// GraphQLScope is the scope of every GraphQL query the App makes, which GitHub budgets apart
// from the REST calls made with the same token: a GraphQL query is charged in points against a
// budget of its own, and GitHub answers a query past it with HTTP 200 and an `errors` entry of
// type RATE_LIMITED rather than with the 403 or 429 a REST call over its budget gets
// (GraphQLRateLimit).
//
// It is one scope for the App rather than one for each installation, where the REST scopes are
// two, because the budget it stands for is the App's: GitHub charges the App's GraphQL points
// whichever installation's token spends them. A primary limit on it leaves every REST call
// going -- a spent GraphQL budget says nothing about the REST budget of the installation whose
// token spent it, and the Dispatch processes read far more over REST than they query -- which
// is what a scope of its own buys, and it stops every process's queries at once rather than
// leaving each to find the limit for itself. A process that meets it can name this scope before
// it holds a token for any installation.
//
// A secondary limit is not recorded here. GitHub shares its secondary limits between REST and
// GraphQL for the installation whose token met one, so a query that meets one is recorded under
// that installation's scope, where it holds that installation's REST reads as well as its
// queries (recordGraphQLLimit). Only a limit met with a token this client did not mint, which
// names no installation, falls back to this scope.
const GraphQLScope = "graphql"

// Option configures a Client New builds.
type Option func(*Client)

// WithLimits makes the client obey, and record, the rate limits limits holds. Without it each
// client obeys only the answers GitHub gives it.
func WithLimits(limits Limits) Option {
	return func(c *Client) { c.limits = limits }
}

// WithRateCounter makes the client count its calls to GitHub on counter, for the rate reading
// (githubrate): every request it sends goes through counter's Transport, and every installation
// token it mints is registered with its installation, so each call made with that token is counted
// under that installation and the caller kind its context carries (githubrate.WithCaller). A
// transport alone could not do the second half, since only the mint knows whose token it is.
// Every client of one process that calls with installation tokens takes the one Counter, as
// Dispatch's two do (the API's, which the architecture importer and the delivery intakes and
// reconcile share, and the CI reconcile's), because GitHub budgets the installation and the
// reading reports what the process spent of it, whichever client spent it. A nil counter counts
// nothing, which is a client built without the option.
func WithRateCounter(counter *githubrate.Counter) Option {
	return func(c *Client) {
		if counter == nil {
			return
		}
		c.rate = counter
		c.http.Transport = counter.Transport(c.http.Transport)
	}
}

// Client calls the GitHub App API as the configured App. A nil *Client is the
// valid "no app credentials yet" state: every method returns ErrNoAppKey.
type Client struct {
	app  auth.AppConfig
	key  *rsa.PrivateKey
	base string
	http *http.Client
	now  func() time.Time
	// limits is the shared rate limits request obeys and records; nil is no shared limit.
	limits Limits
	// rate counts this client's calls to GitHub for the rate reading (WithRateCounter); nil
	// counts none.
	rate *githubrate.Counter

	mu sync.Mutex
	// tokens holds each installation's token in this process alone. GitHub budgets an
	// installation, not a token, so a token per process costs no more of the installation's limit
	// than one shared token would (the limit itself is shared, through limits), and every process's
	// token is valid at once; storing tokens in the database would put live credentials in Postgres
	// and its backups.
	tokens map[int64]cachedToken
	// installationOf maps each token this client minted to its installation and expiry, so a call
	// made with a token is made under that installation's scope (TokenScope). A token keeps its
	// entry until it expires, not only while it is the cached one: a caller that took it before
	// the next mint replaced it in tokens (a long sync, or a concurrent first mint) still holds a
	// token GitHub budgets under that installation. Each mint removes the expired entries, so the
	// map holds no more than the tokens still valid.
	installationOf map[string]mintedToken
	// repositories caches the installation that covers each "owner/repo", for RepositoryToken, for
	// installationsCacheTTL: an App moved to another installation is found within that, whatever
	// the old installation still answers. A token that cannot be minted drops the entry at once.
	repositories *ttlCache[string, int64]
	// installations caches ListInstallations' own answer under one fixed key
	// (installationsCacheKey): the installation list changes only when a human
	// installs/uninstalls the App, far slower than every reconcile pass needs to re-fetch it.
	installations *ttlCache[string, []Installation]
	// installationRepos caches ListInstallationRepositoriesByID's own answer per installation id,
	// for the same reason.
	installationRepos *ttlCache[int64, []string]
}

// installationsCacheKey is the one key installations (a single answer, not per-installation)
// caches under.
const installationsCacheKey = "installations"

// installationsCacheTTL bounds how long ListInstallations, ListInstallationRepositoriesByID and
// RepositoryToken trust their own cached answers before asking GitHub again.
const installationsCacheTTL = 10 * time.Minute

// New builds a client from the loaded App credentials. It returns (nil, nil)
// when app is nil or carries no private key — the caller keeps the nil client
// and methods answer ErrNoAppKey — and an error only for a malformed key, so
// a misconfigured deployment fails at boot. base overrides the GitHub API
// origin (DISPATCH_GITHUB_API_BASE); empty means api.github.com.
func New(app *auth.AppConfig, base string, options ...Option) (*Client, error) {
	if app == nil || app.PEM == "" {
		return nil, nil
	}
	key, err := githubappjwt.ParseKey(app.PEM)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	if base == "" {
		base = defaultBase
	}
	c := &Client{
		app:  *app,
		key:  key,
		base: strings.TrimSuffix(base, "/"),
		// No Timeout here: request applies RequestTimeout to each call's own context instead,
		// so a caller that needs a shorter one sets it on the context it passes.
		http:           &http.Client{},
		now:            time.Now,
		tokens:         map[int64]cachedToken{},
		installationOf: map[string]mintedToken{},
	}
	for _, option := range options {
		option(c)
	}
	nowFunc := func() time.Time { return c.now() }
	c.repositories = newTTLCache[string, int64](installationsCacheTTL, nowFunc)
	c.installations = newTTLCache[string, []Installation](installationsCacheTTL, nowFunc)
	c.installationRepos = newTTLCache[int64, []string](installationsCacheTTL, nowFunc)
	return c, nil
}

// appJWT signs the App-authentication JWT. GitHub accepts the App's client ID
// as iss (it is the one identifier LoadAppFromEnv requires; the numeric App ID
// is optional and zero in e2e).
func (c *Client) appJWT() (string, error) {
	return githubappjwt.Sign(c.app.ClientID, c.key, c.now())
}

// Installation resolves the App installation covering owner/repo and checks
// its Contents permission. A GitHub 404 is ErrNoInstallation; a missing or
// "none" Contents permission is ErrNoContentsRead. Does not consult
// RepositoryToken's installationID cache (c.repositories): that cache exists
// so RepositoryToken can skip resolving owner/repo again before minting a
// token by ID, but GitHub's installation-lookup endpoint already takes
// owner/repo directly in one GET -- there is no separate "resolve the ID"
// round trip here to skip, and a permissions check must read GitHub's
// current answer every call regardless (that is this function's whole job;
// a cached ID doesn't carry cached permissions with it, and an org admin can
// revoke a permission between calls).
func (c *Client) Installation(ctx context.Context, owner, repo string) (Installation, error) {
	installation, err := c.installation(ctx, owner, repo)
	if err != nil {
		return Installation{}, err
	}
	if installation.Permissions.Contents != "read" && installation.Permissions.Contents != "write" {
		return Installation{}, fmt.Errorf("%w on %s/%s", ErrNoContentsRead, owner, repo)
	}
	return installation, nil
}

// DeliveryPermissions resolves the App installation covering owner/repo and checks its Actions
// and Pull-requests permissions (LEGION-567's delivery timeline: the reconcile lists workflow
// runs/jobs and merged pull requests, never Contents). A GitHub 404 is ErrNoInstallation; a
// missing or "none" Actions permission is ErrNoActionsRead; a missing or "none" Pull-requests
// permission is ErrNoPullRequestsRead -- checked in that order, so a caller showing one error at
// a time names Actions first.
func (c *Client) DeliveryPermissions(ctx context.Context, owner, repo string) (Installation, error) {
	installation, err := c.installation(ctx, owner, repo)
	if err != nil {
		return Installation{}, err
	}
	if installation.Permissions.Actions != "read" && installation.Permissions.Actions != "write" {
		return Installation{}, fmt.Errorf("%w on %s/%s", ErrNoActionsRead, owner, repo)
	}
	if installation.Permissions.PullRequests != "read" && installation.Permissions.PullRequests != "write" {
		return Installation{}, fmt.Errorf("%w on %s/%s", ErrNoPullRequestsRead, owner, repo)
	}
	return installation, nil
}

// Installed says whether the App is installed on owner/repo, from the same installation lookup
// Installation and DeliveryPermissions make, with no permission check: GitHub's 404 is false. A
// nil client is ErrNoAppKey; any other failure of the lookup is returned as it is.
func (c *Client) Installed(ctx context.Context, owner, repo string) (bool, error) {
	if _, err := c.installation(ctx, owner, repo); err != nil {
		if errors.Is(err, ErrNoInstallation) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *Client) installation(ctx context.Context, owner, repo string) (Installation, error) {
	if c == nil {
		return Installation{}, ErrNoAppKey
	}
	jwt, err := c.appJWT()
	if err != nil {
		return Installation{}, err
	}
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	body, status, header, err := c.do(ctx, AppScope, http.MethodGet, target, "Bearer "+jwt)
	if err != nil {
		return Installation{}, err
	}
	if status == http.StatusNotFound {
		return Installation{}, fmt.Errorf("%w on %s/%s", ErrNoInstallation, owner, repo)
	}
	if limited := RateLimit(status, header, body); limited != nil {
		return Installation{}, limited
	}
	if status != http.StatusOK {
		return Installation{}, fmt.Errorf("GET %s: %s", ShownBody(target), StatusText(status, body))
	}
	var payload installationPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return Installation{}, fmt.Errorf("decode installation: %w", ShownErr(err))
	}
	return Installation{
		ID: payload.ID, AppID: payload.AppID, AppSlug: payload.AppSlug, Permissions: payload.Permissions.read,
		Granted: payload.Permissions.granted(),
	}, nil
}

// installationPayload is GitHub's one object shape for an installation, shared by
// GET /repos/{owner}/{repo}/installation (installation()) and GET /app/installations
// (ListInstallations): identical except that only the list form's answer needs Account, since a
// repo-scoped lookup already knows the account from its own owner/repo argument.
type installationPayload struct {
	ID      int64  `json:"id"`
	AppID   int64  `json:"app_id"`
	AppSlug string `json:"app_slug"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	Permissions installationPermissions `json:"permissions"`
}

// installationPermissions is an installation's permissions object as GitHub answers it, decoded
// once into the part Dispatch reads (auth.AppPerms) and once whole, every permission whose access
// is a string with its access.
type installationPermissions struct {
	read  auth.AppPerms
	named map[string]string
}

// UnmarshalJSON decodes GitHub's permissions object into both its forms. The whole form is read
// as values of any kind and keeps the strings: GitHub documents a string for every permission
// today, and a value of another kind, a permission GitHub adds later, must fail no decode of an
// installation, which every path that resolves one shares (RepositoryToken, DeliveryPermissions,
// CheckSource, Installed).
func (p *installationPermissions) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &p.read); err != nil {
		return err
	}
	var named map[string]any
	if err := json.Unmarshal(data, &named); err != nil {
		return err
	}
	p.named = make(map[string]string, len(named))
	for permission, access := range named {
		if access, ok := access.(string); ok {
			p.named[permission] = access
		}
	}
	return nil
}

// granted is every permission GitHub named, an empty map where it named none.
func (p installationPermissions) granted() map[string]string {
	if p.named == nil {
		return map[string]string{}
	}
	return p.named
}

// maxInstallations bounds ListInstallations' pagination the same way maxInstallationRepositories
// bounds a single installation's own repository list: a real-world App installed on far more
// accounts than this should fail loudly rather than grow an unbounded page loop forever.
const maxInstallations = 2000

// ListInstallations lists every installation of this App (GET /app/installations,
// JWT-authenticated, paginated): LEGION-567's merged-PR search must cover every installation the
// App has, not only the one covering the configured deploy repository, since the population rule
// (an author allowlist) names no installation or org boundary. Cached for installationsCacheTTL,
// as RepositoryToken's owner/repo cache is: the install list changes only when a human
// installs/uninstalls the App, far slower than any reconcile pass needs a fresh answer. Unlike
// this cache's own pre-ttlCache form, a zero-installations answer is now cached too
// (ttlCache.fresh tests presence, not a non-nil slice): an App installed nowhere re-fetches at
// most once per TTL instead of on every call.
func (c *Client) ListInstallations(ctx context.Context) ([]Installation, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	return c.installations.get(ctx, installationsCacheKey, c.listInstallations)
}

// ReadInstallations is ListInstallations as GitHub answers it now, read past the cache and
// leaving it as it was. The rate reading lists the App's installations at every pass and writes
// one line for each installation the list held, naming how many it held: the cache's ten minutes
// are the reading's whole interval, so through it a pass would read an installation removed since
// and miss one added since for up to a pass, and name a count GitHub no longer answers.
func (c *Client) ReadInstallations(ctx context.Context) ([]Installation, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	return c.listInstallations(ctx)
}

// listInstallations reads every page of GET /app/installations under the App's JWT.
func (c *Client) listInstallations(ctx context.Context) ([]Installation, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	var installations []Installation
	for page := 1; ; page++ {
		target := fmt.Sprintf("%s/app/installations?per_page=100&page=%d", c.base, page)
		body, status, header, err := c.request(ctx, AppScope, http.MethodGet, target, "Bearer "+jwt, mediaTypeJSON, "", nil, responseLimit)
		if err != nil {
			return nil, fmt.Errorf("list installations (page %d): %w", page, err)
		}
		if err := CheckResponse(status, header, body); err != nil {
			return nil, fmt.Errorf("list installations (page %d): %w", page, err)
		}
		var payload []installationPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode installations (page %d): %w", page, ShownErr(err))
		}
		if len(payload) == 0 {
			break
		}
		for _, entry := range payload {
			installations = append(installations, Installation{
				ID: entry.ID, AppID: entry.AppID, AppSlug: entry.AppSlug, Permissions: entry.Permissions.read,
				Granted: entry.Permissions.granted(), AccountLogin: entry.Account.Login,
			})
		}
		if len(installations) > maxInstallations {
			return nil, fmt.Errorf("list installations: the App has more than %d installations", maxInstallations)
		}
		if len(payload) < 100 {
			break
		}
	}
	return installations, nil
}

// AppIdentity is the App's numeric id and its slug as GitHub's GET /app answers them to the App's
// JWT. The configuration's own id and slug can be absent (the JWT names the App by its client ID
// alone, and an end-to-end App has neither), and GitHub's answer is the one each of them is a
// copy of.
func (c *Client) AppIdentity(ctx context.Context) (id int64, slug string, err error) {
	if c == nil {
		return 0, "", ErrNoAppKey
	}
	jwt, err := c.appJWT()
	if err != nil {
		return 0, "", err
	}
	target := c.base + "/app"
	body, status, header, err := c.do(ctx, AppScope, http.MethodGet, target, "Bearer "+jwt)
	if err != nil {
		return 0, "", err
	}
	if err := CheckResponse(status, header, body); err != nil {
		return 0, "", fmt.Errorf("GET %s: %w", target, err)
	}
	var payload struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, "", fmt.Errorf("decode the App: %w", ShownErr(err))
	}
	if payload.ID <= 0 || payload.Slug == "" {
		return 0, "", fmt.Errorf("GET %s answered no App id or slug", target)
	}
	return payload.ID, payload.Slug, nil
}

// RepositoryToken returns an installation token of the installation covering owner/repo. The
// installation a repository resolves to is remembered for installationsCacheTTL; a token that
// cannot be minted for it forgets it at once, so an App moved between installations is looked up
// again within the TTL, or sooner when the old installation is gone.
func (c *Client) RepositoryToken(ctx context.Context, owner, repo string) (string, error) {
	token, _, err := c.RepositoryTokenWithStatuses(ctx, owner, repo)
	return token, err
}

// RepositoryTokenWithStatuses is RepositoryToken with whether that token holds Commit statuses at
// read or write, which is what Dispatch's CI reconcile decides by whether it reads a head's commit
// statuses at all. Both answers come from the one cached token: the permissions are the ones
// GitHub's access_tokens answer named for that token, decoded and kept beside it in the one cache
// entry (token), so a token is never answered beside another mint's permissions.
//
// The permission is the token's rather than the installation's, although the installation lookup
// that resolves owner/repo names the installation's permissions too. A token lives about an hour
// and is kept until tokenExpirySlack before its expiry, while the installation is remembered apart
// for installationsCacheTTL. After a person accepts Commit statuses for the App, the installation
// says the App holds it while the token in hand does not, for up to an hour, and every head the
// reconcile read in that time would send a status read GitHub refuses, at every pass. The token's
// own permissions are exact for each request the token makes. A grant can still be taken away
// while a token lives, so a caller still meets a refused status read, which costs the head its
// statuses alone.
func (c *Client) RepositoryTokenWithStatuses(ctx context.Context, owner, repo string) (string, bool, error) {
	if c == nil {
		return "", false, ErrNoAppKey
	}
	name := owner + "/" + repo
	installationID, err := c.repositories.get(ctx, name, func(ctx context.Context) (int64, error) {
		installation, err := c.installation(ctx, owner, repo)
		return installation.ID, err
	})
	if err != nil {
		return "", false, err
	}
	minted, err := c.token(ctx, installationID)
	if err != nil {
		c.repositories.forget(name)
		return "", false, err
	}
	statuses := minted.permissions.Statuses == "read" || minted.permissions.Statuses == "write"
	return minted.token, statuses, nil
}

// installationRepositoriesPerPage is GitHub's own page size for GET /installation/repositories.
const installationRepositoriesPerPage = 100

// maxInstallationRepositories bounds ListInstallationRepositoriesByID the same way
// delivery.maxDeliveryWindow/maxRunsPerWindow/maxPullRequestsPerWindow bound their own queries:
// an installation covering a very large org should fail loudly past this rather than build an
// ever-growing, unbounded `repo:` qualifier list on every 5-minute reconcile pass.
const maxInstallationRepositories = 2000

// ListInstallationRepositoriesByID lists every repository ("owner/name") installation installs
// -- GET /installation/repositories (an installation-token endpoint, paginated), under a token
// minted directly by id (ListInstallations already resolved the id; there is no representative
// owner/repo to mint one through RepositoryToken's own owner/repo-keyed cache). LEGION-567's
// merged-PR search needs this to scope its query to exactly each installation's own repositories:
// an unqualified GitHub search query is NOT scoped by the authenticating token for
// public-repository content -- it searches all of public GitHub (confirmed live against a real
// installation token) -- so the caller must supply an explicit repo: qualifier per repository
// instead of relying on the token alone. Cached for installationsCacheTTL per installation id,
// for the same reason ListInstallations caches its own answer: round 4's reconcile pass refetched
// every installation's full repository list on every pass, multiplying the across-installation
// search's own cost by the installation count for data that changes on human timescales, not
// every five minutes.
func (c *Client) ListInstallationRepositoriesByID(ctx context.Context, installationID int64) ([]string, error) {
	return c.installationRepos.get(ctx, installationID, func(ctx context.Context) ([]string, error) {
		token, err := c.Token(ctx, installationID)
		if err != nil {
			return nil, fmt.Errorf("mint token for installation %d: %w", installationID, err)
		}
		return listInstallationRepositories(ctx, c, token)
	})
}

func listInstallationRepositories(ctx context.Context, c *Client, token string) ([]string, error) {
	var names []string
	for page := 1; ; page++ {
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", installationRepositoriesPerPage, page)
		body, status, header, err := c.Read(ctx, token, path)
		if err != nil {
			return nil, fmt.Errorf("list installation repositories (page %d): %w", page, err)
		}
		if err := CheckResponse(status, header, body); err != nil {
			return nil, fmt.Errorf("list installation repositories (page %d): %w", page, err)
		}
		var payload struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode installation repositories (page %d): %w", page, ShownErr(err))
		}
		if len(payload.Repositories) == 0 {
			break
		}
		for _, repository := range payload.Repositories {
			names = append(names, repository.FullName)
		}
		if len(names) > maxInstallationRepositories {
			return nil, fmt.Errorf("list installation repositories: installation covers more than %d repositories", maxInstallationRepositories)
		}
		if len(payload.Repositories) < installationRepositoriesPerPage {
			break
		}
	}
	return names, nil
}

// Read performs GET path (an API path with its query, under the API origin) with an
// installation token, returning GitHub's complete answer when it fits within the response limit.
func (c *Client) Read(ctx context.Context, token, path string) ([]byte, int, http.Header, error) {
	return c.ReadWithin(ctx, token, path, responseLimit)
}

// ReadWithin is Read with the caller's own body limit, for a caller that keeps only part of a
// large answer; a body past limit is a *ResponseTooLargeError.
func (c *Client) ReadWithin(ctx context.Context, token, path string, limit int64) ([]byte, int, http.Header, error) {
	return c.ReadConditionalWithin(ctx, token, path, "", limit)
}

// ReadConditionalWithin is ReadWithin with the ETag a caller remembered from this URL's last
// answer: it sends If-None-Match, so GitHub answers 304 Not Modified when nothing changed, which
// costs none of the rate limit. A 304 is a status the caller reads, with an empty body, never an
// error. An empty etag sends no If-None-Match header, which is an unconditional read.
func (c *Client) ReadConditionalWithin(ctx context.Context, token, path, etag string, limit int64) ([]byte, int, http.Header, error) {
	if c == nil {
		return nil, 0, nil, ErrNoAppKey
	}
	return c.request(ctx, c.TokenScope(token), http.MethodGet, c.base+path, "Bearer "+token, mediaTypeJSON, etag, nil, limit)
}

// GraphQL posts query/variables to the API origin's /graphql endpoint with an installation token,
// returning GitHub's complete JSON response body (an `{data, errors}` envelope the caller
// decodes) when it fits within the response limit. Some GitHub Apps' installation tokens can read
// a repository's REST endpoints but are refused by the REST `/search/issues` endpoint for a
// private repository ("cannot be searched... do not have permission") -- GraphQL's `search`
// connection does not share that restriction, which is why delivery/github_prs.go's merged-PR
// search uses this instead of REST search.
//
// It is held by two of the shared rate limits rather than one. The token's own installation
// scope holds it as it holds every REST call made with that token (send), and GraphQLScope holds
// it as GitHub's separate GraphQL budget: a query sent while either is in force is not sent at
// all and answers a held *RateLimitError, and an answer GraphQLRateLimit reads as a GraphQL
// limit -- which request's own REST detection cannot see, since GitHub answers one with HTTP 200
// as readily as with 403 -- is recorded and returned as that error rather than as an answer. The
// caller therefore never has to tell a rate-limited 200 from a good one.
//
// Which scope that limit is recorded under is which of GitHub's two limits it is
// (recordGraphQLLimit), so the query calls send itself rather than request, whose REST detection
// would record both of them under the installation's scope.
//
// A held limit and a limit read in an answer are noted on the attempt ctx carries (githubreads),
// here and not by send, which notes no limit GitHub answers: GitHub answers a spent GraphQL
// budget with 200, which no reading of the status alone can see. The limit GraphQLRateLimit reads
// is noted once recordGraphQLLimit has recorded it, so the cause carries the wait every process
// now holds. A limit read in a 200 is worded in GitHub's own words, never its body, which is
// the query's answer (answeredLimit, graphQLLimitWording): the envelope's entries that name the
// limit, or, for the answer that spends the last point, which carries its data and no error, the
// headers that name the limit. That data can be a private repository's, and GET /healthz serves
// the cause to whoever reaches it, with no credential, so neither the note nor the error the
// caller is given carries it, however a loop wraps and notes that error. The caller is given the
// answer's body as the first result beside the error, and reads the data there if at all.
// A client without Limits reads the answer as request reads a REST call's, and notes the limit
// RateLimit reads, which is only ever a 403 or a 429: GitHub's refusal, which carries no data.
func (c *Client) GraphQL(ctx context.Context, token string, query string, variables map[string]any) ([]byte, int, http.Header, error) {
	if c == nil {
		return nil, 0, nil, ErrNoAppKey
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encode GraphQL request: %w", err)
	}
	if c.limits != nil {
		wait, recorded, err := c.limits.Wait(ctx, GraphQLScope)
		switch {
		case err != nil:
			slog.Warn("dispatch githubapp: the shared GitHub GraphQL rate limit was not read; the query is sent", "scope", GraphQLScope, "error", err)
		case wait > 0:
			held := &RateLimitError{Wait: wait, Scope: GraphQLScope, Held: true, Recorded: recorded}
			noteFailure(ctx, held)
			return nil, 0, nil, held
		}
	}
	installation := c.TokenScope(token)
	target := c.base + "/graphql"
	body, status, header, err := c.send(ctx, installation, http.MethodPost, target, "Bearer "+token, mediaTypeJSON, "", bytes.NewReader(payload), responseLimit)
	if err != nil {
		return body, status, header, err
	}
	if c.limits == nil {
		if limited := RateLimit(status, header, body); limited != nil {
			noteFailure(ctx, limitNoted(http.MethodPost, target, limited))
		}
		return body, status, header, nil
	}
	limited, secondary := GraphQLRateLimit(status, header, body)
	if limited == nil {
		return body, status, header, nil
	}
	c.recordGraphQLLimit(ctx, installation, limited, secondary)
	// Noted once recorded, so the cause carries the wait every process now holds, which a strike
	// can make longer than GitHub's own.
	noteFailure(ctx, limitNoted(http.MethodPost, target, limited))
	return body, status, header, limited
}

// recordGraphQLLimit records the limit a query met under the scope GitHub budgets it against,
// and names that scope on the error the caller is given.
//
// A primary limit is the App's GraphQL points spent, which GitHub budgets apart from the REST
// calls made with the same token: it goes under GraphQLScope, where it stops every process's
// queries and leaves every process's REST reads going, since a spent GraphQL budget says nothing
// about any installation's REST budget.
//
// A secondary limit -- too many points a minute, too many requests at once -- is shared between
// REST and GraphQL for the installation whose token met it, so it goes under that installation's
// scope, where it holds that installation's REST reads too; the pass's own queries are held by
// it as well, since every query reads the token's scope before it is sent. It is recorded with
// Strike rather than Record because GitHub asks a client that keeps meeting one to back off
// further each time.
//
// A token this client did not mint has no installation scope to name (TokenScope). Such a
// secondary limit falls back to GraphQLScope: holding the App's queries is less than GitHub asks
// for, and it is all this client knows to hold.
func (c *Client) recordGraphQLLimit(ctx context.Context, installation string, limited *RateLimitError, secondary bool) {
	record, cancel := limitRecordContext(ctx)
	defer cancel()
	if secondary && installation != "" {
		limited.Scope = installation
		struck, err := c.limits.Strike(record, installation, limited.Wait)
		if err != nil {
			slog.Error("dispatch githubapp: GitHub's secondary rate limit was not recorded; other processes may ask GitHub inside it", "scope", installation, "wait", limited.Wait, "error", err)
			return
		}
		// The caller waits out the limit every process now reads, which the strike's own
		// doubling can make longer than the wait GitHub asked for.
		limited.Wait = max(limited.Wait, struck.Wait)
		return
	}
	limited.Scope = GraphQLScope
	if err := c.limits.Record(record, GraphQLScope, limited.Wait); err != nil {
		slog.Error("dispatch githubapp: GitHub's GraphQL rate limit was not recorded; other processes may query inside it", "scope", GraphQLScope, "wait", limited.Wait, "error", err)
	}
}

// Token returns an installation access token, minting one only when the
// cached token is within tokenExpirySlack of GitHub's expiry.
func (c *Client) Token(ctx context.Context, installationID int64) (string, error) {
	minted, err := c.token(ctx, installationID)
	return minted.token, err
}

// token is Token with the permissions GitHub's access_tokens answer named for the token, which
// are kept in the one cache entry beside the token they came with.
func (c *Client) token(ctx context.Context, installationID int64) (cachedToken, error) {
	if c == nil {
		return cachedToken{}, ErrNoAppKey
	}
	c.mu.Lock()
	cached, ok := c.tokens[installationID]
	c.mu.Unlock()
	if ok && c.now().Before(cached.expires.Add(-tokenExpirySlack)) {
		return cached, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return cachedToken{}, err
	}
	target := c.base + "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	body, status, header, err := c.do(ctx, AppScope, http.MethodPost, target, "Bearer "+jwt)
	if err != nil {
		return cachedToken{}, err
	}
	if limited := RateLimit(status, header, body); limited != nil {
		return cachedToken{}, limited
	}
	if status != http.StatusCreated {
		return cachedToken{}, fmt.Errorf("POST %s: %s", ShownBody(target), StatusText(status, body))
	}
	var payload struct {
		Token       string        `json:"token"`
		ExpiresAt   time.Time     `json:"expires_at"`
		Permissions auth.AppPerms `json:"permissions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return cachedToken{}, fmt.Errorf("decode installation token: %w", ShownErr(err))
	}
	fresh := cachedToken{token: payload.Token, expires: payload.ExpiresAt, permissions: payload.Permissions}
	// Registered before any caller holds the token, so its first call is counted too.
	if c.rate != nil {
		c.rate.Register(payload.Token, strconv.FormatInt(installationID, 10))
	}
	c.mu.Lock()
	now := c.now()
	for token, minted := range c.installationOf {
		if !now.Before(minted.expires) {
			delete(c.installationOf, token)
		}
	}
	c.tokens[installationID] = fresh
	c.installationOf[payload.Token] = mintedToken{installation: installationID, expires: payload.ExpiresAt}
	c.mu.Unlock()
	return fresh, nil
}

// CheckSource proves end to end that the App can read owner/repo: the
// installation exists with Contents: read, a token mints, and the repository
// resolves under that token. Failures a human can fix are the typed errors
// above; anything else is an upstream failure.
func (c *Client) CheckSource(ctx context.Context, owner, repo string) (Source, error) {
	if c == nil {
		return Source{}, ErrNoAppKey
	}
	installation, err := c.Installation(ctx, owner, repo)
	if err != nil {
		return Source{}, err
	}
	token, err := c.Token(ctx, installation.ID)
	if err != nil {
		return Source{}, err
	}
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	body, status, _, err := c.do(ctx, InstallationScope(installation.ID), http.MethodGet, target, "Bearer "+token)
	if err != nil {
		return Source{}, err
	}
	if status == http.StatusNotFound {
		return Source{}, fmt.Errorf("%w: its token cannot read %s/%s", ErrNoInstallation, owner, repo)
	}
	if status != http.StatusOK {
		return Source{}, fmt.Errorf("GET %s: %s", ShownBody(target), StatusText(status, body))
	}
	return Source{InstallationID: installation.ID, AppSlug: installation.AppSlug}, nil
}

// TokenScope is the shared rate-limit scope of a call made with token: its installation's when
// this client minted the token, until the next mint after its expiry forgets it, and empty
// otherwise, since nothing then says whose it is.
func (c *Client) TokenScope(token string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if minted, ok := c.installationOf[token]; ok {
		return InstallationScope(minted.installation)
	}
	return ""
}

// OwnCalls is installation id's own calls on each of its two counters, REST core and GraphQL, by
// caller kind, since each counter's last reset (githubrate.Counter.Own): the calls of every
// client that counts on this client's Counter, which in Dispatch is the whole process's. Both are
// empty, never nil, for a client built without one (WithRateCounter).
func (c *Client) OwnCalls(installationID int64) (core, graphql map[string]int) {
	if c == nil || c.rate == nil {
		return map[string]int{}, map[string]int{}
	}
	return c.rate.Own(strconv.FormatInt(installationID, 10))
}

// SecondaryUntil is the end of the latest secondary limit GitHub answered one of installation
// id's tokens with, as the client's Counter noted it from GitHub's answer
// (githubrate.Counter.SecondaryUntil), and the zero time when none was or the client has no
// Counter. The Counter is the process's, so the limits every client of the process that counts
// through it met are here, and another process's are not.
func (c *Client) SecondaryUntil(installationID int64) time.Time {
	if c == nil || c.rate == nil {
		return time.Time{}
	}
	return c.rate.SecondaryUntil(strconv.FormatInt(installationID, 10))
}

func (c *Client) do(ctx context.Context, scope, method, target, authorization string) ([]byte, int, http.Header, error) {
	return c.doLimited(ctx, scope, method, target, authorization, responseLimit)
}

func (c *Client) doLimited(ctx context.Context, scope, method, target, authorization string, limit int64) ([]byte, int, http.Header, error) {
	return c.request(ctx, scope, method, target, authorization, mediaTypeJSON, "", nil, limit)
}

// request performs one API call under scope asking for the accept media type and returns its
// complete body when it fits within limit, status and headers. body is nil for every GET call;
// GraphQL is this package's only POST with a body. A non-empty ifNoneMatch sends that ETag as
// If-None-Match, so GitHub answers 304 Not Modified when its answer has not changed since the
// caller read it, and charges the rate limit nothing for it; an empty one sends no such header.
//
// It is send with GitHub's own REST limit read, once (keepAnsweredLimit). Every REST call of this
// package goes through here but ReadPastHold's, which keeps the limit its answer names the same
// way. GraphQL calls send itself and reads, notes and records the limit in its answer itself,
// which is another detection and another scope (recordGraphQLLimit).
func (c *Client) request(ctx context.Context, scope, method, target, authorization, accept, ifNoneMatch string, body io.Reader, limit int64) ([]byte, int, http.Header, error) {
	responseBody, status, header, err := c.send(ctx, scope, method, target, authorization, accept, ifNoneMatch, body, limit)
	if err != nil {
		return responseBody, status, header, err
	}
	c.keepAnsweredLimit(ctx, scope, method, target, status, header, responseBody)
	return responseBody, status, header, nil
}

// keepAnsweredLimit reads GitHub's answer to a REST call, method to target, for a limit: one
// RateLimit reads in it is noted on the attempt ctx carries (githubreads), whatever the client and
// the scope, and recorded under scope with GitHub's own wait where the client has Limits and scope
// is named, so no process sends GitHub anything under that scope before the wait ends.
func (c *Client) keepAnsweredLimit(ctx context.Context, scope, method, target string, status int, header http.Header, body []byte) {
	limited := RateLimit(status, header, body)
	if limited == nil {
		return
	}
	noteFailure(ctx, limitNoted(method, target, limited))
	if c.limits != nil && scope != "" {
		// Recorded past the caller's own end: see limitRecordContext.
		record, cancel := limitRecordContext(ctx)
		err := c.limits.Record(record, scope, limited.Wait)
		cancel()
		if err != nil {
			slog.Error("dispatch githubapp: GitHub's rate limit was not recorded; other processes may ask GitHub inside it", "scope", scope, "wait", limited.Wait, "error", err)
		}
	}
}

// ReadPastHold is Read for the rate reading (githubrate): GET path with an installation token,
// sent whatever limit the shared ledger holds the token's installation to. The reading reads an
// installation's counter, and GitHub names the counter in every answer it gives, a refusal at the
// limit among them, so a hold would keep the reading from the hour it is most needed for: one in
// which the installation spent its budget, whose reading before the reset would otherwise be an
// error with no count. GitHub asks that a client send nothing while limited, and this read does
// send past a primary limit: one core request of a held installation a pass (two at its first pass
// and after a 404, when its repository is listed again), and a pass every ten minutes and before
// each counter reset of every installation, so about 6 + 2N core requests of a held installation
// an hour from each process reading, N being the App's installations. The reading sends nothing
// while a secondary limit holds the installation (SecondaryUntil), since GitHub may ban an
// integration that goes on through one. A limit GitHub answers it with is noted and recorded under
// the installation's scope as Read's is (keepAnsweredLimit), so every other read of the
// installation holds, and a refused answer is the caller's, with its status and its headers, never
// an error.
func (c *Client) ReadPastHold(ctx context.Context, token, path string) ([]byte, int, http.Header, error) {
	if c == nil {
		return nil, 0, nil, ErrNoAppKey
	}
	target := c.base + path
	// A concurrent mint can prune this token after its expiry while the request is in flight.
	// Keep the installation that sent it, so a late refusal still holds the right scope.
	scope := c.TokenScope(token)
	// No scope, so send obeys no shared limit; the limit the answer names is kept under the
	// token's own scope below.
	body, status, header, err := c.send(ctx, "", http.MethodGet, target, "Bearer "+token, mediaTypeJSON, "", nil, responseLimit)
	if err != nil {
		return body, status, header, err
	}
	c.keepAnsweredLimit(ctx, scope, http.MethodGet, target, status, header, body)
	return body, status, header, nil
}

// send is the one function that sends anything to GitHub, so it is where the shared limit on
// scope is obeyed: while a limit is in force on it nothing is sent and a held *RateLimitError is
// returned. A limit that cannot be read (the database failing) is logged and the request goes
// ahead, as embedqueue.reserveTokens goes ahead: GitHub still answers each process's own
// requests with the limit. An empty scope, or a client without Limits, is no shared limit, which
// is how ReadPastHold is sent past it.
//
// What GitHub's own answer says about a limit is the caller's to read, note and record, since
// REST and GraphQL read an answer differently and budget it under different scopes: request
// does it for a REST call and GraphQL for a query.
//
// It is also where the attempt ctx carries (githubreads) learns of every request a leased pass
// sends: each request is marked sent, and each failure no caller takes as data, but a limit
// GitHub answers, is noted: no answer at all, a body that could not be read or ran past limit, a
// held limit (noted, though nothing was sent), a 5xx and a 401. A context with no attempt changes
// nothing.
//
// Each error send words itself names target as ShownBody shows it. A target holds names Dispatch
// keeps and text GitHub gave (a blob's sha from a tree answer, a next page link), so none of them
// passes the bound in send's errors.
func (c *Client) send(ctx context.Context, scope, method, target, authorization, accept, ifNoneMatch string, body io.Reader, limit int64) ([]byte, int, http.Header, error) {
	if c.limits != nil && scope != "" {
		wait, recorded, err := c.limits.Wait(ctx, scope)
		switch {
		case err != nil:
			slog.Warn("dispatch githubapp: the shared GitHub rate limit was not read; the request is sent", "scope", scope, "error", err)
		case wait > 0:
			held := &RateLimitError{Wait: wait, Scope: scope, Held: true, Recorded: recorded}
			noteFailure(ctx, held)
			return nil, 0, nil, held
		}
	}
	bounded, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(bounded, method, target, body)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build %s %s: %w", method, ShownBody(target), ShownErr(err))
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", accept)
	if ifNoneMatch != "" {
		request.Header.Set("If-None-Match", ifNoneMatch)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	githubreads.NoteSent(ctx)
	response, err := c.http.Do(request)
	if err != nil {
		err = fmt.Errorf("%s %s: %w", method, ShownBody(target), ShownErr(err))
		noteFailure(ctx, err)
		return nil, 0, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		err = fmt.Errorf("read %s %s response: %w", method, ShownBody(target), ShownErr(err))
		noteFailure(ctx, err)
		return nil, 0, nil, err
	}
	if int64(len(responseBody)) > limit {
		err := fmt.Errorf("%s %s: %w", method, ShownBody(target), &ResponseTooLargeError{Limit: limit})
		noteFailure(ctx, err)
		return nil, 0, nil, err
	}
	// An outage status (httpoutage.Is: a 408 or a 5xx) and a 401 say GitHub could not answer
	// or did not take the App's credentials; its 429 is a limit, noted as one. Every other status, a
	// limit aside, is left to the caller, because the same answer is data at one point and a failure
	// at another (a 404 is a deleted repository to one caller and a wrong workflow path to the
	// next), and only the caller can tell the two apart.
	if status := response.StatusCode; status != http.StatusTooManyRequests && httpoutage.Is(status) || status == http.StatusUnauthorized {
		noteFailure(ctx, &failedAnswer[[]byte]{method: method, target: target, status: status, detail: responseBody})
	}
	return responseBody, response.StatusCode, response.Header, nil
}

// noteFailure notes err on the attempt ctx carries (githubreads) while ctx, the caller's own
// context, is live. A request its caller's context ended is no failure of GitHub's: the pass's
// own bound or this process's shutdown cut it off. The client's own RequestTimeout is not the
// caller's, so a request it ends is noted.
func noteFailure(ctx context.Context, err error) {
	if ctx.Err() == nil {
		githubreads.NoteFailure(ctx, err)
	}
}

// failedAnswer is an answer send, request or GraphQL notes as a failure, worded as the client's
// own errors are: "<METHOD> <target>: status <n>: <body>" (StatusText) for a 408, a 5xx or a 401,
// and "<METHOD> <target>: rate-limited by GitHub (status <n>), next request in <wait>: <body>" for
// a limit GitHub answered (limitNoted, RateLimitError), each with target as ShownBody shows it, as
// send shows it, and each ending at its head where there is no body (withBody). Its text is made
// only where the note can still become a cause (githubreads.NoteFailure), so a request outside the
// leased passes formats nothing. The target carries no token: every token travels in the
// Authorization header.
//
// Its text carries a body only as StatusText and RateLimitError word one, never at a 2xx status:
// for a 408, a 5xx or a 401 detail, the body send read, kept as the bytes it read, and for a limit
// its Body, or at a 2xx GitHub's wording of the limit (answeredLimit) and never Body. So no
// failedAnswer has a text that carries a 2xx answer's data.
//
// It is not the only note of a limit. A limit github_rate_limits already holds is noted as its own
// held *RateLimitError (send, GraphQL), and a loop that a limit stops notes words of its own: the
// CI reconcile's stop.ended, and the redelivery sweep's "a GitHub rate limit stopped the webhook
// redelivery sweep" (Sweeper.sweepAndReport).
type failedAnswer[Detail string | []byte] struct {
	method, target string
	status         int
	detail         Detail
	// limit is the limit GitHub answered, nil for a 5xx or a 401, and never a held one, which is
	// noted as it is.
	limit *RateLimitError
}

func (f *failedAnswer[Detail]) Error() string {
	if f.limit != nil {
		return f.method + " " + ShownBody(f.target) + ": " + f.limit.Error()
	}
	return f.method + " " + ShownBody(f.target) + ": " + StatusText(f.status, f.detail)
}

// limitNoted is the client's note of limited, a limit GitHub answered method's request to target
// with (request, GraphQL), worded by limited's own text: GitHub's refusal, or at a 2xx GitHub's
// wording of the limit and nothing of the answer's data (RateLimitError). It notes no held limit,
// which is noted as its own error (send, GraphQL), and none of the limits a loop words itself
// (failedAnswer).
func limitNoted(method, target string, limited *RateLimitError) *failedAnswer[string] {
	return &failedAnswer[string]{method: method, target: target, limit: limited}
}

// Ref resolves branch to its commit SHA under an installation token
// (`GET /repos/{owner}/{repo}/commits/{branch}`). That read resolves any
// commit-ish, so a SHA, `main~1` or `refs/heads/main` resolves too: Ref answers
// a head, and it does not prove that branch names a branch. A source's save
// proves that with Branch. GitHub answers a value that resolves to nothing with
// 422 and a repository it cannot find or the installation cannot see with 404;
// both are ErrNoBranch. An empty repository answers 409 per GitHub's docs,
// which passes through as a read error.
// It asks for the SHA alone (`Accept: application/vnd.github.sha`), so the
// commit's file list, which GitHub's JSON answer carries, never counts
// against responseLimit. An answer that is not one 40-hex SHA is an error,
// never a SHA: a Dir read at it would answer an empty model.
func (c *Client) Ref(ctx context.Context, token, owner, repo, branch string) (string, error) {
	if c == nil {
		return "", ErrNoAppKey
	}
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/commits/" + url.PathEscape(branch)
	body, status, _, err := c.request(ctx, c.TokenScope(token), http.MethodGet, target, "Bearer "+token, mediaTypeSHA, "", nil, responseLimit)
	if err != nil {
		return "", err
	}
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return "", fmt.Errorf("%w: %s on %s/%s", ErrNoBranch, branch, owner, repo)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", ShownBody(target), StatusText(status, body))
	}
	sha := strings.TrimSpace(string(body))
	if !commitSHA.MatchString(sha) {
		return "", fmt.Errorf("commit for %s/%s@%s: GitHub answered no commit SHA (%d bytes)", owner, repo, branch, len(body))
	}
	return sha, nil
}

// Branch proves branch is the name of a branch owner/repo has and answers its head commit SHA,
// with one exact read under an installation token (`GET /repos/{owner}/{repo}/git/ref/heads/{branch}`,
// the name path-escaped so a `/` in it travels as %2F). A source's save checks its branch with
// Branch rather than Ref: Ref's `commits/{ref}` resolves any commit-ish, so a SHA, `main~1`,
// `HEAD` or `refs/heads/main` would pass it and be stored as the branch, while the exact read
// answers 404 for every one of them and for a prefix of a branch's name. 404 (no such branch,
// or a repository GitHub cannot find or the installation cannot see), 409 (an empty repository)
// and 422 are ErrNoBranch. A 200 naming another ref, or carrying no 40-hex SHA, is ErrNoBranch
// too: GitHub's exact read never answers one, but the check does not trust that it cannot.
func (c *Client) Branch(ctx context.Context, token, owner, repo, branch string) (string, error) {
	if c == nil {
		return "", ErrNoAppKey
	}
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/git/ref/heads/" + url.PathEscape(branch)
	body, status, _, err := c.do(ctx, c.TokenScope(token), http.MethodGet, target, "Bearer "+token)
	if err != nil {
		return "", err
	}
	noBranch := fmt.Errorf("%w: %s on %s/%s", ErrNoBranch, branch, owner, repo)
	switch status {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		return "", noBranch
	default:
		return "", fmt.Errorf("GET %s: %s", ShownBody(target), StatusText(status, body))
	}
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		return "", fmt.Errorf("decode branch %s of %s/%s: %w", branch, owner, repo, ShownErr(err))
	}
	if ref.Ref != "refs/heads/"+branch || !commitSHA.MatchString(ref.Object.SHA) {
		return "", noBranch
	}
	return ref.Object.SHA, nil
}

// Dir is one directory listing at a commit: the directory's own tree object
// id (identical files give an identical SHA, whatever else the commit
// changed) and the files its keep rule accepts.
type Dir struct {
	// SHA is the subtree object id; empty when the directory does not exist
	// at the commit.
	SHA   string
	Files []DirFile
}

// DirFile is one file directly inside the directory that the listing's keep
// rule accepts.
type DirFile struct {
	Name string // file name inside the directory
	Path string // repo-relative path, for error messages
	SHA  string // blob id
	Size int64
}

// Dir lists the files directly inside dir at commit sha that keep accepts with
// one subtree read (`GET /repos/{owner}/{repo}/git/trees/{sha}:{dir}`): only
// the directory's own entries come back, so a large repository elsewhere
// cannot push the response past the read cap. GitHub's 404 for a commit
// without dir is an empty Dir (a valid, empty model). An entry counts when
// keep accepts its name and its kind, read with notes.GitKind from its type
// and mode, the rule notes.ReadSet takes for a directory on disk: notes.IsNote
// for the notes of .dispatch/architecture, which keeps the regular blobs (mode
// 100644 / 100755) named *.md, and specgraph.IsFile for the spec files of
// .dispatch/spec, which keeps those named *.yaml; each skips symlinks
// (120000), submodules, and nested directories. A listing GitHub answers
// truncated is ErrTreeTruncated — it cannot be trusted, so the sync fails
// whole. dir is a plain repo-relative path, no leading slash.
func (c *Client) Dir(ctx context.Context, token, owner, repo, sha, dir string, keep func(name string, kind notes.Kind) bool) (Dir, error) {
	if c == nil {
		return Dir{}, ErrNoAppKey
	}
	dir = strings.Trim(dir, "/")
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/git/trees/" + url.PathEscape(sha+":"+dir)
	body, status, _, err := c.do(ctx, c.TokenScope(token), http.MethodGet, target, "Bearer "+token)
	if err != nil {
		return Dir{}, err
	}
	if status == http.StatusNotFound {
		return Dir{}, nil
	}
	if status != http.StatusOK {
		return Dir{}, fmt.Errorf("GET %s: %s", ShownBody(target), StatusText(status, body))
	}
	var tree struct {
		SHA       string `json:"sha"`
		Truncated bool   `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(body, &tree); err != nil {
		return Dir{}, fmt.Errorf("decode tree %s:%s: %w", sha, dir, ShownErr(err))
	}
	if tree.Truncated {
		return Dir{}, fmt.Errorf("%w: %s/%s@%s:%s", ErrTreeTruncated, owner, repo, sha, dir)
	}
	if tree.SHA == "" {
		return Dir{}, fmt.Errorf("tree %s:%s carries no sha", sha, dir)
	}
	listing := Dir{SHA: tree.SHA}
	for _, entry := range tree.Tree {
		if !keep(entry.Path, notes.GitKind(entry.Type, entry.Mode)) {
			continue
		}
		listing.Files = append(listing.Files, DirFile{
			Name: entry.Path, Path: dir + "/" + entry.Path, SHA: entry.SHA, Size: entry.Size,
		})
	}
	return listing, nil
}

// DirFiles reads every file of a Dir listing (one blob read each, base64),
// keyed by file name. The bounds are the notes package's: a listing over
// notes.MaxFiles files or notes.MaxSetSize bytes in sum is
// notes.ErrSetTooLarge (notes.CheckSet), and a file whose listed size exceeds
// notes.MaxFileSize is notes.ErrFileTooLarge (notes.CheckSize), each before
// any blob is fetched, and so is a decoded blob over MaxFileSize; a blob that
// vanished between the listing and the read fails the whole set. A path is
// GitHub's text (Dir), so the errors of notes.CheckSet and notes.CheckSize
// name it as ShownBody shows it.
func (c *Client) DirFiles(ctx context.Context, token, owner, repo string, listing Dir) (map[string][]byte, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	var listed int64
	for _, file := range listing.Files {
		listed += file.Size
	}
	if len(listing.Files) > 0 {
		// Every file of a listing is directly in its directory, so the first one's names it.
		if err := notes.CheckSet(ShownBody(path.Dir(listing.Files[0].Path)), len(listing.Files), listed); err != nil {
			return nil, err
		}
	}
	for _, file := range listing.Files {
		if err := notes.CheckSize(ShownBody(file.Path), file.Size); err != nil {
			return nil, err
		}
	}
	repoBase := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	files := make(map[string][]byte, len(listing.Files))
	for _, file := range listing.Files {
		content, err := c.blob(ctx, token, repoBase, file.Path, file.SHA)
		if err != nil {
			return nil, err
		}
		files[file.Name] = content
	}
	return files, nil
}

// blob reads the blob sha, the file at path, under token. sha and path come from GitHub's tree
// answer (Dir), so every error blob answers shows sha, path and the target that holds sha as
// ShownBody shows GitHub's text: blob's own errors, send's (which shows its target so), and
// notes.CheckSize's, which blob gives the path as ShownBody shows it.
func (c *Client) blob(ctx context.Context, token, repoBase, path, sha string) ([]byte, error) {
	target := repoBase + "/git/blobs/" + url.PathEscape(sha)
	body, status, _, err := c.doLimited(ctx, c.TokenScope(token), http.MethodGet, target, "Bearer "+token, blobResponseLimit)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET %s (%s): %s", ShownBody(target), ShownBody(path), StatusText(status, body))
	}
	var payload struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode blob %s (%s): %w", ShownBody(sha), ShownBody(path), ShownErr(err))
	}
	return decodeContent(fmt.Sprintf("blob %s (%s)", ShownBody(sha), ShownBody(path)), path, payload.Encoding, payload.Content)
}

// decodeContent is the bytes of a file GitHub answers as content in encoding, the one form a blob
// read (blob) and a contents read (File) both answer a file in: base64, wrapped at 60 characters.
// what names the answer in an error, written by the caller with GitHub's text as ShownBody shows
// it, and path names the file in notes.CheckSize's, which this shows so. An encoding other than
// base64, content that is not base64, and content that decodes past notes.MaxFileSize are errors.
func decodeContent(what, path, encoding, content string) ([]byte, error) {
	if encoding != "base64" {
		return nil, fmt.Errorf("%s has encoding %q, want base64", what, ShownBody(encoding))
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decode the content of %s: %w", what, err)
	}
	if err := notes.CheckSize(ShownBody(path), int64(len(decoded))); err != nil {
		return nil, err
	}
	return decoded, nil
}
