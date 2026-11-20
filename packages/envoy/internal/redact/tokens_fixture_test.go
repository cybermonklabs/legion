package redact

import (
	"encoding/json"
	"os"
	"testing"
)

// TestTokensReadsTheSharedTable: Tokens answers each row of testdata/tokens.json as the row says.
// The TypeScript half, packages/contracts/src/remote-text.ts's tokens, reads the same table.
func TestTokensReadsTheSharedTable(t *testing.T) {
	data, err := os.ReadFile("testdata/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Rows []struct {
			ID     string   `json:"id"`
			Text   string   `json:"text"`
			Limit  int      `json:"limit"`
			Tokens []string `json:"tokens"`
			Want   string   `json:"want"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Rows) == 0 {
		t.Fatal("the table holds no row, so nothing was checked")
	}
	for _, row := range table.Rows {
		if got := Tokens(row.Text, row.Limit, row.Tokens...); got != row.Want {
			t.Errorf("%s: Tokens(%q, %d, %q) = %q, want %q", row.ID, row.Text, row.Limit, row.Tokens, got, row.Want)
		}
	}
}
