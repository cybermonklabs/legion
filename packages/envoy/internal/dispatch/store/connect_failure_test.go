package store

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/pgsetting"
)

// A statement of the store can be the one that opens a connection: pgxpool dials as its callers
// need connections, long after Open's ping, and answers a dial that failed with pgx's own connect
// error, which names DATABASE_URL's user, where a token can stand. So every path of Pool that takes
// a connection names such a failure without the user (pgsetting.ConnectFailure), and errors.As
// still finds the ConnectError under it. The control is the pgx pool under it, asked the same,
// whose error names the token. The token and the password are made at each run, so no
// credential is written in this file.
func TestEveryPathThatTakesAConnectionNamesAFailedConnectWithoutTheUser(t *testing.T) {
	token, password := "made-up-token-"+rand.Text(), "made-up-"+rand.Text()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dsn := (&url.URL{Scheme: "postgres", User: url.UserPassword(token, password), Host: "127.0.0.1:1", Path: "/x", RawQuery: "sslmode=disable&connect_timeout=3"}).String()
	config, err := pgsetting.ParsePoolConfig("DATABASE_URL", dsn)
	if err != nil {
		t.Fatalf("ParsePoolConfig: %v", err)
	}
	raw, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	pool := NewPool(raw)
	defer pool.Close()
	if _, err := raw.Exec(ctx, "select 1"); err == nil || !strings.Contains(err.Error(), token) {
		t.Fatalf("UNKNOWN: the pgx pool's own failed connect does not name the user, so this test saw nothing: %v", err)
	}
	for _, path := range []struct {
		name string
		take func() error
	}{
		{"Query", func() error { _, err := pool.Query(ctx, "select 1"); return err }},
		{"QueryRow", func() error { var one int; return pool.QueryRow(ctx, "select 1").Scan(&one) }},
		{"Exec", func() error { _, err := pool.Exec(ctx, "select 1"); return err }},
		{"Acquire", func() error { _, err := pool.Acquire(ctx); return err }},
		{"AcquireFunc", func() error { return pool.AcquireFunc(ctx, func(*pgxpool.Conn) error { return nil }) }},
		{"CopyFrom", func() error {
			_, err := pool.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"a"}, pgx.CopyFromRows([][]any{{1}}))
			return err
		}},
		{"SendBatch, then Exec", func() error {
			batch := &pgx.Batch{}
			batch.Queue("select 1")
			results := pool.SendBatch(ctx, batch)
			defer results.Close()
			_, err := results.Exec()
			return err
		}},
		{"SendBatch, then Close", func() error {
			batch := &pgx.Batch{}
			QueueExec(batch, "the statement", "select 1")
			return pool.SendBatch(ctx, batch).Close()
		}},
		{"Begin", func() error { _, err := pool.Begin(ctx); return err }},
		{"BeginTx", func() error { _, err := pool.BeginTx(ctx, pgx.TxOptions{}); return err }},
		{"HealthyWithin", func() error { _, err := pool.HealthyWithin(ctx, 10*time.Second); return err }},
		{"Listener", func() error { _, err := pool.Listener(ctx); return err }},
		{"Dedicated", func() error { _, err := pool.Dedicated(ctx, "made-up-name"); return err }},
	} {
		t.Run(path.name, func(t *testing.T) {
			err := path.take()
			var connect *pgconn.ConnectError
			if !errors.As(err, &connect) {
				t.Fatalf("%v, want a failed connect", err)
			}
			for _, part := range []string{token, password} {
				if strings.Contains(err.Error(), part) {
					t.Errorf("the failed connect holds a part of the user part: %q", err)
				}
			}
			if !strings.Contains(err.Error(), "127.0.0.1:1") {
				t.Errorf("%q, want what failed at 127.0.0.1:1", err)
			}
		})
	}
}

