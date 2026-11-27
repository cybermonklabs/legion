package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
)

// The claim route's refusal of a takeover claim whose record did not store logs one WARN naming
// the role, the previous holder the claim read and the claimant, and holds back the role's next
// refusals for a minute, so a full stream that refuses every claim's backoff logs a line a minute
// for each role. The control is another role, whose first refusal logs at once.
func TestARefusedTakeoverClaimIsLoggedOnceAMinuteForEachRole(t *testing.T) {
	// Not parallel: it writes entries of the package variable takeoverUnrecordedWarnings
	// (warnTakeoverUnrecorded, roles role-warned and role-other) and asserts on the warnings
	// they let through.
	var logs bytes.Buffer
	logger := logging.NewWithWriter("test", &logs)
	refused := takeoverUnrecordedError{previous: "ses_previous", err: errors.New("nats: maximum bytes exceeded")}
	warnTakeoverUnrecorded(logger, "role-warned", "ses_claimant", refused)
	warnTakeoverUnrecorded(logger, "role-warned", "ses_claimant", refused)
	warnTakeoverUnrecorded(logger, "role-other", "ses_claimant", refused)
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logged %d lines for two refusals of one role and one of another, want 2:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{`"role":"role-warned"`, `"previous_session_id":"ses_previous"`, `"session_id":"ses_claimant"`, `"level":"WARN"`, "maximum bytes exceeded"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the first line %s lacks %s", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], `"role":"role-other"`) {
		t.Fatalf("control: the other role's refusal was not logged at once: %s", lines[1])
	}
}
