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
	if schema.Version != 10 || !slices.Equal(names, []string{"callout", "ask", "requirement", "plan_task", "plan_role", "check"}) {
		t.Fatalf("schema = version %d, types %v; want version 10 and callout, ask, requirement, plan_task, plan_role, check", schema.Version, names)
	}
	if ciCheck, declared := schema.Types[5].Attributes.Lookup("ci_check"); !declared || ciCheck.Kind != pmdoc.BlockAttributeString {
		t.Fatalf("check attribute ci_check = %+v (declared %v), want a string", ciCheck, declared)
	}
	if steps, declared := schema.Types[3].Attributes.Lookup("steps"); !declared || steps.Kind != pmdoc.BlockAttributeString || steps.Since != 5 || steps.CreatableSince != 5 {
		t.Fatalf("plan_task attribute steps = %+v (declared %v), want a string read and created from version 5", steps, declared)
	}
	role := schema.Types[4]
	if role.Content != "block+" || role.Since != 10 || role.CreatableSince != 10 {
		t.Fatalf("plan_role = content %q since %d creatable_since %d, want block+ read and created from version 10", role.Content, role.Since, role.CreatableSince)
	}
	for _, name := range []string{"type", "model"} {
		if attribute, declared := role.Attributes.Lookup(name); !declared || attribute.Kind != pmdoc.BlockAttributeString {
			t.Fatalf("plan_role attribute %s = %+v (declared %v), want a string", name, attribute, declared)
		}
	}
}
