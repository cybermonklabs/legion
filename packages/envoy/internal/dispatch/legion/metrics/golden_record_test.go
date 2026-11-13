//go:build goldenrecord

// golden_record_test.go records what the Legion metrics computation answers at the base head
// 02a7b3c6 over one legiontest history, so the code that replaces it (T5's per-issue parts and
// T8's six folds in dispatch://INFRA-2916's plan) is compared with the old code's answer rather
// than with its own. It wrote testdata/golden_compute/<from>_<to>.json, one file per window
// holding the three scopes' reports, and testdata/golden_issue_parts.json when it ran at
// 02a7b3c6, with this command from packages/envoy:
//
//	DISPATCH_TEST_DATABASE_URL=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
//		go test -count=1 -tags goldenrecord -run TestRecordLegionGolden ./internal/dispatch/legion/metrics/
//
// Run it only at that head: once T5 and T8 change compute.go, it records the new code's answer and
// the files stop being the oracle. The goldenrecord tag keeps it out of every other test run. It is
// deleted together with metrics.Compute, the code it calls (T13 of the same plan).
package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// goldenHead is the commit the golden files were recorded at.
const goldenHead = "02a7b3c6"

// goldenComputeDir is the directory under testdata the reports are written to, one file per
// window named by its two days, so each file stays small enough to review and to snapshot.
const goldenComputeDir = "golden_compute"

// goldenSpec is the history the golden files are recorded over: 60 trees, which is 240 issues
// that finished with Legion phase work (four of each tree's six), from 2026-09-26 to 2026-10-01.
var goldenSpec = legiontest.Spec{Project: "GOLDEN", Trees: 60, Now: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}

// goldenDay is midnight UTC of one day of 2026.
func goldenDay(month time.Month, day int) time.Time {
	return time.Date(2026, month, day, 0, 0, 0, 0, time.UTC)
}

// goldenWindows are the ten windows the reports are recorded for, every one starting and ending at
// a UTC midnight. The history runs from 2026-09-26 to 2026-10-01 and its first close is on
// 2026-09-27, so the first day finishes no issue. The first window is the whole history, which
// the per-issue parts are read over.
var goldenWindows = []struct {
	Name     string
	From, To time.Time
}{
	{"the whole history", goldenDay(time.September, 26), goldenDay(time.October, 1)},
	{"its first day, before any issue closed", goldenDay(time.September, 26), goldenDay(time.September, 27)},
	{"its second day", goldenDay(time.September, 27), goldenDay(time.September, 28)},
	{"its third day", goldenDay(time.September, 28), goldenDay(time.September, 29)},
	{"its fourth day", goldenDay(time.September, 29), goldenDay(time.September, 30)},
	{"its last day", goldenDay(time.September, 30), goldenDay(time.October, 1)},
	{"from its start to mid-history", goldenDay(time.September, 26), goldenDay(time.September, 29)},
	{"from mid-history to its end", goldenDay(time.September, 28), goldenDay(time.October, 1)},
	{"starting and ending mid-history", goldenDay(time.September, 27), goldenDay(time.September, 30)},
	{"from a week before it to mid-history", goldenDay(time.September, 19), goldenDay(time.September, 28)},
}

// goldenScopes are the three scopes every window is recorded under.
var goldenScopes = []Scope{ScopeAll, ScopeRoots, ScopeChildren}

// goldenWindowKey names a window in the files: its two ends, RFC3339, joined by a slash.
func goldenWindowKey(from, to time.Time) string {
	return from.Format(time.RFC3339) + "/" + to.Format(time.RFC3339)
}

// goldenIssueParts is one finished issue's parts, each the answer of the function compute.go's
// window aggregation takes it from: its work-time row (workTimeOf), its seven role bars with
// their five segments (phaseTime over the issue alone), its rework row (rework over the issue
// alone), the children splitting counts for it, and its tree root (treeRoot).
type goldenIssueParts struct {
	WorkTime *WorkTimeIssue `json:"work_time"`
	Bars     []RoleBar      `json:"bars"`
	Rework   ReworkIssue    `json:"rework"`
	Children int            `json:"children"`
	TreeRoot string         `json:"tree_root"`
	Root     bool           `json:"root"`
}

