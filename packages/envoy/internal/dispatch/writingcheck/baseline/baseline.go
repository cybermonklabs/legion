// Package baseline is the six regular expressions the writing check's detector is measured
// against: one for each phrase row of skills/plain-words/SKILL.md. A sentence any of them
// matches is a sentence the baseline calls vacuous.
package baseline

import "regexp"

// Patterns are the six expressions, each case-insensitive, in the skill's row order: land,
// unblock, name, pay, carry, show.
var Patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bland(s|ed|ing)? the (change|PR|pull request|work)\b`),
	regexp.MustCompile(`(?i)\bunblocks? the\b`),
	regexp.MustCompile(`(?i)\b(names|named|naming) (the|a|an|its|each|every|what|who|which|that)\b`),
	regexp.MustCompile(`(?i)\bpays? (for itself|off)\b`),
	regexp.MustCompile(`(?i)\bcarr(y|ies|ied) (it|the finding|that|this)\b`),
	regexp.MustCompile(`(?i)\b(shows|showed|showing) (the|a|an|that|what|how|its|no)\b`),
}

// Matches is whether any of the Patterns matches text.
func Matches(text string) bool {
	for _, p := range Patterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}
