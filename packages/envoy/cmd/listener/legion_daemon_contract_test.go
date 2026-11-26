package main

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// daemonWritesFixture holds the three requests the Legion daemon's registry mirror makes
// (packages/daemon/internal/agenthistory/mirror.go). Its own
// TestTheMirrorsWritesAreTheOnesTheFixtureHolds pins that the mirror sends exactly these, and this
// test replays them against the real listener mux over real NATS, so the two modules' sides of the
// contract are one file and neither can move alone. The daemon cannot drive this mux directly: it
// is package main, and the dependency runs the other way (the daemon requires github.com/sjawhar/
// envoy, never the reverse).
const daemonWritesFixture = "testdata/legion-daemon-writes.json"

// TestTheLegionDaemonsRegistryWritesAreTakenByTheRealMux: every write the daemon's mirror makes is
// taken by the routes as they are served, the project's answering record and both entry routes,
// and what the entry routes answer is the entry the daemon sent.
func TestTheLegionDaemonsRegistryWritesAreTakenByTheRealMux(t *testing.T) {
	data, err := os.ReadFile(daemonWritesFixture)
	if err != nil {
		t.Fatalf("read %s: %v", daemonWritesFixture, err)
	}
	var writes []struct {
		Route  string          `json:"route"`
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(data, &writes); err != nil {
		t.Fatalf("decode %s: %v", daemonWritesFixture, err)
	}
	if len(writes) == 0 {
		t.Fatalf("%s holds no write", daemonWritesFixture)
	}

	listener := newLegionListener(t, setupPublishTestClient(t))
	for _, write := range writes {
		answer := listener.want(t, http.StatusOK, write.Method, write.Path, string(write.Body))
		if write.Route == "PUT /v1/legion/projects/{project}" {
			var stored struct {
				Answering bool   `json:"answering"`
				UpdatedAt string `json:"updated_at"`
			}
			if err := json.Unmarshal([]byte(answer), &stored); err != nil {
				t.Fatalf("decode the stored answering record %s: %v", answer, err)
			}
			if !stored.Answering || stored.UpdatedAt == "" {
				t.Fatalf("the stored answering record = %s, want the daemon's own", answer)
			}
			continue
		}
		var sent struct {
			Entries []legionAnswer `json:"entries"`
			legionAnswer
		}
		if err := json.Unmarshal(write.Body, &sent); err != nil {
			t.Fatalf("decode the daemon's %s body: %v", write.Route, err)
		}
		for _, entry := range append(sent.Entries, sent.legionAnswer) {
			if entry.SessionID == "" {
				continue
			}
			stored := decodeLegion[legionAnswer](t, listener.want(t, http.StatusOK, http.MethodGet,
				"/v1/legion/agents/sessions/"+entry.SessionID, ""))
			if stored.LegionProject != entry.LegionProject || stored.Issue != entry.Issue || stored.Role != entry.Role ||
				stored.RoleToken != entry.RoleToken || stored.Title != entry.Title || stored.Harness != entry.Harness ||
				stored.StartedAt != entry.StartedAt || stored.RecordedAt == "" {
				t.Fatalf("%s stored %#v, want the daemon's entry %#v", write.Route, stored, entry)
			}
		}
	}
}
