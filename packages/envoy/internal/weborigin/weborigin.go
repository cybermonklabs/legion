// Package weborigin holds the one rule for a web origin a deployment writes down for Dispatch: a
// string a browser would send as its Origin header, written exactly as the browser writes it.
// Dispatch compares a request's Origin with such a string as a string, and a deployment
// registers the same string with its OAuth provider, so another spelling of the same origin is
// refused rather than rewritten. Its refusals name no setting: each caller prefixes its own.
package weborigin

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

var (
	labelRule = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// defaultPorts are the ports a browser leaves out of an origin, by scheme.
	defaultPorts = map[string]int{"http": 80, "https": 443}
)

// Parse reads raw as a web origin, one trailing slash trimmed, and returns it as a URL holding
// only its scheme and host, whose String is raw so trimmed. It refuses:
//   - anything but an absolute http or https URL with no path, user, query or fragment, without
//     echoing it, since a user or a query could carry a credential;
//   - a host label that is not a lowercase DNS label, or one ReservedLabel refuses: "--" as its
//     third and fourth characters, unless it is an xn-- label the IDNA lookup profile takes;
//   - a host ending in a number (NumericLabel) that is not an IPv4 address in its canonical form;
//   - a port outside 1-65535, written with a leading zero, or empty;
//   - any other spelling than the browser's: an upper-case scheme, or the scheme's default port.
func Parse(raw string) (*url.URL, error) {
	origin := strings.TrimSuffix(raw, "/")
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" ||
		parsed.User != nil || strings.ContainsAny(origin, "?#") {
		return nil, errors.New("must be an absolute http or https URL with no path, user, query or fragment, as an origin is (not echoed)")
	}
	host := parsed.Hostname()
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) > 63 || !labelRule.MatchString(label) {
			return nil, fmt.Errorf("host label %q is not a lowercase DNS label", label)
		}
		if err := ReservedLabel(label); err != nil {
			return nil, err
		}
	}
	if NumericLabel(labels[len(labels)-1]) {
		if addr, err := netip.ParseAddr(host); err != nil || !addr.Is4() {
			return nil, fmt.Errorf("host %s ends in a number and is no IPv4 address in its canonical form, which a browser refuses or rewrites", host)
		}
	}
	canonical := parsed.Scheme + "://" + host
	if port := parsed.Port(); port != "" || strings.HasSuffix(parsed.Host, ":") {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return nil, fmt.Errorf("port %q is not a port 1-65535 written without a leading zero", port)
		}
		if n != defaultPorts[parsed.Scheme] {
			canonical += ":" + port
		}
	}
	if origin != canonical {
		return nil, fmt.Errorf("must be written as a browser writes this origin, %s: a lowercase scheme and no default port", canonical)
	}
	return &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}, nil
}

// ReservedLabel refuses a host label with "--" as its third and fourth characters, the reserved
// form RFC 5890 section 2.3.1 names, except an xn-- label the IDNA lookup profile decodes and
// encodes back to itself (such as xn--bcher-kva, bücher, or the emoji label xn--ls8h). The two
// refusals give two reasons. An xn-- label that profile refuses, such as xn--a, is what RFC 5890
// calls a Fake A-label. Any other label of the form, such as ab--cd, is one public certificate
// authorities refuse to issue for (Let's Encrypt's errInvalidRLDH). A label without the form, such
// as abc--d, passes. Each refusal names the label and no setting: callers prefix their own.
func ReservedLabel(label string) error {
	if len(label) < 4 || label[2:4] != "--" {
		return nil
	}
	if strings.HasPrefix(label, "xn--") {
		if ascii, err := idna.Lookup.ToASCII(label); err == nil && ascii == label {
			return nil
		}
		return fmt.Errorf("host label %q is an xn-- label the IDNA lookup profile refuses, a Fake A-label (RFC 5890 section 2.3.1)", label)
	}
	return fmt.Errorf("host label %q has \"--\" as its third and fourth characters, a reserved form (RFC 5890 section 2.3.1) public certificate authorities refuse to issue for", label)
}

