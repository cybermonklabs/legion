package redact

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/natsurl"
)

// Unanswered is err, the failure of an HTTP request that got no answer it could read
// (http.Client.Do's error, or the error of reading the answer's body), as a message may print it:
// net/http's operation and the request named shown, or where shown is "" by the URL it asked as
// askedURL names it, and then the failure by its transport text, a TLS record or certificate
// failure's fixed words, or its class (unansweredFailure). It is for a request whose URL Call does
// not print, such as a GitHub API path: Cause prints a transport failure's own text only beside a
// configured URL and a request URL under /v1/ of it, Class never prints it, and neither keeps a
// timeout's or a refused connection's own words for any other request. A module outside this one,
// the daemon among them, prints such a request through it as oauth2call.Unanswered. A caller
// passes shown only for words of its own that name the call in place of its URL, never for a name
// of the URL another rule made, so a URL has one name wherever it prints.
//
// It never prints net/http's own text of any other failure, which can quote the answering
// party's words: Go parses a redirect's Location before a client's CheckRedirect is asked
// (net/http/client.go of go1.27.3, lines 655-665 before 711-717), so a Location that does not
// parse comes back as `failed to parse Location header "<the Location as it came>"` whatever the
// client's redirect policy; a status line or a header line that does not parse comes back quoted
// too (response.go 174-188, textproto's reader.go 549-621), and so does a chunked body's trailer
// line (transfer.go 956). Whoever answers in the server's place can write the request's
// credential there. For a client that follows a redirect, net/http's error names the URL the
// Location named, and its transport failure the host it named, so a failure on another host than
// request's prints as the redirect's class alone. An error that holds no *url.Error, a body
// read's, prints as its failure alone, under the caller's own words for the call.
//
// A URL that holds user information or an "@" past its host (natsurl.UserinfoPastHost) can hold
// a credential where the host and the port go: a password holding an unencoded "@" leaves its tail
// where url.Parse reads the host (https://user:p@ssword), and a credential holding a "/", "?" or
// "#" leaves its head there and the digits of its next part where the port goes
// (http://user:4821/<tail>@host), and the resolver's or the dialer's own words name both. So such
// a URL is named with its authority as URLWithCredentials (askedURL), and the transport's words
// with its host and its port as URLWithCredentials, or as fixed words where those cannot be found
// in them (hostShown), whether the call is named shown or by its URL.
//
// It unwraps to err, so errors.Is and errors.As still find the failure: a caller decides on it,
// and prints only this. Unanswered(nil, request, shown) is nil.
func Unanswered(err error, request *http.Request, shown string) error {
	if err == nil {
		return nil
	}
	var failed *url.Error
	if !errors.As(err, &failed) {
		return &unanswered{unansweredFailure(err), err}
	}
	name := shown
	switch {
	case name != "":
	case request != nil:
		name = askedURL(request.URL)
	default:
		name = NotPrinted
		if asked, err := url.Parse(failed.URL); err == nil {
			name = askedURL(asked)
		}
	}
	text := unansweredFailure(failed.Err)
	switch {
	case failed.Op == "parse":
		text = ClassNotAURL
	case request != nil && redirectedElsewhere(failed.URL, request.URL):
		text = ClassRedirected
	case request != nil:
		text = hostShown(request.URL, failed.Err, text)
	default:
		if asked, err := url.Parse(failed.URL); err == nil {
			text = hostShown(asked, failed.Err, text)
		}
	}
	return &unanswered{fmt.Sprintf("%s %q: %s", failed.Op, name, text), err}
}

// unanswered is Unanswered's answer: its text, over the failure it names.
type unanswered struct {
	text string
	err  error
}

func (u *unanswered) Error() string { return u.text }
func (u *unanswered) Unwrap() error { return u.err }

