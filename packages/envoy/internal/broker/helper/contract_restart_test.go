// packages/envoy/internal/broker/helper/contract_restart_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// failingTokenTransport fails every request to the broker's token endpoint as an unreachable
// broker would, and sends every other request on.
type failingTokenTransport struct{}

func (failingTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/v1/oauth/token") {
		return nil, errors.New("dial tcp: connection refused")
	}
	return http.DefaultTransport.RoundTrip(r)
}

// losingTokenTransport sends every request on, and after the first refresh grant's round trip
// (which the broker answers and commits) reports the answer lost, as a dropped connection would.
type losingTokenTransport struct {
	mu   sync.Mutex
	lost bool
}

func (l *losingTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || !strings.HasSuffix(r.URL.Path, "/v1/oauth/token") {
		return resp, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return resp, nil
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"refresh_token"`) || !strings.Contains(string(body), "credential_id") {
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		return resp, nil
	}
	l.lost = true
	return nil, errors.New("read tcp: connection reset by peer")
}

// TestContractALostRefreshAnswerIsRetriedInsideTheGrace: the broker rotates a refresh and
// commits it, but its answer never reaches the helper. The helper's retry, at its first retry
// interval, redeems the refresh token it still holds inside the broker's rotation grace, so the
// login is kept, rotated, in memory and on disk.
func TestContractALostRefreshAnswerIsRetriedInsideTheGrace(t *testing.T) {
	first := newContractRig(t)
	credentialID := first.login(t)
	kept, err := LoadMachineFile(first.machineFile)
	if err != nil {
		t.Fatal(err)
	}
	first.cancel()
	lossy := &losingTokenTransport{}
	// The retry after a lost answer is the refresh loop's own wait, armed on this broker's
	// clock at the production retry, and fired here rather than shortened.
	fake := clocktest.New(time.Now())
	b := &Broker{URL: brokeraddr.New(first.broker.URL), OperatorFile: first.broker.OperatorFile, MachineFile: first.machineFile,
		HTTP: &http.Client{Transport: lossy}, Clock: fake}
	t.Cleanup(b.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	fireArmedAt(t, fake, fake.Now().Add(b.refreshRetryOrDefault()), "the refresh loop's retry after the lost answer")
	waitFor(t, func() bool {
		now, err := LoadMachineFile(first.machineFile)
		return err == nil && now.RefreshToken != kept.RefreshToken
	})
	lossy.mu.Lock()
	lost := lossy.lost
	lossy.mu.Unlock()
	if !lost {
		t.Fatal("no refresh answer was lost")
	}
	if ls := b.LoginStatus(); ls.State != "issued" || !ls.CredentialHeld || b.cred.Load().id != credentialID {
		t.Fatalf("login-status after the retried refresh: %+v; want %s issued and held", ls, credentialID)
	}
}

// TestContractALoginSurvivesARestart: an approved machine login leaves a machine file readable
// only by its user, and a new helper started on that file reports issued at once and enrolls a
// session with no new approval. Its start refreshes the login against the real broker, which
// rotates the refresh token the file keeps.
func TestContractALoginSurvivesARestart(t *testing.T) {
	first := newContractRig(t)
	credentialID := first.login(t)
	info, err := os.Stat(first.machineFile)
	if err != nil {
		t.Fatalf("the approved login's machine file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("machine file mode %o; want 600", mode)
	}
	kept, err := LoadMachineFile(first.machineFile)
	if err != nil || kept.CredentialID != credentialID {
		t.Fatalf("machine file %+v (%v); want the login %s", kept, err, credentialID)
	}

	second := startContractHelper(t, first.broker, first.machineFile, http.DefaultClient)
	if status := second.call(t, Request{Op: "login-status"}); !status.OK || status.LoginState != "issued" || !status.CredentialHeld {
		t.Fatalf("login-status of a helper started on the machine file: %+v; want issued and held", status)
	}
	reg := second.call(t, Request{Op: "register", WaitSeconds: 10})
	if !reg.OK || reg.State != "enrolled" || reg.EnrollmentID == "" {
		t.Fatalf("register on the restarted helper: %+v; want the session enrolled", reg)
	}
	waitForIssued(t, second.srv.Broker)
	waitFor(t, func() bool {
		now, err := LoadMachineFile(first.machineFile)
		return err == nil && now.RefreshToken != kept.RefreshToken && now.CredentialID == credentialID
	})
}

// TestContractARevokedMachinesNextRefreshDropsItsLogin: the person who approved the login revokes
// it, as Dispatch's machine-login page does. The next refresh (here a restart's) is refused, so the
// helper drops the credential with the refresh-refused reason, deletes its machine file, and
// login-status reads expired, refused for that reason.
func TestContractARevokedMachinesNextRefreshDropsItsLogin(t *testing.T) {
	first := newContractRig(t)
	credentialID := first.login(t)
	status, body := first.broker.UI(t, http.MethodPost, "/v1/launcher-credentials/"+credentialID+"/revoke-by-approver",
		map[string]any{"approver": first.broker.Operator})
	if status != http.StatusOK {
		t.Fatalf("revoke the machine login = %d: %s", status, body)
	}

	second := startContractHelper(t, first.broker, first.machineFile, http.DefaultClient)
	waitFor(t, func() bool { return !second.srv.Broker.HasCredential() })
	got := second.call(t, Request{Op: "login-status"})
	if !got.OK || got.LoginState != "expired" || !got.LoginRefused || got.CredentialDropped != dropRefreshRefused || got.CredentialHeld {
		t.Fatalf("login-status after the refused refresh: %+v; want expired, dropped for %q", got, dropRefreshRefused)
	}
	if _, err := LoadMachineFile(first.machineFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the machine file after the refused refresh: %v; want it deleted", err)
	}
	refused := `level=ERROR msg="` + dropRefreshRefused + `; cleared: no session can enroll until a human approves a new machine login (run: agent-secrets machine login)" credential_id=` + credentialID + " code=invalid_grant"
	if !strings.Contains(second.logBuf.String(), refused) {
		t.Fatalf("log:\n%s\nwant %s", second.logBuf.String(), refused)
	}
}

// TestContractARefreshThatCannotReachTheBrokerKeepsTheLogin: a restarted helper whose refresh
// cannot reach the token endpoint keeps the login it kept, reports it issued, and enrolls sessions
// with it; the machine file is unchanged.
func TestContractARefreshThatCannotReachTheBrokerKeepsTheLogin(t *testing.T) {
	first := newContractRig(t)
	first.login(t)
	kept, err := LoadMachineFile(first.machineFile)
	if err != nil {
		t.Fatal(err)
	}

	second := startContractHelper(t, first.broker, first.machineFile, &http.Client{Transport: failingTokenTransport{}})
	waitFor(t, func() bool {
		return strings.Contains(second.logBuf.String(), "the machine login's refresh did not reach the broker; retrying")
	})
	if status := second.call(t, Request{Op: "login-status"}); !status.OK || status.LoginState != "issued" || !status.CredentialHeld {
		t.Fatalf("login-status after a refresh that failed: %+v; want issued and held", status)
	}
	if reg := second.call(t, Request{Op: "register", WaitSeconds: 10}); !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register after a refresh that failed: %+v; want the session enrolled", reg)
	}
	now, err := LoadMachineFile(first.machineFile)
	if err != nil || now.RefreshToken != kept.RefreshToken {
		t.Fatalf("the machine file after a refresh that failed: %+v %v; want it unchanged", now, err)
	}
}

// TestContractAnUnrefreshedLoginDropsAtItsExpiry: a helper whose refresh never reaches the broker
// holds its login until the expiry it kept, then drops it for that reason, as the broker's own
// lapse would refuse it.
func TestContractAnUnrefreshedLoginDropsAtItsExpiry(t *testing.T) {
	first := newContractRig(t)
	first.login(t)
	kept, err := LoadMachineFile(first.machineFile)
	if err != nil {
		t.Fatal(err)
	}
	kept.ExpiresAt = time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := kept.Save(first.machineFile); err != nil {
		t.Fatal(err)
	}

	// The expiry watch arms its drop on the broker's own clock, for the expiry the login
	// kept: the row reads that it is armed for exactly that moment and fires it, so the drop
	// cannot come before the expiry and nothing waits an hour.
	fake := clocktest.New(time.Now())
	second := startContractHelper(t, first.broker, first.machineFile, &http.Client{Transport: failingTokenTransport{}}, func(b *Broker) { b.Clock = fake })
	if !second.srv.Broker.HasCredential() {
		t.Fatal("the restarted helper holds no login")
	}
	drop := testwait.Until(t, "the expiry watch to arm its drop at the kept expiry", func() (time.Time, bool) {
		for _, at := range fake.Armed() {
			if at.Equal(kept.ExpiresAt) {
				return at, true
			}
		}
		return time.Time{}, false
	})
	fake.Set(drop)
	waitFor(t, func() bool { return !second.srv.Broker.HasCredential() })
	if got := second.call(t, Request{Op: "login-status"}); got.LoginState != "expired" || got.CredentialDropped != dropExpired {
		t.Fatalf("login-status after the expiry: %+v; want expired, dropped for %q", got, dropExpired)
	}
}

// TestContractAReLoginReplacesTheKeptLogin: a machine login while one is held mints a new key and
// a new login; the machine file names the old login until the new one is issued, then the new one.
func TestContractAReLoginReplacesTheKeptLogin(t *testing.T) {
	cr := newContractRig(t)
	first := cr.login(t)
	code, err := cr.srv.Broker.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatal(err)
	}
	if kept, err := LoadMachineFile(cr.machineFile); err != nil || kept.CredentialID != first {
		t.Fatalf("machine file while the re-login is pending: %+v %v; want the first login %s", kept, err, first)
	}
	second := cr.broker.DecideMachineLogin(t, code, true)
	waitFor(t, func() bool {
		cred := cr.srv.Broker.cred.Load()
		return cred != nil && cred.id == second
	})
	kept, err := LoadMachineFile(cr.machineFile)
	if err != nil || kept.CredentialID != second || kept.Key.D.Cmp(cr.srv.Broker.cred.Load().key.D) != 0 {
		t.Fatalf("machine file after the re-login: %+v %v; want the second login %s and its key", kept, err, second)
	}
}

// TestContractAReLoginRetiresTheCredentialItReplaces: once a re-login is installed, the helper
// retires the credential it replaced at the broker, signing with that credential's own launcher
// proof: no old copy of the machine file can refresh it, and the session it enrolled keeps its
// enrollment and renews it; the new login keeps working.
func TestContractAReLoginRetiresTheCredentialItReplaces(t *testing.T) {
	cr := newContractRig(t, brokertest.WithLease(3*time.Second))
	first := cr.login(t)
	reg := cr.call(t, Request{Op: "register", WaitSeconds: 10})
	if !reg.OK || reg.State != "enrolled" || reg.EnrollmentID == "" {
		t.Fatalf("register: %+v", reg)
	}
	code, err := cr.srv.Broker.Login(context.Background(), cr.srv.Hostname)
	if err != nil {
		t.Fatal(err)
	}
	second := cr.broker.DecideMachineLogin(t, code, true)
	waitFor(t, func() bool {
		return strings.Contains(cr.logBuf.String(), "retired the machine login a new one replaced")
	})
	var revoked, expired bool
	if err := cr.broker.Store.Pool.QueryRow(context.Background(), `select revoked_at is not null, expires_at <= now() from launcher_credentials where id = $1`, first).Scan(&revoked, &expired); err != nil || revoked || !expired {
		t.Fatalf("replaced credential revoked %v, expired %v (%v); want retired: expired, not revoked", revoked, expired, err)
	}
	var lease time.Time
	if err := cr.broker.Store.Pool.QueryRow(context.Background(), `select lease_expires_at from enrollments where id = $1`, reg.EnrollmentID).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	// The session renews its enrollment with its own proof after its login retired.
	waitFor(t, func() bool {
		var live bool
		var now time.Time
		if err := cr.broker.Store.Pool.QueryRow(context.Background(), `select revoked_at is null, lease_expires_at from enrollments where id = $1`, reg.EnrollmentID).Scan(&live, &now); err != nil {
			t.Fatal(err)
		}
		if !live {
			t.Fatalf("the session's enrollment %s ended when its login was retired", reg.EnrollmentID)
		}
		return now.After(lease)
	})
	if s := cr.call(t, Request{Op: "sessions"}); len(s.Sessions) != 1 || s.Sessions[0].EnrollmentID != reg.EnrollmentID {
		t.Fatalf("sessions after the retire = %+v; want the one session on enrollment %s", s.Sessions, reg.EnrollmentID)
	}
	if cred := cr.srv.Broker.cred.Load(); cred == nil || cred.id != second {
		t.Fatalf("after the retire the helper holds %+v; want the new login %s", cred, second)
	}
	if err := cr.srv.Broker.refresh(context.Background()); err != nil || !cr.srv.Broker.HasCredential() {
		t.Fatalf("the new login's refresh after the retire: %v, held %v; want it refreshed", err, cr.srv.Broker.HasCredential())
	}
}

// TestContractALostRefreshAnswerIsRedeemableUntilTheGraceEnds: the broker rotates a refresh but
// its answer is lost. A retry two minutes later (the grace pushed back, not waited for) still
// redeems the token the helper holds and keeps the login; a retry once the grace has ended is a
// reuse, which the broker refuses, and the helper drops the login and its file.
func TestContractALostRefreshAnswerIsRedeemableUntilTheGraceEnds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shift string
		keeps bool
	}{
		{"two minutes later", `update oauth_refresh_tokens set grace_until = grace_until - interval '2 minutes' where grace_until is not null`, true},
		{"after the grace", `update oauth_refresh_tokens set grace_until = now() - interval '1 second' where grace_until is not null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := newContractRig(t)
			first.login(t)
			first.cancel()
			lossy := &losingTokenTransport{}
			// A retry an hour out, so the only refresh after Start's is the row's own call.
			b := &Broker{URL: brokeraddr.New(first.broker.URL), OperatorFile: first.broker.OperatorFile, MachineFile: first.machineFile,
				HTTP: &http.Client{Transport: lossy}, refreshRetry: time.Hour}
			t.Cleanup(b.Close)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := b.Start(ctx); err != nil {
				t.Fatal(err)
			}
			lossy.mu.Lock()
			lost := lossy.lost
			lossy.mu.Unlock()
			if !lost {
				t.Fatal("the start's refresh answer was not lost")
			}
			if _, err := first.broker.Store.Pool.Exec(ctx, tc.shift); err != nil {
				t.Fatal(err)
			}
			err := b.refresh(ctx)
			_, fileErr := LoadMachineFile(first.machineFile)
			if tc.keeps {
				if err != nil || !b.HasCredential() || fileErr != nil {
					t.Fatalf("retry inside the grace: %v, held %v, file %v; want the login kept", err, b.HasCredential(), fileErr)
				}
				return
			}
			if b.HasCredential() || b.LoginStatus().Dropped != dropRefreshRefused || !errors.Is(fileErr, os.ErrNotExist) {
				t.Fatalf("retry after the grace: %v, status %+v, file %v; want the login dropped and its file gone", err, b.LoginStatus(), fileErr)
			}
		})
	}
}
