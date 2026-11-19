package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/redact"
)

// A DATABASE_URL whose user part pgx and net/url can read apart differently is refused at every
// boot, named by its length and the cause the check found. pgx ends the user part at the first @
// before any /, and net/url at the last @ and at the first /, ? or #, so each such form hands pgx a
// piece of the user part as a host or a database name, which its connect error, logged at every
// boot that does not reach the database, and the dev sign-in fence's refusal would print. Each form
// is refused with the dev sign-in and without it, in the words store.Open refuses it with, since
// one function makes both. Its control is the same value with the @ or / of its user part
// percent-encoded: that boots, and under the dev sign-in it is admitted on a loopback host and
// refused naming a remote one. The user, the passwords and the token are made for this test.
func TestBootRefusesAMisreadDatabaseURLByItsLength(t *testing.T) {
	const (
		user   = "made-up-user-0d3b"
		first  = "not-a-secret-a5f1"
		second = "not-a-secret-b27e"
		token  = "not-a-secret-tok3n4c9a"
	)
	const (
		afterPath  = "has an @ after its first / and none before it"
		beforePath = "has more than one @ before its first /"
	)
	for _, tc := range []struct {
		name, value, encoded, cause string
		loopback                    bool
	}{
		{name: "a / in the password", value: "postgres://" + user + ":" + first + "/" + second + "@db.example:5432/x", encoded: "postgres://" + user + ":" + first + "%2F" + second + "@db.example:5432/x", cause: afterPath},
		{name: "a token and a / as the user part", value: "postgres://" + token + "/" + first + "@db.example/x", encoded: "postgres://" + token + "%2F" + first + "@db.example/x", cause: afterPath},
		{name: "an @ in the password", value: "postgres://" + user + ":" + first + "@" + second + "@db.example:5432/x", encoded: "postgres://" + user + ":" + first + "%40" + second + "@db.example:5432/x", cause: beforePath},
		{name: "an @ and a , in the password", value: "postgres://" + user + ":" + first + "@" + second + ",x@db.example:5432/x", encoded: "postgres://" + user + ":" + first + "%40" + second + "%2Cx@db.example:5432/x", cause: beforePath},
		{name: "an @ in the password, on a loopback host", value: "postgresql://" + user + ":" + first + "@" + second + "@127.0.0.1:5432/x", encoded: "postgresql://" + user + ":" + first + "%40" + second + "@127.0.0.1:5432/x", cause: beforePath, loopback: true},
	} {
		for _, devSignIn := range []bool{false, true} {
			name := tc.name
			environment := func(value string) settingValues {
				return bootEnvironment(map[string]string{"DATABASE_URL": value})
			}
			if devSignIn {
				name += ", under the dev sign-in"
				environment = func(value string) settingValues {
					return devSignInEnvironment(map[string]string{"DATABASE_URL": value})
				}
			}
			t.Run(name, func(t *testing.T) {
				_, err := resolveBootConfig(environment(tc.value))
				want := fmt.Sprintf("DATABASE_URL, a value of %d characters, %s", len(tc.value), tc.cause)
				if err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("err = %v, want a refusal that begins %q", err, want)
				}
				if _, opened := store.Open(context.Background(), tc.value); opened == nil || opened.Error() != err.Error() {
					t.Errorf("store.Open refuses it with %v, and the boot with %q: the two differ", opened, err)
				}
				for _, part := range []string{user, first, second, token} {
					if strings.Contains(err.Error(), part) {
						t.Errorf("the refusal holds a part of the user part: %q", err)
					}
				}
				_, err = resolveBootConfig(environment(tc.encoded))
				switch {
				case !devSignIn || tc.loopback:
					if err != nil {
						t.Errorf("the control with the user part percent-encoded: %v, want it admitted", err)
					}
				case err == nil || !strings.Contains(err.Error(), `DATABASE_URL names host "db.example"`):
					t.Errorf("the control with the user part percent-encoded: err = %v, want the refusal of its remote host db.example", err)
				}
			})
		}
	}
}

