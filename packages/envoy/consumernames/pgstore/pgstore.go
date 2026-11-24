// Package pgstore is the Postgres store of selection records (consumernames.Store) of one family,
// Dispatch's or a Legion daemon's, over consumernames.IntakeConsumersTable. It is a package of its
// own so that a binary that keeps only the session family's KV store (consumernames.KVStore), the
// listener among them, links no Postgres driver. It encodes and decodes every record and sentinel
// as consumernames does for every store (EncodeRecord, DecodeRecord, EncodeSentinel,
// DecodeSentinel), and keys its rows by consumernames.PGKey and consumernames.PGSentinelKey.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/consumernames"
)

// table is the table the store reads and writes.
const table = consumernames.IntakeConsumersTable

// undefinedTable is Postgres's SQLSTATE for a relation that does not exist.
const undefinedTable = "42P01"

// Querier is what a Store runs its statements on: a *pgxpool.Pool, a *pgx.Conn or a pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the consumernames.Store of one family over consumernames.IntakeConsumersTable, which it
// reads and writes only under its own family's keys (consumernames.PGKey,
// consumernames.PGSentinelKey). A write at revision 0 inserts the row at revision 1 and is refused
// when the row exists; a write at a revision above 0 sets the next revision where the row still
// holds that one. Either refusal is consumernames.ErrConflict.
type Store struct {
	db Querier
	// family is the family the store reads and writes (New).
	family string
}

// New is the store of family's selection records on db: consumernames.DispatchFamily for
// Dispatch's, consumernames.DaemonFamily(project) for a daemon's. It reads and writes only
// family's sentinel and records, and its sentinel must name family (StoreState.Family). A family
// no store keeps is refused, as consumernames.ErrInvalidFamily naming it
// (consumernames.CheckStoreFamily), since its keys could reach another family's rows.
func New(family string, db Querier) (*Store, error) {
	if err := consumernames.CheckStoreFamily(family); err != nil {
		return nil, err
	}
	return &Store{db: db, family: family}, nil
}

// State implements consumernames.Store: it reads the family's sentinel row. No row is a missing
// sentinel (ErrNoSentinel); a row whose record is not the family's sentinel (DecodeSentinel) is
// ErrMalformedSentinel, never a missing one.
func (s *Store) State(ctx context.Context) (consumernames.StoreState, error) {
	key := consumernames.PGSentinelKey(s.family)
	data, revision, err := s.row(ctx, key)
	if errors.Is(err, consumernames.ErrNoRecord) {
		return consumernames.StoreState{}, fmt.Errorf("%w: %s holds no %s row", consumernames.ErrNoSentinel, table, key)
	}
	if err != nil {
		return consumernames.StoreState{}, err
	}
	state, err := consumernames.DecodeSentinel(key, s.family, data)
	if err != nil {
		return consumernames.StoreState{}, fmt.Errorf("consumer names: %s's %s row at revision %d: %w", table, key, revision, err)
	}
	return state, nil
}

// Read implements consumernames.Store. A row whose record is not the family's record of logical,
// carrying the row's own key (DecodeRecord), is ErrMalformedRecord: a record copied there from
// another family's row carries that family's key.
func (s *Store) Read(ctx context.Context, logical string) (consumernames.Record, uint64, error) {
	key := consumernames.PGKey(s.family, logical)
	data, revision, err := s.row(ctx, key)
	if err != nil {
		return consumernames.Record{}, 0, err
	}
	record, err := consumernames.DecodeRecord(key, data)
	if err != nil {
		return consumernames.Record{}, 0, fmt.Errorf("consumer names: %s's row of %s at revision %d: %w", key, table, revision, err)
	}
	return record, revision, nil
}

// Write implements consumernames.Store. It writes the record under the family's key of logical,
// which the record carries as its own (Record.Key). A record every read would refuse
// (EncodeRecord) is refused as ErrMalformedRecord, and nothing is written.
func (s *Store) Write(ctx context.Context, logical string, record consumernames.Record, revision uint64) (uint64, error) {
	record.Key = consumernames.PGKey(s.family, logical)
	data, err := consumernames.EncodeRecord(record)
	if err != nil {
		return 0, err
	}
	return s.put(ctx, record.Key, data, revision)
}

