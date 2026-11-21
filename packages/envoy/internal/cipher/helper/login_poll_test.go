// packages/envoy/internal/cipher/helper/login_poll_test.go
//go:build linux

package helper

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
)

// TestALoginPollTheBrokerCouldNotAnswerSettlesNothing: the device code grant settles a login only
// from a 400 or 401 answer (RFC 8628 section 3.5). A 5xx or a 429 whose body names access_denied
// or expired_token is a broker that could not answer, so the login stays pending and the helper
// polls again; the same error at the broker's own status then settles it.
func TestALoginPollTheBrokerCouldNotAnswerSettlesNothing(t *testing.T) {
	for _, c := range []struct {
		status  int
		outcome string // the device code grant's answer: "denied" (access_denied) or "expired" (expired_token)
	}{
		{http.StatusServiceUnavailable, "denied"},
		{http.StatusServiceUnavailable, "expired"},
		{http.StatusInternalServerError, "expired"},
		{http.StatusTooManyRequests, "denied"},
	} {
		t.Run(fmt.Sprintf("%d %s", c.status, c.outcome), func(t *testing.T) {
			f := newFakeBroker(t)
			b := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client()}
			t.Cleanup(b.Close)
			f.mu.Lock()
			f.loginOutcome, f.oauthErrorStatus = c.outcome, c.status
			f.mu.Unlock()
			if _, err := b.Login(context.Background(), "host-a"); err != nil {
				t.Fatal(err)
			}
			// A second poll is the helper's answer to the first: it polls again only when that
			// answer settled nothing.
			waitFor(t, func() bool {
				f.mu.Lock()
				defer f.mu.Unlock()
				return f.devicePolls >= 2
			})
			if got := b.LoginStatus().State; got != "pending" {
				t.Fatalf("after the broker answered %d naming the %s login's error twice: login-status %q, want pending", c.status, c.outcome, got)
			}
			f.mu.Lock()
			f.oauthErrorStatus = 0
			f.mu.Unlock()
			waitFor(t, func() bool { return b.LoginStatus().State == c.outcome })
		})
	}
}
