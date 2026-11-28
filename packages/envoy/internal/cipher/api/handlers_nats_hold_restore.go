package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// holdRestoreBody is POST /v1/nats-hold/restore's body.
type holdRestoreBody struct {
	// The gen to restore at: one above every gen nats_hold_log holds, 1 for an empty log.
	Gen int64 `json:"gen"`
	// held, which only after a verified prepare or held log entry of its hold id, or released,
	// which only after a verified released entry, a prepare entry of the hold id it names, or a held
	// one whose hold's journal shows it ended (restoreAdmitted).
	Phase string `json:"phase"`
	// The hold's id, which a held row names.
	HoldID string `json:"hold_id"`
	// Why the operator restores the row.
	Reason string `json:"reason"`
	// When the operator signed the row, RFC 3339 at a whole microsecond.
	SignedAt string `json:"signed_at"`
	// The operator's signature over the row's lines at gen (signedrecord.HoldRow).
	Sig string `json:"sig"`
}

// errHoldLogUnknown is a restore refused because a record it judges by reads as unknown: the log's
// latest entry does not verify, or the log is empty under a row this broker served, read or wrote
// (restoreTrusted); or a hold's done record, which heldByRecord judges, is below this broker's
// record floor, does not verify, or holds an entry whose hold row the verified log does not hold,
// whose log entry does not verify, or that names none (boundToLog). It is answered 409
// below_floor, naming the entry or the record, and nothing is stored.
var errHoldLogUnknown = errors.New("a record the restore judges by reads as unknown")

// handRepair is what a restore refusal names when no route can clear it: a database writer added
// a done record that does not verify, put an older one back (an ended hold's included), put
// another history's journal in this one's place without the log entries it names, deleted a live
// hold's done record or an ended hold's released entry, cut the log back under a hold's entries,
// or put an entry from another history back above a live hold (the log's latest entry, which the
// refusal names by its gen and hold id), and no route can tell those from a live hold's record a
// writer broke, so freeing the restore would free every client mid-hold. Another history's journal
// put back with the log entries it names is no such case: it binds, and the restore stores it
// (infra/README.md's cases that still store).
const handRepair = `no route can clear this, so a database admin deletes or restores the rows named here (infra/README.md, "Restoring a lost hold row")`

