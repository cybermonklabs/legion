package redact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// planted is the credential every case writes into a URL, so that one search of an output finds
// any part of a credential the rule left.
const planted = "SECRET"

// plantedUserinfo is a userinfo that carries planted as its password. A case joins it to a URL
// with "+" so the line that writes the URL holds no literal user:password@host, which the push's
// leak check refuses wherever it is written.
const plantedUserinfo = "u:" + planted

// redirectError is the error Go's HTTP client gives when a tarball read that GitHub redirected to
// its codeload host fails: the redirect's URL, with its token in the query, quoted in the text.
func redirectError() error {
	return &url.Error{
		Op:  "Get",
		URL: "https://codeload.github.com/o/r/legacy.tar.gz/refs/tags/v1?token=" + planted,
		Err: errors.New("dial tcp 127.0.0.1:9: connect: connection refused"),
	}
}

// urlCredentialsCases are the rule's cases: each a text, and what the rule leaves of it.
var urlCredentialsCases = []struct {
	name, text, want string
}{
	{
		name: "a redirect's url.Error",
		text: redirectError().Error(),
		want: `Get "https://codeload.github.com/o/r/legacy.tar.gz/refs/tags/v1": dial tcp 127.0.0.1:9: connect: connection refused`,
	},
	{
		name: "a userinfo with a password",
		text: "clone https://user:" + planted + "@github.com/o/r.git failed",
		want: "clone https://" + URLWithCredentials + "/o/r.git failed",
	},
	{
		name: "a token as the user name",
		text: "fetch https://" + planted + "@github.com/o/r.git: exit status 128",
		want: "fetch https://" + URLWithCredentials + "/o/r.git: exit status 128",
	},
	{
		name: "an authority with two @, nothing of it kept",
		text: "https://a@" + planted + "@host.example:8443/p",
		want: "https://" + URLWithCredentials + "/p",
	},
	{
		// A password holding an unencoded "@" and no host after it: its tail is not printed as a
		// host, as it was when the rule kept what followed the last "@" (`ssword` here).
		name: "a password holding an @ and no host",
		text: "dial http://user:p" + "@" + planted + "ssword no such host",
		want: "dial http://" + URLWithCredentials + " no such host",
	},
	{
		name: "a password holding an @, then a host",
		text: "dial http://user:p" + "@" + planted + "ssword@host.example/x refused",
		want: "dial http://" + URLWithCredentials + "/x refused",
	},
	{
		name: "a URL inside %q quotes",
		text: fmt.Sprintf("parse %q: invalid port", "postgres://"+plantedUserinfo+"@db.example:5432/legion?sslmode=disable&password="+planted),
		want: `parse "postgres://` + URLWithCredentials + `/legion": invalid port`,
	},
	{
		name: "a URL inside <…>",
		text: "see <https://u:" + planted + "@host.example/x?sig=" + planted + "> now",
		want: "see <https://" + URLWithCredentials + "/x> now",
	},
	{
		name: "a URL inside single quotes and one inside backticks",
		text: "'https://host.example/?t=" + planted + "' and `https://host.example/?t=" + planted + "`",
		want: "'https://host.example/' and `https://host.example/`",
	},
	{
		name: "two URLs in one text",
		text: "a https://h.example/?t=" + planted + " then HTTP://" + planted + "@g.example:8080/p?q=" + planted + "#f",
		want: "a https://h.example/ then HTTP://" + URLWithCredentials + "/p#f",
	},
	{
		name: "a query after a port with no path",
		text: "dial http://h.example:8080?key=" + planted,
		want: "dial http://h.example:8080",
	},
	{
		name: "a query before a fragment",
		text: "https://github.com/o/r/blob/main/f.go?token=" + planted + "#L10",
		want: "https://github.com/o/r/blob/main/f.go#L10",
	},
	{
		name: "a query that ends the text",
		text: "go to https://h.example/p?" + planted,
		want: "go to https://h.example/p",
	},
	{
		name: "a URL that ends at a Unicode white space",
		text: "https://h.example/?t=" + planted + "\u00a0more",
		want: "https://h.example/\u00a0more",
	},
	{
		// The inner URL's "@" is past the outer one's host too, so the outer is written whole.
		name: "a URL in the path of another",
		text: "https://archive.example/web/https://u:" + planted + "@h.example/x?t=" + planted,
		want: "https://" + URLWithCredentials,
	},
	{
		name: "a URL in the fragment of another",
		text: "https://h.example/#next=https://" + planted + "@g.example/?t=" + planted + "#top",
		want: "https://" + URLWithCredentials,
	},
	{
		name: "a URL in the query of another",
		text: "https://h.example/login?next=https://" + planted + "@g.example/ done",
		want: "https://" + URLWithCredentials + " done",
	},
	{
		// A credential holding a "?" written as the user: url.Parse reads its head as the host,
		// and nothing of the head is printed.
		name: "an @ in the query, past a head read as the host",
		text: "dial https://madeUpHead0123?" + planted + "@host.example/x refused",
		want: "dial https://" + URLWithCredentials + " refused",
	},
	{
		name: "an @ in the path, past a head read as the host",
		text: "dial https://madeUpHead0123/" + planted + "@host.example/x refused",
		want: "dial https://" + URLWithCredentials + " refused",
	},
	{
		name: "a URL with no @ past its host keeps its path",
		text: "https://h.example/a/b?x=1#f",
		want: "https://h.example/a/b#f",
	},
	{
		name: "a fragment kept",
		text: "dispatch://KEY/spec#b-r1",
		want: "dispatch://KEY/spec#b-r1",
	},
	{
		name: "a ? in the fragment kept",
		text: "https://h.example/p#which?that",
		want: "https://h.example/p#which?that",
	},
	{
		// An "@" past the host is where a credential holding a "/" written as the user lands, so a
		// path's own "@" (a scoped npm package, a user page) is written as the mark too: the
		// decided cost of reading no "@" past a host as safe.
		name: "an @ in the path takes the mark",
		text: "https://h.example/@user/p",
		want: "https://" + URLWithCredentials,
	},
	{
		// A file URL names a path on this machine and has no userinfo to hold a credential: a
		// scoped package's "@" stays, as the plugin module URL `legion controller start` names.
		name: "a file URL keeps its path's @",
		text: "loaded pi-envoy from file:///home/u/.omp/plugins/node_modules/@sjawhar/pi-envoy/dist/envoy.js?mtime=1 which",
		want: "loaded pi-envoy from file:///home/u/.omp/plugins/node_modules/@sjawhar/pi-envoy/dist/envoy.js?mtime=1 which",
	},
	{
		name: "a FILE URL in upper case keeps its path's @",
		text: "FILE:///srv/@scope/x",
		want: "FILE:///srv/@scope/x",
	},
	{
		// A scheme that ends in "file" is another scheme: the rule holds for it.
		name: "a scheme that ends in file takes the mark",
		text: "notfile://h.example/@user/p",
		want: "notfile://" + URLWithCredentials,
	},
	{
		// An "@" in a file URL's query (here a URL with a credential written in it) marks the file
		// URL whole, as it would any other.
		name: "an @ in a file URL's query",
		text: "see file:///plugins/https://h.example/p?u=" + planted + "@g.example end",
		want: "see file://" + URLWithCredentials + " end",
	},
	{
		// The local file URL's query is pending when the URL in its path, with an "@" past its
		// host, is written whole as the mark: the mark covers that query, which is not removed
		// again.
		name: "a URL with an @ past its host in a file URL's path",
		text: "see file:///plugins/https://h.example/@x/p?u=1 end",
		want: "see file:///plugins/https://" + URLWithCredentials + " end",
	},
	{
		name: "a localhost file URL keeps its path's @",
		text: "file://LocalHost/srv/@scope/x",
		want: "file://LocalHost/srv/@scope/x",
	},
	{
		// A file URL that names a host is no local path: a credential holding a "/", "?" or "#"
		// written as its user puts the head where the host goes, and every such URL takes the mark.
		name: "a file URL with a host and an @ in its path",
		text: "file://madeUpHead0123/" + planted + "@host.example/x",
		want: "file://" + URLWithCredentials,
	},
	{
		name: "a file URL with a host and an @ in its fragment",
		text: "File://madeUpHead0123#" + planted + "@host.example/x",
		want: "File://" + URLWithCredentials,
	},
	{
		name: "a file URL with a host and an @ in its query",
		text: "file://madeUpHead0123?" + planted + "@host.example/x",
		want: "file://" + URLWithCredentials,
	},
	{
		// A local file URL whose query holds a URL with a password holding a "#": the "@" stands in
		// the file URL's fragment, which marks it whole.
		name: "a local file URL with an @ in its fragment",
		text: "file:///a?q=https://u:madeUpHead0123#" + planted + "@h/",
		want: "file://" + URLWithCredentials,
	},
	{
		name: "a ? in prose with no URL",
		text: "is it done? yes, at 10:30 // or later?",
		want: "is it done? yes, at 10:30 // or later?",
	},
	{
		name: "a :// after no scheme",
		text: "9://u@h.example/?q and ://u@h.example/?q",
		want: "9://u@h.example/?q and ://u@h.example/?q",
	},
	{
		name: "a file: URL's query kept",
		text: "loaded file:///a.js?mtime=1, then",
		want: "loaded file:///a.js?mtime=1, then",
	},
	{
		name: "a FILE: URL's query kept",
		text: "loaded FILE:///a.js?mtime=1, then",
		want: "loaded FILE:///a.js?mtime=1, then",
	},
	{
		name: "a file: URL's userinfo written as the mark, its query kept",
		text: "file://" + planted + "@host.example/a.js?mtime=1",
		want: "file://" + URLWithCredentials + "/a.js?mtime=1",
	},
	{
		name: "two file: URLs as legion probe-image prints them",
		text: "pi-envoy loaded 2 times (file:///e/dist/envoy.js?mtime=1, file:///e/dist/envoy.js?mtime=2): the worker image's profile links it beside the explicit root",
		want: "pi-envoy loaded 2 times (file:///e/dist/envoy.js?mtime=1, file:///e/dist/envoy.js?mtime=2): the worker image's profile links it beside the explicit root",
	},
	{
		name: "a query whose names are harmless and a credential's removed whole",
		text: "get https://h/p?mtime=1&X-Amz-Signature=" + planted + " done",
		want: "get https://h/p done",
	},
	{
		name: "a scheme that ends in file is no file: URL",
		text: "xfile://h.example/p?token=" + planted + " and profile://h.example/q?" + planted,
		want: "xfile://h.example/p and profile://h.example/q",
	},
	{
		name: "a URL in a file: URL's query is a URL of its own",
		text: "file:///a.js?next=https://h.example/?token=" + planted + "#f",
		want: "file:///a.js?next=https://h.example/#f",
	},
	{
		// An "@" in a file: URL's query is past that URL's host, so the file: URL is the mark
		// whole, as any URL with an "@" past its host is.
		name: "a URL with userinfo in a file: URL's query marks the file: URL",
		text: "file:///a.js?next=https://" + planted + "@h.example/?token=" + planted + "#f",
		want: "file://" + URLWithCredentials,
	},
	{
		name: "a file: URL in another's query goes with that query",
		text: "https://h.example/p?a=file:///a.js?mtime=1&token=" + planted + " done",
		want: "https://h.example/p done",
	},
	{
		// A refusal of a plan quotes a pinned need with the comma of its sentence after it: the
		// pin's "@" is the reference's one "@" past its host, in its fragment, and stays.
		name: "a Dispatch reference pinned in its fragment keeps its pin",
		text: "task d needs dispatch://ACME-2/artifact/plan-md#b-a@v2, which names another issue",
		want: "task d needs dispatch://ACME-2/artifact/plan-md#b-a@v2, which names another issue",
	},
	{
		name: "a Dispatch reference pinned in its path keeps its pin",
		text: "see dispatch://CORE-1/artifact/design-md@v12.",
		want: "see dispatch://CORE-1/artifact/design-md@v12.",
	},
	{
		// A credential holding a "/" written as the user of a dispatch: URL: what follows its "@"
		// is a host, not a pin.
		name: "a Dispatch reference with a host after its @ takes the mark",
		text: "dispatch://CORE-1/" + planted + "@host.example/x",
		want: "dispatch://" + URLWithCredentials,
	},
	{
		name: "a Dispatch reference with two @ past its host takes the mark",
		text: "dispatch://CORE-1/spec#b-" + planted + "@v2@v3",
		want: "dispatch://" + URLWithCredentials,
	},
	{
		name: "a Dispatch reference whose @ is in its query takes the mark",
		text: "dispatch://CORE-1/spec?" + planted + "@v2",
		want: "dispatch://" + URLWithCredentials,
	},
	{
		name: "a version with a leading zero is no pin",
		text: "dispatch://CORE-1/spec#b-" + planted + "@v02",
		want: "dispatch://" + URLWithCredentials,
	},
	{
		name: "a pin followed by a word takes the mark",
		text: "dispatch://CORE-1/spec#b-x@v2" + planted + " end",
		want: "dispatch://" + URLWithCredentials + " end",
	},
	{
		name: "a pin on another scheme is no pin",
		text: "dial https://" + planted + "/x@v2 refused",
		want: "dial https://" + URLWithCredentials + " refused",
	},
	{
		// Dispatch reads `dispatch://` in lower case alone.
		name: "DISPATCH in upper case is no Dispatch reference",
		text: "DISPATCH://CORE-1/spec#b-" + planted + "@v2",
		want: "DISPATCH://" + URLWithCredentials,
	},
	{
		name: "a scheme that ends in dispatch is another scheme",
		text: "xdispatch://CORE-1/spec#b-" + planted + "@v2",
		want: "xdispatch://" + URLWithCredentials,
	},
	{
		// The pin leaves the reference whole past its authority, and its userinfo is written as
		// the mark as any other.
		name: "a Dispatch reference's userinfo takes the mark, and its pin stays",
		text: "dispatch://" + plantedUserinfo + "@CORE-1/spec#b-x@v2",
		want: "dispatch://" + URLWithCredentials + "/spec#b-x@v2",
	},
	{
		// The "@" of the pin is past the https URL's host, which is no Dispatch reference.
		name: "a pinned Dispatch reference in another URL's path marks that URL",
		text: "https://h.example/see/dispatch://CORE-1/spec#b-x@v2 then",
		want: "https://" + URLWithCredentials + " then",
	},
	{
		// The pinned reference is a URL of its own in the path of a file URL of this machine,
		// whose "@" past its host is the pin's, in its path: both stay whole.
		name: "a pinned Dispatch reference in a local file URL's path",
		text: "file:///notes/dispatch://CORE-1/artifact/notes-md@v3 end",
		want: "file:///notes/dispatch://CORE-1/artifact/notes-md@v3 end",
	},
}