// The separate pools Pool hands out - the document rooms', their appends', the compaction's, the
// event horizon's and the loop leases' - dial as their callers need connections too, from the
// shared pool's configuration, so every method of each that takes a connection names a failed
// connect without the user (SeparatePool), and errors.As still finds the ConnectError under it.
// The control is a pgx pool of the same configuration, asked the same, whose error names the
// token. The token and the password are made at each run, so no credential is written in this
// file.
func TestEverySeparatePoolNamesAFailedConnectWithoutTheUser(t *testing.T) {
	token, password := "made-up-token-"+rand.Text(), "made-up-"+rand.Text()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dsn := (&url.URL{Scheme: "postgres", User: url.UserPassword(token, password), Host: "127.0.0.1:1", Path: "/x", RawQuery: "sslmode=disable&connect_timeout=3"}).String()
	config, err := pgsetting.ParsePoolConfig("DATABASE_URL", dsn)
	if err != nil {
		t.Fatalf("ParsePoolConfig: %v", err)
	}
	raw, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	pool := NewPool(raw)
	defer pool.Close()
	control, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer control.Close()
	if _, err := control.Exec(ctx, "select 1"); err == nil || !strings.Contains(err.Error(), token) {
		t.Fatalf("UNKNOWN: a pgx pool of the shared pool's configuration does not name the user when its connect fails, so this test saw nothing: %v", err)
	}
	// Each pool is read through the methods that take a connection, so the row reads a pool of
	// any type that has them.
	type connectionTaker interface {
		Acquire(context.Context) (*pgxpool.Conn, error)
		AcquireFunc(context.Context, func(*pgxpool.Conn) error) error
		Begin(context.Context) (pgx.Tx, error)
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
		Query(context.Context, string, ...any) (pgx.Rows, error)
		QueryRow(context.Context, string, ...any) pgx.Row
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
		SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
		CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error)
		Ping(context.Context) error
	}
	for _, separate := range []struct {
		name string
		open func() (connectionTaker, error)
	}{
		{"Rooms", func() (connectionTaker, error) { return pool.Rooms() }},
		{"RoomAppends", func() (connectionTaker, error) { return pool.RoomAppends() }},
		{"Compaction", func() (connectionTaker, error) { return pool.Compaction() }},
		{"Horizon", func() (connectionTaker, error) { return pool.Horizon() }},
		{"LeaseCallers.For", func() (connectionTaker, error) {
			callers, err := pool.Leases()
			if err != nil {
				return nil, err
			}
			return callers.For("made-up caller")
		}},
	} {
		opened, err := separate.open()
		if err != nil {
			t.Fatalf("open the %s pool: %v", separate.name, err)
		}
		for _, path := range []struct {
			name string
			take func() error
		}{
			{"Acquire", func() error { _, err := opened.Acquire(ctx); return err }},
			{"AcquireFunc", func() error { return opened.AcquireFunc(ctx, func(*pgxpool.Conn) error { return nil }) }},
			{"Begin", func() error { _, err := opened.Begin(ctx); return err }},
			{"BeginTx", func() error { _, err := opened.BeginTx(ctx, pgx.TxOptions{}); return err }},
			{"Query", func() error { _, err := opened.Query(ctx, "select 1"); return err }},
			{"QueryRow", func() error { var one int; return opened.QueryRow(ctx, "select 1").Scan(&one) }},
			{"Exec", func() error { _, err := opened.Exec(ctx, "select 1"); return err }},
			{"SendBatch", func() error {
				batch := &pgx.Batch{}
				QueueExec(batch, "the statement", "select 1")
				return opened.SendBatch(ctx, batch).Close()
			}},
			{"CopyFrom", func() error {
				_, err := opened.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"a"}, pgx.CopyFromRows([][]any{{1}}))
				return err
			}},
			{"Ping", func() error { return opened.Ping(ctx) }},
		} {
			t.Run(separate.name+" "+path.name, func(t *testing.T) {
				err := path.take()
				var connect *pgconn.ConnectError
				if !errors.As(err, &connect) {
					t.Fatalf("%v, want a failed connect", err)
				}
				for _, part := range []string{token, password} {
					if strings.Contains(err.Error(), part) {
						t.Errorf("the failed connect holds a part of the user part: %q", err)
					}
				}
				if !strings.Contains(err.Error(), "127.0.0.1:1") {
					t.Errorf("%q, want what failed at 127.0.0.1:1", err)
				}
			})
		}
	}
}
