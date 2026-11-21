package prompts

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// privateNameTermsVar is the variable .github/scripts/check-private-names.sh reads the
// deployment's own term list from: the names of a deployment that this public repository cannot
// spell, so a test that must refuse them reads them from where that script does.
const privateNameTermsVar = "PRIVATE_NAME_TERMS"

// keyLine is a `key:<KEY>` line of the list, which names one of the deployment's Dispatch projects
// and is no term: the pattern check-private-names.sh's loop over the list tells such a line apart
// from a term with.
var keyLine = regexp.MustCompile(`^key:[A-Z][A-Z0-9]*$`)

// deploymentTerms are the terms of list, read as check-private-names.sh reads PRIVATE_NAME_TERMS
// into its `terms`: one a line, every byte-order mark dropped wherever it stands, the blanks
// around each line dropped (unicode.IsSpace holds every blank the script drops, the no-break space
// and the carriage return among them), and neither a line left empty nor a `key:` line kept.
func deploymentTerms(list string) []string {
	var terms []string
	for _, line := range strings.Split(list, "\n") {
		term := strings.TrimFunc(strings.ReplaceAll(line, "\ufeff", ""), unicode.IsSpace)
		if term != "" && !keyLine.MatchString(term) {
			terms = append(terms, term)
		}
	}
	return terms
}

// termPattern is term as check-private-names.sh's term_pattern makes it for grep -E -i: the term
// anywhere in the text, each letter in any case, and every other character standing for itself.
// Go's (?i) counts every character that folds to a letter, so the Kelvin, long s, Angstrom, Ohm,
// capital theta symbol and capital sharp s that term_pattern writes in beside a letter come with
// it. The dotless i and the capital I with a dot fold to no i, so they are written in beside an i
// here as term_pattern writes them.
func termPattern(term string) *regexp.Regexp {
	var pattern strings.Builder
	pattern.WriteString("(?i)")
	for _, char := range term {
		switch unicode.ToLower(char) {
		case 'i', 'ı':
			pattern.WriteString("[iıİ]")
		default:
			pattern.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	return regexp.MustCompile(pattern.String())
}

// The list is read as check-private-names.sh reads it, and a term matches where the script's
// pattern for it does. The terms are placeholders: the deployment's own list is a secret.
func TestADeploymentTermIsReadAndMatchedAsCheckPrivateNamesDoes(t *testing.T) {
	list := "\ufeff disk.io\r\n\n\u00a0key:ACME\u00a0\nkey:lower\n\tdi\ufeffsk.io \n"
	if got, want := deploymentTerms(list), []string{"disk.io", "key:lower", "disk.io"}; !slices.Equal(got, want) {
		t.Fatalf("the terms of the list are %q, want %q", got, want)
	}
	if got := deploymentTerms(""); len(got) != 0 {
		t.Fatalf("an empty list holds the terms %q, want none", got)
	}
	if got := deploymentTerms("key:ACME\n \n"); len(got) != 0 {
		t.Fatalf("a list of a key and a blank holds the terms %q, want none", got)
	}
	pattern := termPattern("disk.io")
	for _, text := range []string{
		"a line naming disk.io in it",
		"DISK.IO",
		"d\u0131\u017fk.io",      // a dotless i and a long s
		"d\u0130s\u212a.\u0130o", // a capital I with a dot and a Kelvin sign
	} {
		if !pattern.MatchString(text) {
			t.Errorf("the term's pattern misses %q", text)
		}
	}
	for _, text := range []string{"diskxio", "dis k.io", "disk-io"} {
		if pattern.MatchString(text) {
			t.Errorf("the term's pattern matches %q, where its full stop stands for itself", text)
		}
	}
}
