package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// pprofListening is the line Dispatch writes once the profiler listens (startPprof), with the
// address it bound.
var pprofListening = regexp.MustCompile(`msg="dispatch: pprof listening" addr=(127\.0\.0\.1:[0-9]+)`)

// What the document rooms hold - how many rooms are resident, how many browsers are connected,
// what their appends cost - is invisible from outside the process unless it is exported. The
// loopback profiler's /debug/vars serves it as the expvar variable dispatch_rooms, which an
// operator reads through `kubectl port-forward` while people edit: after two editors join two
// documents, it names two resident rooms and two connections.
//
// The profiler takes port 0, a port the kernel chooses at its bind, and the test reads the port
// from the line naming it: a port the test picked and let go of is any process's to take before
// Dispatch binds it, which refuses the boot.
func TestDispatchRoomsReportsTheRoomsHeld(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Env: map[string]string{"DISPATCH_PPROF_PORT": "0"},
	})
	process.waitHealthy(t)
	// The profiler binds before the server serves, so the line is in the output by now.
	listening := pprofListening.FindStringSubmatch(process.Output.String())
	if listening == nil {
		t.Fatalf("Dispatch logged no line naming the profiler's address:\n%s", process.Output.String())
	}
	process.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ROOM","name":"Rooms"}`, http.StatusCreated)
	for _, spec := range []string{"first", "second"} {
		var issue struct {
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		created := process.call(t, http.MethodPost, "/api/v1/issues", `{"project":"ROOM","title":"Room `+spec+`","spec":"`+spec+`"}`, http.StatusCreated)
		if err := json.Unmarshal(created, &issue); err != nil || issue.PrimaryArtifactID == "" {
			t.Fatalf("create issue: %v: %s", err, created)
		}
		process.openEditor(t, issue.PrimaryArtifactID, spec+"\n")
	}

	response, err := http.Get("http://" + listening[1] + "/debug/vars")
	if err != nil {
		t.Fatalf("GET /debug/vars on the profiler: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var vars map[string]json.RawMessage
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &vars) != nil {
		t.Fatalf("GET /debug/vars on the profiler = %d %.200q, want the process's expvar variables", response.StatusCode, body)
	}
	exported, ok := vars["dispatch_rooms"]
	if !ok {
		t.Fatalf("/debug/vars serves no dispatch_rooms variable")
	}
	var rooms struct {
		ResidentRooms *int `json:"resident_rooms"`
		Connections   *int `json:"connections"`
	}
	if err := json.Unmarshal(exported, &rooms); err != nil {
		t.Fatalf("decode dispatch_rooms %s: %v", exported, err)
	}
	if rooms.ResidentRooms == nil || *rooms.ResidentRooms != 2 || rooms.Connections == nil || *rooms.Connections != 2 {
		t.Fatalf("dispatch_rooms = %s, want resident_rooms 2 and connections 2 with two editors in two documents", exported)
	}
}
