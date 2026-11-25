package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// deleteInterest deletes sessionID's interest at the revision the bucket stream's leader reports
// (bus.KeyValue.DeleteAtRead, whose read is never a copy's), each bucket call waiting no longer
// than ctx (bus.BoundedOn), and then the chunks of the record it deleted when that record is the
// row the cache holds, joined, for the session. Which record the delete took is the leader's
// answer, never the cache's: the cache's row only proves that the chunks of that generation are
// this session's, so a cache behind the bucket, holding another generation, deletes none of them.
// A manifest the delete read but no read of this listener joined into the session's row names
// chunks never proved to be this record's, so they are left to the reaper's collection, which
// takes only chunks no row names. A delete ctx abandoned may still land after this answer, and that
// is safe: it deletes only the revision it read, so a write that landed since is kept, and its
// chunks are left to the reaper's collection. A record the read found absent is left alone, delete
// and all, so a registration that stored the session's row between that read and this delete keeps
// it; the cache is brought in line with the bucket either way (reconcileDeleted).
func (r *Registry) deleteInterest(ctx context.Context, sessionID string) error {
	// The fence read before the delete is the bucket's this handle reaches only when the read under
	// way began in its epoch (kvwatch.Copy.FenceFor).
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	revision := r.cache.FenceFor(epoch, sessionID)
	held, _ := r.cache.Get(sessionID)
	r.mu.RUnlock()
	read, answered, err := bus.BoundedOn(ctx, kv, func() (bus.LeaderEntry, error) { return kv.DeleteAtRead(ctx, sessionID) })
	if !answered {
		return fmt.Errorf("delete %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return err
	}
	if layout, err := kvchunk.ReadLayout(read.Value); err == nil && layout.Chunked() && layout.Generation == held.layout.Generation {
		r.deleteChunks(ctx, kv, sessionID, held.layout)
	}
	r.reconcileDeleted(ctx, kv, epoch, sessionID, max(revision, read.Revision), read.Kind != bus.LeaderAbsent)
	return nil
}

// rowOf is whether a value is a row of sessionID (boundInterest), the test a read fenced on a
// revision takes (bus.KeyValue.ReadStored, readRow's bucket read): a value it refuses is read again
// from the stream's leader, whose revision is the one the value is stored at. A manifest this build
// writes is such a row, since the view it carries for an earlier build names the session
// (ChunkedRecordView).
func rowOf(sessionID string) func(value []byte) bool {
	return func(value []byte) bool {
		_, err := boundInterest(sessionID, value)
		return err == nil
	}
}

// deleteInterestAt deletes sessionID's interest record only while its latest revision is
// revision, and reports whether the bucket took the delete: a bucket that refused it holds a row
// stored since, which is no error here (JetStream's wrong last sequence, bus.IsCASConflict), and
// that row stands. layout is the layout of the record read at revision, a row of this session (the
// cache's rows and a bucket read's both decode through decodeRecord, which gives a layout only to a
// record boundInterest binds to its key), whose chunks a delete the bucket took deletes after
// it (deleteChunks): the delete landing at revision is what proves that record was the one the
// bucket held. Each bucket call waits no longer than ctx (bus.BoundedOn). A delete ctx abandoned
// may still land after this answer, and that is safe: it deletes the one revision it names, so a
// write that landed since is kept, and the chunks of the record it deleted are left to the
// reaper's collection.
//
// A revision of 0 fences nothing - nats.go sends no expected-last-sequence header for it, so the
// bucket would take the delete over whatever it holds - and names no row a caller read, since a
// bucket's revisions start at 1. It is the answer of a cache hit whose fence belongs to another
// epoch (kvwatch.Copy.FenceFor). No delete is made for it: the row is reported kept, and the
// caller reads the row's own revision from the bucket and asks again.
func (r *Registry) deleteInterestAt(ctx context.Context, sessionID string, revision uint64, layout kvchunk.Layout) (bool, error) {
	if revision == 0 {
		return false, nil
	}
	kv, epoch := r.watcher.Handle()
	_, answered, err := bus.BoundedOn(ctx, kv, func() (struct{}, error) {
		return struct{}{}, kv.Delete(sessionID, nats.LastRevision(revision))
	})
	if !answered {
		return false, fmt.Errorf("delete %s's interest record at revision %d: %w", sessionID, revision, err)
	}
	if bus.IsCASConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r.deleteChunks(ctx, kv, sessionID, layout)
	r.reconcileDeleted(ctx, kv, epoch, sessionID, revision, true)
	return true, nil
}

// reconcileDeleted brings sessionID's cache entry in line with the bucket after a delete through
// kv, a handle of epoch, that answered past revision, and that deleted the record when deleted:
// it applies the key's latest history entry, which is the delete marker unless a write has landed
// since, or, when the history cannot be read within ctx, evicts the entry behind a fence just past
// revision. A latest entry at a revision the cache's fence already holds is no news - a delete
// that read nothing and so deleted nothing - and the cache is left as it is, its row and that row's
// chunks named. A delete that deleted nothing leaves the cache as it is when the history cannot be
// read either: nothing it did changed the bucket, and evicting the row would leave the chunks of a
// record that still stands to the collection. A manifest another listener stored since is evicted
// behind that fence rather than applied: the copy joins a manifest only from the chunks its watcher
// delivered, which may not have reached it yet, and the watcher delivers that manifest after its
// chunks, past the fence.
func (r *Registry) reconcileDeleted(ctx context.Context, kv bus.KeyValue, epoch uint64, sessionID string, revision uint64, deleted bool) {
	// A history that does not answer falls back to the fence below: the delete itself answered.
	// ReadHistory ends with ctx; bus.RequestTimeout bounds it for a caller that answers no request
	// and so passes a context that never ends.
	historyCtx, cancel := context.WithTimeout(ctx, bus.RequestTimeout)
	entries, err := kv.ReadHistory(historyCtx, sessionID)
	cancel()
	if err != nil && !deleted {
		return
	}
	r.mu.Lock()
	var latest kvwatch.Entry
	if err == nil && len(entries) > 0 {
		latest = kvwatch.JetStreamEntry(entries[len(entries)-1])
		if latest.Op == kvwatch.OpPut && latest.Revision <= r.cache.FenceFor(epoch, sessionID) {
			r.mu.Unlock()
			return
		}
	}
	if err != nil || len(entries) == 0 || (latest.Op == kvwatch.OpPut && kvchunk.IsManifest(latest.Value)) {
		// KeyValue.Delete does not return its revision. A successful delete must
		// still fence off any watcher update from the preceding revision while the
		// delete marker remains pending.
		r.cache.Evict(r.watcher, epoch, sessionID, revision+1)
		r.mu.Unlock()
		return
	}
	_, malformed := r.cache.Apply(r.watcher, epoch, latest)
	r.mu.Unlock()
	if malformed != nil {
		r.logger().Warn("registry cache evicted malformed value",
			slog.String("key", latest.Key),
			slog.Uint64("revision", latest.Revision),
			slog.String("error", malformed.Error()),
		)
	}
}

// Upsert is UpsertWithin with no deadline of its own, for a caller that answers no request.
func (r *Registry) Upsert(item Interest, topics []string) (Interest, error) {
	return r.UpsertWithin(context.Background(), item, topics)
}

// UpsertWithin merges topics into item.SessionID's interest record (mergeForUpsert) and writes it,
// stamped with the registry's clock, each bucket call waiting no longer than ctx (bus.BoundedOn): an
// HTTP route passes its request's own context. It returns the record as it now stands. A record
// this build cannot decode (ErrMalformedInterest) is replaced by item and topics alone, and the
// registry logs that at WARN naming the session.
//
// The row it merges into is the one the bucket holds, read with its revision and layout (getLatest)
// and never from the cache, and the write is one compare-and-swap at that revision
// (putInterestAt), as an unsubscribe's is (removeInterestTopics). A role claim - the one
// production caller, through SetRoleWithPrevious - built from a cached row a write on another
// listener has not reached would otherwise store that row back: every topic an unsubscribe
// removed comes back into the record, and every topic a registration was answered 200 for since
// is dropped from it, with nothing to put it there again. A refused write is another writer
// storing the row between this read and it, so the row is read and built again, up to
// interestWriteAttempts times, past which the caller sees ErrInterestTopicsNotMerged and nothing
// is stored. A write ctx abandoned may still land after this answer, and that is safe for the
// reason an unsubscribe's is: it stores over the one revision it read, so it cannot undo a write
// that landed after it.
//
// A heartbeat re-sends the same topics every 90 seconds, so UpsertWithin returns the current row
// without writing when the encoding of merged equals that of the row it read, apart from
// UpdatedAt, and the row is younger than InterestRefreshAge. The row judged is the row the write
// would be fenced on, so the decision not to write is made against what the bucket holds. An
// unchanged row is thus rewritten once per InterestRefreshAge, its UpdatedAt lags the session's
// last heartbeat by less than that, and Reap adds InterestRefreshAge to its grace window
// (InterestReapAllowance), so that lag never shortens a paused session's grace. A changed row is
// written, and so is a record it cannot decode; any other failed read refuses the write
// (mergeForUpsert). The session row (session.SessionRegistry.Put) is written on every heartbeat:
// its bucket's MaxAge is the liveness clock, and NATS connect notices ($SYS) are not available to
// replace it.
//
// It reads the claim of each controller notice lane's role itself (controllerLaneRoles), and a
// read that did not answer refuses the write. A caller that already read those claims to decide
// the same topics elsewhere, the listener's registration route filtering a session consumer,
// plans once and writes that plan instead (PlanInterest, UpsertPlanned).
func (r *Registry) UpsertWithin(ctx context.Context, item Interest, topics []string) (Interest, error) {
	for attempt := range interestWriteAttempts {
		// A retry follows a write the bucket refused at the revision the last read reported, so it
		// reads the row from the stream's leader (readLeader): a record the server wrote, which a
		// direct get reports at another stream's sequence, and a marker a direct get reports at
		// another key's, are replaced at the sequence each is stored at.
		read := readBucket
		if attempt > 0 {
			read = readLeader
		}
		cur, revision, getErr := r.getLatest(ctx, item.SessionID, read)
		roles, err := r.controllerLaneRoles(ctx, item.SessionID, cur.Topics, item.Topics, topics)
		if err != nil {
			return Interest{}, err
		}
		row, stored, err := r.writeRowAt(ctx, cur, revision, getErr, item, InterestPlan{Roles: roles, Topics: topics})
		if err != nil {
			return Interest{}, err
		}
		if !stored {
			// Another writer stored the row under this read: the row is read and merged again
			// over theirs, where a planned write hands the conflict to its caller instead
			// (UpsertPlanned).
			continue
		}
		return row, nil
	}
	return Interest{}, fmt.Errorf("merge topics into %s's interest record: %w after %d attempts",
		item.SessionID, ErrInterestTopicsNotMerged, interestWriteAttempts)
}

// PlanInterest plans the interest row a registration of sessionID naming requested will store:
// the topics it will hold (MergedTopics of the stored row and requested), the controller roles
// read to decide them, each read once from its claim record for the row's topics and requested
// together (controllerLaneRoles), requested itself and the bucket revision of the row it read.
// The listener's registration route filters the session's consumer on the plan and hands the same
// plan back to UpsertPlanned, so a registration whose body leaves out a controller notice lane
// its row holds keeps that lane in the filter and in the row alike: the roles behind both are
// this one read, which the row's own topics are part of.
//
// A session with no row, and a row this build cannot decode, plan from no stored topics: the
// registration's own topics are the authority on such a record, as the write that follows treats
// it (mergeForUpsert). Any other read that did not answer is returned, and so is a read of a
// controller's claim that did not answer or failed (controllerLaneRoles): the route refuses the
// registration rather than filter a consumer on a row the write would decide differently.
func (r *Registry) PlanInterest(ctx context.Context, sessionID string, requested []string) (InterestPlan, error) {
	return r.planInterest(ctx, sessionID, slices.Clone(requested), readCache)
}

// PlanInterestAfterConflict is PlanInterest over the row the bucket now holds rather than the
// cached one: the caller's last plan was refused its write (UpsertPlanned, ErrInterestPlanConflict)
// by the writer that stored that row, and a plan made from a cache still behind it would be
// refused again. Its caller filters the session's consumer on this plan before writing it, as it
// did for the first, so the filter and the row still name one set after a conflict.
//
// The row is read from the stream's leader (readLeader), and so is the row UpsertPlanned fences
// this plan's write on (InterestPlan.read): a refusal is also what a record the server
// wrote earns a write, since a direct get reports it at another stream's sequence, and the
// leader's read answers such a record as no row of the session's at the sequence it is stored
// at, which the write then replaces.
func (r *Registry) PlanInterestAfterConflict(ctx context.Context, sessionID string, requested []string) (InterestPlan, error) {
	return r.planInterest(ctx, sessionID, slices.Clone(requested), readLeader)
}

// planInterest is PlanInterest over a row read as read says: from the cache, or from the stream's
// leader for a plan made again after the bucket refused a write at the revision the last plan
// read, which must see the row that refused it, which the cache may not hold yet.
func (r *Registry) planInterest(ctx context.Context, sessionID string, requested []string, read interestRead) (InterestPlan, error) {
	cur, revision, err := r.getLatest(ctx, sessionID, read)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) && !errors.Is(err, ErrMalformedInterest) {
		return InterestPlan{}, err
	}
	roles, err := r.controllerLaneRoles(ctx, sessionID, cur.Topics, requested)
	if err != nil {
		return InterestPlan{}, err
	}
	return InterestPlan{
		Roles:    roles,
		Topics:   MergedTopics(sessionID, roles, cur.Topics, requested),
		Revision: revision,
		read:     read,
	}, nil
}

