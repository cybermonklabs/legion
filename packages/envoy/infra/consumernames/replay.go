package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/natsgrant"
)

// The journal's names of replay (holdrecord's table: replay in cutover and selection-recovery,
// replay --final in selection-recovery alone).
const (
	replayCommand      = "replay"
	replayFinalCommand = "replay --final"
)

// replayWrites bounds how many times replay reads K's record again, and repair-sentinel the
// sentinel key's last message, after a write the store refused because the key moved
// (selection.ErrConflict). Under the hold no other writer is connected, so a refusal comes only
// from a write this command could not see; a bound keeps one that repeats from running forever.
const replayWrites = 8

// replay is `consumer-names replay --key K --from SEQ [--final] --hold ID`: it writes K's record
// with start = SEQ and no current, in K's family's store, inside the hold (plan §2, "Recovery is an
// operator step"). The record keeps its retired list, and every name it selected or had in flight
// (its current, and an open operation's candidate and old name) is retired with it, so no name it
// held is selected again and cleanup may delete each: an open operation is settled so, never left
// open under a start it was not reserved at. A value under K that is no record of K is written
// over, retiring nothing of it, and named (replayRead). Everything that can refuse it, the key,
// the store and the stream's sequences, is read before its intent entry, so such a refusal leaves
// the journal as it was; a daemon's store a --daemon-database file names is read by the census's
// rule for the daemon stores, and one it leaves unreconciled or finds gone is such a refusal.
// A refusal after its intent (a read that fails, a write the store refuses
// past its retries, refuses for another reason or does not answer, a done entry whose write fails)
// is its stop (createBucketStopped), naming what the run left.
func (r *run) replay(ctx context.Context) error {
	err := r.replayRun(ctx)
	return connRefusal(r.nc, err)
}

func (r *run) replayRun(ctx context.Context) error {
	if _, err := r.requirePhase(ctx, natsgrant.OperatorPhaseHeld); err != nil {
		return err
	}
	m, err := replayMember(r.o.key)
	if err != nil {
		return err
	}
	nc, _, err := r.conn(ctx)
	if err != nil {
		return err
	}
	// A start past the stream's last sequence + 1 is one the creators' clamp would turn into the
	// stream's first (session_consumers.go create), so the record would not say what its creation
	// does: it is refused, naming the stream's range.
	stream, err := streamInfo(ctx, nc, notifications)
	if err != nil {
		return unknownf(err, "the sequences of %s", notifications)
	}
	if last := stream.State.LastSeq; r.o.from > last+1 {
		return fmt.Errorf("--from %d is past %s's last sequence %d + 1 (its first is %d): a consumer created there starts at the stream's first", r.o.from, notifications, last, stream.State.FirstSeq)
	}
	// A daemon's store a --daemon-database file names is read by the census's rule for the daemon
	// stores, at exactly the address the project's one source names, and written through the pool
	// the rule opened there (daemonStoresByRule): a store the rule leaves unreconciled or finds gone
	// refuses the replay before its intent.
	if project, ok := strings.CutPrefix(m.Family, familyDaemonPrefix); ok {
		if _, named := r.o.daemonDatabase(project); named {
			daemon, err := r.daemonStoreByRule(ctx, project)
			switch {
			case err != nil:
				return err
			case daemon.unreconciled != "":
				return fmt.Errorf("%s's store is unreconciled, so replay writes no record there: %s", m.Family, daemon.unreconciled)
			case daemon.gone != nil:
				return fmt.Errorf("%s's store is gone at the address %s names, so replay writes no record there: %s", m.Family, daemon.addressSource(), daemonGoneText(*daemon.gone))
			}
		}
	}
	store, err := r.store(ctx, m.Family)
	if err != nil {
		return err
	}
	if m.Family == familySession {
		if _, present, err := createBucketStream(ctx, nc, false); err != nil {
			return err
		} else if !present {
			return fmt.Errorf("%w: KV_%s, which create-bucket makes first: replay writes %s's record there", selection.ErrNoStore, selection.SessionBucket, r.o.key)
		}
	} else if _, _, err := store.Read(ctx, m.Logical); err != nil && !errors.Is(err, selection.ErrNoRecord) && !errors.Is(err, selection.ErrMalformedRecord) {
		return fmt.Errorf("read %s's record in %s's store: %w", m.Logical, m.Family, err)
	}

	command := replayCommand
	if r.o.final {
		command = replayFinalCommand
	}
	a, err := r.admitRead(ctx, command, map[string]string{"key": m.Logical, "family": m.Family, "from": formatSeq(r.o.from)})
	if err != nil {
		return err
	}
	var left createBucketLeft
	outputs, err := r.replayHeld(ctx, nc, store, m, &left)
	if err == nil {
		err = a.done(ctx, outputs)
	}
	if err != nil {
		return createBucketStopped(ctx, a, err, &left)
	}
	return nil
}

