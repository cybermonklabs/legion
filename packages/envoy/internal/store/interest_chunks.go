package store

// A session's interest record is stored whole under the session's id while its encoding fits one
// NATS message, and in chunks a manifest under that id names once it does not (internal/kvchunk,
// which states the layout and why no reader gets a torn or mixed record). This file is where the
// registry meets that layout: the row it caches with the layout it is stored in, the read of a
// record from the bucket, its write, and the collection of the chunks no record names.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// interestRecord names the interest record in the lines internal/kvchunk logs.
const interestRecord = "interest record"

// storedRow is a row as the bucket stores it: the row, and the layout it is stored in, so a write
// that replaces it or a delete of it removes the chunks it leaves unnamed.
type storedRow struct {
	Interest
	layout kvchunk.Layout
}

// decodeStoredLocked is the interest copy's decode (kvwatch.Rows), called under r.mu: a record
// stored whole is decoded as it is, and a manifest is joined from the chunks the chunk copy holds
// (kvchunk.Chunks.Join), which the watcher delivered before it. A manifest the copy cannot join is
// a record this build cannot decode, which the copy stores no row for.
func (r *Registry) decodeStoredLocked(entry kvwatch.Entry) (storedRow, bool, error) {
	record, layout, err := r.chunks.Join(entry.Value)
	if err != nil {
		return storedRow{}, false, err
	}
	stored, err := decodeRecord(record, layout)
	return stored, err == nil, err
}

// decodeRecord decodes a record joined or read whole into the row it holds.
func decodeRecord(record []byte, layout kvchunk.Layout) (storedRow, error) {
	var item Interest
	if err := json.Unmarshal(record, &item); err != nil {
		return storedRow{}, err
	}
	return storedRow{Interest: item, layout: layout}, nil
}

// decodeEntry decodes entry, the value under sessionID that a read through kv answered, into the
// row it stores, with the revision of the entry it was read from (kvchunk.Read, which reads a
// record's chunks from the stream's leader and a manifest a rewrite replaced under it again). A
// record this build cannot decode is ErrMalformedInterest, logged at WARN naming the session, with
// the layout kvchunk.Read answered: none for a damaged chunked record (kvchunk.ErrDamaged), whose
// chunks were never proved to be its own, so the write that replaces it deletes none of them, and
// the joined layout for a record whose JSON does not decode, so that write deletes its chunks.
// nats.ErrKeyNotFound is a record deleted while it was read, and any other error a read that did
// not answer.
func (r *Registry) decodeEntry(ctx context.Context, kv bus.KeyValue, sessionID string, entry nats.KeyValueEntry) (storedRow, uint64, error) {
	record, layout, revision, err := kvchunk.Read(ctx, kv, sessionID, entry)
	if errors.Is(err, kvchunk.ErrDamaged) {
		return storedRow{layout: layout}, revision, r.malformedRecord(sessionID, revision, err)
	}
	if err != nil {
		return storedRow{}, 0, err
	}
	stored, err := decodeRecord(record, layout)
	if err != nil {
		return storedRow{layout: layout}, revision, r.malformedRecord(sessionID, revision, err)
	}
	return stored, revision, nil
}

// malformedRecord is ErrMalformedInterest for sessionID's record at revision, whose cause is err,
// logged at WARN naming the session.
func (r *Registry) malformedRecord(sessionID string, revision uint64, err error) error {
	r.logger().Warn("registry read an interest record it cannot decode",
		slog.String("session_id", sessionID),
		slog.Uint64("revision", revision),
		slog.String("error", err.Error()),
	)
	return fmt.Errorf("%w: %w", ErrMalformedInterest, err)
}

// putInterestAt writes record as sessionID's interest record only while its latest revision is
// revision, creating it when revision is 0, which names the session that had no row when its plan
// read the bucket. replaced is the layout of the record read at revision. It reports what the write
// stored (kvchunk.Written: the revision written, the layout record was stored in, and whether the
// bucket took the write) and the epoch of the handle it was written through. A bucket that refused
// the write holds another writer's row since, which is no error here (nats.ErrKeyExists, the
// wrong-last-sequence answer a Create and an Update share), and the caller plans again over that
// row. Each call waits no longer than ctx (bus.Bounded); a write ctx abandoned may still land, and
// is safe for the same reason a conditional delete is: it stores only over the very revision it
// read, so it cannot undo a write that landed after it.
//
// A record past one message is written as its chunks and then its manifest, the manifest with that
// same compare-and-swap, and the chunks replaced named are deleted once it lands, unless another
// session's row names them too (ownLayout): a manifest copied under this session's key names
// another record's chunks, and an unsubscribe of this session lands over it (kvchunk.Write says
// what else a write deletes and what it leaves to the reaper's collection). The manifest carries
// row's view for an earlier build (ChunkedRecordView), so a listener of that build serves and
// counts the session from it. A session id under the chunk keys' prefix is refused
// (kvchunk.ReservedKey): no record stands there.
func (r *Registry) putInterestAt(ctx context.Context, sessionID string, row Interest, record []byte, revision uint64, replaced kvchunk.Layout) (kvchunk.Written, uint64, error) {
	replaced = r.ownLayout(sessionID, replaced)
	kv, epoch := r.watcher.Handle()
	view := func(room int) ([]byte, error) {
		return ChunkedRecordView(room, row.Topics, func(topics []string) ([]byte, error) {
			seen := row
			seen.Topics = topics
			return json.Marshal(seen)
		})
	}
	written, err := kvchunk.Write(ctx, kv, sessionID, record, view, replaced, func(head []byte) (uint64, error) {
		if revision == 0 {
			return kv.Create(sessionID, head)
		}
		return kv.Update(sessionID, head, revision)
	}, r.now, r.logger(), interestRecord)
	return written, epoch, err
}

