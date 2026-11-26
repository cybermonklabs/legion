package admit

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Every rule the repository keeps besides the daemon's own (record.SlotRules) reaches the readers
// of the choices it makes: each row of TestEveryReaderOfTheSlotRuleTakesTheRule whose own rule
// makes only choices the kept rule makes the same way (keptRuleMakes) is run under the kept rule,
// through the daemon's own chain of handlers, and gives the outcome it gives under its own rule
// (slotRuleReader.changed). So a kept rule that a reader read wrongly, or whose other choices
// undid one of them, fails naming the rule and the row; and a choice a kept rule moves off
// record.MainRule that no row reads its way fails too, since nothing would show the rule makes it.
func TestEveryKeptRuleTakesTheReadersRows(t *testing.T) {
	rows := slotRuleReaders()
	for _, rule := range record.SlotRules()[1:] {
		t.Run(rule.Name, func(t *testing.T) {
			read := make(map[string]bool)
			for _, row := range rows {
				if !keptRuleMakes(rule, row) {
					continue
				}
				for _, choice := range row.changes {
					read[choice] = true
				}
				t.Run(row.reader, func(t *testing.T) {
					if got := row.outcome(t, rule); got != row.changed {
						t.Errorf("under the rule %q: %s\nwant %s, as under %q\n(%s)", rule.Name, got, row.changed, row.rule.Name, row.why)
					}
				})
			}
			for _, choice := range choicesMoved(rule) {
				if !read[choice] {
					t.Errorf("the rule %q moves record.SlotRule.%s off the daemon's own, and no row of "+
						"TestEveryReaderOfTheSlotRuleTakesTheRule reads that choice made its way", rule.Name, choice)
				}
			}
		})
	}
}

// keptRuleMakes says whether rule makes each choice row changes the way row's own rule makes it:
// the same value of the field, and for the phases that keep a slot (KeepsSlotIn), the same answer
// in each phase where row's rule answers otherwise than record.MainRule, which is the phase the row
// stands its issue in.
func keptRuleMakes(rule record.SlotRule, row slotRuleReader) bool {
	for _, choice := range row.changes {
		if choice == "KeepsSlotIn" {
			for _, stands := range phase.All {
				moved := row.rule.PhaseKeepsSlot(stands) != record.MainRule.PhaseKeepsSlot(stands)
				if moved && rule.PhaseKeepsSlot(stands) != row.rule.PhaseKeepsSlot(stands) {
					return false
				}
			}
			continue
		}
		made, wanted := reflect.ValueOf(rule).FieldByName(choice).Interface(), reflect.ValueOf(row.rule).FieldByName(choice).Interface()
		if !reflect.DeepEqual(made, wanted) {
			return false
		}
	}
	return true
}