// A DATABASE_URL with one @, which ends its user part, boots, and so does one whose other @ is
// written %40, in its query or in its database name: pgx reads each as net/url does, and decodes
// the %40 of a query value. Under the dev sign-in each is admitted on a loopback host. Its control
// is the same value with its host remote, which the dev sign-in refuses naming that host, so the
// host the fence read is the one both readers read. The user and the password are made for this
// test.
func TestBootAdmitsADatabaseURLWithOneAtOrItsOtherAtWrittenPercent40(t *testing.T) {
	const (
		user     = "made-up-user-8e2d"
		password = "not-a-secret-c41f"
	)
	for _, rest := range []string{
		"/dispatch?application_name=dispatch-box",
		"/dispatch?application_name=dispatch%40box",
		"/dispatch?sslmode=require&application_name=ops%40example.com",
		"/dis%40patch",
	} {
		for _, userPart := range []string{user + ":" + password + "@", user + "@"} {
			value := "postgres://" + userPart + "127.0.0.1:5432" + rest
			for _, environment := range []func(map[string]string) settingValues{bootEnvironment, devSignInEnvironment} {
				if _, err := resolveBootConfig(environment(map[string]string{"DATABASE_URL": value})); err != nil {
					t.Errorf("%d characters: %v, want it admitted", len(value), err)
				}
			}
			remote := "postgres://" + userPart + "db.example:5432" + rest
			_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DATABASE_URL": remote}))
			if err == nil || !strings.Contains(err.Error(), `DATABASE_URL names host "db.example"`) {
				t.Errorf("the control, %d characters, on a remote host: err = %v, want the refusal naming db.example", len(remote), err)
			}
		}
	}
}

// The acceptance test's red test of a DATABASE_URL whose password holds a raw @, kept as it ran, its
// made-up passwords marked for the leak checker: under DISPATCH_DEV_SIGNIN=1, pgx reads the user
// part as ending at the FIRST @, so the host it reads is the password's second half and the host
// after it; the refusal must hold no part of the user part. The control inside the test: a remote
// DATABASE_URL with a plain password is refused naming its host, so the line under test is reached.
func TestADatabaseURLWhosePasswordHoldsAnAtNamesNoPartOfIt(t *testing.T) {
	const (
		user   = "made-up-user-3c9e"
		first  = "not-a-secret-pwa71f0"
		second = "not-a-secret-pwb52d8"
	)
	boot := func(databaseURL string) string {
		_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DATABASE_URL": databaseURL}))
		if err == nil {
			return "(accepted)"
		}
		return err.Error()
	}
	control := boot("postgres://" + user + ":" + first + "@db.example:5432/dispatch_local")
	t.Logf("control (a plain password, a remote host): %s", strings.NewReplacer(user, "<USER>", first, "<P1>").Replace(control))
	if !strings.Contains(control, `names host "db.example"`) {
		t.Fatalf("UNKNOWN: the control is not refused naming its host, so this test saw nothing: %q", control)
	}
	for _, host := range []string{"127.0.0.1:5432", "db.example:5432"} {
		text := boot("postgres://" + user + ":" + first + "@" + second + "@" + host + "/dispatch_local")
		shown := strings.NewReplacer(user, "<USER>", first, "<P1>", second, "<P2>").Replace(text)
		t.Logf("an @ in the password, host %s: %s", host, shown)
		for _, part := range []string{user, first, second} {
			if strings.Contains(text, part) {
				t.Errorf("RED host %s: the boot refusal holds a part of DATABASE_URL's password: %s", host, shown)
				break
			}
		}
	}
}

