package intake

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"math"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// updateVerdicts rewrites daemonVerdictsFile from the daemon's decoder.
var updateVerdicts = flag.Bool("update", false, "rewrite testdata/dispatch/daemon-verdicts.json from the daemon's decoder")

// daemonVerdictsFile is what the daemon's decoder takes at every place of every shared issue event
// envelope: packages/contracts' dispatch-issue-events.test.ts reads it, and requires each schema
// that reads a place to take the forms the decoder takes there and no other, unless its DAEMON
// table names the difference with the reason.
const daemonVerdictsFile = "testdata/dispatch/daemon-verdicts.json"

// verdictProbes is each form a value takes and the values that probe it, as JSON, in the order of
// FORM_PROBES in packages/contracts/src/schema-inspection.testing.ts: the file carries them, and
// the contracts test requires them to be its own, so both sides ask the same values.
var verdictProbes = []struct {
	form   string
	values []string
}{
	{"null", []string{`null`}},
	{"string", []string{`"x"`, `"1"`, `"true"`}},
	{"empty string", []string{`""`}},
	{"number", []string{`1`, `7`}},
	{"zero", []string{`0`}},
	{"negative", []string{`-1`}},
	{"fraction", []string{`1.5`}},
	{"past range", []string{`1e20`}},
	{"boolean", []string{`true`, `false`}},
	{"object", []string{`{}`}},
	{"array", []string{`[]`}},
}

// The daemon's decoder takes, at each place of each shared issue event envelope, exactly the
// forms daemonVerdictsFile says: decodeMessage, as the consumer runs it on the subject the
// envelope's topic names, decodes the envelope with that place's value replaced by a probe of
// the form (or, for a key, left out) with no error and no field taken as unknown, and LabelSetter
// reads its event. A place is the envelope, its event (the text its payload holds) and every key,
// list element and nested value of both; a list's element is probed as a list of the probe alone.
// So a change of the decoder that takes or refuses another form anywhere fails here by its place,
// and the contracts test, which holds each schema to these verdicts, reads what the decoder does.
func TestTheDaemonDecoderTakesWhatItsVerdictsSay(t *testing.T) {
	got := map[string][]string{}
	for _, tc := range sharedIssueEnvelopes {
		maps.Copy(got, exampleVerdicts(t, tc.file))
	}
	written := verdictsJSON(got)
	if *updateVerdicts {
		if err := os.WriteFile(daemonVerdictsFile, written, 0o644); err != nil {
			t.Fatalf("write %s: %v", daemonVerdictsFile, err)
		}
	}
	held, err := os.ReadFile(daemonVerdictsFile)
	if err != nil {
		t.Fatalf("read %s: %v", daemonVerdictsFile, err)
	}
	if bytes.Equal(held, written) {
		return
	}
	var file struct {
		Probes   map[string][]json.RawMessage `json:"probes"`
		Verdicts map[string][]string          `json:"verdicts"`
	}
	if err := json.Unmarshal(held, &file); err != nil {
		t.Fatalf("decode %s: %v", daemonVerdictsFile, err)
	}
	for _, probe := range verdictProbes {
		var values []string
		for _, value := range file.Probes[probe.form] {
			values = append(values, string(value))
		}
		if !slices.Equal(values, probe.values) {
			t.Errorf("%s probes %s with %v, and the decoder was asked %v", daemonVerdictsFile, probe.form, values, probe.values)
		}
	}
	for _, at := range slices.Sorted(maps.Keys(got)) {
		if want, ok := file.Verdicts[at]; !ok {
			t.Errorf("the decoder takes %v at %s, which %s holds no row for", got[at], at, daemonVerdictsFile)
		} else if !slices.Equal(want, got[at]) {
			t.Errorf("the decoder takes %v at %s, and %s says %v", got[at], at, daemonVerdictsFile, want)
		}
	}
	for _, at := range slices.Sorted(maps.Keys(file.Verdicts)) {
		if _, ok := got[at]; !ok {
			t.Errorf("%s holds a row for %s, a place no shared envelope holds", daemonVerdictsFile, at)
		}
	}
	t.Errorf("%s is not what the decoder takes: go test -run TestTheDaemonDecoderTakesWhatItsVerdictsSay -update rewrites it", daemonVerdictsFile)
}

