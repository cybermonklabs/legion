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
	// no network read. It is no maximum age: while the issuer's key set cannot be read, a set past
	// its Lifetime keeps answering the keys it holds, however old it grows, since an unreadable set
	// says nothing about which keys the issuer has withdrawn, so an issuer outage also keeps a
	// withdrawn key verifying until a read succeeds.
	DefaultKeysLifetime = 15 * time.Minute
	// DefaultKeysMinRefresh is the shortest interval between two fetches made because a token
	// named a key id the cached set does not hold, after a fetch that succeeded. It is what keeps
	// a rotation visible within a minute while a token naming a key nobody ever published,
	// replayed in a loop, costs the issuer one read per minute and no more.
	DefaultKeysMinRefresh = time.Minute
	// DefaultKeysRetrySpacing is the shortest interval after a read that failed before a Key
	// holding a set starts the next one, on any path. It is shorter than MinRefresh because an
	// outage says nothing about the key a token names: a key rotated in during a short outage is
	// found seconds after the issuer is back, inside the 12 s Dispatch retries a 503 for, while an
	// issuer that fails fast is still read no more than once in this long. It is the spacing a
	// failed discovery read waits too (discoveryRetrySpacing).
	DefaultKeysRetrySpacing = discoveryRetrySpacing
	// DefaultKeysFetchTimeout bounds one key-set read. An issuer that accepts the connection and
	// never answers would otherwise hold a read open for ever.
	DefaultKeysFetchTimeout = 10 * time.Second
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
	// does not hold, after a fetch that succeeded (DefaultKeysMinRefresh).
	MinRefresh time.Duration
	// RetrySpacing is the shortest interval after a read that failed before the next one, for a
	// Keys that holds a set (DefaultKeysRetrySpacing). A Keys holding none reads again on the next
	// Key, since no token can be judged until a read succeeds, but never two reads at once: a Key
	// that arrives while a read is in flight waits on it, so an issuer that fails or hangs is asked
	// one read at a time however many callers arrive (TestKeyJoinsTheColdReadInFlightWhileTheIssuerFails).
	RetrySpacing time.Duration
	// FetchTimeout bounds one key-set read, which no caller's context ends
	// (DefaultKeysFetchTimeout).
	FetchTimeout time.Duration
	// FetchWait is the longest a Key waits on a read before it answers that the set could not be
	// read (ErrKeySet), the read going on for the next Key. Zero, as NewKeys leaves it, waits the
	// read out, up to FetchTimeout: the secrets broker and Dispatch answer a slow read with the
	// key. A caller whose own answer has a deadline shorter than FetchTimeout sets it, as the
	// Envoy listener does (cmd/listener, listenerKeySetWait).
	FetchWait time.Duration
	// HTTPClient reads the key set. A caller replaces it to route the read through a proxy or an
	// instrumented transport; it is never nil.
	HTTPClient *http.Client

	jwksURI string
	// now is the clock every decision and every read's end is taken from: time.Now, which a test
	// replaces to move past RetrySpacing or Lifetime without waiting.
	now func() time.Time

	// mu guards everything below, and every Key decides what to do under it alone (plan), so no
	// decision rests on a look another caller's read has moved on from.
	mu      sync.Mutex
	keys    []jose.JSONWebKey
	fetched time.Time
	// missed is when a key id the set did not hold last prompted a read: the MinRefresh window.
	missed time.Time
	// last is the latest key-set read, in flight until it is finished and then its outcome; nil
	// before the first.
	last *keysFetch
}

// keysFetch is one key-set read: every caller that arrives while it runs waits on it instead of
// starting its own, and once it is finished it is the Keys' last read, which the next decision
// reads.
type keysFetch struct {
	done chan struct{}
	// finished, ended, keys and err are written under Keys.mu once the read ends, before done is
	// closed: a waiter reads keys and err after done, and a decision reads them under the lock.
	finished bool
	ended    time.Time
	keys     []jose.JSONWebKey
	err      error
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
		Lifetime:     DefaultKeysLifetime,
		MinRefresh:   DefaultKeysMinRefresh,
		RetrySpacing: DefaultKeysRetrySpacing,
		FetchTimeout: DefaultKeysFetchTimeout,
		HTTPClient:   &http.Client{},
		jwksURI:      jwksURI,
		now:          time.Now,
	}
}

