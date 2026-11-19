package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/pgsetting"
)

// Every DATABASE_URL a process opens goes through Open, the server's boot and each subcommand's
// alike, so Open refuses one whose user part pgx may read apart from the rest of it, named by its
// length and the cause the check found, before any connect: pgx would take a piece of that part for
// a host or a database, which its connect error prints. The control is the same value with that
// part percent-encoded, which Open takes to pgx, whose connect to a port nothing listens on then
// fails naming no part of the password (pgx names the user, which is the role). The user, the
// passwords and the token are made for this test.
func TestOpenRefusesAMisreadURLByItsLength(t *testing.T) {
	const (
		user   = "made-up-user-2c5e"
		first  = "not-a-secret-7d10"
		second = "not-a-secret-8e21"
		token  = "not-a-secret-tok4f3"
	)
	for _, tc := range []struct {
		name, value, encoded string
		why                  redact.Misreading
	}{
		{"a / in the password", "postgres://" + user + ":" + first + "/" + second + "@127.0.0.1:1/x?connect_timeout=3", "postgres://" + user + ":" + first + "%2F" + second + "@127.0.0.1:1/x?connect_timeout=3", pgsetting.AtAfterPath},
		{"a token and a / as the user part", "postgres://" + token + "/" + first + "@127.0.0.1:1/x?connect_timeout=3", "postgres://" + token + "%2F" + first + "@127.0.0.1:1/x?connect_timeout=3", pgsetting.AtAfterPath},
		{"an @ in the password", "postgresql://" + user + ":" + first + "@" + second + "@127.0.0.1:1/x?connect_timeout=3", "postgresql://" + user + ":" + first + "%40" + second + "@127.0.0.1:1/x?connect_timeout=3", pgsetting.AtsBeforePath},
		{"an @ in a query with no / before it", "postgres://" + user + ":" + first + "@127.0.0.1:1?application_name=a@b", "postgres://" + user + ":" + first + "@127.0.0.1:1/?application_name=a%40b&connect_timeout=3", pgsetting.AtsBeforePath},
		{"an @ in the query", "postgres://" + user + ":" + first + "@127.0.0.1:1/x?application_name=a@b&connect_timeout=3", "postgres://" + user + ":" + first + "@127.0.0.1:1/x?application_name=a%40b&connect_timeout=3", pgsetting.AtInQuery},
		{"an @, a /, a ? and an = in the password", "postgres://" + user + ":" + first + "@" + second + "/x?connect_timeout=3&a=b@127.0.0.1:1/x", "postgres://" + user + ":" + first + "%40" + second + "%2Fx%3Fconnect_timeout%3D3%26a%3Db@127.0.0.1:1/x?connect_timeout=3", pgsetting.AtInQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(context.Background(), tc.value)
			want := fmt.Sprintf("DATABASE_URL, a value of %d characters, %s", len(tc.value), tc.why)
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			for _, part := range []string{user, first, second, token} {
				if strings.Contains(err.Error(), part) {
					t.Errorf("the refusal holds a part of the user part: %q", err)
				}
			}
			_, err = Open(context.Background(), tc.encoded)
			if err == nil || strings.Contains(err.Error(), "DATABASE_URL, a value of") {
				t.Fatalf("the control with the part percent-encoded: err = %v, want pgx's connect failure", err)
			}
			if strings.Contains(err.Error(), second) {
				t.Errorf("the control: pgx's error holds the password: %q", err)
			}
		})
	}
}

// A ?, # or \ before the @ that ends the user part is read right by pgx, which keeps it in the user
// part, so Open takes such a value to pgx; net/url would end the host there. Each error Open gives
// it, a connect to a port nothing listens on and a mistyped option, names no part of the user
// part: not the password, not a token written as the whole user part, not the user. The control is
// pgx's own connect error for the token form, which names the token as the user. The user, the
// passwords and the token are made for this test.
func TestOpenNamesNoPartOfAUserPartWithARawQuestionMarkOrHash(t *testing.T) {
	const (
		user   = "made-up-user-5b19"
		first  = "not-a-secret-6c2a"
		second = "not-a-secret-7d3b"
		token  = "not-a-secret-tok8e4"
	)
	for _, tail := range []string{"@127.0.0.1:1/x?connect_timeout=3", "@127.0.0.1:1/x?connect_timeout=3&sslmode=bogus"} {
		for _, userPart := range []string{
			user + ":" + first + "?" + second,
			user + ":" + first + "#" + second,
			token + "?" + second,
			token + "#" + second,
		} {
			value := "postgres://" + userPart + tail
			_, err := Open(context.Background(), value)
			if err == nil {
				t.Fatalf("%d characters: Open succeeded, want a failure", len(value))
			}
			for _, part := range []string{user, first, second, token} {
				if strings.Contains(err.Error(), part) {
					t.Errorf("%d characters: the error holds a part of the user part: %q", len(value), err)
				}
			}
		}
	}
	config, err := pgx.ParseConfig("postgres://" + token + "?" + second + "@127.0.0.1:1/x?connect_timeout=3")
	if err != nil {
		t.Fatalf("UNKNOWN: pgx does not parse the token form, so this row saw nothing: %v", err)
	}
	_, err = pgx.ConnectConfig(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), token) {
		t.Fatalf("UNKNOWN: pgx's own connect error does not name the token, so this row saw nothing: %v", err)
	}
}

