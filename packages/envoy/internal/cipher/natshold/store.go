package natshold

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/natsgrant"
)

// The refusals of a write, each the Err of the Conflict a write answers.
var (
	// ErrNoRow is a move of a hold row that is not there: deleted, or never restored.
	ErrNoRow = errors.New("natshold: there is no hold row; the operator restores it")
	// ErrGenMismatch is a write naming a gen the row or record no longer holds, or a restore
	// naming a gen other than the one above every logged gen.
	ErrGenMismatch = errors.New("natshold: the gen named is not the one stored")
	// ErrHoldMismatch is a write naming a hold id other than the current row's, or, for a cutover
	// row, a hold row that is not held.
	ErrHoldMismatch = errors.New("natshold: the hold id named is not the current hold row's")
	// ErrBadTransition is a move the hold's rule does not allow, or a restore of a phase it does
	// not restore.
	ErrBadTransition = errors.New("natshold: the hold row may not move to that phase with that hold id")
	// ErrRowExists is a restore while the hold row is there.
	ErrRowExists = errors.New("natshold: the hold row exists, so there is nothing to restore")
	// ErrGenLogged is a move or restore whose gen nats_hold_log already holds: the row a move
	// starts from is an older one a database writer put back below the log, or a writer added a log
	// row while the restore ran.
	ErrGenLogged = errors.New("natshold: the log already holds the gen the write would store")
)

// Conflict is a write a store refused for what the tables held: Err is which refusal, Current the
// hold row as the write read it (nil when there is none), RestoreGen, for a restore's
// ErrGenMismatch, the gen a restore must carry now, and HoldID, for ErrHoldMismatch, the hold id the
// hold row or log names, which the write must name instead.
type Conflict struct {
	Err        error
	Current    *Row
	RestoreGen uint64
	HoldID     string
}

func (c *Conflict) Error() string { return c.Err.Error() }
func (c *Conflict) Unwrap() error { return c.Err }

// Row is the hold row as the routes write and answer it: every column of nats_hold but its id.
type Row struct {
	Gen       uint64
	Phase     string
	HoldID    string
	Reason    string
	UpdatedAt time.Time
	UpdatedBy string
	SignedAt  time.Time
	Sig       string
}

// The kinds of hold record, as nats_hold_records' check holds them: the cohort a prepare writes,
// the conversion's saved set, the hold's journal and the selection-recovery inputs.
const (
	KindCohort    = "cohort"
	KindConvert   = "convert"
	KindDone      = "done"
	KindSelection = "selection"
)

// RecordKind reports whether kind is one of the four kinds of hold record.
func RecordKind(kind string) bool {
	switch kind {
	case KindCohort, KindConvert, KindDone, KindSelection:
		return true
	}
	return false
}

// The states of a family's cutover row: converted by the hold's conversion, or restored by its
// rollback.
const (
	CutoverDone       = "done"
	CutoverRolledBack = "rolled_back"
)

// Record is one hold record, keyed by its hold id, kind and key.
type Record struct {
	HoldID, Kind, Key string
	Value             json.RawMessage
	Gen               uint64
	WrittenAt         time.Time
	Sig               string
}

// Deletion is a record the routes deleted (nats_hold_record_deletes): its hold id, kind and key,
// the gen it was deleted at, when, and the operator's signature over the delete
// (signedrecord.RecordDelete), the one the delete's request carried. Every broker replica reads it
// beside the records, so each can verify that a record is gone by the operator's delete.
type Deletion struct {
	HoldID, Kind, Key string
	Gen               uint64
	DeletedAt         time.Time
	Sig               string
}

// Cutover is one family's cutover row: its state, and the hold id and hold row gen it was written
// under.
type Cutover struct {
	Family, State, HoldID string
	HoldGen               uint64
	WrittenAt             time.Time
	WrittenBy             string
	Sig                   string
}

