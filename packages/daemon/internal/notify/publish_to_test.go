package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PublishTo answers the session the listener's 200 names as the holder it forwarded the message
// to, and "" for a 200 that names none, as a listener older than the field answers; neither is an
// error, since the listener took the publish. A refusal is the error Publish makes of it.
func TestPublishToAnswersTheHolderTheListenerForwardedTo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		holder string
		fails  bool
	}{
		{name: "a 200 naming its holder", status: http.StatusOK, body: `{"id":"e1","holder":"ses_fresh","holder_state":"live"}`, holder: "ses_fresh"},
		{name: "a 200 naming none", status: http.StatusOK, body: `{"id":"e1"}`},
		{name: "a 200 with no body", status: http.StatusOK},
		{name: "a role nobody holds", status: http.StatusNotFound, body: `{"error":"no holder","reason":"unclaimed"}`, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			holder, err := New(server.URL, "").PublishTo(context.Background(), "notifications.role.owner", "an owner-step", map[string]string{"kind": "owner-step"}, "key-1")
			if (err != nil) != tc.fails || holder != tc.holder {
				t.Fatalf("PublishTo = %q, %v; want %q and an error %t", holder, err, tc.holder, tc.fails)
			}
		})
	}
}
