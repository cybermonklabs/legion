package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The daemon's feeder of a worker's events logs a decision's error at the level the machine's own
// goroutines log it at (decisionFailed, supervise.DecisionErrorLevel): a decision that failed on
// the project release's hold of every start alone (errProjectReleased, a routine wait) writes its
// line at Debug, so a release that holds a claim for its whole bound writes no Error line for it,
// while the same wait joined to a fault, and every other error, is still written at Error. A stop
// of the daemon is no fault, and is written at Info. The negative control is decisionFailed
// logging at Error whatever the error is: the first case then finds an Error line.
func TestADecisionThatFailedOnTheReleasesWaitAloneIsLoggedBelowError(t *testing.T) {
	t.Parallel()
	const token = claim.Token("legion-legion-legion-209-implementer")
	const failed = "supervise: an event failed"
	stopped, stop := context.WithCancel(context.Background())
	stop()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		err    error
		level  string
		failed bool
	}{
		{"the release's wait alone", context.Background(), fmt.Errorf("launch %s: %w", token, errProjectReleased), "DEBUG", true},
		{"the release's wait joined to a fault", context.Background(),
			errors.Join(errProjectReleased, errors.New("the store did not answer")), "ERROR", true},
		{"a fault alone", context.Background(), errors.New("the store did not answer"), "ERROR", true},
		{"the daemon's stop", stopped, fmt.Errorf("launch %s: %w", token, errProjectReleased), "INFO", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logs, log := debugLog()
			s := &supervisor{ctx: tc.ctx, log: log}
			if got := s.decisionFailed(token, "supervise.Timer", tc.err, failed); got != tc.failed {
				t.Errorf("decisionFailed = %t, want %t", got, tc.failed)
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], "level="+tc.level+" ") {
				t.Fatalf("decisionFailed wrote %q, want one line at %s", lines, tc.level)
			}
		})
	}
}
