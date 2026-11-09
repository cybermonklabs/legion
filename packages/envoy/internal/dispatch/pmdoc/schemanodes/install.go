package schemanodes

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// RetryInterval is how soon an Installer reads the store again after a read that failed. Only a
// failure arms it: an installer whose last read succeeded reads again on its wake alone. It is
// delivery.SettingsCache's interval, the pattern this loop follows.
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
	// Store is read for the schema's nodes.
	Store graphstore.Queryer
	// Wake is signalled when a supertag write announces itself. An installer with no wake
	// reads the store once.
	Wake <-chan struct{}
	// Retry is how soon a failed read is made again; zero is RetryInterval.
	Retry time.Duration
}

// RegisterWake registers the schema channel on the process's shared listener and answers the
// wake an Installer loops on. It is the one registration the server makes for the schema, and
// it is a Wake and not a Handle for the reason Installer states. It panics once the hub's Run
// has started, as every registration of that hub does.
func RegisterWake(hub *pglisten.Hub) <-chan struct{} {
	return hub.Wake(Channel)
}

// InstallFromStore reads the schema from the store and installs it. The server calls it at boot,
// beside the migration run, and a failure there fails the boot: a process serving on a schema
// that is not the store's would validate writes against types the deployment has moved on from.
func InstallFromStore(ctx context.Context, q graphstore.Queryer) error {
	value, err := Read(ctx, q)
	if err != nil {
		return err
	}
	return pmdoc.Install(value)
}

// Run installs the store's schema at once, then each time the wake is signalled, until ctx ends.
// A read that fails is made again after the retry interval unless a wake comes first.
func (i *Installer) Run(ctx context.Context) {
	for {
		var retry <-chan time.Time
		var timer *time.Timer
		if !i.reload(ctx) {
			timer = time.NewTimer(i.retryInterval())
			retry = timer.C
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

// reload reads the store and installs what it read. It reports false only for a read that
// failed, which Run makes again: a value the install refuses is an answer, not a failure to
// read, and the process keeps serving the schema it has until the store holds one it can
// install - a schema above this build's version among them, which also makes the process report
// itself not ready (pmdoc.NotReadyReason).
func (i *Installer) reload(ctx context.Context) bool {
	value, err := Read(ctx, i.Store)
	if err != nil {
		slog.Warn("dispatch: read the block schema from the store", "error", err)
		return false
	}
	if err := pmdoc.Install(value); err != nil {
		slog.Error("dispatch: the store's block schema was not installed; this process keeps the schema it has",
			"error", err, "store_version", value.Version, "built_version", pmdoc.BuiltSchemaVersion())
	}
	return true
}

func (i *Installer) retryInterval() time.Duration {
	if i.Retry > 0 {
		return i.Retry
	}
	return RetryInterval
}
