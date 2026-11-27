package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A RePublish of another stream stores, at a session's key, a body that reads as the session's own
// record - its key, a machine, a time, its projects and the client instance a conditional delete
// names - with that stream's headers, so a direct get reports it at that stream's sequence, the
// control, and a delete fenced there is refused. After that refusal the conditional delete and the
// plain delete each read the key from the stream's leader, find a message the server wrote, and
// delete it at the sequence it is stored at (bus.KeyValue.DeleteAt). The leader's read
// the edge confirms a session by (StoredByWriter) reads the session's own record as a writer's and
// the republished one as none.
func TestADeleteOverARepublishedRecordDeletesItAtItsTrueSequence(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	ctx := context.Background()
	kv, err := client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.SessionBucket, err)
	}
	for _, tc := range []struct {
		victim string
		delete func(victim string) error
	}{
		{"ses_republished_deleted_if", func(victim string) error {
			outcome, err := r.DeleteIf(ctx, victim, DeleteConditions{ClientInstanceID: "instance-victim"})
			if err == nil && outcome != EntryDeleted {
				return errors.New(outcome.String())
			}
			return err
		}},
		{"ses_republished_deleted", func(victim string) error { return r.Delete(ctx, victim) }},
	} {
		if err := r.Put(tc.victim, SessionEntry{MachineID: "edge-a", ClientInstanceID: "instance-victim", Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("register %s: %v", tc.victim, err)
		}
		if _, own, err := r.StoredByWriter(ctx, tc.victim); err != nil || !own {
			t.Fatalf("StoredByWriter(%s) over its own registration = %t, %v; want true", tc.victim, own, err)
		}
		body, err := json.Marshal(map[string]any{"session_id": tc.victim, "machine_id": "edge-a", "updated_at": time.Now().UnixMilli(),
			"projects": []string{"PRIV"}, "client_instance_id": "instance-victim"})
		if err != nil {
			t.Fatalf("encode the republished record: %v", err)
		}
		stored := testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, tc.victim, body)
		if entry, err := kv.Get(tc.victim); err != nil || entry.Revision() == stored {
			t.Fatalf("a direct get of %s answers revision %v (%v), stored at %d: want another stream's sequence, or the control shows nothing to correct", tc.victim, entry, err, stored)
		}
		if _, own, err := r.StoredByWriter(ctx, tc.victim); err != nil || own {
			t.Fatalf("StoredByWriter(%s) over the republished record = %t, %v; want false", tc.victim, own, err)
		}
		if err := tc.delete(tc.victim); err != nil {
			t.Fatalf("delete %s over the record republished at %d: %v", tc.victim, stored, err)
		}
		if _, err := kv.Get(tc.victim); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatalf("Get(%s) after the delete = %v, want %v", tc.victim, err, nats.ErrKeyNotFound)
		}
	}
}

