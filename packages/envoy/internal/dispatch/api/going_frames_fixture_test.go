package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/going"
)

// goingFixtureBlock is a block of kind as the server fills it: the spec the answer wrote, held to
// its kind's schema (going.ParseBlock), filled from a read holding tables (Block.Fill).
func goingFixtureBlock(t *testing.T, kind, title, spec string, tables map[string][]map[string]any) going.BlockData {
	t.Helper()
	block, err := going.ParseBlock(going.FinalBlock{Kind: kind, Title: title, Spec: spec})
	if err != nil {
		t.Fatalf("parse the %s block: %v", kind, err)
	}
	data, err := block.Fill(going.Read{Tables: tables})
	if err != nil {
		t.Fatalf("fill the %s block: %v", kind, err)
	}
	return data
}

// TestTheGoingFrameFixturesAreWhatTheServerWrites holds packages/contracts/fixtures/going, which
// the contracts' Bun test (src/going.test.ts) holds to the hand-written frames of
// packages/contracts/src/going.ts key for key, to the data of the frames this server writes on a
// run's stream: a decision with its two best actions, a proposal with an invocation complete, one
// missing an enumeration's value and one missing an item, each kind of block as a block's spec
// and its read fill it, and the roster frame. A field the Go types gain, lose or rename changes
// a fixture here, and the Bun test then fails until going.ts says the same.
// `go test ./internal/dispatch/api -run TestTheGoingFrameFixturesAreWhatTheServerWrites -update`
// rewrites them.
func TestTheGoingFrameFixturesAreWhatTheServerWrites(t *testing.T) {
	confidence, chosen, runnerUp := 0.91, 0.84, 0.12
	frames := map[string]any{
		"decision.json": goingDecision{
			Actions: []goingDecidedAction{
				{Action: "dispatch_issue_update", Confidence: &confidence, Probability: &chosen},
				{Action: "dispatch_comment", Probability: &runnerUp},
			},
			Act:     true,
			Context: going.ContextPage,
			Display: goingDecidedDisplay{Choice: going.DisplayChart, Confidence: 0.77, Chart: true},
		},
		"proposal.json": goingProposal{Offers: []goingOffer{
			{Action: "dispatch_issue_update", Offered: true, Invocations: []goingInvocation{
				{Arguments: map[string]any{"issue": "DEMO-12", "status": "done"}, Automatic: true, TargetArgument: "issue"},
			}},
			{Action: "dispatch_follow", Offered: true, Invocations: []goingInvocation{{
				Arguments:      map[string]any{"ask": "0199e2a0-2222-7000-8000-000000000002"},
				Missing:        &goingMissingArgument{Field: "action", Kind: goingPromptEnum, Choices: []string{"follow", "unfollow"}},
				TargetArgument: "ask",
			}}},
			{Action: "dispatch_claim", Offered: true, Invocations: []goingInvocation{{
				Arguments:      map[string]any{},
				Missing:        &goingMissingArgument{Field: "issue", Kind: goingPromptItem, Item: "issue"},
				TargetArgument: "issue",
			}}},
			{Action: "dispatch_comment", Offered: false, Invocations: []goingInvocation{}},
		}},
		"block_chart.json": goingFixtureBlock(t, going.BlockChart, "Events by week",
			`{"source":"read_project_activity","params":{"project":"DEMO"},"table":"weeks","x":"week","y":"events","mark":"line"}`,
			map[string][]map[string]any{"weeks": {{"week": "2026-W44", "events": 3}, {"week": "2026-W45", "events": 5}}}),
		"block_table.json": goingFixtureBlock(t, going.BlockTable, "Open issues",
			`{"source":"read_progress","params":{"project":"DEMO"},"table":"issues","columns":["key","status"]}`,
			map[string][]map[string]any{going.TableIssues: {{going.FieldKey: "DEMO-12", going.FieldTitle: "Twelve", going.FieldStatus: "todo"}}}),
		"block_metric.json": goingFixtureBlock(t, going.BlockMetric, "Deploys",
			`{"source":"read_delivery","params":{"view":"measures"},"table":"totals","field":"successful_deploys","label":"Deploys","compare":"deploys_with_prs","compare_label":"Deploys with pull requests"}`,
			map[string][]map[string]any{"totals": {{"successful_deploys": 12, "deploys_with_prs": 9}}}),
		"block_progress.json": goingFixtureBlock(t, going.BlockProgress, "DEMO-12's progress",
			`{"source":"read_progress","params":{"issue":"DEMO-12"}}`,
			map[string][]map[string]any{going.TableProgress: {{going.FieldLabel: "tasks", going.FieldDone: 2, going.FieldTotal: 5}}}),
		"block_issue_list.json": goingFixtureBlock(t, going.BlockIssueList, "DEMO's open issues",
			`{"source":"read_progress","params":{"project":"DEMO"}}`,
			map[string][]map[string]any{going.TableIssues: {{going.FieldKey: "DEMO-12", going.FieldTitle: "Twelve", going.FieldStatus: "todo", "priority": 1}}}),
		"block_agent_status.json": goingFixtureBlock(t, going.BlockAgentStatus, "Agents",
			`{"source":"read_roster","params":{}}`,
			map[string][]map[string]any{going.TableSessions: {{
				going.FieldSessionID: "0199e2a0-1111-7000-8000-000000000001", going.FieldTitle: "Planner for DEMO-12",
				going.FieldState: "live", "roles": []string{"planner"}, going.FieldOpenAsks: 1, going.FieldLastActivity: "2026-11-02T10:00:00Z",
			}}}),
		"block_timeline.json": goingFixtureBlock(t, going.BlockTimeline, "Merges and deploys",
			`{"source":"read_delivery","params":{"view":"timeline"}}`,
			map[string][]map[string]any{going.TableEvents: {
				{going.FieldAt: "2026-11-02T10:00:00Z", going.FieldLane: "merge", going.FieldLabel: "acme/widgets#1 A change"},
				{going.FieldAt: "2026-11-02T11:00:00Z", going.FieldLane: "deploy", going.FieldLabel: "run 8: success"},
			}}),
		"roster.json": goingRosterFrame{AgeMS: 1200, Source: goingRosterLive},
	}
	for name, frame := range frames {
		got, err := json.MarshalIndent(frame, "", "  ")
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		got = append(got, '\n')
		path := filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "going", name)
		if *updateFixtures {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (run with -update to write it): %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the frame data the server writes (run with -update):\n%s", path, got)
		}
	}
}
