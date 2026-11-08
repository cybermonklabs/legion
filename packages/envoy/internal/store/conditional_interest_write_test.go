// An interest row is read, merged, and stored under the revision that read answered
// (putInterestAt). The window between the read and the write is what this file holds open:
// another writer's row lands inside it, where the revision a planned write checks before the
// write has already passed, so the conditional write itself is the only thing between that row
// and the write built over it.

package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// racingInterestWrite runs a hook before the first write of an interest row reaches the bucket,
// so the row that write is conditional on can be moved under it. The hook hangs on every call
// that stores a value and not on the conditional one alone: a build whose write is unconditional
// races the same way and is judged by what the bucket then holds. The hook's own write goes
// through the bucket handle and is not hooked again, and the write itself is the real one: what
// it suffers is the server's answer, not one this wrapper invented.
type racingInterestWrite struct {
	natsgo.KeyValue

	writes           int
	beforeFirstWrite func()
}

func (kv *racingInterestWrite) Update(key string, value []byte, revision uint64) (uint64, error) {
	kv.racing()
	return kv.KeyValue.Update(key, value, revision)
}

func (kv *racingInterestWrite) Put(key string, value []byte) (uint64, error) {
	kv.racing()
	return kv.KeyValue.Put(key, value)
}

func (kv *racingInterestWrite) Create(key string, value []byte) (uint64, error) {
	kv.racing()
	return kv.KeyValue.Create(key, value)
}

func (kv *racingInterestWrite) racing() {
	kv.writes++
	if kv.writes == 1 {
		kv.beforeFirstWrite()
	}
}

// A write whose row another writer stored between the read it was built from and the write
// itself is refused by the bucket, and the row that writer stored stands. The revision a planned
// write weighs before it writes was read before that row landed, so it passes, and the
// compare-and-swap at the read's revision is the whole of what is left to catch the race
// (putInterestAt, UpsertPlanned). A write that stored its row whatever the bucket held would
// drop every topic the row that won holds - each one a topic the registration that stored it was
// answered 200 for - with nothing to put them back.
func TestAnInterestWriteIsRefusedByARowStoredUnderTheRevisionItRead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, kv := coldRegistry(t, conn)

	const sessionID = "ses_interest_cas"
	const planned, won = "notifications.planned", "notifications.won"
	putInterest(t, kv, Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{"notifications.agent." + sessionID}})
	plan, err := reg.PlanInterest(context.Background(), sessionID, []string{planned})
	if err != nil {
		t.Fatalf("plan the registration of %s: %v", sessionID, err)
	}

	racing := &racingInterestWrite{KeyValue: kv}
	racing.beforeFirstWrite = func() {
		putInterest(t, kv, Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{won}})
	}
	handle, err := bus.WrapKeyValue(conn, racing)
	if err != nil {
		t.Fatalf("wrap the bucket the race goes through: %v", err)
	}
	reg.watcher = kvwatch.New("interest registry", handle, interestCache{reg})

	row, err := reg.UpsertPlanned(context.Background(), Interest{SessionID: sessionID, MachineID: "m1"}, plan)
	if racing.writes == 0 {
		t.Fatal("the planned write never reached the bucket, so it never raced")
	}
	if !errors.Is(err, ErrInterestPlanConflict) {
		t.Fatalf("the planned write answered %+v (%v), want %v: the row it was built from moved under it", row, err, ErrInterestPlanConflict)
	}
	stored, _, err := reg.RefreshAt(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the row the refused write left: %v", err)
	}
	if want := []string{won}; !slices.Equal(stored.Topics, want) {
		t.Fatalf("the bucket holds %v, want %v: the refused write stored nothing over the row that won the race", stored.Topics, want)
	}
}

// A merging write whose row another writer stored between the read it was built from and the
// write itself is refused by the bucket too, and what it stores is built again over the row that
// won (UpsertWithin, writeRowAt): the topics that writer stored stand, and this write's own are
// merged into them. Its one production caller is a role claim (SetRoleWithPrevious), whose write
// without that compare-and-swap would store the row it read back whole - every topic a
// registration on another listener was answered 200 for since, and every topic an unsubscribe
// removed, gone from the record with nothing to put either back.
//
// A merging write answers a refusal by reading the bucket and merging again rather than by
// handing the conflict to its caller, so nothing in what it answers tells a write the bucket
// took at the first attempt from one it refused: the row the bucket holds afterwards is what
// says the write was conditional at all.
func TestAMergingInterestWriteIsBuiltAgainOverARowStoredUnderTheRevisionItRead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, kv := coldRegistry(t, conn)

	const sessionID = "ses_interest_merge_cas"
	const merging, won = "notifications.merging", "notifications.won"
	putInterest(t, kv, Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{"notifications.agent." + sessionID}})

	racing := &racingInterestWrite{KeyValue: kv}
	racing.beforeFirstWrite = func() {
		putInterest(t, kv, Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{won}})
	}
	handle, err := bus.WrapKeyValue(conn, racing)
	if err != nil {
		t.Fatalf("wrap the bucket the race goes through: %v", err)
	}
	reg.watcher = kvwatch.New("interest registry", handle, interestCache{reg})

	row, err := reg.UpsertWithin(context.Background(), Interest{SessionID: sessionID, MachineID: "m1"}, []string{merging})
	if racing.writes == 0 {
		t.Fatal("the merging write never reached the bucket, so it never raced")
	}
	if err != nil {
		t.Fatalf("the merging write answered %v, want the row it built again over the row that won", err)
	}
	want := []string{merging, won}
	if !slices.Equal(row.Topics, want) {
		t.Fatalf("the merging write answered the topics %v, want %v: the row that won the race carries %s", row.Topics, want, won)
	}
	stored, _, err := reg.RefreshAt(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the row the merging write left: %v", err)
	}
	if !slices.Equal(stored.Topics, want) {
		t.Fatalf("the bucket holds %v, want %v: a write the row moved under must be refused and built again over the row that won, not stored over it", stored.Topics, want)
	}
}
