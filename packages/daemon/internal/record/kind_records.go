package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// This file joins the outbox's payloads to the records of the one list (texts): a notice is built
// from its kind's record (NoticeOf), the keys that record declares are read off its type
// (NoticeKeys), and a role message carries its kind's record as its fields (NewRoleMessage,
// RoleMessage.Record).

// NoticeOf is the notice that carries rec, the record of a notice or controller-notice kind on
// the list (texts.PhaseFinishedRecord, texts.TickRecord and the others, or a pointer to one). It
// is the record's JSON read back into a Notice with unknown fields refused, so a key the record
// declares that Notice lacks is an error, and the notice holds the keys the record set and no
// other. Its kind is the record's. A record whose kind is no notice, or whose type is not its
// kind's record type, is refused. The notice is not validated here: NewOutboxRow does that.
func NoticeOf(rec any) (Notice, error) {
	entry, encoded, err := entryOfRecord(rec)
	if err != nil {
		return Notice{}, err
	}
	if entry.Form != texts.Notice && entry.Form != texts.ControllerNotice {
		return Notice{}, fmt.Errorf("a %s record is a %s, not a notice", entry.Kind, entry.Form)
	}
	var notice Notice
	if err := decodeStrictly(encoded, &notice); err != nil {
		return Notice{}, fmt.Errorf("the %s record does not read as a notice: %w", entry.Kind, err)
	}
	return notice, nil
}

// ControllerNoticeOf is the controller notice that carries rec (NoticeOf), for an outbox row of
// the controller's own: the one conversion every writer of a controller notice makes.
func ControllerNoticeOf(rec any) (ControllerNotice, error) {
	notice, err := NoticeOf(rec)
	return ControllerNotice(notice), err
}

// NoticeKeys is the keys the record of kind declares, in declaration order, an embedded struct
// with no json name giving its keys in its place (texts.NoticeHead), as encoding/json reads
// them: the keys a notice of kind may carry on the wire. It is nil for a kind that is no notice or
// controller notice on the list.
func NoticeKeys(kind texts.Kind) []string {
	entry, ok := texts.Lookup(kind)
	if !ok || (entry.Form != texts.Notice && entry.Form != texts.ControllerNotice) {
		return nil
	}
	return jsonKeys(nil, reflect.TypeOf(entry.Record))
}

// jsonKeys appends to keys the json names of the fields of the struct type typ, as
// encoding/json names them: a field tagged "-" is left out, an embedded struct with no json name
// gives its own keys in its place, and an unexported field is left out.
func jsonKeys(keys []string, typ reflect.Type) []string {
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				keys = jsonKeys(keys, embedded)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		keys = append(keys, name)
	}
	return keys
}

// entryOfRecord is the entry of rec's kind on the list and rec's JSON. The kind is the one rec's
// JSON names, and rec, or what it points to, must be of that kind's record type.
func entryOfRecord(rec any) (texts.Entry, []byte, error) {
	typ := reflect.TypeOf(rec)
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return texts.Entry{}, nil, fmt.Errorf("encode the %T record: %w", rec, err)
	}
	var head struct {
		Kind texts.Kind `json:"kind"`
	}
	if err := json.Unmarshal(encoded, &head); err != nil {
		return texts.Entry{}, nil, fmt.Errorf("the %T record is no object of fields: %w", rec, err)
	}
	entry, ok := texts.Lookup(head.Kind)
	if !ok {
		return texts.Entry{}, nil, fmt.Errorf("the %T record names kind %q, which is not on the list", rec, head.Kind)
	}
	if want := reflect.TypeOf(entry.Record); typ != want {
		return texts.Entry{}, nil, fmt.Errorf("the record of a %s text is a %s, not a %T", entry.Kind, want, rec)
	}
	return entry, encoded, nil
}

// decodeStrictly decodes the one JSON value of data into into, refusing a key into does not
// declare and anything after the value.
func decodeStrictly(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON after the value")
		}
		return err
	}
	return nil
}
