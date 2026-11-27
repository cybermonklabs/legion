package holdrecord_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// keyRotationRows are TestTheHoldJournal's rows of the operator's hold signing key rotated (N-F):
// BROKER_NATS_HOLD_SIGNING_KEYS is the one list every record verifies against, and a retired key
// stays in it, verify-only, while a record it signed is stored. Every signature names its key by
// id (signedrecord.KeyID), so a record whose key has left the list is refused naming that id, and
// a write signed by a key not in the list is refused naming it too, never the key's bytes.
func keyRotationRows(t *testing.T) {
	ctx := context.Background()
	exec := func(t *testing.T, r rig, sql string, args ...any) {
		t.Helper()
		if _, err := r.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	newKey := func(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
		t.Helper()
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return public, private
	}
	// signedBefore re-signs holdID's done record, and every log entry at or below upTo, with key:
	// the records a key retired since signed, as the routes read them once the list is rotated.
	signedBefore := func(t *testing.T, r rig, key ed25519.PrivateKey, holdID string, upTo uint64) {
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
			exec(t, r, `update nats_hold_log set sig = $2 where gen = $1`, e.gen, sig)
		}
		var value string
		var gen int64
		if err := r.Pool.QueryRow(ctx, `select value::text, gen from nats_hold_records where hold_id = $1 and kind = 'done'`, holdID).Scan(&value, &gen); err != nil {
			t.Fatal(err)
		}
		sig, err := signedrecord.Sign(key, signedrecord.Record(holdID, "done", "hold", uint64(gen), []byte(value)))
		if err != nil {
			t.Fatal(err)
		}
		exec(t, r, `update nats_hold_records set sig = $2 where hold_id = $1 and kind = 'done'`, holdID, sig)
	}
	restoreGen := func(t *testing.T, c *holdrecord.Client) uint64 {
		t.Helper()
		var mismatch *holdrecord.RestoreGenError
		if err := c.Bootstrap(ctx); !errors.As(err, &mismatch) {
			t.Fatalf("Bootstrap with the row lost answered %v, want a RestoreGenError", err)
		}
		return mismatch.RestoreGen
	}

	t.Run("a journal signed by a key no longer listed is refused naming its hold and key id; listed verify-only, the released restore stores", func(t *testing.T) {
		// Claude's round-8 probe A: h-old ran under a key since retired, h-new under the rig's,
		// then the row is lost. Fails while the refusal names neither the hold nor the key id
		// (c23c8c1b87 names no key id, and its client keeps no refusal text).
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
		signedBefore(t, r, retiredKey, "h-old", oldEnd)
		exec(t, r, `delete from nats_hold`)
		r.Restart()
		fresh := r.client(t)
		kid := signedrecord.KeyID(retired)
		got := restoreRefusal(t, fresh, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusConflict, "below_floor", `hold "h-old"'s done record`, kid) {
			t.Errorf("the released restore with h-old's key retired answered %s; want 409 below_floor naming h-old's done record and key %s", refusalText(got), kid)
		}
		if got != nil && strings.Contains(got.Message, base64.RawURLEncoding.EncodeToString(retired)) {
			t.Errorf("the refusal %q holds the retired key itself", got.Message)
		}
		r.RestartListing(retired)
		fresh = r.client(t)
		gen := restoreGen(t, fresh)
		if restored, err := fresh.RestoreRow(ctx, gen, natsgrant.OperatorPhaseReleased, "", "the lost row, the retired key listed verify-only"); err != nil || restored.Gen != gen {
			t.Errorf("with the retired key listed verify-only, the released restore at gen %d answered %+v, %v; want it stored", gen, restored, err)
		}
	})

	t.Run("a write signed by a key not listed is refused bad_signature naming its key id", func(t *testing.T) {
		// The operator's client signs with a key the broker does not list (a rotation done in the
		// wrong order). Fails while the refusal names no key id, as at c23c8c1b87.
		r := newRig(t)
		unlisted, unlistedKey := newKey(t)
		cfg := r.Config()
		cfg.SigningKey = unlistedKey
		c, err := holdrecord.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var got *holdrecord.RouteError
		if err := c.Bootstrap(ctx); !errors.As(err, &got) || !refusedWith(got, http.StatusBadRequest, "bad_signature", signedrecord.KeyID(unlisted)) {
			t.Errorf("Bootstrap signed by a key not listed answered %v (%s); want 400 bad_signature naming key %s", err, refusalText(got), signedrecord.KeyID(unlisted))
		}
	})
}
