package workflow

import (
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/pods"
)

// podWakeReaderPrompts are the two prompt files that the readers of a pod-wake task load: every
// phase worker loads go/worker-common.md, and the root and the child architect
// go/architect-common.md (prompts.Compose). They are read from disk, as texts_steps_test.go reads
// them.
var podWakeReaderPrompts = []string{"../prompts/go/worker-common.md", "../prompts/go/architect-common.md"}

// podWakePromptLine is the line of path that opens with opening.
func podWakePromptLine(t *testing.T, path, opening string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, opening) {
			return line
		}
	}
	t.Fatalf("%s has no line opening %q", path, opening)
	return ""
}

// podWakeSegment is the text of line from the first from up to the first to after it.
func podWakeSegment(line, from, to string) string {
	_, rest, ok := strings.Cut(line, from)
	if !ok {
		return ""
	}
	if before, _, found := strings.Cut(rest, to); found {
		return from + before
	}
	return from + rest
}

// Main's WakeTask (workflow/wake.go at d46ec4e0) told a role woken by a message sent to its
// session: "A message that Envoy or Dispatch stored for your session reaches you in this session.
// A direct Envoy message that Envoy refused reaches you when its sender sends it again."
// dispatch://INFRA-3360's R3 (approved v16) has the woken role's task say that "a message stored
// for the role's session reaches it when it runs …; a message that Envoy did not store does not
// reach it, and its sender must send it again", and INFRA-3686's Design keeps "the facts that …
// dispatch://INFRA-3360's R3 … put in `WakeTask`", while its R8 says "a fact or a step that it has
// the text give stays as a field". The role message wake keeps INFRA-2846 R7's facts as fields
// (sender_sends_again and the rest); the agent message wake must keep INFRA-3360 R3's.
func TestAnAgentMessageWakeGivesTheStoredAndRefusedFactsAsFields(t *testing.T) {
	for _, stop := range []WakeStop{StoppedByRelease, StoppedByPause} {
		read := readTaskOf(t, WakeTask("LEGION-208", pods.WakeAgentMessage, "session-1", stop))
		var facts []string
		for key := range read.fields {
			switch key {
			case "kind", "issue", "wake", "session", "stopped_by":
			default:
				facts = append(facts, key)
			}
		}
		if len(facts) == 0 {
			t.Errorf("stopped_by %s: the agent message wake's record holds %v and no field for INFRA-3360 R3's facts "+
				"(a message Envoy stored reaches the session; a direct send Envoy refused does not, and its sender sends it again)",
				stop.word(), read.fields)
		}
	}
}

// Main's WakeTask (workflow/wake.go at d46ec4e0) said a message kept for the role "reaches this
// session as an Envoy card that shows kept_at, possibly before this task, unless its hour ended
// before you came back". dispatch://INFRA-2846's R7 (approved v7, done, held by main) has the role
// message sentence say the card reaches the woken session "unless its hour ended before the session
// came back", and INFRA-3686's R8 says "a fact or a step that it has the text give stays as a
// field". kept_for is the hour, not that a kept copy whose hour ended does not reach the session,
// so the record or each reader's prompt must say that condition.
func TestARoleMessageWakeSaysAKeptCopyWhoseHourEndedDoesNotReachTheSession(t *testing.T) {
	read := readTaskOf(t, WakeTask("LEGION-208", pods.WakeRoleMessage, "legion-legion-legion-208-implementer", StoppedByPause))
	conditions := []string{"unless", "ended before", "ends before", "expired", "does not reach"}
	inRecord := false
	for key, value := range read.fields {
		for _, word := range conditions {
			if strings.Contains(key, strings.ReplaceAll(word, " ", "_")) || strings.Contains(value, word) {
				inRecord = true
			}
		}
	}
	for _, path := range podWakeReaderPrompts {
		line := podWakePromptLine(t, path, "A task of the kind `pod-wake`")
		segment := podWakeSegment(line, "`wake: role_message`", "`wake: agent_message`")
		said := false
		for _, word := range conditions {
			if strings.Contains(segment, word) {
				said = true
			}
		}
		if !said && !inRecord {
			t.Errorf("%s: the role_message wake reads %q, and the record %v: neither says that a kept message whose hour ended before the session came back does not reach it",
				path, segment, read.fields)
		}
	}
}
