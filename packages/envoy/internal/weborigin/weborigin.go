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

	"github.com/sjawhar/envoy/internal/fold"
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

// WithoutDefaultPort is host, a URL's host and port, with scheme's default port (443 for https, 80
// for http) taken off, the scheme compared without case: as a browser writes an origin, and as RFC
// 3986 Section 6.2.3 has a URL compared.
func WithoutDefaultPort(scheme, host string) string {
	switch {
	case strings.EqualFold(scheme, "https"):
		host, _ = strings.CutSuffix(host, ":443")
	case strings.EqualFold(scheme, "http"):
		host, _ = strings.CutSuffix(host, ":80")
	}
	return host
}

// SameHost reports whether a and b, each a scheme and a host (an origin, or a request's Host under
// the scheme of the origin it is compared with), can be named by one request's Host. Two hosts are
// one when their names are one (hostName: without regard to ASCII case, an IPv6 literal's
// brackets or one trailing dot) and their ports are one as numbers, where a scheme's default port
// (443 for https, 80 for http) is equal to no port. So https://b.example.test and
// http://b.example.test share the Host b.example.test, https://B.Example.Test.:443 is
// https://b.example.test's host, and https://b.example.test:8443 is a host of its own. A request's
// Host names origin's host where SameHost(origin, &url.URL{Scheme: origin.Scheme, Host: host}).
func SameHost(a, b *url.URL) bool {
	aPort, aBare := hostPort(a.Scheme, a.Host)
	bPort, bBare := hostPort(b.Scheme, b.Host)
	return hostName(a.Host) == hostName(b.Host) && (aPort == bPort || aBare && bBare)
}

// SameHostName reports whether a and b have one host name, at any port of either: their names
// compared as SameHost compares them (hostName: without regard to ASCII case, an IPv6 literal's
// brackets or one trailing dot) and their ports not at all. It is the host a browser keeps one set
// of cookies for, whatever the port.
func SameHostName(a, b *url.URL) bool {
	return hostName(a.Host) == hostName(b.Host)
}

// hostName is the name of host, a URL's host and port or a request's Host: any port taken off, an
// IPv6 literal's brackets taken off, its ASCII letters lowered (fold.ASCII) and one trailing dot
// dropped, since DNS reads "name." as "name".
func hostName(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	return strings.TrimSuffix(fold.ASCII(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")), ".")
}

// hostPort is host's port under scheme, as SameHost compares it: a number, scheme's default where
// host names none and -1 where it names no string of digits; and whether that port is scheme's
// default, so that a request naming no port names host too (bare).
func hostPort(scheme, host string) (int, bool) {
	written := ""
	if _, port, err := net.SplitHostPort(host); err == nil {
		written = port
	}
	defaultPort, known := defaultPorts[fold.ASCII(scheme)]
	if written == "" {
		return defaultPort, known
	}
	port, err := strconv.Atoi(written)
	if err != nil || strings.Trim(written, "0123456789") != "" {
		return -1, false
	}
	return port, known && port == defaultPort
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
	if net.ParseIP(host) != nil {
		return fmt.Errorf("host %s is an IP address, which Google refuses as the host of an OAuth redirect URI, so browser sign-in at it could not complete: name the origin by a DNS name", host)
	}
	dot := strings.LastIndexByte(host, '.')
	if dot < 0 {
		return fmt.Errorf("host %s is a single label, with no top-level domain on the public suffix list, which Google requires of an OAuth redirect URI's host, so browser sign-in at it could not complete", host)
	}
	if tld := host[dot+1:]; !tldOnPublicSuffixList(tld) {
		return fmt.Errorf("host %s ends in %s, no top-level domain on the public suffix list, which Google requires of an OAuth redirect URI's host, so browser sign-in at it could not complete", host, tld)
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