// A DATABASE_URL with one @, which ends its user part, and one whose other @ is written %40, in its
// query or in its database name, is read by pgx and net/url alike: Open takes each to pgx, whose
// connect to a port nothing listens on then fails naming no part of the password. pgx decodes the
// %40 of a query value. The control is the same values with that @ raw, which Open refuses by
// length and cause: a raw @ after the first one may follow a piece of a password. The user and the
// password are made for this test.
func TestOpenTakesAnOtherAtWrittenPercent40ToPgx(t *testing.T) {
	const (
		user     = "made-up-user-1b7c"
		password = "not-a-secret-3f9a"
	)
	for _, value := range []string{
		"postgres://" + user + ":" + password + "@127.0.0.1:1/dispatch?application_name=dispatch-box&connect_timeout=3",
		"postgres://" + user + ":" + password + "@127.0.0.1:1/dispatch?application_name=dispatch%40box&connect_timeout=3",
		"postgres://" + user + ":" + password + "@127.0.0.1:1/dispatch?sslmode=disable&application_name=ops%40example.com&connect_timeout=3",
		"postgres://" + user + "@127.0.0.1:1/dispatch?application_name=a%40b&connect_timeout=3",
		"postgres://" + user + ":" + password + "@127.0.0.1:1/dis%40patch?connect_timeout=3",
	} {
		_, err := Open(context.Background(), value)
		if err == nil || strings.Contains(err.Error(), "DATABASE_URL, a value of") || !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Errorf("%d characters: err = %v, want pgx's connect failure naming 127.0.0.1:1", len(value), err)
			continue
		}
		if strings.Contains(err.Error(), password) {
			t.Errorf("%d characters: pgx's error holds the password: %q", len(value), err)
		}
		raw := strings.ReplaceAll(value, "%40", "@")
		if raw == value {
			continue
		}
		_, err = Open(context.Background(), raw)
		if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("DATABASE_URL, a value of %d characters, has an @ before its first / and another in its", len(raw))) {
			t.Errorf("the control, the @ raw, %d characters: err = %v, want the refusal by its length", len(raw), err)
		}
	}
}

