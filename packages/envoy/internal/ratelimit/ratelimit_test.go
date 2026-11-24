package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// TestKeyedAllowsABurstThenOnePerEveryForEachKey pins the bucket: Burst requests at once, then one
// per Every, and one key's requests never spend another's.
func TestKeyedAllowsABurstThenOnePerEveryForEachKey(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Minute, Burst: 2})
	if !k.AllowAt("a", t0) || !k.AllowAt("a", t0) {
		t.Fatal("a burst of 2 was refused")
	}
	if k.AllowAt("a", t0) {
		t.Fatal("a third request inside the burst was allowed")
	}
	if !k.AllowAt("b", t0) {
		t.Fatal("a's requests spent b's bucket")
	}
	if k.AllowAt("a", t0.Add(time.Minute-time.Second)) {
		t.Fatal("a's bucket refilled before Every passed")
	}
	if !k.AllowAt("a", t0.Add(time.Minute)) {
		t.Fatal("a's bucket did not refill once Every passed")
	}
}

// TestKeyedDropsOnlyFullBucketsAtItsBound pins the memory bound: once maxBuckets keys hold
// buckets, a new key drops the full ones, and a key that spent its bucket keeps it, so spraying
// fresh keys never hands a flooding key a fresh burst.
func TestKeyedDropsOnlyFullBucketsAtItsBound(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Minute, Burst: 1})
	for i := range maxBuckets - 1 {
		k.AllowAt(fmt.Sprintf("idle-%d", i), t0)
	}
	t1 := t0.Add(time.Minute) // every idle bucket has refilled to its burst
	if !k.AllowAt("flood", t1) {
		t.Fatal("flood's first request was refused")
	}
	if !k.AllowAt("newcomer", t1) {
		t.Fatal("a new key was refused at the bound")
	}
	if k.AllowAt("flood", t1) {
		t.Fatal("flood's spent bucket was dropped at the bound, giving it a fresh burst")
	}
	if n := len(k.buckets); n != 2 {
		t.Fatalf("kept %d buckets past the bound, want the 2 that are not full", n)
	}
}

// TestAKeyFloodingItsOwnLimitSpendsNothingShared pins AllowBothAt's first property: a request its
// key's bucket refuses takes nothing from the shared bucket, so one key flooding past its own
// limit leaves the shared bucket to every other key.
func TestAKeyFloodingItsOwnLimitSpendsNothingShared(t *testing.T) {
	perKey := Limit{Every: time.Hour, Burst: 1}
	k, shared := NewKeyed(perKey), NewBucket(Limit{Every: time.Hour, Burst: 2})
	if _, ok := k.AllowBothAt("flood", shared, t0); !ok {
		t.Fatal("flood's first request was refused")
	}
	for i := range 5 {
		if refused, ok := k.AllowBothAt("flood", shared, t0); ok || refused != perKey {
			t.Fatalf("flood's request %d = %+v, %v; want refused by its own limit", i+2, refused, ok)
		}
	}
	if _, ok := k.AllowBothAt("other", shared, t0); !ok {
		t.Fatal("another key's first request was refused: the flood spent the shared bucket")
	}
}

// TestASharedRefusalSpendsNothingOfTheKeys pins AllowBothAt's second property: a request the
// shared bucket refuses takes nothing from its key's bucket, so a caller retrying through a flood
// of others is not refused by its own limit once the shared bucket refills.
func TestASharedRefusalSpendsNothingOfTheKeys(t *testing.T) {
	all := Limit{Every: time.Second, Burst: 1}
	k, shared := NewKeyed(Limit{Every: time.Hour, Burst: 1}), NewBucket(all)
	if _, ok := k.AllowBothAt("other", shared, t0); !ok {
		t.Fatal("other's first request was refused")
	}
	for i := range 3 {
		if refused, ok := k.AllowBothAt("caller", shared, t0); ok || refused != all {
			t.Fatalf("caller's request %d = %+v, %v; want refused by the shared limit", i+1, refused, ok)
		}
	}
	if _, ok := k.AllowBothAt("caller", shared, t0.Add(time.Second)); !ok {
		t.Fatal("caller was refused once the shared bucket refilled: the shared refusals spent its own bucket")
	}
}

// TestAGivenBackRequestCanBeTakenAgain pins TakeAt's give-back: a request given back is in its
// bucket again, even when the give-back comes after another caller's take, and the other caller's
// request stays taken.
func TestAGivenBackRequestCanBeTakenAgain(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Minute, Burst: 2})
	giveBack, ok := k.TakeAt("a", t0)
	if !ok {
		t.Fatal("a's first take was refused")
	}
	if _, ok := k.TakeAt("a", t0.Add(time.Second)); !ok {
		t.Fatal("a's second take inside the burst was refused")
	}
	giveBack(t0.Add(time.Second))
	if _, ok := k.TakeAt("a", t0.Add(2*time.Second)); !ok {
		t.Fatal("the request given back was not in a's bucket again")
	}
	if k.AllowAt("a", t0.Add(3*time.Second)) {
		t.Fatal("a fourth request was allowed: the give-back returned more than the one request it took")
	}
}

