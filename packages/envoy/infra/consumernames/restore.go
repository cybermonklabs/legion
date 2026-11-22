package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// The rollback's commands as the hold's journal names them in workflow rollback
// (internal/holdrecord/table.go tableFor): restore --prepare once, restore --key for each name it
// left with no start (Repeatable), then restore --finish, which is final. Each is Stoppable in
// rollback (holdrecord's table marks every command of every workflow Stoppable), so each refusal
// after its intent ends it with a stop entry (restoreStop).
const (
	restoreCommandPrepare = "restore --prepare"
	restoreCommandKey     = "restore --key"
	restoreCommandFinish  = "restore --finish"
)

// The phases of one logical name's rollback, as its record's restore field holds them
// (selection.Restore.Phase).
const (
	// restorePlanned: the target and its start are written, and nothing is deleted yet.
	restorePlanned = "planned"
	// restoreNoStart: no start is known; restore --key gives one, and restore --finish stops
	// naming the name until it has.
	restoreNoStart = "no_start"
	// restoreKept: the target exists and passes over nothing from the start, so it stays.
	restoreKept = "kept"
	// restoreSourceDeleted: the target, the current and the candidate are gone, and restore
	// --finish creates the target at the start.
	restoreSourceDeleted = "source_deleted"
	// restoreCreated: restore --finish created the target at the start.
	restoreCreated = "created"
)

const (
	// restoreSetKind and restoreSetKey name the hold record restore --prepare saves its set in.
	// The broker's routes hold four kinds of record (internal/cipher/natshold RecordKind); a
	// rollback hold runs no convert, so its kind convert under its own key holds restore's set,
	// the rollback's counterpart of convert's saved set (key set).
	restoreSetKind = "convert"
	restoreSetKey  = "restore"
	// restoreServerID prefixes each output of restore --prepare's done entry that records a
	// declared server's /varz server_id, by pod.
	restoreServerID = "server_id."
	// restoreAttempts bounds how often one name's step reads its record again after a refused
	// conditional write (selection.ErrConflict).
	restoreAttempts = 16
)

// restoreRuleSet is the rules a restore runs by. Every field set is an earlier design's rule, and
// only a test's control sets one, to show the row fails under it; a real run keeps them all
// unset.
type restoreRuleSet struct {
	// lastFloorFirst is an earlier design's start: the record's last_floor + 1, whatever name
	// and stream it was read from, before every other start.
	lastFloorFirst bool
	// keepAnyTarget is an earlier design's phase kept: a target that exists stays, at
	// restore --prepare and at restore --finish, with no floor check.
	keepAnyTarget bool
	// firstCreateWhenAbsent is an earlier design's start: a name whose current is absent starts
	// at the stream's end, its first-create rule.
	firstCreateWhenAbsent bool
	// createdIsFinal is an earlier design's finish: a record at phase created is done, its
	// target unread.
	createdIsFinal bool
	// noMarkers leaves every family's broker cutover record and store marker as they are.
	noMarkers bool
	// noStopEntry is the rule before restore --prepare and --key were Stoppable in rollback: a
	// refusal after a restore command's intent writes no stop entry, so its intent stays pending
	// and only that command's rerun is admitted.
	noStopEntry bool
	// noCutoverHold is the rule before the saved set recorded the cutover hold (restoreSet's
	// CutoverHold): repair-sentinel after the rollback then finds no hold whose cohort record gives
	// the sentinel's boundary.
	noCutoverHold bool
}

// restoreRules are the rules restore runs by (restoreRuleSet). A test that sets one runs alone.
var restoreRules restoreRuleSet

// restoreHook, when set, runs at each point of a restore it names (a record's write, a create)
// with the logical name, and an error it answers ends the command there as a kill would: no stop
// entry, so the command's intent stays pending and only its rerun is admitted. Only a test sets
// it.
var restoreHook func(point, logical string) error

// restoreKill is restoreHook's end of a command, which restoreStop passes through unwritten.
type restoreKill struct{ err error }

func (k *restoreKill) Error() string { return k.err.Error() }

func restoreAt(point, logical string) error {
	if restoreHook == nil {
		return nil
	}
	if err := restoreHook(point, logical); err != nil {
		return &restoreKill{err: err}
	}
	return nil
}

// restoreSet is restore --prepare's saved set, written as the hold's record (restoreSetKind,
// restoreSetKey) before its first mutation: each logical name of every family, its target F (its
// fixed name), the start S its rules gave, and the configuration F is created with. A rerun after
// a crash takes each from here, never from a consumer an earlier run deleted, and restore --finish
// creates each F from it.
type restoreSet struct {
	Names []restoreSetName `json:"names"`
	// CutoverHold is the hold of the cutover this rollback undoes, as the session family's broker
	// cutover row named it when restore --prepare built the set (restoreCutoverHold): restore
	// --finish then writes that row rolled_back under this hold's id, and keeps the cutover's
	// boundary and hold in the selection bucket's sentinel (restoreMark), so repair-sentinel
	// follows this field to the cutover hold's cohort record. "" when no session row was done: a
	// rollback of a convert that never finished.
	CutoverHold string `json:"cutover_hold,omitempty"`
}

// restoreSetName is one logical name of a restoreSet.
type restoreSetName struct {
	Family  string `json:"family"`
	Logical string `json:"logical"`
	Target  string `json:"target"`
	// Start is 0 for a name with no start (restorePhase restoreNoStart).
	Start uint64 `json:"start,omitempty"`
	// Config is F's configuration, nil when no consumer of the name and no convert set held one.
	Config *jetstream.ConsumerConfig `json:"config,omitempty"`
}

// restore runs restore --prepare, --key or --finish (§4 of the design).
func (r *run) restore(ctx context.Context) error {
	switch {
	case r.o.prepare:
		return r.restorePrepare(ctx)
	case r.o.finish:
		return r.restoreFinish(ctx)
	}
	return r.restoreKey(ctx)
}

// restoreRun is a restore's reach: the operator's connection and client of the hold routes, the
// notification stream's identity and last sequence, every family's broker cutover row, and each
// family's store, opened once.
type restoreRun struct {
	r        *run
	nc       *nats.Conn
	records  *holdrecord.Client
	streamID string
	lastSeq  uint64
	cutovers []holdrecord.CutoverRow
	stores   map[string]familyStore
	// barrier is whether the selection bucket's barrier stands for the reads to come: false at
	// first, and again after each restart, which can move the bucket's leader.
	barrier bool
	// left is what this run changed, which a stop entry names (restoreStop).
	left *restoreLeft
}

