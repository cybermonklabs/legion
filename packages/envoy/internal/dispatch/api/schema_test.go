package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// readBlockSchemaRoute reads GET /api/v1/schema/blocks as an anonymous caller, which the public
// route takes, and answers its body decoded both as the route declares it and as the bytes.
func readBlockSchemaRoute(t *testing.T, handler http.Handler) (blockSchemaRead, []byte) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/schema/blocks", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/schema/blocks status = %d, body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("schema content type = %q, want application/json", got)
	}
	var read blockSchemaRead
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the schema route's answer: %v", err)
	}
	return read, response.Body.Bytes()
}

// The schema route answers the block schema this process installed, member for member, so a
// reader of the block schema alone reads what it always read; and beside it the store's types
// that declare no block type, with their attributes in order, and the lens catalogue, read from
// the store.
func TestBlockSchemaRouteServesTheInstalledSchemaBesideTheStoresTypes(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	read, body := readBlockSchemaRoute(t, handler)

	var served, installed pmdoc.BlockSchema
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the schema route's answer as a block schema: %v", err)
	}
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &installed); err != nil {
		t.Fatalf("decode the installed schema: %v", err)
	}
	servedText, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the served schema: %v", err)
	}
	installedText, err := json.Marshal(installed)
	if err != nil {
		t.Fatalf("encode the installed schema: %v", err)
	}
	if string(servedText) != string(installedText) {
		t.Fatalf("the route's block schema differs from the installed one\nserved:    %s\ninstalled: %s", servedText, installedText)
	}
	names := make([]string, len(served.Types))
	for index, typ := range served.Types {
		names[index] = typ.Name
	}
	want := []string{"callout", "ask", "requirement", "plan_task", "check", "decision", "reading", "criterion"}
	if served.Version != 8 || !slices.Equal(names, want) {
		t.Fatalf("schema = version %d, types %v; want version 8 and %v", served.Version, names, want)
	}
	if parent, declared := served.Types[7].Attributes.Lookup(pmdoc.ParentAttribute); !declared || parent.Kind != pmdoc.BlockAttributeBlockList ||
		!slices.Equal(parent.Items, []pmdoc.BlockItemKind{pmdoc.BlockItemAsk, pmdoc.BlockItemComment, pmdoc.BlockItemMessage}) {
		t.Fatalf("criterion attribute parent = %+v (declared %v), want a block[] naming ask, comment and message", parent, declared)
	}
	if ciCheck, declared := served.Types[4].Attributes.Lookup("ci_check"); !declared || ciCheck.Kind != pmdoc.BlockAttributeString {
		t.Fatalf("check attribute ci_check = %+v (declared %v), want a string", ciCheck, declared)
	}
	if parents := served.Types[6].Parents; !slices.Equal(parents, []string{"decision", "ask"}) {
		t.Fatalf("reading parents = %v, want decision and ask", parents)
	}
	// decision and reading are read from version 6 and created from version 7: a build of version 6
	// reads a store this build wrote and refuses to create them (readers_first_test.go), and version
	// 7, the slice that gives them nodes, creates them.
	for _, typ := range served.Types[5:7] {
		if typ.Since != 6 || typ.CreatableSince != 7 {
			t.Fatalf("typed block %s = since %d, creatable_since %d; want it read from version 6 and created from version 7", typ.Name, typ.Since, typ.CreatableSince)
		}
	}
	if criterion := served.Types[7]; criterion.Since != 8 || criterion.CreatableSince != 8 {
		t.Fatalf("typed block criterion = since %d, creatable_since %d; want it read and created from version 8", criterion.Since, criterion.CreatableSince)
	}
	if shaped, declared := served.Types[1].Attributes.Lookup("shaped"); !declared || shaped.Since != 6 || shaped.CreatableSince != 7 {
		t.Fatalf("ask attribute shaped = %+v (declared %v), want it read from version 6 and created from version 7", shaped, declared)
	}
	if steps, declared := served.Types[3].Attributes.Lookup("steps"); !declared || steps.Kind != pmdoc.BlockAttributeString || steps.Since != 5 || steps.CreatableSince != 5 {
		t.Fatalf("plan_task attribute steps = %+v (declared %v), want a string read and created from version 5", steps, declared)
	}

	types := map[string][]storeAttributeRead{}
	for _, typ := range read.StoreTypes {
		types[typ.Name] = typ.Attributes
		if slices.Contains(names, typ.Name) {
			t.Errorf("store_types lists %s, which is a block type of the schema", typ.Name)
		}
	}
	for name, want := range map[string][]storeAttributeRead{
		graphstore.KindLensKind: {{Name: "words", Kind: "string"}},
		graphstore.KindLens:     {{Name: "verdict", Kind: "string"}, {Name: "artifact_id", Kind: "string"}},
		graphstore.KindRisk:     {{Name: "heading", Kind: "string"}, {Name: "early_warning", Kind: "string"}, {Name: "prevention", Kind: "string"}},
		graphstore.KindConstraint: {
			{Name: "standard", Kind: "string"}, {Name: "reference", Kind: "string"}, {Name: "measurement", Kind: "string"},
			{Name: "value", Kind: "string"}, {Name: "taken_where", Kind: "string"},
		},
		graphstore.KindInteraction: {{Name: "words", Kind: "string"}, {Name: "ends_at", Kind: "timestamp"}, {Name: "frame", Kind: "string"}, {Name: "source", Kind: "string"}},
		graphstore.KindComment:     {},
	} {
		held, listed := types[name]
		if !listed || !slices.Equal(held, want) {
			t.Errorf("store_types lists %s with %v (listed %t), want %v", name, held, listed, want)
		}
	}

	if len(read.LensKinds) < 10 {
		t.Fatalf("the route answers %d lens kinds, want the catalogue's", len(read.LensKinds))
	}
	for _, kind := range read.LensKinds {
		if kind.Ref != "dispatch://lens_kind/"+kind.ID || kind.Words == "" || len(kind.Sources) != 1 {
			t.Errorf("lens kind %+v, want its address, its words and one source", kind)
			continue
		}
		if source := kind.Sources[0]; source.Ref != "dispatch://constraint/"+source.ID || source.Standard == "" || source.Reference == "" {
			t.Errorf("lens kind %s's source %+v, want its address, a standard and a reference", kind.ID, source)
		}
	}
}
