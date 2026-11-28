package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agentwire"
)

// goingLinePage is the page the line tests' runs were started on.
const goingLinePage = `{"path":"/projects/DEMO/issues/DEMO-1","project":"DEMO","issue":"DEMO-1"}`

// queuedFromLine records a queued run of login's asked in words on goingLinePage, started from
// line ("" for none), granted by the header identity, and answers its id.
func (rig goingAgentRig) queuedFromLine(t *testing.T, login, words, line string) string {
	t.Helper()
	id := uuid.NewString()
	rig.exec(t, `
		insert into going_runs (id, author_login, words, context, conversation_id, level, mode, line, state, shape, created_at)
		values ($1::uuid, $2, $3, $4::jsonb, $1::uuid, 'standard', 'ask', nullif($5, ''), 'queued', $6, clock_timestamp())
	`, id, login, words, goingLinePage, line, goingRunShape)
	rig.exec(t, `insert into going_run_grants (run_id, kind, login) values ($1::uuid, 'header', $2)`, id, login)
	return id
}

// lineOf makes the line call of claimed and answers the line it answered.
func (rig goingAgentRig) lineOf(t *testing.T, claimed *agentwire.ClaimedRun) string {
	t.Helper()
	answer, err := rig.call(agentwire.CallRequest{Run: claimed.ID, LeaseToken: claimed.LeaseToken, Kind: agentwire.KindLine})
	if err != nil {
		t.Fatalf("the line call was refused: %v", err)
	}
	var read agentwire.LineAnswer
	if answer.Status != http.StatusOK || json.Unmarshal(answer.Body, &read) != nil {
		t.Fatalf("the line call answered %d %s, want 200 and a line", answer.Status, answer.Body)
	}
	return read.Line
}

// TestALineCallAnswersTheLineJevReadsTheRunsWordsAs: a run started from the line Ask records only
// the line on the screen at the key, which may be Ask because Jev had not answered yet, so the
// agent asks Jev again. The claim hands the agent the run's line and whether the server names a
// decision model; the line call puts the run's words and the page it was started on to Jev, as
// the bar's own decision does, and answers the line Jev's choice gives: Ask for none, File for an
// issue's create, Do for any other action. A decision call that draws no usable answer answers no
// line, and is no failure of the call.
func TestALineCallAnswersTheLineJevReadsTheRunsWordsAs(t *testing.T) {
	t.Parallel()
	standIn := newDecisionStandIn(t)
	rig := newGoingAgentRig(t, func(agent *AgentListener) { agent.s.deps.Decider = standIn.decider(t, staticToken) })
	const words = "who is DEMO-1 assigned to"
	run := rig.queuedFromLine(t, "alice", words, goingLineAsk)
	claimed := rig.claim(t, "i-line")
	if claimed == nil || claimed.ID != run || claimed.Line != agentwire.LineAsk || !claimed.Decider {
		t.Fatalf("the claim took %+v, want run %s started from Ask at a server that names a decision model", claimed, run)
	}

	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	do := ""
	for _, action := range catalogue.Actions() {
		if action.Action != goingIssueCreateTool {
			do = action.Action
			break
		}
	}
	for _, c := range []struct{ action, line string }{
		{going.ActionNone, agentwire.LineAsk},
		{do, agentwire.LineDo},
		{goingIssueCreateTool, agentwire.LineFile},
	} {
		standIn.answer(t, decidedAnswer{action: c.action, confidence: 0.9, about: going.ContextPage, display: going.DisplayProse, displayConfidence: 0.9})
		if line := rig.lineOf(t, claimed); line != c.line {
			t.Errorf("Jev chose %s, and the line call answered %q, want %q", c.action, line, c.line)
		}
	}
	standIn.mu.Lock()
	state := standIn.bodies[len(standIn.bodies)-1]["state"]
	standIn.mu.Unlock()
	var sent, want map[string]any
	if json.Unmarshal(state, &sent) != nil ||
		json.Unmarshal(going.DecisionState(words, going.PageContext{Path: "/projects/DEMO/issues/DEMO-1", Project: "DEMO", Issue: "DEMO-1"}), &want) != nil ||
		!reflect.DeepEqual(sent, want) {
		t.Errorf("Jev was asked about %s, want the run's words and its page %s", state, going.DecisionState(words, going.PageContext{Path: "/projects/DEMO/issues/DEMO-1", Project: "DEMO", Issue: "DEMO-1"}))
	}

	standIn.mu.Lock()
	standIn.reply = "not a decision"
	standIn.mu.Unlock()
	if line := rig.lineOf(t, claimed); line != "" {
		t.Errorf("a decision call that drew no answer made the line call answer %q, want no line", line)
	}
}

// TestARunAtAServerWithNoDecisionModelIsClaimedSo: a server that names no decision model says so
// in the claim, so its agent asks no line of it, and a line call made all the same answers none.
func TestARunAtAServerWithNoDecisionModelIsClaimedSo(t *testing.T) {
	t.Parallel()
	rig := newGoingAgentRig(t, nil)
	run := rig.queuedFromLine(t, "alice", "who is DEMO-1 assigned to", "")
	claimed := rig.claim(t, "i-no-decider")
	if claimed == nil || claimed.ID != run || claimed.Line != "" || claimed.Decider {
		t.Fatalf("the claim took %+v, want run %s of no line at a server that names no decision model", claimed, run)
	}
	if line := rig.lineOf(t, claimed); line != "" {
		t.Errorf("the line call answered %q at a server with no decision model, want no line", line)
	}
}
