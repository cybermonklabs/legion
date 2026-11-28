// This file is in package api because it reads settingsResponse and fixedSettingsFields, neither of
// which is worth exporting so a test can see them.
package api

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
)

// TestTheFixedSettingsFieldsAreTheBodysOwnKeys: fixedSettingsFields, the names Register refuses a
// store's field, are the keys GET /v1/settings writes for settingsResponse's own fields, read from a
// body with every one of them set and no store field. So a fixed field added to the body is one no
// store can take, and MarshalJSON can never write a key twice.
//
// The named revert: write the names by hand and leave one out, and the body holds a key the list
// does not.
func TestTheFixedSettingsFieldsAreTheBodysOwnKeys(t *testing.T) {
	var full settingsResponse
	v := reflect.ValueOf(&full).Elem()
	for i := range v.NumField() {
		switch field := v.Field(i); field.Kind() {
		case reflect.String:
			field.SetString("x")
		case reflect.Bool:
			field.SetBool(true)
		case reflect.Map:
			if v.Type().Field(i).Tag.Get("json") == "-" {
				continue
			}
			m := reflect.MakeMap(field.Type())
			m.SetMapIndex(reflect.ValueOf("k"), reflect.Zero(field.Type().Elem()))
			field.Set(m)
		default:
			t.Fatalf("settingsResponse.%s is a %s, which this test does not fill", v.Type().Field(i).Name, field.Kind())
		}
	}
	body, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatal(err)
	}
	if got, want := slices.Sorted(maps.Keys(keys)), slices.Sorted(slices.Values(fixedSettingsFields)); !slices.Equal(got, want) {
		t.Fatalf("GET /v1/settings writes the fixed keys %v; fixedSettingsFields holds %v", got, want)
	}
}
