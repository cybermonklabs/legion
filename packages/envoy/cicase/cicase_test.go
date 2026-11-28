package cicase

import "testing"

// A case is of a job that ended, of one that has not ended, or of no check a CI job runs (""),
// and never two of these; a case Dispatch does not publish is none of them, and Known refuses it.
func TestEachCaseIsEndedOrNotEndedAndAnyOtherIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		ciCase          string
		ended, notEnded bool
		known           bool
	}{
		{"success", true, false, true},
		{"failure", true, false, true},
		{"cancelled", true, false, true},
		{"job_recorded", true, false, true},
		{"not_completed", false, true, true},
		{"absent", false, true, true},
		{"no_head", false, true, true},
		{"github_unreadable", false, true, true},
		{"read_failed", false, true, true},
		{"not_started", false, true, true},
		{"", false, false, true},
		{"a_case_no_dispatch_writes", false, false, false},
		{"Success", false, false, false},
	} {
		if got := Ended(tc.ciCase); got != tc.ended {
			t.Errorf("Ended(%q) = %t, want %t", tc.ciCase, got, tc.ended)
		}
		if got := NotEnded(tc.ciCase); got != tc.notEnded {
			t.Errorf("NotEnded(%q) = %t, want %t", tc.ciCase, got, tc.notEnded)
		}
		if got := Known(tc.ciCase); got != tc.known {
			t.Errorf("Known(%q) = %t, want %t", tc.ciCase, got, tc.known)
		}
	}
}
