package shimwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// The daemon reads a launch's totals, the shim answers or sends its last ones unasked, the Claude
// Code bridge names its set model to the shim, and a shim that counts says so in its hello. Each
// frame decodes to its own type, not to Raw, and encodes back to the bytes that were sent.
func TestTheTotalsFramesRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		line  string
		frame Frame
	}{
		{"the read", `{"type":"model_totals","id":"read-1"}`, ModelTotals{ID: "read-1"}},
		{
			"an answer with totals",
			`{"type":"model_totals_result","id":"read-1","ok":true,"totals":{"read":3,"setModel":"anthropic/claude-opus-5-5",` +
				`"counts":[{"model":"anthropic/claude-opus-5","calls":3},{"model":"anthropic/claude-opus-5-5","calls":41}],` +
				`"outsideForm":1,"overflowed":false}}`,
			ModelTotalsResult{ID: "read-1", OK: true, Totals: &Totals{
				Read: 3, SetModel: "anthropic/claude-opus-5-5", OutsideForm: 1,
				Counts: []ModelCount{{Model: "anthropic/claude-opus-5", Calls: 3}, {Model: "anthropic/claude-opus-5-5", Calls: 41}},
			}},
		},
		{
			"a refusal",
			`{"type":"model_totals_result","id":"read-2","ok":false,"error":"16 reads already wait for the harness"}`,
			ModelTotalsResult{ID: "read-2", Error: "16 reads already wait for the harness"},
		},
		{
			"the final totals",
			`{"type":"model_totals_final","totals":{"read":4,"counts":[],"outsideForm":0,"overflowed":true}}`,
			ModelTotalsFinal{Totals: Totals{Read: 4, Counts: []ModelCount{}, Overflowed: true}},
		},
		{
			"the bridge's set model",
			`{"type":"legion_set_model","provider":"anthropic","id":"claude-opus-5-5"}`,
			LegionSetModel{Provider: "anthropic", ID: "claude-opus-5-5"},
		},
		{"a hello that counts", `{"type":"hello2","bootToken":"boot-1","modelTotals":true}`, Hello2{BootToken: "boot-1", ModelTotals: true}},
		{"a hello that does not count", `{"type":"hello2","bootToken":"boot-1"}`, Hello2{BootToken: "boot-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.frame)
			if err != nil {
				t.Fatalf("Marshal(%#v): %v", tc.frame, err)
			}
			if string(encoded) != tc.line {
				t.Fatalf("Marshal(%#v) = %s, want %s", tc.frame, encoded, tc.line)
			}
			decoded, err := Decode(encoded)
			if err != nil {
				t.Fatalf("Decode(%s): %v", encoded, err)
			}
			if !reflect.DeepEqual(decoded, tc.frame) {
				t.Fatalf("Decode(%s) = %#v, want %#v", encoded, decoded, tc.frame)
			}
			again, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("Marshal the decoded %#v: %v", decoded, err)
			}
			if string(again) != tc.line {
				t.Fatalf("the decoded frame encodes as %s, want %s", again, tc.line)
			}
		})
	}
}

// The shim is the only sender of an answer and of the final totals, so the daemon refuses totals
// no shim counts: an answer to no read, an answer with no totals, a refusal with no reason, more
// models than MaxTotalsModels, a model counted with no call, a model listed twice and a read
// numbered 0. The final totals hold to the same rules. Totals of exactly MaxTotalsModels models,
// totals of no call and a refusal with its reason validate.
func TestATotalsFrameOutsideItsRulesIsRefused(t *testing.T) {
	counts := func(n int) []ModelCount {
		out := make([]ModelCount, n)
		for index := range out {
			out[index] = ModelCount{Model: fmt.Sprintf("acme/model-%02d", index+1), Calls: uint64(index + 1)}
		}
		return out
	}
	noCall := []ModelCount{{Model: "acme/a", Calls: 1}, {Model: "acme/b", Calls: 0}}
	twice := []ModelCount{{Model: "acme/a", Calls: 1}, {Model: "acme/b", Calls: 1}, {Model: "acme/a", Calls: 2}}
	answer := func(totals Totals) ModelTotalsResult {
		return ModelTotalsResult{ID: "read-1", OK: true, Totals: &totals}
	}

	for name, frame := range map[string]interface{ Validate() error }{
		"an answer with no id":                     ModelTotalsResult{OK: true, Totals: &Totals{Read: 1, Counts: counts(1)}},
		"an answer with no totals":                 ModelTotalsResult{ID: "read-1", OK: true},
		"a refusal with no reason":                 ModelTotalsResult{ID: "read-1"},
		"an answer of 65 models":                   answer(Totals{Read: 1, Counts: counts(65)}),
		"an answer counting a model with no call":  answer(Totals{Read: 1, Counts: noCall}),
		"an answer listing a model twice":          answer(Totals{Read: 1, Counts: twice}),
		"an answer numbered read 0":                answer(Totals{Counts: counts(1)}),
		"final totals of 65 models":                ModelTotalsFinal{Totals: Totals{Read: 1, Counts: counts(65)}},
		"final totals counting a model of no call": ModelTotalsFinal{Totals: Totals{Read: 1, Counts: noCall}},
		"final totals listing a model twice":       ModelTotalsFinal{Totals: Totals{Read: 1, Counts: twice}},
		"final totals numbered read 0":             ModelTotalsFinal{Totals: Totals{Counts: counts(1)}},
	} {
		if err := frame.Validate(); !errors.Is(err, ErrMalformedFrame) {
			t.Errorf("%s: Validate = %v, want ErrMalformedFrame", name, err)
		}
	}

	for name, frame := range map[string]interface{ Validate() error }{
		"an answer of 64 models":       answer(Totals{Read: 1, Counts: counts(64)}),
		"final totals of 64 models":    ModelTotalsFinal{Totals: Totals{Read: 2, Counts: counts(64)}},
		"an answer of no call":         answer(Totals{Read: 1, Counts: []ModelCount{}}),
		"a refusal that says why":      ModelTotalsResult{ID: "read-1", Error: "the harness has exited, and its final totals follow"},
		"final totals that overflowed": ModelTotalsFinal{Totals: Totals{Read: 5, Counts: counts(2), Overflowed: true}},
	} {
		if err := frame.Validate(); err != nil {
			t.Errorf("%s: Validate = %v, want nil", name, err)
		}
	}
}
