package routes

// Dev sign-in: GET /auth/_dev/signin issues the session cookie an issuer sign-in issues to the
// person `login` names, with no issuer exchange, so a local instance can be driven signed-in
// through the production cookie identity. cmd/dispatch turns it on with DISPATCH_DEV_SIGNIN=1
// behind its boot fence, and BuildAppContext mounts it only for a loopback origin. Everything that
// can mint a cookie without the sign-in issuer is in this file.

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"unicode"
)

// DevSignInOrigin returns the host:port of serverURL, the dashboard origin, when it names this
// machine, and refuses it otherwise: a deployment's origin is its public one, so the dev sign-in
// route cannot take effect there. The host must be "localhost" or a loopback IP literal.
func DevSignInOrigin(serverURL string) (string, error) {
	parsed, ok := parseLoopbackURL(serverURL)
	if !ok {
		return "", fmt.Errorf("DISPATCH_DEV_SIGNIN=1 is for a loopback origin only: the dashboard origin %q (DISPATCH_SERVER_URL, or dispatch.serverUrl in envoy.json) must name 127.0.0.1, [::1] or localhost", serverURL)
	}
	return parsed.Host, nil
}

// LoopbackURL reports whether raw parses as a URL whose host LoopbackName accepts. The host is
// url.URL.Hostname: userinfo, a query or a fragment cannot stand in for it, and a raw with no "//"
// has none, so "localhost:9022" is refused as a scheme and an opaque path.
func LoopbackURL(raw string) bool {
	_, ok := parseLoopbackURL(raw)
	return ok
}

func parseLoopbackURL(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || !LoopbackName(parsed.Hostname()) {
		return nil, false
	}
	return parsed, true
}

// LoopbackName reports whether host is "localhost", in any case, or a loopback IP literal.
func LoopbackName(host string) bool {
	return strings.EqualFold(host, "localhost") || LoopbackIP(host)
}

// LoopbackIP reports whether host is an IP literal for a loopback address, with no zone and not an
// IPv4-mapped IPv6 address. Nothing is resolved: a name that points at this machine does not count.
// host carries no brackets; net.SplitHostPort and url.URL.Hostname remove them.
func LoopbackIP(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Zone() == "" && !addr.Is4In6() && addr.IsLoopback()
}

// LoopbackHostPort reports whether address, a host:port such as a listen address or a request's
// RemoteAddr, names a loopback IP literal as LoopbackIP judges it.
func LoopbackHostPort(address string) bool {
	host, _, err := net.SplitHostPort(address)
	return err == nil && LoopbackIP(host)
}

// forwardingHeaders are the headers a proxy adds to a request it relays. The dev sign-in route
// refuses a request carrying any of them, whatever its value: the peer it sees is then the proxy,
// not the browser.
var forwardingHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "Via"}

// authDevSignIn is the local workflow's sign-in: authCallback's recording of the person and
// issueSession, with the pool's exchange and group check left out. The person is named as the
// production identities name them, trimmed and lowercased. Only a loopback peer that no proxy
// forwarded is served, and every mint is logged at WARN. The browser is sent to the page `next`
// names on the dashboard origin (devSignInReturn), the dashboard's root without one, and a next
// off that origin is refused before anyone is recorded or any cookie minted.
func (r *router) authDevSignIn(w http.ResponseWriter, req *http.Request) {
	if !LoopbackHostPort(req.RemoteAddr) {
		writeCodeError(w, http.StatusForbidden, fmt.Sprintf("dev sign-in serves loopback peers only, not %s", req.RemoteAddr), "DEV_SIGNIN_FORBIDDEN")
		return
	}
	for _, name := range forwardingHeaders {
		if _, present := req.Header[http.CanonicalHeaderKey(name)]; present {
			writeCodeError(w, http.StatusForbidden, "dev sign-in refuses a forwarded request: "+name+" is set", "DEV_SIGNIN_FORBIDDEN")
			return
		}
	}
	email := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("login")))
	if email == "" {
		writeCodeError(w, http.StatusBadRequest, "login query parameter required", "DEV_SIGNIN_INPUT")
		return
	}
	page, ok := devSignInReturn(req.URL.Query().Get("next"), r.ctx.devSignInOrigin)
	if !ok {
		writeCodeError(w, http.StatusBadRequest, "next must name a page of the dashboard origin "+r.ctx.devSignInOrigin, "DEV_SIGNIN_INPUT")
		return
	}
	if err := r.ctx.People.Record(req.Context(), email); err != nil {
		slog.Error("dispatch: dev sign-in could not record the person", "email", email, "error", err)
		writeError(w, http.StatusInternalServerError, "record person")
		return
	}
	if !r.issueSession(w, req, email, "") {
		return
	}
	slog.Warn("dispatch: dev sign-in minted a session cookie", "email", email, "remote_addr", req.RemoteAddr)
	w.Header().Set("Cache-Control", "no-store")
	// The origin is the configured dashboard origin, never anything the request sent, and the "/"
	// after it ends the authority: whatever page holds, the browser stays on that origin.
	http.Redirect(w, req, r.ctx.devSignInOrigin+"/"+strings.TrimPrefix(page, "/"), http.StatusFound)
}

// devSignInReturn is the page, a path with its query and fragment, that next names on origin, the
// dashboard's scheme://host:port: a path-absolute reference, or an absolute URL of that origin in
// any case, whose empty path is the root. An empty next is the root. It refuses everything else,
// so the sign-in sends a browser nowhere but its own origin: another origin, a network-path
// reference (//host, ///host) and a path that reads as one once unescaped, userinfo, a relative
// path, and a backslash or control character anywhere, which browsers read as a slash or drop.
func devSignInReturn(next, origin string) (string, bool) {
	if next == "" {
		return "/", true
	}
	if strings.HasPrefix(next, "//") || strings.Contains(next, `\`) || strings.IndexFunc(next, unicode.IsControl) >= 0 {
		return "", false
	}
	parsed, err := url.Parse(next)
	if err != nil || parsed.Opaque != "" || parsed.User != nil {
		return "", false
	}
	page := parsed.EscapedPath()
	if parsed.Scheme != "" || parsed.Host != "" {
		if parsed.Scheme == "" || !strings.EqualFold(parsed.Scheme+"://"+parsed.Host, origin) {
			return "", false
		}
		if page == "" {
			page = "/"
		}
	}
	if !strings.HasPrefix(page, "/") || strings.HasPrefix(parsed.Path, "//") ||
		strings.Contains(parsed.Path, `\`) || strings.IndexFunc(parsed.Path, unicode.IsControl) >= 0 {
		return "", false
	}
	if parsed.RawQuery != "" {
		page += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		page += "#" + parsed.EscapedFragment()
	}
	return page, true
}

// requireHost refuses every request whose Host is not host, the dashboard origin's host:port,
// compared without regard to case: a browser that reached this server through another name (a
// DNS-rebinding page, a tunnel) is told so instead of being answered as the dashboard.
func requireHost(host string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.EqualFold(req.Host, host) {
			writeCodeError(w, http.StatusMisdirectedRequest, fmt.Sprintf("request Host %q is not the dashboard origin %q", req.Host, host), "HOST_MISMATCH")
			return
		}
		next.ServeHTTP(w, req)
	})
}
