// Package ciphertoken decides a caller that presents an access token the secrets broker issued,
// under the RFC 9449 DPoP scheme: Authorization: DPoP <token> with a DPoP: proof header.
//
// Deciding such a caller is two checks. The broker is asked what the token is (RFC 7662
// introspection, IntrospectPath), and the caller is held to the key that token is bound to (RFC
// 9449 Section 7.1, which the library's own resource check performs). Neither check is Dispatch's
// alone — the Envoy listener takes the same one — so this package lives outside
// internal/dispatch and reaches Dispatch only through the interfaces in Options.
//
// What it does not do is write the reasons down for a browser: Verify answers one of the
// sentinels below and whoever mounts it turns each into a status.
package ciphertoken

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc9449"
	"authelia.com/provider/oauth2/token/jose"
	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// The outcomes a caller has to tell apart. None of them carries the token presented.
var (
	// ErrNotPresented is a request carrying no token under the DPoP scheme. Whoever mounts this
	// check decides a request by the credential it presents, so this is reached only by a caller
	// that routed one here without a token.
	ErrNotPresented = errors.New("brokertoken: the request presents no access token under the DPoP scheme")
	// ErrNotActive is the broker answering that the token is not live: unknown, revoked or
	// expired. The caller is refused; the answer is never remembered.
	ErrNotActive = errors.New("brokertoken: the broker answered that the token is not live")
	// ErrProofInvalid is a proof that did not meet the RFC 9449 Section 4.3 criteria: a bad
	// signature, a method or target URI that is not this request's, an 'iat' outside the window,
	// a missing or wrong 'ath', or a proof already spent (a replay).
	ErrProofInvalid = errors.New("brokertoken: the DPoP proof did not verify")
	// ErrKeyMismatch is a sound proof signed by a key that is not the one the token is bound to,
	// which is what makes a stolen token useless. A live token the broker bound to no key at all
	// is refused here too: no proof can be the key it does not have.
	ErrKeyMismatch = errors.New("brokertoken: the DPoP proof's key is not the key the token is bound to")
	// ErrBrokerUnreachable is an introspection that produced no answer: a transport failure, a
	// status the broker does not answer a sound request with, a body that does not decode, or
	// Dispatch's own account token unread. It is not a refusal — a caller told its token was
	// refused would discard a token that may be live — so whoever mounts this check answers 503
	// and the caller comes back.
	ErrBrokerUnreachable = errors.New("brokertoken: the broker could not be reached")
	// ErrRateLimited is a source address with no introspection left in its bucket
	// (DefaultAddressLimit). Nothing was asked of the broker.
	ErrRateLimited = errors.New("brokertoken: too many introspections from this source address")
	// ErrNotASession is a live token that is not a session's credential: the broker's answer
	// names no session, or names neither a person nor a service, or the token was issued to a
	// client other than the exchange every Dispatch access token comes from (ExchangeClientID),
	// or its project scope and its OAuth scope together are no shape this verifier serves
	// (shapeOf): a project-scope word this package does not know, the project scope none in any
	// shape but a read-only token's, the project scope public beside a person, without a
	// service or beside any OAuth scope, the project scope named beside no person and any OAuth
	// scope, the scope read beside the project scope named or person, a narrowed project scope
	// (read-named, read-person, read-none) whose scope is anything but read, or read person on a
	// read-person token that names its person, a cut (as_of) on a token that is not narrowed to
	// reading or a cut that is not an RFC 3339 time, or a read-only token presented to a verifier
	// built without Options.ServesReadOnly; or its list of projects is one no exchange mints: a
	// named list of two or more, a named list of none that names no person and no account, a list
	// of one or more beside a service under any scope but the two a job's token carries (named and
	// public), or a list that is not at most oauthwire.MaxProjects project keys, sorted, each once.
	// A machine login's own access token is exactly that — the credential a dev box holds to ask
	// the broker for other credentials, DPoP-bound and perfectly live, for which no exchange ever
	// decided a session, a person or a project scope — and so is any other token the broker issued
	// for something other than acting as a session. Whoever mounts this check acts as the session a
	// token names, so a token that names none is refused here rather than admitted for a request
	// body to name one in its place.
	//
	// A token that names a service and no person is a session's credential only in the shapes
	// shapeOf and sessionCaller admit: a machine's under the scope person, naming no project; an
	// account entry's under the scope named, naming its account and no project; a job's under
	// the scope named, naming exactly one project; a job's under the scope public, naming 0 to
	// oauthwire.MaxProjects private projects; and, to a verifier that serves one (below), a
	// read-only token, the read entry's or one narrowed from either of the first two. Each other
	// shape of such a token is refused.
	//
	// The scope is refused for the same reason it is read at all: it says what Projects means,
	// so a word this package does not know is a token it cannot read rather than one that names
	// no project. Reading an unknown scope as unscoped would hand its caller every project the
	// moment the broker learns a new word, which is the one mistake a check deciding what a
	// credential covers must not make.
	//
	// A read-only token is refused here by every verifier but one whose service asked for it,
	// exactly as a build that predates the token refuses it: a service that did not ask reads no
	// Caller.ReadOnly, so admitting the token there would open every route that service serves
	// to a token minted only to read. The scope read beside named, person or public, none in any
	// other shape, and a narrowed word beside any scope but its own, are refused by every
	// verifier, the switch or not. The broker mints none of them, and a token that holds read is
	// one its broker meant to read only, so reading it by its project scope alone would let it
	// write: a read-scoped token fails closed.
	ErrNotASession = errors.New("brokertoken: the access token is not a session's credential")
)

