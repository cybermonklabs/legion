package going_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/envoy/going"
)

// transformIssues is the rows of one list read of a run: seven issues of DEMO, with a status, an
// estimate (one with none), the time each closed (two still open) and their labels.
var transformIssues = []map[string]any{
	{"key": "DEMO-1", "status": "done", "estimate": 3, "closed_at": "2026-11-02T10:00:00Z", "labels": []string{"bug"}},
	{"key": "DEMO-2", "status": "done", "estimate": 5, "closed_at": "2026-11-04T09:00:00Z", "labels": []string{"bug", "ui"}},
	{"key": "DEMO-3", "status": "done", "estimate": 2, "closed_at": "2026-11-10T12:00:00Z", "labels": []string{"ui"}},
	{"key": "DEMO-4", "status": "in_progress", "estimate": 8, "closed_at": nil, "labels": []string{}},
	{"key": "DEMO-5", "status": "todo", "estimate": nil, "closed_at": nil, "labels": []string{"bug"}},
	{"key": "DEMO-6", "status": "done", "estimate": 1, "closed_at": "2026-11-15T23:00:00Z", "labels": []string{}},
	{"key": "DEMO-7", "status": "done", "estimate": 0.5, "closed_at": "2026-11-12T08:00:00Z", "labels": []string{"ui"}},
}

// issuePage is the saved result of step, call 1: the issues of DEMO from offset, at most limit of
// them, as the issue list answers a paged read, {issues, total, limit, offset}.
func issuePage(t *testing.T, step, offset, limit int) going.Result {
	t.Helper()
	end := min(offset+limit, len(transformIssues))
	body, err := json.Marshal(map[string]any{"issues": transformIssues[offset:end], "total": len(transformIssues), "limit": limit, "offset": offset})
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(map[string]any{"project": "DEMO", "limit": limit, "offset": offset})
	return going.Result{Step: step, Call: 1, Tool: "dispatch_issues", Arguments: arguments, Status: 200, Body: body}
}

