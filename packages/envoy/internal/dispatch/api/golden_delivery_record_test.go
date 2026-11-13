//go:build goldenrecord

// golden_delivery_record_test.go records what GET /api/v1/delivery/measures and
// GET /api/v1/delivery/pipeline answer at the base head 02a7b3c6 over one deliverytest history,
// so the day parts and folds that replace measures.Compute and the pipeline functions (T7 of
// dispatch://INFRA-2916's plan) are compared with the old code's answer rather than with their
// own. It wrote testdata/golden_delivery/fixture.json (the history's spec and the ordered window
// list) and testdata/golden_delivery/<from>_<to>.json (one file per window, holding its measures
// answers with no facet and for each repository, and its pipeline answer) when it ran at 02a7b3c6,
// with this command from packages/envoy:
//
//	DISPATCH_TEST_DATABASE_URL=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
//		go test -count=1 -tags goldenrecord -run TestRecordDeliveryGolden ./internal/dispatch/api/
//
// Run it only at that head: once T7 moves the two routes onto the folds, it records the new code's
// answer and the file stops being the oracle. The goldenrecord tag keeps it out of every other
// test run. It is deleted together with the two routes it calls, which INFRA-2918 deletes.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
)

const (
	// goldenDeliveryHead is the commit the golden file was recorded at.
	goldenDeliveryHead = "02a7b3c6"
	// goldenDeliveryWindows is how many windows are recorded, and goldenDeliveryWindowSeed the
	// seed of the math/rand/v2 PCG source that picks them: counting from 1, the first seed whose
	// 20 draws hold a window of one day, a window ending at the history's end and a window of 80
	// days or more within 800 days in all (it draws 742).
	goldenDeliveryWindows    = 20
	goldenDeliveryWindowSeed = 18
	// goldenDeliveryMaxDays is the widest window the delivery routes read (maxDeliveryWindow).
	goldenDeliveryMaxDays = 84
)

// goldenDeliveryDir is the directory under testdata the answers are written to, one file per
// window named by its two days, so each file stays small enough to review and to snapshot.
const goldenDeliveryDir = "golden_delivery"

// goldenDeliverySpec is the history the golden files are recorded over, every field named so the
// files do not move with deliverytest's defaults.
var goldenDeliverySpec = deliverytest.Spec{Project: "DELIV", Now: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Days: 120, Seed: 2916}

// goldenDeliveryStripped are the top-level fields of both answers that are left out of the file,
// because each depends on the wall clock rather than on the history and the window:
// computed_at is the moment the answer was computed, and freshness carries last_event_at and
// last_reconcile_at, which intake and the reconcile stamp with the database's clock (and
// last_error and unfetchable_count beside them, which the history does not set).
var goldenDeliveryStripped = []string{"computed_at", "freshness"}

// goldenDeliveryWindow is one recorded window: its two ends, each a UTC midnight, its length, and
// the file under goldenDeliveryDir that holds its answers.
type goldenDeliveryWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Days int       `json:"days"`
	File string    `json:"file"`
}

// TestRecordDeliveryGolden seeds the golden history and records, for each of the windows,
// GET /api/v1/delivery/measures with no facet and with repo= each of the three repositories, and
// GET /api/v1/delivery/pipeline with no facet, each answer with the stripped fields left out.
func TestRecordDeliveryGolden(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	fixture := deliverytest.Seed(t, database.Pool, goldenDeliverySpec)

	picker := rand.New(rand.NewPCG(goldenDeliveryWindowSeed, 0))
	windows := make([]goldenDeliveryWindow, goldenDeliveryWindows)
	for index := range windows {
		days := 1 + picker.IntN(goldenDeliveryMaxDays)
		from := fixture.From.AddDate(0, 0, picker.IntN(fixture.Spec.Days-days+1))
		to := from.AddDate(0, 0, days)
		windows[index] = goldenDeliveryWindow{From: from, To: to, Days: days, File: from.Format(time.DateOnly) + "_" + to.Format(time.DateOnly) + ".json"}
		t.Logf("window %2d: %s to %s, length in days %d", index+1, from.Format(time.RFC3339), windows[index].To.Format(time.RFC3339), days)
	}

	// The window files and fixture.json are the whole of this directory, so it is emptied first:
	// a window the seed no longer draws leaves no file behind.
	dir := filepath.Join("testdata", goldenDeliveryDir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("empty %s: %v", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make %s: %v", dir, err)
	}
	write := func(name string, text []byte) {
		t.Helper()
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s is written twice: two windows share its name", path)
		}
		if err := os.WriteFile(path, text, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s: %d bytes", path, len(text))
	}

	requests, numbers := 0, 0
	for _, window := range windows {
		query := url.Values{"from": {window.From.Format(time.RFC3339)}, "to": {window.To.Format(time.RFC3339)}}
		targets := []string{"/api/v1/delivery/measures?" + query.Encode()}
		for _, repo := range fixture.Repos {
			faceted := url.Values{"from": query["from"], "to": query["to"], "repo": {repo}}
			targets = append(targets, "/api/v1/delivery/measures?"+faceted.Encode())
		}
		targets = append(targets, "/api/v1/delivery/pipeline?"+query.Encode())
		answers := map[string]json.RawMessage{}
		for _, target := range targets {
			response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, body = %s", target, response.Code, response.Body.String())
			}
			body, count := goldenDeliveryAnswer(t, response.Body.Bytes())
			answers[target] = body
			requests, numbers = requests+1, numbers+count
		}
		write(window.File, goldenDeliveryFile(t, map[string]any{"head": goldenDeliveryHead, "window": window}, "answers", answers))
	}
	write("fixture.json", goldenDeliveryFile(t, map[string]any{
		"head": goldenDeliveryHead,
		"fixture": map[string]any{
			"spec": fixture.Spec, "repos": fixture.Repos, "deploy_repo": fixture.DeployRepo, "summary": fixture.Summary(),
		},
		"window_seed": goldenDeliveryWindowSeed,
		"windows":     windows,
		"stripped":    goldenDeliveryStripped,
	}, "", nil))
	t.Logf("recorded %d windows, %d requests holding %d numbers", len(windows), requests, numbers)
}

