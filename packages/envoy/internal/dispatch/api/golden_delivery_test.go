package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
)

// deliveryOracleTolerance is how far apart, relative to the larger, two floats may be and still
// be the same answer: the folds sum and sort the same values the old code did, so only a change
// of summation order could move a float, and that moves it in its last bits.
const deliveryOracleTolerance = 1e-9

// deliveryOracleFixture is testdata/golden_delivery/fixture.json as this test reads it: the
// history the answers were recorded over, the windows recorded, and the top-level fields the
// recorder left out of every answer.
type deliveryOracleFixture struct {
	Head    string `json:"head"`
	Fixture struct {
		Spec       deliverytest.Spec `json:"spec"`
		Repos      []string          `json:"repos"`
		DeployRepo string            `json:"deploy_repo"`
		Summary    string            `json:"summary"`
	} `json:"fixture"`
	Windows []struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
		Days int       `json:"days"`
		File string    `json:"file"`
	} `json:"windows"`
	Stripped []string `json:"stripped"`
}

// TestTheDeliveryRoutesAnswerWhatTheyAnsweredAtTheBaseHead is the oracle of the day parts and
// folds that measures.Compute and the pipeline functions answer through (T7 of
// dispatch://INFRA-2916's plan): over the deliverytest history of
// testdata/golden_delivery/fixture.json, every request each window file holds (GET
// /api/v1/delivery/measures with no facet and with repo= each repository, and GET
// /api/v1/delivery/pipeline) answers, value for value, what the base head answered when
// golden_delivery_record_test.go recorded it, the recorder's stripped fields left out. Integers
// and strings must be equal; floats within deliveryOracleTolerance of each other, relative to the
// larger.
func TestTheDeliveryRoutesAnswerWhatTheyAnsweredAtTheBaseHead(t *testing.T) {
	dir := filepath.Join("testdata", "golden_delivery")
	var recorded deliveryOracleFixture
	readDeliveryOracleFile(t, filepath.Join(dir, "fixture.json"), &recorded)
	if len(recorded.Windows) == 0 || len(recorded.Stripped) == 0 {
		t.Fatalf("fixture.json holds %d windows and %d stripped fields, want both", len(recorded.Windows), len(recorded.Stripped))
	}

	handler, database := newTestHandlerWithStore(t)
	fixture := deliverytest.Seed(t, database.Pool, recorded.Fixture.Spec)
	// A history other than the recorded one would show as every measure moving; name it instead.
	if fixture.Summary() != recorded.Fixture.Summary || !slices.Equal(fixture.Repos, recorded.Fixture.Repos) {
		t.Fatalf("the seeded history is not the recorded one:\n seeded:   %s, repositories %v\n recorded: %s, repositories %v",
			fixture.Summary(), fixture.Repos, recorded.Fixture.Summary, recorded.Fixture.Repos)
	}

	requests, values, differing := 0, 0, 0
	for _, window := range recorded.Windows {
		var file struct {
			Head    string                     `json:"head"`
			Answers map[string]json.RawMessage `json:"answers"`
		}
		readDeliveryOracleFile(t, filepath.Join(dir, window.File), &file)
		if file.Head != recorded.Head || len(file.Answers) == 0 {
			t.Fatalf("%s: head %q and %d answers, want head %q and the window's answers", window.File, file.Head, len(file.Answers), recorded.Head)
		}
		for _, target := range slices.Sorted(maps.Keys(file.Answers)) {
			response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, body = %s", target, response.Code, response.Body.String())
			}
			got := decodeDeliveryOracleTree(t, response.Body.Bytes())
			for _, field := range recorded.Stripped {
				if _, held := got.(map[string]any)[field]; !held {
					t.Fatalf("GET %s answers no %s to strip", target, field)
				}
				delete(got.(map[string]any), field)
			}
			want := decodeDeliveryOracleTree(t, file.Answers[target])
			compared, difference := compareDeliveryOracleTrees("", got, want)
			requests, values = requests+1, values+compared
			if difference != "" {
				differing++
				t.Errorf("%s: GET %s differs from the base head's answer first at %s", window.File, target, difference)
			}
		}
	}
	t.Logf("history %s (seed %d, %d days to %s), recorded at %s: %d windows, %d requests, %d values compared, %d requests differing",
		recorded.Fixture.Spec.Project, recorded.Fixture.Spec.Seed, recorded.Fixture.Spec.Days, recorded.Fixture.Spec.Now.Format(time.DateOnly),
		recorded.Head, len(recorded.Windows), requests, values, differing)
}

func readDeliveryOracleFile(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// decodeDeliveryOracleTree is a JSON text as a tree, each number kept as the text it was
// written as, so an integer is compared as one.
func decodeDeliveryOracleTree(t *testing.T, text []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	return tree
}

// compareDeliveryOracleTrees walks got and want together, objects by sorted key and arrays by
// index, and answers how many leaf values it compared and, at the first that differs, its path
// from the root ($) with both values ("" when every value is the same). Two numbers written as
// integers must be equal; any other two numbers are floats, within deliveryOracleTolerance
// relative to the larger.
func compareDeliveryOracleTrees(path string, got, want any) (int, string) {
	differs := func(format string, args ...any) string {
		return fmt.Sprintf("$%s: "+format, append([]any{path}, args...)...)
	}
	switch want := want.(type) {
	case map[string]any:
		got, ok := got.(map[string]any)
		if !ok {
			return 0, differs("got %v, want an object", got)
		}
		if gotKeys, wantKeys := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(gotKeys, wantKeys) {
			return 0, differs("got the keys %v, want %v", gotKeys, wantKeys)
		}
		compared := 0
		for _, key := range slices.Sorted(maps.Keys(want)) {
			count, difference := compareDeliveryOracleTrees(path+"."+key, got[key], want[key])
			compared += count
			if difference != "" {
				return compared, difference
			}
		}
		return compared, ""
	case []any:
		got, ok := got.([]any)
		if !ok {
			return 0, differs("got %v, want an array", got)
		}
		if len(got) != len(want) {
			return 0, differs("got %d entries, want %d", len(got), len(want))
		}
		compared := 0
		for index := range want {
			count, difference := compareDeliveryOracleTrees(path+"["+strconv.Itoa(index)+"]", got[index], want[index])
			compared += count
			if difference != "" {
				return compared, difference
			}
		}
		return compared, ""
	case json.Number:
		got, ok := got.(json.Number)
		if !ok {
			return 1, differs("got %v, want the number %s", got, want)
		}
		gotInteger, gotErr := strconv.ParseInt(got.String(), 10, 64)
		wantInteger, wantErr := strconv.ParseInt(want.String(), 10, 64)
		if gotErr == nil && wantErr == nil {
			if gotInteger != wantInteger {
				return 1, differs("got %d, want %d", gotInteger, wantInteger)
			}
			return 1, ""
		}
		gotFloat, gotErr := got.Float64()
		wantFloat, wantErr := want.Float64()
		if gotErr != nil || wantErr != nil {
			return 1, differs("got %s, want %s, which do not both read as numbers", got, want)
		}
		if math.Abs(gotFloat-wantFloat) > deliveryOracleTolerance*math.Max(math.Abs(gotFloat), math.Abs(wantFloat)) {
			return 1, differs("got %s, want %s", got, want)
		}
		return 1, ""
	default:
		// A string, a boolean or null: equal or not.
		if got != want {
			return 1, differs("got %#v, want %#v", got, want)
		}
		return 1, ""
	}
}
