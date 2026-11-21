package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// legionSettingResponse is what GET and PUT /api/v1/projects/{key}/legion/daemon answer.
type legionSettingResponse struct {
	Project   string `json:"project"`
	StateURL  string `json:"state_url"`
	CreatedBy struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"created_by"`
	UpdatedAt string `json:"updated_at"`
}

// seedLegionProjects creates each project a Legion settings test writes.
func seedLegionProjects(t *testing.T, handler http.Handler, keys ...string) {
	t.Helper()
	for _, key := range keys {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": key}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", key, created.Code, created.Body.String())
		}
	}
}

// TestALegionSettingRoundTripsAndIsRemoved: a project that names no daemon reads null, a PUT names
// its daemon, the GET reads it with the actor that set it, a second PUT moves the URL and keeps
// that actor, and the DELETE removes it, leaving every other project's alone.
func TestALegionSettingRoundTripsAndIsRemoved(t *testing.T) {
	handler := newTestHandler(t)
	seedLegionProjects(t, handler, "INFRA", "CORE")

	read := func(project string) *httptest.ResponseRecorder {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+project+"/legion/daemon", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s's daemon: status=%d body=%s", project, response.Code, response.Body.String())
		}
		return response
	}
	if none := strings.TrimSpace(read("INFRA").Body.String()); none != "null" {
		t.Fatalf("a project with no daemon read %s, want null", none)
	}

	written := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/INFRA/legion/daemon",
		map[string]any{"state_url": "http://legion-infra.internal:8080/"}, "alice")
	if written.Code != http.StatusOK {
		t.Fatalf("set INFRA's daemon: status=%d body=%s", written.Code, written.Body.String())
	}
	setting := decodeBody[legionSettingResponse](t, written)
	if setting.StateURL != "http://legion-infra.internal:8080" {
		t.Errorf("state_url = %q, want the trailing slash trimmed", setting.StateURL)
	}
	if setting.CreatedBy.Kind != "user" || setting.CreatedBy.ID != "alice" {
		t.Errorf("created_by = %+v, want the person who set it", setting.CreatedBy)
	}

	if moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/INFRA/legion/daemon",
		map[string]any{"state_url": "https://legion-infra.internal"}, "bob"); moved.Code != http.StatusOK {
		t.Fatalf("move INFRA's daemon: status=%d body=%s", moved.Code, moved.Body.String())
	}
	if again := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/legion/daemon",
		map[string]any{"state_url": "https://legion-core.internal"}, "alice"); again.Code != http.StatusOK {
		t.Fatalf("set CORE's daemon: status=%d body=%s", again.Code, again.Body.String())
	}

	infra := decodeBody[legionSettingResponse](t, read("INFRA"))
	if infra.Project != "INFRA" || infra.StateURL != "https://legion-infra.internal" {
		t.Errorf("INFRA reads %+v, want its moved URL", infra)
	}
	if infra.CreatedBy.ID != "alice" {
		t.Errorf("INFRA's created_by = %q, want the actor that first set it", infra.CreatedBy.ID)
	}
	if core := decodeBody[legionSettingResponse](t, read("CORE")); core.StateURL != "https://legion-core.internal" {
		t.Errorf("CORE reads %+v, want its own URL", core)
	}

	removed := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/projects/INFRA/legion/daemon", nil, "alice")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("remove INFRA's daemon: status=%d body=%s", removed.Code, removed.Body.String())
	}
	if again := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/projects/INFRA/legion/daemon", nil, "alice"); again.Code != http.StatusNotFound {
		t.Errorf("removing it twice: status=%d body=%s, want 404", again.Code, again.Body.String())
	}
	if none := strings.TrimSpace(read("INFRA").Body.String()); none != "null" {
		t.Errorf("INFRA read %s after the delete, want null", none)
	}
	if core := decodeBody[legionSettingResponse](t, read("CORE")); core.Project != "CORE" {
		t.Errorf("CORE reads %+v after INFRA's delete, want its daemon untouched", core)
	}
}

// TestAnUnknownProjectAndABadStateURLAreRefused: a setting names a project that exists, and a URL
// every call can be built from. The project is read as every project route reads its path, so a
// badly spelled key is 400 INVALID_PROJECT and an unknown project loadProject's 404
// PROJECT_NOT_FOUND, whichever of the three daemon routes is called.
func TestAnUnknownProjectAndABadStateURLAreRefused(t *testing.T) {
	handler := newTestHandler(t)
	seedLegionProjects(t, handler, "INFRA")

	for _, refused := range []struct {
		project, code string
		status        int
	}{
		{"NOPE", "PROJECT_NOT_FOUND", http.StatusNotFound},
		{"infra", "INVALID_PROJECT", http.StatusBadRequest},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			var body any
			if method == http.MethodPut {
				body = map[string]any{"state_url": "https://legion.internal"}
			}
			response := dispatchRequest(t, handler, method, "/api/v1/projects/"+refused.project+"/legion/daemon", body, "alice")
			if code, _ := refusalOf(t, response); response.Code != refused.status || code != refused.code {
				t.Errorf("a %s naming %q: status=%d body=%s, want %d %s",
					method, refused.project, response.Code, response.Body.String(), refused.status, refused.code)
			}
		}
	}

	for name, value := range map[string]string{
		"nothing":      "",
		"no scheme":    "legion.internal:8080",
		"a path":       "https://legion.internal/legion",
		"a query":      "https://legion.internal?token=secret",
		"a credential": "https://user:pass@legion.internal",
	} {
		refused := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/INFRA/legion/daemon",
			map[string]any{"state_url": value}, "alice")
		if refused.Code != http.StatusBadRequest {
			t.Errorf("a state_url with %s: status=%d body=%s, want 400", name, refused.Code, refused.Body.String())
		}
	}
}

// TestALegionSettingIsRefusedToAProjectScopedToken: pointing Dispatch at a host belongs to the
// deployment, so a credential that may write one project is refused both writes by name, though
// the path names that very project.
func TestALegionSettingIsRefusedToAProjectScopedToken(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-that-may-not-point-dispatch-at-a-daemon"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")

	for _, write := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPut, "/api/v1/projects/" + brokerTestProject + "/legion/daemon", map[string]any{"state_url": "https://legion.internal"}},
		{http.MethodDelete, "/api/v1/projects/" + brokerTestProject + "/legion/daemon", nil},
	} {
		refused := rig.serve(caller.presenting(t, write.method, write.path, token, write.body))
		if refused.Code != http.StatusForbidden {
			t.Fatalf("%s %s under a project-scoped token: status=%d body=%s, want 403",
				write.method, write.path, refused.Code, refused.Body.String())
		}
		refusal := decodeBody[refusalAnswer](t, refused)
		if refusal.Code != apicode.ProjectScopeDeployment.V1Name() {
			t.Errorf("%s %s refusal = %+v, want %s", write.method, write.path, refusal, apicode.ProjectScopeDeployment.V1Name())
		}
		if !strings.Contains(refusal.Error, write.method+" ") {
			t.Errorf("%s %s refusal %q does not name the route", write.method, write.path, refusal.Error)
		}
	}
}