// goldenDeliveryAnswer is one answer as the file holds it: decoded with every number kept as the
// route wrote it (json.Number), the stripped fields removed, and encoded again on one line with
// its keys sorted; and the count of numbers it holds.
func goldenDeliveryAnswer(t *testing.T, body []byte) (json.RawMessage, int) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var answer map[string]any
	if err := decoder.Decode(&answer); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	for _, field := range goldenDeliveryStripped {
		if _, held := answer[field]; !held {
			t.Fatalf("the answer holds no %s to strip: %s", field, body)
		}
		delete(answer, field)
	}
	return goldenDeliveryEncode(t, answer, ""), goldenDeliveryNumbers(answer)
}

// goldenDeliveryNumbers counts the numbers in a decoded JSON tree.
func goldenDeliveryNumbers(tree any) int {
	switch node := tree.(type) {
	case json.Number:
		return 1
	case map[string]any:
		count := 0
		for _, child := range node {
			count += goldenDeliveryNumbers(child)
		}
		return count
	case []any:
		count := 0
		for _, child := range node {
			count += goldenDeliveryNumbers(child)
		}
		return count
	}
	return 0
}

// goldenDeliveryEncode is value's JSON with nothing HTML-escaped (the request targets carry `&`),
// indented under prefix when prefix is not empty. A value that is not already a decoded tree is
// put through one first, so every object's keys come out sorted whatever order a struct declares
// its fields in.
func goldenDeliveryEncode(t *testing.T, value any, prefix string) []byte {
	t.Helper()
	encode := func(value any, prefix string) []byte {
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		if prefix != "" {
			encoder.SetIndent(prefix, "  ")
		}
		if err := encoder.Encode(value); err != nil {
			t.Fatalf("encode the golden file: %v", err)
		}
		return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	}
	decoder := json.NewDecoder(bytes.NewReader(encode(value, "")))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		t.Fatalf("decode the golden file: %v", err)
	}
	return encode(tree, prefix)
}

// goldenDeliveryFile is a file's text: one object whose keys are in order, each field of fields
// indented, and, unless linesKey is empty, under linesKey one line per entry of lines, so a file
// of thousands of numbers stays small and a diff of it names the answer that moved.
func goldenDeliveryFile(t *testing.T, fields map[string]any, linesKey string, lines map[string]json.RawMessage) []byte {
	t.Helper()
	keys := slices.Sorted(maps.Keys(fields))
	if linesKey != "" {
		keys = append(keys, linesKey)
		slices.Sort(keys)
	}
	var out bytes.Buffer
	out.WriteString("{\n")
	for index, key := range keys {
		fmt.Fprintf(&out, "  %s: ", goldenDeliveryEncode(t, key, ""))
		if key == linesKey {
			out.WriteString("{\n")
			entries := slices.Sorted(maps.Keys(lines))
			for at, entry := range entries {
				fmt.Fprintf(&out, "    %s: %s", goldenDeliveryEncode(t, entry, ""), lines[entry])
				if at < len(entries)-1 {
					out.WriteString(",")
				}
				out.WriteString("\n")
			}
			out.WriteString("  }")
		} else {
			out.Write(goldenDeliveryEncode(t, fields[key], "  "))
		}
		if index < len(keys)-1 {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	out.WriteString("}\n")
	return out.Bytes()
}
