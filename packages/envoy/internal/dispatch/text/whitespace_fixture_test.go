package text

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// goSpaceFixturePath is the committed list of the scalars Go's unicode.IsSpace reports, read by
// relative path as the other shared fixtures of this repository are. Its other reader is a Bun
// test of packages/envoy-client, which holds goBlankPattern - the same set written out as a
// regular expression - to the same list.
const goSpaceFixturePath = "../../../../contracts/fixtures/go-unicode-isspace.json"

// goSpaceCodePoints writes scalars as a reader meets them in a Unicode table: U+0085, U+200A.
func goSpaceCodePoints(scalars []rune) string {
	written := make([]string, len(scalars))
	for index, scalar := range scalars {
		written[index] = fmt.Sprintf("U+%04X", scalar)
	}
	return strings.Join(written, ", ")
}

// TestUnicodeIsSpaceReportsTheCommittedScalars measures this Go's unicode.IsSpace over every
// scalar from 0 to 10FFFF and holds the answer to the committed list. Four unit rows elsewhere
// pin the two characters JavaScript's trim and Go's strings.TrimSpace disagree on; nothing but
// this test holds the whole set, so a Go release or a Unicode version that moves it, or a hand
// edit that drops one scalar, is red here rather than silent.
func TestUnicodeIsSpaceReportsTheCommittedScalars(t *testing.T) {
	raw, err := os.ReadFile(goSpaceFixturePath)
	if err != nil {
		t.Fatalf("read the committed list: %v", err)
	}
	var fixture struct {
		Scalars []string `json:"scalars"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse the committed list: %v", err)
	}
	if len(fixture.Scalars) == 0 {
		t.Fatalf("%s holds no scalar, so it says nothing about unicode.IsSpace", goSpaceFixturePath)
	}
	listed := make(map[rune]bool, len(fixture.Scalars))
	for _, written := range fixture.Scalars {
		code, err := strconv.ParseUint(written, 16, 32)
		if err != nil || code > unicode.MaxRune {
			t.Fatalf("%s lists %q, which is no code point written in hexadecimal between 0 and 10FFFF",
				goSpaceFixturePath, written)
		}
		listed[rune(code)] = true
	}
	if len(listed) != len(fixture.Scalars) {
		t.Fatalf("%s lists %d scalars but names only %d of them: it holds the same code point twice",
			goSpaceFixturePath, len(fixture.Scalars), len(listed))
	}

	var reported, missing []rune
	for scalar := rune(0); scalar <= unicode.MaxRune; scalar++ {
		switch {
		case unicode.IsSpace(scalar) && !listed[scalar]:
			reported = append(reported, scalar)
		case listed[scalar] && !unicode.IsSpace(scalar):
			missing = append(missing, scalar)
		}
	}
	if len(reported) == 0 && len(missing) == 0 {
		return
	}
	var moved []string
	if len(reported) > 0 {
		moved = append(moved, fmt.Sprintf("  this Go's unicode.IsSpace reports %s, which the list does not hold.",
			goSpaceCodePoints(reported)))
	}
	if len(missing) > 0 {
		moved = append(moved, fmt.Sprintf("  the list holds %s, which this Go's unicode.IsSpace does not report.",
			goSpaceCodePoints(missing)))
	}
	t.Fatalf("unicode.IsSpace and %s disagree:\n%s\n"+
		"This test measured this Go over every scalar from 0 to 10FFFF, so the characters above are where "+
		"Go stands now and the list is what Go reported when the list was written: raising Go or its "+
		"Unicode version moves the first line, editing the file by hand moves the second.\n"+
		"goBlankPattern in packages/envoy-client/src/dispatch-execute.ts writes the same set out as a "+
		"regular expression, and `dispatch artifact` reads it to tell whether the Dispatch server will "+
		"call a create's content blank; the server reads strings.TrimSpace, which trims what this test "+
		"measured. Measure the two together: write what this test reports into the list, correct the "+
		"pattern to the same characters, and run this test and `goBlankPattern` > `calls blank exactly "+
		"the scalars the committed list holds` (packages/envoy-client) again. Correcting one alone "+
		"reddens the other.",
		goSpaceFixturePath, strings.Join(moved, "\n"))
}