// HostShown writes a URL's host as the mark in an error that names it, and prints no error for a
// host that is not ASCII, whose ASCII form (`xn--…`) the error names instead. The hosts are made up.
func TestHostShownPrintsNoHostOfAURLWithCredentials(t *testing.T) {
	lookup := errors.New("dial tcp: lookup madeuptail0123.invalid: no such host")
	if got := HostShown("madeuptail0123.invalid", "", lookup); got != "dial tcp: lookup "+URLWithCredentials+": no such host" {
		t.Errorf("an ASCII host = %q, want it written as the mark", got)
	}
	if got := HostShown("madeuptail0123\u00f6.invalid", "", errors.New("dial tcp: lookup xn--madeuptail0123-8ib.invalid: no such host")); got != NotASCIIHostError {
		t.Errorf("a host that is not ASCII = %q, want %q", got, NotASCIIHostError)
	}
	if got := HostShown("", "", errors.New("dial tcp: connect: connection refused")); got != "dial tcp: connect: connection refused" {
		t.Errorf("no host = %q, want the text as it is", got)
	}
	// A password's head of digits that url.Parse read as the port: the dialer's connect error names
	// it after the resolved address, and its refusal of an out-of-range port quotes it.
	connect := errors.New("dial tcp 192.0.2.7:48213: connect: connection refused")
	if got := HostShown("legion", "48213", connect); got != "dial tcp 192.0.2.7:"+URLWithCredentials+": connect: connection refused" {
		t.Errorf("a port = %q, want it written as the mark", got)
	}
	if got := HostShown("legion", "1", errors.New("dial tcp 192.0.2.7:12345: connect: connection refused")); got != "dial tcp 192.0.2.7:12345: connect: connection refused" {
		t.Errorf("a port that begins another = %q, want the other kept", got)
	}
	invalid := &net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Err: "invalid port", Addr: "98765432"}}
	if got := HostShown("legion", "98765432", invalid); got != AddressError {
		t.Errorf("an address the dialer refused = %q, want %q", got, AddressError)
	}
}

