package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A struct tag cannot name a constant, so the enum tags the OpenAPI document reads off the step
// route's body and its answer spell the words of a step record out. Each is held here to the words
// contracts gives them (STEP_KINDS, STEP_RESULTS), which Dispatch's store and the Legion daemon
// read, so the document never names a word the store or the daemon does not hold. The answer's
// result is "" too, for a run that still runs.
func TestTheStepRouteNamesContractsWords(t *testing.T) {
	t.Parallel()
	kinds := strings.Join([]string{contracts.StepKindStep, contracts.StepKindRetro, contracts.StepKindMerge}, ",")
	results := strings.Join([]string{contracts.StepResultPassed, contracts.StepResultFailed, contracts.StepResultStopped}, ",")
	for _, field := range []struct {
		of    reflect.Type
		field string
		want  string
	}{
		{reflect.TypeFor[stepRecordInput](), "Kind", kinds},
		{reflect.TypeFor[stepRecordInput](), "Result", results},
		{reflect.TypeFor[model.StepRecord](), "Kind", kinds},
		{reflect.TypeFor[model.StepRecord](), "Result", "," + results},
	} {
		named, ok := field.of.FieldByName(field.field)
		if !ok {
			t.Fatalf("%s has no field %s", field.of, field.field)
		}
		if got := named.Tag.Get("enum"); got != field.want {
			t.Errorf("%s.%s names the enum %q, want contracts' %q", field.of, field.field, got, field.want)
		}
	}
}
