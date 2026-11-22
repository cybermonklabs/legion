package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/jsm.go"
	"github.com/nats-io/jsm.go/api"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// The journal's names of create-bucket's two forms (holdrecord's table: the cutover's create-bucket,
// selection-recovery's create-bucket --after-loss).
const (
	createBucketCommand          = "create-bucket"
	createBucketAfterLossCommand = "create-bucket --after-loss"
)

// The hold's selection record (plan §3a, the hold's records): the recovery's inputs, which
// create-bucket --after-loss writes before any restore and every rerun reads.
const (
	createBucketSelectionKind = "selection"
	createBucketSelectionKey  = "inputs"
)

// createBucketNoBackup is the backup_sha256 of a recovery with no backup, in the selection record
// and the sentinel.
const createBucketNoBackup = "none"

// The JetStream API's answers create-bucket reads that nats.go's jetstream package does not name
// (nats-server errors.json): 10130 a restore refused because the stream's name is still taken, the
// leftover of a cut restore; 10049 a restore's store not made yet, which passes. jsm.go's
// api.IsNatsError compares them.
const (
	errRestoreNameInUse = 10130
	errCreatingStore    = 10049
)

// createBucket is `consumer-names create-bucket --hold ID`, the cutover's (plan §3 step 2), or with
// --after-loss the selection recovery's (§2, "Recovery is an operator step"). Neither deletes a
// bucket it did not restore in this hold.
func (r *run) createBucket(ctx context.Context) error {
	var err error
	if r.o.afterLoss {
		err = r.createBucketAfterLoss(ctx)
	} else {
		err = r.createBucketCutover(ctx)
	}
	return connRefusal(r.nc, err)
}

// createBucketReplicas is the selection bucket's copies: --replicas, else the deployment's
// ENVOY_NATS_REPLICAS (bus.ParseNATSReplicas, as every program reads it), else 1.
func (r *run) createBucketReplicas() (int, error) {
	if r.o.replicas > 0 {
		return r.o.replicas, nil
	}
	replicas, err := bus.ParseNATSReplicas(os.Getenv(bus.ReplicasEnvVar))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", bus.ReplicasEnvVar, err)
	}
	return replicas, nil
}

// createBucketCutover creates the selection bucket and its sentinel with B, the cohort's boundary,
// in the cutover hold, before any creator runs in the fresh mode (plan §3 step 2). A rerun after a
// crash may find the bucket: it adopts it only when the bucket's stream has the configuration
// create-bucket sends (createBucketConfigDiff), and the bucket holds no key but holds' barrier
// keys (createBucketBarrierKey), or only its sentinel with this hold's B. With no key the crash
// fell between the create and the sentinel, so it writes the sentinel and goes on; with the
// sentinel it changes nothing. Anything else is UNKNOWN, naming the bucket: a bucket that is not
// this hold's is one no command deletes. A refusal after its intent is its stop
// (createBucketStopped), naming what the run left.
func (r *run) createBucketCutover(ctx context.Context) error {
	if _, err := r.requirePhase(ctx, natsgrant.OperatorPhaseHeld); err != nil {
		return err
	}
	replicas, err := r.createBucketReplicas()
	if err != nil {
		return err
	}
	cohort, _, ok, err := readPrepareCohort(ctx, r.records, r.o.holdID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("hold %s has no cohort record: consumer-names prepare writes it, with the boundary B the bucket's sentinel holds", r.o.holdID)
	}
	if cohort.Boundary == 0 {
		return unknownf(nil, "the cohort record of hold %s holds no boundary B, which is at least 1", r.o.holdID)
	}
	a, err := r.admitRead(ctx, createBucketCommand, map[string]string{"boundary": formatSeq(cohort.Boundary), "replicas": strconv.Itoa(replicas)})
	if err != nil {
		return err
	}
	var left createBucketLeft
	outputs, err := r.createBucketCutoverHeld(ctx, cohort.Boundary, replicas, &left)
	if err == nil {
		err = a.done(ctx, outputs)
	}
	if err != nil {
		return createBucketStopped(ctx, a, err, &left)
	}
	return nil
}

// createBucketCutoverHeld is create-bucket's work after its intent, each change it makes counted
// in left. Its sentinel, want, is the one selection.KVStore.WriteSentinel writes, its own key and
// the session family included, so a sentinel read back compares with it whole.
func (r *run) createBucketCutoverHeld(ctx context.Context, boundary uint64, replicas int, left *createBucketLeft) (map[string]string, error) {
	want := selection.StoreState{Key: selection.SentinelKey, Family: selection.SessionFamily, Boundary: boundary, Hold: r.o.holdID}
	nc, js, err := r.conn(ctx)
	if err != nil {
		return nil, err
	}
	outputs := map[string]string{"bucket": selection.SessionBucket, "boundary": formatSeq(boundary), "replicas": strconv.Itoa(replicas)}
	info, present, err := createBucketStream(ctx, nc, false)
	if err != nil {
		return nil, err
	}
	r.read("the stream of " + selectionStreamName)
	store, err := r.store(ctx, familySession)
	if err != nil {
		return nil, err
	}
	if !present {
		// The create, then the sentinel once the new stream serves its subjects: on three servers
		// a put right after the create finds no responder yet (selection.CreateSessionBucket writes
		// it at once). A crash between the two is the rerun rule's empty bucket.
		if err := createBucketCreate(ctx, nc, js, replicas, left); err != nil {
			return nil, err
		}
		left.unsure = "the sentinel, if its unanswered write landed"
		if err := createBucketFreshWrite(ctx, func() error {
			_, err := store.WriteSentinel(ctx, want, 0)
			return err
		}); err != nil {
			return nil, fmt.Errorf("write the sentinel of %s: %w", selection.SessionBucket, err)
		}
		left.unsure, left.sentinel = "", true
		fmt.Fprintf(r.stdout, "created %s at %d copies, its sentinel with boundary %d\n", selection.SessionBucket, replicas, boundary)
		outputs["bucket_was"] = "absent"
		return outputs, nil
	}
	if diff := createBucketConfigDiff(info.Config, replicas); len(diff) > 0 {
		return nil, unknownf(nil, "selection bucket %s exists, and its configuration is not the one create-bucket sends (%s): no command deletes a bucket another build may own", selection.SessionBucket, strings.Join(diff, "; "))
	}
	var sentinel createBucketSentinelRead
	var found bool
	left.barrier = true
	if err := behindBarrier(ctx, nc, r.o.holdID, func(ctx context.Context) error {
		if info, _, err = createBucketStream(ctx, nc, true); err != nil {
			return err
		}
		if sentinel, found, err = createBucketSentinel(ctx, nc); err != nil || found || len(createBucketKeys(info, r.o.holdID)) > 0 {
			return err
		}
		// The empty bucket this run adopts may hold other holds' barrier keys, each of which must
		// be exactly one (createBucketCheckValues), as every value create-bucket --after-loss adopts
		// is.
		return createBucketCheckValues(ctx, nc, info.State.LastSeq)
	}); err != nil {
		return nil, err
	}
	r.read(selectionStreamName + "'s keys and sentinel behind the bucket barrier")
	keys := createBucketKeys(info, r.o.holdID)
	switch {
	case len(keys) == 0 && !found:
		// The crash fell between the create and the sentinel.
		left.unsure = "the sentinel, if its unanswered write landed"
		if _, err := store.WriteSentinel(ctx, want, 0); err != nil {
			return nil, err
		}
		left.unsure, left.sentinel = "", true
		fmt.Fprintf(r.stdout, "adopted %s, which held no key: its sentinel written with boundary %d\n", selection.SessionBucket, boundary)
		outputs["bucket_was"] = "empty"
	case len(keys) == 1 && keys[0] == selection.SentinelKey && found && sentinel.marker == "" && createBucketJSON(sentinel.state) == createBucketJSON(want):
		fmt.Fprintf(r.stdout, "adopted %s, whose sentinel already holds this hold's boundary %d\n", selection.SessionBucket, boundary)
		outputs["bucket_was"] = "this hold's"
	default:
		return nil, unknownf(nil, "selection bucket %s exists and is not this hold's: it holds %s, where this hold's holds no key or only its sentinel %s; no command deletes a bucket another build may own",
			selection.SessionBucket, createBucketHeld(keys, sentinel, found), createBucketJSON(want))
	}
	return outputs, nil
}

