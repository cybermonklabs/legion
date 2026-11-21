package oidc

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
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
	// verifies a signature for this long, and in exchange every verification in the window costs
	// no network read.
	DefaultKeysLifetime = 15 * time.Minute
	// DefaultKeysMinRefresh is the shortest interval between two fetches made because a token
	// named a key id the cached set does not hold. It is what keeps a rotation visible within a
	// minute while a token naming a key nobody ever published, replayed in a loop, costs the
	// issuer one read per minute and no more.
	DefaultKeysMinRefresh = time.Minute
	// DefaultKeysFetchTimeout bounds one key-set read. An issuer that accepts the connection and
	// never answers would otherwise hold a verification open for as long as the caller allows.
	DefaultKeysFetchTimeout = 10 * time.Second
	// DefaultKeysFailureSpacing is how long after a failed read no other read starts while no set
	// has ever been read: the failure answers instead, as discovery's does
	// (discoveryRetrySpacing). Without it a key set that fails at once is read as fast as tokens
	// arrive, one read per request.
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
	// does not hold (DefaultKeysMinRefresh).
	MinRefresh time.Duration
	// FetchTimeout bounds one key-set read, under the caller's own context rather than instead of
	// it (DefaultKeysFetchTimeout).
	FetchTimeout time.Duration
	// FailureSpacing is how long a failed read answers for, with no new read, while no set has
	// ever been read (DefaultKeysFailureSpacing).
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
	// failedAt and failure are the last failed read: when it ended, and its error.
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
	provider, err := gooidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovering issuer %s: %w", issuer, err)
	}
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := provider.Claims(&discovery); err != nil {
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
// A set older than Lifetime is read again first. A kid the set does not hold is a rotation the
// cache has not seen yet, so it prompts one more read, at most once per MinRefresh. A read that
// fails leaves the last good set serving and is returned only when that set does not hold the key
// asked for — an issuer that is down stops no verification it does not have to. A kid the set does
// not hold while the read that would settle it has not (one is under way, or the last one failed
// within MinRefresh) is ErrKeySet, not ErrNoKey: no verdict on that key has been reached yet.
//
// Every error wraps ErrNoKey, ErrAmbiguousKey or ErrKeySet. The text names the key id asked for,
// which is a public identifier, shortened so an oversized one cannot flood a log, and no other
// part of the token.
//
// Key takes its course from mayWaitLocked, the one decision of whether the lookup may wait on a
// key-set read, which WouldRead answers too: a lookup that may not is answered from the cache in
// the same hold of the lock, and only one that may goes on to read or join a read (lookup).
func (k *Keys) Key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	k.mu.Lock()
	waits, key, err := k.mayWaitLocked(kid, alg)
	k.mu.Unlock()
	if !waits {
		return key, err
	}
	return k.lookup(ctx, kid, alg)
}

