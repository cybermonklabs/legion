package main

// The one read of a stored message that the session relay (sessionRelays.takeOverFrame), a
// takeover pass (takenRoleMessage), the kept lane's walk (judgeKeptRoleCopy) and the edge's frame
// rule (edgeFrameVerdict) share, so the four tell a role message by one rule.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// utf8BOM is the byte order mark a writer may put before a frame's JSON. The plugins' shared
// transport reads a frame through TextDecoder, which drops it, so the listener reads past it too.
var utf8BOM = []byte("\xef\xbb\xbf")

// errRoleFrameUnreadable is readRoleFrame's answer for a message no read can tell a role message
// or not: after a leading UTF-8 BOM, it is not a JSON object, or it names no role and its topic or
// its role mark is present and not a string.
var errRoleFrameUnreadable = errors.New("the message's role cannot be read")

// roleFrame is what readRoleFrame reads of a stored message: its top-level fields by their exact
// keys, and the role it is a message of, empty for a message that is no role message.
type roleFrame struct {
	fields map[string]json.RawMessage
	role   string
}

// readRoleFrame reads data, past a leading UTF-8 BOM, as a JSON object (topLevelFields) and tells
// its role from two fields alone, each by its exact key, as the plugins' transport reads them: a
// message whose topic is a role's topic (roleOfLaneTopic) is a role message of that role whatever
// else it carries, and one whose topic names no role is a role message of the role its role mark
// names, in either form, keyed as its claim is (store.RoleClaimKeyOf). A reader that matched keys
// without regard to case, as encoding/json does into a struct, would take a later "TOPIC" key as
// the topic; one that decoded the whole envelope would refuse a message for a field that tells no
// role, such as an event id written as a number; and one with encoding/json's limit on nesting
// would refuse a message with a value nested deeper than 10,000, which JSON.parse reads. Each
// would hand a role message to a session that does not hold the role. A null field is an absent
// one. The answer is errRoleFrameUnreadable for a message no read can tell a role message or not.
func readRoleFrame(data []byte) (roleFrame, error) {
	fields, err := topLevelFields(bytes.TrimPrefix(data, utf8BOM))
	if err != nil {
		return roleFrame{}, fmt.Errorf("%w: it is not a JSON object: %w", errRoleFrameUnreadable, err)
	}
	frame := roleFrame{fields: fields}
	topic, topicErr := frame.text("topic")
	mark, markErr := frame.text("role")
	if role, ok := roleOfLaneTopic(topic); ok {
		frame.role = role
		return frame, nil
	}
	if mark != "" {
		frame.role = store.RoleClaimKeyOf(mark)
		if frame.role != "" {
			return frame, nil
		}
	}
	if err := errors.Join(topicErr, markErr); err != nil {
		return roleFrame{}, fmt.Errorf("%w: %w", errRoleFrameUnreadable, err)
	}
	return frame, nil
}

// topLevelFields reads data, past JSON whitespace, as a JSON object and returns its top-level
// fields by their exact keys, the last value of a repeated key winning, as the plugins' JSON.parse
// keeps them. It finds each value's end by its brackets and strings alone, at any depth:
// encoding/json refuses a document nested deeper than 10,000 and JSON.parse does not, so a reader
// with that limit would tell a role message with a value nested that deep a message whose role
// cannot be read, and the session's plugin would read it as the role message it is. It refuses a
// document that is not a JSON object, such as `null` or an array, which JSON.parse reads but which
// holds no topic or role mark to tell, and otherwise only what JSON.parse refuses too: a string
// with no end, a key that is no string or holds a bad escape, a missing colon or comma, a value
// that is missing, an object or array with no end, and text after the object. It checks no more of
// a value than where it ends, so a value JSON.parse would refuse is kept as its raw text, which the
// field's own read (roleFrame.text, roleFrame.envelope) refuses in turn.
func topLevelFields(data []byte) (map[string]json.RawMessage, error) {
	i := skipJSONSpace(data, 0)
	if i == len(data) || data[i] != '{' {
		return nil, errors.New("it does not start with {")
	}
	fields := map[string]json.RawMessage{}
	i = skipJSONSpace(data, i+1)
	if i < len(data) && data[i] == '}' {
		return fields, endOfJSONDocument(data, i+1)
	}
	for {
		if i == len(data) || data[i] != '"' {
			return nil, fmt.Errorf("the key at byte %d is not a string", i)
		}
		keyEnd, err := endOfJSONString(data, i)
		if err != nil {
			return nil, err
		}
		key, err := jsonKey(data[i:keyEnd])
		if err != nil {
			return nil, err
		}
		i = skipJSONSpace(data, keyEnd)
		if i == len(data) || data[i] != ':' {
			return nil, fmt.Errorf("the key %q has no colon after it", key)
		}
		start := skipJSONSpace(data, i+1)
		end, err := endOfJSONValue(data, start)
		if err != nil {
			return nil, fmt.Errorf("the value of %q: %w", key, err)
		}
		fields[key] = json.RawMessage(data[start:end])
		i = skipJSONSpace(data, end)
		if i == len(data) {
			return nil, errors.New("the object has no end")
		}
		switch data[i] {
		case ',':
			i = skipJSONSpace(data, i+1)
		case '}':
			return fields, endOfJSONDocument(data, i+1)
		default:
			return nil, fmt.Errorf("byte %d is neither a comma nor the object's end", i)
		}
	}
}

