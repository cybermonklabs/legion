package testenvoy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testenvoy"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

func TestMain(m *testing.M) {
	os.Exit(testbuild.Main(func() int { return testnats.Main(m) }))
}

// checkoutListener is this checkout's listener, which the helper's own tests run: they hold what
// the helper does, not which release production pins (PinnedBinary, pinned.go).
func checkoutListener(t *testing.T) string {
	t.Helper()
	return testbuild.EnvoyBinary(t, "./cmd/listener")
}

// The rig of this change's checks runs a real Envoy listener, not the stand-in the daemon's other
// live proofs read their own publishes back from: R4's fixes go in the session's plugin and have to
// work against the listener production runs, and R5's mark is read out of that listener's own
// session list.
//
// This is the seam itself: a started listener that answers its health and its session list. A
// test of the mark drives sessions through it; what is held here is that the helper gives a live
// listener and the two reads the daemon makes of it.
func TestAStartedListenerAnswersItsHealthAndItsSessionList(t *testing.T) {
	listener := testenvoy.Start(t, checkoutListener(t), testnats.URL(t), testenvoy.Options{})

	health := get(t, listener, "/healthz")
	if health["status"] != "healthy" {
		t.Fatalf("the listener's /healthz says %v, want healthy: %v", health["status"], health)
	}
	// The field the daemon's own code names when it says how long a record stands (the change's
	// spec reads it as 5 minutes in production).
	if _, named := health["session_max_age"]; !named {
		t.Errorf("/healthz carries no session_max_age: %v", health)
	}

	// The read R5 makes once a minute. An empty server lists no session, which is a list and not
	// a refusal: a daemon that could not tell an empty list from a failed read would mark every
	// claim of the project at once.
	sessions := getRaw(t, listener, "/v1/sessions?include=unresponsive")
	var list []map[string]any
	if err := json.Unmarshal(sessions, &list); err != nil {
		var wrapped map[string]json.RawMessage
		if err2 := json.Unmarshal(sessions, &wrapped); err2 != nil {
			t.Fatalf("the session list is neither an array nor an object: %v / %v (%s)", err, err2, sessions)
		}
		if _, refused := wrapped["error"]; refused {
			t.Fatalf("the session list answered an error: %s", sessions)
		}
	}
	if len(list) != 0 {
		t.Errorf("a listener nobody has registered with lists %d sessions, want none: %s", len(list), sessions)
	}
}

// The helper's own windows reach the listener, so a check that needs a session to read
// unresponsive in seconds rather than in five minutes can say so.
func TestTheWindowsTheCallerNamesReachTheListener(t *testing.T) {
	listener := testenvoy.Start(t, checkoutListener(t), testnats.URL(t), testenvoy.Options{
		LiveWindow:  2 * time.Second,
		GoneHorizon: time.Minute,
		SessionAge:  10 * time.Minute,
	})

	health := get(t, listener, "/healthz")
	if got := health["session_max_age"]; got != "10m0s" {
		t.Errorf("session_max_age = %v, want the 10m0s the caller asked for; the listener took none of the helper's windows", got)
	}
}

// The daemon reads the listener's bearer from a file (`envoy_token_file`), so the helper writes
// one: a live proof names it in the daemon's configuration rather than carrying the token about.
func TestTheTokenFileHoldsTheBearerTheListenerTakes(t *testing.T) {
	listener := testenvoy.Start(t, checkoutListener(t), testnats.URL(t), testenvoy.Options{})

	path := listener.TokenFile(t)
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the token file: %v", err)
	}
	if string(held) != listener.Token() {
		t.Errorf("the token file holds %d bytes that are not the listener's bearer", len(held))
	}
	if second := listener.TokenFile(t); second != path {
		t.Errorf("TokenFile wrote a second file %s; one listener has one token file", second)
	}
}
func get(t *testing.T, listener *testenvoy.Listener, path string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(getRaw(t, listener, path), &body); err != nil {
		t.Fatalf("GET %s is not a JSON object: %v", path, err)
	}
	return body
}

func getRaw(t *testing.T, listener *testenvoy.Listener, path string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, listener.BaseURL()+path, nil)
	if err != nil {
		t.Fatalf("build the request for %s: %v", path, err)
	}
	if token := listener.Token(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	body := make([]byte, 0, 4096)
	buffer := make([]byte, 4096)
	for {
		n, err := response.Body.Read(buffer)
		body = append(body, buffer[:n]...)
		if err != nil {
			break
		}
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s\nlistener output:\n%s", path, response.StatusCode, body, listener.Output())
	}
	return body
}
