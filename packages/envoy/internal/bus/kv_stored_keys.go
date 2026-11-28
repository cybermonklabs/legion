// The one listing of a KV bucket's keys, beside its test: every key the bucket's stream holds a
// message on (KeyValue.StoredKeys), and the value of each such key that holds one, taken from one
// watch of the keys and read from the stream's leader for a key the watch left out
// (KeyValue.HeldEntries). A key another client writes again while the list is read is still
// listed, where nats.go's Keys and the jetstream watcher's first delivery can leave it out, so a
// read that must see the whole bucket reads through here.

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrIncompleteKeyList is a page of a bucket's keys that cannot be a page of one sorted list: a
// page short of the end that lists fewer than two subjects, so the next page could not start past
// it (StoredKeys).
var ErrIncompleteKeyList = errors.New("the bucket's key list did not add up to one list")

// ErrKeyListBehind is a list of a bucket's keys from a copy of its stream that may lack a key the
// bucket holds: one that is not the stream's leader, or one that had not yet stored the sequence
// the list was asked through (StoredKeys).
var ErrKeyListBehind = errors.New("the bucket's key list came from a copy of its stream that is not its leader, or is behind the sequence asked for")

// subjectsPage is one STREAM.INFO answer with its subject list (nats-server v2.15.1
// jetstream_api.go:503-507): the page's place in the whole list, the stream's state with that page
// of its subjects, and the cluster the answering copy names, with its leader.
type subjectsPage struct {
	Error   *jetstream.APIError    `json:"error,omitempty"`
	Total   int                    `json:"total"`
	State   *jetstream.StreamState `json:"state,omitempty"`
	Cluster *jetstream.ClusterInfo `json:"cluster,omitempty"`
}

// HeldEntries reads every key under the KV key pattern keys that holds a value, with that value
// and its revision, in the order the keys are listed. It reads the stream's state
// (ReadStreamState), which it answers, and lists the keys the stream holds a message on through
// that state's last sequence (StoredKeys), a list that names each key whose newest message is a
// delete or a purge marker too. It then opens one watch of the same pattern (WatchKeys) that
// delivers the markers as well as the values, and reads its first delivery up to the nil entry
// that ends it, keeping the last entry delivered for each key (newestWatched). The watch is opened
// after the list is read, so each entry it delivers is the message its key held at that state's
// last sequence or one written since.
//
// Each listed key is answered from the watch where it can be, in the list's order
// (heldInListOrder). A key the watch delivered as a put is answered with that entry's value and
// revision, and one it delivered as a marker is left out, neither with a read of its own. A key
// the watch did not deliver is read from the stream's leader (GetFromLeader), which answers the
// value the leader holds when it is read, and a key that read answers nats.ErrKeyNotFound for, one
// whose newest message is a marker or one the leader no longer holds, is left out. A key the watch
// delivered that the list does not name, one first written after the list's sequence, is not
// answered: the list is the bucket as of that sequence (StoredKeys).
//
// The values come from the watch because the list names every key whose newest message is a
// marker, and a read of each listed key from the leader costs a round trip per marker.
// cistore.Writer.VerifyRequests lists the verify requests through here every five seconds within
// RecordWait, 4 s, while every request answered in the bucket's last seven days still leaves its
// marker. With 25,000 such markers one watch listed the live requests in 0.3 s, where a read of
// each listed key from the leader, sixteen at a time, took 1.8 to 1.95 s at 10,000 and did not end
// within RecordWait at 20,000 (dispatch://INFRA-6356/comment/294ab3e8-7eec-47ef-ac08-a5a3673d09d1).
// So a key whose newest message is a marker costs no read of its own, however many the bucket
// holds, and the leader is read only for the keys the watch left out, one after another.
//
// The keys come from the list and not from the watch, for a read that must see every key the
// bucket holds. A key another client writes again while the read runs is still listed here, where
// nats.go's Keys and the jetstream watcher's first delivery can leave it out: each reads a
// consumer that delivers the newest message each key held when the consumer was created, a key
// written again while that runs can lose that message before it is delivered, and its new one can
// then come after the first delivery has ended. A probe left such a key out of 1,958 of 16,451
// reads of Keys and 1,438 of 14,231 of the watcher, and the stream's subject list named every key
// in 41,352 of 41,352 (INFRA-6356). Such a key is listed, left out of the watch's first delivery,
// and read from the leader. The witness of cireconcile's tests and cistore.Writer.VerifyRequests
// read the bucket through it.
//
// The answer holds every listed key holding a value, or it is an error. A watch whose updates
// close before its nil entry, because ctx ended or its subscription did, ends the read with an
// error naming the bucket. A key the watch left out that this build cannot read (RefusesRead) ends
// the read with GetFromLeader's refusal, as any error of that read but nats.ErrKeyNotFound does. A
// list that names no key answers no entry and opens no watch.
//
// ctx bounds the whole read, the stream's state, every page of the list, the watch and every key's
// read included. A ctx carrying no deadline is bounded by RequestTimeout, as GetFromLeader is.
func (kv KeyValue) HeldEntries(ctx context.Context, keys string) (StreamState, []nats.KeyValueEntry, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
	}
	state, err := kv.ReadStreamState(ctx)
	if err != nil {
		return StreamState{}, nil, err
	}
	listed, err := kv.StoredKeys(ctx, keys, state.LastSeq)
	if err != nil {
		return StreamState{}, nil, err
	}
	if len(listed) == 0 {
		return state, nil, nil
	}
	watched, err := kv.newestWatched(ctx, keys)
	if err != nil {
		return StreamState{}, nil, err
	}
	entries, err := heldInListOrder(listed, watched, func(key string) (nats.KeyValueEntry, error) {
		return kv.GetFromLeader(ctx, key)
	})
	if err != nil {
		return StreamState{}, nil, err
	}
	return state, entries, nil
}

