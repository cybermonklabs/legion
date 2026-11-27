package main

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestTheEdgeAndTheRelayReadAFramesFieldsAlike runs the edge's read of a frame's field
// (exactFrameField, which edgeFrameVerdict judges a frame by) and the relay's (readRoleFrame, which
// the session relay, a takeover pass, the kept lane's walk and the edge's role read tell a role
// message by) over the same frames, and requires both to read the topic and the role mark the row
// names wherever both read the frame: past one byte order mark, with a key's escapes read, past a
// value nested deeper than encoding/json reads and past brackets and quotes inside strings. Each
// row names where the two may part: the relay refuses a frame with text after its object, as
// JSON.parse does, which the edge still reads and judges by frameIsJSON, and the edge refuses a
// field readers disagree on (errAmbiguousFrameKey, errPrototypeFrameKey), under which it reads
// none. Both read through one reader of a frame's top-level fields (topLevelJSONFields); a second
// reader for either call site that splits one of these frames' fields differently makes its row
// fail, and one that agrees with it on every row here passes them all.
func TestTheEdgeAndTheRelayReadAFramesFieldsAlike(t *testing.T) {
	t.Parallel()
	const bom = "\xef\xbb\xbf"
	const plain = `{"topic":"notifications.agent.ses_v","role":"legion-priv-priv-1-tester"}`
	const topic, role = "notifications.agent.ses_v", "legion-priv-priv-1-tester"
	for _, tc := range []struct {
		name string
		data []byte
		// topic and role are the fields both read, empty for one the frame does not name.
		topic, role string
		// withheld is the edge's answer for the frame's topic key, and for its role key when it is
		// errPrototypeFrameKey; the edge reads no field under it.
		withheld error
		// relay is whether readRoleFrame reads the frame.
		relay bool
	}{
		{"a frame's topic and role mark", []byte(plain), topic, role, nil, true},
		{"past one byte order mark", []byte(bom + plain), topic, role, nil, true},
		{"keys written with escapes", []byte(`{"top\u0069c":"` + topic + `","r\u006fle":"` + role + `"}`), topic, role, nil, true},
		{"beside a value nested 10,001 deep", nestedDeep([]byte(plain)), topic, role, nil, true},
		{"after brackets, a comma and an escaped quote in a string", []byte(`{"x":"]},\"{[","topic":"` + topic + `"}`), topic, "", nil, true},
		{"after a misspelt literal", []byte(`{"topic":"` + topic + `","x":tru,"role":"` + role + `"}`), topic, role, nil, true},
		{"with JSON whitespace around every token", []byte(" \n{ \"topic\" :\t\"" + topic + "\" ,\r\"role\": \"" + role + "\" }\n"), topic, role, nil, true},
		{"a topic that is no string beside a role mark", []byte(`{"topic":7,"role":"` + role + `"}`), "", role, nil, true},
		{"a null topic", []byte(`{"topic":null}`), "", "", nil, true},
		{"an empty object", []byte(`{}`), "", "", nil, true},
		{"text after the object, which the relay refuses and the edge reads", []byte(plain + " x"), topic, role, nil, false},
		{"two byte order marks", []byte(bom + bom + plain), "", "", nil, false},
		{"an array", []byte(`[` + plain + `]`), "", "", nil, false},
		{"an object with no end", []byte(`{"topic":"` + topic + `"`), "", "", nil, false},
		{"a key with a bad escape", []byte(`{"top\x":"` + topic + `"}`), "", "", nil, false},
		{"a repeated topic key", []byte(`{"topic":"` + topic + `","topic":"notifications.agent.ses_w"}`), "", "", errAmbiguousFrameKey, true},
		{"a __proto__ key", []byte(`{"__proto__":{"topic":"` + topic + `"}}`), "", "", errPrototypeFrameKey, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, relayErr := readRoleFrame(tc.data)
			if (relayErr == nil) != tc.relay {
				t.Fatalf("readRoleFrame(%s) = %v; want a read %t", tc.data, relayErr, tc.relay)
			}
			for _, field := range []struct{ key, want string }{{"topic", tc.topic}, {"role", tc.role}} {
				wantErr := tc.withheld
				if field.key == "role" && !errors.Is(tc.withheld, errPrototypeFrameKey) {
					wantErr = nil
				}
				value, named, err := exactFrameField(tc.data, field.key)
				if !errors.Is(err, wantErr) || (err != nil) != (wantErr != nil) {
					t.Fatalf("exactFrameField(%s, %q) answers %v, want %v", tc.data, field.key, err, wantErr)
				}
				if err != nil {
					continue
				}
				if value != field.want || named != (field.want != "") {
					t.Fatalf("the edge reads the %s of %s as %q (named %t), want %q", field.key, tc.data, value, named, field.want)
				}
				if relayErr != nil {
					continue
				}
				if got, _ := fieldText(field.key, map[string]json.RawMessage{"topic": frame.topic, "role": frame.mark}[field.key]); got != field.want {
					t.Fatalf("the relay reads the %s of %s as %q, the edge %q", field.key, tc.data, got, field.want)
				}
			}
		})
	}
}
