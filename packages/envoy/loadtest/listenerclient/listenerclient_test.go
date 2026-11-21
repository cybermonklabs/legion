package listenerclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/listenerwire"
)

// Every call sends the bearer Bearer answers at that call, and no Authorization header when it
// answers none or the client has no Bearer, whichever route it calls; a Bearer that fails refuses
// the call before anything reaches the listener.
func TestEveryCallSendsTheBearerOnlyWhenThereIsOne(t *testing.T) {
	var mu sync.Mutex
	headers := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers[r.URL.Path] = append(headers[r.URL.Path], r.Header.Values("Authorization")...)
		if len(r.Header.Values("Authorization")) == 0 {
			headers[r.URL.Path] = append(headers[r.URL.Path], "<none>")
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	ctx := context.Background()
	calls := func(client *Client) []error {
		_, subscribe := client.Subscribe(ctx, listenerwire.SubscribeRequest{SessionID: "ses_a"})
		publish := client.Publish(ctx, listenerwire.PublishRequest{Topic: "notifications.team.a"})
		_, unsubscribe := client.Unsubscribe(ctx, listenerwire.UnsubscribeRequest{SessionID: "ses_a"})
		_, _, sessions := client.Sessions(ctx)
		return []error{subscribe, publish, unsubscribe, sessions}
	}
	for _, bearer := range []func() (string, error){
		nil,
		func() (string, error) { return "", nil },
		func() (string, error) { return "stack-token", nil },
	} {
		for _, err := range calls(&Client{URL: server.URL, Bearer: bearer, HTTP: server.Client()}) {
			if err != nil {
				t.Fatalf("a call: %v", err)
			}
		}
	}
	unreadable := errors.New("the token file cannot be read")
	for _, err := range calls(&Client{URL: server.URL, Bearer: func() (string, error) { return "", unreadable }, HTTP: server.Client()}) {
		if !errors.Is(err, unreadable) {
			t.Errorf("a call whose Bearer failed answered %v, want the Bearer's error", err)
		}
	}
	for _, route := range []string{"/v1/interests/subscribe", "/v1/messages/publish", "/v1/interests/unsubscribe", SessionsRoute} {
		got := headers[route]
		if len(got) != 3 || got[0] != "<none>" || got[1] != "<none>" || got[2] != "Bearer stack-token" {
			t.Errorf("%s got Authorization %q, want none with no Bearer or an empty one, %q with one, and no request when the Bearer failed", route, got, "Bearer stack-token")
		}
	}
}
