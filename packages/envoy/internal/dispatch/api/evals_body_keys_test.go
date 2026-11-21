package api

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// TestTheDocumentedEvalReadingKeysAreTheKeysARowTakes: the readings route documents its body, a
// row and a link in types of its own (evalReadingsInput, evalReadingInput, evalLinkInput), the
// contract a client writes to; the handler decodes the body into a type of its own
// (evalReadingsBody, whose keys decodeEvalBody holds a body to), and the registry decodes and
// holds the rows and links to types of its own (evals.ReadingKeys, ReadingLinkKeys): a key one
// states and the other does not is a documented key refused, or a key taken that no client is
// told of. Each documented key is one the body, the row or the link takes, and the other way
// round.
func TestTheDocumentedEvalReadingKeysAreTheKeysARowTakes(t *testing.T) {
	documented := bodyKeysOf(reflect.TypeFor[evalReadingInput]())
	for _, shape := range []struct {
		what             string
		documented, read []string
	}{
		{"the readings write's body", slices.Sorted(maps.Keys(bodyKeysOf(reflect.TypeFor[evalReadingsInput]()))), slices.Sorted(maps.Keys(evalReadingsBodyKeys))},
		{"a reading's row", slices.Sorted(maps.Keys(documented)), slices.Sorted(slices.Values(evals.ReadingKeys()))},
		{"a reading's link", slices.Sorted(maps.Keys(documented["links"])), slices.Sorted(slices.Values(evals.ReadingLinkKeys()))},
	} {
		if !slices.Equal(shape.documented, shape.read) {
			t.Errorf("%s: the route documents the keys %v, and the server takes %v", shape.what, shape.documented, shape.read)
		}
		t.Logf("%s: %d keys, documented and taken alike: %s", shape.what, len(shape.read), strings.Join(shape.read, ", "))
	}
}

// TestEveryEvalRegisterKeySetsOneFieldOfTheRegistration: a register write tells a field it sets
// from one it leaves out by the keys its body names (decodeEvalRegistration's fieldOf), which spell
// each key again beside the body type's tags. A key spelled otherwise there would be taken by the
// body's decode and set nothing: a write accepted and then ignored (R5). So a body naming each key
// the type takes, alone, sets exactly one field of the registration, at the top level and in a
// series; the slug, the actor and the series list are what the registration is of, not fields it
// sets.
func TestEveryEvalRegisterKeySetsOneFieldOfTheRegistration(t *testing.T) {
	decode := func(t *testing.T, body string) evals.Registration {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/EVL/evals", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		change, _, err := decodeEvalRegistration(request)
		if err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return change
	}
	checked := 0
	for _, key := range slices.Sorted(maps.Keys(evalRegisterBodyKeys)) {
		if key == "slug" || key == "actor" || key == "series" {
			continue
		}
		body := `{"slug": "review-time", "` + key + `": null}`
		if set := setFields(reflect.ValueOf(decode(t, body))); !slices.Equal(set, []string{fieldNameOf(t, evalRegisterInput{}, key)}) {
			t.Errorf("%s sets %v, want its own field alone", body, set)
		}
		checked++
	}
	for _, key := range slices.Sorted(maps.Keys(evalRegisterBodyKeys["series"])) {
		if key == "series" {
			continue
		}
		body := `{"slug": "review-time", "series": [{"series": "minutes", "` + key + `": null}]}`
		change := decode(t, body)
		if len(change.Series) != 1 {
			t.Fatalf("%s names %d series, want 1", body, len(change.Series))
		}
		if set := setFields(reflect.ValueOf(change.Series[0])); !slices.Equal(set, []string{fieldNameOf(t, evalSeriesInput{}, key)}) {
			t.Errorf("%s sets %v of the series, want its own field alone", body, set)
		}
		checked++
	}
	t.Logf("%d keys each set their own field alone", checked)
}

// setFields is the name of every evals.Field the registration or series change value sets, those of
// a struct it embeds among them.
func setFields(value reflect.Value) []string {
	var set []string
	for index := range value.NumField() {
		field, held := value.Type().Field(index), value.Field(index)
		if field.Anonymous {
			set = append(set, setFields(held)...)
			continue
		}
		if held.Kind() != reflect.Struct {
			continue
		}
		if flag := held.FieldByName("Set"); flag.IsValid() && flag.Kind() == reflect.Bool && flag.Bool() {
			set = append(set, field.Name)
		}
	}
	return set
}

// fieldNameOf is the Go name of the field of shape whose json key is key.
func fieldNameOf(t *testing.T, shape any, key string) string {
	t.Helper()
	for field := range reflect.TypeOf(shape).Fields() {
		if name, _, _ := strings.Cut(field.Tag.Get("json"), ","); name == key {
			return field.Name
		}
	}
	t.Fatalf("%T has no field whose key is %s", shape, key)
	return ""
}
