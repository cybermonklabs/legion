package legion

// whole_read.go holds the intake's whole read of a daemon's state (dispatch://INFRA-3931, the
// plan's D9): the reasons a read is made, how soon the next one may be and why a read the daemon
// answered fails all the same (ReadReason, ReadRetryAfter, the Err values); the read as it is
// fetched and judged (wholeRead, fetchWhole); its record and its log line (finishWhole,
// recordWhole, wholeReadLine); and the read made on a project's runner (Intake.readWhole,
// Intake.makeRead, Intake.reads) and made for a caller outside the intake (ReadWhole). What a
// read's record leaves on the cursor is in cursor.go (cursor.afterRead, owedSince).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ReadRetryAfter is how long a project's whole read waits after the one before it began, for
// every reason, the starting read included (read-rule-cases v11). The cursor keeps the start of the
// project's newest whole read (last_read_started_at), and a write of the setting does not reset
// it. What a call for a read inside the minute does, and which message holds the read until it is
// due, is the read rule's (cursor.readFor). No recurring timer makes a read. A project with no
// read yet has no read to wait after.
const ReadRetryAfter = time.Minute

// The reasons a whole read is made, which legion_reads.reason records and the whole read's log
// line names. There are no others: no recurring timer reads a daemon. A reason that calls for a
// read while one is owed joins the owed read (cursor.readReasons), and the read is recorded under
// the first reason it was owed for and logged with every one it joined (owedRead).
const (
	// ReadReasonStart is a project with a setting and no starting point: no cursor, or one that
	// follows another daemon than the setting names, or none at all, as a write of the setting
	// leaves it (settingWritten). It is read at the intake's take-over (which covers the durable's
	// first creation), on a settings change, and at the project's next message that calls for it
	// (judge) if neither has read it yet, at none of them while startBlocked blocks it. Where a
	// message holds it, the redelivery of that message makes it, and neither a take-over nor a
	// settings change does (cursor.readFor). It is due ReadRetryAfter after the project's newest
	// whole read began, as every read is.
	ReadReasonStart = "start"
	// ReadReasonBoot is a boot event that does not follow the cursor: its sequence is not the next
	// one, or it is at or below the cursor under another run (a daemon whose counter went back).
	ReadReasonBoot = "boot"
	// ReadReasonSchema is a boot event above the cursor whose schemaVersion is not the one the
	// cursor holds: the boot moved the store's schema, and a migration may have rewritten what
	// Dispatch observes outside the writes that publish events.
	ReadReasonSchema = "schema"
	// ReadReasonGap is a message whose first sequence is past the next one: events the stream no
	// longer holds. A message that does not continue the parts of a split transaction the intake
	// holds (part) is one too, since the transaction cannot be completed.
	ReadReasonGap = "gap"
	// ReadReasonRetry is a read owed since the last one failed.
	ReadReasonRetry = "retry"
	// ReadReasonStore is a message whose stateId is not the cursor's: it is counted in another
	// store than the one the starting point read, so its sequences say nothing of the cursor's.
	// The read finds the store the daemon the setting names counts in now.
	ReadReasonStore = "store"
	// ReadReasonRefused is a message the database refused for good (refusedForGood): it is not
	// applied, and the read puts the project's observations at the daemon's state. The message is
	// acknowledged, or goes back to the stream where it holds the owed read (cursor.readFor).
	ReadReasonRefused = "refused"
)

// Why a whole read the daemon answered is a failed read all the same. Each leaves the read owed;
// all but the first record the read's observations, since the state is the project's own.
var (
	// ErrStateOfAnotherProject is a state whose daemon.project is not the setting's project.
	ErrStateOfAnotherProject = errors.New("the daemon serves another project's state")
	// ErrNoStateSeq is a state with no daemon.stateSeq: a daemon without state events.
	ErrNoStateSeq = errors.New("the daemon serves no daemon.stateSeq, so it publishes no state events")
	// ErrStateSeqZero is a state whose daemon.stateSeq is 0: a daemon that has committed no state
	// event, which a daemon with a workflow never serves, since its boot event commits first.
	ErrStateSeqZero = errors.New("the daemon serves daemon.stateSeq 0: it has published no state event")
	// ErrNoDispatchURL is a state whose settings carry no dispatch_url: a daemon that runs no
	// workflow now, so it publishes no state events, whatever it published before. A daemon that
	// never ran one serves daemon.stateSeq 0 too, and this is the reason its read names.
	ErrNoDispatchURL = errors.New("the daemon's settings carry no dispatch_url: it runs no workflow, so it publishes no state events")
	// ErrNoStateID is a state with a daemon.stateSeq and no daemon.stateId: no message can be
	// matched to the store that sequence is counted in.
	ErrNoStateID = errors.New("the daemon serves no daemon.stateId, so its state messages cannot be matched to its store")
)

