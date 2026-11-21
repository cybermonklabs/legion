package oidc

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrIssuerUnavailable means the issuer could not be asked: its discovery document has not been
// read yet, or a call to it failed in a way that says nothing about the token or the grant it
// carried (no answer, a timeout, a 5xx, a 429, a refusal of the client itself, an answer without a
// verifiable ID token). A caller answers it as a service that is down, and ends nothing on it.
var ErrIssuerUnavailable = errors.New("oidc: the issuer cannot be reached")

// IssuerUnavailableCode and IssuerUnavailableMessage are the error code and the sentence of the
// answer, 503, to a service-account token whose issuer could not be asked (ErrIssuerUnavailable).
// The broker's routes, its token exchange and Dispatch's API all answer that token with this
// pair, and a client compares the code. A sign-in whose issuer could not be asked is answered in
// the sign-in's own words, not these.
const (
	IssuerUnavailableCode    = "OIDC_ISSUER_UNAVAILABLE"
	IssuerUnavailableMessage = "the service-account token issuer cannot be reached; try again in a minute"
)

// ErrGrantRefused means the issuer's token endpoint refused a refresh token (RFC 6749 section
// 5.2, invalid_grant): it has expired, been revoked, or is not the client's. The sign-in it renews
// is over.
var ErrGrantRefused = errors.New("oidc: the issuer refused the refresh token")

const (
	// discoveryAttemptTimeout bounds one read of an issuer's discovery document.
	discoveryAttemptTimeout = 10 * time.Second
	// discoveryRetrySpacing is the least time between two failed discovery attempts: a request
	// that finds the issuer undiscovered within it fails at once with the last attempt's error,
	// so a burst of requests during an outage sends the issuer one read, not one each.
	discoveryRetrySpacing = 5 * time.Second
	// discoveryRetryFirst and discoveryRetryMax are the background loop's wait after its first
	// failed attempt, which doubles after each one up to the most.
	discoveryRetryFirst = time.Second
	discoveryRetryMax   = 30 * time.Second
	// discoveryLogInterval is the least time between two lines logging a failed discovery.
	discoveryLogInterval = time.Minute
)

// discovery reads one issuer's discovery document when it is first needed rather than at
// construction, so a process starts and serves whatever does not need the issuer while the issuer
// is down. Once read, the result is kept for the process's life. Concurrent attempts share one
// read (single flight), each read is bounded by discoveryAttemptTimeout, and after a failed read
// no other is made for discoveryRetrySpacing.
type discovery[T any] struct {
	issuer string
	read   func(ctx context.Context) (T, error)
	// now is the clock the spacing between failed attempts and the log's rate are measured by.
	now    func() time.Time
	flight singleflight.Group

	mu         sync.Mutex
	value      T
	discovered bool
	failedAt   time.Time
	failure    error
	loggedAt   time.Time
}

func newDiscovery[T any](issuer string, read func(ctx context.Context) (T, error)) *discovery[T] {
	return &discovery[T]{issuer: issuer, read: read, now: time.Now}
}

// done reports whether an attempt has succeeded.
func (d *discovery[T]) done() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.discovered
}

// discover reads the discovery document unless it has been read, joining an attempt already in
// flight, or fails at once with the last attempt's error when that attempt failed less than
// discoveryRetrySpacing ago. The attempt runs on a context of its own, detached from ctx's
// cancellation, since its result belongs to every caller; ctx ending stops only this caller's
// wait. Every error matches ErrIssuerUnavailable.
func (d *discovery[T]) discover(ctx context.Context) (T, error) {
	var zero T
	if value, err, settled := d.settled(); settled {
		return value, err
	}
	attempt := d.flight.DoChan("discover", func() (any, error) {
		// An attempt that ended just before this flight began has settled it already.
		if value, err, settled := d.settled(); settled {
			return value, err
		}
		attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discoveryAttemptTimeout)
		defer cancel()
		value, err := d.read(attemptCtx)
		d.mu.Lock()
		defer d.mu.Unlock()
		if err != nil {
			d.failedAt = d.now()
			d.failure = fmt.Errorf("%w: %w", ErrIssuerUnavailable, err)
			return zero, d.failure
		}
		d.value, d.discovered = value, true
		return value, nil
	})
	select {
	case result := <-attempt:
		if result.Err != nil {
			return zero, result.Err
		}
		return result.Val.(T), nil
	case <-ctx.Done():
		return zero, fmt.Errorf("%w: waiting for issuer %s's discovery: %w", ErrIssuerUnavailable, d.issuer, ctx.Err())
	}
}

// settled is the discovered value, or the last attempt's error while it is younger than
// discoveryRetrySpacing; settled is false when a new attempt is due.
func (d *discovery[T]) settled() (value T, err error, settled bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.discovered {
		return d.value, nil, true
	}
	if d.failure != nil && d.now().Sub(d.failedAt) < discoveryRetrySpacing {
		return value, d.failure, true
	}
	return value, nil, false
}

