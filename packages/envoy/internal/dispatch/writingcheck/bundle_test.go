package writingcheck

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/rules"
)

// Every rule package rules carries is either enabled by the bundle or turned off by its
// measurement with a reason, never both, and a rule the measurement turned off refused more
// than the 5% ceiling.
func TestTheBundleNamesEveryRuleOnceAndOffRulesAreOverTheCeiling(t *testing.T) {
	off := map[string]RuleMeasurement{}
	for _, m := range MeasuredOff {
		off[m.ID] = m
		if m.PooledShare <= 0.05 || m.Reason == "" {
			t.Errorf("%s is measured off at %.3f with reason %q, want a share over 0.05 and a reason", m.ID, m.PooledShare, m.Reason)
		}
	}
	for _, rule := range rules.All() {
		enabled := slices.Contains(EnabledRules, rule.ID)
		_, measuredOff := off[rule.ID]
		if enabled == measuredOff {
			t.Errorf("%s: enabled %t, measured off %t; want exactly one", rule.ID, enabled, measuredOff)
		}
	}
	for _, id := range EnabledRules {
		if !slices.ContainsFunc(rules.All(), func(r rules.Rule) bool { return r.ID == id }) {
			t.Errorf("EnabledRules names %s, which package rules does not carry", id)
		}
	}
}
