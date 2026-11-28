package going_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
)

// The words decide an explicit block request: each kind is named by its own words,
// a plain question names none, and the words other kinds of question use - the command bar, a
// line of work - name no chart.
func TestKindsNamedReadsTheTypedWords(t *testing.T) {
	for _, test := range []struct {
		words string
		want  []string
	}{
		{"chart the activity of DEMO", []string{going.BlockChart}},
		{"Graph the deploys per day", []string{going.BlockChart}},
		{"plot DEMO's issues by status as a bar chart", []string{going.BlockChart}},
		{"show it as a table", []string{going.BlockTable}},
		{"what is the progress of DEMO-12", []string{going.BlockProgress}},
		{"give me the list of open issues in DEMO", []string{going.BlockIssueList}},
		{"issue list for DEMO", []string{going.BlockIssueList}},
		{"agent status please", []string{going.BlockAgentStatus}},
		{"who’s working on DEMO", []string{going.BlockAgentStatus}},
		{"how many issues are open", []string{going.BlockMetric}},
		{"the deploy timeline this week", []string{going.BlockTimeline}},
		{"summarise this project", nil},
		{"what does the command bar do", nil},
		{"is DEMO-12 in line with the plan", nil},
	} {
		if got := going.KindsNamed(test.words); !reflect.DeepEqual(got, test.want) {
			t.Errorf("KindsNamed(%q) = %v, want %v", test.words, got, test.want)
		}
	}
}

// A block is drawn when the words name its kind, and no other is: a chart included, which a plain
// question never draws.
func TestDrawnOnlyForTheNamedKind(t *testing.T) {
	plain := "summarise this project"
	for _, kind := range going.BlockKinds {
		if going.Drawn(kind, plain) {
			t.Errorf("Drawn(%s) for a plain question = true, want false", kind)
		}
	}
	if !going.Drawn(going.BlockTable, "show the issues as a table") {
		t.Error("Drawn(table) for words naming a table = false, want true")
	}
	if going.Drawn(going.BlockChart, "show the issues as a table") {
		t.Error("Drawn(chart) for words naming a table = true, want false")
	}
}

// TestNoBlockIsOfferedWithoutAReadThatFillsIt: a kind the words name is offered only where one of
// the run's reads fills it, so a run with no read offers no block at all, and an agent-status
// block needs the roster read.
func TestNoBlockIsOfferedWithoutAReadThatFillsIt(t *testing.T) {
	noop := func(context.Context, json.RawMessage) (going.Read, error) { return going.Read{}, nil }
	words := "chart the agent status"
	if got := going.OfferedKinds(words, nil); len(got) != 0 {
		t.Errorf("OfferedKinds with no read = %v, want none", got)
	}
	got := going.OfferedKinds(words, map[string]going.ReadFunc{going.ReadProjectActivity: noop})
	if !reflect.DeepEqual(got, []string{going.BlockChart}) {
		t.Errorf("OfferedKinds with the activity read alone = %v, want [chart]: no read fills agent status", got)
	}
	got = going.OfferedKinds(words, map[string]going.ReadFunc{going.ReadProjectActivity: noop, going.ReadRoster: noop})
	if !reflect.DeepEqual(got, []string{going.BlockChart, going.BlockAgentStatus}) {
		t.Errorf("OfferedKinds with the roster read too = %v, want [chart agent_status]", got)
	}
}

