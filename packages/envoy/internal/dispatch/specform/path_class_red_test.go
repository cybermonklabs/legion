package specform

// Red test of the acceptance round at fc72c17788 (place it at
// packages/envoy/internal/dispatch/specform/path_class_red_test.go). An address of a host in an
// rsync or scp command, written in code font, is refused as a person at fc72c17788 and opened at
// 3c2d063fed: its ":" is followed by ":", a quote, "$" or "*", which the new path class leaves out.
// The control rows are the round's own hidden forms, which must stay refused.

import "testing"

func TestAHostAddressBeforeAnyPathOfACommandNamesNoPerson(t *testing.T) {
	read := func(t *testing.T, body string) []Finding {
		t.Helper()
		extracted, err := ExtractDocument("spec.md", 1, ":::requirement{#honest}\n"+body+"\n:::\n")
		if err != nil {
			t.Fatal(err)
		}
		return check(Context{}, Input{Texts: extracted.Texts, Proposed: extracted.Terms})
	}
	person := func(findings []Finding) bool {
		for _, finding := range findings {
			if finding.Rule == "person" {
				return true
			}
		}
		return false
	}
	for _, form := range []string{
		"ops@backup.example.com::backups",
		`deploy@build.example.com:"/srv/my app"`,
		"deploy@build.example.com:'/srv/app'",
		"deploy@build.example.com:$HOME/app",
		"deploy@build.example.com:*.log",
	} {
		if findings := read(t, "Legion reads `"+form+"` before each run."); person(findings) {
			t.Errorf("%s in code font: %v, want no person", form, findings)
		}
	}
	for _, hidden := range []string{
		"Dispatch sends each summary (to ada@example.com:) each day.",
		"Dispatch mails ada@example.com:, and nobody else.",
		"Dispatch mails one person: ada@example.com: she approves each change.",
	} {
		if findings := read(t, hidden); !person(findings) {
			t.Errorf("control %q: %v, want a person", hidden, findings)
		}
	}
}
