package oidc

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/redact"
)

// The class of every Key failure. A key the issuer does not publish and an issuer that cannot be
// read are different faults — the first is a token signed by something the issuer never had, the
// second is an outage — and only the second is worth an alarm, so a caller tells them apart by
// sentinel rather than by error text.
var (
	ErrNoKey        = errors.New("oidc: issuer publishes no such signing key")
	ErrAmbiguousKey = errors.New("oidc: issuer publishes several keys and the token names none")
	ErrKeySet       = errors.New("oidc: key set could not be read")
)

// The timings a Keys starts with. A caller that wants others sets the fields before its first Key.
const (
	// DefaultKeysLifetime is how long a fetched set answers before the next Key reads the issuer
	// again. It is the far side of the trade a cache makes: a key the issuer has withdrawn still
	// verifies a signature for this long after the last good read, and in exchange every
	// verification in the window costs no network read. A set that is stale by less than
	// MinRefresh still answers a key it holds at once while it is read again in the background,
	// and an older set is read again first. So for a caller that waits, while the issuer answers
	// within the caller's wait, a withdrawn key verifies for at most Lifetime + MinRefresh after
	// the last good read. A caller whose keys deadline (WithKeysDeadline) ends first is answered
	// from the cached set until that read lands, at most FetchTimeout later. While the issuer
	// fails, the cached set answers the keys it holds. A caller whose own context has ended gets
	// no answer from a set past Lifetime + MinRefresh; younger sets answer it as they answer any
	// caller.
	DefaultKeysLifetime = 15 * time.Minute
	// DefaultKeysMinRefresh is the shortest interval between two fetches made because a token
	// named a key id the cached set does not hold, while reads succeed; while they fail, it is one
	// such read per FailureSpacing. It is what keeps a rotation visible within a minute while a
	// token naming a key nobody ever published, replayed in a loop, costs the issuer one read per
	// minute and no more while the key set answers. It is also how long past Lifetime a stale set
	// still answers a key it holds without waiting for its refresh.
	DefaultKeysMinRefresh = time.Minute
	// DefaultKeysFetchTimeout bounds one key-set read. An issuer that accepts the connection and
	// never answers would otherwise hold a verification open for as long as the caller allows.
	DefaultKeysFetchTimeout = 10 * time.Second
	// DefaultKeysFailureSpacing is how long after a failed read no other read starts, until a
	// read succeeds: the failure answers instead, from the cached set where it holds the key, as
	// discovery's does (discoveryRetrySpacing). Without it a key set that is down is read as fast
	// as tokens arrive, one read per request. It is also how soon a key id a failed read could not
	// find may be looked for again, rather than waiting out MinRefresh.
	DefaultKeysFailureSpacing = 5 * time.Second
)

// maxKeySetBytes is the most of an issuer's key-set response that is read. A real set is a few
// kilobytes; the limit is what stops a misbehaving or hostile endpoint from being decoded into
// the broker's memory in full.
const maxKeySetBytes = 1 << 20

// Keys is an issuer's published signing keys as a key source: it answers the public key a token's
// `kid` and `alg` name, where a Verifier only answers whether a whole token verifies. The RFC 8693
// token exchange needs the key itself, because the OAuth provider validates a subject token
// through a jwt.Keyfunc — a function handed the parsed token that returns the key the library then
// checks the signature with — and go-oidc's RemoteKeySet verifies signatures without ever handing
// its keys out. So the exchange reads the issuer's set through this, beside the Verifier rather
// than through it.
//
// A Keys holds the set in memory and is safe for concurrent use: callers that find it stale or
// missing share one fetch rather than each making their own. The timing fields are read without
// the lock, so a caller changes them before its first Key and not after.
type Keys struct {
	// Lifetime is how long a fetched set is answered from before a Key reads the issuer again
	// (DefaultKeysLifetime).
	Lifetime time.Duration
	// MinRefresh is the shortest interval between two fetches prompted by a key id the cached set
	// does not hold, while reads succeed; after a failed one, the next may come once FailureSpacing
	// has passed. It is also how long past Lifetime a stale set answers a key it holds at once
	// (DefaultKeysMinRefresh).
	MinRefresh time.Duration
	// FetchTimeout bounds one key-set read, which runs detached from the caller that started it:
	// a caller that hangs up ends only its own wait (DefaultKeysFetchTimeout).
	FetchTimeout time.Duration
	// FailureSpacing is how long a failed read answers for, with no new read, until a read
	// succeeds; and how soon after it a key id the failed read was looking for may prompt another
	// read (DefaultKeysFailureSpacing).
	FailureSpacing time.Duration
	// HTTPClient reads the key set. A caller replaces it to route the read through a proxy or an
	// instrumented transport; it is never nil.
	HTTPClient *http.Client

	jwksURI string

	mu       sync.Mutex
	keys     []jose.JSONWebKey
	fetched  time.Time
	missed   time.Time
	inflight *keysFetch
	// failure and failedAt are the last failed read, kept only until a read succeeds: a good read
	// clears them. One read runs at a time (startReadLocked), so reads end in the order they start,
	// and failure != nil says the key set's latest answer was a failure, never judged by comparing
	// clock times, which can be equal.
	failedAt time.Time
	failure  error
}