// lookup is Key's course for a lookup mayWaitLocked says may wait on a key-set read: the set read
// first where it was never read or is past Lifetime, else a kid it does not hold read for while the
// miss window is open, else the read under way joined (settleMiss). Each step reads the cache
// again under its own hold of the lock, so a read another caller started or ended since the
// decision is joined or used, as it is found.
func (k *Keys) lookup(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	set, fetched := k.state()
	var fetchErr error
	read := false
	if k.stale(fetched) {
		read = true
		fresh, err := k.fetch(ctx)
		switch {
		case err == nil:
			set = fresh
		case fetched.IsZero():
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

// WouldRead reports whether Key, asked now for kid and alg, would read the issuer's key set, or
// wait for a read of it under way, before it answers (mayWaitLocked, the decision Key takes its
// course from). It reads nothing, waits on nothing and opens no window, so a caller asks it before
// Key to decide what to give up for the read's length (up to FetchTimeout). Its answer is the cache
// as it stands: a Key that runs after the set's Lifetime ends, after the miss window reopens, or
// after another caller starts a read, can answer otherwise.
func (k *Keys) WouldRead(kid, alg string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	waits, _, _ := k.mayWaitLocked(kid, alg)
	return waits
}

// mayWaitLocked is the one decision of whether a lookup of kid and alg, from the cache as it
// stands, may wait on a key-set read before it answers - a read of its own, or another token's
// under way - with k.mu held. Key takes its course from it and WouldRead answers it, so the two
// cannot part: where it answers false, Key answers key and err, the lookup's answer with no read
// and no wait, and where it answers true Key reads or joins (lookup).
//
// A set never read or older than Lifetime is read, or the read under way joined (fetch), except
// that while no set has ever been read and none is under way, a read that failed less than
// FailureSpacing ago answers its failure. A kid the fresh set does not hold prompts a read while
// the miss window is open (MinRefresh), and once it has closed joins the read under way; with
// none under way it is ErrKeySet while the last read failed within MinRefresh, else ErrNoKey
// (settleMiss). Every other lookup is the cached set's answer (selectKey).
func (k *Keys) mayWaitLocked(kid, alg string) (bool, crypto.PublicKey, error) {
	if k.stale(k.fetched) {
		if k.inflight != nil || !k.failureAnswersLocked() {
			return true, nil, nil
		}
		return false, nil, k.failure
	}
	key, err := selectKey(k.keys, kid, alg)
	if !errors.Is(err, ErrNoKey) {
		return false, key, err
	}
	if k.missWindowOpenLocked(time.Now()) || k.inflight != nil {
		return true, nil, nil
	}
	if k.missFailedLocked() {
		return false, nil, k.missFailure(kid, k.failure)
	}
	return false, nil, err
}

// state is the cached set and when it was fetched, the zero time standing for a set never read.
func (k *Keys) state() ([]jose.JSONWebKey, time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keys, k.fetched
}

// stale reports whether a set fetched at fetched is read again before Key answers from it: a set
// never read, which the zero time stands for, or one older than Lifetime (mayWaitLocked, lookup).
func (k *Keys) stale(fetched time.Time) bool {
	return fetched.IsZero() || time.Since(fetched) >= k.Lifetime
}

// failureAnswersLocked reports whether a read asked for now, with none under way, is answered the
// last read's failure instead of being made (fetch, mayWaitLocked): no set has ever been read, and
// the last read failed less than FailureSpacing ago. k.mu is held.
func (k *Keys) failureAnswersLocked() bool {
	return k.fetched.IsZero() && !k.failedAt.IsZero() && time.Since(k.failedAt) < k.FailureSpacing
}

// missWindowOpenLocked reports whether a key id the cached set does not hold may prompt a read at
// now: no window has opened yet, or the last one opened MinRefresh ago or more (openMissWindow,
// mayWaitLocked). k.mu is held.
func (k *Keys) missWindowOpenLocked(now time.Time) bool {
	return k.missed.IsZero() || now.Sub(k.missed) >= k.MinRefresh
}

// missFailedLocked reports whether the last read failed less than MinRefresh ago, with no good read
// since: a kid the fresh set does not hold is then answered missFailure, not ErrNoKey
// (settleMiss, mayWaitLocked). k.mu is held.
func (k *Keys) missFailedLocked() bool {
	return !k.failedAt.IsZero() && k.failedAt.After(k.fetched) && time.Since(k.failedAt) < k.MinRefresh
}

// missFailure is the answer for a kid the fresh set does not hold while the last read, which
// failed with failure, has not settled whether the issuer publishes it (missFailedLocked).
func (k *Keys) missFailure(kid string, failure error) error {
	return fmt.Errorf("%w: key id %s is not in the cached set, and the last read of %s failed: %w", ErrKeySet, keyIDName(kid), k.jwksURI, failure)
}

// openMissWindow reports whether a key id the cached set does not hold may prompt a read now, and
// opens the next MinRefresh window when it does. The window opens on the attempt rather than on
// the answer, so an issuer that is failing is asked no more often than one that is rotating.
func (k *Keys) openMissWindow() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	if !k.missWindowOpenLocked(now) {
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
	failedRecently := k.missFailedLocked()
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
		return true, nil, k.missFailure(kid, failure)
	}
	return false, nil, nil
}

// fetch reads the key set and keeps it, or joins the read already in flight. One read serves every
// caller waiting on it, so a key set that is slow to answer costs the issuer one request however
// many tokens arrive meanwhile; a caller whose own context ends first leaves with that error and
// the read carries on for the others. A read that fails keeps the last good set.
//
// A caller whose context carries a keys deadline (WithKeysDeadline) waits for the read only until
// that deadline, and leaves with ErrKeySet then, its context still live. The read it starts runs on
// a context of its own, detached from the caller's (bounded by FetchTimeout alone), so a key set
// slower than one caller's deadline is still read and kept for the next.
//
// While no set has ever been read, a read that failed less than FailureSpacing ago answers every
// caller with its failure and no new read starts, so a key set that fails at once is read at most
// once per FailureSpacing however many tokens arrive.
func (k *Keys) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	_, budgeted := keysDeadline(ctx)
	k.mu.Lock()
	inflight := k.inflight
	if inflight == nil {
		if k.failureAnswersLocked() {
			failure := k.failure
			k.mu.Unlock()
			return nil, failure
		}
		inflight = &keysFetch{done: make(chan struct{})}
		k.inflight = inflight
		k.mu.Unlock()
		if !budgeted {
			k.complete(ctx, inflight)
			return inflight.keys, inflight.err
		}
		go k.complete(context.WithoutCancel(ctx), inflight)
	} else {
		k.mu.Unlock()
	}
	return k.wait(ctx, inflight)
}

// wait is inflight's set, or its error, once it ends; or ErrKeySet when the caller's wait ends
// first: ctx ending, or the keys deadline ctx carries (WithKeysDeadline) passing.
func (k *Keys) wait(ctx context.Context, inflight *keysFetch) ([]jose.JSONWebKey, error) {
	wait := ctx
	if deadline, budgeted := keysDeadline(ctx); budgeted {
		var cancel context.CancelFunc
		wait, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	select {
	case <-inflight.done:
		return inflight.keys, inflight.err
	case <-wait.Done():
		return nil, fmt.Errorf("%w: waiting on %s: %w", ErrKeySet, k.jwksURI, wait.Err())
	}
}

// complete runs inflight's read on ctx, keeps the set it read when it read one, and releases
// every caller waiting on inflight. A read that failed on its own account, not because ctx ended,
// is kept as the last failure (settleMiss, FailureSpacing): a caller that hung up says nothing
// about the issuer.
func (k *Keys) complete(ctx context.Context, inflight *keysFetch) {
	inflight.keys, inflight.err = k.read(ctx)
	k.mu.Lock()
	k.inflight = nil
	switch {
	case inflight.err == nil:
		k.keys, k.fetched = inflight.keys, time.Now()
	case ctx.Err() == nil:
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

// read fetches and parses the issuer's key set once, under FetchTimeout and the caller's context.
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
		return nil, fmt.Errorf("%w: %s answered %s", ErrKeySet, k.jwksURI, resp.Status)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxKeySetBytes)).Decode(&set); err != nil {
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
