package agenthistory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// listenerWritesFixture holds the three requests the mirror makes of the Envoy listener's Legion
// agent registry. This test pins what the daemon sends; the listener's own
// TestTheLegionDaemonsRegistryWritesAreTakenByTheRealMux replays the same file against the real
// listener mux over real NATS, so the two modules' sides of the contract are one file and neither
// can move alone. The path reaches into packages/envoy, which the daemon module already requires
// at that place (the `replace github.com/sjawhar/envoy => ../envoy` in go.mod).
const listenerWritesFixture = "../../../envoy/cmd/listener/testdata/legion-daemon-writes.json"

// listenerWrite is one request of the fixture: the route it belongs to, for a reader, and the
// request itself.
type listenerWrite struct {
	Route  string          `json:"route"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

// recordingListener answers every registry write 200 and records it.
type recordingListener struct {
	*httptest.Server
	writes []listenerWrite
}

func newRecordingListener(t *testing.T) *recordingListener {
	t.Helper()
	listener := &recordingListener{}
	listener.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s %s: %v", r.Method, r.URL.Path, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		listener.writes = append(listener.writes, listenerWrite{Method: r.Method, Path: r.URL.Path, Body: body})
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(listener.Close)
	return listener
}

func TestTheMirrorsWritesAreTheOnesTheFixtureHolds(t *testing.T) {
	listener := newRecordingListener(t)
	history := testerRow(t, "ses-history", 0)
	registered := testerRow(t, "ses-registered", 5)
	mirror := NewMirror(MirrorConfig{
		BaseURL: listener.URL, Token: listenerToken, Project: legionProject, Answering: true,
		Store: &fakeStore{rows: []store.AgentConversation{history}}, Log: (&logs{}).logger(),
	})
	mirror.Put(registered)

	ctx := context.Background()
	mirror.beat(ctx, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	if result := mirror.writeAll(ctx); result != sent {
		t.Fatalf("the mirror's writes came to %v, want every one sent", result)
	}

	want := readListenerWrites(t)
	if len(listener.writes) != len(want) {
		t.Fatalf("the mirror made %d writes, want the fixture's %d", len(listener.writes), len(want))
	}
	for i, made := range listener.writes {
		if made.Method != want[i].Method || made.Path != want[i].Path {
			t.Fatalf("write %d = %s %s, want %s %s", i, made.Method, made.Path, want[i].Method, want[i].Path)
		}
		if got, expected := decodeJSON(t, made.Body), decodeJSON(t, want[i].Body); !reflect.DeepEqual(got, expected) {
			t.Fatalf("%s body = %s, want %s (update %s, which the listener's own contract test replays)",
				want[i].Route, made.Body, want[i].Body, listenerWritesFixture)
		}
	}
}

func readListenerWrites(t *testing.T) []listenerWrite {
	t.Helper()
	data, err := os.ReadFile(listenerWritesFixture)
	if err != nil {
		t.Fatalf("read the listener contract fixture: %v", err)
	}
	var writes []listenerWrite
	if err := json.Unmarshal(data, &writes); err != nil {
		t.Fatalf("decode the listener contract fixture: %v", err)
	}
	return writes
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return value
}
