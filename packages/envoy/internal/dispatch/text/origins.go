package text

import (
	"net/url"
	"strings"
)

// Origins is the web origins one Dispatch answers at, whose dashboard URLs text reads as
// references: the canonical origin (DISPATCH_SERVER_URL) first, then any other it also answers at
// (DISPATCH_OUTSIDE_ORIGIN). A link written on one origin is a reference whichever origin reads it.
// The zero value names none, so text read with it reads only `dispatch://` references.
type Origins struct {
	// urls is each origin parsed, the canonical one first.
	urls []*url.URL
}

// NewOrigins is the origins canonical and others name, in that order, each written as a URL with an
// optional base path. A value that is empty, or names no scheme and host, is skipped, so a
// deployment with no outside origin reads exactly the canonical one.
func NewOrigins(canonical string, others ...string) Origins {
	var origins Origins
	for _, raw := range append([]string{canonical}, others...) {
		parsed, err := url.Parse(raw)
		if raw == "" || err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		origins.urls = append(origins.urls, parsed)
	}
	return origins
}

// base is the origin whose scheme and host page has, compared as written, or nil when it is none of
// them.
func (o Origins) base(page *url.URL) *url.URL {
	for _, origin := range o.urls {
		if page.Scheme == origin.Scheme && page.Host == origin.Host {
			return origin
		}
	}
	return nil
}

// Hosts is each origin's host name in lower case, the canonical one first.
func (o Origins) Hosts() []string {
	hosts := make([]string, 0, len(o.urls))
	for _, origin := range o.urls {
		hosts = append(hosts, strings.ToLower(origin.Hostname()))
	}
	return hosts
}
