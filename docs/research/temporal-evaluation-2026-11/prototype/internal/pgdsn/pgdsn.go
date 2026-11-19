// Package pgdsn is how the experiments read their -postgres-dsn (LEGION_TEST_PG_DSN when the flag
// is not given): a PostgreSQL URL, which can carry a user and a password. A refusal of it names it
// by its length and the cause its check found, never a part of it, and a failed connect names no
// user and no database.
//
// The repository's other Go programs read such a URL through the Envoy module's pgsetting, whose
// refusals have the same form. This module cannot import it: it holds no direct dependency beyond
// the libraries it evaluates (libx.go says why), and the Envoy module is one.
package pgdsn

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// The causes a refusal of the DSN names.
const (
	// Unparsed: net/url cannot read the value. Its error quotes the value whole, and a user part
	// that holds a /, ?, # or @ is the usual cause (its password then reads as a port).
	Unparsed = "does not parse as a URL; if its user part holds a /, ?, # or @, write each of them percent-encoded (/ as %2F, ? as %3F, # as %23, @ as %40)"
	// NotPostgres: the scheme net/url read is not postgres. A value with no "://" is read with the
	// text before its first colon as the scheme, which can be its user.
	NotPostgres = "is not a postgres:// or postgresql:// URL"
	// NoPort: the port does not parse. net/url reads the text after a colon as the port, which in
	// a user part that holds a / is a piece of the password.
	NoPort = "has a port that is not a number from 0 to 65535; if its user part holds a /, write it as %2F"
	// AtOutsideUserPart: an @ is left once net/url takes out the user part it read. net/url ends
	// the authority at the first /, so a password that holds a raw @ and then a raw /
	// (u:p@S/x@host/db) is read with S, a piece of it, as the host, which the experiments would then
	// connect to and name; an honest @ in the database name or the query reads the same.
	AtOutsideUserPart = "has an @ outside its user part (in its database name or query), so the host Go's URL parser reads may be a piece of a password that holds a raw @ and /; write each @ and / in the password, and an @ in the database name or query, percent-encoded (@ as %40, / as %2F)"
)

// Refusal is the refusal of dsn for why, named by its length alone.
func Refusal(dsn, why string) error {
	return fmt.Errorf("-postgres-dsn, a value of %d characters, %s", len(dsn), why)
}

// Parse is dsn as net/url reads it, refused when it is empty, when net/url cannot read it, when
// its scheme is not postgres, and when an @ is left outside the user part net/url read.
func Parse(dsn string) (*url.URL, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("-postgres-dsn is empty")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, Refusal(dsn, Unparsed)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, Refusal(dsn, NotPostgres)
	}
	withoutUser := *u
	withoutUser.User = nil
	if strings.Contains(withoutUser.String(), "@") {
		return nil, Refusal(dsn, AtOutsideUserPart)
	}
	return u, nil
}

// ConnectFailure is err without the words pgx puts in front of a failed connect, which name the
// DSN's user and database ("failed to connect to `user=… database=…`: "), and err itself
// otherwise. pgx's error then names what failed at each host it tried. It matches the words, not
// pgx's type, since this module does not import pgx itself.
func ConnectFailure(err error) error {
	if inner := errors.Unwrap(err); inner != nil && strings.HasPrefix(err.Error(), "failed to connect to `") {
		return fmt.Errorf("failed to connect: %w", inner)
	}
	return err
}