// createBucketLeft is what a run of create-bucket, create-bucket --after-loss, replay or
// repair-sentinel has changed outside the journal since its intent, which the stop of a refusal
// names beside what every earlier run of its command in the hold left (createBucketStopped), so
// the way out of the hold knows what to undo or keep.
type createBucketLeft struct {
	// selectionRecord: the hold's selection record, written.
	selectionRecord bool
	// barrier: this hold's barrier key put in the selection bucket (writerhold.ReadBucket,
	// barrierLeft), set right before the put, whose answer may be lost after it landed, and
	// cleared when the run then deleted the bucket that held it (bucketDeleted).
	barrier bool
	// created and restored: the selection bucket created empty, or restored from a snapshot, by
	// this run; deleted: a cut restore's leftover deleted; unloaded: a restored snapshot deleted
	// again, as it held a value that is no exact value of its key's kind (createBucketCheckRestored).
	created, restored, deleted, unloaded bool
	// replayed counts a walk's records put in the bucket.
	replayed int
	// sentinel: the bucket's sentinel written.
	sentinel bool
	// record names the record replay wrote.
	record string
	// unsure is a write whose answer was lost, which may or may not have landed.
	unsure string
}

// String names each change, or that the run changed nothing outside the journal.
func (l *createBucketLeft) String() string {
	if !l.changed() {
		return "nothing outside the journal"
	}
	var parts []string
	add := func(set bool, text string) {
		if set {
			parts = append(parts, text)
		}
	}
	add(l.selectionRecord, "the hold's selection record")
	add(l.deleted, "the leftover of a cut restore or load of "+selectionStreamName+" deleted")
	add(l.created, "the bucket "+selectionStreamName+" created")
	add(l.restored, "the bucket "+selectionStreamName+" restored from the backup")
	add(l.unloaded, "the bucket "+selectionStreamName+" restored from the backup and deleted again")
	add(l.replayed > 0, fmt.Sprintf("%d of the walk's records put in %s", l.replayed, selectionStreamName))
	add(l.barrier, barrierLeft)
	add(l.sentinel, "the sentinel of "+selectionStreamName+" written")
	add(l.record != "", l.record)
	add(l.unsure != "", l.unsure)
	return strings.Join(parts, "; ")
}

// changed reports whether the run changed anything outside the journal, a write whose answer was
// lost among them: one change String names.
func (l *createBucketLeft) changed() bool {
	return l.selectionRecord || l.barrier || l.created || l.restored || l.deleted || l.unloaded || l.replayed > 0 ||
		l.sentinel || l.record != "" || l.unsure != ""
}

// bucketDeleted records that the run deleted the selection bucket, and with it this hold's barrier
// key, which then is no change the run left (the control's earlier rule,
// leftRuleSet.barrierAfterDelete, keeps counting it).
func (l *createBucketLeft) bucketDeleted() {
	if !leftRules.barrierAfterDelete {
		l.barrier = false
	}
}

// createBucketStopped ends a refusal after the command's intent with its stop (admission.stop,
// which writes the stop entry where the workflow's table makes the command Stoppable), the reason
// naming what this run left, and answers err with that named: an UNKNOWN stays UNKNOWN. The stop
// records what every run of the command in this hold left (admission.leftAfter):
// holdrecord.LeftUnchanged only when this run and each earlier one changed nothing, so a barrier
// key or a bucket an earlier run left still counts, and an UNKNOWN's under leftUnknown, never
// unchanged. Under the control's earlier rule (leftRuleSet.ownRunOnly) it records this run alone.
func createBucketStopped(ctx context.Context, a *admission, err error, left *createBucketLeft) error {
	if !stopsAfter(err) {
		return fmt.Errorf("%w; it left %s", err, left)
	}
	reason := fmt.Sprintf("%v; it left %s", err, left)
	var recorded string
	switch {
	case !leftRules.ownRunOnly:
		recorded = a.leftAfter(ctx, err, !left.changed(), left.String())
	case isUnknown(err) && !leftRules.unknownAsAccount:
		recorded = leftUnknown(a.r.reads, err, left.String())
	case left.changed():
		recorded = left.String()
	default:
		recorded = holdrecord.LeftUnchanged
	}
	if serr := a.stop(ctx, reason, recorded); errors.Unwrap(serr) != nil {
		return fmt.Errorf("%w; it left %s; and its stop entry: %w", err, left, errors.Unwrap(serr))
	}
	return fmt.Errorf("%w; it left %s", err, left)
}

// selectionKeyPrefix begins the subject of every key of the selection bucket, $KV.<bucket>.<key>,
// as nats.go's KV API names a key's subject.
const selectionKeyPrefix = "$KV." + selection.SessionBucket + "."

// createBucketStream reads the selection bucket's stream, with each key's subject when subjects is
// set, and false for a stream the server does not hold (jetstream.JSErrCodeStreamNotFound). Any
// other failure is UNKNOWN: a configuration that cannot be read is never a match.
func createBucketStream(ctx context.Context, nc *nats.Conn, subjects bool) (*jetstream.StreamInfo, bool, error) {
	var body any
	if subjects {
		body = map[string]string{"subjects_filter": selectionKeyPrefix + ">"}
	}
	raw, err := apiRequest(ctx, nc, "$JS.API.STREAM.INFO."+selectionStreamName, body)
	if apiErrorIs(err, jetstream.JSErrCodeStreamNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, unknownf(err, "the stream of selection bucket %s", selection.SessionBucket)
	}
	var info jetstream.StreamInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, false, unknownf(err, "the stream of selection bucket %s", selection.SessionBucket)
	}
	return &info, true, nil
}

// createBucketStreamConfig is the stream configuration create-bucket sends for the selection
// bucket at replicas copies: selection.SessionBucketConfig as nats.go's jetstream.CreateKeyValue
// turns a bucket's configuration into its stream's (nats.go v1.55.0 jetstream/kv.go
// prepareKeyValueConfig).
func createBucketStreamConfig(replicas int) jetstream.StreamConfig {
	kv := selection.SessionBucketConfig(replicas)
	orMinus := func(v int64) int64 {
		if v == 0 {
			return -1
		}
		return v
	}
	return jetstream.StreamConfig{
		Subjects:          []string{"$KV." + kv.Bucket + ".>"},
		Retention:         jetstream.LimitsPolicy,
		Storage:           kv.Storage,
		Replicas:          max(kv.Replicas, 1),
		MaxMsgsPerSubject: int64(max(kv.History, 1)),
		MaxAge:            kv.TTL,
		MaxBytes:          orMinus(kv.MaxBytes),
		MaxMsgSize:        int32(orMinus(int64(kv.MaxValueSize))),
		Discard:           jetstream.DiscardNew,
		AllowRollup:       true,
		DenyDelete:        true,
		DenyPurge:         false,
		AllowDirect:       true,
	}
}

