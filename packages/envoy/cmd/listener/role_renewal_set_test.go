// The registration's renewal takes its roles from the interest row its own write returned, beside
// the role claim copy (store.Registry.RenewRoleClaims): a claim the copy lost is renewed at the
// session's next POST /v1/interests/subscribe, with no whole read of the role claims in between,
// and an interest cache that has not followed the bucket hides no role from it and costs it no
// read of its own.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/store"
)

// bucketInterestRow is sessionID's interest row as the interest bucket holds it, read through a
// handle of client's own, so the read caches nothing in any registry.
func bucketInterestRow(t *testing.T, client *bus.Client, sessionID string) store.Interest {
	t.Helper()
	interests, err := client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	entry, err := interests.Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's interest row: %v", sessionID, err)
	}
	var row store.Interest
	if err := json.Unmarshal(entry.Value(), &row); err != nil {
		t.Fatalf("decode %s's interest row: %v", sessionID, err)
	}
	return row
}

// holdInterestCache stops registry's interest watcher, reached through the field
// breakInterestReads reaches it by, so its cache keeps the rows it holds while the bucket moves
// on: what a listener's cache is between another listener's write and its delivery. The
// registry's own reads and writes go on through the same handle, and each still caches what it
// read or wrote.
func holdInterestCache(t *testing.T, registry *store.Registry) {
	t.Helper()
	field := reflect.ValueOf(registry).Elem().FieldByName("watcher")
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*kvwatch.Watcher).Stop()
}

// renewalMarkKV is a role bucket whose first Get of role calls mark before it reads: the renewal's
// read of that role's claim, the first a registration that names no controller notice lane makes,
// so mark runs when the registration's own store work is done and the renewal's begins.
type renewalMarkKV struct {
	natsgo.KeyValue
	role string
	once *sync.Once
	mark func()
}

func (kv renewalMarkKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.role {
		kv.once.Do(kv.mark)
	}
	return kv.KeyValue.Get(key)
}

// c-renewal-reads-the-record, listener half. A session's interest record names a role's lane
// topic (the grant wrote it there), the store holds the session's claim of that role, and the
// listener's role claim copy has lost the claim. One registration renews the claim: its renewal
// moves to the registration's moment, and no whole read of the role claims completes in between,
// since the watcher's scheduled read is held. The test logs the renewal before and after.
func TestARenewOnSubscribeRenewsAClaimTheCopyLost(t *testing.T) {
	const (
		holder = "ses_renew_copy_lost"
		role   = "renew-copy-lost"
	)
	rig := newGrantGuardRig(t)
	claimed := rig.seed(t, holder, role)
	dropRoleFromCopy(t, rig.deps.registry, role)
	requireDrift(t, rig.deps.registry, role, "", holder)
	if row := bucketInterestRow(t, rig.client, holder); !slices.Contains(row.Topics, contracts.RoleTopicPrefix+role) {
		t.Fatalf("%s's interest record holds %v, want the lane topic of %s, which the grant writes there", holder, row.Topics, role)
	}
	reads := rig.deps.registry.RoleClaimRepair().Completed
	rig.clock.Advance(contracts.SessionHeartbeat)
	at := rig.clock.Now()

	rig.register(t, holder, holdingProcess)

	renewed := rig.storedClaim(t, role)
	t.Logf("the claim was renewed at %d before the registration and at %d after it (the registration's moment is %d)",
		claimed.RenewedAt, renewed.RenewedAt, at.UnixMilli())
	if renewed.RenewedAt != at.UnixMilli() || renewed.HolderSessionID != holder || renewed.ClientInstanceID != holdingProcess {
		t.Fatalf("after the registration the claim is %+v, want %s's claim by %s renewed at the registration's moment %d", renewed, holder, holdingProcess, at.UnixMilli())
	}
	if got := rig.deps.registry.RoleClaimRepair().Completed; got != reads {
		t.Fatalf("%d whole reads of the role claims had completed before the registration and %d after it, want none between: the renewal must not wait on the repair", reads, got)
	}
	for _, warning := range warnLinesNaming(rig.logs.String(), holder) {
		if warning["msg"] == "listener subscribe left a role claim unrenewed" {
			t.Fatalf("the registration logged a renewal it could not make: %v", warning)
		}
	}
}

