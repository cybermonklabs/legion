package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// RequestOrigin is the router's reading of the address a request came to, by its Host, from its
// one table of the server's origins (routes: hostOrigins): the canonical origin
// (DISPATCH_SERVER_URL), the served addresses (DISPATCH_SERVED_ORIGINS) and the former addresses
// (DISPATCH_FORMER_ORIGINS). routes.BuildAppContext hands the API that table's own methods
// (DepsInput.RequestOrigin), so the cookie-origin check (EnforceCookieOrigin) and the command
// bar's in-process requests (goingBarDoer) answer a request at the address the router answers it
// at. No code of this package reads a request's Host itself (textcheck's
// TestNoCodeOutsideTheOriginTablePicksByHost).
type RequestOrigin struct {
	// Public is the origin req came to (routes: hostOrigins.publicOrigin): the served address whose
	// host and port its Host names, else the canonical origin, or, where ServerURL names none, the
	// origin its own Host and TLS state make; "" on a former address's host, at any port.
	Public func(req *http.Request) string
	// AtFormer reports whether req's Host names a former address's host, at any port (routes:
	// hostOrigins.atFormer).
	AtFormer func(req *http.Request) bool
}

// canonicalOrigin is the RequestOrigin of a server no router built, which serves one address
// (NewDeps with no DepsInput.RequestOrigin, as this package's tests build a server): every request
// came to serverURL's origin, its scheme and host, or to none, "", where serverURL names none; and
// no Host is a former address's.
func canonicalOrigin(serverURL string) RequestOrigin {
	origin := ""
	if parsed, err := url.Parse(serverURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		origin = parsed.Scheme + "://" + parsed.Host
	}
	return RequestOrigin{
		Public:   func(*http.Request) string { return origin },
		AtFormer: func(*http.Request) bool { return false },
	}
}

// EnforceCookieOrigin is the cross-site check every write a browser's credential carries is held
// to: a POST, PUT, PATCH or DELETE that presents a session cookie or an IAP assertion (under IAP
// the browser sends IAP's cookie, for which IAP adds a valid assertion) and no Authorization
// header runs only when its Origin is the origin of the request's own Host (origin.Public: the
// canonical origin or a served address), or when it sends no Origin and the browser says the
// write is same-origin (Sec-Fetch-Site): so neither another site's page nor a page on one of the
// server's addresses can write as a person signed in on another. At a former address's host, at
// any port, every such request is refused, whatever its Origin and Sec-Fetch-Site say
// (origin.AtFormer): the server no longer serves that address, and a page there writes as no one.
// A document socket (opensDocumentSocket) is such a write there too, though its upgrade is a GET:
// it carries the person's edits on the cookie alone, and it reads no Authorization header, so one
// beside the cookie makes it no agent's. Everywhere else it is left to the socket's own upgrade,
// which holds it to the page's origin. A refusal is 403 {"error": "invalid request origin"},
// before the route sees the request. A bearer, and a request with neither credential, pass
// through: a page on another site cannot make a browser send either.
//
// It is the one check: routes.New wraps the whole server in it at the router's table (routes:
// router.enforceCookieOrigin), and register wraps the route table the command bar calls in-process
// in it at the same table (Deps.RequestOrigin, server.inProcess). Each request the bar sends for a
// person comes to the address the person's own request came to (goingBarDoer), so a write the bar
// makes is judged as the same write from the page at that address would be. Such a request names
// its caller on its context and carries no cookie and no assertion, so it passes here as every
// request with neither credential does. It is a call of the agent process, which reaches the
// server over its Unix socket and never from a browser, or a request of a plan's read or of an
// undo, which a person's own request made; the browser requests that approved a run's plan or
// pressed its undo were held to the origin on their way in.
func EnforceCookieOrigin(origin RequestOrigin, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The method and the headers are read before any cookie is parsed, as most requests are
		// safe ones that open no document socket.
		docSocket := opensDocumentSocket(req)
		if !docSocket && (!unsafeMethod(req.Method) || strings.TrimSpace(req.Header.Get("Authorization")) != "") ||
			!auth.HasSessionCookie(req) && !identity.HasIAPAssertion(req) {
			next.ServeHTTP(w, req)
			return
		}
		if origin.AtFormer(req) {
			WriteJSON(w, http.StatusForbidden, map[string]string{"error": "invalid request origin"})
			return
		}
		if docSocket {
			next.ServeHTTP(w, req)
			return
		}
		if sent := req.Header.Get("Origin"); sent != "" && sent == origin.Public(req) {
			next.ServeHTTP(w, req)
			return
		}
		if req.Header.Get("Origin") == "" && req.Header.Get("Sec-Fetch-Site") == "same-origin" {
			next.ServeHTTP(w, req)
			return
		}
		WriteJSON(w, http.StatusForbidden, map[string]string{"error": "invalid request origin"})
	})
}

// unsafeMethod reports whether a request of method changes something, which is what the origin
// check holds a browser's credential, a session cookie or an IAP assertion, to.
func unsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// opensDocumentSocket reports whether req opens a document room's socket (GET /ws/doc/{room}),
// which writes a document as the person whose session cookie opened it.
func opensDocumentSocket(req *http.Request) bool {
	return req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/ws/doc/")
}