// A RePublish of another stream stores, at a session's key, a record that names another session,
// with that stream's headers, so a direct get reports it at that stream's sequence (the control)
// and DecodeEntry refuses it, which the registry's copy evicts. PutWithin's compare-and-swap loop
// reads the key back: a direct get fences the write on the copied sequence and the bucket refuses
// it at every attempt, so before the fix every registration of the key times out and answers 503
// for good. After the first refusal the loop reads the key from the stream's leader, finds a
// message the server wrote, and replaces it at the sequence it is stored at, so the registration
// heals.
func TestARegistrationOverARepublishedRecordHealsAtItsTrueSequence(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	kv, err := client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.SessionBucket, err)
	}
	const victim = "ses_republished_registration"
	// The victim registers first, so the bucket's stream holds a message of its own before the
	// copy and stores the copy at a sequence past the one the copy's headers name.
	if err := r.Put(victim, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register %s: %v", victim, err)
	}
	body, err := json.Marshal(map[string]any{"session_id": "ses_other", "machine_id": "edge-x",
		"updated_at": time.Now().UnixMilli(), "projects": []string{"PUB"}})
	if err != nil {
		t.Fatalf("encode the republished record: %v", err)
	}
	stored := testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, victim, body)
	awaitApplied(t, r, victim, stored)
	if entry, err := kv.Get(victim); err != nil || entry.Revision() == stored {
		t.Fatalf("a direct get of %s answers revision %v (%v), stored at %d: want another stream's sequence, or the control shows nothing to correct", victim, entry, err, stored)
	}
	if _, own, err := r.StoredByWriter(context.Background(), victim); err != nil || own {
		t.Fatalf("StoredByWriter(%s) over the republished record = %t, %v; want false", victim, own, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.PutWithin(ctx, victim, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register %s over the record republished at %d: %v", victim, stored, err)
	}
	if _, own, err := r.StoredByWriter(context.Background(), victim); err != nil || !own {
		t.Fatalf("StoredByWriter(%s) after the registration = %t, %v; want true", victim, own, err)
	}
	entry, err := r.Refresh(context.Background(), victim)
	if err != nil || entry.MachineID != "edge-a" || entry.SessionID != victim || !slices.Equal(entry.Projects, []string{"PRIV"}) {
		t.Fatalf("Refresh(%s) after the registration = %+v, %v; want the victim's own registration", victim, entry, err)
	}
}

// A direct get (Refresh, the read bucketSessionEntry makes) and the leader read (StoredByWriter) are
// two reads of one key, and a writer's write landing between them makes them return different
// records. cmd/listener's role-claim refresh keeps the projects of the one record StoredByWriter
// returns, never the direct get's, so such a write cannot leave a RePublished record's projects
// standing. This forces the divergence with the registry's own read seam
// (interleavingSessionGetKeyValue): the direct get reads [PRIV PUB], a writer's record of [PRIV]
// lands after it, and the leader read returns [PRIV]. A decision made on the direct get keeps PUB;
// the leader read does not.
func TestADirectGetAndTheLeaderReadCanReturnDifferentProjects(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	const victim = "ses_two_read_race"
	if err := r.Put(victim, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV", "PUB"}}); err != nil {
		t.Fatalf("seed %s: %v", victim, err)
	}
	writer, err := json.Marshal(map[string]any{"session_id": victim, "machine_id": "edge-a",
		"updated_at": time.Now().UnixMilli(), "projects": []string{"PRIV"}})
	if err != nil {
		t.Fatalf("encode the writer's record: %v", err)
	}
	seam := &interleavingSessionGetKeyValue{KeyValue: r.watcher.KV().Legacy(), key: victim, value: writer, interleaves: 1}
	useKV(t, r, seam)
	if _, err := r.Refresh(context.Background(), victim); err != nil {
		t.Fatalf("Refresh(%s): %v", victim, err)
	}
	// Assert on the record the direct get itself returned, not on Refresh's answer: Refresh returns
	// through applyRead, which re-reads the cache after applying the read, and the watcher can apply
	// the seam's interleaved [PRIV] write between the two, so Refresh's answer is timing-dependent.
	// The direct get's own value is what the role-claim refresh's keepProjects decision reads.
	directGet, err := DecodeEntry(victim, seam.firstGetValue())
	if err != nil {
		t.Fatalf("decode the direct get's value: %v", err)
	}
	if !slices.Equal(directGet.Projects, []string{"PRIV", "PUB"}) {
		t.Fatalf("the direct get read projects %v, want the seeded [PRIV PUB] before the interleaved write", directGet.Projects)
	}
	confirmed, ok, err := r.StoredByWriter(context.Background(), victim)
	if err != nil || !ok {
		t.Fatalf("StoredByWriter(%s) = %+v, %t, %v; want a writer's record", victim, confirmed, ok, err)
	}
	if !slices.Equal(confirmed.Projects, []string{"PRIV"}) {
		t.Errorf("StoredByWriter returned projects %v, want the leader's [PRIV]; the role-claim refresh keeps these, not the direct get's [PRIV PUB]", confirmed.Projects)
	}
}

// A RePublish of another stream stores, at an outside session's key, a body that reads as the
// session's own record - its key, the edge's machine, a time, its projects and a repository gap
// mark the writer chose - with that stream's headers, so StoredByWriter reads it as no writer's
// record and the edge grants it no project. The edge's relay clears a gap mark with its own frame
// (ClearRepositoryGap) and sets one when it withholds a GitHub event (SetRepositoryGap), each
// through rewriteStoredEntry. On a client that reads the bucket by a stream message get, as the
// edge's does (bus.WithStreamMessageReads), a direct get reports the RePublished record at its true
// sequence, so a write fenced there would land; rewriteStoredEntry reads the key from the stream
// leader instead, finds a message the server wrote, and writes nothing - the clear is nil and the
// set is the missing error - so StoredByWriter stays false and the record is never laundered into a
// writer's. The control is the same steps on a direct-get client (every in-house listener), which
// refused the laundering write before this fix too.
func TestAGapWriteDoesNotLaunderARepublishedRecordForTheEdge(t *testing.T) {
	for _, edgeReads := range []bool{true, false} {
		name := "direct gets"
		if edgeReads {
			name = "edge stream message reads"
		}
		t.Run(name, func(t *testing.T) {
			options := []bus.ConnectOption{bus.WithReplicas(1)}
			if edgeReads {
				options = append(options, bus.WithStreamMessageReads())
			}
			client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, options...)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			// The test's own server: its registry creates the session bucket it finds missing, as
			// a fixture's does (setupNATS), since no listener creates one.
			bustest.Fixture(client.Conn)
			r := openBindingRegistry(t, client)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			const victim, mark = "ses_edge_launder", "mark-chosen-by-the-writer"
			if err := r.PutWithin(ctx, victim, SessionEntry{MachineID: "edge-a", Projects: []string{"OUT"}, Delivery: DeliveryConsumer}); err != nil {
				t.Fatalf("register %s: %v", victim, err)
			}
			body, err := json.Marshal(map[string]any{
				"session_id": victim, "machine_id": "edge-a", "updated_at": time.Now().UnixMilli(),
				"projects": []string{"OUT", "PRIV"}, "delivery": string(DeliveryConsumer), "repository_gap": mark,
			})
			if err != nil {
				t.Fatal(err)
			}
			stored := testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, victim, body)
			awaitApplied(t, r, victim, stored)
			if cached, err := r.Get(victim); err != nil || cached.RepositoryGap != mark {
				t.Fatalf("the cache did not take the RePublished record's mark: %+v, %v", cached, err)
			}
			if _, own, err := r.StoredByWriter(ctx, victim); err != nil || own {
				t.Fatalf("precondition: StoredByWriter over the RePublished record = %t, %v; want false", own, err)
			}
			// The clear reads a message the server wrote at the key, so it writes nothing: no error.
			if err := r.ClearRepositoryGap(ctx, victim, mark); err != nil {
				t.Fatalf("clear %s's gap over a RePublished record: %v", victim, err)
			}
			after, own, err := r.StoredByWriter(ctx, victim)
			if err != nil {
				t.Fatalf("StoredByWriter after the clear: %v", err)
			}
			if own || slices.Contains(after.Projects, "PRIV") {
				t.Errorf("the gap clear laundered the RePublished record: StoredByWriter own=%t projects=%v, want false", own, after.Projects)
			}
			// The set over a server-written record is the missing error, so the relay gives its
			// withheld event back rather than acknowledging it against a laundered record.
			if err := r.SetRepositoryGap(ctx, victim, "a-new-mark"); !errors.Is(err, nats.ErrKeyNotFound) {
				t.Errorf("SetRepositoryGap over a RePublished record = %v, want it refused as a missing entry", err)
			}
			if _, own, err := r.StoredByWriter(ctx, victim); err != nil || own {
				t.Errorf("StoredByWriter after the set = own=%t, %v; want false, the record not laundered", own, err)
			}
		})
	}
}

