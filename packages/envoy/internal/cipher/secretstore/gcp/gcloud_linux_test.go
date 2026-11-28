//go:build linux && (amd64 || arm64)

// A gcloud call that reaches its deadline kills every process left in gcloud's process group
// (runGcloud, through the RunChild it is given), a child that holds gcloud's output among them.
// These tests read /proc to see a stub gcloud's child running while the call runs and gone once
// the call has ended (gcptest.GcloudChild), so they build where it does: Linux on amd64 or arm64.

package gcp_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
)

// gcloudHeldDeadline is what these tests make the deadline on each store call
// (secretstore.CallDeadline) and gcp.GcloudSettingsDeadline: longer than any run, so no timer ends
// a call. The test ends each call itself, once it has seen the stub gcloud's child running
// (gcptest.GcloudChild.CallEnds), by firing the call's context as a deadline (firedDeadline), the
// way a timer's expiry ends it. So the child is always seen before the kill, whatever the machine's
// load. That the timer itself ends the call at the deadline, naming it, is
// TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline's and the CLI's
// TestGCPSecretGcloudLoginListThatNeverAnswersEndsAtTheStoreDeadline's.
const gcloudHeldDeadline = time.Hour

// runWaitDelay is how long gcptest.Run waits for a gcloud's output once gcloud has exited (its
// WaitDelay): what a call whose gcloud exited, leaving a child that holds the output, may take
// once released.
const runWaitDelay = time.Second

// firedDeadline is a context that ends when the test fires it (fire): Done closes then and Err
// answers context.DeadlineExceeded, as an expired deadline's context does. A context the code
// derives from it (context.WithTimeout) ends with it, with the same error, so the code takes the
// path its own deadline takes: the kill of gcloud's group (the RunChild's cancel) and its "did not
// answer" error.
type firedDeadline struct {
	done chan struct{}
	once sync.Once
}

// newFiredDeadline is a firedDeadline that t's end fires, so a test that failed before firing it
// still lets its call end.
func newFiredDeadline(t *testing.T) *firedDeadline {
	d := &firedDeadline{done: make(chan struct{})}
	t.Cleanup(d.fire)
	return d
}

func (d *firedDeadline) Deadline() (time.Time, bool) { return time.Time{}, false }
func (d *firedDeadline) Done() <-chan struct{}       { return d.done }
func (d *firedDeadline) Value(any) any               { return nil }
func (d *firedDeadline) fire()                       { d.once.Do(func() { close(d.done) }) }

