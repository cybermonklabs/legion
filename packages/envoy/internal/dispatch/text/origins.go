package text

import (
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/weborigin"
)

// Origins is the web origins of one Dispatch whose dashboard URLs text reads as references: the
// canonical origin (DISPATCH_SERVER_URL) first, then each other address it serves
// (DISPATCH_SERVED_ORIGINS) and each it served before (DISPATCH_FORMER_ORIGINS). A link written on
// one origin is a reference whichever origin reads it, a former origin's included, so a link keeps
// its target after its address stops serving. Origins reads links and decides nothing else: which
// address a request is answered at is the router's. The zero value names none, so text read with it
// reads only `dispatch://` references.
type Origins struct {
	// urls is each origin parsed, the canonical one first.
	urls []*url.URL
}

// NewOrigins is the origins canonical and others name, in that order (parseOrigins).
func NewOrigins(canonical string, others ...string) Origins {
	return parseOrigins(append([]string{canonical}, others...))
}

// ServerOrigins is the list of origins a Dispatch server reads dashboard links of, in this order:
// canonical (DISPATCH_SERVER_URL) less one trailing `/`, each of served (DISPATCH_SERVED_ORIGINS)
// and each of former (DISPATCH_FORMER_ORIGINS). It is the one constructor of that list:
// cmd/dispatch's main and rebuild-refs, cmd/dispatch-seed and api.NewDeps build theirs through it,
// so the reference index, the document service, the API's URL parameters, rebuild-refs, the seed's
// mentions and a result line's words read one list. textcheck's
// TestOnlyOneConstructorBuildsTheServersOrigins scans the non-test files of cmd/dispatch,
// cmd/dispatch-seed and internal/dispatch for a call of NewOrigins outside this function, under any
// name their file imports text with or through a dot import. The `/` trimmed is not redundant with
// parseServer, which trims one from each base path: together they read a server URL ending in `//`
// as the one ending in `/`.
func ServerOrigins(canonical string, served, former []string) Origins {
	raw := make([]string, 0, 1+len(served)+len(former))
	raw = append(raw, strings.TrimSuffix(canonical, "/"))
	raw = append(raw, served...)
	return parseOrigins(append(raw, former...))
}

// parseOrigins is the origins raw names, in its order, each written as a URL with an optional base
// path. A value that is empty, or names no scheme and host, is skipped, so a deployment with no
// server URL reads only the others.
func parseOrigins(raw []string) Origins {
	var origins Origins
	for _, value := range raw {
		parsed, err := url.Parse(value)
		if value == "" || err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		origins.urls = append(origins.urls, parsed)
	}
	return origins
}

// base is the origin whose scheme and host page has, or nil when it is none of them. Both are
// compared as the web page compares them when it reads a link's origin: without regard to case,
// and with the scheme's default port dropped, since a browser lowercases a link's scheme and host
// and leaves out a default port. So `https://B.EXAMPLE.TEST/issues/X` and
// `https://b.example.test:443/issues/X` are the reference `https://b.example.test/issues/X` is.
func (o Origins) base(page *url.URL) *url.URL {
	host := weborigin.WithoutDefaultPort(page.Scheme, page.Host)
	for _, origin := range o.urls {
		if strings.EqualFold(page.Scheme, origin.Scheme) && strings.EqualFold(host, weborigin.WithoutDefaultPort(origin.Scheme, origin.Host)) {
			return origin
		}
	}
	return nil
}

// Canonical is the first origin of the list, the canonical one, written as a URL with its base
// path, or empty for the zero value. A reader that resolves an address with no scheme resolves it
// against this origin (specform).
func (o Origins) Canonical() string {
	if len(o.urls) == 0 {
		return ""
	}
	return o.urls[0].String()
}

// Hosts is each origin's host name in lower case, the canonical one first.
func (o Origins) Hosts() []string {
	hosts := make([]string, 0, len(o.urls))
	for _, origin := range o.urls {
		hosts = append(hosts, strings.ToLower(origin.Hostname()))
	}
	return hosts
}