// verdictsJSON is daemonVerdictsFile's text for verdicts: the probes, then one line for each
// place, in order.
func verdictsJSON(verdicts map[string][]string) []byte {
	var out bytes.Buffer
	out.WriteString("{\n  \"probes\": {\n")
	for index, probe := range verdictProbes {
		name, _ := json.Marshal(probe.form)
		fmt.Fprintf(&out, "    %s: [%s]", name, strings.Join(probe.values, ", "))
		out.WriteString(map[bool]string{true: ",\n", false: "\n"}[index < len(verdictProbes)-1])
	}
	out.WriteString("  },\n  \"verdicts\": {\n")
	keys := slices.Sorted(maps.Keys(verdicts))
	for index, at := range keys {
		name, _ := json.Marshal(at)
		forms, _ := json.Marshal(verdicts[at])
		fmt.Fprintf(&out, "    %s: %s", name, forms)
		out.WriteString(map[bool]string{true: ",\n", false: "\n"}[index < len(keys)-1])
	}
	out.WriteString("  }\n}\n")
	return out.Bytes()
}

// verdictPlace is one place of a shared envelope the decoder is asked about: its path as the
// contracts test names it (`event.payload.labels[]`), the steps to it in the envelope or, for
// event, in the event its payload holds, the values it holds (each element, for a list's
// element), and whether it is a key, which can be left out.
type verdictPlace struct {
	path  string
	steps []any
	event bool
	held  []any
	key   bool
}

// listElement is the step to a list's element: a probe of it replaces the list with a list of
// the probe alone.
type listElement struct{}

// exampleVerdicts is what the decoder takes at each place of one shared envelope, by
// `<file> <path>`: the forms whose probes, or the probes the values held there add (heldProbes),
// it takes one of, and absent where it takes the key left out. A path a list's elements share is
// each element's together.
func exampleVerdicts(t *testing.T, file string) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var envelope map[string]any
	decodeNumbers(t, data, &envelope)
	text, _ := envelope["payload"].(string)
	var event any
	decodeNumbers(t, []byte(text), &event)
	subject, _ := envelope["topic"].(string)
	takes := func(message []byte) bool {
		decoded, err := decodeMessage(subject, "CAPTURE", capturedRepositories, message)
		if err != nil || len(decoded.Unread) != 0 {
			return false
		}
		var carried struct {
			Payload string `json:"payload"`
		}
		if json.Unmarshal(message, &carried) != nil {
			return false
		}
		_, err = LabelSetter([]json.RawMessage{json.RawMessage(carried.Payload)})
		return err == nil
	}
	with := func(place verdictPlace, value any, remove bool) []byte {
		var out any = envelope
		if place.event {
			changed, err := json.Marshal(setAt(event, place.steps, value, remove))
			if err != nil {
				t.Fatalf("encode %s's event with a probe at %s: %v", file, place.path, err)
			}
			copied := maps.Clone(envelope)
			copied["payload"] = string(changed)
			out = copied
		} else {
			out = setAt(envelope, place.steps, value, remove)
		}
		message, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("encode %s with a probe at %s: %v", file, place.path, err)
		}
		return message
	}
	places := []verdictPlace{{path: "envelope", held: []any{envelope}}, {path: "event", event: true, held: []any{event}}}
	addVerdictPlaces("envelope", nil, false, envelope, &places)
	addVerdictPlaces("event", nil, true, event, &places)
	verdicts := map[string]map[string]bool{}
	for _, place := range places {
		at := path.Base(file) + " " + place.path
		taken := verdicts[at]
		if taken == nil {
			taken = map[string]bool{}
			verdicts[at] = taken
		}
		var probes []any
		for _, probe := range verdictProbes {
			for _, value := range probe.values {
				var decoded any
				decodeNumbers(t, []byte(value), &decoded)
				probes = append(probes, decoded)
			}
		}
		for _, value := range place.held {
			probes = append(probes, heldProbes(value)...)
		}
		for _, probe := range probes {
			if form := verdictForm(probe); !taken[form] && takes(with(place, probe, false)) {
				taken[form] = true
			}
		}
		if place.key && takes(with(place, nil, true)) {
			taken["absent"] = true
		}
	}
	out := map[string][]string{}
	order := []string{"absent"}
	for _, probe := range verdictProbes {
		order = append(order, probe.form)
	}
	for at, taken := range verdicts {
		forms := []string{}
		for _, form := range order {
			if taken[form] {
				forms = append(forms, form)
			}
		}
		out[at] = forms
	}
	return out
}

