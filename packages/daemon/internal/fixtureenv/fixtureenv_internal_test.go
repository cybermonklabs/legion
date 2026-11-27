package fixtureenv

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A wrapper Guard reads before m.Run can block, so runningBinaryContext ends the read on the
// context it was given and names the wrapper. The row gives it a 10 ms deadline and a wrapper that
// blocks far past any test timeout (sleep 100000), so only the context can end the read; a probe
// that stopped honouring its context would hold the row until testwait.Receive gives up, which
// fails it naming the call. Guard's own runningBinary gives each real git and jj 30 seconds.
func TestRunningBinaryNamesAWrapperThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runningBinaryContext(ctx, t.TempDir(), "/usr/bin:/bin", sh, []string{"-c", "exec sleep 100000"}, nil)
		done <- err
	}()
	err = testwait.Receive(t, "runningBinaryContext to return once its 10 ms deadline ended, with the wrapper still blocking", done)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("running a blocking wrapper = %v, want its deadline", err)
	}
	if got := err.Error(); !strings.Contains(got, sh) || !strings.Contains(got, "did not answer within") {
		t.Errorf("running a blocking wrapper = %q, want it to name %s and its bound", got, sh)
	}
}