// AffirmativeCacheTTL is how long one live token's introspection answer is reused, so a caller
// sending many requests asks the broker about its token a few times a minute rather than once a
// request.
//
// Five seconds is sized against the broker's budget. The broker spends one source-address bucket
// for its whole account-authenticated class before it verifies anything — a burst of 200 and 50 a
// second per broker process (internal/cipher/api/limits.go, DefaultAccountLimit) — and that one
// bucket is shared with the approval relays a person's click produces, so introspection has to
// stay well under it or a flood of checks would refuse a person their own approval. With
// single-flight in front, and while the cache keeps each answer, one Dispatch pod's sustained
// introspection rate is its distinct live tokens divided by this window: under 128 live tokens
// that is under 26 a second, half the bucket's refill and a fifth of its burst, leaving the relays
// theirs. Verifier.known says when the cache keeps no answer. The address bucket
// (DefaultAddressLimit) does not hold that rate down: a live token's introspection gives back the
// request it took (Verifier.known).
//
// Shortening it costs broker requests in that proportion; lengthening it leaves a revoked token
// accepted for longer, which is the other half of the trade and why the window is seconds.
//
// The window runs from the broker's answer, not from the revocation: an answer is remembered
// when the introspection that produced it completes, and an introspection already in flight
// when the token is revoked may take up to introspectTimeout to complete. So a revoked token
// stops resolving within this window of the last introspection that called it live, which is up
// to introspectTimeout later than the revocation itself.
const AffirmativeCacheTTL = 5 * time.Second

// AffirmativeCacheSize bounds how many answers are remembered at once, so a flood of distinct
// live tokens cannot grow the cache without bound. It is far above the live tokens one pod sees —
// one per running agent session — and an answer that does not fit is simply not remembered, which
// costs that token an introspection and nothing else.
const AffirmativeCacheSize = 4096

// DefaultAddressLimit is the introspection bucket each source address (Options.Address) has of
// its own: a burst of 20 and five a second, per process. Each introspection takes one request from
// it before it is sent, and which of them give that request back is Verifier.known's rule.
//
// Taking the request before the call is the rule INFRA-228 wrote down
// (docs/solutions/best-practices/a-route-that-verifies-before-it-limits-lets-a-stranger-spend-your-upstream-INFRA-228.md):
// a check that costs an upstream call is work an unauthenticated caller can make you do, so the
// bucket is spent before the call and not inside the handler after it. The budget being guarded
// is the broker's account-class bucket, which a person's approval relay shares.
//
// The residual that best practice records stands here too: the bucket is per process, so a fleet
// of N pods allows N times this, and a flood spread over many addresses still reaches the broker.
// What bounds that overall is the broker's own bucket refusing Dispatch 429, which is
// ErrBrokerUnreachable rather than a refusal of anyone's token.
var DefaultAddressLimit = ratelimit.Limit{Every: 200 * time.Millisecond, Burst: 20}

// ProofClockSkew and ProofLifespan are the window a proof's 'iat' must fall in: the library
// accepts a proof from ProofClockSkew before its 'iat' until ProofLifespan plus ProofClockSkew
// after it. They are the broker's own figures — BROKER_PROOF_SKEW_SECONDS' default of a minute
// and the one second internal/cipher/oauth sets as the library's lifespan — so a client whose
// proofs the broker accepts is not refused here for its clock.
const (
	ProofClockSkew = time.Minute
	ProofLifespan  = time.Second
)

// proofAlgorithms are the JWS algorithms a proof may be signed with. The broker binds every token
// it issues to a key its own helper minted, and that key is ES256 (internal/cipher/proof.NewKey),
// so a wider set would admit no token the broker issues and would widen the signatures a stranger
// can make Dispatch verify.
var proofAlgorithms = []jose.SignatureAlgorithm{jose.ES256}

// proofAlgorithmNames is proofAlgorithms as the library's StrategyConfig spells them. It is built
// once here rather than in the method below because the resource check asks for it on every proof
// it reads — once per DPoP-authenticated request — and the answer never differs: the set is a
// package-level constant in all but name, and the library only reads the names it is given.
var proofAlgorithmNames = func() []string {
	names := make([]string, 0, len(proofAlgorithms))
	for _, algorithm := range proofAlgorithms {
		names = append(names, string(algorithm))
	}
	return names
}()

