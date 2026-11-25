// A write planned on a key that held no entry lands only while the key still holds none: a write
// that comes late, after another writer created the key and deleted it again, is refused over that
// delete's marker, and the caller plans again over what the bucket holds now (putInterestAt,
// SetRoleWithPrevious).
//
// Before those writes went out as an Update at the revision their plan read, a plan that found no
// entry wrote through Create, and nats.go's Create, refused over a delete marker, reads the key
// again and updates at that marker's revision (nats.go@v1.55.0 kv.go:729-739): the late write
// landed over the delete and brought the record back.

package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// lateKeyWrite runs late, once, before the first write through it of key (Put, Create or Update)
// reaches the bucket: what another writer does between a plan's read and its write. late's own
// writes go through the raw handle and are not hooked again. Create is hooked whole: nats.go's
// Create updates through its own handle, not through this one.
type lateKeyWrite struct {
	natsgo.KeyValue

	key    string
	late   func()
	writes int
}

func (kv *lateKeyWrite) Put(key string, value []byte) (uint64, error) {
	kv.before(key)
	return kv.KeyValue.Put(key, value)
}

func (kv *lateKeyWrite) Create(key string, value []byte) (uint64, error) {
	kv.before(key)
	return kv.KeyValue.Create(key, value)
}

func (kv *lateKeyWrite) Update(key string, value []byte, revision uint64) (uint64, error) {
	kv.before(key)
	return kv.KeyValue.Update(key, value, revision)
}

func (kv *lateKeyWrite) before(key string) {
	if key != kv.key {
		return
	}
	kv.writes++
	if kv.writes == 1 {
		kv.late()
	}
}

// createdAndDeleted plays the other writer: it stores value under key and deletes it again,
// through raw, leaving a delete marker as the key's newest entry.
func createdAndDeleted(t *testing.T, raw natsgo.KeyValue, key string, value []byte) {
	t.Helper()
	if _, err := raw.Put(key, value); err != nil {
		t.Fatalf("another writer stores %s: %v", key, err)
	}
	if err := raw.Delete(key); err != nil {
		t.Fatalf("another writer deletes %s: %v", key, err)
	}
}

// A registration's planned write of a session with no interest record, which another writer's
// row and its delete (an unsubscribe, a reap) overtake before the write lands, is refused
// (ErrInterestPlanConflict), and the bucket still holds no record for the session. The plan made
// again over the bucket reads the delete, and its write lands over that delete.
func TestALatePlannedInterestWriteIsRefusedOverADeleteMadeAfterItsPlan(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, kv := coldRegistry(t, conn)
	const sessionID = "ses_late_interest"
	topics := []string{contracts.LegacyAgentSubject(sessionID), "notifications.team.late"}

	plan, err := reg.PlanInterest(context.Background(), sessionID, topics)
	if err != nil {
		t.Fatalf("plan the registration of %s: %v", sessionID, err)
	}
	if plan.Revision != 0 {
		t.Fatalf("the plan of %s read revision %d; the row needs a key that holds no entry", sessionID, plan.Revision)
	}
	late := &lateKeyWrite{KeyValue: kv, key: sessionID, late: func() {
		createdAndDeleted(t, kv, sessionID, []byte(`{"session_id":"`+sessionID+`","topics":["notifications.team.other"]}`))
	}}
	handle, err := bus.WrapKeyValue(conn, late)
	if err != nil {
		t.Fatalf("wrap the bucket the late write goes through: %v", err)
	}
	reg.watcher = kvwatch.New("interest registry", handle, interestCache{reg})

	row, err := reg.UpsertPlanned(context.Background(), Interest{SessionID: sessionID, MachineID: "m1"}, plan)
	if late.writes == 0 {
		t.Fatal("the planned write never reached the bucket, so nothing overtook it")
	}
	if entry, getErr := kv.Get(sessionID); !errors.Is(getErr, natsgo.ErrKeyNotFound) {
		t.Fatalf("the bucket holds %s's record at revision %d (%s) after its late planned write (answered %+v, %v): the write planned on no entry landed over the delete made after its plan",
			sessionID, entry.Revision(), entry.Value(), row, err)
	}
	if !errors.Is(err, ErrInterestPlanConflict) {
		t.Fatalf("the late planned write answered %+v (%v), want %v, so its caller plans again", row, err, ErrInterestPlanConflict)
	}

	replan, err := reg.PlanInterestAfterConflict(context.Background(), sessionID, topics)
	if err != nil {
		t.Fatalf("plan the registration of %s again: %v", sessionID, err)
	}
	if replan.Revision == 0 {
		t.Fatalf("the plan made again over the delete read revision 0; its write would be refused over the delete marker for good")
	}
	if _, err := reg.UpsertPlanned(context.Background(), Interest{SessionID: sessionID, MachineID: "m1"}, replan); err != nil {
		t.Fatalf("the write planned over the delete at revision %d: %v", replan.Revision, err)
	}
	if _, err := kv.Get(sessionID); err != nil {
		t.Fatalf("the bucket holds no record of %s after the write planned over the delete: %v", sessionID, err)
	}
}

