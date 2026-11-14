// packages/envoy/internal/broker/helper/schedule_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/serverwait"
)

func answered(status int, retryAfter string) error {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &oauth2.RetrieveError{Response: &http.Response{StatusCode: status, Header: h}, ErrorCode: "invalid_request"}
}

// TestAfterALostAnswerAnsweredRefusalsKeepTheFixedRetry: once a refresh went unanswered, the
// broker may hold a rotation the helper never learned of, so answered 4xx and 429s that follow
// keep the 15 s retry rather than starting the backoff, until a success (reset) ends that; only
// then does a 4xx back off again.
func TestAfterALostAnswerAnsweredRefusalsKeepTheFixedRetry(t *testing.T) {
	lost := &url.Error{Op: "Post", URL: "https://broker.internal.example/v1/oauth/token", Err: io.ErrUnexpectedEOF}
	s := (&Broker{}).newRetrySchedule()
	if wait := s.next(lost); wait != defaultRefreshRetry {
		t.Fatalf("after the lost answer: %s; want %s", wait, defaultRefreshRetry)
	}
	for i, err := range []error{answered(400, ""), answered(403, ""), answered(429, "600"), answered(400, ""), lost, answered(401, "")} {
		if wait := s.next(err); wait != defaultRefreshRetry {
			t.Fatalf("answer %d after the lost one: wait %s; want the fixed %s", i+1, wait, defaultRefreshRetry)
		}
	}
	s.reset()
	if wait := s.next(answered(400, "")); wait != defaultRefreshBackoff {
		t.Fatalf("a 400 after a success: %s; want the backoff's %s", wait, defaultRefreshBackoff)
	}
}

// TestARefreshWithNoCredentialSettlesNothing: a refresh that finds the credential already
// dropped (a concurrent LAUNCHER_INVALID) returns errNoOutcome, which the schedule does not read
// as a settled refresh: after a lost answer, the 4xx that follows still waits the fixed retry.
func TestARefreshWithNoCredentialSettlesNothing(t *testing.T) {
	if err := (&Broker{}).refresh(context.Background()); !errors.Is(err, errNoOutcome) {
		t.Fatalf("a refresh holding no credential = %v; want errNoOutcome", err)
	}
	lost := &url.Error{Op: "Post", URL: "https://broker.internal.example/v1/oauth/token", Err: io.ErrUnexpectedEOF}
	s := (&Broker{}).newRetrySchedule()
	day := func() time.Duration { return 24 * time.Hour }
	s.after(lost, day)
	s.after(errNoOutcome, day)
	if wait := s.after(answered(400, ""), day); wait != defaultRefreshRetry {
		t.Fatalf("a 400 after a lost answer and a refresh with no outcome: %s; want the fixed %s", wait, defaultRefreshRetry)
	}
	if wait := s.after(nil, day); wait != 24*time.Hour || s.unknown {
		t.Fatalf("a settled refresh: wait %s, unknown %v; want a day, cleared", wait, s.unknown)
	}
}

// TestARetryAfterIsCappedAtAMinute: a 429's Retry-After is honoured up to 60 s; a longer one waits
// 60 s, so no hop's large value stops the refresh.
func TestARetryAfterIsCappedAtAMinute(t *testing.T) {
	s := (&Broker{}).newRetrySchedule()
	for retryAfter, want := range map[string]time.Duration{"45": 45 * time.Second, "60": time.Minute, "61": time.Minute, "86400": time.Minute} {
		if wait := s.next(answered(429, retryAfter)); wait != want {
			t.Fatalf("Retry-After %s: wait %s; want %s", retryAfter, wait, want)
		}
	}
}

// TestARetryAfterIsReadByTheOneBound: retryAfter reads the broker's Retry-After by the one rule
// every reader of a server's wait in this module keeps (internal/serverwait), and the schedule
// clamps what it reads. A Retry-After of 9999999999 seconds and one dated in the year 9999 are
// past what a Duration holds and read as serverwait.Longest, so the schedule waits its cap, a
// minute: converted as it came, the count read as a negative wait, which the schedule's floor
// turned into the shortest wait it has, the fixed 15 s retry. A negative Retry-After reads as no
// wait, which waits that retry, and an ordinary one reads as itself. The test counts the rows it
// drove and fails under the number the table must hold.
func TestARetryAfterIsReadByTheOneBound(t *testing.T) {
	const rowsToDrive = 4
	rows := []struct {
		retryAfter  string
		read, waits time.Duration
	}{
		{"9999999999", serverwait.Longest, maxRetryAfter},
		{"Fri, 31 Dec 9999 23:59:59 GMT", serverwait.Longest, maxRetryAfter},
		{"-30", 0, defaultRefreshRetry},
		{"30", 30 * time.Second, 30 * time.Second},
	}
	driven := 0
	for _, row := range rows {
		t.Run("Retry-After "+row.retryAfter, func(t *testing.T) {
			driven++
			err := answered(http.StatusTooManyRequests, row.retryAfter)
			if read := retryAfter(err); read != row.read {
				t.Errorf("retryAfter = %s, want %s", read, row.read)
			}
			s := (&Broker{}).newRetrySchedule()
			if wait := s.next(err); wait != row.waits {
				t.Errorf("the schedule waits %s, want %s", wait, row.waits)
			}
		})
	}
	if driven < rowsToDrive {
		t.Fatalf("the table drove %d rows, want %d", driven, rowsToDrive)
	}
}

// TestATokenAnswerWithoutARefreshTokenIsNotSaved: a 200 that names no refresh_token (x/oauth2
// would copy the presented one in) is an error answer: the held login and the machine file keep
// the presented token, and the refresh is retried on the fixed retry.
func TestATokenAnswerWithoutARefreshTokenIsNotSaved(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	held := b.cred.Load()
	f.mu.Lock()
	f.refreshOutcome = "no_refresh_token"
	f.mu.Unlock()
	err := b.refresh(context.Background())
	if err == nil {
		t.Fatal("a token answer with no refresh_token must be an error")
	}
	if wait := b.newRetrySchedule(); wait.next(err) != defaultRefreshRetry {
		t.Fatalf("its retry: %s; want %s", wait.next(err), defaultRefreshRetry)
	}
	if b.cred.Load() != held {
		t.Fatal("the answer without a refresh_token replaced the held login")
	}
	if kept, err := LoadMachineFile(b.MachineFile); err != nil || kept.RefreshToken != held.refreshToken {
		t.Fatalf("machine file %+v, %v; want the presented token kept", kept, err)
	}
}

// TestAReLoginAfterALauncherRefusalRetiresTheDroppedCredential: a credential a 401
// LAUNCHER_INVALID dropped is retired at the broker once a new login is installed, as one the new
// login replaces is, so no old copy of its file can still refresh it.
func TestAReLoginAfterALauncherRefusalRetiresTheDroppedCredential(t *testing.T) {
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	dropped := b.cred.Load().id
	f.mu.Lock()
	f.enrollUnauthorizedNext = 1
	f.mu.Unlock()
	if _, _, err := b.EnrollSessionKey(context.Background(), "rt", "tp", nil); !errors.Is(err, errNoCredential) {
		t.Fatalf("the refused enroll: %v; want errNoCredential", err)
	}
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { cred := b.cred.Load(); return cred != nil && cred.id != dropped })
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.retired, dropped)
	})
}