// createBucketConfigDiff names each field of got, a present bucket's stream configuration, that
// differs from the one create-bucket sends at replicas copies, in the fields the plan's rerun rule
// compares (§3 step 2): subjects, retention, storage, replicas, max_msgs_per_subject (the
// bucket's history), max_age, max_bytes, max_msg_size, discard, allow_rollup_hdrs, deny_delete,
// deny_purge and allow_direct.
func createBucketConfigDiff(got jetstream.StreamConfig, replicas int) []string {
	want := createBucketStreamConfig(replicas)
	var diff []string
	field := func(name string, same bool, g, w any) {
		if !same {
			diff = append(diff, fmt.Sprintf("%s %v, not %v", name, g, w))
		}
	}
	field("subjects", slices.Equal(got.Subjects, want.Subjects), got.Subjects, want.Subjects)
	field("retention", got.Retention == want.Retention, got.Retention, want.Retention)
	field("storage", got.Storage == want.Storage, got.Storage, want.Storage)
	field("num_replicas", got.Replicas == want.Replicas, got.Replicas, want.Replicas)
	field("max_msgs_per_subject", got.MaxMsgsPerSubject == want.MaxMsgsPerSubject, got.MaxMsgsPerSubject, want.MaxMsgsPerSubject)
	field("max_age", got.MaxAge == want.MaxAge, got.MaxAge, want.MaxAge)
	field("max_bytes", got.MaxBytes == want.MaxBytes, got.MaxBytes, want.MaxBytes)
	field("max_msg_size", got.MaxMsgSize == want.MaxMsgSize, got.MaxMsgSize, want.MaxMsgSize)
	field("discard", got.Discard == want.Discard, got.Discard, want.Discard)
	field("allow_rollup_hdrs", got.AllowRollup == want.AllowRollup, got.AllowRollup, want.AllowRollup)
	field("deny_delete", got.DenyDelete == want.DenyDelete, got.DenyDelete, want.DenyDelete)
	field("deny_purge", got.DenyPurge == want.DenyPurge, got.DenyPurge, want.DenyPurge)
	field("allow_direct", got.AllowDirect == want.AllowDirect, got.AllowDirect, want.AllowDirect)
	return diff
}

// createBucketKeys are the keys info, the bucket's stream read with its subjects, holds, but every
// hold's barrier key (createBucketBarrierKey), which a held command puts before it reads and every
// reader ignores. A stream whose subjects the answer does not hold in full (num_subjects past the
// subjects listed) answers one key more than it lists, so it is never read as empty.
func createBucketKeys(info *jetstream.StreamInfo, holdID string) []string {
	var keys []string
	for subject := range info.State.Subjects {
		if key := strings.TrimPrefix(subject, selectionKeyPrefix); !createBucketBarrierKey(key, holdID) {
			keys = append(keys, key)
		}
	}
	if listed := uint64(len(info.State.Subjects)); info.State.NumSubjects > listed {
		keys = append(keys, fmt.Sprintf("(%d keys the answer does not list)", info.State.NumSubjects-listed))
	}
	slices.Sort(keys)
	return keys
}

// createBucketBarrierKey reports whether key is a hold's barrier key, hold.barrier.<hold id>
// (strictBarrierPrefix), which create-bucket and create-bucket --after-loss read as no key of the
// bucket: no command reads one as a record, and only the whole bucket's delete removes one
// (holdrecord.Barrier). So a bucket with no sentinel that holds only another hold's barrier key,
// as a run of that hold leaves it when it put its barrier and stopped short of the sentinel, is
// the empty bucket each of them takes. Under the earlier rule a row's control sets
// (createBucketIgnoresEveryBarrier unset) only this hold's key, holdID's, is one.
func createBucketBarrierKey(key, holdID string) bool {
	if !createBucketIgnoresEveryBarrier {
		return key == strictBarrierPrefix+holdID
	}
	return strings.HasPrefix(key, strictBarrierPrefix)
}

// createBucketSentinelRead is the bucket's sentinel as its stream's leader answers it.
type createBucketSentinelRead struct {
	state selection.StoreState
	// revision is its message's sequence, which a write over it names.
	revision uint64
	// marker is a KV delete or purge marker found in its place, which no code writes.
	marker string
}

// createBucketSentinel reads the selection bucket's sentinel from its stream's leader, and false
// for a bucket that holds none. A sentinel that is no exact selection.StoreState
// (createBucketExactValue) is refused, naming the key and its sequence.
func createBucketSentinel(ctx context.Context, nc *nats.Conn) (createBucketSentinelRead, bool, error) {
	last, found, err := replayLeaderRead(ctx, nc, selection.SessionBucket, selection.SentinelKey)
	if err != nil || !found {
		return createBucketSentinelRead{}, false, err
	}
	state, err := createBucketExactValue(selection.SentinelKey, last.Op, last.Data)
	if err != nil {
		return createBucketSentinelRead{}, false, fmt.Errorf("selection bucket %s's key %s at sequence %d %w", selection.SessionBucket, selection.SentinelKey, last.Seq, err)
	}
	read := createBucketSentinelRead{revision: last.Seq, marker: last.Op}
	if state != nil {
		read.state = *state
	}
	return read, true, nil
}

// createBucketHeld names what a bucket that is not adopted holds, for its refusal.
func createBucketHeld(keys []string, sentinel createBucketSentinelRead, found bool) string {
	text := fmt.Sprintf("%d keys (%s)", len(keys), strings.Join(keys, ", "))
	switch {
	case found && sentinel.marker != "":
		text += ", its sentinel's last message a " + sentinel.marker + " marker"
	case found:
		text += ", its sentinel " + createBucketJSON(sentinel.state)
	default:
		text += ", no sentinel"
	}
	return text
}

func createBucketJSON(state selection.StoreState) string {
	raw, _ := json.Marshal(state)
	return string(raw)
}

// createBucketInputs is the hold's selection record (kind selection, key inputs, plan §2): the
// recovery's inputs, written before any restore. A rerun under the hold takes its inputs from it
// and refuses others.
type createBucketInputs struct {
	// ReplayFrom is where a key with no record starts while replay mode lasts.
	ReplayFrom uint64 `json:"replay_from"`
	// BackupSHA256 is the backup's sha256 (createBucketReadBackup), or createBucketNoBackup.
	BackupSHA256 string `json:"backup_sha256"`
	// Hold is the recovery's hold id.
	Hold string `json:"hold"`
	// Format is the backup's format, createBucketFormatSnapshot or createBucketFormatWalk; empty
	// with no backup. A record written before formats were named holds none for a snapshot.
	Format string `json:"format,omitempty"`
	// Archive is a snapshot's recorded state, which its restore reproduces, so a rerun that finds
	// the bucket can tell a restore of it from anything else without the backup at hand; nil with
	// no backup and for a walk, whose load starts the sequences over, so a rerun tells it by its
	// records with the walk at hand (createBucketWalkHolds).
	Archive *createBucketArchive `json:"archive,omitempty"`
}

// format is in's backup format: "" with no backup, and a snapshot for a record that names none.
func (in createBucketInputs) format() string {
	switch {
	case in.BackupSHA256 == createBucketNoBackup:
		return ""
	case in.Format == "":
		return createBucketFormatSnapshot
	}
	return in.Format
}

// createBucketArchive is a snapshot's recorded stream state (jsm.go's backup.json), which its
// restore reproduces. A walk has none: its load publishes each record again (kvrecords).
type createBucketArchive struct {
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	Msgs     uint64 `json:"messages"`
}

// createBucketReadInputs reads the hold's selection record with its gen, and false for none.
func (r *run) createBucketReadInputs(ctx context.Context) (createBucketInputs, uint64, bool, error) {
	return readHoldRecord[createBucketInputs](ctx, r.records, r.o.holdID, createBucketSelectionKind, createBucketSelectionKey,
		fmt.Sprintf("the selection record of hold %s", r.o.holdID))
}