// restoreNATSHold writes the hold row while there is none, once its signature verifies, judging
// from the log's latest entry, which must verify (restoreTrusted), whether the row may be restored
// after it (restoreAdmitted). A latest entry that does not verify, or that this broker's floor of
// the row reads as a replay or a fork (a restore at or below the floor), is 409 below_floor and
// stores nothing. The row's signed_at is judged before the write (rowSignedAtAdmitted): one outside
// the allowance is 409 clock_skew, and nothing is stored.
func (s *server) restoreNATSHold(w http.ResponseWriter, r *http.Request, operator string) {
	var body holdRestoreBody
	if !readHoldBody(w, r, &body) {
		return
	}
	signedAt, ok := holdSignedAt(body.SignedAt)
	if body.Gen < 1 || !ok {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_request", "gen is a gen, at least 1, and signed_at an RFC 3339 time at a whole microsecond", nil)
		return
	}
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, body.Sig, signedrecord.HoldRow(uint64(body.Gen), body.Phase, body.HoldID, signedAt)); err != nil {
		writeHoldRefusal(w, http.StatusBadRequest, "bad_signature", "no key of BROKER_NATS_HOLD_SIGNING_KEYS signed the row at its gen: "+err.Error(), nil)
		return
	}
	admit := func(log []natshold.LogEntry, journals []natshold.Record, next natshold.Row) error {
		if err := s.rowSignedAtAdmitted(next.Gen, next.SignedAt, log); err != nil {
			return err
		}
		return s.restoreAdmitted(log, journals, next)
	}
	row, err := s.holdStore().Restore(r.Context(), natshold.Row{
		Gen: uint64(body.Gen), Phase: body.Phase, HoldID: body.HoldID, Reason: body.Reason, UpdatedBy: operator, SignedAt: signedAt, Sig: body.Sig,
	}, natshold.RestoreJudge{Trust: s.restoreTrusted, Admit: admit})
	var conflict *natshold.Conflict
	switch {
	case err == nil:
	case errors.Is(err, errRowSkewed):
		writeHoldRefusal(w, http.StatusConflict, "clock_skew", "the restore stores nothing: "+err.Error(), nil)
		return
	case errors.Is(err, errHoldLogUnknown), errors.Is(err, signedrecord.ErrReplay), errors.Is(err, signedrecord.ErrFork), errors.Is(err, signedrecord.ErrBackdated), errors.Is(err, natshold.ErrGenLogged):
		writeHoldRefusal(w, http.StatusConflict, "below_floor", "the restore stores nothing: "+err.Error(), nil)
		return
	case !errors.As(err, &conflict):
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "restore the hold row", err)
		return
	case errors.Is(err, natshold.ErrBadTransition):
		s.writeHoldConflict(w, http.StatusBadRequest, "bad_transition", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrHoldMismatch):
		s.writeHoldConflict(w, http.StatusConflict, "hold_mismatch", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrRowExists):
		s.writeHoldConflict(w, http.StatusConflict, "row_exists", err.Error(), conflict)
		return
	case errors.Is(err, natshold.ErrGenMismatch):
		s.writeHoldConflict(w, http.StatusConflict, "gen_mismatch", err.Error(), conflict)
		return
	default:
		writeInternal(w, "restore the hold row", err)
		return
	}
	// The restored row is one above the log's latest entry, which the floor accepted, so it raises
	// the floor once it verifies as the store answered it.
	if err := s.acceptWritten(row); err != nil {
		writeHoldRefusal(w, http.StatusConflict, "below_floor", "the restored row reads as unknown: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, holdRowAnswer(row))
}

// restoreTrusted is a restore's trust in the log's latest entry, before any gen is compared: the
// entry must verify against BROKER_NATS_HOLD_SIGNING_KEYS, since a database writer can add or
// change a log row but not one that verifies, and this broker's floor of the row must take it,
// which refuses an entry below a row the floor has accepted (so a restore at or below the floor)
// and another entry at the floor's gen. An empty log is trusted only by a floor that has accepted
// nothing: under a row this broker served, read or wrote, it is a log a writer emptied.
func (s *server) restoreTrusted(latest *natshold.LogEntry) error {
	if latest == nil {
		if s.holdRowFloor.Reached(1) {
			return fmt.Errorf("%w: the log is empty, under a hold row this broker has served, read or written", errHoldLogUnknown)
		}
		return nil
	}
	lines := signedrecord.HoldRow(latest.Gen, latest.Phase, latest.HoldID, latest.SignedAt)
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, latest.Sig, lines); err != nil {
		return fmt.Errorf("%w: the entry at gen %d: %w", errHoldLogUnknown, latest.Gen, err)
	}
	return s.holdRowFloor.Accept(latest.Gen, latest.SignedAt, lines)
}

