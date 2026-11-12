package going_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sjawhar/envoy/going"
)

// TestTheRecordReadsBack reads each entry the bake-off wrote back through DecisionRecord, by its
// model's name, with the thresholds the file holds, and holds the chosen entry to R7: one model is
// chosen, it was measured on at least 80 cases and 20 chart-worthy ones, it reaches 90% on the
// cases that name their action and 70% on the rest at a p95 under 500 ms, and its action
// threshold holds.
func TestTheRecordReadsBack(t *testing.T) {
	raw, err := os.ReadFile("decision_models.json")
	if err != nil {
		t.Fatal(err)
	}
	var file []struct {
		Model           string   `json:"model"`
		ActionThreshold *float64 `json:"action_threshold"`
		ChartThreshold  *float64 `json:"chart_threshold"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decision_models.json: %v", err)
	}
	if len(file) == 0 {
		t.Fatal("the record holds no measured model")
	}
	chosen := 0
	for _, entry := range file {
		record, held := going.DecisionRecord(entry.Model)
		if !held {
			t.Errorf("DecisionRecord(%q) holds no entry, though the file has one", entry.Model)
			continue
		}
		if !sameThreshold(record.ActionThreshold, entry.ActionThreshold) || !sameThreshold(record.ChartThreshold, entry.ChartThreshold) {
			t.Errorf("%s reads back thresholds %v and %v, want the file's %v and %v", entry.Model,
				show(record.ActionThreshold), show(record.ChartThreshold), show(entry.ActionThreshold), show(entry.ChartThreshold))
		}
		for _, v := range []*float64{record.ActionThreshold, record.ChartThreshold} {
			if v != nil && (*v < 0 || *v > 1) {
				t.Errorf("%s holds the threshold %v, outside a confidence's 0 to 1", entry.Model, *v)
			}
		}
		if !record.Chosen {
			continue
		}
		chosen++
		switch {
		case record.ActionThreshold == nil:
			t.Errorf("the chosen %s has no action threshold", record.Model)
		case record.Cases < 80 || record.ChartWorthyCases < 20:
			t.Errorf("the chosen %s was measured on %d cases, %d chart-worthy; R7 needs 80 and 20", record.Model, record.Cases, record.ChartWorthyCases)
		case record.NamedAccuracy < 0.9 || record.OtherAccuracy < 0.7 || record.P95MS >= 500:
			t.Errorf("the chosen %s reaches %.3f named, %.3f on the rest at p95 %.0f ms; R7 needs 0.9, 0.7 and under 500",
				record.Model, record.NamedAccuracy, record.OtherAccuracy, record.P95MS)
		}
	}
	if chosen != 1 {
		t.Errorf("%d entries are chosen, want one", chosen)
	}
	if _, held := going.DecisionRecord("a-model-nobody-measured"); held {
		t.Error("an unmeasured model reads back an entry")
	}
}

func sameThreshold(a, b *float64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func show(v *float64) any {
	if v == nil {
		return "null"
	}
	return *v
}
