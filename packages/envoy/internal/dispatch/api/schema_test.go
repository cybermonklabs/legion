package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestBlockSchemaRouteServesEmbeddedSchema(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	Register(mux, t.Context(), Deps{})

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/schema/blocks", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/schema/blocks status = %d, body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("schema content type = %q, want application/json", got)
	}
	if response.Body.String() != string(pmdoc.SchemaJSON()) {
		t.Fatalf("schema response differs from embedded schema\nresponse: %s\nembedded: %s", response.Body.String(), pmdoc.SchemaJSON())
	}
	var schema pmdoc.BlockSchema
	if err := json.Unmarshal(response.Body.Bytes(), &schema); err != nil {
		t.Fatalf("decode schema response: %v", err)
	}
	names := make([]string, len(schema.Types))
	for index, typ := range schema.Types {
		names[index] = typ.Name
	}
	if schema.Version != 12 || !slices.Equal(names, []string{"callout", "ask", "requirement", "plan_task", "check"}) {
		t.Fatalf("schema = version %d, types %v; want version 12 and callout, ask, requirement, plan_task, check", schema.Version, names)
	}
	if ciCheck, declared := schema.Types[4].Attributes.Lookup("ci_check"); !declared || ciCheck.Kind != pmdoc.BlockAttributeString {
		t.Fatalf("check attribute ci_check = %+v (declared %v), want a string", ciCheck, declared)
	}
	if steps, declared := schema.Types[3].Attributes.Lookup("steps"); !declared || steps.Kind != pmdoc.BlockAttributeString || steps.Since != 5 || steps.CreatableSince != 5 {
		t.Fatalf("plan_task attribute steps = %+v (declared %v), want a string read and created from version 5", steps, declared)
	}
	if merges, declared := schema.Types[3].Attributes.Lookup("merges"); !declared || merges.Kind != pmdoc.BlockAttributeEnum || !slices.Equal(merges.Choices, []string{"true", "false"}) || merges.Since != 11 || merges.CreatableSince != 11 {
		t.Fatalf("plan_task attribute merges = %+v (declared %v), want an enum of true and false read and created from version 11", merges, declared)
	}
	if owner, declared := schema.Types[3].Attributes.Lookup("owner"); !declared || owner.Kind != pmdoc.BlockAttributeString || owner.Since != 12 || owner.CreatableSince != 12 {
		t.Fatalf("plan_task attribute owner = %+v (declared %v), want a string read and created from version 12", owner, declared)
	}
	if subject, declared := schema.Types[4].Attributes.Lookup("subject"); !declared || subject.Kind != pmdoc.BlockAttributeEnum ||
		!slices.Equal(subject.Choices, []string{"code", "production", "graph"}) || subject.Since != 6 || subject.CreatableSince != 7 {
		t.Fatalf("check attribute subject = %+v (declared %v), want an enum of code, production and graph, read from version 6 and created from 7", subject, declared)
	}
	if program, declared := schema.Types[4].Attributes.Lookup("program"); !declared || program.Kind != pmdoc.BlockAttributeString || program.Since != 6 || program.CreatableSince != 7 {
		t.Fatalf("check attribute program = %+v (declared %v), want a string read from version 6 and created from 7", program, declared)
	}
	if partOf, declared := schema.Types[3].Attributes.Lookup("part_of"); declared {
		t.Fatalf("plan_task declares attribute part_of = %+v, want none", partOf)
	}
}
