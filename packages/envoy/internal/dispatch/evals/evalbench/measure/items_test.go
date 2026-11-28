package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTheItemsMeasureExplainsEveryExecutionOnce captures one read, not the concurrent reads,
// and counts both executions of a statement used with different parameters for the two ranges.
func TestTheItemsMeasureExplainsEveryExecutionOnce(t *testing.T) {
	t.Parallel()
	const begin = "begin isolation level repeatable read read only"
	const definition = "select eval_id from evals_current where eval_id = $1"
	const readings = "select measured_at from eval_readings where eval_id = $1 and measured_at >= $2"
	var log strings.Builder
	line := func(pid, queryID int, sql, detail string) {
		t.Helper()
		record, err := json.Marshal(map[string]any{
			"timestamp": "2026-11-02 12:00:00.000 UTC", "pid": pid, "query_id": queryID,
			"message": "duration: 1.2 ms  execute <unnamed>: " + sql, "detail": detail,
		})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&log, string(record))
	}
	line(1, 0, begin, "")
	line(1, 4, definition, "Parameters: $1 = 'EVL/other'")
	line(2, 0, begin, "")
	line(2, 3, "select key from projects where key = $1", "Parameters: $1 = 'EVL'")
	line(2, 4, definition, "Parameters: $1 = 'EVL/steps'")
	line(1, 5, readings, "Parameters: $1 = 'EVL/other', $2 = '2026-11-01'")
	line(3, 6, "select 1", "")
	line(2, 5, readings, "Parameters: $1 = 'EVL/steps', $2 = '2026-11-01'")
	line(1, 0, "commit", "")
	line(2, 5, readings, "Parameters: $1 = 'EVL/steps', $2 = '2026-10-01'")
	line(2, 0, "commit", "")

	path := filepath.Join(t.TempDir(), "postgres.json")
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := waitItemsStatements(t.Context(), path, 0, "EVL/steps")
	if err != nil {
		t.Fatal(err)
	}
	var replayed []string
	r := &report{prefix: "RESULT"}
	ms, err := explainItems(t.Context(), r, read, func(_ context.Context, text string, params []string) (string, float64, error) {
		replayed = append(replayed, text+" "+strings.Join(params, ","))
		plan := fmt.Sprintf("Index Scan\nPlanning Time: 0.500 ms\nExecution Time: %.3f ms", float64(len(replayed)))
		return plan, executionTime(plan), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"select key from projects where key = $1 'EVL'",
		definition + " 'EVL/steps'",
		readings + " 'EVL/steps','2026-11-01'",
		readings + " 'EVL/steps','2026-10-01'",
	}
	if !slices.Equal(replayed, want) || ms != 12 {
		t.Errorf("replayed %q, total %.3f ms; want %q, 12 ms", replayed, ms, want)
	}
}
