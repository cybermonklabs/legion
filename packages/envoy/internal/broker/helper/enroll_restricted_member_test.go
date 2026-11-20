// packages/envoy/internal/broker/helper/enroll_restricted_member_test.go
//go:build linux

package helper

import (
	"strings"
	"testing"
	"time"
)

// TestARestrictedMembersEnrollmentIsAskedOnce: the broker refuses every enrollment under a machine
// login whose person Dispatch calls a restricted member, 403 RESTRICTED_MEMBER_ENROLLS_NOTHING,
// and that answer is about the person, so no retry mends it. The helper makes ONE enroll call for
// the session and asks no more: a `register --wait` waiting on the enrollment is answered at once,
// state refused, with the broker's refusal in its error, rather than when its wait runs out; a
// sign answers NOT_ENROLLED naming the refusal; and no second call reaches the broker in the time
// the retry's first backoffs (one second, then two) would have made two more.
func TestARestrictedMembersEnrollmentIsAskedOnce(t *testing.T) {
	r := startRig(t, "")
	r.fake.mu.Lock()
	r.fake.enrollRestricted = true
	r.fake.mu.Unlock()

	const wait = 20
	started := time.Now()
	reg := r.call(t, Request{Op: "register", WaitSeconds: wait})
	if took := time.Since(started); took >= wait*time.Second/2 {
		t.Fatalf("register --wait %d answered after %s; want it answered at the refusal, not when the wait ran out", wait, took)
	}
	if !reg.OK || reg.State != "refused" || !strings.Contains(reg.Error, "RESTRICTED_MEMBER_ENROLLS_NOTHING") {
		t.Fatalf("register while the broker refuses the machine login's person: %+v; want state refused naming RESTRICTED_MEMBER_ENROLLS_NOTHING", reg)
	}

	signed := r.call(t, Request{Op: "sign", Method: "GET", URL: "https://x/v1/enrollments/self"})
	if signed.OK || signed.Code != CodeNotEnrolled || !strings.Contains(signed.Error, "refuses to enroll this session") ||
		!strings.Contains(signed.Error, "RESTRICTED_MEMBER_ENROLLS_NOTHING") {
		t.Fatalf("sign of a refused session: %+v; want NOT_ENROLLED naming the refusal", signed)
	}

	// Past the first two backoffs of the retry (1 s, then 2 s), which would have made two calls more.
	time.Sleep(4 * time.Second)
	r.fake.mu.Lock()
	arrivals := r.fake.enrollArrivals
	r.fake.mu.Unlock()
	if arrivals != 1 {
		t.Fatalf("the broker received %d enroll calls for the refused session; want exactly one", arrivals)
	}
}
