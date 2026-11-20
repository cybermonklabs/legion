package testdb

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Each fixture makes the fault it is named for, so a test that takes it can fail when the code
// under test meets that fault wrongly: a round trip through Delayed takes at least its delay,
// Breaking ends the connection that sends its marker and serves the next, Flapping answers a
// connection that it cannot take one now (57P03) while Down is set and reaches the database once
// it is not, counting each, and RefusedAddress is refused by the host for as long as the test runs,
// since no other listener can take its port.
func TestEachFixtureProxyMakesItsFault(t *testing.T) {
	dsn := Database(t, "legion_testdb_fixture_test")
	open := func(t *testing.T, url string) (*pgx.Conn, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, url)
		if err == nil {
			t.Cleanup(func() { _ = conn.Close(context.Background()) })
		}
		return conn, err
	}
	exec := func(conn *pgx.Conn, statement string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := conn.Exec(ctx, statement)
		return err
	}
	t.Run("Delayed holds each round trip for its delay", func(t *testing.T) {
		const delay = 200 * time.Millisecond
		conn, err := open(t, Delayed(t, dsn, delay))
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if err := exec(conn, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(started); took < delay {
			t.Fatalf("a statement through Delayed took %s, want at least its delay of %s", took, delay)
		}
	})
	t.Run("Breaking ends the connection that sends its marker, and serves the next", func(t *testing.T) {
		const marker = "the fixture ends this connection"
		through := Breaking(t, dsn, marker, false)
		conn, err := open(t, through)
		if err != nil {
			t.Fatal(err)
		}
		if err := exec(conn, "SELECT '"+marker+"'"); err == nil {
			t.Fatal("the connection that sent the marker was served")
		}
		next, err := open(t, through)
		if err != nil {
			t.Fatal(err)
		}
		if err := exec(next, "SELECT '"+marker+"'"); err != nil {
			t.Fatalf("the next connection was refused: %v", err)
		}
	})
	t.Run("Flapping cannot take a connection while down and reaches the database once up", func(t *testing.T) {
		store := NewFlapping(t, dsn, true)
		var answer *pgconn.PgError
		if _, err := open(t, store.URL); !errors.As(err, &answer) || answer.Code != "57P03" {
			t.Fatalf("a connection while the store is down answered %v, want 57P03", err)
		}
		store.Down.Store(false)
		conn, err := open(t, store.URL)
		if err != nil {
			t.Fatal(err)
		}
		if err := exec(conn, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
		if refused, passed := store.Refused.Load(), store.Passed.Load(); refused != 1 || passed != 1 {
			t.Fatalf("the store counted %d refused and %d passed, want 1 and 1", refused, passed)
		}
	})
	t.Run("RefusedAddress is refused, and no listener can take its port while the test runs", func(t *testing.T) {
		address := RefusedAddress(t)
		if _, err := net.Dial("tcp", address); !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("a dial of the refused address answered %v, want the host's refusal", err)
		}
		if listener, err := net.Listen("tcp", address); err == nil {
			_ = listener.Close()
			t.Fatal("a listener took the refused address's port")
		}
	})
}