// WriteSentinel writes the family's sentinel row, under its own key (StoreState.Key) and naming the
// store's family, which it sets when state names none (EncodeSentinel), if its revision is still
// revision (0: never written). A state naming another family is refused as ErrSentinelFamily, and
// nothing is written.
func (s *Store) WriteSentinel(ctx context.Context, state consumernames.StoreState, revision uint64) (uint64, error) {
	key := consumernames.PGSentinelKey(s.family)
	data, err := consumernames.EncodeSentinel(state, key, s.family)
	if err != nil {
		return 0, fmt.Errorf("consumer names: %s's %s row: %w", table, key, err)
	}
	return s.put(ctx, key, data, revision)
}

// Rows is what List runs its query on: a *pgxpool.Pool, a *pgx.Conn or a pgx.Tx.
type Rows interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// List reads every record of the family, by logical name: what an operator's status lists, retired
// names included. It reads no sentinel and no other family's record. It needs a db that is also a
// Rows.
func (s *Store) List(ctx context.Context) (map[string]consumernames.Record, error) {
	db, ok := s.db.(Rows)
	if !ok {
		return nil, fmt.Errorf("consumer names: %T cannot list %s", s.db, table)
	}
	prefix := consumernames.PGKey(s.family, "")
	every := "every logical name of " + s.family
	// left() rather than like: a family's project may hold '_', which like reads as a wildcard.
	rows, err := db.Query(ctx, `select logical, record from `+table+` where left(logical, length($1)) = $1 order by logical`, prefix)
	if err != nil {
		return nil, s.failed("list", every, err)
	}
	defer rows.Close()
	records := map[string]consumernames.Record{}
	for rows.Next() {
		var key string
		var data []byte
		if err := rows.Scan(&key, &data); err != nil {
			return nil, s.failed("list", every, err)
		}
		record, err := consumernames.DecodeRecord(key, data)
		if err != nil {
			return nil, fmt.Errorf("consumer names: %s's row of %s: %w", key, table, err)
		}
		records[strings.TrimPrefix(key, prefix)] = record
	}
	if err := rows.Err(); err != nil {
		return nil, s.failed("list", every, err)
	}
	return records, nil
}

// row reads the row of key.
func (s *Store) row(ctx context.Context, key string) ([]byte, uint64, error) {
	var data []byte
	var revision int64
	err := s.db.QueryRow(ctx, `select record, revision from `+table+` where logical = $1`, key).Scan(&data, &revision)
	if err != nil {
		return nil, 0, s.failed("read", key, err)
	}
	return data, uint64(revision), nil
}

// put writes data as the row of key if the row is still at revision (0: never written).
func (s *Store) put(ctx context.Context, key string, data []byte, revision uint64) (uint64, error) {
	var next int64
	var err error
	if revision == 0 {
		err = s.db.QueryRow(ctx, `insert into `+table+` (logical, record, revision) values ($1, $2, 1)
			on conflict (logical) do nothing returning revision`, key, data).Scan(&next)
	} else {
		err = s.db.QueryRow(ctx, `update `+table+` set record = $2, revision = revision + 1
			where logical = $1 and revision = $3 returning revision`, key, data, int64(revision)).Scan(&next)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s's row of %s is not at revision %d", consumernames.ErrConflict, key, table, revision)
	}
	if err != nil {
		return 0, s.failed("write", key, err)
	}
	return uint64(next), nil
}

func (s *Store) failed(doing, key string, err error) error {
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return consumernames.ErrNoRecord
	case errors.As(err, &pg) && pg.Code == undefinedTable:
		return fmt.Errorf("%w: %s (%w)", consumernames.ErrNoStore, table, err)
	}
	return fmt.Errorf("consumer names: %s %s's row of %s: %w", doing, key, table, err)
}
