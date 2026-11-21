package api

import (
	"testing"
	"time"
)

// widgetAnswer is a made-up state body, as a PUT of an answer carries one.
type widgetAnswer struct {
	State    string     `json:"state"`
	Selected []string   `json:"selected"`
	Text     *string    `json:"text"`
	Until    *time.Time `json:"until"`
	internal string
}

// A PUT is the no-op of decision 8 only where its whole body equals the object's present state and
// the caller set it, or the object records no setter: another caller's equal PUT, and the
// setter's PUT that changes any one member, are changes. Member by member, a nil and an empty list
// are one state and two times are one at one instant in any zone.
func TestAnEqualPutBySetterChangesNothing(t *testing.T) {
	text, other := "Yes.", "No."
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	sameInstant := at.In(time.FixedZone("UTC+1", 3600))
	later := at.Add(time.Second)
	current := widgetAnswer{State: "answered", Selected: nil, Text: &text, Until: &at, internal: "the row's"}
	equal := widgetAnswer{State: "answered", Selected: []string{}, Text: new(string), Until: &sameInstant, internal: "the body's"}
	*equal.Text = text

	for name, test := range map[string]struct {
		sent, current widgetAnswer
		setter        putSetter
		caller        string
		unchanged     bool
	}{
		"the setter's equal PUT":                         {equal, current, setBy("alice"), "alice", true},
		"another caller's equal PUT":                     {equal, current, setBy("alice"), "bob", false},
		"an equal PUT where nobody set it":               {equal, current, setBy(""), "alice", false},
		"any caller's equal PUT where no setter is kept": {equal, current, noSetterRecorded, "bob", true},
		"the setter's PUT of another state":              {widgetAnswer{State: "declined", Text: &text, Until: &at}, current, setBy("alice"), "alice", false},
		"the setter's PUT of another text":               {widgetAnswer{State: "answered", Text: &other, Until: &at}, current, setBy("alice"), "alice", false},
		"the setter's PUT leaving the text out":          {widgetAnswer{State: "answered", Until: &at}, current, setBy("alice"), "alice", false},
		"the setter's PUT of another selection":          {widgetAnswer{State: "answered", Selected: []string{"a"}, Text: &text, Until: &at}, current, setBy("alice"), "alice", false},
		"the setter's PUT of a later time":               {widgetAnswer{State: "answered", Text: &text, Until: &later}, current, setBy("alice"), "alice", false},
		"any caller's PUT that changes a member":         {widgetAnswer{State: "answered", Text: &other, Until: &at}, current, noSetterRecorded, "bob", false},
	} {
		if got := putChangesNothing(test.sent, test.current, test.setter, test.caller); got != test.unchanged {
			t.Errorf("%s: changes nothing = %v, want %v", name, got, test.unchanged)
		}
	}
}
