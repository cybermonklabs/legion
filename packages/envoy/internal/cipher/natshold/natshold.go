// Package natshold is the broker's store of the NATS writer hold: the hold row, its log, the
// hold's records and each family's cutover row (migration 0041_nats_hold). The auth service reads
// the row at every authorization request (Read), and the operator writes every table through the
// broker's /v1/nats-hold routes alone. Every row carries the operator's signature
// (internal/bus/signedrecord), which each reader verifies: this package stores and reads rows and
// verifies nothing itself.
package natshold

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReadDeadline bounds one Read: the connection's acquire and the whole transaction. The auth
// service reads the row inside a decision's 1.5 s budget, so a Postgres that does not answer costs
// a decision this much and no more.
const ReadDeadline = 500 * time.Millisecond

// Reading is the hold row as Read reads it: the columns its signature covers, and the signature.
type Reading struct {
	Gen      uint64
	Phase    string
	HoldID   string
	SignedAt time.Time
	Sig      string
}

// Read reads the hold row of schema's nats_hold, in a read-only transaction that first sets its
// search_path to pg_catalog and pg_temp, so no object another writer put in a schema of the pool's
// search path is resolved in its place, under ReadDeadline. It answers found false for no row, and
// an error for any failure, a deadline passed included: the caller reads every error as unknown,
// whatever it read before. schema is public in a deployment; a test names the schema of its own.
func Read(ctx context.Context, pool *pgxpool.Pool, schema string) (Reading, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, ReadDeadline)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Reading{}, false, fmt.Errorf("natshold: begin the hold read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `set local search_path = pg_catalog, pg_temp`); err != nil {
		return Reading{}, false, fmt.Errorf("natshold: set the hold read's search path: %w", err)
	}
	var reading Reading
	var gen int64
	err = tx.QueryRow(ctx, `select gen, phase, hold_id, signed_at, sig from `+pgx.Identifier{schema, "nats_hold"}.Sanitize()+` where id = 1`).
		Scan(&gen, &reading.Phase, &reading.HoldID, &reading.SignedAt, &reading.Sig)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Reading{}, false, nil
	case err != nil:
		return Reading{}, false, fmt.Errorf("natshold: read the hold row: %w", err)
	case gen < 0:
		return Reading{}, false, fmt.Errorf("natshold: the hold row's gen is %d", gen)
	}
	reading.Gen = uint64(gen)
	return reading, true, nil
}