// The words Unanswered names a failure by where it prints none of net/http's, beside the classes
// class names (ClassNotAURL, ClassRequestFailed). ClassHostNotASCII and ClassAddressRefused are the
// words the daemon's redact.NotASCIIHostError and redact.AddressError print for the same failures,
// so one failure has one set of words in either module.
const (
	ClassRedirected       = "the request was redirected to another host, which did not answer (the redirect's target is not printed: it is the answering party's own words)"
	ClassLocationUnparsed = "the answer was a redirect whose Location does not parse (the Location is not printed: it is the answering party's own words)"
	ClassHostNotASCII     = "(the request failed; its error is not printed: it names the host of an address that holds a credential, and that host is not ASCII)"
	ClassAddressRefused   = "(the request failed; its error is not printed: the address holds a credential, and the dialer refused it as no valid host and port)"
)

// URLWithCredentials stands in place of the authority of a URL a request asked that holds user
// information or an "@" past its host, in the name Unanswered and AskedURL give it, and of its host
// and its port in the transport's words Unanswered prints. It is the daemon's
// redact.URLWithCredentials, so such a URL has one mark wherever it prints.
const URLWithCredentials = "[a URL with credentials]"

// unansweredFailure is the words Unanswered prints for err, the failure under a request that got
// no answer: for the first error down err's chain (errors.Unwrap) that is a transport failure
// (transport: a dial or another net.OpError, name resolution, a timeout, a refused, reset or
// broken connection, an end of the stream, an http answer to https, a TLS record or certificate
// failure) or the caller's own cancellation (context.Canceled), its text as transportWords prints
// it: the transport's or context's own fixed words and the addresses the request dialled, or for
// a TLS record or certificate failure the fixed words for its type, since x509's text names what
// the certificate the answering party sent holds; and otherwise fixed text naming its class. A
// Location that did not parse is told by net/http's own fixed words for it, a decision on them
// that prints none of them.
func unansweredFailure(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if transport(e) || e == context.Canceled {
			return transportWords(e)
		}
	}
	if err != nil && strings.HasPrefix(err.Error(), "failed to parse Location header ") {
		return ClassLocationUnparsed
	}
	return ClassRequestFailed
}

// unreadable is err, the failure of a read of an answer's body (ReadAnswer, StreamAnswer's
// stream), as a message may print it: err itself where it is nil, a transport failure other than
// a TLS record or certificate failure, or the caller's cancellation (io.EOF, io.ErrUnexpectedEOF,
// a timeout, a reset connection), whose text is the transport's own words, so a reader that
// compares it with io.EOF still finds it as it came; and otherwise Unanswered of it, its class or
// a TLS failure's fixed words, unwrapping to err. net/http's own text of a body it could not read
// can quote a line the answer wrote: a chunked body's trailer line that does not parse comes back
// quoted (net/http/transfer.go of go1.27.3, line 956, then textproto's reader.go 549-621), and
// whoever answers in the server's place can write the request's credential there.
func unreadable(err error) error {
	if err == nil || err == context.Canceled || transport(err) && !tlsRecordOrCertificate(err) {
		return err
	}
	return Unanswered(err, nil, "")
}

// redirectedElsewhere reports whether failed, the URL net/http's error names, is on another host
// than asked, the request's own: a redirect the client followed, whose target is the answering
// party's words. A URL that does not parse is counted as elsewhere.
func redirectedElsewhere(failed string, asked *url.URL) bool {
	at, err := url.Parse(failed)
	return err != nil || !strings.EqualFold(at.Host, asked.Host)
}

// AskedURL is u, the URL a request asked, as Unanswered names it (askedURL): the name a line about
// a request that got an answer gives the URL too, so the URL has one name wherever it prints,
// whether the request got an answer or not.
func AskedURL(u *url.URL) string { return askedURL(u) }

// askedURL is u, the URL a request asked, as Unanswered names it, daemon and envoy alike: scheme
// "://" host path, with u's query and its fragment removed, where URL prints that unchanged, and
// NotPrinted where URL refuses it (an escape, a path or a host it does not allow) or u is nil or
// opaque. The host is the one net/url parsed, the host the request dialled, which a failed dial's
// own words name too. A URL that holds user information or an "@" past its host is named by
// markedURL, its authority as URLWithCredentials: where a password holds an '@' and no host follows
// it (URL's https://user:p@ssword), the host net/url read is the password's tail, and nothing here
// can tell it from a host. The query goes whole, its keys with its values: a client can write a
// credential as a key alone (?<token>), and neither URL's allow-list, which a token's letters,
// digits and '-' pass, nor the credentials the request carried, whose query values requestTokens
// reads and never its keys, can tell such a key from a parameter's name.
func askedURL(u *url.URL) string {
	if u == nil || u.Opaque != "" {
		return NotPrinted
	}
	if u.User != nil || natsurl.UserinfoPastHost(u) {
		return markedURL(u)
	}
	return URL(u.Scheme + "://" + u.Host + u.EscapedPath())
}

