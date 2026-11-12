// Package appauth mints GitHub installation tokens for Legion's two GitHub Apps.
package appauth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// refreshWindow is how long before a lease's expiry Token stops serving it from the cache and
// mints a fresh one. The daemon rewrites each role's gh token file from the cached lease, so a
// container sees a new token only after that rewrite reaches it, and a Kubernetes Secret volume
// reaches a running pod within about the kubelet's one-minute sync period. Twenty minutes leaves
// a container the new token at least some fifteen minutes before the old one expires, while GitHub
// keeps the old token valid until its own expiry, so a `gh` or `git` that started on the old one
// finishes on it.
const refreshWindow = 20 * time.Minute

// narrowingRefusalWindow is how long a Manager serves a narrowing GitHub already refused
// (ErrNarrowingRefused) from its own memory, asking GitHub nothing (lease). The refusal is
// GitHub's definitive answer about what the account grants the App's installation, and nothing
// the daemon does changes it: only an account approving the grant does, which the next ask past
// this window finds. Without the memory every refresh of every role of every live pod asks the
// refused scope again, each exchange preceded by an uncached grant read — for one refused scope
// of an issue pod, six role bundles times two exchanges a minute — and a rate limit then fails
// every bundle of the project, the write leases included. Five minutes keeps that at one
// exchange per scope per five minutes, while leaving an account's fix to reach the daemon well
// inside the hour an installation token lasts.
const narrowingRefusalWindow = 5 * time.Minute

// AppRole names the GitHub App a Legion role acts as.
type AppRole string

const (
	Implement AppRole = "implement"
	Review    AppRole = "review"
)

// Roles is every App role, the Apps a Legion deployment configures one of each of.
var Roles = []AppRole{Implement, Review}

// GitIdentity is the bot identity that jj and git must use for work a role performs.
type GitIdentity = runtime.GitIdentity

// Lease is one installation-token lease, including its expiry, when it was minted and the App bot
// identity.
type Lease struct {
	Token     string
	ExpiresAt time.Time
	// Minted is when GitHub issued the token: one moments old may not be accepted yet.
	Minted   time.Time
	Identity GitIdentity
}

// TransientError is a GitHub answer that says nothing about the App or its installation, so the
// same request may pass when it is made again: no answer at all (a transport failure, including a
// request or a body read cut off by its deadline), a 5xx, or a rate limit (a 429, or a 403 that
// says it is one). Every other failure is GitHub's definitive answer, or one about the
// configuration, and asking again changes nothing.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// transient marks a response, whose body is body, as GitHub's trouble rather than an answer: a
// 5xx, or a rate limit (githubrest.RateLimited, which reads the body for a secondary limit GitHub
// names there alone).
func transient(response *http.Response, body []byte, err error) error {
	if response.StatusCode >= http.StatusInternalServerError || githubrest.RateLimited(response, body) {
		return &TransientError{Err: err}
	}
	return err
}

// refusalBody is the body GitHub sent with a refusal, read whole, and the response closed. One
// that cannot be read is empty: the status and the headers still say what the answer is.
func refusalBody(response *http.Response) []byte {
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return body
}

// responseBound caps what the daemon reads of every GitHub answer but a token exchange's, which
// is longer and has a bound of its own (exchangeAnswerBound): the installation discovery, the
// grant read, the two identity lookups and one page of an installation's repositories. GitHub
// has already acted by the time the answer arrives, so the bound is far above any answer it
// makes rather than a limit the daemon expects to meet.
const responseBound = 1 << 20

// exchangeAnswerBound caps what a token exchange reads of GitHub's answer, the longest answer
// the daemon reads: GitHub lists one object for every repository the installation holds,
// whatever the request named, and the request names none at all (accessTokensBody), so the list
// is the installation's whole breadth. Those objects measure 4.9 to 5.3 KB each, which is what
// the secrets broker bounds one of them at 64 KB on
// (packages/envoy/internal/broker/secrets/githubapp.go, githubRepositoryAnswerBound), and this
// deployment's theorem-labs installation held 216 repositories at 15:45Z on 2026-11-10, about
// 1.1 MB of answer. 16 MiB holds about 3,000 repositories at the measured size, far above any
// installation a Legion deployment mints on, and an answer past it is refused rather than read
// into the daemon's memory.
const exchangeAnswerBound = 16 << 20

