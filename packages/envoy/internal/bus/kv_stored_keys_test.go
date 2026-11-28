package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// StoredKeys lists every key the bucket's stream holds a message on: a key holding a value, one
// past the length Get refuses (testnats.LegacyKeys' unreadable key), and a key whose newest message
// is a delete or a purge marker, which HeldEntries then leaves out. A pattern naming part
// of the bucket lists the keys under it alone. A list asked through a sequence the stream has not
// stored is refused, since the copy that answered could lack a key that held a record there.
func TestStoredKeysListsEveryKeyTheStreamHoldsAMessageOn(t *testing.T) {
	t.Parallel()
	const bucket = "kv_stored_keys"
	_, kv := leaderReadBucket(t, bucket)
	ctx := testwait.Context(t)
	_, legacy := testnats.LegacyKeys(bucket, "u")
	if _, err := kv.Legacy().Put(legacy, []byte("1")); err != nil {
		t.Fatalf("put the %d-byte legacy key: %v", len(legacy), err)
	}
	for _, key := range []string{"held", "deleted", "purged", "p.a", "p.b", "q.c"} {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := kv.Delete("deleted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := kv.Purge("purged"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	state, err := kv.ReadStreamState(ctx)
	if err != nil {
		t.Fatalf("read the stream's state: %v", err)
	}
	through := state.LastSeq

	for _, row := range []struct {
		keys string
		want []string
	}{
		{keys: ">", want: []string{"deleted", "held", legacy, "p.a", "p.b", "purged", "q.c"}},
		{keys: "p.*", want: []string{"p.a", "p.b"}},
	} {
		keys, err := kv.StoredKeys(ctx, row.keys, through)
		if err != nil {
			t.Fatalf("StoredKeys(%s) through the stream's last sequence %d: %v", row.keys, through, err)
		}
		slices.Sort(keys)
		want := slices.Sorted(slices.Values(row.want))
		if !slices.Equal(keys, want) {
			t.Fatalf("StoredKeys(%s) = %v, want %v: every key under the pattern with a message, markers included", row.keys, keys, want)
		}
	}
	if _, err := kv.StoredKeys(ctx, ">", through+1); !errors.Is(err, ErrKeyListBehind) {
		t.Fatalf("StoredKeys through %d, past the stream's last sequence %d = %v, want %v", through+1, through, err, ErrKeyListBehind)
	}
}

// A key another client writes again while the bucket is listed is still listed, in every list:
// the store counts the key's new message before it removes the one it replaces, so the stream's
// subject list never finds the key without one. nats.go's Keys, run the same way, leaves the key
// out of some of its lists, since the key's new message can come after its watcher's first
// delivery has ended.
func TestStoredKeysListsAKeyWrittenAgainDuringTheList(t *testing.T) {
	t.Parallel()
	const bucket = "kv_stored_keys_written_again"
	const lists = 100
	_, kv := leaderReadBucket(t, bucket)
	ctx := testwait.Context(t)
	keys := []string{"steady", "written-again"}
	for _, key := range keys {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	state, err := kv.ReadStreamState(ctx)
	if err != nil {
		t.Fatalf("read the stream's state: %v", err)
	}

	stop, stopped := make(chan struct{}), make(chan struct{})
	writes := 0
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := kv.Put("written-again", []byte("2")); err != nil {
				t.Errorf("write written-again again: %v", err)
				return
			}
			writes++
		}
	}()
	var missed []string
	var listErr error
	for i := range lists {
		listed, err := kv.StoredKeys(ctx, ">", state.LastSeq)
		if err != nil {
			listErr = fmt.Errorf("list %d of %d: %w", i, lists, err)
			break
		}
		for _, key := range keys {
			if !slices.Contains(listed, key) {
				missed = append(missed, fmt.Sprintf("list %d left out %s (listed %v)", i, key, listed))
			}
		}
	}
	close(stop)
	testwait.Receive(t, "the writer of written-again to stop", stopped)

	if listErr != nil {
		t.Fatalf("StoredKeys while written-again is written again: %v", listErr)
	}
	if len(missed) > 0 {
		t.Fatalf("StoredKeys left a key the bucket holds throughout out of %d of %d lists, while written-again was written again %d times: %s",
			len(missed), lists, writes, strings.Join(missed, "; "))
	}
	if writes == 0 {
		t.Fatal("written-again was not written again while the lists ran, so the lists show nothing")
	}
}

// HeldEntries answers each listed key that holds a value, with its revision and value, and leaves
// out a key whose newest message is a delete or a purge marker, which StoredKeys lists like any
// other. The stream state it answers is the one its list was read through.
func TestHeldEntriesLeavesOutAKeyWhoseNewestMessageIsAMarker(t *testing.T) {
	t.Parallel()
	const bucket = "kv_held_entries"
	_, kv := leaderReadBucket(t, bucket)
	ctx := testwait.Context(t)
	revision, err := kv.Put("held", []byte("value"))
	if err != nil {
		t.Fatalf("put held: %v", err)
	}
	for _, key := range []string{"deleted", "purged"} {
		if _, err := kv.Put(key, []byte("gone")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := kv.Delete("deleted"); err != nil {
		t.Fatalf("delete deleted: %v", err)
	}
	if err := kv.Purge("purged"); err != nil {
		t.Fatalf("purge purged: %v", err)
	}
	want, err := kv.ReadStreamState(ctx)
	if err != nil {
		t.Fatalf("read the stream's state: %v", err)
	}
	listed, err := kv.StoredKeys(ctx, ">", want.LastSeq)
	slices.Sort(listed)
	if err != nil || !slices.Equal(listed, []string{"deleted", "held", "purged"}) {
		t.Fatalf("StoredKeys = %v, %v; want [deleted held purged], the markers' keys listed", listed, err)
	}

	state, entries, err := kv.HeldEntries(ctx, ">")
	if err != nil {
		t.Fatalf("HeldEntries: %v", err)
	}
	if state.Name != want.Name || state.LastSeq != want.LastSeq {
		t.Errorf("HeldEntries read stream %s through %d, want %s through its last sequence %d", state.Name, state.LastSeq, want.Name, want.LastSeq)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, fmt.Sprintf("%s@%d=%q (%v)", entry.Key(), entry.Revision(), entry.Value(), entry.Operation()))
	}
	if len(entries) != 1 || entries[0].Key() != "held" || entries[0].Revision() != revision ||
		string(entries[0].Value()) != "value" || entries[0].Operation() != nats.KeyValuePut {
		t.Fatalf("HeldEntries = %v, want held alone, a put at revision %d of \"value\"", got, revision)
	}
}

// HeldEntries answers each of forty listed keys that holds a value, with its revision and value,
// in the list's order, and leaves out each key whose newest message is a delete marker, spread
// through the list.
func TestHeldEntriesAnswersEveryHeldKeyInTheListsOrder(t *testing.T) {
	t.Parallel()
	const bucket = "kv_held_entries_many"
	const put, deleted = 40, 10
	_, kv := leaderReadBucket(t, bucket)
	ctx := testwait.Context(t)
	revisions := make(map[string]uint64, put)
	for i := range put {
		key := fmt.Sprintf("k%02d", i)
		revision, err := kv.Put(key, []byte(key))
		if err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		revisions[key] = revision
	}
	for i := range deleted {
		key := fmt.Sprintf("k%02d", i*put/deleted)
		if err := kv.Delete(key); err != nil {
			t.Fatalf("delete %s: %v", key, err)
		}
		delete(revisions, key)
	}
	state, err := kv.ReadStreamState(ctx)
	if err != nil {
		t.Fatalf("read the stream's state: %v", err)
	}
	listed, err := kv.StoredKeys(ctx, ">", state.LastSeq)
	if err != nil || len(listed) != put {
		t.Fatalf("StoredKeys = %v, %v; want the %d keys put, the deleted ones' markers included", listed, err, put)
	}
	var want []string
	for _, key := range listed {
		if revision, held := revisions[key]; held {
			want = append(want, fmt.Sprintf("%s@%d=%s", key, revision, key))
		}
	}

	_, entries, err := kv.HeldEntries(ctx, ">")
	if err != nil {
		t.Fatalf("HeldEntries: %v", err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, fmt.Sprintf("%s@%d=%s", entry.Key(), entry.Revision(), entry.Value()))
	}
	if len(want) != put-deleted || !slices.Equal(got, want) {
		t.Fatalf("HeldEntries = %v, want the %d keys still held, each at the revision it was put at, in the list's order %v", got, put-deleted, want)
	}
}

// HeldEntries takes each key its watch delivers from that watch, a key holding a value with the
// watch's entry and a key whose newest message is a delete or a purge marker left out, and reads
// neither from the stream's leader. A leader read is the stream message get GetFromLeader sends
// (TestTheLeaderReadSendsTheStreamMessageGetWhereGetSendsTheDirectGet), which the handle's own
// connection receives too, since NATS hands a connection the messages it publishes; the requests
// are counted once a sentinel published after HeldEntries answered has come back
// (leaderReadsBefore), so none is still on its way. A HeldEntries that reads each listed key from
// the leader, as 8fdced00's did, sends six here.
func TestHeldEntriesReadsNoKeyFromTheLeaderThatItsWatchDelivered(t *testing.T) {
	t.Parallel()
	const bucket = "kv_held_entries_watched"
	_, kv := leaderReadBucket(t, bucket)
	ctx := testwait.Context(t)
	held := []string{"held.a", "held.b", "held.c"}
	revisions := make(map[string]uint64, len(held))
	for _, key := range append([]string{"deleted.a", "deleted.b", "purged"}, held...) {
		revision, err := kv.Put(key, []byte(key))
		if err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		revisions[key] = revision
	}
	for _, key := range []string{"deleted.a", "deleted.b"} {
		if err := kv.Delete(key); err != nil {
			t.Fatalf("delete %s: %v", key, err)
		}
	}
	if err := kv.Purge("purged"); err != nil {
		t.Fatalf("purge purged: %v", err)
	}
	// The subscription is made on the connection the handle reads through, so the server takes it
	// ahead of every request HeldEntries sends on it.
	conn := kv.Conn()
	leaderReads, err := conn.SubscribeSync(jsAPI("STREAM.MSG.GET", "KV_"+bucket))
	if err != nil {
		t.Fatalf("subscribe to the stream message gets: %v", err)
	}
	t.Cleanup(func() { _ = leaderReads.Unsubscribe() })

	_, entries, err := kv.HeldEntries(ctx, ">")
	if err != nil {
		t.Fatalf("HeldEntries: %v", err)
	}
	if read := leaderReadsBefore(t, conn, leaderReads, bucket); len(read) != 0 {
		t.Errorf("HeldEntries sent %d stream message gets, of the keys %v; want none, since its watch delivered every key", len(read), read)
	}
	var got, want []string
	for _, entry := range entries {
		got = append(got, fmt.Sprintf("%s@%d=%s (%v)", entry.Key(), entry.Revision(), entry.Value(), entry.Operation()))
	}
	for _, key := range held {
		want = append(want, fmt.Sprintf("%s@%d=%s (%v)", key, revisions[key], key, nats.KeyValuePut))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("HeldEntries = %v, want %v: each key holding a value, a put at the revision it was put at, and no marker's key", got, want)
	}
}

// leaderReadsBefore is the key each stream message get that leaderReads, a subscription on conn,
// holds names (its last_by_subj, under the bucket's prefix), counted only once a sentinel published
// on conn after the read under test answered has come back to it. The server hands one connection
// the messages it publishes in the order it took them, and nats.go files each with its
// subscription as it reads it, so every request conn sent before the sentinel is held by then and
// none is still on its way, with no wait on a clock.
func leaderReadsBefore(t *testing.T, conn *nats.Conn, leaderReads *nats.Subscription, bucket string) []string {
	t.Helper()
	sentinels := make(chan *nats.Msg, 1)
	sentinel, err := conn.ChanSubscribe(nats.NewInbox(), sentinels)
	if err != nil {
		t.Fatalf("subscribe to the sentinel: %v", err)
	}
	defer func() { _ = sentinel.Unsubscribe() }()
	if err := conn.Publish(sentinel.Subject, nil); err != nil {
		t.Fatalf("publish the sentinel: %v", err)
	}
	testwait.Receive(t, "the sentinel published once the read answered", sentinels)
	pending, _, err := leaderReads.Pending()
	if err != nil {
		t.Fatalf("count the stream message gets: %v", err)
	}
	keys := make([]string, 0, pending)
	for range pending {
		// Each is held already (Pending), so the read takes it at once.
		request, err := leaderReads.NextMsg(0)
		if err != nil {
			t.Fatalf("read a stream message get the subscription holds: %v", err)
		}
		var body struct {
			LastBySubj string `json:"last_by_subj"`
		}
		if err := json.Unmarshal(request.Data, &body); err != nil {
			t.Fatalf("decode the stream message get %s: %v", request.Data, err)
		}
		keys = append(keys, strings.TrimPrefix(body.LastBySubj, subjectsPrefix(bucket)))
	}
	return keys
}

// HeldEntries' answer from its list and its watch (heldInListOrder) holds each listed key that
// holds a value, in the list's order: a put the watch delivered is taken from it and a marker it
// delivered is left out, neither read; a key it did not deliver is read, a put answered and
// nats.ErrKeyNotFound left out; any other error of a read ends the answer with that error; and a
// key the watch delivered that the list does not name is not answered. The read records the keys
// it is asked for, which are exactly the listed keys the watch left out, up to the first that
// fails. No server is needed: the watch and the leader are given.
func TestHeldEntriesReadsOnlyTheListedKeysItsWatchLeftOut(t *testing.T) {
	t.Parallel()
	errRead := errors.New("the leader did not answer")
	put := func(key string, revision uint64) watchedEntry {
		return watchedEntry{key: key, value: "w-" + key, revision: revision, op: jetstream.KeyValuePut}
	}
	for _, row := range []struct {
		name    string
		listed  []string
		watched []watchedEntry
		reads   map[string]error
		want    []string
		wantErr error
		read    []string
	}{
		{
			name:    "a put the watch delivered is taken without a read",
			listed:  []string{"a"},
			watched: []watchedEntry{put("a", 3)},
			want:    []string{"a@3=w-a"},
		},
		{
			name:    "a delete and a purge marker the watch delivered are left out without a read",
			listed:  []string{"deleted", "purged"},
			watched: []watchedEntry{{key: "deleted", revision: 4, op: jetstream.KeyValueDelete}, {key: "purged", revision: 5, op: jetstream.KeyValuePurge}},
		},
		{
			name:   "a listed key the watch did not deliver is read, a put answered and a key not found left out",
			listed: []string{"read", "gone"},
			reads:  map[string]error{"read": nil, "gone": nats.ErrKeyNotFound},
			want:   []string{"read@100=r-read"},
			read:   []string{"read", "gone"},
		},
		{
			name:    "another error of a read ends the read with that error",
			listed:  []string{"a", "broken", "after"},
			watched: []watchedEntry{put("a", 3)},
			reads:   map[string]error{"broken": errRead, "after": nil},
			wantErr: errRead,
			read:    []string{"broken"},
		},
		{
			name:    "the answer keeps the list's order, whichever way each key was answered",
			listed:  []string{"d", "b", "c", "a"},
			watched: []watchedEntry{put("a", 7), put("c", 8)},
			reads:   map[string]error{"b": nil, "d": nil},
			want:    []string{"d@100=r-d", "b@100=r-b", "c@8=w-c", "a@7=w-a"},
			read:    []string{"d", "b"},
		},
		{
			name:    "a key the watch delivered that the list does not name is not answered",
			listed:  []string{"a"},
			watched: []watchedEntry{put("a", 3), put("later", 9)},
			want:    []string{"a@3=w-a"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			watched := make(map[string]jetstream.KeyValueEntry, len(row.watched))
			for _, entry := range row.watched {
				watched[entry.key] = entry
			}
			var read []string
			entries, err := heldInListOrder(row.listed, watched, func(key string) (nats.KeyValueEntry, error) {
				read = append(read, key)
				failed, scripted := row.reads[key]
				if !scripted {
					return nil, fmt.Errorf("no read of %s is scripted", key)
				}
				if failed != nil {
					return nil, failed
				}
				return leaderEntry{key: key, value: []byte("r-" + key), revision: 100, op: nats.KeyValuePut}, nil
			})
			if !slices.Equal(read, row.read) {
				t.Errorf("heldInListOrder read %v, want %v: only the listed keys the watch left out", read, row.read)
			}
			if row.wantErr != nil {
				if !errors.Is(err, row.wantErr) || entries != nil {
					t.Fatalf("heldInListOrder = %v, %v; want no entries and %v", entries, err, row.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("heldInListOrder: %v", err)
			}
			var got []string
			for _, entry := range entries {
				if entry.Operation() != nats.KeyValuePut {
					t.Errorf("heldInListOrder answered %s as %v, want a put", entry.Key(), entry.Operation())
				}
				got = append(got, fmt.Sprintf("%s@%d=%s", entry.Key(), entry.Revision(), entry.Value()))
			}
			if !slices.Equal(got, row.want) {
				t.Fatalf("heldInListOrder = %v, want %v", got, row.want)
			}
		})
	}
}

// watchedEntry is an entry a watch delivers, as heldInListOrder takes it: a key, its value, the
// revision it is stored at and whether it is a put or a marker.
type watchedEntry struct {
	key      string
	value    string
	revision uint64
	op       jetstream.KeyValueOp
}

func (e watchedEntry) Bucket() string                  { return "held" }
func (e watchedEntry) Key() string                     { return e.key }
func (e watchedEntry) Value() []byte                   { return []byte(e.value) }
func (e watchedEntry) Revision() uint64                { return e.revision }
func (e watchedEntry) Created() time.Time              { return time.Time{} }
func (e watchedEntry) Delta() uint64                   { return 0 }
func (e watchedEntry) Operation() jetstream.KeyValueOp { return e.op }

// captureBucket and captureKeys are the bucket TestCaptureStreamInfoAnswers creates and the keys it
// puts in it before it captures the two STREAM.INFO answers TestStoredKeysTakesThePageOfTheLeaderAlone
// reads.
const captureBucket = "stored-keys-capture"

var captureKeys = []string{"a", "b", "c"}

// The leader rule of StoredKeys (current) read on what a real cluster answers: the two STREAM.INFO
// answers in testdata, captured from a three-member cluster of the module's nats-server by
// TestCaptureStreamInfoAnswers (kv_stored_keys_capture_test.go), run by hand with
//
//	go test -tags capture -run '^TestCaptureStreamInfoAnswers$' ./internal/bus/
//
// The leader's answer, sent through another member, names its server the leader and lists the
// other two as peers; it is taken. The answer of the member left running once the other two
// stopped names no leader and lists the same keys from a copy that has stored as far; it is
// refused (ErrKeyListBehind), however whole its copy is.
func TestStoredKeysTakesThePageOfTheLeaderAlone(t *testing.T) {
	t.Parallel()
	read := func(name string) subjectsPage {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read the captured answer: %v", err)
		}
		page, err := decodeSubjectsPage(data)
		if err != nil {
			t.Fatalf("decode the captured answer %s: %v", name, err)
		}
		return page
	}
	onePage := func(page subjectsPage) func(int) (subjectsPage, error) {
		return func(offset int) (subjectsPage, error) {
			if offset != 0 {
				return subjectsPage{}, fmt.Errorf("asked a page at offset %d of a one-page list", offset)
			}
			return page, nil
		}
	}
	leader, member := read("stream-info-leader.json"), read("stream-info-member.json")
	leaderSubjects := slices.Sorted(maps.Keys(leader.State.Subjects))
	memberSubjects := slices.Sorted(maps.Keys(member.State.Subjects))
	if !slices.Equal(memberSubjects, leaderSubjects) || member.State.LastSeq != leader.State.LastSeq {
		t.Fatalf("the captured member lists %v through %d, want the leader's %v through %d: the capture shows no member as whole as the leader",
			memberSubjects, member.State.LastSeq, leaderSubjects, leader.State.LastSeq)
	}
	prefix, through := subjectsPrefix(captureBucket), leader.State.LastSeq

	if err := current(leader, through); err != nil {
		t.Fatalf("current(the leader's answer, naming %s the leader) = %v, want it taken", leader.Cluster.Leader, err)
	}
	keys, err := storedKeys(prefix, through, onePage(leader))
	slices.Sort(keys)
	if err != nil || !slices.Equal(keys, captureKeys) {
		t.Fatalf("storedKeys of the leader's answer = %v, %v; want %v", keys, err, captureKeys)
	}

	if err := current(member, through); !errors.Is(err, ErrKeyListBehind) {
		t.Fatalf("current(the leaderless member's answer, naming leader %q) = %v, want %v", member.Cluster.Leader, err, ErrKeyListBehind)
	}
	if keys, err := storedKeys(prefix, through, onePage(member)); !errors.Is(err, ErrKeyListBehind) {
		t.Fatalf("storedKeys of the leaderless member's answer = %v, %v; want %v", keys, err, ErrKeyListBehind)
	}
}

// A list of more subjects than one STREAM.INFO answer holds comes in pages, each read from the
// store as it is when asked, so the store can change between them. The list holds every key the
// store holds from its first ask to its last whatever changed between them: a key replaced, which
// moves no subject, a key added before the page boundary, which repeats a subject, and a key
// removed before the boundary or the last key taken itself, which a page asked at the count of
// subjects taken so far would skip a subject past. It lists no key the store never held. Each store
// here pages two subjects at a time, as a server pages 100,000, and is changed before the ask a row
// names. The rows that read the answers' leader, sequence or size are scripted answers, as the
// server's JSON.
func TestStoredKeysListsEveryKeyTheStoreHoldsWhateverChangesBetweenItsPages(t *testing.T) {
	t.Parallel()
	const prefix = "$KV.b."
	for _, row := range []struct {
		name string
		// before changes the store before the ask of its index, counted from 0.
		before map[int]func(s *pagedStore)
		// must is the keys the store holds at every ask: the list holds them all, and nothing
		// the store never held.
		must []string
	}{
		{name: "a store that does not change", must: []string{"a", "b", "c", "d", "e"}},
		{
			name:   "a key replaced between pages",
			before: map[int]func(s *pagedStore){1: func(s *pagedStore) { s.replace("a") }, 2: func(s *pagedStore) { s.replace("d") }},
			must:   []string{"a", "b", "c", "d", "e"},
		},
		{
			name:   "keys added before the page boundary",
			before: map[int]func(s *pagedStore){1: func(s *pagedStore) { s.put("0a"); s.put("0b") }},
			must:   []string{"a", "b", "c", "d", "e"},
		},
		{
			name:   "a key before the page boundary removed",
			before: map[int]func(s *pagedStore){1: func(s *pagedStore) { s.remove("a") }},
			must:   []string{"b", "c", "d", "e"},
		},
		{
			name:   "the last key taken removed",
			before: map[int]func(s *pagedStore){1: func(s *pagedStore) { s.remove("b") }},
			must:   []string{"a", "c", "d", "e"},
		},
		{
			// A key before the boundary expires and a later one is stored in the same interval,
			// so the subject at the next count is one past the key it shifted.
			name:   "a key removed before the boundary and one added after it",
			before: map[int]func(s *pagedStore){1: func(s *pagedStore) { s.remove("a"); s.put("f") }},
			must:   []string{"b", "c", "d", "e"},
		},
		{
			name:   "every key at or before the boundary removed",
			before: map[int]func(s *pagedStore){1: func(s *pagedStore) { s.remove("a"); s.remove("b") }},
			must:   []string{"c", "d", "e"},
		},
		{
			name:   "the store ends before the page asked",
			before: map[int]func(s *pagedStore){1: func(s *pagedStore) { s.remove("c"); s.remove("d"); s.remove("e") }},
			must:   []string{"a", "b"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			store := newPagedStore(t, prefix, 2, "a", "b", "c", "d", "e")
			store.before = row.before
			keys, err := storedKeys(prefix, 3, store.read)
			if err != nil {
				t.Fatalf("storedKeys = %v, %v; want every key the store holds throughout", keys, err)
			}
			for _, key := range row.must {
				if !slices.Contains(keys, key) {
					t.Fatalf("storedKeys = %v after %d asks, want %s, which the store holds at every ask", keys, store.asks, key)
				}
			}
			for _, key := range keys {
				if !store.ever[key] {
					t.Fatalf("storedKeys = %v, listing %s, which the store never held as a key", keys, key)
				}
			}
			if len(keys) != len(slices.Compact(slices.Sorted(slices.Values(keys)))) {
				t.Fatalf("storedKeys = %v, listing a key twice", keys)
			}
		})
	}

	// The rows below are one scripted answer each.
	pages := func(t *testing.T, byOffset map[int]answer) func(int) (subjectsPage, error) {
		return func(offset int) (subjectsPage, error) {
			scripted, ok := byOffset[offset]
			if !ok {
				return subjectsPage{}, fmt.Errorf("no page scripted at offset %d", offset)
			}
			return scripted.page(t, prefix), nil
		}
	}
	for _, row := range []struct {
		name  string
		pages map[int]answer
		want  []string
		fails error
	}{
		{
			// A server that runs no cluster names itself the leader and no peer
			// (jetstream_cluster.go:14013-14017).
			name:  "one page",
			pages: map[int]answer{0: {total: 2, first: 1, last: 3, msgs: 3, subjects: []string{"a", "b"}}},
			want:  []string{"a", "b"},
		},
		{
			name:  "a page short of the end that lists one subject",
			pages: map[int]answer{0: {total: 3, first: 1, last: 3, msgs: 3, subjects: []string{"a"}}},
			fails: ErrIncompleteKeyList,
		},
		{
			name:  "a page from a copy behind the sequence asked for",
			pages: map[int]answer{0: {total: 1, first: 1, last: 2, msgs: 2, subjects: []string{"a"}}},
			fails: ErrKeyListBehind,
		},
		{
			// A member answers while the stream has no leader (jetstream_api.go:2100-2104) and
			// names none (jetstream_cluster.go:14075, 14081-14083), with every peer, itself too.
			name:  "a page from a member while the stream has no leader",
			pages: map[int]answer{0: {total: 1, first: 1, last: 3, msgs: 3, subjects: []string{"a"}, leaderless: true, others: []string{"n1", "n2", "n3"}}},
			fails: ErrKeyListBehind,
		},
		{
			// The preferred member of a scale-up answers while n1 leads
			// (jetstream_api.go:2112-2133): it names n1 the leader, among the peers other than
			// itself.
			name:  "a page from a member while another server leads",
			pages: map[int]answer{0: {total: 1, first: 1, last: 3, msgs: 3, subjects: []string{"a"}, leader: "n1", others: []string{"n1", "n3"}}},
			fails: ErrKeyListBehind,
		},
		{
			// The leader of a clustered stream names itself, and every other peer but itself.
			name:  "a page from the leader of a clustered stream",
			pages: map[int]answer{0: {total: 1, first: 1, last: 3, msgs: 3, subjects: []string{"a"}, leader: "n2", others: []string{"n1", "n3"}}},
			want:  []string{"a"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			keys, err := storedKeys(prefix, 3, pages(t, row.pages))
			if row.fails != nil {
				if !errors.Is(err, row.fails) {
					t.Fatalf("storedKeys = %v, %v; want %v", keys, err, row.fails)
				}
				return
			}
			slices.Sort(keys)
			if err != nil || !slices.Equal(keys, row.want) {
				t.Fatalf("storedKeys = %v, %v; want %v", keys, err, row.want)
			}
		})
	}
	outside := map[int]answer{0: {total: 1, first: 1, last: 1, msgs: 1}}
	outsidePage := func(offset int) (subjectsPage, error) {
		page, err := pages(t, outside)(offset)
		page.State.Subjects = map[string]uint64{"$KV.other.a": 1}
		return page, err
	}
	if _, err := storedKeys(prefix, 0, outsidePage); err == nil || !strings.Contains(err.Error(), "outside "+prefix) {
		t.Fatalf("storedKeys of a subject outside the filter = %v, want it refused", err)
	}
}

// pagedStore is a bucket's store as STREAM.INFO pages it (jetstream_api.go:2207-2243): its
// subjects sorted at each ask, pageSize of them from the asked offset, with the total and the
// stream's last sequence of that moment. before changes it ahead of the ask of its index.
type pagedStore struct {
	t        *testing.T
	prefix   string
	pageSize int
	keys     map[string]bool
	// ever is every key the store has held.
	ever    map[string]bool
	lastSeq uint64
	asks    int
	before  map[int]func(s *pagedStore)
}

func newPagedStore(t *testing.T, prefix string, pageSize int, keys ...string) *pagedStore {
	s := &pagedStore{t: t, prefix: prefix, pageSize: pageSize, keys: map[string]bool{}, ever: map[string]bool{}, lastSeq: 3}
	for _, key := range keys {
		s.put(key)
	}
	return s
}

// put stores a record of key, a new subject if the store holds none.
func (s *pagedStore) put(key string) {
	s.keys[key], s.ever[key] = true, true
	s.lastSeq++
}

// replace stores a new record of key in place of its last: the subjects stay, the sequence moves.
func (s *pagedStore) replace(key string) { s.lastSeq++ }

// remove drops key's last message, as its MaxAge or a purge of its subject does.
func (s *pagedStore) remove(key string) { delete(s.keys, key) }

func (s *pagedStore) read(offset int) (subjectsPage, error) {
	if change := s.before[s.asks]; change != nil {
		change(s)
	}
	s.asks++
	if s.asks > 100 {
		s.t.Fatalf("the list asked the store more than 100 times")
	}
	sorted := slices.Sorted(maps.Keys(s.keys))
	start := min(offset, len(sorted))
	end := min(start+s.pageSize, len(sorted))
	return answer{total: len(sorted), first: 1, last: s.lastSeq, msgs: uint64(len(sorted)), subjects: sorted[start:end]}.page(s.t, s.prefix), nil
}

// answer is one scripted STREAM.INFO answer with its subject list: the total the server counts,
// the stream's first and last sequence and message count, this page's subjects under the prefix it
// is rendered with, and the cluster the answering copy names: the server it names the stream's
// leader (s1, the one server of an unclustered deployment, unless leader is set; none when
// leaderless) and the stream's other peers it lists (others).
type answer struct {
	total             int
	first, last, msgs uint64
	subjects          []string
	leader            string
	leaderless        bool
	others            []string
}

// page is a's JSON as nats-server v2.15.1 sends it (jetstream_api.go:503-507), decoded as
// StoredKeys decodes an answer.
func (a answer) page(t *testing.T, prefix string) subjectsPage {
	t.Helper()
	subjects := map[string]uint64{}
	for _, subject := range a.subjects {
		subjects[prefix+subject] = 1
	}
	cluster := map[string]any{"name": "c"}
	switch {
	case a.leaderless:
	case a.leader != "":
		cluster["leader"] = a.leader
	default:
		cluster["leader"] = "s1"
	}
	var peers []map[string]any
	for _, name := range a.others {
		peers = append(peers, map[string]any{"name": name, "current": true, "active": 1000000})
	}
	if peers != nil {
		cluster["replicas"] = peers
	}
	data, err := json.Marshal(map[string]any{
		"type":    "io.nats.jetstream.api.v1.stream_info_response",
		"total":   a.total,
		"state":   map[string]any{"first_seq": a.first, "last_seq": a.last, "messages": a.msgs, "subjects": subjects},
		"cluster": cluster,
	})
	if err != nil {
		t.Fatalf("render the scripted answer: %v", err)
	}
	page, err := decodeSubjectsPage(data)
	if err != nil {
		t.Fatalf("decode the scripted answer: %v", err)
	}
	return page
}
