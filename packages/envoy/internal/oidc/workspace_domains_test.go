package oidc

import (
	"strings"
	"testing"
)

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

// TestIsDomainNameTakesALowercaseDNSNameOfAtMost253Characters holds the one shape a Workspace
// domain setting takes, which Dispatch's DISPATCH_SIGNIN_DOMAIN and the secrets broker's
// BROKER_SIGNIN_WORKSPACE_DOMAINS both read through IsDomainName: two or more labels of 1 to 63
// lowercase letters, digits and inner hyphens, at most 253 characters in all, and nothing else.
//
// The named reverts: drop the length bound, and the 259-character row is taken; take capitals, and
// the Example.com row is.
func TestIsDomainNameTakesALowercaseDNSNameOfAtMost253Characters(t *testing.T) {
	longest := strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("a", 61)
	for _, name := range []string{"example.com", "d.example", "eng.example.co.uk", "x-1.example", "xn--bcher-kva.example", strings.Repeat("a", 63) + ".com", longest} {
		if !IsDomainName(name) {
			t.Errorf("IsDomainName(%q) = false, want true", name)
		}
	}
	if len(longest) != maxDomainLength {
		t.Fatalf("the longest row is %d characters, want %d", len(longest), maxDomainLength)
	}
	for _, name := range []string{"", "Example.com", "example", "exa_mple.com", "https://example.com", "alice@example.com", "-example.com", "example-.com", "example..com", ".example.com", "example.com.", "ex ample.com", "bücher.example",
		strings.Repeat("a", 64) + ".com", strings.Repeat(strings.Repeat("a", 63)+".", 4) + "com", longest + "a"} {
		if IsDomainName(name) {
			t.Errorf("IsDomainName(%q) = true, want false", name)
		}
	}
}

// TestWorkspaceDomainALabelWritesAnEntryAsItsALabels: an entry of a Workspace's other domains is
// taken written in Unicode or as its A-labels and answered as its A-labels, the form
// OnWorkspaceDomain compares in; one in capitals, one the IDNA lookup refuses and one that is no
// lowercase DNS name once written as A-labels are refused.
func TestWorkspaceDomainALabelWritesAnEntryAsItsALabels(t *testing.T) {
	for entry, want := range map[string]string{
		"second.example":        "second.example",
		"bücher.example":        "xn--bcher-kva.example",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
	} {
		if got, ok := WorkspaceDomainALabel(entry); !ok || got != want {
			t.Errorf("WorkspaceDomainALabel(%q) = %q, %v; want %q, true", entry, got, ok, want)
		}
	}
	for _, entry := range []string{"Second.example", "Bücher.example", "xn--a.example", "second..example", "third", strings.Repeat("a", 64) + ".com"} {
		if _, ok := WorkspaceDomainALabel(entry); ok {
			t.Errorf("WorkspaceDomainALabel(%q) took it, want it refused", entry)
		}
	}
}

// TestDomainNameRefusalNamesTheSettingAndTheValuesLengthAndNeverTheValue: the refusal both
// callers write names the setting as the caller calls it and the value's length in characters,
// and never the value, which may be a secret pasted into the wrong setting.
func TestDomainNameRefusalNamesTheSettingAndTheValuesLengthAndNeverTheValue(t *testing.T) {
	err := DomainNameRefusal("BROKER_SIGNIN_WORKSPACE_DOMAINS entry 2 of 3", "Bücher.example")
	if want := "BROKER_SIGNIN_WORKSPACE_DOMAINS entry 2 of 3 must be a lowercase domain name such as example.com; got a value of 14 characters"; err == nil || err.Error() != want {
		t.Fatalf("DomainNameRefusal = %v, want %q", err, want)
	}
}
