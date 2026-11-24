package redacttest

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// keptVerbs are the verbs KeptPrintTest prints under: each verb R2 names, %t and %d, which no
// pointer and no string takes, and %z, which no kind takes. fmt prints a value under a verb its
// kind does not take as a bad verb does: the value itself, at the top level, under %v, where a
// pointer to a slice, a map or a struct is spelled out.
var keptVerbs = []string{"%v", "%+v", "%#v", "%s", "%q", "%t", "%d", "%x", "%X", "%z"}

// kept holds a value in an unexported field, as a type of another package holds one: fmt reads
// such a field's storage and calls none of the value's methods.
type kept struct{ v any }

// keptHoldings are the places KeptPrintTest puts a value: itself, behind a pointer, in a slice and
// as a map's value, and each of those inside a struct's unexported field (kept).
var keptHoldings = []struct {
	name string
	hold func(v any) any
}{
	{"value", func(v any) any { return v }},
	{"pointer", pointerTo},
	{"slice", sliceOf},
	{"map", mapOf},
	{"struct", func(v any) any { return kept{v} }},
	{"struct-pointer", func(v any) any { return &kept{v} }},
	{"struct-slice", func(v any) any { return []kept{{v}} }},
	{"struct-map", func(v any) any { return map[string]kept{"k": {v}} }},
}

func pointerTo(v any) any {
	p := reflect.New(reflect.TypeOf(v))
	p.Elem().Set(reflect.ValueOf(v))
	return p.Interface()
}

func sliceOf(v any) any {
	s := reflect.MakeSlice(reflect.SliceOf(reflect.TypeOf(v)), 0, 1)
	return reflect.Append(s, reflect.ValueOf(v)).Interface()
}

func mapOf(v any) any {
	m := reflect.MakeMap(reflect.MapOf(reflect.TypeFor[string](), reflect.TypeOf(v)))
	m.SetMapIndex(reflect.ValueOf("k"), reflect.ValueOf(v))
	return m.Interface()
}

// KeptPrintTest prints v, a value that keeps one of Secrets where no printer may reach it - the
// credentials a box's or an answer's record of its request keeps - under every verb of keptVerbs,
// at every place of keptHoldings, each a subtest. A row fails, as REDACT-ASSERT
// kept/<name>/<holding>/<verb>, when its output holds a secret, as written or hex-encoded
// (Leaks), or a run of one of credentials, the values the request carried where that is not one
// of Secrets (a key's signed JWT), as written or hex-encoded (shownPieces); and as blind when
// twin, a plain string that holds a secret or one of credentials, printed under the same verb in
// a struct's field at the same holding, prints none, since such a row could not see one. It logs
// one `REDACT-KEPT <name> rows=<n> blind=<n>` line.
func KeptPrintTest(t *testing.T, name string, v any, twin string, credentials ...string) {
	t.Helper()
	type plain struct{ s string }
	rows, blind := 0, 0
	for _, at := range keptHoldings {
		for _, verb := range keptVerbs {
			rows++
			out := fmt.Sprintf(verb, at.hold(v))
			twinOut := fmt.Sprintf(verb, at.hold(plain{twin}))
			seen := len(Leaks(twinOut)) > 0 || shownPieces(twinOut, credentials) != nil
			if !seen {
				blind++
			}
			t.Run(at.name+"/"+verb, func(t *testing.T) {
				capture := "kept/" + name + "/" + at.name + "/" + verb
				Capture(t, capture, out)
				for _, s := range Leaks(out) {
					t.Errorf("REDACT-ASSERT %s: the value prints the secret %q it keeps: %q", capture, s, out)
				}
				for _, piece := range shownPieces(out, credentials) {
					t.Errorf("REDACT-ASSERT %s: the value prints %q, a run of a credential its request carried: %q", capture, piece, out)
				}
				if !seen {
					t.Errorf("blind: the twin, a plain string in a struct's field at this holding, prints no secret under %s: %q", verb, twinOut)
				}
			})
		}
	}
	t.Logf("REDACT-KEPT %s rows=%d blind=%d", name, rows, blind)
}

// shownPieces is, for each of credentials that text shows, the first run of it text holds: a
// Piece as written, or the 16 hex digits of 8 of its bytes, as %x and %X write them.
func shownPieces(text string, credentials []string) []string {
	var shown []string
	lower := strings.ToLower(text)
	for _, credential := range credentials {
		if piece := Piece(text, credential); piece != "" {
			shown = append(shown, piece)
			continue
		}
		hexed := hex.EncodeToString([]byte(credential))
		for at := 0; at+16 <= len(hexed); at += 2 {
			if piece := hexed[at : at+16]; strings.Contains(lower, piece) {
				shown = append(shown, piece)
				break
			}
		}
	}
	return shown
}
