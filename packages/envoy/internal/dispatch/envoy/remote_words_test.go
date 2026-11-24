package envoy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// listenerWordsRows are the listener's reasons that quote a URL carrying a credential, each with
// what the rule for remote text prints of it.
var listenerWordsRows = []struct{ name, reason, shown string }{
	{"user information and a query value", redacttest.Words, redacttest.Shown},
	{"a fragment's value", redacttest.FragmentWords, redacttest.FragmentShown},
	{"words the rule cannot read", redacttest.Unreadable, redact.WordsNotReadable},
}

// TestTheListenersRefusalPrintsItsWordsAsTheRuleReadsThem: the listener answers a list read's
// refilling 503, a list read's other refusal, a GET's refusal and a send's refusal with a reason
// that quotes a URL carrying a credential. Each error the client returns prints the reason as the
// rule reads it, under every verb for the RefillingError, and none of the credentials; the
// refilling one still matches ErrRefilling.
func TestTheListenersRefusalPrintsItsWordsAsTheRuleReadsThem(t *testing.T) {
	for _, row := range listenerWordsRows {
		t.Run(row.name, func(t *testing.T) {
			var answer func(w http.ResponseWriter, r *http.Request)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { answer(w, r) }))
			t.Cleanup(srv.Close)
			refuse := func(status int, code string) {
				answer = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": row.reason})
				}
			}
			client := New(srv.URL)

			refuse(http.StatusServiceUnavailable, listenerRefillingCode)
			_, refilling := client.Sessions(context.Background())
			var held *RefillingError
			if !errors.As(refilling, &held) || !errors.Is(refilling, ErrRefilling) {
				t.Fatalf("Sessions = %v, want the listener's refilling answer", refilling)
			}
			redacttest.RequireNoneFormatted(t, "envoy.RefillingError", held)
			if !strings.HasSuffix(refilling.Error(), "returned 503: "+row.shown) {
				t.Errorf("the refilling answer prints %q, want it to end with %q", refilling, row.shown)
			}

			refuse(http.StatusBadRequest, "bad_request")
			_, listed := client.Sessions(context.Background())
			_, sent := client.Send(context.Background(), SendInput{TargetSession: "session-1", Message: "hello"})
			for what, err := range map[string]error{"envoy.listFailure": listed, "envoy.listenerRefusal": sent} {
				if err == nil {
					t.Fatalf("%s answered no error", what)
				}
				redacttest.RequireNone(t, what, err.Error())
				if !strings.HasSuffix(err.Error(), row.shown) {
					t.Errorf("%s prints %q, want it to end with %q", what, err, row.shown)
				}
			}
		})
	}
}

// TestAListenerCallNamesNoCredentialTheConfiguredURLCarries: a listener URL configured with user
// information, or with a token in its query, that nothing answers at fails each call with an
// error that names the call by its route and the failure by its class, and holds no part of the
// credential; a clean URL's failure keeps the transport's own words.
func TestAListenerCallNamesNoCredentialTheConfiguredURLCarries(t *testing.T) {
	param := "access" + "_token"
	for _, row := range []struct{ name, base, sessions, unsubscribe string }{
		{"user information", "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1",
			"GET /v1/sessions: connection refused", "POST /v1/interests/unsubscribe: connection refused"},
		{"a token in the query", "http://127.0.0.1:1/?" + param + "=" + redacttest.Token,
			"GET /v1/sessions: connection refused", "POST /v1/interests/unsubscribe: connection refused"},
		{"a clean URL", "http://127.0.0.1:1",
			"GET /v1/sessions: connection refused", "POST /v1/interests/unsubscribe: dial tcp 127.0.0.1:1: connect: connection refused"},
	} {
		t.Run(row.name, func(t *testing.T) {
			client := New(row.base)
			_, sessionsErr := client.Sessions(context.Background())
			_, unsubscribeErr := client.Unsubscribe(context.Background(), "session-1", []string{"agent.session-1"})
			_, sendErr := client.Send(context.Background(), SendInput{TargetSession: "session-1", Message: "hello"})
			for what, call := range map[string]struct {
				err  error
				want string
			}{
				"Sessions":    {sessionsErr, row.sessions},
				"Unsubscribe": {unsubscribeErr, row.unsubscribe},
				"Send":        {sendErr, ""},
			} {
				if call.err == nil || !errors.Is(call.err, ErrUnreachable) {
					t.Fatalf("%s = %v, want an ErrUnreachable failure", what, call.err)
				}
				redacttest.RequireNone(t, "envoy.Client."+what, call.err.Error())
				if call.want != "" && !strings.HasSuffix(call.err.Error(), call.want) {
					t.Errorf("%s's error is %q, want it to end %q", what, call.err, call.want)
				}
			}
		})
	}
}

// TestARequestTheClientCannotBuildNamesNoCredentialTheConfiguredURLCarries: a listener URL with
// the probe's user information and an escape net/url refuses fails each call before it is sent,
// and net/url's error quotes the URL whole; each call's error names its route and the failure's
// class, and none of the credentials.
func TestARequestTheClientCannotBuildNamesNoCredentialTheConfiguredURLCarries(t *testing.T) {
	client := New("http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/%zz")
	_, sessionsErr := client.Sessions(context.Background())
	_, unsubscribeErr := client.Unsubscribe(context.Background(), "session-1", []string{"agent.session-1"})
	_, sendErr := client.Send(context.Background(), SendInput{TargetSession: "session-1", Message: "hello"})
	for capture, err := range map[string]error{"envoy.build-get": sessionsErr, "envoy.build-unsubscribe": unsubscribeErr, "envoy.build-send": sendErr} {
		t.Run(capture, func(t *testing.T) {
			if err == nil || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("the call = %v, want an ErrUnavailable failure", err)
			}
			redacttest.RequireNone(t, capture, err.Error())
			if !strings.Contains(err.Error(), "request: ") {
				t.Errorf("the call's error %q does not name the request it could not build", err)
			}
		})
	}
}

// TestAReadsRefusalPrintsTheListenersWordsAsTheRuleReadsThem: the listener refuses a session read
// with a status the client does not take and a reason that quotes a URL carrying a credential; the
// error names the route, the status and the reason as the rule reads it, and none of the
// credentials.
func TestAReadsRefusalPrintsTheListenersWordsAsTheRuleReadsThem(t *testing.T) {
	for _, row := range listenerWordsRows {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": row.reason})
			}))
			t.Cleanup(srv.Close)
			_, err := New(srv.URL).Session(context.Background(), "session-1")
			if err == nil {
				t.Fatal("Session answered no error")
			}
			redacttest.RequireNone(t, "envoy.unavailableGetStatus", err.Error())
			if want := "returned 500: " + row.shown; !strings.HasSuffix(err.Error(), want) {
				t.Errorf("Session's error is %q, want it to end %q", err, want)
			}
		})
	}
}
