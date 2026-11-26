package main

// The one read of a stored message that the session relay (sessionRelays.takeOverFrame), a
// takeover pass (takenRoleMessage), the kept lane's walk (judgeKeptRoleCopy) and the edge's frame
// rule (edgeFrameVerdict) share, so the four tell a role message by one rule, and the one reader
// of a frame's top-level fields (topLevelJSONFields) under it and under the edge's exact-key read
// of a field (exactJSONString), so the edge and the relay read each field of a frame alike.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

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

// roleFrame is what readRoleFrame reads of a stored message: the message past its byte order mark,
// which envelope reads whole, the raw values of its topic, its role mark and its event id by their
// exact keys, nil for a field the message does not hold, and the role it is a message of, empty
// for a message that is no role message.
type roleFrame struct {
	data                 []byte
	topic, mark, eventID json.RawMessage
	role                 string
}

// readRoleFrame reads data, past a leading UTF-8 BOM, as a JSON object (eachTopLevelField) and
// tells its role from two fields alone, each by its exact key: the topic, as the plugins'
// transport reads it (InboundEnvelopeSchema in packages/envoy-client/src/delivery.ts, whose topic
// renderInbound reads), and the role mark, which no plugin reads from an inbound frame, so the
// rule for it is the listener's own. A message whose topic is a role's topic (roleOfLaneTopic) is
// a role message of that role whatever else it carries, and one whose topic names no role is a
// role message of the role its role mark names, in either form, keyed as its claim is
// (store.RoleClaimKeyOf). A reader that matched keys without regard to case, as encoding/json does
// into a struct, would take a later "TOPIC" key as the topic, which the plugins' transport does
// not; one that decoded the whole envelope would refuse a message for a field that tells no role,
// such as an event id written as a number; and one with encoding/json's limit on nesting would
// refuse a message with a value nested deeper than 10,000, which JSON.parse reads. Each would hand
// a role message to a session that does not hold the role. A null field is an absent one.
//
// The read keeps the topic, the role mark and the event id alone, and leaves every other field to
// envelope: the session relay makes this read of every frame it judges, before it reads a claim
// (sessionRelays.takeOverFrame), so it makes no allocation for each key a frame holds, and reads
// the others only for a role message it copies. Its error names a field and the JSON type it
// holds, or a key quoted and cut to shownChars characters, and never a whole value or key, which
// can be as long as the message: the kept lane's walk logs it (judgeKeptRoleCopy). The answer is
// errRoleFrameUnreadable for a message no read can tell a role message or not.
func readRoleFrame(data []byte) (roleFrame, error) {
	frame := roleFrame{data: bytes.TrimPrefix(data, utf8BOM)}
	err := eachTopLevelField(frame.data, func(key, value []byte) {
		switch {
		case keyNames(key, "topic"):
			frame.topic = value
		case keyNames(key, "role"):
			frame.mark = value
		case keyNames(key, "event_id"):
			frame.eventID = value
		}
	})
	if err != nil {
		return roleFrame{}, fmt.Errorf("%w: it is not a JSON object: %w", errRoleFrameUnreadable, err)
	}
	topic, topicErr := fieldText("topic", frame.topic)
	mark, markErr := fieldText("role", frame.mark)
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

// eachTopLevelField reads data, past JSON whitespace, as a JSON object, and calls visit with each
// of its top-level fields in order: the key's token as written, its quotes and escapes included,
// and the value's raw text (topLevelJSONFields). A repeated key is visited each time, so a caller
// that keeps the last value keeps what the plugins' JSON.parse does. It refuses, beside what
// topLevelJSONFields refuses, text after the object, as JSON.parse does (endOfJSONDocument).
func eachTopLevelField(data []byte, visit func(key, value []byte)) error {
	end, err := topLevelJSONFields(data, visit)
	if err != nil {
		return err
	}
	return endOfJSONDocument(data, end)
}

// topLevelJSONFields reads data, past JSON whitespace, as a JSON object, calls visit with each of
// its top-level fields in order - the key's token as written, its quotes and escapes included, and
// the value's raw text - and answers the index just past the object's closing brace. It is the one
// reader of a frame's top-level fields: the relay's (eachTopLevelField, under readRoleFrame) and
// the edge's (exactJSONField, exactJSONObject) both read through it, so the two cannot split a
// frame's fields differently. It finds each value's end by its brackets and strings alone, at any
// depth: encoding/json refuses a document nested deeper than 10,000 and JSON.parse does not, so a
// reader with that limit would tell a role message with a value nested that deep a message whose
// role cannot be read, and the session's plugin would read it as the role message it is. It
// refuses a document that is not a JSON object, such as `null` or an array, which JSON.parse reads
// but which holds no topic or role mark to tell, and otherwise only what JSON.parse refuses too: a
// string with no end, a key that is no string or holds a bad escape, a missing colon or comma, a
// value that is missing, and an object or array with no end. It reads nothing after the closing
// brace, which each caller judges (eachTopLevelField, frameIsJSON), and checks no more of a value
// than where it ends, so a value JSON.parse would refuse is kept as its raw text, which the field's
// own read (fieldText, roleFrame.envelope, exactJSONString) refuses in turn. Its error names a key
// by its first shownChars quoted characters and where it starts.
func topLevelJSONFields(data []byte, visit func(key, value []byte)) (int, error) {
	i := skipJSONSpace(data, 0)
	if i == len(data) || data[i] != '{' {
		return 0, errors.New("it does not start with {")
	}
	i = skipJSONSpace(data, i+1)
	if i < len(data) && data[i] == '}' {
		return i + 1, nil
	}
	for {
		if i == len(data) || data[i] != '"' {
			return 0, fmt.Errorf("the key at byte %d is not a string", i)
		}
		keyEnd, err := endOfJSONString(data, i)
		if err != nil {
			return 0, err
		}
		key := data[i:keyEnd]
		if !jsonStringTextValid(key[1 : len(key)-1]) {
			return 0, fmt.Errorf("the key %s at byte %d holds a bad escape", shown(key[1:len(key)-1]), i)
		}
		keyAt := i
		i = skipJSONSpace(data, keyEnd)
		if i == len(data) || data[i] != ':' {
			return 0, fmt.Errorf("the key %s at byte %d has no colon after it", shown(key[1:len(key)-1]), keyAt)
		}
		start := skipJSONSpace(data, i+1)
		end, err := endOfJSONValue(data, start)
		if err != nil {
			return 0, fmt.Errorf("the value of the key %s at byte %d: %w", shown(key[1:len(key)-1]), keyAt, err)
		}
		visit(key, data[start:end])
		i = skipJSONSpace(data, end)
		if i == len(data) {
			return 0, errors.New("the object has no end")
		}
		switch data[i] {
		case ',':
			i = skipJSONSpace(data, i+1)
		case '}':
			return i + 1, nil
		default:
			return 0, fmt.Errorf("byte %d is neither a comma nor the object's end", i)
		}
	}
}

// jsonStringTextValid reports whether text, the text between a JSON string's quotes, is what
// encoding/json's scanner takes there, for text that holds an escape: each backslash before one of
// " \ / b f n r t, or before u and four hex digits, and no control character. Text with no escape
// is taken whole, as the reader has always taken a key with none. A key that passes is one
// encoding/json decodes (jsonKey), and keyNames reads its escapes alike.
func jsonStringTextValid(text []byte) bool {
	if bytes.IndexByte(text, '\\') < 0 {
		return true
	}
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c < 0x20:
			return false
		case c == '\\':
			i++
			if i == len(text) {
				return false
			}
			switch text[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if len(text)-i-1 < 4 {
					return false
				}
				for _, digit := range text[i+1 : i+5] {
					if _, ok := hexDigit(digit); !ok {
						return false
					}
				}
				i += 4
			default:
				return false
			}
		}
	}
	return true
}

