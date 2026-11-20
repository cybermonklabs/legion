package oidc

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// OnWorkspaceDomain reports whether at, an address's domain, is one the Google Workspace owns or a
// subdomain of one: one of workspace, the Workspace's domains, each a lowercase DNS name with an
// internationalized label written as its A-label. It is the one rule Dispatch's sign-in and the
// secrets broker's edge read an address by, so an address one of them takes as a person of the
// Workspace the other takes as one too.
//
// at is compared without regard to case and without one trailing dot, which names the same domain,
// and, when it holds a character outside ASCII, also as IDNA lookup maps it (idna.Lookup.ToASCII),
// the A-label form a mail server resolves it in: so bücher.example is on xn--bcher-kva.example, and
// so is that domain written with a capital Ü, or the Workspace's domain written in fullwidth
// letters. An ASCII domain is its own A-label form. The mapped form is trimmed of one trailing dot
// too: IDNA maps U+3002, U+FF0E and U+FF61 onto the full stop, so example.com followed by one of
// them maps to "example.com.".
//
// A domain still holding an empty label after that trim (example.com.。, example.com.., a lone "")
// is answered as the Workspace's. The lookup profile sets no verifyDNSLength, so it maps such a
// domain through, and one trim cannot reach what a second dot hides. No mail server resolves a
// domain with an empty label, so this refuses nothing a person outside the Workspace is named by,
// and what it stops is a spelling of the Workspace's own domain the comparison would not see.
func OnWorkspaceDomain(at string, workspace []string) bool {
	at = strings.TrimSuffix(at, ".")
	if onAnyDomain(at, workspace) || hasEmptyLabel(at) {
		return true
	}
	if isASCII(at) {
		return false
	}
	// The lookup maps every label it can even when it refuses another (one with -- as its third
	// and fourth characters, say), and answers that mapping beside its error, so the error is not
	// read: a domain it maps onto one the Workspace owns is the Workspace's whatever else it holds.
	ascii, _ := idna.Lookup.ToASCII(at)
	ascii = strings.TrimSuffix(ascii, ".")
	return onAnyDomain(ascii, workspace) || hasEmptyLabel(ascii)
}

// onAnyDomain reports whether at is one of domains or a subdomain of one, compared without regard
// to case. An empty entry of domains is no domain.
func onAnyDomain(at string, domains []string) bool {
	for _, domain := range domains {
		tail := len(at) - len(domain)
		switch {
		case domain == "" || tail < 0:
		case tail == 0 && strings.EqualFold(at, domain):
			return true
		case tail > 0 && at[tail-1] == '.' && strings.EqualFold(at[tail:], domain):
			return true
		}
	}
	return false
}

// hasEmptyLabel reports whether at, a domain already trimmed of one trailing dot, is empty or holds
// a label with nothing in it: a leading dot, a trailing one a second dot left, or two dots together.
func hasEmptyLabel(at string) bool {
	return at == "" || strings.HasPrefix(at, ".") || strings.HasSuffix(at, ".") || strings.Contains(at, "..")
}

// isASCII reports whether s holds no byte outside ASCII.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