// A DATABASE_URL with an @ before its first / and another after it, in its database name or its
// query, reads two ways: a password that holds a raw @ and then a raw / (and, for the query, a ?
// and an =) gives pgx the host <piece>, a piece of the password
// (postgres://<user>:<p>@<piece>/x@<host>/db, postgres://<user>:<p>@<piece>/x?a=b@<host>/db),
// and an honest host with an @ in its database name or a query value reads the same
// (postgres://<user>:<p>@<host>/dis@patch, …/db?application_name=dispatch@box). pgx names the host
// it read in every error of a connect, a later query's included, so no line can be kept from
// printing the piece once such a value is admitted: each is refused at every boot, under the dev
// sign-in or not, by the census and by store.Open, by its length and the cause. Each honest form
// can be written with the @ as %40, in the password, the database name or the query, and boots.
// The control is the same value with no @ after its /, whose host each line names, so each line
// under test is reached. The user, the password and its piece are made for this test.
func TestADatabaseURLWithAnAtInItsPasswordAndAnotherAfterItsSlashIsRefusedByItsLength(t *testing.T) {
	const (
		user  = "made-up-user-6b0e"
		first = "not-a-secret-pw6b0e"
		piece = "not-a-secret-piece-6b0e"
	)
	control := "postgres://" + user + ":" + first + "@" + piece + "/dispatch?connect_timeout=3"
	censusText := func(databaseURL string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var out, errOut bytes.Buffer
		census(ctx, databaseURL, &out, &errOut)
		return out.String() + errOut.String()
	}
	open := func(databaseURL string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := store.Open(ctx, databaseURL); err != nil {
			return err.Error()
		}
		return "(opened)"
	}
	boot := func(environment func(map[string]string) settingValues) func(string) string {
		return func(databaseURL string) string {
			if _, err := resolveBootConfig(environment(map[string]string{"DATABASE_URL": databaseURL})); err != nil {
				return err.Error()
			}
			return "(admitted)"
		}
	}
	for _, shape := range []struct{ name, value, cause string }{
		{"in its database name", "postgres://" + user + ":" + first + "@" + piece + "/x@127.0.0.1:1/dispatch?connect_timeout=3", "has an @ before its first / and another in its database name"},
		{"in its query", "postgres://" + user + ":" + first + "@" + piece + "/x?connect_timeout=3&a=b@127.0.0.1:1/dispatch", "has an @ before its first / and another in its query"},
	} {
		named := fmt.Sprintf("DATABASE_URL, a value of %d characters, %s", len(shape.value), shape.cause)
		for _, line := range []struct {
			name    string
			text    func(string) string
			control bool
		}{
			{"the boot", boot(bootEnvironment), false},
			{"the boot under the dev sign-in", boot(devSignInEnvironment), true},
			{"the census", censusText, true},
			{"store.Open", open, true},
		} {
			t.Run(shape.name+", "+line.name, func(t *testing.T) {
				if text := line.text(control); line.control && !strings.Contains(text, piece) {
					t.Fatalf("UNKNOWN: the control names no host, so this row saw nothing: %q", text)
				}
				text := line.text(shape.value)
				shown := strings.NewReplacer(user, "<USER>", first, "<P1>", piece, "<PIECE>").Replace(text)
				if !strings.Contains(text, named) {
					t.Errorf("%q, want the refusal %q", shown, named)
				}
				for _, part := range []string{user, first, piece} {
					if strings.Contains(text, part) {
						t.Errorf("the line names a part of the password as the host or the user: %q", shown)
						break
					}
				}
			})
		}
	}
	for _, encoded := range []string{
		"postgres://" + user + ":" + first + "%40" + piece + "%2Fx@127.0.0.1:1/dispatch",
		"postgres://" + user + ":" + first + "%40" + piece + "%2Fx%3Fa%3Db@127.0.0.1:1/dispatch",
		"postgres://" + user + ":" + first + "@127.0.0.1:1/dis%40patch",
		"postgres://" + user + ":" + first + "@127.0.0.1:1/dispatch?application_name=dispatch%40box",
	} {
		for _, environment := range []func(map[string]string) settingValues{bootEnvironment, devSignInEnvironment} {
			if _, err := resolveBootConfig(environment(map[string]string{"DATABASE_URL": encoded})); err != nil {
				t.Errorf("the %%40 spelling, %d characters: %v, want it admitted", len(encoded), err)
			}
		}
	}
}