// keyNames reports whether key, a JSON string token eachTopLevelField took, names name, a key of
// ASCII letters and underscores: its text, each escape read as encoding/json reads one, so a key
// written top\u0069c names the topic. It reads into no memory of its own, so the relay's first
// read of a frame makes no allocation for each of its keys. A \u escape of a character past ASCII,
// a surrogate among them, is no character of name.
func keyNames(key []byte, name string) bool {
	text := key[1 : len(key)-1]
	if bytes.IndexByte(text, '\\') < 0 {
		return string(text) == name
	}
	n := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\\' {
			i++
			switch text[i] {
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'u':
				var r rune
				for _, digit := range text[i+1 : i+5] {
					value, _ := hexDigit(digit)
					r = r<<4 | value
				}
				i += 4
				if r >= 0x80 {
					return false
				}
				c = byte(r)
			default:
				c = text[i]
			}
		}
		if n == len(name) || name[n] != c {
			return false
		}
		n++
	}
	return n == len(name)
}

// hexDigit is the value of the hex digit digit, and whether it is one.
func hexDigit(digit byte) (rune, bool) {
	switch {
	case '0' <= digit && digit <= '9':
		return rune(digit - '0'), true
	case 'a' <= digit && digit <= 'f':
		return rune(digit-'a') + 10, true
	case 'A' <= digit && digit <= 'F':
		return rune(digit-'A') + 10, true
	}
	return 0, false
}

