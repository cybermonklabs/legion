package listenerwire_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/wirecheck"
)

// The listener's production Go clients encode every request body and decode every answer with a
// type this package declares, and copy none, whole or in part: the broker's wake, Dispatch's
// client, and the daemon's publisher and controller probe, which is another module and so is read
// by its own directory. A type with the same fields, a map and an anonymous struct are each a
// finding. The load-test harness and the refill rig are not read (the package comment says why).
func TestListenerClientsUseListenerwire(t *testing.T) {
	const wire = "github.com/sjawhar/envoy/listenerwire"
	for _, clients := range []struct {
		dir  string
		pkgs []string
	}{
		{dir: "..", pkgs: []string{"./internal/broker/wake", "./internal/dispatch/envoy"}},
		{dir: "../../daemon", pkgs: []string{"./internal/notify", "./internal/controller"}},
	} {
		for _, finding := range wirecheck.Check(t, clients.dir, clients.pkgs, wire, nil) {
			t.Errorf("%s: %s", clients.dir, finding)
		}
	}
}