// restoreAdmitted is whether next may be restored after the log's latest entry, which verified
// (restoreTrusted), given journals, every hold's done record. An empty log takes only the first
// row's released. Released follows a released entry; a prepare entry, since a prepare row refuses
// no client, only with that entry's hold id (409 hold_mismatch otherwise, naming that hold): a
// released row under another id leaves the prepared hold's cohort and journal where no command
// reaches them, since Admit and the cohort delete need its id on the row and it cannot be prepared
// again; or a held one whose hold's journal, the done record, verifies and shows the hold ended (a
// done hold release or hold abort), as when the row was lost between that done entry and the row's
// move; after any other held entry it would free every client mid-hold. After a held entry whose
// hold has no done record, which a held hold always has, a database writer deleted it: the
// refusal names the hand repair, not a held restore, which the operator's client refuses for a
// hold with no done record. Whatever the latest entry, a released row is refused while a hold's
// own record holds it (heldByRecord): that hold is held by its own record, and a log a database
// writer emptied or cut back, on a replica whose floor started again, would otherwise free every
// client mid-hold. Held follows only a prepare or held entry of its own hold id, so a hold the log
// shows ended, by a release or by abort from prepare, which leaves no journal entry, is not held
// again.
func (s *server) restoreAdmitted(log []natshold.LogEntry, journals []natshold.Record, next natshold.Row) error {
	refuse := func(why string) error {
		return &natshold.Conflict{Err: fmt.Errorf("%w: %s", natshold.ErrBadTransition, why)}
	}
	var latest *natshold.LogEntry
	if len(log) > 0 {
		latest = &log[len(log)-1]
	}
	if next.Phase == string(natsgrant.OperatorPhaseReleased) {
		if err := s.heldByRecord(log, latest, journals); err != nil {
			return err
		}
	}
	if latest == nil {
		if next.Phase == string(natsgrant.OperatorPhaseHeld) {
			return refuse("the log is empty, and a held row is restored only after a prepare or held entry of its own hold")
		}
		return nil
	}
	if next.Phase == string(natsgrant.OperatorPhaseHeld) {
		if (latest.Phase == string(natsgrant.OperatorPhasePrepare) || latest.Phase == string(natsgrant.OperatorPhaseHeld)) && latest.HoldID == next.HoldID {
			return nil
		}
		return refuse(fmt.Sprintf("the log's latest entry is %s with hold id %q, and a held row is restored only after a prepare or held entry of its own hold", latest.Phase, latest.HoldID))
	}
	switch latest.Phase {
	case string(natsgrant.OperatorPhasePrepare):
		if next.HoldID != latest.HoldID {
			return &natshold.Conflict{Err: fmt.Errorf("%w: the log's latest entry is prepare with hold id %q, and a released row after it names that hold, or the prepared hold's cohort and journal are left where no command reaches them", natshold.ErrHoldMismatch, latest.HoldID), HoldID: latest.HoldID}
		}
		return nil
	case string(natsgrant.OperatorPhaseHeld):
	default:
		return nil
	}
	journal := journalOf(journals, latest.HoldID)
	if journal == nil {
		return refuse(fmt.Sprintf("the log's latest entry is held with hold id %q, and that hold has no done record, which a held hold always has: a database writer deleted it, so no released restore is judged after it, and a held one is refused too, since its client reads the journal first; %s", latest.HoldID, handRepair))
	}
	if s.holdEnded(latest.HoldID, journal) {
		return nil
	}
	return refuse("the log's latest entry is held, and its hold's journal shows no done hold release or hold abort, so a released row restored after it would free every client mid-hold; restore the row held with its hold id")
}