// createBucketAfterLoss is `create-bucket --after-loss`, in a selection-recovery hold: the
// selection bucket made again after it was lost, from a backup (--backup: a snapshot or a walk,
// createBucketReadBackup) or empty, with its sentinel in replay mode (plan §2). Its inputs,
// replay_from (--replay-from, by default the notification stream's first sequence), the backup's
// sha256 and format or "none", and the hold id, are the hold's selection record, written before
// any restore; a rerun takes them from that record and refuses others. The sentinel then holds
// them, with the session consumers no record selects, listed under the hold after the backup is
// loaded (the orphans), the broker's cutover state of the session family, and the backup's
// boundary. A bucket it finds present is adopted by the rules of "Adopting a completed recovery"
// (createBucketAdopt), and never restored again. A refusal after its intent is its stop
// (createBucketStopped), naming what the run left.
func (r *run) createBucketAfterLoss(ctx context.Context) error {
	if _, err := r.requirePhase(ctx, natsgrant.OperatorPhaseHeld); err != nil {
		return err
	}
	replicas, err := r.createBucketReplicas()
	if err != nil {
		return err
	}
	var backup *createBucketBackup
	if r.o.backup != "" {
		if backup, err = createBucketReadBackup(r.o.backup); err != nil {
			return err
		}
	}
	nc, _, err := r.conn(ctx)
	if err != nil {
		return err
	}
	in, gen, recorded, err := r.createBucketReadInputs(ctx)
	if err != nil {
		return err
	}
	if recorded {
		if err := createBucketSameInputs(in, r.o.replayFrom, backup); err != nil {
			return err
		}
	} else {
		in = createBucketInputs{ReplayFrom: r.o.replayFrom, BackupSHA256: createBucketNoBackup, Hold: r.o.holdID}
		if in.ReplayFrom == 0 {
			stream, err := streamInfo(ctx, nc, notifications)
			if err != nil {
				return unknownf(err, "the first sequence of %s, the default --replay-from", notifications)
			}
			// A start of 0 is refused (consumer.go), and an empty stream reports first_seq 0.
			in.ReplayFrom = max(1, stream.State.FirstSeq)
		}
		if backup != nil {
			in.BackupSHA256, in.Format, in.Archive = backup.sha256, backup.format, backup.archive
		}
	}
	a, err := r.admitRead(ctx, createBucketAfterLossCommand, map[string]string{"replay_from": formatSeq(in.ReplayFrom),
		"backup_sha256": in.BackupSHA256, "replicas": strconv.Itoa(replicas)})
	if err != nil {
		return err
	}
	var left createBucketLeft
	outputs, err := r.createBucketAfterLossHeld(ctx, a, in, gen, recorded, backup, replicas, &left)
	if err == nil {
		err = a.done(ctx, outputs)
	}
	if err != nil {
		return createBucketStopped(ctx, a, err, &left)
	}
	return nil
}

// createBucketAfterLossHeld is create-bucket --after-loss's work after its intent, each change it
// makes counted in left.
func (r *run) createBucketAfterLossHeld(ctx context.Context, a *admission, in createBucketInputs, gen uint64, recorded bool, backup *createBucketBackup, replicas int, left *createBucketLeft) (map[string]string, error) {
	nc, js, err := r.conn(ctx)
	if err != nil {
		return nil, err
	}
	if !recorded {
		value, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		if _, err := r.records.PutRecord(ctx, r.o.holdID, createBucketSelectionKind, createBucketSelectionKey, gen, value); err != nil {
			return nil, fmt.Errorf("write the hold's selection record: %w", err)
		}
		left.selectionRecord = true
	}
	store, err := r.store(ctx, familySession)
	if err != nil {
		return nil, err
	}
	// Step 1 of the postcheck: every protected stream, read by name before the first mutation,
	// absent only where the selection recovery declares it (the selection bucket). The held
	// re-read took it just before the intent (r.heldStreams); a run whose env replaces the re-read
	// reads it here.
	step1 := r.heldStreams
	if step1 == nil {
		if step1, err = writerhold.ReadProtectedStreams(ctx, nc, heldExpectedAbsent(holdrecord.WorkflowSelectionRecovery)); err != nil {
			return nil, hubError(err, "the held re-read, the postcheck's step 1")
		}
	}
	r.read("the protected streams, the postcheck's step 1")
	outcome, made, err := r.createBucketRecover(ctx, a, nc, js, store, in, backup, replicas, step1, left)
	if err != nil {
		return nil, err
	}
	if err := r.createBucketPostcheck(ctx, nc, step1, made); err != nil {
		return nil, err
	}
	r.read("the postcheck")
	fmt.Fprintf(r.stdout, "%s: %s; replay from %d, backup %s; postcheck passed\n", selection.SessionBucket, outcome, in.ReplayFrom, in.BackupSHA256)
	return map[string]string{"bucket": selection.SessionBucket, "replay_from": formatSeq(in.ReplayFrom),
		"backup_sha256": in.BackupSHA256, "outcome": outcome, "change": string(made.kind)}, nil
}

// createBucketMade is create-bucket --after-loss's own account of the selection bucket, which its
// postcheck compares (writerhold.Change): what it did to it, the reading its writes count from, and
// the sequences of its own writes.
type createBucketMade struct {
	// kind is writerhold.ChangeKept for a bucket present at step 1 or restored from a snapshot by
	// this run (whose writes count from the restore, base), and writerhold.ChangeRecreated for one
	// it created, empty or for a walk's replay.
	kind writerhold.ChangeKind
	// base is the bucket's reading the writes count from: step 1's, or the one right after this
	// run's restore. A created bucket counts from sequence 0.
	base writerhold.StreamReading
	// own are the sequences this run's writes were acknowledged at: a walk's replayed records and
	// the sentinel.
	own map[uint64]bool
}

// createBucketPostcheck is create-bucket --after-loss's postcheck (plan §2, "runs its postcheck
// and completes the hold"; §3a, "The postcheck, per stream incarnation and per operation"), after
// its last mutation and before its done entry, which completes the hold: writerhold.Postcheck
// over every protected stream against step1, with the selection bucket's change as made states it.
// The bucket's writes are counted from its base: every sequence past it must be this run's own, a
// put of the hold's barrier key (writerhold.ReadBucket may put it more than once), a write whose
// acknowledged sequence made records (a walk's replayed record, the sentinel), or a sequence the
// next put of the same key replaced (history 1 keeps one message a key). A message there of any
// other key is a write this command did not make, and stops it, naming the bucket. A restored
// bucket is compared from its reading right after the restore, which the restore's own check
// holds to the archive's state (createBucketRestore). A finding stops the command before its done
// entry, so the hold stays incomplete.
func (r *run) createBucketPostcheck(ctx context.Context, nc *nats.Conn, step1 []writerhold.StreamReading, made createBucketMade) error {
	info, present, err := createBucketStream(ctx, nc, false)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("the postcheck: %s is absent after the command", selectionStreamName)
	}
	from := made.base.LastSeq
	if made.kind == writerhold.ChangeRecreated {
		from = 0
	}
	last := info.State.LastSeq
	if bound := uint64(len(made.own)) + createBucketArchiveMaxGap; last < from || last-from > bound {
		return fmt.Errorf("the postcheck: %s's last_seq is %d, and this command's writes counted from %d are at most %d", selectionStreamName, last, from, bound)
	}
	barrier := selectionKeyPrefix + strictBarrierPrefix + r.o.holdID
	for seq := from + 1; seq <= last; seq++ {
		m, held, err := replayStreamMessage(ctx, nc, selection.SessionBucket, map[string]any{"seq": seq})
		if err != nil {
			return err
		}
		if held && m.Subject != barrier && !made.own[seq] {
			return fmt.Errorf("the postcheck: %s holds at sequence %d a write of %s, which this command did not make", selectionStreamName, seq, m.Subject)
		}
	}
	readings := make([]writerhold.StreamReading, len(step1))
	for i, reading := range step1 {
		if reading.Name == selectionStreamName && made.kind == writerhold.ChangeKept {
			reading = made.base
		}
		readings[i] = reading
	}
	changes := map[string]writerhold.Change{selectionStreamName: {Kind: made.kind, Writes: last - from}}
	if _, err := writerhold.Postcheck(ctx, nc, readings, changes); err != nil {
		return hubError(err, "the held re-read, the postcheck")
	}
	return nil
}

// createBucketReading is the selection bucket's reading now, as writerhold's step 1 records a
// stream: its created time, last sequence and message count.
func createBucketReading(ctx context.Context, nc *nats.Conn) (writerhold.StreamReading, error) {
	info, present, err := createBucketStream(ctx, nc, false)
	if err != nil {
		return writerhold.StreamReading{}, err
	}
	if !present {
		return writerhold.StreamReading{}, unknownf(nil, "selection bucket %s is absent after it was made", selection.SessionBucket)
	}
	return writerhold.StreamReading{Name: selectionStreamName, Created: info.Created, LastSeq: info.State.LastSeq, Messages: info.State.Msgs}, nil
}

