package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAReflectionEntryThatSetsTwoWaysToAllowItsSiteIsRefused: a reflection entry allows its site
// in one way - by its operands, or by records_boxes, exported_fields_only or guarded_package in
// their place - so the inventory is refused where an entry sets two of the modes, or a mode and
// operands, whose check would read one of them and leave the other unread; an entry in each one
// way loads.
func TestAReflectionEntryThatSetsTwoWaysToAllowItsSiteIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry map[string]any
		want  string // "" where it loads
	}{
		{"operands", map[string]any{"operands": []string{"example.com/p.T"}}, ""},
		{"records_boxes", map[string]any{"records_boxes": true}, ""},
		{"exported_fields_only", map[string]any{"exported_fields_only": true}, ""},
		{"guarded_package", map[string]any{"guarded_package": "example.com/lib"}, ""},
		{"records_boxes and guarded_package", map[string]any{"records_boxes": true, "guarded_package": "example.com/lib"}, "it sets records_boxes and guarded_package"},
		{"exported_fields_only and records_boxes", map[string]any{"exported_fields_only": true, "records_boxes": true}, "it sets records_boxes and exported_fields_only"},
		{"exported_fields_only and operands", map[string]any{"exported_fields_only": true, "operands": []string{"example.com/p.T"}}, "it sets exported_fields_only and lists operands"},
		{"guarded_package and operands", map[string]any{"guarded_package": "example.com/lib", "operands": []string{"example.com/p.T"}}, "it sets guarded_package and lists operands"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := map[string]any{"file": "p/p.go", "function": "example.com/p.walk", "why": "test"}
			for k, v := range tc.entry {
				entry[k] = v
			}
			data, err := json.Marshal(map[string]any{"gate": map[string]any{"base": "main", "rule": []string{"example.com/r.Rule"}, "reflection": []any{entry}}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "inventory.json")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			err = (&gate{inventoryPath: path}).load(false)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("the entry was refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("load answered %v, want a refusal of reflection entry example.com/p.walk naming %q", err, tc.want)
			}
		})
	}
}