// faultyDeleteKeyValue injects a chosen error on the nth Delete call through this handle, counting
// from 1, so a test drives the fenced delete's refusal (bus.KeyValue.DeleteAt) without a real race:
// the real delete runs where fault answers nil. NewestFromLeader reads the stream, not this
// handle's Delete, so DeleteAt's leader read still classifies the key's record while its own delete is
// faulted.
type faultyDeleteKeyValue struct {
	nats.KeyValue
	mu      sync.Mutex
	deletes int
	fault   func(n int) error
}

// armFault sets the fault and resets the call count to 0, so a test arms the fault only for the
// deletes it drives next (DeleteIf's), past any delete useKV's rebuild made.
func (kv *faultyDeleteKeyValue) armFault(fault func(n int) error) {
	kv.mu.Lock()
	kv.deletes = 0
	kv.fault = fault
	kv.mu.Unlock()
}

func (kv *faultyDeleteKeyValue) Delete(key string, opts ...nats.DeleteOpt) error {
	kv.mu.Lock()
	fault := kv.fault
	kv.deletes++
	n := kv.deletes
	kv.mu.Unlock()
	if fault != nil {
		if err := fault(n); err != nil {
			return err
		}
	}
	return kv.KeyValue.Delete(key, opts...)
}

// lostToAReplicatingWrite is what nats.go v1.55.0 answers a Delete with nats.LastRevision that the
// leader of a bucket of more than one copy refuses while another write of the key is still being
// replicated: JetStream's wrong last sequence as error 10164, wrapped in nats.ErrKeyRevisionMismatch.
// nats.ErrKeyExists matches 10071 alone, so it does not match this refusal; bus.IsCASConflict does.
func lostToAReplicatingWrite() error {
	refused := &nats.APIError{Code: 400, ErrorCode: nats.JSErrCodeStreamWrongLastSequenceConstant, Description: "wrong last sequence"}
	return fmt.Errorf("%w: %w", refused, nats.ErrKeyRevisionMismatch)
}