// draw parses spec and draws it from results, answering its rows as JSON.
func draw(t *testing.T, spec string, results ...going.Result) (string, error) {
	t.Helper()
	block, err := going.ParseRunBlock(json.RawMessage(spec))
	if err != nil {
		return "", err
	}
	data, err := block.Draw(results)
	if err != nil {
		return "", err
	}
	rows, err := json.Marshal(data.Rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(rows), nil
}

// TestEachTransformMakesItsNumber: each of the eight steps of a chain, and a chain of them, makes
// the rows a block draws from a run's saved results (settlement 9, R3), and every number in them
// is one a result held or a step made. A spec that carries a number is refused, and so is a chain
// over a result the server cut or over a list read the run did not read to its last page, each
// saying the data was not whole.
func TestEachTransformMakesItsNumber(t *testing.T) {
	whole := func(t *testing.T) going.Result { return issuePage(t, 2, 0, 50) }
	for _, row := range []struct {
		name, spec, want string
	}{
		{
			name: "filter",
			spec: `{"kind":"table","title":"Done","steps":[{"step":2,"call":1}],"chain":[{"op":"filter","field":"status","is":"done"}],"columns":["key"]}`,
			want: `[{"key":"DEMO-1"},{"key":"DEMO-2"},{"key":"DEMO-3"},{"key":"DEMO-6"},{"key":"DEMO-7"}]`,
		},
		{
			name: "group",
			spec: `{"kind":"table","title":"Weeks","steps":[{"step":2,"call":1}],"chain":[{"op":"filter","field":"closed_at","present":true},{"op":"group","field":"closed_at","per":"week"}],"columns":["week"]}`,
			want: `[{"week":"2026-11-02"},{"week":"2026-11-09"}]`,
		},
		{
			name: "count",
			spec: `{"kind":"table","title":"By status","steps":[{"step":2,"call":1}],"chain":[{"op":"group","field":"status"},{"op":"count"}],"columns":["status","count"]}`,
			want: `[{"count":5,"status":"done"},{"count":1,"status":"in_progress"},{"count":1,"status":"todo"}]`,
		},
		{
			name: "sum",
			spec: `{"kind":"table","title":"Estimates","steps":[{"step":2,"call":1}],"chain":[{"op":"group","field":"status"},{"op":"sum","field":"estimate"}],"columns":["status","sum"]}`,
			want: `[{"status":"done","sum":11.5},{"status":"in_progress","sum":8},{"status":"todo","sum":0}]`,
		},
		{
			name: "mean",
			spec: `{"kind":"metric","title":"Mean estimate","steps":[{"step":2,"call":1}],"chain":[{"op":"mean","field":"estimate"}],"field":"mean"}`,
			want: `[{"mean":3.25}]`,
		},
		{
			name: "min",
			spec: `{"kind":"metric","title":"Least estimate","steps":[{"step":2,"call":1}],"chain":[{"op":"min","field":"estimate"}],"field":"min"}`,
			want: `[{"min":0.5}]`,
		},
		{
			name: "max",
			spec: `{"kind":"metric","title":"Greatest estimate","steps":[{"step":2,"call":1}],"chain":[{"op":"max","field":"estimate"}],"field":"max"}`,
			want: `[{"max":8}]`,
		},
		{
			name: "sort",
			spec: `{"kind":"table","title":"Largest first","steps":[{"step":2,"call":1}],"chain":[{"op":"sort","field":"estimate","order":"desc"}],"columns":["key"]}`,
			want: `[{"key":"DEMO-4"},{"key":"DEMO-2"},{"key":"DEMO-1"},{"key":"DEMO-3"},{"key":"DEMO-6"},{"key":"DEMO-7"},{"key":"DEMO-5"}]`,
		},
		{
			name: "a chain of filter, group, count and sort draws the issues closed each week",
			spec: `{"kind":"chart","title":"Issues closed each week","steps":[{"step":2,"call":1}],"chain":[` +
				`{"op":"filter","field":"closed_at","present":true},{"op":"group","field":"closed_at","per":"week"},{"op":"count"},{"op":"sort","field":"week","order":"desc"}],` +
				`"x":"week","y":"count"}`,
			want: `[{"count":3,"week":"2026-11-09"},{"count":2,"week":"2026-11-02"}]`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := draw(t, row.spec, whole(t))
			if err != nil {
				t.Fatalf("the block was refused: %v", err)
			}
			if got != row.want {
				t.Errorf("the block draws %s, want %s", got, row.want)
			}
		})
	}

	t.Run("a block spec that carries a number is refused", func(t *testing.T) {
		const counted = `{"kind":"metric","title":"Issues closed","steps":[{"step":2,"call":1}],"chain":[{"op":"filter","field":"status","is":"done"},{"op":"count"}],"field":"count"}`
		if got, err := draw(t, counted, whole(t)); err != nil || got != `[{"count":5}]` {
			t.Fatalf("the spec with no number drew %s, %v; want the server's count of 5", got, err)
		}
		for name, spec := range map[string]string{
			"a number in the chain": strings.Replace(counted, `"is":"done"`, `"is":5`, 1),
			"a number of its own":   strings.Replace(counted, `"field":"count"}`, `"field":"count","rows":[{"count":42}]}`, 1),
			"a number in its title": strings.Replace(counted, `"title":"Issues closed"`, `"title":"42 issues closed"`, 1),
		} {
			_, err := going.ParseRunBlock(json.RawMessage(spec))
			if err == nil || !strings.Contains(err.Error(), "holds no number") {
				t.Errorf("a spec with %s was answered %v, want it refused for holding a number", name, err)
			}
		}
	})

	t.Run("a chain over a cut result is refused and says so", func(t *testing.T) {
		cut := whole(t)
		cut.Body = cut.Body[:len(cut.Body)/2]
		cut.NotWhole = "was cut by the server at 1048576 bytes, and what it held past them is not saved"
		_, err := draw(t, `{"kind":"metric","title":"Issues","steps":[{"step":2,"call":1}],"chain":[{"op":"count"}],"field":"count"}`, cut)
		if err == nil || !strings.Contains(err.Error(), "the data was not whole") || !strings.Contains(err.Error(), "cut by the server") {
			t.Fatalf("a chain over a cut result was answered %v, want it refused, saying the data was not whole because the server cut it", err)
		}
	})

	t.Run("a chain over a list read not read to its last page is refused", func(t *testing.T) {
		const count = `{"kind":"metric","title":"Issues","steps":[%s],"chain":[{"op":"count"}],"field":"count"}`
		first, second, last := issuePage(t, 2, 0, 3), issuePage(t, 4, 3, 3), issuePage(t, 6, 6, 3)
		for name, steps := range map[string]string{
			"its first page":         `{"step":2,"call":1}`,
			"its first two pages":    `{"step":2,"call":1},{"step":4,"call":1}`,
			"its first and its last": `{"step":2,"call":1},{"step":6,"call":1}`,
		} {
			_, err := draw(t, strings.Replace(count, "%s", steps, 1), first, second, last)
			if err == nil || !strings.Contains(err.Error(), "the data was not whole") {
				t.Errorf("a chain over %s of seven issues was answered %v, want it refused, saying the data was not whole", name, err)
			}
		}
		got, err := draw(t, strings.Replace(count, "%s", `{"step":2,"call":1},{"step":4,"call":1},{"step":6,"call":1}`, 1), first, second, last)
		if err != nil || got != `[{"count":7}]` {
			t.Errorf("a chain over every page drew %s, %v; want the count of all 7", got, err)
		}
	})

	t.Run("two reads not read to their last page are refused for the same read every time", func(t *testing.T) {
		demo, ops := issuePage(t, 2, 0, 3), issuePage(t, 4, 0, 3)
		ops.Arguments = json.RawMessage(`{"project":"OPS","limit":3,"offset":0}`)
		const spec = `{"kind":"metric","title":"Issues","steps":[{"step":4,"call":1},{"step":2,"call":1}],"chain":[{"op":"count"}],"field":"count"}`
		for range 20 {
			if _, err := draw(t, spec, demo, ops); err == nil || !strings.Contains(err.Error(), "step 2 read up to row 3 of the 7 rows") {
				t.Fatalf("a chain over the first pages of two reads was answered %v, want it refused for step 2's, the read of DEMO, whose arguments sort first", err)
			}
		}
	})
}