// restoreReach opens a restore's reach, which records what it changes in left.
func (r *run) restoreReach(ctx context.Context, left *restoreLeft) (*restoreRun, error) {
	records, err := r.hold(ctx)
	if err != nil {
		return nil, err
	}
	nc, _, err := r.conn(ctx)
	if err != nil {
		return nil, err
	}
	info, err := streamInfo(ctx, nc, notifications)
	if err != nil {
		return nil, unknownf(err, "the info of %s", notifications)
	}
	cutovers, err := records.Cutovers(ctx)
	if err != nil {
		return nil, err
	}
	r.read("the info of " + notifications + " and the broker's cutover rows")
	return &restoreRun{r: r, nc: nc, records: records, streamID: selection.StreamIdentity(info),
		lastSeq: info.State.LastSeq, cutovers: cutovers, stores: map[string]familyStore{}, left: left}, nil
}

// store is family's store. The selection bucket is read only after the hold's bucket barrier on
// the run's connection (writerhold.ReadBucket with a read of nothing: the put, and the bucket's
// leader read before and after it), put once on each connection, before its first read on it, and
// again after each reconnect, so every read is past the last write a server accepted before the
// hold or before the latest restart; a bucket that does not exist is selection.ErrNoStore. The
// barrier's key is a change this run left (restoreLeft.barrier).
func (rr *restoreRun) store(ctx context.Context, family string) (familyStore, error) {
	if family == familySession && !rr.barrier {
		if err := rr.answered(ctx, func(ctx context.Context) error { _, err := streamInfo(ctx, rr.nc, selectionStreamName); return err }); err != nil {
			if apiErrorIs(err, jetstream.JSErrCodeStreamNotFound) {
				return nil, fmt.Errorf("%w: %s (%w)", selection.ErrNoStore, selectionStreamName, err)
			}
			return nil, unknownf(err, "the info of %s", selectionStreamName)
		}
		rr.left.barrier = true
		if err := writerhold.ReadBucket(ctx, rr.nc, selection.SessionBucket, rr.r.o.holdID, func(context.Context) error { return nil }); err != nil {
			return nil, hubUnknown(err, "the hub's restart")
		}
		rr.barrier = true
	}
	if st, ok := rr.stores[family]; ok {
		return st, nil
	}
	st, err := rr.r.store(ctx, family)
	if err != nil {
		return nil, err
	}
	rr.stores[family] = st
	return st, nil
}

// reconnect moves restore to a connection made after a restart (run.reconnect), with the
// selection bucket's store on it, and the bucket's barrier put again before its next read, since
// the restart can move the bucket's leader.
func (rr *restoreRun) reconnect(ctx context.Context) error {
	delete(rr.stores, familySession)
	rr.barrier = false
	nc, _, err := rr.r.reconnect(ctx)
	if err != nil {
		return err
	}
	rr.nc = nc
	return nil
}

// restoreSettleWithin bounds how long restore asks again a JetStream request no leader answered:
// after a restart each stream's and consumer's group elects a leader again, and a request in
// between times out, finds no responder, or answers 503.
const restoreSettleWithin = time.Minute

// answered runs request again, every 250 ms, while its error says no leader answered it
// (restoreUnanswered), until restoreSettleWithin passes; any other outcome is request's.
func (rr *restoreRun) answered(ctx context.Context, request func(context.Context) error) error {
	deadline := time.Now().Add(restoreSettleWithin)
	for {
		err := request(ctx)
		if err == nil || !restoreUnanswered(err) || time.Now().After(deadline) {
			return err
		}
		if sleep(ctx, 250*time.Millisecond) != nil {
			return err
		}
	}
}

// restoreUnanswered reports whether err is a JetStream request no leader answered: a timeout, no
// responders, or the API's 503.
func restoreUnanswered(err error) bool {
	var api *jetstream.APIError
	if errors.As(err, &api) {
		return api.Code == 503
	}
	return noAnswer(err)
}

// noAnswer reports whether err is a request no server answered: a deadline, a timeout or no
// responders. restoreUnanswered tests it after the API's 503, and createBucketAwaitGone tests it
// alone, since a 503 answer ends that wait.
func noAnswer(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout) || errors.Is(err, nats.ErrNoResponders)
}

// info reads consumer name of the notification stream (consumerInfo), asked again while no leader
// answers.
func (rr *restoreRun) info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	var info *jetstream.ConsumerInfo
	err := rr.answered(ctx, func(ctx context.Context) error {
		var err error
		info, err = consumerInfo(ctx, rr.nc, notifications, name)
		return err
	})
	return info, err
}

// delete deletes consumer name of the notification stream, which set's logical name names, once
// its info shows it is that logical name's own (selection.Names.Owns of familyNames: the target,
// the fixed name, by its name, a fresh name by the metadata its family's consumers carry), asked
// again while no leader answers: a delete that landed unanswered reads as not found the next time,
// which is no error. One of another key is refused (selection.ErrMalformedRecord) and not deleted.
// The run's account (restoreLeft) records each name it deleted or found gone.
func (rr *restoreRun) delete(ctx context.Context, set restoreSetName, name string) error {
	m, err := familyMember(set.Family, set.Logical)
	if err != nil {
		return err
	}
	info, err := rr.info(ctx, name)
	if err != nil {
		return err
	}
	if info != nil {
		if err := familyNames(m).Owns(m.Logical, info); err != nil {
			return err
		}
	}
	err = rr.answered(ctx, func(ctx context.Context) error { return deleteConsumer(ctx, rr.nc, notifications, name) })
	if err == nil {
		rr.left.deleted = append(rr.left.deleted, name)
	}
	return err
}