// readBody reads a successful response's body whole, up to limit: a read that fails is a
// transport failure, and transient, where what the body then fails to parse as is GitHub's
// answer. A body past limit is no answer GitHub makes, and is refused naming the bound rather
// than parsed, so a body that never ends cannot take the daemon's memory with it; the refusal
// names the bound and never what the body held, which for a token exchange is the token.
func readBody(response *http.Response, what string, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	response.Body.Close()
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("%s: read the response: %w", what, err)}
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: the response is longer than the %d bytes an answer holds", what, limit)
	}
	return body, nil
}

// Tokens mints a GitHub installation token for an App on one owner account.
type Tokens interface {
	// Token is the write token for owner: the App role's installation token on that account,
	// holding the write permissions the App's roles use (writePermissions). It reaches every
	// repository the account's installation holds, which the account chooses when it installs
	// the App and the daemon narrows no further (INFRA-2543). An owner with no name is refused
	// before anything is minted, since there is no account to mint on.
	Token(ctx context.Context, role AppRole, owner string) (Lease, error)
	// ReadToken is the App's installation token for owner narrowed to reading: contents, pull
	// requests, Actions and metadata read. A pane holds one for every owner its App is
	// installed on but the one its issue's repository is under (a configured rename's two
	// owners count as that one, GitHubCredential), a controller holds one for every owner its
	// App is installed on, and the Kubernetes runtime's provisioning token is one of the
	// implement App for the issue's own owner, which
	// every issue pod's workspace-fetch and every repository cache clones with
	// (implementTokens, internal/daemon). An App on token_command gets what its command's
	// read-only release holds instead, which the command decides: the secrets broker's is the
	// same four (its readOnlyPermissions). With a key of its own the mint asks for what the
	// installation is granted of those four and leaves the rest out (exchangeGranted), so an
	// installation whose account has not granted the App Actions gets the other three rather
	// than nothing. An owner with no name is refused before anything is minted, as Token's is.
	ReadToken(ctx context.Context, role AppRole, owner string) (Lease, error)
}

// readPermissions narrows an installation token to reading a repository's contents, its pull
// requests, its Actions runs and its metadata: what a role needs of every repository of the
// project but its own issue's, what a controller needs of every one of them, and what the
// provisioning token takes of the issue's own repository, which a clone, a fetch and a
// repository cache only read (implementTokens, internal/daemon). A mint asks for what the
// installation is granted of this set, at the access it is granted (exchangeGranted), and
// GitHub mints a token with the permissions the request names.
//
// actions:read is here because reading a repository includes reading what its merges did: a
// deployment check of a merge reads the push run that carries it, that run's jobs and a job's
// log, and a role that cannot read them cannot tell a deploy that applied from one that was
// skipped. Measured on 2026-11-04 against a private repository whose installation holds
// actions:write: unnarrowed, actions/runs and actions/runs/{id}/jobs answered 200 and
// actions/jobs/{id}/logs answered 302 to a signed URL that answered 200; narrowed to contents,
// pull requests and metadata, all three answered 403 "Resource not accessible by integration".
// The narrowing was the whole refusal, and no App
// or installation setting was missing. It stays a read: actions:read cannot cancel, re-run or
// dispatch a workflow, and reaches no Actions secret or variable, which are permissions of their
// own.
//
// The set is the most a read lease holds, and the only thing that holds it: the lease reaches
// every repository its installation holds, as every token this package mints does, so what
// keeps a reading lease from writing is these four permissions and nothing beside them. A
// permission the installation is granted none of is left out of the request rather than minted.
var readPermissions = map[string]string{"contents": "read", "pull_requests": "read", "actions": "read", "metadata": "read"}

// Options makes a token manager testable. The zero value uses api.github.com, http.DefaultClient,
// the wall clock, and the daemon's own stderr for what a token_command writes there.
type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
	Stderr     io.Writer
}

