package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// cutAnswer serves status with half of body under a Content-Length for the whole of it, then
// closes the connection, as a reset mid-body reads to the client: the headers, part of the body,
// then an unexpected end.
func cutAnswer(t *testing.T, status int, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, out, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack the connection: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(out, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			status, http.StatusText(status), len(body), body[:len(body)/2])
		_ = out.Flush()
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// A listener answer whose body could not be read to its end is no answer at all (review round 19
// of #4220): what the listener said is unknown, so each reader returns a plain error that asking
// again can clear, never a refusal or an outcome read from part of a body. RoleHolder gives no
// RoleLookupRefused, which a forget would report as a fault an operator must fix; Publish gives
// none of its refusal kinds, a role nobody holds among them; and DeleteSession does not report
// a delete the listener may have refused.
//
// The controls (the pull request records each run): RoleHolder reading a cut 200 or 404 as a
// refusal; DeleteSession reading a cut 200 as deleted.
func TestAnAnswerCutOffMidBodyIsNoAnswer(t *testing.T) {
	ctx := context.Background()
	t.Run("RoleHolder", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			body   string
		}{
			{"a 200", http.StatusOK, `{"role":"legion-x","holder":"ses_s","holder_state":"live","last_seen":1,"title":"a title long enough to cut"}`},
			{"a 404", http.StatusNotFound, `{"error":"…","reason":"holder_lapsed","holder":"ses_s","holder_state":"unresponsive"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				holder, state, err := New(cutAnswer(t, tc.status, tc.body), "").RoleHolder(ctx, "legion-x")
				var refused *RoleLookupRefused
				if err == nil || errors.As(err, &refused) {
					t.Fatalf("RoleHolder of a cut %s = %q (%q), %v; want a plain error, no RoleLookupRefused", tc.name, holder, state, err)
				}
				if !strings.Contains(err.Error(), "could not be read to its end") {
					t.Fatalf("RoleHolder of a cut %s = %v; want it to say the answer could not be read", tc.name, err)
				}
			})
		}
	})
	t.Run("Publish", func(t *testing.T) {
		err := New(cutAnswer(t, http.StatusNotFound, `{"error":"no live session holds the role","reason":"unclaimed"}`), "").
			Publish(ctx, "notifications.role.legion-x", "a notice", map[string]string{"k": "v"}, "key-1")
		if err == nil || errors.Is(err, ErrNoHolder) || errors.Is(err, ErrHolderUnresponsive) {
			t.Fatalf("Publish answered by a cut 404 = %v; want a plain error, no refusal kind", err)
		}
		if !strings.Contains(err.Error(), "could not be read to its end") {
			t.Fatalf("Publish answered by a cut 404 = %v; want it to say the answer could not be read", err)
		}
	})
	t.Run("DeleteSession", func(t *testing.T) {
		stopped, err := New(cutAnswer(t, http.StatusOK, `{"deleted":false,"reason":"the registration is newer than the stop"}`), "").
			DeleteSession(ctx, "ses_s", DeleteConditions{NotRegisteredAfter: time.Now()})
		if err == nil || stopped.Deleted {
			t.Fatalf("DeleteSession answered by a cut 200 = %+v, %v; want an error and no delete reported", stopped, err)
		}
	})
}

// plantedUser is the user name a refusal's URL carries: a user name with no password, which the
// push's leak check reads as no credential. No other text of the test, and no word a reader's
// error writes, holds four of its bytes in a row.
const plantedUser = "qzv-planted-user"

// heldPart is the first four bytes in a row of planted that text holds, and "" where it holds none.
func heldPart(text, planted string) string {
	for i := 0; i+4 <= len(planted); i++ {
		if strings.Contains(text, planted[i:i+4]) {
			return planted[i : i+4]
		}
	}
	return ""
}

// A listener's answer longer than refusalBodyLimit whose bound falls inside a URL's user name,
// before its "@", is named in the error with no part of that user name, by every reader: the read
// knows its bound cut the body and drops the run it stopped inside before the URL rule reads it,
// and a 200's body, read whole, is cut only after the rule. A cut made first leaves the user
// name's start where the rule reads it as the URL's host. A body whose read ends on an error
// inside the user name, short of the bound, is cut there as well, and a reader that quotes it
// with its error dropped names no part of the user name either.
func TestAnAnswerCutInsideAURLsUserNameNamesNoPartOfIt(t *testing.T) {
	cutAt := " https://" + plantedUser[:len("qzv-plan")]
	body := strings.Repeat("w", refusalBodyLimit-len(cutAt)) + " https://" + plantedUser + "@listener.example/v1/messages/publish was the listener asked"
	if !strings.HasSuffix(body[:refusalBodyLimit], cutAt) {
		t.Fatalf("the body's first %d bytes end %q, not inside the user name", refusalBodyLimit, body[refusalBodyLimit-len(cutAt):refusalBodyLimit])
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		status int
		call   func(p *HTTPPublisher) error
	}{
		{"Publish", http.StatusInternalServerError, func(p *HTTPPublisher) error {
			return p.Publish(ctx, "notifications.role.legion-x", "a notice", map[string]string{"k": "v"}, "key-1")
		}},
		{"ReadSessionRegistration", http.StatusInternalServerError, func(p *HTTPPublisher) error {
			_, err := p.ReadSessionRegistration(ctx, "ses_s")
			return err
		}},
		{"RoleHolder", http.StatusInternalServerError, func(p *HTTPPublisher) error {
			_, _, err := p.RoleHolder(ctx, "legion-x")
			return err
		}},
		{"RoleHolder's body of a 200", http.StatusOK, func(p *HTTPPublisher) error {
			_, _, err := p.RoleHolder(ctx, "legion-x")
			return err
		}},
		{"DeleteSession", http.StatusInternalServerError, func(p *HTTPPublisher) error {
			_, err := p.DeleteSession(ctx, "ses_s", DeleteConditions{NotRegisteredAfter: time.Now()})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			err := tc.call(New(server.URL, ""))
			if err == nil || !strings.Contains(err.Error(), "wwww") {
				t.Fatalf("%s, the listener answering %d: %v; want an error naming the listener's answer", tc.name, tc.status, err)
			}
			if part := heldPart(err.Error(), plantedUser); part != "" {
				t.Fatalf("%s, the listener answering %d: an error ending %q, which holds %q of the URL's user name",
					tc.name, tc.status, err.Error()[max(0, len(err.Error())-40):], part)
			}
		})
	}
	t.Run("ReadSessionRegistration's body ending on a read error", func(t *testing.T) {
		// cutAnswer sends the first half of the body, which ends inside the user name, and
		// then closes the connection.
		half := strings.Repeat("w", 100) + cutAt
		rest := plantedUser[len("qzv-plan"):] + "@listener.example/v1/registry/ses_s was the listener asked"
		ended := half + rest + strings.Repeat("w", len(half)-len(rest))
		if ended[:len(ended)/2] != half {
			t.Fatalf("the served half of the body ends %q, not inside the user name", ended[len(ended)/2-len(cutAt):len(ended)/2])
		}
		_, err := New(cutAnswer(t, http.StatusInternalServerError, ended), "").ReadSessionRegistration(ctx, "ses_s")
		if err == nil || !strings.Contains(err.Error(), "wwww") {
			t.Fatalf("ReadSessionRegistration, its answer's body ending on a read error: %v; want an error naming the listener's answer", err)
		}
		if part := heldPart(err.Error(), plantedUser); part != "" {
			t.Fatalf("ReadSessionRegistration, its answer's body ending on a read error: an error ending %q, which holds %q of the URL's user name",
				err.Error()[max(0, len(err.Error())-40):], part)
		}
	})
}