// newestWatched is the last entry one watch of the KV key pattern keys delivers for each key in
// its first delivery, delete and purge markers included, for HeldEntries: the watch (WatchKeys,
// with no IgnoreDeletes) delivers the newest message each key holds when its consumer is created,
// and nats.go ends that delivery with a nil entry. Once the nil entry comes, the watch is stopped
// and read until it closes (DrainWatcher), as ReadHistory leaves its own. A watch whose updates
// close before the nil entry, because ctx ended or its subscription did, is an error naming the
// bucket, as ReadHistory answers one, rather than a delivery that may lack keys HeldEntries would
// then read from the leader one by one.
func (kv KeyValue) newestWatched(ctx context.Context, keys string) (map[string]jetstream.KeyValueEntry, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcher, err := kv.WatchKeys(ctx, keys)
	if err != nil {
		return nil, err
	}
	watched := make(map[string]jetstream.KeyValueEntry)
	for entry := range watcher.Updates() {
		if entry != nil {
			watched[entry.Key()] = entry
			continue
		}
		_ = watcher.Stop()
		go DrainWatcher(watcher)
		return watched, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("watch the keys %s of KV bucket %s: %w", keys, kv.Bucket(), err)
	}
	return nil, fmt.Errorf("watch the keys %s of KV bucket %s: the watch ended after %d keys, before it had delivered them all", keys, kv.Bucket(), len(watched))
}

// watchedPut is a put the watch of HeldEntries delivered, as the nats.KeyValueEntry HeldEntries
// answers: the two interfaces differ only in the package of the operation's type, and only a put
// is answered (heldInListOrder).
type watchedPut struct {
	jetstream.KeyValueEntry
}

func (e watchedPut) Operation() nats.KeyValueOp { return nats.KeyValuePut }