// A DATABASE_URL whose scheme is not in lower case, or with white space around it, holds :// but
// pgx reads it as keyword=value text: the URL up to its first =, the password among it, becomes the
// name of a setting pgx sends the server, whose refusal of an unknown setting names it, and pgx
// dials the local socket, so the refusals for an @ after the first never see it. Each is refused at
// every boot, under the dev sign-in or not, by the census and by store.Open, by its length and the
// cause. The boot trims its value, so white space around it reaches only the census and store.Open.
// The control is the same URL in lower case with no space around it: the boot admits it, and the
// dev sign-in fence, the census and store.Open each name its host, so each line under test is
// reached. The user, the password and its piece are made for this test.
func TestADatabaseURLThatPgxReadsAsKeywordValueTextIsRefusedByItsLength(t *testing.T) {
	const (
		user  = "made-up-user-5c1a"
		first = "not-a-secret-pw5c1a"
		piece = "not-a-secret-piece-5c1a"
		cause = "holds :// but does not start with postgres:// or postgresql:// in lower case with no space around it"
	)
	control := "postgres://" + user + ":" + first + "@" + piece + "/dispatch?connect_timeout=3"
	censusText := func(databaseURL string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var out, errOut bytes.Buffer
		census(ctx, databaseURL, &out, &errOut)
		return out.String() + errOut.String()
	}
	open := func(databaseURL string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := store.Open(ctx, databaseURL); err != nil {
			return err.Error()
		}
		return "(opened)"
	}
	boot := func(environment func(map[string]string) settingValues) func(string) string {
		return func(databaseURL string) string {
			if _, err := resolveBootConfig(environment(map[string]string{"DATABASE_URL": databaseURL})); err != nil {
				return err.Error()
			}
			return "(admitted)"
		}
	}
	if text := boot(bootEnvironment)(control); text != "(admitted)" {
		t.Fatalf("the control in lower case: %q, want it admitted", strings.NewReplacer(user, "<USER>", first, "<P1>", piece, "<PIECE>").Replace(text))
	}
	for _, shape := range []struct {
		name, value      string
		trimmedByTheBoot bool
	}{
		{"an upper-case scheme and an @, a /, a ? and an = in the password", "POSTGRES://" + user + ":" + first + "@" + piece + "/x?a=b@127.0.0.1:1/dispatch", false},
		{"a scheme with one capital", "Postgres://" + user + ":" + first + "@" + piece + "/dispatch?connect_timeout=3", false},
		{"postgresql:// in capitals", "POSTGRESQL://" + user + ":" + first + "@" + piece + "/dispatch?connect_timeout=3", false},
		{"a space before the URL", " " + control, true},
		{"a line ending after the URL", control + "\n", true},
	} {
		named := fmt.Sprintf("DATABASE_URL, a value of %d characters, %s", len(shape.value), cause)
		for _, line := range []struct {
			name    string
			text    func(string) string
			control bool
			atBoot  bool
		}{
			{"the boot", boot(bootEnvironment), false, true},
			{"the boot under the dev sign-in", boot(devSignInEnvironment), true, true},
			{"the census", censusText, true, false},
			{"store.Open", open, true, false},
		} {
			if line.atBoot && shape.trimmedByTheBoot {
				continue
			}
			t.Run(shape.name+", "+line.name, func(t *testing.T) {
				if text := line.text(control); line.control && !strings.Contains(text, piece) {
					t.Fatalf("UNKNOWN: the control names no host, so this row saw nothing: %q", text)
				}
				text := line.text(shape.value)
				shown := strings.NewReplacer(user, "<USER>", first, "<P1>", piece, "<PIECE>").Replace(text)
				if !strings.Contains(text, named) {
					t.Errorf("%q, want the refusal %q", shown, named)
				}
				for _, part := range []string{user, first, piece} {
					if strings.Contains(text, part) {
						t.Errorf("the line names a part of the URL: %q", shown)
						break
					}
				}
			})
		}
	}
}

