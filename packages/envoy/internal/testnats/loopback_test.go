package testnats

import (
	"net"
	"net/url"
	"runtime"
	"testing"

	"github.com/sjawhar/envoy/internal/testport"
)

// Every address a fresh cluster's members listen on, and every address its URLs name, is on this
// binary's own loopback address (testport.Loopback) and not on 127.0.0.1, where a client another
// test binary left reconnecting dials the ports its binary let go. A member's real client, route
// and monitoring listeners count as well as its relays, and so does a gate in front of a server: a
// client of another binary that dials 127.0.0.1 and the port a member or a gate listens on would
// reach the member without any relay.
func TestAFreshClusterListensOnlyOnThisBinarysLoopback(t *testing.T) {
	if runtime.GOOS == "linux" && testport.Loopback == "127.0.0.1" {
		t.Fatalf("testport.Loopback = %s on Linux, which serves all of 127.0.0.0/8: want an address of this binary's own", testport.Loopback)
	}
	cluster := StartCluster(t, 2)
	for i, member := range cluster.Servers {
		client, err := url.Parse(member.URL())
		if err != nil {
			t.Fatalf("parse member %s's URL: %v", cluster.Name(i), err)
		}
		real, err := member.address()
		if err != nil {
			t.Fatalf("member %s's client address: %v", cluster.Name(i), err)
		}
		route, err := member.clusterAddress()
		if err != nil {
			t.Fatalf("member %s's route address: %v", cluster.Name(i), err)
		}
		monitor, err := url.Parse(member.MonitorURL())
		if err != nil {
			t.Fatalf("parse member %s's monitoring URL: %v", cluster.Name(i), err)
		}
		for what, address := range map[string]string{"relay": client.Host, "client listener": real, "route listener": route, "monitoring listener": monitor.Host} {
			host, _, err := net.SplitHostPort(address)
			if err != nil || host != testport.Loopback {
				t.Errorf("member %s's %s is at %s, want a port at %s", cluster.Name(i), what, address, testport.Loopback)
			}
		}
	}
	if runtime.GOOS != "linux" {
		return
	}
	gate, err := url.Parse(StartGate(t, cluster.Servers[0]).URL())
	if err != nil {
		t.Fatalf("parse the gate's URL: %v", err)
	}
	if gate.Hostname() != testport.Loopback {
		t.Errorf("the gate is at %s, want a port at %s", gate.Host, testport.Loopback)
	}
}