// jsonKey is the key a JSON string token names: its text, with any escape read as encoding/json
// reads one, so a key written top\u0069c is the topic.
func jsonKey(token []byte) (string, error) {
	if bytes.IndexByte(token, '\\') < 0 {
		return string(token[1 : len(token)-1]), nil
	}
	var key string
	if err := json.Unmarshal(token, &key); err != nil {
		return "", fmt.Errorf("the key %s: %w", token, err)
	}
	return key, nil
}

// endOfJSONValue is the index just past the JSON value that starts at data[i]: a string, an
// object or array found by counting its brackets outside strings, with no limit on depth, or a
// scalar that runs to the next comma, closing bracket or whitespace.
func endOfJSONValue(data []byte, i int) (int, error) {
	if i == len(data) {
		return 0, errors.New("it is missing")
	}
	switch data[i] {
	case '"':
		return endOfJSONString(data, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(data); j++ {
			switch data[j] {
			case '"':
				end, err := endOfJSONString(data, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, errors.New("an object or array has no end")
	}
	j := i
	for j < len(data) && !strings.ContainsRune(",}] \t\n\r", rune(data[j])) {
		j++
	}
	if j == i {
		return 0, errors.New("it is missing")
	}
	return j, nil
}

// endOfJSONString is the index just past the JSON string whose opening quote is data[i]; a
// backslash escapes the byte after it.
func endOfJSONString(data []byte, i int) (int, error) {
	for j := i + 1; j < len(data); j++ {
		switch data[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		}
	}
	return 0, fmt.Errorf("the string at byte %d has no end", i)
}

// endOfJSONDocument refuses text after the object that ends at data[i-1], as JSON.parse does.
func endOfJSONDocument(data []byte, i int) error {
	if skipJSONSpace(data, i) != len(data) {
		return errors.New("text follows the object")
	}
	return nil
}

// skipJSONSpace is the index of the first byte at or after i that is not JSON whitespace.
func skipJSONSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

// text is the string the frame's field key holds, empty when the field is absent or null, and an
// error naming the field when it holds a value of another type.
func (f roleFrame) text(key string) (string, error) {
	raw, ok := f.fields[key]
	if !ok {
		return "", nil
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("its %s is %s, not a string", key, raw)
	}
	if value == nil {
		return "", nil
	}
	return *value, nil
}

// envelope is the frame's envelope read field by field, each by its exact key: a field whose value
// the envelope's type for it does not take is left unset, rather than refusing the whole message
// as a decode into the envelope would, and an integer field written as a JSON number with a zero
// fraction, such as 1795191371979.0, is read as that integer, while one written as a JSON string is
// left unset, as encoding/json leaves it (wholeNumber). Nothing here applies the wire rules
// or the envelope's own (decodeStoredEnvelope), so a message is copied and forwarded whatever they
// would say of it: an urgency or a source a newer build adds is a string the envelope holds, and
// the copy keeps it for the holder's reader to judge; an expects_reply word ExpectsReply does not
// know is a value that field cannot hold, so the copy leaves it out, and the holder reads the
// message with no expects_reply. Every field of the envelope is read this way, the kept lane's
// walk needing them all - the dedupe key, the source, the sender's session, the role mark and the
// payload to judge a copy, and every field to forward it - and a field a later version of the
// envelope adds is read the same way with no change here.
func (f roleFrame) envelope() contracts.Envelope {
	var item contracts.Envelope
	value := reflect.ValueOf(&item).Elem()
	for i := range value.NumField() {
		field := value.Type().Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		raw, ok := f.fields[name]
		if !ok || name == "" || name == "-" {
			continue
		}
		slot := reflect.New(field.Type)
		if err := json.Unmarshal(raw, slot.Interface()); err == nil {
			value.Field(i).Set(slot.Elem())
			continue
		}
		if whole, ok := wholeNumber(raw); ok {
			switch {
			case field.Type.Kind() == reflect.Int64:
				value.Field(i).SetInt(whole)
			case field.Type.Kind() == reflect.Pointer && field.Type.Elem().Kind() == reflect.Int64:
				value.Field(i).Set(reflect.ValueOf(&whole))
			}
		}
	}
	return item
}

// wholeNumber is raw read as a JSON number with no fraction that an int64 holds, and whether it is
// one. A JSON string that holds a number is none: encoding/json refuses it for an integer, and the
// envelope's schema types such a field as an integer.
func wholeNumber(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || raw[0] == '"' {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || parsed != math.Trunc(parsed) || math.Abs(parsed) >= math.MaxInt64 {
		return 0, false
	}
	return int64(parsed), true
}
