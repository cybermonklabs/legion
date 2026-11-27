package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity/directorytest"
)

// The signer, the delegated user, the two logins and the probe scope of the directory-check tests.
const (
	directoryCheckSigner = "dispatch-directory@example-project.iam.gserviceaccount.com"
	directoryCheckAdmin  = "directory-reader@example.com"
	directoryCheckActive = "ada@example.com"
	directoryCheckHeld   = "leaver@example.com"
	directoryCheckProbe  = "https://www.googleapis.com/auth/admin.directory.user"
)

// directoryCheckArgs are the command's arguments for the two logins and the probe.
var directoryCheckArgs = []string{"--active", directoryCheckActive, "--held", directoryCheckHeld, "--scope-probe", directoryCheckProbe}

// directoryCheckSettings are the two settings, with overrides.
func directoryCheckSettings(overrides map[string]string) settingValues {
	values := map[string]string{"DISPATCH_DIRECTORY_ACCOUNT": directoryCheckSigner, "DISPATCH_DIRECTORY_ADMIN": directoryCheckAdmin}
	for name, value := range overrides {
		values[name] = value
	}
	return envGetter(values)
}

// directoryCheckGoogle is the fake Google of a healthy deployment: ada's account is active, the
// leaver's is gone (404), and the grant gives the Directory's read scope alone.
func directoryCheckGoogle(t *testing.T) *directorytest.Google {
	t.Helper()
	google := directorytest.ServeGoogle(t)
	google.SetUser(directoryCheckActive, directorytest.User{ID: "u-ada", PrimaryEmail: directoryCheckActive})
	return google
}

// runDirectoryCheck runs the command in process against google, with env's settings.
func runDirectoryCheck(google *directorytest.Google, env settingValues, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = directoryCheck(context.Background(), args, env, &out, &errOut, google.ClientOptions()...)
	return code, out.String(), errOut.String()
}

