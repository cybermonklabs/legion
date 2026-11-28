package store

// The server writes an interest row itself when a request names that row's subject as its reply
// subject (testnats.ServerWritesKey): a direct get's copy of another session's row, or a
// JetStream API answer. Neither is the row of the session its key names, and the next write for
// that session replaces it with the session's own row, under the session's own key: a stored row
// never chooses the key a write lands on, and never lends its topics to another session.

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	bindingPubTopic  = "notifications.dispatch.issue.PUB.>"
	bindingPrivTopic = "notifications.dispatch.issue.PRIV.>"
)

// awaitInterestApplied waits until the registry's copy has taken the row stored for key at
// revision, whether it kept it or evicted it: the copy's fence holds the revision of every record
// it applied.
func awaitInterestApplied(t *testing.T, r *Registry, key string, revision uint64) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		_, epoch := r.watcher.Handle()
		return r.cache.FenceFor(epoch, key) >= revision
	})
}

// registerPlanned writes item's row with topics as the listener's registration route does: a plan,
// then the planned write, planned again over the bucket's row after a conflict, three plans at
// most (cmd/listener's registrationPlanAttempts).
func registerPlanned(ctx context.Context, r *Registry, item Interest, topics []string) (Interest, error) {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		var plan InterestPlan
		if attempt == 1 {
			plan, err = r.PlanInterest(ctx, item.SessionID, topics)
		} else {
			plan, err = r.PlanInterestAfterConflict(ctx, item.SessionID, topics)
		}
		if err != nil {
			return Interest{}, err
		}
		var row Interest
		row, err = r.UpsertPlanned(ctx, item, plan)
		if errors.Is(err, ErrInterestPlanConflict) {
			continue
		}
		return row, err
	}
	return Interest{}, err
}

// rowWrites are the two writes that store a session's own row: the registration's planned write
// and the merging write a role claim makes. suffix names each one's sessions apart.
var rowWrites = []struct {
	name, suffix string
	write        func(ctx context.Context, r *Registry, item Interest, topics []string) (Interest, error)
}{
	{"the registration's planned write", "planned", registerPlanned},
	{"the merging write", "merged", func(ctx context.Context, r *Registry, item Interest, topics []string) (Interest, error) {
		return r.UpsertWithin(ctx, item, topics)
	}},
}

// requireOwnRow fails the test unless row is sessionID's own, on edge-a, holding the PRIV topic
// and no PUB topic.
func requireOwnRow(t *testing.T, row Interest, sessionID, what string) {
	t.Helper()
	if row.SessionID != sessionID || row.MachineID != "edge-a" || !slices.Contains(row.Topics, bindingPrivTopic) || slices.Contains(row.Topics, bindingPubTopic) {
		t.Errorf("%s answers %+v, want %s's own row on edge-a holding %s and not %s", what, row, sessionID, bindingPrivTopic, bindingPubTopic)
	}
}

// G3pre at the store: the server copies S_pub's row onto a session's key before that session's
// first registration. Read plainly the copy names S_pub, and the write merged into it and stored
// it under S_pub's key, at S_pub's own revision, which a direct get of the copy reports: the
// registration was answered S_pub's row and wrote its own topics into S_pub's. The copy names
// another key, so the write replaces it with the session's own row under the session's key, and
// S_pub's row keeps its revision, which is to say its stored message.
func TestAnInterestRowNamingAnotherKeyIsReplacedNotMerged(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg := openRegistry(t, conn)
	ctx := context.Background()
	for _, write := range rowWrites {
		t.Run(write.name, func(t *testing.T) {
			source, victim := "ses_pub_"+write.suffix, "ses_priv_"+write.suffix
			if _, err := reg.Upsert(Interest{SessionID: source, MachineID: "shared-a", Dir: "/pub"}, []string{bindingPubTopic}); err != nil {
				t.Fatalf("register %s: %v", source, err)
			}
			sourceRow, sourceRevision, err := reg.RefreshAt(ctx, source)
			if err != nil {
				t.Fatalf("read %s's row: %v", source, err)
			}
			stored := testnats.ServerCopiesKey(t, conn, bus.InterestBucket, victim, bus.InterestBucket, source)
			awaitInterestApplied(t, reg, victim, stored)

			row, err := write.write(ctx, reg, Interest{SessionID: victim, MachineID: "edge-a", Dir: "/priv"}, []string{bindingPrivTopic})
			if err != nil {
				t.Fatalf("%s for %s over the copy: %v", write.name, victim, err)
			}
			requireOwnRow(t, row, victim, write.name)

			after, revision, err := reg.RefreshAt(ctx, source)
			if err != nil || revision != sourceRevision || !reflect.DeepEqual(after, sourceRow) {
				t.Errorf("%s's row after %s is %+v at revision %d (%v), want it unchanged: %+v at revision %d", source, write.name, after, revision, err, sourceRow, sourceRevision)
			}
			own, ownRevision, err := reg.RefreshAt(ctx, victim)
			if err != nil || ownRevision <= stored {
				t.Fatalf("the bucket's row of %s is %+v at revision %d (%v), want one written after the copy stored at %d", victim, own, ownRevision, err, stored)
			}
			requireOwnRow(t, own, victim, "the bucket's row of "+victim)
		})
	}
}

