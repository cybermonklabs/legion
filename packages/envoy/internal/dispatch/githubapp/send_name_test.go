package githubapp

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestACallThatGotNoAnswerNamesTheURLItAskedAsUnansweredNamesIt: send names a request that got no
// answer in its own words by its target as ShownBody shows it, and then net/http's *url.Error
// through redact.Unanswered, which names the URL the request asked by the one naming every
// request with no answer gets: its scheme, host and path, never its query. The deliveries
// listing's first page asks ?per_page=100 of an address nothing listens at; the error names that
// target once, in send's words, then the URL without its query and net/http's own text of the
// refused connection, which it still reaches.
func TestACallThatGotNoAnswerNamesTheURLItAskedAsUnansweredNamesIt(t *testing.T) {
	dead := redirectstest.Dead(t)
	base := "http://" + dead
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Deliveries(context.Background(), time.Time{}, nil)
	want := "GET " + base + "/app/hook/deliveries?per_page=100: " +
		`Get "` + base + `/app/hook/deliveries": dial tcp ` + dead + ": connect: connection refused"
	if err == nil || err.Error() != want {
		t.Fatalf("Deliveries of an address nothing listens at = %v, want %q", err, want)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("Deliveries = %#v, which no longer finds the refused connection", err)
	}
}
