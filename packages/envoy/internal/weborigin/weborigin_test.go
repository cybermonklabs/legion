package weborigin_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/weborigin"
)

// Every origin Parse takes is returned as the string a browser sends in Origin for it, which is
// what Dispatch compares a request's Origin with: the same string, a trailing slash trimmed.
func TestParseTakesAnOriginWrittenAsABrowserWritesIt(t *testing.T) {
	for raw, want := range map[string]string{
		"https://dispatch-outside.example.com":                "https://dispatch-outside.example.com",
		"https://dispatch-outside.example.com/":               "https://dispatch-outside.example.com",
		"https://dispatch-outside.example.com:8443":           "https://dispatch-outside.example.com:8443",
		"http://dispatch.dispatch.svc.cluster.local:8766":     "http://dispatch.dispatch.svc.cluster.local:8766",
		"https://10.0.0.1":                                    "https://10.0.0.1",
		"https://abc--d.example.com":                          "https://abc--d.example.com",
		"https://a.example.com":                               "https://a.example.com",
		"https://1outside.example.com":                        "https://1outside.example.com",
		"https://" + strings.Repeat("a", 63) + ".example.com": "https://" + strings.Repeat("a", 63) + ".example.com",
		"https://single":                                      "https://single",
		// An xn-- label the IDNA lookup profile decodes and encodes back to itself passes the
		// reserved-form rule (ReservedLabel), an emoji label among them.
		"https://xn--bcher-kva.example.com": "https://xn--bcher-kva.example.com",
		"https://xn--p1ai.example.com":      "https://xn--p1ai.example.com",
		"https://xn--ls8h.example.com":      "https://xn--ls8h.example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			u, err := weborigin.Parse(raw)
			require.NoError(t, err)
			require.Equal(t, want, u.String())
		})
	}
}

// Every string that is not an origin, that a browser writes otherwise, or that no browser opens
// is refused with the reason, never rewritten: a deployment registers the string it set with its
// OAuth provider, so a rewrite would render one string and register another. The refusal before
// the URL is known to carry no user or query echoes nothing, since either could hold a
// credential. The browser column is WHATWG URL's origin serialization, as Node's
// new URL(v).origin prints it.
func TestParseRefusesWhatIsNoBrowsersOrigin(t *testing.T) {
	const notOrigin = "must be an absolute http or https URL with no path, user, query or fragment, as an origin is (not echoed)"
	reserved := func(label string) string {
		return `host label "` + label + `" has "--" as its third and fourth characters, a reserved form (RFC 5890 section 2.3.1) public certificate authorities refuse to issue for`
	}
	fakeALabel := func(label string) string {
		return `host label "` + label + `" is an xn-- label the IDNA lookup profile refuses, a Fake A-label (RFC 5890 section 2.3.1)`
	}
	notLabel := func(label string) string { return `host label "` + label + `" is not a lowercase DNS label` }
	notCanonical := func(canonical string) string {
		return "must be written as a browser writes this origin, " + canonical + ": a lowercase scheme and no default port"
	}
	badPort := func(port string) string {
		return `port "` + port + `" is not a port 1-65535 written without a leading zero`
	}
	notIPv4 := func(host string) string {
		return "host " + host + " ends in a number and is no IPv4 address in its canonical form, which a browser refuses or rewrites"
	}
	for _, row := range []struct{ raw, want string }{
		{"", notOrigin},
		{"dispatch-outside.example.com", notOrigin},
		{"ftp://dispatch-outside.example.com", notOrigin},
		{"https://dispatch-outside.example.com/dispatch", notOrigin},
		{"https://dispatch-outside.example.com//", notOrigin},
		{"https://dispatch-outside.example.com?a=b", notOrigin},
		{"https://dispatch-outside.example.com?", notOrigin},
		{"https://dispatch-outside.example.com#top", notOrigin},
		{"https://dispatch-outside.example.com#", notOrigin},
		{"https://example-user@dispatch-outside.example.com", notOrigin},
		{"https://Dispatch-Outside.example.com", notLabel("Dispatch-Outside")},
		{"https://dispatch-outside..example.com", notLabel("")},
		{"https://dispatch-outside.example.com.", notLabel("")},
		{"https://" + strings.Repeat("a", 64) + ".example.com", notLabel(strings.Repeat("a", 64))},
		// An IPv6 literal is no host Parse takes: Hostname drops its brackets, and ":" is in no
		// DNS label.
		{"https://[::1]", notLabel("::1")},
		{"https://-outside.example.com", notLabel("-outside")},
		{"https://outside-.example.com", notLabel("outside-")},
		{"https://ab--cd.example.com", reserved("ab--cd")},
		{"https://outside.ab--cd.com", reserved("ab--cd")},
		{"https://xn--a.example.com", fakeALabel("xn--a")},
		{"https://xn--1ug.example.com", fakeALabel("xn--1ug")},
		// "xn--" ends in a hyphen, so the DNS label rule refuses it before the reserved-label one.
		{"https://xn--.example.com", notLabel("xn--")},
		// The review's table: each of these resolved and rendered as written at 5cbf7456a0.
		{"https://dispatch-outside.example.com:443", notCanonical("https://dispatch-outside.example.com")},
		{"https://outside.test:443", notCanonical("https://outside.test")},
		{"http://dispatch-outside.example.com:80", notCanonical("http://dispatch-outside.example.com")},
		{"HTTPS://dispatch-outside.example.com", notCanonical("https://dispatch-outside.example.com")},
		{"https://dispatch-outside.example.com:", badPort("")},
		{"https://dispatch-outside.example.com:0443", badPort("0443")},
		{"https://dispatch-outside.example.com:0", badPort("0")},
		{"https://dispatch-outside.example.com:65536", badPort("65536")},
		{"https://dispatch-outside.example.com:99999", badPort("99999")},
		{"https://127.1", notIPv4("127.1")},
		{"https://0x7f.0.0.1", notIPv4("0x7f.0.0.1")},
		{"https://2130706433", notIPv4("2130706433")},
		{"https://010.0.0.1", notIPv4("010.0.0.1")},
		{"https://1.2.3.4.5", notIPv4("1.2.3.4.5")},
		{"https://example.123", notIPv4("example.123")},
		{"https://a.example.0x1", notIPv4("a.example.0x1")},
	} {
		t.Run(row.raw, func(t *testing.T) {
			_, err := weborigin.Parse(row.raw)
			require.EqualError(t, err, row.want)
			require.NotContains(t, err.Error(), "example-user", "the refusal echoes no user, which could carry a credential")
		})
	}
}