// wholeReadLine is the log line of each whole read the intake records, at INFO (finishWhole). A
// read whose setting moved or went before its record logs another line there and records nothing,
// and a read that a stop of the intake cuts short logs neither (readWhole).
const wholeReadLine = "legion intake: whole read"

// makeRead makes read, the whole read the attempt's message calls for, outside any transaction
// (readWhole), and answers outcomeAgain, so the attempt is made again on the cursor the read left.
// It sets readMade once readWhole has recorded the read, after which the message calls for no read
// of its own (ownRead), and readGood where the read recorded was a good one. applyMessage and
// readRefused call it.
func (in *Intake) makeRead(ctx context.Context, attempt *messageAttempt, read owedRead) (outcome, error) {
	// A read takes up to RequestTimeout: the message is still in hand.
	inProgress(attempt.msg)
	recorded, err := in.readWhole(ctx, read, &attempt.pending)
	if err != nil {
		return outcomeDone, err
	}
	if recorded != nil {
		attempt.readMade, attempt.readGood = true, recorded.failure == nil
	}
	return outcomeAgain, nil
}

// wholeRead is one whole read of a daemon's state as fetchWhole made it, held until it is recorded
// (recordWhole), so a record the database refuses is made again without reading the daemon again.
type wholeRead struct {
	stateURL string
	// stateSeq is the daemon.stateSeq the state serves, stateID its daemon.stateId, stateRun its
	// daemon.stateRun and schemaVersion its daemon.schemaVersion, each nil for none.
	stateSeq      *int64
	stateID       *string
	stateRun      *string
	schemaVersion *int64
	// observations is what the state observes (State.Observations), recorded where observed is
	// true: the daemon answered with the setting's project's state, failed read or not. bytes is
	// the answer's size as the read knows it (Client.State).
	observations []Observation
	observed     bool
	bytes        int64
	// started is Dispatch's clock before the call to the daemon: the read's start, which the next
	// read waits ReadRetryAfter after (last_read_started_at), however long the daemon takes to
	// answer. at is Dispatch's clock when the daemon had answered: the read's time, which its
	// legion_reads row and its observations take. took is how long the call took.
	started time.Time
	at      time.Time
	took    time.Duration
	// failure is why the read is a failed one, nil for a good read.
	failure error
}

// fetchWhole reads setting's daemon's state and judges it: a read the daemon did not answer, or
// answered with another project's state, or with a state that cannot be a starting point
// (ErrNoDispatchURL, ErrNoStateSeq, ErrStateSeqZero, ErrNoStateID, judged in that order), is a
// failed read. A daemon that runs no workflow is judged by its settings first, whatever stateSeq it
// serves: it commits no state event, so it serves stateSeq 0 too. The read's start is started, the
// intake's clock before the call, and its time now's once the daemon has answered. A call presents
// no credential: `GET /legion/v1/state` authenticates nobody. Every Client shares the default
// transport, so a client per read still reuses the connection the last read left.
func fetchWhole(ctx context.Context, setting Settings, started time.Time, now func() time.Time) *wholeRead {
	call := time.Now()
	state, bytes, err := New(setting.StateURL).State(ctx)
	read := &wholeRead{stateURL: setting.StateURL, bytes: bytes, started: started, at: now().UTC(), took: time.Since(call)}
	if state.Daemon != nil {
		read.stateSeq, read.schemaVersion = state.Daemon.StateSeq, state.Daemon.SchemaVersion
		if state.Daemon.StateID != "" {
			read.stateID = &state.Daemon.StateID
		}
		if state.Daemon.StateRun != "" {
			read.stateRun = &state.Daemon.StateRun
		}
	}
	switch {
	case err != nil:
		read.failure = err
		return read
	case state.Daemon == nil || state.Daemon.Project != setting.Project:
		project := ""
		if state.Daemon != nil {
			project = state.Daemon.Project
		}
		read.failure = fmt.Errorf("%w: it serves %q, and the setting is %s's", ErrStateOfAnotherProject, project, setting.Project)
		return read
	}
	read.observations, read.observed = state.Observations(), true
	switch seq := read.stateSeq; {
	case JSONString(state.Settings["dispatch_url"]) == "":
		read.failure = ErrNoDispatchURL
	case seq == nil:
		read.failure = ErrNoStateSeq
	case *seq == 0:
		read.failure = ErrStateSeqZero
	case read.stateID == nil:
		read.failure = ErrNoStateID
	}
	return read
}

