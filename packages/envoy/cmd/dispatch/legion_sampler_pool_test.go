package main

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// legionSettingsListStatement is the first statement of every pass of the Legion sampler, as
// pg_stat_activity shows it: the read of every project's setting (legion.ListSettings), whose
// `order by project` no other reader of legion_settings sends.
const legionSettingsListStatement = "from legion_settings order by project"

// The Legion sampler is background work: a timer under the legion-state lease, reading every
// configured daemon once a minute from its first tick at start (legion.RunSampler), and recording
// what moved in write transactions (legion.RecordRead, legion.PruneReads). Its statements belong
// on the background pool, where every other loop's are and which carries the 70 s
// idle-in-transaction bound, and take no connection of the request pool, whose connections
// admission hands out to requests. Main started it on database.Pool, the request pool.
//
// This reads the running process, as the webhook sweep's row does: legion_settings is held under
// an access exclusive lock from before the process starts, so the sampler's first pass queues its
// read of the settings behind that lock, and pg_stat_activity names the pool of the connection it
// waits on. Pointed back at database.Pool, the same wait answers dispatch-requests.
func TestTheLegionSamplerTakesNoRequestPoolConnection(t *testing.T) {
	database := storetest.Open(t)
	gate := holdTableExclusively(t, database, "legion_settings")
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{})
	// The sampler's first tick is at start, so its read comes within seconds of the loops
	// starting; one SampleInterval also covers the tick after a first one whose lease was late.
	application := applicationBehind(t, database, gate, process, legionSettingsListStatement, legion.SampleInterval)
	// Released as soon as the name is read, so the pass runs on and the process stops in order.
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release legion_settings: %v", err)
	}
	if want := "dispatch-" + store.BackgroundPool; application != want {
		t.Fatalf("the Legion sampler read its settings on a %s connection, want %s: a loop's statements belong on the background pool, not on the connections requests are served from", application, want)
	}
}
