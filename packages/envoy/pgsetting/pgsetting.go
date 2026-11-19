// Package pgsetting is how a program reads a Postgres connection string a person configured (a
// DATABASE_URL, the broker's BROKER_DATABASE_URL, the daemon's DSN, a tool's -pg) before it opens
// the database the string names. Such a string may carry a user and a password, and pgx prints
// parts of it: its parse error masks the password and quotes the rest, the user among it, its
// connect error names the user and the database in front of what failed, and a server that refuses
// the sign-in names the user in its own answer. And a user part written with a raw @ or / is read
// by pgx in part as the host or the database, and a URL pgx does not take for one is read as
// keyword=value text. So each opener reads its string here: one whose user part pgx reads apart, or
// a URL pgx would not read as one, is refused (Refuse), one pgx cannot parse is refused too, each
// named by its length and the cause the check found (redact.Refusal), and a failed connect names no
// user and no database, and a server's answer by its SQLSTATE alone (ConnectFailure). A reader that
// also parses the string with net/url refuses what net/url reads apart first (URLParserReadsApart).
package pgsetting

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/redact"
)

// The causes a refusal of a Postgres connection string names, in redact.Refusal's words.
const (
	// SchemeNotRead: a value that holds :// is a URL, but pgx reads it as one only when it starts
	// with postgres:// or postgresql:// as written, in lower case with nothing before it, and reads
	// any other value as keyword=value text. A URL whose scheme is not in lower case, or with white
	// space before it, is then one keyword whose name is the whole URL up to its first =, the
	// password among it, which pgx sends to the server as the name of a runtime parameter while it
	// dials the local socket; and a URL with white space after it ends its last part with that
	// space. Written in lower case with no space around it, the same URL reads as one.
	SchemeNotRead redact.Misreading = "holds :// but does not start with postgres:// or postgresql:// in lower case with no space around it, so pgx reads it as keyword=value text and sends the URL up to its first =, the password among it, as the name of a setting; write the scheme in lower case, with no space before or after the value"
	// AtAfterPath: an @ after the first / and none before it. pgx and net/url both end the
	// authority at that /, so a user part that holds a / is read as a host, a port and a database
	// name, which pgx's connect error prints.
	AtAfterPath redact.Misreading = "has an @ after its first / and none before it, where a user part that holds a / is read as its host and database; write each / in its user part as %2F, and an @ in its database name or query as %40"
	// AtsBeforePath: more than one @ before the first /. pgx ends the user part at the first,
	// net/url at the last, so one of them reads a piece of the password as the host.
	AtsBeforePath redact.Misreading = "has more than one @ before its first /, and pgx ends its user part at the first while Go's URL parser ends it at the last; write each @ in its user part, and one in its query, as %40"
	// AtInDatabaseName: an @ before the first / and another in the database name, after the first
	// / and before any ?. A password that holds a raw @ and then a raw / (u:p@S/x@host/db) gives
	// pgx the host S, a piece of the password, and an honest host with an @ in its database name
	// (u:p@host/dis@patch) reads the same: no rule of the string tells the two apart, and pgx names
	// the host it read in every error of a connect, a later query's included, so no line could be
	// kept from printing the piece. The honest form can always be written with that @ as %40.
	AtInDatabaseName redact.Misreading = "has an @ before its first / and another in its database name, so the host pgx reads may be a piece of a password that holds a raw @ and /; write the @ in its password, or in its database name, as %40 (and a / in its password as %2F)"
	// AtInQuery: an @ before the first / and another in the query, after a ?. It reads two ways for
	// the same reason as AtInDatabaseName: a password that holds a raw @, /, ? and = in that order
	// (u:p@S/x?a=b@host/db) gives pgx the host S, a piece of the password, the database x and an
	// option a, and an honest query value with an @ (u:p@host/db?application_name=dispatch@box)
	// reads the same. pgx decodes %40 in a query value, so the honest form can always be written so.
	AtInQuery redact.Misreading = "has an @ before its first / and another in its query, so the host pgx reads may be a piece of a password that holds a raw @, / and ?; write the @ in its query as %40 (application_name=dispatch@box as application_name=dispatch%40box), or the @ in its password as %40 and a / in it as %2F"
	// EndBeforeAt: a ?, # or \ before the @ that ends pgx's user part. pgx reads such a value
	// right, so the openers admit it (misread finds nothing in it), but net/url ends the authority
	// at that character and reads a piece of the user part as the host, or fails with an error that
	// quotes the value whole. A reader that parses the value with net/url refuses it with these
	// words first (URLParserReadsApart).
	EndBeforeAt redact.Misreading = "has a ?, # or \\ in its user part, where Go's URL parser ends its host; write each of them in the user part percent-encoded (? as %3F, # as %23, \\ as %5C)"
	// NotParsed: pgx cannot parse the value. pgx's parse error masks the password and quotes the
	// rest, the user among it, which can be a token written where the user stands.
	NotParsed redact.Misreading = "is not a Postgres connection string pgx can parse; check its port and the name and value of each of its options (sslmode=require, say)"
)