// ErrInterestPlanConflict is a write whose interest row was rewritten under the plan it was made
// for: another writer stored the row between the plan and the write, so the compare-and-swap
// found a revision that is not the plan's. Its caller plans again (PlanInterestAfterConflict),
// filters the session's consumer on the new plan and writes again, a bounded number of times
// (registrationPlanAttempts in cmd/listener, whose loop this error belongs to); past that the
// listener answers the bus-unavailable 503 and the plugin's next heartbeat registers again.
// Nothing is stored for a refused write, so the row keeps whatever the write that won it holds.
var ErrInterestPlanConflict = errors.New("the interest record changed under the plan of this registration")

// UpsertPlanned is UpsertWithin for a caller holding a plan (PlanInterest): it writes the row
// that plan decided, under the roles that decided it, rather than reading the session's roles
// again.
//
// The write is one compare-and-swap at the revision the plan read (putInterestAt), because
// seconds pass between the plan and this call - the listener's registration route creates or
// re-filters the session's consumer in between - and an unsubscribe landing in that window would
// otherwise be undone: the plan merged the row as it stood, so writing it back puts every topic
// the unsubscribe removed into the row again, and the row only ever grows from there. A refused
// write is ErrInterestPlanConflict and stores nothing. Planning again is the caller's, not this
// call's: the filter of the session's consumer is built from the plan, so a plan made here would
// store a row the filter was never set for, and the two would name different sets until the
// session's next heartbeat (InterestPlan, cmd/listener's subscribeHandler).
//
// The row it merges and judges is read from the bucket, and a bucket that answers a revision
// that is not the plan's is ErrInterestPlanConflict before anything is written: that row is one
// no plan of this registration merged and no filter of it was set for, and reading the cache
// here would hide it. The skip a heartbeat takes (interestRow) is why the read cannot wait for
// the compare-and-swap to catch it: a skip decided against a cached row stores nothing, so the
// swap that would have refused it never runs, and the registration is answered 200 from a row
// the bucket does not hold. What is left open is the window between this read and the write or
// the skip, which is one call wide rather than the seconds the consumer call takes: a row
// stored inside it refuses the write, and the skip inside it answers from the row that moved.
func (r *Registry) UpsertPlanned(ctx context.Context, item Interest, plan InterestPlan) (Interest, error) {
	read := readBucket
	if plan.read == readLeader {
		read = readLeader
	}
	cur, revision, getErr := r.getLatest(ctx, item.SessionID, read)
	if revision != plan.Revision {
		return Interest{}, fmt.Errorf("write %s's interest record: %w", item.SessionID, ErrInterestPlanConflict)
	}
	row, stored, err := r.writeRowAt(ctx, cur, revision, getErr, item, plan)
	if err != nil {
		return Interest{}, err
	}
	if !stored {
		// Another writer stored the row between the read above and the write, which is the
		// caller's to plan again over (PlanInterestAfterConflict), since the filter of the
		// session's consumer was set for this plan.
		return Interest{}, fmt.Errorf("write %s's interest record: %w", item.SessionID, ErrInterestPlanConflict)
	}
	return row, nil
}