// heldByRecord refuses a released restore while a hold's own record holds it. It first judges
// every hold's done record, an ended hold's included: one below this replica's record floor (an
// older signed version put back), one that does not verify, or one whose entries name rows this
// log does not hold as verified entries (boundToLog: another history's journal put back without
// the log entries it names, a cut log, or entries a key no longer listed signed, or an entry that
// names none) is unknown (409 below_floor). It then skips a hold whose journal shows it ended (a
// done hold release or hold abort), and a hold the verified log names released (a released entry
// naming it, at any gen) while its journal shows hold release's or hold abort's intent as that
// command's latest entry and no other command's intent pending: the end that history admitted,
// cut before its done entry. A released entry ends nothing else: a live hold's own released entry
// from another history, put back, says nothing of a hold idle between commands, stopped, or
// running one. A journal that shows hold set done and no end refuses the restore
// (ErrBadTransition), and so does one whose hold set is not done while the verified log holds the
// hold's held entry: the row reached held, so hold set runs, or stopped, under a live hold. A log
// cut back below a hold's own entries shows nothing of that hold, so its record still refuses.
// Each refusal names the hold and why its record holds the restore. It advises restoring the row
// held only when the route admits that restore, after latest, the log's latest entry, when that is
// the hold's own prepare or held entry; otherwise it names latest by its phase, gen and hold id,
// the causes and the hand repair (handRepair), and for a signature whose key left
// BROKER_NATS_HOLD_SIGNING_KEYS, that key's id.
func (s *server) heldByRecord(log []natshold.LogEntry, latest *natshold.LogEntry, journals []natshold.Record) error {
	ix := s.indexLog(log)
	for i := range journals {
		holdID := journals[i].HoldID
		if s.holdFloors.below(recordFloorKey{holdID: holdID, kind: natshold.KindDone, key: holdJournalKey}, journals[i].Gen) {
			return fmt.Errorf("%w: hold %q's done record is at gen %d, below a gen this broker wrote or served, an older version put back; %s", errHoldLogUnknown, holdID, journals[i].Gen, handRepair)
		}
		d, err := s.journalDone(holdID, &journals[i])
		if err != nil {
			return unknownJournal(holdID, err)
		}
		if err := boundToLog(holdID, d, ix); err != nil {
			return err
		}
		if journalEnded(d) {
			continue
		}
		running := runningCommand(d)
		releasedGen, releasedInLog := ix.released[holdID]
		if releasedInLog && running == "" && endAdmitted(d) {
			continue
		}
		heldGen, heldInLog := ix.held[holdID]
		_, setDone := d.Latest(holdrecord.CommandSet, holdrecord.StateDone)
		if !setDone && !heldInLog {
			continue
		}
		state := "shows hold set done and no end"
		if !setDone {
			state = fmt.Sprintf("shows %s and no end, while the log holds its held entry at gen %d", setState(d), heldGen)
		}
		held := fmt.Errorf("%w: hold %s's journal %s, so a released row restored now would free every client mid-hold", natshold.ErrBadTransition, holdID, state)
		if releasedInLog {
			shows := "neither hold release's nor hold abort's intent as that command's latest entry"
			if running != "" {
				shows = running + "'s intent pending"
			}
			held = fmt.Errorf("%w; the log's released entry naming it, at gen %d, did not end it, since its journal shows %s: that entry was put back from another history, or this journal is an older version of this hold's journal put back", held, releasedGen, shows)
		}
		if latest != nil && latest.HoldID == holdID && (latest.Phase == string(natsgrant.OperatorPhasePrepare) || latest.Phase == string(natsgrant.OperatorPhaseHeld)) {
			return &natshold.Conflict{Err: fmt.Errorf("%w; restore the row held with its hold id %s", held, holdID)}
		}
		after := "the log is empty"
		if latest != nil {
			after = fmt.Sprintf("the log's latest entry, %s at gen %d with hold id %q, is not that hold's prepare or held entry", latest.Phase, latest.Gen, latest.HoldID)
		}
		return &natshold.Conflict{Err: fmt.Errorf("%w, and %s, so no held restore of it is admitted either: the log was cut back under the live hold, the ended hold's released entry was deleted, an entry from another history put back above a live hold is the latest entry, or an older version of this hold's journal was put back; %s", held, after, handRepair)}
	}
	return nil
}

// setState says where d's hold set stands short of done: its intent pending, stopped, or not
// begun.
func setState(d holdrecord.Done) string {
	e, ok := d.Latest(holdrecord.CommandSet)
	switch {
	case !ok:
		return "no hold set entry"
	case e.State == holdrecord.StateStop:
		return "hold set stopped"
	}
	return "hold set's intent pending"
}

// endAdmitted reports whether d's latest entry of hold release or of hold abort is its intent:
// the end that journal's history admitted, its row move and done entry still to come or cut.
func endAdmitted(d holdrecord.Done) bool {
	for _, end := range []string{holdrecord.CommandRelease, holdrecord.CommandAbort} {
		if e, ok := d.Latest(end); ok && e.State == holdrecord.StateIntent {
			return true
		}
	}
	return false
}