// Refuse is the refusal of raw, the value of the setting named, whose user part pgx reads apart
// (misread), named by its length and what the check found, or nil.
func Refuse(setting, raw string) error {
	if why := misread(raw); why != "" {
		return redact.Refusal(setting, raw, why)
	}
	return nil
}

// ParseConfig is raw, the value of the setting named, as pgx reads it: refused first when Refuse
// refuses it, and named by its length when pgx cannot parse it.
func ParseConfig(setting, raw string) (*pgx.ConnConfig, error) {
	if err := Refuse(setting, raw); err != nil {
		return nil, err
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		return nil, redact.Refusal(setting, raw, NotParsed)
	}
	return config, nil
}

// ParsePoolConfig is ParseConfig for a pool: raw as pgxpool reads it, refused the same way.
func ParsePoolConfig(setting, raw string) (*pgxpool.Config, error) {
	if err := Refuse(setting, raw); err != nil {
		return nil, err
	}
	config, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return nil, redact.Refusal(setting, raw, NotParsed)
	}
	return config, nil
}

// misread is what makes raw a Postgres URL that pgx does not read as the URL it is, or whose user
// part pgx itself reads apart, so that pgx takes a piece of the user part for a host or a database
// name, which its connect error and a refusal naming its host would print; the empty Misreading
// when nothing does. A value that holds :// is a URL, and one pgx would read as keyword=value text
// is refused first (SchemeNotRead). pgx reads the user part as libpq does, up to the FIRST @
// before the first / (pgconn's parseURLSettings), and keeps a ?, # or \ before that @ in it, where
// net/url would end the authority (URLParserReadsApart). So a raw @ anywhere after the first @ is
// refused: a second @ before the first / (AtsBeforePath); an @ after the first / with none before
// it, which may end a user part that held a / (AtAfterPath); and, with an @ before the /, another
// in the database name (AtInDatabaseName) or in the query (AtInQuery), which may follow a piece of
// a password that held an @ and a /. No rule of the string tells such a password from an honest @
// in the same place, and the honest form can always write that @ as %40. The key=value form,
// which holds no ://, has no such part.
func misread(raw string) redact.Misreading {
	authority, afterPath, ok := splitURL(raw)
	if strings.Contains(raw, "://") && (!ok || strings.TrimSpace(raw) != raw) {
		return SchemeNotRead
	}
	if !ok {
		return ""
	}
	database, _, _ := strings.Cut(afterPath, "?")
	at := strings.Index(authority, "@")
	switch {
	case at < 0 && strings.Contains(afterPath, "@"):
		return AtAfterPath
	case at < 0:
		return ""
	case strings.Count(authority, "@") > 1:
		return AtsBeforePath
	case strings.Contains(database, "@"):
		return AtInDatabaseName
	case strings.Contains(afterPath, "@"):
		return AtInQuery
	}
	return ""
}

