package redirects

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// TestARefusedClientAnswersTheRedirectAndLeavesTheCallersClientAsItWas: a stand-in answers 302
// naming a target that counts what it is asked. The client Refused makes answers the 302 itself and
// asks the target nothing; the caller's client, which Refused copied, still follows the redirect,
// and the copy shares its transport.
func TestARefusedClientAnswersTheRedirectAndLeavesTheCallersClientAsItWas(t *testing.T) {
	var asked atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
	}))
	defer target.Close()
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/moved", http.StatusFound)
	}))
	defer moved.Close()
	callers := &http.Client{Transport: &http.Transport{}}
	refusing := Refused(callers)
	if refusing == callers || refusing.Transport != callers.Transport || callers.CheckRedirect != nil {
		t.Fatalf("Refused(%p) = %p: want a copy sharing the transport, the caller's client unchanged", callers, refusing)
	}
	response, err := refusing.Get(moved.URL)
	if err != nil {
		t.Fatalf("the refusing client's read = %v, want the 302 answered", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusFound || asked.Load() != 0 {
		t.Fatalf("the refusing client answered %d and asked the target %d times, want 302 and none", response.StatusCode, asked.Load())
	}
	response, err = callers.Get(moved.URL)
	if err != nil {
		t.Fatalf("the caller's client's read = %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || asked.Load() != 1 {
		t.Fatalf("the caller's client answered %d and asked the target %d times, want it to follow as before", response.StatusCode, asked.Load())
	}
}

// TestASameOriginClientFollowsARedirectOnlyOnItsOwnOrigin: a stand-in answers 301 naming a path on
// its own host, which the client SameOrigin makes follows and reads, and 302 naming a target on
// another host, which it answers itself, asking the target nothing; the caller's client is
// unchanged.
func TestASameOriginClientFollowsARedirectOnlyOnItsOwnOrigin(t *testing.T) {
	var asked atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/renamed":
			http.Redirect(w, r, "/repositories/7", http.StatusMovedPermanently)
		case "/elsewhere":
			http.Redirect(w, r, target.URL+"/moved", http.StatusFound)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer server.Close()
	callers := &http.Client{}
	within := SameOrigin(callers)
	if within == callers || callers.CheckRedirect != nil {
		t.Fatalf("SameOrigin(%p) = %p: want a copy, the caller's client unchanged", callers, within)
	}
	for path, want := range map[string]int{"/renamed": http.StatusTeapot, "/elsewhere": http.StatusFound} {
		response, err := within.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s = %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Errorf("GET %s answered %d, want %d", path, response.StatusCode, want)
		}
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("the target on another host was asked %d times, want none", n)
	}
}

// TestSameOriginURLIsTheSchemeAndTheHostWithItsPort: a URL is on an origin when its scheme and
// its host with its port are the origin's, the host in any case; its user information, path and
// query are not compared.
func TestSameOriginURLIsTheSchemeAndTheHostWithItsPort(t *testing.T) {
	origin := &url.URL{Scheme: "https", Host: "mcp.example:8443", Path: "/sse"}
	for raw, want := range map[string]bool{
		"https://mcp.example:8443/message?session_id=1": true,
		"https://someone@MCP.Example:8443/":             true,
		"https://mcp.example:8444/sse":                  false,
		"https://mcp.example/sse":                       false,
		"http://mcp.example:8443/sse":                   false,
		"https://other.example:8443/sse":                false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q) = %v", raw, err)
		}
		if got := SameOriginURL(u, origin); got != want {
			t.Errorf("SameOriginURL(%s, %s) = %v, want %v", raw, origin, got, want)
		}
	}
}

// TestStatusIsTheThreeHundreds: a redirect's status is 300 to 399, and no other.
func TestStatusIsTheThreeHundreds(t *testing.T) {
	for code, want := range map[int]bool{
		http.StatusOK: false, 299: false, http.StatusMultipleChoices: true, http.StatusFound: true,
		http.StatusPermanentRedirect: true, 399: true, http.StatusBadRequest: false,
	} {
		if got := Status(code); got != want {
			t.Errorf("Status(%d) = %v, want %v", code, got, want)
		}
	}
}