// GoogleRedirectHost takes a host under a top-level domain in the public suffix list's ICANN
// section: ones the list carries only as a "*.<tld>" rule (np, ck, as er, fk, jm, kh, mm and pg
// are), an internationalised one (xn--p1ai), and one under a private-section suffix (ts.net),
// which the top-level domain decides and not that suffix.
func TestGoogleRedirectHostTakesAHostUnderAPublicSuffix(t *testing.T) {
	for _, host := range []string{
		"dispatch-outside.example.com",
		"xn--bcher-kva.example.com",
		"dispatch.example.com.np",
		"dispatch.example.ck",
		"dispatch.example.xn--p1ai",
		"example-outside.example-tailnet.ts.net",
	} {
		t.Run(host, func(t *testing.T) {
			require.NoError(t, weborigin.GoogleRedirectHost(host))
		})
	}
}

// GoogleRedirectHost refuses each host Google refuses in an OAuth redirect URI, with the reason and
// no setting named: an IP address, a single label, and a top-level domain off the public suffix
// list's ICANN section, the special-use and private-use names among them.
func TestGoogleRedirectHostRefusesWhatGoogleRefuses(t *testing.T) {
	ip := func(host string) string {
		return "host " + host + " is an IP address, which Google refuses as the host of an OAuth redirect URI, so browser sign-in at it could not complete: name the origin by a DNS name"
	}
	single := func(host string) string {
		return "host " + host + " is a single label, with no top-level domain on the public suffix list, which Google requires of an OAuth redirect URI's host, so browser sign-in at it could not complete"
	}
	offList := func(host, tld string) string {
		return "host " + host + " ends in " + tld + ", no top-level domain on the public suffix list, which Google requires of an OAuth redirect URI's host, so browser sign-in at it could not complete"
	}
	for _, row := range []struct{ host, want string }{
		{"10.0.0.1", ip("10.0.0.1")},
		{"127.0.0.1", ip("127.0.0.1")},
		{"dispatch", single("dispatch")},
		{"dispatch-outside.example", offList("dispatch-outside.example", "example")},
		{"dispatch.test", offList("dispatch.test", "test")},
		{"dispatch.invalid", offList("dispatch.invalid", "invalid")},
		{"dispatch.localhost", offList("dispatch.localhost", "localhost")},
		{"dispatch.local", offList("dispatch.local", "local")},
		{"dispatch.internal", offList("dispatch.internal", "internal")},
	} {
		t.Run(row.host, func(t *testing.T) {
			require.EqualError(t, weborigin.GoogleRedirectHost(row.host), row.want)
		})
	}
}
