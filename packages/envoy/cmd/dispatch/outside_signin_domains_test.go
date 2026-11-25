package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// Boot refuses DISPATCH_SIGNIN_WORKSPACE_DOMAINS without the sign-in domain it lists the
// Workspace's other domains beside, beside header identity with the other sign-in settings, and
// with an entry that is not a lowercase domain name IDNA lookup takes (an upper-case letter, or a
// label such as xn--a that decodes to nothing), naming the entry's position and never its value.
// It takes a list of domains, each trimmed, an internationalized one written in Unicode or as its
// A-labels.
func TestOutsideSignInWorkspaceDomainsSettingRefusals(t *testing.T) {
	const name = "DISPATCH_SIGNIN_WORKSPACE_DOMAINS"
	google := func(domains string) map[string]string {
		return signInEnvironment(map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "example.com", name: domains})
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no sign-in at all", noSignInEnvironment(map[string]string{name: "second.example"}), name + " needs DISPATCH_SIGNIN_DOMAIN"},
		{"a Cognito group", signInEnvironment(map[string]string{name: "second.example"}), name + " needs DISPATCH_SIGNIN_DOMAIN"},
		{"header identity", headerIdentityEnvironment(map[string]string{name: "second.example"}), name + " must be unset because header identity"},
	} {
		t.Run("beside "+tc.name, func(t *testing.T) {
			if _, err := resolveBootConfig(envGetter(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolveBootConfig: err = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
	for value, want := range map[string]string{
		"Second.example":                 "entry 1 of 1 must be a lowercase domain name such as example.com; got a value of 14 characters",
		"Bücher.example":                 "entry 1 of 1 must be a lowercase domain name such as example.com; got a value of 14 characters",
		"second.example,xn--a.example":   "entry 2 of 2 must be a lowercase domain name such as example.com; got a value of 13 characters",
		"second.example,https://x.test":  "entry 2 of 2 must be a lowercase domain name such as example.com; got a value of 14 characters",
		"second.example,bob@x.test":      "entry 2 of 2 must be a lowercase domain name such as example.com; got a value of 10 characters",
		"second.example,third":           "entry 2 of 2 must be a lowercase domain name such as example.com; got a value of 5 characters",
		"second.example,,third.example":  "entry 2 of 3 is empty",
		"second.example,":                "entry 2 of 2 is empty",
		strings.Repeat("a", 64) + ".com": "entry 1 of 1 must be a lowercase domain name such as example.com; got a value of 68 characters",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := resolveBootConfig(envGetter(google(value))); err == nil || err.Error() != name+" "+want {
				t.Fatalf("resolveBootConfig: err = %v, want %q", err, name+" "+want)
			}
		})
	}
	for value, want := range map[string][]string{
		"second.example":                       {"second.example"},
		" second.example , eu.third.example ":  {"second.example", "eu.third.example"},
		"bücher.example,xn--bcher-kva.example": {"bücher.example", "xn--bcher-kva.example"},
		"":                                     nil,
	} {
		if boot := resolveWith(t, google(value)); !slices.Equal(boot.SignInWorkspaceDomains, want) {
			t.Errorf("%s=%q: SignInWorkspaceDomains = %q, want %q", name, value, boot.SignInWorkspaceDomains, want)
		}
	}
}

// workspaceDomainsReachTheirReader hands DISPATCH_SIGNIN_WORKSPACE_DOMAINS, through the settings
// read alone, to boot, which holds each entry trimmed, and to the rule passkey sign-in builds from
// boot (signInRule), which reads each entry as its A-label (workspaceDomainALabels): an address on a
// listed domain, written either way, or on a subdomain of one, is a Workspace address, which the
// rule refuses without reading the lists, and an address on no listed domain is left to the lists.
// Boot refuses the setting without the sign-in domain.
//
// Control: hand the rule the entries as written (no workspaceDomainALabels), and
// bob@xn--bcher-kva.example is no Workspace address and the reader fails naming it.
func workspaceDomainsReachTheirReader(t *testing.T) {
	boot := resolveWith(t, map[string]string{
		"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "example.com",
		"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": " second.example , bücher.example ",
		"DISPATCH_SIGNIN_PASSKEY":           "1", "DISPATCH_OUTSIDE_ORIGIN": "https://outside.example.com",
		"DISPATCH_TRUSTED_PROXY_RANGES": googleProxyRanges,
	})
	if !slices.Equal(boot.SignInWorkspaceDomains, []string{"second.example", "bücher.example"}) {
		t.Errorf("SignInWorkspaceDomains = %q, want [second.example bücher.example]", boot.SignInWorkspaceDomains)
	}
	rule, err := signInRule(boot, identity.ListerFunc(func(context.Context, string) (bool, error) { return true, nil }))
	passkey, ok := rule.(identity.PasskeyRule)
	if err != nil || !ok {
		t.Fatalf("signInRule with passkey sign-in = %T, %v; want an identity.PasskeyRule", rule, err)
	}
	for _, address := range []string{"bob@bücher.example", "bob@xn--bcher-kva.example", "bob@eu.second.example", "bob@example.com"} {
		if !passkey.WorkspaceAddress(address) {
			t.Errorf("the rule boot built reads %s as no Workspace address, want one", address)
		}
		if err := passkey.AdmitsRestricted(context.Background(), address); !errors.Is(err, identity.ErrWorkspaceAddress) {
			t.Errorf("the rule boot built admits %s as %v, want ErrWorkspaceAddress", address, err)
		}
	}
	if passkey.WorkspaceAddress("bob@outside.example") || passkey.AdmitsRestricted(context.Background(), "bob@outside.example") != nil {
		t.Error("the rule boot built refuses bob@outside.example, on no listed domain, whom the lists name")
	}
	refusedWith(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": "second.example"}, "DISPATCH_SIGNIN_WORKSPACE_DOMAINS needs DISPATCH_SIGNIN_DOMAIN")
}
