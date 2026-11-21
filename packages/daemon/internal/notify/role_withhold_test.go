package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// roleWithholdCalls is WithholdRole and LiftRoleWithhold by the method each sends.
var roleWithholdCalls = []struct {
	name   string
	method string
	call   func(p *HTTPPublisher, role, session string) error
}{
	{"a withhold", http.MethodPut, func(p *HTTPPublisher, role, session string) error {
		return p.WithholdRole(context.Background(), role, session)
	}},
	{"a lift", http.MethodDelete, func(p *HTTPPublisher, role, session string) error {
		return p.LiftRoleWithhold(context.Background(), role, session)
	}},
}

// A withhold and its lift are one request each to the Envoy listener's role withholds, a PUT and a
// DELETE of /v1/role-withholds, with the bearer every other call of this client presents and a JSON
// body naming the role and the session and nothing else; the listener's 204 is nil.
//
// The controls (the pull request records each run): the lift sent as a PUT fails the lift's row
// naming the method; the body's session left out fails both rows naming the body.
func TestARoleWithholdIsOneRequestNamingTheRoleAndTheSession(t *testing.T) {
	const role, session = "legion-legion-legion-951-implementer", "ses_withheld"
	for _, tc := range roleWithholdCalls {
		t.Run(tc.name, func(t *testing.T) {
			var method, path, bearer, contentType string
			var body map[string]any
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				method, path, bearer, contentType = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode the body of %s %s: %v", r.Method, r.URL.Path, err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			if err := tc.call(New(server.URL, "envoy-bearer"), role, session); err != nil {
				t.Fatalf("%s answered 204 = %v, want nil", tc.name, err)
			}
			if requests != 1 || method != tc.method || path != "/v1/role-withholds" {
				t.Errorf("%s sent %d requests, the last %s %q; want one %s /v1/role-withholds", tc.name, requests, method, path, tc.method)
			}
			if bearer != "Bearer envoy-bearer" || contentType != "application/json" {
				t.Errorf("%s carried bearer %q and content type %q, want the listener's bearer and application/json", tc.name, bearer, contentType)
			}
			if want := map[string]any{"role": role, "session": session}; !reflect.DeepEqual(body, want) {
				t.Errorf("%s sent the body %v, want %v", tc.name, body, want)
			}
		})
	}
}

// Every answer of the listener but 204 is an error that names the call, its status and the
// listener's own words, and never the bearer: the 403 the listener gives every caller that is not
// a Legion daemon's broker token, the 409 of another daemon's withhold, and the 503 of a bus that
// did not answer. A listener the client cannot reach is an error too.
//
// The control (the pull request records the run): the error built from the status alone fails
// the 403, 409 and 503 rows, naming the listener's words each was not given.
func TestARefusedRoleWithholdNamesTheStatusAndTheListenersWords(t *testing.T) {
	const role, session, bearer = "legion-legion-legion-951-implementer", "ses_withheld", "envoy-bearer"
	for _, answer := range []struct {
		name   string
		status int
		body   string
		words  string
	}{
		{"a caller that is not a daemon", http.StatusForbidden, `{"error":"role withholds are a Legion daemon's own"}`, "role withholds are a Legion daemon's own"},
		{"another daemon's withhold", http.StatusConflict, `{"error":"role_withheld_by_another","role":"` + role + `"}`, "role_withheld_by_another"},
		{"a bus that did not answer", http.StatusServiceUnavailable, `{"error":"the bus did not answer"}`, "the bus did not answer"},
		{"a 200 where the route answers 204", http.StatusOK, `{}`, "200 OK"},
	} {
		for _, tc := range roleWithholdCalls {
			t.Run(answer.name+", "+tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(answer.status)
					_, _ = io.WriteString(w, answer.body)
				}))
				defer server.Close()

				err := tc.call(New(server.URL, bearer), role, session)
				if err == nil {
					t.Fatalf("%s answered %d = nil, want an error", tc.name, answer.status)
				}
				text := err.Error()
				for _, want := range []string{role, session, http.StatusText(answer.status), answer.words} {
					if !strings.Contains(text, want) {
						t.Errorf("%s answered %d = %q, want it to name %q", tc.name, answer.status, text, want)
					}
				}
				if strings.Contains(text, bearer) {
					t.Errorf("%s answered %d = %q, which carries the bearer", tc.name, answer.status, text)
				}
			})
		}
	}

	for _, tc := range roleWithholdCalls {
		t.Run("a listener it cannot reach, "+tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.NotFoundHandler())
			server.Close()
			if err := tc.call(New(server.URL, bearer), role, session); err == nil || !strings.Contains(err.Error(), role) {
				t.Fatalf("%s of a closed listener = %v, want an error naming the role", tc.name, err)
			}
		})
	}
}
