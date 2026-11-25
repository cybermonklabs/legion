package schemanodes

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// RetryInterval is how soon an Installer reads the store again after a read that failed or a
// value the install refused. Only those arm it: an installer whose last read it installed reads
// again on its wake alone. It is delivery.SettingsCache's interval, the pattern this loop follows.
const RetryInterval = 30 * time.Second

// Installer reinstalls the block schema from the store: at Run's first pass, and again each
// time its wake is signalled. In the server the wake is the shared listener's (pglisten.Hub) on
// Channel, which the hub also signals after every LISTEN it makes, so a change made while the
// listener had no connection is read once it has one again and this loop needs no timer of its
// own for an outage.
//
// It runs on a goroutine of its own and never inside a Hub.Handle handler: a handler runs on
// Run's goroutine and must not block, and reading the store blocks. RegisterWake is the whole of
// what the server registers on the hub.
type Installer struct {
	// Store is read for the schema's nodes, in one transaction of its own (Read).
	Store Beginner
	// Wake is signalled when a supertag write announces itself. An installer with no wake
	// reads the store once.
	Wake <-chan struct{}
	// Retry is how soon a failed read, or a value the install refused, is read again; zero is
	// RetryInterval.
	Retry time.Duration
	// Clock is what the retry timer runs on: clock.Real() when nil, as in the server, and a
	// test's clocktest.Fake, which fires the retry only when the test moves it.
	Clock clock.Clock
	// OnSwitch runs after a read that moved the schema this process serves: the installed
	// value changed, its version or any element of it. The server refreshes its row of the
	// server process registry there and closes its document sockets
	// (docs.Service.CloseDocumentSockets), because a socket's admission was decided against the
	// schema the process served as it connected, and the browser connects again on that close,
	// where admission decides against the schema installed now. It runs on this loop's
	// goroutine, so a hook that blocks delays the next read; a reload that installed the same
	// schema runs it not at all, which is what leaves an ordinary reinstall's editors connected,
	// and so does a store a newer build moved on that this build installs the same as before
	// (pmdoc.Install). A nil hook is nothing.
	OnSwitch func()
}

// RegisterWake registers the schema channel on the process's shared listener and answers the
// wake an Installer loops on. It is the one registration the server makes for the schema, and
// it is a Wake and not a Handle for the reason Installer states. It panics once the hub's Run
// has started, as every registration of that hub does.
func RegisterWake(hub *pglisten.Hub) <-chan struct{} {
	return hub.Wake(Channel)
}

// InstallFromStore reads the schema from the store and installs it. The server calls it at boot,
// beside the migration run, and so does every subcommand that reads or writes documents after
// its own migration run; a failure there fails the boot or the command: a process serving on a
// schema that is not the store's would validate writes against types the deployment has moved
// on from. A store a newer build migrated is no failure: this build installs it at its own
// version (pmdoc.Install).
func InstallFromStore(ctx context.Context, db Beginner) error {
	value, err := Read(ctx, db)
	if err != nil {
		return err
	}
	return pmdoc.Install(value)
}

// Run installs the store's schema at once, then each time the wake is signalled, until ctx ends.
// A read that fails, and a value the install refuses, is read again after the retry interval
// unless a wake comes first.
func (i *Installer) Run(ctx context.Context) {
	for {
		var retry <-chan time.Time
		var timer clock.Timer
		if !i.reload(ctx) {
			timer = i.clock().NewTimer(i.retryInterval())
			retry = timer.C()
		}
		select {
		case <-ctx.Done():
		case <-i.Wake:
		case <-retry:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// reload reads the store and installs what it read. It reports false for a read that failed and
// for a value the install refused, each of which Run reads again after the retry interval: the
// process keeps serving the schema it has meanwhile.
//
// What the process serves is read before the install and again after it, and OnSwitch runs when
// the two differ. Both are read here and not inside pmdoc, so the hook runs once per reload and
// the install stays what it is - one pointer stored.
func (i *Installer) reload(ctx context.Context) bool {
	value, err := Read(ctx, i.Store)
	if err != nil {
		slog.Warn("dispatch: read the block schema from the store", "error", err)
		return false
	}
	before := pmdoc.SchemaJSON()
	if err := pmdoc.Install(value); err != nil {
		slog.Error("dispatch: the store's block schema was not installed; this process keeps the schema it has and reads the store again",
			"error", err, "store_version", value.Version, "built_version", pmdoc.BuiltSchemaVersion(), "retry_in", i.retryInterval())
		return false
	}
	if i.OnSwitch != nil && !bytes.Equal(pmdoc.SchemaJSON(), before) {
		i.OnSwitch()
	}
	return true
}

func (i *Installer) retryInterval() time.Duration {
	if i.Retry > 0 {
		return i.Retry
	}
	return RetryInterval
}

func (i *Installer) clock() clock.Clock {
	if i.Clock != nil {
		return i.Clock
	}
	return clock.Real()
}