// A port written with leading zeros (a password's head of digits that url.Parse read as the port,
// `0048213`) is dialed as a number and printed without them: a real request to a closed port
// written so prints neither the port as written nor its digits, and the password stays out. The
// password is made up.
func TestRequestErrorMasksAPortWrittenWithLeadingZeros(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	route := "http://op:" + "madeuphead0123" + "@127.0.0.1:00" + port + "/x"
	response, err := http.Get(route)
	if err == nil {
		response.Body.Close()
		t.Fatal("the request to a closed port got an answer")
	}
	shown := RequestError("the server", route, err).Error()
	if !strings.Contains(shown, "dial tcp "+URLWithCredentials+":"+URLWithCredentials+": ") {
		t.Errorf("RequestError = %q, want the dial's host and port written as the mark", shown)
	}
	for _, part := range []string{":" + port, ":00" + port, "madeuphead"} {
		if strings.Contains(shown, part) {
			t.Errorf("RequestError = %q, which holds %q", shown, part)
		}
	}
}

// RequestError names a request's route through URL and its error through the URL rule and
// HostShown, and unwraps to the request's own error, so a deadline and a *url.Error are still read.
// A route that does not parse is named in fixed words. The credentials are made up.
func TestRequestErrorNamesNoCredentialAndKeepsTheRequestsError(t *testing.T) {
	route := "http://legion:madeuphead0123" + "@madeuptail0123.invalid/v1/x"
	dial := &url.Error{Op: "Get", URL: route, Err: errors.New("dial tcp: lookup madeuptail0123.invalid: no such host")}
	err := RequestError("the server", route, dial)
	if want := "http://" + URLWithCredentials + "/v1/x: dial tcp: lookup " + URLWithCredentials + ": no such host"; err.Error() != want {
		t.Errorf("RequestError = %q, want %q", err, want)
	}
	var transport *url.Error
	if !errors.As(err, &transport) || transport != dial {
		t.Errorf("RequestError unwraps to %v, want the request's *url.Error", errors.Unwrap(err))
	}
	timedOut := &url.Error{Op: "Get", URL: route, Err: context.DeadlineExceeded}
	if err := RequestError("the server", route, timedOut); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RequestError(%v) is not a deadline", err)
	}
	notParsed := &url.Error{Op: "parse", URL: route, Err: errors.New(`invalid port ":madeuphead0123" after host`)}
	if err := RequestError("the server", route, notParsed); err.Error() != "the server: its address does not parse as a URL" {
		t.Errorf("RequestError(a parse error) = %q", err)
	}
	if err := RequestError("the server", "http://server.example/v1/x?token=madeUpQueryToken0123", errors.New("dial tcp: connection refused")); err.Error() != "http://server.example/v1/x: dial tcp: connection refused" {
		t.Errorf("RequestError(no credential) = %q, want the route without its query and the error as it is", err)
	}
}

