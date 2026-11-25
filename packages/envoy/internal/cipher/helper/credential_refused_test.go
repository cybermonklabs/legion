// packages/envoy/internal/cipher/helper/credential_refused_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact"
)

// launcherInvalid is the broker's 401 LAUNCHER_INVALID, its answer to a launcher proof it rejects,
// as Broker.do reads it beside a clean address.
func launcherInvalid() *BrokerError {
	return newBrokerError(http.StatusUnauthorized, redact.NewRemoteText("LAUNCHER_INVALID"), redact.NewRemoteText("the launcher credential is not valid"), false)
}

// TestNoCredentialAndExpiredCredentialBothNameTheLoginCommand covers the fail-closed contract:
// Enroll before any Login names the login command; once a broker 401 LAUNCHER_INVALID clears the
// credential (expired or revoked), the very next Enroll names it again, with no auto-relogin. The
// refusal keeps the machine file, since the broker answers it for clock skew or a URL mismatch as
// well, and the next start's refresh settles whether the login still stands; nothing else is
// written under HOME.
func TestNoCredentialAndExpiredCredentialBothNameTheLoginCommand(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv).
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := newFakeBroker(t)
	b := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client()}
	t.Cleanup(b.Close)
	sess, _ := newSession(1, 1, "h:1:1", "", nil, nil)

	if _, _, err := b.EnrollIdentity(context.Background(), sess.identity()); err == nil || !strings.Contains(err.Error(), "agent-secrets machine login") {
		t.Fatalf("no credential yet: expected the remedy in the error, got %v", err)
	}

	if _, err := b.Login(context.Background(), "example-host-devbox"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "issued" })
	if _, _, err := b.EnrollIdentity(context.Background(), sess.identity()); err != nil {
		t.Fatalf("a fresh credential must enroll: %v", err)
	}

	f.mu.Lock()
	f.enrollUnauthorizedNext = 1
	f.mu.Unlock()
	if _, _, err := b.EnrollIdentity(context.Background(), sess.identity()); err == nil || !strings.Contains(err.Error(), "agent-secrets machine login") {
		t.Fatalf("expired credential: expected the remedy in the error, got %v", err)
	}
	if b.cred.Load() != nil {
		t.Fatal("a 401 LAUNCHER_INVALID must clear the credential")
	}
	if _, _, err := b.EnrollIdentity(context.Background(), sess.identity()); err == nil || !strings.Contains(err.Error(), "agent-secrets machine login") {
		t.Fatalf("cleared credential: expected the remedy again, got %v", err)
	}
	if _, err := LoadMachineFile(b.MachineFile); err != nil {
		t.Fatalf("a 401 LAUNCHER_INVALID must keep the machine file: %v", err)
	}

	files := 0
	if err := filepath.WalkDir(home, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("nothing but the machine file may be written; found %d file(s) under HOME", files)
	}
}

// TestALateRejectionOfAnOldCredentialLeavesTheNewLoginAlone covers a 401 LAUNCHER_INVALID that
// answers a call signed with a credential another login has since replaced (a session's enroll
// retry in flight while the operator logs in again): it clears nothing, since the credential it
// rejects is no longer the one installed, and the new login still reports "issued". The caller
// gets the broker's refusal rather than errNoCredential, since the helper holds a credential and
// its retry can use it.
func TestALateRejectionOfAnOldCredentialLeavesTheNewLoginAlone(t *testing.T) {
	t.Parallel()
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	old := b.cred.Load()
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.cred.Load() != old && b.LoginStatus().State == "issued" })
	fresh := b.cred.Load()

	rejected := launcherInvalid()
	if err := b.clearOnInvalid(context.Background(), old, rejected); err != rejected {
		t.Fatalf("a rejection of a replaced credential reached its caller as %v; want the broker's refusal as it came", err)
	}
	if b.cred.Load() != fresh || b.LoginStatus().State != "issued" {
		t.Fatalf("a late rejection of the replaced credential changed the new login: cred replaced %v, state %q", b.cred.Load() != fresh, b.LoginStatus().State)
	}

	if err := b.clearOnInvalid(context.Background(), fresh, rejected); !errors.Is(err, errNoCredential) {
		t.Fatal(err)
	}
	if b.cred.Load() != nil || b.LoginStatus().State == "issued" {
		t.Fatalf("rejecting the installed credential must clear it and end its login: state %q", b.LoginStatus().State)
	}
}

// TestLoginStatusReadsARefusalWithTheClearThatCausedIt: a refusal clears the credential and
// records that the broker refused it in one write under stateMu. A login-status read between the
// two would find no credential and no refusal, and report the denied re-login before it as the
// answer, where the answer is the refused credential. The test hook reads login-status between the
// clear and the record; it waits up to 200 ms for that read, which returns at once unless the lock
// holds it back until the refusal is recorded.
func TestLoginStatusReadsARefusalWithTheClearThatCausedIt(t *testing.T) {
	t.Parallel()
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	held := b.cred.Load()
	f.mu.Lock()
	f.loginOutcome = "denied"
	f.mu.Unlock()
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.LoginStatus().State == "denied" })
	read := make(chan loginState, 1)
	b.testClearHook = func() {
		go func() { read <- b.LoginStatus() }()
		select {
		case got := <-read:
			read <- got
		case <-time.After(200 * time.Millisecond):
		}
	}
	rejected := launcherInvalid()
	if err := b.clearOnInvalid(context.Background(), held, rejected); !errors.Is(err, errNoCredential) {
		t.Fatal(err)
	}
	if got := <-read; got.State != "expired" || !got.Refused || got.CredentialHeld {
		t.Fatalf("login-status read as the credential was refused after a denied re-login: %+v, want expired, refused, no credential", got)
	}
}

// TestALoginPendingWhenTheCredentialIsRefusedReportsItsOwnOutcome: a re-login is waiting for
// approval when the broker refuses the credential an earlier login installed. While it waits,
// login-status reads it pending and the credential refused. Once the operator denies it, it reads
// denied and no longer refused: its outcome is newer than the refusal, and `machine login`, which
// reads the same answer, must tell the operator it was denied.
func TestALoginPendingWhenTheCredentialIsRefusedReportsItsOwnOutcome(t *testing.T) {
	t.Parallel()
	f := newFakeBroker(t)
	b := loggedInBroker(t, f, "ada@example.com")
	held := b.cred.Load()
	f.mu.Lock()
	f.loginOutcome = "pending"
	f.mu.Unlock()
	if _, err := b.Login(context.Background(), "helper-host"); err != nil {
		t.Fatal(err)
	}
	rejected := launcherInvalid()
	if err := b.clearOnInvalid(context.Background(), held, rejected); !errors.Is(err, errNoCredential) {
		t.Fatal(err)
	}
	if got := b.LoginStatus(); got.State != "pending" || !got.Refused || got.CredentialHeld {
		t.Fatalf("a re-login pending when the credential was refused: %+v, want pending, refused, no credential", got)
	}
	f.mu.Lock()
	f.loginOutcome = "denied"
	f.mu.Unlock()
	deadline := time.Now().Add(15 * time.Second) // pollLogin backs off 2 s, then 4 s, between polls
	for b.login.Load().State == "pending" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := b.LoginStatus(); got.State != "denied" || got.Refused || got.CredentialHeld {
		t.Fatalf("that re-login once denied: %+v, want denied, not refused, no credential", got)
	}
}
