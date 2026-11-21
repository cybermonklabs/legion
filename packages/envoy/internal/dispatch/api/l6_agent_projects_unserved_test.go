package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The OpenAPI document declares no `projects` on Agent: no answer of GET /api/v1/agents carries
// one (TestAnAgentsRowCarriesNoProjectsWhateverTheListenerSends), since agentResponse's field of
// that name shadows the listener's list and nothing sets it, so a client told of the member would
// wait for one it never reads. The schema still declares the members a row does carry, so a
// document that declared no Agent at all cannot pass. A row of unservedFields that names no field
// fails the emit, as a stale rawFieldSchemas row does, so the table cannot outlive what it leaves
// out.
func TestTheAgentSchemaDeclaresNoProjects(t *testing.T) {
	spec, err := openAPIDocument()
	if err != nil {
		t.Fatalf("emit the OpenAPI document: %v", err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("encode the OpenAPI document: %v", err)
	}
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode the OpenAPI document: %v", err)
	}
	agent, declared := document.Components.Schemas["Agent"]
	if !declared {
		t.Fatal("the document declares no Agent schema")
	}
	for _, served := range []string{"session_id", "title", "open_asks", "last_activity"} {
		if _, held := agent.Properties[served]; !held {
			t.Errorf("Agent declares no %s, a member every row of GET /api/v1/agents carries", served)
		}
	}
	if _, held := agent.Properties["projects"]; held {
		t.Errorf("Agent declares projects, which no answer carries: %s", agent.Properties["projects"])
	}

	stale := rawFieldKeyOf{Owner: reflect.TypeOf(agentResponse{}), Field: "no_such_member"}
	unservedFields[stale] = "a row naming no field"
	defer delete(unservedFields, stale)
	if _, err := openAPIDocument(); err == nil || !strings.Contains(err.Error(), "unservedFields names") {
		t.Errorf("an unservedFields row naming no field emitted %v, want the emit refused naming the row", err)
	}
}
