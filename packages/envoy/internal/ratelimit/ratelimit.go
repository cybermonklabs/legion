// Package ratelimit is the broker's token buckets: per key (a source address, the person a machine
// login names, the one bucket every service's login shares, an enrollment whose requests name
// secrets the policy does not serve and so cost a policy reread), so a flood under one key never
// spends another's, and one every caller shares. It is also the listener's count of the messages
// each agent session sends through it (cmd/listener, sender_limit.go): one Keyed whose key is the
// sending session, so one session past its limit never spends another's.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limit is a token bucket: Burst requests at once, refilled at one every Every.
type Limit struct {
	Every time.Duration
	Burst int
}

// RetryAfterSeconds is the Retry-After a refusal names for wait: its whole seconds, rounded up,
// and at least 1, since 0 would invite an immediate retry the bucket would refuse again. The
// broker's own 429s (internal/broker/api), Dispatch's refusal of an introspection it would not
// make (internal/dispatch/api) and the listener's refusal of a broker token it would not check
// pass their bucket's Every, how long one request takes to come back. The listener's refusal of
// a sender past its limit (cmd/listener) passes the wait until the sender's bucket holds one
// request (WaitAt), which a bucket deeper than one request reaches sooner. One rounding for every
// refusal means a caller reads each wait the same way, whichever refused it.
func RetryAfterSeconds(wait time.Duration) int {
	return max(int((wait+time.Second-1)/time.Second), 1)
}

// maxBuckets bounds a Keyed's memory: past it, full buckets (indistinguishable from fresh ones)
// are dropped.
const maxBuckets = 10000

// Keyed is one Limit applied to each key separately.
type Keyed struct {
	limit   Limit
	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

// NewKeyed gives every key its own bucket of limit.
func NewKeyed(limit Limit) *Keyed {
	return &Keyed{limit: limit, buckets: map[string]*tokenBucket{}}
}

// Every is how often each of k's buckets gains a request back: how long a refused caller waits.
// It is read under k's lock, which SetRatesAt writes the rate under.
func (k *Keyed) Every() time.Duration {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.limit.Every
}

// Allow takes one request from key's bucket now, reporting false when it has none left.
func (k *Keyed) Allow(key string) bool {
	return k.AllowAt(key, time.Now())
}

// AllowAt takes one request from key's bucket at now, reporting false when it has none left.
func (k *Keyed) AllowAt(key string, now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.bucket(key, now).allowAt(now)
}

// TakeAt takes one request from key's bucket at now, as AllowAt does, and answers ok with
// giveBack, which returns that request to the bucket: for a caller that learns only afterwards
// that its request decided nothing the bucket exists to slow, such as one the server could not
// complete. A refused take answers false and spends nothing.
//
// giveBack(at) returns the request at at, the time of the give-back: the bucket refills to at,
// gains the one request and is capped at its burst. Where at is before the bucket's last update,
// because the caller read it before another update of the bucket won k's lock (a later take of
// key, a change of rate by SetRatesAt, or another give-back), the request is added at that
// update's time instead, so the give-back refills no time twice. So the bucket holds what it would
// hold had the request never been taken, with every other take of key allowed meanwhile still
// taken, however long after this one it came. Only where another take came while the bucket would
// have been full without this one can it hold more than that, by up to one request.
//
// That bound is exact only where each take of key is stamped in the order it takes k's lock. A
// take stamped before the bucket's last update moves the bucket's clock back to its own stamp, as
// rate.Limiter's takes do (tokenBucket.allowAt), and so refills the time between the two again.
// This take and its give-back each move the clock forward, so after such a take the bucket can
// hold more than the bound, by up to about twice the requests the rate refills in the time that
// take waited for k's lock. giveBack runs under k's lock, and only its first call gives anything
// back.
func (k *Keyed) TakeAt(key string, now time.Time) (giveBack func(at time.Time), ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	bucket := k.bucket(key, now)
	if bucket.tokensAt(now) < 1 {
		return nil, false
	}
	bucket.allowAt(now)
	given := false
	return func(at time.Time) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if given {
			return
		}
		given = true
		// The request goes back to the bucket it came from, even one k has dropped since: k drops
		// only a full bucket, which has refilled the request already, so the key's new bucket is
		// owed nothing.
		bucket.giveBackAt(at)
	}, true
}

// WaitAt is how long key's bucket at now waits until it holds one request: 0 for a key with no
// bucket, since a new bucket is full, and 0 for a bucket that holds one request or more. It reads
// key's bucket under k's lock and makes none, so asking after a key spends no slot of maxBuckets.
// The wait is truncated to whole nanoseconds, so two thirds of a request missing at one request
// every 15 s is 10 s exactly, and not 10 s and 1 ns, which RetryAfterSeconds would round up to 11.
func (k *Keyed) WaitAt(key string, now time.Time) time.Duration {
	k.mu.Lock()
	defer k.mu.Unlock()
	bucket, ok := k.buckets[key]
	if !ok {
		return 0
	}
	tokens := bucket.tokensAt(now)
	if tokens >= 1 {
		return 0
	}
	return time.Duration((1 - tokens) * float64(k.limit.Every))
}

// AllowBothAt takes one request from key's bucket and one from shared at now when each has one
// left, reporting true. Otherwise it takes from neither and answers false with the limit that
// refused, key's bucket's first: a request one bucket refuses spends nothing of the other, so a
// key flooding past its own limit leaves shared to everyone else, and a request shared refuses
// leaves its key's bucket as it was. Key's bucket is taken from only under k's lock, so a request
// shared allows always finds it as it was checked.
func (k *Keyed) AllowBothAt(key string, shared *Bucket, now time.Time) (refused Limit, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	bucket := k.bucket(key, now)
	if bucket.tokensAt(now) < 1 {
		return k.limit, false
	}
	if !shared.bucket.allowAt(now) {
		return shared.limit, false
	}
	bucket.allowAt(now)
	return Limit{}, true
}