// Q6 folds DeleteIf's fenced-delete fallback into bus.DeleteAt, which triggers on one
// compare-and-swap predicate (bus.IsCASConflict: a wrong last sequence, 10071 or 10164), where the
// old DeleteIf triggered on errors.Is(nats.ErrKeyExists) alone, which matches 10071 alone. Two
// outcomes change, and each row is red with the old ErrKeyExists check put back (delete.go):
//   - a wrong last sequence ErrKeyExists does not match over a record the server wrote now re-reads
//     from the leader and deletes it (EntryDeleted); the old check misses that refusal and answers
//     EntryUnanswered.
//   - a non-CAS failure of that server-written delete now answers EntryUnanswered; the old code
//     retried it and answered EntryKeptAfterLostRace.
func TestSessionRegistryDeleteIf_Q6ChangedOutcomes(t *testing.T) {
	// A wrong last sequence that errors.Is(nats.ErrKeyExists) does not match and bus.IsCASConflict
	// does: the case Q6's predicate catches and the old one missed.
	rawWrongLastSeq := lostToAReplicatingWrite()
	if errors.Is(rawWrongLastSeq, nats.ErrKeyExists) || !bus.IsCASConflict(rawWrongLastSeq) {
		t.Fatalf("the fault %v: errors.Is(nats.ErrKeyExists) = %t, bus.IsCASConflict = %t; want false, true", rawWrongLastSeq, errors.Is(rawWrongLastSeq, nats.ErrKeyExists), bus.IsCASConflict(rawWrongLastSeq))
	}
	for _, tc := range []struct {
		name      string
		key       string
		fault     func(n int) error
		want      DeleteOutcome
		wantError bool
	}{
		{
			name: "a raw wrong last sequence over a server-written record re-reads and deletes it",
			key:  "ses_q6_raw_wls",
			fault: func(n int) error { // the first fenced delete refuses with a raw wrong last sequence
				if n == 1 {
					return rawWrongLastSeq
				}
				return nil // the server-written delete at the true sequence runs
			},
			want: EntryDeleted,
		},
		{
			name: "a non-CAS failure of the server-written delete answers unanswered",
			key:  "ses_q6_server_write_fails",
			fault: func(n int) error { // the fenced delete refuses (ErrKeyExists), the server-written delete fails
				if n%2 == 0 {
					return errors.New("the bucket did not answer the server-written delete")
				}
				return nil
			},
			want:      EntryUnanswered,
			wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := setupNATS(t)
			r := openBindingRegistry(t, client)
			ctx := context.Background()
			if err := r.Put(tc.key, SessionEntry{MachineID: "edge-a", ClientInstanceID: "instance-victim", Projects: []string{"PRIV"}}); err != nil {
				t.Fatalf("register %s: %v", tc.key, err)
			}
			body, err := json.Marshal(map[string]any{"session_id": tc.key, "machine_id": "edge-a", "updated_at": time.Now().UnixMilli(),
				"projects": []string{"PRIV"}, "client_instance_id": "instance-victim"})
			if err != nil {
				t.Fatal(err)
			}
			testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, tc.key, body)
			if _, own, err := r.StoredByWriter(ctx, tc.key); err != nil || own {
				t.Fatalf("StoredByWriter(%s) over the republished record = %t, %v; want false", tc.key, own, err)
			}
			faulty := &faultyDeleteKeyValue{KeyValue: r.watcher.KV().Legacy()}
			useKV(t, r, faulty)
			faulty.armFault(tc.fault)
			outcome, err := r.DeleteIf(ctx, tc.key, DeleteConditions{ClientInstanceID: "instance-victim"})
			if tc.wantError != (err != nil) {
				t.Fatalf("DeleteIf(%s) error = %v, want an error: %t", tc.key, err, tc.wantError)
			}
			if outcome != tc.want {
				t.Errorf("DeleteIf(%s) = %s, want %s", tc.key, outcome, tc.want)
			}
		})
	}
}