// Manager caches installation tokens by App and owner, and bot identities by App id. An App
// configured with a token_command is in token mode (tokencommand.go): the Manager holds no key and
// signs no JWT for it, and every lease comes from the command's release.
type Manager struct {
	apps    config.GitHubApps
	baseURL string
	client  *http.Client
	now     func() time.Time
	stderr  io.Writer

	mu                sync.Mutex
	leases            map[scope]Lease
	pending           map[scope]*pendingLease
	installations     map[AppRole]map[string]string
	identities        map[string]GitIdentity
	pendingIdentities map[string]*pendingIdentity
	commands          map[scope]*commandRun
	// keys holds each App private key parsed, by its PEM text (generateJWT).
	keys map[string]*rsa.PrivateKey

	// refusals holds, per scope, the narrowing GitHub refused and when it did (lease,
	// narrowingRefusalWindow): while the window holds, that scope is answered from here and
	// GitHub is asked nothing. A mint of the scope that succeeds drops its entry, as does a
	// Forget of its owner.
	refusals map[scope]refusedNarrowingAt

	// saidShortfalls holds every shortfall already said on stderr, each by the App role, the
	// owner whose installation is granted less than the role's set asks for, and the shortfall
	// itself (shortfallMessage).
	saidShortfalls map[saidShortfall]bool

	// root ends every token_command run on Close (stop), runs counts the runs still going, and
	// closed, set under mu, refuses a run after Close.
	root   context.Context
	stop   context.CancelFunc
	runs   sync.WaitGroup
	closed bool
}

type pendingLease struct {
	done  chan struct{}
	lease Lease
	err   error
}

// refusedNarrowingAt is one remembered narrowing refusal: GitHub's own refusal, served again to
// every caller of the scope while the window holds, and the Manager's clock reading when it was
// met.
type refusedNarrowingAt struct {
	err error
	at  time.Time
}

type pendingIdentity struct {
	done     chan struct{}
	identity GitIdentity
	err      error
}

// A Manager answers every interface a caller reaches it through, the optional one included: a
// bundle takes the RefusalForgetter by type assertion (listInstallation's caller is the other),
// so a method renamed without its interface would leave the Manager silently un-forgotten
// rather than fail the build.
var (
	_ Tokens             = (*Manager)(nil)
	_ OwnerInstallations = (*Manager)(nil)
	_ Forgetter          = (*Manager)(nil)
	_ RefusalForgetter   = (*Manager)(nil)
)

// New makes an App token manager. Config.Load resolves command and secret key sources before
// constructing this manager; callers using LoadForValidation must resolve them themselves.
func New(apps config.GitHubApps, options Options) *Manager {
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	client := options.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	root, stop := context.WithCancel(context.Background())
	return &Manager{
		root:              root,
		stop:              stop,
		apps:              apps,
		baseURL:           baseURL,
		client:            client,
		now:               now,
		stderr:            stderr,
		leases:            map[scope]Lease{},
		pending:           map[scope]*pendingLease{},
		installations:     map[AppRole]map[string]string{},
		identities:        map[string]GitIdentity{},
		pendingIdentities: map[string]*pendingIdentity{},
		commands:          map[scope]*commandRun{},
		keys:              map[string]*rsa.PrivateKey{},
		refusals:          map[scope]refusedNarrowingAt{},
		saidShortfalls:    map[saidShortfall]bool{},
	}
}

// Token returns the cached write lease for owner until fewer than twenty minutes remain
// (refreshWindow), otherwise minting a fresh installation token on owner's installation with
// the App role's write permissions (writePermissions). Concurrent requests for the same App and
// owner share that one exchange. The lease reaches every repository that installation holds:
// the account chooses those when it installs the App, and the daemon asks for no narrowing of
// them (INFRA-2543). An owner with no name is no account to mint on: it is refused, and nothing
// is minted.
func (m *Manager) Token(ctx context.Context, role AppRole, owner string) (Lease, error) {
	return m.lease(ctx, mintRequest{role: role, owner: owner, permissions: writePermissions[role]})
}

// ReadToken is Token for a token narrowed to readPermissions, cached under a scope of its own,
// so an owner's write and read tokens never stand in for each other. Each of those four
// permissions is asked for at what the installation is granted of it, and one it is granted
// none of is left out (exchangeGranted). In token mode it is the command's read-only release
// (TokenCommandReadOnly) instead, which the command decides; the secrets broker's is
// readPermissions' own set. A write request of the same owner is a run of its own either way:
// it asks for no read-only release, so the two never stand in for each other. An owner with no
// name is refused, and nothing is minted.
func (m *Manager) ReadToken(ctx context.Context, role AppRole, owner string) (Lease, error) {
	return m.lease(ctx, mintRequest{role: role, owner: owner, read: true, permissions: readPermissions})
}