// Store writes and reads the hold's tables in one schema: public in a deployment, a test's own in
// a test. Every statement runs in a transaction whose search path is pg_catalog and pg_temp alone,
// as Read's does, and names each table by its schema, so no object another writer put in a schema
// of the pool's search path is resolved in its place. It checks what the tables hold (the gen, the
// hold id and the allowed move) and verifies no signature: the routes verify each before they
// write, and the cutover's, which covers the hold row's gen, inside the write (PutCutover).
type Store struct {
	Pool   *pgxpool.Pool
	Schema string
}

// The columns a hold row is read and answered by, in Row's order.
const rowColumns = `gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig`

func (s Store) table(name string) string { return pgx.Identifier{s.Schema, name}.Sanitize() }

// in runs fn in one transaction under the hold's search path, committing when fn answers nil.
func (s Store) in(ctx context.Context, access pgx.TxAccessMode, fn func(pgx.Tx) error) error {
	return s.inTx(ctx, pgx.TxOptions{AccessMode: access}, fn)
}

// inTx is in with the transaction's options whole, its isolation level among them.
func (s Store) inTx(ctx context.Context, options pgx.TxOptions, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, options)
	if err != nil {
		return fmt.Errorf("natshold: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `set local search_path = pg_catalog, pg_temp`); err != nil {
		return fmt.Errorf("natshold: set the search path: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func scanRow(row pgx.Row) (Row, error) {
	var out Row
	var gen int64
	if err := row.Scan(&gen, &out.Phase, &out.HoldID, &out.Reason, &out.UpdatedAt, &out.UpdatedBy, &out.SignedAt, &out.Sig); err != nil {
		return Row{}, err
	}
	if gen < 0 {
		return Row{}, fmt.Errorf("natshold: the hold row's gen is %d", gen)
	}
	out.Gen = uint64(gen)
	return out, nil
}

// currentRow reads the hold row inside tx, taking lock on it ("for update", "for share", or ""
// for none), and answers found false for no row.
func (s Store) currentRow(ctx context.Context, tx pgx.Tx, lock string) (Row, bool, error) {
	row, err := scanRow(tx.QueryRow(ctx, `select `+rowColumns+` from `+s.table("nats_hold")+` where id = 1 `+lock))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Row{}, false, nil
	case err != nil:
		return Row{}, false, fmt.Errorf("natshold: read the hold row: %w", err)
	}
	return row, true, nil
}

// Row reads the hold row whole, answering found false for no row.
func (s Store) Row(ctx context.Context) (Row, bool, error) {
	var row Row
	var found bool
	err := s.in(ctx, pgx.ReadOnly, func(tx pgx.Tx) error {
		var err error
		row, found, err = s.currentRow(ctx, tx, "")
		return err
	})
	return row, found, err
}

// moveRefusal is why the row current may not move to phase under holdID, or nil when it may. The
// hold's rule: released to prepare with a new hold id, which is not empty and is no hold id the
// log holds (used), the current row's included, so no record of an earlier hold is read as this
// one's; prepare to held with the same hold id; prepare or held to released with the same hold id,
// which the row keeps. Every hold goes through prepare, and no phase moves to itself.
func moveRefusal(current Row, phase, holdID string, used bool) error {
	released, prepare, held := string(natsgrant.OperatorPhaseReleased), string(natsgrant.OperatorPhasePrepare), string(natsgrant.OperatorPhaseHeld)
	switch {
	case current.Phase == released && phase == prepare:
		if holdID == "" || holdID == current.HoldID || used {
			return fmt.Errorf("%w: a hold begins with a new hold id, never empty and never one an earlier hold held", ErrBadTransition)
		}
		return nil
	case current.Phase == prepare && phase == held, (current.Phase == prepare || current.Phase == held) && phase == released:
		if holdID != current.HoldID {
			return ErrHoldMismatch
		}
		return nil
	}
	return fmt.Errorf("%w: %s may not move to %q", ErrBadTransition, current.Phase, phase)
}

// Move moves the hold row from expectedGen to next's phase, hold id, reason, writer, signing time
// and signature, at gen expectedGen+1, and answers the row it stored. The row is locked while the
// move is checked, and the write is one statement: the update, which still names expectedGen, and
// its nats_hold_log row. accept sees the locked row first, with every log entry by gen as the same
// transaction reads it, and its error (the routes' floor of the row refusing an older row put back,
// or a new row's signing time) is answered as it is. It refuses with a Conflict: ErrNoRow,
// ErrGenMismatch when the row's gen is not expectedGen, moveRefusal's refusal, and ErrGenLogged
// when the log already holds the gen the move would store, which only a row put back below the log
// leads to.
func (s Store) Move(ctx context.Context, expectedGen uint64, next Row, accept func(current Row, log []LogEntry) error) (Row, error) {
	var moved Row
	err := s.in(ctx, pgx.ReadWrite, func(tx pgx.Tx) error {
		current, found, err := s.currentRow(ctx, tx, "for update")
		switch {
		case err != nil:
			return err
		case !found:
			return &Conflict{Err: ErrNoRow}
		}
		log, err := s.logIn(ctx, tx)
		if err != nil {
			return err
		}
		if err := accept(current, log); err != nil {
			return err
		}
		if current.Gen != expectedGen {
			return &Conflict{Err: ErrGenMismatch, Current: &current}
		}
		used := false
		if current.Phase == string(natsgrant.OperatorPhaseReleased) && next.Phase == string(natsgrant.OperatorPhasePrepare) && next.HoldID != "" {
			if err := tx.QueryRow(ctx, `select exists (select 1 from `+s.table("nats_hold_log")+` where hold_id = $1)`, next.HoldID).Scan(&used); err != nil {
				return fmt.Errorf("natshold: read the log's hold ids: %w", err)
			}
		}
		if err := moveRefusal(current, next.Phase, next.HoldID, used); err != nil {
			conflict := &Conflict{Err: err, Current: &current}
			if errors.Is(err, ErrHoldMismatch) {
				conflict.HoldID = current.HoldID
			}
			return conflict
		}
		moved, err = scanRow(tx.QueryRow(ctx, `with moved as (
				update `+s.table("nats_hold")+` set gen = gen + 1, phase = $2, hold_id = $3, reason = $4, updated_at = now(),
					updated_by = $5, signed_at = $6, sig = $7
				where id = 1 and gen = $1
				returning `+rowColumns+`
			), logged as (
				insert into `+s.table("nats_hold_log")+` (gen, phase, hold_id, reason, written_at, written_by, signed_at, sig)
				select gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig from moved
			)
			select `+rowColumns+` from moved`,
			int64(expectedGen), next.Phase, next.HoldID, next.Reason, next.UpdatedBy, next.SignedAt, next.Sig))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return &Conflict{Err: ErrGenMismatch, Current: &current}
		case store.IsUniqueViolation(err):
			// The statement's one unique key it can break is nats_hold_log's gen.
			return &Conflict{Err: fmt.Errorf("%w: gen %d", ErrGenLogged, expectedGen+1)}
		}
		return err
	})
	return moved, err
}