// TestAGivenBackRequestComesBackAfterALaterTake pins that a request given back comes back whole
// however long after it the key's next take came. The rate package's Reservation.CancelAt, which
// TakeAt gave back with before, returned nothing once that take came a whole Every later, so a
// message the listener gives back cost its sender one all the same. In a bucket the key all but
// emptied, the give-back returns the whole request; in one refilled close to its burst, it fills
// the bucket to its burst and no further. A give-back stamped before that later take, as the
// listener's and the broker's are when such a take wins the lock after they read the clock and
// before their give-back takes it, returns the whole request and refills no time twice. A second
// call of the give-back returns nothing more.
func TestAGivenBackRequestComesBackAfterALaterTake(t *testing.T) {
	limit := Limit{Every: time.Minute / 60, Burst: 60}
	later := 3 * limit.Every // the key's next take, three requests' refill after the one given back
	for _, row := range []struct {
		name string
		// spent is how many requests the key takes at t0 before the take it gives back.
		spent int
		// givenBack is how long after t0 the give-back is stamped, and want how many requests the
		// bucket holds at the later of that stamp and the key's later take.
		givenBack time.Duration
		want      int
	}{
		// The two takes leave 2 requests at the later one, and the give-back makes them 3.
		{"an emptied bucket", limit.Burst - 1, later, 3},
		// The two takes leave 59 at the later one, 59.5 half an Every on, and the give-back fills
		// the bucket to its 60 and not to 60.5.
		{"a bucket refilled close to its burst", 0, later + limit.Every/2, limit.Burst},
		// The two takes leave 2 requests at the later one, and the give-back stamped an Every
		// before it makes them 3 there, and not the 4 a refill of that Every a second time would.
		{"a give-back stamped before the later take", limit.Burst - 1, later - limit.Every, 3},
	} {
		t.Run(row.name, func(t *testing.T) {
			k := NewKeyed(limit)
			for range row.spent {
				k.AllowAt("a", t0)
			}
			giveBack, ok := k.TakeAt("a", t0)
			if !ok {
				t.Fatal("the take to give back was refused")
			}
			if _, ok := k.TakeAt("a", t0.Add(later)); !ok {
				t.Fatal("the key's later take was refused")
			}
			giveBack(t0.Add(row.givenBack))
			giveBack(t0.Add(row.givenBack))
			// The bucket is counted at its last event: the give-back, or the later take where the
			// give-back is stamped before it.
			at := t0.Add(max(row.givenBack, later))
			taken := 0
			for taken <= limit.Burst && k.AllowAt("a", at) {
				taken++
			}
			if taken != row.want {
				t.Fatalf("after the give-back a's bucket allowed %d requests, want %d", taken, row.want)
			}
			// Those takes left the bucket empty to the nanosecond, so it held whole requests and no
			// part of one more.
			if wait := k.WaitAt("a", at); wait != limit.Every {
				t.Fatalf("after its %d requests a's bucket waits %s, want %s", taken, wait, limit.Every)
			}
		})
	}
}

// TestARefusedTakeSpendsNothing pins TakeAt's refusal: a take its bucket refuses answers no
// give-back and spends nothing, so the bucket refills on its own schedule as AllowAt's does.
func TestARefusedTakeSpendsNothing(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Minute, Burst: 1})
	if _, ok := k.TakeAt("a", t0); !ok {
		t.Fatal("a's first take was refused")
	}
	for i := range 3 {
		if giveBack, ok := k.TakeAt("a", t0.Add(time.Duration(i)*time.Second)); ok || giveBack != nil {
			t.Fatalf("a's take %d inside its empty bucket = %v, want refused with no give-back", i+2, ok)
		}
	}
	if _, ok := k.TakeAt("a", t0.Add(time.Minute)); !ok {
		t.Fatal("a's bucket did not refill once Every passed: the refused takes spent it")
	}
}

