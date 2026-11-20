package main

// The one read of a stored message that the session relay (sessionRelays.takeOverFrame), a
// takeover pass (takenRoleMessage) and the kept lane's walk (judgeKeptRoleCopy) share, so the
// three tell a role message, and end a message, by one rule.

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

// readRoleFrame reads data, past a leading UTF-8 BOM, as a JSON object and tells its role from two
// fields alone, each by its exact key, as the plugins' transport reads them: a message whose topic
// is a role's topic (roleOfLaneTopic) is a role message of that role whatever else it carries, and
// one whose topic names no role is a role message of the role its role mark names, in either form
// (contracts.CanonicalRole). A reader that matched keys without regard to case, as encoding/json
// does into a struct, would take a later "TOPIC" key as the topic, and one that decoded the whole
// envelope would refuse a message for a field that tells no role, such as an event id written as a
// number; either would hand a role message to a session that does not hold the role. A null field
// is an absent one. The answer is errRoleFrameUnreadable for a message no read can tell a role
// message or not.
func readRoleFrame(data []byte) (roleFrame, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &fields); err != nil {
		return roleFrame{}, fmt.Errorf("%w: it is not a JSON object: %w", errRoleFrameUnreadable, err)
	}
	if fields == nil {
		return roleFrame{}, fmt.Errorf("%w: it is not a JSON object: it is null", errRoleFrameUnreadable)
	}
	frame := roleFrame{fields: fields}
	topic, topicErr := frame.text("topic")
	mark, markErr := frame.text("role")
	if role, ok := roleOfLaneTopic(topic); ok {
		frame.role = role
		return frame, nil
	}
	if mark != "" {
		_, frame.role, _ = contracts.CanonicalRole(mark)
		if frame.role != "" {
			return frame, nil
		}
	}
	if err := errors.Join(topicErr, markErr); err != nil {
		return roleFrame{}, fmt.Errorf("%w: %w", errRoleFrameUnreadable, err)
	}
	return frame, nil
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
// as a decode into the envelope would, and an integer field written as a JSON number with no
// fraction, such as 1795191371979.0, is read as that integer. Nothing here applies the wire rules
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
// one.
func wholeNumber(raw json.RawMessage) (int64, bool) {
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