// heldInListOrder is HeldEntries' answer from listed, the keys StoredKeys listed, and watched, the
// last entry the watch delivered for each key it delivered (newestWatched): each listed key that
// holds a value, in the list's order. A put the watch delivered is taken from it and a marker it
// delivered is left out, neither read; a key it did not deliver is read (read, GetFromLeader for
// HeldEntries) and left out where that read answers nats.ErrKeyNotFound, and any other error of a
// read ends the answer with that error. A key watched holds that listed does not name is not
// answered.
func heldInListOrder(listed []string, watched map[string]jetstream.KeyValueEntry, read func(key string) (nats.KeyValueEntry, error)) ([]nats.KeyValueEntry, error) {
	var entries []nats.KeyValueEntry
	for _, key := range listed {
		if entry, ok := watched[key]; ok {
			if entry.Operation() == jetstream.KeyValuePut {
				entries = append(entries, watchedPut{entry})
			}
			continue
		}
		entry, err := read(key)
		if errors.Is(err, nats.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// StoredKeys lists every key under the KV key pattern keys (">" for the whole bucket, or a pattern
// naming part of it, such as "verify.*.*.*.*") that the bucket's stream holds a message on, from a
// copy of the stream that has stored at least sequence through: the stream's STREAM.INFO, asked
// for its subjects under `$KV.<bucket>.<keys>` (subjects_filter), page by page.
//
// The server counts the messages it stores on each subject from its per-subject index
// (nats-server v2.15.1 jetstream_api.go:2206-2245, filestore.go:5093-5106, memstore.go:1002-1024),
// and a delete or purge marker is a message, so a key whose newest message is a marker is listed
// like any other. A caller that wants only the keys holding a value reads them through
// HeldEntries, which takes each listed key's value from one watch that delivers the markers too
// and reads from the stream's leader only a listed key the watch left out (GetFromLeader, which
// answers a marker as nats.ErrKeyNotFound). A key written again while the list is read is listed
// too: the store counts the new message on the key's subject before it removes the one that
// message replaces (memstore.go:322-331, filestore.go:5427-5485), so no read of the index finds
// the key without a message.
//
// Which copy of the stream answers is nats-server's: its leader, but also one of its members while
// the stream has no leader, or a new member while a stream of one replica scales up
// (jetstream_api.go:2100-2134). A member can have stored through and still lack a key the leader
// holds: its older record expired or a snapshot it installed deleted it, and the record that
// replaced it has not reached the member yet. So a page is taken only from the leader. Each
// answer names the server its copy holds the stream's leader (`cluster.leader`,
// jetstream_cluster.go:14075) and every other peer of the stream but the server answering
// (:14068-14121), so the answering server is the leader exactly when it names a leader and no
// other peer of that name. A member with no leader names none (:14075, 14081-14083); the
// preferred member of a scale-up, which answers while another server leads
// (jetstream_api.go:2112-2133), names that leader among the other peers; and a server that runs no
// cluster names itself and no peer (jetstream_cluster.go:14013-14017). A page from any copy but
// the leader, or from one whose last stored sequence is below through, is refused
// (ErrKeyListBehind): the leader stored every write it acknowledged, through included, so it holds
// a message on every key that held a record at through, whether that record or a later one.
//
// A list of more than JSMaxSubjectDetails subjects, 100,000 (jetstream_api.go:494-495), comes in
// pages: each answer sorts the subjects the store holds when it is asked and returns the ones from
// the asked offset on (jetstream_api.go:2207-2243), all of them from one read of the per-subject
// index, taken after the state whose last sequence is checked against through. Pages asked at
// different times read different stores, and a store that changed between them shifts the offsets.
// A write that only replaces a key's record, which is almost every write to these buckets, moves
// no subject and so no offset. A new key shifts the later offsets forward, so the next page repeats
// a subject, which is taken once. A key whose last message is removed (its MaxAge passed, or its
// subject was purged) shifts them back, so the next page would start past a subject no page
// listed. So each page after the first is asked at the offset of the last subject taken, and
// overlaps the one before by that subject. A page whose first subject sorts at or before it
// continues the list from it, since its subjects are contiguous in its own store. A page that
// starts past it, or holds nothing, is asked again further back, one subject and then twice as far
// each time, until it starts at or before it or is asked at offset 0, which starts at the store's
// first subject. The list ends at a page that reaches the end of its own store.
//
// That list holds every key that held a record at through and still holds one, whose subject is in
// every store from through on: each range of subjects is read from the store of the page that
// reaches past it. A key added after through, or removed after it, can be listed or not: the list
// is the bucket as of through, so HeldEntries answers no key the list does not name, and leaves
// out a listed key its watch does not deliver and the leader no longer holds (GetFromLeader
// answers it as no record). No write fails a list, so a bucket of any size that is written to
// steadily is listed in one pass. A list makes one request per page, and one more for each step
// back. A page costs each subject's length and 5 bytes more: 3,701,041 bytes for a page of
// 100,000 subjects of 32 characters, measured against nats-server v2.15.1 in pull request #5181.
//
// ctx bounds the whole list, every page included. A ctx carrying no deadline is bounded by
// RequestTimeout, as every other read of a bucket's stream through this handle is. The request is
// sent to the default JetStream API prefix (`$JS.API.`), as the handle's own jetstream context,
// which wrapKeyValue builds with jetstream.New, sends every other request; nats.go's own paged
// StreamInfo is not used because it asks each page at the count of subjects taken so far, so it
// skips a subject a removal shifts, and it loops on a page that answers fewer subjects than remain
// (nats.go@v1.55.0 jetstream/stream.go:458-503).
func (kv KeyValue) StoredKeys(ctx context.Context, keys string, through uint64) ([]string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
	}
	listed, err := storedKeys(subjectsPrefix(kv.Bucket()), through, func(offset int) (subjectsPage, error) {
		return kv.subjectsPageAt(ctx, keys, offset)
	})
	if err != nil {
		return nil, fmt.Errorf("list the keys %s of KV bucket %s from its stream: %w", keys, kv.Bucket(), err)
	}
	return listed, nil
}

// subjectsPrefix is the prefix of every subject the stream of the bucket named bucket stores a
// key under.
func subjectsPrefix(bucket string) string {
	return "$KV." + bucket + "."
}

// subjectsRequest is the STREAM.INFO request of one page of StoredKeys' list of the keys under the
// pattern keys of the bucket named bucket, from offset on: the subject it is sent to and its body.
func subjectsRequest(bucket, keys string, offset int) (string, []byte, error) {
	body, err := json.Marshal(struct {
		SubjectsFilter string `json:"subjects_filter"`
		Offset         int    `json:"offset,omitempty"`
	}{subjectsPrefix(bucket) + keys, offset})
	if err != nil {
		return "", nil, err
	}
	return jsAPI("STREAM.INFO", "KV_"+bucket), body, nil
}

// subjectsPageAt is the bucket's stream's STREAM.INFO with its subjects under the pattern keys
// from offset on, one page of StoredKeys' list, asked within ctx. The server's refusal of the
// request under the connection's grant ends it as soon as it arrives, an error wrapping
// ErrPublishDenied (refusedOn), rather than after ctx's own wait with a bare "context deadline
// exceeded": a read makes one list, so this costs what a watch's create costs, not what every key
// read would.
func (kv KeyValue) subjectsPageAt(ctx context.Context, keys string, offset int) (subjectsPage, error) {
	subject, request, err := subjectsRequest(kv.Bucket(), keys, offset)
	if err != nil {
		return subjectsPage{}, err
	}
	answer, err := refusedOn(kv, []string{subject}, func() (*nats.Msg, error) {
		return kv.streams.Conn().RequestWithContext(ctx, subject, request)
	})
	if err != nil {
		return subjectsPage{}, err
	}
	return decodeSubjectsPage(answer.Data)
}

// decodeSubjectsPage decodes data, one STREAM.INFO answer to subjectsRequest, as a page of
// StoredKeys' list: the server's error answer is that error, nats.ErrStreamNotFound for a stream
// that is gone, and an answer with no state is refused.
func decodeSubjectsPage(data []byte) (subjectsPage, error) {
	var page subjectsPage
	if err := json.Unmarshal(data, &page); err != nil {
		return subjectsPage{}, fmt.Errorf("decode the stream's answer: %w", err)
	}
	if page.Error != nil {
		if page.Error.ErrorCode == jetstream.JSErrCodeStreamNotFound {
			return subjectsPage{}, fmt.Errorf("%w: %w", nats.ErrStreamNotFound, page.Error)
		}
		return subjectsPage{}, page.Error
	}
	if page.State == nil {
		return subjectsPage{}, errors.New("the stream's answer holds no state")
	}
	return page, nil
}

// storedKeys collects the keys under prefix that read's pages list, read from offset 0 on, as
// StoredKeys documents.
func storedKeys(prefix string, through uint64, read func(offset int) (subjectsPage, error)) ([]string, error) {
	var keys []string
	listed := map[string]struct{}{}
	// last is the greatest subject taken so far, offset the page asked next, and back how far
	// before the overlap the page is asked again when it starts past last.
	var last string
	for offset, back := 0, 1; ; {
		page, err := read(offset)
		if err != nil {
			return nil, err
		}
		if err := current(page, through); err != nil {
			return nil, err
		}
		subjects := slices.Sorted(maps.Keys(page.State.Subjects))
		if offset > 0 && (len(subjects) == 0 || subjects[0] > last) {
			// Subjects at or before last have gone since the page before, so this page may start
			// past one no page listed: ask again further back.
			offset = max(0, offset-back)
			back *= 2
			continue
		}
		for _, subject := range subjects {
			key, ok := strings.CutPrefix(subject, prefix)
			if !ok {
				return nil, fmt.Errorf("the stream listed subject %s, outside %s>", subject, prefix)
			}
			if _, again := listed[key]; again {
				continue
			}
			listed[key] = struct{}{}
			keys = append(keys, key)
		}
		if offset+len(subjects) >= page.Total {
			return keys, nil
		}
		if len(subjects) < 2 {
			return nil, fmt.Errorf("%w: a page at %d of %d subjects listed %d", ErrIncompleteKeyList, offset, page.Total, len(subjects))
		}
		last = max(last, subjects[len(subjects)-1])
		offset, back = offset+len(subjects)-1, 1
	}
}

// current refuses page when the copy of the stream that answered it may lack a key that held a
// record at sequence through (ErrKeyListBehind): a copy that is not the stream's leader, by the
// cluster it names, or one behind through, as StoredKeys documents.
func current(page subjectsPage, through uint64) error {
	if page.Cluster == nil || page.Cluster.Leader == "" {
		return fmt.Errorf("%w: the copy that answered names no leader of the stream", ErrKeyListBehind)
	}
	for _, peer := range page.Cluster.Replicas {
		if peer != nil && peer.Name == page.Cluster.Leader {
			return fmt.Errorf("%w: the copy that answered names %s, another peer, the stream's leader", ErrKeyListBehind, peer.Name)
		}
	}
	if page.State.LastSeq < through {
		return fmt.Errorf("%w: the copy that answered has stored through sequence %d, the list was asked through %d",
			ErrKeyListBehind, page.State.LastSeq, through)
	}
	return nil
}