// writeRowAt stores the row a write of item under plan over cur makes (interestRow), under the
// revision cur was read at, and reports the row the session's record now holds with whether the
// bucket took the write. getErr is what that read answered with. The row is written under
// item.SessionID, the session the write is for, and names it (mergeForUpsert): never under a key a
// stored row names, so a row the server copied under this key from another session's cannot
// carry the write into that session's row (ErrInterestUnbound).
//
// It is the whole of what a merging write and a planned write do once their row is read
// (UpsertWithin, UpsertPlanned): the heartbeat skip, which answers cur and writes nothing, the
// compare-and-swap at the read's revision (putInterestAt), which deletes the chunks of cur's
// layout once it lands, and the cache of what it stored (recordInterest). The two differ in their
// answer to a write the bucket refused, and that is what the bool leaves them: a merging write
// builds the row again over the row that won, and a planned write is refused
// (ErrInterestPlanConflict), because the session's consumer is already filtered on the plan this
// row came from.
func (r *Registry) writeRowAt(ctx context.Context, cur storedRow, revision uint64, getErr error, item Interest, plan InterestPlan) (Interest, bool, error) {
	merged, buf, unchanged, err := r.interestRow(cur.Interest, getErr, item, plan)
	if err != nil {
		return Interest{}, false, err
	}
	if unchanged {
		return cur.Interest, true, nil
	}
	written, epoch, err := r.putInterestAt(ctx, item.SessionID, merged, buf, revision, cur.layout)
	if err != nil {
		return Interest{}, false, err
	}
	if !written.Stored {
		return Interest{}, false, nil
	}
	r.recordInterest(storedRow{Interest: merged, layout: written.Layout}, written.Revision, epoch, getErr)
	return merged, true, nil
}

