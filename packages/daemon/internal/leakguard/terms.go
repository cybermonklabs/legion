package leakguard

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// TermsVar is the deployment's own list of private names, the ones no file of this public
// repository can spell: the same list CI's .github/scripts/check-private-names.sh reads from the
// Actions secret of that name. The deployment hands it to the daemon in the daemon's environment
// under this name, as it hands the daemon its other settings; the daemon hands it to every launch
// as a launch secret of the same name, so a role holds it behind TermsVar+"_FILE" (TermsFileVar)
// and never in its environment or a process's arguments. The guard reads either, the file first.
//
// The value holds one term a line. Each line's blanks are dropped, and an empty line is dropped
// too, as the script drops them; so is a `key:` line, which names a Dispatch project for the
// script's issue-key rule and is no name this guard looks for. A list with no term left is no list.
const TermsVar = "LEGION_PRIVATE_NAME_TERMS"

// TermsFileVar is the pointer a role is handed the term list behind: the path of a file that
// holds TermsVar's value (runtime.SecretFile, the `<NAME>_FILE` form of every launch secret).
const TermsFileVar = TermsVar + "_FILE"

// termLabel is what a refusal of a record that names a term says of it: the class alone, never
// the term, since the report is printed and the term is what must not be. It is the script's own
// label for its fifth rule, word for word, which names the list by its CI secret's name, so the
// port's comparison (.github/scripts/check-private-names-port.test.sh) reads one class for both.
const termLabel = "names a term of the deployment's own list (PRIVATE_NAME_TERMS)"

// Terms is the deployment's term list as env holds it: the file TermsFileVar names, else TermsVar
// itself, read as TermsVar says. A named file that cannot be read is an error, so the caller
// refuses the write rather than run without the list it was told it has.
func Terms(env []string) ([]string, error) {
	text := value(env, TermsVar)
	if path := value(env, TermsFileVar); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("the file %s names could not be read: %w", TermsFileVar, err)
		}
		text = string(raw)
	}
	var terms []string
	for line := range strings.Lines(text) {
		term := strings.TrimSpace(line)
		if term == "" || strings.HasPrefix(term, "key:") {
			continue
		}
		terms = append(terms, term)
	}
	return terms, nil
}

// termMatcher is one pattern that matches any of terms, each as given, in any case and anywhere
// in a text or a path, as the script matches one; nil where there is no term. Go's case folding
// is Unicode's simple folding, so a term's letters match their folded forms (the Kelvin sign for a
// k among them), as grep -i and the script's added folds do.
func termMatcher(terms []string) *regexp.Regexp {
	if len(terms) == 0 {
		return nil
	}
	quoted := make([]string, len(terms))
	for i, term := range terms {
		quoted[i] = regexp.QuoteMeta(term)
	}
	return regexp.MustCompile("(?i)(?:" + strings.Join(quoted, "|") + ")")
}

// scanTerms is the report of records against the deployment's terms: one line for a record whose
// text names a term, and one for a record whose label, a file's path, names one. Each line names
// the record by its label, masked by the repository's own rules (rules.mask) and then with every
// path part a term touches as "<name>", or "<name>" whole where a term still shows across the
// parts; it never names the term.
func scanTerms(engine *rules, terms *regexp.Regexp, records []Record) []string {
	if terms == nil {
		return nil
	}
	var lines []string
	for _, record := range records {
		label := maskTerms(terms, engine.mask(record.Label))
		if terms.MatchString(record.Text) {
			lines = append(lines, label+": "+termLabel)
		}
		if terms.MatchString(record.Label) {
			lines = append(lines, label+": "+termLabel+" in its path")
		}
	}
	return lines
}

// maskTerms is label with each slash-separated part a term matches replaced by "<name>", and the
// whole label "<name>" where a term that holds a "/" still matches across what is left.
func maskTerms(terms *regexp.Regexp, label string) string {
	parts := strings.Split(label, "/")
	for i, part := range parts {
		if terms.MatchString(part) {
			parts[i] = "<name>"
		}
	}
	masked := strings.Join(parts, "/")
	if terms.MatchString(masked) {
		return "<name>"
	}
	return masked
}
