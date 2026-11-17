package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// PostgreSQL's text and jsonb cannot hold U+0000, so a statement given caller text holding one
// fails, and a route that reached that statement answered a 500 naming nothing the caller could
// fix. The decoder of every request body (decodeJSON) refuses such text before the route reads
// it, naming where the caller wrote the character. A JSON body can hold no other text PostgreSQL
// refuses, since decoding writes each byte that is not UTF-8 as U+FFFD.

// nulEscape is the only way JSON carries U+0000: a decoder refuses the raw control character.
var nulEscape = []byte(`\u0000`)

// unstorableJSON refuses the first string of the JSON value body holds - a member name or a
// value, an object's members read in name order - that holds U+0000, naming it by its place in
// the body: planReason, tasks[0].text, or "body" for a bare string. It names the place and never
// the string, since a body can carry a secret (refuseBadBody). A body without the escape is passed
// over unread. The value is read as a route's own decoder reads it, the first value in body and
// the last of a repeated member, so what is checked is what is kept; a body that does not decode
// holds nothing a route keeps.
func unstorableJSON(body []byte) error {
	if !bytes.Contains(body, nulEscape) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	return unstorableJSONValue("", value)
}

func unstorableJSONValue(path string, value any) error {
	switch value := value.(type) {
	case string:
		if path == "" {
			path = "body"
		}
		return unstorableText(path, value)
	case []any:
		for index, element := range value {
			if err := unstorableJSONValue(fmt.Sprintf("%s[%d]", path, index), element); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(value)) {
			// A name is written with each U+0000 in it as the six characters \u0000, so no
			// refusal sends the character back.
			member := strings.ReplaceAll(name, "\x00", `\u0000`)
			if path != "" {
				member = path + "." + member
			}
			if err := unstorableText(member, name); err != nil {
				return err
			}
			if err := unstorableJSONValue(member, value[name]); err != nil {
				return err
			}
		}
	}
	return nil
}

// unstorableText refuses value when it holds U+0000, naming place and where the first one
// stands, counted from 1 in code points as the daemon's caps count (planReasonMax).
func unstorableText(place, value string) error {
	at := strings.IndexByte(value, 0)
	if at < 0 {
		return nil
	}
	return fmt.Errorf("%s holds a NUL character (U+0000) at character %d, which the daemon cannot store", place, utf8.RuneCountInString(value[:at])+1)
}