// Open reads DATABASE_URL twice: with pgx's parse first (declaresPoolSize), which keeps a pool
// option as a parameter, and then with pgxpool's, which reads it. So a pool option pgxpool cannot
// read is refused only by the second parse, and its refusal names the value by its length: pgxpool's
// own parse error quotes the value with only its password masked, so a token written as the whole
// user part would be printed. The control is the first parse admitting each value, so the refusal
// under test is the pool parse's. The token is made for this test.
func TestOpenNamesAPoolOptionPgxpoolCannotReadByItsLength(t *testing.T) {
	const token = "not-a-secret-tok-r9p2"
	for _, value := range []string{
		"postgres://" + token + "@127.0.0.1:1/dispatch?pool_min_conns=abc&connect_timeout=3",
		"postgres://" + token + "@127.0.0.1:1/dispatch?pool_max_conn_lifetime=1hr&connect_timeout=3",
	} {
		if declared, err := declaresPoolSize(value); declared || err != nil {
			t.Fatalf("UNKNOWN: pgx's parse does not admit the value, %d characters, so the pool parse is not reached: %v, %v", len(value), declared, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := Open(ctx, value)
		cancel()
		if want := fmt.Sprintf("DATABASE_URL, a value of %d characters, %s", len(value), pgsetting.NotParsed); err == nil || err.Error() != want {
			t.Errorf("Open = %q, want %q", strings.ReplaceAll(fmt.Sprint(err), token, "<TOKEN>"), want)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Errorf("the refusal names the token written as the user part: %q", strings.ReplaceAll(err.Error(), token, "<TOKEN>"))
		}
	}
}

// The acceptance test's red test of a DATABASE_URL pgx cannot parse, kept as it ran with the cause
// asserted: the boot logs Open's error at "dispatch: open database", and pgx's parse error masks the
// password but quotes the user, which can be a token written where the user stands. Open names
// such a value by its length and says that pgx cannot parse it. The control inside the test: the
// same value with no user part is refused at the parse too, so the line under test is reached. The
// user, the password and the token are made for this test.
func TestOpenNamesAValuePgxCannotParseByItsLength(t *testing.T) {
	const (
		user     = "made-up-user-5f8c"
		password = "made-up-pw-not-a-secret-2b7e"
		token    = "madeuptoken4d1a"
	)
	open := func(databaseURL string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := Open(ctx, databaseURL); err != nil {
			return err.Error()
		}
		return "(opened)"
	}
	controlURL := "postgres://127.0.0.1:1/x?sslmode=bogus"
	control := open(controlURL)
	if control != fmt.Sprintf("DATABASE_URL, a value of %d characters, %s", len(controlURL), pgsetting.NotParsed) {
		t.Fatalf("UNKNOWN: the control is not refused at the parse, so this test saw nothing: %q", control)
	}
	for name, databaseURL := range map[string]string{
		"a password and a user": "postgres://" + user + ":" + password + "@127.0.0.1:1/x?sslmode=bogus",
		"a token as the user":   "postgres://" + token + "@127.0.0.1:1/x?sslmode=bogus",
	} {
		text := open(databaseURL)
		if want := fmt.Sprintf("DATABASE_URL, a value of %d characters, %s", len(databaseURL), pgsetting.NotParsed); text != want {
			t.Errorf("%s: %q, want %q", name, strings.NewReplacer(user, "<USER>", password, "<PASSWORD>", token, "<TOKEN>").Replace(text), want)
		}
		for _, part := range []string{user, password, token} {
			if strings.Contains(text, part) {
				t.Errorf("RED %s: the error the boot logs at \"dispatch: open database\" holds a part of DATABASE_URL's user part", name)
				break
			}
		}
	}
}

// A database Open cannot reach fails naming what failed at the host it tried, and no part of the
// URL's user part: pgx names the user in front of its connect error, and a token can stand where
// the user does. The control is a URL with no user part, whose error names the host the same way.
// The user, the password and the token are made for this test.
func TestOpenNamesNoPartOfTheUserPartWhenItCannotConnect(t *testing.T) {
	const (
		user     = "made-up-user-5a3d"
		password = "not-a-secret-6b02"
		token    = "not-a-secret-tok7c1"
	)
	for _, userPart := range []string{user + ":" + password + "@", token + "@", ""} {
		_, err := Open(context.Background(), "postgres://"+userPart+"127.0.0.1:1/x?connect_timeout=3")
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Fatalf("user part %q: err = %v, want the failed connect naming the host", userPart, err)
		}
		for _, part := range []string{user, password, token} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("user part %q: the error holds a part of the user part: %q", userPart, err)
			}
		}
	}
}

// CheckVersions, `envoy-dispatch versions`, reads DATABASE_URL as Census does: a value pgx cannot
// parse is named by its length, and a database it cannot reach fails naming the host and no part
// of the user part, where pgx's own parse and connect errors name the user, which can be a token.
// The control is a URL with no user part, whose connect error names the host the same way. The
// user, the password and the token are made for this test.
func TestCheckVersionsNamesNoPartOfTheUserPart(t *testing.T) {
	const (
		user     = "made-up-user-9c4e"
		password = "not-a-secret-2d7f"
		token    = "not-a-secret-tok5a8"
	)
	for _, userPart := range []string{user + ":" + password + "@", token + "@", ""} {
		unparsed := "postgres://" + userPart + "127.0.0.1:1/x?sslmode=bogus"
		_, err := CheckVersions(context.Background(), unparsed)
		if want := fmt.Sprintf("DATABASE_URL, a value of %d characters, %s", len(unparsed), pgsetting.NotParsed); err == nil || err.Error() != want {
			t.Errorf("user part %q, a value pgx cannot parse: err = %q, want %q", userPart, strings.NewReplacer(user, "<USER>", password, "<PASSWORD>", token, "<TOKEN>").Replace(fmt.Sprint(err)), want)
		}
		_, err = CheckVersions(context.Background(), "postgres://"+userPart+"127.0.0.1:1/x?connect_timeout=3")
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Fatalf("user part %q: err = %v, want the failed connect naming the host", userPart, err)
		}
		for _, part := range []string{user, password, token} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("user part %q: the error holds a part of the user part: %q", userPart, err)
			}
		}
	}
}
