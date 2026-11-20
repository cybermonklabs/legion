package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// readRoleFrame refuses a document that is not a JSON object, and each object the plugins'
// JSON.parse refuses at the object's own level, as its doc comment says (topLevelFields), and tells
// the role of each object JSON.parse reads, however its top level is written. A refused document is
// no role message, so the relay sends it to the session whose subject holds it, as it sends every
// frame that is no role message.
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
				t.Fatalf("encoding/json reads %q as an object; each case must be no JSON object, or an object JSON.parse refuses too", c.data)
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

// envelope reads an integer field written as a JSON number, with or without a zero fraction, and
// leaves out one written as a JSON string that holds a number: encoding/json refuses such a string
// for an integer, and the envelope's schema types the field as an integer, so a copy that took it
// would carry a time the original does not.
func TestTheEnvelopeReadTakesAWholeNumberAndNoStringHoldingOne(t *testing.T) {
	read := func(t *testing.T, data string) (int64, string) {
		t.Helper()
		frame, err := readRoleFrame([]byte(data))
		if err != nil {
			t.Fatalf("readRoleFrame(%q): %v", data, err)
		}
		item := frame.envelope()
		expires := "none"
		if item.ExpiresAt != nil {
			expires = strconv.FormatInt(*item.ExpiresAt, 10)
		}
		return item.IssuedAt, expires
	}
	t.Run("a whole number, and one written with a zero fraction, is read", func(t *testing.T) {
		issued, expires := read(t, `{"topic":"notifications.role.reader","issued_at":1795191371979.0,"expires_at":7}`)
		if issued != 1795191371979 || expires != "7" {
			t.Errorf("envelope reads issued_at %d and expires_at %s; want 1795191371979 and 7", issued, expires)
		}
	})
	t.Run("a string that holds a number is left out", func(t *testing.T) {
		issued, expires := read(t, `{"topic":"notifications.role.reader","issued_at":"1795191371979","expires_at":"7"}`)
		if issued != 0 || expires != "none" {
			t.Errorf("envelope reads issued_at %d and expires_at %s; want 0 and none, as encoding/json reads neither", issued, expires)
		}
	})
}
