package holdrecord_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// rotationRows are TestTheHoldJournal's rows of infra/README.md's key rotation followed as
// written: a routine rotation on a history signed partly by the old key and partly by the new one
// (Claude's round-10 probe ROT4), a leaked key's repair while a hold is live, which ends that hold
// with the new key before step 4, and a leaked key that signed the row itself.
func rotationRows(t *testing.T) {
	ctx := context.Background()
	// afterStep4 asserts what step 4 leaves: the view reads released, known, Cutovers reads, and a
	// later lost row restores released after a restart.
	afterStep4 := func(t *testing.T, r rig) {
		t.Helper()
		if p, known := r.HoldView(false).State(ctx); !known || p != natsgrant.OperatorPhaseReleased {
			t.Errorf("after step 4 the auth service's view reads %q, known %v; want released, known", p, known)
		}
		if _, err := r.client(t).Cutovers(ctx); err != nil {
			t.Errorf("after step 4 Cutovers answered %v; want every family's row read", err)
		}
		loseRow(t, r)
		r.Restart()
		fresh := r.client(t)
		if got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, ""); got != nil {
			t.Errorf("after step 4 a later lost row's released restore answered %s; want it stored", refusalText(got))
		}
	}
	keepAll := func(string, string) bool { return true }

	t.Run("ROT4: a routine rotation on a mixed history, step 4 as written: the row restored only when the read listed it", func(t *testing.T) {
		// h-old ran under the old key; steps 1 and 2 moved signing to the new key, and h-new ran
		// under it, so the hold row is the new key's and step 3's read lists only h-old's rows.
		// Fails while step 4 restores the row whatever the read listed, as at 886868daac's README:
		// Bootstrap answers 409 row_exists.
		readmeSays(t, restoreWhenListed)
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-old", holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, "h-old", cmd("buckets create"), nil, nil)
		release(t, c, "h-old")
		old, oldKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signedBy(t, r, oldKey)
		r.RestartListing(old) // step 1: both keys listed; the rig signs with the new key (step 2)
		c = listingClient(t, r, old)
		hold(t, c, "h-new", holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, "h-new", cmd("buckets create"), nil, nil)
		release(t, c, "h-new")
		listed := readKeyRows(t, r, signedrecord.KeyID(old))
		tables := runKeyDeletes(t, r, listed, keepAll)
		if tables["nats_hold"] || !tables["nats_hold_log"] {
			t.Fatalf("step 3's read listed %v; want h-old's log entries and records, and not the hold row the new key signed", listed)
		}
		r.RestartListing() // the old key removed, each replica restarted
		if tables["nats_hold"] {
			if err := r.client(t).Bootstrap(ctx); err != nil {
				t.Fatal(err)
			}
		}
		afterStep4(t, r)
	})

	t.Run("a leaked key while a hold is live: steps 1 and 2, the hold ended with the new key, then step 4", func(t *testing.T) {
		// TN deep's hardening 2, Claude's P3-2: every row was signed by a key that then leaked,
		// while h-live was held. Steps 1 and 2 list the new key and sign with it alone; hold
		// release ends h-live with the new key; step 3 lists h-live's prepare and held entries,
		// which the leaked key signed, and not its journal, which the new key wrote last; step 4
		// deletes each listed log entry's hold's journal before the entry, since that journal
		// names those rows. Fails while step 4 deletes the log entries alone: h-live's journal
		// names rows the log no longer holds, and every later released restore is refused 409.
		readmeSays(t, "end the live hold with the new key")
		r := newRig(t)
		hold(t, r.client(t), "h-live", holdrecord.WorkflowBucketsCreate, nil)
		leaked, leakedKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signedBy(t, r, leakedKey)
		r.RestartListing(leaked) // step 1
		release(t, listingClient(t, r, leaked), "h-live")
		listed := readKeyRows(t, r, signedrecord.KeyID(leaked))
		tables := runKeyDeletes(t, r, listed, keepAll)
		if tables["nats_hold"] || !tables["nats_hold_log"] {
			t.Fatalf("step 3's read listed %v; want h-live's earlier log entries, and not the hold row its release wrote", listed)
		}
		r.RestartListing()
		if tables["nats_hold"] {
			if err := r.client(t).Bootstrap(ctx); err != nil {
				t.Fatal(err)
			}
		}
		afterStep4(t, r)
	})

	t.Run("a leaked key that signed the live row: step 4 deletes the row, and every client but the operator is refused until the restore", func(t *testing.T) {
		// TN deep's Minor 2: when the read lists the hold row, deleting it leaves the auth
		// service's view unknown until the row is restored. Fails while the README does not say
		// so in step 4.
		readmeSays(t, "every client but the operator is refused from that delete until the restore")
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-1", holdrecord.WorkflowBucketsCreate, nil)
		release(t, c, "h-1")
		leaked, leakedKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signedBy(t, r, leakedKey)
		r.RestartListing(leaked)
		listed := readKeyRows(t, r, signedrecord.KeyID(leaked))
		tables := runKeyDeletes(t, r, listed, keepAll)
		if !tables["nats_hold"] {
			t.Fatalf("step 3's read listed %v; want the hold row", listed)
		}
		r.RestartListing()
		if p, known := r.HoldView(false).State(ctx); known {
			t.Errorf("after the hold row's delete the view reads %q, known; want unknown until the restore", p)
		}
		if err := r.client(t).Bootstrap(ctx); err != nil {
			t.Fatalf("Bootstrap after step 4 answered %v; want the first row stored", err)
		}
		afterStep4(t, r)
	})
}

// listingClient is the operator's client after step 1 rolled the new key out to every command that
// verifies the hold's rows: it signs with the rig's key, the new one, and verifies against it and
// old.
func listingClient(t *testing.T, r rig, old ed25519.PublicKey) *holdrecord.Client {
	t.Helper()
	cfg := r.Config()
	cfg.VerifyKeys = append(cfg.VerifyKeys, old)
	c, err := holdrecord.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