// A spec is held to its kind's schema: an unknown kind, a spec that is no object, a field the kind
// does not take - a number the model wrote among them - a read outside the kind's, and a field the
// kind draws left unnamed are each dropped; kind none is no block.
func TestParseBlockDropsWhatDoesNotFitTheKindsSchema(t *testing.T) {
	if _, err := going.ParseBlock(going.FinalBlock{Kind: going.BlockNone}); !errors.Is(err, going.ErrNoBlock) {
		t.Errorf("ParseBlock(none) = %v, want ErrNoBlock", err)
	}
	for name, block := range map[string]going.FinalBlock{
		"unknown kind":           {Kind: "pie", Spec: `{"source":"read_project_activity","params":{},"table":"weeks","x":"week","y":"events"}`},
		"no object":              {Kind: going.BlockChart, Spec: `[1,2,3]`},
		"a number in the spec":   {Kind: going.BlockMetric, Spec: `{"source":"read_progress","params":{},"table":"progress","field":"done","value":42}`},
		"a number for a field":   {Kind: going.BlockChart, Spec: `{"source":"read_project_activity","params":{},"table":"weeks","x":"week","y":12}`},
		"a read outside":         {Kind: going.BlockIssueList, Spec: `{"source":"read_roster","params":{}}`},
		"no y":                   {Kind: going.BlockChart, Spec: `{"source":"read_project_activity","params":{},"table":"weeks","x":"week"}`},
		"a mark no chart draws":  {Kind: going.BlockChart, Spec: `{"source":"read_project_activity","params":{},"table":"weeks","x":"week","y":"events","mark":"pie"}`},
		"params that is no args": {Kind: going.BlockAgentStatus, Spec: `{"source":"read_roster","params":"all"}`},
	} {
		if _, err := going.ParseBlock(block); err == nil || errors.Is(err, going.ErrNoBlock) {
			t.Errorf("%s: ParseBlock(%+v) = %v, want the block dropped", name, block, err)
		}
	}
	block, err := going.ParseBlock(going.FinalBlock{Kind: going.BlockChart, Title: "Weekly activity",
		Spec: `{"source":"read_project_activity","params":{"project":"DEMO","weeks":4},"table":"weeks","x":"week","y":"events"}`})
	if err != nil || block.Source != going.ReadProjectActivity || string(block.Params) != `{"project":"DEMO","weeks":4}` {
		t.Fatalf("ParseBlock(chart) = %+v, %v, want the activity read with its arguments", block, err)
	}
}

// A chart is filled from its read's table alone: each point is the read's own x and y, a row whose
// y is no number is left out, and fewer than two points draw no chart (R3).
func TestFillChartTakesEveryPointFromTheRead(t *testing.T) {
	block := mustParse(t, going.BlockChart, `{"source":"read_project_activity","params":{},"table":"weeks","x":"week","y":"events","mark":"line"}`)
	read := going.Read{Tables: map[string][]map[string]any{"weeks": {
		{"week": "2026-W44", "events": 3, "start": "x"},
		{"week": "2026-W45", "events": nil},
		{"week": "2026-W46", "events": 7},
	}}}
	data, err := block.Fill(read)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	want := []map[string]any{{"week": "2026-W44", "events": json.Number("3")}, {"week": "2026-W46", "events": json.Number("7")}}
	if !reflect.DeepEqual(data.Rows, want) {
		t.Errorf("chart rows = %v, want %v: the read's own points, the null one left out", data.Rows, want)
	}
	var spec map[string]any
	if json.Unmarshal(data.Spec, &spec) != nil || spec["mark"] != "line" || spec["x"] != "week" || spec["y"] != "events" {
		t.Errorf("chart spec = %s, want the encodings the model named", data.Spec)
	}

	one := going.Read{Tables: map[string][]map[string]any{"weeks": {{"week": "2026-W46", "events": 7}}}}
	if _, err := block.Fill(one); err == nil {
		t.Error("Fill of a one-point read drew a chart, want none: a chart plots at least two points")
	}
	if _, err := block.Fill(going.Read{Tables: map[string][]map[string]any{"kinds": {}}}); err == nil {
		t.Error("Fill of a read with no such table drew a chart, want none")
	}
}

