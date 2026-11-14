package daemon

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A message a NATS rebuild's handover is delivered while it runs is applied, never acknowledged
// unapplied. On one server the replaced consumers and the ones the rebuild opened share their
// durables, so from the new consumers' SetRepositories, which widens the GitHub durable's filter
// to what the daemon is to hear, until the replaced intake ends, that intake is the one pulling
// the subjects the widening added — here a `repository_renames` partner — while the set it
// decodes against is its own older one, which does not hear them. Acknowledged there, the
// partner's event reaches no handler, and nothing redelivers it.
//
// The partner's pull request is published between the new consumers' SetRepositories and the
// swap, and the replaced intake, the only one running, is let be delivered it and settle it
// before the swap: acknowledged, which the durable's ack floor shows, or handed back, which its
// next delivery shows. What is asserted is that the fact reaches the handler.
//
// Before that, the control: a handover that fails leaves the consumers it was replacing serving
// as they did, acknowledging an event of a repository they do not hear rather than handing it
// back for consumers that were never swapped in, which would hand it back for ever.
func TestAMessageDeliveredDuringTheHandoverIsAppliedNotAcknowledgedUnapplied(t *testing.T) {
	r := newHandoverRig(t)
	w := r.w

	// A rebuild whose own connection is closed before its handover, so the handover's
	// SetRepositories fails.
	open := w.nats.open
	w.nats.open = func(ctx context.Context, c config.Config) (*nats.Conn, *intake.Consumers, error) {
		conn, consumers, err := open(ctx, c)
		if err == nil {
			conn.Close()
		}
		return conn, consumers, err
	}
	if err := w.nats.rebuild(&r.cfg); err == nil {
		t.Fatal("a rebuild whose connection closed before its handover succeeded, so this proves nothing")
	}
	w.nats.open = open
	publishDaemonGrantEnvelope(t, r.js, "notifications.github.sjawhar.legion.pr.7", "github", "unheard-after-a-failed-handover",
		`{"action":"opened","author":"author","base_ref":"main","body":"Dispatch: CAPTURE-1","head_ref":"legion/CAPTURE-1",`+
			`"head_sha":"head-captured","kind":"pr","number":"7","repo":"someone/else","title":"Captured GitHub event",`+
			`"updated_at":"2026-09-23T00:00:00Z","url":"https://github.com/someone/else/pull/7"}`)
	unheard, err := r.stream.Info(t.Context())
	if err != nil {
		t.Fatalf("read the stream's last sequence: %v", err)
	}
	testwait.Eventually(t, "the consumers a failed handover left serving acknowledging an event of a repository they do not hear", func() bool {
		info, ok := r.durable(t.Context())
		return ok && info.AckFloor.Stream >= unheard.State.LastSeq
	})

	handover := w.nats.handover
	w.nats.handover = func(ctx context.Context, consumers *intake.Consumers, swap func()) error {
		return handover(ctx, consumers, func() {
			defer swap()
			r.deliverPartner(t, ctx)
		})
	}

	if !r.transfer() {
		t.Fatal("the change did not rebuild the NATS holder, so this proves nothing")
	}
	r.awaitPartnerApplied(t)
}

// The consumers a NATS rebuild replaces are marked as handing over before the set is handed to
// the ones it opened, not once that call has returned (workflowRuntime.natsHandover). JetStream
// widens the GitHub durable's filter when it takes the new consumers' SetRepositories, before the
// call answers, so a `repository_renames` partner's event can reach the replaced intake while the
// call is still in flight; marked only once it returned, that intake decodes the event against
// its own older set and acknowledges it unapplied. Publishing once the call has returned, as
// TestAMessageDeliveredDuringTheHandoverIsAppliedNotAcknowledgedUnapplied does, cannot tell the
// two orders apart.
//
// So the call is held in flight: a stand-in for it (setRepositories) makes the real one and then
// waits until the test releases it. While it waits, the partner's pull request is published and
// the replaced intake, the only one running, is let be delivered it and settle it. Then the call
// returns, and what is asserted is that the fact reaches the handler.
func TestAMessageDeliveredWhileTheHandedSetIsInFlightIsAppliedNotAcknowledgedUnapplied(t *testing.T) {
	r := newHandoverRig(t)
	reached, held := make(chan struct{}), make(chan struct{})
	r.w.setRepositories = func(ctx context.Context, consumers *intake.Consumers, repositories []ghrepo.Repository) error {
		err := consumers.SetRepositories(ctx, repositories)
		close(reached)
		<-held
		return err
	}

	var swapped bool
	rebuilt := make(chan struct{})
	go func() {
		defer close(rebuilt)
		swapped = r.transfer()
	}()
	release := sync.OnceFunc(func() { close(held) })
	t.Cleanup(func() { release(); <-rebuilt })
	select {
	case <-reached:
	case <-rebuilt:
		t.Fatal("the rebuild ended before its handover handed the new consumers their set, so this proves nothing")
	}

	r.deliverPartner(t, t.Context())
	release()
	<-rebuilt
	if !swapped {
		t.Fatal("the change did not rebuild the NATS holder, so this proves nothing")
	}
	r.awaitPartnerApplied(t)
}

