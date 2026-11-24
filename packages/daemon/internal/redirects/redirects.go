// Package redirects is the daemon's one redirect policy: an HTTP client of the daemon follows no
// redirect (Refused), or, where its server answers a redirect it must follow, none off the origin
// its request was sent to (SameOrigin); any other redirect is the answer itself, refused by its
// status.
package redirects

import (
	"net/http"
	"strings"
)

// Refused is client with redirects refused, which an HTTP client of the daemon that carries a
// credential or reads a party's answer as its own is made with, unless its server answers a
// redirect it must follow (SameOrigin). A redirect's Location is the answering party's own words:
// a followed one makes them the URL of the next request, of the answer and of net/http's error
// (its user information and query included), lets whoever answers in the server's place choose
// the host the request goes to next, and keeps the request's credential on the way (net/http
// strips Authorization only on the way to another domain, and strips no other header). The last
// response is handed back instead, which each caller reads as the answer it is: one other than the
// statuses it reads, refused by its status (Status), its Location neither asked nor printed. A
// Location that does not parse is still net/http's error, since Go parses it before it asks
// CheckRedirect (net/http/client.go of go1.27.3, lines 655-665 before 711-717): every error of a
// request that got no answer prints through redact.Unanswered, which never prints net/http's own
// words.
//
// It is a copy and never the caller's own client, as envoy's internal/redirects.Refused is, which
// this module cannot import: a client a caller passes in belongs to that caller (a test passes
// httptest's), and a CheckRedirect written onto it would change how its other calls behave. The
// copy shares the transport, so it shares the connection pool and costs nothing.
func Refused(client *http.Client) *http.Client {
	refusing := *client
	refusing.CheckRedirect = refuse
	return &refusing
}

// refuse is the CheckRedirect of a client Refused made: the response that redirected is the answer.
func refuse(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// SameOrigin is client following a redirect only to the scheme and host its first request was
// sent to, ten at most, as Go's own policy counts them, and answering any other redirect itself,
// as a client Refused made does. It is for a server that answers a redirect its client must
// follow on its own origin: GitHub's REST API answers a renamed repository's path with a 301 to
// /repositories/<id> on the same host, which the daemon reads the repository's name now from. A
// Location naming any other host is the answering party's own words, and is never asked, so the
// request's credential goes to no host but the one it was sent to. A copy, as Refused makes.
func SameOrigin(client *http.Client) *http.Client {
	within := *client
	within.CheckRedirect = sameOrigin
	return &within
}

// sameOrigin is the CheckRedirect of a client SameOrigin made: next, the redirect's request, is
// asked only on the scheme and host of via[0], the first request, and within ten hops.
func sameOrigin(next *http.Request, via []*http.Request) error {
	first := via[0].URL
	if len(via) >= 10 || next.URL.Scheme != first.Scheme || !strings.EqualFold(next.URL.Host, first.Host) {
		return http.ErrUseLastResponse
	}
	return nil
}

// Status reports whether code is a redirect's (3xx): the answer itself under a client Refused
// made, which a caller that reads only some statuses as answers of its own refuses by its status.
func Status(code int) bool {
	return code >= http.StatusMultipleChoices && code < http.StatusBadRequest
}