// ENVOY_URL and DISPATCH_AGENT_SECRETS_URL are each refused at boot, named by its length and the
// cause, when an @ is left once net/url takes out its user part. A token written with a raw #, ? or
// / as the whole user part (http://<a>#<b>@host) is otherwise read with <a> as the host, so the
// server asks DNS for the token's first half and connects to it; an @ in the query alone is refused
// with the same cause, since net/url leaves it outside the user part too. The control is the same
// value with that character percent-encoded, which boots. The token's halves are made for this
// test.
func TestBootRefusesAServiceURLWithAnAtLeftOverByItsLength(t *testing.T) {
	const (
		first  = "not-a-secret-tok1e4"
		second = "not-a-secret-tok2b7"
	)
	for _, setting := range []string{"ENVOY_URL", "DISPATCH_AGENT_SECRETS_URL"} {
		environment := func(value string) settingValues {
			overrides := map[string]string{setting: value}
			if setting == "DISPATCH_AGENT_SECRETS_URL" {
				overrides["DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"] = "/var/run/made-up/account-token"
			}
			return bootEnvironment(overrides)
		}
		for _, form := range []struct{ name, value, encoded string }{
			{"a # in its user part", "http://" + first + "#" + second + "@127.0.0.1:9020", "http://" + first + "%23" + second + "@127.0.0.1:9020"},
			{"a ? in its user part", "http://" + first + "?" + second + "@127.0.0.1:9020", "http://" + first + "%3F" + second + "@127.0.0.1:9020"},
			{"a / in its user part", "http://" + first + "/" + second + "@127.0.0.1:9020", "http://" + first + "%2F" + second + "@127.0.0.1:9020"},
			{"an @ in its query and no user part", "http://127.0.0.1:9020?via=" + first + "@" + second, "http://127.0.0.1:9020?via=" + first + "%40" + second},
		} {
			t.Run(setting+" with "+form.name, func(t *testing.T) {
				_, err := resolveBootConfig(environment(form.value))
				want := fmt.Sprintf("%s, a value of %d characters, %s", setting, len(form.value), redact.AtOutsideUserPart)
				if err == nil || err.Error() != want {
					t.Fatalf("err = %v, want %q", err, want)
				}
				if strings.Contains(err.Error(), first) || strings.Contains(err.Error(), second) {
					t.Errorf("the refusal holds a part of the token: %q", err)
				}
				if _, err := resolveBootConfig(environment(form.encoded)); err != nil {
					t.Errorf("the control %s: %v, want it admitted", form.encoded, err)
				}
			})
		}
	}
}

// ENVOY_URL and DISPATCH_AGENT_SECRETS_URL written with a user part and no scheme
// (<user>:<password>@host:port) are refused naming the missing scheme, by the value's length: net/url
// reads the text before the first colon as the scheme and the rest as an opaque path, so the @ that
// ends the user part is left over, and words that told the reader to write it as %40 would point at
// the wrong correction. A :// later in the value, in its query, is no scheme's, and the refusal is
// the same. The control is the same value with http:// before it, which boots, or, with that query,
// is refused for no scheme (DISPATCH_AGENT_SECRETS_URL takes no path). The user and the password are
// made for this test.
func TestBootRefusesAServiceURLWithAUserPartAndNoSchemeNamingTheScheme(t *testing.T) {
	const (
		user     = "made-up-user-2f8d"
		password = "not-a-secret-pw2f8d"
	)
	for _, setting := range []string{"ENVOY_URL", "DISPATCH_AGENT_SECRETS_URL"} {
		environment := func(value string) settingValues {
			overrides := map[string]string{setting: value}
			if setting == "DISPATCH_AGENT_SECRETS_URL" {
				overrides["DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"] = "/var/run/made-up/account-token"
			}
			return bootEnvironment(overrides)
		}
		for _, value := range []string{user + ":" + password + "@host.example", user + ":" + password + "@host.example/?next=http://x.example"} {
			t.Run(fmt.Sprintf("%s, %d characters", setting, len(value)), func(t *testing.T) {
				_, err := resolveBootConfig(environment(value))
				want := fmt.Sprintf("%s, a value of %d characters, does not start with http:// or https://", setting, len(value))
				if err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("err = %v, want a refusal that begins %q", err, want)
				}
				if strings.Contains(err.Error(), user) || strings.Contains(err.Error(), password) {
					t.Errorf("the refusal holds a part of the user part: %q", err)
				}
				_, err = resolveBootConfig(environment("http://" + value))
				switch {
				case !strings.Contains(value, "?") && err != nil:
					t.Errorf("the control with http:// before it: %v, want it admitted", err)
				case err != nil && strings.Contains(err.Error(), "does not start with http"):
					t.Errorf("the control with http:// before it: %v, want no refusal of its scheme", err)
				}
			})
		}
	}
}
