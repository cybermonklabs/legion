package listenerclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/listenerwire"
)

// Every call sends the bearer when the client holds one and no Authorization header when it holds
// none, whichever route it calls.
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
	for _, token := range []string{"", "stack-token"} {
		client := &Client{URL: server.URL, Token: token, HTTP: server.Client()}
		if _, err := client.Subscribe(ctx, listenerwire.SubscribeRequest{SessionID: "ses_a"}); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if err := client.Publish(ctx, listenerwire.PublishRequest{Topic: "notifications.team.a"}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if _, err := client.Unsubscribe(ctx, listenerwire.UnsubscribeRequest{SessionID: "ses_a"}); err != nil {
			t.Fatalf("unsubscribe: %v", err)
		}
		if _, _, err := client.Sessions(ctx); err != nil {
			t.Fatalf("sessions: %v", err)
		}
	}
	for _, route := range []string{"/v1/interests/subscribe", "/v1/messages/publish", "/v1/interests/unsubscribe", SessionsRoute} {
		got := headers[route]
		if len(got) != 2 || got[0] != "<none>" || got[1] != "Bearer stack-token" {
			t.Errorf("%s got Authorization %q, want none with no token and %q with one", route, got, "Bearer stack-token")
		}
	}
}