// families are the families a restore covers, sorted: the session consumers' and the listener
// durables' (the selection bucket), Dispatch's when --dispatch-database-file names its store, each
// daemon --daemon-database names, and every family a broker cutover row names, whose store must
// then be named.
func (rr *restoreRun) families() []string {
	set := map[string]bool{familySession: true}
	if rr.r.o.dispatchDatabaseFile != "" {
		set[familyDispatch] = true
	}
	for _, project := range rr.r.o.daemonProjects() {
		set[daemonFamily(project)] = true
	}
	for _, row := range rr.cutovers {
		set[row.Family] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// hasCutoverRow reports whether a broker cutover row names family.
func (rr *restoreRun) hasCutoverRow(family string) bool {
	return slices.ContainsFunc(rr.cutovers, func(row holdrecord.CutoverRow) bool { return row.Family == family })
}

// restoreEntry is one logical name's record as restore listed it.
type restoreEntry struct {
	member member
	record selection.Record
	// deleted is a key whose last write is a delete or purge marker: its record is lost.
	deleted bool
}

// entries lists every record of family's store, by logical name, and false for a family whose
// store does not exist and that no broker cutover row names, which has nothing to restore.
func (rr *restoreRun) entries(ctx context.Context, family string) ([]restoreEntry, bool, error) {
	st, err := rr.store(ctx, family)
	if errors.Is(err, selection.ErrNoStore) && !rr.hasCutoverRow(family) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var out []restoreEntry
	if family == familySession {
		keys, err := statusSelectionKeys(ctx, rr.nc)
		if errors.Is(err, selection.ErrNoStore) && !rr.hasCutoverRow(family) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		// Every value is read by its key's kind (readSelectionValue), the sentinel's and the
		// barriers' too: a malformed one anywhere is UNKNOWN, naming each such key, before the
		// listing answers anything.
		var malformed []string
		for _, key := range keys {
			var record selection.Record
			switch {
			case key.Deleted && (key.Key == selection.SentinelKey || strings.HasPrefix(key.Key, strictBarrierPrefix)):
				continue
			case !key.Deleted:
				value, err := readSelectionValue(key)
				if err != nil {
					malformed = append(malformed, fmt.Sprintf("%s (%v)", key.Key, err))
					continue
				}
				if value.record == nil {
					continue
				}
				record = *value.record
			}
			m, err := restoreMember(family, key.Key)
			if err != nil {
				return nil, false, unknownf(err, "the key %q of %s", key.Key, selection.SessionBucket)
			}
			out = append(out, restoreEntry{member: m, record: record, deleted: key.Deleted})
		}
		if len(malformed) > 0 {
			return nil, false, unknownf(nil, "the values of %s in %s are malformed, so restore reads no record there", strings.Join(malformed, ", "), selection.SessionBucket)
		}
		return out, true, nil
	}
	pg, ok := st.(pgFamilyStore)
	if !ok {
		return nil, false, fmt.Errorf("%s's store %T lists no records", family, st)
	}
	records, err := pg.List(ctx)
	if errors.Is(err, selection.ErrNoStore) && !rr.hasCutoverRow(family) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for _, logical := range slices.Sorted(maps.Keys(records)) {
		m, err := restoreMember(family, logical)
		if err != nil {
			return nil, false, unknownf(err, "the row %q of %s's %s", logical, family, selection.IntakeConsumersTable)
		}
		record, _, err := pg.Read(ctx, logical)
		if err != nil {
			return nil, false, err
		}
		out = append(out, restoreEntry{member: m, record: record})
	}
	return out, true, nil
}

// restoreMember is the member a record of family keyed logical is: a session id or a listener
// durable's selection.DurableKey in the selection bucket, Dispatch's or a daemon's durable name in
// its intake_consumers.
func restoreMember(family, logical string) (member, error) {
	name := logical
	if family == familySession {
		switch {
		case selection.CheckDurableKey(logical) == nil:
			durable, err := durableOf(logical)
			switch {
			case errors.Is(err, errEscapeCut):
				return member{}, fmt.Errorf("the durable key %q ends inside an escape", logical)
			case err != nil:
				return member{}, fmt.Errorf("the durable key %q holds a malformed escape", logical)
			}
			name = durable
		case selection.CheckKey(logical) == nil:
			name = sessionFixedPrefix + logical
		default:
			return member{}, fmt.Errorf("%q is no session id and no durable's key", logical)
		}
	}
	m, ok := classify(name)
	if !ok || m.Family != family || m.Logical != logical {
		return member{}, fmt.Errorf("%q is no logical name of family %s", logical, family)
	}
	return m, nil
}

// restoreKeyMember is the member --key names: a listener durable's key, any family's fixed name
// (classify), or a session id.
func restoreKeyMember(key string) (member, error) {
	if selection.CheckDurableKey(key) == nil {
		return restoreMember(familySession, key)
	}
	if m, ok := classify(key); ok {
		return m, nil
	}
	if err := selection.CheckKey(key); err != nil {
		return member{}, err
	}
	return restoreMember(familySession, key)
}

// restoreFloor is the ack floor restore reads of a consumer: its stream sequence as the server
// reports it, which is never above what the consumer acked.
func restoreFloor(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream }

// readSet reads the hold's saved set, and false when restore --prepare has written none.
func (rr *restoreRun) readSet(ctx context.Context) (restoreSet, bool, error) {
	return readRestoreSet(ctx, rr.records, rr.r.o.holdID)
}

// readRestoreSet reads hold holdID's saved set (restoreSetKind, restoreSetKey), verified as
// holdrecord.Client.Records verifies every record, and false when the hold holds none.
func readRestoreSet(ctx context.Context, records *holdrecord.Client, holdID string) (restoreSet, bool, error) {
	set, _, found, err := readHoldRecord[restoreSet](ctx, records, holdID, restoreSetKind, restoreSetKey,
		fmt.Sprintf("hold %s's %s record %s", holdID, restoreSetKind, restoreSetKey))
	return set, found, err
}

// savedSet is the hold's saved set: the one an earlier run wrote, else one built now and written
// before any mutation.
func (rr *restoreRun) savedSet(ctx context.Context) (restoreSet, error) {
	if set, found, err := rr.readSet(ctx); err != nil || found {
		return set, err
	}
	set, err := rr.buildSet(ctx)
	if err != nil {
		return restoreSet{}, err
	}
	value, err := json.Marshal(set)
	if err != nil {
		return restoreSet{}, err
	}
	if _, err := rr.records.PutRecord(ctx, rr.r.o.holdID, restoreSetKind, restoreSetKey, 0, value); err != nil {
		var refusal *holdrecord.RouteError
		if errors.As(err, &refusal) && refusal.Code == "gen_mismatch" {
			// Another run of this command wrote its set first: that one governs.
			if set, found, err := rr.readSet(ctx); err != nil || found {
				return set, err
			}
		}
		return restoreSet{}, err
	}
	return set, nil
}

// buildSet reads every family's records and gives each logical name its target, its start and its
// target's configuration, and records the cutover hold the session family's broker cutover row
// names (restoreCutoverHold).
func (rr *restoreRun) buildSet(ctx context.Context) (restoreSet, error) {
	var set restoreSet
	if !restoreRules.noCutoverHold {
		hold, err := restoreCutoverHold(rr.cutovers)
		if err != nil {
			return restoreSet{}, err
		}
		set.CutoverHold = hold
	}
	for _, family := range rr.families() {
		entries, present, err := rr.entries(ctx, family)
		if err != nil {
			return restoreSet{}, err
		}
		if !present {
			continue
		}
		for _, entry := range entries {
			name, err := rr.planName(ctx, family, entry)
			if err != nil {
				return restoreSet{}, err
			}
			set.Names = append(set.Names, name)
		}
	}
	return set, nil
}

// restoreCutoverHold is the hold of the cutover a rollback undoes: the hold id of the session
// family's broker cutover row, which convert writes done once it converted every family
// (convert.go convertRule.run). A row that reads anything but done is refused, naming it: a row
// rolled_back is a rollback's already, and holds that rollback's hold id, not a cutover's. No
// row is "": a convert that never finished wrote none, and its rollback (the interrupted
// conversion's) has no cutover hold to record.
func restoreCutoverHold(rows []holdrecord.CutoverRow) (string, error) {
	for _, row := range rows {
		if row.Family != familySession {
			continue
		}
		if row.State != selection.CutoverDone {
			return "", fmt.Errorf("the broker's %s cutover row reads %s from hold %s: restore --prepare records the hold of a cutover that is %s, which no rollback has undone",
				familySession, row.State, row.HoldID, selection.CutoverDone)
		}
		return row.HoldID, nil
	}
	return "", nil
}

// planName is entry's name in the saved set: its target, its start (restoreStart) and the
// configuration its target is created with: the target's own while it exists, else that of the
// record's current, its operation's candidate or the old name of that operation, the first the
// server holds, else the target's as the cutover's convert saved it.
func (rr *restoreRun) planName(ctx context.Context, family string, entry restoreEntry) (restoreSetName, error) {
	name := restoreSetName{Family: family, Logical: entry.member.Logical, Target: entry.member.Fixed}
	infos := map[string]*jetstream.ConsumerInfo{}
	read := func(consumer string) (*jetstream.ConsumerInfo, error) {
		if consumer == "" {
			return nil, nil
		}
		if info, ok := infos[consumer]; ok {
			return info, nil
		}
		info, err := rr.info(ctx, consumer)
		if err != nil {
			return nil, unknownf(err, "the info of %s", consumer)
		}
		infos[consumer] = info
		return info, nil
	}
	if !entry.deleted {
		start, err := rr.restoreStart(entry.record, read)
		if err != nil {
			return restoreSetName{}, err
		}
		name.Start = start
	}
	sources := []string{name.Target, entry.record.Current}
	if op := entry.record.Op; op != nil {
		sources = append(sources, op.Candidate, op.Old)
	}
	for _, source := range sources {
		info, err := read(source)
		if err != nil {
			return restoreSetName{}, err
		}
		if info != nil {
			name.Config = restoreTargetConfig(info.Config, name.Target)
			return name, nil
		}
	}
	config, err := rr.convertedConfig(ctx, family, name.Target)
	if err != nil {
		return restoreSetName{}, err
	}
	if config != nil {
		name.Config = restoreTargetConfig(*config, name.Target)
	}
	return name, nil
}

// restoreStart is the start S of a record (§4 step 2): the open operation's saved start; else, with
// the current present, its ack floor + 1; else the last floor + 1 when it was read from that
// current on this stream; else the record's saved start; else 0, no start known.
func (rr *restoreRun) restoreStart(record selection.Record, read func(string) (*jetstream.ConsumerInfo, error)) (uint64, error) {
	if lf := record.LastFloor; restoreRules.lastFloorFirst && lf != nil {
		return lf.Floor + 1, nil
	}
	current, err := read(record.Current)
	if err != nil {
		return 0, err
	}
	if restoreRules.firstCreateWhenAbsent && current == nil {
		return rr.lastSeq + 1, nil
	}
	if record.Op != nil && record.Start > 0 {
		return record.Start, nil
	}
	if current != nil {
		return restoreFloor(current) + 1, nil
	}
	if lf := record.LastFloor; lf != nil && record.Current != "" && lf.Name == record.Current && lf.StreamID != "" && lf.StreamID == rr.streamID {
		return lf.Floor + 1, nil
	}
	return record.Start, nil
}

// restoreTargetConfig is config, a consumer's of one logical name, as its target F is created:
// named F, without the selection's logical-name metadata, which no fixed consumer carried, and
// without the metadata the server writes itself (keys beginning "_nats."). The start is set at the
// create (restoreRun.create).
func restoreTargetConfig(config jetstream.ConsumerConfig, target string) *jetstream.ConsumerConfig {
	config.Name, config.Durable = target, target
	config.OptStartSeq, config.OptStartTime = 0, nil
	config.FilterSubjects = slices.Clone(config.FilterSubjects)
	config.Metadata = maps.Clone(config.Metadata)
	maps.DeleteFunc(config.Metadata, func(key, _ string) bool {
		return key == selection.LogicalMetadataKey || strings.HasPrefix(key, "_nats.")
	})
	if len(config.Metadata) == 0 {
		config.Metadata = nil
	}
	return &config
}

// convertedConfig is target's configuration as a convert saved it before it deleted the target
// (convert.go convertSet), read from the convert record of the hold family's broker cutover row
// names, then of every other hold a cutover row names; nil when none holds it.
func (rr *restoreRun) convertedConfig(ctx context.Context, family, target string) (*jetstream.ConsumerConfig, error) {
	var holds []string
	for _, row := range rr.cutovers {
		if row.Family == family && !slices.Contains(holds, row.HoldID) {
			holds = append([]string{row.HoldID}, holds...)
		} else if !slices.Contains(holds, row.HoldID) {
			holds = append(holds, row.HoldID)
		}
	}
	for _, hold := range holds {
		set, _, found, err := readConvertSet(ctx, rr.records, hold)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		for _, saved := range set.Names {
			if saved.Fixed == target && saved.Config != nil {
				return saved.Config, nil
			}
		}
	}
	return nil, nil
}

// restorePrepare is restore --prepare --hold ID (§4 step 2): the saved set, then each name settled
// to its phase, then the declared servers' ids, read after the last delete, in its done entry,
// which restore --finish's restart must replace. Every refusal after its intent writes its stop
// entry, naming each record it left at each phase and each consumer it deleted (restoreStop); its
// rerun goes on from the phase each record holds.
func (r *run) restorePrepare(ctx context.Context) error {
	a, err := r.admit(ctx, restoreCommandPrepare, nil)
	if err != nil {
		return err
	}
	left := &restoreLeft{}
	if err := r.restorePrepareAdmitted(ctx, a, left); err != nil {
		return restoreStop(ctx, a, err, left)
	}
	return nil
}

// restorePrepareAdmitted is restore --prepare after its intent, recording what it changes in left.
func (r *run) restorePrepareAdmitted(ctx context.Context, a *admission, left *restoreLeft) error {
	rr, err := r.restoreReach(ctx, left)
	if err != nil {
		return err
	}
	set, err := rr.savedSet(ctx)
	if err != nil {
		return err
	}
	r.read(fmt.Sprintf("the saved set of %d names", len(set.Names)))
	var unresolved []string
	phases := map[string]int{}
	for _, name := range set.Names {
		phase, err := rr.settle(ctx, name, name.Start)
		if err != nil {
			return err
		}
		phases[phase]++
		if phase == restoreNoStart {
			unresolved = append(unresolved, name.Family+"/"+name.Logical)
		}
	}
	r.read(fmt.Sprintf("the record and target of each of the %d saved names", len(set.Names)))
	op, err := r.operatorHub(ctx)
	if err != nil {
		return err
	}
	declared, err := writerhold.Discover(ctx, op.Hub)
	if err != nil {
		return hubUnknown(err, "the hub's restart")
	}
	r.read("the declared servers")
	outputs := map[string]string{"names": strconv.Itoa(len(set.Names)), "no_start": strings.Join(unresolved, " ")}
	for _, server := range declared {
		outputs[restoreServerID+server.Pod] = server.Varz.ServerID
	}
	if err := a.done(ctx, outputs); err != nil {
		return err
	}
	fmt.Fprintf(r.stdout, "restore --prepare: %d names: %d kept, %d source_deleted, %d with no start\n", len(set.Names),
		phases[restoreKept], phases[restoreSourceDeleted], phases[restoreNoStart])
	for _, name := range unresolved {
		fmt.Fprintf(r.stdout, "  %s has no start: give it one with restore --key <K> --from <seq> --hold %s\n", name, r.o.holdID)
	}
	return nil
}

// settle takes one name of the saved set to the phase restore --prepare leaves it at, from the
// phase its record holds, each step a conditional write: its restore field written with its
// target and start, phase planned (no_start for a start of 0); then the target kept when its ack
// floor + 1 is at most the start, else deleted; the current, the candidate and the operation's old
// name, fresh names never used again, deleted and retired; then phase kept or source_deleted. A
// record whose restore field names another target or start is a rollback's of an earlier hold,
// which this one plans again.
func (rr *restoreRun) settle(ctx context.Context, name restoreSetName, start uint64) (string, error) {
	st, err := rr.store(ctx, name.Family)
	if err != nil {
		return "", err
	}
	for range restoreAttempts {
		record, revision, err := rr.readRecord(ctx, st, name)
		if err != nil {
			return "", err
		}
		restore := record.Restore
		if restore == nil || restore.Target != name.Target || restore.Start != start {
			next := record
			next.Restore = &selection.Restore{Target: name.Target, Start: start, Phase: restorePlanned}
			if start == 0 {
				next.Restore.Phase = restoreNoStart
			}
			if _, err := st.Write(ctx, name.Logical, next, revision); errors.Is(err, selection.ErrConflict) {
				continue
			} else if err != nil {
				return "", err
			}
			rr.left.wrote(name, next.Restore.Phase)
			if err := restoreAt(next.Restore.Phase, name.Logical); err != nil {
				return "", err
			}
			continue
		}
		if restore.Phase != restorePlanned {
			return restore.Phase, nil
		}
		target, err := rr.info(ctx, name.Target)
		if err != nil {
			return "", unknownf(err, "the info of %s", name.Target)
		}
		keep := target != nil && (restoreRules.keepAnyTarget || restoreFloor(target)+1 <= start)
		if target != nil && !keep {
			if err := rr.delete(ctx, name, name.Target); err != nil {
				return "", unknownf(err, "the delete of %s", name.Target)
			}
		}
		next := record
		for _, fresh := range restoreFreshNames(record, name.Target) {
			if err := rr.delete(ctx, name, fresh); err != nil {
				return "", unknownf(err, "the delete of %s", fresh)
			}
			next = retire(next, name.Target, fresh)
		}
		if err := restoreAt("deleted", name.Logical); err != nil {
			return "", err
		}
		next.Current, next.Op = "", nil
		next.Restore = &selection.Restore{Target: name.Target, Start: start, Phase: restoreSourceDeleted}
		if keep {
			next.Restore.Phase = restoreKept
		}
		if _, err := st.Write(ctx, name.Logical, next, revision); errors.Is(err, selection.ErrConflict) {
			continue
		} else if err != nil {
			return "", err
		}
		rr.left.wrote(name, next.Restore.Phase)
		if err := restoreAt(next.Restore.Phase, name.Logical); err != nil {
			return "", err
		}
		return next.Restore.Phase, nil
	}
	return "", fmt.Errorf("%s's record changed under each of %d writes", name.Logical, restoreAttempts)
}

// restoreFreshNames are the names of record a restore deletes beside its target: the current, the
// operation's candidate and its old name, each once, the target never among them.
func restoreFreshNames(record selection.Record, target string) []string {
	var names []string
	add := func(name string) {
		if name != "" && name != target && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	add(record.Current)
	if record.Op != nil {
		add(record.Op.Candidate)
		add(record.Op.Old)
	}
	return names
}

// readRecord reads name's record and its revision. A selection-bucket key whose last write is a
// delete marker reads as an empty record at the marker's revision, so the next write lands over the
// marker; a key never written is an empty record at revision 0.
func (rr *restoreRun) readRecord(ctx context.Context, st familyStore, name restoreSetName) (selection.Record, uint64, error) {
	record, revision, err := st.Read(ctx, name.Logical)
	switch {
	case errors.Is(err, selection.ErrNoRecord):
		return selection.Record{}, 0, nil
	case errors.Is(err, selection.ErrRecordDeleted) && name.Family == familySession:
		keys, err := statusSelectionKeys(ctx, rr.nc)
		if err != nil {
			return selection.Record{}, 0, err
		}
		for _, key := range keys {
			if key.Key == name.Logical && key.Deleted {
				return selection.Record{}, key.Seq, nil
			}
		}
		return selection.Record{}, 0, unknownf(nil, "%s's delete marker is gone from %s between two reads", name.Logical, selection.SessionBucket)
	case err != nil:
		return selection.Record{}, 0, err
	}
	return record, revision, nil
}

// restoreKey is restore --key K --from SEQ --hold ID: it gives a name restore --prepare left with
// no start its start, then settles it as restore --prepare does. K's form is checked before its
// intent, as a flag's value; everything after its intent that refuses (a name that has its start
// already, one restore --prepare did not save) writes its stop entry (restoreStop), so restore
// --finish is admitted after it.
func (r *run) restoreKey(ctx context.Context) error {
	m, err := restoreKeyMember(r.o.key)
	if err != nil {
		return fmt.Errorf("--key %q: %w", r.o.key, err)
	}
	a, err := r.admit(ctx, restoreCommandKey, map[string]string{"key": r.o.key, "from": formatSeq(r.o.from)})
	if err != nil {
		return err
	}
	left := &restoreLeft{}
	if err := r.restoreKeyAdmitted(ctx, a, m, left); err != nil {
		return restoreStop(ctx, a, err, left)
	}
	return nil
}

// restoreKeyAdmitted is restore --key after its intent, recording what it changes in left.
func (r *run) restoreKeyAdmitted(ctx context.Context, a *admission, m member, left *restoreLeft) error {
	rr, err := r.restoreReach(ctx, left)
	if err != nil {
		return err
	}
	name, err := rr.keyName(ctx, m)
	if err != nil {
		return err
	}
	r.read(fmt.Sprintf("%s/%s's record and its name in the saved set", name.Family, name.Logical))
	phase, err := rr.settle(ctx, name, r.o.from)
	if err != nil {
		return err
	}
	if err := a.done(ctx, map[string]string{"family": name.Family, "logical": name.Logical, "target": name.Target,
		"start": formatSeq(r.o.from), "phase": phase}); err != nil {
		return err
	}
	fmt.Fprintf(r.stdout, "restore --key: %s/%s restores %s at %d (%s)\n", name.Family, name.Logical, name.Target, r.o.from, phase)
	return nil
}

// keyName is m's name in the saved set, refused unless its record is at phase no_start, or already
// carries --from's start for the same target (a rerun).
func (rr *restoreRun) keyName(ctx context.Context, m member) (restoreSetName, error) {
	set, found, err := rr.readSet(ctx)
	if err != nil {
		return restoreSetName{}, err
	}
	if !found {
		return restoreSetName{}, errors.New("hold " + rr.r.o.holdID + " has no saved set: restore --prepare runs first")
	}
	i := slices.IndexFunc(set.Names, func(n restoreSetName) bool { return n.Family == m.Family && n.Logical == m.Logical })
	if i < 0 {
		return restoreSetName{}, fmt.Errorf("%s/%s is no name restore --prepare saved", m.Family, m.Logical)
	}
	name := set.Names[i]
	st, err := rr.store(ctx, name.Family)
	if err != nil {
		return restoreSetName{}, err
	}
	record, _, err := rr.readRecord(ctx, st, name)
	if err != nil {
		return restoreSetName{}, err
	}
	switch restore := record.Restore; {
	case restore == nil || restore.Target != name.Target:
		return restoreSetName{}, fmt.Errorf("%s/%s has no rollback of %s in its record: restore --prepare has not reached it", name.Family, name.Logical, name.Target)
	case restore.Phase == restoreNoStart, restore.Start == rr.r.o.from:
		return name, nil
	}
	return restoreSetName{}, fmt.Errorf("%s/%s restores %s at %d already (phase %s), not at --from %d",
		name.Family, name.Logical, name.Target, record.Restore.Start, record.Restore.Phase, rr.r.o.from)
}

// restoreFinish is restore --finish --hold ID (§4 steps 3 and 4). Every name must have its start
// first. Then, as its first effect, it runs the hold's restart, proof and barrier again
// (writerhold.RestartProveAndBarrier), which ends every server-side writer of each target restore
// --prepare deleted, and requires every declared server's id to differ from the one restore
// --prepare recorded. It reads each record's live target and completes only when each is present
// at its safe start: a kept target passing its floor check is adopted; one failing it is deleted,
// and the restart runs once more, after that last read that saw the name present, before the target
// is created; a created target at its start is adopted, at another start refused; an absent one
// is created at its start. Then every family's broker cutover row and store marker read
// rolled_back, and its done entry completes the hold. Every refusal after its intent writes its
// stop entry, naming what the run left (restoreStop).
func (r *run) restoreFinish(ctx context.Context) error {
	a, err := r.admit(ctx, restoreCommandFinish, nil)
	if err != nil {
		return err
	}
	left := &restoreLeft{}
	if err := r.restoreFinishAdmitted(ctx, a, left); err != nil {
		return restoreStop(ctx, a, err, left)
	}
	return nil
}

// restoreFinishAdmitted is restore --finish after its intent, recording what it changes in left.
func (r *run) restoreFinishAdmitted(ctx context.Context, a *admission, left *restoreLeft) error {
	rr, err := r.restoreReach(ctx, left)
	if err != nil {
		return err
	}
	set, found, err := rr.readSet(ctx)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("hold " + r.o.holdID + " has no saved set of restore --prepare")
	}
	recorded, err := rr.preparedServerIDs(ctx)
	if err != nil {
		return err
	}
	r.read("the saved set and the server ids restore --prepare recorded")
	var unresolved []string
	for _, name := range set.Names {
		st, err := rr.store(ctx, name.Family)
		if err != nil {
			return err
		}
		record, _, err := rr.readRecord(ctx, st, name)
		if err != nil {
			return err
		}
		if restore := record.Restore; restore == nil || restore.Target != name.Target || restore.Start == 0 || restore.Phase == restorePlanned || restore.Phase == restoreNoStart {
			unresolved = append(unresolved, name.Family+"/"+name.Logical)
		}
	}
	r.read(fmt.Sprintf("the record of each of the %d saved names", len(set.Names)))
	if len(unresolved) > 0 {
		return fmt.Errorf("these names have no settled start, so nothing is restarted or created: %s; give each its start with restore --key <K> --from <seq> --hold %s",
			strings.Join(unresolved, ", "), r.o.holdID)
	}
	op, err := r.operatorHub(ctx)
	if err != nil {
		return err
	}
	round := func() (writerhold.Declared, error) {
		after, err := writerhold.RestartProveAndBarrier(ctx, op, r.o.holdID)
		if err != nil {
			return nil, hubUnknown(err, "the hub's restart")
		}
		if err := restoreServersRestarted(recorded, after); err != nil {
			return nil, err
		}
		if err := rr.reconnect(ctx); err != nil {
			return nil, err
		}
		return after, nil
	}
	after, err := round()
	if err != nil {
		return err
	}
	r.read("the hub's restart, its proof and its barrier")
	var creates []restoreSetName
	fence := false
	for _, name := range set.Names {
		create, deleted, err := rr.check(ctx, name)
		if err != nil {
			return err
		}
		fence = fence || deleted
		if create {
			creates = append(creates, name)
		}
	}
	r.read(fmt.Sprintf("the record and live target of each of the %d saved names after the restart", len(set.Names)))
	if fence {
		// A target this run saw present after its restart, and deleted: its server-side writer
		// may still write until its server restarts once more.
		if after, err = round(); err != nil {
			return err
		}
	}
	for _, name := range creates {
		if err := rr.create(ctx, name); err != nil {
			return err
		}
	}
	if !restoreRules.noMarkers {
		if err := rr.markRolledBack(ctx); err != nil {
			return err
		}
	}
	outputs := map[string]string{"names": strconv.Itoa(len(set.Names)), "created": strconv.Itoa(len(creates))}
	for _, server := range after {
		outputs[restoreServerID+server.Pod] = server.Varz.ServerID
	}
	if err := a.done(ctx, outputs); err != nil {
		return err
	}
	fmt.Fprintf(r.stdout, "restore --finish: %d names restored, %d targets created at their starts; every family reads %s\n",
		len(set.Names), len(creates), selection.CutoverRolledBack)
	return nil
}

// check reads name's record and its live target after the restart, and answers whether the
// target is to be created at its start, and whether check deleted a target it saw present, which
// needs one more restart before the create (§4 step 4, "The fence before a create"). A delete
// stays answered across a refused write: the target is gone whatever the next read finds.
func (rr *restoreRun) check(ctx context.Context, name restoreSetName) (create, deleted bool, err error) {
	st, err := rr.store(ctx, name.Family)
	if err != nil {
		return false, false, err
	}
	for range restoreAttempts {
		record, revision, err := rr.readRecord(ctx, st, name)
		if err != nil {
			return false, deleted, err
		}
		if record.Restore == nil || record.Restore.Target != name.Target {
			return false, deleted, fmt.Errorf("%s/%s's record holds no rollback of %s", name.Family, name.Logical, name.Target)
		}
		restore := *record.Restore
		target, err := rr.info(ctx, name.Target)
		if err != nil {
			return false, deleted, unknownf(err, "the info of %s", name.Target)
		}
		atStart := target != nil && target.Config.DeliverPolicy == jetstream.DeliverByStartSequencePolicy && target.Config.OptStartSeq == restore.Start
		next := record
		switch restore.Phase {
		case restoreKept:
			switch {
			case target != nil && (restoreRules.keepAnyTarget || restoreFloor(target)+1 <= restore.Start):
				if record.Current == name.Target {
					return false, deleted, nil
				}
				next.Current = name.Target
			case target != nil:
				// A native policy's start cannot change in place (nats-server consumer.go
				// 2541-2557), so the target goes and is created at the start.
				if err := rr.delete(ctx, name, name.Target); err != nil {
					return false, deleted, unknownf(err, "the delete of %s", name.Target)
				}
				deleted = true
				next.Current = ""
				next.Restore = &selection.Restore{Target: name.Target, Start: restore.Start, Phase: restoreSourceDeleted}
			default:
				return true, deleted, nil
			}
		case restoreCreated:
			switch {
			case restoreRules.createdIsFinal, atStart && record.Current == name.Target:
				return false, deleted, nil
			case atStart:
				next.Current = name.Target
			case target != nil:
				return false, deleted, fmt.Errorf("restore target %s exists with another start (%s at %d, not %d)",
					name.Target, target.Config.DeliverPolicy, target.Config.OptStartSeq, restore.Start)
			default:
				// Removed for inactivity after its create (consumer.go 2234-2275): this run's
				// restart came after the run that saw it present.
				return true, deleted, nil
			}
		case restoreSourceDeleted:
			switch {
			case atStart:
				// An earlier run created it and ended before its record said so.
				next.Current = name.Target
				next.Restore = &selection.Restore{Target: name.Target, Start: restore.Start, Phase: restoreCreated}
			case target != nil:
				if err := rr.delete(ctx, name, name.Target); err != nil {
					return false, deleted, unknownf(err, "the delete of %s", name.Target)
				}
				return true, true, nil
			default:
				return true, deleted, nil
			}
		default:
			return false, deleted, fmt.Errorf("%s/%s is at phase %q, which restore --finish does not settle", name.Family, name.Logical, restore.Phase)
		}
		if _, err := st.Write(ctx, name.Logical, next, revision); errors.Is(err, selection.ErrConflict) {
			continue
		} else if err != nil {
			return false, deleted, err
		}
		rr.left.wrote(name, next.Restore.Phase)
		return next.Restore.Phase == restoreSourceDeleted, deleted, nil
	}
	return false, deleted, fmt.Errorf("%s's record changed under each of %d writes", name.Logical, restoreAttempts)
}

// create creates name's target at its start with its saved configuration, by the operator's
// CONSUMER.CREATE with action create, then writes current = the target and phase created. An
// answer that does not come is settled by the target's info: present at the start is the create.
func (rr *restoreRun) create(ctx context.Context, name restoreSetName) error {
	if name.Config == nil {
		return fmt.Errorf("restore target %s has no saved configuration: no consumer of %s/%s existed at restore --prepare, and no convert saved it",
			name.Target, name.Family, name.Logical)
	}
	st, err := rr.store(ctx, name.Family)
	if err != nil {
		return err
	}
	record, revision, err := rr.readRecord(ctx, st, name)
	if err != nil {
		return err
	}
	start := record.Restore.Start
	config := *name.Config
	config.FilterSubjects = slices.Clone(config.FilterSubjects)
	config.Metadata = maps.Clone(config.Metadata)
	config.DeliverPolicy, config.OptStartSeq, config.OptStartTime = jetstream.DeliverByStartSequencePolicy, start, nil
	if m, err := restoreMember(name.Family, name.Logical); err == nil && strings.HasPrefix(m.Fixed, listenerFixedPrefix) && config.DeliverSubject != "" {
		// The listener's push consumer gets a new random deliver inbox, as startListenerSubscription
		// gives it (cmd/listener/durable.go).
		config.DeliverSubject = nats.NewInbox()
	}
	// The config is made once, its inbox included, so a create sent again is the same create, which
	// the server answers with the consumer it made.
	err = rr.answered(ctx, func(ctx context.Context) error {
		_, err := writeConsumer(ctx, rr.nc, notifications, config, actionCreateConsumer)
		return err
	})
	if err != nil {
		// An earlier send of this create may have landed: the target at its start is the create.
		info, readErr := rr.info(ctx, name.Target)
		atStart := readErr == nil && info != nil && info.Config.OptStartSeq == start && info.Config.DeliverPolicy == jetstream.DeliverByStartSequencePolicy
		var api *jetstream.APIError
		switch {
		case atStart:
		case errors.As(err, &api) && !restoreUnanswered(err) && readErr == nil:
			return fmt.Errorf("the create of restore target %s at %d: %w", name.Target, start, err)
		default:
			return unknownf(errors.Join(err, readErr), "the create of restore target %s at %d", name.Target, start)
		}
	}
	rr.left.created = append(rr.left.created, name.Target)
	if err := restoreAt("target", name.Logical); err != nil {
		return err
	}
	created := func(record selection.Record) selection.Record {
		next := record
		next.Current = name.Target
		next.Restore = &selection.Restore{Target: name.Target, Start: start, Phase: restoreCreated}
		return next
	}
	next := created(record)
	for range restoreAttempts {
		_, err := st.Write(ctx, name.Logical, next, revision)
		if !errors.Is(err, selection.ErrConflict) {
			if err != nil {
				return err
			}
			rr.left.wrote(name, restoreCreated)
			if err := restoreAt(restoreCreated, name.Logical); err != nil {
				return err
			}
			fmt.Fprintf(rr.r.stdout, "  %s/%s: %s created at %d\n", name.Family, name.Logical, name.Target, start)
			return nil
		}
		if record, revision, err = rr.readRecord(ctx, st, name); err != nil {
			return err
		}
		next = created(record)
	}
	return fmt.Errorf("%s's record changed under each of %d writes", name.Logical, restoreAttempts)
}

// markRolledBack writes every family's broker cutover row rolled_back, and the marker
// cutover = rolled_back in each family's store that exists: the selection bucket's sentinel and
// each intake_consumers sentinel row. A creator in the fresh mode then refuses every consumer call
// until convert runs again (selection.Admit), and one in the fixed mode runs, since rolled_back is
// no done.
func (rr *restoreRun) markRolledBack(ctx context.Context) error {
	for _, family := range rr.families() {
		st, err := rr.store(ctx, family)
		if errors.Is(err, selection.ErrNoStore) && !rr.hasCutoverRow(family) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := rr.records.PutCutover(ctx, family, selection.CutoverRolledBack, rr.r.o.holdID); err != nil {
			return fmt.Errorf("the %s cutover row: %w", family, err)
		}
		rr.left.marked = append(rr.left.marked, family+" broker cutover row")
		if err := restoreMark(ctx, family, st); err != nil {
			return err
		}
		rr.left.marked = append(rr.left.marked, family+" store marker")
	}
	return nil
}

// restoreMark writes cutover = rolled_back in st's sentinel, keeping what else it holds (the
// selection bucket's B), by a write conditional on the sentinel's revision (convertSentinel), read
// again after a refused one.
func restoreMark(ctx context.Context, family string, st familyStore) error {
	for range restoreAttempts {
		state, revision, err := convertSentinel(ctx, st, family)
		if err != nil {
			return unknownf(err, "the %s family's store marker", family)
		}
		if state.Cutover == selection.CutoverRolledBack {
			return nil
		}
		state.Cutover = selection.CutoverRolledBack
		if _, err := st.WriteSentinel(ctx, state, revision); errors.Is(err, selection.ErrConflict) {
			continue
		} else if err != nil {
			return fmt.Errorf("write the %s family's store marker: %w", family, err)
		}
		return nil
	}
	return unknownf(nil, "the %s family's store marker moved under %d writes", family, restoreAttempts)
}

// preparedServerIDs are the server ids restore --prepare's done entry recorded, by pod.
func (rr *restoreRun) preparedServerIDs(ctx context.Context) (map[string]string, error) {
	d, _, err := rr.records.Journal(ctx, rr.r.o.holdID)
	if err != nil {
		return nil, err
	}
	var outputs map[string]string
	for _, op := range d.Ops {
		if op.Command == restoreCommandPrepare && op.State == "done" {
			outputs = op.Outputs
		}
	}
	ids := map[string]string{}
	for key, id := range outputs {
		if pod, ok := strings.CutPrefix(key, restoreServerID); ok {
			ids[pod] = id
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("restore --prepare's done entry records no server id, so no restart can be shown to come after its deletes")
	}
	return ids, nil
}

// restoreServersRestarted refuses unless every server restore --prepare recorded is among after,
// the servers the restart left, each with another id.
func restoreServersRestarted(recorded map[string]string, after writerhold.Declared) error {
	var findings []string
	seen := map[string]bool{}
	for _, server := range after {
		seen[server.Pod] = true
		switch old, ok := recorded[server.Pod]; {
		case !ok:
			findings = append(findings, fmt.Sprintf("server %s is no server restore --prepare recorded", server.Pod))
		case old == server.Varz.ServerID:
			findings = append(findings, fmt.Sprintf("server %s still has the id %s that restore --prepare recorded", server.Pod, old))
		}
	}
	for _, pod := range slices.Sorted(maps.Keys(recorded)) {
		if !seen[pod] {
			findings = append(findings, fmt.Sprintf("server %s, which restore --prepare recorded, was not restarted", pod))
		}
	}
	if len(findings) > 0 {
		return errors.New(strings.Join(findings, "; "))
	}
	return nil
}

// restoreStop writes a restore command's stop entry (admission.stop), its reason cause and what
// this run left (restoreLeft), its record of what every run of the command in this hold left
// (admission.leftAfter, an UNKNOWN cause's under leftUnknown), and answers the same text with cause
// wrapped, so an UNKNOWN cause still exits 2. Every error after a restore command's intent comes
// here, a done entry whose write failed among them: restore --prepare, --key and --finish are each
// Stoppable in rollback, so the stop ends the intent and the hold is never left with only a rerun
// admissible. Where the workflow's table holds the command not Stoppable, admission.stop writes
// nothing, and the intent stays pending. admission.stop answers a plain error when its stop entry
// is written, and one wrapping the write's error when it is not. A test's kill (restoreKill)
// writes nothing, as a killed process writes nothing.
func restoreStop(ctx context.Context, a *admission, cause error, left *restoreLeft) error {
	var kill *restoreKill
	if errors.As(cause, &kill) {
		return cause
	}
	what := fmt.Errorf("%w; %s", cause, left)
	if restoreRules.noStopEntry || !stopsAfter(cause) {
		return what
	}
	if err := errors.Unwrap(a.stop(ctx, what.Error(), a.leftAfter(ctx, cause, left.empty(), left.String()))); err != nil {
		return errors.Join(what, fmt.Errorf("and its stop entry: %w", err))
	}
	return what
}

// restoreLeft is what one run of a restore command changed outside the journal, which its stop
// entry names, so whoever reads a stopped hold knows what the run left before acting: each record
// it wrote, by the phase it last wrote there, each consumer it deleted or found gone, each target
// it created, each broker cutover row and store marker it set rolled_back, and this hold's
// barrier key when it put it.
type restoreLeft struct {
	phases  map[string]string
	order   []string
	deleted []string
	created []string
	marked  []string
	// barrier: this run put this hold's barrier key in the selection bucket (restoreRun.store,
	// barrierLeft), set right before the put, whose answer may be lost after it landed. No restore
	// command deletes the bucket, so the key stays a change the run left; restore --prepare puts
	// it before it saves the restore set, so a run that saved no set may still have left it.
	barrier bool
}

// wrote records that this run wrote name's record with its restore field at phase.
func (l *restoreLeft) wrote(name restoreSetName, phase string) {
	key := name.Family + "/" + name.Logical
	if l.phases == nil {
		l.phases = map[string]string{}
	}
	if _, ok := l.phases[key]; !ok {
		l.order = append(l.order, key)
	}
	l.phases[key] = phase
}

// String is the account a stop entry's reason ends with.
func (l *restoreLeft) String() string {
	var parts []string
	for _, phase := range []string{restorePlanned, restoreNoStart, restoreKept, restoreSourceDeleted, restoreCreated} {
		var names []string
		for _, key := range l.order {
			if l.phases[key] == phase {
				names = append(names, key)
			}
		}
		if len(names) > 0 {
			parts = append(parts, "records at "+phase+": "+listNames(names))
		}
	}
	if len(l.deleted) > 0 {
		parts = append(parts, "consumers deleted or found gone: "+listNames(l.deleted))
	}
	if len(l.created) > 0 {
		parts = append(parts, "targets created: "+listNames(l.created))
	}
	if len(l.marked) > 0 {
		parts = append(parts, "rolled_back now: "+listNames(l.marked))
	}
	if len(parts) == 0 {
		none := "this run changed no selection record, consumer, cutover row or store marker"
		if l.putBarrier() {
			return none + "; it put " + barrierLeft
		}
		return none
	}
	if l.putBarrier() {
		parts = append(parts, barrierLeft)
	}
	return "this run left " + strings.Join(parts, "; ") + "; each record's restore field holds its phase, and the command's rerun goes on from it"
}

// empty reports whether this run changed nothing outside the journal.
func (l *restoreLeft) empty() bool {
	return len(l.phases) == 0 && len(l.deleted) == 0 && len(l.created) == 0 && len(l.marked) == 0 && !l.putBarrier()
}

// putBarrier reports whether this run's account counts this hold's barrier key: the run put it,
// and the control's earlier rule that the key is no change (leftRuleSet.barrierNoChange) is not set.
func (l *restoreLeft) putBarrier() bool { return l.barrier && !leftRules.barrierNoChange }
