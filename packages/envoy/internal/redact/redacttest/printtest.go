package redacttest

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The box print test: a holder of the box, holding the probe, printed by every printer at every
// placement, and beside each such row its plain-string twin, which must print a marker so that the
// row is known able to see one. A twin a printer cannot reach at the row's placement uses the
// placement TwinPlacements records for the holder's kind, that printer and that placement; the
// row says which.

// Holder is one holder of the box for the print test: Boxed holds the probe in each of its box
// fields; Same is its plain-string twin, the same shape with a plain string holding the probe
// where each box was; Exported is the twin with every one of those fields exported, for a printer
// that cannot read an unexported field (encoding/json, slog's JSON handler). Kind names the
// shape, one of Kinds, whose records in TwinPlacements the rows read.
//
// A holder that holds its box only behind a pointer below its top level, where no printer
// follows (fmt prints a nested pointer as an address, encoding/json reads no unexported field),
// has no same-shape twin a printer reaches: its Same and Exported are nil, its rows use its kind's
// representative twins, and every such row counts as reshaped, never same.
type Holder struct {
	Name     string
	Kind     string
	Boxed    any
	Same     any
	Exported any
}

// Kinds are the shapes a holder has, each with the representative twins TwinPlacements was
// measured on (redacttest/gentable):
//
//   - "text": the box itself, whose twin is the probe as a string;
//   - "fields": a struct value whose box fields are exported, whose twin has a string there;
//   - "struct": a struct value whose box fields are unexported and which has no method, whose
//     twin has a string in each of those fields and whose exported twin has each exported;
//   - "pointer": a pointer to such a struct;
//   - "error": a pointer to a struct whose box fields are unexported and whose Error method
//     prints them, whose twin is a pointer to a struct with a string in each of those fields and
//     an Error method that prints them as they are, and whose exported twin is a pointer to a
//     struct with each of those fields exported.
//
// A holder's own twins must print as its kind's do; one that prints less makes its rows blind.
var Kinds = map[string]func() (same, exported any){
	"text":    func() (any, any) { return Words, Words },
	"fields":  func() (any, any) { return struct{ V string }{Words}, struct{ V string }{Words} },
	"struct":  func() (any, any) { return struct{ v string }{Words}, struct{ V string }{Words} },
	"pointer": func() (any, any) { return &struct{ v string }{Words}, &struct{ V string }{Words} },
	"error":   func() (any, any) { return &errorTwin{Words}, &struct{ V string }{Words} },
}

type errorTwin struct{ v string }

func (e *errorTwin) Error() string { return e.v }

// Printer is one way a value is printed. Print answers false where the printer does not apply to
// the value (Error() of a value that is no error, say).
type Printer struct {
	Name  string
	Print func(v any) (string, bool)
}

// Placement is one place a value can sit: Wrap puts it there.
type Placement struct {
	Name string
	Wrap func(v any) any
}

type exported struct{ V any }
type unexported struct{ v any }

// Placements are the placements the print test runs each printer at: the value itself, an
// exported and an unexported field, nested one, two and three deep in unexported fields, a slice,
// a map value and a pointer.
var Placements = []Placement{
	{"value", func(v any) any { return v }},
	{"exported", func(v any) any { return exported{v} }},
	{"unexported", func(v any) any { return unexported{v} }},
	{"nested-1", func(v any) any { return unexported{unexported{v}} }},
	{"nested-2", func(v any) any { return unexported{unexported{unexported{v}}} }},
	{"nested-3", func(v any) any { return unexported{unexported{unexported{unexported{v}}}} }},
	{"slice", func(v any) any { return []any{v} }},
	{"map", func(v any) any { return map[string]any{"k": v} }},
	{"pointer", func(v any) any { return &exported{v} }},
}

func placement(name string) Placement {
	for _, p := range Placements {
		if p.Name == name {
			return p
		}
	}
	panic("redacttest: no placement " + name)
}

func verb(v string) func(any) (string, bool) {
	return func(x any) (string, bool) { return fmt.Sprintf(v, x), true }
}

// Printers are the printers the print test runs.
var Printers = []Printer{
	{"%v", verb("%v")}, {"%+v", verb("%+v")}, {"%#v", verb("%#v")}, {"%s", verb("%s")},
	{"%q", verb("%q")}, {"%x", verb("%x")}, {"%X", verb("%X")}, {"%d", verb("%d")}, {"%t", verb("%t")},
	{"Sprint", func(x any) (string, bool) { return fmt.Sprint(x), true }},
	{"String()", func(x any) (string, bool) {
		s, ok := x.(fmt.Stringer)
		if !ok {
			return "", false
		}
		return s.String(), true
	}},
	{"Error()", func(x any) (string, bool) {
		e, ok := x.(error)
		if !ok {
			return "", false
		}
		return e.Error(), true
	}},
	{"json.Marshal", func(x any) (string, bool) {
		data, err := json.Marshal(x)
		if err != nil {
			return "json.Marshal failed: " + err.Error(), true
		}
		return string(data), true
	}},
	{"slog-text", func(x any) (string, bool) {
		var b strings.Builder
		slog.New(slog.NewTextHandler(&b, nil)).Info("probe", "value", x)
		return b.String(), true
	}},
	{"slog-json", func(x any) (string, bool) {
		var b strings.Builder
		slog.New(slog.NewJSONHandler(&b, nil)).Info("probe", "value", x)
		return b.String(), true
	}},
	{"errors.Join", func(x any) (string, bool) {
		e, ok := x.(error)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("%v", errors.Join(e)), true
	}},
	{"Errorf-w", func(x any) (string, bool) {
		e, ok := x.(error)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("%v", fmt.Errorf("wrapped: %w", e)), true
	}},
}

