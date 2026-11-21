package natshold

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
)

// Conflict is a write a store refused for what the tables held: Err is which refusal, Current the
// hold row as the write read it (nil when there is none), and RestoreGen, for a restore's
// ErrGenMismatch, the gen a restore must carry now.
type Conflict struct {
	Err        error
	Current    *Row
	RestoreGen uint64
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
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: access})
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
// its nats_hold_log row. It refuses with a Conflict: ErrNoRow, ErrGenMismatch when the row's gen
// is not expectedGen, and moveRefusal's refusal.
func (s Store) Move(ctx context.Context, expectedGen uint64, next Row) (Row, error) {
	var moved Row
	err := s.in(ctx, pgx.ReadWrite, func(tx pgx.Tx) error {
		current, found, err := s.currentRow(ctx, tx, "for update")
		switch {
		case err != nil:
			return err
		case !found:
			return &Conflict{Err: ErrNoRow}
		case current.Gen != expectedGen:
			return &Conflict{Err: ErrGenMismatch, Current: &current}
		}
		used := false
		if current.Phase == string(natsgrant.OperatorPhaseReleased) && next.Phase == string(natsgrant.OperatorPhasePrepare) && next.HoldID != "" {
			if err := tx.QueryRow(ctx, `select exists (select 1 from `+s.table("nats_hold_log")+` where hold_id = $1)`, next.HoldID).Scan(&used); err != nil {
				return fmt.Errorf("natshold: read the log's hold ids: %w", err)
			}
		}
		if err := moveRefusal(current, next.Phase, next.HoldID, used); err != nil {
			return &Conflict{Err: err, Current: &current}
		}
		moved, err = scanRow(tx.QueryRow(ctx, `with moved as (
				update `+s.table("nats_hold")+` set gen = gen + 1, phase = $2, hold_id = $3, reason = $4, updated_at = now(),
					updated_by = $5, signed_at = $6, sig = $7
				where id = 1 and gen = $1
				returning `+rowColumns+`
			), logged as (
				insert into `+s.table("nats_hold_log")+` (gen, phase, hold_id, reason, written_at, written_by)
				select gen, phase, hold_id, reason, updated_at, updated_by from moved
			)
			select `+rowColumns+` from moved`,
			int64(expectedGen), next.Phase, next.HoldID, next.Reason, next.UpdatedBy, next.SignedAt, next.Sig))
		if errors.Is(err, pgx.ErrNoRows) {
			return &Conflict{Err: ErrGenMismatch, Current: &current}
		}
		return err
	})
	return moved, err
}

// Restore writes the hold row when it is absent, the first row or one a database admin deleted,
// at next.Gen, which must be one above the highest gen nats_hold_log holds (1 for an empty log),
// so a restored row's gen exceeds every earlier one; the insert and its log row are one
// statement. It restores released, or held with a hold id. It refuses with a Conflict:
// ErrBadTransition for another phase, ErrRowExists while the row is there, and ErrGenMismatch
// naming the gen to carry (RestoreGen).
func (s Store) Restore(ctx context.Context, next Row) (Row, error) {
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
		var err error
		restored, err = scanRow(tx.QueryRow(ctx, `with next as (
				select coalesce(max(gen), 0) + 1 as gen from `+s.table("nats_hold_log")+`
			), restored as (
				insert into `+s.table("nats_hold")+` (id, gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig)
				select 1, next.gen, $2, $3, $4, now(), $5, $6, $7 from next where next.gen = $1
				on conflict (id) do nothing
				returning `+rowColumns+`
			), logged as (
				insert into `+s.table("nats_hold_log")+` (gen, phase, hold_id, reason, written_at, written_by)
				select gen, phase, hold_id, reason, updated_at, updated_by from restored
			)
			select `+rowColumns+` from restored`,
			int64(next.Gen), next.Phase, next.HoldID, next.Reason, next.UpdatedBy, next.SignedAt, next.Sig))
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		current, found, err := s.currentRow(ctx, tx, "")
		switch {
		case err != nil:
			return err
		case found:
			return &Conflict{Err: ErrRowExists, Current: &current}
		}
		var want int64
		if err := tx.QueryRow(ctx, `select coalesce(max(gen), 0) + 1 from `+s.table("nats_hold_log")).Scan(&want); err != nil {
			return fmt.Errorf("natshold: read the log's highest gen: %w", err)
		}
		return &Conflict{Err: ErrGenMismatch, RestoreGen: uint64(want)}
	})
	return restored, err
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
		return Row{}, &Conflict{Err: ErrHoldMismatch, Current: &current}
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
// its phase, holding the row so no move commits between the check and the write. It refuses with
// a Conflict: ErrHoldMismatch, and ErrGenMismatch for a record that exists at expectedGen 0 or
// does not hold expectedGen.
func (s Store) PutRecord(ctx context.Context, rec Record, expectedGen uint64) (Record, error) {
	var stored Record
	err := s.in(ctx, pgx.ReadWrite, func(tx pgx.Tx) error {
		current, err := s.heldBy(ctx, tx, rec.HoldID, "")
		if err != nil {
			return err
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

// Records reads every record of holdID's of kind, by key.
func (s Store) Records(ctx context.Context, holdID, kind string) ([]Record, error) {
	var out []Record
	err := s.in(ctx, pgx.ReadOnly, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select `+recordColumns+` from `+s.table("nats_hold_records")+` where hold_id = $1 and kind = $2 order by key`, holdID, kind)
		if err != nil {
			return fmt.Errorf("natshold: read the records: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			rec, err := scanRecord(rows, holdID, kind)
			if err != nil {
				return err
			}
			out = append(out, rec)
		}
		return rows.Err()
	})
	return out, err
}

// DeleteCohort deletes holdID's cohort record of key at gen, for hold abort from prepare, its one
// caller, which deletes it before it writes released. It deletes only while the hold row is
// prepare with holdID (ErrHoldMismatch otherwise), holding the row so no move commits between the
// check and the delete, and only the record whose gen is gen (ErrGenMismatch otherwise), the gen
// the route verified the operator's signature over, so an old signature cannot delete a newer
// record. It deletes no record of another kind, so the journal outlives an abort.
func (s Store) DeleteCohort(ctx context.Context, holdID, key string, gen uint64) error {
	return s.in(ctx, pgx.ReadWrite, func(tx pgx.Tx) error {
		current, err := s.heldBy(ctx, tx, holdID, string(natsgrant.OperatorPhasePrepare))
		if err != nil {
			return err
		}
		deleted, err := tx.Exec(ctx, `delete from `+s.table("nats_hold_records")+` where hold_id = $1 and kind = $2 and key = $3 and gen = $4`,
			holdID, KindCohort, key, int64(gen))
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