// markedURL is u, a URL that holds user information or an "@" past its host, as askedURL names
// it: scheme "://" URLWithCredentials path, where the scheme is http or https and the path one URL
// prints (plainPath), and NotPrinted otherwise. A URL with an "@" past its host
// (natsurl.UserinfoPastHost) is scheme "://" URLWithCredentials alone: a credential holding a "/",
// "?" or "#" ends url.Parse's authority inside it, so its head is the host and the port and its
// tail the path, as the daemon's redact.URLCredentials writes such a URL.
func markedURL(u *url.URL) string {
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return NotPrinted
	}
	if natsurl.UserinfoPastHost(u) {
		return u.Scheme + "://" + URLWithCredentials
	}
	if path := u.EscapedPath(); plainPath(path) {
		return u.Scheme + "://" + URLWithCredentials + path
	}
	return NotPrinted
}

// hostShown is text, the words unansweredFailure gave err, the failure of a request to asked, as
// Unanswered prints them: unchanged where asked holds no user information and no "@" past its host
// (natsurl.UserinfoPastHost), and otherwise with asked's host and port written as
// URLWithCredentials, as the daemon's redact.HostShown writes them. A resolver's error names the
// host (`lookup <host>: no such host`) and a dial's the address and the port (`dial tcp
// <ip>:<port>`), and either can be a part of the credential. The host is masked as written and, for
// an IP address, as the dialer writes it back (net.IP's String: `[0:0::1]` is dialled as `[::1]`);
// the port as written and as the dialer writes it back, a number with no leading zeros (`:0048213`
// is dialled and printed as `:48213`). A host that is not ASCII is dialled by its ASCII form
// (`xn--…`, its ASCII letters kept, lower-cased), which no replacement of the host as written
// finds, so such a failure is ClassHostNotASCII alone; and an address the dialer refused as no host
// and port is ClassAddressRefused alone, since *net.AddrError quotes the part it refused, which can
// be a password's head url.Parse read as the port.
func hostShown(asked *url.URL, err error, text string) string {
	if asked == nil || asked.User == nil && !natsurl.UserinfoPastHost(asked) {
		return text
	}
	host, port := asked.Hostname(), asked.Port()
	var address *net.AddrError
	switch {
	case strings.IndexFunc(host, func(r rune) bool { return r > unicode.MaxASCII }) >= 0:
		return ClassHostNotASCII
	case errors.As(err, &address):
		return ClassAddressRefused
	}
	if host != "" {
		text = strings.ReplaceAll(text, host, URLWithCredentials)
		literal, _, _ := strings.Cut(host, "%")
		if ip := net.ParseIP(literal); ip != nil {
			text = strings.ReplaceAll(text, ip.String(), URLWithCredentials)
		}
	}
	if port != "" {
		text = portShown(text, port)
		if n, err := strconv.Atoi(port); err == nil && strconv.Itoa(n) != port {
			text = portShown(text, strconv.Itoa(n))
		}
	}
	return text
}

// portShown is text with each ":"+port that no further digit follows written as
// ":"+URLWithCredentials, so a port of `1` leaves `:12345` as it is.
func portShown(text, port string) string {
	var out strings.Builder
	needle := ":" + port
	for {
		i := strings.Index(text, needle)
		if i < 0 {
			out.WriteString(text)
			return out.String()
		}
		end := i + len(needle)
		out.WriteString(text[:i])
		if end < len(text) && '0' <= text[end] && text[end] <= '9' {
			out.WriteString(needle)
		} else {
			out.WriteString(":" + URLWithCredentials)
		}
		text = text[end:]
	}
}