func TestURLCredentialsRemovesTheQueryAndTheUserinfoOfEveryURL(t *testing.T) {
	for _, tc := range urlCredentialsCases {
		t.Run(tc.name, func(t *testing.T) {
			got := URLCredentials(tc.text)
			if got != tc.want {
				t.Fatalf("URLCredentials(%q) = %q, want %q", tc.text, got, tc.want)
			}
			if strings.Contains(got, planted) {
				t.Fatalf("URLCredentials(%q) = %q, which still holds the planted credential", tc.text, got)
			}
			if again := URLCredentials(got); again != got {
				t.Fatalf("URLCredentials(%q) = %q, want it kept as the rule left it", got, again)
			}
		})
	}
}

// schemeRunes are the characters a scheme holds, for urlCredentialsQuadratic's own reading of it.
const schemeRunes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

// pinTail is what follows the "@" of a pinned Dispatch reference up to the URL's end, for
// urlCredentialsQuadratic's own reading of it: "v", a version from 1 with no leading zero, and
// the punctuation Dispatch drops from the end of a reference written in prose.
var pinTail = regexp.MustCompile(`^v[1-9][0-9]*[.,;:!?*_~)]*$`)

// urlCredentialsQuadratic is URLCredentials as it was before it kept its searches: the same rule,
// which for each "://" searches again to the URL's end, three times, so a run of "://" that
// nothing ends (`a://a://…`) takes it time quadratic in the run's length. It is kept as the
// reference URLCredentials's output is held to
// (TestURLCredentialsGivesWhatTheRuleAsFirstWrittenGives): URLCredentials's kept searches are
// right only under an order of the positions it asks from, and this one asks every search afresh.
func urlCredentialsQuadratic(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	var out strings.Builder
	// text[:kept] is written to out or replaced. Every replacement starts after a "://", so kept
	// is 0 until the first one.
	kept := 0
	replace := func(from, to int, with string) {
		if kept == 0 {
			out.Grow(len(text))
		}
		out.WriteString(text[kept:from])
		out.WriteString(with)
		kept = to
	}
	remove := func(from, to int) { replace(from, to, "") }
	// A URL's query is removed once the search for the next "://" passes its start: a URL in the
	// path before it has the same query, and a userinfo of that URL comes first in the text.
	query, fragment := -1, -1
	for at := 0; ; {
		i := strings.Index(text[at:], "://")
		if query >= 0 && (i < 0 || at+i >= query) {
			remove(query, fragment)
			at, query = fragment, -1
			continue
		}
		if i < 0 {
			break
		}
		sep := at + i
		at = sep + len("://")
		if !schemeEndsAt(text, sep) {
			continue
		}
		end := at + orLen(strings.IndexFunc(text[at:], endsURL), len(text)-at)
		host := at + orLen(strings.IndexAny(text[at:end], "/?#"), end-at)
		start := sep
		for start > 0 && strings.IndexByte(schemeRunes, text[start-1]) >= 0 {
			start--
		}
		scheme, authority := text[start:sep], text[at:host]
		pathEnd := host + orLen(strings.IndexAny(text[host:end], "?#"), end-host)
		local := strings.EqualFold(scheme, "file") && (authority == "" || strings.EqualFold(authority, "localhost")) &&
			strings.IndexByte(text[pathEnd:end], '@') < 0
		// A Dispatch reference whose one "@" past its host stands in its path or its fragment,
		// with a pin and the end of a sentence after it.
		fragmentAt := host + orLen(strings.IndexByte(text[host:end], '#'), end-host)
		pin := host + strings.LastIndexByte(text[host:end], '@')
		pinned := scheme == "dispatch" && strings.Count(text[host:end], "@") == 1 &&
			(pin < pathEnd || pin > fragmentAt) && pinTail.MatchString(text[pin+1:end])
		if strings.IndexByte(text[host:end], '@') >= 0 && !local && !pinned {
			replace(at, end, URLWithCredentials)
			at = end
			query = -1
			continue
		}
		if strings.IndexByte(text[at:host], '@') >= 0 {
			replace(at, host, URLWithCredentials)
		}
		if fileSchemeEndsAt(text, sep) {
			continue
		}
		frag := host + orLen(strings.IndexByte(text[host:end], '#'), end-host)
		if q := strings.IndexByte(text[host:frag], '?'); q >= 0 {
			query, fragment = host+q, frag
		}
	}
	if kept == 0 {
		return text
	}
	out.WriteString(text[kept:])
	return out.String()
}