// TestKeyedWaitAt pins the wait the listener's refusal of a sender past its limit names: none for
// a key with no bucket, which WaitAt does not make, nor for a full one; else the time until the
// key's bucket holds one request again, exact to the nanosecond, which RetryAfterSeconds turns
// into whole seconds of at least 1. A wait one nanosecond too long would round up to 11 s where
// the bucket holds its request again after 10.
func TestKeyedWaitAt(t *testing.T) {
	k := NewKeyed(Limit{Every: 15 * time.Second, Burst: 1})
	if wait := k.WaitAt("unseen", t0); wait != 0 {
		t.Fatalf("a key with no bucket waits %s, want 0: a new bucket is full", wait)
	}
	if _, made := k.buckets["unseen"]; made {
		t.Fatal("WaitAt made a bucket for the key it was asked about")
	}

	giveBack, ok := k.TakeAt("full", t0)
	if !ok {
		t.Fatal("the take that makes a full bucket was refused")
	}
	giveBack(t0)
	if wait := k.WaitAt("full", t0); wait != 0 {
		t.Fatalf("a full bucket waits %s, want 0", wait)
	}

	if _, ok := k.TakeAt("a", t0); !ok {
		t.Fatal("a's first take was refused")
	}
	if wait := k.WaitAt("a", t0); wait != 15*time.Second {
		t.Fatalf("at the take a waits %s, want 15s", wait)
	}
	wait := k.WaitAt("a", t0.Add(5*time.Second))
	if wait != 10*time.Second {
		t.Fatalf("5 s after the take a waits %s, want 10s", wait)
	}
	if seconds := RetryAfterSeconds(wait); seconds != 10 {
		t.Fatalf("RetryAfterSeconds(%s) = %d, want 10", wait, seconds)
	}
	if wait := k.WaitAt("a", t0.Add(15*time.Second)); wait != 0 {
		t.Fatalf("15 s after the take a waits %s, want 0: its bucket holds its request again", wait)
	}

	if seconds := RetryAfterSeconds(time.Nanosecond); seconds != 1 {
		t.Fatalf("RetryAfterSeconds(1ns) = %d, want 1", seconds)
	}
}

// TestSetRatesAtKeepsEachKeysTokensAndSlowsItsRefill pins what a rate change does to a bucket a
// caller already spent from: the tokens it holds stay, and only the rate it gains them back at
// moves. The broker rewrites these rates when its replica count changes
// (api.RereadRate.SetReplicas), and a rewrite that refilled every bucket would hand every
// caller a fresh burst at each change of the fleet's size.
func TestSetRatesAtKeepsEachKeysTokensAndSlowsItsRefill(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Second, Burst: 2})
	// A depth no row here can reach, so the only refusal below is the per-key one.
	free := Limit{Every: time.Millisecond, Burst: 1000}
	shared := NewBucket(free)
	if _, ok := k.AllowBothAt("a", shared, t0); !ok {
		t.Fatal("a's first request was refused")
	}
	slower := Limit{Every: 10 * time.Second, Burst: 2}
	k.SetRatesAt(t0, slower, shared, free)
	if got := k.Every(); got != slower.Every {
		t.Fatalf("Every() = %s after the change, want %s", got, slower.Every)
	}
	if _, ok := k.AllowBothAt("a", shared, t0); !ok {
		t.Fatal("the request a's bucket still held was refused: the rate change emptied it")
	}
	if refused, ok := k.AllowBothAt("a", shared, t0.Add(time.Second)); ok || refused != slower {
		t.Fatalf("a's request one second on = %+v, %v; want refused by the new rate, which gives one back every %s", refused, ok, slower.Every)
	}
	if _, ok := k.AllowBothAt("a", shared, t0.Add(slower.Every)); !ok {
		t.Fatal("a's bucket did not refill once the new Every passed")
	}
	if _, ok := k.AllowBothAt("b", shared, t0.Add(slower.Every)); !ok {
		t.Fatal("a key first seen after the change was refused its burst")
	}
}

// TestSetRatesAtMovesTheSharedBucketAndItsRefusal pins the same for the bucket every key shares,
// and that a refusal names the rate that bucket now refills at: Retry-After is read off the
// limit AllowBothAt answers (api.refuseReread), so a stale limit there would tell a caller to
// come back while the bucket is still empty.
func TestSetRatesAtMovesTheSharedBucketAndItsRefusal(t *testing.T) {
	// A per-key depth no row here can reach, so the only refusal below is the shared one.
	free := Limit{Every: time.Millisecond, Burst: 1000}
	k, shared := NewKeyed(free), NewBucket(Limit{Every: time.Second, Burst: 1})
	if _, ok := k.AllowBothAt("a", shared, t0); !ok {
		t.Fatal("the shared bucket refused the first request of its burst")
	}
	slower := Limit{Every: 5 * time.Second, Burst: 1}
	k.SetRatesAt(t0, free, shared, slower)
	if refused, ok := k.AllowBothAt("a", shared, t0.Add(time.Second)); ok || refused != slower {
		t.Fatalf("a request one second on = %+v, %v; want refused by the new shared rate of one every %s", refused, ok, slower.Every)
	}
	if _, ok := k.AllowBothAt("a", shared, t0.Add(slower.Every)); !ok {
		t.Fatal("the shared bucket did not refill once its new Every passed")
	}
}
