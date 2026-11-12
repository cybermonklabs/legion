// Package docnumbers holds a number a constant owns to the prose that states it. A count the code
// decides - a pool's size, a bound, a margin - is usually written again in a guide or an
// architecture document, and nothing holds the two together: the constant is raised, every test
// stays green because no test reads a sentence, and the number a person reads is left behind.
// dispatch://INFRA-558 met that four times in one night, in three shapes (digits in two files and
// a word in a third), each found by a person reading rather than by a row.
//
// A caller registers its own constants and documents and gets one row that fails when a sentence
// disagrees with the constant it states:
//
//	func TestTheDocumentsStateTheCountsThisPackageHolds(t *testing.T) {
//		docnumbers.Check(t, filepath.Join("..", "..", ".."), []docnumbers.Stated{
//			{File: "docs/site/.../running-dispatch.md", Pattern: `up to ` + docnumbers.Number + ` for the work`,
//				Number: LeasePoolSize, Says: "the connections one server opens for the work it runs once"},
//		})
//	}
//
// Each Stated names one sentence: the file, a pattern with exactly one group over
// docnumbers.Number, the constant that group must equal, and what that number is of, which is the
// words the failure uses. Every pattern reads a count written as a word as readily as one in
// digits, since prose does both. A pattern that matches nothing fails too: a sentence someone
// reworded or deleted would otherwise leave the row passing over nothing, which is the same fault
// in a quieter place.
//
// Keep a caller's list to the documents that state its own constants. A number of something else -
// a migration version, a count of review rounds - belongs to no entry here.
package docnumbers

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Number is the one group every pattern gives: digits, or the count written as a word. The word
// may begin with a capital, because prose spells a count at the start of a sentence ("Four of
// them wait on ..."); a class of lower-case letters alone matched the tail of such a word and
// reported the sentence as stating "our", which is a reader producing the fault it exists to
// catch. The comparison below is case-insensitive for the same reason, and for the word alone:
// digits have no case, so nothing else of the reading changes.
const Number = `(\d+|[A-Za-z][a-z]*(?:-[a-z]+)?)`

// Stated is one sentence of one document and the constant it states.
type Stated struct {
	// File is the path from the repository root.
	File string
	// Pattern holds exactly one group, which is Number.
	Pattern string
	// Number is what that group must equal, in digits or as a word.
	Number int
	// Says is what the number is of, in the words the failure should use.
	Says string
}

// Check reads each sentence under root and fails for every number that disagrees with its
// constant, naming the file, what was read and what was wanted.
func Check(t *testing.T, root string, stated []Stated) {
	t.Helper()
	for _, want := range stated {
		body, err := os.ReadFile(filepath.Join(root, want.File))
		if err != nil {
			t.Fatalf("read %s: %v", want.File, err)
		}
		matches := regexp.MustCompile(want.Pattern).FindAllStringSubmatch(string(body), -1)
		if len(matches) == 0 {
			t.Errorf("%s states nothing this pattern reads, so the number it holds is unchecked: %s (%s)",
				want.File, want.Pattern, want.Says)
			continue
		}
		digits, word := strconv.Itoa(want.Number), asAWord(t, want.Number)
		for _, match := range matches {
			if said := match[1]; said != digits && !strings.EqualFold(said, word) {
				t.Errorf("%s says %q of %s, want %s or %q: %s",
					want.File, said, want.Says, digits, word, strings.TrimSpace(match[0]))
			}
		}
	}
}

// asAWord gives the count as a sentence that spells it writes it, for every count below a
// hundred, which is the range prose of this repository spells: "five", "sixty-four". A count of a
// hundred or more it writes in digits, as the documents do with 109 and 218, so no word is wanted
// there. A count it cannot write at all - a negative one, which no constant of this kind holds -
// fails here, where the fault is, rather than reporting a document that states its own number
// correctly.
func asAWord(t *testing.T, number int) string {
	t.Helper()
	ones := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight",
		"nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen",
		"seventeen", "eighteen", "nineteen"}
	tens := []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty",
		"ninety"}
	switch {
	case number < 0:
		t.Fatalf("this reader cannot write %d as a word, and no count of a document is negative: "+
			"the entry that asks for it names the wrong constant", number)
		return ""
	case number >= 100:
		return strconv.Itoa(number)
	case number < len(ones):
		return ones[number]
	case number%10 == 0:
		return tens[number/10]
	default:
		return tens[number/10] + "-" + ones[number%10]
	}
}