// Caller is who a verified token names, whether that token only reads, and whether it holds the
// scope legion-status: what a writer is recorded under, and nothing else the broker's answer
// carries. It never carries the token.
type Caller struct {
	// Session is the session the writes are recorded as, which a Caller always names: an answer
	// naming none is ErrNotASession and never reaches one.
	Session string
	// Person is the person that session acts for. A Caller names a person or a service, never
	// both and never neither: an answer that names no person names the service it is.
	Person string
	// Service is the service a caller that names no person is, as the broker's own
	// configuration names it: the fleet the machine presenting the token belongs to, the
	// service a read entry names, the account of an account entry naming no person, which the
	// token acts as (system:serviceaccount:<namespace>:<name>), or, for a job's token, the
	// identity its job entry lists — a pod's Kubernetes service account, a workflow, or a Google
	// account. Whoever mounts this check decides what such a caller may do; what this package
	// promises is that one of the two is set.
	Service string
	// Projects is the token's list of projects, the caller's own copy: a ProjectScopeNamed
	// token's one project, or none for an account entry naming none (the shared public-session
	// account's, and an account entry naming no person), and exactly one for a job's token; the
	// projects a ProjectScopePerson token's trade asked for, each one its person could see then,
	// nil for none, and none for a token that names a service; and a ProjectScopePublic token's
	// private projects, its job entry's whole list of 0 to oauthwire.MaxProjects, beside which it
	// reaches every public project, which no list names. A read-only caller (ReadOnly) names what
	// its base word names: a token narrowed to reading names the projects the same trade would
	// have named without read, and a ProjectScopeNone token names none. Sorted, each once.
	Projects []string
	// ProjectScope is the broker's own `project_scope` claim read as its base word
	// (oauthwire.BaseProjectScope), and not down to a flag: a reader deciding what a token may do
	// has to tell "the broker scoped this to its person" from every other answer, and a job's
	// public list, which every public project joins, from a named token's one project; an absent
	// or misspelt scope must not read as ProjectScopePerson. A narrowed word is carried as the
	// word it narrows, with ReadOnly beside it, so a reader of the projects reads one rule for
	// both. It is ProjectScopeNone only when ReadOnly is set, but a read-only caller may carry any
	// of the three words. ProjectScopePublic only comes beside a Service and no Person.
	ProjectScope string
	// ReadOnly is true exactly when the verifier accepted a read-only token (shapeOf): the read
	// entry's token, which is the project scope ProjectScopeNone beside the one scope ScopeRead, a
	// service, and no person and no project; or a token narrowed to reading, which is a narrowed
	// project scope (oauthwire.ProjectScopeReadNamed, ProjectScopeReadPerson or
	// ProjectScopeReadNone) beside the scope read, or read person for a read-person token that
	// names its person. Only a verifier built with Options.ServesReadOnly answers such a Caller,
	// and the service that set the switch is the one that decides which of its routes the caller
	// may call; it writes none of them.
	ReadOnly bool
	// Cut is the past time a read-only token reads at, which its trade asked for (the answer's
	// as_of, oauthwire.FormAsOf), in UTC; it is the zero time for a token that names no cut. Only a
	// token narrowed to reading carries one: an answer that names a cut in any other shape, the
	// read entry's none included, or a cut that is not an RFC 3339 time, is ErrNotASession.
	Cut time.Time
	// LegionStatus is true when the broker's answer's Scope holds the word ScopeLegionStatus.
	// The broker grants that scope to a Legion daemon's own token alone. This package reads the
	// word and decides nothing with it: the reader decides what it allows (Dispatch,
	// brokerTokenActor). A reader that ignores it reads the token as it reads one without it.
	LegionStatus bool
}

// Options is what a Verifier needs. BrokerURL, Account, Origins, Replay and Address are required.
type Options struct {
	// BrokerURL is the secrets broker's base URL (DISPATCH_AGENT_SECRETS_URL for Dispatch,
	// ENVOY_AGENT_SECRETS_URL for a listener), the origin IntrospectPath is asked on.
	BrokerURL string
	// Account reads the caller's own platform identity, the bearer each introspection presents:
	// a projected Kubernetes ServiceAccount token (AccountToken) or a machine's Google identity
	// token (GoogleAccount).
	Account Account
	// Origins are every origin the mounting program is reached at, each an absolute http(s) URL
	// with no path: for Dispatch DISPATCH_SERVER_URL's first and then each DISPATCH_PROOF_ORIGINS
	// adds, for a listener ENVOY_PUBLIC_URL alone. A proof's 'htu' is checked against the one of
	// them it names plus the request's path, never against a forwarded header or the Host the
	// request arrived under — see originFor and atOrigin. A proof naming none of them is checked
	// against the first, and so refused as a target that is not this request's.
	Origins []string
	// Replay is where a spent proof is recorded, so a captured one cannot be presented twice
	// (store.DPoPProofs for Dispatch, the spent-proof KV bucket for a listener).
	Replay rfc9449.DPoPReplayStorage
	// Address is the source address one introspection bucket is keyed on (DefaultAddressLimit),
	// read from the request as the deployment's own proxy-trust rule reads it — Dispatch passes
	// auth.ForwardedAddress. It is required and has no default: a default would either carry
	// one caller's proxy-trust rule into another's listener, or key every request behind a
	// proxy on one bucket, and both stop the bucket bounding what reaches the broker per
	// caller. New refuses a Verifier without one rather than letting a release lose the bound
	// quietly.
	Address func(r *http.Request) string
	// HTTP sends the introspection; nil is a client bounded at introspectTimeout.
	HTTP *http.Client
	// AddressLimit is each source address's introspection bucket; the zero value is
	// DefaultAddressLimit.
	AddressLimit ratelimit.Limit
	// Now is the clock the cache and the address buckets read; nil is time.Now. A proof's own
	// acceptance window is the library's and stays on the real clock whatever this says, and so
	// does an answer's 'exp', which the broker dated — so a test's clock starts from the real one
	// and moves forward from there rather than naming an arbitrary instant.
	Now func() time.Time
	// ServesReadOnly is this service serving a read-only token on the routes it chooses: the
	// read entry's token and a token narrowed to reading (Caller.ReadOnly, shapeOf). A verifier
	// built with it accepts such a token and marks its Caller ReadOnly, with the token's cut
	// (Caller.Cut), and the service holds every route it does not choose closed to such a caller.
	// Only Dispatch sets it, and Dispatch decides which of its routes such a caller may call
	// (cmd/dispatch, brokerTokenVerifier).
	//
	// Without it a read-only token is ErrNotASession, as on a build before the switch existed:
	// a service that leaves it unset, the Envoy listener among them, reads no Caller.ReadOnly,
	// so admitting the token there would open every route that service serves to a token minted
	// only to read.
	ServesReadOnly bool
}

