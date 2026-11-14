package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// EnforceCookieOrigin is the cross-site check every write a browser's session cookie carries is
// held to: a POST, PUT, PATCH or DELETE that presents the cookie and no Authorization header runs
// only when its Origin is this deployment's public origin (PublicOrigin), or when it sends no
// Origin and the browser says the request is same-origin (Sec-Fetch-Site). Any other is 403
// {"error": "invalid request origin"}, before the route sees it. A bearer, and a request with no
// cookie, pass through: a page on another site cannot make a browser send either.
//
// It is the one check: routes.New wraps the whole server in it, and register wraps the route
// table the going writes call in-process in it too (server.inProcess), so a write the command bar
// performs as its caller is held to the origin exactly as the caller's own request was.
func EnforceCookieOrigin(serverURL string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !unsafeMethod(req.Method) || strings.TrimSpace(req.Header.Get("Authorization")) != "" || !auth.HasSessionCookie(req) {
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
// check holds a cookie to.
func unsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}