// DeleteIf's own switch case, apart from the fenced-delete fallback inside bus.DeleteAt that the Q6
// rows above drive: a key that becomes a plain delete marker between DeleteIf's read and its fenced
// delete. The read finds the session's registration and every condition admits it; the fenced delete
// at that revision is then refused with a wrong last sequence errors.Is(nats.ErrKeyExists) does not
// match (lostToAReplicatingWrite) and bus.IsCASConflict catches; and bus.DeleteAt leaves the refusal
// standing, since the leader holds a marker, not a record the server wrote. DeleteIf's
// `case bus.IsCASConflict` re-reads the key, finds it gone, and answers EntryDeleted. The Q6 rows'
// faults are consumed inside bus.DeleteAt, so this is the only row that reaches DeleteIf's own
// switch: with that case put back to errors.Is(err, nats.ErrKeyExists), the refusal falls through
// to EntryUnanswered with an error.
func TestSessionRegistryDeleteIf_AKeyThatBecameAMarkerReReadsAndAnswersDeleted(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	ctx := context.Background()
	const key = "ses_deleteif_marker_race"
	if err := r.Put(key, SessionEntry{MachineID: "edge-a", ClientInstanceID: "instance-victim", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register %s: %v", key, err)
	}
	faulty := &faultyDeleteKeyValue{KeyValue: r.watcher.KV().Legacy()}
	useKV(t, r, faulty)
	var marked bool
	faulty.armFault(func(n int) error {
		if n != 1 {
			return nil
		}
		// The key becomes a plain delete marker between DeleteIf's read and its fenced delete: a real
		// unconditional delete lands through the embedded handle (no fault counted), so the stream's
		// leader holds a marker, and the fenced delete the fault stands in for is refused as a write
		// that lost to another write of the key still being replicated.
		if err := faulty.KeyValue.Delete(key); err != nil {
			t.Errorf("mark %s: %v", key, err)
		}
		marked = true
		return lostToAReplicatingWrite()
	})
	outcome, err := r.DeleteIf(ctx, key, DeleteConditions{ClientInstanceID: "instance-victim"})
	if err != nil {
		t.Fatalf("DeleteIf(%s) error = %v, want none", key, err)
	}
	if !marked {
		t.Fatal("the marker fault never fired, so no raw wrong-last-sequence reached DeleteIf's switch")
	}
	if outcome != EntryDeleted {
		t.Errorf("DeleteIf(%s) = %s, want %s: the retry re-reads the key, finds the marker, and answers deleted", key, outcome, EntryDeleted)
	}
}

// raceOnFirstUpdate runs before once, just before the first Update through this handle, so a test
// lands a competing write between a writer's read of a key's revision and its conditional write,
// making that write lose the compare-and-swap.
type raceOnFirstUpdate struct {
	nats.KeyValue
	updates int
	before  func()
}

func (kv *raceOnFirstUpdate) Update(key string, value []byte, last uint64) (uint64, error) {
	kv.updates++
	if kv.updates == 1 && kv.before != nil {
		kv.before()
	}
	return kv.KeyValue.Update(key, value, last)
}

// N1: PutClearingGap writes no gap mark, so a claim refresh over a forged record drops the forged
// mark. But when its write loses a compare-and-swap to this session's own writer's record that landed
// in between - a registration, or a relay's gap-mark write - the retry reads that record from the
// leader (readEntryFromLeader) and carries its gap mark rather than clearing it, so the session is not
// made to miss the one notice the mark owes. Control (b785a2a6db): the retry clears the mark whatever
// landed, and the record ends with none.
func TestPutClearingGapKeepsAWritersGapMarkThatLandsOnALostRace(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	other := openBindingRegistry(t, client)
	ctx := context.Background()
	const sessionID = "ses_clear_gap_race"
	if err := r.Put(sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	// A relay's gap-mark write lands on the session's record just before PutClearingGap's own write,
	// so that write loses the compare-and-swap and the loop reads the leader again.
	var raced bool
	seam := &raceOnFirstUpdate{KeyValue: r.watcher.KV().Legacy(), before: func() {
		if err := other.SetRepositoryGap(ctx, sessionID, "relay-mark"); err != nil {
			t.Errorf("land the relay's gap mark: %v", err)
		}
		raced = true
	}}
	useKV(t, r, seam)
	if err := r.PutClearingGap(ctx, sessionID, "edge-a", true, []string{"PRIV"}); err != nil {
		t.Fatalf("PutClearingGap(%s): %v", sessionID, err)
	}
	if !raced {
		t.Fatal("the racing write never landed, so no lost compare-and-swap drove the retry")
	}
	stored, found, err := r.StoredByWriter(ctx, sessionID)
	if err != nil || !found {
		t.Fatalf("read %s after the refresh: found=%t err=%v", sessionID, found, err)
	}
	if stored.RepositoryGap != "relay-mark" {
		t.Errorf("PutClearingGap dropped the writer's gap mark on a lost race: gap=%q, want relay-mark", stored.RepositoryGap)
	}
}

// N1b/N9/N9b: when the session's own writer's record has caught up in this registry's cache before
// the clearing write (the ordering the lost-race row above does not reach, since the write lands on
// its first try), PutClearingGap writes that record back with only the claim's machine,
// self-subscription and projects changed, keeping its gap mark, its whole delivery set (Delivery,
// Epoch, ConsumerStart), its client instance and its capabilities. Keeping Delivery without
// ConsumerStart would read as a consumer holding every stored message; clearing the instance or
// capabilities would unprotect a live session's role and mode. Control (ba7e0cc7aa): the fast cache
// path, or a keep of only the gap mark and Delivery, drops the rest.
func TestPutClearingGapKeepsTheStandingWritersRecordWhenTheCacheHasCaughtUp(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	other := openBindingRegistry(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const sessionID, mark = "ses_clear_gap_cached", "relay-mark"
	if err := r.PutWithin(ctx, sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}, Delivery: DeliveryConsumer, Epoch: "e1", ConsumerStart: 3}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "machine_id": "edge-a", "updated_at": time.Now().UnixMilli(),
		"projects": []string{"PRIV"}, "title": "forged"})
	if err != nil {
		t.Fatal(err)
	}
	stored := testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, sessionID, body)
	awaitApplied(t, r, sessionID, stored)
	if _, own, err := r.StoredByWriter(ctx, sessionID); err != nil || own {
		t.Fatalf("precondition: StoredByWriter over the RePublish = %t, %v; want false (the refresh's clearForged)", own, err)
	}
	// The session's own writer's record lands, with a consumer delivery set, a client instance and
	// capabilities, then a relay marks it; this registry's cache applies both before PutClearingGap
	// reads its fence.
	live := SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}, Delivery: DeliveryConsumer, Epoch: "e2",
		ConsumerStart: 7, ClientInstanceID: "instance-live", Capabilities: []string{"aside"}, Title: "live"}
	if err := other.PutWithin(ctx, sessionID, live); err != nil {
		t.Fatalf("the writer's registration: %v", err)
	}
	if err := other.SetRepositoryGap(ctx, sessionID, mark); err != nil {
		t.Fatalf("the relay's mark: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		cached, err := r.Get(sessionID)
		return err == nil && cached.RepositoryGap == mark && cached.Epoch == "e2"
	})
	// The refresh's write, as roleSetHandler makes it under clearForged.
	if err := r.PutClearingGap(ctx, sessionID, "edge-a", true, []string{"PRIV"}); err != nil {
		t.Fatalf("PutClearingGap(%s): %v", sessionID, err)
	}
	after, own, err := r.StoredByWriter(ctx, sessionID)
	if err != nil || !own {
		t.Fatalf("read %s after the refresh: own=%t err=%v", sessionID, own, err)
	}
	if after.UpdatedAt <= 0 {
		t.Fatalf("the refresh stamped no registration time on %s: %+v", sessionID, after)
	}
	// The whole written entry is compared (as the H1 row does), so a display field dropped, or a new
	// SessionEntry field added later, fails the row: only the machine, self-subscription, projects and
	// time change; the gap mark, the whole delivery set and the client instance, capabilities and
	// display fields are kept.
	got := after
	got.UpdatedAt = 0
	want := live
	want.SessionID = sessionID
	want.SelfSubscribed = true
	want.RepositoryGap = mark
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PutClearingGap over the standing writer's record wrote %+v, want %+v", got, want)
	}
}

