package workflow

import "testing"

// TestTmCountRows prints the number of rows in Table. It is injected into package workflow with
// `go build -overlay`, so the repository tree stays unchanged.
func TestTmCountRows(t *testing.T) {
	literal := 0
	backward := 0
	for _, row := range Table {
		if row.Trigger == TriggerBackward {
			backward++
			continue
		}
		literal++
	}
	t.Logf("rows=%d literal=%d backward=%d", len(Table), literal, backward)
}
