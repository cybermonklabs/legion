package specform

import (
	"strings"
	"testing"
)

// The stated limit of the email rule: an address user@host with no path after it reads as the
// address of a person, also in an honest command that names a machine. So scp and rsync to the
// home folder (the colon ends the address, or white space follows it), a remote glob that starts
// with "?", and an ssh or sftp target with no path are refused as a person, and the refusal gives
// the ways out: write the path after the colon, or write the user and the host apart. The two
// controls, a person's address alone and before a colon and a space, are refused too. The rows are
// the acceptance test's remote targets, which stay refused by this limit.
func TestAnAddressOfAMachineWithNoPathIsReadAsAPersonWithItsWayOut(t *testing.T) {
	read := func(t *testing.T, body string) []Finding {
		t.Helper()
		extracted, err := ExtractDocument("spec.md", 1, ":::requirement{#honest}\n"+body+"\n:::\n")
		if err != nil {
			t.Fatal(err)
		}
		return Check(Context{}, Input{Texts: extracted.Texts, Proposed: extracted.Terms})
	}
	person := func(findings []Finding) (Finding, bool) {
		for _, finding := range findings {
			if finding.Rule == rulePerson {
				return finding, true
			}
		}
		return Finding{}, false
	}
	const address = "deploy@build.example.com"
	ways := []string{address + ":~/", address + ":./?.log", "ssh -l deploy build.example.com"}
	for _, form := range []string{
		"scp report.txt deploy@build.example.com:",
		"rsync -a dist/ deploy@build.example.com:",
		"scp deploy@build.example.com:?.log .",
		"ssh deploy@build.example.com",
		"sftp deploy@build.example.com",
	} {
		findings := read(t, "Legion runs `"+form+"` before each build.")
		found, ok := person(findings)
		if !ok {
			t.Errorf("%s in code font: %v, want a person", form, findings)
			continue
		}
		refusal := Message(ActRequest, findings, len(findings), "").Refusal
		for _, way := range ways {
			if !strings.Contains(found.Ways, way) || !strings.Contains(refusal, way) {
				t.Errorf("%s: the ways out %q of the finding and the refusal %q do not give the way out %q", form, found.Ways, refusal, way)
			}
		}
	}
	for _, named := range []string{
		"Dispatch sends each answer to ada@example.com at once.",
		"Dispatch mails one person: ada@example.com: she approves each change.",
	} {
		if findings := read(t, named); !func() bool { _, ok := person(findings); return ok }() {
			t.Errorf("control %q: %v, want a person", named, findings)
		}
	}
	for _, way := range []string{"Legion runs `scp report.txt " + ways[0] + "` before each build.", "Legion runs `scp " + ways[1] + " .` before each build.",
		"Legion runs `" + ways[2] + "` before each build."} {
		if findings := read(t, way); len(findings) > 0 {
			t.Errorf("the way out %q: %v, want no finding", way, findings)
		}
	}
}
