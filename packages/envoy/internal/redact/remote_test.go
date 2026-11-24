package redact

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// remoteCategories are the fixture's categories this suite requires, each held by at least one
// row: the hostile inputs of the rule's grammar and of the reviews, the benign forms whose text
// must print as before, and the known residual, a form the rule prints by design (a bare
// "label: secret@host"), pinned so a change to it shows. The TypeScript twin requires the same
// list (packages/contracts/src/remote-text.test.ts).
var remoteCategories = []string{
	"probe", "text-after-query", "value-split", "fragment", "scheme-relative", "backslash", "quote",
	"space", "tab", "line-feed", "angle", "backtick", "percent", "percent-encoded-url",
	"percent-encoded-twice", "mixed-encoding", "unicode-escape", "hex-escape", "equals-in-token",
	"question-in-token", "line-separator", "control", "whitespace-split", "known-residual", "non-ascii", "at-after-authority", "two-at",
	"port-not-digits", "authority-without-slashes", "colon-at-before-start", "at-after-url",
	"trailing-punctuation", "trailing-punctuation-in-value", "json-body", "two-urls",
	"bare-segment", "ipv6", "scheme-case", "other-scheme", "user-only", "r4-no-url",
	"r4-clean-url", "r4-prose-after-url", "r4-percent", "r4-email-without-url", "r4-not-a-url",
	"r4-empty", "r4-unreadable-clean", "html-reference", "r4-html-text", "brace-escape",
	"percent-u-escape", "long-unicode-escape", "octal-escape", "full-width", "r4-full-width-text",
	"path-parameter", "scheme-not-a-scheme", "html-reference-full-width", "reference-depth",
	"field-length",
}

// remoteRow is one row of testdata/remote.json.
type remoteRow struct {
	ID       string     `json:"id"`
	Category string     `json:"category"`
	Field    string     `json:"field"`
	Readable bool       `json:"readable"`
	Printed  *string    `json:"printed"`
	Secrets  []string   `json:"secrets"`
	Repeat   *rowRepeat `json:"repeat"`
}

// rowRepeat is how a row writes a long field in one short line: its field holds repeatAt where
// Part stands Count times over (expanded), as a field nested thousands of references deep does.
type rowRepeat struct {
	Part  string `json:"part"`
	Count int    `json:"count"`
}

// repeatAt is where a row with a repeat writes its repeated part.
const repeatAt = "{repeat}"

// expanded is row's field with its repeat written out, and whether the row's repeat is one: a part,
// a count above zero, and a field that holds repeatAt. A row with no repeat is its field as it is.
func (row remoteRow) expanded() (string, bool) {
	if row.Repeat == nil {
		return row.Field, true
	}
	if row.Repeat.Part == "" || row.Repeat.Count < 1 || !strings.Contains(row.Field, repeatAt) {
		return "", false
	}
	return strings.ReplaceAll(row.Field, repeatAt, strings.Repeat(row.Repeat.Part, row.Repeat.Count)), true
}

// readRemoteFixture reads testdata/remote.json, each row's field with its repeat written out
// (expanded), and fails t on a repeated id, a row of a category this suite does not know, a
// required category no row holds, a readable row with no printed, a repeat that is not one, or a
// secret missing from its row's field.
func readRemoteFixture(t *testing.T) []remoteRow {
	t.Helper()
	data, err := os.ReadFile("testdata/remote.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		NotReadable string      `json:"not_readable"`
		Rows        []remoteRow `json:"rows"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.NotReadable != WordsNotReadable {
		t.Fatalf("the fixture's not_readable is %q, want WordsNotReadable, %q", fixture.NotReadable, WordsNotReadable)
	}
	known := map[string]int{}
	for _, c := range remoteCategories {
		known[c] = 0
	}
	ids := map[string]bool{}
	for i := range fixture.Rows {
		row := &fixture.Rows[i]
		if ids[row.ID] {
			t.Errorf("the fixture repeats the id %q", row.ID)
		}
		ids[row.ID] = true
		field, ok := row.expanded()
		if !ok {
			t.Errorf("row %q's repeat %+v names no part, no count above zero, or no %s in its field", row.ID, *row.Repeat, repeatAt)
		}
		row.Field = field
		if _, ok := known[row.Category]; !ok {
			t.Errorf("row %q has the category %q, which this suite does not require", row.ID, row.Category)
		}
		known[row.Category]++
		if row.Readable != (row.Printed != nil) {
			t.Errorf("row %q is readable=%t with printed=%v: a readable row names what it prints, and only one", row.ID, row.Readable, row.Printed)
		}
		for _, secret := range row.Secrets {
			if !strings.Contains(row.Field, secret) {
				t.Errorf("row %q's secret %q is not in its field, so the row cannot show it is dropped", row.ID, secret)
			}
		}
	}
	for _, c := range remoteCategories {
		if known[c] == 0 {
			t.Errorf("no fixture row holds the required category %q", c)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return fixture.Rows
}

// TestRemotePrintsEveryCorpusRowAsItsPrinted: Remote reads each fixture row as the row says - a
// readable row as its printed, which holds none of its secrets, and any other row as not readable
// - and Field prints the first on one line and the second as WordsNotReadable. The fixture holds
// every required category, and no id twice.
func TestRemotePrintsEveryCorpusRowAsItsPrinted(t *testing.T) {
	for _, row := range readRemoteFixture(t) {
		t.Run(row.ID, func(t *testing.T) {
			got, ok := Remote(row.Field)
			field := Field(row.Field)
			// The designated assertion of cmd/redact-gate's control (1): the output the rule gave,
			// captured, and a failure naming fixture/<id> where it holds one of the row's secrets.
			redacttest.Capture(t, "fixture/"+row.ID, got+"\n"+field)
			for _, secret := range row.Secrets {
				if strings.Contains(got, secret) || strings.Contains(field, secret) {
					t.Errorf("REDACT-ASSERT fixture/%s: Remote(%q) = %q, Field = %q, which holds the secret %q", row.ID, row.Field, got, field, secret)
				}
			}
			switch {
			case ok != row.Readable:
				t.Errorf("Remote(%q) reads it as readable=%t, want %t", row.Field, ok, row.Readable)
			case ok && got != *row.Printed:
				t.Errorf("Remote(%q) = %q, want %q", row.Field, got, *row.Printed)
			case ok && field != OneLine(*row.Printed):
				t.Errorf("Field(%q) = %q, want %q", row.Field, field, OneLine(*row.Printed))
			case !ok && (got != "" || field != WordsNotReadable):
				t.Errorf("Remote(%q) = %q, Field = %q, want \"\" and WordsNotReadable", row.Field, got, field)
			}
		})
	}
}