func (d *firedDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// TestGcloudTokenRefreshAtItsDeadlineKillsGcloudsChild: a refresh of an expired gcloud token
// (gcloudToken.Token, `gcloud auth print-access-token`) that gcloud never answers ends at the
// deadline on each store call (secretstore.CallDeadline), naming the account and the deadline, and
// the child that held gcloud's output is gone with it, not left running. The test fires the
// deadline once it has seen the child running (CallEnds); the timer's own expiry and its error
// alone, on every platform, are TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline's.
func TestGcloudTokenRefreshAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (gcptest.GcloudStubIn's t.Setenv); it sets a package variable the package's other tests read (secretstore.CallDeadline, through useCallDeadline); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (gcptest.GcloudChild.CallEnds).
	useCallDeadline(t, gcloudHeldDeadline)
	gcloud, child := gcptest.UseGcloudWhoseChildHoldsItsOutput(t)
	deadline := newFiredDeadline(t)
	expired := &oauth2.Token{AccessToken: "old", TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute)}
	source := oauth2.ReuseTokenSource(expired, gcp.GcloudTokenForTest(deadline, gcloud, gcptest.Run, "ada@example.com"))
	err := child.CallEnds(t, "the refresh of an expired token", 0, deadline.fire, func() error {
		_, err := source.Token()
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "ada@example.com") || !strings.Contains(err.Error(), "did not answer within 3600s") {
		t.Fatalf("Token() = %v; want a failure naming the account and the deadline", err)
	}
	child.RequireGone(t)
}

// TestGcloudLoginListAtItsDeadlineKillsGcloudsChild: `gcloud auth list` (gcloudLogins) runs under
// secretstore.CallDeadline as the token's refresh does, so a gcloud that never answers ends at that
// deadline, naming it, and the child that held gcloud's output is gone with it, not left running.
// The test fires the deadline once it has seen the child running (CallEnds).
func TestGcloudLoginListAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (gcptest.GcloudStubIn's t.Setenv); it sets a package variable the package's other tests read (secretstore.CallDeadline, through useCallDeadline); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (gcptest.GcloudChild.CallEnds).
	useCallDeadline(t, gcloudHeldDeadline)
	gcloud, child := gcptest.UseGcloudWhoseChildHoldsItsOutput(t)
	deadline := newFiredDeadline(t)
	err := child.CallEnds(t, "`gcloud auth list`", 0, deadline.fire, func() error {
		return gcp.GcloudLoginsForTest(deadline, gcloud, gcptest.Run)
	})
	if err == nil || !strings.Contains(err.Error(), "`gcloud auth list` did not answer within 3600s") {
		t.Fatalf("gcloudLogins() = %v; want a failure naming the deadline", err)
	}
	child.RequireGone(t)
}

// TestGcloudThatExitsLeavingAChildTakesTheChildWithIt: a gcloud that exits by itself and leaves
// a child holding its output, which the runner's bounded wait for output (runWaitDelay) stops the
// wait for but does not kill, has that child killed once the runner has waited for gcloud,
// whatever the call's outcome. The stub exits once the test, having seen the child running, writes
// its go-ahead file (CallEnds).
func TestGcloudThatExitsLeavingAChildTakesTheChildWithIt(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (gcptest.GcloudStubIn's t.Setenv); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (gcptest.GcloudChild.CallEnds).
	dir := t.TempDir()
	goAhead := filepath.Join(dir, "go")
	// The stub waits for the go-ahead, and gives up when this test's process is gone, so a test
	// that failed before its release leaves no stub behind.
	wait := "while [ ! -e " + secretstore.ShellWord(goAhead) + " ]; do kill -0 " + strconv.Itoa(os.Getpid()) + " 2>/dev/null || exit 1; sleep 0.01; done\nexit 0"
	gcloud, child := gcptest.GcloudStubIn(t, dir, wait)
	release := func() {
		if err := os.WriteFile(goAhead, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = child.CallEnds(t, "a gcloud that exited leaving its child", runWaitDelay, release, func() error {
		return gcp.RunGcloudForTest(context.Background(), gcloud, gcptest.Run, "auth", "list", "--format=json")
	})
	child.RequireGone(t)
}

// TestGcloudSettingsReadAtItsDeadlineKillsGcloudsChild: `gcloud config list`
// (readGcloudSettings) runs under gcp.GcloudSettingsDeadline, so a gcloud that never answers ends
// at that deadline as a failed read, and the child that held gcloud's output is gone with it, not
// left running. The test fires the deadline once it has seen the child running (CallEnds); the
// timer's own expiry is the CLI's TestGCPSecretGcloudSettingsReadIsBestEffort's.
func TestGcloudSettingsReadAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (gcptest.GcloudStubIn's t.Setenv); it sets a package variable the package's other tests read (gcp.GcloudSettingsDeadline); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (gcptest.GcloudChild.CallEnds).
	restore := gcp.GcloudSettingsDeadline
	gcp.GcloudSettingsDeadline = gcloudHeldDeadline
	t.Cleanup(func() { gcp.GcloudSettingsDeadline = restore })
	gcloud, child := gcptest.UseGcloudWhoseChildHoldsItsOutput(t)
	deadline := newFiredDeadline(t)
	err := child.CallEnds(t, "`gcloud config list`", 0, deadline.fire, func() error {
		return gcp.GcloudSettingsForTest(deadline, gcloud, gcptest.Run)
	})
	if err == nil || !strings.Contains(err.Error(), "`gcloud config list` failed") {
		t.Fatalf("readGcloudSettings() = %v; want the read failed", err)
	}
	child.RequireGone(t)
}
