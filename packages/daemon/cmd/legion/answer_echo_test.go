package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
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
// default owner, an owner's name, the App. Neither what the start prints on stderr nor its error
// holds the secret, and the error names the member: a parser's or a validator's error quotes the
// value it refused, so the command prints a sentence of its own, and the App is printed only
// where it is one of the App roles. A valid answer starts, naming the review App.
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
		{"app", "(app)", func(answer map[string]any, secret string) { answer["app"] = secret }},
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
// cannot take - a time that is not one, a live repository that is no `owner/name` - and the
// error names the member and holds none of the value: encoding/json's errors, time.Time's and
// ghrepo.Parse's quote it, so the state decodes through oauth2call.Answer.Decode and the live
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
		return `{"repositories":{"owners":[],"live":[],"refused":[],"withoutWebhookApp":[],"caches":[]` + inner + `}}`
	}
	for _, tc := range []struct {
		name, body, names string
		read              func(t *testing.T, daemon *httptest.Server) string
	}{
		{
			name: "repositories-time", names: `"repositories.caches.(item).retainedUntil"`,
			body: repositories(`,"caches":[{"repo":"acme/widgets","sandbox":"s","state":"ready","ready":true,"retainedUntil":"` + echoProbe + `"}]`),
			read: readRepositories,
		},
		{
			name: "repositories-live", names: "repositories.live",
			body: repositories(`,"live":[{"repo":"acme/widgets","default":true},{"repo":"` + echoProbe + `","default":false}]`),
			read: readRepositories,
		},
		{
			name: "legion-state", names: `"daemon.startedAt"`,
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

// TestADesignGateStepPrintsNoBearerItEchoes: the daemon's 200 to the controller's secret, or a
// stand-in's, names a design gate step that is the operator bearer the request carried; the
// refusal names the step by its place in the list and holds none of the bearer.
func TestADesignGateStepPrintsNoBearerItEchoes(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"secret": "s", "designGateSteps": []string{"decisions", bearerOf(r)}})
	}))
	t.Cleanup(daemon.Close)
	_, _, err := fetchControllerSecret(context.Background(), daemon.URL, echoProbe, api.DaemonAPIVersion())
	if err == nil {
		t.Fatal("the secret fetch took a design gate step this legion does not know")
	}
	if strings.Contains(err.Error(), echoProbe) {
		t.Errorf("the secret fetch printed the bearer its answer echoed: %v", err)
	}
	if !strings.Contains(err.Error(), "designGateSteps[1]") {
		t.Errorf("the secret fetch = %v, want it to name designGateSteps[1]", err)
	}
}

