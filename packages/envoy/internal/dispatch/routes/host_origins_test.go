package routes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The addresses of the server these rows drive: its canonical origin (DISPATCH_SERVER_URL), a
// served address (DISPATCH_SERVED_ORIGINS) and a former address (DISPATCH_FORMER_ORIGINS).
const (
	tableCanonical = "https://a.example.test"
	tableServed    = "https://b.example.test"
	tableFormer    = "https://old.example.test"
)

// tableHost is a Host a row's request carries: the host of one of the server's addresses, or of
// none (unknown), at that address's port, the default one, or at another (8443).
type tableHost struct {
	host      string
	address   string
	otherPort bool
}

// tableHosts are the Hosts each member is driven at.
var tableHosts = []tableHost{
	{"a.example.test", "canonical", false},
	{"a.example.test:8443", "canonical", true},
	{"b.example.test", "served", false},
	{"b.example.test:8443", "served", true},
	{"old.example.test", "former", false},
	{"old.example.test:8443", "former", true},
	{"other.example.test", "unknown", false},
	{"other.example.test:8443", "unknown", true},
}

// noServedOrigin is the answer of publicOrigin that names none of the origins the server answers
// at: the canonical and the served origin.
const noServedOrigin = "(none of the server's origins)"

// sameOriginWrite is the write enforceCookieOrigin admits with no Origin, the browser saying with
// Sec-Fetch-Site that it is same-origin.
const sameOriginWrite = "no Origin, Sec-Fetch-Site: same-origin"

// requestAt is a request of method for path whose Host is host.
func requestAt(method, host, path string) *http.Request {
	return httptest.NewRequest(method, "http://"+host+path, nil)
}

// TestOneTableAnswersEachPickByHost: every member of the router that picks an origin, or decides a
// request, by its Host answers from the one table (hostOrigins) of the canonical origin, the served
// addresses and the former addresses, driven at the host of each and of none, at its default port
// and at another:
//   - a served address is an origin the server answers as itself, matched as weborigin.SameHost
//     matches (the name without case or a trailing dot, the port as a number), so at its own Host
//     the cookie check takes its Origin and the Google sign-in comes back to its callback, and at
//     another port of its host the canonical origin answers, as it answers any other Host;
//   - a former address's host, at any port, is served no sign-in (404) and no browser write that
//     carries a credential (403, the write with no Origin and Sec-Fetch-Site: same-origin
//     included), and the canonical origin is not what it is answered as, since a sign-in started
//     there would end at another host.
//
// The answers that are no member's of a served or a former address - every one at the canonical
// and an unknown Host - are what the router answered before either list existed.
func TestOneTableAnswersEachPickByHost(t *testing.T) {
	rig := newServedRig(t)
	r, handler := &router{ctx: rig.ctx}, rig.handler
	served := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, member := range []struct {
		name string
		// answer is what the member answers a request at host.
		answer func(t *testing.T, host string) string
		// want is what it must answer there.
		want func(host tableHost) string
	}{
		{
			name: "publicOrigin",
			answer: func(t *testing.T, host string) string {
				switch origin := r.ctx.hosts.publicOrigin(requestAt(http.MethodGet, host, "/")); origin {
				case tableCanonical, tableServed:
					return origin
				default:
					return noServedOrigin
				}
			},
			want: func(host tableHost) string {
				switch {
				case host.address == "former":
					return noServedOrigin
				case host.address == "served" && !host.otherPort:
					return tableServed
				}
				return tableCanonical
			},
		},
		{
			// The callback the Google sign-in's start sends the issuer (redirect_uri), or the 404
			// a path no route serves answers.
			name: "callbackURL",
			answer: func(t *testing.T, host string) string {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, requestAt(http.MethodGet, host, "/auth/start"))
				switch response.Code {
				case http.StatusNotFound:
					return "404"
				case http.StatusFound:
					location, err := url.Parse(response.Header().Get("Location"))
					if err != nil {
						t.Fatalf("GET /auth/start at Host %s: Location %q: %v", host, response.Header().Get("Location"), err)
					}
					return location.Query().Get("redirect_uri")
				}
				return fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String()))
			},
			want: func(host tableHost) string {
				switch {
				case host.address == "former":
					return "404"
				case host.address == "served" && !host.otherPort:
					return tableServed + "/auth/callback"
				}
				return tableCanonical + "/auth/callback"
			},
		},
		{
			// The writes carrying a session cookie that the check lets through: each Origin of the
			// server's addresses and of another site, and a write with no Origin.
			name: "enforceCookieOrigin",
			answer: func(t *testing.T, host string) string {
				var admitted []string
				for _, origin := range []string{tableCanonical, tableServed, tableFormer, "https://other.example.test", ""} {
					request := requestAt(http.MethodPost, host, "/api/v1/issues")
					request.AddCookie(&http.Cookie{Name: "dsession", Value: "a-session"})
					label := origin
					if origin == "" {
						request.Header.Set("Sec-Fetch-Site", "same-origin")
						label = sameOriginWrite
					} else {
						request.Header.Set("Origin", origin)
					}
					response := httptest.NewRecorder()
					r.enforceCookieOrigin(served).ServeHTTP(response, request)
					switch {
					case response.Code == http.StatusNoContent:
						admitted = append(admitted, label)
					case response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "invalid request origin"):
						t.Fatalf("POST at Host %s with %s: %d %s, want it served or 403 invalid request origin", host, label, response.Code, response.Body.String())
					}
				}
				if len(admitted) == 0 {
					return "none"
				}
				return strings.Join(admitted, ", ")
			},
			want: func(host tableHost) string {
				switch {
				case host.address == "former":
					return "none"
				case host.address == "served" && !host.otherPort:
					return tableServed + ", " + sameOriginWrite
				}
				return tableCanonical + ", " + sameOriginWrite
			},
		},
		{
			// Whether the Google sign-in's start and callback are served, or answer 404.
			name: "googleSignInHere",
			answer: func(t *testing.T, host string) string {
				response := httptest.NewRecorder()
				r.googleSignInHere(served)(response, requestAt(http.MethodGet, host, "/auth/start"))
				switch response.Code {
				case http.StatusNoContent:
					return "served"
				case http.StatusNotFound:
					return "404"
				}
				return strconv.Itoa(response.Code)
			},
			want: func(host tableHost) string {
				if host.address == "former" {
					return "404"
				}
				return "served"
			},
		},
	} {
		t.Run(member.name, func(t *testing.T) {
			for _, host := range tableHosts {
				t.Run(host.host, func(t *testing.T) {
					if got, want := member.answer(t, host.host), member.want(host); got != want {
						t.Errorf("%s at Host %s (the %s address's host, other port %t) = %q, want %q", member.name, host.host, host.address, host.otherPort, got, want)
					}
				})
			}
		})
	}
}
