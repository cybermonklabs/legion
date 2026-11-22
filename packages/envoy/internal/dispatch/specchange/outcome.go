package specchange

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// An outcome is one thing a change makes different for a caller (R27, D6 of the work-units plan).
// A change has three lists of them, recorded per pull request head: the author's, the reviewer's
// own, written before the reviewer reads the author's, and the reviewer's final list. A conclusion
// block lists them too, one `Outcome:` paragraph each. Every surface writes an outcome as one
// line, with at most one coverage marker in front of its text:
//
//   - `(requires <statement-id>) <text>`: the approved statement that requires the outcome;
//   - `(restored) <text>`: an outcome the change restores for the conclusion's statement;
//   - `<text>`: an outcome that no approved statement covers, kept as written.

// Outcome is one outcome: its text, and the approved statement that requires it (Statement) or
// that it is restored for the conclusion's statement (Restored), never both.
type Outcome struct {
	Text      string
	Statement string
	Restored  bool
}

// statementIDPattern is the form of an id of The Spec a marker names.
var statementIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// IsStatementID is whether id has the form of an id of The Spec.
func IsStatementID(id string) bool { return statementIDPattern.MatchString(id) }

// The two markers of a line, and the opening the requires marker starts with.
const (
	requiresOpening = "(requires"
	restoredMarker  = "(restored)"
)

// ParseOutcome reads one outcome line. A line that opens with a marker must hold one in full -
// `(requires <statement-id>)` or `(restored)` - and a text after it, or it is refused: a marker
// that names no statement id is a writer's mistake, and reading it as text would record another
// outcome than the one meant. Any other nonblank line is an outcome no approved statement covers,
// as written. The space around the line and between the marker and its text is no part of either.
func ParseOutcome(line string) (Outcome, error) {
	line = strings.TrimSpace(line)
	switch {
	case line == "":
		return Outcome{}, errors.New("the line is blank: an outcome is one line of text")
	case strings.HasPrefix(line, restoredMarker):
		text := strings.TrimSpace(line[len(restoredMarker):])
		if text == "" {
			return Outcome{}, fmt.Errorf("%q names no outcome after (restored)", line)
		}
		return Outcome{Text: text, Restored: true}, nil
	case strings.HasPrefix(line, requiresOpening+" ") || strings.HasPrefix(line, requiresOpening+")"):
		marker, text, closed := strings.Cut(line[len(requiresOpening):], ")")
		id := strings.TrimSpace(marker)
		if !closed || !IsStatementID(id) {
			return Outcome{}, fmt.Errorf("%q names no statement id in (requires <id>): an id is lower-case letters, digits, '.', '_' and '-'", line)
		}
		if text = strings.TrimSpace(text); text == "" {
			return Outcome{}, fmt.Errorf("%q names no outcome after (requires %s)", line, id)
		}
		return Outcome{Text: text, Statement: id}, nil
	}
	return Outcome{Text: line}, nil
}

// Line is the outcome in the line form, which ParseOutcome reads back.
func (o Outcome) Line() string {
	switch {
	case o.Statement != "":
		return requiresOpening + " " + o.Statement + ") " + o.Text
	case o.Restored:
		return restoredMarker + " " + o.Text
	}
	return o.Text
}

// OutcomeKey is the key two outcomes are one outcome by: the text with the space at each end
// trimmed, each run of space one space, one full stop at the end dropped, and in lower case. The
// marker is no part of it, so a final list that holds an outcome under another marker than the
// conclusion's still holds that outcome.
func OutcomeKey(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(collapsed, ".")))
}

// CheckOutcomes is the fault of a list of outcomes as a record takes them, nil for none: an
// outcome whose text is blank or holds a line break, one both required and restored, one whose
// statement is no statement id, one whose text opens with a marker, so that its line would read
// back as another outcome, and two outcomes of one key. An empty list has no fault.
func CheckOutcomes(outcomes []Outcome) error {
	seen := make(map[string]string, len(outcomes))
	for index, outcome := range outcomes {
		text := strings.TrimSpace(outcome.Text)
		switch {
		case text == "":
			return fmt.Errorf("outcome %d is blank: give each outcome its text", index+1)
		case strings.ContainsAny(text, "\r\n"):
			return fmt.Errorf("outcome %d holds a line break: an outcome is one line", index+1)
		case outcome.Statement != "" && outcome.Restored:
			return fmt.Errorf("outcome %d is both required by %s and restored: give one marker", index+1, outcome.Statement)
		case outcome.Statement != "" && !IsStatementID(outcome.Statement):
			return fmt.Errorf("outcome %d names %q, which is no statement id: an id is lower-case letters, digits, '.', '_' and '-'", index+1, outcome.Statement)
		}
		trimmed := Outcome{Text: text, Statement: outcome.Statement, Restored: outcome.Restored}
		if back, err := ParseOutcome(trimmed.Line()); err != nil || back != trimmed {
			return fmt.Errorf("outcome %d opens its text with a marker: put the statement in statement or set restored, and keep the marker out of the text", index+1)
		}
		key := OutcomeKey(text)
		if first, held := seen[key]; held {
			return fmt.Errorf("outcomes %s and %d are one outcome: list each outcome once", first, index+1)
		}
		seen[key] = fmt.Sprint(index + 1)
	}
	return nil
}
