package texts

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

// leader is a record whose text opens with a line of its own, kept word for word.
type leader interface {
	Lead() string
}

// keeper is a record that carries sentences that another requirement keeps word for word.
type keeper interface {
	Kept() []string
}

// maxSafeInteger is the largest integer that a JavaScript number holds exactly. TOON prints a
// number with String(n), so a larger integer would not print as Go prints it.
const maxSafeInteger = 1<<53 - 1

var (
	timeType     = reflect.TypeFor[time.Time]()
	wordsType    = reflect.TypeFor[Words]()
	wordsPtrType = reflect.TypeFor[*Words]()
)

// Render writes a task record as its text: the lead line, the fields in TOON form, the kept
// sentences, then the blocks of its Words, and nothing else. The text ends with no new line.
// Each block's tag is chosen against the whole text: the lines before the blocks as well as the
// words of every block (blockTags). The package documentation gives the rules.
func Render(record any) (string, error) {
	value := reflect.ValueOf(record)
	methods := record
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return "", fmt.Errorf("texts: render a nil %T", record)
		}
		value = value.Elem()
	case reflect.Struct:
		// A pointer to a copy has the methods of both receivers.
		self := reflect.New(value.Type())
		self.Elem().Set(value)
		methods = self.Interface()
	}
	if value.Kind() != reflect.Struct {
		return "", fmt.Errorf("texts: render a %T: a record is a struct", record)
	}

	var r renderer
	fields, err := r.fields(value, "")
	if err != nil {
		return "", err
	}
	encoded, err := encodeObject(fields)
	if err != nil {
		return "", err
	}
	var lead string
	if l, ok := methods.(leader); ok {
		lead = l.Lead()
	}
	var kept []string
	if k, ok := methods.(keeper); ok {
		kept = k.Kept()
	}
	lines := make([]string, 0, 1+len(encoded)+len(kept)+len(r.blocks))
	if lead != "" {
		lines = append(lines, lead)
	}
	lines = append(lines, encoded...)
	lines = append(lines, kept...)
	lines = append(lines, drawBlocks(lines, r.blocks)...)
	return strings.Join(lines, "\n"), nil
}

// renderer collects the blocks of a record's Words while it reads the record's fields, to draw
// them together once it has them all (drawBlocks).
type renderer struct {
	blocks []block
}