// A metric is one number with at most one comparison, read from a table of one row; a table of
// more is no one number, and a field that is no number draws nothing.
func TestFillMetricIsOneNumberFromOneRow(t *testing.T) {
	block := mustParse(t, going.BlockMetric, `{"source":"read_delivery","params":{"view":"measures"},"table":"totals","field":"successful_deploys","label":"Deploys","compare":"deploys_with_prs"}`)
	data, err := block.Fill(going.Read{Tables: map[string][]map[string]any{"totals": {{"successful_deploys": 12, "deploys_with_prs": 9, "rework_share": 0.1}}}})
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if want := []map[string]any{{"successful_deploys": json.Number("12"), "deploys_with_prs": json.Number("9")}}; !reflect.DeepEqual(data.Rows, want) {
		t.Errorf("metric rows = %v, want %v", data.Rows, want)
	}
	if _, err := block.Fill(going.Read{Tables: map[string][]map[string]any{"totals": {{"successful_deploys": 1}, {"successful_deploys": 2}}}}); err == nil {
		t.Error("Fill of a two-row table drew a metric, want none")
	}
	if _, err := block.Fill(going.Read{Tables: map[string][]map[string]any{"totals": {{"successful_deploys": "twelve", "deploys_with_prs": 9}}}}); err == nil {
		t.Error("Fill of a field that is no number drew a metric, want none")
	}
}

// A progress block draws a bar per row with something to count; a timeline lays the read's dated
// events in lanes, leaving out an event with no time a reader can place.
func TestFillProgressAndTimeline(t *testing.T) {
	progress := mustParse(t, going.BlockProgress, `{"source":"read_progress","params":{"issue":"DEMO-1"}}`)
	data, err := progress.Fill(going.Read{Tables: map[string][]map[string]any{"progress": {
		{"label": "tasks", "done": 2, "total": 5}, {"label": "children", "done": 0, "total": 0},
	}}})
	if err != nil {
		t.Fatalf("Fill progress: %v", err)
	}
	if want := []map[string]any{{"label": "tasks", "done": json.Number("2"), "total": json.Number("5")}}; !reflect.DeepEqual(data.Rows, want) {
		t.Errorf("progress rows = %v, want %v: a bar of nothing to count left out", data.Rows, want)
	}

	timeline := mustParse(t, going.BlockTimeline, `{"source":"read_delivery","params":{"view":"timeline"}}`)
	data, err = timeline.Fill(going.Read{Tables: map[string][]map[string]any{"events": {
		{"at": "2026-11-02T10:00:00Z", "lane": "merge", "label": "acme/widgets#1"},
		{"at": "yesterday", "lane": "deploy", "label": "run 7"},
		{"at": "2026-11-02T11:00:00Z", "lane": "deploy", "label": "run 8: success"},
	}}})
	if err != nil {
		t.Fatalf("Fill timeline: %v", err)
	}
	lanes := make([]string, 0, len(data.Rows))
	for _, row := range data.Rows {
		lanes = append(lanes, row["lane"].(string)+" "+row["label"].(string))
	}
	if !slices.Equal(lanes, []string{"merge acme/widgets#1", "deploy run 8: success"}) {
		t.Errorf("timeline events = %v, want the two dated ones in their lanes", lanes)
	}
}

func mustParse(t *testing.T, kind, spec string) going.Block {
	t.Helper()
	block, err := going.ParseBlock(going.FinalBlock{Kind: kind, Title: "A block", Spec: spec})
	if err != nil {
		t.Fatalf("ParseBlock(%s %s): %v", kind, spec, err)
	}
	return block
}

// issueList is the saved result of step, call 1: a read of every issue of a project at once
// (dispatch_issues with all), whose answer is the list of its issues.
func issueList(t *testing.T, step int, project string, issues []map[string]any) going.Result {
	t.Helper()
	body, err := json.Marshal(issues)
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(map[string]any{"project": project, "all": true})
	return going.Result{Step: step, Call: 1, Tool: "dispatch_issues", Arguments: arguments, Status: 200, Body: body}
}

// issuesOf is count issues of project, each with status and priority, nil for none.
func issuesOf(project string, count int, status string, priority any) []map[string]any {
	issues := make([]map[string]any, count)
	for index := range issues {
		issues[index] = map[string]any{"project": project, "status": status, "priority": priority}
	}
	return issues
}

