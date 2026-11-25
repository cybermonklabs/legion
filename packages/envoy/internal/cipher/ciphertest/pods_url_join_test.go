package ciphertest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestPodsAskAndSignTheBrokersOwnURL: a Base with a path and a query (path and query), or with
// user information (user information). Each request Pods makes, for a service's login, a pod's
// enrollment, a refused enrollment, a request, its state, its cancel and a grant's values, asks
// its route joined onto Base's path with the query kept (urljoin.Join); each proof signs that URL
// without its query or user information (proof.TargetURI), and each request object names Base
// without either as its audience (proof.Audience), which the stand-in broker checks as the broker
// does (record.VerifyRequestObject). Where Pods wrote the route after Base's text, the route went
// inside the query and the stand-in was asked /prefix; where it signed Base's whole text, the
// proofs and the login named user information the broker's URL does not hold. A plain origin
// (origin) is asked and signed as before.
func TestPodsAskAndSignTheBrokersOwnURL(t *testing.T) {
	for _, c := range []struct{ name, user, suffix, path, query string }{
		{"path and query", "", "/prefix?key=k3y-made-up", "/prefix", "key=k3y-made-up"},
		{"user information", "registrar:pw-made-up@", "/prefix", "/prefix", ""},
		{"origin", "", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			var public string
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, r.Method+" "+r.URL.RequestURI())
				route, under := strings.CutPrefix(r.URL.Path, c.path)
				if !under || !strings.HasPrefix(route, "/") || r.URL.RawQuery != c.query {
					t.Errorf("the broker was asked %s %s, want a route under %s/ with the query %q", r.Method, r.URL.RequestURI(), c.path, c.query)
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"code":"NOT_FOUND"}`)
					return
				}
				if signed := r.Header.Get("Proof"); signed != "" {
					if htu, want := proofTarget(t, signed), public+route; htu != want {
						t.Errorf("the proof of %s %s signs %q, want %q", r.Method, route, htu, want)
					}
				}
				var body struct {
					Request   string `json:"request"`
					RuntimeID string `json:"runtime_id"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body) // a step with no body decodes none
				if body.Request != "" {
					if _, err := record.VerifyRequestObject(body.Request, public, time.Minute, time.Now()); err != nil {
						t.Errorf("the request object of %s %s for %s: %v", r.Method, route, public, err)
						w.WriteHeader(http.StatusUnauthorized)
						fmt.Fprint(w, `{"code":"REQUEST_INVALID"}`)
						return
					}
				}
				switch r.Method + " " + route {
				case "POST /v1/launcher-credentials/token":
					fmt.Fprint(w, `{"credential_id":"credential-1"}`)
				case "POST /v1/enrollments":
					if body.RuntimeID == "pod-refused" {
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `{"code":"ENROLLMENT_REFUSED"}`)
						return
					}
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"enrollment_id":"enrollment-1"}`)
				case "POST /v1/requests":
					fmt.Fprint(w, `{"request_id":"request-1","state":"pending","record_id":"record-1","approve_url":"https://approve.example/r/1"}`)
				case "GET /v1/requests/request-1":
					fmt.Fprint(w, `{"request_id":"request-1","state":"approved","grant_id":"grant-1"}`)
				case "POST /v1/requests/request-1/cancel":
					fmt.Fprint(w, `{}`)
				case "POST /v1/grants/grant-1/values":
					fmt.Fprint(w, `{"values":{"TOKEN":"value-1"}}`)
				default:
					t.Errorf("the broker was asked %s %s, a route no step of Pods asks", r.Method, r.URL.RequestURI())
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"code":"NOT_FOUND"}`)
				}
			}))
			t.Cleanup(broker.Close)
			public = broker.URL + c.path
			pods := Pods{
				T:     t,
				Base:  strings.Replace(broker.URL, "://", "://"+c.user, 1) + c.suffix,
				Token: func(account, podUID string) string { return "token-of-" + podUID },
			}

			launcher := pods.LogIn("example-launcher", "system:serviceaccount:example-system:example-launcher")
			pod := pods.Enroll(launcher, "pod-1", "system:serviceaccount:example:example-worker")
			if status, code := pods.EnrollRefused(launcher, "pod-refused", "system:serviceaccount:example:example-worker"); status != http.StatusForbidden || code != "ENROLLMENT_REFUSED" {
				t.Errorf("EnrollRefused = %d %q, want 403 ENROLLMENT_REFUSED", status, code)
			}
			reply := pods.Ask(pod, "example-secret")
			state := pods.State(pod, reply.RequestID)
			pods.Cancel(pod, reply.RequestID)
			if state.GrantID == nil {
				t.Fatalf("State = %+v, want the stand-in's grant", state)
			}
			if values := pods.Values(pod, *state.GrantID); values["TOKEN"] != "value-1" {
				t.Errorf("Values = %v, want the stand-in's", values)
			}

			query := ""
			if c.query != "" {
				query = "?" + c.query
			}
			var want []string
			for _, step := range []string{
				"POST /v1/launcher-credentials/token", "POST /v1/enrollments", "POST /v1/enrollments", "POST /v1/requests",
				"GET /v1/requests/request-1", "POST /v1/requests/request-1/cancel", "POST /v1/grants/grant-1/values",
			} {
				method, route, _ := strings.Cut(step, " ")
				want = append(want, method+" "+c.path+route+query)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(asked, want) {
				t.Errorf("Pods at %s asked %q, want %q", pods.Base, asked, want)
			}
		})
	}
}

// proofTarget is the htu a proof signs, read from its payload without checking its signature.
func proofTarget(t *testing.T, compact string) string {
	t.Helper()
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Errorf("the Proof header is not a compact JWS")
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Errorf("the proof's payload: %v", err)
		return ""
	}
	var claims struct {
		URL string `json:"htu"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Errorf("the proof's payload: %v", err)
	}
	return claims.URL
}
