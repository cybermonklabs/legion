package going_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/going"
)

// The words decide an explicit block request (decision 17): each kind is named by its own words,
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

// A block is drawn when the words name its kind; a chart is also drawn when the decision settled
// on one unasked, and no other kind is.
func TestDrawnOnlyForTheNamedKindOrAChartTheDecisionChose(t *testing.T) {
	plain := "summarise this project"
	for _, kind := range going.BlockKinds {
		if going.Drawn(kind, plain, false) {
			t.Errorf("Drawn(%s) for a plain question with no chart decided = true, want false", kind)
		}
		if want := kind == going.BlockChart; going.Drawn(kind, plain, true) != want {
			t.Errorf("Drawn(%s) for a plain question with a chart decided = %v, want %v", kind, !want, want)
		}
	}
	if !going.Drawn(going.BlockTable, "show the issues as a table", false) {
		t.Error("Drawn(table) for words naming a table = false, want true")
	}
}

// TestNoBlockIsOfferedWithoutAReadThatFillsIt: a kind the words name is offered only where one of
// the run's reads fills it, so a run with no read offers no block at all, and an agent-status
// block needs the roster read.
func TestNoBlockIsOfferedWithoutAReadThatFillsIt(t *testing.T) {
	noop := func(context.Context, json.RawMessage) (going.Read, error) { return going.Read{}, nil }
	words := "chart the agent status"
	if got := going.OfferedKinds(words, true, nil); len(got) != 0 {
		t.Errorf("OfferedKinds with no read = %v, want none", got)
	}
	got := going.OfferedKinds(words, false, map[string]going.ReadFunc{going.ReadProjectActivity: noop})
	if !reflect.DeepEqual(got, []string{going.BlockChart}) {
		t.Errorf("OfferedKinds with the activity read alone = %v, want [chart]: no read fills agent status", got)
	}
	got = going.OfferedKinds(words, false, map[string]going.ReadFunc{going.ReadProjectActivity: noop, going.ReadRoster: noop})
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
// y is no number is left out, and fewer than two points draw no chart (R9).
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
