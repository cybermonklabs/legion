// Package natsurl holds the rules a NATS server URL must meet for the NATS client (nats.go) to
// read it as written, with the fixed reasons a refusal of one gives, for a caller in either module:
// the Envoy bus's Dial and Connect, natsprobe, and the Legion daemon's configuration and its NATS
// connection (packages/daemon/internal/config readNatsURLs, natsauth.Connect). It is one rule in
// one place, so the two modules cannot refuse different URLs or word the refusal differently.
//
// The rules exist because a credential written in a URL's userinfo that holds a "/", "?", "#",
// space or comma ends url.Parse's authority early, is no userinfo it reads, or is split from the
// rest of the URL by a caller that splits its list on commas. A password whose head is followed by a
// "/" makes url.Parse read that head as the port, and its error quotes it (`invalid port ":<the
// password's head>" after host`); a token holding a "/" written as the URL's user parses, the
// token's head as the host, which nats.go looks up and names as a host it could not reach; and a
// token or password holding a comma reaches nats.go as two URLs, the credential's head, which can
// parse, and its tail, its "@" and the host. nats.go refuses a URL it cannot parse with url.Parse's
// error, which quotes the URL whole. A refusal under these rules names the URL by its position in
// its list alone, with a reason of fixed words: any text of such a URL can be a part of the
// credential.
//
// A path, a query or a fragment with no "@" is kept: nats.go sends a websocket URL's path and
// query in its handshake (nats.go v1.55.0 ws.go wsInitHandshake), and the daemon's configuration
// takes a path. Like natsversion, it is a door for the daemon's module, which Go's internal rule
// keeps out of the envoy module's internal packages.
package natsurl

import (
	"net/url"
	"slices"
	"strings"
)

// The reasons a NATS server URL is refused for. They are fixed words, never url.Parse's, whose
// text quotes the part of the URL it could not read.
const (
	UnparsedReason         = `it does not parse as a URL; write a credential in it percent-encoded (a "/" as %2F, a comma as %2C)`
	UserinfoPastHostReason = `an "@" stands after its host, as it does where a credential in its userinfo holds a "/", "?" or "#"; write such a credential percent-encoded`
	HandshakeReason        = `its path does not parse once decoded, as the NATS client's websocket handshake decodes it; write a "%" in its path as %2525`
	// HandshakeHostReason is HandshakeReason's for a host that stops parsing once decoded, as one
	// holding an IPv6 zone or an encoded "%" does.
	HandshakeHostReason = `its host does not parse once decoded, as the NATS client's websocket handshake decodes it; write the host as it is read, with no percent-encoding`
	// CommaReason is a comma's in one URL: the NATS client reads one as the end of a server URL, so
	// a URL that holds one is a list of servers or a credential holding a comma.
	CommaReason = `it holds a comma, which the NATS client reads as a list of servers: list each server as its own nats_urls entry, and write a comma in a credential percent-encoded (%2C)`
	// TailReason is a later URL's, of a list a caller split on its commas, that carries a user name
	// or password and names none of the schemes NATS servers are written with: what a comma in a
	// credential leaves after it. In a nats or tls list nats.go dials such a URL too, a scheme-less
	// one as nats:// and one of any other scheme as plain NATS, so a list written so connected
	// before; in a ws list it dials a scheme-less one as ws or wss and refuses one of another
	// scheme as mixing (nats.go:2299-2300). It is refused all the same: a user name or password
	// written in a URL is a credential there, and where a comma split it, the part before the comma
	// is a URL nats.go dials, its head read as a host and named as the host it could not reach.
	TailReason = `it carries a user name or password and names none of the schemes nats, tls, ws and wss, as the part after a comma in a credential does once the list is split on its commas; in a nats or tls list such a URL connected before, but a credential split there has its head dialled and printed as a host: name the URL's scheme, and write a comma in a credential percent-encoded (%2C)`
)

// schemes are the schemes NATS servers are written with. In a nats or tls list nats.go dials a URL
// of any other scheme too, as plain NATS; the tail rule refuses one that carries a user name or
// password.
var schemes = []string{"nats", "tls", "ws", "wss"}

// Refusal is why nats.go would read server, one trimmed server URL, other than as written, and ""
// when it would not: read as nats.go reads it, nats:// added where it names no scheme, it does not
// parse; it parses with an "@" after its host (UserinfoPastHost); or it is a websocket URL whose
// handshake URL does not parse (handshakeRefusal).
func Refusal(server string) string { return refusal(server, "nats") }

// refusal is Refusal with scheme added where server names none, as nats.go adds its pool's.
func refusal(server, scheme string) string {
	if !strings.Contains(server, "://") {
		server = scheme + "://" + server
	}
	parsed, err := url.Parse(server)
	switch {
	case err != nil:
		return UnparsedReason
	case UserinfoPastHost(parsed):
		return UserinfoPastHostReason
	}
	return handshakeRefusal(parsed)
}

