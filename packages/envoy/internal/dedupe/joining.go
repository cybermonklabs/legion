package dedupe

import (
	"sync/atomic"
	"time"
)

// Joining is the delivery ledger of a listener that started without the listener state bucket (its
// NATS user lacked the grant) and joins its machine's delivery lease once a grant lets it open the
// bucket (cmd/listener's joinDeliveryLease): it is a Memory until Join hands it the machine's KV
// ledger, and the KV ledger from then on, but for the pairs its Memory still holds a record of.
//
// The KV ledger holds nothing of what this process delivered before the join, so a repeat of a
// message it forwarded then - the message published again under its dedupe key - would find no
// pair there. So once it has joined, an Attempt first asks the Memory, without writing to it, and a
// pair the Memory still holds a record of answers from that record (Delivered or InFlight) for as
// long as that record stops a delivery: the same window it stopped one in before the join
// (DeliveredAge, AttemptedAge, the takeover age). Only a pair the Memory holds nothing of is the
// KV ledger's. The Memory's records are not written into the KV ledger at the join: another pod of
// the machine that meets such a repeat forwards it again, and the stream stores that forward once,
// since every forward to a session carries one Nats-Msg-Id however often it is sent
// (bus.ForwardMsgID) and the stream keeps that id for its duplicate window, 72 h, so the session
// is handed the message once. A copy would also need a second bucket write for every attempt still
// in flight at the join, which settles after it.
//
// An attempt is settled (Record, Clear, Keep) by the ledger that made it, on whichever side of the
// join the settle comes: an attempt the Memory made carries no key, and one the KV ledger made
// always does.
type Joining struct {
	memory *Memory
	kv     atomic.Pointer[KV]
}

// NewJoining makes the Joining ledger of a listener that starts without the listener state bucket,
// its Memory taking an unsettled attempt over after takeoverAfter, as NewMemory's.
func NewJoining(takeoverAfter time.Duration) *Joining {
	return &Joining{memory: NewMemory(takeoverAfter)}
}

// Join hands the ledger kv, the machine's KV ledger under the lease the listener joined: every
// Attempt from here on is kv's, but for a pair the Memory still holds a record of. It is called
// once.
func (j *Joining) Join(kv *KV) {
	j.kv.Store(kv)
}

// Attempt is the Memory's before the join. After it, a pair the Memory still holds a record of is
// answered from it (Delivered or InFlight), and any other pair is the KV ledger's.
func (j *Joining) Attempt(dedupeKey, sessionID string) (Attempt, error) {
	kv := j.kv.Load()
	if kv == nil {
		return j.memory.Attempt(dedupeKey, sessionID)
	}
	if attempt, held := j.memory.held(Key{DedupeKey: dedupeKey, SessionID: sessionID}); held {
		return attempt, nil
	}
	return kv.Attempt(dedupeKey, sessionID)
}

// Record marks the pair of a Proceed attempt delivered, in the ledger that made the attempt.
func (j *Joining) Record(attempt Attempt) error {
	return j.of(attempt).Record(attempt)
}

// Clear gives up a Proceed attempt, in the ledger that made it.
func (j *Joining) Clear(attempt Attempt) error {
	return j.of(attempt).Clear(attempt)
}

// Keep settles a Proceed attempt whose record is to stand, in the ledger that made it.
func (j *Joining) Keep(attempt Attempt) {
	j.of(attempt).Keep(attempt)
}

// of is the ledger that made attempt: the KV ledger for an attempt that carries a key, which only
// that ledger gives one, and the Memory for any other, the zero Attempt included, which settles
// nothing there.
func (j *Joining) of(attempt Attempt) Ledger {
	if attempt.key != "" {
		if kv := j.kv.Load(); kv != nil {
			return kv
		}
	}
	return j.memory
}
