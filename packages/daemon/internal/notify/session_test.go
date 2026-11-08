package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The clean stop is one DELETE of the session's own key, with the listener's bearer, and it names
// both of the conditions it holds under: the client instance whose registration it means to
// remove (client_instance_id) and the moment the stop belongs to (if_not_registered_after). An
// instance id is a client's own string, so it is escaped rather than written into the query raw.
func TestDeleteSessionAsksTheSessionRouteWithTheBearerTheMomentAndTheInstance(t *testing.T) {
	var method, path, query, bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query, bearer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	goneAt := time.Now()

	stopped, err := New(server.URL+"/", "envoy-bearer").DeleteSession(context.Background(), "ses_implementer",
		DeleteConditions{ClientInstanceID: "pi/omp 1+1", NotRegisteredAfter: goneAt})
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if !stopped.Deleted {
		t.Fatalf("DeleteSession = %+v, want the registration taken as deleted", stopped)
	}
	if method != http.MethodDelete || path != "/v1/sessions/ses_implementer" || bearer != "Bearer envoy-bearer" {
		t.Fatalf("%s %s with %q, want a DELETE of the session's key with the listener's bearer", method, path, bearer)
	}
	want := fmt.Sprintf("if_not_registered_after=%d&client_instance_id=pi%%2Fomp+1%%2B1", goneAt.UnixMilli())
	if query != want {
		t.Fatalf("query = %q, want both conditions, the id escaped: %q", query, want)
	}
}

// An entry that names no client process — one a client predating the field wrote — leaves the
// moment to judge the delete alone, and the parameter is left out rather than sent empty: the
// listener reads an empty client_instance_id as a condition of its own, which would keep every
// registration that names a process.
func TestDeleteSessionLeavesOutAnInstanceTheEntryDoesNotName(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	goneAt := time.Now()

	if _, err := New(server.URL, "").DeleteSession(context.Background(), "ses_old_client",
		DeleteConditions{NotRegisteredAfter: goneAt}); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if want := fmt.Sprintf("if_not_registered_after=%d", goneAt.UnixMilli()); query != want {
		t.Fatalf("query = %q, want the moment alone, %q", query, want)
	}
}

// A listener that kept the registration says so, and the caller is told which it was: that answer
// is the one that stops a slow stop deleting the registration a relaunched process wrote under
// the same session id (dispatch://INFRA-392).
func TestDeleteSessionReportsARegistrationTheListenerKept(t *testing.T) {
	const reason = "the registration belongs to another client instance"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"deleted":false,"reason":"`+reason+`"}`)
	}))
	defer server.Close()

	stopped, err := New(server.URL, "").DeleteSession(context.Background(), "ses_relaunched",
		DeleteConditions{ClientInstanceID: "inst_dead", NotRegisteredAfter: time.Now()})
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if stopped.Deleted || stopped.Reason != reason {
		t.Fatalf("DeleteSession = %+v, want the registration kept with the listener's reason", stopped)
	}
}

// A registry that holds no record for the session has already reached the outcome the stop asks
// for, so its 404 is no failure; a refusal that leaves the registration standing is one, and says
// what the listener answered so an operator can see why the session still holds its role.
func TestDeleteSessionTellsAnAlreadyGoneSessionFromARefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "the listener removed it", status: http.StatusOK, body: `{"deleted":true}`},
		{name: "an older listener removed it without saying so", status: http.StatusOK, body: ``},
		{name: "the registry holds no record of it", status: http.StatusNotFound, body: `{"error":"session not found"}`},
		{name: "the bus could not be reached", status: http.StatusServiceUnavailable, body: `{"error":"session registry unavailable"}`,
			wantErr: "session registry unavailable"},
		{name: "the bearer is wrong", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, wantErr: "401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			stopped, err := New(server.URL, "").DeleteSession(context.Background(), "ses_implementer",
				DeleteConditions{ClientInstanceID: "inst_dead", NotRegisteredAfter: time.Now()})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("DeleteSession = %v, want the session taken as stopped", err)
			case tc.wantErr == "" && !stopped.Deleted:
				t.Fatalf("DeleteSession = %+v, want the registration taken as deleted", stopped)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("DeleteSession = %v, want a failure naming %q", err, tc.wantErr)
			}
		})
	}
}

// The read before the stop is one GET of the session's registry entry, with the listener's bearer
// and the session id escaped into the path, and it asks include=unresponsive: a dead role's last
// registration is past the live window exactly when the stop matters, and without the selector
// the route answers 404 for a record it still holds. What it answers is the entry's own client
// instance and its last registration.
func TestReadSessionRegistrationAsksTheRegistryForTheEntrysInstance(t *testing.T) {
	var method, path, query, bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query, bearer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"session_id":"ses/dead","client_instance_id":"inst_dead","last_seen":1762560000000}`)
	}))
	defer server.Close()

	registration, err := New(server.URL, "envoy-bearer").ReadSessionRegistration(context.Background(), "ses/dead")
	if err != nil {
		t.Fatalf("ReadSessionRegistration: %v", err)
	}
	want := SessionRegistration{Registered: true, ClientInstanceID: "inst_dead", LastSeen: 1762560000000}
	if registration != want {
		t.Fatalf("ReadSessionRegistration = %+v, want %+v", registration, want)
	}
	if method != http.MethodGet || path != "/v1/registry/ses/dead" || bearer != "Bearer envoy-bearer" {
		t.Fatalf("%s %s with %q, want a GET of the session's registry entry with the listener's bearer", method, path, bearer)
	}
	if query != "include=unresponsive" {
		t.Fatalf("query = %q, want the unresponsive record asked for too", query)
	}
}

// A registry that holds no record of the session answers 404, and that is the outcome the stop
// wanted rather than a failure: the caller is told there is no registration, with no error, and
// sends no delete.
func TestReadSessionRegistrationReadsA404AsNoRegistration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"no live session ses_gone"}`)
	}))
	defer server.Close()

	registration, err := New(server.URL, "").ReadSessionRegistration(context.Background(), "ses_gone")
	if err != nil {
		t.Fatalf("ReadSessionRegistration = %v, want a registry holding none taken as no failure", err)
	}
	if registration != (SessionRegistration{}) {
		t.Fatalf("ReadSessionRegistration = %+v, want no registration at all", registration)
	}
}

// An answer this client cannot read is no registration it may name in a delete, and neither is a
// listener that refused the read: both are failures, and the refusal names the status so an
// operator can see why no stop was sent.
func TestReadSessionRegistrationTellsARefusalFromAnEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "the body is not an entry at all", status: http.StatusOK, body: `not json`, wantErr: "read the registration"},
		{name: "the bus could not be reached", status: http.StatusServiceUnavailable,
			body: `{"error":"session registry unavailable"}`, wantErr: "503"},
		{name: "the listener failed", status: http.StatusInternalServerError, body: `{"error":"boom"}`, wantErr: "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			registration, err := New(server.URL, "").ReadSessionRegistration(context.Background(), "ses_implementer")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ReadSessionRegistration = %+v, %v, want a failure naming %q", registration, err, tc.wantErr)
			}
		})
	}
}