// A deregistration of a session whose key holds a copy of another session's row deletes the copy.
// The direct get of the copy reports the revision its own headers name, the source row's, so a
// delete fenced on it was refused with wrong last sequence and the deregistration failed; the
// delete is fenced on the revision the copy is stored at, and the source's row is not touched.
func TestADeregistrationDeletesAnInterestRowNamingAnotherKey(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg := openRegistry(t, conn)
	ctx := context.Background()
	if _, err := reg.Upsert(Interest{SessionID: "ses_pub", MachineID: "shared-a", Dir: "/pub"}, []string{bindingPubTopic}); err != nil {
		t.Fatalf("register ses_pub: %v", err)
	}
	pubRow, pubRevision, err := reg.RefreshAt(ctx, "ses_pub")
	if err != nil {
		t.Fatalf("read ses_pub's row: %v", err)
	}
	testnats.ServerCopiesKey(t, conn, bus.InterestBucket, "ses_priv", bus.InterestBucket, "ses_pub")

	if _, err := reg.Remove(ctx, "ses_priv", nil, RoleReleaseAuthority{}); err != nil {
		t.Fatalf("deregister ses_priv over the copy: %v", err)
	}
	if row, revision, err := reg.RefreshAt(ctx, "ses_priv"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("ses_priv's row after its deregistration = %+v at revision %d, %v; want none", row, revision, err)
	}
	if after, revision, err := reg.RefreshAt(ctx, "ses_pub"); err != nil || revision != pubRevision || !reflect.DeepEqual(after, pubRow) {
		t.Fatalf("ses_pub's row after the deregistration is %+v at revision %d (%v), want it unchanged: %+v at revision %d", after, revision, err, pubRow, pubRevision)
	}
}

