package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheDashboardProbeAsksTheSSEFeedJoinedOntoTheDispatchURL: the Dispatch URL can carry a path
// and a query. The dashboard probe asks for the SSE feed at that path with the feed's joined onto
// it, the URL's query kept: http://host/prefix?key=... asks /prefix/api/v1/events?key=... (query),
// and so does http://host/prefix/?key=... (query-trailing-slash), where the feed's path appended
// to the URL's text sat inside the query, Dispatch was asked /prefix and the probe found no
// stream. A URL with no query asks where it did (no-query). A stand-in that serves the SSE feed
// alone records the path and the query each request reaches it with; the probe chooses the feed
// and answers the URL it asked.
func TestTheDashboardProbeAsksTheSSEFeedJoinedOntoTheDispatchURL(t *testing.T) {
	for _, c := range joinURLCases {
		t.Run(c.name, func(t *testing.T) {
			base, asked := joinAskedStand(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, ssePath) {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
			})
			kind, sseURL, err := probeDashboard(context.Background(), base+c.suffix)
			feed := 0
			for _, u := range asked() {
				if strings.HasSuffix(u.Path, ssePath) {
					feed++
					if want := "/prefix" + ssePath; u.Path != want || u.RawQuery != c.query {
						t.Errorf("a Dispatch URL ending %q asked the feed at path %q with query %q; want path %q with query %q", c.suffix, u.Path, u.RawQuery, want, c.query)
					}
				}
			}
			if feed != 1 {
				t.Errorf("a Dispatch URL ending %q asked the stand-in %v; want the SSE feed asked once", c.suffix, asked())
			}
			want := base + "/prefix" + ssePath
			if c.query != "" {
				want += "?" + c.query
			}
			if err != nil || kind != dashboardSSE || sseURL != want {
				t.Errorf("probeDashboard = %q, %q, %v; want %q on %q", kind, sseURL, err, dashboardSSE, want)
			}
		})
	}
}

// TestADashboardURLThatDoesNotJoinPrintsNoneOfIt: a Dispatch URL url.Parse refuses (an invalid
// escape in its path), with redacttest's credentials in its user information and its query, fails
// the dashboard probe in fixed words, with none of the URL (unparsed); http.NewRequest's error,
// which the probe answered before, quotes it whole (twin). A Dispatch URL that parses, whose
// joined URL, as url.URL.String writes it, url.Parse refuses (an IPv6 zone escape), is refused the
// same way (reparsed); its twin shows http.NewRequest's error for the joined URL holds the
// credentials. Captures loadtest.dashboard-url/<case>.
func TestADashboardURLThatDoesNotJoinPrintsNoneOfIt(t *testing.T) {
	unparsed, reparsed := joinRefusedURLs("/dispatch")
	joinRequireTwins(t, unparsed, reparsed, ssePath)
	for _, c := range []struct{ name, url, want string }{
		{"unparsed", unparsed, "the Dispatch URL is not an absolute URL with a host (it is not printed)"},
		{"reparsed", reparsed, "the Dispatch URL joined with the SSE feed's path does not parse (it is not printed)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := probeDashboard(context.Background(), c.url)
			if err == nil {
				t.Fatal("probeDashboard took a Dispatch URL that does not join")
			}
			redacttest.RequireNoPiece(t, "loadtest.dashboard-url/"+c.name, err.Error())
			if err.Error() != c.want {
				t.Errorf("probeDashboard = %q; want %q", err, c.want)
			}
		})
	}
}
