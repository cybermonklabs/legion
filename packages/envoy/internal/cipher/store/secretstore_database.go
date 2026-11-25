package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// SecretStoreDatabase is the store's pool as the secret stores read and write it
// (secretstore.OpenEnv.Database): each InTx one transaction of the pool, over plain Go values, so
// no store package names a pgx type and the agent-secrets CLI, which links every store, links no
// driver. cmd/cipherd hands it to the store it opens.
func (s *Store) SecretStoreDatabase() secretstore.Database { return poolDatabase{s} }

// poolDatabase is secretstore.Database over a Store's pool.
type poolDatabase struct{ store *Store }

// InTx runs fn in one transaction of the pool, committed when fn returns nil.
func (d poolDatabase) InTx(ctx context.Context, fn func(secretstore.Tx) error) error {
	return pgx.BeginFunc(ctx, d.store.Pool, func(tx pgx.Tx) error { return fn(poolTx{tx}) })
}

// poolTx is secretstore.Tx over one pgx transaction.
type poolTx struct{ tx pgx.Tx }

// Exec runs the statement, and answers its error alone: no store reads the command tag.
func (t poolTx) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := t.tx.Exec(ctx, sql, args...)
	return err
}

// QueryRow runs the query; pgx's Row is a secretstore.Row.
func (t poolTx) QueryRow(ctx context.Context, sql string, args ...any) secretstore.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}
