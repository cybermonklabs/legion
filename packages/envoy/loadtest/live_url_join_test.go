package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// TestTheDashboardProbeDialsTheLiveSocketJoinedOntoTheDispatchURL: the Dispatch URL can carry a
// path and a query. The dashboard probe dials the live socket at that path with /ws/live joined
// onto it, the URL's query kept: http://host/prefix?key=... dials ws://host/prefix/ws/live?key=...
// (query), and so does http://host/prefix/?key=... (query-trailing-slash), where /ws/live appended
// to the URL's text sat inside the query, Dispatch was asked /prefix and the probe found no live
// socket. A URL with no query dials where it did (no-query). A stand-in that upgrades the live
// socket alone records the path and the query each request reaches it with; the probe chooses the
// live socket and answers the URL it dialled.
func TestTheDashboardProbeDialsTheLiveSocketJoinedOntoTheDispatchURL(t *testing.T) {
	for _, c := range joinURLCases {
		t.Run(c.name, func(t *testing.T) {
			base, asked := joinAskedStand(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/prefix/ws/live" {
					http.NotFound(w, r)
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err == nil {
					_ = conn.Close()
				}
			})
			kind, liveURL, err := probeDashboard(context.Background(), base+c.suffix)
			got := asked()
			if len(got) == 0 || got[0].Path != "/prefix/ws/live" || got[0].RawQuery != c.query {
				t.Errorf("a Dispatch URL ending %q was asked %v; want the live socket dialled first at path /prefix/ws/live with query %q", c.suffix, got, c.query)
			}
			want := "ws" + strings.TrimPrefix(base, "http") + "/prefix/ws/live"
			if c.query != "" {
				want += "?" + c.query
			}
			if err != nil || kind != dashboardLive || liveURL != want {
				t.Errorf("probeDashboard = %q, %q, %v; want %q on %q", kind, liveURL, err, dashboardLive, want)
			}
		})
	}
}
