// Package tlstimeout records, in the process that met it and at that moment, what stood around a
// Go client's `net/http: TLS handshake timeout`: the phases of a fresh Go handshake to the same
// host beside one curl handshake, the process's CPU, and the cgroup's CPU quota, usage and process
// count (Recorder). In busy issue pods every gh call failed its handshake for hours while curl and
// git finished theirs in under a second (dispatch://INFRA-3562), and nothing in the pod said why;
// a record is that measurement, made where the failure was.
//
// Three clients record: `legion gh` around the real gh, which reads gh's own line (HostInLine);
// agent-secrets; and `legion`'s own HTTP clients, which wrap their transport (Transport). The
// record changes nothing of the call that failed: not its error, its response or its status, and
// not what the leak guard allows.
//
// It is public so that the Legion daemon (packages/daemon, module github.com/sjawhar/legion/daemon),
// which Go's internal rule bars from this module's internal packages, records as agent-secrets
// does, as githubrate and httpoutage are.
package tlstimeout

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// timeoutWords is net/http's own text for a handshake its transport's TLSHandshakeTimeout ended
// (its unexported tlsHandshakeTimeoutError), which no other error of the standard library says.
const timeoutWords = "net/http: TLS handshake timeout"

// IsTimeout reports whether err is a TLS handshake timeout: err, or an error it wraps (through
// Unwrap() error and Unwrap() []error), whose whole text is net/http's own words, or an err whose
// text ends with `: ` and those words, as an error made from another program's line does. nil, an
// HTTP status, a name that does not resolve, a refused dial and a context's end are not, and
// neither is the client's own Timeout, which net/http words as a context's end.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasSuffix(err.Error(), ": "+timeoutWords) || holdsWords(err)
}

// holdsWords reports whether err or an error down its chain reads exactly timeoutWords.
func holdsWords(err error) bool {
	for err != nil {
		if err.Error() == timeoutWords {
			return true
		}
		switch wrapped := err.(type) {
		case interface{ Unwrap() []error }:
			for _, each := range wrapped.Unwrap() {
				if holdsWords(each) {
					return true
				}
			}
			return false
		case interface{ Unwrap() error }:
			err = wrapped.Unwrap()
		default:
			return false
		}
	}
	return false
}

// HostInLine is the host, with its port where the URL names one, of the last line of text that
// reads `<Method> "<https URL>": net/http: TLS handshake timeout`: what Go's *url.Error prints for
// a request whose handshake its transport's TLSHandshakeTimeout ended, and so what gh prints on its
// standard error for one, `Get "https://api.github.com/repos/<owner>/<name>": net/http: TLS
// handshake timeout` (leakguard's ghTransportFailure reads the same line). The method is one word,
// a capital and lower-case letters, as *url.Error writes it; the URL is Go-quoted, as *url.Error
// quotes it, and must be https with a host that is a plain host (plainHost). Nothing of the URL's
// user, path, query or fragment is answered, and any other line is none: another cause, the words
// with more after them, and the proxyconnect form of a handshake to a proxy among them.
func HostInLine(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if host, ok := lineHost(strings.TrimRight(lines[i], "\r")); ok {
			return host, true
		}
	}
	return "", false
}

// lineHost is HostInLine for one line.
func lineHost(line string) (string, bool) {
	rest, ok := strings.CutSuffix(line, ": "+timeoutWords)
	if !ok {
		return "", false
	}
	method, quoted, ok := strings.Cut(rest, " ")
	if !ok || !methodWord(method) {
		return "", false
	}
	if prefix, err := strconv.QuotedPrefix(quoted); err != nil || prefix != quoted {
		return "", false
	}
	address, err := strconv.Unquote(quoted)
	if err != nil {
		return "", false
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || !plainHost(parsed.Host) {
		return "", false
	}
	return parsed.Host, true
}

// methodWord reports whether word is a method as *url.Error writes one: a capital and then lower
// case (`Get`, `Post`).
func methodWord(word string) bool {
	if word == "" || word[0] < 'A' || word[0] > 'Z' {
		return false
	}
	for _, r := range word[1:] {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// plainHost reports whether host is a host and port a record may print as it stands and probe:
// not empty, at most 261 bytes (a name of 255 and a port), and letters, digits and `.-_:[]%` alone,
// which a DNS name, an IPv4 address, a bracketed IPv6 address with its zone and a port are made
// of. A host holding anything else, a space, a quote, a `/`, `@`, `?` or `#`, is none, so no host
// can add a field to a line or another part to the URL the probes ask.
func plainHost(host string) bool {
	if host == "" || len(host) > 261 {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(".-_:[]%", r):
		default:
			return false
		}
	}
	return true
}

// Transport is base with a record of each TLS handshake timeout it meets: RoundTrip calls base and,
// where its error IsTimeout, records for the request URL's host (Recorder.Record), then hands back
// base's own response and error, unchanged, recorded or not. A nil base is http.DefaultTransport,
// read at each call as http.Client reads it; a nil r records nothing. CloseIdleConnections reaches
// base where base has it, so a client's CloseIdleConnections still closes base's.
//
// http.DefaultTransport itself is never replaced: code that asserts it is an *http.Transport
// (packages/daemon/internal/workspace/cache_packs.go, cmd/cipherd) would then fail. A client
// wraps its own.
func Transport(base http.RoundTripper, r *Recorder) http.RoundTripper {
	return &transport{base: base, recorder: r}
}

type transport struct {
	base     http.RoundTripper
	recorder *Recorder
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.roundTripper().RoundTrip(request)
	if t.recorder != nil && IsTimeout(err) {
		t.recorder.Record(request.URL.Host)
	}
	return response, err
}

func (t *transport) CloseIdleConnections() {
	if closer, ok := t.roundTripper().(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *transport) roundTripper() http.RoundTripper {
	if t.base == nil {
		return http.DefaultTransport
	}
	return t.base
}
