package eval

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestTheWritingChecksDescriptionParsesAndLacksNothing(t *testing.T) {
	d, err := Parse(WritingCheck)
	if err != nil {
		t.Fatal(err)
	}
	if problems := d.Check(); len(problems) > 0 {
		t.Errorf("scanner.yaml lacks: %v", problems)
	}
	if d.Use.Purpose != UseGate || d.ErrorAnalysis.Size != 110 || d.ErrorAnalysis.SessionsOrIssues != 90 ||
		d.ErrorAnalysis.ReadsSinceLastNewKind != 44 || d.ErrorAnalysis.SampleSHA256 != "a5294a7aa614e476da3a6b551a47b91cab297fb8a608fe05e9b671ebf3cf697b" {
		t.Errorf("the error analysis: %+v", d.ErrorAnalysis)
	}
	kinds := 0
	for _, k := range d.ErrorAnalysis.FailureKinds {
		kinds += k.Count
	}
	if len(d.ErrorAnalysis.FailureKinds) != 14 || kinds != 232 {
		t.Errorf("%d failure kinds counting %d", len(d.ErrorAnalysis.FailureKinds), kinds)
	}
	if !d.FinalTest.Pending.Pending || d.FinalTest.Reason == "" {
		t.Error("the final test is not marked pending")
	}
	if !strings.Contains(d.Schedule.Note, "weekly live scoring is not built") {
		t.Errorf("schedule: %q", d.Schedule.Note)
	}
	if strings.Contains(string(WritingCheck), "TODO") {
		t.Error("scanner.yaml holds a TODO")
	}
}

func TestParseRefusesAFieldTheSchemaDoesNotName(t *testing.T) {
	if _, err := Parse(append([]byte("unknown_field: 1\n"), WritingCheck...)); err == nil {
		t.Error("an unknown field parsed")
	}
}

func TestCheckListsWhatADescriptionLacks(t *testing.T) {
	d, err := Parse(WritingCheck)
	if err != nil {
		t.Fatal(err)
	}
	d.Baseline = nil
	d.KPI = &KPI{Pending: Pending{Pending: true}}
	d.Ceilings = &Ceilings{}
	d.Definition.BoundaryCases = d.Definition.BoundaryCases[:2]
	got := strings.Join(d.Check(), "\n")
	for _, want := range []string{"baseline: missing", "kpi: pending with no reason", "ceilings: needs latency_seconds and tokens", "at least five"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
}

// TestTheSchemaNamesEveryField holds the schema and the Go type together: the schema's top-level
// properties are the description's fields, every one but rule_costs is required, and nothing else
// is allowed.
func TestTheSchemaNamesEveryField(t *testing.T) {
	var schema struct {
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties bool                       `json:"additionalProperties"`
		Defs                 map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(Schema, &schema); err != nil {
		t.Fatalf("scanner.schema.json: %v", err)
	}
	var fields []string
	typ := reflect.TypeFor[Description]()
	for i := range typ.NumField() {
		fields = append(fields, strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0])
	}
	var props []string
	for p := range schema.Properties {
		props = append(props, p)
	}
	slices.Sort(fields)
	slices.Sort(props)
	if !slices.Equal(fields, props) {
		t.Errorf("schema properties %v\nGo fields %v", props, fields)
	}
	want := slices.DeleteFunc(slices.Clone(fields), func(f string) bool { return f == "rule_costs" })
	required := slices.Clone(schema.Required)
	slices.Sort(required)
	if !slices.Equal(required, want) {
		t.Errorf("required %v, want %v", required, want)
	}
	if schema.AdditionalProperties {
		t.Error("the schema allows fields it does not name")
	}
	for _, method := range []string{"definition", "error_analysis", "eligible_population", "bootstrap_seed", "serves_top", "bundle_digest",
		"manifests", "final_test", "repeatability", "comparability_bars", "audit_sample_size", "ceilings", "use", "acceptance_bar",
		"reader_model", "summarizer", "labellers", "validate_task", "reproduce_task", "kpi", "schedule", "alert_route", "owner"} {
		if !slices.Contains(props, method) {
			t.Errorf("the schema leaves out the method's field %s", method)
		}
	}
	for _, def := range []string{"sha256", "share", "pendable", "is_pending", "manifest", "test_figures"} {
		if _, ok := schema.Defs[def]; !ok {
			t.Errorf("$defs lacks %s", def)
		}
	}
}

func TestEverySchemaRefResolves(t *testing.T) {
	var schema struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(Schema, &schema); err != nil {
		t.Fatal(err)
	}
	refs := 0
	for _, part := range strings.Split(string(Schema), `"$ref": "#/$defs/`)[1:] {
		name, _, _ := strings.Cut(part, `"`)
		refs++
		if _, ok := schema.Defs[name]; !ok {
			t.Errorf("$ref to #/$defs/%s, which is not defined", name)
		}
	}
	if refs == 0 {
		t.Error("no $ref found")
	}
}