// goldenPartsOf is the issue's parts, computed by c.
func (c *computation) goldenPartsOf(issue *Issue) goldenIssueParts {
	parts := goldenIssueParts{
		Bars:     c.phaseTime([]*Issue{issue}).Bars,
		Rework:   c.rework([]*Issue{issue}).Issues[0],
		Children: len(issue.Children),
		TreeRoot: c.treeRoot(issue.Key),
		Root:     c.isRoot(issue.Key),
	}
	if row, ok := c.workTimeOf(issue); ok {
		parts.WorkTime = &row
	}
	return parts
}

// newGoldenComputation is a computation over population, built as Compute builds its own.
func newGoldenComputation(population *Population) *computation {
	return &computation{
		population: population,
		scope:      ScopeAll,
		phases:     map[string]phaseTimeline{},
		views:      map[string]*issueView{},
		roots:      map[string]string{},
		workTimes:  map[string]workTimeRow{},
		barSpans:   map[string][]Span{},
	}
}

// TestRecordLegionGolden seeds the golden history, makes for each window and scope the read and
// the computation GET /api/v1/legion/metrics makes (api/legion_metrics.go: metrics.Read with no
// GitHub client, as the route's test handler has none, and metrics.Compute), and records each
// Report, and records every finished issue's parts over the whole history.
func TestRecordLegionGolden(t *testing.T) {
	database := storetest.Open(t)
	fixture := legiontest.Seed(t, database.Pool, goldenSpec)
	ctx := t.Context()

	spec := map[string]any{"project": goldenSpec.Project, "trees": goldenSpec.Trees, "now": goldenSpec.Now.Format(time.RFC3339)}
	// The window files are the whole of this directory, so it is emptied first: a window taken
	// out of goldenWindows leaves no file behind.
	if err := os.RemoveAll(filepath.Join("testdata", goldenComputeDir)); err != nil {
		t.Fatalf("empty testdata/%s: %v", goldenComputeDir, err)
	}
	reportNumbers, written := 0, map[string]bool{}
	for _, window := range goldenWindows {
		from, to := window.From.UTC(), window.To.UTC()
		reports := map[string]json.RawMessage{}
		for _, scope := range goldenScopes {
			population, err := Read(ctx, database.Pool, nil, fixture.Project, from, to)
			if err != nil {
				t.Fatalf("read %s: %v", goldenWindowKey(from, to), err)
			}
			// computed_at is the wall clock of the computation, the one field of a Report that
			// is not a function of the history and the window, so it is left out of the file.
			tree, numbers := goldenTree(t, Compute(population, scope, time.Now().UTC()), "computed_at")
			reports[string(scope)] = goldenEncode(t, tree, "")
			reportNumbers += numbers
		}
		name := filepath.Join(goldenComputeDir, from.Format(time.DateOnly)+"_"+to.Format(time.DateOnly)+".json")
		if written[name] {
			t.Fatalf("two windows are named %s", name)
		}
		written[name] = true
		goldenWrite(t, name, map[string]any{
			"head": goldenHead, "spec": spec, "scopes": goldenScopes, "stripped": []string{"computed_at"},
			"window": map[string]string{"name": window.Name, "from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		}, "reports", reports)
	}

	// The per-issue parts are read over the whole history, the first window, and the same parts
	// are computed again inside every window that finishes the issue, which says whether one
	// issue's parts move with the window they are computed in.
	history := goldenWindows[0]
	population, err := Read(ctx, database.Pool, nil, fixture.Project, history.From, history.To)
	if err != nil {
		t.Fatalf("read the whole history: %v", err)
	}
	whole := newGoldenComputation(population)
	parts := map[string]json.RawMessage{}
	wholeParts := map[string]goldenIssueParts{}
	partNumbers := 0
	for _, key := range population.Keys {
		issue := population.Issues[key]
		if !whole.finishedInWindow(issue) {
			continue
		}
		wholeParts[key] = whole.goldenPartsOf(issue)
		tree, numbers := goldenTree(t, wholeParts[key])
		parts[key] = goldenCompact(t, tree)
		partNumbers += numbers
	}
	if len(parts) < 200 {
		t.Fatalf("the history finished %d issues with Legion phase work, want at least the 200 the folds' golden check counts", len(parts))
	}
	compared, moved := 0, []string{}
	for _, window := range goldenWindows[1:] {
		windowed, err := Read(ctx, database.Pool, nil, fixture.Project, window.From, window.To)
		if err != nil {
			t.Fatalf("read %s: %v", goldenWindowKey(window.From, window.To), err)
		}
		c := newGoldenComputation(windowed)
		for _, key := range windowed.Keys {
			issue := windowed.Issues[key]
			if !c.finishedInWindow(issue) {
				continue
			}
			compared++
			if got, want := goldenCompact(t, c.goldenPartsOf(issue)), goldenCompact(t, wholeParts[key]); !bytes.Equal(got, want) {
				moved = append(moved, key+" in "+goldenWindowKey(window.From, window.To))
			}
		}
	}

	goldenWrite(t, "golden_issue_parts.json", map[string]any{
		"head": goldenHead, "spec": spec,
		"population_window": map[string]string{"from": history.From.Format(time.RFC3339), "to": history.To.Format(time.RFC3339)},
	}, "issues", parts)

	t.Logf("recorded %d windows x %d scopes = %d reports holding %d numbers, and the parts of %d finished issues holding %d numbers",
		len(goldenWindows), len(goldenScopes), len(goldenWindows)*len(goldenScopes), reportNumbers, len(parts), partNumbers)
	t.Logf("each issue's parts computed again in each window that finishes it: %d compared, %d differ from the whole history's%s",
		compared, len(moved), goldenFirst(moved))
}

// goldenTree is value as the JSON tree its encoding decodes to, with each named top-level key
// removed, and the count of numbers the tree holds. Numbers decode as json.Number, so the file
// carries each one exactly as encoding/json formatted it, and maps encode in key order, so the
// file's keys are sorted whatever order a struct declares its fields in.
func goldenTree(t *testing.T, value any, strip ...string) (any, int) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T: %v", value, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		t.Fatalf("decode %T: %v", value, err)
	}
	if object, ok := tree.(map[string]any); ok {
		for _, key := range strip {
			delete(object, key)
		}
	}
	return tree, goldenNumbers(tree)
}

// goldenNumbers counts the numbers in a decoded JSON tree.
func goldenNumbers(tree any) int {
	switch node := tree.(type) {
	case json.Number:
		return 1
	case map[string]any:
		count := 0
		for _, child := range node {
			count += goldenNumbers(child)
		}
		return count
	case []any:
		count := 0
		for _, child := range node {
			count += goldenNumbers(child)
		}
		return count
	}
	return 0
}

// goldenCompact is value's JSON on one line, keys sorted and nothing HTML-escaped.
func goldenCompact(t *testing.T, value any) json.RawMessage {
	t.Helper()
	tree, _ := goldenTree(t, value)
	return goldenEncode(t, tree, "")
}

// goldenEncode is tree's JSON, indented under prefix when prefix is not empty.
func goldenEncode(t *testing.T, tree any, prefix string) []byte {
	t.Helper()
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if prefix != "" {
		encoder.SetIndent(prefix, "  ")
	}
	if err := encoder.Encode(tree); err != nil {
		t.Fatalf("encode the golden file: %v", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// goldenWrite writes testdata/<name>: one object whose keys are in order, each field of fields
// indented, and under linesKey one line per entry of lines, so a file of thousands of values stays
// small and a diff of it names the entry that moved.
func goldenWrite(t *testing.T, name string, fields map[string]any, linesKey string, lines map[string]json.RawMessage) {
	t.Helper()
	keys := slices.Sorted(maps.Keys(fields))
	keys = append(keys, linesKey)
	slices.Sort(keys)
	var out bytes.Buffer
	out.WriteString("{\n")
	for index, key := range keys {
		fmt.Fprintf(&out, "  %s: ", goldenEncode(t, key, ""))
		if key == linesKey {
			out.WriteString("{\n")
			entries := slices.Sorted(maps.Keys(lines))
			for at, entry := range entries {
				fmt.Fprintf(&out, "    %s: %s", goldenEncode(t, entry, ""), lines[entry])
				if at < len(entries)-1 {
					out.WriteString(",")
				}
				out.WriteString("\n")
			}
			out.WriteString("  }")
		} else {
			tree, _ := goldenTree(t, fields[key])
			out.Write(goldenEncode(t, tree, "  "))
		}
		if index < len(keys)-1 {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	out.WriteString("}\n")
	path := filepath.Join("testdata", name)
	// This package had no testdata directory before these files.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("wrote %s: %d bytes", path, out.Len())
}

// goldenFirst names the first few of keys, for a log line.
func goldenFirst(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return " (" + strings.Join(keys[:min(len(keys), 5)], "; ") + ")"
}
