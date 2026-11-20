package oidc

import "testing"

// TestOnWorkspaceDomainHoldsBothCallersCases holds the one Workspace rule to every case Dispatch's
// sign-in and the secrets broker's edge each held their own copy of it to: an address's domain is
// the Workspace's when it is one of the Workspace's domains or a subdomain of one, in any case,
// with one trailing dot, in Unicode that IDNA lookup maps onto an A-label the Workspace owns
// (fullwidth letters, a capital Ü, an ideographic full stop among them, beside a label the lookup
// refuses), or with a label left empty; a domain that only shares a suffix or a prefix with one is
// not.
//
// The named reverts: drop the IDNA mapping, and the Unicode rows fail; drop the empty-label rule,
// and the rows with two dots fail.
func TestOnWorkspaceDomainHoldsBothCallersCases(t *testing.T) {
	workspace := []string{"example.com", "xn--bcher-kva.example", "second.example"}
	for _, at := range []string{
		"example.com",
		"eng.example.com",
		"EXAMPLE.com",
		"second.example",
		"xn--bcher-kva.example",
		"bücher.example",
		"eu.bücher.example",
		"EU.bücher.example",
		"b\u00dccher.example",
		"eu\u3002bücher.example",
		"ab--cd.bücher.example",
		"\uff45\uff58\uff41\uff4d\uff50\uff4c\uff45.com",
		"example.com.",
		"bücher.example.",
		"example.com\u3002",
		"example.com\uff0e",
		"example.com\uff61",
		"bücher.example\u3002",
		"example.com.\u3002",
		"example.com\u3002\u3002",
		"example.com..",
		".example.com",
		"bücher.example\u3002\u3002",
		"",
	} {
		if !OnWorkspaceDomain(at, workspace) {
			t.Errorf("OnWorkspaceDomain(%q) = false, want true", at)
		}
	}
	for _, at := range []string{
		"outside.example",
		"bücherei.example",
		"xn--bcherei-zxa.example",
		"notbücher.example",
		"notexample.com",
		"example.com.evil.example",
		"example.co",
		"second.example.org",
	} {
		if OnWorkspaceDomain(at, workspace) {
			t.Errorf("OnWorkspaceDomain(%q) = true, want false", at)
		}
	}
	if OnWorkspaceDomain("example.com", []string{""}) {
		t.Error(`OnWorkspaceDomain("example.com", [""]) = true: an empty entry is no domain`)
	}
}
