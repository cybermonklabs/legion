package testnats

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A member Isolate takes off the cluster's network holds no route, and neither member left holds
// one to it, so it shares nothing with them. Its client port still answers, as a replica cut off
// serves its own clients, and each member's client, made before the cut, still reads that member
// after it. Rejoin gives every member its routes to both others again. Control first: before the
// cut every member holds routes to both others, so a count of none afterwards is the cut.
func TestAnIsolatedMemberHoldsNoRouteAndRejoins(t *testing.T) {
	c := StartCluster(t, Image)
	routesOf := func(i int) []string {
		routes, err := c.Routes(i)
		if err != nil {
			t.Fatalf("member %s's routes: %v", c.Name(i), err)
		}
		return routes
	}
	reaches := func(i, j int) bool {
		for _, route := range routesOf(i) {
			if strings.HasPrefix(route, c.Name(j)+"@") {
				return true
			}
		}
		return false
	}
	connected := func() bool {
		for i := range clusterSize {
			for j := range clusterSize {
				if i != j && !reaches(i, j) {
					return false
				}
			}
		}
		return true
	}
	testwait.Eventually(t, "every member routed to both others", connected)
	var clients [clusterSize]*nats.Conn
	for i := range clients {
		clients[i], _ = c.Connect(t, i)
	}

	c.Isolate(t, 0)
	if len(routesOf(0)) != 0 || reaches(1, 0) || reaches(2, 0) || !reaches(1, 2) {
		t.Fatalf("after Isolate, n1 routes to %q, n2 to %q, n3 to %q; want n1 cut off and n2 and n3 still routed", routesOf(0), routesOf(1), routesOf(2))
	}
	for i, client := range clients {
		if err := client.FlushTimeout(5 * time.Second); err != nil {
			t.Fatalf("member %s's client after the cut: %v", c.Name(i), err)
		}
		if got := client.ConnectedServerName(); got != c.Name(i) {
			t.Fatalf("the client of member %s reads %s after the cut", c.Name(i), got)
		}
	}
	c.Rejoin(t, 0)
	testwait.Eventually(t, "every member routed to both others again", connected)
	c.AwaitReady(t)
}