// interestRow is the row a write of item under plan over the row cur that getErr reports the read
// of stores, and its encoding, or unchanged when the bucket already holds that row young enough
// to leave alone (the heartbeat skip of UpsertWithin and UpsertPlanned). cur is the row of the
// read the write is fenced on, so the decision not to write is made against the row the fence
// would judge.
func (r *Registry) interestRow(cur Interest, getErr error, item Interest, plan InterestPlan) (merged Interest, buf []byte, unchanged bool, err error) {
	now := r.now().UnixMilli()
	merged, err = mergeForUpsert(cur, getErr, item, now, plan)
	if err != nil {
		return Interest{}, nil, false, err
	}
	buf, err = encodeInterest(merged)
	if err != nil {
		return Interest{}, nil, false, err
	}
	if getErr == nil && now-cur.UpdatedAt < InterestRefreshAge.Milliseconds() {
		// What the skip asks is whether this write would store the row it holds, so it compares
		// the encoding of merged with that of the current row rather than the fields one by one:
		// a field added to Interest, a key only a newer build knows (Interest.unknown), and the
		// difference between a null and an empty topic list are then covered by the encoding
		// itself. The current row is stamped with merged's UpdatedAt, the one field a write of an
		// unchanged row would move.
		stored := cur
		stored.UpdatedAt = merged.UpdatedAt
		encoded, err := encodeInterest(stored)
		if err != nil {
			return Interest{}, nil, false, err
		}
		if bytes.Equal(buf, encoded) {
			return merged, buf, true, nil
		}
	}
	return merged, buf, false, nil
}

// recordInterest caches the row a write stored at its revision, with the layout it was stored in,
// through the handle of epoch it was written with (kvwatch.Copy.Put), and logs the replacement of
// a record this build cannot decode, which getErr reports.
func (r *Registry) recordInterest(stored storedRow, revision, epoch uint64, getErr error) {
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, stored.SessionID, stored, revision)
	r.mu.Unlock()
	if errors.Is(getErr, ErrMalformedInterest) {
		r.logger().Warn("registry replaced an interest record it cannot decode",
			slog.String("session_id", stored.SessionID),
			slog.Uint64("revision", revision),
			slog.Any("topics", stored.Topics),
		)
	}
}