// A row naming its key and no machine, or no update time, is no row a registration writes: the
// registry reads it as one it cannot decode, and the next write replaces it with the session's
// own row, merging none of its topics.
func TestAnInterestRowNamingNoMachineOrTimeIsReplacedNotMerged(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg := openRegistry(t, conn)
	ctx := context.Background()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.InterestBucket, err)
	}
	for key, row := range map[string]string{
		"ses_no_machine": `{"session_id":"ses_no_machine","topics":["` + bindingPubTopic + `"],"updated_at":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `}`,
		"ses_no_time":    `{"session_id":"ses_no_time","machine_id":"shared-a","topics":["` + bindingPubTopic + `"]}`,
	} {
		if _, err := kv.Create(key, []byte(row)); err != nil {
			t.Fatalf("store %s: %v", key, err)
		}
		if got, _, err := reg.RefreshAt(ctx, key); !errors.Is(err, ErrMalformedInterest) {
			t.Errorf("RefreshAt(%s) = %+v, %v; want ErrMalformedInterest", key, got, err)
		}
		row, err := registerPlanned(ctx, reg, Interest{SessionID: key, MachineID: "edge-a", Dir: "/priv"}, []string{bindingPrivTopic})
		if err != nil {
			t.Fatalf("register %s over the row: %v", key, err)
		}
		requireOwnRow(t, row, key, "the registration of "+key)
	}
}

// A2 at the store: the server answers a stream info request onto a session's interest key. Read
// plainly it is a row naming the empty session id, and the next write merged into it and wrote it
// under the key "", which no write may name, so every later registration of the session failed
// until something else replaced the row. The answer names no key, so the next write replaces it
// under the session's own key.
func TestAJetStreamAnswerAtAnInterestKeyIsHealedByTheNextWrite(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg := openRegistry(t, conn)
	ctx := context.Background()
	for _, write := range rowWrites {
		t.Run(write.name, func(t *testing.T) {
			victim := "ses_priv_" + write.suffix
			if _, err := write.write(ctx, reg, Interest{SessionID: victim, MachineID: "edge-a", Dir: "/priv"}, []string{bindingPrivTopic}); err != nil {
				t.Fatalf("register %s: %v", victim, err)
			}
			stored := testnats.ServerWritesKey(t, conn, bus.InterestBucket, victim, testnats.StreamInfo("KV_"+bus.InterestBucket), nil)
			awaitInterestApplied(t, reg, victim, stored)

			row, err := write.write(ctx, reg, Interest{SessionID: victim, MachineID: "edge-a", Dir: "/priv"}, []string{bindingPrivTopic})
			if err != nil {
				t.Fatalf("%s for %s over the stream info answer: %v", write.name, victim, err)
			}
			requireOwnRow(t, row, victim, write.name)
			own, revision, err := reg.RefreshAt(ctx, victim)
			if err != nil || revision <= stored {
				t.Fatalf("the bucket's row of %s is %+v at revision %d (%v), want one written after the answer stored at %d", victim, own, revision, err, stored)
			}
			requireOwnRow(t, own, victim, "the bucket's row of "+victim)
		})
	}
}

// The forge proof's P2 at the store: the server copies a delete marker onto a session's key from
// another key's. A direct get answers the copy as a key with no row, so the write that follows
// creates the row, and nats.go's Create, refused for the marker the key holds, updated at the
// revision a direct get reports for that marker: for a copy that is its source's, the
// Nats-Sequence header the copy carries first, so it was refused at every attempt and every
// registration of the session answered 503 for as long as the copy stood. The write replaces the
// marker at the sequence the stream's leader reports it stored at. The read the write planned from
// still answers no row at revision 0, which is what a fence on that read is built on (cmd/listener's
// storedTopics). The control is the session's own delete marker, written at its key and not copied,
// which a write replaced before as now: the copy's reported revision is what the write was refused
// for.
func TestAWriteOverACopiedDeleteMarkerReplacesItAtItsTrueSequence(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg := openRegistry(t, conn)
	ctx := context.Background()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.InterestBucket, err)
	}
	for _, marker := range []struct {
		name, suffix string
		copied       bool
		mark         func(key string) error
	}{
		// A plain marker: nats.go's Delete and Purge with no LastRevision send no
		// expected-sequence header.
		{"a delete marker copied from another key", "copied_delete", true, func(key string) error { return kv.Delete(key) }},
		{"a purge marker copied from another key", "copied_purge", true, func(key string) error { return kv.Purge(key) }},
		{"the session's own delete marker", "own_delete", false, func(key string) error { return kv.Delete(key) }},
	} {
		for _, write := range rowWrites {
			t.Run(marker.name+"/"+write.name, func(t *testing.T) {
				victim := "ses_marked_" + marker.suffix + "_" + write.suffix
				if _, err := write.write(ctx, reg, Interest{SessionID: victim, MachineID: "edge-a", Dir: "/priv"}, []string{bindingPrivTopic}); err != nil {
					t.Fatalf("register %s: %v", victim, err)
				}
				var stored uint64
				if marker.copied {
					source := victim + "_source"
					putInterest(t, kv, Interest{SessionID: source, MachineID: "shared-a", Topics: []string{bindingPubTopic}})
					if err := marker.mark(source); err != nil {
						t.Fatalf("mark %s: %v", source, err)
					}
					stored = testnats.ServerCopiesKey(t, conn, bus.InterestBucket, victim, bus.InterestBucket, source)
				} else {
					if err := marker.mark(victim); err != nil {
						t.Fatalf("mark %s: %v", victim, err)
					}
					stored = testnats.LastSequence(t, conn, bus.InterestBucket, victim)
				}
				awaitInterestApplied(t, reg, victim, stored)
				if row, revision, err := reg.RefreshAt(ctx, victim); !errors.Is(err, natsgo.ErrKeyNotFound) || revision != 0 {
					t.Fatalf("RefreshAt(%s) over %s = %+v at revision %d, %v; want no row at revision 0", victim, marker.name, row, revision, err)
				}

				row, err := write.write(ctx, reg, Interest{SessionID: victim, MachineID: "edge-a", Dir: "/priv"}, []string{bindingPrivTopic})
				if err != nil {
					t.Fatalf("%s for %s over %s stored at %d: %v", write.name, victim, marker.name, stored, err)
				}
				requireOwnRow(t, row, victim, write.name)
				own, revision, err := reg.RefreshAt(ctx, victim)
				if err != nil || revision <= stored {
					t.Fatalf("the bucket's row of %s is %+v at revision %d (%v), want one written after %s stored at %d", victim, own, revision, err, marker.name, stored)
				}
				requireOwnRow(t, own, victim, "the bucket's row of "+victim)
			})
		}
	}
}