// A soft claim of a role no claim record holds, which another session's claim and its release
// overtake before the claim's write lands, is refused by the bucket, and the role is left
// unclaimed, as the release left it. The claim is answered its write's own refusal, an error
// matching nats.ErrKeyExists, since the release left no holder to name in an *ErrRoleHeld. POST
// /v1/roles/set never meets this answer: its soft claim names the revision it decided on
// (IfClaimRevision), so it is answered *ErrRoleClaimRevisionMismatch, on which it decides once
// more.
func TestALateSoftClaimIsRefusedOverAReleaseMadeAfterItsRead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)
	const (
		role     = "legion-reviewer"
		claimant = "ses_late_claimant"
		other    = "ses_claimed_and_released"
	)
	raw := reg.roleKV.Legacy()
	late := &lateKeyWrite{KeyValue: raw, key: role, late: func() {
		createdAndDeleted(t, raw, role, []byte(`{"holder_session_id":"`+other+`"}`))
	}}
	reg.roleKV = bus.NewKeyValue(late, reg.roleKV.WatchHandle(), reg.roleKV.Streams())

	_, err := reg.SetRoleWithPrevious(context.Background(), RoleClaimRequest{
		SessionID: claimant,
		MachineID: "example-host",
		Role:      role,
		Soft:      true,
	})
	if late.writes == 0 {
		t.Fatal("the claim never reached its conditional write, so nothing overtook it")
	}
	if stood := storedRoleClaim(t, reg, role); stood.HolderSessionID != "" {
		t.Fatalf("the role is held by %q at revision %d after the late soft claim (answered %v): the claim read on no record landed over the release made after its read",
			stood.HolderSessionID, stood.Revision, err)
	}
	if !errors.Is(err, natsgo.ErrKeyExists) {
		t.Fatalf("the late soft claim answered %v, want its write's refusal (nats.ErrKeyExists)", err)
	}
}

// returnedKeyWrite is lateKeyWrite that also closes returned once the first write through it of
// its key has come back from the bucket: a row whose claim abandons that write waits for it there,
// since an abandoned write runs on after its caller has answered (bus.BoundedOn).
type returnedKeyWrite struct {
	*lateKeyWrite
	returned chan struct{}
	once     sync.Once
}

func (kv *returnedKeyWrite) Put(key string, value []byte) (uint64, error) {
	defer kv.back(key)
	return kv.lateKeyWrite.Put(key, value)
}

func (kv *returnedKeyWrite) Create(key string, value []byte) (uint64, error) {
	defer kv.back(key)
	return kv.lateKeyWrite.Create(key, value)
}

func (kv *returnedKeyWrite) Update(key string, value []byte, revision uint64) (uint64, error) {
	defer kv.back(key)
	return kv.lateKeyWrite.Update(key, value, revision)
}

func (kv *returnedKeyWrite) back(key string) {
	if key == kv.key {
		kv.once.Do(func() { close(kv.returned) })
	}
}

