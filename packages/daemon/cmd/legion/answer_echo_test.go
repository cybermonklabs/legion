package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// echoProbe is the credential these tests hand a request and have a stand-in echo back into its
// answer: a GitHub-shaped token, `ghs_` and 36 characters, in two parts so no line of this file
// reads as a credential to a scanner.
const echoProbe = "ghs" + "_0123456789abcdefghijABCDEFGHIJ012345"

// portOf is the port of a stand-in's URL, for a command's --port.
func portOf(t *testing.T, server *httptest.Server) string {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Port()
}

// bearerOf is the bearer a stand-in's request carried.
func bearerOf(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// credentialEcho is a stand-in daemon whose github-credential route answers a 200 of the
// contract's shape, a valid gh bundle for acme, which echo changes with the secret the request
// carried.
func credentialEcho(t *testing.T, echo func(answer map[string]any, secret string)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var asked api.ControllerCredentialRequest
		if err := json.NewDecoder(r.Body).Decode(&asked); err != nil || r.URL.Path != "/legion/v1/controller/github-credential" {
			http.NotFound(w, r)
			return
		}
		files := map[string]string{"hosts": "github.com:\n    oauth_token: ghs_acme_read\n", "config": "version: \"1\"\n"}
		answer := map[string]any{
			"owners": map[string]any{"acme": files}, "default": "acme", "app": "review",
			"expiresAt": "2026-11-21T17:00:00Z", "installed": []string{"acme"},
			"renames": []map[string]string{{"from": "acme/old", "to": "acme/new"}},
		}
		echo(answer, asked.Secret)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestAControllerCredentialThatDoesNotParsePrintsNoSecretItEchoes: the daemon's 200 to `legion
// controller start`'s credential fetch, or a stand-in's, echoes the capability secret the request
// carried into a member the command parses or checks: the expiry, a rename's either side, the
// default owner, an owner's name. Neither what the start prints on stderr nor its error holds the
// secret, and the error names the member: a parser's or a validator's error quotes the value it
// refused, so the command prints a sentence of its own. A valid answer starts, naming the review
// App.
func TestAControllerCredentialThatDoesNotParsePrintsNoSecretItEchoes(t *testing.T) {
	for _, tc := range []struct {
		name, names string
		echo        func(answer map[string]any, secret string)
	}{
		{"valid", "", func(map[string]any, string) {}},
		{"expiresAt", "(expiresAt)", func(answer map[string]any, secret string) { answer["expiresAt"] = secret }},
		{"renames-from", "(renames[0].from)", func(answer map[string]any, secret string) {
			answer["renames"] = []map[string]string{{"from": secret, "to": "acme/new"}}
		}},
		{"renames-to", "(renames[0].to)", func(answer map[string]any, secret string) {
			answer["renames"] = []map[string]string{{"from": "acme/old", "to": secret}}
		}},
		{"default", "(default, owners)", func(answer map[string]any, secret string) { answer["default"] = secret }},
		{"owners", "(default, owners)", func(answer map[string]any, secret string) {
			answer["owners"].(map[string]any)[secret] = answer["owners"].(map[string]any)["acme"]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := credentialEcho(t, tc.echo)
			var stderr bytes.Buffer
			stop, _, _, err := controllerStartGitHubCredential(context.Background(), daemon.URL, echoProbe, t.TempDir(), &stderr)
			stop()
			printed := stderr.String()
			if err != nil {
				printed += "\n" + err.Error()
			}
			if strings.Contains(printed, echoProbe) {
				t.Errorf("the credential fetch printed the secret its answer echoed:\n%s", printed)
			}
			if tc.names == "" {
				if err != nil || !strings.Contains(printed, "acts as the review App for owners acme") {
					t.Fatalf("the valid answer = %v, stderr %q; want the start naming the review App", err, stderr.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the credential fetch = %v, want an error naming %s", err, tc.names)
			}
		})
	}
}

// TestADaemonStateThatDoesNotDecodePrintsNoValueItEchoes: the daemon's state, read by `legion
// controller start` for the repository mapping and by `legion state`, holds a value its reader
// cannot take - a time that is not one, a mapped repository that is no `owner/name` - and the
// error names the member and holds none of the value: encoding/json's errors, time.Time's and
// ghrepo.Parse's quote it, so the state decodes through oauth2call.Answer.Decode and the mapped
// repository is named by its place.
func TestADaemonStateThatDoesNotDecodePrintsNoValueItEchoes(t *testing.T) {
	stateServing := func(t *testing.T, body string) *httptest.Server {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/legion/v1/state" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(server.Close)
		return server
	}
	repositories := func(inner string) string {
		return `{"repositories":{"owners":[],"mapped":[],"refused":[],"withoutWebhookApp":[],"caches":[]` + inner + `}}`
	}
	for _, tc := range []struct {
		name, body, names string
		read              func(t *testing.T, daemon *httptest.Server) string
	}{
		{
			name: "repositories-time", names: `"repositories"`,
			body: repositories(`,"caches":[{"repo":"acme/widgets","sandbox":"s","state":"ready","ready":true,"retainedUntil":"` + echoProbe + `"}]`),
			read: readRepositories,
		},
		{
			name: "repositories-mapped", names: "repositories.mapped",
			body: repositories(`,"mapped":[{"repo":"acme/widgets","default":true},{"repo":"` + echoProbe + `","default":false}]`),
			read: readRepositories,
		},
		{
			name: "legion-state", names: `"daemon"`,
			body: `{"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"` + echoProbe + `"},` +
				`"admission":{"cap":2,"active":[],"waiting":[]},"issues":{},"pendingStatusWrites":[]}`,
			read: func(t *testing.T, daemon *httptest.Server) string {
				t.Chdir(t.TempDir())
				t.Setenv("LEGION_DAEMON_URL", daemon.URL)
				t.Setenv("LEGION_ISSUE", "")
				var out, errb bytes.Buffer
				if code := run(context.Background(), []string{"legion", "state"}, &out, &errb); code != 1 {
					t.Errorf("legion state over a state that does not decode = %d, want 1", code)
				}
				return out.String() + errb.String()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			printed := tc.read(t, stateServing(t, tc.body))
			if strings.Contains(printed, echoProbe) {
				t.Errorf("the state's reader printed the value it could not read:\n%s", printed)
			}
			if !strings.Contains(printed, tc.names) {
				t.Errorf("the state's reader printed %q, want it to name %s", printed, tc.names)
			}
		})
	}
}

// readRepositories is controller start's read of the repository mapping, as the error it prints.
func readRepositories(t *testing.T, daemon *httptest.Server) string {
	t.Helper()
	err := checkControllerRepositories(context.Background(), daemon.URL)
	if err == nil {
		t.Fatal("the repository read took a state it cannot read")
	}
	return err.Error()
}

// claimsEcho is a stand-in daemon for the operator's claim routes: the list answers list of the
// bearer the request carried.
func claimsEcho(t *testing.T, list func(bearer string) string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == claimsRoute:
			_, _ = io.WriteString(w, list(bearerOf(r)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestAClaimsAnswerPrintsNoBearerItEchoes: the daemon's answers to `legion claims`, or a
// stand-in's, echo the operator bearer the request carried - as a member the claims list does not
// have, as a time - and no line the command prints holds it. The list decodes through
// decodeAnswer, whose error names the member of its own type and never the answer's.
func TestAClaimsAnswerPrintsNoBearerItEchoes(t *testing.T) {
	tokenFile := writeFile(t, "operator-token", echoProbe+"\n")
	claims := func(t *testing.T, list func(bearer string) string, args ...string) string {
		t.Helper()
		port := portOf(t, claimsEcho(t, list))
		var out, errb bytes.Buffer
		argv := append(append([]string{"legion", "claims"}, args...), "--port", port, "--operator-token-file", tokenFile)
		if code := run(context.Background(), argv, &out, &errb); code != 1 {
			t.Errorf("legion claims %s = %d, stderr %q; want 1", args[0], code, errb.String())
		}
		return out.String() + errb.String()
	}
	member := func(bearer string) string { return `{"claims":[],"` + bearer + `":true}` }
	for _, tc := range []struct {
		name, names string
		printed     func(t *testing.T) string
	}{
		{"list-member", "a member the api.OperatorClaims", func(t *testing.T) string { return claims(t, member, "list") }},
		{"list-time", "api.OperatorClaims", func(t *testing.T) string {
			return claims(t, func(bearer string) string { return `{"claims":[{"token":"x","lastTurnStart":"` + bearer + `"}]}` }, "list")
		}},
		{"retry-failed-member", "a member the api.OperatorClaims", func(t *testing.T) string { return claims(t, member, "retry", "--failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			printed := tc.printed(t)
			if strings.Contains(printed, echoProbe) {
				t.Errorf("legion claims printed the bearer its answer echoed:\n%s", printed)
			}
			if !strings.Contains(printed, tc.names) {
				t.Errorf("legion claims printed %q, want it to name %q", printed, tc.names)
			}
		})
	}
}

// TestAGrantAnswerThatDoesNotDecodePrintsNoBearerItEchoes: the daemon's 200 to the grant `legion
// issue status` mints, or a stand-in's, is no grant and echoes the operator bearer the request
// carried - its Authorization header beside no grantId (the shape of the round-5 review's probe),
// or beside a grantId that is no string - and the command names the member it reads, grantId,
// and prints none of the answer. A grant is the control: the status is set and nothing refused.
func TestAGrantAnswerThatDoesNotDecodePrintsNoBearerItEchoes(t *testing.T) {
	tokenFile := writeFile(t, "operator-token", echoProbe+"\n")
	for _, tc := range []struct {
		name  string
		grant func(r *http.Request) string
	}{
		{"grant", func(*http.Request) string { return `{"grantId":"g-1"}` }},
		{"echoed-authorization", func(r *http.Request) string {
			return `{"headers":{"Authorization":"` + r.Header.Get("Authorization") + `"}}`
		}},
		{"grant-id-no-string", func(r *http.Request) string { return `{"grantId":7,"echo":"` + bearerOf(r) + `"}` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/legion/v1/grants":
					_, _ = io.WriteString(w, tc.grant(r))
				case "/legion/v1/issues/status":
					_, _ = io.WriteString(w, `{}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(daemon.Close)
			code, out, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile, "--port", portOf(t, daemon))
			if strings.Contains(out+errb, echoProbe) {
				t.Errorf("legion issue status printed the bearer its answer echoed: stdout %q, stderr %q", out, errb)
			}
			if tc.name == "grant" {
				if code != 0 || errb != "" {
					t.Fatalf("legion issue status over a grant = %d, stderr %q; want 0", code, errb)
				}
				return
			}
			if code != 1 || out != "" || !strings.Contains(errb, "grantId") {
				t.Errorf("legion issue status over an answer that is no grant = %d, stdout %q, stderr %q; want 1, nothing printed and grantId named", code, out, errb)
			}
		})
	}
}