// keysFetch is one key-set read in progress, which every caller that arrives while it runs waits
// on instead of starting its own.
type keysFetch struct {
	done chan struct{}
	keys []jose.JSONWebKey
	err  error
}

// NewKeys reads issuer's OIDC discovery document now and returns the key source for the keys it
// names, so a caller configures an issuer and nothing else. It refuses, naming the issuer, one
// whose discovery document cannot be read or names no jwks_uri. No key is fetched here: the first
// Key reads the set, and ctx bounds discovery alone.
func NewKeys(ctx context.Context, issuer string) (*Keys, error) {
	s := redact.Scope[*gooidc.Provider](ctx)
	provider, err := redact.OAuth2(s.Read(gooidc.NewProvider(s.Context(), issuer)))
	if err != nil {
		return nil, fmt.Errorf("oidc: discovering issuer %s: %w", issuer, err)
	}
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	claims := redact.Scope[struct{}](ctx)
	if _, err := redact.OAuth2(claims.Read(struct{}{}, provider.Claims(&discovery))); err != nil {
		return nil, fmt.Errorf("oidc: reading issuer %s discovery document: %w", issuer, err)
	}
	if discovery.JWKSURI == "" {
		return nil, fmt.Errorf("oidc: issuer %s names no jwks_uri in its discovery document", issuer)
	}
	return NewKeysAt(discovery.JWKSURI), nil
}

// NewKeysAt returns the key source for the key set published at jwksURI, for an issuer that
// publishes no discovery document to name it: Identity-Aware Proxy, whose set is at a fixed
// address (IAPKeySetURL). It reads nothing: the first Key reads the set.
func NewKeysAt(jwksURI string) *Keys {
	return &Keys{
		Lifetime:       DefaultKeysLifetime,
		MinRefresh:     DefaultKeysMinRefresh,
		FetchTimeout:   DefaultKeysFetchTimeout,
		FailureSpacing: DefaultKeysFailureSpacing,
		HTTPClient:     &http.Client{},
		jwksURI:        jwksURI,
	}
}

