package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/testport"
)

// unreadableGitHubBases are DISPATCH_GITHUB_API_BASE values credentialURLRefusal cannot read,
// each holding a made-up token: an unencoded "%" or a space in the password (net/url parses
// neither), the scheme dropped (`ghuser:<token>@…`, read as the scheme `ghuser` and an opaque
// rest), and a host read as the scheme (`ghe.invalid:<token>@…`).
func unreadableGitHubBases(host string) map[string]string {
	const token = "madeUpGitHubToken0123"
	at := "@"
	return map[string]string{
		"an unencoded % in the user info": "https://ghuser:" + token + "%zz" + at + host + "/api/v3",
		"a space in the user info":        "https://ghuser:" + token + " x" + at + host + "/api/v3",
		"no scheme":                       "ghuser:" + token + at + host + "/api/v3",
		"a host read as the scheme":       "ghe.invalid:" + token + at + host,
	}
}

// A DISPATCH_GITHUB_API_BASE that is no absolute http(s) URL with a host is refused by the setting,
// its value printed through redact.URL, at boot and in redeliver-webhooks, before either opens the
// database: the GitHub App client builds every request on it and names the request's URL, password
// and all, on a failed call.
func TestGitHubAPIBaseRefusesAValueThatIsNoHTTPURL(t *testing.T) {
	const shape = "(expected an absolute http(s) URL with a host)"
	for name, base := range unreadableGitHubBases("ghe.invalid") {
		t.Run(name, func(t *testing.T) {
			_, err := resolveBootConfig(envGetter(signInEnvironment(map[string]string{"DISPATCH_GITHUB_API_BASE": base})))
			if err == nil || !strings.HasPrefix(err.Error(), `DISPATCH_GITHUB_API_BASE="`) || !strings.Contains(err.Error(), shape) {
				t.Errorf("resolveBootConfig = %v, want the shape refusal of DISPATCH_GITHUB_API_BASE", err)
			} else if strings.Contains(err.Error(), "madeUpGitHub") {
				t.Errorf("the boot refusal %q holds part of the token", err)
			}
			var out bytes.Buffer
			code := redeliverWebhooks(t.Context(), []string{"--since", "1h"}, envGetter(map[string]string{
				"DATABASE_URL":             "postgres://dispatch@127.0.0.1:1/nonexistent",
				"DISPATCH_GITHUB_API_BASE": base,
			}), &out)
			if code != 2 || !strings.HasPrefix(out.String(), `redeliver-webhooks: DISPATCH_GITHUB_API_BASE="`) || !strings.Contains(out.String(), shape) {
				t.Errorf("redeliverWebhooks = %d, %q; want 2 and the shape refusal of DISPATCH_GITHUB_API_BASE", code, out.String())
			} else if strings.Contains(out.String(), "madeUpGitHub") {
				t.Errorf("the redeliver refusal %q holds part of the token", out.String())
			}
		})
	}
}

// No base that boots puts a credential in the error the App installation route's 502 carries
// (`read the GitHub App's installation on <owner>/<repo>: <the client's error>`): of bases holding
// a made-up token in every shape round 10 and round 11 refuse, and clean ones, each that
// gitHubAPIBaseRefusal admits is given to the GitHub App client, whose Installation call to a
// refused port must name none of the token. The clean bases must boot, so the row reads a client
// error for at least them.
func TestNoGitHubAPIBaseThatBootsPutsACredentialInTheAppClientsError(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := &auth.AppConfig{ClientID: "Iv1.base", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
	refused := testport.Refused(t)
	const token = "madeUpGitHubToken0123"
	at := "@"
	bases := unreadableGitHubBases(refused)
	bases["a user and password"] = "http://ghuser:" + token + at + refused + "/api/v3"
	bases["a token as the user"] = "http://" + token + at + refused + "/api/v3"
	bases["a token in the query"] = "http://" + refused + "/api/v3?access_token=" + token
	bases["a token in the fragment"] = "http://" + refused + "/api/v3#" + token
	clean := map[string]string{"a clean base": "http://" + refused, "a clean base with a path": "http://" + refused + "/api/v3"}
	for name, base := range clean {
		bases[name] = base
	}
	for name, base := range bases {
		if gitHubAPIBaseRefusal(base) != nil {
			if _, isClean := clean[name]; isClean {
				t.Errorf("%s (%s) is refused, want it to boot", name, base)
			}
			continue
		}
		client, err := githubapp.New(app, base)
		if err != nil || client == nil {
			t.Fatalf("githubapp.New(%s) = %v, %v", name, client, err)
		}
		_, err = client.Installation(t.Context(), "acme", "widgets")
		if err == nil {
			t.Fatalf("Installation with %s = nil error, want the refused port's", name)
		}
		if strings.Contains(err.Error(), "madeUpGitHub") {
			t.Errorf("%s boots, and the App client's error %q holds part of the token", name, err)
		}
	}
}
