// Package weborigin holds the one rule for a web origin a deployment writes down for Dispatch: a
// string a browser would send as its Origin header, written exactly as the browser writes it.
// Dispatch compares a request's Origin with such a string as a string, and a deployment
// registers the same string with its OAuth provider, so another spelling of the same origin is
// refused rather than rewritten. Its refusals name no setting: each caller prefixes its own.
package weborigin

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

var (
	// labelRule is a lowercase DNS label; its length, at most 63, is checked beside it.
	labelRule = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// numericLabelRule is a host label a browser's URL parser reads as a number, decimal or hex,
	// and ipv4Rule an IPv4 address in its one canonical form: four decimal octets, no leading
	// zero. A host whose last label is numeric is an IPv4 address to a browser (WHATWG URL, "ends
	// in a number"), which it refuses or rewrites unless it is already in that form.
	numericLabelRule = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-f]*)$`)
	ipv4Rule         = regexp.MustCompile(`^(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])$`)
	// defaultPorts are the ports a browser leaves out of an origin, by scheme.
	defaultPorts = map[string]int{"http": 80, "https": 443}
)

// Parse reads raw as a web origin, one trailing slash trimmed, and returns it as a URL holding
// only its scheme and host, whose String is raw so trimmed. It refuses:
//   - anything but an absolute http or https URL with no path, user, query or fragment, without
//     echoing it, since a user or a query could carry a credential;
//   - a host label that is not a lowercase DNS label, or one ReservedLabel refuses: "--" as its
//     third and fourth characters, unless it is an xn-- label the IDNA lookup profile takes;
//   - a host ending in a number that is not an IPv4 address in its canonical form;
//   - a port outside 1-65535, written with a leading zero, or empty;
//   - any other spelling than the browser's: an upper-case scheme, or the scheme's default port.
func Parse(raw string) (*url.URL, error) {
	origin := strings.TrimSuffix(raw, "/")
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(origin, "#") {
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
	if numericLabelRule.MatchString(labels[len(labels)-1]) && !ipv4Rule.MatchString(host) {
		return nil, fmt.Errorf("host %s ends in a number and is no IPv4 address in its canonical form, which a browser refuses or rewrites", host)
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
