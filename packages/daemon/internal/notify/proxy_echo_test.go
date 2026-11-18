package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A proxy in the listener's place can answer with a page of its own that echoes the request back,
// the bearer included. That body is not the listener's error JSON, so each refusal names it in one
// line with the bearer and any Authorization or Bearer value redacted, where the listener's own
// JSON still reads whole (TestHTTPPublisherTellsANoHolderRefusalFromAnyOther).
func TestEveryRefusalNamesAProxysPageWithoutTheBearer(t *testing.T) {
	const bearer = "envoy-bearer-Q7vK2mXr9LtW"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		_, sent, _ := strings.Cut(authorization, " ")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "<html><body><h1>502 Bad Gateway</h1><pre>Authorization: %s\nseen=%s</pre></body></html>", authorization, sent)
	}))
	defer server.Close()
	publisher := New(server.URL, bearer)
	ctx := context.Background()

	published := publisher.Publish(ctx, "notifications.legion.LEGION.LEGION-208", "held on LEGION-208", map[string]string{"kind": "held"}, "legion-outbox:3")
	_, read := publisher.ReadSessionRegistration(ctx, "ses_implementer")
	_, deleted := publisher.DeleteSession(ctx, "ses_implementer", DeleteConditions{NotRegisteredAfter: time.Now()})

	const page = `listener returned 502 Bad Gateway: a body that is not the listener's error JSON ("502 Bad Gateway Authorization: Bearer [redacted] seen=[redacted]")`
	for name, err := range map[string]error{"publish": published, "registration read": read, "delete": deleted} {
		if err == nil || strings.Contains(err.Error(), bearer) || !strings.HasSuffix(err.Error(), page) {
			t.Errorf("the %s's refusal = %v, want one ending %q and no bearer", name, err, page)
		}
	}
}

// The listener's own error JSON reads as it always did, whole, on the routes whose existing rows
// pass no body of that shape through a refusal message.
func TestARefusalStillNamesTheListenersOwnErrorWhole(t *testing.T) {
	const own = `{"error":"session registry unavailable","reason":"bus"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, own)
	}))
	defer server.Close()
	publisher := New(server.URL, "envoy-bearer")

	_, read := publisher.ReadSessionRegistration(context.Background(), "ses_implementer")
	_, deleted := publisher.DeleteSession(context.Background(), "ses_implementer", DeleteConditions{NotRegisteredAfter: time.Now()})
	for name, err := range map[string]error{"registration read": read, "delete": deleted} {
		if want := "listener returned 503 Service Unavailable: " + own; err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("the %s's refusal = %v, want one ending %q", name, err, want)
		}
	}
}
