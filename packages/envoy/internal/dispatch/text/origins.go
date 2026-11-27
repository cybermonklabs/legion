package text

import (
	"net/url"
	"strings"
)

// Origins is the web origins one Dispatch answers at, whose dashboard URLs text reads as
// references: the canonical origin (DISPATCH_SERVER_URL) first, then any other it also answers at.
// A link written on one origin is a reference whichever origin reads it. No build passes a second
// origin yet: cmd/dispatch reads DISPATCH_SERVER_URL alone, and the second origin
// (DISPATCH_OUTSIDE_ORIGIN) is set from slice S6a. The zero value names none, so text read with it
// reads only `dispatch://` references.
type Origins struct {
	// canonical is the canonical origin parsed, nil where its value names none.
	canonical *url.URL
	// urls is each origin parsed, the canonical one first where its value names one.
	urls []*url.URL
}

// NewOrigins is the origins canonical and others name, in that order, each written as a URL with an
// optional base path. A value that is empty, or names no scheme and host, is skipped, so a
// deployment with no outside origin reads exactly the canonical one. A canonical value so skipped
// leaves the origins with no canonical one: another origin is still read, and is never taken for
// it (Canonical).
func NewOrigins(canonical string, others ...string) Origins {
	var origins Origins
	if parsed, ok := parseOrigin(canonical); ok {
		origins.canonical = parsed
		origins.urls = append(origins.urls, parsed)
	}
	for _, raw := range others {
		if parsed, ok := parseOrigin(raw); ok {
			origins.urls = append(origins.urls, parsed)
		}
	}
	return origins
}

// parseOrigin is raw parsed as an origin, ok false where it is empty or names no scheme and host.
func parseOrigin(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(raw)
	if raw == "" || err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, false
	}
	return parsed, true
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

// Canonical is the canonical origin as it was written (DISPATCH_SERVER_URL), and empty where the
// value names none, whatever other origin the Dispatch answers at: what a caller that still
// carries one URL of the deployment reads, until it carries the origins themselves. Every such
// caller builds the deployment's own links and first lines from it, as it did from the one URL,
// so it is never the outside origin.
func (o Origins) Canonical() string {
	if o.canonical == nil {
		return ""
	}
	return o.canonical.String()
}

// Hosts is each origin's host name in lower case, the canonical one first where its value names
// one.
func (o Origins) Hosts() []string {
	hosts := make([]string, 0, len(o.urls))
	for _, origin := range o.urls {
		hosts = append(hosts, strings.ToLower(origin.Hostname()))
	}
	return hosts
}
