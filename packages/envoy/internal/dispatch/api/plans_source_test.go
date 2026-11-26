package api

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A plan's source is one of the store's sources, which two enum tags restate for the schema: the
// request's (planRecordInput.Source) and the answer's (model.PlanRecord.Source). The store's own list
// is read from its refusal of a source it does not take, which names every source it takes, so a
// source the store gains and a tag lacks fails here rather than in a client the schema refuses it to.
func TestThePlanSourceTagsNameTheStoresSources(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	response := putPlanRecord(t, handler, issue, uuid.NewString(),
		withFields(defaultPlanBody(daemonSessionOf("LEG"), 1), map[string]any{"source": "no-such-source"}))
	refusal := decodeBody[refusalAnswer](t, response)
	_, listed, named := strings.Cut(refusal.Error, "one of ")
	if response.Code != http.StatusBadRequest || refusal.Code != codePlanInput || !named {
		t.Fatalf("an unknown source: status=%d body=%s, want 400 %s naming the sources", response.Code, response.Body.String(), codePlanInput)
	}
	sources := strings.Split(listed, ", ")
	for _, shape := range []reflect.Type{reflect.TypeFor[planRecordInput](), reflect.TypeFor[model.PlanRecord]()} {
		field, _ := shape.FieldByName("Source")
		if tagged := strings.Split(field.Tag.Get("enum"), ","); !slices.Equal(tagged, sources) {
			t.Errorf("%s.Source is tagged enum %v, want the store's sources %v", shape, tagged, sources)
		}
	}
}