// Key answers the public key a token naming kid and signed with alg is to be checked against. An
// empty kid is answered only where one key of the set is usable for alg, since choosing among
// several would settle a signature's fate by position in a JSON array; an empty alg asks for no
// particular algorithm and narrows nothing.
//
// A set stale by less than MinRefresh (older than Lifetime, younger than Lifetime + MinRefresh)
// that holds the key asked for answers at once, and is read again in the background
// (refreshInBackground): a slow key set costs no verification its wait. An older set, and a stale
// set that does not hold the key, is read again first. So for a caller that waits, while the
// issuer answers within the caller's wait, a key it has withdrawn is answered for at most
// Lifetime + MinRefresh after the last good read. A caller whose keys deadline passes during
// that read is answered from the cached set where it holds the key, so for it the old set
// answers until the read lands, at most FetchTimeout later; a caller whose own context has ended
// is not answered from a set that old, while younger sets answer it as they answer any caller. A
// kid the set does not hold is a rotation the cache has not seen yet, so it prompts one more read,
// at most once per MinRefresh, or once FailureSpacing has passed since that read failed. A read
// that fails leaves the last good set serving a caller still waiting, with no new read for
// FailureSpacing, and is returned only when that set does not hold the key asked for — an issuer
// that is down stops no verification it does not have to. A kid the set does not hold while the
// read that would settle it has not (one is under way, or the last
// one failed within MinRefresh) is ErrKeySet, not ErrNoKey: no verdict on that key has been
// reached yet.
//
// Every error wraps ErrNoKey, ErrAmbiguousKey or ErrKeySet. The text names the key id asked for,
// which is a public identifier, shortened so an oversized one cannot flood a log, and no other
// part of the token.
func (k *Keys) Key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	set, fetched := k.state()
	stale := !fetched.IsZero() && time.Since(fetched) >= k.Lifetime
	if stale && time.Since(fetched) < k.Lifetime+k.MinRefresh {
		if key, err := selectKey(set, kid, alg); err == nil {
			k.refreshInBackground(ctx)
			return key, nil
		}
	}
	var fetchErr error
	read := false
	if fetched.IsZero() || stale {
		read = true
		fresh, err := k.fetch(ctx)
		switch {
		case err == nil:
			set = fresh
		case fetched.IsZero() || ctx.Err() != nil:
			// No set to fall back to, or a caller that has gone: a client that hung up or
			// half-closed gets no answer from a set past Lifetime + MinRefresh. A live caller whose
			// read failed, or whose keys deadline passed (wait cuts only a context derived from
			// ctx), falls back.
			return nil, err
		default:
			fetchErr = err
		}
	}
	key, err := selectKey(set, kid, alg)
	if err == nil {
		return key, nil
	}
	if read || !errors.Is(err, ErrNoKey) || !k.openMissWindow() {
		if fetchErr != nil {
			return nil, fetchErr
		}
		if !read && errors.Is(err, ErrNoKey) {
			if settling, key, missErr := k.settleMiss(ctx, kid, alg); settling {
				return key, missErr
			}
		}
		return nil, err
	}
	fresh, err := k.fetch(ctx)
	if err != nil {
		return nil, err
	}
	return selectKey(fresh, kid, alg)
}

// state is the cached set and when it was fetched, the zero time standing for a set never read.
func (k *Keys) state() ([]jose.JSONWebKey, time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keys, k.fetched
}

// openMissWindow reports whether a key id the cached set does not hold may prompt a read now, and
// opens the next MinRefresh window when it does. The window opens on the attempt rather than on
// the answer, so an issuer that is failing is asked no more often than one that is rotating. A
// window whose read has not succeeded, the key set's latest answer being a failure, closes early,
// once FailureSpacing has passed since that failure: the read it opened settled nothing, so the
// key may be looked for again, at most once per FailureSpacing while the key set keeps failing. A
// good read clears the failure, so a window it settled stays shut.
func (k *Keys) openMissWindow() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	unsettled := k.failure != nil && now.Sub(k.failedAt) >= k.FailureSpacing
	if !k.missed.IsZero() && now.Sub(k.missed) < k.MinRefresh && !unsettled {
		return false
	}
	k.missed = now
	return true
}

// settleMiss answers a key id the cached set does not hold, once the miss window has closed, while
// the read that would settle whether the issuer publishes it has not: a read under way is joined
// and the key is looked for in the set it reads (ErrKeySet when the read fails or the caller's wait
// ends first), and a last read that failed less than MinRefresh ago, with no good read since, is
// ErrKeySet. Either way the answer is never ErrNoKey, a signature no published key makes, before a
// read has said so. Its first result is false once a read since the miss has succeeded: that set's
// verdict stands.
func (k *Keys) settleMiss(ctx context.Context, kid, alg string) (bool, crypto.PublicKey, error) {
	k.mu.Lock()
	inflight := k.inflight
	failedRecently := k.failure != nil && time.Since(k.failedAt) < k.MinRefresh
	failure := k.failure
	k.mu.Unlock()
	switch {
	case inflight != nil:
		set, err := k.wait(ctx, inflight)
		if err != nil {
			return true, nil, err
		}
		key, err := selectKey(set, kid, alg)
		return true, key, err
	case failedRecently:
		return true, nil, fmt.Errorf("%w: key id %s is not in the cached set, and the last read of %s failed: %w", ErrKeySet, keyIDName(kid), k.jwksURI, failure)
	}
	return false, nil, nil
}