// mergeForUpsert computes the Interest to persist by reconciling the requested
// item/topics with the current cached/durable state returned by r.Get.
//
// Critical invariant: a transient KV error (anything other than ErrKeyNotFound or ErrMalformedInterest)
// MUST be returned so the caller refuses to overwrite durable state with the
// partial heartbeat payload. Silently treating transient errors as "no prior
// state" is what caused Atlas's pr.11416.> subscription to disappear: when r.Get
// failed transiently during a 2-minute heartbeat, the original implementation
// merged only the agent topic and Put a truncated state to KV, which then
// propagated to the in-memory cache via watch() and made all subsequent github
// events fall through registry.Match with "no matching interests".
//
// ErrKeyNotFound is the genuinely-new-session case — proceed with the passed-in
// item and topics. ErrMalformedInterest is merged the same way: the record answered, so this is
// not the transient case, its topics are ones this build never routed (the cache holds no record
// it cannot decode), and the session's own subscribe is the authority on its record. A record
// naming another session than its key is one of these (ErrInterestUnbound): its topics are another
// session's, or a JetStream answer's, and none of them is merged.
//
// The merged row names the requesting session on every branch: the key it is written under
// (writeRowAt), whatever the stored row named.
//
// The merged row keeps no topic reaching a private stream subject that is not the session's own,
// and holds them sorted (MergedTopics): a row stored before the listener refused such a topic is
// cleaned at the session's next registration. plan is the topics to merge in and the roles the
// session holds (controllerLaneRoles), which decide the one private subject a session registers
// beside its own agent subject: the controller notice lane of the project whose controller role
// it holds. The topics are merged into the row the write read, never into the plan's copy of
// it, so a topic the bucket's row holds and a stale cache lacked is kept. The listener's
// registration route filters the session's consumer on the row this merge produces, having
// planned it with these same roles (PlanInterest), and plans again when the row it stored is not
// the one it filtered on. plan's Withheld topics are taken out of the row the write read before
// the merge: a caller that holds a session to a rule of its own (cmd/listener's edge hold) takes
// a stored topic out of the row that way. The row keeps none of the plan's LeftOut either, the
// topics that route keeps out of the row because the session's consumer filter leaves them out
// (InterestPlan.LeftOut), so a row an earlier build stored with one is cleaned by the same write.
//
// The machine id and the directory are the requesting registration's, and the stored row is the
// fallback for whichever of the two the request leaves empty. A session that registers again
// through another listener therefore moves its row to that listener, and every reader of the row
// - the fan-out index (subjectIndex.set, which moves the session's subjects to the new root),
// Registry.Match, the durable path's ownership check and the sender stamp of a message the
// session sends (cmd/listener's senderStamp) - names the listener the session is registered
// through now. A request that names no machine or no directory says nothing about where the
// session runs, so for it the stored value stands: a role claim carries the claiming listener's
// machine id and no directory (SetRoleWithPrevious), and claiming a role must not clear the
// row's Cwd.
func mergeForUpsert(cur Interest, getErr error, item Interest, now int64, plan InterestPlan) (Interest, error) {
	if getErr != nil && !errors.Is(getErr, nats.ErrKeyNotFound) && !errors.Is(getErr, ErrMalformedInterest) {
		return Interest{}, getErr
	}
	requested := item
	if getErr == nil {
		item = cur
	}
	item.MachineID = first(requested.MachineID, cur.MachineID)
	item.Dir = first(requested.Dir, cur.Dir)
	item.UpdatedAt = now
	item.SessionID = requested.SessionID
	stored := item.Topics
	if len(plan.Withheld) > 0 {
		stored = slices.DeleteFunc(slices.Clone(stored), func(topic string) bool { return slices.Contains(plan.Withheld, topic) })
	}
	item.Topics = MergedTopics(requested.SessionID, plan.Roles, stored, plan.Topics)
	if len(plan.LeftOut) > 0 {
		item.Topics = slices.DeleteFunc(item.Topics, func(topic string) bool { return slices.Contains(plan.LeftOut, topic) })
	}
	return item, nil
}

// controllerLaneRoles reports the controller roles sessionID holds among those of the Legion
// controller notice lanes the topic sets name (bus.ControllerNoticeRole), as their claim records
// say (ReadLaneHolders), and nil when they name none (bus.NamesControllerNoticeLane): that lane is
// the one private stream subject whose owner the rule reads a session's roles for
// (bus.ReachesPrivateStreamSubject), so a write naming none reads no claim and is refused for none.
// A read that did not answer or failed is returned, naming the session, and the write is refused
// rather than storing the row without the lane: a notice published while the lane was out of the
// consumer's filter reaches the controller on no later path. The listener's registration route
// answers that refusal with the bus-unavailable 503 (cmd/listener's writeStoreError).
func (r *Registry) controllerLaneRoles(ctx context.Context, sessionID string, topics ...[]string) ([]string, error) {
	holders, err := r.ReadLaneHolders(ctx, topics...)
	if err != nil {
		return nil, fmt.Errorf("decide whether %s holds a controller notice lane: %w", sessionID, err)
	}
	return holders.heldBy(sessionID), nil
}

// LaneHolders is what the claim records of Legion controller roles say about their notice lanes
// (bus.ControllerNoticeRole): for each role a read weighed, the session its record names, or the
// empty string for a role whose lane is nobody's. Registry.ReadLaneHolders reads it, and Keeps
// judges a session's stored topics by it.
type LaneHolders map[string]string

// ReadLaneHolders reads the claim record of the controller role of each controller notice lane
// the topic sets name, once each however many of the sets name its lane, from the role bucket
// stream's leader and waiting no longer than ctx (roleClaimFromLeader), and never the role claim
// copy (RolesHeldBy); it reads nothing and answers nil when the sets name no lane
// (bus.NamesControllerNoticeLane). What it answers decides a session's consumer filter, so a copy
// that lost the controller's claim would drop the lane from its holder's filter, and a copy that
// still names a session the claim has left would hand that session another controller's notices,
// each until the copy's next read repaired it. A direct get is no better: on a replicated bucket
// any copy near enough to current answers one, and a copy one write behind a takeover names the
// former holder, which kept the lane on that holder's row and filter at the very takeover that
// should drop it, and counted it as a recipient of the lane (cmd/listener's recipientCounts). A
// record that names a holder gives the lane to that session. A record this build cannot decode
// (ErrMalformedRoleClaim) names no holder, as the copy reads it, and a key NATS refuses
// (bus.ErrRefused) is one no read can tell the holder of, which the role claim reaper removes; each
// gives the lane to nobody and is no error, so a registration naming it stores the rest of its row
// rather than being refused at every heartbeat. Any other read that did not answer or failed is
// returned, naming the role.
func (r *Registry) ReadLaneHolders(ctx context.Context, topics ...[]string) (LaneHolders, error) {
	if !slices.ContainsFunc(topics, bus.NamesControllerNoticeLane) {
		return nil, nil
	}
	holders := LaneHolders{}
	for _, set := range topics {
		for _, topic := range set {
			role, lane := bus.ControllerNoticeRole(topic)
			if _, weighed := holders[role]; !lane || weighed {
				continue
			}
			claim, err := r.roleClaimFromLeader(ctx, role)
			switch {
			case errors.Is(err, ErrMalformedRoleClaim), errors.Is(err, bus.ErrRefused):
				// A claim no read can name a holder of: the lane is nobody's.
				holders[role] = ""
			case err != nil:
				return nil, err
			case claim.Revision == 0:
				holders[role] = ""
			default:
				holders[role] = claim.HolderSessionID
			}
		}
	}
	return holders, nil
}

