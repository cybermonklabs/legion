package ompsessions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/pgsetting"
)

// The session database's URL is read as every opener of a Postgres connection string reads it
// (pgsetting): one whose user part pgx and net/url read apart is refused by its length and cause
// before any connect; one pgx cannot parse is refused by its length too, since pgx's parse error
// quotes the user; and a connect that fails names the address and no user (store.ConnectError).
// The control is the same URL with its @ percent-encoded, which reaches the connect and is refused
// naming the address. The user, the password and the token are made for this test.
func TestConnectURLRefusesAMisreadURLByItsLength(t *testing.T) {
	const (
		user     = "made-up-user-4d18"
		password = "not-a-secret-7f30"
		token    = "not-a-secret-tok2c6"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for raw, why := range map[string]any{
		"postgres://" + user + ":" + password + "@x@127.0.0.1:1/omp":               pgsetting.AtsBeforePath,
		"postgres://" + token + "@127.0.0.1:1/omp?connect_timeout=3&sslmode=bogus": pgsetting.NotParsed,
	} {
		want := fmt.Sprintf("the session database URL %s names, a value of %d characters, %s", DSNFileVariable, len(raw), why)
		if _, err := ConnectURL(ctx, raw); err == nil || err.Error() != want {
			t.Errorf("%d characters: %v, want %q", len(raw), err, want)
		}
	}
	encoded := "postgres://" + user + ":" + password + "%40x@127.0.0.1:1/omp?connect_timeout=3"
	_, err := ConnectURL(ctx, encoded)
	if err == nil || !strings.Contains(err.Error(), "connect to Postgres at 127.0.0.1:1") {
		t.Fatalf("the control: %v, want the connect's refusal naming the address", err)
	}
	_, err = ConnectURL(ctx, "postgres://"+token+"@127.0.0.1:1/omp?connect_timeout=3")
	if err == nil || !strings.Contains(err.Error(), "connect to Postgres at 127.0.0.1:1") || strings.Contains(err.Error(), token) {
		t.Errorf("a token as the user: %v, want the connect's refusal naming the address and not the token", err)
	}
	// A ? or # before the @, which pgx keeps in the user part and net/url reads otherwise, reaches
	// the connect, and its refusal names the address and no part of the user part.
	for _, userPart := range []string{user + ":" + password + "?w", user + ":" + password + "#w", token + "?w", token + "#w"} {
		dsn := "postgres://" + userPart + "@127.0.0.1:1/omp?connect_timeout=3"
		_, err := ConnectURL(ctx, dsn)
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

// A session database URL with an @ before its first / and another after it, in its database name or
// its query, whose host pgx reads may be a piece of a password that holds a raw @ and then a raw /
// (and a ? and an = for the query), is refused by ConnectURL by its length and the cause before any
// connect: pgx names that host in every error of a connect. The control is the same URL with no @
// after its /, whose refusal names the address. The user, the password and its piece are made for
// this test.
func TestConnectURLRefusesAURLWithAnAtInItsPasswordAndAnotherAfterItsSlash(t *testing.T) {
	const (
		user  = "made-up-user-5e2b"
		first = "not-a-secret-pw5e2b"
		piece = "not-a-secret-piece-5e2b"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	control := "postgres://" + user + ":" + first + "@" + piece + "/omp?connect_timeout=3"
	if _, err := ConnectURL(ctx, control); err == nil || !strings.Contains(err.Error(), "connect to Postgres at "+piece) {
		t.Fatalf("UNKNOWN: the control's refusal names no address, so this row saw nothing: %v", err)
	}
	for _, shape := range []struct{ value, cause string }{
		{"postgres://" + user + ":" + first + "@" + piece + "/x@127.0.0.1:1/omp?connect_timeout=3", "another in its database name"},
		{"postgres://" + user + ":" + first + "@" + piece + "/x?connect_timeout=3&a=b@127.0.0.1:1/omp", "another in its query"},
	} {
		_, err := ConnectURL(ctx, shape.value)
		named := fmt.Sprintf("the session database URL %s names, a value of %d characters, has an @ before its first / and %s", DSNFileVariable, len(shape.value), shape.cause)
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