// URLParserReadsApart reports whether net/url reads raw's user part apart from where pgx reads it:
// a ?, # or \ before the @ that ends pgx's user part, where net/url ends the host (EndBeforeAt).
// pgx reads such a value right, so the openers admit it; a reader that parses the value with
// net/url first refuses it, since net/url would name a piece of the user part as the host or quote
// the value whole in its error.
func URLParserReadsApart(raw string) bool {
	authority, _, ok := splitURL(raw)
	if !ok {
		return false
	}
	at := strings.Index(authority, "@")
	return at >= 0 && strings.ContainsAny(authority[:at], "?#\\")
}

// splitURL is raw's authority, up to its first / after the scheme, and what follows that /, when
// raw is a Postgres URL; ok is false for the key=value form.
func splitURL(raw string) (authority, afterPath string, ok bool) {
	rest, ok := strings.CutPrefix(raw, "postgres://")
	if !ok {
		if rest, ok = strings.CutPrefix(raw, "postgresql://"); !ok {
			return "", "", false
		}
	}
	authority, afterPath, _ = strings.Cut(rest, "/")
	return authority, afterPath, true
}

// connectFailure is err, an error that holds a pgconn.ConnectError, as a message may print it: the
// words of each wrapper over the ConnectError as they are, and the ConnectError as what failed at
// each host the connection tried, without the user and the database pgx names in front of it, since
// a URL's user part can be a token written where the user stands, and with each answer a server
// gave named by its severity and SQLSTATE alone, since the server's own words name the user too
// (`password authentication failed for user "..."`, `role "..." does not exist`). It unwraps to
// err, so errors.Is and errors.As still find what err holds, the ConnectError and the server's
// *pgconn.PgError under it among them.
type connectFailure struct{ err error }

func (c connectFailure) Error() string {
	return rewritten(c.err, func(connect *pgconn.ConnectError) string {
		return "failed to connect: " + rewritten(connect.Unwrap(), func(server *pgconn.PgError) string {
			return server.Severity + " (SQLSTATE " + server.Code + ")"
		})
	})
}
func (c connectFailure) Unwrap() error { return c.err }

// ConnectFailure is err, when it holds a pgconn.ConnectError at any depth, written with that error
// named without the user and the database pgx names in front of it and without the words of any
// server's answer under it (connectFailure), and err itself otherwise. A caller may wrap an error
// with it wherever a connect can have failed under it: the words around the failure stay.
func ConnectFailure(err error) error {
	var connect *pgconn.ConnectError
	if errors.As(err, &connect) {
		return connectFailure{err}
	}
	return err
}

// rewritten is err's text with each error of type T in it, at any depth, written as name writes
// it. An error that holds none prints as it is, and so does the text of a wrapper in front of what
// it wraps, fmt.Errorf's %w and pgconn's own wrappers alike (the address and host a connect
// dialled, what failed there); a wrapper whose text does not end with the text it wraps prints
// only what it wraps, and one that wraps several (errors.Join, one per host a connect tried) prints
// each on a line of its own, as errors.Join does.
func rewritten[T error](err error, name func(T) string) string {
	if found, ok := err.(T); ok {
		return name(found)
	}
	var found T
	if !errors.As(err, &found) {
		return err.Error()
	}
	switch wrapper := err.(type) {
	case interface{ Unwrap() []error }:
		var each []string
		for _, inner := range wrapper.Unwrap() {
			if inner != nil {
				each = append(each, rewritten(inner, name))
			}
		}
		return strings.Join(each, "\n")
	case interface{ Unwrap() error }:
		if inner := wrapper.Unwrap(); inner != nil && errors.As(inner, new(T)) {
			own, ok := strings.CutSuffix(err.Error(), inner.Error())
			if !ok {
				own = ""
			}
			return own + rewritten(inner, name)
		}
	}
	// Found through err's own As method rather than in what err wraps: named alone.
	return name(found)
}