// Verifier decides callers presenting a broker-issued DPoP token. It is safe for concurrent use
// and is built once per process.
type Verifier struct {
	brokerURL string
	account   Account
	// origins are Options.Origins, the first the one a proof naming none of them is checked
	// against (originFor).
	origins   []proofOrigin
	http      *http.Client
	strategy  *rfc9449.DefaultStrategy
	address   func(r *http.Request) string
	addresses *ratelimit.Keyed
	cache     *answerCache
	flight    singleflight.Group
	now       func() time.Time

	// servesReadOnly is Options.ServesReadOnly: whether live lets a read-only answer through.
	servesReadOnly bool
}

// proofOrigin is one origin of Options.Origins as the verifier keeps it.
type proofOrigin struct {
	// url is the origin as configured: atOrigin puts its scheme and host on the request it
	// rewrites.
	url *url.URL
	// host is url's host as the library compares an 'htu' (normalizeHTU), the scheme's default
	// port dropped, which originFor compares a proof's host with, without case.
	host string
	// tls is the TLS state atOrigin puts on the request it rewrites: one empty state for the
	// whole process when the origin is https, nil when it is not. Nothing reads more of it than
	// whether it is there, and nothing writes to it, so every request shares this one.
	tls *tls.ConnectionState
}

// New builds a Verifier, refusing options it cannot serve a single request with rather than
// failing one request at a time: a missing dependency, no origin at all, and an origin that is
// not an absolute http(s) origin are all deployment errors.
func New(options Options) (*Verifier, error) {
	if options.BrokerURL == "" {
		return nil, errors.New("brokertoken: the broker URL is required")
	}
	if options.Account == nil {
		return nil, errors.New("brokertoken: the account token reader is required")
	}
	if options.Replay == nil {
		return nil, errors.New("brokertoken: the DPoP replay store is required")
	}
	if options.Address == nil {
		return nil, errors.New("brokertoken: the source address function is required")
	}
	if len(options.Origins) == 0 {
		return nil, errors.New("brokertoken: at least one origin is required")
	}
	origins := make([]proofOrigin, len(options.Origins))
	for i, raw := range options.Origins {
		origin, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("brokertoken: the origin %q does not parse: %w", raw, err)
		}
		if (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.Path != "" {
			return nil, fmt.Errorf("brokertoken: the origin %q must be an absolute http(s) URL with no path", raw)
		}
		origins[i] = proofOrigin{url: origin, host: withoutDefaultPort(origin.Scheme, origin.Host)}
		// One empty state serves every request atOrigin rewrites to this origin, built here
		// rather than per request: what reads it asks only whether the request carried TLS.
		if origin.Scheme == "https" {
			origins[i].tls = &tls.ConnectionState{}
		}
	}
	limit := options.AddressLimit
	if limit == (ratelimit.Limit{}) {
		limit = DefaultAddressLimit
	}
	client := options.HTTP
	if client == nil {
		client = &http.Client{Timeout: introspectTimeout}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		// The trailing slash goes so BrokerURL + IntrospectPath is one path, as cipher.New
		// trims it for the same reason.
		brokerURL: strings.TrimRight(options.BrokerURL, "/"),
		account:   options.Account,
		origins:   origins,
		http:      client,
		strategy:  rfc9449.NewDefaultStrategy(proofConfig{}, withoutNonces{options.Replay}),
		address:   options.Address,
		addresses: ratelimit.NewKeyed(limit),
		cache:     newAnswerCache(),
		now:       now,

		servesReadOnly: options.ServesReadOnly,
	}, nil
}

// RetryAfter is how long a source address waits for its next introspection, for the Retry-After
// an ErrRateLimited answer carries.
func (v *Verifier) RetryAfter() time.Duration { return v.addresses.Every() }

// Presented reports the access token r carries under the DPoP scheme, and whether it carries one
// at all. A bearer token is not one: RFC 9449 Section 7.2 refuses a DPoP-bound token presented
// under any other scheme, and whoever mounts this check routes a Bearer request to the credential
// that owns that scheme.
//
// The header is read here rather than through rfc9449.AccessTokenFromRequest, which this would
// otherwise be a line of: for a request not under the DPoP scheme that helper falls back to
// RFC 6750's other two places for a token, and a request carrying no Authorization header at all
// — a browser's, under its session cookie — reaches the form one, which costs
// r.ParseMultipartForm. A check asked whether such a request is this credential would consume
// the body of every multipart upload that is not. The scheme comparison below is the helper's
// own, case insensitive as RFC 9110 Section 11.1 requires of an auth scheme.
func Presented(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, oauth2.DPoPAccessToken) || token == "" {
		return "", false
	}
	return token, true
}

// Verify decides the caller of r. It asks the broker what the presented token is — answering an
// affirmative answer from its cache while that answer is good — and then holds the caller to the
// key that token is bound to, through the library's own RFC 9449 resource check.
//
// Its error is one of the sentinels above, and the Caller it answers is only ever one the broker
// called live and whose proof verified.
func (v *Verifier) Verify(ctx context.Context, r *http.Request) (Caller, error) {
	token, answer, caller, err := v.live(ctx, r, true)
	if err != nil {
		return Caller{}, err
	}
	signed := v.atOrigin(r, v.originFor(r))
	if _, err := v.strategy.ValidateResourceAccess(ctx, signed, token, answer.Thumbprint, false); err != nil {
		return Caller{}, v.refused(err, signed, answer.Thumbprint)
	}
	return caller, nil
}