// createBucketSameInputs refuses a rerun's flags that name other inputs than the hold's selection
// record: a --replay-from or a --backup given that is not the record's. A flag left off is taken
// from the record.
func createBucketSameInputs(in createBucketInputs, replayFrom uint64, backup *createBucketBackup) error {
	var others []string
	if replayFrom != 0 && replayFrom != in.ReplayFrom {
		others = append(others, fmt.Sprintf("--replay-from %d, where it holds %d", replayFrom, in.ReplayFrom))
	}
	if backup != nil && backup.sha256 != in.BackupSHA256 {
		others = append(others, fmt.Sprintf("--backup %s of sha256 %s, where it holds %s", backup.dir, backup.sha256, in.BackupSHA256))
	}
	if len(others) > 0 {
		return fmt.Errorf("this recovery's inputs are its hold's selection record, and this run names others: %s", strings.Join(others, "; "))
	}
	return nil
}

// createBucketRecover makes or adopts the selection bucket for in, and answers what it did and its
// account of the bucket for the postcheck (createBucketMade), from step1, the reading at its start,
// each change it makes counted in left. Before each mutation step (the restore, the create or the
// load of a walk, a leftover's delete, the sentinel's write) it runs the held re-read again
// (admission.reread, plan §3a "Every held command"). Every value it loads or adopts is an exact
// value of its key's kind (createBucketExactValue): a walk's, as the backup is read; a snapshot's,
// once restored (createBucketCheckRestored); and a bucket present at its start, before it adopts
// it (createBucketCheckValues).
func (r *run) createBucketRecover(ctx context.Context, a *admission, nc *nats.Conn, js jetstream.JetStream, store familyStore, in createBucketInputs, backup *createBucketBackup, replicas int, step1 []writerhold.StreamReading, left *createBucketLeft) (string, createBucketMade, error) {
	made := createBucketMade{kind: writerhold.ChangeKept, own: map[uint64]bool{}}
	for _, reading := range step1 {
		if reading.Name == selectionStreamName {
			made.base = reading
		}
	}
	// restored: this run loaded the backup; checked: this run made the bucket, so each of its values
	// is checked already.
	restored, checked := false, false
	if made.base.Absent {
		if err := a.reread(ctx); err != nil {
			return "", made, err
		}
		var err error
		if made, err = r.createBucketMake(ctx, nc, js, in, backup, replicas, left); err != nil {
			return "", made, err
		}
		if err := r.createBucketCheckRestored(ctx, a, nc, in, left); err != nil {
			return "", made, err
		}
		restored, checked = in.format() != "", true
	}
	// A leftover of this recovery's own cut restore, or a bucket its cut walk load left partly
	// loaded, is deleted and made again once; a bucket that is still not adopted after that stops
	// the command.
	for range 2 {
		var sentinel createBucketSentinelRead
		var found bool
		var adopt createBucketAdoption
		var orphans []string
		// Every read of the bucket, the decision's and the orphans' records, is behind the barrier.
		left.barrier = true
		err := behindBarrier(ctx, nc, r.o.holdID, func(ctx context.Context) error {
			info, present, err := createBucketStream(ctx, nc, true)
			if err != nil {
				return err
			}
			if !present {
				return unknownf(nil, "selection bucket %s is absent after its barrier", selection.SessionBucket)
			}
			if sentinel, found, err = createBucketSentinel(ctx, nc); err != nil {
				return err
			}
			if adopt, err = createBucketAdopt(ctx, nc, info, sentinel, found, in, backup, r.o.holdID); err != nil {
				return err
			}
			if (adopt.action == createBucketAdopted || adopt.action == createBucketWriteSentinel) && !checked {
				if err := createBucketCheckValues(ctx, nc, info.State.LastSeq); err != nil {
					return err
				}
			}
			if adopt.action == createBucketWriteSentinel {
				orphans, err = createBucketOrphans(ctx, nc, store)
			}
			return err
		})
		if err != nil {
			return "", made, err
		}
		r.read(selectionStreamName + "'s keys and sentinel behind the bucket barrier")
		switch adopt.action {
		case createBucketAdopted:
			return adopt.why, made, nil
		case createBucketStop:
			return "", made, errors.New(adopt.why)
		case createBucketRestoreAgain:
			if backup == nil {
				return "", made, fmt.Errorf("%s; the selection record names backup %s, and this run was given no --backup to restore it from", adopt.why, in.BackupSHA256)
			}
			if err := a.reread(ctx); err != nil {
				return "", made, err
			}
			left.deleted = true
			if err := createBucketDeleteLeftover(ctx, nc); err != nil {
				return "", made, err
			}
			// The barrier this loop put went with the leftover; the next pass puts it again.
			left.bucketDeleted()
			if made, err = r.createBucketMake(ctx, nc, js, in, backup, replicas, left); err != nil {
				return "", made, err
			}
			if err := r.createBucketCheckRestored(ctx, a, nc, in, left); err != nil {
				return "", made, err
			}
			restored, checked = true, true
			continue
		}
		state, err := r.createBucketSentinelState(ctx, in, backup, sentinel, found, adopt, orphans)
		if err != nil {
			return "", made, err
		}
		if err := a.reread(ctx); err != nil {
			return "", made, err
		}
		left.unsure = "the sentinel, if its unanswered write landed"
		seq, err := store.WriteSentinel(ctx, state, adopt.revision)
		if err != nil {
			return "", made, fmt.Errorf("write the sentinel of %s: %w", selection.SessionBucket, err)
		}
		left.unsure, left.sentinel = "", true
		made.own[seq] = true
		if restored {
			return "restored and its sentinel written", made, nil
		}
		return adopt.why + "; its sentinel written", made, nil
	}
	return "", made, fmt.Errorf("selection bucket exists and is not this recovery's: %s is still not this recovery's after its restore was made again", selection.SessionBucket)
}

// createBucketCheckRestored checks each value of the snapshot this run just restored
// (createBucketCheckValues): a snapshot's values can be read only once restored, as its archive
// holds the server's own block files. A restored bucket holding a value that is no exact value of
// its key's kind is deleted again (createBucketDeleteLeftover, after the held re-read), so the
// refused load leaves no bucket, and the command stops naming the key and its sequence. With no
// snapshot it does nothing: a walk's values are checked as it is read.
func (r *run) createBucketCheckRestored(ctx context.Context, a *admission, nc *nats.Conn, in createBucketInputs, left *createBucketLeft) error {
	if in.format() != createBucketFormatSnapshot {
		return nil
	}
	info, present, err := createBucketStream(ctx, nc, false)
	if err != nil {
		return err
	}
	if !present {
		return unknownf(nil, "selection bucket %s is absent after its restore", selection.SessionBucket)
	}
	refused := createBucketCheckValues(ctx, nc, info.State.LastSeq)
	if refused == nil {
		return nil
	}
	if err := a.reread(ctx); err != nil {
		return fmt.Errorf("%w; and the held re-read before the restored bucket's delete: %w", refused, err)
	}
	if err := createBucketDeleteLeftover(ctx, nc); err != nil {
		return fmt.Errorf("%w; and the delete of the restored bucket: %w", refused, err)
	}
	left.restored, left.unloaded = false, true
	return refused
}

// createBucketRestoresLeftover is the leftover rule of "Adopting a completed recovery": an empty
// bucket with no sentinel, after a non-empty archive, is this recovery's cut restore's, deleted and
// restored again. It is a var so its row's control can run the command without the rule, which
// then reads the leftover as another recovery's bucket and stops.
var createBucketRestoresLeftover = true

// createBucketIgnoresEveryBarrier is createBucketBarrierKey's rule: every hold's barrier key is no
// key of the bucket's. It is a var so its row's control can run the command under the earlier
// rule, which took this hold's key alone for none, and so read a bucket holding another hold's
// barrier key and no sentinel as not this hold's, which no command of this package then took.
var createBucketIgnoresEveryBarrier = true

