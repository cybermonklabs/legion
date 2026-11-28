package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testport"
)

// RoleHolder names every session the listener names as a role's holder, whatever state it gives
// it, since a forget of a session its claim does not hold refuses on every one of them
// (supervise's mayForget, review rounds 17 and 18 of #4220): the live holder of a 200, and the
// lapsed holder of a 404, unresponsive, gone, or with no state from a listener that predates the
// field. A 200 is read within answerBodyLimit, since its title, dir and capabilities are not
// bounded: a holder with many capabilities is named as any other. Only a 404 naming no holder
// under reason `unclaimed`, a role no claim names, is no holder; one naming no holder under any
// other reason, and every other answer, is an error, which the forget refuses on too. A listener
// outage (listenerwire.Outage: a 408, a 429, a 5xx but the listener's own 500,
// listenerwire.FaultStatus, which it answers the same way each time it is asked) is a plain error
// the caller waits on and asks again about. The lookup is one GET of the role with the listener's
// bearer.
//
// The controls (the pull request records each run): RoleHolder answering a lapsed holder with no
// state as no holder, as round 17 first did, fails the older listener's row; the 200 read within
// refusalBodyLimit fails the many-capabilities row; any 404 naming no holder read as no holder
// fails the other-reason row.
func TestRoleHolderNamesEveryHolderTheListenerNames(t *testing.T) {
	capabilities := make([]string, 400)
	for i := range capabilities {
		capabilities[i] = fmt.Sprintf("capability-%04d", i)
	}
	long, err := json.Marshal(map[string]any{"role": "legion-x", "holder": "ses_s", "holder_state": "live", "last_seen": 1, "capabilities": capabilities})
	if err != nil {
		t.Fatal(err)
	}
	if len(long) <= refusalBodyLimit {
		t.Fatalf("the long answer is %d bytes, want it past refusalBodyLimit, %d", len(long), refusalBodyLimit)
	}
	const refused, transient = "refused", "transient"
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		holder, state string
		fails         string
	}{
		{"a live holder", http.StatusOK, `{"role":"legion-x","holder":"ses_s","holder_state":"live","last_seen":1}`, "ses_s", "live", ""},
		{"a live holder with many capabilities", http.StatusOK, string(long), "ses_s", "live", ""},
		{"an unresponsive holder", http.StatusNotFound, `{"error":"…","reason":"holder_lapsed","holder":"ses_s","holder_state":"unresponsive"}`, "ses_s", "unresponsive", ""},
		{"a gone holder whose claim stands", http.StatusNotFound, `{"error":"…","reason":"holder_lapsed","holder":"ses_s","holder_state":"gone"}`, "ses_s", "gone", ""},
		{"an older listener's lapsed holder with no state", http.StatusNotFound, `{"error":"…","reason":"holder_lapsed","holder":"ses_s"}`, "ses_s", "lapsed", ""},
		{"a role no claim names", http.StatusNotFound, `{"error":"…","reason":"unclaimed"}`, "", "", ""},
		{"a 404 naming no holder under another reason", http.StatusNotFound, `{"error":"…","reason":"holder_lapsed"}`, "", "", refused},
		{"a 404 naming no holder and no reason", http.StatusNotFound, `{"error":"no such route"}`, "", "", refused},
		{"a 200 naming no holder", http.StatusOK, `{"role":"legion-x"}`, "", "", refused},
		{"an unreadable 404", http.StatusNotFound, `not json`, "", "", refused},
		{"a wrong bearer", http.StatusUnauthorized, `{"error":"unauthorized"}`, "", "", refused},
		{"a claim record the listener cannot decode", http.StatusInternalServerError, `{"error":"envoy: the claim record of role legion-x is unreadable: …"}`, "", "", refused},
		{"a listener whose bus does not answer", http.StatusServiceUnavailable, `{"error":"…"}`, "", "", transient},
		{"a proxy that timed out the request", http.StatusRequestTimeout, `<html>408 Request Timeout</html>`, "", "", transient},
		{"a limiter in front of the listener", http.StatusTooManyRequests, `<html>429 Too Many Requests</html>`, "", "", transient},
		{"a gateway in front of the listener", http.StatusBadGateway, `<html>502 Bad Gateway</html>`, "", "", transient},
		{"a proxy that does not implement the method", http.StatusNotImplemented, `<html>501 Not Implemented</html>`, "", "", transient},
		{"a proxy that refuses the HTTP version", http.StatusHTTPVersionNotSupported, `<html>505 HTTP Version Not Supported</html>`, "", "", transient},
		{"a 599", 599, ``, "", "", transient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var path, bearer string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, bearer = r.URL.Path, r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			holder, state, err := New(server.URL, "envoy-bearer").RoleHolder(context.Background(), "legion-legion-legion-942-implementer")
			if path != "/v1/roles/legion-legion-legion-942-implementer" || bearer != "Bearer envoy-bearer" {
				t.Errorf("the lookup asked %q with %q, want the role's path with the listener's bearer", path, bearer)
			}
			if tc.fails != "" {
				var lookup *RoleLookupRefused
				switch {
				case err == nil:
					t.Fatalf("RoleHolder = %q (%q), want an error for that answer", holder, state)
				case errors.As(err, &lookup) != (tc.fails == refused):
					t.Fatalf("RoleHolder = %v; want it a RoleLookupRefused %t", err, tc.fails == refused)
				case lookup != nil && lookup.Status != tc.status:
					t.Fatalf("RoleHolder = %v; want the refusal to carry status %d", err, tc.status)
				}
				return
			}
			if err != nil || holder != tc.holder || state != tc.state {
				t.Fatalf("RoleHolder = %q (%q), %v; want %q (%q)", holder, state, err, tc.holder, tc.state)
			}
		})
	}

	t.Run("a listener it cannot reach", func(t *testing.T) {
		// A port the test holds and nothing listens on: a closed server's port is any process's
		// to take, and one that takes it answers the lookup.
		refused := "http://" + testport.Refused(t)
		_, _, err := New(refused, "").RoleHolder(context.Background(), "legion-x")
		var lookup *RoleLookupRefused
		if err == nil || errors.As(err, &lookup) {
			t.Fatalf("RoleHolder of a listener that refuses every dial = %v, want a plain error that asking again can clear", err)
		}
	})
}