// openIssues is the saved lists of DEMO (step 4) and OPS (step 5): 10 open issues of DEMO at P0
// or P1 and 6 of OPS, beside open issues at other priorities and done ones at P0 and P1.
func openIssues(t *testing.T) []going.Result {
	demo := slices.Concat(issuesOf("DEMO", 4, "todo", 0), issuesOf("DEMO", 6, "in_progress", 1), issuesOf("DEMO", 3, "todo", 2),
		issuesOf("DEMO", 2, "done", 0), issuesOf("DEMO", 1, "triage", nil))
	ops := slices.Concat(issuesOf("OPS", 2, "in_progress", 0), issuesOf("OPS", 4, "todo", 1), issuesOf("OPS", 3, "done", 1), issuesOf("OPS", 2, "todo", 3))
	return []going.Result{issueList(t, 4, "DEMO", demo), issueList(t, 5, "OPS", ops)}
}

// TestAFiltersValueMayBeANumber: a filter's value picks the rows the chain reads and is never
// drawn, so a number there is no number of the model's that a block shows. chart-17 of the eval
// filtered priority in [0, 1] and was refused for the 0, and wrote no chart; that chain draws the
// open P0 and P1 issues of each project, and is, is_not and has take a number too. A number
// anywhere a block could show it - its title, a group's or a count's as, any field other than a
// filter's condition - is still refused.
func TestAFiltersValueMayBeANumber(t *testing.T) {
	const chart = `{"kind":"chart","title":"Open P0 and P1 issues by project","steps":[{"step":4,"call":1,"rows":""},{"step":5,"call":1,"rows":""}],` +
		`"chain":[{"op":"filter","field":"status","is_not":"done"},{"op":"filter","field":"priority","in":[0,1]},{"op":"group","field":"project"},{"op":"count"}],"x":"project","y":"count"}`
	got, err := draw(t, chart, openIssues(t)...)
	if err != nil || got != `[{"count":10,"project":"DEMO"},{"count":6,"project":"OPS"}]` {
		t.Fatalf("the chart of open P0 and P1 issues drew %s, %v; want DEMO's 10 and OPS's 6", got, err)
	}
	for _, row := range []struct{ filter, want string }{
		{`{"op":"filter","field":"priority","is":0}`, `[{"count":6}]`},
		{`{"op":"filter","field":"priority","is_not":1}`, `[{"count":12}]`},
	} {
		spec := `{"kind":"metric","title":"Issues","steps":[{"step":4,"call":1},{"step":5,"call":1}],"chain":[{"op":"filter","field":"status","is_not":"done"},` + row.filter + `,{"op":"count"}],"field":"count"}`
		if got, err := draw(t, spec, openIssues(t)...); err != nil || got != row.want {
			t.Errorf("the metric over the filter %s drew %s, %v; want %s", row.filter, got, err, row.want)
		}
	}
	labelled := issueList(t, 2, "DEMO", []map[string]any{{"key": "DEMO-1", "sizes": []any{1, 3}}, {"key": "DEMO-2", "sizes": []any{2}}})
	if got, err := draw(t, `{"kind":"table","title":"Issues of that size","steps":[{"step":2,"call":1}],"chain":[{"op":"filter","field":"sizes","has":3}],"columns":["key"]}`, labelled); err != nil || got != `[{"key":"DEMO-1"}]` {
		t.Errorf("the table of the issues whose sizes hold 3 drew %s, %v; want DEMO-1", got, err)
	}

	for name, spec := range map[string]string{
		"a number in its title":        strings.Replace(chart, `"title":"Open P0 and P1 issues by project"`, `"title":"Top 5 projects"`, 1),
		"a number as a group's as":     strings.Replace(chart, `{"op":"group","field":"project"}`, `{"op":"group","field":"project","as":7}`, 1),
		"a number as a count's as":     strings.Replace(chart, `{"op":"count"}`, `{"op":"count","as":16}`, 1),
		"a number as a filter's field": strings.Replace(chart, `"field":"priority","in"`, `"field":3,"in"`, 1),
		"a number as its x":            strings.Replace(chart, `"x":"project"`, `"x":10`, 1),
	} {
		if _, err := going.ParseRunBlock(json.RawMessage(spec)); err == nil || !strings.Contains(err.Error(), "holds no number") {
			t.Errorf("a spec with %s was answered %v, want it refused for holding a number", name, err)
		}
	}
	if _, err := going.ParseRunBlock(json.RawMessage(strings.Replace(chart, `"in":[0,1]`, `"in":[{"p":0}]`, 1))); err == nil || !strings.Contains(err.Error(), "a filter's value is text, a number, or true or false") {
		t.Errorf("a filter whose value is an object was answered %v, want it refused, saying what a filter's value is", err)
	}
}

