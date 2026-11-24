package main

import "testing"

// TestEveryControlIsListedOnceUnderItsOwnID: runControls runs the controls list, -only selects
// from it by id, and the round records name a control by its id, so each list's controls are in
// it, in order, and no two controls share an id.
func TestEveryControlIsListedOnceUnderItsOwnID(t *testing.T) {
	want := len(coreControls) + len(censusControls) + len(judgesControls)
	if len(controls) != want {
		t.Fatalf("controls holds %d controls, want the %d of coreControls, censusControls and judgesControls", len(controls), want)
	}
	seen := map[string]string{}
	for _, ctl := range controls {
		if ctl.id == "" {
			t.Errorf("a control has no id: %s", ctl.what)
			continue
		}
		if what, ok := seen[ctl.id]; ok {
			t.Errorf("controls %q and %q share the id %s", what, ctl.what, ctl.id)
		}
		seen[ctl.id] = ctl.what
	}
}
