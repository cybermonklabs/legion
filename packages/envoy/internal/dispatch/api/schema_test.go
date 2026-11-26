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
	want := []string{"callout", "ask", "requirement", "plan_task", "check", "decision", "reading"}
	if schema.Version != 7 || !slices.Equal(names, want) {
		t.Fatalf("schema = version %d, types %v; want version 7 and %v", schema.Version, names, want)
	}
	if ciCheck, declared := schema.Types[4].Attributes.Lookup("ci_check"); !declared || ciCheck.Kind != pmdoc.BlockAttributeString {
		t.Fatalf("check attribute ci_check = %+v (declared %v), want a string", ciCheck, declared)
	}
	if parents := schema.Types[6].Parents; !slices.Equal(parents, []string{"decision", "ask"}) {
		t.Fatalf("reading parents = %v, want decision and ask", parents)
	}
	// decision and reading are read from version 6 and created from version 7: a build of version 6
	// reads a store this build wrote and refuses to create them (readers_first_test.go), and this
	// build, at version 7, the slice that gives them nodes, creates them.
	for _, typ := range schema.Types[5:] {
		if typ.Since != 6 || typ.CreatableSince != 7 {
			t.Fatalf("typed block %s = since %d, creatable_since %d; want it read from version 6 and created from version 7", typ.Name, typ.Since, typ.CreatableSince)
		}
	}
	if shaped, declared := schema.Types[1].Attributes.Lookup("shaped"); !declared || shaped.Since != 6 || shaped.CreatableSince != 7 {
		t.Fatalf("ask attribute shaped = %+v (declared %v), want it read from version 6 and created from version 7", shaped, declared)
	}
	if steps, declared := schema.Types[3].Attributes.Lookup("steps"); !declared || steps.Kind != pmdoc.BlockAttributeString || steps.Since != 5 || steps.CreatableSince != 5 {
		t.Fatalf("plan_task attribute steps = %+v (declared %v), want a string read and created from version 5", steps, declared)
	}
}
