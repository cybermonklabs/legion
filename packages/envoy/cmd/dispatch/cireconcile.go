package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logging"
)

// reviewerAppID is DISPATCH_REVIEWER_APP_ID as a reconcile pass takes it: the text of the number
// GitHub's own check run objects carry under app.id, which the pass compares a verdict check's
// App against (contracts.ReviewerVerdictDecision). Unset it is empty, and the pass then records
// no `tester` or `architect` check run at all. Anything but a positive decimal number refuses
// startup naming the variable, as DISPATCH_APP_ID's own reader does (auth.LoadAppFromEnv).
func reviewerAppID(env settingValues) (string, error) {
	raw := strings.TrimSpace(env.get("DISPATCH_REVIEWER_APP_ID"))
	if raw == "" {
		return "", nil
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return "", fmt.Errorf("DISPATCH_REVIEWER_APP_ID=%q (expected the review App's numeric ID)", raw)
	}
	// The number as GitHub spells it, so a value written with a leading zero still matches the
	// app.id of the checks the review App posted.
	return strconv.FormatUint(id, 10), nil
}

// registeredInstance carries this process's row in the registry of live server processes from
// the registration to whatever runs before it. The CI reconcile loop is started before
// joinRegistry is called -- registration waits out a live peer whose signing keys do not match
// this process's, and the loops must not wait on that -- and each of its passes records its
// counts under that instance id (cireconcile.Reconciler.Instance), so the id reaches the pass
// through this rather than as a value nothing holds yet.
//
// A pass that runs before the registration reads "" and records no row, which is the honest
// answer: there is no process row for its record to be read against.
type registeredInstance struct{ id atomic.Pointer[string] }

// set records the id Register gave this process. It is called once, from the one goroutine that
// boots the server, after every loop that reads it has started.
func (r *registeredInstance) set(id string) { r.id.Store(&id) }

// get is that id, and "" until the process is registered.
func (r *registeredInstance) get() string {
	if id := r.id.Load(); id != nil {
		return *id
	}
	return ""
}

// ciReconciler builds the CI reconcile pass for the App whose key Dispatch holds, writing through
// the listener's own CI record bucket. It returns nil when the pass cannot run: no NATS to write
// its records to, or no App private key to sign the reads with. The bucket is bound by each pass
// that finds the client connected, not here, exactly as the webhook redelivery sweep's state is
// (webhookSweeper): the client connects in the background (serverBus), and a bus that is down
// at boot must not stop the boot; a pass before then reads GitHub nothing and says so. Its App
// client obeys the GitHub rate limits every process shares, in the database's github_rate_limits,
// since GitHub budgets the App rather than a process.
//
// interval is the period the loop runs the pass on, which is the pass's own bound: a pass too big
// for one interval ends on it, logs its line and leaves the next pass the cursor it reached, so
// the loop's lease is never held past the pass that is due next. It is also what the pass spreads
// its requests over: the gap it leaves between two of them is a share of this interval divided by
// the requests the work it knows of will take (cireconcile.RequestPace is the slowest it will
// read, cireconcile.FastestRequestPace the fastest).
//
// instance is the holder the registration fills in (registeredInstance), which each pass records
// its row under: the loop starts before the process is registered, so a pass has to ask for the
// id rather than be handed it.
func ciReconciler(natsClient *bus.Client, database *store.Store, app *auth.AppConfig, apiBase, reviewer string,
	interval time.Duration, instance *registeredInstance) (*cireconcile.Reconciler, error) {
	if natsClient == nil {
		return nil, nil
	}
	limits := ratelimit.New(database.Pool)
	github, err := githubapp.New(app, apiBase, githubapp.WithLimits(limits))
	if err != nil || github == nil {
		return nil, err
	}
	// cistore's writer logs its KV calls through this logger; the pass's own lines go to the
	// server's default logger, as every other loop's do.
	logger := logging.New("ci-reconcile")
	open := func() (cireconcile.Writer, error) {
		if !natsClient.Connected() {
			return nil, errors.New("NATS is not connected, so the CI record writer cannot be bound yet")
		}
		// The client's own connection and JetStream context, both read under its lock as
		// redeliver.OpenState's caller reads the context: the client replaces the connection and
		// the context together when it reconnects a closed connection, which reading either field
		// here would race. Every pass binds through this, so a pass after such a reconnect writes
		// through the connection the client now holds.
		writer, err := cistore.OpenWriter(natsClient.Connection(), natsClient.JS(), logger)
		if err != nil {
			return nil, err
		}
		return writer, nil
	}
	return &cireconcile.Reconciler{
		Pool:          database.Pool,
		GitHub:        github,
		Limits:        limits,
		Open:          open,
		ReviewerAppID: reviewer,
		Logger:        slog.Default(),
		Pace:          cireconcile.RequestPace,
		PassTimeout:   interval,
		Instance:      instance.get,
	}, nil
}
