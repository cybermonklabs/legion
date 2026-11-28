package routes

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/sjawhar/envoy/internal/weborigin"
)

// hostOrigins is the router's one table of the origins it picks among by a request's Host: the
// canonical origin (ServerURL), the served addresses (DISPATCH_SERVED_ORIGINS) and the former
// addresses (DISPATCH_FORMER_ORIGINS). Every pick of an origin by Host is one of its methods
// (textcheck's TestNoCodeOutsideTheOriginTablePicksByHost), and each member of the router that
// picks by Host reads it (TestOneTableAnswersEachPickByHost).
//
// A Host is a served address's as weborigin.SameHost compares them: the name without case or a
// trailing dot, the port as a number and the scheme's default port equal to none, so the same name
// at another port is another origin, and the canonical origin answers it, as it answers every Host
// the table does not name. A Host is a former address's host as weborigin.SameHostName compares
// them, at any port: a browser keeps one host's cookies for every port of it. cmd/dispatch refuses
// at boot every two of these addresses whose host names are one (checkServedOrigins), so no Host is
// the host of two addresses.
type hostOrigins struct {
	// canonical is ServerURL's origin, scheme://host, or "" where ServerURL names none, an
	// unconfigured local deployment, where each request's own Host and TLS state make its origin.
	canonical string
	// served and former are the served and the former addresses.
	served, former []tableOrigin
	// origins is what GET /auth/whoami and GET /api/v1/whoami name as the server's origins
	// (originsAt): the canonical origin, each served and each former address, in that order; nil
	// where the table names no origin but the canonical one.
	origins []string
}

// tableOrigin is one address of the table: as weborigin.Parse writes it (origin), which is what
// the table answers, and parsed (url), which is what a request's Host is compared with.
type tableOrigin struct {
	origin string
	url    *url.URL
}

// servesHost reports whether host, a request's Host, names o's host and port under o's scheme
// (weborigin.SameHost).
func (o tableOrigin) servesHost(host string) bool {
	return weborigin.SameHost(o.url, &url.URL{Scheme: o.url.Scheme, Host: host})
}

// namesHost reports whether host, a request's Host, names o's host name at any port
// (weborigin.SameHostName).
func (o tableOrigin) namesHost(host string) bool {
	return weborigin.SameHostName(o.url, &url.URL{Host: host})
}

// newHostOrigins is the table of serverURL, the canonical origin, and of served and former, each an
// origin weborigin.Parse takes. BuildAppContext builds the one a router reads, once, and refuses
// an entry that is no origin, and either list beside a serverURL that names none.
func newHostOrigins(serverURL string, served, former []string) (hostOrigins, error) {
	var table hostOrigins
	if parsed, err := url.Parse(serverURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		table.canonical = parsed.Scheme + "://" + parsed.Host
	}
	if len(served) == 0 && len(former) == 0 {
		return table, nil
	}
	if table.canonical == "" {
		return hostOrigins{}, fmt.Errorf("ServedOrigins and FormerOrigins need a ServerURL, the canonical origin each is beside")
	}
	var err error
	if table.served, err = tableOrigins("ServedOrigins", served); err != nil {
		return hostOrigins{}, err
	}
	if table.former, err = tableOrigins("FormerOrigins", former); err != nil {
		return hostOrigins{}, err
	}
	table.origins = make([]string, 0, 1+len(served)+len(former))
	table.origins = append(table.origins, table.canonical)
	for _, address := range slices.Concat(table.served, table.former) {
		table.origins = append(table.origins, address.origin)
	}
	return table, nil
}

// tableOrigins is each of entries, the list name names, as weborigin.Parse reads it.
func tableOrigins(name string, entries []string) ([]tableOrigin, error) {
	origins := make([]tableOrigin, 0, len(entries))
	for index, entry := range entries {
		parsed, err := weborigin.Parse(entry)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d of %d: %w", name, index+1, len(entries), err)
		}
		origins = append(origins, tableOrigin{origin: parsed.String(), url: parsed})
	}
	return origins, nil
}

// publicOrigin is the origin req came to: none, "", on a former address's host, at any port
// (atFormer), since a sign-in or a write that started there as the canonical origin's would end at
// another host; the served address whose host and port req's Host names; else the canonical
// origin, whenever ServerURL names one. A backend request's Host and TLS state make the origin only
// for unconfigured local deployments. Forwarded headers are deliberately ignored.
func (o hostOrigins) publicOrigin(req *http.Request) string {
	if o.atFormer(req) {
		return ""
	}
	for _, served := range o.served {
		if served.servesHost(req.Host) {
			return served.origin
		}
	}
	if o.canonical != "" {
		return o.canonical
	}
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + req.Host
}

// callbackURL is the sign-in's callback at the origin req came to (publicOrigin): a sign-in that
// starts at a served address comes back to it. googleSignInHere serves no sign-in on a former
// address's host, where publicOrigin names none.
func (o hostOrigins) callbackURL(req *http.Request) string {
	return o.publicOrigin(req) + "/auth/callback"
}

// atFormer reports whether req's Host names a former address's host, at any port: the former
// fence, behind which the router serves no sign-in (googleSignInHere) and no browser write
// (enforceCookieOrigin).
func (o hostOrigins) atFormer(req *http.Request) bool {
	return slices.ContainsFunc(o.former, func(former tableOrigin) bool { return former.namesHost(req.Host) })
}

// originsAt is what GET /auth/whoami names as the server's origins to req, and GET /api/v1/whoami
// too (api.Deps.OriginsAt), so the dispatch command reads the list the page reads: the canonical
// origin, each served and each former address, at every Host; nil where neither list is set, so
// the answer of a server at one address is what it always was.
func (o hostOrigins) originsAt(*http.Request) []string {
	return o.origins
}
