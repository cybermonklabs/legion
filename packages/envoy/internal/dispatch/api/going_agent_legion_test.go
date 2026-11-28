package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// The words the bar's agent tells its model after a read of an issue's Legion state that holds
// none (going/agent's legionNote), by the issue (%[1]s) and its project (%[2]s): the project names
// no Legion daemon, or it names one and Dispatch holds no phase of the issue from it; and that the
// answer says nothing of the sessions that hold a claim on the issue, which another read answers.
const (
	goingLegionNoState = "\n[Legion runs no phase of %[1]s and holds no place in any line for it: %[1]s has no slot and no place in a waiting line. " +
		"This answer holds no Legion state (state is null) because "
	goingLegionNoClaim  = " It says nothing of which sessions hold a claim on %[1]s: dispatch_read_role_holders answers that.]"
	goingLegionNoDaemon = goingLegionNoState + "the project %[2]s names no Legion daemon (configured is false)." + goingLegionNoClaim
	goingLegionNoPhase  = goingLegionNoState + "Dispatch holds no phase of %[1]s from the Legion daemon the project %[2]s names (configured is true)." + goingLegionNoClaim
)

// goingShownMember is a member the bar's agent writes into a tool result's body after a value the
// issue page writes in words (going/agent's shownMembers), such as `,"at_shown":"2 hours ago"`.
var goingShownMember = regexp.MustCompile(`,"[a-z_]+_shown":"[^"]*"`)

// TestTheAgentSaysLegionRunsNothingOfAnIssueItsLegionReadHoldsNoStateOf: a run asked about an
// issue's Legion phase, slot and waiting line calls dispatch_read_issue_legion through call_tool,
// and the tool result its model reads next holds the route's answer and, where that answer holds
// no Legion state, after it words that say Legion runs no phase of the issue and holds no place
// in any line for it, naming the issue, and why: DEMO-1's project names no Legion daemon
// (configured false), and LGN-4's names one that records nothing of it. LGN-1, which that daemon
// records in planning and third in its waiting line, is read as the route answers it, with no
// such words. The saved step reads back as the route's answer, which draw_block reads, and the
// public route answers each of the three as it did: its six members, each holding the seed's
// value, and no words of the bar's. In 1 of 3 census runs the model, given only DEMO-7's
// configured false and null state, told a person Legion was working on DEMO-7 in planning and that
// it was third in the waiting line (run 2649b455, dispatch://INFRA-1588).
func TestTheAgentSaysLegionRunsNothingOfAnIssueItsLegionReadHoldsNoStateOf(t *testing.T) {
	t.Parallel()
	rig := newGoingAgentRig(t, nil)
	loop := newGoingLoop(t, rig)
	seedLegionProjects(t, rig.mux, "LGN")
	nameLegionDaemon(t, rig.mux, "LGN", "http://legion-lgn.internal:13370")
	at := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	for index, title := range []string{"The issue in planning", "The first in the line", "The second in the line", "The issue the daemon does not record"} {
		seedLegionIssue(t, rig.database, "LGN", index+1, title, string(rune('a'+index)), nil, "")
	}
	for _, observed := range []struct{ issue, field, value string }{
		{"LGN-1", legion.FieldPhase, `"planning"`},
		{"LGN-1", legion.ClaimFieldPrefix + "planner", `"working"`},
		{"LGN-2", legion.FieldPhase, `"admitted"`},
		{"LGN-3", legion.FieldPhase, `"admitted"`},
		{legion.AdmissionKey, legion.FieldAdmission, `{"cap": 1, "active": [], "waiting": ["LGN-2", "LGN-3", "LGN-1"], "blocked": []}`},
	} {
		seedLegionObservation(t, rig.database, "LGN", observed.issue, observed.field, observed.value, at)
	}
	seedLegionRead(t, rig.database, "LGN", at.Add(time.Hour), "")
	lgnRead := &wireLegionRead{At: at.Add(time.Hour), OK: true}

	for _, row := range []struct {
		issue string
		body  legionIssueBody
		words string
	}{
		{"DEMO-1", legionIssueBody{configured: false}, fmt.Sprintf(goingLegionNoDaemon, "DEMO-1", "DEMO")},
		{"LGN-4", legionIssueBody{configured: true, read: lgnRead}, fmt.Sprintf(goingLegionNoPhase, "LGN-4", "LGN")},
		{"LGN-1", legionIssueBody{configured: true, read: lgnRead, state: &legionIssueWant{
			phase: "planning", since: at, roles: []wireLegionRole{{"planner", "working"}},
			line: &wireLegionWaitingLine{Place: 3, Length: 3}, blockedBy: []string{},
		}}, ""},
	} {
		t.Run(row.issue, func(t *testing.T) {
			words := "which Legion phase, slot and place in the waiting line does " + row.issue + " hold"
			loop.script.set(words, func(n int) agenttest.Reply {
				if n == 1 {
					return toolCall("call_tool", map[string]any{"tool": "dispatch_read_issue_legion", "arguments": map[string]any{"issue": row.issue}})
				}
				return answered("Read.")
			})
			run := rig.begin(t, goingLoopRun{login: "alice", words: words})
			if record := rig.ended(t, run, time.Minute); record.state != "done" {
				t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
			}

			route := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/issues/"+row.issue+"/legion", nil, "alice")
			if route.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/issues/%s/legion: status=%d body=%s, want 200", row.issue, route.Code, route.Body.String())
			}
			answer := decodeLegionIssue(t, route.Body.Bytes())
			requireLegionNames(t, row.issue, answer)
			requireLegionBody(t, row.issue, answer, row.body)

			asked := loop.fake.Asking(words)
			if len(asked) != 2 || len(asked[1].ToolResults()) != 1 {
				t.Fatalf("the run made %d model calls, want 2, the second told the one tool result", len(asked))
			}
			result := asked[1].ToolResults()[0]
			_, told, headed := strings.Cut(result, ": status 200\n")
			body, worded := strings.CutSuffix(told, row.words)
			if !headed || !worded || goingShownMember.ReplaceAllString(body, "") != route.Body.String() {
				t.Errorf("the model read %s's Legion read as %q, want the route's answer %q followed by %q", row.issue, result, route.Body.String(), row.words)
			}

			var step []byte
			if err := rig.database.Pool.QueryRow(context.Background(), `select body from going_run_steps where run_id = $1::uuid and kind = 'tool'`, run).Scan(&step); err != nil {
				t.Fatalf("read the saved Legion read: %v", err)
			}
			saved, err := agent.ReadSavedResult(step)
			if err != nil || saved.Tool != "dispatch_read_issue_legion" || saved.Status != http.StatusOK || string(saved.Body) != route.Body.String() {
				t.Errorf("the saved Legion read of %s reads as %s, status %d, with %q (%v), want dispatch_read_issue_legion, 200, with the route's answer %q",
					row.issue, saved.Tool, saved.Status, saved.Body, err, route.Body.String())
			}
		})
	}
}
