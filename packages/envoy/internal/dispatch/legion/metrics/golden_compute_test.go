package metrics

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestComputeAnswersWhatItAnsweredAtTheBaseHead: with the per-issue parts split out of the window
// aggregation (barPartsOf, reworkOf), Compute still answers, number for number, every report the
// base head answered over the same history: each window of testdata/golden_compute in each of
// the three scopes, which golden_record_test.go recorded at that head and nothing records again.
// computed_at, the wall clock of the computation, is the one field left out, as the recorder
// left it out.
func TestComputeAnswersWhatItAnsweredAtTheBaseHead(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "golden_compute", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list testdata/golden_compute: %d files, %v", len(files), err)
	}
	type recorded struct {
		Head string `json:"head"`
		Spec struct {
			Project string    `json:"project"`
			Trees   int       `json:"trees"`
			Now     time.Time `json:"now"`
		} `json:"spec"`
		Window struct {
			From time.Time `json:"from"`
			To   time.Time `json:"to"`
		} `json:"window"`
		Scopes  []Scope                    `json:"scopes"`
		Reports map[string]json.RawMessage `json:"reports"`
	}
	windows := make([]recorded, 0, len(files))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var window recorded
		if err := json.Unmarshal(raw, &window); err != nil {
			t.Fatalf("decode %s: %v", file, err)
		}
		if len(windows) > 0 && window.Spec != windows[0].Spec {
			t.Fatalf("%s was recorded over %+v and %s over %+v, want one history", file, window.Spec, files[0], windows[0].Spec)
		}
		windows = append(windows, window)
	}
	spec := windows[0].Spec

	database := storetest.Open(t)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: spec.Project, Trees: spec.Trees, Now: spec.Now})
	reports, numbers, finished := 0, 0, 0
	for index, window := range windows {
		for _, scope := range window.Scopes {
			population, err := Read(t.Context(), database.Pool, nil, fixture.Project, window.Window.From, window.Window.To)
			if err != nil {
				t.Fatalf("read %s to %s: %v", window.Window.From, window.Window.To, err)
			}
			report := Compute(population, scope, time.Now().UTC())
			got, want := goldenJSON(t, report, "computed_at"), goldenJSON(t, window.Reports[string(scope)])
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s, scope %s: Compute answers\n%s\nwant the base head's\n%s",
					files[index], scope, encodeGolden(t, got), encodeGolden(t, want))
			}
			reports++
			numbers += countNumbers(want)
			finished += report.Finished
		}
	}
	t.Logf("history %s, %d trees, as of %s (recorded at %s): %d windows, %d reports, %d numbers compared, %d finished issues counted across them",
		spec.Project, spec.Trees, spec.Now.Format(time.RFC3339), windows[0].Head, len(windows), reports, numbers, finished)
}

// goldenJSON is value as the JSON tree its encoding decodes to, numbers kept as the text
// encoding/json wrote, with each named top-level key left out.
func goldenJSON(t *testing.T, value any, strip ...string) any {
	t.Helper()
	encoded, ok := value.(json.RawMessage)
	if !ok {
		var err error
		if encoded, err = json.Marshal(value); err != nil {
			t.Fatalf("encode %T: %v", value, err)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		t.Fatalf("decode %T: %v", value, err)
	}
	if object, isObject := tree.(map[string]any); isObject {
		for key := range object {
			if slices.Contains(strip, key) {
				delete(object, key)
			}
		}
	}
	return tree
}

// encodeGolden is a decoded tree as one line of JSON, for a failure message.
func encodeGolden(t *testing.T, tree any) string {
	t.Helper()
	encoded, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("encode a report: %v", err)
	}
	return string(encoded)
}

// countNumbers is how many numbers a decoded tree holds.
func countNumbers(tree any) int {
	switch node := tree.(type) {
	case json.Number:
		return 1
	case map[string]any:
		count := 0
		for _, child := range node {
			count += countNumbers(child)
		}
		return count
	case []any:
		count := 0
		for _, child := range node {
			count += countNumbers(child)
		}
		return count
	}
	return 0
}
