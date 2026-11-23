package writingcheck

import "testing"

func TestExemptionFollowsTheActorRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind, id, act string
		exemption           string
	}{
		{"agent session", "session", "01a1d1e8-0000-7000-8000-000000000001", "", ""},
		{"agent session with an act", "session", "01a1d1e8-0000-7000-8000-000000000001", "other", ""},
		{"personal token session", "session", "", "", ""},
		{"daemon session", "session", "legion-daemon:PROJ", "", ExemptDaemon},
		{"person at a browser", "user", "someone@example.com", "", ExemptPerson},
		{"agent acting for a person", "user", "someone@example.com", "01a1d1e8-0000-7000-8000-000000000002", ""},
		{"daemon acting for a person", "user", "someone@example.com", "legion-daemon:PROJ", ExemptDaemon},
		{"system", "system", "document-settlement", "", ExemptSystem},
		{"system with an act", "system", "importer", "01a1d1e8-0000-7000-8000-000000000003", ExemptSystem},
		{"no kind", "", "", "", ExemptSystem},
	} {
		if exemption := Exemption(tc.kind, tc.id, tc.act); exemption != tc.exemption {
			t.Errorf("%s: Exemption(%q, %q, %q) = %q, want %q",
				tc.name, tc.kind, tc.id, tc.act, exemption, tc.exemption)
		}
	}
}