// LogEntry is one nats_hold_log row: the row a write stored, as the operator signed it, with its
// signing time and signature, which a reader verifies over signedrecord.HoldRow.
type LogEntry struct {
	Gen      uint64
	Phase    string
	HoldID   string
	SignedAt time.Time
	Sig      string
}

// RestoreJudge is what Restore's caller decides from the log, inside the restore's transaction,
// each answering nil to go on and its refusal otherwise, which Restore answers as it is. Trust sees
// the log's latest entry (nil for an empty log) before any gen is compared, and answers whether a
// restore may judge by it at all: the route verifies its signature, since a database writer can
// add or change a log row but not one that verifies. Admit sees, once next's gen is the one above
// the latest entry, every log entry by gen (the latest last) and every hold's done record, as the
// same transaction reads them, and answers whether next may be restored after them.
type RestoreJudge struct {
	Trust func(latest *LogEntry) error
	Admit func(log []LogEntry, journals []Record, next Row) error
}

// journalKey is the done record's one key, the hold's journal (internal/holdrecord's).
const journalKey = "hold"

// journalIn reads holdID's done record inside tx, nil when there is none.
func (s Store) journalIn(ctx context.Context, tx pgx.Tx, holdID string) (*Record, error) {
	rec, err := scanRecord(tx.QueryRow(ctx, `select `+recordColumns+` from `+s.table("nats_hold_records")+` where hold_id = $1 and kind = $2 and key = $3`,
		holdID, KindDone, journalKey), holdID, KindDone)
	switch {
	case err == nil:
		return &rec, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	}
	return nil, fmt.Errorf("natshold: read the journal of hold %s: %w", holdID, err)
}

