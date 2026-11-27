package main

//ci:reads-set refgen_api TestOnlyTheRenewCredentialListsEnrollmentNotLive and TestOnlyTheHoldStepsClassesListTheHoldsRefusals readAPI(repo root) parses it

import (
	"path/filepath"
	"testing"
)

// TestOnlyTheRenewCredentialListsEnrollmentNotLive reads the broker's own API package, as the docs
// build does, and checks that the renew route's credential alone lists ENROLLMENT_NOT_LIVE, while
// every other session route's lists PROOF_INVALID and not that code: the error and API pages are
// generated from these classes, so a shared authenticate clause would put the renew-only code on
// every session route.
func TestOnlyTheRenewCredentialListsEnrollmentNotLive(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]map[string]bool{}
	for _, c := range a.classes {
		codes[c.Wrapper] = map[string]bool{}
		for _, o := range refusals(c.Refusals) {
			codes[c.Wrapper][o.Code] = true
		}
	}
	if !codes["renewAuth"]["ENROLLMENT_NOT_LIVE"] || !codes["renewAuth"]["PROOF_INVALID"] {
		t.Errorf("renewAuth refusals = %v, want ENROLLMENT_NOT_LIVE and PROOF_INVALID", codes["renewAuth"])
	}
	if codes["sessionAuth"]["ENROLLMENT_NOT_LIVE"] || !codes["sessionAuth"]["PROOF_INVALID"] {
		t.Errorf("sessionAuth refusals = %v, want PROOF_INVALID and not ENROLLMENT_NOT_LIVE", codes["sessionAuth"])
	}
	if credentialLabels["renewAuth"] == credentialLabels["sessionAuth"] {
		t.Errorf("renewAuth and sessionAuth share the label %q, so the pages cannot tell their rows apart", credentialLabels["sessionAuth"])
	}
}

// TestOnlyTheHoldStepsClassesListTheHoldsRefusals reads the broker's own API package, as the docs
// build does. launcherAuth and sessionAuth ask the hold gate in their own step after
// authentication (the routeHandler's hold), and their classes list that step's refusals, 403
// MEMBERSHIP_HELD and 503 HOLD_UNCHECKABLE, and sessionAuth's the PROOF_INVALID of an enrollment
// gone since its proof verified; the classes that ask the hold nothing, launcherEndAuth,
// sessionEndAuth and renewAuth, list neither hold code.
//
// The fault: refgen reading authenticate's case alone, as it did; the hold step's refusals reach
// no class, and the first two rows fail.
func TestOnlyTheHoldStepsClassesListTheHoldsRefusals(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]map[string]int{}
	for _, c := range a.classes {
		statuses[c.Wrapper] = map[string]int{}
		for _, o := range refusals(c.Refusals) {
			statuses[c.Wrapper][o.Code] = o.Status
		}
	}
	for _, wrapper := range []string{"launcherAuth", "sessionAuth"} {
		if statuses[wrapper]["MEMBERSHIP_HELD"] != 403 || statuses[wrapper]["HOLD_UNCHECKABLE"] != 503 {
			t.Errorf("%s refusals = %v, want 403 MEMBERSHIP_HELD and 503 HOLD_UNCHECKABLE from its hold step", wrapper, statuses[wrapper])
		}
	}
	for _, wrapper := range []string{"launcherEndAuth", "sessionEndAuth", "renewAuth"} {
		if _, held := statuses[wrapper]["MEMBERSHIP_HELD"]; held {
			t.Errorf("%s refusals = %v, want no MEMBERSHIP_HELD: it asks the hold nothing", wrapper, statuses[wrapper])
		}
		if _, unchecked := statuses[wrapper]["HOLD_UNCHECKABLE"]; unchecked {
			t.Errorf("%s refusals = %v, want no HOLD_UNCHECKABLE: it asks the hold nothing", wrapper, statuses[wrapper])
		}
	}
}