// Forgetter drops the leases a token source holds for an App role and owner, so the next one is
// minted: a token GitHub refused (401) is not served again from the cache.
type Forgetter interface {
	Forget(role AppRole, owner string)
}

// Forget drops both leases cached for role and owner, the write token's and the read token's,
// and every narrowing refusal remembered for them (refusals): the owner is what a refused token
// is known by, and a caller that met one holds no account of which of the two scopes it was, so
// the next ask of either reaches GitHub.
func (m *Manager) Forget(role AppRole, owner string) {
	forgotten := strings.ToLower(strings.TrimSpace(owner))
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.leases, func(key scope, _ Lease) bool {
		return key.role == role && key.owner == forgotten
	})
	maps.DeleteFunc(m.refusals, func(key scope, _ refusedNarrowingAt) bool {
		return key.role == role && key.owner == forgotten
	})
}

// RefusalForgetter drops the narrowing refusal a token source remembers for one owner's read
// lease, so the next ask of that scope reaches GitHub rather than the memory
// (Manager.ForgetNarrowingRefusal). It is optional, as Forgetter is: a source that remembers no
// refusal has none to drop, so a test fake and a harness that implement Tokens alone keep
// working.
type RefusalForgetter interface {
	ForgetNarrowingRefusal(role AppRole, owner string)
}

// ForgetNarrowingRefusal drops the narrowing refusal remembered for role's read lease on owner
// (lease, narrowingRefusalWindow), and nothing else: no lease, and no other scope's refusal. It
// is for the caller that knows that one memory is stale — the token-mode mapping check, which
// asks whether the App's installation holds a repository and must have GitHub's answer rather
// than the memory of a refusal met before an operator fixed the grant (listInstallation) —
// where Forget, which drops every lease and every refusal of the owner, would throw away a
// token that is good and make the owner's other scope mint again.
func (m *Manager) ForgetNarrowingRefusal(role AppRole, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.refusals, leaseScope(true, role, owner))
}

// scope is one lease's identity, and the key every lease is cached, awaited and warned under:
// which kind of token it is, the App role it is minted as, and the owner whose installation
// mints it. Those three are the whole of what a lease may do, since a token reaches every
// repository its own installation holds: a read lease must never be served to a caller that
// asked for a write one, and no owner's token may stand in for another owner's.
type scope struct {
	read  bool
	role  AppRole
	owner string
}

// leaseScope is the scope of role's lease on owner, read or write. The owner is compared
// trimmed and lowercased, as GitHub matches an account's name, so two callers that spell one
// account differently share the one lease rather than mint twice.
func leaseScope(read bool, role AppRole, owner string) scope {
	return scope{read: read, role: role, owner: strings.ToLower(strings.TrimSpace(owner))}
}

// mintRequest is one ask for a lease, built by Token and ReadToken and carried whole through
// the mint: the App role the token is minted as, the owner whose installation mints it, whether
// it is the read token, and the permissions GitHub is asked for.
type mintRequest struct {
	role        AppRole
	owner       string
	read        bool
	permissions map[string]string
}

// scope is the key the request's lease is cached and awaited under (leaseScope).
func (r mintRequest) scope() scope { return leaseScope(r.read, r.role, r.owner) }