// TestAChartOrATableTakesTheColumnsItsChainMakes: chart-08 of the eval wrote a chart's chain of
// a group by priority and a count, with no x and no y, and was refused on its run's second-to-last
// call, whose last could write only prose. A chart that names no x takes the column of its chain's
// group, and one that names no y the one column a count, sum, mean, min or max writes; a table that
// names no columns takes the group's and then those, in the chain's order. A chain that leaves
// either open - no group, or two counts - is refused, naming the columns its answer holds.
func TestAChartOrATableTakesTheColumnsItsChainMakes(t *testing.T) {
	const chain = `"chain":[{"op":"filter","field":"status","is_not":"done"},{"op":"group","field":"priority"},{"op":"count","as":"issues"},{"op":"sort","field":"priority"}]`
	const steps = `"steps":[{"step":4,"call":1,"rows":""},{"step":5,"call":1,"rows":""}]`
	block, err := going.ParseRunBlock(json.RawMessage(`{"kind":"chart","title":"Open issues by priority",` + steps + `,` + chain + `}`))
	if err != nil {
		t.Fatalf("the chart that names no x and no y was refused: %v", err)
	}
	if block.X != "priority" || block.Y != "issues" {
		t.Errorf("the chart draws x %q and y %q, want the chain's group priority and its count issues", block.X, block.Y)
	}
	data, err := block.Draw(openIssues(t))
	if err != nil {
		t.Fatalf("the chart was refused: %v", err)
	}
	rows, _ := json.Marshal(data.Rows)
	if want := `[{"issues":6,"priority":0},{"issues":10,"priority":1},{"issues":3,"priority":2},{"issues":2,"priority":3},{"issues":1,"priority":"none"}]`; string(rows) != want {
		t.Errorf("the chart draws %s, want %s", rows, want)
	}
	var spec map[string]any
	if json.Unmarshal(data.Spec, &spec) != nil || spec["x"] != "priority" || spec["y"] != "issues" {
		t.Errorf("the chart's spec is %s, want the x and y it was drawn with", data.Spec)
	}

	table, err := going.ParseRunBlock(json.RawMessage(`{"kind":"table","title":"Open issues by priority",` + steps + `,` +
		`"chain":[{"op":"group","field":"priority"},{"op":"count","as":"issues"},{"op":"max","field":"priority","as":"highest"}]}`))
	if err != nil || !slices.Equal(table.Columns, []string{"priority", "issues", "highest"}) {
		t.Errorf("the table that names no columns draws %v (%v), want the chain's group and its columns in their order", table.Columns, err)
	}

	for _, row := range []struct{ name, spec, says string }{
		{"no group", `{"kind":"chart","title":"Open issues",` + steps + `,"chain":[{"op":"filter","field":"status","is_not":"done"},{"op":"count","as":"issues"}]}`,
			"names no x, and its chain's answer holds the columns issues: name x"},
		{"two counts", `{"kind":"chart","title":"Issues by status",` + steps + `,"chain":[{"op":"group","field":"status"},{"op":"count","as":"issues"},{"op":"sum","field":"priority","as":"points"}]}`,
			"names no y, and its chain's answer holds the columns status, issues, points: name y"},
		{"a table of the rows themselves", `{"kind":"table","title":"Open issues",` + steps + `,"chain":[{"op":"filter","field":"status","is_not":"done"}]}`,
			"names no columns, and its chain's answer holds the rows it reads, each with its own fields"},
	} {
		if _, err := going.ParseRunBlock(json.RawMessage(row.spec)); err == nil || !strings.Contains(err.Error(), row.says) {
			t.Errorf("the spec with %s was answered %v, want it refused saying %q", row.name, err, row.says)
		}
	}

	// The saved results hold each priority as the server answered it, and no priority_shown,
	// which the agent gives its model beside it: a group by it is refused, naming priority.
	shown := `{"kind":"chart","title":"Open issues by priority",` + steps + `,"chain":[{"op":"group","field":"priority_shown"},{"op":"count","as":"issues"}]}`
	if _, err := draw(t, shown, openIssues(t)...); err == nil || !strings.Contains(err.Error(), "no row holds priority_shown") || !strings.Contains(err.Error(), "such as priority") {
		t.Errorf("the chart grouped by priority_shown was answered %v, want it refused, naming priority", err)
	}
}

