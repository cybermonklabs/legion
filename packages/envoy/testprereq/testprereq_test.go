package testprereq

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/fixtureenv"
)

// recorder is a testing.TB that keeps what Require asked of it instead of ending the test, so the
// test reads the failure or the skip Require would have given a real one.
type recorder struct {
	testing.TB
	failed, skipped string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) { r.failed = fmt.Sprintf(format, args...) }

func (r *recorder) Skipf(format string, args ...any) { r.skipped = fmt.Sprintf(format, args...) }

// A missing prerequisite fails the test on GitHub Actions, naming the prerequisite and the job
// that must give it, and skips it, naming the prerequisite, where GITHUB_ACTIONS is unset. A
// prerequisite that is there does neither, on GitHub Actions or off it.
func TestRequireFailsOnGitHubActionsAndSkipsElsewhere(t *testing.T) {
	const reason = "DISPATCH_TEST_DATABASE_URL is unset"
	t.Run("on GitHub Actions", func(t *testing.T) {
		t.Setenv("GITHUB_ACTIONS", "true")
		missing := &recorder{TB: t}
		Require(missing, false, "envoy-go", "%s", reason)
		if !strings.Contains(missing.failed, reason) || !strings.Contains(missing.failed, "ci.yaml's envoy-go job") || missing.skipped != "" {
			t.Errorf("a missing prerequisite under GITHUB_ACTIONS=true failed with %q and skipped with %q, want a failure naming %q and the envoy-go job, and no skip", missing.failed, missing.skipped, reason)
		}
		present := &recorder{TB: t}
		Require(present, true, "envoy-go", "%s", reason)
		if present.failed != "" || present.skipped != "" {
			t.Errorf("a prerequisite that is there failed with %q and skipped with %q, want neither", present.failed, present.skipped)
		}
	})
	t.Run("off GitHub Actions", func(t *testing.T) {
		fixtureenv.Unset(t, "GITHUB_ACTIONS")
		missing := &recorder{TB: t}
		Require(missing, false, "envoy-go", "%s", reason)
		if missing.skipped != reason || missing.failed != "" {
			t.Errorf("a missing prerequisite with GITHUB_ACTIONS unset skipped with %q and failed with %q, want the skip %q and no failure", missing.skipped, missing.failed, reason)
		}
		present := &recorder{TB: t}
		Require(present, true, "envoy-go", "%s", reason)
		if present.failed != "" || present.skipped != "" {
			t.Errorf("a prerequisite that is there failed with %q and skipped with %q, want neither", present.failed, present.skipped)
		}
	})
}
