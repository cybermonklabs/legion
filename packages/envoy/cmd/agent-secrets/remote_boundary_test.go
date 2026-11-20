package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAGcloudRefusalPrintsNoneOfItsWords: gcloud that cannot print an access token writes Google's
// answer to the refresh it failed on its standard error, a URL carrying a credential among it; the
// gcloud sign-in's error, which names that standard error, prints none of the credential.
func TestAGcloudRefusalPrintsNoneOfItsWords(t *testing.T) {
	dir := t.TempDir()
	script := strings.Join([]string{
		"#!/bin/sh",
		`case "$1 $2" in`,
		`"auth list") printf '[{"account":"ada@example.com","status":"ACTIVE"}]\n' ;;`,
		`"auth print-access-token") printf '%s\n' ` + shellWord("ERROR: (gcloud.auth.print-access-token) "+redacttest.Words) + ` >&2; exit 1 ;;`,
		"esac",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	gcloud, err := findGcloud()
	if err != nil {
		t.Fatalf("findGcloud: %v", err)
	}
	err = (&gcpBackend{}).signInWithGcloud(context.Background(), sourceActiveGcloud, "the active gcloud account", gcloud, "", nil)
	if err == nil {
		t.Fatal("the gcloud sign-in answered no error for a gcloud that prints no token")
	}
	redacttest.RequireNone(t, "agent-secrets.gcloud-sign-in", err.Error())
}

// answering is an http.RoundTripper that answers every request with answer.
type answering func(*http.Request) *http.Response

func (a answering) RoundTrip(r *http.Request) (*http.Response, error) { return a(r), nil }

// refusingSource is a token source whose every Token is an OAuth error answer whose description
// quotes a URL that carries a credential, as Google's token endpoint can answer a refresh.
type refusingSource struct{}

func (refusingSource) Token() (*oauth2.Token, error) {
	return nil, &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"},
		ErrorCode: "invalid_grant", ErrorDescription: redacttest.Words}
}

// TestTheUserinfoReadPrintsNoneOfGooglesWords: the userinfo read --owner me makes
// (gcpBackend.personEmail) prints none of a credential's URL Google's words quote: neither the
// body of a refusal it answers (gcpUserinfo's status line and body, including the bearer it
// echoes), nor the refusal of the token source the read asks for a token through the http.Client
// oauth2.NewClient builds, which comes back inside that client's *url.Error.
func TestTheUserinfoReadPrintsNoneOfGooglesWords(t *testing.T) {
	refused := answering(func(r *http.Request) *http.Response {
		body := redacttest.Words + " the request carried " + r.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}
	})
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: refused})
	for name, source := range map[string]oauth2.TokenSource{
		"a refused userinfo read":  oauth2.StaticTokenSource(&oauth2.Token{AccessToken: redacttest.Token, TokenType: "Bearer"}),
		"a token source's refusal": refusingSource{},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := gcpUserinfo(ctx, &google.Credentials{TokenSource: source})
			if err == nil {
				t.Fatal("gcpUserinfo answered no error")
			}
			redacttest.RequireNone(t, "agent-secrets.userinfo", err.Error())
		})
	}
}