// replayHeld is replay's work after its intent: K's record read (behind the bucket's barrier for
// the session family) and written over its revision, the changes counted in left. A value that is
// no record of K (replayRead) is written over, and named in its output replaced.
func (r *run) replayHeld(ctx context.Context, nc *nats.Conn, store familyStore, m member, left *createBucketLeft) (map[string]string, error) {
	var written selection.Record
	var replaced string
	for attempt := 1; ; attempt++ {
		var old selection.Record
		var revision uint64
		read := func(ctx context.Context) error {
			var err error
			old, revision, replaced, err = replayRead(ctx, nc, store, m)
			return err
		}
		var err error
		if m.Family == familySession {
			// The bucket barrier, before each read of the bucket (writerhold.ReadBucket): every
			// write acknowledged before the hold is applied on the stream's leader before replay
			// reads the record it overwrites.
			left.barrier = true
			err = behindBarrier(ctx, nc, r.o.holdID, read)
		} else {
			err = read(ctx)
		}
		if err != nil {
			return nil, err
		}
		r.read(fmt.Sprintf("%s's record in %s's store", m.Logical, m.Family))
		written = replayRecord(old, r.o.from, m.Fixed)
		left.unsure = fmt.Sprintf("%s's record in %s's store, if its unanswered write landed", m.Logical, m.Family)
		_, err = store.Write(ctx, m.Logical, written, revision)
		if err == nil {
			left.unsure, left.record = "", fmt.Sprintf("%s's record in %s's store written with start %d", m.Logical, m.Family, r.o.from)
			break
		}
		if errors.Is(err, selection.ErrConflict) {
			left.unsure = ""
		}
		if !errors.Is(err, selection.ErrConflict) || attempt == replayWrites {
			return nil, fmt.Errorf("write %s's record in %s's store: %w", m.Logical, m.Family, err)
		}
	}
	outputs := map[string]string{"key": m.Logical, "family": m.Family, "start": formatSeq(r.o.from),
		"retired": strings.Join(written.Retired, ",")}
	line := fmt.Sprintf("%s: %s's record starts at %d with no current; retired: %s", m.Family, m.Logical, r.o.from, strings.Join(written.Retired, ", "))
	if replaced != "" {
		outputs["replaced"] = replaced
		line += "; replaced a value that is no record of it: " + replaced
	}
	fmt.Fprintln(r.stdout, line)
	return outputs, nil
}

// replayRecord is the record replay writes over old: start from and no current, old's retired
// names kept, and every name old selected or had in flight retired but fixed, the logical's fixed
// name, which no record retires (retire).
func replayRecord(old selection.Record, from uint64, fixed string) selection.Record {
	next := selection.Record{Start: from, Retired: old.Retired, Restore: old.Restore}
	next = retire(next, fixed, old.Current)
	if old.Op != nil {
		next = retire(next, fixed, old.Op.Candidate)
		next = retire(next, fixed, old.Op.Old)
	}
	return next
}

// replayRead reads m's record and the revision a write over it names: 0 for a key never written,
// and, for a session key whose last message is a KV delete or purge marker
// (selection.ErrRecordDeleted), the marker's own sequence, which the selection store does not
// return, read from the stream's leader. A value that is no record of the key
// (selection.ErrMalformedRecord: a document a reply subject stored there, garbage, or another
// key's record copied there), in any family, selects nothing replay can trust: it answers no
// record, so none of its names is retired (the census removes a consumer no record names), the
// revision the store holds it at (replayRevision), and replaced, the store's error naming it.
// Every other read error, a sentinel's included, is a refusal or UNKNOWN.
func replayRead(ctx context.Context, nc *nats.Conn, store familyStore, m member) (record selection.Record, revision uint64, replaced string, err error) {
	record, revision, err = store.Read(ctx, m.Logical)
	switch {
	case err == nil:
		return record, revision, "", nil
	case errors.Is(err, selection.ErrNoRecord):
		return selection.Record{}, 0, "", nil
	case errors.Is(err, selection.ErrRecordDeleted) && m.Family == familySession:
		revision, err = replayRevision(ctx, nc, store, m)
		return selection.Record{}, revision, "", err
	case errors.Is(err, selection.ErrMalformedRecord):
		replaced = err.Error()
		revision, err = replayRevision(ctx, nc, store, m)
		return selection.Record{}, revision, replaced, err
	}
	return selection.Record{}, 0, "", fmt.Errorf("read %s's record in %s's store: %w", m.Logical, m.Family, err)
}

// replayRevision is the revision a write over m's value names where the store answers none: for a
// session key, the sequence of its last message, read from the stream's leader
// (replayLeaderRead); for a Postgres family, the revision of its intake_consumers row
// (selection.PGKey), read by SQL. One that cannot be read is UNKNOWN.
func replayRevision(ctx context.Context, nc *nats.Conn, store familyStore, m member) (uint64, error) {
	if m.Family == familySession {
		last, found, err := replayLeaderRead(ctx, nc, selection.SessionBucket, m.Logical)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, unknownf(nil, "%s's last message in %s: the leader read of it finds none", m.Logical, selection.SessionBucket)
		}
		return last.Seq, nil
	}
	pg, ok := store.(pgFamilyStore)
	if !ok {
		return 0, unknownf(nil, "the revision of %s's row in %s's store: the store is no Postgres store", m.Logical, m.Family)
	}
	revision, err := pgRevision(ctx, pg.db, selection.PGKey(m.Family, m.Logical))
	if err != nil {
		return 0, unknownf(err, "the revision of %s's row in %s's store", m.Logical, m.Family)
	}
	return uint64(revision), nil
}

