package specgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaCase is one row of testdata/schema-cases.json: the data of one document, whether the
// schema takes it, and the JSON pointer of each place where it refuses it, "" for the node. The
// TypeScript test of the schema (packages/dispatch/e2e/__tests__/spec-node-schema.test.ts) reads
// the same table, so the two validators read the schema the same way.
type schemaCase struct {
	Name  string          `json:"name"`
	Node  json.RawMessage `json:"node"`
	Valid bool            `json:"valid"`
	At    []string        `json:"at"`
}

// The schema takes each valid row and refuses each invalid one exactly at the places the row
// names.
func TestSchemaCases(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("testdata/schema-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []schemaCase
	if err := json.Unmarshal(content, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("testdata/schema-cases.json holds no row")
	}
	for _, row := range rows {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(row.Node))
			if err != nil {
				t.Fatal(err)
			}
			err = nodeSchema().Validate(inst)
			if (err == nil) != row.Valid {
				t.Fatalf("Validate(%s) = %v, want valid %t", row.Node, err, row.Valid)
			}
			if row.Valid {
				return
			}
			var refused *jsonschema.ValidationError
			if !errors.As(err, &refused) {
				t.Fatalf("Validate: %v, want a *jsonschema.ValidationError", err)
			}
			got := map[string]bool{}
			for _, leaf := range schemaLeaves(refused) {
				got[jsonPointer(leaf.InstanceLocation)] = true
			}
			want := map[string]bool{}
			for _, at := range row.At {
				want[at] = true
			}
			if !maps.Equal(got, want) {
				t.Errorf("Validate(%s) refuses at %q, want %q:\n%v", row.Node, slices.Sorted(maps.Keys(got)), row.At, err)
			}
		})
	}
}