// journalsIn reads every hold's done record inside tx, by hold id.
func (s Store) journalsIn(ctx context.Context, tx pgx.Tx) ([]Record, error) {
	rows, err := tx.Query(ctx, `select hold_id, `+recordColumns+` from `+s.table("nats_hold_records")+` where kind = $1 and key = $2 order by hold_id`,
		KindDone, journalKey)
	if err != nil {
		return nil, fmt.Errorf("natshold: read the journals: %w", err)
	}
	journals, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Record, error) {
		var holdID string
		var value string
		var gen int64
		out := Record{Kind: KindDone}
		if err := row.Scan(&holdID, &out.Key, &value, &gen, &out.WrittenAt, &out.Sig); err != nil {
			return Record{}, err
		}
		if gen < 1 {
			return Record{}, fmt.Errorf("natshold: a hold record's gen is %d", gen)
		}
		out.HoldID, out.Value, out.Gen = holdID, json.RawMessage(value), uint64(gen)
		return out, nil
	})
	if err != nil {
		return nil, fmt.Errorf("natshold: read the journals: %w", err)
	}
	return journals, nil
}

// Restore writes the hold row when it is absent, the first row or one a database admin deleted,
// at next.Gen, which must be one above the highest gen nats_hold_log holds (1 for an empty log),
// so a restored row's gen exceeds every earlier one; the insert and its log row are one statement.
// It restores released, with or without a hold id, or held with one, and judge decides from the
// log's latest entry whether the row may be restored after it (RestoreJudge). It refuses with a
// Conflict: ErrBadTransition for another phase, ErrRowExists while the row is there, ErrGenMismatch
// naming the gen to carry (RestoreGen), and ErrGenLogged when the log took that gen meanwhile; and
// with judge's refusals as they are.
func (s Store) Restore(ctx context.Context, next Row, judge RestoreJudge) (Row, error) {
	switch next.Phase {
	case string(natsgrant.OperatorPhaseReleased):
	case string(natsgrant.OperatorPhaseHeld):
		if next.HoldID == "" {
			return Row{}, &Conflict{Err: fmt.Errorf("%w: a held row names its hold", ErrBadTransition)}
		}
	default:
		return Row{}, &Conflict{Err: fmt.Errorf("%w: a restore writes released or held", ErrBadTransition)}
	}
	var restored Row
	err := s.in(ctx, pgx.ReadWrite, func(tx pgx.Tx) error {
		current, found, err := s.currentRow(ctx, tx, "")
		switch {
		case err != nil:
			return err
		case found:
			return &Conflict{Err: ErrRowExists, Current: &current}
		}
		log, err := s.logIn(ctx, tx)
		if err != nil {
			return err
		}
		var latest *LogEntry
		if len(log) > 0 {
			latest = &log[len(log)-1]
		}
		if err := judge.Trust(latest); err != nil {
			return err
		}
		want := uint64(1)
		if latest != nil {
			want = latest.Gen + 1
		}
		if want != next.Gen {
			return &Conflict{Err: ErrGenMismatch, RestoreGen: want}
		}
		journals, err := s.journalsIn(ctx, tx)
		if err != nil {
			return err
		}
		if err := judge.Admit(log, journals, next); err != nil {
			return err
		}
		restored, err = scanRow(tx.QueryRow(ctx, `with restored as (
				insert into `+s.table("nats_hold")+` (id, gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig)
				values (1, $1, $2, $3, $4, now(), $5, $6, $7)
				on conflict (id) do nothing
				returning `+rowColumns+`
			), logged as (
				insert into `+s.table("nats_hold_log")+` (gen, phase, hold_id, reason, written_at, written_by, signed_at, sig)
				select gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig from restored
			)
			select `+rowColumns+` from restored`,
			int64(next.Gen), next.Phase, next.HoldID, next.Reason, next.UpdatedBy, next.SignedAt, next.Sig))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return &Conflict{Err: ErrRowExists}
		case store.IsUniqueViolation(err):
			return &Conflict{Err: fmt.Errorf("%w: gen %d", ErrGenLogged, next.Gen)}
		}
		return err
	})
	return restored, err
}