// An outright claim of a role no claim record holds, whose caller abandons it while its write is in
// flight (the claim route's request ended), is refused by the bucket once another session's claim
// and its release have overtaken it: the role is left unclaimed, as the release left it, and not
// held by a claimant that was answered a failure. The claim writes at the revision of the entry it
// read (roleClaimToWrite: 0 for a key that held none), never through Create, which, refused over
// the release's delete marker, updates at that marker's revision (nats.go@v1.55.0 kv.go:729-739)
// and lands.
func TestALateOutrightClaimItsCallerAbandonedIsRefusedOverAReleaseMadeAfterItsRead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)
	const (
		role     = "legion-planner"
		claimant = "ses_abandoned_claimant"
		other    = "ses_claimed_and_released"
	)
	ctx, abandon := context.WithCancel(context.Background())
	defer abandon()
	raw := reg.roleKV.Legacy()
	late := &returnedKeyWrite{lateKeyWrite: &lateKeyWrite{KeyValue: raw, key: role, late: func() {
		createdAndDeleted(t, raw, role, []byte(`{"holder_session_id":"`+other+`"}`))
		abandon()
	}}, returned: make(chan struct{})}
	reg.roleKV = bus.NewKeyValue(late, reg.roleKV.WatchHandle(), reg.roleKV.Streams())

	_, err := reg.SetRoleWithPrevious(ctx, RoleClaimRequest{
		SessionID: claimant,
		MachineID: "example-host",
		Role:      role,
	})
	if late.writes == 0 {
		t.Fatal("the claim never reached its write, so nothing overtook it")
	}
	select {
	case <-late.returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned claim's write never came back from the bucket")
	}
	if stood := storedRoleClaim(t, reg, role); stood.HolderSessionID != "" {
		t.Fatalf("the role is held by %q at revision %d after the abandoned outright claim (answered %v): the claim read on no record landed over the release made after its read",
			stood.HolderSessionID, stood.Revision, err)
	}
	if err == nil {
		t.Fatal("the abandoned outright claim answered success")
	}
}

// A conditional claim that required no claim record (IfClaimRevision 0), which another session's
// claim and its release overtake before its write lands, is refused for its write's lost
// compare-and-swap (*ErrRoleClaimRevisionMismatch), and the read after that finds no claim record
// again: both revisions are 0. The refusal says the write lost to a write since this claim's read.
// POST /v1/roles/set answers a soft claim of a role no record holds with this text when it loses
// that race twice, since it conditions such a claim on the revision it decided on, 0
// (IfClaimRevision).
//
// Before, the text named the one revision twice: "role legion-tester's claim is at revision 0, not
// the 0 this claim required".
func TestALateConditionalClaimOverAClaimAndItsReleaseSaysItsWriteLost(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)
	const (
		role     = "legion-tester"
		claimant = "ses_late_conditional_claimant"
		other    = "ses_claimed_and_released_again"
	)
	raw := reg.roleKV.Legacy()
	late := &lateKeyWrite{KeyValue: raw, key: role, late: func() {
		createdAndDeleted(t, raw, role, []byte(`{"holder_session_id":"`+other+`"}`))
	}}
	reg.roleKV = bus.NewKeyValue(late, reg.roleKV.WatchHandle(), reg.roleKV.Streams())

	none := uint64(0)
	_, err := reg.SetRoleWithPrevious(context.Background(), RoleClaimRequest{
		SessionID:       claimant,
		MachineID:       "example-host",
		Role:            role,
		IfClaimRevision: &none,
	})
	if late.writes == 0 {
		t.Fatal("the claim never reached its conditional write, so nothing overtook it")
	}
	var mismatch *ErrRoleClaimRevisionMismatch
	if !errors.As(err, &mismatch) || mismatch.Expected != 0 || mismatch.Actual != 0 || mismatch.Holder != "" {
		t.Fatalf("UNKNOWN: the late conditional claim answered %v (%+v), want a mismatch whose required and read revisions are both 0 and that names no holder; the row needs the refusal that reads none twice", err, mismatch)
	}
	want := "role " + role + "'s claim changed after this claim read it at revision 0: its conditional write lost to a write that landed since"
	if got := mismatch.Error(); got != want {
		t.Fatalf("the late conditional claim's refusal reads %q, want %q", got, want)
	}
}
