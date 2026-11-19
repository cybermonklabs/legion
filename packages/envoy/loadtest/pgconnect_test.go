package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/pgsetting"
)

// A -pg value is read as every opener of a Postgres connection string reads it (pgsetting): one
// whose user part pgx may read apart is refused by its length and cause before any connect, by the
// connection and the pool alike, and a connect that fails names no user. The control is a token as
// the whole user part on a port nothing listens on, whose failure names the host. The user, the
// password and the token are made for this test.
func TestAPGValueIsReadWithNoPartOfItsUserPartPrinted(t *testing.T) {
	const (
		user     = "made-up-user-3f61"
		password = "not-a-secret-8c0b"
		token    = "not-a-secret-tok5e2"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	misread := "postgres://" + user + ":" + password + "@" + token + "@127.0.0.1:1/dispatch"
	want := fmt.Sprintf("-pg, a value of %d characters, %s", len(misread), pgsetting.AtsBeforePath)
	if _, err := connectPG(ctx, misread); err == nil || err.Error() != want {
		t.Errorf("connectPG: %v, want %q", err, want)
	}
	if _, err := newPGPool(ctx, misread); err == nil || err.Error() != want {
		t.Errorf("newPGPool: %v, want %q", err, want)
	}
	_, err := connectPG(ctx, "postgres://"+token+"@127.0.0.1:1/dispatch?connect_timeout=3")
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("a connect that fails: %v, want it to name the host", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("a connect that fails names the user: %q", err)
	}
}

// A -pg value with an @ before its first / and another after it, in its database name or its
// query, whose host pgx reads may be a piece of a password that holds a raw @ and then a raw / (and
// a ? and an = for the query), is refused by connectPG and by connectAs (the rooms probe) alike, by
// its length and the cause before any connect: pgx names that host in every error of a connect. The
// control is the same value with no @ after its /, whose failed connect names the host. The user,
// the password and its piece are made for this test.
func TestAPGValueWithAnAtInItsPasswordAndAnotherAfterItsSlashIsRefused(t *testing.T) {
	const (
		user  = "made-up-user-7c3d"
		first = "not-a-secret-pw7c3d"
		piece = "not-a-secret-piece-7c3d"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	control := "postgres://" + user + ":" + first + "@" + piece + "/dispatch?connect_timeout=3"
	shapes := []struct{ value, cause string }{
		{"postgres://" + user + ":" + first + "@" + piece + "/x@127.0.0.1:1/dispatch?connect_timeout=3", "another in its database name"},
		{"postgres://" + user + ":" + first + "@" + piece + "/x?connect_timeout=3&a=b@127.0.0.1:1/dispatch", "another in its query"},
	}
	for name, connect := range map[string]func(string) error{
		"connectPG": func(url string) error { _, err := connectPG(ctx, url); return err },
		"connectAs": func(url string) error { _, err := connectAs(ctx, url, "loadgen-row"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := connect(control); err == nil || !strings.Contains(err.Error(), piece) {
				t.Fatalf("UNKNOWN: the control's failed connect names no host, so this row saw nothing: %v", err)
			}
			for _, shape := range shapes {
				named := fmt.Sprintf("-pg, a value of %d characters, has an @ before its first / and %s", len(shape.value), shape.cause)
				err := connect(shape.value)
				if err == nil || !strings.Contains(err.Error(), named) {
					t.Errorf("%v, want a failed connect naming %q", err, named)
					continue
				}
				for _, part := range []string{user, first, piece} {
					if strings.Contains(err.Error(), part) {
						t.Errorf("the failed connect names a part of the password as the host or the user: %q", err)
					}
				}
			}
		})
	}
}