// handoverRig is the daemon a handover test rebuilds: booted as a daemon boots, on a project
// whose mapping serves sjawhar/legion (grantWorkflow), with a handler that records every pull
// request it applies (heard) and the daemon's own intake loop running the consumers of each
// generation the holder hands it (runIntake), already pulling from the GitHub durable when the
// rig is answered.
type handoverRig struct {
	w       *workflowRuntime
	js      jetstream.JetStream
	stream  jetstream.Stream
	cfg     config.Config
	rebuild *rebuilder
	renames config.Change
	heard   *heardPullRequests
}

func newHandoverRig(t *testing.T) *handoverRig {
	t.Helper()
	url := testnats.URL(t)
	js, stream := notificationsStreamOn(t, url)
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	rebuild := newRebuilder(w.log, config.Settled(cfg))
	if err := connectAsBootDoes(t, w, cfg, rebuild, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	r := &handoverRig{w: w, js: js, stream: stream, cfg: cfg, rebuild: rebuild, renames: changeOfPath(t, "repository_renames"), heard: &heardPullRequests{}}
	w.handlers = []intake.Handler{r.heard}

	ctx, cancel := context.WithCancel(context.Background())
	intaking := make(chan error, 1)
	go func() { intaking <- w.runIntake(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-intaking; err != nil {
			t.Errorf("the intake loop stopped: %v", err)
		}
	})
	testwait.Eventually(t, "the boot's intake pulling from the GitHub durable", func() bool {
		info, ok := r.durable(t.Context())
		return ok && info.NumWaiting > 0
	})
	return r
}

// durable reads the GitHub durable, which the replaced consumers and the ones a rebuild opens
// share on one server.
func (r *handoverRig) durable(ctx context.Context) (*jetstream.ConsumerInfo, bool) {
	consumer, err := r.stream.Consumer(ctx, "legion-go-"+r.cfg.Project+"-github")
	if err != nil {
		return nil, false
	}
	info, err := consumer.Info(ctx)
	return info, err == nil
}

// deliverPartner publishes the pull request of the rename partner, cybermonklabs/legion, while
// the replaced intake is the only one running, and waits until that intake has been delivered it
// and settled it: acknowledged, which the durable's ack floor shows, or handed back, which its
// next delivery shows. It runs while a rebuild is under way, so it reports what fails rather than
// stopping there.
func (r *handoverRig) deliverPartner(t *testing.T, ctx context.Context) {
	t.Helper()
	before, ok := r.durable(ctx)
	if !ok {
		t.Error("read the GitHub durable before publishing the partner's event")
		return
	}
	publishDaemonGrantEnvelope(t, r.js, "notifications.github.cybermonklabs.legion.pr.42", "github", "handover-partner-pr-opened",
		`{"action":"opened","author":"author","base_ref":"main","body":"Dispatch: CAPTURE-1","head_ref":"legion/CAPTURE-1",`+
			`"head_sha":"head-captured","kind":"pr","number":"42","repo":"cybermonklabs/legion","title":"Captured GitHub event",`+
			`"updated_at":"2026-09-23T00:00:00Z","url":"https://github.com/cybermonklabs/legion/pull/42"}`)
	published, err := r.stream.Info(ctx)
	if err != nil {
		t.Errorf("read the stream's last sequence: %v", err)
		return
	}
	seq := published.State.LastSeq
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool {
		info, ok := r.durable(ctx)
		return ok && info.Delivered.Stream >= seq && (info.AckFloor.Stream >= seq || info.Delivered.Consumer >= before.Delivered.Consumer+2)
	}) {
		t.Error("the replaced intake was never delivered the partner's event and settled it before the swap, so this proves nothing")
	}
}

// transfer is the operator configuring the transfer of sjawhar/legion to cybermonklabs/legion
// while the daemon runs, handed to the rebuilder as the settings holder hands it a change
// (rebuilder.observe). It returns once the rebuild that change causes has, and answers whether
// that rebuild swapped in a connection of its own.
func (r *handoverRig) transfer() bool {
	before := r.w.nats.connection()
	changed := r.cfg
	changed.RepositoryRenames = ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	r.rebuild.observe(&changed, []config.Change{r.renames})
	return r.w.nats.connection() != before
}

// awaitPartnerApplied waits for the daemon to apply the partner's pull request.
func (r *handoverRig) awaitPartnerApplied(t *testing.T) {
	t.Helper()
	testwait.Eventually(t, "the daemon applying the partner's pull request its rebuild's handover was delivered", func() bool {
		return slices.Contains(r.heard.repositories(), "cybermonklabs/legion")
	})
}

// heardPullRequests is an intake handler that records the repository of every pull request the
// daemon applied, so a test can say whose event reached it.
type heardPullRequests struct {
	mu    sync.Mutex
	repos []string
}

func (h *heardPullRequests) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if opened, is := fact.(intake.PullRequestOpened); is {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.repos = append(h.repos, opened.Repo)
	}
	return intake.Result{}, nil
}

func (h *heardPullRequests) repositories() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.repos)
}