// lease serves the cached lease of the request's scope while more than refreshWindow remains,
// and otherwise mints one with the permissions the request names, sharing one exchange among
// concurrent callers. A caller that waited on another's mint, which ended only because that
// caller's own deadline did (a context error, or ErrCommandPending), asks again under its own
// context rather than take that caller's failure as its own.
//
// A narrowing GitHub refused is remembered per scope for narrowingRefusalWindow and served from
// that memory while it holds, GitHub asked nothing: it is GitHub's definitive answer about what
// the account grants the App's installation, where every refresh of every role of every live pod
// would otherwise ask it again. Only ErrNarrowingRefused is remembered — GitHub's trouble
// (TransientError) and every other failure keep asking as they always did — and a mint of the
// scope that succeeds drops what was remembered of it.
//
// The memory answers the ask that would repeat a refusal, never one that already knows better.
// A caller holding what the window cannot know — the token-mode mapping check, which asks
// whether an installation holds a repository after an operator may have changed the grant
// (listInstallation) — drops that one scope's memory before it asks (ForgetNarrowingRefusal),
// so what it is served is GitHub's answer now.
//
// What a cached answer is worth is worth saying, since a caller may read such an ask as what
// its installation does: a lease served from here says a token of that scope was minted within
// the hour, not that GitHub mints one now. An App an account uninstalls leaves every lease of
// that owner cached and good-looking until its expiry nears (refreshWindow), so a bundle built
// in that hour may hold a directory whose token GitHub would no longer mint; what the bundle
// does not do is guess, since the owners it holds a directory for at all are read from GitHub
// at every build (InstalledOwners, credential.go).
//
// A mint that succeeds also drops every lease the cache holds that has expired. The cache is
// keyed by the owner a lease is minted on, so a mapping that changes over a long-lived daemon's
// life leaves entries no caller asks for again, and an expired one can never be served: a lease
// is served only while more than refreshWindow remains.
func (m *Manager) lease(ctx context.Context, request mintRequest) (Lease, error) {
	request.owner = strings.TrimSpace(request.owner)
	if request.owner == "" {
		return Lease{}, fmt.Errorf("the %s App's token names no owner account to mint on", request.role)
	}
	cacheKey := request.scope()
	for {
		now := m.now()
		m.mu.Lock()
		if lease, ok := m.leases[cacheKey]; ok && lease.ExpiresAt.Sub(now) > refreshWindow {
			m.mu.Unlock()
			return lease, nil
		}
		if refused, ok := m.refusals[cacheKey]; ok {
			if now.Sub(refused.at) < narrowingRefusalWindow {
				m.mu.Unlock()
				return Lease{}, refused.err
			}
			delete(m.refusals, cacheKey)
		}
		if waiting, ok := m.pending[cacheKey]; ok {
			m.mu.Unlock()
			select {
			case <-waiting.done:
				if callerEnded(waiting.err) && ctx.Err() == nil {
					continue
				}
				return waiting.lease, waiting.err
			case <-ctx.Done():
				return Lease{}, ctx.Err()
			}
		}
		waiting := &pendingLease{done: make(chan struct{})}
		m.pending[cacheKey] = waiting
		m.mu.Unlock()

		lease, err := m.mint(ctx, request)
		minted := m.now()
		m.mu.Lock()
		switch {
		case err == nil:
			m.leases[cacheKey] = lease
			delete(m.refusals, cacheKey)
			maps.DeleteFunc(m.leases, func(_ scope, held Lease) bool { return !held.ExpiresAt.After(minted) })
		case NarrowingRefused(err):
			m.refusals[cacheKey] = refusedNarrowingAt{err: err, at: minted}
		}
		waiting.lease, waiting.err = lease, err
		delete(m.pending, cacheKey)
		close(waiting.done)
		m.mu.Unlock()
		return lease, err
	}
}

// NarrowingRefused reports GitHub refusing the narrowing a request named, and not GitHub's own
// trouble, which the same request may pass through a moment later (TransientError). An error
// that carries both is the second. No mint builds one today: the exchange returns a rate-limited
// 422 as a TransientError alone (exchange.go), and token mode returns its trouble before it
// builds the refusal (mintFromCommand), so excluding it here is a guard for a later producer. It
// is the one predicate every caller that acts on a refusal reads — a Manager deciding what to
// remember (refusals), a bundle deciding which directories it holds (credential.go's dirEntry),
// the provisioning adapter's fallback (internal/daemon's implementTokens) and the controller
// route's answer (internal/api) — so that none of them reads a rate limit as an answer about
// what an installation grants.
func NarrowingRefused(err error) bool {
	var trouble *TransientError
	return errors.Is(err, ErrNarrowingRefused) && !errors.As(err, &trouble)
}

// callerEnded reports a failure that says only that the calling context ended: the context's own
// errors, or a token_command still running past it (ErrCommandPending).
func callerEnded(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrCommandPending)
}

