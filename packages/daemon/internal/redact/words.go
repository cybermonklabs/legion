package redact

import (
	"slices"
	"strings"
)

// HeldRun is the shortest run of a withheld word's characters a printed text may not hold, compared
// without case: the run the printed-credential tests count as a leak.
const HeldRun = 10

// Word is one text Withheld takes out of what it prints, and what stands in its place.
type Word struct {
	// Text is replaced by Shown wherever a copy of it stands, inside a longer word too.
	Text, Shown string
	// Exact, when set, takes out an exact copy of Text alone: a run of it left in the text does
	// not withhold it. It is for a word whose credential is a part of it, such as the value of a
	// NAME=value word, which is a Word of its own, so the NAME's run does not withhold every
	// line that names the variable.
	Exact bool
}

// Withheld is text with every copy of each word's Text replaced by its Shown, the longest first so
// a word holding another is replaced whole, and ok false, nothing of it to be printed, when what is
// left still holds HeldRun characters in a row of a word that is not Exact, compared without case:
// a program that is not this repository's can quote a word folded, wrapped, escaped or cut short.
// A word shorter than HeldRun is replaced but never withholds the text. It is the one rule the
// daemon prints a child's output by when that output can echo a secret it was handed: a `secrets`
// command's stderr, `private_key_command`'s, and a launch whose prefix carries a credential.
func Withheld(text string, words []Word) (shown string, ok bool) {
	sorted := slices.Clone(words)
	slices.SortStableFunc(sorted, func(a, b Word) int { return len(b.Text) - len(a.Text) })
	for _, word := range sorted {
		if word.Text != "" {
			text = strings.ReplaceAll(text, word.Text, word.Shown)
		}
	}
	folded := strings.ToLower(text)
	for _, word := range sorted {
		if word.Exact {
			continue
		}
		lower := strings.ToLower(word.Text)
		for i := 0; i+HeldRun <= len(lower); i++ {
			if strings.Contains(folded, lower[i:i+HeldRun]) {
				return "", false
			}
		}
	}
	return text, true
}
