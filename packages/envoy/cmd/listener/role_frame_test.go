package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// readRoleFrame refuses each document the plugins' JSON.parse refuses at the object's own level,
// as its doc comment says (topLevelFields), and tells the role of each object JSON.parse reads,
// however its top level is written. A refused document is no role message, so the relay sends it
// to the session whose subject holds it, as it sends every frame that is no role message.
func TestTheRoleReadRefusesWhatJSONParseRefusesAtTheTopLevel(t *testing.T) {
	const role = `"topic":"notifications.role.reader"`
	refused := []struct{ name, data string }{
		{"not an object", `["notifications.role.reader"]`},
		{"empty", ``},
		{"only whitespace", " \t\r\n"},
		{"a vertical tab before the object", "\v{" + role + "}"},
		{"a form feed before the object", "\f{" + role + "}"},
		{"null", `null`},
		{"a string with no end", `{"topic":"notifications.role.reader`},
		{"a key with no end", `{"top`},
		{"a key that is no string", `{topic:"notifications.role.reader"}`},
		{"a bad escape in a key", `{"top\qic":"notifications.role.reader"}`},
		{"a missing colon", `{"topic" "notifications.role.reader"}`},
		{"a missing comma", `{` + role + ` "role":"reader"}`},
		{"a missing value", `{"x":,` + role + `}`},
		{"a trailing comma", `{` + role + `,}`},
		{"an object with no end", `{` + role},
		{"an object cut after its brace", `{`},
		{"an object cut after a key", `{"topic"`},
		{"an object cut after a colon", `{"topic":`},
		{"an array with no end", `{` + role + `,"nested":[[1]}`},
		{"text after the object", `{` + role + `}x`},
		{"text after an empty object", `{}x`},
		{"a second object", `{` + role + `}{` + role + `}`},
	}
	for _, c := range refused {
		t.Run("refused: "+c.name, func(t *testing.T) {
			var parsed map[string]json.RawMessage
			if json.Unmarshal([]byte(c.data), &parsed) == nil && parsed != nil {
				t.Fatalf("encoding/json reads %q as an object; the case must be one JSON.parse refuses too", c.data)
			}
			if _, err := readRoleFrame([]byte(c.data)); !errors.Is(err, errRoleFrameUnreadable) {
				t.Errorf("readRoleFrame(%q) answers %v; want errRoleFrameUnreadable", c.data, err)
			}
		})
	}

	read := []struct{ name, data string }{
		{"whitespace around the object", " \t\r\n{" + role + "} \n"},
		{"one byte order mark", "\xef\xbb\xbf{" + role + "}"},
		{"brackets, commas and an escaped quote in a string before the topic", `{"payload":"]},{\"[","topic":"notifications.role.reader"}`},
		{"an escaped backslash ending a string before the topic", `{"payload":"a\\",` + role + `}`},
		{"a nested object that holds another topic", `{"meta":{"topic":"notifications.agent.ses_x"},` + role + `}`},
		{"a value nested 100,000 deep", `{"nested":` + strings.Repeat("[", 100_000) + strings.Repeat("]", 100_000) + `,` + role + `}`},
		{"the last of a repeated key", `{"topic":"notifications.agent.ses_x",` + role + `}`},
		{"an escaped key", `{"top\u0069c":"notifications.role.reader"}`},
	}
	for _, c := range read {
		t.Run("read: "+c.name, func(t *testing.T) {
			frame, err := readRoleFrame([]byte(c.data))
			if err != nil || frame.role != "reader" {
				t.Errorf("readRoleFrame answers role %q and %v; want role reader", frame.role, err)
			}
		})
	}
}
