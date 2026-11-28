package routes

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/logtest"
)

// logBuffer is a log sink the router's goroutines may write while the test reads it.
type logBuffer = logtest.Buffer

// routeLines are the access-log lines logged holds for route, in order.
func routeLines(logged *logBuffer, route string) []string {
	var lines []string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, `msg="`+accesslog.Message+`"`) && strings.Contains(line, " route="+route+" ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func captureAccessLog(t *testing.T) *logBuffer {
	t.Helper()
	logged := &logBuffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	return logged
}

// A request for an API path no route answers writes its line too, so a client calling a route
// that does not exist, or a route with a method it does not serve, shows among the callers, with
// the Host it was sent to as it wrote it.
func TestAnUnknownAPIPathWritesOneAccessLogLine(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault, through
	// captureAccessLog).
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/does-not-exist"},
		{http.MethodPost, "/auth/whoami"},
	} {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			handler, _ := newTestRouter(t)
			logged := captureAccessLog(t)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(request.method, "http://A.Example.Test:8443"+request.path, nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d", request.method, request.path, response.Code)
			}
			lines := routeLines(logged, unmatchedRoute)
			if len(lines) != 1 {
				t.Fatalf("an unknown API path wrote %d access-log lines", len(lines))
			}
			for _, want := range []string{`method=` + request.method, `path=` + request.path, `host=A.Example.Test:8443 `, `status=404`, `caller_kind=anonymous`} {
				if !strings.Contains(lines[0], want) {
					t.Errorf("access-log line %q does not carry %q", lines[0], want)
				}
			}
		})
	}
}

// A route's line names the Host the request was sent to, its case and its port as the client
// wrote them, beside the caller.
func TestARoutesLineNamesTheHostItWasSentTo(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault, through
	// captureAccessLog).
	handler, _ := newTestRouter(t)
	logged := captureAccessLog(t)
	request := httptest.NewRequest(http.MethodGet, "http://B.Example.Test:8443/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", "sami@d.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /auth/whoami = %d %s", response.Code, response.Body.String())
	}
	lines := routeLines(logged, "/auth/whoami")
	if len(lines) != 1 {
		t.Fatalf("one whoami wrote %d access-log lines", len(lines))
	}
	for _, want := range []string{`host=B.Example.Test:8443 `, `caller_kind=user`, `caller=sami@d.example`, `status=200`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access-log line %q does not carry %q", lines[0], want)
		}
	}
}

// A sign-in that succeeds names the person it signed in on its callback's line, the Host of the
// address the sign-in came back to and the browser's user agent; no other line of the sign-in
// names the person.
func TestASignInCallbackNamesThePersonItSignedIn(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault, through
	// captureAccessLog).
	rig := newSignInRig(t, "")
	logged := captureAccessLog(t)
	rig.issuer.SignInAs(personClaims("other-group", rigGroup))
	authURL, stateCookie := rig.start(t, "http://dispatch.test/auth/start?next=/issues/CORE-1")
	code, state := rig.issuer.Authorize(t, authURL.String())
	callback := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	callback.AddCookie(stateCookie)
	callback.Header.Set("User-Agent", "sign-in-browser/1.0")
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, callback)
	if response.Code != http.StatusFound {
		t.Fatalf("callback: status %d body=%s", response.Code, response.Body.String())
	}
	lines := routeLines(logged, "/auth/callback")
	if len(lines) != 1 {
		t.Fatalf("one callback wrote %d access-log lines", len(lines))
	}
	for _, want := range []string{`caller_kind=user`, `caller=` + rigEmail, `host=dispatch.test `, `user_agent=sign-in-browser/1.0`, `status=302`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access-log line %q does not carry %q", lines[0], want)
		}
	}
	named := 0
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, rigEmail) {
			named++
		}
	}
	if named != 1 {
		t.Errorf("the sign-in wrote %d log lines naming %s, want its callback's access-log line alone:\n%s", named, rigEmail, logged.String())
	}
}

// A request from a trusted proxy (DISPATCH_TRUSTED_PROXY_RANGES) whose X-Forwarded-For that proxy
// could not have written, a line of one entry here, is answered 400 FORWARDED_FOR_INVALID before
// any route reads it, and writes one line: the refused route, its Host, and the connection's own
// address, never the entry the header names.
func TestARequestTheRouterRefusesForItsForwardedForWritesOneLine(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault, through
	// captureAccessLog).
	handler, _ := newTestRouterWith(t, func(options *AppContextOptions) {
		options.TrustedProxyRanges = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	})
	logged := captureAccessLog(t)

	request := httptest.NewRequest(http.MethodGet, "http://B.Example.Test:8443/auth/whoami", nil)
	request.RemoteAddr = "127.0.0.1:40000"
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	request.Header.Set("X-Dispatch-User", "sami@d.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"FORWARDED_FOR_INVALID"`) {
		t.Fatalf("GET /auth/whoami from the trusted proxy with one X-Forwarded-For entry = %d %s, want 400 FORWARDED_FOR_INVALID", response.Code, response.Body.String())
	}
	if lines := routeLines(logged, "/auth/whoami"); len(lines) != 0 {
		t.Fatalf("the refused request reached its route, which wrote %q", lines)
	}
	lines := routeLines(logged, accesslog.RefusedRoute)
	if len(lines) != 1 {
		t.Fatalf("the refused request wrote %d refused lines:\n%s", len(lines), logged.String())
	}
	for _, want := range []string{`path=/auth/whoami `, `host=B.Example.Test:8443 `, `address=127.0.0.1 `, `caller_kind=anonymous`, `status=400`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access-log line %q does not carry %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "198.51.100.8") {
		t.Errorf("the refused request's line %q names the entry its X-Forwarded-For wrote", lines[0])
	}
}
