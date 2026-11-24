// Package settingsfields turns a contributor's fields of GET /v1/settings into the map the
// settings answer carries beside its fixed fields. Each contributor - a secret store, and later a
// caller identity provider - documents its fields in a struct of string fields with JSON tags,
// which cipher-refgen reads, and hands the broker Of of that struct; the broker merges the maps
// with Merge, which refuses a field two contributors would both answer.
package settingsfields

import (
	"fmt"
	"reflect"
	"strings"
)

// Of is v's string fields by JSON name, each field whose value is "" left out, as omitempty leaves
// it out of the JSON body. v is a struct or a pointer to one; a field with no JSON name, or named
// "-", is not a settings field. A field of any type but string panics: a settings field is text.
func Of(v any) map[string]string {
	rv := reflect.Indirect(reflect.ValueOf(v))
	if rv.Kind() != reflect.Struct {
		panic(fmt.Sprintf("settingsfields.Of of %T, which is no struct", v))
	}
	fields := map[string]string{}
	for i := range rv.NumField() {
		field := rv.Type().Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if field.Type.Kind() != reflect.String {
			panic(fmt.Sprintf("settingsfields.Of: %s.%s (%s) is no string", rv.Type(), field.Name, field.Type))
		}
		if value := rv.Field(i).String(); value != "" {
			fields[name] = value
		}
	}
	return fields
}

// Merge adds src's fields to dst, and refuses, changing nothing, when a key of src is one dst
// already holds, naming the key.
func Merge(dst, src map[string]string) error {
	for key := range src {
		if _, held := dst[key]; held {
			return fmt.Errorf("the settings field %s is answered twice", key)
		}
	}
	for key, value := range src {
		dst[key] = value
	}
	return nil
}