// fields reads the fields of the struct v, in declaration order, as the members of an object.
// A field without a json tag, or with the tag "-", is no field of the record. An embedded
// struct with no json tag gives its fields in its place, as encoding/json reads it. Its type
// must be exported, since reflect cannot read a time or Words through an unexported one. A
// Words field is no member: its block goes to r.blocks. path is empty for the record itself
// and names the record in its list, such as kept[1], for a record of a list.
func (r *renderer) fields(v reflect.Value, path string) (object, error) {
	var members object
	for i := range v.NumField() {
		field := v.Type().Field(i)
		tag, tagged := field.Tag.Lookup("json")
		if tag == "-" {
			continue
		}
		value := v.Field(i)
		if field.Anonymous && !tagged {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() != reflect.Struct {
				continue
			}
			if !field.IsExported() {
				return nil, fmt.Errorf("texts: %s embeds %s, whose type is not exported", v.Type(), field.Name)
			}
			if value.Kind() == reflect.Pointer {
				if value.IsNil() {
					continue
				}
				value = value.Elem()
			}
			inner, err := r.fields(value, path)
			if err != nil {
				return nil, err
			}
			for _, m := range inner {
				if _, taken := members.get(m.key); taken {
					return nil, fmt.Errorf("texts: %s: two fields are named %s", v.Type(), m.key)
				}
				members = append(members, m)
			}
			continue
		}
		if !tagged || !field.IsExported() {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if name == "" {
			return nil, fmt.Errorf("texts: %s.%s: its json tag names no field", v.Type(), field.Name)
		}
		if field.Type == wordsType || field.Type == wordsPtrType {
			r.addBlock(name, value, path)
			continue
		}
		if omitted(value, options) {
			continue
		}
		key := name
		if path != "" {
			key = path + "." + name
		}
		encoded, err := r.value(value, key)
		if err != nil {
			return nil, err
		}
		if _, taken := members.get(name); taken {
			return nil, fmt.Errorf("texts: %s: two fields are named %s", v.Type(), name)
		}
		members = append(members, member{key: name, value: encoded})
	}
	return members, nil
}

// addBlock collects the block of a Words field named name. A nil *Words and Words that carry
// nothing draw no block. In a record of a list, the block's field attribute is the path of the
// words, such as kept[1].answer.
func (r *renderer) addBlock(name string, value reflect.Value, path string) {
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	words := value.Interface().(Words)
	if words.empty() {
		return
	}
	if path != "" {
		words.Field = path + "." + name
	}
	r.blocks = append(r.blocks, block{name: name, words: words})
}

// value converts a field's value to a value of the encoder. key is the field's path in the
// record, such as kept[1].ref, which names the records of a list and the field in an error.
func (r *renderer) value(v reflect.Value, key string) (any, error) {
	if v.Type() == timeType {
		return formatTime(v.Interface().(time.Time)), nil
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil, nil
		}
		return r.value(v.Elem(), key)
	case reflect.String:
		return v.String(), nil
	case reflect.Bool:
		return v.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := v.Int()
		if n > maxSafeInteger || n < -maxSafeInteger {
			return nil, fmt.Errorf("texts: field %s: %d is outside the integers a JavaScript number holds", key, n)
		}
		return n, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := v.Uint()
		if n > maxSafeInteger {
			return nil, fmt.Errorf("texts: field %s: %d is outside the integers a JavaScript number holds", key, n)
		}
		return int64(n), nil
	case reflect.Slice, reflect.Array:
		return r.list(v, key)
	case reflect.Map:
		return nil, fmt.Errorf("texts: field %s: a map is refused: the encoder writes an object of primitive fields, a list of primitives and a list of uniform flat records", key)
	case reflect.Struct:
		return nil, fmt.Errorf("texts: field %s: a nested object is refused: the encoder writes an object of primitive fields, a list of primitives and a list of uniform flat records", key)
	}
	return nil, fmt.Errorf("texts: field %s: a value of kind %s is refused", key, v.Kind())
}

// list converts a slice or an array to a list of the encoder: a list of primitives, or a list
// of records whose Words draw blocks with the path key[i].<field>, counted from 0. A nil slice
// is an empty list.
func (r *renderer) list(v reflect.Value, key string) (any, error) {
	element := v.Type().Elem()
	switch {
	case element.Kind() == reflect.Uint8:
		return nil, fmt.Errorf("texts: field %s: a list of bytes is refused", key)
	case element == wordsType || element == wordsPtrType:
		return nil, fmt.Errorf("texts: field %s: a list of Words is refused: put each in a field of a record of the list", key)
	}
	items := make([]any, v.Len())
	for i := range v.Len() {
		item := v.Index(i)
		for item.Kind() == reflect.Pointer && !item.IsNil() {
			item = item.Elem()
		}
		itemKey := fmt.Sprintf("%s[%d]", key, i)
		if item.Kind() == reflect.Struct && item.Type() != timeType {
			row, err := r.fields(item, itemKey)
			if err != nil {
				return nil, err
			}
			items[i] = row
			continue
		}
		value, err := r.value(item, itemKey)
		if err != nil {
			return nil, err
		}
		items[i] = value
	}
	return items, nil
}

// omitted says whether a field is left out by its json options, as encoding/json leaves it
// out: omitempty leaves out false, 0, an empty string, a nil pointer or interface and an empty
// list or map, and omitzero leaves out a zero value, by its IsZero method where it has one.
func omitted(v reflect.Value, options string) bool {
	var omitEmpty, omitZero bool
	for options != "" {
		var option string
		option, options, _ = strings.Cut(options, ",")
		switch option {
		case "omitempty":
			omitEmpty = true
		case "omitzero":
			omitZero = true
		}
	}
	if omitEmpty && isEmpty(v) {
		return true
	}
	return omitZero && isZero(v)
}

// isEmpty is the empty value of encoding/json's omitempty.
func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

// isZero is the zero value of encoding/json's omitzero.
func isZero(v reflect.Value) bool {
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
		return true
	}
	if z, ok := v.Interface().(interface{ IsZero() bool }); ok {
		return z.IsZero()
	}
	return v.IsZero()
}