// NumericLabel reports whether a host label, in lower case, is a number as the WHATWG URL
// parser's IPv4 reading takes one: decimal, or 0x and hex digits, where 0x alone is the number 0.
// A host whose last label is numeric is an IPv4 address to a browser (WHATWG URL, "ends in a
// number"), which it refuses or rewrites unless it is already in its one canonical form: four
// decimal octets with no leading zero, the only IPv4 form netip.ParseAddr takes. The last label
// of no host name is one.
func NumericLabel(label string) bool {
	if hex, ok := strings.CutPrefix(label, "0x"); ok {
		return strings.Trim(hex, "0123456789abcdef") == ""
	}
	return label != "" && strings.Trim(label, "0123456789") == ""
}

// GoogleRedirectHost refuses a host Google takes in no OAuth redirect URI, so browser sign-in at an
// origin on it could not complete: an IP address, a single label, and a host whose top-level
// domain is not in the public suffix list's ICANN section
// (https://developers.google.com/identity/protocols/oauth2/web-server, "Redirect URI validation
// rules": "Hosts cannot be raw IP addresses" and "Hosts must use a TLD in the public suffix
// list"). Parse takes all three, since an origin a person reaches without Google sign-in may have
// any of them. Each refusal begins with "host" and names no setting: callers prefix their own.
func GoogleRedirectHost(host string) error {
	return publicDomainHost(host, "Google refuses as the host of an OAuth redirect URI", "Google requires of an OAuth redirect URI's host", "browser sign-in")
}

// PasskeyHost refuses a host Dispatch takes as no passkey's relying party, so passkey sign-in at an
// origin on it could not complete: the hosts GoogleRedirectHost refuses, by the same rule, since a
// relying party ID is a registrable domain name and never an IP address
// (https://www.w3.org/TR/webauthn-3/#relying-party-identifier). It holds the outside origin, where
// people from outside sign in with a passkey and Google signs in nobody. Each refusal begins with
// "host" and names no setting: callers prefix their own.
func PasskeyHost(host string) error {
	return publicDomainHost(host, "no passkey takes as its relying party", "Dispatch requires of a passkey's relying party", "passkey sign-in")
}

// publicDomainHost refuses host unless it is a DNS name under a top-level domain in the public
// suffix list's ICANN section: ipRefusal says who refuses an IP address, listRequirement who
// requires a listed top-level domain, and signIn names the sign-in at host that could not complete.
func publicDomainHost(host, ipRefusal, listRequirement, signIn string) error {
	if net.ParseIP(host) != nil {
		return fmt.Errorf("host %s is an IP address, which %s, so %s at it could not complete: name the origin by a DNS name", host, ipRefusal, signIn)
	}
	dot := strings.LastIndexByte(host, '.')
	if dot < 0 {
		return fmt.Errorf("host %s is a single label, with no top-level domain on the public suffix list, which %s, so %s at it could not complete", host, listRequirement, signIn)
	}
	if tld := host[dot+1:]; !tldOnPublicSuffixList(tld) {
		return fmt.Errorf("host %s ends in %s, no top-level domain on the public suffix list, which %s, so %s at it could not complete", host, tld, listRequirement, signIn)
	}
	return nil
}

// tldOnPublicSuffixList reports whether tld is a top-level domain in the public suffix list's ICANN
// section. It looks up "x.<tld>" because eight ccTLDs (np, ck, er, fk, jm, kh, mm, pg) are listed
// only as a "*.<tld>" rule, which a bare tld does not match; and it looks up the top-level domain
// alone, not the host, so a private-section suffix such as ts.net cannot decide.
func tldOnPublicSuffixList(tld string) bool {
	_, icann := publicsuffix.PublicSuffix("x." + tld)
	return icann
}