// Live is Verify without the proof check: the broker's answer alone, cached or fresh.
//
// It is for a connection that outlives the request that opened it and whose caller has to keep
// resolving while it is open — a websocket Dispatch re-resolves on every heartbeat
// (api.whileCallerResolves) — where the opening request's proof already authenticated the
// caller. A proof is single use by its 'jti' and 'htu', so presenting that one again is a replay
// and would close a sound connection; what is left to ask of an open connection is whether the
// token is still live, which is this. Nothing admits a caller through it.
func (v *Verifier) Live(ctx context.Context, r *http.Request) (Caller, error) {
	_, _, caller, err := v.live(ctx, r, true)
	return caller, err
}

// LiveUntil is Live and the instant the broker's answer says the token stops being live: its
// 'exp', or the zero time when the broker named none. A connection that outlives its request reads
// it to tell a token whose life ended from one the broker ended early, since both answer
// ErrNotActive once they are over and only the first was due: the listener's relay socket closes
// the one for a fresh trade at once and the other as a refusal (cmd/listener, relay_socket.go).
//
// Unlike Live it spends nothing of the source address's bucket: its caller is a connection whose
// opening request Verify admitted with its proof, which spent the bucket then, and which asks
// again at an interval its own process sets (the relay socket's 20 s check, of at most
// relaySocketMaxPerListener sockets). Charging it would let a stranger who empties the bucket of
// an address, which every client behind one load balancer shares, close every connection opened
// from it.
func (v *Verifier) LiveUntil(ctx context.Context, r *http.Request) (Caller, time.Time, error) {
	_, answer, caller, err := v.live(ctx, r, false)
	if err != nil {
		return Caller{}, time.Time{}, err
	}
	return caller, expiryOf(answer), nil
}

// live is the broker's half of both checks: the token r presents, the broker's answer about it,
// and the caller sessionCaller reads that answer as, held to this verifier's read-only switch.
// Verify and Live both read the answer through here, so an open connection's heartbeat is held to
// the same rule as the request that opened it, the read-only switch included. spend says whether
// an introspection spends the source address's bucket (known).
func (v *Verifier) live(ctx context.Context, r *http.Request, spend bool) (string, Introspection, Caller, error) {
	token, ok := Presented(r)
	if !ok {
		return "", Introspection{}, Caller{}, ErrNotPresented
	}
	answer, err := v.known(ctx, r, token, spend)
	if err != nil {
		return "", Introspection{}, Caller{}, err
	}
	caller, err := sessionCaller(answer, v.servesReadOnly)
	if err != nil {
		return "", Introspection{}, Caller{}, err
	}
	return token, answer, caller, nil
}

// SessionCaller is the NATS auth service's reading of the broker's answer about a token as a
// session's caller (internal/cipher/natscallout): sessionCaller with the read-only switch off, so it
// serves no read-only token, as a verifier built without Options.ServesReadOnly serves none. It
// holds no proof to the key; whoever calls it checks the proof its credential carries against
// answer.Thumbprint.
func SessionCaller(answer Introspection) (Caller, error) {
	return sessionCaller(answer, false)
}

// sessionCaller is the one reading of the broker's answer about a token as a session's caller:
// Verify and Live make it through live with their verifier's Options.ServesReadOnly, and the NATS
// auth service through SessionCaller with the switch off. The token is live (ErrNotActive), bound
// to a key (ErrKeyMismatch: no proof can be the key it does not have), and issued for acting as a
// session in a shape this package reads, a read-only one only when servesReadOnly is set
// (ErrNotASession); the Caller it names is callerOf the answer. A job's token, which names a
// service and no person, is read in its two shapes: named with exactly one project, and public
// with 0 to oauthwire.MaxProjects private projects.
func sessionCaller(answer Introspection, servesReadOnly bool) (Caller, error) {
	if !answer.Active {
		return Caller{}, ErrNotActive
	}
	if answer.Thumbprint == "" {
		return Caller{}, ErrKeyMismatch
	}
	// A token names a person or a service: an exchange decided one of the two for every token
	// issued for acting as a session, and one that names neither is no session's credential
	// whatever else it carries.
	//
	// The rest is the shape and the list the Projects beside it are read under. The shape is the
	// project scope read together with the OAuth scope: a project-scope word this package does
	// not know, or a shape the broker mints no token in, is a token it cannot read, and is refused
	// here rather than taken as a token that names no project at all (ErrNotASession). A
	// read-only token passes only a verifier whose service serves one, and a public token only
	// beside a service and no person (shapeOf). A named token is an operator's entry's, which
	// names one project, or none for an account entry that names none: such an entry names a
	// person, or names no person and acts as its account, which is the token's service and is
	// always a whole Kubernetes account subject (oauthwire.AccountSubject), so a named answer
	// naming no person, no project and any other service, a fleet's name or a string that only
	// opens like an account among them, is no token the broker mints. A job's named token names
	// no person, exactly one project and its job entry's identity, which may be an account, a
	// workflow or a Google account, and holds no OAuth scope; it is the one named shape that names
	// a project and no person, and a token narrowed from a named one is not it, since the
	// exchange narrows no job's token. A named answer naming two or more projects, which a reader
	// holding a write to one project would take as no limit at all, is the same case. A token
	// narrowed from a named one is held to the same rules, since its projects mean what a named
	// token's do (oauthwire.BaseProjectScope). A service's token under the scope person names no
	// project, since the exchange refuses a machine's trade naming one; under named and public a
	// service's token may name its entry's list. A list no exchange mints (oauthwire.ValidProjects:
	// more than oauthwire.MaxProjects, a key that is none, or keys not sorted and named once) is
	// refused under every scope for the same reason. So is a cut that is not an RFC 3339 time
	// (cutOf); shapeOf has already refused a cut on a token that is not narrowed to reading.
	shape := shapeOf(answer)
	named := oauthwire.BaseProjectScope(answer.ProjectScope) == ProjectScopeNamed
	// jobScope is whether the answer's project scope is a job's token's, compared as the word the
	// broker answered rather than its base word (oauthwire.BaseProjectScope): a service and no
	// person may name projects only under named or public unnarrowed, since the broker narrows no
	// job's token (oauth's decide refuses a job's read trade). A narrowed token naming no person
	// and carrying projects, read-named among them, is no token the broker mints, and reading it
	// as its base word would take it for a job's.
	jobScope := answer.ProjectScope == ProjectScopeNamed || answer.ProjectScope == ProjectScopePublic
	cut, cutErr := cutOf(answer)
	if answer.Session == "" || (answer.Person == "" && answer.Service == "") || answer.ClientID != ExchangeClientID ||
		shape == shapeNotASession || (shape == shapeReadOnly && !servesReadOnly) ||
		(named && len(answer.Projects) > 1) ||
		(named && answer.Person == "" && len(answer.Projects) == 0 && !oauthwire.AccountSubject.MatchString(answer.Service)) ||
		(answer.Person == "" && len(answer.Projects) > 0 && !jobScope) ||
		oauthwire.ValidProjects(answer.Projects) != nil || cutErr != nil {
		return Caller{}, ErrNotASession
	}
	return callerOf(answer, shape, cut), nil
}

