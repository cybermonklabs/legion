package api

import (
	"strings"
	"testing"
)

// The refusal of a priority id over priorityIDMax says "an id is at most 200 characters, and this
// one is N", so N is the id's characters, each one code point whatever its script, and the bound is
// held in the same unit. An id of 150 accented letters is inside that bound, so it is refused for
// its grammar and not for its length; one of 201 is refused for its length and named as 201.
func TestAPriorityIDIsMeasuredInCharacters(t *testing.T) {
	for _, row := range []struct {
		name, id, want string
	}{
		// characters: code points, priorityIDRefusal counts the id with utf8.RuneCountInString.
		{name: "control: 201 Latin letters", id: strings.Repeat("a", 201), want: "an id is at most 200 characters, and this one is 201"},
		{name: "201 accented letters", id: strings.Repeat("é", 201), want: "an id is at most 200 characters, and this one is 201"},
		{name: "150 accented letters", id: strings.Repeat("é", 150), want: "is no priority id: an id is lowercase letters and digits"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := priorityIDRefusal(row.id, false); !strings.Contains(got, row.want) {
				t.Fatalf("priorityIDRefusal of %d characters (%d bytes) = %q, want it to say %q",
					len([]rune(row.id)), len(row.id), got, row.want)
			}
		})
	}
}