// runningCommand is a command whose intent is d's latest entry of it, other than hold release's
// and hold abort's, and "" when there is none: a command that may be running, since its intent was
// written and neither its done entry nor a stop followed. Hold release's and hold abort's intents
// are the hold's end, cut before their done entry.
func runningCommand(d holdrecord.Done) string {
	for _, o := range d.Ops {
		if o.Command == holdrecord.CommandRelease || o.Command == holdrecord.CommandAbort {
			continue
		}
		if latest, ok := d.Latest(o.Command); ok && latest.State == holdrecord.StateIntent {
			return o.Command
		}
	}
	return ""
}

// boundToLog answers errHoldLogUnknown, naming holdID, the entry and the hand repair, for the
// first entry of d whose hold row (Operation.Row, the row it was written under) ix, the log's
// entries by gen, does not hold as a verified entry at that gen with that signing time, or that
// names no row. The operator's client names the row of every entry it writes, and every such row
// is a log entry of the history the journal was written in, so a journal of another history put
// in this one's place without the log entries it names names a row this log does not hold once
// that history wrote an entry under a row it moved to (its release's done entry, written under
// its released row); so does a journal whose hold's log entries a writer cut back or deleted,
// which the restore cannot tell from it. A journal of a history cut after its row's move and
// before that done entry names only the held row the histories share, so it binds even put back
// alone, and heldByRecord then refuses it 400 for showing hold set done and no end. A journal from
// a fork taken while the row was held, put back together with that history's later log entries,
// binds: every row its entries name is then in the log (infra/README.md states that case among
// those that still store). An entry whose row is in the log but does not verify names that log
// entry's Verify error, and the key id of a key the list no longer holds with the step-4 repair
// (keyRepair): a log entry a retired key signed is not a missing one. This PR writes the first
// journals, so no journal the routes wrote names no row.
func boundToLog(holdID string, d holdrecord.Done, ix logIndex) error {
	for i, o := range d.Ops {
		entry := fmt.Sprintf("hold %q's journal entry %d (%s %s)", holdID, i, o.Command, o.State)
		if o.Row == nil {
			return fmt.Errorf("%w: %s names no hold row it was written under, which every entry the operator's client writes names; %s", errHoldLogUnknown, entry, handRepair)
		}
		if at, ok := ix.rows[o.Row.Gen]; ok && at.Equal(o.Row.SignedAt) {
			continue
		}
		if err, ok := ix.unverified[o.Row.Gen]; ok {
			return fmt.Errorf("%w: %s was written under the hold row at gen %d, whose log entry does not verify: %v; %s",
				errHoldLogUnknown, entry, o.Row.Gen, err, keyRepair(err))
		}
		return fmt.Errorf("%w: %s was written under the hold row at gen %d signed at %s, which the verified log does not hold: a journal of another history of the tables put in this one's place without the log entries it names, or a log cut back or with entries deleted; %s",
			errHoldLogUnknown, entry, o.Row.Gen, o.Row.SignedAt.UTC().Format(time.RFC3339Nano), handRepair)
	}
	return nil
}

// unknownJournal is heldByRecord's refusal of holdID's done record, which err says does not
// verify, with the repair keyRepair names.
func unknownJournal(holdID string, err error) error {
	return fmt.Errorf("%w: hold %q's done record does not verify: %v; %s", errHoldLogUnknown, holdID, err, keyRepair(err))
}