// mint mints the request's installation token. An installation the daemon found that answers the
// permissions read or the exchange 404 was removed since (errInstallationGone): it is forgotten
// and looked for once more, so a removed App answers ErrNotInstalled and an App installed there
// again mints on its new installation. A configured installation answering 404 is not installed
// either.
func (m *Manager) mint(ctx context.Context, request mintRequest) (Lease, error) {
	app, err := m.app(request.role)
	if err != nil {
		return Lease{}, err
	}
	if app.TokenCommand != "" {
		return m.mintFromCommand(ctx, app, request)
	}
	installation, err := m.installation(ctx, request.role, app, request.owner)
	if err != nil {
		return Lease{}, err
	}
	jwt, err := m.generateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return Lease{}, err
	}
	lease, err := m.exchangeGranted(ctx, jwt, installation, request)
	if errors.Is(err, errInstallationGone) && configuredInstallation(app, request.owner) == "" {
		m.forgetInstallation(request.role, request.owner)
		if installation, err = m.installation(ctx, request.role, app, request.owner); err != nil {
			return Lease{}, err
		}
		lease, err = m.exchangeGranted(ctx, jwt, installation, request)
	}
	if errors.Is(err, errInstallationGone) {
		m.forgetInstallation(request.role, request.owner)
		return Lease{}, fmt.Errorf("%w: %s's installation %s on %s answers %w", ErrNotInstalled, request.role, installation, request.owner, err)
	}
	if err != nil {
		return Lease{}, err
	}
	identity, err := m.identity(ctx, app.AppID, func(ctx context.Context) (GitIdentity, error) {
		return m.fetchIdentity(ctx, jwt, lease.Token)
	})
	if err != nil {
		return Lease{}, err
	}
	lease.Identity = identity
	return lease, nil
}

func (m *Manager) app(role AppRole) (config.GitHubApp, error) {
	switch role {
	case Implement:
		return m.apps.Implement, nil
	case Review:
		return m.apps.Review, nil
	default:
		return config.GitHubApp{}, fmt.Errorf("role_not_configured: %s", role)
	}
}

func (m *Manager) installation(ctx context.Context, role AppRole, app config.GitHubApp, owner string) (string, error) {
	ownerKey := strings.ToLower(owner)
	if installation := configuredInstallation(app, owner); installation != "" {
		return installation, nil
	}
	m.mu.Lock()
	if installations := m.installations[role]; installations != nil {
		if installation := installations[ownerKey]; installation != "" {
			m.mu.Unlock()
			return installation, nil
		}
	}
	m.mu.Unlock()

	found, err := m.discoverInstallations(ctx, app)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.installations[role] == nil {
		m.installations[role] = map[string]string{}
	}
	for discoveredOwner, installation := range found {
		m.installations[role][strings.ToLower(discoveredOwner)] = installation
	}
	installation := m.installations[role][ownerKey]
	m.mu.Unlock()
	if installation == "" {
		return "", fmt.Errorf("%w: %s not installed on %s", ErrNotInstalled, role, owner)
	}
	return installation, nil
}

// configuredInstallation is the installation app's configuration names for owner, if any.
func configuredInstallation(app config.GitHubApp, owner string) string {
	for configuredOwner, installation := range app.Installations {
		if strings.EqualFold(configuredOwner, owner) && installation != "" {
			return installation
		}
	}
	return ""
}

// forgetInstallation drops the installation found for role on owner, for the next mint to look
// for it again.
func (m *Manager) forgetInstallation(role AppRole, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.installations[role], strings.ToLower(owner))
}

// errInstallationGone is a permissions read or a token exchange GitHub answers 404: the
// installation is no longer there.
var errInstallationGone = errors.New("the installation is gone")

// ErrNarrowingRefused is a token exchange GitHub answers 422, every one of them that is not a
// rate limit: a permission the installation is not granted at the access the request asked for,
// which GitHub refuses whole rather than leaving that one permission out. A mint asks only for
// what the installation was granted a moment before (exchangeGranted), so a refusal here is a
// grant that changed between those two calls. A repository cannot cause it any longer: no
// request names one (exchange), so there is none for GitHub to refuse. In token mode it is the
// secrets broker refusing the narrowing its own release names (brokerNarrowingRefused).
//
// It is exported because its callers read it rather than fail on it: the daemon's provisioning
// adapter falls back on it (implementTokens in internal/daemon), where an installation that
// refuses a read token is served the owner's own token instead; a role's gh bundle leaves out
// the directory whose lease is refused and keeps the rest (mintBundle), and refuses a bundle
// whose own default owner it leaves out (bundleDefault), which the API's controller route
// answers as a refusal of its own (internal/api's controllerGitHubCredential). The Manager
// remembers it per scope and serves it without asking GitHub again while the window holds
// (lease, narrowingRefusalWindow), which it does for no other refusal. No other refusal is read
// any of these ways.
var ErrNarrowingRefused = errors.New("the installation does not hold the permissions asked for")