// What createBucketAdopt decides for a present bucket.
const (
	// createBucketAdopted: the bucket is this recovery's, with its sentinel: nothing is written.
	createBucketAdopted = iota + 1
	// createBucketWriteSentinel: the bucket is this recovery's or is adopted; its sentinel is
	// written over revision.
	createBucketWriteSentinel
	// createBucketRestoreAgain: the bucket is this recovery's leftover, the empty stream of its
	// cut restore or the partly loaded bucket of its cut walk load: deleted and made again.
	createBucketRestoreAgain
	// createBucketStop: the bucket is not this recovery's; why is the refusal.
	createBucketStop
)

// createBucketAdoption is createBucketAdopt's decision.
type createBucketAdoption struct {
	action int
	why    string
	// revision is the sentinel's, which its write names (0: none).
	revision uint64
	// lowered is set where an earlier recovery's sentinel is adopted with this recovery's lower
	// replay_from.
	lowered bool
}

// createBucketAdopt decides what a recovery does with the selection bucket it finds present, read
// after its barrier: info its stream with its subjects, and its sentinel (plan §2, "Adopting a
// completed recovery"). It never restores over a present bucket and never raises a lower
// replay_from:
//   - a sentinel that names the selection record's inputs is this recovery's: adopted as it is,
//     its orphan list and every record replay wrote kept;
//   - for a walk, whose load publishes each key's last value again and so starts the sequences
//     over, a bucket with no sentinel is told by each key's last value and the count of its keys,
//     never by its last_seq (plan §2, createBucketWalkHolds): when each of its keys is one of the
//     walk's holding the walk's value (or a hold's barrier key, createBucketBarrierKey), it is this
//     recovery's load, adopted with its sentinel written when it holds every key of the walk, and
//     otherwise, its keys a subset of the walk's, the leftover of its cut load, deleted and loaded
//     again;
//   - for a snapshot, a bucket whose stream holds exactly the archive, and since then only this
//     hold's barrier, is this recovery's restore: adopted, and its sentinel written;
//   - an empty bucket (no key but the barrier) with no sentinel is, after a non-empty archive, the
//     leftover of this recovery's cut restore, which is deleted and restored again, and with no
//     backup the bucket this recovery made: adopted, and its sentinel written;
//   - another recovery's sentinel in replay mode (another backup or hold, or another replay_from)
//     is adopted with the lower replay_from kept, and the command stops when its own inputs would
//     raise it;
//   - anything else stops the command with "selection bucket exists and is not this recovery's".
func createBucketAdopt(ctx context.Context, nc *nats.Conn, info *jetstream.StreamInfo, sentinel createBucketSentinelRead, found bool, in createBucketInputs, backup *createBucketBackup, holdID string) (createBucketAdoption, error) {
	keys := createBucketKeys(info, holdID)
	s := sentinel.state
	if found && sentinel.marker == "" && s.ReplayFrom == in.ReplayFrom && s.BackupSHA256 == in.BackupSHA256 && s.Hold == in.Hold {
		return createBucketAdoption{action: createBucketAdopted, why: "adopted: its sentinel names this recovery's inputs"}, nil
	}
	if !found && in.format() == createBucketFormatWalk {
		if backup == nil || backup.walk == nil {
			return createBucketAdoption{}, fmt.Errorf("selection bucket %s holds no sentinel, and this recovery's walk %s, which tells its replay from anything else, was not given with --backup", selection.SessionBucket, in.BackupSHA256)
		}
		held, extra, foreign, err := createBucketWalkHolds(ctx, nc, info, backup.walk, holdID)
		switch {
		case err != nil:
			return createBucketAdoption{}, err
		case foreign != "":
			return createBucketAdoption{action: createBucketStop, why: fmt.Sprintf("selection bucket exists and is not this recovery's: %s's key %s holds another message than the walk's", selection.SessionBucket, foreign)}, nil
		case extra > 0:
			return createBucketAdoption{action: createBucketStop, why: fmt.Sprintf("selection bucket exists and is not this recovery's: %s holds %d keys that are no record of the walk (%s)", selection.SessionBucket, extra, createBucketHeld(keys, sentinel, found))}, nil
		case len(held) == len(backup.walk.Entries):
			return createBucketAdoption{action: createBucketWriteSentinel, why: fmt.Sprintf("adopted: it holds the walk's %d records as replayed", len(held))}, nil
		}
		return createBucketAdoption{action: createBucketRestoreAgain,
			why: fmt.Sprintf("%s holds %d of the walk's %d records and no sentinel, the leftover of this recovery's cut load", selection.SessionBucket, len(held), len(backup.walk.Entries))}, nil
	}
	if in.format() == createBucketFormatSnapshot && in.Archive != nil && in.Archive.Msgs > 0 {
		intact, err := createBucketArchiveIntact(ctx, nc, info, *in.Archive, holdID)
		if err != nil {
			return createBucketAdoption{}, err
		}
		if intact {
			return createBucketAdoption{action: createBucketWriteSentinel, why: "adopted: it holds the backup's archive as restored", revision: sentinel.revision}, nil
		}
	}
	if len(keys) == 0 && !found {
		switch {
		case in.Archive == nil || in.Archive.Msgs == 0:
			return createBucketAdoption{action: createBucketWriteSentinel, why: "adopted: it held no key"}, nil
		case createBucketRestoresLeftover:
			return createBucketAdoption{action: createBucketRestoreAgain, why: fmt.Sprintf("%s is empty with no sentinel, the leftover of this recovery's cut restore of a backup of %d messages", selection.SessionBucket, in.Archive.Msgs)}, nil
		}
	}
	if found && sentinel.marker == "" && s.ReplayFrom > 0 {
		if in.ReplayFrom > s.ReplayFrom {
			return createBucketAdoption{action: createBucketStop, why: fmt.Sprintf("selection bucket %s holds another recovery's sentinel %s, with replay_from %d, and this recovery's replay_from %d would raise it: a replay_from is never raised",
				selection.SessionBucket, createBucketJSON(s), s.ReplayFrom, in.ReplayFrom)}, nil
		}
		return createBucketAdoption{action: createBucketWriteSentinel, why: fmt.Sprintf("adopted: another recovery's sentinel (hold %s, backup %s), with the lower replay_from %d kept", s.Hold, s.BackupSHA256, in.ReplayFrom),
			revision: sentinel.revision, lowered: true}, nil
	}
	return createBucketAdoption{action: createBucketStop, why: fmt.Sprintf("selection bucket exists and is not this recovery's: %s holds %s", selection.SessionBucket, createBucketHeld(keys, sentinel, found))}, nil
}

// createBucketArchiveMaxGap bounds how many sequences past the archive's last a restored bucket
// may have, every one a put of this hold's barrier: one for each run of the command.
const createBucketArchiveMaxGap = 64

// createBucketArchiveIntact reports whether the bucket's stream, info, holds exactly archive as a
// restore reproduces it, and since then only puts of this hold's barrier key: the same first
// sequence, every sequence through the archive's last untouched (its message count plus the one
// barrier key), the last message the barrier, and each sequence between the archive's last and it
// a barrier put that the next replaced (absent, as history 1 keeps one message a key). A write of
// any other key since the restore leaves a message there, so it is not read as the archive.
func createBucketArchiveIntact(ctx context.Context, nc *nats.Conn, info *jetstream.StreamInfo, archive createBucketArchive, holdID string) (bool, error) {
	s := info.State
	if s.FirstSeq != archive.FirstSeq || s.Msgs != archive.Msgs+1 || s.LastSeq <= archive.LastSeq || s.LastSeq-archive.LastSeq > createBucketArchiveMaxGap {
		return false, nil
	}
	barrier, found, err := replayLeaderRead(ctx, nc, selection.SessionBucket, strictBarrierPrefix+holdID)
	if err != nil {
		return false, err
	}
	if !found || barrier.Seq != s.LastSeq {
		return false, nil
	}
	for seq := archive.LastSeq + 1; seq < s.LastSeq; seq++ {
		_, held, err := replayStreamMessage(ctx, nc, selection.SessionBucket, map[string]any{"seq": seq})
		if err != nil {
			return false, err
		}
		if held {
			return false, nil
		}
	}
	return true, nil
}