// addVerdictPlaces adds each place below node, at prefix and steps, to places: each key of an
// object, and a list's element, with what lies below each.
func addVerdictPlaces(prefix string, steps []any, event bool, node any, places *[]verdictPlace) {
	switch node := node.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(node)) {
			at := prefix + "." + key
			next := append(slices.Clone(steps), key)
			*places = append(*places, verdictPlace{path: at, steps: next, event: event, held: []any{node[key]}, key: true})
			addVerdictPlaces(at, next, event, node[key], places)
		}
	case []any:
		*places = append(*places, verdictPlace{path: prefix + "[]", steps: append(slices.Clone(steps), listElement{}), event: event, held: node})
		for index, item := range node {
			addVerdictPlaces(prefix+"[]", append(slices.Clone(steps), index), event, item, places)
		}
	}
}

// setAt is node with the value at steps replaced by value, or the key there left out, the rest
// shared with node.
func setAt(node any, steps []any, value any, remove bool) any {
	if len(steps) == 0 {
		return value
	}
	switch step := steps[0].(type) {
	case string:
		object := maps.Clone(node.(map[string]any))
		if len(steps) == 1 && remove {
			delete(object, step)
			return object
		}
		object[step] = setAt(object[step], steps[1:], value, remove)
		return object
	case int:
		list := slices.Clone(node.([]any))
		list[step] = setAt(list[step], steps[1:], value, remove)
		return list
	default:
		return []any{value}
	}
}

// decodeNumbers decodes JSON keeping each number's own text (json.Number), so a value is written
// back as it was read.
func decodeNumbers(t *testing.T, data []byte, into any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

// numberText is the text of a JSON number, as heldProbes in schema-inspection.testing.ts reads it.
var numberText = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// heldProbes is what a value held at a place adds to the fixed probes, as heldProbes in
// packages/contracts/src/schema-inspection.testing.ts adds it: the value, its text where it is a
// number or a boolean, the number a string spells where it spells one, and a list holding it.
func heldProbes(value any) []any {
	probes := []any{value}
	switch value := value.(type) {
	case json.Number:
		probes = append(probes, string(value))
	case bool:
		probes = append(probes, strconv.FormatBool(value))
	case string:
		if numberText.MatchString(value) {
			probes = append(probes, json.Number(value))
		}
	}
	return append(probes, []any{value})
}

// verdictForm is the form a JSON value holds, as formOf in schema-inspection.testing.ts names it.
func verdictForm(value any) string {
	switch value := value.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case bool:
		return "boolean"
	case string:
		if value == "" {
			return "empty string"
		}
		return "string"
	case json.Number:
		number, err := strconv.ParseFloat(string(value), 64)
		switch {
		case err != nil || number != math.Trunc(number):
			return "fraction"
		case math.Abs(number) > 1<<53-1:
			return "past range"
		case number == 0:
			return "zero"
		case number < 0:
			return "negative"
		}
		return "number"
	}
	panic(fmt.Sprintf("no JSON value: %#v", value))
}