// URLCredentials leaves of every text what the rule as first written leaves of it
// (urlCredentialsQuadratic): of each text of the cases, and of 5000 texts of up to 300 pieces
// built under a fixed seed, each from "a", "://" and about half of the other pieces the rule
// reads, so that URLs in the paths, the queries and the fragments of others, userinfos, invalid
// UTF-8 and every character that ends a URL meet in many orders. What it leaves, it leaves as it
// is.
func TestURLCredentialsGivesWhatTheRuleAsFirstWrittenGives(t *testing.T) {
	var texts []string
	for _, tc := range urlCredentialsCases {
		texts = append(texts, tc.text)
	}
	pieces := []string{"https", "file", "FILE", "localhost", "9", "@", ":", "/", "?", "#", "=", " ", "\u00a0", `"`, "'", "`", "<", ">", "é", "\xff", "dispatch", "v", ","}
	random := rand.New(rand.NewPCG(4193, 4463))
	for range 5000 {
		some := []string{"a", "://"}
		for _, piece := range pieces {
			if random.IntN(2) == 0 {
				some = append(some, piece)
			}
		}
		var text strings.Builder
		for range random.IntN(301) {
			text.WriteString(some[random.IntN(len(some))])
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		got, want := URLCredentials(text), urlCredentialsQuadratic(text)
		if got != want {
			t.Fatalf("URLCredentials(%q) = %q, want %q, what the rule as first written gives", text, got, want)
		}
		if again := URLCredentials(got); again != got {
			t.Fatalf("URLCredentials(%q) = %q, want it kept as the rule left it", got, again)
		}
	}
}

// The rule's time grows as its text's length does, not as its square. A line of "a://" repeated
// is the rule as first written at its worst (urlCredentialsQuadratic searched to the line's end
// again for each "://"): on 1 MiB of it, a rule linear in the length returns in tens of
// milliseconds, and that one took about 15 minutes. The run is held to a deadline of 30 s, far
// from both, rather than to a ratio of the times of two lengths: a machine under load moves such
// a ratio for a correct rule as well, and a quadratic rule would run past go test's 10-minute
// timeout before any ratio was read, where the deadline fails it in 30 s.
func TestURLCredentialsTakesTimeLinearInTheTextsLength(t *testing.T) {
	const deadline = 30 * time.Second
	line := strings.Repeat("a://", (1<<20)/len("a://"))
	done := make(chan string, 1)
	go func() { done <- URLCredentials(line) }()
	select {
	case got := <-done:
		if got != line {
			t.Fatalf("URLCredentials changed a line of a:// repeated, which holds no credential, to one of %d bytes", len(got))
		}
	case <-time.After(deadline):
		t.Fatalf("URLCredentials of 1 MiB of a:// did not return within %v; a rule linear in the length takes tens of milliseconds", deadline)
	}
}

// stringer is a fmt.Stringer that is no error, as a *url.URL is.
type stringer struct{ text string }

func (s stringer) String() string { return s.text }

// Every text of a line takes the rule: the message, a string, an error, a fmt.Stringer, a grouped
// attribute and one a Logger.With carries. A number stays a number, and the caller's own
// ReplaceAttr still runs, before the rule, which reads what it returns.
func TestHandlerOptionsGiveEveryTextOfALineThroughTheURLRule(t *testing.T) {
	var ownCalls []string
	own := func(groups []string, a slog.Attr) slog.Attr {
		ownCalls = append(ownCalls, strings.Join(append(slices.Clone(groups), a.Key), "."))
		switch a.Key {
		case slog.TimeKey:
			return slog.Attr{}
		case "replaced":
			return slog.String("replaced", "by the caller's own https://"+planted+"@h.example/")
		}
		return a
	}
	opts := &slog.HandlerOptions{ReplaceAttr: own}
	var logged bytes.Buffer
	target, err := url.Parse("https://u:" + planted + "@h.example/p?token=" + planted)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(&logged, HandlerOptions(opts))).With("remote", "https://"+planted+"@g.example/o/r.git")
	log.Info("fetch https://h.example/a?token="+planted+" failed",
		"url", "https://"+plantedUserinfo+"@h.example/p?q="+planted,
		"error", redirectError(),
		"target", target,
		"named", stringer{"see https://h.example/?sig=" + planted},
		"count", 3,
		"replaced", "as it was",
		slog.Group("g", "inner", "https://h.example/?t="+planted),
	)
	if strings.Contains(logged.String(), planted) {
		t.Fatalf("the line holds the planted credential: %s", logged.String())
	}
	var line map[string]any
	if err := json.Unmarshal(logged.Bytes(), &line); err != nil {
		t.Fatalf("the line %q is no JSON: %v", logged.String(), err)
	}
	want := map[string]any{
		"level":    "INFO",
		"msg":      "fetch https://h.example/a failed",
		"remote":   "https://" + URLWithCredentials + "/o/r.git",
		"url":      "https://" + URLWithCredentials + "/p",
		"error":    `Get "https://codeload.github.com/o/r/legacy.tar.gz/refs/tags/v1": dial tcp 127.0.0.1:9: connect: connection refused`,
		"target":   "https://" + URLWithCredentials + "/p",
		"named":    "see https://h.example/",
		"count":    3.0,
		"replaced": "by the caller's own https://" + URLWithCredentials + "/",
		"g":        map[string]any{"inner": "https://h.example/"},
	}
	if !reflect.DeepEqual(line, want) {
		t.Fatalf("the line is %v, want %v", line, want)
	}
	for _, key := range []string{slog.TimeKey, slog.MessageKey, "remote", "error", "replaced", "g.inner"} {
		if !slices.Contains(ownCalls, key) {
			t.Fatalf("the caller's own ReplaceAttr saw %q, want %q among them", ownCalls, key)
		}
	}
	// The options given are the caller's still: their ReplaceAttr is the caller's own, unwrapped.
	if a := opts.ReplaceAttr(nil, slog.String("url", "https://"+planted+"@h.example/")); a.Value.String() != "https://"+planted+"@h.example/" {
		t.Fatalf("the caller's options were changed: their ReplaceAttr gave %q", a.Value.String())
	}
}

// Nil options are the zero options with the rule: the default level, and every text cleaned.
func TestHandlerOptionsOfNilAreTheZeroOptionsWithTheURLRule(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, HandlerOptions(nil)))
	log.Debug("not written")
	log.Info("read", "url", "https://"+planted+"@h.example/")
	got := logged.String()
	if strings.Contains(got, "not written") || strings.Contains(got, planted) || !strings.Contains(got, `url="https://`+URLWithCredentials+`/"`) {
		t.Fatalf("logged %q, want the Info line alone, with the URL's authority as the mark of its credentials", got)
	}
}

// A Stringer whose method panics (a nil *url.URL) is written as the handler writes it with no
// rule, and the rule does not turn it into a panic of the caller's log call.
func TestHandlerOptionsLeaveAPanickingStringerToTheHandler(t *testing.T) {
	var logged bytes.Buffer
	var target *url.URL
	slog.New(slog.NewJSONHandler(&logged, HandlerOptions(nil))).Info("read", "target", target)
	if got := logged.String(); !strings.Contains(got, `"target":null`) {
		t.Fatalf("logged %q, want the nil URL written as null", got)
	}
}
