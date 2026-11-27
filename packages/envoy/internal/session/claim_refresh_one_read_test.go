package session

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// raceOnFirstWrite runs before once, just before the first write of a value through this handle,
// a Put or an Update, so a test lands a write between a writer's read of a key and its own write
// whichever way that write is sent.
type raceOnFirstWrite struct {
	nats.KeyValue

	mu     sync.Mutex
	writes int
	before func()
}

func (kv *raceOnFirstWrite) first() {
	kv.mu.Lock()
	kv.writes++
	first := kv.writes == 1
	kv.mu.Unlock()
	if first && kv.before != nil {
		kv.before()
	}
}

func (kv *raceOnFirstWrite) Put(key string, value []byte) (uint64, error) {
	kv.first()
	return kv.KeyValue.Put(key, value)
}

func (kv *raceOnFirstWrite) Update(key string, value []byte, last uint64) (uint64, error) {
	kv.first()
	return kv.KeyValue.Update(key, value, last)
}

func (kv *raceOnFirstWrite) count() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.writes
}

// forgedSessionBody is a body a RePublish of another stream stores at sessionID's key: it names the
// key, a machine, a time and projects, so it reads as sessionID's own registration
// (DecodeEntry), and it carries a title, a delivery, a gap mark and a client instance of the
// forger's choosing.
func forgedSessionBody(t *testing.T, sessionID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id": sessionID, "machine_id": "edge-x", "updated_at": time.Now().UnixMilli(),
		"projects": []string{"PUB"}, "title": "forged", "delivery": "broken", "epoch": "forged-epoch",
		"repository_gap": "forged-mark", "client_instance_id": "forged-instance",
	})
	if err != nil {
		t.Fatalf("encode the RePublished record: %v", err)
	}
	return body
}

// requireClearedClaimRefresh fails the test unless sessionID's record, as the stream's leader holds
// it, is a writer's record naming sessionID with only what a claim refresh over a record no
// writer's stands for writes (PutClearingGap): the machine, the self-subscription, the claim
// token's projects and a stamped UpdatedAt, and none of the forged fields. It reads the first three.
func requireClearedClaimRefresh(t *testing.T, r *SessionRegistry, sessionID, what string) {
	t.Helper()
	after, own, err := r.StoredByWriter(context.Background(), sessionID)
	if err != nil || !own {
		t.Fatalf("%s: StoredByWriter(%s) = %+v, %t, %v; want a writer's record", what, sessionID, after, own, err)
	}
	if after.SessionID != sessionID || after.MachineID != "edge-a" || !after.SelfSubscribed || !slices.Equal(after.Projects, []string{"PRIV"}) {
		t.Errorf("%s: the record of %s is %+v, want its own key, edge-a, self-subscribed and the claim token's [PRIV]", what, sessionID, after)
	}
	if after.Title != "" || after.Delivery != "" || after.Epoch != "" || after.RepositoryGap != "" || after.ClientInstanceID != "" {
		t.Errorf("%s: the record of %s keeps the server-written record's fields: %+v", what, sessionID, after)
	}
}

// A role claim's refresh decides what it writes from one read of the stream leader's record and
// writes at that read's revision (PutClearingGap). A record the server wrote - a RePublish whose
// body reads as the session's own registration - is replaced with the session's own record naming
// its key, carrying none of the RePublish's fields: neither the cache nor a direct get can tell it
// from a registration, and the leader's read can. Nor is it answered as the session's own record,
// so cmd/listener's roleSetHandler takes the client instance its predecessor rule falls back to
// from its own read of the claimant (liveSessionEntry) instead, which the cache answers where it
// holds the session, and the cache reads the RePublish as a registration.
func TestAClaimRefreshReplacesAServerWrittenRecordWithOneNamingItsKey(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	const sessionID = "ses_refresh_server_written"
	if err := r.Put(sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}, Title: "own"}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	stored := testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, sessionID, forgedSessionBody(t, sessionID))
	awaitApplied(t, r, sessionID, stored)
	if cached, err := r.Get(sessionID); err != nil || cached.Title != "forged" {
		t.Fatalf("precondition: the cache reads %s as %+v (%v), want the RePublished record, which it cannot tell from a registration", sessionID, cached, err)
	}
	if _, own, err := r.StoredByWriter(context.Background(), sessionID); err != nil || own {
		t.Fatalf("precondition: StoredByWriter(%s) over the RePublish = %t, %v; want false", sessionID, own, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prior, own, err := r.PutClearingGap(ctx, sessionID, "edge-a", true, []string{"PRIV"})
	if err != nil {
		t.Fatalf("PutClearingGap(%s) over the record republished at %d: %v", sessionID, stored, err)
	}
	requireNoOwnRecord(t, prior, own, "over the RePublish")
	requireClearedClaimRefresh(t, r, sessionID, "after the refresh over the RePublish")
}

// A RePublish the server stores between the refresh's read of the session's own record and its
// write moves the key's revision, so the write, fenced on that read's revision, loses; the refresh
// reads the leader again, finds the RePublish, and decides again from it: the record it writes is
// the session's own key with the claim's machine, self-subscription and projects, and none of the
// RePublish's fields, nor the fields of the own record the first read found, since a decision is
// never carried over from a read whose revision the write lost on.
//
// Control: the write not conditioned on the read's revision (an unconditioned Put in place of the
// Update at that revision in PutClearingGap) lands over the RePublish on the first pass with the
// first read's own-record fields: no second write is sent, and the stored record keeps the title
// the first read found.
func TestAServerWriteBetweenAClaimRefreshsReadAndItsWriteMakesItReadAgain(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	const sessionID = "ses_refresh_raced_by_server"
	if err := r.Put(sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}, Title: "own", Delivery: DeliveryConsumer, Epoch: "e1", ConsumerStart: 4}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	var raced uint64
	seam := &raceOnFirstWrite{KeyValue: r.watcher.KV().Legacy(), before: func() {
		raced = testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, sessionID, forgedSessionBody(t, sessionID))
	}}
	useKV(t, r, seam)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := r.PutClearingGap(ctx, sessionID, "edge-a", true, []string{"PRIV"}); err != nil {
		t.Fatalf("PutClearingGap(%s): %v", sessionID, err)
	}
	if raced == 0 {
		t.Fatal("the RePublish never landed between the refresh's read and its write, so the row tests nothing")
	}
	if writes := seam.count(); writes != 2 {
		t.Errorf("the refresh sent %d writes, want 2: the one the RePublish at %d made lose, and the one decided again from the leader's read", writes, raced)
	}
	requireClearedClaimRefresh(t, r, sessionID, "after the refresh raced by a RePublish")
	after, _, _ := r.StoredByWriter(context.Background(), sessionID)
	if after.Delivery != "" || after.ConsumerStart != 0 {
		t.Errorf("the refresh carried the first read's delivery set over the RePublish it lost to: %+v", after)
	}
}
