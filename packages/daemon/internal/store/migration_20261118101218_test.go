package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// updateDriftItemsVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const updateDriftItemsVersion = 20261118101218

// 20261118101218 adds claims.update_drift, the drift an update round was opened for as items
// (supervise.Claim.UpdateDrift), beside its text (claims.update_reasons). Before it, a write of the
// column is refused: the column is not there. A claim in a round the release before opened reads
// back after it with its text and no items, which is what that round recorded, so its ask and its
// notice name the text. Items written after it read back whole for the round they were written
// for, and items of any other round, which an older daemon's write leaves in place, read as none.
func TestTheUpdateDriftMigrationLeavesAnOlderRoundWithItsTextAndTheItemsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, updateDriftItemsVersion-1)
	const round, other = "pod:a-pod-uid", "pod:another-pod-uid"
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
		plugin_contract, update_round, update_reasons, update_opened_at)
		values ('legion-LEGION-209-implementer', 'legion', 'LEGION-208', 'LEGION-209', 'implementer', 1,
		'ses_implementer', '', 'idle', 0, 0, 0, 0, 46, $1, 'image old, now new', now())`, round); err != nil {
		t.Fatalf("seed a claim in a round the release before opened: %v", err)
	}
	_, err := store.Pool().Exec(ctx, `update claims set update_drift = '{"round":"pod:a-pod-uid","items":[]}'`)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a write of update_drift before the migration = %v, want it refused: the column is the migration's", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the update round's drift items: %v", err)
	}
	read := func(when string) []runtime.DriftItem {
		t.Helper()
		claims, err := store.Claims(ctx)
		if err != nil {
			t.Fatalf("%s: read the claims: %v", when, err)
		}
		if len(claims) != 1 || claims[0].UpdateRound != round || claims[0].UpdateReasons != "image old, now new" {
			t.Fatalf("%s: the claims read %+v, want the one claim in its round with its text", when, claims)
		}
		return claims[0].UpdateDrift
	}
	if got := read("after the migration"); got != nil {
		t.Fatalf("after the migration the older round's claim reads the items %+v, want none: that round recorded its text alone", got)
	}

	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := claims[0]
	items := []runtime.DriftItem{
		{Key: runtime.DriftImage, Image: "ghcr.io/cybermonklabs/legion-worker@sha256:" + strings.Repeat("ab", 32)},
		{Key: "resources"}, {Key: "account"},
	}
	c.UpdateDrift = items
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("write the claim with its round's items: %v", err)
	}
	if got := read("after a write of the items"); !slices.Equal(got, items) {
		t.Fatalf("the claim reads the items %+v, want %+v as written", got, items)
	}

	if _, err := store.Pool().Exec(ctx, `update claims set update_round = $1`, other); err != nil {
		t.Fatalf("move the claim to another round, as an older daemon's write does: %v", err)
	}
	claims, err = store.Claims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := claims[0].UpdateDrift; got != nil {
		t.Fatalf("the claim in round %s reads the items %+v written for %s, want none", other, got, round)
	}

	c = claims[0]
	c.UpdateRound, c.UpdateReasons, c.UpdateOpenedAt = "", "", nil
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("write the claim out of its round: %v", err)
	}
	var cleared bool
	if err := store.Pool().QueryRow(ctx, `select update_drift is null from claims`).Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("a claim written in no round holds update_drift, want null")
	}
}
