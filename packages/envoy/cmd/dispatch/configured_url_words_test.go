package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// credentialedAddress is a configured address that carries the probe's credentials in each place
// one can sit: user information, a query value and a fragment value, after scheme and before
// host/path. The parameter's name is in two parts so that no line of this file reads as a
// credential to a scanner.
func credentialedAddress(scheme, hostPath string) string {
	return scheme + "://" + redacttest.User + ":" + redacttest.Password + "@" + hostPath +
		"?access" + "_token=" + redacttest.Token + "#access" + "_token=" + redacttest.Token
}

// TestABootRefusalNamesNoCredentialItsAddressCarries: each boot refusal that names a configured
// address - ENVOY_URL that is not an absolute http(s) URL, ENVOY_URL beside the dev sign-in when
// it names another machine, and DISPATCH_AGENT_SECRETS_URL's two of the same - names the setting
// and holds none of the credentials the address carries in its user information, a query value or
// a fragment value. Captures dispatch.configured-url/<case>.
func TestABootRefusalNamesNoCredentialItsAddressCarries(t *testing.T) {
	const tokenFile = "/var/run/secrets/broker/token"
	for _, row := range []struct {
		name, setting string
		env           settingValues
	}{
		{"envoy-shape", "ENVOY_URL", envGetter(headerIdentityEnvironment(map[string]string{
			"ENVOY_URL": credentialedAddress("ftp", "listener.example/path"),
		}))},
		{"envoy-dev-fence", "ENVOY_URL", devSignInEnvironment(map[string]string{
			"ENVOY_URL": credentialedAddress("http", "listener.example:9020/path"),
		})},
		{"agent-secrets-shape", "DISPATCH_AGENT_SECRETS_URL", envGetter(headerIdentityEnvironment(map[string]string{
			"DISPATCH_AGENT_SECRETS_URL":                credentialedAddress("https", "broker.example/v1"),
			"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": tokenFile,
		}))},
		{"agent-secrets-dev-fence", "DISPATCH_AGENT_SECRETS_URL", devSignInEnvironment(map[string]string{
			"DISPATCH_AGENT_SECRETS_URL":                credentialedAddress("https", "broker.example"),
			"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": tokenFile,
		})},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := resolveBootConfig(row.env)
			if err == nil {
				t.Fatalf("boot accepted a %s it refuses", row.setting)
			}
			redacttest.RequireNone(t, "dispatch.configured-url/"+row.name, err.Error())
			if !strings.Contains(err.Error(), row.setting+"=") {
				t.Errorf("the refusal %q does not name %s", err, row.setting)
			}
		})
	}
}