// runChain runs chain, written as a block spec writes it, over rows, a JSON list of objects read as
// a saved result is, and answers the rows it leaves as JSON, or its refusal.
func runChain(t *testing.T, chain, rows string) (string, error) {
	t.Helper()
	var steps going.Chain
	if err := json.Unmarshal([]byte(chain), &steps); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(rows))
	decoder.UseNumber()
	var table []map[string]any
	if err := decoder.Decode(&table); err != nil {
		t.Fatal(err)
	}
	answer, err := steps.Run(table)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded), nil
}

// TestAGroupHoldsEachRowOnce: a row is counted once in each group it belongs to, though its list
// holds a value twice or two times of one week; a row whose list is empty is in the group none,
// with a period as without one, so no count drops it; and a refusal that names a value cuts the
// value where a character starts.
func TestAGroupHoldsEachRowOnce(t *testing.T) {
	for _, row := range []struct{ name, chain, rows, want string }{
		{
			name:  "a value a list holds twice",
			chain: `[{"op":"group","field":"labels"},{"op":"count"}]`,
			rows:  `[{"key":"A","labels":["bug","bug"]},{"key":"B","labels":["bug","ui"]},{"key":"C","labels":[]}]`,
			want:  `[{"count":2,"labels":"bug"},{"count":1,"labels":"ui"},{"count":1,"labels":"none"}]`,
		},
		{
			name:  "two times of one week, and an empty list, by week",
			chain: `[{"op":"group","field":"closed","per":"week"},{"op":"count"}]`,
			rows:  `[{"key":"A","closed":["2026-11-02T10:00:00Z","2026-11-03T10:00:00Z"]},{"key":"B","closed":[]},{"key":"C","closed":"2026-11-10T10:00:00Z"}]`,
			want:  `[{"count":1,"week":"2026-11-02"},{"count":1,"week":"none"},{"count":1,"week":"2026-11-09"}]`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := runChain(t, row.chain, row.rows)
			if err != nil || got != row.want {
				t.Errorf("the chain answered %s, %v; want %s", got, err, row.want)
			}
		})
	}

	t.Run("a refusal cuts the value it names where a character starts", func(t *testing.T) {
		// {"name":" is 9 bytes and each é 2, so the 80th byte falls inside an é.
		_, err := runChain(t, `[{"op":"group","field":"owner"},{"op":"count"}]`, `[{"owner":{"name":"`+strings.Repeat("é", 60)+`"}}]`)
		if err == nil || !strings.Contains(err.Error(), "which no group is made of") || !utf8.ValidString(err.Error()) {
			t.Fatalf("a group by an object was answered %q, want it refused in valid UTF-8", err)
		}
	})
}