// EntryRefusal is Refusal for one URL of a list its caller joins with commas before nats.go reads
// it, as the Legion daemon's nats_urls are: a comma in the URL ends it there, so the URL is a list
// of servers or a credential holding a comma (CommaReason), and otherwise Refusal's reason.
func EntryRefusal(server string) string {
	if strings.Contains(server, ",") {
		return CommaReason
	}
	return Refusal(server)
}

// Check reads urls, a list a caller split on its commas, as nats.go reads the list it dials: each
// entry trimmed, and an empty one dropped (nats.go v1.55.0 processUrlString, nats.go:1968-1979,
// which Connect runs on its URL string). It returns those entries, the list to dial: nats.go parses
// a server as written, so an untrimmed URL would pass the rules trimmed and reach nats.go
// untrimmed, whose parse error quotes it whole. It also returns the 1-based position, in urls, of
// the first URL nats.go would read other than as written, with its reason (Refusal, and for a URL
// after the first TailReason), or 0 and "".
//
// A later URL that names no scheme is read with the pool's, as nats.go reads it
// (parseServerURL, connScheme): the websocket scheme of the first URL it adds, the first that is
// not empty, as url.Parse reads that URL's scheme (addURLToPool and isWebsocketScheme,
// nats.go:2296-2298), so a ws or wss list's later scheme-less URL goes through the websocket
// handshake rule too, and nats:// otherwise. url.Parse, not the text before "://": a first URL
// such as `ws:<opaque>://…` is a ws URL to nats.go.
func Check(urls []string) (dialled []string, position int, reason string) { return check(urls, true) }

// CheckEntries is Check for a list whose entries are whole, none split from a credential: the
// Legion daemon's nats_urls, whose loader and natsauth.Connect refuse a comma in any entry first.
// It runs the per-URL rules and the pool's scheme, but not the tail rule, which guards only a
// comma-split credential; a later entry with a user name and any scheme connects, as nats.go
// dials it.
func CheckEntries(urls []string) (dialled []string, position int, reason string) {
	return check(urls, false)
}

// check is Check, with the tail rule when tail is set.
func check(urls []string, tail bool) (dialled []string, position int, reason string) {
	pool := "nats"
	for index, raw := range urls {
		server := strings.TrimSpace(raw)
		if server == "" {
			continue
		}
		first := len(dialled) == 0
		dialled = append(dialled, server)
		if first {
			pool = poolScheme(server)
		}
		scheme, _, named := strings.Cut(server, "://")
		scheme = strings.ToLower(scheme)
		if tail && !first && strings.Contains(server, "@") && (!named || !slices.Contains(schemes, scheme)) {
			return dialled, index + 1, TailReason
		}
		if why := refusal(server, pool); why != "" {
			return dialled, index + 1, why
		}
	}
	return dialled, 0, ""
}

// poolScheme is the scheme nats.go gives a later scheme-less URL of a list whose first URL is
// first: ws or wss when url.Parse reads first, nats:// added where it names none, with a websocket
// scheme (connScheme), and nats otherwise.
func poolScheme(first string) string {
	if !strings.Contains(first, "://") {
		first = "nats://" + first
	}
	if parsed, err := url.Parse(first); err == nil {
		if scheme := strings.ToLower(parsed.Scheme); scheme == "ws" || scheme == "wss" {
			return scheme
		}
	}
	return "nats"
}

// UserinfoPastHost reports whether parsed, a NATS server URL that parsed, holds an "@" after its
// host, in its path, query or fragment. That is where a credential holding a "/", "?" or "#" ends
// url.Parse's authority before its "@", so the URL's host is a part of the credential and every
// line that names the host names it.
func UserinfoPastHost(parsed *url.URL) bool {
	return strings.Contains(parsed.Path, "@") || strings.Contains(parsed.RawQuery, "@") || strings.Contains(parsed.Fragment, "@")
}

// handshakeRefusal is why parsed, a NATS server URL that parsed, fails nats.go's websocket
// handshake, or "" when it is no websocket URL or one whose handshake URL parses. The handshake
// builds a URL of the host, the decoded path and the raw query and parses it again, and its error
// quotes that URL whole, its query, and a token in it, included. The reason names the part that
// fails: the host alone, or the path and query after it.
func handshakeRefusal(parsed *url.URL) string {
	if !strings.EqualFold(parsed.Scheme, "ws") && !strings.EqualFold(parsed.Scheme, "wss") {
		return ""
	}
	if _, err := url.Parse("http://" + parsed.Host); err != nil {
		return HandshakeHostReason
	}
	rebuilt := "http://" + parsed.Host + parsed.Path
	if parsed.RawQuery != "" {
		rebuilt += "?" + parsed.RawQuery
	}
	if _, err := url.Parse(rebuilt); err != nil {
		return HandshakeReason
	}
	return ""
}