// V2: over a writer's record that names ANOTHER session - one readForClear's seam returns as a
// writer's record whose session_id is not this key - PutClearingGap keeps none of its gap mark or
// delivery set: the keep is only for the session's own record. Control: the key test loosened from
// `found && stored.SessionID == sessionID` to `found` keeps the foreign record's fields.
func TestPutClearingGapKeepsNothingFromAWritersRecordNamingAnotherSession(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const sessionID = "ses_clear_foreign"
	if err := r.PutWithin(ctx, sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	foreign := SessionEntry{SessionID: "ses_someone_else", MachineID: "edge-a", Projects: []string{"PRIV"},
		Delivery: DeliveryConsumer, Epoch: "e9", ConsumerStart: 99, ClientInstanceID: "foreign", Capabilities: []string{"forged"}, RepositoryGap: "foreign-mark"}
	restore := r.SetStoredByWriterSeamForTest(sessionID, foreign, true, nil)
	err := r.PutClearingGap(ctx, sessionID, "edge-a", true, []string{"PRIV"})
	restore()
	if err != nil {
		t.Fatalf("PutClearingGap(%s): %v", sessionID, err)
	}
	after, own, err := r.StoredByWriter(ctx, sessionID)
	if err != nil || !own {
		t.Fatalf("read %s after the refresh: own=%t err=%v", sessionID, own, err)
	}
	if after.RepositoryGap != "" || after.Delivery != "" || after.Epoch != "" || after.ConsumerStart != 0 || after.ClientInstanceID != "" || len(after.Capabilities) != 0 {
		t.Errorf("PutClearingGap kept a foreign record's fields: %+v", after)
	}
}

// Q19: PutClearingGap decides from the record each pass read. The first pass reads the session's own
// writer's record (consumer delivery) and its write loses a compare-and-swap to a RePublish that
// lands in between; the second pass reads that RePublished record and clears. The written entry
// carries no delivery from the first pass's record. Control (ba7e0cc7aa): the clearing arm mutates an
// entry that lives across passes, so the second pass writes the first pass's delivery.
func TestPutClearingGapDecidesFromEachPassRecord(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const sessionID = "ses_clear_perpass"
	if err := r.PutWithin(ctx, sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}, Delivery: DeliveryConsumer, Epoch: "e1", ConsumerStart: 5}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	var raced bool
	seam := &raceOnFirstUpdate{KeyValue: r.watcher.KV().Legacy(), before: func() {
		body, err := json.Marshal(map[string]any{"session_id": sessionID, "machine_id": "edge-a", "updated_at": time.Now().UnixMilli(),
			"projects": []string{"PRIV"}, "title": "forged"})
		if err != nil {
			t.Errorf("marshal the RePublish body: %v", err)
			return
		}
		testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, sessionID, body)
		raced = true
	}}
	useKV(t, r, seam)
	if err := r.PutClearingGap(ctx, sessionID, "edge-a", true, []string{"PRIV"}); err != nil {
		t.Fatalf("PutClearingGap(%s): %v", sessionID, err)
	}
	if !raced {
		t.Fatal("the racing RePublish never landed, so no lost compare-and-swap drove a second pass")
	}
	after, own, err := r.StoredByWriter(ctx, sessionID)
	if err != nil || !own {
		t.Fatalf("read %s after the refresh: own=%t err=%v", sessionID, own, err)
	}
	if after.Delivery != "" || after.ConsumerStart != 0 {
		t.Errorf("PutClearingGap carried the first pass's record's delivery set over the second pass's forged record: delivery=%q start=%d, want cleared", after.Delivery, after.ConsumerStart)
	}
}

