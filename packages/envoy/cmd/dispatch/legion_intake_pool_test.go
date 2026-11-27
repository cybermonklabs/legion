package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// legionSettingsListStatement is the statement the Legion state intake reads every project's
// setting with once it has taken its durable over (legion.ListSettings, from Intake.SettingsChanged
// on the take-over's runner), as pg_stat_activity shows it: its `order by project` no other reader
// of legion_settings sends.
const legionSettingsListStatement = "from legion_settings order by project"

// legionIntakePoolWindow is how long the wait below gives that statement: the intake connects to
// NATS in the background, takes its durable's hold and takes the durable over within seconds of
// the process starting its loops, and a take-over that found the hold held by nobody yet tries
// again at the hold's next poll.
const legionIntakePoolWindow = time.Minute

// The Legion state intake is background work: a held durable that records each daemon's state
// events and reads a daemon whole where the events cannot carry the state (legion.Intake). Its
// statements belong on the background pool, where the delivery intakes' and every other loop's
// are and which carries the 70 s idle-in-transaction bound, and take no connection of the request
// pool, whose connections admission hands out to requests. Main started it on database.Pool, the
// request pool.
//
// This reads the running process, as the webhook sweep's row does: legion_settings is held under
// an access exclusive lock from before the process starts, so the intake's read of the settings
// at its take-over queues behind that lock, and pg_stat_activity names the pool of the
// connection it waits on. Pointed back at database.Pool, the same wait answers dispatch-requests.
func TestTheLegionStateIntakeTakesNoRequestPoolConnection(t *testing.T) {
	database := storetest.Open(t)
	_, natsURL := testnats.Start(t)
	gate := holdTableExclusively(t, database, "legion_settings")
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{NATSURL: natsURL})
	application := applicationBehind(t, database, gate, process, legionSettingsListStatement, legionIntakePoolWindow)
	// Released as soon as the name is read, so the take-over runs on and the process stops in order.
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release legion_settings: %v", err)
	}
	if want := "dispatch-" + store.BackgroundPool; application != want {
		t.Fatalf("the Legion state intake read its settings on a %s connection, want %s: an intake's statements belong on the background pool, not on the connections requests are served from", application, want)
	}
}
