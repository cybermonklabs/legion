// Package busprojects is Dispatch's proof that the hubs and listeners a private project would
// reach keep it apart: every hub that serves private projects admits Dispatch afresh through its
// auth service, which it does only on a hub rendered with private-project support, and every
// listener replica that serves those hubs reads which projects are private afresh, in required
// privacy mode, from its expected source. A switch of a project to private runs the proof last
// before it commits, and the outbox holds private projects' events while the last proof failed.
//
// It writes nothing, on the bus or anywhere else, and reads no project: Dispatch's database is the
// one record of which projects are private, and every listener reads it at each decision, so no
// copy of it exists for the proof to keep.
package busprojects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// ErrNoBus is Prove's answer on a nil *HubAdmission: the deployment names no hub that serves
// private projects, so no project can be made private.
var ErrNoBus = errors.New("busprojects: no hub serves private projects (DISPATCH_PRIVATE_HUBS is empty), so no project can be made private")

// ErrNoListenerInventory is Prove's answer while the admission holds no listener workload to read:
// before SetListeners, and after one that gave none. With no listener read, nothing shows that a
// private project's frames reach only the sessions its rows admit.
var ErrNoListenerInventory = errors.New("busprojects: no listener inventory (DISPATCH_PRIVATE_LISTENERS), so no listener replica was read")

const (
	// proofPeriod is how often Run proves every hub and listener replica again.
	proofPeriod = 60 * time.Second
	// proofTimeout bounds one proof, whatever its caller's context allows. A hub whose auth service
	// does not answer refuses the connection at its authorization timeout, 2 s, and a dial gives up
	// at the bus's connect timeout, 5 s, so a proof that has not ended by then will not succeed; a
	// switch to private holds its transaction open while it waits.
	proofTimeout = 10 * time.Second
)

// HubAdmission proves, at each Prove and on Run's period, that every hub of DISPATCH_PRIVATE_HUBS
// admits a new connection of Dispatch's and that every listener replica of
// DISPATCH_PRIVATE_LISTENERS answers a fresh privacy read; Ready and Report give the last proof's
// result. A nil *HubAdmission is a deployment with no private hub: Prove answers ErrNoBus, Ready
// true, Run returns at once and Report is empty.
type HubAdmission struct {
	hubs   []hub
	period time.Duration
	lookup func(ctx context.Context, host string) ([]string, error)
	now    func() time.Time
	// wake asks Run for a proof now: a connection event of a hub's client, or a new inventory.
	wake chan struct{}
	// proofs numbers each proof as it starts, so a proof's result is kept only when no proof that
	// started after it has kept one (keep).
	proofs atomic.Uint64

	mu        sync.Mutex
	listeners []ListenerTarget
	client    *http.Client
	// kept is the number of the proof whose result ready and report hold; SetListeners raises it
	// to the last number given, so no proof started under an older inventory is kept.
	kept   uint64
	ready  bool
	report Report
	// logged is the outcome Run last logged, and loggedOnce whether it has logged one.
	logged     string
	loggedOnce bool
}

// hub is one hub of DISPATCH_PRIVATE_HUBS: the client whose credential and servers each proof
// dials with, and its servers as the report and the errors name them.
type hub struct {
	client *bus.Client
	url    string
}

// NewHubAdmission is the admission over conns, one client per hub of DISPATCH_PRIVATE_HUBS,
// Dispatch's own hub first, each connecting as Dispatch with its hub-admission token. With no
// client it answers nil, the deployment with no private hub. Each proof dials a new connection
// from a client's servers and credential (bus.Client.ProveAdmission) and never tests the client's
// own connection: the auth service decides a connection once, when it connects, so a connection
// it admitted earlier proves nothing about now. A client's connection events wake Run for a
// proof. Each client should be built WithInboxPrefix, naming its grant's own reply inboxes, and
// WithoutStreamSubjects, since the hub-admission grant lets it subscribe nothing else and publish
// nothing but its ping (busprojectstest.ProofClient builds one so).
func NewHubAdmission(conns []*bus.Client) *HubAdmission {
	if len(conns) == 0 {
		return nil
	}
	a := &HubAdmission{
		period: proofPeriod,
		lookup: net.DefaultResolver.LookupHost,
		now:    time.Now,
		wake:   make(chan struct{}, 1),
	}
	for _, c := range conns {
		a.hubs = append(a.hubs, hub{client: c, url: c.Servers()})
	}
	a.report = a.unproven(nil)
	return a
}

// SetListeners gives the admission the listener workloads that serve its hubs
// (DISPATCH_PRIVATE_LISTENERS) and the client that reads their replicas, which presents Dispatch's
// own credential; nil is http.DefaultClient. Until it is called, and after a call that gives no
// workload, every proof fails with ErrNoListenerInventory. A new inventory sets Ready false and the
// report to unknown until a proof under it ends, and wakes Run for that proof. It needs a non-nil
// admission: a deployment with no private hub has no listener to prove, and its caller sets none.
func (a *HubAdmission) SetListeners(inv []ListenerTarget, client *http.Client) {
	if client == nil {
		client = http.DefaultClient
	}
	targets := append([]ListenerTarget(nil), inv...)
	a.mu.Lock()
	a.listeners, a.client = targets, client
	a.kept = max(a.kept, a.proofs.Load())
	a.ready = false
	a.report = a.unproven(targets)
	a.mu.Unlock()
	a.signal()
}