// tokenShape is what an answer's project scope and its OAuth scope together make of a token
// issued for acting as a session (shapeOf).
type tokenShape int

const (
	// shapeNotASession is an answer no verifier reads as a session's token, whatever its
	// switch: a project scope that is absent or a word this package does not know, the project
	// scope none in any shape but shapeReadOnly's or beside a cut, the project scope public beside
	// a person, without a service or beside any OAuth scope or a cut, the project scope named
	// beside no person and any OAuth scope, the scope read or a cut beside the project scope named
	// or person, or a narrowed project scope in any shape but shapeReadOnly's.
	shapeNotASession tokenShape = iota
	// shapeOrdinary is the project scope named beside a person with no scope read and no cut
	// beside it, named beside no person with no OAuth scope and no cut, the project scope person
	// with no scope read and no cut beside it, or the project scope public beside a service, no
	// person, no OAuth scope and no cut: every token the broker minted before read entries
	// existed, every job's token, and every token it mints from any other entry, and for any
	// trade that does not ask for read, now.
	shapeOrdinary
	// shapeReadOnly is a token that only reads, which a verifier accepts only under
	// Options.ServesReadOnly. It is the token a read entry mints (BROKER_EXCHANGE_READ_ACCOUNTS):
	// the project scope none, the scope exactly the one word read, a service, no person, no
	// projects and no cut. It is also a token narrowed to reading because its trade asked for
	// read: a narrowed project scope (read-named, read-person, read-none) beside the scope read,
	// or read person for a read-person token that names its person, with a cut or none.
	shapeReadOnly
)

// shapeOf is the one rule that says what a read-only token is and what a job's token is, and so
// what every other answer is beside them. The read-only shapes are exact. The read entry's token
// is the project scope ProjectScopeNone, a Scope that is the one word ScopeRead, a service, no
// person, no projects and no cut, and oauthwire.ProjectScopeReadNone is held to the same but may
// carry a cut. A token of oauthwire.ProjectScopeReadNamed or oauthwire.ProjectScopeReadPerson
// holds the one word ScopeRead, and a read-person token that names its person may hold ScopeRead
// and oauthwire.ScopePerson; sessionCaller holds its projects to its base word's rules. An answer
// carrying none or a narrowed word in any other shape is no session's token, and so is one whose
// Scope holds the word read, or that names a cut, beside the project scope named or person, and
// one that names a cut beside none: no broker mints any of them, since a cut needs a trade that
// asks for read, which mints a narrowed word, and read beside a project scope that writes would
// let a token meant to read write.
//
// A token that names no person and is no read-only token — a job's, or an account entry's naming
// no person — holds no OAuth scope at all, since the broker grants neither one, and no cut, since
// the exchange narrows neither. A job's public shape is ProjectScopePublic beside a service, no
// person and no scope word; public beside a person, which no entry that names a person mints,
// with no service, or beside any scope word is no session's token. The named shape of both is
// ProjectScopeNamed beside no person and no scope word; named beside no person and any scope
// word, person and approve included, is no session's token, while named beside a person keeps
// the scope its entry grants.
//
// Scope is read as RFC 7662 spells it, words separated by spaces, and as the set of words RFC 6749
// (section 3.3) makes it: the order of its words means nothing, so read person and person read
// are one scope. A word named twice is no set the broker grants, and no read-only shape. It is
// read one word at a time and with no slice built of them, because this runs on every request a
// verifier decides, an answer served from the cache included.
func shapeOf(answer Introspection) tokenShape {
	// reads and persons count the two words a narrowed token's scope is made of, and other is any
	// word besides them.
	reads, persons, other := 0, 0, false
	for word := range strings.FieldsSeq(answer.Scope) {
		switch word {
		case ScopeRead:
			reads++
		case oauthwire.ScopePerson:
			persons++
		default:
			other = true
		}
	}
	read := reads > 0
	// scoped is whether the answer holds any OAuth scope at all.
	scoped := read || persons > 0 || other
	readAlone := reads == 1 && persons == 0 && !other
	readPerson := reads == 1 && persons == 1 && !other
	switch answer.ProjectScope {
	case ProjectScopeNamed:
		// A named token that names no person is a job's or an account entry's naming no person,
		// and the broker grants neither one an OAuth scope.
		if read || answer.AsOf != "" || (answer.Person == "" && scoped) {
			return shapeNotASession
		}
		return shapeOrdinary
	case ProjectScopePerson:
		if read || answer.AsOf != "" {
			return shapeNotASession
		}
		return shapeOrdinary
	case ProjectScopePublic:
		if !scoped && answer.AsOf == "" && answer.Person == "" && answer.Service != "" {
			return shapeOrdinary
		}
	case ProjectScopeNone:
		// The read entry's token: a trade that asks for no scope keeps the word none, and the
		// broker refuses a cut on a trade that does not ask for read, so none never carries one.
		if readAlone && answer.AsOf == "" && answer.Person == "" && len(answer.Projects) == 0 && answer.Service != "" {
			return shapeReadOnly
		}
	case oauthwire.ProjectScopeReadNone:
		if readAlone && answer.Person == "" && len(answer.Projects) == 0 && answer.Service != "" {
			return shapeReadOnly
		}
	case oauthwire.ProjectScopeReadNamed:
		if readAlone {
			return shapeReadOnly
		}
	case oauthwire.ProjectScopeReadPerson:
		if readAlone || (readPerson && answer.Person != "") {
			return shapeReadOnly
		}
	}
	return shapeNotASession
}

