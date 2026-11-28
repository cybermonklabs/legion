package listenerwire

import "testing"

// LegionHandoffSubject is the subject legionAgentHandoffSubject in packages/contracts builds, which
// that package's own test pins to the same string (src/legion-handoff.test.ts), and a project's
// consumer filter is the same subject with ">" for the session (intake.OpenHandoffs in
// packages/daemon).
func TestLegionHandoffSubjectIsTheContractsSubject(t *testing.T) {
	for _, tc := range []struct{ project, session, want string }{
		{"legion", "ses_tester", "notifications.legion.legion.agents.ses_tester"},
		{"infra", ">", "notifications.legion.infra.agents.>"},
	} {
		if got := LegionHandoffSubject(tc.project, tc.session); got != tc.want {
			t.Errorf("LegionHandoffSubject(%q, %q) = %q, want %q", tc.project, tc.session, got, tc.want)
		}
	}
}
