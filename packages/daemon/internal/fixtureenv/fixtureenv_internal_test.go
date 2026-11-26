package fixtureenv

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A wrapper Guard reads before m.Run can block, so runningBinaryContext ends the read on the
// context it was given and names the wrapper. The row gives it a short deadline; Guard's own
// runningBinary gives each real git and jj 30 seconds.
func TestRunningBinaryNamesAWrapperThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err = runningBinaryContext(ctx, t.TempDir(), "/usr/bin:/bin", sh, []string{"-c", "exec sleep 10"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("running a blocking wrapper = %v, want its deadline", err)
	}
	if got := err.Error(); !strings.Contains(got, sh) || !strings.Contains(got, "did not answer within") {
		t.Errorf("running a blocking wrapper = %q, want it to name %s and its bound", got, sh)
	}
}