// known is the broker's answer about token: the one remembered for it while that answer is still
// good, or a fresh introspection.
//
// The cache is read first, because an answer served from it reaches no broker and so spends
// nothing of the budget the bucket guards. A miss joins the single flight for that token, and the
// request is taken from the source address's bucket inside that flight, by whichever request opens
// it and once the flight's own look at the cache has missed too — so one request of the bucket is
// taken for exactly one introspection. N requests arriving at once with one cold token therefore
// cost one introspection and take one request of the bucket, where charging each of them would
// refuse an honest caller that made a burst of calls on a token it had just been given.
//
// The request is taken before the introspection is sent, as INFRA-228 requires
// (DefaultAddressLimit), and given back only when the broker answers that the token is live,
// stamped with Options.Now, the clock the take read. The give-back follows the broker's answer
// alone, whatever Verify decides about the token after it (a token that is no session's
// credential, a proof that does not verify), since a token the broker calls live is one it
// issued. A not-live answer keeps its request, and so does an introspection that ends in an
// error, an answer that never came or one this reader does not read: nothing has shown the token
// live, and the call still cost the broker's budget.
//
// So the bucket bounds only the introspections of tokens the broker does not answer live. A
// stranger pays one request per distinct token it presents, which is the flood the bucket exists
// for, while a live token's introspection gives back what it took, so many honest callers that
// share one address, as every caller behind a gateway does, are not refused however many live
// tokens they hold between them. What holds the introspections of a live token down is the cache
// and the single flight, not the bucket: its answer is remembered (answerCache.remember), so the
// requests after it are served from the cache until that answer lapses. Where the cache keeps
// nothing, because it is full of answers still good and keeps those rather than evict one, or
// because the answer's own 'exp' has already passed, every request on that token is a new
// introspection and each gives its request back, so the bucket bounds none of them. What bounds
// them then is the single flight, which holds the token to one introspection in flight at a time,
// and past it the broker's own bucket, whose 429 is ErrBrokerUnreachable.
//
// With spend false the bucket is neither taken from nor given back to (LiveUntil).
//
// The introspection runs under the context of whichever request opened the flight, so one that
// goes away takes the others' answer with it; they are answered ErrBrokerUnreachable and come
// back, which is what every other lost answer does.
func (v *Verifier) known(ctx context.Context, r *http.Request, token string, spend bool) (Introspection, error) {
	key := sha256.Sum256([]byte(token))
	if answer, ok := v.cache.lookup(key, v.now()); ok {
		return answer, nil
	}
	shared, err, _ := v.flight.Do(string(key[:]), func() (any, error) {
		// The flight's own look at the cache: a request that waited for this flight's
		// predecessor to finish would otherwise introspect again for an answer just remembered.
		if answer, ok := v.cache.lookup(key, v.now()); ok {
			return answer, nil
		}
		giveBack := func(time.Time) {}
		if spend {
			back, ok := v.addresses.TakeAt(v.address(r), v.now())
			if !ok {
				return Introspection{}, ErrRateLimited
			}
			giveBack = back
		}
		answer, err := v.introspect(ctx, token)
		if err != nil {
			return Introspection{}, err
		}
		// Only an affirmative answer is remembered. A refusal held even briefly would keep
		// refusing a token the broker has just issued or reinstated, and it saves nothing worth
		// having: a caller presenting a token the broker does not know is the caller the address
		// bucket above is there to slow.
		if answer.Active {
			giveBack(v.now())
			v.cache.remember(key, answer, v.now())
		}
		return answer, nil
	})
	if err != nil {
		return Introspection{}, err
	}
	return shared.(Introspection), nil
}

// originFor is the origin of Options.Origins that r's proof names in its 'htu', which Verify
// checks the proof against. A proof that names none of them, or whose 'htu' cannot be read here,
// is checked against the first, and the library refuses it as it refuses any target that is not
// this request's: ErrProofInvalid, before the replay store is asked anything. A proof is spent in
// the one replay store under the target it names, so it is spent once whichever origin it names
// and whichever host the request arrived under. With one origin there is nothing to choose, and
// the proof is not read here at all.
//
// The 'htu' is read here without the proof's signature being checked: it only chooses the
// expected target, and the library's check, which verifies that signature, compares that same
// claim with it, so a proof whose claim was altered is refused there whatever was chosen.
// Scheme and host are compared as the library compares them (normalizeHTU): without case, and
// with the scheme's default port dropped, so no proof the library would accept at an origin is
// refused for having been checked against another.
func (v *Verifier) originFor(r *http.Request) *proofOrigin {
	if len(v.origins) > 1 {
		if target, err := url.Parse(claimedTarget(r.Header.Get("DPoP"))); err == nil {
			host := withoutDefaultPort(target.Scheme, target.Host)
			for i := range v.origins {
				origin := &v.origins[i]
				if strings.EqualFold(target.Scheme, origin.url.Scheme) && strings.EqualFold(host, origin.host) {
					return origin
				}
			}
		}
	}
	return &v.origins[0]
}

