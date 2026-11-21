package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// EnforceCookieOrigin is the cross-site check every write a browser's credential carries is held
// to: a POST, PUT, PATCH or DELETE that presents a session cookie or an IAP assertion (under IAP
// the browser sends IAP's cookie, for which IAP adds a valid assertion) and no Authorization
// header runs only when its Origin is this deployment's public origin (PublicOrigin), or when it
// sends no Origin and the browser says the request is same-origin (Sec-Fetch-Site). Any other is
// 403 {"error": "invalid request origin"}, before the route sees it. A bearer, and a request with
// neither credential, pass through: a page on another site cannot make a browser send either.
//
// It is the one check: routes.New wraps the whole server in it, and register wraps the route
// table the command bar calls in-process in it too (server.inProcess). A request the bar sends
// in-process names its caller on its context and carries no cookie and no assertion, so it passes
// here as every request with neither credential does. It is a call of the agent process, which
// reaches the server over its Unix socket and never from a browser, or a request of a plan's read
// or of an undo, which a person's own request made; the browser requests that approved a run's
// plan or pressed its undo were held to the origin on their way in.
func EnforceCookieOrigin(serverURL string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !unsafeMethod(req.Method) || strings.TrimSpace(req.Header.Get("Authorization")) != "" || !auth.HasSessionCookie(req) && !identity.HasIAPAssertion(req) {
			next.ServeHTTP(w, req)
			return
		}
		if origin := req.Header.Get("Origin"); origin != "" && origin == PublicOrigin(serverURL, req) {
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

// PublicOrigin is Dispatch's configured canonical public origin whenever it is available. A
// backend request's Host and TLS state are used only for unconfigured local deployments;
// forwarded headers are deliberately ignored.
func PublicOrigin(serverURL string, req *http.Request) string {
	if parsed, err := url.Parse(serverURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
	}
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, req.Host)
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
