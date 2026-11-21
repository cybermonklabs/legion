// Package busprojects is Dispatch's proof that the hubs and listeners a private project would
// reach keep it apart: every hub that serves private projects admits Dispatch afresh through its
// auth service, which it does only on a hub rendered with private-project support, and every
// listener replica that serves those hubs reads which projects are private afresh, in required
// privacy mode, from its expected source. A switch of a project to private is to run the proof
// last before it commits, and the outbox to hold private projects' events while the last proof
// failed (Ready); neither caller is wired at this head.
//
// A hub's entry names every server of the hub, each by its own name: a clustered hub's servers each
// by their pod's name in the headless Service (<pod>.<name>-headless.<namespace>.svc.cluster.local),
// never the Service that balances over them. A cluster's server tags are each server's own, so a
// proof through a Service would reach whichever server it picked and say nothing of the others; the
// proof refuses a server of a cluster that its URL does not name (bus.Client.ProveAdmission). It
// cannot see whether an entry names every server, since a clustered hub advertises none to its
// clients: that the list is whole is the render's to make true, which sets each hub's entry from
// the hub's replica count.
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
	"slices"
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
	kept       uint64
	ready      bool
	report     Report
	logged     string
	loggedOnce bool
}

type hub struct {
	client *bus.Client
	url    string
}

// NewHubAdmission is the admission over conns, one client per hub of DISPATCH_PRIVATE_HUBS,
// Dispatch's own hub first, each connecting as Dispatch with its hub-admission token. Each client's
// servers are every server of its hub, each named on its own: a clustered hub's per-pod names
// (<pod>.<name>-headless.<namespace>.svc.cluster.local), all of them, never its Service; the proof
// refuses a server of a cluster that its URL does not name, and cannot tell a list that leaves a
// server out (the package's doc). With no client it answers nil, the deployment with no private
// hub. Each proof dials a new connection to each of a client's servers with the client's
// credential (bus.Client.ProveAdmission) and never tests the client's own connection: the auth
// service decides a connection once, when it connects, so a connection it admitted earlier proves
// nothing about now. A client's connection events wake Run for a proof. Each client should be
// built WithInboxPrefix, naming its grant's own reply inboxes, and WithoutStreamSubjects, since
// the hub-admission grant lets it subscribe nothing else and publish nothing but its ping
// (busprojectstest.ProofClient builds one so).
func NewHubAdmission(conns []*bus.Client) *HubAdmission {
	if len(conns) == 0 {
		return nil
	}
	a := &HubAdmission{
		period: proofPeriod,
		lookup: net.DefaultResolver.LookupHost,
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
// own credential; nil is http.DefaultClient. The proof reads every replica but a loopback one over
// https, so the credential and the answer never cross a network in the clear; the client's
// transport is the one that verifies the replica's certificate. The proof reads through a copy of
// the client that follows no redirect, so a replica's 3xx is its answer, and fails it: what the
// address a redirect names answers is no read of the replica, and a transport that adds the
// credential would send it there too.
// Until it is called, and after a call that gives no workload, every proof fails with
// ErrNoListenerInventory. A new inventory sets Ready false and the report to unknown until a
// proof under it ends, and wakes Run for that proof. It needs a non-nil admission: a deployment
// with no private hub has no listener to prove, and its caller sets none.
func (a *HubAdmission) SetListeners(inv []ListenerTarget, client *http.Client) {
	if client == nil {
		client = http.DefaultClient
	}
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	targets := append([]ListenerTarget(nil), inv...)
	a.mu.Lock()
	a.listeners, a.client = targets, &noRedirects
	a.kept = max(a.kept, a.proofs.Load())
	a.ready = false
	a.report = a.unproven(targets)
	a.mu.Unlock()
	a.signal()
}

// Ready reports whether the last proof succeeded: false before the first proof ends, while the
// last one failed for any hub or listener replica, and from SetListeners until a proof under the
// new inventory ends. A nil admission is ready, as the approved plan's contract has it (K7): the
// deployment names no private hub, and a hub carries a private project only once it is rendered
// private (R9), so false would hold only private projects' events, of which such a deployment has
// none, and a switch to private is refused there (ErrNoBus). That rests on the deployment, since
// Dispatch's database, not DISPATCH_PRIVATE_HUBS, is the record of which projects are private: a
// project can still be private when the setting is emptied, and Ready true then lets its events go
// with no proof. They stay apart only while its hub is still rendered private, so a deployment must
// not stop rendering a hub private while Dispatch's database holds a private project. The outbox
// is to hold private projects' events while Ready is false; at this head nothing reads it, since
// outbox.Deps has no field its caller sets from it.
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
// already ended, or ctx ended before the proof did: a proof its caller cancelled answers ctx's
// error and is not kept, so Ready and Report keep the last proof that ran to its end.
func (a *HubAdmission) Prove(ctx context.Context) error {
	if a == nil {
		return ErrNoBus
	}
	number := a.proofs.Add(1)
	proof, cancel := context.WithTimeout(ctx, proofTimeout)
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
		wg.Go(func() { hubs[i], hubErrs[i] = proveHub(proof, h) })
	}
	if len(targets) > 0 {
		wg.Go(func() { replicas, replicaErrs = readListeners(proof, a.lookup, client, targets) })
	}
	wg.Wait()
	// ctx, not proof: a proof that ran out of proofTimeout is a result, kept as every hub and
	// replica it did not reach failing; one whose caller ended first says nothing of the hubs.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("busprojects: the proof's caller ended before the proof did, so its result is not kept: %w", err)
	}

	var errs []error
	if len(targets) == 0 {
		errs = append(errs, ErrNoListenerInventory)
		replicas = []ReplicaReport{}
	}
	err := errors.Join(slices.Concat(errs, hubErrs, replicaErrs)...)
	a.keep(number, Report{ProvedAt: time.Now().UTC(), Hubs: hubs, Listeners: replicas}, err == nil)
	return err
}

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

func (a *HubAdmission) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

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
