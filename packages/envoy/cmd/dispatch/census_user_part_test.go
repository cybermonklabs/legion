package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// The acceptance test's red test of `envoy-dispatch census`, kept as it ran: the deployment's
// pre-deploy census reads the same DATABASE_URL as the server and prints its error to stderr
// whole, so it reads the value as store.Open does. A value whose user part pgx may read apart is
// refused by its length, and a failed connect names no user; a ? or # before the @, which pgx keeps
// in the user part, reaches the connect and is named no better. The control inside the test: a
// value with no user part on a port nothing listens on is answered with a connect failure naming
// 127.0.0.1:1, so the line under test is reached. The user, the password and the token are made
// for this test.
func TestCensusNamesNoPartOfTheUserPartOfItsDatabaseURL(t *testing.T) {
	const (
		user     = "made-up-user-7e2a"
		password = "made-up-pw-not-a-secret-c41d"
		token    = "madeuptoken9b3f"
	)
	run := func(databaseURL string) (int, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var out, errOut bytes.Buffer
		code := census(ctx, databaseURL, &out, &errOut)
		return code, out.String() + errOut.String()
	}
	code, control := run("postgres://127.0.0.1:1/x?connect_timeout=3")
	t.Logf("control (no user part): exit %d: %s", code, strings.TrimSpace(control))
	if !strings.Contains(control, "127.0.0.1:1") {
		t.Fatalf("UNKNOWN: the control's output names no host, so this test saw nothing: %q", control)
	}
	for name, databaseURL := range map[string]string{
		"a token and a / as the user part":  "postgres://" + token + "/" + password + "@127.0.0.1:1/x?connect_timeout=3",
		"a password with a /":               "postgres://" + user + ":5432/" + password + "@127.0.0.1:1/x?connect_timeout=3",
		"a password with an @":              "postgres://" + user + ":" + token + "@" + password + "@127.0.0.1:1/x?connect_timeout=3",
		"a password and a user":             "postgres://" + user + ":" + password + "@127.0.0.1:1/x?connect_timeout=3",
		"a ? in a password and a user":      "postgres://" + user + ":" + token + "?" + password + "@127.0.0.1:1/x?connect_timeout=3",
		"a # in a password and a user":      "postgres://" + user + ":" + token + "#" + password + "@127.0.0.1:1/x?connect_timeout=3",
		"a token with a ? as the user part": "postgres://" + token + "?" + password + "@127.0.0.1:1/x?connect_timeout=3",
		"a token with a # as the user part": "postgres://" + token + "#" + password + "@127.0.0.1:1/x?connect_timeout=3",
	} {
		code, text := run(databaseURL)
		shown := strings.NewReplacer(user, "<USER>", password, "<PASSWORD>", token, "<TOKEN>").Replace(text)
		t.Logf("%s: exit %d: %s", name, code, strings.TrimSpace(shown))
		for _, part := range []string{user, password, token} {
			if strings.Contains(text, part) {
				t.Errorf("RED %s: what census prints holds a part of DATABASE_URL's user part: %s", name, strings.TrimSpace(shown))
				break
			}
		}
	}
}
