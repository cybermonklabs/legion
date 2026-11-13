package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The block tests (c5) drive the real answer path - the route, the run, going's answerer over
// agentloop - against goingtest's model, which calls the read a block needs and then writes the
// block's spec, never a number. Every number the block frame carries is asserted equal to what
// the route that read serves answers for the same seeded fixture.

// blockRig is a server whose answering model is goingtest's, answering with replies.
type blockRig struct {
	goingRoutesRig
	model  *goingtest.Model
	served *server
}

func newBlockRig(t *testing.T, options testServerOptions, replies ...goingtest.Reply) blockRig {
	t.Helper()
	answerer, model := newGoingModelAnswerer(t, replies...)
	options.answerer = answerer
	handler, database, deps, served := newRegisteredTestServer(t, options)
	return blockRig{goingRoutesRig: goingRoutesRig{handler: handler, server: directServer(deps), database: database}, model: model, served: served}
}

// seed runs each statement against the rig's database.
func (rig blockRig) seed(t *testing.T, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := rig.database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed: %v\n%s", err, statement)
		}
	}
}

// readCall is a turn that calls one read tool with arguments.
func readCall(name, arguments string) goingtest.Reply {
	return goingtest.Reply{ToolCalls: []goingtest.ToolCall{{ID: "call_" + name, Name: name, Arguments: arguments}}}
}

// blockAnswer is a turn that answers with a short text and a block of kind with spec.
func blockAnswer(kind, spec string) goingtest.Reply {
	return goingtest.Reply{Final: &goingtest.Final{
		Answer: "Here is what Dispatch holds.", Block: goingtest.Block{Kind: kind, Title: "What Dispatch holds", Spec: spec}, Arguments: "{}",
	}}
}

// blockFrame is a block frame's data as a reader decodes it.
type blockFrame struct {
	Kind  string           `json:"kind"`
	Title string           `json:"title"`
	Spec  map[string]any   `json:"spec"`
	Rows  []map[string]any `json:"rows"`
}

// askForBlock asks words of project as alice and answers the run's frames and its block frame,
// nil where it stored none. The run must have ended with an answer.
func (rig blockRig) askForBlock(t *testing.T, words, project string) ([]goingEvent, *blockFrame) {
	t.Helper()
	frames := storedGoingFrames(t, rig.goingRoutesRig, askRun(t, rig.goingRoutesRig, "alice", map[string]any{"words": words, "project": project}))
	names := frameNames(frames)
	if names[len(names)-1] != contracts.GoingEventEnd {
		t.Fatalf("the run stored %v, want it to end with an answer", names)
	}
	var block blockFrame
	if !frameData(t, frames, contracts.GoingEventBlock, &block) {
		return frames, nil
	}
	// A block rides after every text frame and before the ending (R2).
	at := slices.Index(names, contracts.GoingEventBlock)
	if slices.Contains(names[at+1:], contracts.GoingEventText) {
		t.Errorf("the run stored %v, want the block after every text frame", names)
	}
	return frames, &block
}

// number is a decoded JSON number as an int, failing the test where it is none.
func number(t *testing.T, value any) int {
	t.Helper()
	parsed, ok := value.(float64)
	if !ok {
		t.Fatalf("%v (%T) is no number", value, value)
	}
	return int(parsed)
}

// seedProgressFixture seeds DEMO-1 with a counted task list and two children, one done.
func seedProgressFixture(t *testing.T, rig blockRig) {
	t.Helper()
	seedActivityFixture(t, rig.database)
	rig.seed(t,
		`update issues set tasks_done = 2, tasks_total = 5, tasks_version = 1 where key = 'DEMO-1'`,
		`insert into issues (key, project_key, number, title, status, parent_key, created_by, rank) values
			('DEMO-3', 'DEMO', 3, 'First child', 'done', 'DEMO-1', '{"kind":"system","id":"test"}', 'W'),
			('DEMO-4', 'DEMO', 4, 'Second child', 'todo', 'DEMO-1', '{"kind":"system","id":"test"}', 'X')`,
	)
}