// Ready reports whether the last proof succeeded: false before the first proof ends, while the
// last one failed for any hub or listener replica, and from SetListeners until a proof under the
// new inventory ends. A nil admission is ready, since no project can be private there. The outbox
// reads it (outbox.Deps.PrivateHubsReady) and holds private projects' events while it is false.
func (a *HubAdmission) Ready() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ready
}

// Prove dials a new connection on every hub, each a fresh decision of its auth service, and reads
// every listener replica of the inventory afresh, all at once, within proofTimeout. It answers nil
// only when every hub admitted its new connection and every replica answered in required privacy
// mode, from the source its workload expects, that its read answered; otherwise an error naming
// each hub and replica that did not, hubs first, each in the order the deployment names them. It
// answers ErrNoBus on a nil admission and an error that is ErrNoListenerInventory while the
// admission holds no listener workload, after proving the hubs all the same, so the report shows
// them. Either way the result becomes Ready's and Report's, unless a proof that started later has
// already ended.
func (a *HubAdmission) Prove(ctx context.Context) error {
	if a == nil {
		return ErrNoBus
	}
	number := a.proofs.Add(1)
	ctx, cancel := context.WithTimeout(ctx, proofTimeout)
	defer cancel()
	a.mu.Lock()
	targets, client := a.listeners, a.client
	a.mu.Unlock()

	hubs := make([]HubReport, len(a.hubs))
	hubErrs := make([]error, len(a.hubs))
	var replicas []ReplicaReport
	var replicaErrs []error
	var wg sync.WaitGroup
	for i, h := range a.hubs {
		wg.Go(func() { hubs[i], hubErrs[i] = proveHub(ctx, h) })
	}
	if len(targets) > 0 {
		wg.Go(func() { replicas, replicaErrs = readListeners(ctx, a.lookup, client, targets) })
	}
	wg.Wait()

	var errs []error
	if len(targets) == 0 {
		errs = append(errs, ErrNoListenerInventory)
		replicas = []ReplicaReport{}
	}
	for _, err := range append(hubErrs, replicaErrs...) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	a.keep(number, Report{ProvedAt: a.now().UTC(), Hubs: hubs, Listeners: replicas}, err == nil)
	return err
}

// proveHub is one hub's part of a proof: a new connection, admitted or not.
func proveHub(ctx context.Context, h hub) (HubReport, error) {
	if err := h.client.ProveAdmission(ctx); err != nil {
		return HubReport{URL: h.url, Admission: AdmissionRefused, Error: err.Error()},
			fmt.Errorf("busprojects: hub %s did not admit a new proof connection: %w", h.url, err)
	}
	return HubReport{URL: h.url, Admission: AdmissionAdmitted}, nil
}

// keep makes a proof's result Ready's and Report's, unless a proof numbered after it, or a
// SetListeners after it started, has already set them.
func (a *HubAdmission) keep(number uint64, report Report, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if number <= a.kept {
		return
	}
	a.kept, a.ready, a.report = number, ok, report
}

// Run proves at once, then again every proofPeriod, at each connection event of a hub's client (a
// disconnect, a reconnect or a replaced connection), and after each SetListeners, until ctx is
// done. It logs the outcome when it changes: a WARN naming every hub and replica that failed,
// and an INFO when every one passes again. A nil admission returns at once.
func (a *HubAdmission) Run(ctx context.Context) {
	if a == nil {
		return
	}
	for _, h := range a.hubs {
		go a.follow(ctx, h.client)
	}
	ticker := time.NewTicker(a.period)
	defer ticker.Stop()
	for {
		err := a.Prove(ctx)
		if ctx.Err() != nil {
			return
		}
		a.logOutcome(err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.wake:
		}
	}
}

// follow wakes Run at each connection event of c until ctx is done.
func (a *HubAdmission) follow(ctx context.Context, c *bus.Client) {
	for {
		changed := c.ConnectionChanged()
		select {
		case <-ctx.Done():
			return
		case <-changed:
			a.signal()
		}
	}
}

// signal asks Run for a proof now; one asked for and not yet taken covers any number of asks.
func (a *HubAdmission) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// logOutcome logs Run's proof when its outcome differs from the last one Run logged.
func (a *HubAdmission) logOutcome(err error) {
	outcome := ""
	if err != nil {
		outcome = err.Error()
	}
	a.mu.Lock()
	changed := !a.loggedOnce || outcome != a.logged
	a.logged, a.loggedOnce = outcome, true
	a.mu.Unlock()
	if !changed {
		return
	}
	if err != nil {
		slog.Warn("busprojects: the private-hub admission proof failed: every switch to private is refused and private projects' events are held until it passes", slog.String("error", outcome))
		return
	}
	slog.Info("busprojects: the private-hub admission proof passed: every private hub admits Dispatch and every listener replica read the private projects afresh")
}
