package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestAMemberKindReadTheCallerEndedIsNoDispatchFailure: the proxy's refusal of a call whose
// member-kind read failed because its caller gave up (context.Canceled) is 503
// MEMBER_KIND_UNCHECKABLE in the fixed words and is logged at INFO, since it says nothing of
// Dispatch; a failure to ask Dispatch is the same answer logged at ERROR.
func TestAMemberKindReadTheCallerEndedIsNoDispatchFailure(t *testing.T) {
	for _, row := range []struct {
		name    string
		failure error
		level   slog.Level
	}{
		{"the caller hung up", fmt.Errorf("projectaccess: ask dispatch: %w", context.Canceled), slog.LevelInfo},
		{"Dispatch did not answer", errors.New("dial tcp 10.20.0.9:8766: connect: connection refused"), slog.LevelError},
	} {
		t.Run(row.name, func(t *testing.T) {
			var records logtest.Recorder
			s := &Server{log: slog.New(&records)}
			uncheckable := &projectaccess.MemberKindUncheckable{Person: "uma@example.com", Err: row.failure}
			status, code, words := s.grantRefusal("connection", uncheckable)
			if status != http.StatusServiceUnavailable || code != projectaccess.UncheckableCode || words != uncheckable.Error() {
				t.Fatalf("grantRefusal = %d %s %q; want 503 %s in the fixed words", status, code, words, projectaccess.UncheckableCode)
			}
			got := records.Records()
			if len(got) != 1 || got[0].Level != row.level {
				var levels []slog.Level
				for _, r := range got {
					levels = append(levels, r.Level)
				}
				t.Fatalf("grantRefusal logged at %v; want one line at %v", levels, row.level)
			}
		})
	}
}

// TestAPersonWhoseMemberKindCannotBeReadIsCarriedNothing: while the broker cannot learn from
// Dispatch whether the person a grant's session was made for is a restricted member
// (*projectaccess.MemberKindUncheckable, which requests.Machine answers on both reads the proxy
// makes), the proxy carries nothing: a call to the bound host whose values read cannot tell, and a
// CONNECT whose connection read cannot tell, are each 503 in the broker's fixed words under
// MEMBER_KIND_UNCHECKABLE, and never GRANT_UNREADABLE, which says only that a read did not happen.
// No value is forwarded and the bound host is sent nothing, and no answer carries a word of the
// failure itself, which can name Dispatch's address.
func TestAPersonWhoseMemberKindCannotBeReadIsCarriedNothing(t *testing.T) {
	uncheckable := &projectaccess.MemberKindUncheckable{Person: "uma@example.com", Err: databaseDown}
	carriesNothing := func(t *testing.T, r *rig, status int, body string) {
		t.Helper()
		if status != http.StatusServiceUnavailable || !strings.Contains(body, uncheckable.Error()) {
			t.Fatalf("the answer is %d %q; want 503 in the words %q", status, body, uncheckable.Error())
		}
		if strings.Contains(body, grantUnreadable) || strings.Contains(body, "10.20.0.7") || strings.Contains(body, workerValue) {
			t.Fatalf("the answer %q says the read did not happen, names the failure or carries the value", body)
		}
		if seen := r.requests(); len(seen) != 0 {
			t.Fatalf("the bound host was sent %d requests; want none", len(seen))
		}
	}

	t.Run("a call to the bound host whose values read cannot tell", func(t *testing.T) {
		r := newRig(t, Options{})
		r.grants.refuseValues(uncheckable)
		answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
		carriesNothing(t, r, answer.StatusCode, body)
	})

	t.Run("a CONNECT whose connection read cannot tell", func(t *testing.T) {
		r := newRig(t, Options{})
		r.grants.refuseConnection(workerPlaceholder, uncheckable)
		got := connectAnswer(t, r, net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
		carriesNothing(t, r, got.status, got.body)
	})
}

// TestAGrantWhosePersonBecameRestrictedIsRefusedAtTheNextCall: the proxy reads a grant's values
// once per pod and serves from what it read, so the connection read, which every call makes, is
// where a person Dispatch later calls a restricted member is refused (requests.Machine's
// AirlockConnection ends such a grant and answers ErrGrantNotLive). The first call is carried with
// the value; once the connection read refuses, the next call is 407 in the broker's words, and the
// bound host is sent nothing more.
func TestAGrantWhosePersonBecameRestrictedIsRefusedAtTheNextCall(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the first call answered %d %q; want 200", answer.StatusCode, body)
	}
	restricted := fmt.Errorf("%w: restricted member: this grant is released to no restricted member's session", requests.ErrGrantNotLive)
	r.grants.refuseConnection(workerPlaceholder, restricted)
	// The same client, so the same connection the first call left open: nothing but the read
	// each request makes can refuse it.
	answer, body = r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode != http.StatusProxyAuthRequired || !strings.Contains(body, restricted.Error()) {
		t.Fatalf("the call after the person became restricted answered %d %q; want 407 in the broker's words", answer.StatusCode, body)
	}
	if seen := r.requests(); len(seen) != 1 {
		t.Fatalf("the bound host was sent %d requests; want only the first call's", len(seen))
	}
}