// ReadWhole is the intake's whole read (Intake.readWhole) of setting's daemon for reason, made once
// and on Dispatch's wall clock, for a caller that is not on the project's runner. It is the test
// seam of the read's record: the record's tests outside this package (api/legion_board_test.go,
// api/legion_issue_capture_test.go) and this package's own tests drive the intake's own read
// through it, and the intake itself calls readWhole. Its intake is built as NewIntake builds one,
// with every map, but for the durable (newIntake). Its record clears every reason the project's
// cursor owes as it begins, as the record of a read the read rule makes does (owedRead.clears). It
// answers readWhole's error, or the error of the cursor it could not read (position).
func ReadWhole(ctx context.Context, pool *store.Pool, setting Settings, reason string) error {
	in := newIntake(pool, nil)
	_, current, _, err := in.position(ctx, setting.Project)
	if err != nil {
		return err
	}
	owed := readOf(setting, reason)
	owed.clears = current.readReasons
	var pending *wholeRead
	_, err = in.readWhole(ctx, owed, &pending)
	return err
}

// readWhole reads the daemon of owed's setting whole and records it (D9's whole read, made for a
// reason and never on a timer), for owed's reasons, which name the one legion_reads records
// (owedRead): the state through Client.State (fetchWhole), its observations at one moment, the
// intake's now or just after the project's newest row (recordObservations), a legion_reads row of
// kind `read` at the intake's now, and the project's cursor set to the daemon's own
// daemon.stateSeq, daemon.stateId and daemon.schemaVersion, which takes the cursor back too after a
// database restore took the daemon's counter back, and to its daemon.stateRun where it serves one
// (recordWhole). A read that fails, and one fetchWhole judges failed, records a failed legion_reads
// row naming why, its observations where the state is the project's own, and leaves the read owed.
// Each read it records is announced on StateChannel and logged at INFO with its reasons
// (finishWhole, wholeReadLine).
//
// Before it calls the daemon, it writes the read's start on the project's cursor and empties the
// sequences the cursor skipped past stateMaxWaiting (writeReadStart, read-rule-cases row 7.6), so
// a read that a stop of the intake cuts short keeps its start. From the read's start until
// readWhole returns, the intake knows that start as the start of the read in flight on the
// project's runner (reading).
//
// It keeps the read it fetched in pending until it is recorded, so an attempt made again after a
// refused record records that read rather than reading the daemon again. It answers the read as it
// was recorded, failed or good, and nil where it was not recorded: when the setting moved or went
// meanwhile (recordWhole). It answers an error only for a write the database refused, the record
// or the write of the read's start, which empties the skipped sequences, and for a stop that cut
// the read short.
func (in *Intake) readWhole(ctx context.Context, owed owedRead, pending **wholeRead) (*wholeRead, error) {
	read := *pending
	if read == nil || read.stateURL != owed.setting.StateURL {
		started := in.now().UTC()
		if err := writeReadStart(ctx, in.pool, owed.setting.Project, started); err != nil {
			return nil, err
		}
		in.reads(owed.setting.Project, &started)
		defer in.reads(owed.setting.Project, nil)
		read = fetchWhole(ctx, owed.setting, started, in.now)
		if err := ctx.Err(); err != nil {
			// The read was cut short by the stop, not by the daemon: nothing is recorded but its
			// start, written above, which the next read waits ReadRetryAfter after (row 7.6).
			return nil, err
		}
		*pending = read
	}
	recorded, err := finishWhole(ctx, in.pool, owed, read)
	if err != nil {
		return nil, err
	}
	*pending = nil
	return recorded, nil
}

// reads records started as the start of the whole read in flight on project's runner (reading),
// with no message past stateMaxWaiting judged since it began, and nil as no read in flight.
func (in *Intake) reads(project string, started *time.Time) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if started == nil {
		delete(in.reading, project)
		return
	}
	in.reading[project] = readInFlight{started: *started}
}

