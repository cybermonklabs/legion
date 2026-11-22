package consumernames

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// repairCommand is repair-sentinel's command in the hold's journal, the one command of the workflow
// holdrecord.WorkflowSentinelRepair (internal/holdrecord/table.go, tableFor): final and Stoppable.
const repairCommand = "consumer-names repair-sentinel"

// repairSentinel is `consumer-names repair-sentinel --hold ID`: it rewrites the selection bucket's
// sentinel (selection.SentinelKey) from the broker's records in Postgres, never from what NATS
// holds. Any principal whose grant holds a JetStream API request can store the server's answer
// under the sentinel's key, by naming the key's subject as the request's reply subject
// (strictdecode.go); the library's decode refuses that value as selection.ErrMalformedSentinel, so
// every session consumer call fails closed, and no other command rewrites a sentinel it cannot
// read: restore --finish, the one other writer after the cutover, reads it first and is UNKNOWN on
// it (restoreMark, convertSentinel).
//
// Before its intent it reads the sentinel the records give (repairWant), and refuses, or is
// UNKNOWN, writing nothing, when they give none. After its intent, behind the bucket's barrier, it
// reads the key's last message from the stream's leader (replayLeaderRead): its value is read raw,
// since its decode is what failed. What it does with that message is repairDecide's: it writes the
// records' sentinel conditionally on the message's sequence (selection.KVStore.WriteSentinel), and
// reads again after a write the store refused because the key moved (selection.ErrConflict). A
// refusal after its intent is its stop (createBucketStopped), naming what the run left.
func (r *run) repairSentinel(ctx context.Context) error {
	err := r.repairSentinelRun(ctx)
	return connRefusal(r.nc, err)
}

func (r *run) repairSentinelRun(ctx context.Context) error {
	if _, err := r.requirePhase(ctx, natsgrant.OperatorPhaseHeld); err != nil {
		return err
	}
	want, err := r.repairWant(ctx)
	if err != nil {
		return err
	}
	value, err := json.Marshal(want)
	if err != nil {
		return err
	}
	nc, _, err := r.conn(ctx)
	if err != nil {
		return err
	}
	store, err := r.store(ctx, familySession)
	if err != nil {
		return err
	}
	inputs := map[string]string{"cutover": want.Cutover, "boundary": formatSeq(want.Boundary), "hold": want.Hold}
	a, err := r.admitRead(ctx, repairCommand, inputs)
	if err != nil {
		return err
	}
	var left createBucketLeft
	was, err := r.repairHeld(ctx, nc, store, want, value, &left)
	if err == nil {
		inputs["sentinel_was"] = was
		err = a.done(ctx, inputs)
	}
	if err != nil {
		return createBucketStopped(ctx, a, err, &left)
	}
	fmt.Fprintf(r.stdout, "repair-sentinel: %s of %s reads the broker's records: cutover %s, boundary %d, hold %s (it held %s)\n",
		selection.SentinelKey, selectionStreamName, want.Cutover, want.Boundary, want.Hold, was)
	return nil
}

// repairWant is the sentinel the broker's records give, as selection.KVStore.WriteSentinel writes
// it (its own key, selection.SentinelKey, and the session family), each read verified as the
// hold's client verifies every row and record (holdrecord.Client.Cutovers, Records): the session
// family's cutover row (repairRow) gives the state; for done, the hold it names is the cutover's;
// for rolled_back it names the rollback's, whose saved set records the cutover's hold (restoreSet's
// CutoverHold); and that cutover hold's cohort record gives the boundary B. The sentinel is the one
// convert and restore --finish leave, and nothing else: create-bucket writes {boundary B, hold}
// (createbucket.go createBucketCutoverHeld), convert adds cutover = done (convert.go markDone), and
// restore --finish sets cutover = rolled_back keeping the rest (restore.go restoreMark). A selection
// recovery's fields (replay_from, orphans, backup_sha256) are create-bucket --after-loss's, and the
// session sentinel carries no stream identity. A read that fails, or a record that is missing, is
// UNKNOWN.
func (r *run) repairWant(ctx context.Context) (selection.StoreState, error) {
	rows, err := r.records.Cutovers(ctx)
	if err != nil {
		return selection.StoreState{}, unknownf(err, "the broker's cutover rows")
	}
	row, err := repairRow(rows)
	if err != nil {
		return selection.StoreState{}, err
	}
	cutoverHold := row.HoldID
	if row.State == selection.CutoverRolledBack {
		set, found, err := readRestoreSet(ctx, r.records, row.HoldID)
		if err != nil {
			return selection.StoreState{}, unknownf(err, "the saved set of hold %s, which the broker's %s cutover row names", row.HoldID, familySession)
		}
		if !found || set.CutoverHold == "" {
			return selection.StoreState{}, unknownf(nil, "hold %s, whose rollback the broker's %s cutover row names, records no cutover hold in its saved set (restore --prepare's %s record %s), and the sentinel's boundary is that cutover's",
				row.HoldID, familySession, restoreSetKind, restoreSetKey)
		}
		cutoverHold = set.CutoverHold
	}
	cohort, _, found, err := readPrepareCohort(ctx, r.records, cutoverHold)
	if err != nil {
		return selection.StoreState{}, unknownf(err, "the cohort record of hold %s", cutoverHold)
	}
	if !found {
		return selection.StoreState{}, unknownf(nil, "hold %s, the session family's cutover, has no cohort record: consumer-names prepare writes it, with the boundary B the sentinel holds", cutoverHold)
	}
	if cohort.Boundary == 0 {
		return selection.StoreState{}, unknownf(nil, "the cohort record of hold %s holds no boundary B, which is at least 1", cutoverHold)
	}
	return selection.StoreState{Key: selection.SentinelKey, Family: selection.SessionFamily, Cutover: row.State, Boundary: cohort.Boundary, Hold: cutoverHold}, nil
}