// createBucketSentinelState is the sentinel a recovery writes: replay mode from the inputs, the
// orphans listed now (with an adopted sentinel's own kept), the broker's cutover state of the
// session family (holdrecord.Client.Cutovers, the one the commands decide on), and the boundary
// the bucket's sentinel held, which a restored snapshot carries and a walk's replay leaves out
// (the walked sentinel is read from the walk instead).
func (r *run) createBucketSentinelState(ctx context.Context, in createBucketInputs, backup *createBucketBackup, sentinel createBucketSentinelRead, found bool, adopt createBucketAdoption, orphans []string) (selection.StoreState, error) {
	var held selection.StoreState
	switch {
	case found && sentinel.marker == "":
		held = sentinel.state
	case !found && backup != nil && backup.walk != nil && backup.walk.Sentinel != nil:
		held = *backup.walk.Sentinel
	}
	if adopt.lowered {
		state := held
		state.ReplayFrom = in.ReplayFrom
		state.Orphans = createBucketUnion(held.Orphans, orphans)
		return state, nil
	}
	cutover := held.Cutover
	rows, err := r.records.Cutovers(ctx)
	if err != nil {
		return selection.StoreState{}, err
	}
	for _, row := range rows {
		if row.Family == familySession {
			cutover = row.State
		}
	}
	return selection.StoreState{Cutover: cutover, Boundary: held.Boundary, ReplayFrom: in.ReplayFrom, Orphans: orphans,
		BackupSHA256: in.BackupSHA256, Hold: in.Hold}, nil
}

