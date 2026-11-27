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
// (evalReadingsBody, whose keys decodeEvalBody holds a body to), and the registry takes a row's
// keys as those of a stored reading (evals.Reading, the route's answer) less the keys the server
// sets, and a link's as those of a stored link (evals.Link) less its recorded_at, which evals'
// TestARowTakesTheKeysOfAStoredReadingButThoseTheServerSets holds: a key one states and the other
// does not is a documented key refused, or a key taken that no client is told of. Each documented
// key is one the body, the row or the link takes, and the other way round. The keys the server
// sets, the five evalReadingInput names as refused on a row, are the registry's serverSetKeys,
// which the evals test named above holds to that list.
func TestTheDocumentedEvalReadingKeysAreTheKeysARowTakes(t *testing.T) {
	documented := bodyKeysOf(reflect.TypeFor[evalReadingInput]())
	stored := bodyKeysOf(reflect.TypeFor[evals.Reading]())
	less := func(keys bodyKeys, set ...string) []string {
		return slices.DeleteFunc(slices.Sorted(maps.Keys(keys)), func(key string) bool { return slices.Contains(set, key) })
	}
	for _, shape := range []struct {
		what             string
		documented, read []string
	}{
		{"the readings write's body", slices.Sorted(maps.Keys(bodyKeysOf(reflect.TypeFor[evalReadingsInput]()))), slices.Sorted(maps.Keys(evalReadingsBodyKeys))},
		{"a reading's row", slices.Sorted(maps.Keys(documented)), less(stored, "id", "eval_id", "recorded_by", "recorded_at", "superseded_at")},
		{"a reading's link", slices.Sorted(maps.Keys(documented["links"])), less(stored["links"], "recorded_at")},
	} {
		if !slices.Equal(shape.documented, shape.read) {
			t.Errorf("%s: the route documents the keys %v, and the server takes %v", shape.what, shape.documented, shape.read)
			continue
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

// TestABodyNamingAKeyTwiceInOneObjectIsRefused: the one walk of an eval write's body
// (refusedBodyKey) refuses a key named twice in any object of the body, as it refuses a key
// spelled otherwise: 400 INVALID_JSON naming the key and where it stands, at the top level, in an
// object under a key, in a list's element and in an object of an element, and in the objects
// whose keys the walk does not spell: a reading's row, which the registry reads key by key, and its
// attributes and links. encoding/json, the registry's row read and jsonb would each take the last
// value and pass over the first. It takes one key named in two objects, and the same key at two
// depths; and it names the first refused key in the order the body holds them.
func TestABodyNamingAKeyTwiceInOneObjectIsRefused(t *testing.T) {
	for _, test := range []struct {
		what  string
		keys  bodyKeys
		body  string
		names string // "" for a body the walk takes
	}{
		{"the slug twice", evalRegisterBodyKeys, `{"slug": "a", "name": "A", "slug": "b"}`, `the body holds the key "slug" twice`},
		{"the target twice, once null", evalRegisterBodyKeys, `{"slug": "a", "target": null, "target": 3}`, `the body holds the key "target" twice`},
		{"a series' unit twice", evalRegisterBodyKeys, `{"slug": "a", "series": [{"series": "s", "unit": "x", "unit": "y"}]}`, `series[0] holds the key "unit" twice`},
		{"a second series' link's href twice", evalRegisterBodyKeys, `{"slug": "a", "series": [{"series": "s"}, {"series": "t", "links": [{"rel": "via", "href": "https://a.example", "href": "https://b.example"}]}]}`, `series[1].links[0] holds the key "href" twice`},
		{"the readings' actor's kind twice", evalReadingsBodyKeys, `{"actor": {"kind": "session", "id": "s-1", "kind": "user"}, "readings": []}`, `actor holds the key "kind" twice`},
		{"a key twice after a key spelled otherwise", evalRegisterBodyKeys, `{"Slug": "a", "slug": "b", "slug": "c"}`, `the body holds the key "Slug", which is none of the keys it takes`},
		{"a key twice before a key spelled otherwise", evalRegisterBodyKeys, `{"slug": "b", "slug": "c", "Slug": "a"}`, `the body holds the key "slug" twice`},
		{"a reading's series twice", evalReadingsBodyKeys, `{"readings": [{"series": "a", "row_kind": "run", "series": "b"}]}`, `readings[0] holds the key "series" twice`},
		{"a second reading's attribute twice", evalReadingsBodyKeys, `{"readings": [{"series": "a"}, {"series": "a", "attributes": {"repo": "x", "repo": "y"}}]}`, `readings[1].attributes holds the key "repo" twice`},
		{"an attribute twice deep in a reading", evalReadingsBodyKeys, `{"readings": [{"series": "a", "attributes": {"ci": [{"job": "x", "job": "y"}]}}]}`, `readings[0].attributes.ci[0] holds the key "job" twice`},
		{"a reading's link's rel twice", evalReadingsBodyKeys, `{"readings": [{"series": "a", "links": [{"rel": "via", "rel": "self", "href": "https://a.example"}]}]}`, `readings[0].links[0] holds the key "rel" twice`},
		{"one key in two readings and their attributes", evalReadingsBodyKeys, `{"readings": [{"series": "a", "attributes": {"repo": "x"}}, {"series": "a", "attributes": {"repo": "x"}}]}`, ""},
		{"one key in two objects", evalRegisterBodyKeys, `{"slug": "a", "unit": "x", "series": [{"series": "s", "unit": "x"}, {"series": "t", "unit": "x"}]}`, ""},
		{"each key once beside lists and nulls", evalRegisterBodyKeys, `{"slug": "a", "links": [], "series": null, "failure_modes": [{"mode": "m", "guard": "g"}], "target_judges": ["s.median"]}`, ""},
	} {
		err := refusedBodyKey([]byte(test.body), test.keys)
		if test.names == "" {
			if err != nil {
				t.Errorf("%s: %s is refused %v, want it taken", test.what, test.body, err)
			}
			continue
		}
		refused, ok := err.(*apiError)
		if !ok || refused.status != http.StatusBadRequest || refused.code != "INVALID_JSON" || !strings.Contains(refused.message, test.names) {
			t.Errorf("%s: %s is answered %v, want 400 INVALID_JSON naming %s", test.what, test.body, err, test.names)
			continue
		}
		t.Logf("%s: %s", test.what, refused.message)
	}
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