// finishWhole records read, made for owed (recordWhole, which announces it on StateChannel), then
// logs it (wholeReadLine), naming owed's recorded reason as its reason and every reason it joined
// as its reasons, comma-separated, and answers the read as it was recorded. A read it does not
// record, its setting moved or gone, it logs with a line of its own and answers as nil. A record
// Postgres refused is recorded as a failed read instead, its failure `record: <why>` and none of
// its observations, so the last good observations stand and the read is owed rather than its
// project's runner held on a value the database will never take. An error is answered only when
// that record is refused too: the database itself is failing, and the caller tries the record
// again in place.
func finishWhole(ctx context.Context, pool *store.Pool, owed owedRead, read *wholeRead) (*wholeRead, error) {
	project := owed.setting.Project
	recorded, written, err := recordWhole(ctx, pool, owed, read)
	if err != nil && ctx.Err() == nil {
		slog.Warn("legion intake: record a whole read; recording it as a failed read", "project", project, "reason", owed.reason, "error", err)
		refused := *read
		refused.failure, refused.observed = fmt.Errorf("record: %w", err), false
		if recorded, written, err = recordWhole(ctx, pool, owed, &refused); err == nil {
			read = &refused
		}
	}
	if err != nil || !recorded {
		if err == nil {
			slog.Info("legion intake: the Legion setting changed during a whole read; it is not recorded", "project", project, "reason", owed.reason)
		}
		return nil, err
	}
	attributes := []any{"project", project, "reason", owed.reason, "reasons", strings.Join(owed.joined, ","),
		"stateSeq", logValue(read.stateSeq), "stateId", logValue(read.stateID), "stateRun", logValue(read.stateRun),
		"bytes", read.bytes, "duration", read.took, "ok", read.failure == nil, "rows", written}
	if read.failure != nil {
		attributes = append(attributes, "error", read.failure.Error())
	}
	slog.Info(wholeReadLine, attributes...)
	return read, nil
}

// recordWhole writes read, made for owed, in one transaction that holds the project's position
// (lockCursor): the step every whole read's record takes (recordRead: its observations where it
// observed them, its legion_reads row naming owed's recorded reason, and its announcement), then
// the cursor the read leaves (cursor.afterRead), in one statement (writeCursor). That write sets
// the read's start once more: readWhole wrote it before the call to the daemon (read-rule-cases
// row 7.6), and writeCursor keeps the later of the two. A project with no cursor is given one that
// follows no daemon before anything is computed, so a message past stateMaxWaiting that
// judgeCrowded judges while the read is in flight either waits for this record or is seen by it,
// and nothing it recorded is written over (R3-h, read-rule-cases' second invariant). readWhole
// gave a cursor before the call to a project that had a setting then, so only a project whose
// setting was written during the read has none here. A good read deletes the project's held parts
// (part), which its snapshot holds, and so does a read of a cursor that followed another daemon. A
// failed read of the daemon the cursor follows keeps them: the read holds none of the events, and
// a part held is applied whole when its transaction's last part arrives.
//
// recorded is false when the project has no setting any more, and when its setting names another
// URL than the daemon read. Where it has no setting, nothing is written here. Where the setting
// names another URL, nothing of the read is written but its start (last_read_started_at), which
// readWhole wrote before the call already and which this writes again, the later of the two kept:
// the daemon was read all the same, so the read that the setting's change calls for waits
// ReadRetryAfter after it.
func recordWhole(ctx context.Context, pool *store.Pool, owed owedRead, read *wholeRead) (recorded bool, written int64, err error) {
	project := owed.setting.Project
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("open the transaction of the Legion state read of %s: %w", project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stateURL, current, found, err := lockCursor(ctx, tx, project)
	if err != nil || !found {
		return false, 0, err
	}
	if stateURL != read.stateURL {
		if _, err := tx.Exec(ctx, `
			update legion_event_cursors set last_read_started_at = greatest(last_read_started_at, $2) where project = $1
		`, project, read.started); err != nil {
			return false, 0, fmt.Errorf("record the start of a whole read of %s whose setting moved: %w", project, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, 0, fmt.Errorf("commit the start of a whole read of %s whose setting moved: %w", project, err)
		}
		return false, 0, nil
	}
	if written, err = recordRead(ctx, tx, project, owed.reason, read); err != nil {
		return false, 0, err
	}
	if err := writeCursor(ctx, tx, project, current.afterRead(owed, read)); err != nil {
		return false, 0, fmt.Errorf("set where the Legion events of %s stand: %w", project, err)
	}
	if read.failure == nil || !current.follows(stateURL) {
		if _, err := tx.Exec(ctx, `delete from legion_event_parts where project = $1`, project); err != nil {
			return false, 0, fmt.Errorf("delete the held state events of %s: %w", project, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, fmt.Errorf("commit the Legion state read of %s: %w", project, err)
	}
	return true, written, nil
}