// N8: PutWithin carries no gap mark from a record DecodeEntry refuses for the key - a writer's plain
// put that names another session, which every reader reads as absent. The cache drops such a record,
// so the next PutWithin reads it by direct get at its true sequence and would otherwise carry its
// mark. Control: a record that names its own key carries its mark, as designed.
func TestPutWithinCarriesNoMarkFromARecordDecodeEntryRefuses(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	carried := func(sessionID, names string) string {
		t.Helper()
		if err := r.PutWithin(ctx, sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("register %s: %v", sessionID, err)
		}
		body, err := json.Marshal(map[string]any{"session_id": names, "machine_id": "edge-a", "updated_at": time.Now().UnixMilli(),
			"projects": []string{"PRIV"}, "repository_gap": "mark-of-" + names})
		if err != nil {
			t.Fatal(err)
		}
		revision, err := r.watcher.KV().Legacy().Put(sessionID, body)
		if err != nil {
			t.Fatalf("plain put at %s: %v", sessionID, err)
		}
		awaitApplied(t, r, sessionID, revision)
		if err := r.PutWithin(ctx, sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("re-register %s: %v", sessionID, err)
		}
		stored, own, err := r.StoredByWriter(ctx, sessionID)
		if err != nil || !own {
			t.Fatalf("read %s: own=%t err=%v", sessionID, own, err)
		}
		return stored.RepositoryGap
	}
	if own := carried("ses_q2_own", "ses_q2_own"); own == "" {
		t.Fatal("CONTROL: PutWithin carried no mark from a record naming its own key; the row cannot tell")
	}
	if other := carried("ses_q2_victim", "ses_q2_someone_else"); other != "" {
		t.Errorf("PutWithin carried the gap mark %q of a record every reader reads as absent", other)
	}
}