// run discovers the issuer from the process's start, so the first request that needs it does not
// wait for the read: it attempts at once and, while attempts fail, again after a wait of
// discoveryRetryFirst that doubles after each failure up to discoveryRetryMax. It logs a failure
// at ERROR at most once per discoveryLogInterval, and the success once, and returns once the
// issuer is discovered or ctx ends.
func (d *discovery[T]) run(ctx context.Context) {
	wait := discoveryRetryFirst
	for {
		if _, err := d.discover(ctx); err == nil {
			slog.Info("oidc: discovered the issuer", "issuer", d.issuer)
			return
		} else if ctx.Err() == nil {
			d.logFailure(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, discoveryRetryMax)
	}
}

func (d *discovery[T]) logFailure(err error) {
	d.mu.Lock()
	now := d.now()
	due := d.loggedAt.IsZero() || now.Sub(d.loggedAt) >= discoveryLogInterval
	if due {
		d.loggedAt = now
	}
	d.mu.Unlock()
	if due {
		slog.Error("oidc: discover the issuer; retrying in the background, and what needs it answers 503 meanwhile", "issuer", d.issuer, "error", err)
	}
}

// LazyVerifier is a Verifier whose issuer is discovered when first needed (see CodeFlow's
// discovery), for a server that must start and serve its other callers while that issuer is
// down. Until the discovery document has been read, Verify and Key fail with
// ErrIssuerUnavailable.
type LazyVerifier struct {
	discovery *discovery[lazyIssuer]
}

// lazyIssuer is what one read of a LazyVerifier's discovery document makes: the issuer's key set,
// and the verifier that checks tokens against it.
type lazyIssuer struct {
	verifier *Verifier
	keys     *Keys
}

// NewLazyVerifier returns a verifier for tokens issuer issues to audience, and reads nothing: the
// discovery document is read by Discover, by Run, or by the first Verify or Key.
func NewLazyVerifier(issuer, audience string, options ...LazyVerifierOption) *LazyVerifier {
	var settings lazyVerifierSettings
	for _, option := range options {
		option(&settings)
	}
	return &LazyVerifier{discovery: newDiscovery(issuer, func(ctx context.Context) (lazyIssuer, error) {
		keys, err := NewKeys(ctx, issuer)
		if err != nil {
			return lazyIssuer{}, err
		}
		keys.FailureSpacing = settings.keyFailureSpacing
		return lazyIssuer{verifier: NewWithKeys(issuer, audience, keys), keys: keys}, nil
	})}
}

// LazyVerifierOption changes how a LazyVerifier made by NewLazyVerifier reads its issuer.
type LazyVerifierOption func(*lazyVerifierSettings)

// lazyVerifierSettings are what the options of one NewLazyVerifier set; the zero value is a
// verifier read as New reads one.
type lazyVerifierSettings struct {
	keyFailureSpacing time.Duration
}

// WithKeyFailureSpacing sets the FailureSpacing of the verifier's key set (Keys), so that while
// the issuer's key set cannot be read, the tokens that arrive cost it one read per spacing rather
// than one each. A verifier made without it reads the key set again at every token that needs it.
func WithKeyFailureSpacing(spacing time.Duration) LazyVerifierOption {
	return func(settings *lazyVerifierSettings) { settings.keyFailureSpacing = spacing }
}

// Verify is Verifier.Verify once the issuer is discovered, discovering it first when it is not
// and an attempt is due; before that every error matches ErrIssuerUnavailable.
func (v *LazyVerifier) Verify(ctx context.Context, raw string) (Claims, error) {
	return v.VerifyClaims(ctx, raw)
}

// VerifyClaims is Verifier.VerifyClaims once the issuer is discovered, as Verify is Verifier.Verify:
// it also unmarshals the payload the signature covers into each of into.
func (v *LazyVerifier) VerifyClaims(ctx context.Context, raw string, into ...any) (Claims, error) {
	issuer, err := v.discovery.discover(ctx)
	if err != nil {
		return Claims{}, err
	}
	return issuer.verifier.VerifyClaims(ctx, raw, into...)
}

// Key is Keys.Key on the key set Verify reads, once the issuer is discovered, discovering it
// first when it is not and an attempt is due: the key a token naming kid and signed with alg is
// checked against, for a caller that needs the key itself, as the RFC 8693 token exchange does.
// Its error matches ErrIssuerUnavailable while the discovery document has not been read, and
// where Keys.Key answers ErrKeySet, since a key set that cannot be read decides nothing about the
// token, as Verify classifies the same error. Every other error is Keys.Key's.
func (v *LazyVerifier) Key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	issuer, err := v.discovery.discover(ctx)
	if err != nil {
		return nil, err
	}
	key, err := issuer.keys.Key(ctx, kid, alg)
	if errors.Is(err, ErrKeySet) {
		return nil, fmt.Errorf("%w: %w", ErrIssuerUnavailable, err)
	}
	return key, err
}

// Discover reads the issuer's discovery document unless it has been read; its error matches
// ErrIssuerUnavailable.
func (v *LazyVerifier) Discover(ctx context.Context) error {
	_, err := v.discovery.discover(ctx)
	return err
}

// Discovered reports whether the issuer's discovery document has been read.
func (v *LazyVerifier) Discovered() bool { return v.discovery.done() }

// Run discovers the issuer in the background until it succeeds or ctx ends (discovery.run).
func (v *LazyVerifier) Run(ctx context.Context) { v.discovery.run(ctx) }