// SetRatesAt sets, at now, the rate k's buckets refill at to address's and shared's to
// sharedLimit's. Every bucket k holds keeps the tokens it has (tokenBucket.setRateAt), so a
// change of rate neither refills a bucket nor empties one, and the depth of a bucket that
// already exists stays as it was: a burst is one caller's own momentary allowance, which the
// rate it refills at does not change.
//
// Both rates move under k's one lock, because AllowBothAt is the only reader of either limit and
// holds that lock: a reread is answered by the two rates of one sizing rather than by one rate
// of each, and a refusal can never carry a rate no bucket has.
func (k *Keyed) SetRatesAt(now time.Time, address Limit, shared *Bucket, sharedLimit Limit) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.limit = address
	rate := every(address.Every)
	for _, bucket := range k.buckets {
		bucket.setRateAt(now, rate)
	}
	shared.limit = sharedLimit
	shared.bucket.setRateAt(now, every(sharedLimit.Every))
}

// bucket is key's bucket, made full when key has none. The caller holds k.mu.
func (k *Keyed) bucket(key string, now time.Time) *tokenBucket {
	bucket, ok := k.buckets[key]
	if !ok {
		if len(k.buckets) >= maxBuckets {
			for other, b := range k.buckets {
				if b.tokensAt(now) >= float64(k.limit.Burst) {
					delete(k.buckets, other)
				}
			}
		}
		bucket = newTokenBucket(k.limit)
		k.buckets[key] = bucket
	}
	return bucket
}

// Bucket is one Limit every caller shares: a bound on the work all of them together can cause.
//
// It holds no lock of its own: its limit is read in Keyed.AllowBothAt and written in
// Keyed.SetRatesAt, both under the Keyed's lock, so the two buckets a reread is held to always
// carry one sizing and a refusal always names a rate its bucket really refills at. A Bucket
// passed to two Keyeds would lose that, and nothing does it.
type Bucket struct {
	limit  Limit
	bucket *tokenBucket
}

// NewBucket is one bucket of limit, full.
func NewBucket(limit Limit) *Bucket {
	return &Bucket{limit: limit, bucket: newTokenBucket(limit)}
}

// noLimit is the rate of a bucket whose Limit's Every is 0 or less, which allows every take: the
// rate package's rate.Inf.
const noLimit = math.MaxFloat64

// every is interval as a rate in requests a second, as rate.Every converts it, and noLimit for
// an interval of 0 or less.
func every(interval time.Duration) float64 {
	if interval <= 0 {
		return noLimit
	}
	return 1 / interval.Seconds()
}

// tokenBucket is one bucket: the requests it held at last, refilled at rate requests a second up
// to burst. For each call this package made of the rate.Limiter of golang.org/x/time v0.16.0 it
// held before, a tokenBucket does that Limiter's arithmetic, so it allows and refuses as that
// one did. It adds giveBackAt, which that Limiter has no exact form of: its
// Reservation.CancelAt restores one request less the refill between the reservation and the
// latest take since, so nothing once another take came a whole Every later.
//
// It holds no lock: its Keyed's lock guards every read and write of it.
type tokenBucket struct {
	rate   float64
	burst  int
	tokens float64
	last   time.Time
}

// newTokenBucket is one bucket of limit, full.
func newTokenBucket(limit Limit) *tokenBucket {
	return &tokenBucket{rate: every(limit.Every), burst: limit.Burst, tokens: float64(limit.Burst)}
}

// tokensAt is the requests b holds at now: what it held at last, refilled for the time since and
// capped at its burst. A now before last counts as no time passed.
func (b *tokenBucket) tokensAt(now time.Time) float64 {
	last := b.last
	if now.Before(last) {
		last = now
	}
	return min(b.tokens+now.Sub(last).Seconds()*b.rate, float64(b.burst))
}

// allowAt takes one request from b at now when it holds one, reporting whether it did, as
// rate.Limiter.AllowN(now, 1) decides: it refuses only when the part of a request b lacks would
// take a whole nanosecond or more to refill, so a float rounding that leaves b short by less
// refuses nothing. A refusal changes nothing, and with no limit every take is allowed and
// changes nothing.
func (b *tokenBucket) allowAt(now time.Time) bool {
	if b.rate == noLimit {
		return true
	}
	tokens := b.tokensAt(now) - 1
	if b.burst < 1 || tokens < 0 && -tokens/b.rate*float64(time.Second) >= 1 {
		return false
	}
	b.tokens, b.last = tokens, now
	return true
}

// setRateAt moves b to rate at now, as rate.Limiter.SetLimitAt does: b keeps the requests it
// holds at now, refilled at its old rate until now and at rate from then on.
func (b *tokenBucket) setRateAt(now time.Time, rate float64) {
	b.tokens, b.last, b.rate = b.tokensAt(now), now, rate
}

// giveBackAt returns one request to b at now: b refills to now, gains the request and is capped
// at its burst (Keyed.TakeAt says what that leaves it holding). A give-back stamped before b's
// last update adds its request at that update's time: b already holds the refill up to last, so
// last stays where it is, since moving it back to now would refill the time from now to last a
// second time.
func (b *tokenBucket) giveBackAt(now time.Time) {
	b.tokens = min(b.tokensAt(now)+1, float64(b.burst))
	if now.After(b.last) {
		b.last = now
	}
}
