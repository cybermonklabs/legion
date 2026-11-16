package judge

import "strings"

// specRunes bounds the spec extract the model is shown (D14). It holds the Summary and
// Requirements of a spec written to the house outline, and caps the text an agent-written
// document can put in front of the judge.
const specRunes = 12000

// The two sections of a spec the judge shows the model, in the order it joins them. They are
// matched against a level-2 heading's own words, without case.
const (
	summaryHeading      = "summary"
	requirementsHeading = "requirements"
)

// specExtract is the part of an issue's spec document the model is shown: the first "## Summary"
// section and the first "## Requirements" section, each from its heading to the next heading of
// level 1 or 2, joined in that order; the head of the document when it holds neither heading;
// and at most specRunes runes either way (D14).
//
// A heading inside a fenced code block is text rather than a heading, so a requirements section
// whose example holds a "## " line is not cut short at it.
func specExtract(markdown string) string {
	sections := make([]string, 0, 2)
	for _, heading := range [...]string{summaryHeading, requirementsHeading} {
		if section := specSection(markdown, heading); section != "" {
			sections = append(sections, section)
		}
	}
	if len(sections) == 0 {
		return clampRunes(markdown, specRunes)
	}
	return clampRunes(strings.Join(sections, "\n\n"), specRunes)
}

// specSection is one "## <heading>" section of markdown: the heading line itself and every line
// to the next heading of level 1 or 2, with the blank lines at its end dropped. It answers "" for
// a document with no such heading, and reads the first of them in a document that repeats it.
func specSection(markdown, heading string) string {
	var section []string
	inside, fenced := false, false
	for line := range strings.SplitSeq(markdown, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if fence(line) {
			fenced = !fenced
		}
		switch level := headingLevel(line, fenced); {
		case inside && (level == 1 || level == 2):
			return joinSection(section)
		case inside:
			section = append(section, line)
		case level == 2 && strings.EqualFold(headingWords(line), heading):
			inside = true
			section = append(section, line)
		}
	}
	return joinSection(section)
}

// joinSection is a section's lines as one string, without the blank lines at its end: a section
// runs to the line before the next heading, which in a document written with a blank line
// between its sections is a blank one.
func joinSection(lines []string) string {
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\n")
}

// fence is whether line opens or closes a fenced code block.
func fence(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

// headingLevel is the ATX heading level of line, and 0 for every line that is no heading,
// every line inside a fenced code block among them. Only levels 1 and 2 end a section, so a
// deeper heading inside one is more of that section.
func headingLevel(line string, fenced bool) int {
	if fenced {
		return 0
	}
	hashes := len(line) - len(strings.TrimLeft(line, "#"))
	if hashes == 0 || hashes > 6 {
		return 0
	}
	rest := line[hashes:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0
	}
	return hashes
}

// headingWords is a heading line's own words: what is left once the opening hashes, the hashes
// that close it where it is written closed, and the space around them are taken off.
func headingWords(line string) string {
	return strings.TrimSpace(strings.TrimRight(strings.TrimLeft(line, "#"), "# \t"))
}

// clampRunes is s cut to at most n runes. It cuts on a rune boundary, so what it answers is
// still the UTF-8 the caller handed it, and it walks s once without allocating.
func clampRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