// TestAOnePointChartIsToldWhatToDrawInstead: chart-17 of the eval drew open P0 issues by project,
// which one project held, and was refused a chart of one point with no word of what to draw; at
// 59e2a6ec, told to chart a field with more values, it kept only its chart of P1 issues. The
// refusal says to draw a metric or a table for one point; the minimum of two points stays (R3).
func TestAOnePointChartIsToldWhatToDrawInstead(t *testing.T) {
	results := openIssues(t)
	results[0] = issueList(t, 4, "DEMO", issuesOf("DEMO", 3, "todo", 2))
	_, err := draw(t, `{"kind":"chart","title":"Open P0 issues by project","steps":[{"step":4,"call":1},{"step":5,"call":1}],`+
		`"chain":[{"op":"filter","field":"status","is_not":"done"},{"op":"filter","field":"priority","is":0},{"op":"group","field":"project"},{"op":"count"}]}`, results...)
	if err == nil || !strings.Contains(err.Error(), "the chart holds 1 point, and a chart plots at least 2 points; draw a metric or a table for one") {
		t.Errorf("the chart of one point was answered %v, want it refused, saying what to draw instead", err)
	}
}

// eachStatus is the issues of DEMO and OPS in each status, as the eval's seed holds them.
var eachStatus = []struct {
	status    string
	demo, ops int
}{{"backlog", 6, 4}, {"done", 11, 10}, {"in_progress", 10, 5}, {"needs_review", 2, 1}, {"testing", 2, 1}, {"todo", 8, 8}, {"triage", 3, 2}}