// fetch reads the key set and keeps it, or joins the read already in flight. One read serves every
// caller waiting on it, so a key set that is slow to answer costs the issuer one request however
// many tokens arrive meanwhile. A read that fails keeps the last good set.
//
// Every read runs on a context of its own, detached from the caller that started it and bounded by
// FetchTimeout alone. A caller waits for it only until its own context ends or until the keys
// deadline its context carries (WithKeysDeadline) passes, and then leaves with ErrKeySet; the read
// carries on and is kept for the next caller. So a caller that hangs up ends only its own wait, and
// a read's failure is always the issuer's.
//
// A read that failed less than FailureSpacing ago, with no good read since, answers every caller
// with its failure and no new read starts, whether or not a set was ever read: so a key set that
// is down is read at most once per FailureSpacing however many tokens arrive, whether their
// callers wait or hang up, and Key answers a stale set's callers from that set meanwhile.
func (k *Keys) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	k.mu.Lock()
	inflight := k.inflight
	if inflight == nil {
		if k.spacedLocked() {
			failure := k.failure
			k.mu.Unlock()
			return nil, failure
		}
		inflight = k.startReadLocked(ctx)
		k.mu.Unlock()
	} else {
		k.mu.Unlock()
	}
	return k.wait(ctx, inflight)
}

// spacedLocked reports whether the key set's latest answer is a failure less than FailureSpacing
// old, so no new read starts. k.mu is held.
func (k *Keys) spacedLocked() bool {
	return k.failure != nil && time.Since(k.failedAt) < k.FailureSpacing
}

// startReadLocked makes a new read the read in flight and starts it on a context detached from
// ctx, bounded by FetchTimeout alone (complete): the one way a read starts, so every read is
// detached from the caller that started it. k.mu is held and k.inflight is nil: one read runs at a
// time, so reads end in the order they start, and the outcome complete records last is the latest.
func (k *Keys) startReadLocked(ctx context.Context) *keysFetch {
	k.inflight = &keysFetch{done: make(chan struct{})}
	go k.complete(context.WithoutCancel(ctx), k.inflight)
	return k.inflight
}

// refreshInBackground starts a read of the key set detached from ctx, for a stale set that has
// answered a caller already: the caller does not wait for it, and the set it reads is kept for the
// next. It starts nothing while a read is under way, while the set is fresh again, within
// FailureSpacing of a failed read, or for a caller that has hung up.
func (k *Keys) refreshInBackground(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	k.mu.Lock()
	if k.inflight != nil || time.Since(k.fetched) < k.Lifetime || k.spacedLocked() {
		k.mu.Unlock()
		return
	}
	k.startReadLocked(ctx)
	k.mu.Unlock()
}