// claimedTarget is the 'htu' claim of proof, a compact JWS, read without checking its signature
// (originFor says why that is sound), or "" when proof holds none this can read.
func claimedTarget(proof string) string {
	_, rest, found := strings.Cut(proof, ".")
	if !found {
		return ""
	}
	payload, _, found := strings.Cut(rest, ".")
	if !found {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims struct {
		Target string `json:"htu"`
	}
	if json.Unmarshal(decoded, &claims) != nil {
		return ""
	}
	return claims.Target
}

// withoutDefaultPort is host with scheme's default port dropped, as RFC 3986 Section 6.2.3 has an
// 'htu' compared and the library compares it.
func withoutDefaultPort(scheme, host string) string {
	switch {
	case strings.EqualFold(scheme, "https"):
		host, _ = strings.CutSuffix(host, ":443")
	case strings.EqualFold(scheme, "http"):
		host, _ = strings.CutSuffix(host, ":80")
	}
	return host
}

// atOrigin is r as if it had reached origin, one of the mounting program's own origins, directly:
// its URL's scheme and host and its Host are that origin's, it carries TLS state exactly when that
// origin is https, and it has no forwarding headers.
//
// The library rebuilds a proof's expected 'htu' from X-Forwarded-Proto and r.Host
// (oauth2.RequestURL), both of which a caller behind a proxy could choose, so the proof would
// name a URL the caller picked rather than one of Dispatch's. The broker answers this the same
// way for its own OAuth routes (internal/cipher/api/handlers_oauth.go, atPublicURL): the caller
// signs the origin it calls plus the request's path, and Dispatch checks against exactly that,
// for an origin it is configured to be reached at (originFor).
func (v *Verifier) atOrigin(r *http.Request, origin *proofOrigin) *http.Request {
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host, out.Host = origin.url.Scheme, origin.url.Host, origin.url.Host
	out.TLS = origin.tls
	out.Header.Del("X-Forwarded-Proto")
	out.Header.Del("X-Forwarded-Host")
	return out
}

// refused is what the library's refusal of a proof means here.
//
// The library reports every RFC 9449 Section 4.3 criterion as the one invalid_dpop_proof error,
// so the key mismatch — the criterion that tells a stolen token from a sound one, and the one
// worth its own outcome — is told apart by reading the proof's own key rather than the refusal's
// prose. ParseProof is the library's own parser and runs only on this path, behind the address
// bucket; a proof that does not even parse is no key mismatch, which is what the error from it
// means here. A failure of the replay store is the library's server_error, which is Dispatch's
// own failure and neither a refusal nor the broker's.
func (v *Verifier) refused(err error, signed *http.Request, boundJKT string) error {
	if errors.Is(err, oauth2.ErrServerError) {
		return fmt.Errorf("brokertoken: check the DPoP proof: %w", err)
	}
	if proof, parseErr := rfc9449.ParseProof(signed.Header.Get("DPoP"), proofAlgorithms); parseErr == nil && proof.Thumbprint != boundJKT {
		return ErrKeyMismatch
	}
	// The hint, never the debug field: a hint names the claim that failed and the request's own
	// method and target URI, and nothing of the token or the key.
	return fmt.Errorf("%w: %s", ErrProofInvalid, oauth2.ErrorToRFC6749Error(err).HintField)
}

// proofConfig is the rfc9449.StrategyConfig the resource check reads its window and algorithms
// from.
type proofConfig struct{}

func (proofConfig) GetDPoPAllowedJWSAlgorithms(context.Context) []string {
	return proofAlgorithmNames
}

func (proofConfig) GetDPoPClockSkew(context.Context) time.Duration     { return ProofClockSkew }
func (proofConfig) GetDPoPProofLifespan(context.Context) time.Duration { return ProofLifespan }

// GetDPoPNonceLifespan is read only where a nonce is minted (DefaultStrategy.NewDPoPNonce), which
// this package never calls, so it names no window.
func (proofConfig) GetDPoPNonceLifespan(context.Context) time.Duration { return 0 }

// withoutNonces is a replay store as the whole rfc9449.Storage the strategy takes. Dispatch
// issues no DPoP nonce: a nonce challenge is a second round trip for a proof already bound to its
// token by 'ath', so Verify asks for none (requireNonce false), and with that
// ValidateResourceAccess reaches neither of these methods —
// handler/rfc9449/strategy.go's ValidateDPoPProof skips the whole nonce branch, and
// CreateDPoPNonce is reached only from NewDPoPNonce.
//
// So each answers an error rather than a value: a release that starts asking for a nonce fails
// loudly instead of minting one nothing stored or accepting one nobody issued.
type withoutNonces struct {
	rfc9449.DPoPReplayStorage
}

// errNoNonces is why: Dispatch mints none.
var errNoNonces = errors.New("brokertoken: Dispatch issues no DPoP nonce")

func (withoutNonces) CreateDPoPNonce(context.Context, string, time.Time) error { return errNoNonces }

func (withoutNonces) IsDPoPNonceValid(context.Context, string) (bool, error) {
	return false, errNoNonces
}