// c-set-read-from-the-store, listener half. The listener's interest cache holds the session's row
// from before another listener's grant, without the role's lane topic, while the bucket's row has
// it, and the role claim copy has lost the claim. One registration plans from the cache, has its
// write refused the stale revision (store.ErrInterestPlanConflict), and plans and writes again
// over the bucket; the renewal is handed the row that second write returned, and renews the claim.
//
// The interest bucket's direct gets are counted on the server's subject for them, as
// internal/store/kv_test.go counts the role bucket's, up to the renewal's read of the claim and
// after it. The renewal's count must be zero: its set is the row it was handed. The registration's
// is logged as measured: its three plans and writes, the read confirmRegistered makes through
// RefreshAt (storedTopics), and any the consumer filter fence adds; fewer than the three would say
// the count is not counting.
func TestTheSetReadFromTheStoreRenewsOnTheRegistrationsConflictPath(t *testing.T) {
	const (
		holder = "ses_set_read_from_store"
		role   = "set-read-from-store"
	)
	roleTopic := contracts.RoleTopicPrefix + role
	rig := newGrantGuardRig(t)
	registry := rig.deps.registry
	rig.register(t, holder, holdingProcess)
	holdInterestCache(t, registry)

	// Another listener grants the role: the claim, and the lane topic on the bucket's row.
	other, err := store.Open(rig.client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the other listener's registry: %v", err)
	}
	t.Cleanup(other.StopWatch)
	waitForRegistry(t, other)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := other.SetRoleWithPrevious(ctx, store.RoleClaimRequest{SessionID: holder, MachineID: "other-machine", ClientInstanceID: holdingProcess, Role: role}); err != nil {
		t.Fatalf("grant %s to %s on the other listener: %v", role, holder, err)
	}
	waitFor(t, 10*time.Second, fmt.Sprintf("%s's claim to reach the role claim copy", role), func() bool {
		held, err := registry.RolesHeldBy(holder)
		return err == nil && slices.Contains(held, role)
	})
	dropRoleFromCopy(t, registry, role)
	requireDrift(t, registry, role, "", holder)
	cached, _ := registry.Cached(holder)
	stored := bucketInterestRow(t, rig.client, holder)
	t.Logf("the cache holds %s's topics %v and the bucket %v", holder, cached.Topics, stored.Topics)
	if slices.Contains(cached.Topics, roleTopic) || !slices.Contains(stored.Topics, roleTopic) {
		t.Fatalf("the cache holds %v and the bucket %v, want the lane topic %s in the bucket's row alone", cached.Topics, stored.Topics, roleTopic)
	}
	claimed := rig.storedClaim(t, role)

	gets, err := rig.client.Conn.SubscribeSync(fmt.Sprintf("$JS.API.DIRECT.GET.KV_%s.>", bus.InterestBucket))
	if err != nil {
		t.Fatalf("watch the interest bucket's direct gets: %v", err)
	}
	t.Cleanup(func() { _ = gets.Unsubscribe() })
	// count is how many direct gets of the interest bucket the server has handed the watch: each
	// get the registry made was answered, so the server had already copied it to the watch, and
	// the flush has the connection read that copy before it reads the flush's answer.
	count := func() int {
		if err := rig.client.Conn.Flush(); err != nil {
			t.Errorf("flush the watch of direct gets: %v", err)
		}
		n, _, err := gets.Pending()
		if err != nil {
			t.Errorf("read the watch of direct gets: %v", err)
		}
		return n
	}
	start := count()
	// The mark runs on the goroutine the renewal's bounded read runs on (bus.Bounded).
	var atRenewal atomic.Int64
	atRenewal.Store(-1)
	setRegistryRoleKV(t, registry, renewalMarkKV{
		KeyValue: registryRoleKV(t, registry),
		role:     role,
		once:     &sync.Once{},
		mark:     func() { atRenewal.Store(int64(count())) },
	})
	rig.clock.Advance(contracts.SessionHeartbeat)
	at := rig.clock.Now()

	rig.register(t, holder, holdingProcess)

	end := count()
	marked := int(atRenewal.Load())
	if marked < 0 {
		t.Fatalf("the registration never read %s's claim: no renewal of it ran", role)
	}
	registrationReads, renewalReads := marked-start, end-marked
	t.Logf("interest bucket direct gets: the registration's %d, the renewal's %d", registrationReads, renewalReads)
	if renewalReads != 0 {
		t.Fatalf("the renewal read the interest bucket %d times, want none: its set is the row the registration's write returned", renewalReads)
	}
	if registrationReads < 3 {
		t.Fatalf("the registration read the interest bucket %d times, want at least its three plans and writes: the count is not counting", registrationReads)
	}
	replanned := 0
	for _, warning := range warnLinesNaming(rig.logs.String(), holder) {
		if warning["msg"] == "listener plans the session's interest again: another writer stored the row" {
			replanned++
		}
	}
	if replanned != 1 {
		t.Fatalf("the registration planned again on a refused write %d times, want once: the case is the conflict path\n%s", replanned, rig.logs.String())
	}
	renewed := rig.storedClaim(t, role)
	t.Logf("the claim was renewed at %d before the registration and at %d after it (the registration's moment is %d)",
		claimed.RenewedAt, renewed.RenewedAt, at.UnixMilli())
	if renewed.RenewedAt != at.UnixMilli() || renewed.HolderSessionID != holder || renewed.ClientInstanceID != holdingProcess {
		t.Fatalf("after the registration the claim is %+v, want %s's claim by %s renewed at the registration's moment %d", renewed, holder, holdingProcess, at.UnixMilli())
	}
}
