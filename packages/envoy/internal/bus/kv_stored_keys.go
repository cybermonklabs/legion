// The list of every key a KV bucket's stream holds a message on (KeyValue.StoredKeys), beside its
// test: what a kvwatch read compares its copy against, so a key the bucket holds and the read's
// consumer has not delivered is read before the read completes.

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

// StoredKeys lists every key the bucket's stream holds a message on, from a copy of the stream
// that has stored at least sequence through: the stream's STREAM.INFO, asked for its subjects under
// `$KV.<bucket>.>` (subjects_filter), page by page. The read barrier's key (BarrierKey) is left
// out, since no record of the bucket is stored under it.
//
// The server counts the messages it stores on each subject from its per-subject index
// (nats-server v2.15.1 jetstream_api.go:2206-2245, filestore.go:5093-5106, memstore.go:1002-1024),
// and a delete or purge marker is a message, so a key whose newest message is a marker is listed
// like any other. A caller that wants only the keys holding a value reads each listed key it
// lacks (HeldFromLeader, which answers a marker as nats.ErrKeyNotFound).
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
// reaches past it. A key added after through, or removed after it, can be listed or not, and
// either is correct: a key added later is delivered to the read's own consumer after the barrier,
// and a key the leader no longer holds is answered as no record (HeldFromLeader). No write fails a
// list, so a bucket of any size that is written to steadily is listed in one pass. A list makes
// one request per page, and one more for each step back. A page costs each subject's length and 5
// bytes more: 3,701,041 bytes for a page of 100,000 subjects of 32 characters
// (TestStoredKeysListsABucketPastOnePageWhileItChanges).
//
// ctx bounds the whole list, every page included. A ctx carrying no deadline is bounded by
// RequestTimeout, as every other read of a bucket's stream through this handle is. The request is
// sent to the default JetStream API prefix (`$JS.API.`), as the handle's own jetstream context,
// which wrapKeyValue builds with jetstream.New, sends every other request; nats.go's own paged
// StreamInfo is not used because it asks each page at the count of subjects taken so far, so it
// skips a subject a removal shifts, and it loops on a page that answers fewer subjects than remain
// (nats.go@v1.53.1 jetstream/stream.go:457-502).
func (kv KeyValue) StoredKeys(ctx context.Context, through uint64) ([]string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
	}
	keys, err := storedKeys(kv.subjectsPrefix(), through, func(offset int) (subjectsPage, error) {
		return kv.subjectsPageAt(ctx, offset)
	})
	if err != nil {
		return nil, fmt.Errorf("list the keys of KV bucket %s from its stream: %w", kv.Bucket(), err)
	}
	return keys, nil
}

// subjectsPrefix is the prefix of every subject the bucket's stream stores a key under.
func (kv KeyValue) subjectsPrefix() string {
	return "$KV." + kv.Bucket() + "."
}

// subjectsPageAt is the bucket's stream's STREAM.INFO with its subjects from offset on, one page
// of StoredKeys' list, asked within ctx. The server's refusal of the request under the
// connection's grant ends it as soon as it arrives, an error wrapping ErrPublishDenied
// (refusedOn), rather than after ctx's own wait with a bare "context deadline exceeded": a read
// makes one list, so this costs what a watch's create costs, not what every key read would.
func (kv KeyValue) subjectsPageAt(ctx context.Context, offset int) (subjectsPage, error) {
	request, err := json.Marshal(struct {
		SubjectsFilter string `json:"subjects_filter"`
		Offset         int    `json:"offset,omitempty"`
	}{kv.subjectsPrefix() + ">", offset})
	if err != nil {
		return subjectsPage{}, err
	}
	subject := jsAPI("STREAM.INFO", "KV_"+kv.Bucket())
	answer, err := refusedOn(kv, []string{subject}, func() (*nats.Msg, error) {
		return kv.streams.Conn().RequestWithContext(ctx, subject, request)
	})
	if err != nil {
		return subjectsPage{}, err
	}
	var page subjectsPage
	if err := json.Unmarshal(answer.Data, &page); err != nil {
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
			if _, again := listed[key]; again || key == BarrierKey {
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
