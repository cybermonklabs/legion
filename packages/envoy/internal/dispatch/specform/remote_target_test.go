package specform

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The stated limit of the email rule: an address user@host with no path after it reads as the
// address of a person, also in an honest command that names a machine. So scp and rsync to the
// home folder (the colon ends the address, or white space follows it), a remote glob that starts
// with "?", and an ssh or sftp target with no path are refused as a person, and the refusal
// writes the line of that finding. The two controls, a person's address alone and before a colon
// and a space, are refused too, and the same commands with a path after the colon, or with the
// user and the host apart, are refused by no rule. The rows are the acceptance test's remote
// targets, which stay refused by this limit.
func TestAnAddressOfAMachineWithNoPathIsReadAsAPerson(t *testing.T) {
	read := func(t *testing.T, body string) []Finding {
		t.Helper()
		extracted, err := ExtractDocument("spec.md", 1, ":::requirement{#honest}\n"+body+"\n:::\n")
		if err != nil {
			t.Fatal(err)
		}
		return check(Context{}, Input{Texts: extracted.Texts, Proposed: extracted.Terms})
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
		// The line is written here, not read from findingLine, and the refusal must hold it whole,
		// so a line that writes more than the finding's detail fails this row.
		line := fmt.Sprintf("- %s %s in %s: %s.", found.Kind, found.Node, found.Where, found.Detail)
		if !slices.Contains(strings.Split(refusal, "\n"), line) {
			t.Errorf("%s: the refusal\n%s\ndoes not hold the line %q of the finding", form, refusal, line)
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
	for _, fixed := range []string{"Legion runs `scp report.txt " + address + ":~/` before each build.", "Legion runs `scp " + address + ":./?.log .` before each build.",
		"Legion runs `ssh -l deploy build.example.com` before each build."} {
		if findings := read(t, fixed); len(findings) > 0 {
			t.Errorf("%q: %v, want no finding", fixed, findings)
		}
	}
}