// Key answers the public key a token naming kid and signed with alg is to be checked against. An
// empty kid is answered only where one key of the set is usable for alg, since choosing among
// several would settle a signature's fate by position in a JSON array; an empty alg asks for no
// particular algorithm and narrows nothing.
//
// With no set yet, Key waits on a read. A set older than Lifetime that holds the key answers at
// once and is read again behind the answer; one that does not hold it is read again first. A kid
// a fresh set does not hold is a rotation the cache has not seen yet, so it prompts one more
// read, at most once per MinRefresh. A read that fails leaves the last good set serving, and is
// answered only to a Key that set cannot answer — an issuer that is down, or hangs, stops no
// verification it does not have to. A Key that set cannot answer waits on a read in flight;
// after a read that failed it gets that read's error, not ErrNoKey, until RetrySpacing has passed
// and the next read is started: the issuer has not said it publishes no such key, so no verdict
// was reached.
//
// Every error wraps ErrNoKey, ErrAmbiguousKey or ErrKeySet. The text names the key id asked for,
// which is a public identifier, shortened so an oversized one cannot flood a log, and no other
// part of the token.
func (k *Keys) Key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	read, key, err := k.plan(ctx, kid, alg)
	if read == nil {
		return key, err
	}
	fresh, err := k.await(ctx, read)
	if err != nil {
		return nil, err
	}
	return selectKey(fresh, kid, alg)
}

// plan decides, under one lock, what a Key for kid and alg does: answer from the set (a key, or
// an error no read would change), or wait on read, which it joins or starts. A stale set that
// holds the key is read again behind its answer, unless a read is in flight or RetrySpacing has
// not passed since one failed; and with no set held yet, a read that failed less than
// RetrySpacing ago answers every Key in that time, so an issuer that fails fast is not read once
// per call.
func (k *Keys) plan(ctx context.Context, kid, alg string) (*keysFetch, crypto.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if k.fetched.IsZero() {
		switch {
		case k.inFlight():
			return k.last, nil, nil
		case k.failedWithin(now):
			return nil, nil, k.last.err
		}
		return k.start(ctx), nil, nil
	}
	stale := now.Sub(k.fetched) >= k.Lifetime
	key, err := selectKey(k.keys, kid, alg)
	switch {
	case err == nil:
		if stale && !k.inFlight() && !k.failedWithin(now) {
			k.start(ctx)
		}
		return nil, key, nil
	case !stale && !errors.Is(err, ErrNoKey):
		return nil, nil, err
	}
	// The set cannot answer: it is stale, or fresh and without the key id.
	windowOpen := k.missed.IsZero() || now.Sub(k.missed) >= k.MinRefresh
	if !stale && windowOpen {
		k.missed = now
	}
	switch {
	case k.inFlight():
		return k.last, nil, nil
	case k.failedWithin(now):
		return nil, nil, k.last.err
	case k.last.err == nil && !stale && !windowOpen:
		return nil, nil, err
	}
	return k.start(ctx), nil, nil
}

// inFlight reports whether the last read is still running. The caller holds mu.
func (k *Keys) inFlight() bool {
	return k.last != nil && !k.last.finished
}

// failedWithin reports whether the last read failed less than RetrySpacing before now. The caller
// holds mu.
func (k *Keys) failedWithin(now time.Time) bool {
	return k.last != nil && k.last.finished && k.last.err != nil && now.Sub(k.last.ended) < k.RetrySpacing
}

// start begins a key-set read and makes it the last read. The read runs on ctx's values but not
// its cancellation, under FetchTimeout alone, so no caller that leaves ends it for the others,
// and its outcome is kept when it ends: the set, or the failure, beside the last good set. The
// caller holds mu.
func (k *Keys) start(ctx context.Context) *keysFetch {
	read := &keysFetch{done: make(chan struct{})}
	k.last = read
	go func() {
		keys, err := k.read(context.WithoutCancel(ctx))
		k.mu.Lock()
		read.finished, read.ended, read.keys, read.err = true, k.now(), keys, err
		if err == nil {
			k.keys, k.fetched = keys, read.ended
		}
		k.mu.Unlock()
		close(read.done)
	}()
	return read
}

// await waits for read, at most FetchWait when it is set. A caller whose own context ends first,
// or whose wait runs out, leaves with that error, and the read carries on for the others. One
// read serves every caller waiting on it, so a key set that is slow to answer costs the issuer
// one request however many tokens arrive meanwhile.
func (k *Keys) await(ctx context.Context, read *keysFetch) ([]jose.JSONWebKey, error) {
	var waited <-chan time.Time
	if k.FetchWait > 0 {
		wait := time.NewTimer(k.FetchWait)
		defer wait.Stop()
		waited = wait.C
	}
	select {
	case <-read.done:
		return read.keys, read.err
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: waiting on %s: %w", ErrKeySet, k.jwksURI, ctx.Err())
	case <-waited:
		return nil, fmt.Errorf("%w: %s has not answered in %s", ErrKeySet, k.jwksURI, k.FetchWait)
	}
}

// read fetches and parses the issuer's key set once, under FetchTimeout.
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