// shownChars is how many characters of a key, as an error of the read quotes it, the error holds: a
// key, like a value, can be as long as the message, and the kept lane's walk logs the error whole
// (judgeKeptRoleCopy). The bound is on the quoted text, not on the key's bytes: the quote writes a
// byte that is no UTF-8 or a control byte as four characters (\xff), and a rune it cannot print as
// up to ten (\U0010ffff).
const shownChars = 40

// shown is text quoted for an error, with as many of its runes as fit in shownChars quoted
// characters, whole, followed by its length when the quote holds less than all of it.
func shown(text []byte) string {
	var quoted strings.Builder
	quoted.WriteByte('"')
	i := 0
	for i < len(text) {
		_, size := utf8.DecodeRune(text[i:])
		part := strconv.Quote(string(text[i : i+size]))
		part = part[1 : len(part)-1]
		if quoted.Len()-1+len(part) > shownChars {
			break
		}
		quoted.WriteString(part)
		i += size
	}
	quoted.WriteByte('"')
	if i == len(text) {
		return quoted.String()
	}
	return fmt.Sprintf("%s... (%d bytes)", quoted.String(), len(text))
}

// jsonKey is the key a JSON string token eachTopLevelField took names: its text, with any escape
// read as encoding/json reads one, so a key written top\u0069c is the topic.
func jsonKey(token []byte) string {
	if bytes.IndexByte(token, '\\') < 0 {
		return string(token[1 : len(token)-1])
	}
	var key string
	// eachTopLevelField refuses a key whose escapes encoding/json does not take
	// (jsonStringTextValid), so this decode does not fail.
	_ = json.Unmarshal(token, &key)
	return key
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

// fieldText is the string raw, the value of the frame's field name, holds: empty when the field is
// absent or null, and an error naming the field and the JSON type it holds when that is not a
// string. The error holds nothing of the value, which can be as long as the message.
func fieldText(name string, raw json.RawMessage) (string, error) {
	if raw == nil || string(raw) == "null" {
		return "", nil
	}
	var value string
	if raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("its %s is %s, not a string", name, jsonType(raw))
	}
	return value, nil
}

// jsonType names the JSON type of raw, a value eachTopLevelField kept as its raw text. It reads
// only the first byte of an object, an array or a string, so it names one nested at any depth.
func jsonType(raw json.RawMessage) string {
	switch raw[0] {
	case '{':
		return "a JSON object"
	case '[':
		return "a JSON array"
	case '"':
		return "a JSON string that does not decode"
	}
	switch {
	case string(raw) == "true" || string(raw) == "false":
		return "a JSON boolean"
	case json.Valid(raw):
		return "a JSON number"
	}
	return "no JSON value"
}

// envelope is the frame's envelope read field by field, each by its exact key, from every
// top-level field the message holds, which readRoleFrame left to this read: the session relay
// reads them only for a role message it copies. A field whose value the envelope's type for it
// does not take is left unset, rather than refusing the whole message as a decode into the
// envelope would, and an integer field written as a JSON number with a zero fraction, such as
// 1795191371979.0, is read as that integer, while one written as a JSON string is left unset, as
// encoding/json leaves it (wholeNumber). Nothing here applies the wire rules or the envelope's own
// (decodeStoredEnvelope), so a message is copied and forwarded whatever they would say of it: an
// urgency or a source a newer build adds is a string the envelope holds, and the copy keeps it for
// the holder's reader to judge; an expects_reply word ExpectsReply does not know is a value that
// field cannot hold, so the copy leaves it out, and the holder reads the message with no
// expects_reply. Every field of the envelope is read this way, the kept lane's walk needing them
// all - the dedupe key, the source, the sender's session, the role mark and the payload to judge a
// copy, and every field to forward it - and a field a later version of the envelope adds is read
// the same way with no change here.
func (f roleFrame) envelope() contracts.Envelope {
	fields := map[string]json.RawMessage{}
	// readRoleFrame read these bytes by the same walk with no error, so this walk ends the same
	// way; the last value of a repeated key wins, as JSON.parse keeps it.
	_ = eachTopLevelField(f.data, func(key, value []byte) { fields[jsonKey(key)] = value })
	var item contracts.Envelope
	value := reflect.ValueOf(&item).Elem()
	for i := range value.NumField() {
		field := value.Type().Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		raw, ok := fields[name]
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
