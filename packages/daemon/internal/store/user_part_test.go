package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/pgsetting"
)

// LEGION_POSTGRES_DSN is read as every opener of a Postgres connection string reads it
// (pgsetting): a DSN whose user part pgx and net/url read apart is refused by its length and cause
// before any connect, since the address ConnectError names would be a piece of that user part; one
// pgx cannot parse is refused by its length too, since pgx's parse error quotes the user; and a
// connect that fails names the address and no user, which can be a token written where the user
// stands. The control is the same DSN with its / percent-encoded, which reaches the connect and is
// refused naming the address. The user, the password and the token are made for this test.
func TestOpenRefusesAMisreadDSNByItsLength(t *testing.T) {
	const (
		user     = "made-up-user-0e93"
		password = "not-a-secret-5a7c"
		token    = "not-a-secret-tok1b4"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for raw, why := range map[string]any{
		"postgres://" + user + ":" + password + "/x@127.0.0.1:1/legion":               pgsetting.AtAfterPath,
		"postgres://" + token + "@127.0.0.1:1/legion?connect_timeout=3&sslmode=bogus": pgsetting.NotParsed,
	} {
		want := fmt.Sprintf("LEGION_POSTGRES_DSN, a value of %d characters, %s", len(raw), why)
		if _, err := Open(ctx, raw); err == nil || err.Error() != want {
			t.Errorf("%d characters: %v, want %q", len(raw), err, want)
		}
	}
	encoded := "postgres://" + user + ":" + password + "%2Fx@127.0.0.1:1/legion?connect_timeout=3"
	_, err := Open(ctx, encoded)
	if err == nil || !strings.Contains(err.Error(), "connect to Postgres at 127.0.0.1:1") {
		t.Fatalf("the control: %v, want the connect's refusal naming the address", err)
	}
	_, err = Open(ctx, "postgres://"+token+"@127.0.0.1:1/legion?connect_timeout=3")
	if err == nil || !strings.Contains(err.Error(), "connect to Postgres at 127.0.0.1:1") || strings.Contains(err.Error(), token) {
		t.Errorf("a token as the user: %v, want the connect's refusal naming the address and not the token", err)
	}
	// A ? or # before the @, which pgx keeps in the user part and net/url reads otherwise, reaches
	// the connect, and its refusal names the address and no part of the user part.
	for _, userPart := range []string{user + ":" + password + "?w", user + ":" + password + "#w", token + "?w", token + "#w"} {
		dsn := "postgres://" + userPart + "@127.0.0.1:1/legion?connect_timeout=3"
		_, err := Open(ctx, dsn)
		if err == nil || !strings.Contains(err.Error(), "connect to Postgres at 127.0.0.1:1") {
			t.Errorf("%d characters: %v, want the connect's refusal naming the address", len(dsn), err)
			continue
		}
		for _, part := range []string{user, password, token} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("%d characters: the refusal holds a part of the user part: %q", len(dsn), err)
			}
		}
	}
}

// A DSN with an @ before its first / and another after it, in its database name or its query,
// whose host pgx reads may be a piece of a password that holds a raw @ and then a raw / (and a ?
// and an = for the query), is refused by Open by its length and the cause before any connect: pgx
// names that host in every error of a connect, and ConnectError names it as the address. The
// control is the same DSN with no @ after its /, whose refusal names the address. The user, the
// password and its piece are made for this test.
func TestOpenRefusesADSNWithAnAtInItsPasswordAndAnotherAfterItsSlash(t *testing.T) {
	const (
		user  = "made-up-user-9d4e"
		first = "not-a-secret-pw9d4e"
		piece = "not-a-secret-piece-9d4e"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	control := "postgres://" + user + ":" + first + "@" + piece + "/legion?connect_timeout=3"
	if _, err := Open(ctx, control); err == nil || !strings.Contains(err.Error(), "connect to Postgres at "+piece) {
		t.Fatalf("UNKNOWN: the control's refusal names no address, so this row saw nothing: %v", err)
	}
	for _, shape := range []struct{ value, cause string }{
		{"postgres://" + user + ":" + first + "@" + piece + "/x@127.0.0.1:1/legion?connect_timeout=3", "another in its database name"},
		{"postgres://" + user + ":" + first + "@" + piece + "/x?connect_timeout=3&a=b@127.0.0.1:1/legion", "another in its query"},
	} {
		_, err := Open(ctx, shape.value)
		named := fmt.Sprintf("LEGION_POSTGRES_DSN, a value of %d characters, has an @ before its first / and %s", len(shape.value), shape.cause)
		if err == nil || !strings.Contains(err.Error(), named) {
			t.Errorf("%v, want a refusal naming %q", err, named)
			continue
		}
		for _, part := range []string{user, first, piece} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("the refusal names a part of the password as the address or the user: %q", err)
			}
		}
	}
}