// ChunkedRecordView is the view of a record of topics that a manifest carries for a listener of an
// earlier build (kvchunk.Write), encoded by encode with the topics it keeps, in at most room bytes.
// Such a listener reads a manifest as a record of its own type and does not join its chunks, so
// the view is the record with a topic list that matches every subject topics match and fits: the
// covering topics first (CoveringTopics), the same subjects in fewer topics. When those do not
// fit, the topics a session consumer's filter carries (consumerCarries) are reduced as that filter
// is (bus.ReduceFilterSubjects), inside the literal families they name, which may match more of
// those families' subjects; the role lanes and every topic the filter does not carry stay as they
// are, so no reduction widens to a wildcard over every subject. A record whose topics fit neither
// is bus.ErrTooLarge.
func ChunkedRecordView(room int, topics []string, encode func(topics []string) ([]byte, error)) ([]byte, error) {
	covering := CoveringTopics(topics)
	view, err := encode(covering)
	if err != nil {
		return nil, err
	}
	if len(view) <= room {
		return view, nil
	}
	var kept, carried []string
	for _, topic := range covering {
		if !strings.HasPrefix(topic, contracts.RoleTopicPrefix) && consumerCarries(topic) {
			carried = append(carried, topic)
		} else {
			kept = append(kept, topic)
		}
	}
	coveringBytes := len(view)
	view, err = encode(slices.Sorted(slices.Values(append(kept, bus.ReduceFilterSubjects(carried)...))))
	if err != nil {
		return nil, err
	}
	if len(view) <= room {
		return view, nil
	}
	return nil, fmt.Errorf("%w: the view of a record of %d topics an earlier build reads takes %d bytes, or %d reduced, past the %d a manifest leaves it",
		bus.ErrTooLarge, len(topics), coveringBytes, len(view), room)
}

// consumerCarries reports whether a session consumer's filter carries topic: the stream stores it
// and the server takes it as a filter subject, as cmd/listener's sessionConsumerFilter keeps a
// stored row's topics, from the subjects this binary compiles into the stream (bus.StreamCaptures).
func consumerCarries(topic string) bool {
	return bus.StreamCaptures(topic) && bus.CheckFilterSubject(topic) == nil
}

// CoveringTopics is topics without each topic another of them covers, sorted and each once: a list
// that matches every subject topics match, and no other, in fewer topics. A topic is covered when
// every name form of it (contracts.TopicForms, which the subject index matches) is contained by a
// form of the other (bus.SubjectContains); of two that cover each other the first in order stays.
// Only a topic every form of which a session consumer's filter carries (consumerCarries) covers
// another: an earlier build filters a session's consumer on the carried topics of its row alone, so
// a wildcard that filter leaves out - one the stream does not store, or one with a `>` before its
// last token - covering the topics under it would leave the consumer filtering on none of them.
// That rule also keeps every role lane (contracts.RoleTopicPrefix), which a reader of the row
// weighs by its name as well as by what it matches: no subject the stream stores contains a role
// lane, so no carried topic covers one (TestNoStreamSubjectContainsARoleLane).
func CoveringTopics(topics []string) []string {
	unique := slices.Compact(slices.Sorted(slices.Values(topics)))
	forms := make([][]string, len(unique))
	carries := make([]bool, len(unique))
	for i, topic := range unique {
		forms[i] = contracts.TopicForms(topic)
		carries[i] = !slices.ContainsFunc(forms[i], func(form string) bool { return !consumerCarries(form) })
	}
	covers := func(pattern, subject int) bool {
		if !carries[pattern] {
			return false
		}
		for _, form := range forms[subject] {
			if !slices.ContainsFunc(forms[pattern], func(p string) bool { return bus.SubjectContains(p, form) }) {
				return false
			}
		}
		return true
	}
	kept := make([]string, 0, len(unique))
	for i, topic := range unique {
		covered := false
		for j := range unique {
			if j != i && covers(j, i) && (j < i || !covers(i, j)) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, topic)
		}
	}
	return kept
}

// deleteChunks deletes the chunks of sessionID's record that a delete of it left unnamed
// (kvchunk.Delete), each within ctx; one it could not delete is left to the reaper's collection.
func (r *Registry) deleteChunks(ctx context.Context, kv bus.KeyValue, sessionID string, layout kvchunk.Layout) {
	kvchunk.Delete(ctx, kv, layout.Keys(), r.logger(), interestRecord, sessionID)
}

// collectInterestChunks deletes every chunk of the interest bucket that no row of the cache names
// and that the chunk copy took longer ago than kvchunk.Grace (kvchunk.Chunks.Unnamed), and reports
// how many it deleted: the chunks a crash, a call that did not answer or an earlier build's delete
// of a manifest left behind, which no write of this build deletes. It judges nothing while the
// cache does not follow its bucket (kvwatch.Watcher.Following), since a copy behind the bucket
// could lack the manifest that names a chunk. Each delete waits no longer than ctx.
func (r *Registry) collectInterestChunks(ctx context.Context) (int, error) {
	if !r.watcher.Following() {
		return 0, nil
	}
	cutoff := r.now().Add(-kvchunk.Grace)
	r.mu.RLock()
	named := map[string]bool{}
	for _, row := range r.cache.All() {
		if row.layout.Chunked() {
			named[row.layout.Generation] = true
		}
	}
	unnamed := r.chunks.Unnamed(func(generation string) bool { return named[generation] }, cutoff)
	kv, _ := r.watcher.Handle()
	r.mu.RUnlock()
	return kvchunk.Collect(ctx, kv, unnamed)
}
