package redact

import (
	"encoding/json"
	"os"
	"testing"
)

// TestOneLineQuotesAsTheSharedTableSays reads testdata/oneline.json, which the TypeScript oneLine
// reads too (packages/contracts/src/remote-text-oneline.test.ts), so both languages print one
// text another party wrote as the same line.
func TestOneLineQuotesAsTheSharedTableSays(t *testing.T) {
	data, err := os.ReadFile("testdata/oneline.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Rows []struct{ ID, Text, Quoted string }
	}
	if err := json.Unmarshal(data, &table); err != nil || len(table.Rows) == 0 {
		t.Fatalf("testdata/oneline.json holds no rows (%v)", err)
	}
	for _, row := range table.Rows {
		t.Run(row.ID, func(t *testing.T) {
			if got := OneLine(row.Text); got != row.Quoted {
				t.Errorf("OneLine(%q) = %q, want %q", row.Text, got, row.Quoted)
			}
		})
	}
}
