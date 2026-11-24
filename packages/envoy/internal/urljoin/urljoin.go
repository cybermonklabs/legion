// Package urljoin builds a request URL from a configured URL's text and a path: the path joined
// onto the configured URL's path, the configured query kept ahead of the path's own, user
// information, host and port kept, and the fragment, which no request sends, dropped. A path
// appended to a configured URL's text sits inside its query or its fragment where it has one
// (http://host/api?key=v with /v1/x appended is http://host/api?key=v/v1/x), so the request goes
// to another path and the query's value changes. Join is how this module builds a request URL from
// a configured URL.
//
// Neither error quotes either text. url.Parse's error quotes the URL whole, user information and
// query included, so Join answers in fixed words (ErrBase, ErrRef) and its caller says in its own
// which URL it could not use.
//
// packages/daemon's internal/urljoin is the same code and the same tests, for that module, which
// Go's internal-package rule refuses this package: a change to one is made to the other.
package urljoin

import (
	"errors"
	"net/url"
	"strings"
)

// ErrBase is Join's answer for a base that does not parse, has no scheme or has no host.
var ErrBase = errors.New("urljoin: the URL is not an absolute URL with a host (it is not printed)")

// ErrRef is the answer for a ref that does not parse as a relative reference, or holds a scheme, a
// host or a fragment. A ref written in code - fixed text that neither begins with two slashes nor
// holds a colon in its first segment, followed by elements url.PathEscape wrote, which escapes
// every '%', '?', '#' and control byte - never draws it, so a caller whose ref is such reads any
// error of Join as ErrBase.
var ErrRef = errors.New("urljoin: the path to join is not a relative reference without a host or a fragment (it is not printed)")

// Join is base, a configured URL's text, with ref joined onto it. ref is a path, with or without
// a leading slash, and optionally its own query (/v1/x, v1/x, /v1/x?a=b); it never names a scheme
// or a host. The result is base as url.Parse reads it, with:
//   - its path joined with ref's as url.URL.JoinPath joins them: a leading slash in ref does not
//     replace base's path, the joined path is cleaned of . and .. elements and repeated slashes,
//     and ref's trailing slash is kept. ref's path is read as already escaped, so an element
//     url.PathEscape wrote (an escaped "/" as %2F, a space as %20) reaches the request as written;
//     a ref with no path leaves base's path as it is;
//   - base's query first and ref's after, the two raw texts joined by "&", neither decoded or
//     encoded again;
//   - base's user information, host and port;
//   - no fragment.
//
// A base that does not parse, has no scheme or has no host is ErrBase; a ref that is not a
// relative reference, or holds a scheme, a host or a fragment, is ErrRef.
func Join(base string, ref string) (*url.URL, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, ErrBase
	}
	return JoinURL(parsed, ref)
}

// JoinURL is Join for a base already parsed: base is left as it was, and ErrRef is its only error.
func JoinURL(base *url.URL, ref string) (*url.URL, error) {
	// url.Parse reads "//" as the start of a host, and everything after the first "#" as a fragment.
	if strings.HasPrefix(ref, "//") || strings.Contains(ref, "#") {
		return nil, ErrRef
	}
	parsed, err := url.Parse(ref)
	if err != nil || parsed.Scheme != "" {
		return nil, ErrRef
	}
	joined := *base
	if path := parsed.EscapedPath(); path != "" {
		// url.URL.JoinPath leaves the path of a URL with a host and no path relative ("sse"), which
		// String writes with the slash it lacks; "/" is that URL's path as a request asks it.
		if joined.Host != "" && joined.Path == "" {
			joined.Path = "/"
		}
		joined = *joined.JoinPath(path)
	}
	switch {
	case parsed.RawQuery == "":
	case joined.RawQuery == "":
		joined.RawQuery = parsed.RawQuery
	default:
		joined.RawQuery += "&" + parsed.RawQuery
	}
	joined.Fragment, joined.RawFragment = "", ""
	return &joined, nil
}