// repairRow is the session family's broker cutover row among rows. No such row, or one that reads
// neither done nor rolled_back, is refused, naming it: no cutover's records then give the sentinel,
// which before a cutover is create-bucket's.
func repairRow(rows []holdrecord.CutoverRow) (holdrecord.CutoverRow, error) {
	for _, row := range rows {
		if row.Family != familySession {
			continue
		}
		if row.State != selection.CutoverDone && row.State != selection.CutoverRolledBack {
			return row, fmt.Errorf("the broker's %s cutover row reads %q from hold %s, neither %s nor %s: repair-sentinel writes the sentinel a cutover or its rollback left",
				familySession, row.State, row.HoldID, selection.CutoverDone, selection.CutoverRolledBack)
		}
		return row, nil
	}
	return holdrecord.CutoverRow{}, fmt.Errorf("the broker holds no %s cutover row: no cutover converted the session family, so no record gives its sentinel, which before a cutover create-bucket writes",
		familySession)
}

// repairHeld is repair-sentinel's work after its intent: the sentinel key's last message read
// behind the bucket's barrier (writerhold.ReadBucket) and decided on (repairDecide), and the
// records' sentinel, want, whose JSON is value, written over it at its sequence; the changes are
// counted in left. It answers what the key held.
func (r *run) repairHeld(ctx context.Context, nc *nats.Conn, store familyStore, want selection.StoreState, value []byte, left *createBucketLeft) (string, error) {
	for attempt := 1; ; attempt++ {
		var last replayMessage
		var found bool
		left.barrier = true
		if err := behindBarrier(ctx, nc, r.o.holdID, func(ctx context.Context) error {
			var err error
			last, found, err = replayLeaderRead(ctx, nc, selection.SessionBucket, selection.SentinelKey)
			return err
		}); err != nil {
			return "", err
		}
		r.read("the last message of " + selection.SentinelKey + " behind the bucket barrier")
		write, was, err := repairDecide(last, found, value)
		if err != nil || !write {
			return was, err
		}
		left.unsure = "the sentinel of " + selectionStreamName + ", if its unanswered write landed"
		_, err = store.WriteSentinel(ctx, want, last.Seq)
		if err == nil {
			left.unsure, left.sentinel = "", true
			return was, nil
		}
		if errors.Is(err, selection.ErrConflict) {
			left.unsure = ""
			if attempt < replayWrites {
				continue
			}
		}
		return "", fmt.Errorf("write the sentinel of %s: %w", selection.SessionBucket, err)
	}
}

// What the sentinel key held, as repair-sentinel's done entry and its line name it.
const (
	repairWasMalformed = "a malformed value"
	repairWasOther     = "another store state"
	repairWasRecords   = "the records' sentinel"
)

// repairDecide is what repair-sentinel does with last, the sentinel key's last message (found false
// for a key with no message), given want, the records' sentinel as json.Marshal writes it: it
// answers whether to write want over last, and what last held. A key with no value, or whose last
// message is a KV delete or purge marker, is refused: a lost sentinel is a lost bucket's, whose
// records a selection recovery's create-bucket --after-loss brings back before it writes the
// sentinel. So is a store state holding a selection recovery's fields, which create-bucket
// --after-loss writes and this command never does. A value that is want already is left as it is;
// every other value, malformed (readSelectionValue refuses it, a sentinel of another key or family
// among them) or not, is written over.
func repairDecide(last replayMessage, found bool, want []byte) (bool, string, error) {
	switch {
	case !found:
		return false, "", fmt.Errorf("selection bucket %s holds no %s: a lost sentinel is a lost bucket's, which a selection recovery's create-bucket --after-loss writes",
			selection.SessionBucket, selection.SentinelKey)
	case last.Op != "":
		return false, "", fmt.Errorf("selection bucket %s's key %s at sequence %d is a KV %s marker: a lost sentinel is a lost bucket's, which a selection recovery's create-bucket --after-loss writes",
			selection.SessionBucket, selection.SentinelKey, last.Seq, last.Op)
	case bytes.Equal(last.Data, want):
		return false, repairWasRecords, nil
	}
	value, err := readSelectionValue(statusKey{Key: selection.SentinelKey, Value: last.Data})
	if err != nil {
		return true, repairWasMalformed, nil
	}
	state := value.state
	if state.ReplayFrom != 0 || len(state.Orphans) > 0 || state.BackupSHA256 != "" {
		return false, "", fmt.Errorf("selection bucket %s's key %s at sequence %d is a selection recovery's sentinel (hold %s): create-bucket --after-loss writes it, never this command",
			selection.SessionBucket, selection.SentinelKey, last.Seq, state.Hold)
	}
	return true, repairWasOther, nil
}