// TestTheChartTasksChainsDrawTheirRows: the chains the system prompt gives for the words of
// chart-17, "P0 and P1 open issues per project", and of chart-19, "how many issues in each status
// across DEMO and OPS", draw the rows going/eval/tasks.json grades them by. At 59e2a6ec the model
// grouped each by project and then by a second field, which drew no block. A count split by two
// fields, each status per project, is a table with a column for each and one row for each pair
// of values; a chart of two groups, as chart-17 wrote it, is refused, saying to draw a table.
func TestTheChartTasksChainsDrawTheirRows(t *testing.T) {
	const openP0AndP1 = `"chain":[{"op":"filter","field":"status","is_not":"done"},{"op":"filter","field":"priority","in":[0,1]},{"op":"group","field":"project"}%s,{"op":"count","as":"issues"}]`
	got, err := draw(t, `{"kind":"chart","title":"Open P0 and P1 issues per project","steps":[{"step":4,"call":1},{"step":5,"call":1}],`+fmt.Sprintf(openP0AndP1, "")+`}`, openIssues(t)...)
	if want := `[{"issues":10,"project":"DEMO"},{"issues":6,"project":"OPS"}]`; err != nil || got != want {
		t.Errorf("chart-17's chain drew %s, %v; want %s", got, err, want)
	}

	var demo, ops []map[string]any
	var demoRows, opsRows []string
	for _, row := range eachStatus {
		demo = append(demo, issuesOf("DEMO", row.demo, row.status, nil)...)
		ops = append(ops, issuesOf("OPS", row.ops, row.status, nil)...)
		demoRows = append(demoRows, fmt.Sprintf(`{"issues":%d,"project":"DEMO","status":%q}`, row.demo, row.status))
		opsRows = append(opsRows, fmt.Sprintf(`{"issues":%d,"project":"OPS","status":%q}`, row.ops, row.status))
	}
	lists := []going.Result{issueList(t, 2, "DEMO", demo), issueList(t, 3, "OPS", ops)}
	const steps = `"steps":[{"step":2,"call":1},{"step":3,"call":1}]`
	got, err = draw(t, `{"kind":"table","title":"Issues in each status",`+steps+`,"chain":[{"op":"group","field":"status"},{"op":"count","as":"issues"}]}`, lists...)
	if want := `[{"issues":10,"status":"backlog"},{"issues":21,"status":"done"},{"issues":15,"status":"in_progress"},{"issues":3,"status":"needs_review"},` +
		`{"issues":3,"status":"testing"},{"issues":16,"status":"todo"},{"issues":5,"status":"triage"}]`; err != nil || got != want {
		t.Errorf("chart-19's chain drew %s, %v; want %s", got, err, want)
	}

	const twoFields = `{"kind":"table","title":"Issues in each status per project",` + steps + `,"chain":[{"op":"group","field":"project"},{"op":"group","field":"status"},{"op":"count","as":"issues"}]}`
	table, err := going.ParseRunBlock(json.RawMessage(twoFields))
	if err != nil || !slices.Equal(table.Columns, []string{"project", "status", "issues"}) {
		t.Errorf("the table of each status per project draws the columns %v (%v), want project, status and issues", table.Columns, err)
	}
	got, err = draw(t, twoFields, lists...)
	if want := "[" + strings.Join(slices.Concat(demoRows, opsRows), ",") + "]"; err != nil || got != want {
		t.Errorf("the table of each status per project drew %s, %v; want %s", got, err, want)
	}

	_, err = going.ParseRunBlock(json.RawMessage(`{"kind":"chart","title":"Open P0 and P1 issues per project","steps":[{"step":4,"call":1},{"step":5,"call":1}],` +
		fmt.Sprintf(openP0AndP1, `,{"op":"group","field":"priority"}`) + `,"x":"project","y":"issues"}`))
	if err == nil || !strings.Contains(err.Error(), "a chart plots one series, and its chain groups by project and priority: draw a table for two fields, or chart one of them") {
		t.Errorf("the chart of two groups was answered %v, want it refused, saying to draw a table", err)
	}
}

// TestRowsNamedOnAListResultIsToldToLeaveRowsOut: chart-08 of the eval named rows "issues" on two
// reads of every issue of a project, each of which answers the list itself, and was told only
// that the result held no list there. The refusal says the result is the list and to leave rows
// out, whatever rows names; the same spec with rows left out draws the list.
func TestRowsNamedOnAListResultIsToldToLeaveRowsOut(t *testing.T) {
	const spec = `{"kind":"chart","title":"Open issues by project","steps":[{"step":4,"call":1%s},{"step":5,"call":1%s}],` +
		`"chain":[{"op":"filter","field":"status","is_not":"done"},{"op":"group","field":"project"},{"op":"count","as":"issues"}],"x":"project","y":"issues"}`
	for _, rows := range []string{"issues", "$"} {
		named := `,"rows":"` + rows + `"`
		_, err := draw(t, fmt.Sprintf(spec, named, named), openIssues(t)...)
		if err == nil || !strings.Contains(err.Error(), `step 4's result is the list itself, so it has no field "`+rows+`". Leave rows out to read the list`) {
			t.Errorf("rows %q on a result that is a list was answered %v, want it refused, saying to leave rows out", rows, err)
		}
	}
	got, err := draw(t, fmt.Sprintf(spec, "", ""), openIssues(t)...)
	if want := `[{"issues":14,"project":"DEMO"},{"issues":8,"project":"OPS"}]`; err != nil || got != want {
		t.Errorf("the spec with rows left out drew %s, %v; want %s", got, err, want)
	}
}

