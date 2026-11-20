// packages/envoy/internal/broker/helper/enroll_restricted_member_test.go
//go:build linux

package helper

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestARestrictedMembersEnrollmentIsAskedOnce: the broker refuses every enrollment under a machine
// login whose person Dispatch calls a restricted member, 403 RESTRICTED_MEMBER_ENROLLS_NOTHING,
// and that answer is about the person, so no retry mends it. The helper's enroll loop makes ONE
// call for the session and returns: the session reads "refused" with the broker's refusal as its
// last error, a sign answers NOT_ENROLLED naming it, and a `register --wait` is answered at the
// refusal rather than when its wait runs out.
//
// No clock decides it. The session's wake is set before the loop starts, so a loop that went on
// retrying after the refusal would read that wake at once and make its second call without
// waiting out any backoff; the test waits for whichever comes first, the loop returning or that
// second call, and then requires the first with exactly one call. A loop that retries never
// returns, so it can only fail. The register is given an hour's wait, which only the refusal can
// end, and the bounds below are for a box too stalled to answer at all, never a pass.
func TestARestrictedMembersEnrollmentIsAskedOnce(t *testing.T) {
	const stalled = 60 * time.Second
	f := newFakeBroker(t)
	f.enrollRestricted = true
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := &Server{
		Registry: NewRegistry(filepath.Join(t.TempDir(), "sessions.json")),
		Broker: &Broker{URL: brokeraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"),
			MachineFile: machineFilePath(t), HTTP: f.srv.Client(), Log: log},
		Hostname: "testhost", PeerOf: PeerOf, Log: log, MinRenew: time.Second,
	}
	t.Cleanup(srv.Broker.Close)
	if _, err := srv.Broker.Login(context.Background(), "testhost"); err != nil {
		t.Fatalf("login: %v", err)
	}
	waitFor(t, func() bool { return srv.Broker.LoginStatus().State == "issued" })

	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	// No peer: this drives the enrollment loop alone, which needs the session's identity and
	// nothing of the process behind it.
	sess, err := newSession(os.Getpid(), 1, "testhost:1:1", "", key, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Registry.Add(sess)
	// The wake a retrying loop would read after its first failure, at once.
	sess.wakeEnroll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		srv.enrollLoop(ctx, sess)
		close(done)
	}()
	arrivals := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.enrollArrivals
	}
	returned := func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	if !testwait.Holds(stalled, 5*time.Millisecond, func() bool { return returned() || arrivals() >= 2 }) {
		t.Fatalf("in %s the enroll loop neither returned nor made a second call (%d calls)", stalled, arrivals())
	}
	if !returned() {
		t.Fatalf("the enroll loop is still retrying a session the broker refused RESTRICTED_MEMBER_ENROLLS_NOTHING: the broker received %d enroll calls, want one and the loop ended", arrivals())
	}
	if n := arrivals(); n != 1 {
		t.Fatalf("the broker received %d enroll calls for the refused session; want exactly one", n)
	}

	st := sess.snapshot()
	if st.State != "refused" || st.EnrollmentID != "" || !strings.Contains(st.LastError, "RESTRICTED_MEMBER_ENROLLS_NOTHING") {
		t.Fatalf("the refused session reads %+v; want state refused, no enrollment, and the broker's refusal as its last error", st)
	}
	if signed := srv.notEnrolled(st); signed.Code != CodeNotEnrolled || !strings.Contains(signed.Error, "refuses to enroll this session") ||
		!strings.Contains(signed.Error, "RESTRICTED_MEMBER_ENROLLS_NOTHING") {
		t.Fatalf("sign of the refused session: %+v; want NOT_ENROLLED naming the refusal", signed)
	}

	replied := make(chan Response, 1)
	go func() { replied <- srv.registerReply(sess, time.Hour) }()
	select {
	case reg := <-replied:
		if !reg.OK || reg.State != "refused" || !strings.Contains(reg.Error, "RESTRICTED_MEMBER_ENROLLS_NOTHING") {
			t.Fatalf("register --wait of the refused session: %+v; want state refused naming RESTRICTED_MEMBER_ENROLLS_NOTHING", reg)
		}
	case <-time.After(stalled):
		t.Fatalf("a register given an hour's wait was not answered in %s: it waits for an enrollment that cannot come, not for the refusal", stalled)
	}
}