func createBucketUnion(a, b []string) []string {
	out := slices.Clone(a)
	for _, name := range b {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// createBucketOrphans lists the session consumers no record selects (the sentinel's orphans):
// every consumer of the notification stream whose name starts with session~ and whose metadata
// names its session (bus.SessionIDMetadataKey), as the listener's session listing selects them,
// whose session's record neither names it current nor holds it as an open operation's candidate.
// The listing is complete (consumerNames) or UNKNOWN.
func createBucketOrphans(ctx context.Context, nc *nats.Conn, store familyStore) ([]string, error) {
	names, err := consumerNames(ctx, nc, notifications)
	if err != nil {
		return nil, err
	}
	orphans := []string{}
	for _, name := range names {
		if !strings.HasPrefix(name, sessionFreshPrefix+"~") {
			continue
		}
		info, err := consumerInfo(ctx, nc, notifications, name)
		if err != nil {
			return nil, unknownf(err, "the info of %s", name)
		}
		if info == nil {
			continue // gone since the listing
		}
		session := info.Config.Metadata[bus.SessionIDMetadataKey]
		if session == "" {
			continue
		}
		record, _, err := store.Read(ctx, session)
		switch {
		case errors.Is(err, selection.ErrNoRecord) || errors.Is(err, selection.ErrRecordDeleted):
		case err != nil:
			return nil, unknownf(err, "the record of session %s, which %s names", session, name)
		case record.Current == name || (record.Op != nil && record.Op.Candidate == name):
			continue
		}
		orphans = append(orphans, name)
	}
	return orphans, nil
}

// createBucketMake makes the absent selection bucket, each change counted in left, and answers
// its account for the postcheck:
//   - with no backup, an empty bucket at replicas copies with no sentinel yet (CreateKeyValue with
//     selection.SessionBucketConfig), which counts as recreated from sequence 0;
//   - from a walk, the same bucket and the replay of the walk's records (createBucketReplayWalk),
//     recreated from sequence 0 with each replayed put the run's own;
//   - from a snapshot, its restore, which counts as kept from its reading right after the restore.
func (r *run) createBucketMake(ctx context.Context, nc *nats.Conn, js jetstream.JetStream, in createBucketInputs, backup *createBucketBackup, replicas int, left *createBucketLeft) (createBucketMade, error) {
	made := createBucketMade{kind: writerhold.ChangeRecreated, own: map[uint64]bool{}}
	if in.format() != "" && backup == nil {
		return made, fmt.Errorf("selection bucket %s is absent, and the selection record names backup %s, which this run was given no --backup to restore", selection.SessionBucket, in.BackupSHA256)
	}
	if in.format() == createBucketFormatSnapshot {
		made.kind = writerhold.ChangeKept
		err := createBucketRestore(ctx, nc, backup)
		if err != nil {
			left.unsure = "whatever the failed restore of " + selectionStreamName + " left, which its error names"
			return made, err
		}
		left.restored = true
		made.base, err = createBucketReading(ctx, nc)
		return made, err
	}
	if err := createBucketCreate(ctx, nc, js, replicas, left); err != nil {
		return made, err
	}
	if in.format() == createBucketFormatWalk {
		err := createBucketReplayWalk(ctx, nc, backup.walk, made.own)
		left.replayed += len(made.own)
		return made, err
	}
	return made, nil
}

// The bounds of a restore's start and of its leftover's wait (plan §3a, "One rerun"): a restore
// start's transients, 10049 and "no responders", are tried createBucketStartTries times
// createBucketStartPause apart; a cut restore's leftover is waited for createBucketLeftoverWait on
// a clustered hub, three times the measured 10 s; a forced reconnect, for
// createBucketReconnectWait; each jsm.go request of a restore, for createBucketRestoreTimeout,
// which tests shorten. They are vars so a test may shorten them.
var (
	createBucketStartTries     = 3
	createBucketStartPause     = time.Second
	createBucketLeftoverWait   = 30 * time.Second
	createBucketReconnectWait  = 30 * time.Second
	createBucketRestoreTimeout = 30 * time.Second
)

// createBucketRestore restores backup into the absent selection bucket through jsm.go on the
// operator's own connection (jsm.Manager.RestoreSnapshotFromDirectory, plan §3a "The held restore
// and backup"), with the one rerun: before each run it forces a reconnect and waits for it, so the
// restore starts on a fresh decision with its user's whole 10 minutes; a run cut part way is
// followed by the deletion of its leftover and one run more from the first chunk; a second cut
// stops the command, naming the bucket and both runs' errors, with the leftover deleted. A
// restored stream whose state is not the archive's stops it too.
func createBucketRestore(ctx context.Context, nc *nats.Conn, backup *createBucketBackup) error {
	var runs []error
	for attempt := 1; attempt <= 2; attempt++ {
		if err := createBucketReconnect(ctx, nc); err != nil {
			return err
		}
		state, started, err := createBucketRestoreOnce(ctx, nc, backup)
		if err == nil {
			if state.LastSeq != backup.archive.LastSeq || state.Msgs != backup.archive.Msgs {
				return unknownf(nil, "the restore of %s answered last_seq %d and %d messages, where its archive holds %d and %d", selection.SessionBucket, state.LastSeq, state.Msgs, backup.archive.LastSeq, backup.archive.Msgs)
			}
			return nil
		}
		runs = append(runs, fmt.Errorf("run %d: %w", attempt, err))
		if !started {
			// The server refused the start, so it made no stream to leave behind.
			break
		}
		if err := createBucketDeleteLeftover(ctx, nc); err != nil {
			return fmt.Errorf("%w (after %w)", err, errors.Join(runs...))
		}
	}
	return fmt.Errorf("the restore of %s stopped, with no leftover and the hold still set: %w", selection.SessionBucket, errors.Join(runs...))
}

// createBucketRestoreSubject is the restore's start, which the operator's held grant names.
const createBucketRestoreSubject = "$JS.API.STREAM.RESTORE." + selectionStreamName

// createBucketRestoreOnce is one run of the restore, its start retried through its transients:
// 10049 and "no responders" createBucketStartTries times, and 10130, a cut restore's leftover, once
// that leftover is deleted. started reports whether the server may have made a stream: false only
// for a start it refused, by a JetStream answer or under the connection's grant.
func createBucketRestoreOnce(ctx context.Context, nc *nats.Conn, backup *createBucketBackup) (*api.StreamState, bool, error) {
	mgr, err := jsm.New(nc, jsm.WithTimeout(createBucketRestoreTimeout))
	if err != nil {
		return nil, false, err
	}
	for try := 1; ; try++ {
		// jsm.go notifies once its start is answered, before the first chunk.
		var started atomic.Bool
		notify := jsm.RestoreNotify(func(jsm.RestoreProgress) { started.Store(true) })
		state, err := createBucketRestoreRun(ctx, mgr, backup.dir, notify)
		if err == nil {
			return state, true, nil
		}
		err = apiRefusal(nc, createBucketRestoreSubject, err)
		var pointer *api.ApiError
		var value api.ApiError
		answered := errors.As(err, &pointer) || errors.As(err, &value)
		if started.Load() || !(answered || errors.Is(err, nats.ErrPermissionViolation) || errors.Is(err, nats.ErrNoResponders)) {
			return nil, true, err
		}
		if try >= createBucketStartTries {
			return nil, false, err
		}
		switch {
		case api.IsNatsError(err, errRestoreNameInUse):
			if err := createBucketDeleteLeftover(ctx, nc); err != nil {
				return nil, false, err
			}
		case api.IsNatsError(err, errCreatingStore) || errors.Is(err, nats.ErrNoResponders):
		default:
			return nil, false, err
		}
		if err := sleep(ctx, createBucketStartPause); err != nil {
			return nil, false, err
		}
	}
}

// createBucketRestoreBound bounds one restore run: the operator's user expires 10 minutes after
// the decision the forced reconnect starts the run on, and the server closes the connection then.
var createBucketRestoreBound = 10 * time.Minute

// createBucketRestoreRun is one jsm.go restore of dir, ended by ctx or createBucketRestoreBound.
// jsm.go's last request, the empty one whose answer is the restored stream, waits up to an hour
// with no ctx (jsm.go v0.5.0 snapshots.go restoreSnapshot), so a connection cut at that request
// would hold the command for that hour; the run returns at the bound instead, and the request,
// still waiting, ends when the command closes its connection.
func createBucketRestoreRun(ctx context.Context, mgr *jsm.Manager, dir string, notify jsm.SnapshotOption) (*api.StreamState, error) {
	ctx, cancel := context.WithTimeout(ctx, createBucketRestoreBound)
	defer cancel()
	type result struct {
		state *api.StreamState
		err   error
	}
	done := make(chan result, 1)
	go func() {
		_, state, err := mgr.RestoreSnapshotFromDirectory(ctx, selectionStreamName, dir, notify)
		done <- result{state, err}
	}()
	select {
	case res := <-done:
		return res.state, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("the restore of %s did not end: %w", selection.SessionBucket, ctx.Err())
	}
}

// createBucketReconnect forces nc to reconnect and waits until it has, within
// createBucketReconnectWait.
func createBucketReconnect(ctx context.Context, nc *nats.Conn) error {
	before := nc.Stats().Reconnects
	if err := nc.ForceReconnect(); err != nil {
		return fmt.Errorf("force a reconnect before the restore of %s: %w", selection.SessionBucket, err)
	}
	deadline := time.Now().Add(createBucketReconnectWait)
	for nc.Stats().Reconnects == before || !nc.IsConnected() {
		if time.Now().After(deadline) {
			return unknownf(nil, "the reconnect before the restore of %s did not complete within %s", selection.SessionBucket, createBucketReconnectWait)
		}
		if err := sleep(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// createBucketDeleteLeftover deletes what a cut restore of the selection bucket left (plan §3a,
// "One rerun"): on a clustered hub STREAM.INFO answers 10059 for about 10 s after the cut, then an
// empty stream appears and stays, so it waits for the stream to appear, deletes it, and reads
// STREAM.INFO 10059 after the delete. A read right after the delete that the hub does not answer,
// or that still finds the stream while its meta group applies the removal, is read again within the
// same wait (createBucketAwaitGone). A leftover that does not appear within the wait is UNKNOWN on
// a clustered hub, naming the bucket; a lone server leaves none.
func createBucketDeleteLeftover(ctx context.Context, nc *nats.Conn) error {
	clustered := nc.ConnectedClusterName() != ""
	deadline := time.Now().Add(createBucketLeftoverWait)
	for {
		_, present, err := createBucketStream(ctx, nc, false)
		if err != nil {
			return err
		}
		if present {
			if _, err := apiRequest(ctx, nc, "$JS.API.STREAM.DELETE."+selectionStreamName, nil); err != nil {
				return unknownf(err, "the delete of %s's leftover", selection.SessionBucket)
			}
			return createBucketAwaitGone(ctx, func(ctx context.Context) (bool, error) {
				_, present, err := createBucketStream(ctx, nc, false)
				return present, err
			}, deadline)
		}
		if !clustered {
			return nil
		}
		if time.Now().After(deadline) {
			return unknownf(nil, "no leftover of the cut restore of %s appeared within %s", selection.SessionBucket, createBucketLeftoverWait)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// createBucketAwaitGone reads the selection bucket's stream (read) after its delete until STREAM.INFO
// answers it absent. Until deadline it reads again both a stream that still answers present (a
// clustered hub answers the stream for a moment after its delete, while the meta group applies the
// removal) and a read the hub does not answer in time (a timeout or no responder); any other failed
// read ends it with that error. A stream still present at deadline, or no answer by then, is
// UNKNOWN, naming the bucket.
func createBucketAwaitGone(ctx context.Context, read func(context.Context) (bool, error), deadline time.Time) error {
	for {
		present, err := read(ctx)
		switch {
		case err == nil && !present:
			return nil
		case err == nil && time.Now().After(deadline):
			return unknownf(nil, "%s's leftover is still present after its delete", selection.SessionBucket)
		case err == nil: // still present before deadline: read again
		case !noAnswer(err):
			return err
		case time.Now().After(deadline):
			return unknownf(err, "read %s after the delete of its leftover", selection.SessionBucket)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// createBucketCreate creates the absent selection bucket at replicas copies, then waits for its
// stream's leader (createBucketAwaitLeader), counting the create in left: unsure until the create
// is answered, created once it is. createBucketCutoverHeld and createBucketMake both create the
// bucket through it.
func createBucketCreate(ctx context.Context, nc *nats.Conn, js jetstream.JetStream, replicas int, left *createBucketLeft) error {
	left.unsure = "the bucket, if its unanswered create landed"
	if _, err := js.CreateKeyValue(ctx, selection.SessionBucketConfig(replicas)); err != nil {
		return fmt.Errorf("create %s: %w", selection.SessionBucket, err)
	}
	left.unsure, left.created = "", true
	return createBucketAwaitLeader(ctx, nc)
}

// createBucketLeaderWait bounds createBucketAwaitLeader.
var createBucketLeaderWait = 30 * time.Second

// createBucketAwaitLeader waits until the selection bucket's stream, just created, names its
// leader (STREAM.INFO's cluster.leader; a lone server's stream names none and needs no wait). A
// stream with no leader within createBucketLeaderWait is UNKNOWN, naming the bucket. A named leader
// is not yet a responder on every server: the write after it goes through createBucketFreshWrite.
func createBucketAwaitLeader(ctx context.Context, nc *nats.Conn) error {
	deadline := time.Now().Add(createBucketLeaderWait)
	for {
		info, present, err := createBucketStream(ctx, nc, false)
		if err != nil {
			return err
		}
		if present && (info.Cluster == nil || info.Cluster.Leader != "") {
			return nil
		}
		if time.Now().After(deadline) {
			return unknownf(nil, "selection bucket %s named no leader within %s of its create", selection.SessionBucket, createBucketLeaderWait)
		}
		if err := sleep(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

// createBucketFreshWrite runs write, a write to the selection bucket just created, again while it
// is answered "no responders", within createBucketLeaderWait: on the stack's three servers a put
// right after the create is answered so, even with the stream's leader named, until the server the
// connection is on serves the new stream's subjects. A request no stream received wrote nothing,
// so writing again is safe. Any other answer is write's own.
func createBucketFreshWrite(ctx context.Context, write func() error) error {
	deadline := time.Now().Add(createBucketLeaderWait)
	for {
		err := write()
		if !errors.Is(err, nats.ErrNoResponders) || time.Now().After(deadline) {
			return err
		}
		if err := sleep(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}
