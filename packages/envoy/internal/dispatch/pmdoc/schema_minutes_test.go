package pmdoc

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A plan task says how big it is in minutes (R18 of the issue tree's spec): its `size` is of the
// kind `minutes`, written as a whole number of hours, a whole number of minutes, or both with the
// hours first, and read as its whole number of minutes. The block's text keeps the size as its
// author wrote it, so each size the schema takes reads back from the render character for
// character, and the render's attribute carries it as written, under the kind its type declares.
// The schema refuses every other shape - a unit other than m or h, a zero, a negative, a fraction,
// a leading zero, the minutes before the hours - and a duration past MaxMinutes, 10,000 minutes,
// while 10,000 minutes itself, written `10000m` or `166h40m`, is taken. `1h90m` is both numbers
// with the hours first, which R18 admits, and reads as 150 minutes.
func TestAPlanTasksSizeIsReadInWholeMinutesAndRefusedPastItsShape(t *testing.T) {
	t.Parallel()
	plan := func(size string) string {
		return ":::plan_task{#t1 size=\"" + size + "\"}\nDo it.\n:::\n"
	}
	for _, taken := range []struct {
		size    string
		minutes int
	}{
		{"1m", 1}, {"45m", 45}, {"2h", 120}, {"1h30m", 90}, {"1h90m", 150}, {"10000m", MaxMinutes}, {"166h40m", MaxMinutes},
	} {
		if minutes, err := Minutes(taken.size); err != nil || minutes != taken.minutes {
			t.Errorf("Minutes(%q) = %d, %v; want %d minutes", taken.size, minutes, err, taken.minutes)
		}
		tree, err := Parse(plan(taken.size))
		if err != nil {
			t.Errorf("parse a plan task of the size %q: %v, want it taken", taken.size, err)
			continue
		}
		rendered, offsets, err := RenderWithBlockOffsets(tree)
		if err != nil {
			t.Fatalf("render a plan task of the size %q: %v", taken.size, err)
		}
		if rendered != plan(taken.size) {
			t.Errorf("a plan task of the size %q renders as %q, want its text as written, %q", taken.size, rendered, plan(taken.size))
		}
		want := []model.BlockAttributeValue{{Name: "size", Value: taken.size, Kind: string(BlockAttributeMinutes)}}
		if got := attributesOf(offsets, "t1"); !reflect.DeepEqual(got, want) {
			t.Errorf("a plan task of the size %q carries the attributes %+v, want %+v", taken.size, got, want)
		}
	}
	const shape = "is not a duration: a whole number of hours, a whole number of minutes, or both with the hours first"
	const past = "is more than 10000 minutes"
	for _, refused := range []struct{ size, reason string }{
		{"10001m", past}, {"166h41m", past}, {"167h", past}, {"100000m", past}, {"99999999999999999999h", past},
		{"0m", shape}, {"0h", shape}, {"1h0m", shape}, {"0h30m", shape}, {"-5m", shape}, {"-1h", shape},
		{"2d", shape}, {"1.5h", shape}, {"90", shape}, {"30m1h", shape}, {"045m", shape}, {"1H", shape},
		{"1h 30m", shape},
	} {
		if minutes, err := Minutes(refused.size); err == nil || !strings.Contains(err.Error(), refused.reason) {
			t.Errorf("Minutes(%q) = %d, %v; want a refusal saying it %s", refused.size, minutes, err, refused.reason)
		}
		if _, err := Parse(plan(refused.size)); err == nil || !strings.Contains(err.Error(), refused.reason) {
			t.Errorf("parse a plan task of the size %q: %v, want a refusal saying it %s", refused.size, err, refused.reason)
		}
	}
	if minutes, err := Minutes(""); err == nil || !strings.Contains(err.Error(), shape) {
		t.Errorf(`Minutes("") = %d, %v; want a refusal saying it %s`, minutes, err, shape)
	}
}

// A plan task also says what its implementer should read (R18): `skills` and `docs`, each an
// attribute of kind `string[]` with no default, written in the block's text as a JSON array of
// strings, which the render's attribute carries as that array, beside the size in the order the
// type declares them. A task written without them holds neither in its text, and an empty string
// in either is refused, as it is in every `string[]` value.
func TestAPlanTasksSkillsAndDocsAreStringListsWithNoDefault(t *testing.T) {
	t.Parallel()
	typ, declared := typedBlock("plan_task")
	if !declared {
		t.Fatal("the schema declares no plan_task block")
	}
	for _, name := range []string{"skills", "docs"} {
		attribute, held := typ.Attributes.Lookup(name)
		if !held || attribute.Kind != BlockAttributeStringList || attribute.Default != nil {
			t.Errorf("plan_task declares %s as %+v (declared %t), want a string[] with no default", name, attribute, held)
		}
	}
	if size, held := typ.Attributes.Lookup("size"); !held || size.Kind != BlockAttributeMinutes || size.Default != nil {
		t.Errorf("plan_task declares size as %+v (declared %t), want a minutes attribute with no default", size, held)
	}

	const written = ":::plan_task{#t1 size=\"45m\" skills=\"[&#x22;writing-plans&#x22;,&#x22;dispatch&#x22;]\" docs=\"[&#x22;docs/testing.md&#x22;,&#x22;packages/envoy/AGENTS.md&#x22;]\"}\nDo it.\n:::\n"
	tree, err := Parse(written)
	if err != nil {
		t.Fatalf("parse a plan task with skills and docs: %v", err)
	}
	rendered, offsets, err := RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render a plan task with skills and docs: %v", err)
	}
	if rendered != written {
		t.Errorf("a plan task with skills and docs renders as %q, want %q", rendered, written)
	}
	want := []model.BlockAttributeValue{
		{Name: "size", Value: "45m", Kind: string(BlockAttributeMinutes)},
		{Name: "skills", Value: `["writing-plans","dispatch"]`, Kind: string(BlockAttributeStringList)},
		{Name: "docs", Value: `["docs/testing.md","packages/envoy/AGENTS.md"]`, Kind: string(BlockAttributeStringList)},
	}
	if got := attributesOf(offsets, "t1"); !reflect.DeepEqual(got, want) {
		t.Errorf("a plan task with skills and docs carries the attributes %+v, want %+v", got, want)
	}

	const bare = ":::plan_task{#t2}\nDo it.\n:::\n"
	if tree, err = Parse(bare); err != nil {
		t.Fatalf("parse a plan task with neither: %v", err)
	}
	if rendered, offsets, err = RenderWithBlockOffsets(tree); err != nil || rendered != bare || attributesOf(offsets, "t2") != nil {
		t.Errorf("a plan task written with neither renders as %q with the attributes %+v (%v), want %q with none", rendered, attributesOf(offsets, "t2"), err, bare)
	}

	for _, name := range []string{"skills", "docs"} {
		empty := ":::plan_task{#t3 " + name + "=\"[&#x22;&#x22;]\"}\nDo it.\n:::\n"
		if _, err := Parse(empty); err == nil || !strings.Contains(err.Error(), "must not contain an empty string") {
			t.Errorf("parse a plan task whose %s holds an empty string: %v, want it refused", name, err)
		}
	}
}

// attributesOf is the attributes the render's offsets give the block id.
func attributesOf(offsets []BlockOffset, id string) []model.BlockAttributeValue {
	for _, offset := range offsets {
		if offset.ID == id {
			return offset.Attributes
		}
	}
	return nil
}
