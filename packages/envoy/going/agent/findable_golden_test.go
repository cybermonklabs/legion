package agent

// findable_golden_test.go holds every tool find_tools answers from, as the bar's model is given it,
// to a committed golden file (findableGolden), so a tool the model can find that a change adds,
// drops or rewords is a difference a reviewer reads. It uses findableTools and the standard library
// alone, so this same file, dropped into another tree, writes that tree's golden to diff against
// this one. GOING_FINDABLE_UPDATE=1 writes the golden instead of comparing:
//
//	cd packages/envoy && GOING_FINDABLE_UPDATE=1 go test -count=1 -run '^TestFindableToolsMatchTheGolden$' ./going/agent

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// findableGolden is the golden file of every findable tool, relative to this package.
const findableGolden = "testdata/findable_tools.json"

// findableEntry is one findable tool as the model is given it: its name, its whole description
// and its arguments' schema.
type findableEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Arguments   json.RawMessage `json:"arguments"`
}

// findableDump is every findable tool (findableTools), sorted by name, then by description, as
// indented JSON ending in a newline. dispatch_read is findable twice, as its own spec and as the
// form that lists an issue's comments, which its description tells apart.
func findableDump(t *testing.T) ([]findableEntry, []byte) {
	t.Helper()
	tools := findableTools()
	entries := make([]findableEntry, 0, len(tools))
	for _, tool := range tools {
		entries = append(entries, findableEntry{Name: tool.name, Description: tool.description, Arguments: tool.schema})
	}
	slices.SortStableFunc(entries, func(a, b findableEntry) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Description, b.Description))
	})
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(entries); err != nil {
		t.Fatalf("encode the findable tools: %v", err)
	}
	return entries, out.Bytes()
}

// findableByName is each entry's compact JSON, by its name.
func findableByName(t *testing.T, entries []findableEntry) map[string][]string {
	t.Helper()
	byName := make(map[string][]string, len(entries))
	for _, entry := range entries {
		var compact bytes.Buffer
		if err := json.Compact(&compact, entry.Arguments); err != nil {
			t.Fatalf("%s's arguments are no JSON: %v", entry.Name, err)
		}
		encoded, err := json.Marshal(findableEntry{Name: entry.Name, Description: entry.Description, Arguments: compact.Bytes()})
		if err != nil {
			t.Fatalf("encode %s: %v", entry.Name, err)
		}
		byName[entry.Name] = append(byName[entry.Name], string(encoded))
	}
	return byName
}

// TestFindableToolsMatchTheGolden: every tool find_tools answers from - name, description and the
// arguments' schema - is the golden file's, and a difference names each tool that differs: one
// the golden holds and find_tools does not, one find_tools holds and the golden does not, and one
// both hold as different specs.
func TestFindableToolsMatchTheGolden(t *testing.T) {
	entries, dump := findableDump(t)
	if os.Getenv("GOING_FINDABLE_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(findableGolden), 0o755); err != nil {
			t.Fatalf("make %s's directory: %v", findableGolden, err)
		}
		if err := os.WriteFile(findableGolden, dump, 0o644); err != nil {
			t.Fatalf("write %s: %v", findableGolden, err)
		}
		t.Logf("wrote %d findable tools to %s", len(entries), findableGolden)
		return
	}
	golden, err := os.ReadFile(findableGolden)
	if err != nil {
		t.Fatalf("read %s: %v (GOING_FINDABLE_UPDATE=1 writes it)", findableGolden, err)
	}
	if bytes.Equal(golden, dump) {
		return
	}
	var held []findableEntry
	if err := json.Unmarshal(golden, &held); err != nil {
		t.Fatalf("%s holds no list of tools: %v", findableGolden, err)
	}
	was, now := findableByName(t, held), findableByName(t, entries)
	names := make([]string, 0, len(was)+len(now))
	for name := range was {
		names = append(names, name)
	}
	for name := range now {
		names = append(names, name)
	}
	slices.Sort(names)
	var differ []string
	for _, name := range slices.Compact(names) {
		before, after := was[name], now[name]
		switch {
		case slices.Equal(before, after):
		case len(after) == 0:
			differ = append(differ, name+": the golden holds it, and find_tools does not")
		case len(before) == 0:
			differ = append(differ, name+": find_tools holds it, and the golden does not")
		default:
			differ = append(differ, fmt.Sprintf("%s: find_tools holds %d specs of it, the golden %d, and they differ", name, len(after), len(before)))
		}
	}
	if len(differ) == 0 {
		differ = append(differ, "no tool: the file's bytes differ only in their layout")
	}
	t.Errorf("find_tools answers from %d tools, %s from %d; they differ in:\n%s\nGOING_FINDABLE_UPDATE=1 writes the golden.",
		len(entries), findableGolden, len(held), strings.Join(differ, "\n"))
}