// Each of the seven kinds is filled from its read, and every number it carries is the value the
// route serving that read answers for the same fixture: the activity chart from GET
// .../activity, the progress from GET /api/v1/issues, the component table from GET
// .../architecture, the issue list from GET /api/v1/issues, the agent status from GET
// /api/v1/agents, and the metric and the timeline from the delivery measures and timeline.
func TestGoingBlockEachKindsNumbersAreItsReads(t *testing.T) {
	t.Run("chart", func(t *testing.T) {
		spec := `{"source":"read_project_activity","params":{"project":"DEMO","weeks":4},"table":"weeks","x":"week","y":"events"}`
		rig := newBlockRig(t, testServerOptions{}, readCall(going.ReadProjectActivity, `{"project":"DEMO","weeks":4}`), blockAnswer(going.BlockChart, spec))
		seedActivityFixture(t, rig.database)
		_, block := rig.askForBlock(t, "chart the weekly activity of DEMO", "DEMO")
		served := decodeBody[projectActivity](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/DEMO/activity?weeks=4", nil, "alice"))
		if block == nil || block.Kind != going.BlockChart || len(block.Rows) != len(served.Weeks) {
			t.Fatalf("block = %+v, want a chart of the %d weeks the activity route answers", block, len(served.Weeks))
		}
		for index, week := range served.Weeks {
			row := block.Rows[index]
			if row["week"] != week.Week || number(t, row["events"]) != week.Events {
				t.Errorf("point %d = %v, want %s: %d as the activity route answers", index, row, week.Week, week.Events)
			}
		}
		if block.Spec["x"] != "week" || block.Spec["y"] != "events" || block.Spec["mark"] != going.MarkBar {
			t.Errorf("chart spec = %v, want the encodings the model named, drawn as bars", block.Spec)
		}
	})

	t.Run("progress", func(t *testing.T) {
		rig := newBlockRig(t, testServerOptions{}, readCall(going.ReadProgress, `{"issue":"DEMO-1"}`),
			blockAnswer(going.BlockProgress, `{"source":"read_progress","params":{"issue":"DEMO-1"}}`))
		seedProgressFixture(t, rig)
		_, block := rig.askForBlock(t, "what is the progress of DEMO-1", "DEMO")
		listed := decodeBody[[]model.IssueSummary](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues?project=DEMO", nil, "alice"))
		index := slices.IndexFunc(listed, func(issue model.IssueSummary) bool { return issue.Key == "DEMO-1" })
		if index < 0 || listed[index].Progress.Tasks == nil || listed[index].Progress.Children == nil {
			t.Fatalf("GET /api/v1/issues lists %+v, want DEMO-1 with task and child progress", listed)
		}
		progress := listed[index].Progress
		if block == nil || block.Kind != going.BlockProgress || len(block.Rows) != 2 {
			t.Fatalf("block = %+v, want a progress block of DEMO-1's tasks and children", block)
		}
		for _, row := range block.Rows {
			want := progress.Tasks
			if row["label"] == "children" {
				want = progress.Children
			}
			if number(t, row["done"]) != want.Done || number(t, row["total"]) != want.Total {
				t.Errorf("progress bar %v, want %d of %d as GET /api/v1/issues answers", row, want.Done, want.Total)
			}
		}
	})

	t.Run("table", func(t *testing.T) {
		spec := `{"source":"read_architecture","params":{"project":"ACME"},"table":"components","columns":["id","done","total"]}`
		rig := newBlockRig(t, testServerOptions{}, readCall(going.ReadArchitecture, `{"project":"ACME"}`), blockAnswer(going.BlockTable, spec))
		seedDeliveryIssueFacts(t, rig.database)
		rig.seed(t, `update issues set status = 'done' where key = 'ACME-2'`)
		_, block := rig.askForBlock(t, "show ACME's components as a table", "ACME")
		tree := decodeBody[struct {
			Components []struct {
				ID          string `json:"id"`
				Done, Total int
			} `json:"components"`
		}](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/ACME/architecture", nil, "alice"))
		if block == nil || block.Kind != going.BlockTable || len(block.Rows) != len(tree.Components) {
			t.Fatalf("block = %+v, want a table of the %d components the architecture route answers", block, len(tree.Components))
		}
		counted := false
		for index, component := range tree.Components {
			row := block.Rows[index]
			if row["id"] != component.ID || number(t, row["done"]) != component.Done || number(t, row["total"]) != component.Total {
				t.Errorf("row %d = %v, want %s: %d of %d as the architecture route answers", index, row, component.ID, component.Done, component.Total)
			}
			counted = counted || component.Total > 0
			if len(row) != 3 {
				t.Errorf("row %d = %v, want only the three columns the spec names", index, row)
			}
		}
		if !counted {
			t.Fatal("the fixture counts no issue for any component, so the table proves nothing")
		}
	})

	t.Run("issue list", func(t *testing.T) {
		rig := newBlockRig(t, testServerOptions{}, readCall(going.ReadProgress, `{"project":"DEMO"}`),
			blockAnswer(going.BlockIssueList, `{"source":"read_progress","params":{"project":"DEMO"}}`))
		seedProgressFixture(t, rig)
		_, block := rig.askForBlock(t, "list the open issues in DEMO", "DEMO")
		page := decodeBody[struct {
			Issues []struct{ Key, Title, Status string } `json:"issues"`
		}](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues?project=DEMO&open=true&limit=25", nil, "alice"))
		if block == nil || block.Kind != going.BlockIssueList || len(block.Rows) != len(page.Issues) || len(page.Issues) == 0 {
			t.Fatalf("block = %+v, want the %d open issues GET /api/v1/issues lists", block, len(page.Issues))
		}
		for index, issue := range page.Issues {
			row := block.Rows[index]
			if row["key"] != issue.Key || row["title"] != issue.Title || row["status"] != issue.Status {
				t.Errorf("row %d = %v, want %+v in the listing's order", index, row, issue)
			}
		}
	})

	t.Run("agent status", func(t *testing.T) {
		listenerURL, _ := sessionsListener(t, `[
			{"session_id":"ses-a","title":"Planner","last_seen":200,"state":"live"},
			{"session_id":"ses-b","title":"Worker","last_seen":100,"state":"unresponsive"}
		]`)
		rig := newBlockRig(t, testServerOptions{envoyURL: listenerURL}, readCall(going.ReadRoster, `{}`),
			blockAnswer(going.BlockAgentStatus, `{"source":"read_roster","params":{}}`))
		seedActivityFixture(t, rig.database)
		rig.seed(t, `insert into asks (issue_key, author, question, state) values
			('DEMO-1', '{"kind":"session","id":"ses-a"}', 'Which week?', 'open'),
			('DEMO-1', '{"kind":"session","id":"ses-a"}', 'Which chart?', 'open')`)
		_, block := rig.askForBlock(t, "agent status", "")
		agents := decodeBody[[]struct {
			SessionID    string  `json:"session_id"`
			Title        string  `json:"title"`
			State        string  `json:"state"`
			OpenAsks     int     `json:"open_asks"`
			LastActivity *string `json:"last_activity"`
		}](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/agents", nil, "alice"))
		if block == nil || block.Kind != going.BlockAgentStatus || len(block.Rows) != len(agents) {
			t.Fatalf("block = %+v, want the %d sessions GET /api/v1/agents lists", block, len(agents))
		}
		for index, agent := range agents {
			row := block.Rows[index]
			activity, _ := row["last_activity"].(string)
			if row["session_id"] != agent.SessionID || row["state"] != agent.State || number(t, row["open_asks"]) != agent.OpenAsks ||
				agent.LastActivity == nil || activity != *agent.LastActivity {
				t.Errorf("row %d = %v, want %+v as GET /api/v1/agents answers", index, row, agent)
			}
		}
	})

	window := map[string]string{"from": "2026-08-30T00:00:00Z", "to": "2026-09-27T20:00:00Z"}
	params := fmt.Sprintf(`"from":%q,"to":%q`, window["from"], window["to"])

	t.Run("metric", func(t *testing.T) {
		spec := `{"source":"read_delivery","params":{"view":"measures",` + params + `},"table":"totals","field":"successful_deploys","label":"Deploys","compare":"deploys_with_prs","compare_label":"Deploys with pull requests"}`
		rig := newBlockRig(t, testServerOptions{}, readCall(going.ReadDelivery, `{"view":"measures",`+params+`}`), blockAnswer(going.BlockMetric, spec))
		seedMeasuresFixture(t, rig.database)
		_, block := rig.askForBlock(t, "how many deploys shipped in that window", "")
		measures := getDeliveryMeasures(t, rig.handler, measuresFixtureWindow).Measures.DeployFrequency
		if block == nil || block.Kind != going.BlockMetric || len(block.Rows) != 1 {
			t.Fatalf("block = %+v, want one metric", block)
		}
		row := block.Rows[0]
		if number(t, row["successful_deploys"]) != measures.SuccessfulDeploys || number(t, row["deploys_with_prs"]) != measures.DeploysWithPRs || measures.SuccessfulDeploys == 0 {
			t.Errorf("metric = %v, want %d deploys and %d with pull requests as GET /api/v1/delivery/measures answers", row, measures.SuccessfulDeploys, measures.DeploysWithPRs)
		}
	})

	t.Run("timeline", func(t *testing.T) {
		spec := `{"source":"read_delivery","params":{"view":"timeline",` + params + `}}`
		rig := newBlockRig(t, testServerOptions{}, readCall(going.ReadDelivery, `{"view":"timeline",`+params+`}`), blockAnswer(going.BlockTimeline, spec))
		seedMeasuresFixture(t, rig.database)
		_, block := rig.askForBlock(t, "show the deploy timeline", "")
		served := getDeliveryTimeline(t, rig.handler, measuresFixtureWindow)
		var want []string
		for _, pr := range served.PRs {
			if pr.MergedAt != nil {
				want = append(want, "merge "+pr.MergedAt.UTC().Format(time.RFC3339))
			}
		}
		for _, run := range served.Runs {
			want = append(want, "deploy "+run.StartedAt.UTC().Format(time.RFC3339))
		}
		if block == nil || block.Kind != going.BlockTimeline {
			t.Fatalf("block = %+v, want a timeline", block)
		}
		got := make([]string, 0, len(block.Rows))
		for _, row := range block.Rows {
			got = append(got, fmt.Sprintf("%s %s", row["lane"], row["at"]))
		}
		slices.Sort(got)
		slices.Sort(want)
		if len(want) == 0 || !slices.Equal(got, want) {
			t.Errorf("timeline events = %v, want every merge and run GET /api/v1/delivery/timeline answers, each at its own time: %v", got, want)
		}
	})
}

// A block with no read behind it is not drawn, and the answer stays prose and links: a spec
// naming a read its kind is not filled by, one naming a table its read does not hold, one whose
// read refuses (a project Dispatch does not hold), and a delivery block on a deployment with no
// delivery settings.
func TestGoingBlockWithNoReadBehindItIsNotDrawn(t *testing.T) {
	for name, test := range map[string]struct{ kind, spec, words string }{
		"a read outside its kind": {going.BlockAgentStatus, `{"source":"read_progress","params":{"project":"DEMO"}}`, "agent status"},
		"a table the read lacks":  {going.BlockChart, `{"source":"read_project_activity","params":{"project":"DEMO"},"table":"deploys","x":"day","y":"count"}`, "chart DEMO's deploys"},
		"a refused read":          {going.BlockChart, `{"source":"read_project_activity","params":{"project":"NOPE"},"table":"weeks","x":"week","y":"events"}`, "chart the activity of NOPE"},
		"no delivery settings":    {going.BlockMetric, `{"source":"read_delivery","params":{"view":"measures"},"table":"totals","field":"successful_deploys"}`, "how many deploys"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newBlockRig(t, testServerOptions{}, blockAnswer(test.kind, test.spec))
			seedActivityFixture(t, rig.database)
			frames, block := rig.askForBlock(t, test.words, "")
			if block != nil {
				t.Errorf("the run drew %+v, want no block: no read fills it", block)
			}
			if !slices.Contains(frameNames(frames), contracts.GoingEventText) {
				t.Errorf("the run stored %v, want the answer's prose all the same", frameNames(frames))
			}
		})
	}
}

// A chart whose read returns fewer than two points is not drawn (R9): one week of activity is one
// point.
func TestGoingBlockUnderTwoPointsDrawsNoChart(t *testing.T) {
	spec := `{"source":"read_project_activity","params":{"project":"DEMO","weeks":1},"table":"weeks","x":"week","y":"events"}`
	rig := newBlockRig(t, testServerOptions{}, blockAnswer(going.BlockChart, spec))
	seedActivityFixture(t, rig.database)
	if _, block := rig.askForBlock(t, "chart this week's activity of DEMO", "DEMO"); block != nil {
		t.Errorf("the run drew %+v from one week, want no chart", block)
	}
}

// A block the model writes unasked for a plain question is dropped, and so is a kind outside the
// seven: the words decide, and the list is closed (R2). The structured call is told
// to write no block, since the words name none.
func TestGoingBlockUnaskedOrOfAnUnknownKindIsDropped(t *testing.T) {
	table := `{"source":"read_project_activity","params":{"project":"DEMO"},"table":"kinds","columns":["kind","count"]}`
	for name, test := range map[string]struct{ kind, words string }{
		"unasked for a plain question": {going.BlockTable, "what happened in DEMO lately"},
		"a kind outside the seven":     {"pie", "show DEMO's activity as a pie table"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newBlockRig(t, testServerOptions{}, blockAnswer(test.kind, table))
			seedActivityFixture(t, rig.database)
			if _, block := rig.askForBlock(t, test.words, "DEMO"); block != nil {
				t.Errorf("the run drew %+v, want the block dropped", block)
			}
		})
	}
	rig := newBlockRig(t, testServerOptions{}, blockAnswer(going.BlockTable, table))
	seedActivityFixture(t, rig.database)
	rig.askForBlock(t, "what happened in DEMO lately", "DEMO")
	told := userMessage(rig.model.Requests()[0])
	if !strings.Contains(told, "block kind none") || strings.Contains(told, "kind: table") {
		t.Errorf("the structured call was told %q, want it told to write no block", told)
	}
}

// A chart is drawn unasked when the decision's display choice is a chart at or above the measured
// chart-display threshold, and the same plain question below it draws none.
func TestGoingBlockChartDrawnUnaskedWhenTheDecisionSettlesOnOne(t *testing.T) {
	_, chartThreshold := measuredThresholds(t)
	if chartThreshold == nil || *chartThreshold <= 0 {
		t.Skipf("the record holds no positive chart-display threshold for %s, so no chart is drawn unasked", goingDecisionModel)
	}
	spec := `{"source":"read_project_activity","params":{"project":"DEMO","weeks":4},"table":"weeks","x":"week","y":"events"}`
	standIn := newDecisionStandIn(t)
	rig := newBlockRig(t, testServerOptions{decider: standIn.decider(t, staticToken)}, blockAnswer(going.BlockChart, spec))
	seedActivityFixture(t, rig.database)
	words := "how active was DEMO lately"

	standIn.answer(t, decidedAnswer{action: going.ActionNone, confidence: 1, about: going.ContextNone, display: going.DisplayChart, displayConfidence: *chartThreshold})
	_, block := rig.askForBlock(t, words, "DEMO")
	if block == nil || block.Kind != going.BlockChart || len(block.Rows) != 4 {
		t.Fatalf("at the chart-display threshold the run drew %+v, want the chart of four weeks", block)
	}
	if told := userMessage(rig.model.Requests()[0]); !strings.Contains(told, "kind: chart") {
		t.Errorf("the structured call was told %q, want the chart offered", told)
	}

	standIn.answer(t, decidedAnswer{action: going.ActionNone, confidence: 1, about: going.ContextNone, display: going.DisplayChart, displayConfidence: *chartThreshold - 0.01})
	if _, block := rig.askForBlock(t, words, "DEMO"); block != nil {
		t.Errorf("below the chart-display threshold the run drew %+v, want no chart for a plain question", block)
	}
}

// userMessage is the user message one call was sent: the question, its items and the offer.
func userMessage(request goingtest.ReceivedRequest) string {
	for _, message := range request.Messages {
		if message.Role == "user" {
			return message.Content
		}
	}
	return ""
}
