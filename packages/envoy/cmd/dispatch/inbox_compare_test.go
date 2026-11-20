package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The comparison reads loopback servers alone: an address whose host is no loopback name or
// literal, given as either flag, is refused with exit 2 before anything is opened or read, naming
// the flag and the address.
func TestInboxCompareRefusesAServerNotOnThisMachine(t *testing.T) {
	for _, run := range []struct {
		args          []string
		refused, host string
	}{
		{[]string{"--old-url", "https://dispatch.example", "--new-url", "http://127.0.0.1:8777"}, "--old-url", "https://dispatch.example"},
		{[]string{"--old-url", "http://localhost:8777", "--new-url", "http://10.0.0.7:8777"}, "--new-url", "http://10.0.0.7:8777"},
	} {
		var out, errOut bytes.Buffer
		code := inboxCompare(context.Background(), run.args, "postgres://never-opened.example/dispatch", &out, &errOut)
		if code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), run.refused+` "`+run.host+`" names no server on this machine`) {
			t.Errorf("inbox-compare %v exited %d, printed %q and said %q; want exit 2, no report, and %s %s refused as no server on this machine",
				run.args, code, out.String(), errOut.String(), run.refused, run.host)
		}
	}
}

// A server that serves no dev sign-in - a deployment never does - is refused before any person is
// compared: the comparison signs in only through GET /auth/_dev/signin, so it exits 2 naming that
// refusal and stores no run. The old server is the one without it, and the first the run asks.
func TestInboxCompareRefusesAServerWithoutDevSignIn(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into people (email) values ('alice@example.com')`); err != nil {
		t.Fatalf("record a person: %v", err)
	}
	signsIn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/_dev/signin" {
			http.NotFound(w, r)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "dsession", Value: "signed-in", Path: "/"})
		http.Redirect(w, r, "/", http.StatusFound)
	}))
	t.Cleanup(signsIn.Close)
	deployed := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(deployed.Close)

	var out, errOut bytes.Buffer
	code := inboxCompare(ctx, []string{"--old-url", deployed.URL, "--new-url", signsIn.URL},
		database.Pool.Config().ConnString(), &out, &errOut)
	if code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "refused: --old-url "+deployed.URL+" refused the dev sign-in") {
		t.Errorf("inbox-compare over a server without dev sign-in exited %d, printed %q and said %q; want exit 2, no report, and the old server refused for its sign-in",
			code, out.String(), errOut.String())
	}
	var runs int
	if err := database.Pool.QueryRow(ctx, `select count(*) from inbox_comparison_runs`).Scan(&runs); err != nil {
		t.Fatalf("count the stored runs: %v", err)
	}
	if runs != 0 {
		t.Errorf("the refused comparison stored %d run(s), want none", runs)
	}
}
