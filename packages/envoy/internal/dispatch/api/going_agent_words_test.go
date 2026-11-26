package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/going/chips"
)

// TestAWordsCallCountsTheWordsTheBarDraws: an answer of the command bar's agent has fewer than 120
// words as the bar draws them, and at 8a3e562b 6 of 11 answers drew 120 or more once their chips'
// words were counted. The agent's words call asks the server for the count of a reply as the run's
// answer: the text the server would store, each link to an issue drawn as the issue's chip, read
// as the run's person may see it - its key, its status and its title - and not the link's own
// words. The call records nothing, and a run whose lease does not hold is refused, as every call
// of agent/v1 is.
func TestAWordsCallCountsTheWordsTheBarDraws(t *testing.T) {
	t.Parallel()
	rig := newGoingAgentRig(t, nil)
	issue := rig.issue(t, "Rotate the signing keys")
	run, token := rig.working(t, "alice", "what does "+issue+" wait on")
	// The link's words are the issue's title, which its chip draws, so the server stores the link
	// as it came and the bar draws the chip in place of its four words.
	text := "[Rotate the signing keys](dispatch://" + issue + ") waits on two reviews."
	status := chips.StatusText(rig.issueRead(t, issue).Status)
	want := len(strings.Fields(issue+" "+status+" Rotate the signing keys")) + len(strings.Fields("waits on two reviews."))

	answer, err := rig.call(agentwire.CallRequest{Run: run, LeaseToken: token, Kind: agentwire.KindWords, Text: text})
	if err != nil {
		t.Fatalf("the words call was refused: %v", err)
	}
	var counted agentwire.WordsAnswer
	if answer.Status != http.StatusOK || json.Unmarshal(answer.Body, &counted) != nil {
		t.Fatalf("the words call answered %d %s, want 200 and a count", answer.Status, answer.Body)
	}
	if counted.Words != want {
		t.Errorf("the words call counted %d words of %q, want %d: the chip's key, its status %q and its title, and the four words after it", counted.Words, text, want, status)
	}
	if recorded := rig.count(t, `select count(*) from going_run_steps where run_id = $1::uuid`, run) +
		rig.count(t, `select count(*) from going_run_events where run_id = $1::uuid`, run) +
		rig.count(t, `select count(*) from going_landings where run_id = $1::uuid`, run); recorded != 0 {
		t.Errorf("the words call recorded %d steps, events and landings, want none", recorded)
	}

	_, err = rig.call(agentwire.CallRequest{Run: run, LeaseToken: uuid.NewString(), Kind: agentwire.KindWords, Text: text})
	requireAgentCode(t, err, agentwire.CodeStaleLease, "a words call under a stale token")
}