// ErrNotInstalled is a mint refused because the App has no installation on the owner: an answer
// about the owner, not GitHub's trouble, which a retry would only repeat.
var ErrNotInstalled = errors.New("github_app_not_installed")

// OwnerInstallations reads the owner accounts an App is installed on. It is what decides the
// directories of a role's gh bundle (credential.go's GitHubCredential and ControllerCredential):
// the installation is the whole of a role's reach, since every token this package mints reaches
// every repository its installation holds and names no repository at all (INFRA-2543), so the
// accounts the App is installed on are the owners a bundle can hold a directory for. It is an
// interface of its own, as Tokens is, so a bundle can be built over a fake in a test and over
// the Manager in the daemon.
type OwnerInstallations interface {
	InstalledOwners(ctx context.Context, role AppRole) ([]string, error)
}

// InstalledOwners is the owner accounts role's App is installed on, lowercased and sorted, read
// from GitHub at the moment of the call: GET /app/installations under the App's own JWT, every
// page of it (discoverInstallations), or, in token mode, the accounts the command's release of
// every installation names (runTokenCommand with no owner, which records them as this role's
// installations itself).
//
// It asks GitHub every time rather than answering from the installations the Manager has found
// before, because its caller builds a bundle from the answer: an owner the account removed the
// App from must stop minting at the next bundle, and one that installed it must mint at that
// same bundle, with no cached set and no timer between the account's change and the role's
// reach. What it read replaces the installation ids the Manager holds for the role, so a mint
// that follows reaches the installation GitHub names now and an owner no longer listed is
// looked for again rather than minted on a stale id.
func (m *Manager) InstalledOwners(ctx context.Context, role AppRole) ([]string, error) {
	app, err := m.app(role)
	if err != nil {
		return nil, err
	}
	found := map[string]string{}
	if app.TokenCommand != "" {
		released, err := m.runTokenCommand(ctx, app, mintRequest{role: role})
		if err != nil {
			return nil, err
		}
		for owner := range released {
			found[owner] = owner
		}
	} else if found, err = m.discoverInstallations(ctx, app); err != nil {
		return nil, err
	}
	installations := make(map[string]string, len(found))
	owners := make([]string, 0, len(found))
	for owner, installation := range found {
		key := strings.ToLower(owner)
		installations[key] = installation
		owners = append(owners, key)
	}
	m.mu.Lock()
	m.installations[role] = installations
	m.mu.Unlock()
	slices.Sort(owners)
	return owners, nil
}

