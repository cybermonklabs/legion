package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/testnats"
)

// StoredKeys lists every key the bucket's stream holds a message on: a key holding a value, one
// past the length Get refuses (testnats.LegacyKeys' unreadable key), and a key whose newest message
// is a delete or a purge marker, which a caller then reads (HeldFromLeader). The read barrier's key
// is left out. A list asked through a sequence the stream has not stored is refused, since the copy
// that answered could lack a key that held a record there.
func TestStoredKeysListsEveryKeyTheStreamHoldsAMessageOn(t *testing.T) {
	const bucket = "kv_stored_keys"
	_, kv := leaderReadBucket(t, bucket)
	_, legacy := testnats.LegacyKeys(bucket, "u")
	if _, err := kv.Legacy().Put(legacy, []byte("1")); err != nil {
		t.Fatalf("put the %d-byte legacy key: %v", len(legacy), err)
	}
	for _, key := range []string{"held", "deleted", "purged"} {
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
	barrier, err := kv.WriteBarrier(context.Background())
	if err != nil {
		t.Fatalf("write the barrier: %v", err)
	}

	keys, err := kv.StoredKeys(context.Background(), barrier)
	if err != nil {
		t.Fatalf("StoredKeys through the barrier at %d: %v", barrier, err)
	}
	slices.Sort(keys)
	want := []string{"deleted", "held", legacy, "purged"}
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Fatalf("StoredKeys = %v, want %v: every key with a message, markers included, the barrier's left out", keys, want)
	}
	if _, err := kv.StoredKeys(context.Background(), barrier+1); !errors.Is(err, ErrKeyListBehind) {
		t.Fatalf("StoredKeys through %d, past the stream's last sequence %d = %v, want %v", barrier+1, barrier, err, ErrKeyListBehind)
	}
}

// A bucket of more keys than one STREAM.INFO answer holds, 100,000 (JSMaxSubjectDetails), is
// listed whole by a real server, and not only when it holds still. The known-session bucket is
// sized for about 100,000 records over its 30 days, and its live sessions rewrite their records
// all the time, so its list pages while it is written to. Here one writer replaces a key through
// the whole list, so the stream's last sequence moves between every two pages, and then the
// subject of the first key is purged between the first page and the second, which shifts every
// later subject back by one and would carry a page asked at the count of subjects taken so far
// one subject past the boundary.
func TestStoredKeysListsABucketPastOnePageWhileItChanges(t *testing.T) {
	const bucket = "kv_stored_keys_pages"
	const count = 100_050
	_, kv := leaderReadBucket(t, bucket)
	js := kv.Streams()
	key := func(i int) string { return fmt.Sprintf("k%06d", i) }
	for i := range count {
		if _, err := js.PublishAsync(kv.subjectsPrefix()+key(i), []byte("1")); err != nil {
			t.Fatalf("store key %d: %v", i, err)
		}
		if i%2000 == 1999 || i == count-1 {
			select {
			case <-js.PublishAsyncComplete():
			case <-time.After(30 * time.Second):
				t.Fatalf("the stores up to key %d were not acknowledged within 30 s", i)
			}
		}
	}
	barrier, err := kv.WriteBarrier(context.Background())
	if err != nil {
		t.Fatalf("write the barrier: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if answer, err := kv.streams.Conn().RequestWithContext(ctx, "$JS.API.STREAM.INFO.KV_"+bucket, []byte(`{"subjects_filter":"`+kv.subjectsPrefix()+`>"}`)); err == nil {
		t.Logf("the first page of %d subjects of %d characters is %d bytes", count+1, len(kv.subjectsPrefix()+key(0)), len(answer.Data))
	}
	holdsEvery := func(t *testing.T, keys []string, from int) {
		t.Helper()
		listed := map[string]bool{}
		for _, k := range keys {
			listed[k] = true
		}
		for i := from; i < count; i++ {
			if !listed[key(i)] {
				t.Fatalf("StoredKeys listed %d keys and not %s, which the bucket holds throughout", len(keys), key(i))
			}
		}
	}

	t.Run("while a key is replaced between pages", func(t *testing.T) {
		stop := make(chan struct{})
		var writer sync.WaitGroup
		writes := 0
		writer.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := kv.Put(key(count-1), []byte("2")); err != nil {
					t.Errorf("replace %s: %v", key(count-1), err)
					return
				}
				writes++
			}
		})
		keys, err := kv.StoredKeys(ctx, barrier)
		close(stop)
		writer.Wait()
		if err != nil {
			t.Fatalf("StoredKeys of %d keys while one is replaced (%d replacements) = %v", count, writes, err)
		}
		if writes == 0 {
			t.Fatal("no replacement was stored while the list ran, so the row shows nothing")
		}
		holdsEvery(t, keys, 0)
	})

	t.Run("while a key before the page boundary is removed between pages", func(t *testing.T) {
		asks := 0
		keys, err := storedKeys(kv.subjectsPrefix(), barrier, func(offset int) (subjectsPage, error) {
			if asks == 1 {
				stream, err := js.Stream(ctx, "KV_"+bucket)
				if err != nil {
					return subjectsPage{}, err
				}
				if err := stream.Purge(ctx, jetstream.WithPurgeSubject(kv.subjectsPrefix()+key(0))); err != nil {
					return subjectsPage{}, fmt.Errorf("purge %s: %w", key(0), err)
				}
			}
			asks++
			return kv.subjectsPageAt(ctx, offset)
		})
		if err != nil {
			t.Fatalf("StoredKeys of %d keys with the first one removed after the first page = %v", count, err)
		}
		holdsEvery(t, keys, 1)
		if asks < 3 {
			t.Fatalf("the list asked %d times, want a step back past the removed subject", asks)
		}
	})
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
			store := newPagedStore(t, prefix, 2, "a", "b", "c", "d", "e", BarrierKey)
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
				if !store.ever[key] || key == BarrierKey {
					t.Fatalf("storedKeys = %v, listing %s, which the store never held as a key", keys, key)
				}
			}
			if len(keys) != len(slices.Compact(slices.Sorted(slices.Values(keys)))) {
				t.Fatalf("storedKeys = %v, listing a key twice", keys)
			}
		})
	}

	// The rows below are one scripted answer each.
	pages := func(byOffset map[int]answer) func(int) (subjectsPage, error) {
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
			name:  "one page",
			pages: map[int]answer{0: {total: 2, first: 1, last: 3, msgs: 3, subjects: []string{"a", BarrierKey}}},
			want:  []string{"a"},
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
			keys, err := storedKeys(prefix, 3, pages(row.pages))
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
		page, err := pages(outside)(offset)
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
	var page subjectsPage
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatalf("decode the scripted answer: %v", err)
	}
	return page
}