// roleClaimFromLeader is RoleClaim with the leader of the role bucket's stream answering the read
// (bus.KeyValue.GetFromLeader) instead of whichever copy of the bucket a direct get reaches, so it
// answers the newest claim the bucket took, also one written a moment ago. ctx bounds the whole
// read, and so does its connection (bus.BoundedOn): a read whose connection is lost before the
// leader answers fails at the reconnect rather than waiting out ctx, since nats.go fails no
// request its connection drops. It answers as RoleClaim answers: a zero claim for a role no record
// claims (a marker included), the record's revision on the claim, ErrMalformedRoleClaim for a
// record that is not a claim, and the refusal of a key NATS refuses (bus.ErrRefused). A role
// bucket handle whose legacy half answers the leader's read itself (leaderReader) is read through
// it.
func (r *Registry) roleClaimFromLeader(ctx context.Context, role string) (RoleClaim, error) {
	roles := r.roles()
	read := roles.GetFromLeader
	if reader, answers := roles.Legacy().(leaderReader); answers {
		read = reader.GetFromLeader
	}
	entry, answered, err := bus.BoundedOn(ctx, roles, func() (nats.KeyValueEntry, error) { return read(ctx, role) })
	if !answered {
		return RoleClaim{}, fmt.Errorf("read role %s's claim from the bucket's stream leader: %w", role, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return RoleClaim{}, nil
	}
	if err != nil {
		return RoleClaim{}, fmt.Errorf("read role %s's claim from the bucket's stream leader: %w", role, err)
	}
	claim, err := DecodeRoleClaim(entry.Value())
	if err != nil {
		return RoleClaim{}, err
	}
	claim.Revision = entry.Revision()
	return claim, nil
}

// leaderReader is a legacy bucket handle that answers the leader's read of a key itself, as a
// test's handle does to count or fail that read beside the direct gets it counts: that read goes to
// the bucket stream's leader on the connection (bus.KeyValue.GetFromLeader) and never through the
// legacy handle. No nats.go handle is one, so every listener's read goes to the leader.
type leaderReader interface {
	GetFromLeader(ctx context.Context, key string) (nats.KeyValueEntry, error)
}

// heldBy is the controller roles h gives sessionID, sorted.
func (h LaneHolders) heldBy(sessionID string) []string {
	var held []string
	for role, holder := range h {
		if holder != "" && holder == sessionID {
			held = append(held, role)
		}
	}
	slices.Sort(held)
	return held
}

// Keeps reports whether a row of sessionID keeps topic, one its stored row or its known-session
// record names: whether topic reaches no private stream subject that is not the session's own
// (bus.ReachesPrivateStreamSubject), the session holding the controller roles h names it the
// holder of. It is the store's half of the one judgment of a stored topic (cmd/listener's
// storedTopicFiltered, which every filter built from a stored row and a publish's recipient count
// make), whose other half is whether the consumer filter can take the topic at all. A controller
// notice lane whose role h weighed no claim of is held by nobody here, so a caller reads the
// holders of every lane its answer turns on: a filter build those of every lane the row names, and
// the recipient count that of the lane it publishes on, the one lane that can match its subject,
// since a lane is a literal.
func (h LaneHolders) Keeps(sessionID, topic string) bool {
	return !bus.ReachesPrivateStreamSubject(sessionID, topic, h.heldBy(sessionID))
}

// Remove releases the role claims of sessionID that release authorises (RoleReleaseAuthority)
// and removes topics from its interest record, or deletes the record when topics is empty, each
// bucket call waiting no longer than ctx (bus.BoundedOn): an HTTP route passes its request's own
// context. A write or delete ctx abandoned may still land after this answer;
// releaseReadRoleClaim, deleteInterest and removeInterestTopics each say why that is safe. A
// record this build cannot decode (ErrMalformedInterest) is deleted all the same; removing some
// topics leaves it as it is (removeInterestTopics). Removing some topics writes the row the
// bucket holds, under that row's revision, so it drops no topic another writer stored; a caller
// whose every attempt at that write the bucket refused sees ErrInterestTopicsNotRemoved with
// nothing stored, and the role claims this call released stay released.
//
// How the release is authorised is the caller's to state and is never inferred from the topics.
// The one thing release decides is which claims are freed, and a release that authorises none
// frees none - which is a request from a second process that resumed this session's id, or one
// from a client that cannot say which process it is, and neither may take a role from the
// process that holds it. A claim that names no process is no exception: it names no process a
// release could match, so only the administrator's force frees it (RoleReleaseAuthority.frees).
//
// What the release freed decides one thing in the interest half too. A role lane topic the
// caller named whose claim still names this session and which this release did not free stays
// in the record (releaseRoleClaims answers which topics those are), because one interest record
// serves every process running under one session id and that topic is the interest half of a
// claim that still stands, written there by the grant (SetRoleWithPrevious). No delivery rides
// on it - a session's consumer filter leaves every role lane out (bus.RoleLane, which
// cmd/listener's sessionConsumerFilter reads), since the stream never stores one, and a role
// message reaches its holder's agent subject with the holder read off the claim (cmd/listener's
// resolveCoreRoleHolder) - but the record is
// where a deregistration of this session finds the role claims it frees (the empty-topics
// branch below, releaseAllRoleClaims), so a release from a process that does not hold the claim
// would otherwise take the lane topic out from under the process that does and leave the claim
// standing with nothing in the record naming it. Every other topic the caller named is
// removed exactly as before - one that is not a role lane's, a wildcard, a role no record
// claims, a role another session holds, and a role whose claim this release did free. A caller
// whose every named topic was kept writes nothing at all, since an empty list is how
// removeInterestTopics is told to delete the whole record.
//
// Remove answers the topics it took out of the record, and that list is what POST
// /v1/interests/unsubscribe reports as removed. It is the removed list rather than the one the
// caller named because the two part company over a kept lane topic: the caller named it, the
// record still holds it, and an answer echoing the request would report a removal that did not
// happen. A deregistration answers no list at all - it deletes the whole record, so there is no
// list of named topics to report, and its route writes the empty list for it.
//
// An empty topics is the whole of the session's interests, so a deregistration releases every
// role either of two sources names, each once and each as its own claim record says
// (releaseAllRoleClaims): the role topics of the session's interest record, and the roles the
// role claim view names it the holder of. The record is read here from the bucket
// (getWithRevision) and not from the interest cache Get answers from, which is a copy of the
// bucket as well and may not yet hold the lane topic a grant on another listener wrote into the
// record. A role the view has lost is therefore released from the record, and a role whose topic
// the record has lost is released from the view, by the same authority as the rest; while the
// view cannot be served, a scan of the role bucket stands in for it. A session the bucket holds
// no record for, and a record this build cannot decode, name no roles, and the view's are
// released alone. A read of the record that did not answer or failed fails the deregistration
// before it releases anything.
//
// Whether this release may mark a claim it cannot free handed back (RoleClaim.HandedBack) is
// decided here and nowhere else, and carried on the release itself
// (RoleReleaseAuthority.handsBack, which no caller outside this package can set): a release
// marks exactly when the caller named topics of its own, and stated no process, and did not
// force. The named topics are what make the release a hand-back of those very roles - the
// caller named that role's lane topic - while the empty-topics branch weighs the stored record's
// roles and the view's, which is a deregistration of everything the session has and a hand-back
// of no one role, so it marks nothing whatever those topics turn out to be. A release that names
// a process frees the claims of that process and leaves another process's alone, and the
// administrator's force frees them all; neither leaves a mark, since a mark is for the hand-back
// that could free nothing, whether the claim it marks names a process or none.
//
// A session id under the chunk keys' prefix (kvchunk.Prefix) names no session, and a delete of it
// would delete a chunk a stored manifest names, so its removal is refused (kvchunk.ReservedKey, a
// bus.ErrRefused the routes answer 400) before anything is read or released.
func (r *Registry) Remove(ctx context.Context, sessionID string, topics []string, release RoleReleaseAuthority) ([]string, error) {
	if kvchunk.IsChunkKey(sessionID) {
		return nil, kvchunk.ReservedKey(sessionID)
	}
	if len(topics) == 0 {
		record, _, err := r.getWithRevision(ctx, sessionID, readBucket)
		if err != nil && !errors.Is(err, nats.ErrKeyNotFound) && !errors.Is(err, ErrMalformedInterest) {
			return nil, err
		}
		if err := r.releaseAllRoleClaims(ctx, sessionID, record.Topics, release); err != nil {
			return nil, err
		}
		return nil, r.removeInterestTopics(ctx, sessionID, topics)
	}
	release.handsBack = release.ClientInstanceID == "" && !release.Force
	kept, err := r.releaseRoleClaims(ctx, sessionID, topics, release)
	if err != nil {
		return nil, err
	}
	removed := topicsExcept(topics, kept)
	if len(removed) == 0 {
		return removed, nil
	}
	// Removing a topic removes every form of it the record holds (Remove), so the answer names
	// those too: a client that followed both forms reads that the one it did not name is gone,
	// rather than finding it missing later. They are read from the row as it stands before the
	// removal, which is the row the removal starts from unless another writer races it.
	others := r.otherFormsHeld(ctx, sessionID, removed)
	if err := r.removeInterestTopics(ctx, sessionID, removed); err != nil {
		return nil, err
	}
	return slices.Concat(removed, others), nil
}

// otherFormsHeld is each topic sessionID's record holds that is another form of one of topics
// (oneInterest) and not itself among them, in the record's order; none when the record cannot be
// read.
func (r *Registry) otherFormsHeld(ctx context.Context, sessionID string, topics []string) []string {
	record, _, err := r.getWithRevision(ctx, sessionID, readCache)
	if err != nil {
		return nil
	}
	interests := make(map[string]bool, len(topics))
	for _, topic := range topics {
		interests[oneInterest(topic)] = true
	}
	var others []string
	for _, topic := range record.Topics {
		if interests[oneInterest(topic)] && !slices.Contains(topics, topic) {
			others = append(others, topic)
		}
	}
	return others
}

// topicsExcept is topics less the ones in kept, which releaseRoleClaims left standing. It
// returns topics itself where nothing was kept, the usual case, so a release that freed what it
// named allocates nothing here. kept holds the role lane topics of one request, which is a
// handful at most, so each topic is weighed against it by a walk rather than by a map built for
// the purpose.
func topicsExcept(topics, kept []string) []string {
	if len(kept) == 0 {
		return topics
	}
	removed := make([]string, 0, len(topics)-len(kept))
	for _, topic := range topics {
		if !slices.Contains(kept, topic) {
			removed = append(removed, topic)
		}
	}
	return removed
}

// interestWriteAttempts bounds the times a writer of an interest row builds that row again after
// the bucket refused the write or delete it built: an unsubscribe's (removeInterestTopics) and a
// role claim's (UpsertWithin). Each refusal is another writer storing the session's row between
// that call's read and its write, which only a registration, a role claim or an unsubscribe of
// the same session does; three attempts cover a burst of those and still end, rather than
// letting one writer spin against another that keeps winning. The registration's loop is
// bounded the same way and for the same reason (registrationPlanAttempts in cmd/listener).
const interestWriteAttempts = 3

// ErrInterestTopicsNotRemoved is an unsubscribe every attempt of which the bucket refused
// (interestWriteAttempts): each attempt built the row from the newest row the bucket held and
// wrote it under that row's revision, and another writer stored the row in between every time.
// Nothing is stored for a refused write, so the record holds what the writer that won it stored,
// the topics this unsubscribe asked to remove among them, and its session keeps receiving them.
// The listener's unsubscribe route answers the bus-unavailable 503 for it, as it does for a
// registration whose plans run out (ErrInterestPlanConflict, cmd/listener's writeStoreError),
// and the plugin's retry removes the topics from the row that won.
var ErrInterestTopicsNotRemoved = errors.New("the interest record changed under every attempt to remove topics from it")

// ErrInterestTopicsNotMerged is the same for a write that merges topics in (UpsertWithin): every
// attempt built the row from the newest row the bucket held and was refused it. Nothing is
// stored, so the record holds what the writer that won it stored and none of the topics this
// call asked to add. Its one production caller is a role claim (SetRoleWithPrevious), which
// writes no claim after it, so POST /v1/roles/set answers the bus-unavailable 503 for it
// (cmd/listener's writeStoreError) and the role stays where the writer that won it left it.
var ErrInterestTopicsNotMerged = errors.New("the interest record changed under every attempt to merge topics into it")

// removeInterestTopics removes topics from sessionID's interest record, deleting it when none is
// left or topics is empty, each bucket call waiting no longer than ctx.
//
// The row it stores is built from the newest row the bucket holds and written under that row's
// revision (putInterestAt), as a registration's is (UpsertPlanned). An unsubscribe that built
// its row from a cache a write on another listener has not reached would otherwise store that
// stale row's topics, dropping from the record a topic the other listener's registration was
// answered 200 for, and no later write puts it back. The first attempt reads the cache, the row
// the bucket holds in every pass but the raced one; a refused write reads the bucket itself and
// builds the row again (readRow), up to interestWriteAttempts times, past which the
// caller sees ErrInterestTopicsNotRemoved and nothing is stored. The delete of a record the
// removal empties is fenced on that same revision (deleteInterestAt) for the same reason, and a
// delete the bucket did not take - a row written since, or a cache hit that named no revision -
// is another attempt too. Either one deletes the chunks of the record it replaced or deleted
// once the bucket took it, so a removal that shrinks a chunked record to one message leaves no
// chunk behind.
//
// A write ctx abandoned may still land after this answer, and that is safe for the reason a
// registration's conditional write is: it stores over the one revision it read, so it cannot
// undo a write that landed after it. A record this build cannot decode routes none of its topics
// here, so there is nothing to remove from it: it is left for the session's next subscribe to
// replace or a delete to remove, and the registry logs that at WARN naming the session.
func (r *Registry) removeInterestTopics(ctx context.Context, sessionID string, topics []string) error {
	// Empty topics = unsubscribe from everything (delete the entry).
	if len(topics) == 0 {
		return r.deleteInterest(ctx, sessionID)
	}
	for attempt := range interestWriteAttempts {
		from := readCache
		if attempt > 0 {
			from = readLeader
		}
		read, revision, err := r.readRow(ctx, sessionID, from)
		if errors.Is(err, ErrMalformedInterest) {
			r.logger().Warn("registry left an interest record it cannot decode unchanged",
				slog.String("session_id", sessionID),
				slog.Any("topics", topics),
			)
			return nil
		}
		if err != nil {
			return err
		}
		item := Remove(read.Interest, topics)
		roles, err := r.controllerLaneRoles(ctx, sessionID, item.Topics)
		if err != nil {
			return err
		}
		item.Topics = dropPrivateStreamTopics(sessionID, roles, item.Topics)
		if len(item.Topics) == 0 {
			deleted, err := r.deleteInterestAt(ctx, sessionID, revision, read.layout)
			if err != nil || deleted {
				return err
			}
			continue
		}
		item.UpdatedAt = r.now().UnixMilli()
		buf, err := encodeInterest(item)
		if err != nil {
			return err
		}
		written, epoch, err := r.putInterestAt(ctx, sessionID, item, buf, revision, read.layout)
		if err != nil {
			return err
		}
		if !written.Stored {
			continue
		}
		r.mu.Lock()
		r.cache.Put(r.watcher, epoch, sessionID, storedRow{Interest: item, layout: written.Layout}, written.Revision)
		r.mu.Unlock()
		return nil
	}
	return fmt.Errorf("remove topics from %s's interest record: %w after %d attempts",
		sessionID, ErrInterestTopicsNotRemoved, interestWriteAttempts)
}

func first(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
