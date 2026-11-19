package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/pgsetting"
)

// BROKER_DATABASE_URL is read as every opener of a Postgres connection string reads it
// (pgsetting): a value whose user part pgx may read apart is refused by its length and cause, a
// value pgx cannot parse is too, and a ping that cannot connect names no user. The control is a
// value with a token as its whole user part on a port nothing listens on, whose ping fails naming
// the host. The user, the password and the token are made for this test.
func TestOpenNamesNoPartOfTheBrokerDatabaseURLsUserPart(t *testing.T) {
	const (
		user     = "made-up-user-8a4f"
		password = "not-a-secret-1c7e"
		token    = "not-a-secret-tok2d9"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for raw, why := range map[string]string{
		"postgres://" + user + ":" + password + "/x@127.0.0.1:1/broker":             string(pgsetting.AtAfterPath),
		"postgres://" + user + ":" + password + "@127.0.0.1:1/broker?sslmode=bogus": string(pgsetting.NotParsed),
	} {
		_, err := Open(ctx, raw)
		want := fmt.Sprintf("BROKER_DATABASE_URL, a value of %d characters, %s", len(raw), why)
		if err == nil || err.Error() != want {
			t.Errorf("%d characters: %v, want %q", len(raw), err, want)
		}
	}
	_, err := Open(ctx, "postgres://"+token+"@127.0.0.1:1/broker?connect_timeout=3")
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("a ping that cannot connect: %v, want it to name the host", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("a ping that cannot connect names the user: %q", err)
	}
}

// A BROKER_DATABASE_URL with an @ before its first / and another after it, in its database name or
// its query, whose host pgx reads may be a piece of a password that holds a raw @ and then a raw /
// (and a ? and an = for the query), is refused by Open by its length and the cause before any
// connect: pgx names that host in every error of a connect. The control is the same value with no @
// after its /, whose failed ping names the host, so the row would see a printed host. The user, the
// password and its piece are made for this test.
func TestOpenRefusesABrokerDatabaseURLWithAnAtInItsPasswordAndAnotherAfterItsSlash(t *testing.T) {
	const (
		user  = "made-up-user-3a1c"
		first = "not-a-secret-pw3a1c"
		piece = "not-a-secret-piece-3a1c"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	control := "postgres://" + user + ":" + first + "@" + piece + "/broker?connect_timeout=3"
	if _, err := Open(ctx, control); err == nil || !strings.Contains(err.Error(), piece) {
		t.Fatalf("UNKNOWN: the control's failed ping names no host, so this row saw nothing: %v", err)
	}
	for _, shape := range []struct{ value, cause string }{
		{"postgres://" + user + ":" + first + "@" + piece + "/x@127.0.0.1:1/broker?connect_timeout=3", "another in its database name"},
		{"postgres://" + user + ":" + first + "@" + piece + "/x?connect_timeout=3&a=b@127.0.0.1:1/broker", "another in its query"},
	} {
		_, err := Open(ctx, shape.value)
		named := fmt.Sprintf("BROKER_DATABASE_URL, a value of %d characters, has an @ before its first / and %s", len(shape.value), shape.cause)
		if err == nil || !strings.Contains(err.Error(), named) {
			t.Errorf("%v, want a failed ping naming %q", err, named)
			continue
		}
		for _, part := range []string{user, first, piece} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("the failed ping names a part of the password as the host or the user: %q", err)
			}
		}
	}
}