// logIn reads every log entry inside tx, by gen, the latest last; empty for an empty log.
func (s Store) logIn(ctx context.Context, tx pgx.Tx) ([]LogEntry, error) {
	rows, err := tx.Query(ctx, `select gen, phase, hold_id, signed_at, sig from `+s.table("nats_hold_log")+` order by gen`)
	if err != nil {
		return nil, fmt.Errorf("natshold: read the log: %w", err)
	}
	log, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LogEntry, error) {
		var e LogEntry
		var gen int64
		if err := row.Scan(&gen, &e.Phase, &e.HoldID, &e.SignedAt, &e.Sig); err != nil {
			return LogEntry{}, err
		}
		if gen < 1 {
			return LogEntry{}, fmt.Errorf("natshold: a log entry is at gen %d", gen)
		}
		e.Gen = uint64(gen)
		return e, nil
	})
	if err != nil {
		return nil, fmt.Errorf("natshold: read the log: %w", err)
	}
	return log, nil
}

// heldBy reads the hold row inside tx under a share lock, so no move commits until tx does, and
// refuses with a Conflict carrying ErrHoldMismatch unless the row is there with holdID and, when
// phase is not empty, in phase.
func (s Store) heldBy(ctx context.Context, tx pgx.Tx, holdID, phase string) (Row, error) {
	current, found, err := s.currentRow(ctx, tx, "for share")
	switch {
	case err != nil:
		return Row{}, err
	case !found:
		return Row{}, &Conflict{Err: ErrHoldMismatch}
	case current.HoldID != holdID || (phase != "" && current.Phase != phase):
		return Row{}, &Conflict{Err: ErrHoldMismatch, Current: &current, HoldID: current.HoldID}
	}
	return current, nil
}

// The columns a record is read by, after its hold id and kind, in Record's order.
const recordColumns = `key, value::text, gen, written_at, sig`

func scanRecord(row pgx.Row, holdID, kind string) (Record, error) {
	out := Record{HoldID: holdID, Kind: kind}
	var value string
	var gen int64
	if err := row.Scan(&out.Key, &value, &gen, &out.WrittenAt, &out.Sig); err != nil {
		return Record{}, err
	}
	if gen < 1 {
		return Record{}, fmt.Errorf("natshold: a hold record's gen is %d", gen)
	}
	out.Value, out.Gen = json.RawMessage(value), uint64(gen)
	return out, nil
}

