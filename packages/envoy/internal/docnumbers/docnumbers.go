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

// Number is the one group every pattern gives: digits, or the count written as a word.
const Number = `(\d+|[a-z]+(?:-[a-z]+)?)`

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
			if said := match[1]; said != digits && said != word {
				t.Errorf("%s says %q of %s, want %s or %q: %s",
					want.File, said, want.Says, digits, word, strings.TrimSpace(match[0]))
			}
		}
	}
}

// asAWord gives the count as a sentence that spells it writes it. A count a document would spell
// and this table has no word for is a count this reader cannot read: it fails here, where the
// fault is, rather than reporting a document that states its own number correctly. A count of a
// hundred or more is never spelled in prose of this repository, which writes 109 and 218 in
// digits, so none is wanted there.
func asAWord(t *testing.T, number int) string {
	t.Helper()
	spelled := map[int]string{
		1: "one", 2: "two", 3: "three", 4: "four", 5: "five", 6: "six", 7: "seven", 8: "eight",
		9: "nine", 10: "ten", 11: "eleven", 12: "twelve", 13: "thirteen", 14: "fourteen",
		15: "fifteen", 16: "sixteen", 17: "seventeen", 18: "eighteen", 19: "nineteen",
		20: "twenty", 21: "twenty-one", 22: "twenty-two", 23: "twenty-three", 24: "twenty-four",
		25: "twenty-five", 26: "twenty-six", 27: "twenty-seven", 28: "twenty-eight",
		29: "twenty-nine", 30: "thirty",
	}
	if number >= 100 {
		return strconv.Itoa(number)
	}
	word, ok := spelled[number]
	if !ok {
		t.Fatalf("this reader has no word for %d, which a sentence spelling its count would write: "+
			"add it to docnumbers rather than reading a document as wrong", number)
	}
	return word
}
