package workflow

import "testing"

// The zero value of RefusalLasts names none of its three kinds, so a RoundRefusal whose writer
// gives a Reason and no Lasts is no refusal at this head: the reviewer's row keeps nothing of it
// for the stuck notice (review), and the daemon's read of the reviews does not remember it as one
// that stands at every head (daemon's reviewToApply).
func TestTheZeroValueOfRefusalLastsIsNoKind(t *testing.T) {
	var zero RefusalLasts
	for name, kind := range map[string]RefusalLasts{
		"RefusedAtThisHead":     RefusedAtThisHead,
		"RefusedAtEveryHead":    RefusedAtEveryHead,
		"RefusedWhileNoSession": RefusedWhileNoSession,
	} {
		if zero == kind {
			t.Errorf("the zero value of RefusalLasts is %s, want it to be none of the three kinds", name)
		}
	}
}