// TestDirectoryCheckExitsOnlyOnTheExpectedVerdicts: `envoy-dispatch directory-check` exits 0 only
// when the active login is Active, the held login is Held and Google refuses the probe scope with
// unauthorized_client, and prints one line for each; any other verdict exits 1, and arguments or
// settings it cannot take exit 2. Nothing Google wrote is printed but an OAuth error code.
func TestDirectoryCheckExitsOnlyOnTheExpectedVerdicts(t *testing.T) {
	refusedProbe := "scope " + directoryCheckProbe + ": refused (unauthorized_client)\n"
	rows := []struct {
		name   string
		answer func(*directorytest.Google)
		code   int
		stdout string
	}{
		{name: "Active, Held and a probe refused with unauthorized_client", code: 0,
			stdout: directoryCheckActive + ": Active\n" + directoryCheckHeld + ": Held\n" + refusedProbe},
		{name: "a suspended held login is Held too", code: 0, answer: func(g *directorytest.Google) {
			g.SetUser(directoryCheckHeld, directorytest.User{ID: "u-leaver", PrimaryEmail: directoryCheckHeld, Suspended: true})
		}, stdout: directoryCheckActive + ": Active\n" + directoryCheckHeld + ": Held\n" + refusedProbe},
		{name: "an archived held login, not suspended, is Held too", code: 0, answer: func(g *directorytest.Google) {
			g.SetUser(directoryCheckHeld, directorytest.User{ID: "u-leaver", PrimaryEmail: directoryCheckHeld, Archived: true})
		}, stdout: directoryCheckActive + ": Active\n" + directoryCheckHeld + ": Held\n" + refusedProbe},
		{name: "the active login Held", code: 1, answer: func(g *directorytest.Google) {
			g.SetUser(directoryCheckActive, directorytest.User{ID: "u-ada", PrimaryEmail: directoryCheckActive, Suspended: true})
		}, stdout: directoryCheckActive + ": Held\n" + directoryCheckHeld + ": Held\n" + refusedProbe},
		{name: "the active login answered for another account", code: 1, answer: func(g *directorytest.Google) {
			g.SetUser(directoryCheckActive, directorytest.User{ID: "u-bob", PrimaryEmail: "bob@example.com"})
		}, stdout: directoryCheckActive + ": Held\n" + directoryCheckHeld + ": Held\n" + refusedProbe},
		{name: "the active login Failed", code: 1, answer: func(g *directorytest.Google) {
			g.AnswerUser(directoryCheckActive, directorytest.Answer{Status: http.StatusInternalServerError, Body: `{"error":{"message":"` + directorytest.Planted + `"}}`})
		}, stdout: directoryCheckActive + ": Failed (the Directory read answered an error (HTTP 500))\n" + directoryCheckHeld + ": Held\n" + refusedProbe},
		{name: "the held login Active", code: 1, answer: func(g *directorytest.Google) {
			g.SetUser(directoryCheckHeld, directorytest.User{ID: "u-leaver", PrimaryEmail: directoryCheckHeld})
		}, stdout: directoryCheckActive + ": Active\n" + directoryCheckHeld + ": Active\n" + refusedProbe},
		{name: "the held login Failed", code: 1, answer: func(g *directorytest.Google) {
			g.AnswerUser(directoryCheckHeld, directorytest.Answer{Status: http.StatusTooManyRequests, Body: `{"error":{"message":"` + directorytest.Planted + `"}}`})
		}, stdout: directoryCheckActive + ": Active\n" + directoryCheckHeld + ": Failed (the Directory read answered an error (HTTP 429))\n" + refusedProbe},
		{name: "the probe granted", code: 1, answer: func(g *directorytest.Google) {
			g.Grant(directorytest.DirectoryScope, directoryCheckProbe)
		}, stdout: directoryCheckActive + ": Active\n" + directoryCheckHeld + ": Held\nscope " + directoryCheckProbe + ": granted\n"},
		{name: "the probe refused with another code", code: 1, answer: func(g *directorytest.Google) {
			g.AnswerScope(directoryCheckProbe, &directorytest.Answer{Status: http.StatusBadRequest, Body: `{"error":"invalid_scope","error_description":"` + directorytest.Planted + `"}`})
		}, stdout: directoryCheckActive + ": Active\n" + directoryCheckHeld + ": Held\nscope " + directoryCheckProbe + ": refused (invalid_scope)\n"},
		{name: "the probe refused with no OAuth error code", code: 1, answer: func(g *directorytest.Google) {
			g.AnswerScope(directoryCheckProbe, &directorytest.Answer{Status: http.StatusInternalServerError, Body: `{"error":"` + directorytest.Planted + ` here"}`})
		}, stdout: directoryCheckActive + ": Active\n" + directoryCheckHeld + ": Held\nscope " + directoryCheckProbe + ": Failed (the token exchange answered an error (HTTP 500))\n"},
		{name: "signJwt refused", code: 1, answer: func(g *directorytest.Google) {
			g.AnswerSigner(&directorytest.Answer{Status: http.StatusForbidden, Body: `{"error":{"message":"` + directorytest.Planted + `"}}`})
		}, stdout: directoryCheckActive + ": Failed (signJwt answered an error (HTTP 403))\n" + directoryCheckHeld + ": Failed (signJwt answered an error (HTTP 403))\n" +
			"scope " + directoryCheckProbe + ": Failed (signJwt answered an error (HTTP 403))\n"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			google := directoryCheckGoogle(t)
			if row.answer != nil {
				row.answer(google)
			}
			code, stdout, stderr := runDirectoryCheck(google, directoryCheckSettings(nil), directoryCheckArgs...)
			if code != row.code || stdout != row.stdout {
				t.Errorf("exit %d, stdout:\n%s\nwant exit %d, stdout:\n%s\nstderr: %s", code, stdout, row.code, row.stdout, stderr)
			}
			if strings.Contains(stdout+stderr, directorytest.Planted) {
				t.Errorf("the output prints Google's words:\n%s%s", stdout, stderr)
			}
			// The probe asks with the same signer and delegated user as the checks, for its scope.
			for _, sign := range google.Signs() {
				if sign.Principal != directoryCheckSigner || sign.Subject != directoryCheckAdmin ||
					(sign.Scope != directorytest.DirectoryScope && sign.Scope != directoryCheckProbe) {
					t.Errorf("signJwt %+v, want the signer and the delegated user, for the Directory scope or the probe's", sign)
				}
			}
		})
	}

	t.Run("arguments and settings it cannot take exit 2", func(t *testing.T) {
		google := directoryCheckGoogle(t)
		for name, args := range map[string][]string{
			"no --held":          {"--active", directoryCheckActive, "--scope-probe", directoryCheckProbe},
			"no --active":        {"--held", directoryCheckHeld, "--scope-probe", directoryCheckProbe},
			"no --scope-probe":   {"--active", directoryCheckActive, "--held", directoryCheckHeld},
			"a blank login":      {"--active", " ", "--held", directoryCheckHeld, "--scope-probe", directoryCheckProbe},
			"an argument":        append(append([]string{}, directoryCheckArgs...), "extra"),
			"an unknown flag":    append([]string{"--known-id", "u-ada"}, directoryCheckArgs...),
			"no argument at all": nil,
		} {
			if code, stdout, stderr := runDirectoryCheck(google, directoryCheckSettings(nil), args...); code != 2 || stdout != "" {
				t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 2 and nothing on stdout", name, code, stdout, stderr)
			}
		}
		for _, setting := range []string{"DISPATCH_DIRECTORY_ACCOUNT", "DISPATCH_DIRECTORY_ADMIN"} {
			code, stdout, stderr := runDirectoryCheck(google, directoryCheckSettings(map[string]string{setting: " "}), directoryCheckArgs...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, setting) {
				t.Errorf("%s blank: exit %d, stdout %q, stderr %q; want exit 2 naming it", setting, code, stdout, stderr)
			}
		}
		if signs := google.Signs(); len(signs) != 0 {
			t.Errorf("a refused run asked Google to sign %d JWTs", len(signs))
		}
	})
}

// directoryCheckReaches runs directory-check with env's settings and answers the signJwt requests
// it sent: what reached the reader through the settings table.
func directoryCheckReaches(t *testing.T, env map[string]string) []directorytest.Sign {
	t.Helper()
	google := directoryCheckGoogle(t)
	if code, stdout, stderr := runDirectoryCheck(google, envGetter(env), directoryCheckArgs...); code != 0 {
		t.Fatalf("directory-check: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	signs := google.Signs()
	if len(signs) == 0 {
		t.Fatal("directory-check asked Google to sign nothing")
	}
	return signs
}