// PutRecord writes rec's value and signature at gen expectedGen+1 and answers the record it
// stored: expectedGen 0 inserts a record that is not there, and any other updates the record
// whose gen is expectedGen. It writes only while the hold row's hold id is rec.HoldID, whatever
// its phase, holding the row so no move commits between the check and the write. An insert of a
// key the routes deleted (nats_hold_record_deletes) is refused while deleted, the caller's check of
// that delete's signature, answers true, so a deleted cohort is not put back on any replica. It
// refuses with a Conflict: ErrHoldMismatch, and ErrGenMismatch for a record that exists at
// expectedGen 0, one whose delete stands, or one that does not hold expectedGen.
func (s Store) PutRecord(ctx context.Context, rec Record, expectedGen uint64, deleted func(Deletion) bool) (Record, error) {
	var stored Record
	err := s.in(ctx, pgx.ReadWrite, func(tx pgx.Tx) error {
		current, err := s.heldBy(ctx, tx, rec.HoldID, "")
		if err != nil {
			return err
		}
		if expectedGen == 0 {
			d := Deletion{HoldID: rec.HoldID, Kind: rec.Kind, Key: rec.Key}
			var gen int64
			err := tx.QueryRow(ctx, `select gen, deleted_at, sig from `+s.table("nats_hold_record_deletes")+` where hold_id = $1 and kind = $2 and key = $3`,
				rec.HoldID, rec.Kind, rec.Key).Scan(&gen, &d.DeletedAt, &d.Sig)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return fmt.Errorf("natshold: read the record's delete: %w", err)
			case gen >= 1:
				d.Gen = uint64(gen)
				if deleted(d) {
					return &Conflict{Err: fmt.Errorf("%w: the routes deleted the record at gen %d", ErrGenMismatch, d.Gen), Current: &current}
				}
			}
		}
		statement := `insert into ` + s.table("nats_hold_records") + ` (hold_id, kind, key, value, gen, written_at, sig)
			values ($1, $2, $3, $4::jsonb, 1, now(), $5)
			on conflict (hold_id, kind, key) do nothing
			returning ` + recordColumns
		args := []any{rec.HoldID, rec.Kind, rec.Key, string(rec.Value), rec.Sig}
		if expectedGen != 0 {
			statement = `update ` + s.table("nats_hold_records") + ` set value = $4::jsonb, gen = gen + 1, written_at = now(), sig = $5
				where hold_id = $1 and kind = $2 and key = $3 and gen = $6
				returning ` + recordColumns
			args = append(args, int64(expectedGen))
		}
		stored, err = scanRecord(tx.QueryRow(ctx, statement, args...), rec.HoldID, rec.Kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Conflict{Err: ErrGenMismatch, Current: &current}
		}
		return err
	})
	return stored, err
}