// twinError and twinStringer are the twins of a printer that needs an error or a Stringer: a
// plain string the printer reaches through the method it calls.
type twinError struct{ s string }

func (e twinError) Error() string { return e.s }

type twinStringer struct{ s string }

func (s twinStringer) String() string { return s.s }

// Leaks reports the secrets text holds, as written or hex-encoded (%x, %X).
func Leaks(text string) []string {
	var found []string
	for _, s := range Secrets {
		for _, form := range []string{s, hex.EncodeToString([]byte(s)), strings.ToUpper(hex.EncodeToString([]byte(s)))} {
			if strings.Contains(text, form) {
				found = append(found, s)
				break
			}
		}
	}
	return found
}

// PrintTest runs every printer at every placement on h, each a subtest, and logs one
// `REDACT-PRINT <holder> rows=<n> same=<n> reshaped=<n> relocated=<n> blind=<n>` line the check
// counts: same is a twin at the row's own placement in the row's own shape, reshaped one at the
// row's placement in another shape (exported fields, an error, a Stringer), relocated one at
// another placement. A row fails when the boxed value's output holds a secret (REDACT-ASSERT
// print/<holder>/<printer>/<placement>), and as blind when its twin's output holds none.
func PrintTest(t *testing.T, h Holder) {
	t.Helper()
	if _, ok := Kinds[h.Kind]; !ok {
		t.Fatalf("redacttest: holder %s has kind %q, which is none of Kinds", h.Name, h.Kind)
	}
	representative := h.Same == nil
	if representative {
		h.Same, h.Exported = Kinds[h.Kind]()
	}
	rows, same, reshaped, relocated, blind := 0, 0, 0, 0, 0
	for _, printer := range Printers {
		for _, at := range Placements {
			boxed, applies := printer.Print(at.Wrap(h.Boxed))
			if !applies {
				continue
			}
			rows++
			name := printer.Name + "/" + at.Name
			record := TwinPlacements[h.Kind+"|"+printer.Name+"|"+at.Name]
			twin, where := h.Twin(at.Name, record)
			switch _, placed, _ := strings.Cut(record, "@"); {
			case record == "same" && !representative:
				same++
			case record == "same" || placed == at.Name:
				reshaped++
			default:
				relocated++
			}
			out, reached := "", false
			if twin != nil {
				out, reached = printer.Print(twin)
			}
			seen := reached && len(Leaks(out)) > 0
			if !seen {
				blind++
			}
			t.Run(name, func(t *testing.T) {
				capture := "print/" + h.Name + "/" + name
				Capture(t, capture, boxed)
				for _, s := range Leaks(boxed) {
					t.Errorf("REDACT-ASSERT %s: the boxed value prints the secret %q: %q", capture, s, boxed)
				}
				if !seen {
					t.Errorf("blind: the twin, placed %s, prints no marker under %s: %q", where, printer.Name, out)
				}
			})
		}
	}
	t.Logf("REDACT-PRINT %s rows=%d same=%d reshaped=%d relocated=%d blind=%d", h.Name, rows, same, reshaped, relocated, blind)
}

// Twin is h's twin at placement at, placed as record says: "same" (the row's placement, the same
// twin), or "<variant>@<placement>", where variant is "same", "exported", "error" (an error whose
// Error is the probe) or "stringer" (a Stringer whose String is the probe). A record that is
// empty (the table holds none) or "unreachable" (no twin was measured to reach the printer) gives
// no twin, and the row is blind.
func (h Holder) Twin(at, record string) (any, string) {
	switch record {
	case "same":
		return placement(at).Wrap(h.Same), at
	case "":
		return nil, "nowhere: TwinPlacements records no twin for it"
	case "unreachable":
		return nil, "nowhere: no twin was measured to reach this printer here"
	}
	variant, where, _ := strings.Cut(record, "@")
	var v any
	switch variant {
	case "same":
		v = h.Same
	case "exported":
		v = h.Exported
	case "error":
		v = twinError{Words}
	case "stringer":
		v = twinStringer{Words}
	default:
		return nil, "nowhere: TwinPlacements records the twin variant " + variant + ", which is none"
	}
	for _, p := range Placements {
		if p.Name == where {
			return p.Wrap(v), record
		}
	}
	return nil, "nowhere: TwinPlacements records the placement " + where + ", which is none"
}
