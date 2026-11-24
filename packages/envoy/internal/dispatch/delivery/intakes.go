package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The names StartIntakes asks connect for, one per intake, so the NATS server's connection
// listings tell each intake's connection from Dispatch's publisher and from every listener.
const (
	intakeConnectionName    = "dispatch-intake"
	jobIntakeConnectionName = "dispatch-job-intake"
)

// IntakeLeases is the lease-pool reservation of each intake's callers (ReserveIntakeLeases),
// which StartIntakes hands the intake it was taken for.
type IntakeLeases struct {
	events intakeLeases
	jobs   intakeLeases
}

// ReserveIntakeLeases reserves the connections of the lease pool the intakes can be inside at the
// same moment, three per durable: the consumer lease a take-over of that durable runs under
// (consumerLease), the statements of its hold the intake's own goroutine runs (duty.TryHold,
// SetToken, Release, one at a time, the SetToken inside the take-over and so beside the
// connection its lease is held on), and the hold's renewal goroutine (duty.Hold's renew). The
// three are reservations, not pools: every caller takes its connections from the one lease pool,
// and the reservation is how the server counts the callers against the pool's size.
//
// The server reserves them while it wires itself, whether or not this configuration starts the
// intakes - without NATS or the GitHub App key they are idle - so that every configuration
// reserves the same callers and the count the server checks against the pool's size is the one
// the pool is sized for.
func ReserveIntakeLeases(callers *store.LeaseCallers) (IntakeLeases, error) {
	events, err := reserveDurableLeases(callers, bus.DispatchDeliveryDurable)
	if err != nil {
		return IntakeLeases{}, err
	}
	jobs, err := reserveDurableLeases(callers, bus.DispatchJobDurable)
	if err != nil {
		return IntakeLeases{}, err
	}
	return IntakeLeases{events: events, jobs: jobs}, nil
}

// reserveDurableLeases reserves one durable's three callers (store.LeaseCallers.ForAll) and
// returns the lease pool they all take their connections from.
func reserveDurableLeases(callers *store.LeaseCallers, durable string) (intakeLeases, error) {
	leases, err := callers.ForAll(consumerLease(durable), intakeHoldName(durable), intakeHoldName(durable)+" renewal")
	if err != nil {
		return nil, err
	}
	return leases, nil
}

// StartIntakes runs Intake and JobIntake until ctx is cancelled, each on a bus client of its own
// that connect opens under the name it is given (dispatch-intake, dispatch-job-intake). It connects
// both before it returns, and returns the error of a connect that fails with neither intake
// started. Without the GitHub App neither intake reads anything, so neither connects. broker is the
// server's event broker, through which Intake appends issue.deployed (RecordDeployedEvents).
// pushes is what Intake hands a push of a branch to (PushImports): in the server, the
// architecture importer, which imports nothing without the App either. nil hands pushes nowhere.
//
// leases is each intake's reservation of the lease pool (ReserveIntakeLeases), taken by the
// caller while it wires itself rather than here, so that a configuration these intakes are idle
// in reserves the same callers as one they run in.
//
// Both intakes route by one SettingsCache, which reads delivery_settings at start and again each
// time settingsWake is signalled -- in the server, the shared listener's wake on SettingsChannel --
// so no message and no timer reads the row. A nil settingsWake reads it once.
//
// attributor is the Attributor of the rename pairs the process read at boot. Intake attributes
// each pull request and keys its head row with it.
//
// The returned stop, called once ctx is cancelled, waits for both intakes' Runs and the cache's,
// closes both clients, then waits for a hand-back finalizer each Run left behind. A Run can return
// at its stop deadline with a hand-back still waiting for NATS, or written and not confirmed; close
// settles that hand-back, which reaches the server before the close or is dropped with the socket
// the close resets (bus.Client.MarkUnconfirmed), so none of its bytes is sent after the release, and
// its finalizer releases the hold through the still-open pool before stop returns. A message in its
// handler finishes and is acknowledged before the Run returns; messages behind it are handed back
// while the hold
// covers them, and the next process takes the durable over at once rather than at the
// hold's lapse. Each Run's stop has one deadline, intakeStopBudget after ctx ends (stopContext), which a take-over under way at that
// moment spends rather than extends: the handlers still running then are given up and their
// messages handed back too. Every handler of Intake's has returned by the time stop does, and each
// finalizer released its hold, so a caller that closes the pool after stop returns, as cmd/dispatch's
// shutdown does, closes it under neither's writes. A second call returns once the first has.
//
// The intakes cannot share a client: a bus.Client holds one JetStream subscription, and refuses the
// second intake's subscribe with bus.ErrSubscriptionHeld.
//
// Each intake delivers from its durable only while it holds that durable's hold
// (intakeHoldName), and changes the durable only under that durable's own consumer lease
// (consumerLease), both taken and the hold renewed on the pool's lease connections, so that of
// every envoy-dispatch process one delivers from each durable at a time -- and so that neither
// intake's take-over can block the other's, which one lease over both durables did (the
// measurement is on consumerLease).
func StartIntakes(ctx context.Context, connect func(name string) (*bus.Client, error), pool *store.Pool, leases IntakeLeases, github *githubapp.Client, broker *events.Broker, settingsWake <-chan struct{}, pushes PushImports, attributor *Attributor) (stop func(), err error) {
	if github == nil {
		slog.Info("dispatch delivery: no GitHub App key — the intakes are idle")
		return func() {}, nil
	}
	intakeClient, err := connect(intakeConnectionName)
	if err != nil {
		return nil, fmt.Errorf("connect the intake: %w", err)
	}
	jobClient, err := connect(jobIntakeConnectionName)
	if err != nil {
		intakeClient.Close()
		return nil, fmt.Errorf("connect the job intake: %w", err)
	}
	settings := newSettingsCache(pool, settingsWake)
	intake := newIntake(intakeClient, pool, leases.events, github, broker, settings, attributor)
	intake.pushes = pushes
	job := newJobIntake(jobClient, pool, leases.jobs, github, realJobClock{}, settings)
	var running sync.WaitGroup
	running.Go(func() { settings.Run(ctx) })
	running.Go(func() { intake.Run(ctx) })
	running.Go(func() { job.Run(ctx) })
	return sync.OnceFunc(func() {
		stopIntakes(&running, &intake.heldDurable, &job.heldDurable)
	}), nil
}

// stopIntakes waits for their Runs, closes their NATS clients through each durable, then waits for
// any hand-back finalizer each Run left behind. A closed client makes such a finalizer settle: its
// close reset each socket that carried an unconfirmed hand-back (bus.Client.MarkUnconfirmed), so the
// finalizer releases its hold through the still-open database pool before this stop returns, with
// none of that hand-back's bytes left to send (StartIntakes).
func stopIntakes(running *sync.WaitGroup, durables ...*heldDurable) {
	running.Wait()
	for _, durable := range durables {
		durable.nats.Close()
	}
	for _, durable := range durables {
		durable.waitFinalizers()
	}
}
