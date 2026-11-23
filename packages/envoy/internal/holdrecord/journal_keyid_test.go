package holdrecord_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// keyIDRows are TestTheHoldJournal's rows of a journal that verifies under a listed key whose
// entries name log entries signed by a key the list no longer holds: the refusal names that log
// entry, the key id that signed it and the step-4 repair, not a fork or a cut log, and listing the
// key again, verify-only, stores the restore. A journal the new key wrote last over a hold that
// began under the old key is that journal when the old key leaves the list before step 4.
func keyIDRows(t *testing.T) {
	ctx := context.Background()
	newKey := func(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
		t.Helper()
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return public, private
	}
	// resignLog re-signs every log entry at or below upTo with key: entries that key signed.
	resignLog := func(t *testing.T, r rig, key ed25519.PrivateKey, upTo uint64) {
		t.Helper()
		rows, err := r.Pool.Query(ctx, `select gen, phase, hold_id, signed_at from nats_hold_log where gen <= $1`, int64(upTo))
		if err != nil {
			t.Fatal(err)
		}
		type entry struct {
			gen           int64
			phase, holdID string
			signedAt      time.Time
		}
		var entries []entry
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.gen, &e.phase, &e.holdID, &e.signedAt); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			sig, err := signedrecord.Sign(key, signedrecord.HoldRow(uint64(e.gen), e.phase, e.holdID, e.signedAt))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Pool.Exec(ctx, `update nats_hold_log set sig = $2 where gen = $1`, e.gen, sig); err != nil {
				t.Fatal(err)
			}
		}
	}
	// refusedNamingKey asserts the released restore is refused 409 below_floor naming holdID's
	// journal entry, the log entry that does not verify, kid and the step-4 repair; then, with kid's
	// key listed again verify-only, that the restore stores.
	refusedNamingKey := func(t *testing.T, r rig, holdID string, key ed25519.PublicKey) {
		t.Helper()
		loseRow(t, r)
		r.Restart()
		fresh := r.client(t)
		kid := signedrecord.KeyID(key)
		got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusConflict, "below_floor", `hold "`+holdID+`"'s journal entry`, "whose log entry does not verify",
			"signed by key id "+kid, "goes back in BROKER_NATS_HOLD_SIGNING_KEYS, verify-only", `"Rotating the hold signing key", step 4`) {
			t.Errorf("the released restore answered %s; want 409 below_floor naming %s's journal entry, the log entry that does not verify, key id %s and the step-4 repair",
				refusalText(got), holdID, kid)
		}
		r.RestartListing(key)
		fresh = r.client(t)
		if got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, ""); got != nil {
			t.Errorf("with key id %s listed again, verify-only, the released restore answered %s; want it stored", kid, refusalText(got))
		}
	}

	t.Run("ENTRIES-RETIRED: a hold's log entries signed by a key no longer listed, its journal by the listed key: refused naming that key id", func(t *testing.T) {
		// Claude's round-11 probe ENTRIES-RETIRED (P3-3, Legion M-5). Fails while the log index
		// keeps only entries that verify, as at a0a2c7622f: the refusal reads the unverified
		// entries as missing and blames another history or a cut log, naming no key id.
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-old", holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, "h-old", cmd("buckets create"), nil, nil)
		release(t, c, "h-old")
		oldEnd := row(t, c).Gen
		hold(t, c, "h-new", holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, "h-new", cmd("buckets create"), nil, nil)
		release(t, c, "h-new")
		retired, retiredKey := newKey(t)
		resignLog(t, r, retiredKey, oldEnd)
		refusedNamingKey(t, r, "h-old", retired)
	})

	t.Run("the old key removed before step 4: a journal the new key wrote last over the old key's rows is refused naming the old key id", func(t *testing.T) {
		// Legion M-5: h-live began under the old key; steps 1 and 2 listed the new key and signed
		// with it alone, and hold release ended h-live with the new key, which wrote its journal
		// last; then the old key was removed from the list before step 4 deleted what it signed.
		// Fails as ENTRIES-RETIRED does.
		r := newRig(t)
		hold(t, r.client(t), "h-live", holdrecord.WorkflowBucketsCreate, nil)
		old, oldKey := newKey(t)
		signedBy(t, r, oldKey)
		r.RestartListing(old) // step 1
		release(t, listingClient(t, r, old), "h-live")
		r.RestartListing() // the old key removed before step 4
		refusedNamingKey(t, r, "h-live", old)
	})
}