// wait is inflight's set, or its error, once it ends; or ErrKeySet when the caller's wait ends
// first: ctx ending, or the keys deadline ctx carries (WithKeysDeadline) passing. A wait that has
// ended before it begins (a caller that hung up before it asked, a keys deadline already past) is
// ErrKeySet even where the read has ended too by then: a select picks at random among the cases
// ready when it runs, so the select alone would answer that caller with the read's set or with
// ErrKeySet by chance.
func (k *Keys) wait(ctx context.Context, inflight *keysFetch) ([]jose.JSONWebKey, error) {
	wait := ctx
	if deadline, budgeted := keysDeadline(ctx); budgeted {
		var cancel context.CancelFunc
		wait, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	if wait.Err() == nil {
		select {
		case <-inflight.done:
			return inflight.keys, inflight.err
		case <-wait.Done():
		}
	}
	return nil, fmt.Errorf("%w: waiting on %s: %w", ErrKeySet, k.jwksURI, wait.Err())
}

// complete runs inflight's read on ctx, which no caller ends, and releases every caller waiting on
// it. A read that succeeds keeps its set and clears the recorded failure; a read that fails is
// recorded as the last failure (settleMiss, FailureSpacing). One read runs at a time, so the
// outcome recorded here is the latest, by the order the reads ran in, never their end times.
func (k *Keys) complete(ctx context.Context, inflight *keysFetch) {
	inflight.keys, inflight.err = k.read(ctx)
	k.mu.Lock()
	k.inflight = nil
	if inflight.err == nil {
		k.keys, k.fetched = inflight.keys, time.Now()
		k.failedAt, k.failure = time.Time{}, nil
	} else {
		k.failedAt, k.failure = time.Now(), inflight.err
	}
	k.mu.Unlock()
	close(inflight.done)
}

// keysDeadlineKey is the context key WithKeysDeadline stores its deadline under.
type keysDeadlineKey struct{}

// WithKeysDeadline returns ctx carrying deadline for every key-set read a verification under it
// waits for: each wait ends by deadline and fails as ErrKeySet, which a verifier reports as
// ErrIssuerUnavailable, while ctx itself goes on. That is the difference from a deadline on ctx,
// whose end a verifier reports as ErrCancelled: a server that must answer within a window uses it
// to bound the key read without turning a slow issuer into a refused token.
func WithKeysDeadline(ctx context.Context, deadline time.Time) context.Context {
	return context.WithValue(ctx, keysDeadlineKey{}, deadline)
}

// keysDeadline is the deadline WithKeysDeadline put on ctx, if any.
func keysDeadline(ctx context.Context) (time.Time, bool) {
	deadline, ok := ctx.Value(keysDeadlineKey{}).(time.Time)
	return deadline, ok
}

// read fetches and parses the issuer's key set once, under FetchTimeout and the detached context
// it is given (startReadLocked), never a caller's.
func (k *Keys) read(ctx context.Context) ([]jose.JSONWebKey, error) {
	ctx, cancel := context.WithTimeout(ctx, k.FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.jwksURI, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: requesting %s: %w", ErrKeySet, k.jwksURI, err)
	}
	resp, err := k.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrKeySet, k.jwksURI, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s answered %s", ErrKeySet, k.jwksURI, redact.StatusLine(resp.StatusCode))
	}
	answer, err := redact.ReadAnswer(resp, maxKeySetBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrKeySet, k.jwksURI, err)
	}
	var set jose.JSONWebKeySet
	if err := answer.Decode(&set); err != nil {
		return nil, fmt.Errorf("%w: parsing %s: %w", ErrKeySet, k.jwksURI, err)
	}
	return set.Keys, nil
}

// selectKey answers the one key of set a token naming kid and signed with alg may be checked
// against. A key the set marks for encryption, or for another algorithm, is not that key and is
// passed over rather than refused, so a set that carries an encryption key beside the signing one
// still resolves.
func selectKey(set []jose.JSONWebKey, kid, alg string) (crypto.PublicKey, error) {
	var match jose.JSONWebKey
	usable := 0
	for _, key := range set {
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if alg != "" && key.Algorithm != "" && key.Algorithm != alg {
			continue
		}
		if kid != "" && key.KeyID != kid {
			continue
		}
		if usable == 0 {
			match = key
		}
		usable++
	}
	switch {
	case usable == 1:
		return match.Key, nil
	case usable == 0 && kid == "":
		return nil, fmt.Errorf("%w: the token names no key id and the issuer publishes none for %s", ErrNoKey, algorithmName(alg))
	case usable == 0:
		return nil, fmt.Errorf("%w: key id %s for %s", ErrNoKey, keyIDName(kid), algorithmName(alg))
	case kid == "":
		return nil, fmt.Errorf("%w: the token names no key id and the issuer publishes %d for %s", ErrAmbiguousKey, usable, algorithmName(alg))
	default:
		return nil, fmt.Errorf("%w: the issuer publishes %d keys under key id %s", ErrAmbiguousKey, usable, keyIDName(kid))
	}
}

// algorithmName names alg in an error, an empty one being the ask that narrows nothing.
func algorithmName(alg string) string {
	if alg == "" {
		return "any algorithm"
	}
	return alg
}

// maxKeyIDInError is how much of a key id an error repeats. A key id comes from a header no one
// has verified yet, so its length is whatever the caller sent.
const maxKeyIDInError = 64

// keyIDName quotes kid for an error, shortened to maxKeyIDInError bytes.
func keyIDName(kid string) string {
	if len(kid) > maxKeyIDInError {
		return fmt.Sprintf("%q (%d bytes, shortened)", kid[:maxKeyIDInError], len(kid))
	}
	return fmt.Sprintf("%q", kid)
}
