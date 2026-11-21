package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// IntakeConsumersTable is the Postgres table PGStore keeps Dispatch's and the Legion daemon's
// selection records in, each in its own database. The family's migration creates it as:
//
//	create table intake_consumers (
//	    logical  text primary key,
//	    record   jsonb not null,
//	    revision bigint not null check (revision > 0)
//	);
//
// One row per logical name, never deleted, whose record is a Record, and the sentinel row
// (PGSentinelKey), whose record is a StoreState.
const IntakeConsumersTable = "intake_consumers"

// PGSentinelKey is the logical name of a PGStore's sentinel row. It holds a '.', which no logical
// name holds (CheckKey).
const PGSentinelKey = "envoy.sentinel"

// undefinedTable is Postgres's SQLSTATE for a relation that does not exist.
const undefinedTable = "42P01"

// PGQuerier is what PGStore runs its statements on: a *pgxpool.Pool, a *pgx.Conn or a pgx.Tx.
type PGQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGStore is a Store over IntakeConsumersTable. A write at revision 0 inserts the row at revision
// 1 and is refused when the row exists; a write at a revision above 0 sets the next revision where
// the row still holds that one. Either refusal is ErrConflict.
type PGStore struct {
	db PGQuerier
}

// NewPGStore is a PGStore on db.
func NewPGStore(db PGQuerier) *PGStore { return &PGStore{db: db} }

// State implements Store: it reads the sentinel row. A row whose record is not the sentinel
// (decodeSentinel) is a missing sentinel.
func (s *PGStore) State(ctx context.Context) (StoreState, error) {
	data, revision, err := s.row(ctx, PGSentinelKey)
	if errors.Is(err, ErrNoRecord) {
		return StoreState{}, fmt.Errorf("%w: %s holds no %s row", ErrNoSentinel, IntakeConsumersTable, PGSentinelKey)
	}
	if err != nil {
		return StoreState{}, err
	}
	state, err := decodeSentinel(PGSentinelKey, data)
	if err != nil {
		return StoreState{}, fmt.Errorf("%w: %s's %s row at revision %d holds no sentinel (%w)", ErrNoSentinel, IntakeConsumersTable, PGSentinelKey, revision, err)
	}
	return state, nil
}

// Read implements Store. A row whose record is not logical's (decodeRecord) is ErrMalformedRecord.
func (s *PGStore) Read(ctx context.Context, logical string) (Record, uint64, error) {
	data, revision, err := s.row(ctx, logical)
	if err != nil {
		return Record{}, 0, err
	}
	record, err := decodeRecord(logical, data)
	if err != nil {
		return Record{}, 0, fmt.Errorf("consumer names: %s's row of %s at revision %d: %w", logical, IntakeConsumersTable, revision, err)
	}
	return record, revision, nil
}

// Write implements Store. It writes the record under its own key (Record.Key).
func (s *PGStore) Write(ctx context.Context, logical string, record Record, revision uint64) (uint64, error) {
	record.Key = logical
	data, err := json.Marshal(record)
	if err != nil {
		return 0, err
	}
	return s.put(ctx, logical, data, revision)
}

// WriteSentinel writes the sentinel row, under its own key (StoreState.Key), if its revision is
// still revision (0: never written).
func (s *PGStore) WriteSentinel(ctx context.Context, state StoreState, revision uint64) (uint64, error) {
	state.Key = PGSentinelKey
	data, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	return s.put(ctx, PGSentinelKey, data, revision)
}

// PGRows is what List runs its query on: a *pgxpool.Pool, a *pgx.Conn or a pgx.Tx.
type PGRows interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// List reads every record of IntakeConsumersTable but the sentinel, by logical name: what an
// operator's status lists, retired names included. It needs a db that is also a PGRows.
func (s *PGStore) List(ctx context.Context) (map[string]Record, error) {
	db, ok := s.db.(PGRows)
	if !ok {
		return nil, fmt.Errorf("consumer names: %T cannot list %s", s.db, IntakeConsumersTable)
	}
	rows, err := db.Query(ctx, `select logical, record from `+IntakeConsumersTable+` where logical <> $1 order by logical`, PGSentinelKey)
	if err != nil {
		return nil, s.failed("list", "every logical name", err)
	}
	defer rows.Close()
	records := map[string]Record{}
	for rows.Next() {
		var logical string
		var data []byte
		if err := rows.Scan(&logical, &data); err != nil {
			return nil, s.failed("list", "every logical name", err)
		}
		record, err := decodeRecord(logical, data)
		if err != nil {
			return nil, fmt.Errorf("consumer names: %s's row of %s: %w", logical, IntakeConsumersTable, err)
		}
		records[logical] = record
	}
	if err := rows.Err(); err != nil {
		return nil, s.failed("list", "every logical name", err)
	}
	return records, nil
}

func (s *PGStore) row(ctx context.Context, logical string) ([]byte, uint64, error) {
	var data []byte
	var revision int64
	err := s.db.QueryRow(ctx, `select record, revision from `+IntakeConsumersTable+` where logical = $1`, logical).Scan(&data, &revision)
	if err != nil {
		return nil, 0, s.failed("read", logical, err)
	}
	return data, uint64(revision), nil
}

func (s *PGStore) put(ctx context.Context, logical string, data []byte, revision uint64) (uint64, error) {
	var next int64
	var err error
	if revision == 0 {
		err = s.db.QueryRow(ctx, `insert into `+IntakeConsumersTable+` (logical, record, revision) values ($1, $2, 1)
			on conflict (logical) do nothing returning revision`, logical, data).Scan(&next)
	} else {
		err = s.db.QueryRow(ctx, `update `+IntakeConsumersTable+` set record = $2, revision = revision + 1
			where logical = $1 and revision = $3 returning revision`, logical, data, int64(revision)).Scan(&next)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s's row of %s is not at revision %d", ErrConflict, logical, IntakeConsumersTable, revision)
	}
	if err != nil {
		return 0, s.failed("write", logical, err)
	}
	return uint64(next), nil
}

func (s *PGStore) failed(doing, logical string, err error) error {
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNoRecord
	case errors.As(err, &pg) && pg.Code == undefinedTable:
		return fmt.Errorf("%w: %s (%w)", ErrNoStore, IntakeConsumersTable, err)
	}
	return fmt.Errorf("consumer names: %s %s's row of %s: %w", doing, logical, IntakeConsumersTable, err)
}
