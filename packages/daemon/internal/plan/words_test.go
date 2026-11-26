package plan

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Words is the one spelling of steps that the settings, register_plan's answer, a plan node and
// `legion config check` show: each step as a plan writes it, in order, and no step as an empty
// list on the wire, never null.
func TestWordsSpellEachStepAsAPlanWritesItAndNoStepAsAnEmptyList(t *testing.T) {
	got := Words([]Step{{Role: "implementer"}, {Role: "reviewer", Key: "r"}, {Role: "acceptance", Key: "owner"}})
	if want := []string{"implementer", "reviewer:r", "acceptance:owner"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Words = %q, want %q", got, want)
	}
	for _, none := range [][]Step{nil, {}} {
		wire, err := json.Marshal(Words(none))
		if err != nil || string(wire) != "[]" {
			t.Errorf("Words(%#v) on the wire = %s (%v), want []", none, wire, err)
		}
	}
}