// keyRepair is the repair a restore refusal names for a record or log entry that err says does
// not verify. One signed by a key no longer listed names that key's id: the log and the journals
// are kept, so a retired key stays listed, verify-only, until the hand repair deletes what it
// signed. Every refusal names the hand repair, the only one for a record a database writer added
// or broke, or one a leaked key signed.
func keyRepair(err error) string {
	var unlisted *signedrecord.UnlistedKeyError
	if errors.As(err, &unlisted) {
		return fmt.Sprintf("if key id %s is a retired key's, it goes back in BROKER_NATS_HOLD_SIGNING_KEYS, verify-only: the log and the journals are kept, so a retired key stays listed until the hand repair deletes what it signed (infra/README.md, \"Rotating the hold signing key\", step 4); otherwise %s", unlisted.KeyID, handRepair)
	}
	return handRepair
}

// logIndex is what heldByRecord reads from the log, each entry verified once: each verified
// entry's signing time by its gen, the Verify error of each entry that does not verify by its gen,
// and the gen of each hold's latest verified released and latest verified held entry. A prepare of
// another hold at a higher gen says nothing about a hold: it may be an entry of another history of
// the log (a point-in-time restore, or a log cut back and saved rows put back), and gens and
// signing times say nothing across holds.
type logIndex struct {
	rows       map[uint64]time.Time
	unverified map[uint64]error
	released   map[string]uint64
	held       map[string]uint64
}

// indexLog is log's logIndex: only the entries that verify count as rows, ends and held entries.
func (s *server) indexLog(log []natshold.LogEntry) logIndex {
	ix := logIndex{rows: map[uint64]time.Time{}, unverified: map[uint64]error{}, released: map[string]uint64{}, held: map[string]uint64{}}
	for _, e := range log {
		if err := s.logEntryVerified(e); err != nil {
			ix.unverified[e.Gen] = err
			continue
		}
		ix.rows[e.Gen] = e.SignedAt
		if e.HoldID == "" {
			continue
		}
		switch e.Phase {
		case string(natsgrant.OperatorPhaseReleased):
			ix.released[e.HoldID] = max(ix.released[e.HoldID], e.Gen)
		case string(natsgrant.OperatorPhaseHeld):
			ix.held[e.HoldID] = max(ix.held[e.HoldID], e.Gen)
		}
	}
	return ix
}

// journalOf is holdID's done record among journals, nil when there is none.
func journalOf(journals []natshold.Record, holdID string) *natshold.Record {
	for i := range journals {
		if journals[i].HoldID == holdID {
			return &journals[i]
		}
	}
	return nil
}

// holdEnded reports whether journal, holdID's done record, verifies and shows the hold ended
// (journalEnded).
func (s *server) holdEnded(holdID string, journal *natshold.Record) bool {
	d, err := s.journalDone(holdID, journal)
	return err == nil && journalEnded(d)
}

// journalEnded reports whether d shows the hold ended. It asks the journal's own Decide: it answers
// hold release ErrAlreadyDone only once the hold has ended, its first refusal (EndIsFinal).
func journalEnded(d holdrecord.Done) bool {
	return errors.Is(holdrecord.Decide(d, holdrecord.Request{Command: holdrecord.CommandRelease}), holdrecord.ErrAlreadyDone)
}

// journalDone is journal, holdID's done record, decoded, once its signature verifies against
// BROKER_NATS_HOLD_SIGNING_KEYS. Its error says why not: there is no record, no listed key verifies
// its signature (signedrecord.Verify's refusal, which names the signing key's id when the
// signature carries one, never the key), or its value is no journal.
func (s *server) journalDone(holdID string, journal *natshold.Record) (holdrecord.Done, error) {
	if journal == nil {
		return holdrecord.Done{}, errors.New("there is no done record")
	}
	if err := signedrecord.Verify(s.deps.NATSHold.Keys, journal.Sig, signedrecord.Record(holdID, natshold.KindDone, journal.Key, journal.Gen, journal.Value)); err != nil {
		return holdrecord.Done{}, err
	}
	var d holdrecord.Done
	if json.Unmarshal(journal.Value, &d) != nil {
		return holdrecord.Done{}, errors.New("its value is no journal")
	}
	return d, nil
}