// TestAColumnItsChainDoesNotWriteIsToldTheColumnsItDoes: a chain that groups or counts answers only
// the columns it writes, a group's named as, else per, else field. chart-01 of the eval drew x
// closed_at over a group of closed_at per week, whose column is week, and chart-09 a table of
// session_title over a group of claim.actor.origin.session_title; each was told the read's rows
// held no such field, which they did, and one run asked for closed_at seven times. The spec is
// refused naming the columns the chain's answer holds and the column the group writes, and the
// spec that draws that column draws its rows.
func TestAColumnItsChainDoesNotWriteIsToldTheColumnsItDoes(t *testing.T) {
	const weekly = `{"kind":"chart","title":"Issues closed each week","steps":[{"step":2,"call":1}],` +
		`"chain":[{"op":"filter","field":"closed_at","present":true},{"op":"group","field":"closed_at","per":"week"},{"op":"count","as":"closed"}],"x":"%s","y":"closed"}`
	_, err := going.ParseRunBlock(json.RawMessage(fmt.Sprintf(weekly, "closed_at")))
	if err == nil || !strings.Contains(err.Error(), `the chart draws "closed_at", and its chain's answer holds no such column: it holds the columns week, closed. `+
		`The group of closed_at writes its values in the column week: draw week`) {
		t.Errorf("the chart of x closed_at over a group of it per week was answered %v, want it refused, naming the column week", err)
	}
	if got, err := draw(t, fmt.Sprintf(weekly, "week"), issuePage(t, 2, 0, 50)); err != nil || got != `[{"closed":2,"week":"2026-11-02"},{"closed":3,"week":"2026-11-09"}]` {
		t.Errorf("the chart of x week drew %s, %v; want the two weeks DEMO closed issues in", got, err)
	}

	holder := func(title string) map[string]any {
		return map[string]any{"actor": map[string]any{"origin": map[string]any{"session_title": title}}}
	}
	claims := issueList(t, 3, "DEMO", []map[string]any{{"key": "DEMO-7", "claim": holder("Planner of DEMO-7")}, {"key": "DEMO-15", "claim": holder("Implementer of DEMO-15")}, {"key": "DEMO-1", "claim": nil}})
	const byHolder = `{"kind":"table","title":"Issues by claim holder","steps":[{"step":3,"call":1}],` +
		`"chain":[{"op":"group","field":"claim.actor.origin.session_title"},{"op":"count","as":"issues"}],"columns":["%s","issues"]}`
	_, err = going.ParseRunBlock(json.RawMessage(fmt.Sprintf(byHolder, "session_title")))
	if err == nil || !strings.Contains(err.Error(), `the table draws "session_title", and its chain's answer holds no such column: it holds the columns claim.actor.origin.session_title, issues. `+
		`The group of claim.actor.origin.session_title writes its values in the column claim.actor.origin.session_title: draw claim.actor.origin.session_title`) {
		t.Errorf("the table of session_title over a group of claim.actor.origin.session_title was answered %v, want it refused, naming the group's column", err)
	}
	got, err := draw(t, fmt.Sprintf(byHolder, "claim.actor.origin.session_title"), claims)
	if want := `[{"claim.actor.origin.session_title":"Planner of DEMO-7","issues":1},{"claim.actor.origin.session_title":"Implementer of DEMO-15","issues":1},` +
		`{"claim.actor.origin.session_title":"none","issues":1}]`; err != nil || got != want {
		t.Errorf("the table of the group's column drew %s, %v; want %s", got, err, want)
	}

	// A metric is held to its chain's columns too; a chain that neither groups nor counts answers
	// the rows it reads, so a table of their own fields is drawn as before.
	if _, err := going.ParseRunBlock(json.RawMessage(`{"kind":"metric","title":"Issues","steps":[{"step":2,"call":1}],"chain":[{"op":"count","as":"issues"}],"field":"count"}`)); err == nil ||
		!strings.Contains(err.Error(), `the metric draws "count", and its chain's answer holds no such column: it holds the columns issues`) {
		t.Errorf("the metric of count over a count named issues was answered %v, want it refused, naming the column issues", err)
	}
	if got, err := draw(t, `{"kind":"table","title":"In progress","steps":[{"step":2,"call":1}],"chain":[{"op":"filter","field":"status","is":"in_progress"}],"columns":["key","estimate"]}`, issuePage(t, 2, 0, 50)); err != nil ||
		got != `[{"estimate":8,"key":"DEMO-4"}]` {
		t.Errorf("the table of a filter's rows drew %s, %v; want DEMO-4 with its estimate", got, err)
	}
}
