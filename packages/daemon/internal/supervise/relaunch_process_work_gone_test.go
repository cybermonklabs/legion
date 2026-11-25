package supervise

import (
	"slices"
	"strings"
	"testing"
)

// relaunchNoteKeys is each key the rendered relaunch note of ended states, in front of a task
// where task is true.
func relaunchNoteKeys(t *testing.T, ended EndedWork, task bool) (string, []string) {
	t.Helper()
	text := partText(RelaunchNote("LEGION-7", ended, task))
	var keys []string
	for _, line := range strings.Split(text, "\n") {
		key, _, found := strings.Cut(line, ":")
		if !found || strings.HasPrefix(line, " ") {
			continue
		}
		key, _, _ = strings.Cut(key, "[")
		keys = append(keys, key)
	}
	return text, keys
}

// Main's relaunch sentence (supervise/relaunch.go:relaunchSentence at d46ec4e0) says that the
// work a relaunch ended is lost beyond the turn it cut off wherever no report shows that work:
// for a process in no turn, "The daemon has no report of the background work of your last turn.
// So every subagent and background job that you started in that process is gone.", and for a
// turn cut off whose record names no background work, "Every other subagent and background job
// that you started in that process is gone too." dispatch://INFRA-714's R6 (approved, done):
// "the restarted worker's first prompt names the cause and says its tool calls and subagents were
// lost", and dispatch://INFRA-3276's R3: "the agent's next turn reads a sentence naming it" (the
// work of the agent that was running). dispatch://INFRA-3686's R8: "a fact or a step that it has
// the text give stays as a field". Round 13 made the loss of a cut turn's own work a field
// (turn_background_work: gone); the loss of every other subagent and background job of the
// process is still only the role prompts' reading of absent fields ("Without `reported`, ... every
// subagent and background job that you started in that process is gone"). So the note states it
// as a field where main said it, and not where main did not: a process whose report speaks of its
// last turn, and a turn cut off whose record names what stopped.
func TestARelaunchNoteOfAnUnreportedProcessSaysAsAFieldThatAllItsBackgroundWorkIsGone(t *testing.T) {
	known := []string{"kind", "issue", "cause", "in_turn", "task_turn", "in_flight_tool_calls", "turn_background_work",
		"stopped", "background", "reported", "task"}
	said := []struct {
		name  string
		ended EndedWork
		main  string
	}{
		{"a process in no turn that left no report", EndedWork{Cause: "the process died"},
			"So every subagent and background job that you started in that process is gone."},
		{"a turn cut off whose record names no background work", EndedWork{Cause: "the process died", Turn: true},
			"Every other subagent and background job that you started in that process is gone too."},
	}
	var added []string
	for _, c := range said {
		for _, task := range []bool{false, true} {
			text, keys := relaunchNoteKeys(t, c.ended, task)
			var extra []string
			for _, key := range keys {
				if !slices.Contains(known, key) {
					extra = append(extra, key)
				}
			}
			if len(extra) == 0 {
				t.Errorf("%s (task %v): main said %q, and the note states no field of it:\n%s", c.name, task, c.main, text)
			}
			added = append(added, extra...)
		}
	}
	unsaid := []struct {
		name  string
		ended EndedWork
	}{
		{"a process in no turn whose report speaks of its last turn", EndedWork{Cause: "the process died", Reported: true}},
		{"a turn cut off whose record names what stopped", EndedWork{Cause: "the process died", Turn: true, Kinds: []string{"subagent"}, Background: true}},
	}
	for _, c := range unsaid {
		text, keys := relaunchNoteKeys(t, c.ended, true)
		for _, key := range keys {
			if slices.Contains(added, key) {
				t.Errorf("%s: main said no more of its work than the record names, and the note states %s:\n%s", c.name, key, text)
			}
		}
	}
}
