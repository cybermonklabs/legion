package testenvoy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testenvoy"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// The three fields R5 reads out of Envoy's session list once a minute: which session a row is, the
// state it stands in, and the registration the 20 minutes are counted from. A listener release that
// renamed or dropped one of them would leave the daemon reading no registration for any session —
// which is either every claim of the project marked at once or none ever marked, and neither
// failure says anything about itself.
//
// Read from the listener this checkout builds, with a session registered through its own route,
// so the shape held here is the listener's own and not a copy of it in this repository.
func TestTheSessionListCarriesTheSessionItsStateAndItsLastRegistration(t *testing.T) {
	listener := testenvoy.Start(t, checkoutListener(t), testnats.URL(t), testenvoy.Options{LiveWindow: 5 * time.Minute})

	const session = "ses_probe"
	body := []byte(`{"session_id":"` + session + `","topics":["notifications.role.legion-LEGION-1-implementer"],` +
		`"dir":"/work","plugin":"@sjawhar/pi-envoy","plugin_version":"1.0.0",` +
		`"self_subscribed":true,"delivery":{"consumer":true,"epoch":"e"}}`)
	before := time.Now().Add(-time.Second).UnixMilli()
	subscribe(t, listener, body)

	raw := getRaw(t, listener, "/v1/sessions?include=unresponsive")
	var list []struct {
		SessionID string   `json:"session_id"`
		State     string   `json:"state"`
		LastSeen  int64    `json:"last_seen"`
		Roles     []string `json:"roles"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("the session list is not an array of rows: %v\n%s", err, raw)
	}
	if len(list) != 1 || list[0].SessionID != session {
		t.Fatalf("the list holds %d rows, want the one registered session %s: %s", len(list), session, raw)
	}
	if list[0].State != "live" {
		t.Errorf("state = %q, want live for a session that registered a moment ago: %s", list[0].State, raw)
	}
	if list[0].LastSeen < before {
		t.Errorf("last_seen = %d, want the registration's own moment, at or after %d: %s", list[0].LastSeen, before, raw)
	}
	if list[0].Roles == nil {
		t.Errorf("roles is absent; a row carries the roles whose claims name the session, empty for none: %s", raw)
	}
}

// subscribe registers a session with the listener through the route a session's own plugin uses.
func subscribe(t *testing.T, listener *testenvoy.Listener, body []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, listener.BaseURL()+"/v1/interests/subscribe", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+listener.Token())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("subscribe: %v\n%s", err, listener.Output())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		answer := make([]byte, 4096)
		n, _ := response.Body.Read(answer)
		t.Fatalf("subscribe = %d: %s", response.StatusCode, answer[:n])
	}
}