// replayMessage is a KV key's last message as the stream's leader answers it.
type replayMessage struct {
	Seq     uint64
	Subject string
	Data    []byte
	// Op is the message's KV-Operation header: "DEL" or "PURGE" for a marker, "" for a value.
	Op string
}

// replayLeaderRead reads key's last message in bucket from the leader of the bucket's stream (a
// STREAM.MSG.GET by last_by_subj, which the operator's grant names), and false for a key with no
// message.
func replayLeaderRead(ctx context.Context, nc *nats.Conn, bucket, key string) (replayMessage, bool, error) {
	return replayStreamMessage(ctx, nc, bucket, map[string]any{"last_by_subj": "$KV." + bucket + "." + key})
}

// replayStreamMessage is one STREAM.MSG.GET of bucket's stream, by request, and false for a
// message the stream does not hold (jetstream.JSErrCodeMessageNotFound).
func replayStreamMessage(ctx context.Context, nc *nats.Conn, bucket string, request map[string]any) (replayMessage, bool, error) {
	raw, err := apiRequest(ctx, nc, "$JS.API.STREAM.MSG.GET.KV_"+bucket, request)
	if apiErrorIs(err, jetstream.JSErrCodeMessageNotFound) {
		return replayMessage{}, false, nil
	}
	if err != nil {
		return replayMessage{}, false, unknownf(err, "a message of KV_%s", bucket)
	}
	var answer msgGetAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return replayMessage{}, false, unknownf(err, "a message of KV_%s", bucket)
	}
	m := replayMessage{Seq: answer.Message.Seq, Subject: answer.Message.Subject, Data: answer.Message.Data}
	if m.Op, err = kvOperation(answer.Message.Header); err != nil {
		return replayMessage{}, false, unknownf(err, "the header of message %d of KV_%s", m.Seq, bucket)
	}
	return m, true, nil
}

// msgGetAnswer is a STREAM.MSG.GET answer: the stored message, zero when the answer holds none.
type msgGetAnswer struct {
	Message struct {
		Subject string `json:"subject"`
		Seq     uint64 `json:"seq"`
		Header  []byte `json:"hdrs"`
		Data    []byte `json:"data"`
	} `json:"message"`
}

// kvOperation is the KV operation a stored message's encoded headers hdrs carry
// (kvOperationHeader): "DEL" or "PURGE" for a marker, and "" for a value or no headers.
func kvOperation(hdrs []byte) (string, error) {
	if len(hdrs) == 0 {
		return "", nil
	}
	header, err := nats.DecodeHeadersMsg(hdrs)
	if err != nil {
		return "", err
	}
	return header.Get(kvOperationHeader), nil
}

// replayMember is the member a logical name K belongs to, as `replay --key` and `restore --key`
// name it: a listener durable's record key (selection.DurableKey of listener-<machine id>), one of
// Dispatch's durables, a Legion daemon's legion-go-<project>-<kind>, and otherwise a session id.
// A key holding a '.' is a system key's or a durable key's, never a session's (selection.CheckKey),
// so it names a listener durable or nothing; a bare listener-<machine id> is refused, since its
// record is kept under its durable key and a session record of that name would select nothing.
func replayMember(key string) (member, error) {
	if strings.Contains(key, ".") {
		if err := selection.CheckDurableKey(key); err != nil {
			return member{}, fmt.Errorf("--key %q holds a '.', which only a system key or a listener durable's key (%s<durable>) holds: %w", key, selection.DurableKey(""), err)
		}
		durable, err := durableOf(key)
		if err != nil {
			return member{}, fmt.Errorf("--key %q %w", key, err)
		}
		m, ok := classify(durable)
		if !ok || !strings.HasPrefix(durable, listenerFixedPrefix) || m.Logical != key {
			return member{}, fmt.Errorf("--key %q is the key of %q, which is no listener's durable", key, durable)
		}
		return m, nil
	}
	if m, ok := classify(key); ok && m.Family != familySession {
		return m, nil
	}
	if strings.HasPrefix(key, listenerFixedPrefix) {
		return member{}, fmt.Errorf("--key %q: a listener durable's record is kept under its durable key, --key %s", key, selection.DurableKey(key))
	}
	if err := selection.CheckKey(key); err != nil {
		return member{}, fmt.Errorf("--key: %w", err)
	}
	m, ok := classify(sessionFixedPrefix + key)
	if !ok {
		return member{}, fmt.Errorf("--key %q names no logical name of any family", key)
	}
	return m, nil
}