// Records reads every record of holdID's of kind, by key, and every Deletion of one, by key, in one
// snapshot (repeatable read), so a delete that commits while it reads is seen whole, its record
// gone and its deletion there, or not at all.
func (s Store) Records(ctx context.Context, holdID, kind string) ([]Record, []Deletion, error) {
	var records []Record
	var deletions []Deletion
	err := s.inTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select `+recordColumns+` from `+s.table("nats_hold_records")+` where hold_id = $1 and kind = $2 order by key`, holdID, kind)
		if err != nil {
			return fmt.Errorf("natshold: read the records: %w", err)
		}
		records, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Record, error) { return scanRecord(row, holdID, kind) })
		if err != nil {
			return fmt.Errorf("natshold: read the records: %w", err)
		}
		rows, err = tx.Query(ctx, `select key, gen, deleted_at, sig from `+s.table("nats_hold_record_deletes")+` where hold_id = $1 and kind = $2 order by key`, holdID, kind)
		if err != nil {
			return fmt.Errorf("natshold: read the record deletes: %w", err)
		}
		deletions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Deletion, error) {
			d := Deletion{HoldID: holdID, Kind: kind}
			var gen int64
			if err := row.Scan(&d.Key, &gen, &d.DeletedAt, &d.Sig); err != nil {
				return Deletion{}, err
			}
			if gen < 1 {
				return Deletion{}, fmt.Errorf("natshold: a hold record delete's gen is %d", gen)
			}
			d.Gen = uint64(gen)
			return d, nil
		})
		if err != nil {
			return fmt.Errorf("natshold: read the record deletes: %w", err)
		}
		return nil
	})
	return records, deletions, err
}

// DeleteCohort deletes holdID's cohort record of key at gen, for hold abort from prepare, which
// deletes it before it writes released, and for hold abort on a row lost in prepare and restored
// released. It deletes only while the hold row is prepare with holdID, or released with holdID
// when released, the caller's judge of that hold's done record (nil when there is none), read in
// the same transaction, answers nil; it answers ErrHoldMismatch otherwise, and released's refusal
// as it is. It holds the row so no move commits between the check and the delete, and deletes only
// the record whose gen is gen (ErrGenMismatch otherwise), the gen the route verified the
// operator's signature over, so an old signature cannot delete a newer record. The same statement
// keeps the delete in nats_hold_record_deletes with sig, that signature, so every broker replica
// reads the record as the operator's delete (Records). It deletes no record of another kind, so
// the journal outlives an abort.
func (s Store) DeleteCohort(ctx context.Context, holdID, key string, gen uint64, sig string, released func(journal *Record) error) error {
	return s.in(ctx, pgx.ReadWrite, func(tx pgx.Tx) error {
		current, err := s.heldBy(ctx, tx, holdID, "")
		if err != nil {
			return err
		}
		switch current.Phase {
		case string(natsgrant.OperatorPhasePrepare):
		case string(natsgrant.OperatorPhaseReleased):
			journal, err := s.journalIn(ctx, tx, holdID)
			if err != nil {
				return err
			}
			if err := released(journal); err != nil {
				return err
			}
		default:
			return &Conflict{Err: ErrHoldMismatch, Current: &current, HoldID: current.HoldID}
		}
		deleted, err := tx.Exec(ctx, `with deleted as (
				delete from `+s.table("nats_hold_records")+` where hold_id = $1 and kind = $2 and key = $3 and gen = $4
				returning hold_id, kind, key, gen
			)
			insert into `+s.table("nats_hold_record_deletes")+` (hold_id, kind, key, gen, deleted_at, sig)
			select hold_id, kind, key, gen, now(), $5 from deleted
			on conflict (hold_id, kind, key) do update set gen = excluded.gen, deleted_at = excluded.deleted_at, sig = excluded.sig`,
			holdID, KindCohort, key, int64(gen), sig)
		if err != nil {
			return fmt.Errorf("natshold: delete the cohort: %w", err)
		}
		if deleted.RowsAffected() == 0 {
			return &Conflict{Err: ErrGenMismatch, Current: &current}
		}
		return nil
	})
}

// The columns a cutover row is read by, in Cutover's order.
const cutoverColumns = `family, state, hold_id, hold_gen, written_at, written_by, sig`

func scanCutover(row pgx.Row) (Cutover, error) {
	var out Cutover
	var holdGen int64
	if err := row.Scan(&out.Family, &out.State, &out.HoldID, &holdGen, &out.WrittenAt, &out.WrittenBy, &out.Sig); err != nil {
		return Cutover{}, err
	}
	if holdGen < 0 {
		return Cutover{}, fmt.Errorf("natshold: a cutover row's hold gen is %d", holdGen)
	}
	out.HoldGen = uint64(holdGen)
	return out, nil
}

// PutCutover writes c's family's cutover row, its state, hold id, writer and signature, with
// hold_gen the hold row's current gen, and answers the row it stored. It writes only while the
// hold row is held with c.HoldID (ErrHoldMismatch otherwise), holding the row until it commits,
// and only once verify accepts the signature over that gen, whose error it answers as it is.
func (s Store) PutCutover(ctx context.Context, c Cutover, verify func(holdGen uint64) error) (Cutover, error) {
	var stored Cutover
	err := s.in(ctx, pgx.ReadWrite, func(tx pgx.Tx) error {
		current, err := s.heldBy(ctx, tx, c.HoldID, string(natsgrant.OperatorPhaseHeld))
		if err != nil {
			return err
		}
		if err := verify(current.Gen); err != nil {
			return err
		}
		stored, err = scanCutover(tx.QueryRow(ctx, `insert into `+s.table("nats_cutover")+` (family, state, hold_id, hold_gen, written_at, written_by, sig)
			values ($1, $2, $3, $4, now(), $5, $6)
			on conflict (family) do update set state = excluded.state, hold_id = excluded.hold_id, hold_gen = excluded.hold_gen,
				written_at = excluded.written_at, written_by = excluded.written_by, sig = excluded.sig
			returning `+cutoverColumns, c.Family, c.State, c.HoldID, int64(current.Gen), c.WrittenBy, c.Sig))
		return err
	})
	return stored, err
}

// Cutovers reads every family's cutover row, by family.
func (s Store) Cutovers(ctx context.Context) ([]Cutover, error) {
	var out []Cutover
	err := s.in(ctx, pgx.ReadOnly, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select `+cutoverColumns+` from `+s.table("nats_cutover")+` order by family`)
		if err != nil {
			return fmt.Errorf("natshold: read the cutover rows: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCutover(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}
