package redact

import (
	"errors"
	"net"
	"net/url"
	"testing"
)

// TestHostShownMasksTheHostAndPortAsTheDialerWritesThem: the transport's words of a request to a
// URL with credentials print its host and its port as URLWithCredentials, as written and as the
// dialer writes them back: an IPv6 address written in full is dialled in its short form, and a
// port masks only where no further digit follows it. A URL with no credential, or none parsed,
// leaves the words as they came; a host that is not ASCII and an address the dialer refused print
// fixed words alone.
func TestHostShownMasksTheHostAndPortAsTheDialerWritesThem(t *testing.T) {
	refused := errors.New("connection refused")
	invalidPort := &net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Err: "invalid port", Addr: "98765432"}}
	for _, c := range []struct {
		name, raw, text, want string
		err                   error
	}{
		{"no-credential", "http://127.0.0.1:8080/x", "dial tcp 127.0.0.1:8080: connect: connection refused", "dial tcp 127.0.0.1:8080: connect: connection refused", refused},
		{"ipv6-in-full", "http://u:p@[0:0:0:0:0:0:0:1]:8080/x", "dial tcp [::1]:8080: connect: connection refused",
			"dial tcp [" + URLWithCredentials + "]:" + URLWithCredentials + ": connect: connection refused", refused},
		{"port-boundary", "http://u:p@127.0.0.1:1/x", "dial tcp 127.0.0.1:1: connect: connection refused (local :12345)",
			"dial tcp " + URLWithCredentials + ":" + URLWithCredentials + ": connect: connection refused (local :12345)", refused},
		{"past-host", "http://legion:4821/tail@host/x", "dial tcp: lookup legion: no such host", "dial tcp: lookup " + URLWithCredentials + ": no such host", refused},
		{"not-ascii", "http://u:p@t\u00f6rd.invalid/x", "dial tcp: lookup xn--trd-sna.invalid: no such host", ClassHostNotASCII, refused},
		{"address", "http://u:p@tail.invalid:98765432/x", "dial tcp: address 98765432: invalid port", ClassAddressRefused, invalidPort},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := hostShown(u, c.err, c.text); got != c.want {
				t.Errorf("hostShown(%q, %q) = %q, want %q", c.raw, c.text, got, c.want)
			}
		})
	}
	if got := hostShown(nil, refused, "words"); got != "words" {
		t.Errorf("hostShown(nil) = %q, want the words as they came", got)
	}
}

// TestMarkedURLNamesOnlyWhatTheURLRuleAllows: a URL with credentials is named by its scheme, the
// mark and its path only where the scheme is http or https and the path one the URL rule allows,
// and by its scheme and the mark alone where an "@" lies past its host, whose path is then a part
// of the credential.
func TestMarkedURLNamesOnlyWhatTheURLRuleAllows(t *testing.T) {
	for raw, want := range map[string]string{
		"https://u:p@host.example/widgets/healthz": "https://" + URLWithCredentials + "/widgets/healthz",
		"https://u:p@host.example":                 "https://" + URLWithCredentials,
		"ftp://u:p@host.example/widgets":           NotPrinted,
		"https://u:p@host.example/sig:secret":      NotPrinted,
		"https://u:p@host.example/%2Fsecret":       NotPrinted,
		"https://head/tail@host.example/widgets":   "https://" + URLWithCredentials,
		"https://head#tail@host.example":           "https://" + URLWithCredentials,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := AskedURL(u); got != want {
			t.Errorf("AskedURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