func (m *Manager) discoverInstallations(ctx context.Context, app config.GitHubApp) (map[string]string, error) {
	jwt, err := m.generateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return nil, err
	}
	installations := map[string]string{}
	for page := 1; ; page++ {
		endpoint, err := url.Parse(m.baseURL + "/app/installations")
		if err != nil {
			return nil, fmt.Errorf("GitHub App installation URL: %w", err)
		}
		query := endpoint.Query()
		query.Set("per_page", "100")
		query.Set("page", strconv.Itoa(page))
		endpoint.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		m.appHeaders(request, jwt)
		response, err := m.client.Do(request)
		if err != nil {
			return nil, &TransientError{Err: fmt.Errorf("GitHub App installation discovery: %w", err)}
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return nil, transient(response, refusalBody(response), fmt.Errorf("GitHub App installation discovery failed (%d)", response.StatusCode))
		}
		body, err := readBody(response, "GitHub App installation discovery", responseBound)
		if err != nil {
			return nil, err
		}
		var payload []struct {
			ID      json.RawMessage `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("GitHub App installation discovery returned an invalid response")
		}
		for _, installation := range payload {
			id, err := installationID(installation.ID)
			if err != nil || installation.Account.Login == "" {
				return nil, fmt.Errorf("GitHub App installation discovery returned an invalid installation account")
			}
			installations[installation.Account.Login] = id
		}
		if len(payload) < 100 {
			return installations, nil
		}
	}
}

func installationID(raw json.RawMessage) (string, error) {
	var stringID string
	if err := json.Unmarshal(raw, &stringID); err == nil && stringID != "" {
		return stringID, nil
	}
	var numberID int64
	if err := json.Unmarshal(raw, &numberID); err == nil && numberID > 0 {
		return strconv.FormatInt(numberID, 10), nil
	}
	return "", fmt.Errorf("invalid installation id")
}

// identity is the bot identity of the App appID, read once by fetch and cached; concurrent callers
// share the one read.
func (m *Manager) identity(ctx context.Context, appID string, fetch func(context.Context) (GitIdentity, error)) (GitIdentity, error) {
	m.mu.Lock()
	if identity, ok := m.identities[appID]; ok {
		m.mu.Unlock()
		return identity, nil
	}
	if waiting, ok := m.pendingIdentities[appID]; ok {
		m.mu.Unlock()
		select {
		case <-waiting.done:
			return waiting.identity, waiting.err
		case <-ctx.Done():
			return GitIdentity{}, ctx.Err()
		}
	}
	waiting := &pendingIdentity{done: make(chan struct{})}
	m.pendingIdentities[appID] = waiting
	m.mu.Unlock()

	identity, err := fetch(ctx)
	m.mu.Lock()
	if err == nil {
		m.identities[appID] = identity
	}
	waiting.identity, waiting.err = identity, err
	delete(m.pendingIdentities, appID)
	close(waiting.done)
	m.mu.Unlock()
	return identity, err
}

func (m *Manager) fetchIdentity(ctx context.Context, jwt, installationToken string) (GitIdentity, error) {
	slug, err := m.appSlug(ctx, jwt)
	if err != nil {
		return GitIdentity{}, err
	}
	return m.botIdentity(ctx, slug+"[bot]", installationToken)
}

// botIdentity is the git identity of the App bot login, its noreply email naming the bot user's
// id, read from GET /users/<login> under installationToken.
func (m *Manager) botIdentity(ctx context.Context, login, installationToken string) (GitIdentity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/users/"+url.PathEscape(login), nil)
	if err != nil {
		return GitIdentity{}, err
	}
	request.Header.Set("Authorization", "token "+installationToken)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
	response, err := m.client.Do(request)
	if err != nil {
		return GitIdentity{}, &TransientError{Err: fmt.Errorf("GitHub App bot identity lookup: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return GitIdentity{}, transient(response, refusalBody(response), fmt.Errorf("GitHub App bot identity lookup failed (%d)", response.StatusCode))
	}
	body, err := readBody(response, "GitHub App bot identity lookup", responseBound)
	if err != nil {
		return GitIdentity{}, err
	}
	var bot struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &bot); err != nil {
		return GitIdentity{}, fmt.Errorf("GitHub App bot identity lookup returned an invalid bot user id")
	}
	id, err := installationID(bot.ID)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("GitHub App bot identity lookup returned an invalid bot user id")
	}
	return GitIdentity{
		Name:  login,
		Email: id + "+" + login + "@users.noreply.github.com",
	}, nil
}

// appSlug is the slug of the App jwt signs for, read from GET /app: its bot login is the slug's,
// "<slug>[bot]".
func (m *Manager) appSlug(ctx context.Context, jwt string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/app", nil)
	if err != nil {
		return "", err
	}
	m.appHeaders(request, jwt)
	response, err := m.client.Do(request)
	if err != nil {
		return "", &TransientError{Err: fmt.Errorf("GitHub App identity lookup: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", transient(response, refusalBody(response), fmt.Errorf("GitHub App identity lookup failed (%d)", response.StatusCode))
	}
	body, err := readBody(response, "GitHub App identity lookup", responseBound)
	if err != nil {
		return "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(body, &app); err != nil || app.Slug == "" {
		return "", fmt.Errorf("GitHub App identity lookup returned an invalid app slug")
	}
	return app.Slug, nil
}

func (m *Manager) appHeaders(request *http.Request, jwt string) {
	request.Header.Set("Authorization", "Bearer "+jwt)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
}