// claimsEcho is a stand-in daemon for the operator's claim routes: the list answers list of the
// bearer the request carried, and a retry is refused 409, or answered 200 with retried where it is
// not "".
func claimsEcho(t *testing.T, list func(bearer string) string, retried string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == claimsRoute:
			_, _ = io.WriteString(w, list(bearerOf(r)))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/retry") && retried != "":
			_, _ = io.WriteString(w, retried)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/retry"):
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"the claim is not failed"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestAClaimsAnswerPrintsNoBearerItEchoes: the daemon's answers to `legion claims`, or a
// stand-in's, echo the operator bearer the request carried - as a member the claims list does not
// have, as a time, as a failed claim's token, as the state of the claim a suspension waits on -
// and no line the command prints holds it. The list decodes through decodeAnswer, whose error
// names the member of its own type and never the answer's, and a token or a state prints only in
// the shape the daemon makes one (claimWord, stateWord). A failed claim's token is also the path
// its retry asks, which the line of a retry answered with a body that does not read prints: a
// token in no shape the daemon makes one in is not asked for (legionclaim.Token.Shaped), and a
// token in such a shape still is, the line naming its path.
func TestAClaimsAnswerPrintsNoBearerItEchoes(t *testing.T) {
	tokenFile := writeFile(t, "operator-token", echoProbe+"\n")
	answered := func(t *testing.T, list func(bearer string) string, retried string, args ...string) string {
		t.Helper()
		port := portOf(t, claimsEcho(t, list, retried))
		var out, errb bytes.Buffer
		argv := append(append([]string{"legion", "claims"}, args...), "--port", port, "--operator-token-file", tokenFile)
		if code := run(context.Background(), argv, &out, &errb); code != 1 {
			t.Errorf("legion claims %s = %d, stderr %q; want 1", args[0], code, errb.String())
		}
		return out.String() + errb.String()
	}
	claims := func(t *testing.T, list func(bearer string) string, args ...string) string {
		t.Helper()
		return answered(t, list, "", args...)
	}
	failed := func(token string) string { return `{"claims":[{"token":"` + token + `","state":"failed"}]}` }
	suspend := func(t *testing.T, wait string, seen seenClaim) string {
		t.Helper()
		port, _ := heldSuspendDaemon(t, heldWorking, seen)
		var out, errb bytes.Buffer
		argv := []string{"legion", "claims", "suspend", "--port", port, "--operator-token-file", tokenFile, "--claim", string(architectClaim), "--wait", wait}
		if code := run(context.Background(), argv, &out, &errb); code != 1 {
			t.Errorf("legion claims suspend = %d, stderr %q; want 1", code, errb.String())
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
		{"retry-failed-token", "was not retried", func(t *testing.T) string {
			return claims(t, func(bearer string) string { return `{"claims":[{"token":"` + bearer + `","state":"failed"}]}` }, "retry", "--failed")
		}},
		{"retry-failed-token-unread", "was not retried", func(t *testing.T) string {
			return answered(t, failed, "not a claim", "retry", "--failed")
		}},
		{"retry-failed-shaped-unread", "/" + string(architectClaim) + "/retry served", func(t *testing.T) string {
			return answered(t, func(string) string { return failed(string(architectClaim)) }, "not a claim", "retry", "--failed")
		}},
		{"suspend-dropped-state", "no longer holds the suspension of " + string(architectClaim), func(t *testing.T) string {
			return suspend(t, "1h", seenClaim{echoProbe, false})
		}},
		{"suspend-held-state", "after the --wait bound of 1s", func(t *testing.T) string {
			return suspend(t, "1s", seenClaim{echoProbe, true})
		}},
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

// TestACredentialRefusalIsDecidedOnItsCodeAsItCame: the daemon refuses the controller's
// credential fetch with a code the command decides on - CONTROLLER_LAUNCHED ends the refresh
// (controllerCredentialSuperseded), GITHUB_TOKEN_SOURCE_UNAVAILABLE is a daemon with no App, on
// which the start goes on acting as nobody - while the capability secret the request carried
// shares a run of eight or more characters with that code, so the rule's reading of the code
// (SendBack) is not the code. The decision is the same as with a secret that shares none: it is
// made on the code as it came.
func TestACredentialRefusalIsDecidedOnItsCodeAsItCame(t *testing.T) {
	refusing := func(t *testing.T, status int, code string) *httptest.Server {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"code":"`+code+`","error":"the daemon refuses the credential"}`)
		}))
		t.Cleanup(server.Close)
		return server
	}
	for _, secret := range []struct{ name, superseded, noApp string }{
		{"control", echoProbe, echoProbe},
		{"shares-a-run", "ghs_CONTROLLER_LAUNCHED0123456789abcdefg", "ghs_GITHUB_TOKEN_SOURCE_UNAVAILABLE01234"},
	} {
		t.Run("superseded/"+secret.name, func(t *testing.T) {
			_, _, err := fetchControllerCredential(context.Background(), refusing(t, http.StatusConflict, "CONTROLLER_LAUNCHED").URL, secret.superseded, "acme")
			if !controllerCredentialSuperseded(err) {
				t.Errorf("the refresh refused CONTROLLER_LAUNCHED = %v, want the refresh ended", err)
			}
		})
		t.Run("no-app/"+secret.name, func(t *testing.T) {
			var stderr bytes.Buffer
			stop, _, _, err := controllerStartGitHubCredential(context.Background(), refusing(t, http.StatusServiceUnavailable, "GITHUB_TOKEN_SOURCE_UNAVAILABLE").URL, secret.noApp, t.TempDir(), &stderr)
			stop()
			if err != nil || !strings.Contains(stderr.String(), "the controller's gh acts as nobody") {
				t.Errorf("the start refused GITHUB_TOKEN_SOURCE_UNAVAILABLE = %v, stderr %q; want it acting as nobody", err, stderr.String())
			}
		})
	}
}

// echoForkRead is the path of a file of the test's own holding the fork read's capture name with
// every old replaced by with, which must change it.
func echoForkRead(t *testing.T, name, old, with string) string {
	t.Helper()
	data, err := os.ReadFile(forkReadFixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s holds no %q to change", name, old)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), old, with)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAForkReadAnswerPrintsNoTokenItEchoes: GitHub's answer to the credential helper's fork read,
// or a stand-in's, echoes the token the read carried into what the refusal names - a refused
// read's message, a redirect's Location, the repository's full_name, the parent's and source's
// full names - and the refusal holds none of it, naming each value it cannot print by what it is.
// The read carries no record of its token (it is curl's), so a value prints only in a shape that
// holds no credential: plain words, a redirect to a repository by its id, `owner/name` as GitHub
// names one.
func TestAForkReadAnswerPrintsNoTokenItEchoes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gh")
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{Default: "acme", Dirs: map[string]ghconfig.Rendered{
		"acme": ghconfig.Render(echoProbe, "implement", time.Now().Add(time.Hour)),
	}}); err != nil {
		t.Fatal(err)
	}
	const slug = "outside-author/sample-project"
	for _, tc := range []struct {
		name, names string
		answer      func(t *testing.T) string
	}{
		{"message", "the status 301", func(t *testing.T) string {
			return echoForkRead(t, "moved.http", `"message": "Moved Permanently"`, `"message": "Moved Permanently, see `+echoProbe+`"`)
		}},
		// The echo is in the Location's path as well as its query: stderr's URL rule (newRoot's
		// redact.Writer) drops a URL's query, so an echo there alone would print nothing even
		// where the helper printed every Location.
		{"location", "a redirect the helper does not follow", func(t *testing.T) string {
			return echoForkRead(t, "moved.http", "location: https://api.github.com/repositories/1146927990", "location: https://api.github.com/repositories/1146927990/"+echoProbe+"?access_token="+echoProbe)
		}},
		{"full-name", "GitHub answered the read of " + slug, func(t *testing.T) string {
			return echoForkRead(t, "fork-of-a-served-repository.http", `"full_name": "`+slug+`"`, `"full_name": "`+echoProbe+`"`)
		}},
		{"upstream-names", "neither repository it comes from routes to a directory of the bundle", func(t *testing.T) string {
			return echoForkRead(t, "fork-of-a-served-repository.http", `"full_name": "acme/sample-project"`, `"full_name": "`+echoProbe+`"`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newFakeGitHub(t, echoProbe, printsAnswer(tc.answer(t), 0))
			useForkRead(t, github.program, forkReadBound)
			forkReadPane(t, root, ghconfig.Dir(root, "acme"))
			code, stdout, stderr := runGitCredentialHere(t, forkRequest(slug), "get")
			answersNothingForTheFork(t, code, stdout, stderr, root, slug, tc.names)
		})
	}
}