// TestARunsMetricKeepsTheLabelsItsSpecWrites: a metric of a run draws the label its spec writes for
// its field and the one it writes for the field it is compared with, and each is the field's own
// name where the spec writes none.
func TestARunsMetricKeepsTheLabelsItsSpecWrites(t *testing.T) {
	const spec = `{"kind":"metric","title":"Estimates","steps":[{"step":2,"call":1}],` +
		`"chain":[{"op":"sum","field":"estimate","as":"total"},{"op":"max","field":"estimate","as":"largest"}],"field":"total","compare":"largest"%s}`
	for _, row := range []struct{ name, labels, label, compareLabel string }{
		{"written by the spec", `,"label":"Points","compare_label":"Largest issue"`, "Points", "Largest issue"},
		{"written by none", "", "total", "largest"},
	} {
		t.Run(row.name, func(t *testing.T) {
			block, err := going.ParseRunBlock(json.RawMessage(fmt.Sprintf(spec, row.labels)))
			if err != nil {
				t.Fatalf("the spec was refused: %v", err)
			}
			data, err := block.Draw([]going.Result{issuePage(t, 2, 0, 50)})
			if err != nil {
				t.Fatalf("the block was refused: %v", err)
			}
			var drawn struct {
				Label        string `json:"label"`
				CompareLabel string `json:"compare_label"`
			}
			if err := json.Unmarshal(data.Spec, &drawn); err != nil {
				t.Fatal(err)
			}
			if drawn.Label != row.label || drawn.CompareLabel != row.compareLabel {
				t.Errorf("the metric draws the labels %q and %q, want %q and %q", drawn.Label, drawn.CompareLabel, row.label, row.compareLabel)
			}
		})
	}
}

// TestASpecOfAnotherShapeIsToldDrawBlocksArguments: a block spec the model wrote in a shape of its
// own - its results under data, or a chart's kind under chart, as two runs of the eval wrote them
// (chart-08 of run 3) - is refused for the field draw_block does not take, not for the step and
// call numbers under it, and the refusal names every argument draw_block takes, so the model can
// write the spec again in one call. A number of the model's own under such a field is still
// refused as a number.
func TestASpecOfAnotherShapeIsToldDrawBlocksArguments(t *testing.T) {
	for name, row := range map[string]struct{ spec, field string }{
		"its results under data": {field: `unknown field "data"`,
			spec: `{"data":[{"step":7,"call":1,"group":["priority"],"count":true},{"step":11,"call":1,"group":["priority"],"count":true}],"title":"Open issues by priority","x":"priority"}`},
		"its kind under chart": {field: `unknown field "chart"`,
			spec: `{"chart":"bar","title":"Open issues by priority","series":[{"name":"DEMO","source":"step 8, call 1","group_by":["priority"],"count":true}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := going.ParseRunBlock(json.RawMessage(row.spec))
			if err == nil || !strings.Contains(err.Error(), row.field) || !strings.Contains(err.Error(), "draw_block takes kind (chart, table or metric), title, steps") {
				t.Errorf("the spec was answered %v, want it refused for %s, naming draw_block's arguments", err, row.field)
			}
		})
	}
	_, err := going.ParseRunBlock(json.RawMessage(`{"kind":"chart","title":"Issues","data":[{"step":2,"call":1,"count":42}],"steps":[{"step":2,"call":1}],"chain":[{"op":"count"}],"x":"count","y":"count"}`))
	if err == nil || !strings.Contains(err.Error(), "holds no number") {
		t.Errorf("a spec with a count of its own under data was answered %v, want it refused for holding a number", err)
	}
}

// TestAGroupAfterACountSaysToGroupFirst: a count before the chain's group reads every row as one
// group, so the group after it is refused; the refusal says to put the group first, which is the
// chain the model meant (chart-06 of the eval's run 3 wrote filter, count, group and, told only
// that a chain groups once, gave up the grouping). The chain in that order draws one row a group.
func TestAGroupAfterACountSaysToGroupFirst(t *testing.T) {
	const rows = `[{"key":"A","labels":["bug","ui"]},{"key":"B","labels":["bug"]}]`
	_, err := runChain(t, `[{"op":"count","as":"issues"},{"op":"group","field":"labels"}]`, rows)
	if err == nil || !strings.Contains(err.Error(), "put the group before the count") {
		t.Fatalf("a group after a count was answered %v, want it refused, saying to put the group before the count", err)
	}
	got, err := runChain(t, `[{"op":"group","field":"labels"},{"op":"count","as":"issues"}]`, rows)
	if want := `[{"issues":2,"labels":"bug"},{"issues":1,"labels":"ui"}]`; err != nil || got != want {
		t.Errorf("the chain grouped first answered %s, %v; want %s", got, err, want)
	}
}
