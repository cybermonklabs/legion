package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A changed NATS seed rebuilds the NATS client: the holder opens a connection for the
// configuration the change settled, moves this project's durable consumers onto it, and closes
// the connection it replaced (acceptance A5).
//
// The connection is opened through the daemon's own openNATS, and the seed the changed key
// names is not carried onto the wire, since this pod's NATS server takes no nkeys
// (bootedNATSHolder). The seed is what drives the rebuild, which is what this test is about;
// that the seed a key names is the one the connection authenticates as is
// TestTheWorkflowConnectsAsTheNatsSeedTheDaemonResolved's, against a server that takes one.
func TestAChangedNatsSeedRebuildsTheNatsClient(t *testing.T) {
	url := testnats.URL(t)
	notificationsStreamOn(t, url)
	w, live, _, opened := bootedNATSHolder(t, url,
		"nats_nkey_seed_file: "+testnats.SeedFile(t, testnats.UserSeed(t)+"\n"))
	before := w.nats.connection()

	rotated := testnats.SeedFile(t, testnats.UserSeed(t)+"\n")
	applied := apply(t, live, map[string]any{"nats_nkey_seed_file": rotated})

	var seeds []string
	for _, open := range opened() {
		seeds = append(seeds, open.NatsNkeySeedFile)
	}
	if len(seeds) != 2 || seeds[1] != rotated {
		t.Fatalf("the holder opened its connections for %v, want the second for the seed the change named", seeds)
	}

	if len(applied) != 1 || applied[0].Holder != config.HolderNATS || applied[0].New != "(withheld)" {
		t.Fatalf("the apply reported %+v, want one nats rebuild whose value is withheld", applied)
	}
	after := w.nats.connection()
	if after == before {
		t.Fatal("the changed seed left the daemon on the connection it had, want the client rebuilt")
	}
	if !after.IsConnected() {
		t.Fatalf("the rebuilt NATS client is not connected: %v", after.LastError())
	}
	if !before.IsClosed() {
		t.Fatal("the connection the rebuild replaced was not closed")
	}
	if w.nats.intake() == nil {
		t.Fatal("the rebuilt client carries no durable consumers")
	}
}

// A changed `nats_urls` moves the daemon to the server the change names: the holder opens a
// connection there, moves this project's durable consumers onto it, and closes the connection
// it had. `nats_urls` is one of the keys this change promises applies with no restart.
//
// Every other rebuild test here reconnects to the one test server, so none of them can tell a
// client rebuilt on the configuration the change settled from one rebuilt on the configuration
// the daemon booted with: both answer the caller `rebuilt`, both leave a connected client, and
// in production the second hears nothing the change asked for. This one moves the daemon
// between two servers, so the connection and the durables are read on the server the change
// named and nowhere else.
func TestAChangedNatsURLMovesTheDaemonToTheServerTheChangeNames(t *testing.T) {
	// The server the daemon boots on is the package's shared one; the server it is moved to
	// runs in this process (testnats.StartInProcess), since testnats serves one shared server
	// and the containers it starts need a Docker a Sandbox pod has not got.
	booted, moved := testnats.URL(t), testnats.StartInProcess(t)
	// Envoy's notification stream is what this project's durable consumers are opened on, so
	// the server the daemon is moved to carries one as the server it booted on does.
	notificationsStreamOn(t, booted)
	_, movedStream := notificationsStreamOn(t, moved)
	w, live, rebuild, _ := bootedNATSHolder(t, booted)
	before := w.nats.connection()
	if got := before.ConnectedUrl(); got != booted {
		t.Fatalf("the daemon booted on %q, want the server its configuration named, %q", got, booted)
	}

	applied, failed := routeChange(t, live, rebuild, map[string]any{"nats_urls": []string{moved}})

	if len(applied) != 1 || applied[0].Path != "nats_urls" || applied[0].Holder != config.HolderNATS {
		t.Fatalf("the apply reported %+v, want one nats_urls change the operator is told the NATS holder holds", applied)
	}
	if len(failed) != 0 {
		t.Fatalf("the route was told %v, want the move made: a rebuild that failed keeps the connection it has", failed)
	}
	after := w.nats.connection()
	if after == before {
		t.Fatal("the changed nats_urls left the daemon on the connection it had, want the client rebuilt")
	}
	if !after.IsConnected() {
		t.Fatalf("the rebuilt NATS client is not connected: %v", after.LastError())
	}
	if got := after.ConnectedUrl(); got != moved {
		t.Fatalf("the rebuilt NATS client is connected to %q, want the server the change named, %q", got, moved)
	}
	if w.nats.intake() == nil {
		t.Fatal("the moved client carries no durable consumers")
	}
	// The connection alone is not the move: what the daemon reads are its durable consumers,
	// and a rebuild that dialled the new server while its consumers stayed on the old one
	// would hear every event on a server the configuration no longer names. They are read on
	// the server the change named, through a client of the test's own.
	project := live.Snapshot().Project
	for _, durable := range []string{"legion-go-" + project + "-dispatch", "legion-go-" + project + "-github", "legion-go-" + project + "-wake"} {
		consumer, err := movedStream.Consumer(t.Context(), durable)
		if err != nil {
			t.Fatalf("read %s on the server the change named: %v", durable, err)
		}
		info, err := consumer.Info(t.Context())
		if err != nil {
			t.Fatalf("read %s's state on the server the change named: %v", durable, err)
		}
		if info.Config.Durable != durable {
			t.Fatalf("the consumer read as %s on the server the change named is %s", durable, info.Config.Durable)
		}
	}
	// And what GitHub intake hears there is this project's own repository (grantWorkflow's
	// mapping), worked out by the handover the move ran and written onto the durable the move
	// opened, never copied from the server the daemon left.
	if filters := githubFiltersOf(t, movedStream, project); !slices.Contains(filters, "notifications.github.sjawhar.legion.>") {
		t.Fatalf("the GitHub durable on the server the change named filters on %v, want this project's repository among them", filters)
	}
	if !before.IsClosed() {
		t.Fatal("the connection the move replaced was not closed")
	}
}

// The control of the move above: a `nats_urls` the daemon cannot open is answered `failed` for
// that path, carrying the open's own error, and the daemon is left on the server it had.
//
// The apply itself succeeds — the daemon runs on the URL the operator set, and its next rebuild
// dials that one — so the only thing that tells the caller the move did not happen is the
// failure the route carries (rebuilder.route, api.SettingsRebuildFailures). Until that answer
// existed the rebuilder logged the failure and told the caller `rebuilt`: a move that never
// happened read exactly like one that did, and this control could not be written.
func TestANatsURLTheDaemonCannotOpenIsAnsweredFailed(t *testing.T) {
	booted := testnats.URL(t)
	notificationsStreamOn(t, booted)
	w, live, rebuild, _ := bootedNATSHolder(t, booted)
	before := w.nats.connection()

	// Nothing listens on port 1 here, and no server a test starts can be given it: each takes a
	// port the kernel picks. nats.go retries no failed first connect (natsauth.Connect sets no
	// RetryOnFailedConnect), so the open answers at once with no servers available.
	const dead = "nats://127.0.0.1:1"
	applied, failed := routeChange(t, live, rebuild, map[string]any{"nats_urls": []string{dead}})

	if len(applied) != 1 || applied[0].Path != "nats_urls" || applied[0].Holder != config.HolderNATS {
		t.Fatalf("the apply reported %+v, want one nats_urls change the operator is told the NATS holder holds", applied)
	}
	why, told := failed["nats_urls"]
	if !told {
		t.Fatalf("the route was answered %v, want nats_urls carrying what the daemon could not do for it", failed)
	}
	if !strings.Contains(why, config.HolderNATS+": connect Envoy NATS:") {
		t.Fatalf("nats_urls was answered %q, want the NATS holder's own failure to open the connection the change named", why)
	}
	if after := w.nats.connection(); after != before {
		t.Fatal("the daemon left the connection it had for a URL it could not open")
	}
	if !before.IsConnected() || before.IsClosed() {
		t.Fatalf("the daemon's connection is connected=%v closed=%v after a move it could not make, want it serving as it was",
			before.IsConnected(), before.IsClosed())
	}
	// The change is applied for all that, and it is the rebuild alone that failed: the daemon
	// runs on the URL the operator set, and its next rebuild dials that one.
	if urls := live.Snapshot().NatsURLs; !slices.Equal(urls, []string{dead}) {
		t.Fatalf("the settings hold %v, want the change applied: it is the rebuild that failed, not the apply", urls)
	}
}

// `worker_rpc_timeout_seconds` is the acknowledgement window the durable consumers are opened
// with, and intake derives the limit one fact's handling is cut at from that window as it opens
// them (intake.HandlerLimitFor, read in intake's normalizedSpec): the limit is settled at the
// open and never read again. So the key's row names the NATS holder beside the worker stream
// (config.Classes, Also), and a route change of it reopens the consumers on the window the
// change settled. Without that row the route would answer the caller `rebuilt` — the worker
// stream's own — while intake went on cutting a handler at the limit it booted with.
func TestAChangedWorkerRPCTimeoutReopensTheIntakeConsumers(t *testing.T) {
	url := testnats.URL(t)
	notificationsStreamOn(t, url)
	w, live, _, opened := bootedNATSHolder(t, url)
	before := w.nats.connection()

	// A window wide enough to lift the limit off intake's ten-minute floor: a change that left
	// the floor deciding would prove nothing about the limit the reopened consumers run with.
	const raised = 10 * time.Minute
	if _, derived := intake.HandlerLimitFor(raised); !derived {
		t.Fatalf("a %s window derives no limit above intake's floor; this test proves nothing", raised)
	}
	applied := apply(t, live, map[string]any{"worker_rpc_timeout_seconds": int(raised.Seconds())})

	if len(applied) != 1 || applied[0].Holder != config.HolderWorkerStream || applied[0].Outcome != config.OutcomeRebuilt {
		t.Fatalf("the apply reported %+v, want the one rebuilt change the operator is told the worker stream holds", applied)
	}
	var windows []time.Duration
	for _, open := range opened() {
		windows = append(windows, open.WorkerRPCTimeout)
	}
	if len(windows) != 2 || windows[1] != raised {
		t.Fatalf("the holder opened its consumers for %v, want the second on the window the change settled", windows)
	}
	if after := w.nats.connection(); after == before {
		t.Fatal("the changed window left the daemon on the connection it had, want its consumers reopened")
	}
	if w.nats.intake() == nil {
		t.Fatal("the reopened client carries no durable consumers")
	}
	booted, _ := intake.HandlerLimitFor(windows[0])
	limit, derived := intake.HandlerLimitFor(windows[1])
	if !derived || limit == booted {
		t.Fatalf("the reopened consumers cut a fact's handling at %s, what the boot's %s window already derived; the change reached no handler limit",
			limit, windows[0])
	}
}

// A NATS rebuild hears every repository the daemon hears now, not the set it booted on. The
// repositories GitHub intake hears move while the daemon runs — a mapping read replaces them on
// the consumers themselves (refreshMapping's SetRepositories) and names no setting, so nothing
// is rebuilt for it — and a rebuild that rewrote the shared durable consumer's FilterSubjects
// back to the boot's list would leave a repository mapped since boot unheard: JetStream would
// skip its merges, check runs and reviews, and redeliver none of them when the filter widened
// again.
func TestANatsRebuildHearsTheRepositoriesMappedSinceBoot(t *testing.T) {
	url := testnats.URL(t)
	_, stream := notificationsStreamOn(t, url)
	logs := &syncBuffer{}
	w := grantWorkflow(t, logs)
	cfg := daemonGrantConfig(url)
	// The boot: the consumers are opened through the daemon's own path, with the repositories
	// the mapping serves now (grantWorkflow's sjawhar/legion).
	if err := connectAsBootDoes(t, w, cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	mapped := ghrepo.MustParse("acme/widgets")
	if err := w.nats.intake().SetRepositories(t.Context(), append(w.nats.intake().Repositories(), mapped)); err != nil {
		t.Fatalf("hear the repository mapped after boot: %v", err)
	}

	// Any rebuild: a rotated seed, a changed URL, a reread of the credential files.
	if err := w.nats.rebuild(&cfg); err != nil {
		t.Fatalf("rebuild the NATS holder: %v", err)
	}

	if heard := w.nats.intake().Repositories(); !slices.Contains(heard, mapped) {
		t.Fatalf("after the rebuild GitHub intake hears %v, want the repository mapped since boot among them", heard)
	}
	if filters := githubFiltersOf(t, stream, cfg.Project); !slices.Contains(filters, "notifications.github.acme.widgets.>") {
		t.Fatalf("the GitHub durable consumer filters on %v, want the subject of the repository mapped since boot among them", filters)
	}
}

// A NATS rebuild whose new consumers read the daemon's pool as they are handed what to hear
// completes on a pool of one connection. The handover's SetRepositories is a stream call, and a
// GitHub durable that has gone missing has it create the durable again at the position the pool
// keeps for it (intake's storedStreamTime), which takes a connection of that same pool. Made
// inside a transaction of the handover's, that is a second connection asked for while holding
// one (INFRA-3169): on a pool of one the rebuild waits for the connection its own transaction
// holds until natsRebuildBound ends it, and under the rebuild's hold mark
// (store.WithTransactionTracking) the pool refuses it at once (store.ErrNestedAcquire). Either
// way the operator is answered that the rebuild failed and the daemon stays on the connection it
// had.
//
// The durable is deleted between the open and the handover, the one window in which only the
// handover's own stream call can find it missing.
func TestANatsRebuildWhoseConsumersReadThePoolCompletesOnOneConnection(t *testing.T) {
	url := testnats.URL(t)
	_, stream := notificationsStreamOn(t, url)
	w := grantWorkflow(t, &syncBuffer{})
	// The daemon's store opened on the workflow's database with one connection (daemonDSN), so
	// a second connection asked for under a held one waits for ever rather than being handed a
	// spare.
	st, err := store.Open(context.Background(), daemonDSN(t, w.pool, "legion-nats-rebuild-"+strings.ToLower(randomSuffix(t))))
	if err != nil {
		t.Fatalf("open the daemon's store on one connection: %v", err)
	}
	t.Cleanup(st.Close)
	w.pool = st.Pool()
	cfg := daemonGrantConfig(url)
	if err := connectAsBootDoes(t, w, cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	before := w.nats.connection()
	github := "legion-go-" + cfg.Project + "-github"
	open := w.nats.open
	w.nats.open = func(ctx context.Context, c config.Config) (*nats.Conn, *intake.Consumers, error) {
		conn, consumers, err := open(ctx, c)
		if err != nil {
			return nil, nil, err
		}
		if err := stream.DeleteConsumer(ctx, github); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("delete the GitHub durable before the handover: %w", err)
		}
		return conn, consumers, nil
	}

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- w.nats.rebuild(&cfg) }()
	select {
	case err := <-rebuilt:
		if err != nil {
			t.Fatalf("a rebuild whose new consumers read the pool failed: %v (a refused connection or a deadline here is the handover's stream call made inside a transaction of its own)", err)
		}
	case <-time.After(natsRebuildBound + 10*time.Second):
		t.Fatalf("the rebuild did not return within its own bound (%s)", natsRebuildBound)
	}

	if w.nats.connection() == before {
		t.Fatal("the rebuild left the daemon on the connection it had, want the new one")
	}
	if filters := githubFiltersOf(t, stream, cfg.Project); !slices.Contains(filters, "notifications.github.sjawhar.legion.>") {
		t.Fatalf("the GitHub durable the handover created again filters on %v, want the served repository's subject among them", filters)
	}
}

// A NATS rebuild takes no hold of the lock every fact applies under (intake.SerializeFacts): it
// completes while another transaction holds that lock, as a fact being applied does. Its handover
// hands the new consumers their repositories with a stream call, which waits on JetStream and, for
// a durable that is missing, on the pool (TestANatsRebuildWhoseConsumersReadThePoolCompletesOnOneConnection),
// so a handover that held the fact lock across it would stop every fact of the project for as
// long as NATS took to answer (INFRA-3169). No fact needs it held either: what the handover reads
// stands still under the mapping lock the rebuild holds
// (TestANatsRebuildOpensNoConsumerWhileAMappingReadHoldsTheMappingLock), and
// workflowRuntime.natsHandover says why the swap moves nothing a fact reads. A handover that
// took the lock again waits here on the held transaction and fails at the test's deadline.
func TestANatsRebuildCompletesWhileATransactionHoldsTheFactSerializer(t *testing.T) {
	url := testnats.URL(t)
	notificationsStreamOn(t, url)
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	if err := connectAsBootDoes(t, w, cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	before := w.nats.connection()

	// A fact in flight: its transaction holds the fact lock until it commits.
	fact, err := w.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the fact's transaction: %v", err)
	}
	defer func() { _ = fact.Rollback(t.Context()) }()
	if err := intake.SerializeFacts(t.Context(), fact); err != nil {
		t.Fatalf("take the fact lock as a fact does: %v", err)
	}

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- w.nats.rebuild(&cfg) }()
	select {
	case err := <-rebuilt:
		if err != nil {
			t.Fatalf("rebuild the NATS holder while a fact held the fact lock: %v", err)
		}
	case <-time.After(10 * time.Second):
		// The fact lets the lock go and the rebuild is waited out (natsRebuildBound bounds it),
		// so the connection it swaps in is the one the cleanup closes.
		_ = fact.Rollback(t.Context())
		<-rebuilt
		t.Fatal("the rebuild did not complete while a fact held the fact lock: its handover waits on that lock")
	}

	if w.nats.connection() == before {
		t.Fatal("the rebuild left the daemon on the connection it had, want the new one")
	}
	if err := fact.Commit(t.Context()); err != nil {
		t.Fatalf("commit the fact that held the lock across the rebuild: %v", err)
	}
}

// A mapping read is kept out of the rebuild by the lock it is made under, and from before the
// open, because opening the new consumers is itself an UpdateConsumer on the very GitHub durable
// the old intake is still pulling from. Without that hold: the rebuild reads set A; a mapping
// read then widens the durable to B and serves the new mapping; the rebuild's open writes A back
// over B; and until the handover sets B again the old intake pulls under A. The pinned
// nats-server keeps a consumer's position across a filter update and steps past what the filter
// does not match, so a GitHub event of a repository in B and not in A that lands in that window
// is delivered to nothing, ever.
//
// The rebuild therefore holds the lock a mapping read is made under (mappingMu) from before it
// reads that set to after the swap: while a mapping read holds it the rebuild has not opened
// anything, and the set it opens with is the one that read left.
func TestANatsRebuildOpensNoConsumerWhileAMappingReadHoldsTheMappingLock(t *testing.T) {
	url := testnats.URL(t)
	_, stream := notificationsStreamOn(t, url)
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	if err := connectAsBootDoes(t, w, cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)

	// A mapping read in flight, holding the lock it is made under (refreshMapping): it has not
	// widened the durable's filter yet.
	w.mappingMu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			w.mappingMu.Unlock()
		}
	}()

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- w.nats.rebuild(&cfg) }()
	select {
	case err := <-rebuilt:
		t.Fatalf("the rebuild opened its consumers, and so wrote the GitHub durable's filter, while a mapping read held the mapping lock: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	// The mapping read widens the durable to the set it serves, then lets the lock go.
	mapped := ghrepo.MustParse("acme/widgets")
	if err := w.nats.intake().SetRepositories(t.Context(), append(w.nats.intake().Repositories(), mapped)); err != nil {
		t.Fatalf("hear the repository the mapping read mapped: %v", err)
	}
	w.mappingMu.Unlock()
	unlocked = true

	select {
	case err := <-rebuilt:
		if err != nil {
			t.Fatalf("rebuild the NATS holder: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rebuild did not return once the mapping read had let the mapping lock go")
	}

	if heard := w.nats.intake().Repositories(); !slices.Contains(heard, mapped) {
		t.Fatalf("after the rebuild GitHub intake hears %v, want the repository the mapping read mapped while the rebuild waited among them", heard)
	}
	if filters := githubFiltersOf(t, stream, cfg.Project); !slices.Contains(filters, "notifications.github.acme.widgets.>") {
		t.Fatalf("the GitHub durable consumer filters on %v, want the subject of the repository the mapping read mapped among them", filters)
	}
}

// A `repository_renames` change on a running daemon has GitHub intake hear the pair's other
// name. Four things hold that key (config.Classes' row), the intake consumers among them, and
// their rebuild is what must add the partner: the set the last mapping read left was worked out
// under the renames before the change, so a rebuild that copied it would answer the operator
// `rebuilt` with the GitHub durable still filtering the old name's subjects alone — the merges,
// check runs and reviews GitHub publishes under the name the repository moved to delivered to
// nothing, and never redelivered, until something unrelated read the mapping again.
//
// The two holders are registered as the daemon registers them: the mapping before the consumers
// (workflowRuntime.connect), so the equivalence reaches the mapping first and the consumers'
// rebuild, which works out what they hear under the mapping's own renames
// (workflowRuntime.natsHearing), follows it.
func TestAChangedRepositoryRenamesHasGitHubIntakeHearThePartner(t *testing.T) {
	url := testnats.URL(t)
	js, stream := notificationsStreamOn(t, url)
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	rebuild := newRebuilder(w.log, config.Settled(cfg))
	if err := connectAsBootDoes(t, w, cfg, rebuild, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	const moved = "notifications.github.cybermonklabs.legion.>"
	if filters := githubFiltersOf(t, stream, cfg.Project); slices.Contains(filters, moved) {
		t.Fatalf("the GitHub durable consumer filters on %v before the change, so this proves nothing", filters)
	}

	// The operator configures the transfer while the daemon runs.
	changed := cfg
	changed.RepositoryRenames = ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	rebuild.observe(&changed, []config.Change{changeOfPath(t, "repository_renames")})

	if filters := githubFiltersOf(t, stream, cfg.Project); !slices.Contains(filters, moved) {
		t.Fatalf("the GitHub durable consumer filters on %v, want the subject of the name the repository moved to among them", filters)
	}

	// And the event GitHub publishes under that name reaches the daemon: JetStream matches the
	// subject onto the durable, and the decoder takes an event's repository from the set the
	// same swap set (intake's SetRepositories).
	heard := &heardPullRequests{}
	stop := runDaemonGrantConsumers(t, w, heard)
	defer stop()
	publishDaemonGrantEnvelope(t, js, "notifications.github.cybermonklabs.legion.pr.42", "github", "renamed-partner-pr-opened",
		`{"action":"opened","author":"author","base_ref":"main","body":"Dispatch: CAPTURE-1","head_ref":"legion/CAPTURE-1",`+
			`"head_sha":"head-captured","kind":"pr","number":"42","repo":"cybermonklabs/legion","title":"Captured GitHub event",`+
			`"updated_at":"2026-09-23T00:00:00Z","url":"https://github.com/cybermonklabs/legion/pull/42"}`)
	testwait.Eventually(t, "the daemon applying the pull request GitHub published under the name the repository moved to", func() bool {
		return slices.Contains(heard.repositories(), "cybermonklabs/legion")
	})
}

// A mapping read made between the two rebuilds a `repository_renames` change runs — the intake
// consumers' and the repository mapping's (config.Classes' row, Also) — hands those consumers no
// narrower a set of repositories than the one their filter was just built with.
//
// The read works out what GitHub intake is to hear under the mapping's own equivalence
// (refreshMapping, repositories.Renames), so the order the two holders are rebuilt in decides
// what it reads. Rebuilt the other way round — the consumers first — that read answers the pair
// before the change while the durable already filters on the pair after it, and hands the
// consumers a set without the name the repository moved to: the pinned nats-server steps a
// consumer past what its filter does not match, so every merge, check run and review GitHub
// publishes under the new name is delivered to nothing, with nothing to redeliver it.
//
// Nothing here decides that order: the holders are the daemon's own, registered by the daemon's
// own path (workflowRuntime.connect), the change goes through the real rebuilder.observe, and
// the read is the real refreshMapping, made from inside the consumers' own rebuild so that it
// lands in the window whatever the order is. What is asserted is what that read handed the
// consumers, not which holder ran first.
func TestAMappingReadBetweenTheRenameRebuildsNarrowsNoGitHubFilter(t *testing.T) {
	const project = "TRANSFER"
	url := testnats.URL(t)
	_, stream := notificationsStreamOn(t, url)

	// A workflow whose project maps one repository, with a Dispatch and a GitHub of its own, so
	// the mapping read below is the daemon's own read and not a stand-in for it.
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"})
	client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: project, Default: true}}}
	repos := newRepositories(project, ghrepo.Renames{})
	w := &workflowRuntime{
		pool: isolatedOutboxPool(t), records: record.NewStore(), dispatch: client, dispatchProject: project,
		installs: &installations{}, repositories: repos, log: quietLogger(), mappingChanged: make(chan struct{}, 1),
		defaultBranches: newDefaultBranches(roleTokens{}, repos, github.url, defaultBranchTTL, time.Now),
	}
	if err := w.loadMapping(t.Context()); err != nil {
		t.Fatalf("read the project's repository mapping: %v", err)
	}
	if err := w.nameMapping(t.Context()); err != nil {
		t.Fatalf("name the mapping as GitHub spells it: %v", err)
	}
	cfg := config.Config{Project: project, NatsURLs: []string{url}, WorkerRPCTimeout: time.Second,
		Projects: map[string]config.Project{project: {}}}
	rebuild := newRebuilder(w.log, config.Settled(cfg))
	if err := connectAsBootDoes(t, w, cfg, rebuild, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)

	// The read, made the moment the consumers' own rebuild returns: the first instant of the
	// window between the two holders, with the filter that rebuild has just written on the
	// durable. It takes the place of the NATS holder the daemon's own path registered, so every
	// holder keeps the position that path gave it (rebuilder.replace).
	var filterBuiltWith, handed []ghrepo.Repository
	rebuild.replace(&readingNATSHolder{natsHolder: w.nats, read: func() {
		filterBuiltWith = w.nats.intake().Repositories()
		if err := w.refreshMapping(t.Context()); err != nil {
			t.Errorf("read the repository mapping in the window: %v", err)
			return
		}
		handed = w.nats.intake().Repositories()
	}})

	// The operator configures the transfer while the daemon runs.
	moved := ghrepo.MustParse("acme-archive/widgets")
	changed := cfg
	changed.RepositoryRenames = ghrepo.MustRenames(ghrepo.MustRename("acme/widgets", moved.String()))
	rebuild.observe(&changed, []config.Change{changeOfPath(t, "repository_renames")})

	if !slices.Contains(filterBuiltWith, moved) {
		t.Fatalf("the consumers were rebuilt hearing %v, without the name the repository moved to, so a read that leaves it out narrows nothing and this proves nothing", filterBuiltWith)
	}
	if !slices.Contains(handed, moved) {
		t.Fatalf("the mapping read handed the consumers %v, want the name the repository moved to among them: it worked out what GitHub intake hears under renames older than the ones their filter was built with", handed)
	}
	for _, repo := range filterBuiltWith {
		if !slices.Contains(handed, repo) {
			t.Fatalf("the mapping read handed the consumers %v, narrower than the %v their filter had just been built with", handed, filterBuiltWith)
		}
	}
	if filters := githubFiltersOf(t, stream, project); !slices.Contains(filters, "notifications.github.acme-archive.widgets.>") {
		t.Fatalf("the GitHub durable consumer filters on %v, want the subject of the name the repository moved to among them", filters)
	}
}

// Every reader of the NATS connection and its consumers reads them from the holder at each use,
// so after a rebuild it reads what the rebuild swapped in (rebuilder.go: a reader holds the
// holder, never the thing inside it). Three of those readers are started once, at boot, long
// before any rebuild: the hold poll (pollHoldRelease) and the reconnect watchdog
// (reconnectWatchdog), which workflowRuntime.run starts, and the consumer lag the state route
// reads (consumerLagOf), which serve hands the API. Each once took what the holder held when it
// started and kept it, so after one rebuild the poll read every position from consumers on the
// closed connection and never released a hold placed after it, the watchdog warned for ever
// about the closed connection and never watched the new one, and the state reported every
// consumer unavailable.
//
// So the three are started as the daemon starts them, before the rebuild, and driven only after
// it. Each part reports on its own, so a failure names every reader that kept what it started
// with.
func TestTheReadersOfTheNatsHolderReadWhatARebuildSwappedIn(t *testing.T) {
	url := testnats.URL(t)
	notificationsStreamOn(t, url)
	logs := &syncBuffer{}
	w := grantWorkflow(t, logs)
	cfg := daemonGrantConfig(url)
	if err := connectAsBootDoes(t, w, cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: cfg.Project, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, w.log)
	admission := admit.New(record.NewStore(), engine, 1, cfg.Project, w.log)
	w.admission, w.handlers, w.dispatchProject = admission, []intake.Handler{engine, admission}, cfg.Project
	const warnEvery = 50 * time.Millisecond
	w.holdPollInterval, w.reconnectPollInterval, w.reconnectWarnEvery = 20*time.Millisecond, 5*time.Millisecond, warnEvery

	// The readers, started as the daemon starts them: the state's lag as serve builds it, and the
	// two loops as run starts them, all before anything rebuilds.
	lag := consumerLagOf(w)
	ctx, cancel := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	readers.Go(func() { w.pollHoldRelease(ctx) })
	readers.Go(func() { w.reconnectWatchdog(ctx) })
	t.Cleanup(func() { cancel(); readers.Wait() })

	before := w.nats.connection()
	if err := w.nats.rebuild(&cfg); err != nil {
		t.Fatalf("rebuild the NATS holder: %v", err)
	}
	if w.nats.connection() == before || !before.IsClosed() {
		t.Fatal("the rebuild did not swap in a new connection and close the one it replaced, so this proves nothing")
	}
	rebuilt := time.Now()

	// A hold placed after the rebuild is released, on the position the poll reads from the
	// Dispatch durable the rebuild swapped in: nothing is published, so that consumer is idle
	// and only its idle reading can reach a target this far past the empty stream.
	if err := pgx.BeginFunc(t.Context(), w.pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(t.Context(), tx, record.Issue{
			Key: "LEGION-EDGE", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Project: cfg.Project, Title: "edge", Tree: "LEGION-EDGE", Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
		})
	}); err != nil {
		t.Fatalf("seed the root the hold is placed on: %v", err)
	}
	reconcileAdmission(t, w.pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-EDGE", ResolvedRepo: "sjawhar/legion", Title: "edge", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2},
	}, 1000, intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false})
	if !admission.Held() {
		t.Fatal("the reconcile placed no hold, so this proves nothing")
	}
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool { return !admission.Held() }) {
		t.Error("a hold placed after the rebuild was never released: the poll reads the Dispatch consumer's position from the consumers the rebuild closed")
	}

	// The lag answers from the consumers the rebuild swapped in.
	for _, consumer := range lag(t.Context()) {
		if consumer.Unavailable != "" {
			t.Errorf("the state's lag of %s after the rebuild is unavailable (%s): it reads the consumers the rebuild closed", consumer.Name, consumer.Unavailable)
		}
	}

	// The watchdog watches the connection the rebuild swapped in: no warning while that one is
	// up, though twenty warnings' worth of time has passed since the old one closed, and one once
	// it goes down.
	warned := func() bool { return strings.Contains(logs.String(), "NATS has not reconnected") }
	if testwait.Holds(time.Until(rebuilt.Add(20*warnEvery)), testwait.Poll, warned) {
		t.Error("the watchdog warned while the connection the rebuild swapped in was up: it watches the one the rebuild closed")
		return
	}
	w.nats.connection().Close()
	if !testwait.Holds(10*time.Second, testwait.Poll, warned) {
		t.Error("the watchdog never warned once the connection the rebuild swapped in went down: it does not watch that connection")
	}
}

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
	url := testnats.URL(t)
	js, stream := notificationsStreamOn(t, url)
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	rebuild := newRebuilder(w.log, config.Settled(cfg))
	if err := connectAsBootDoes(t, w, cfg, rebuild, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	heard := &heardPullRequests{}
	w.handlers = []intake.Handler{heard}

	// The daemon's own intake loop, which runs the consumers of each generation the holder
	// hands it (runIntake).
	ctx, cancel := context.WithCancel(context.Background())
	intaking := make(chan error, 1)
	go func() { intaking <- w.runIntake(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-intaking; err != nil {
			t.Errorf("the intake loop stopped: %v", err)
		}
	})
	github := "legion-go-" + cfg.Project + "-github"
	durable := func(ctx context.Context) (*jetstream.ConsumerInfo, bool) {
		consumer, err := stream.Consumer(ctx, github)
		if err != nil {
			return nil, false
		}
		info, err := consumer.Info(ctx)
		return info, err == nil
	}
	testwait.Eventually(t, "the boot's intake pulling from the GitHub durable", func() bool {
		info, ok := durable(t.Context())
		return ok && info.NumWaiting > 0
	})

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
	if err := w.nats.rebuild(&cfg); err == nil {
		t.Fatal("a rebuild whose connection closed before its handover succeeded, so this proves nothing")
	}
	w.nats.open = open
	publishDaemonGrantEnvelope(t, js, "notifications.github.sjawhar.legion.pr.7", "github", "unheard-after-a-failed-handover",
		`{"action":"opened","author":"author","base_ref":"main","body":"Dispatch: CAPTURE-1","head_ref":"legion/CAPTURE-1",`+
			`"head_sha":"head-captured","kind":"pr","number":"7","repo":"someone/else","title":"Captured GitHub event",`+
			`"updated_at":"2026-09-23T00:00:00Z","url":"https://github.com/someone/else/pull/7"}`)
	unheard, err := stream.Info(t.Context())
	if err != nil {
		t.Fatalf("read the stream's last sequence: %v", err)
	}
	testwait.Eventually(t, "the consumers a failed handover left serving acknowledging an event of a repository they do not hear", func() bool {
		info, ok := durable(t.Context())
		return ok && info.AckFloor.Stream >= unheard.State.LastSeq
	})

	handover := w.nats.handover
	w.nats.handover = func(ctx context.Context, consumers *intake.Consumers, swap func()) error {
		return handover(ctx, consumers, func() {
			defer swap()
			before, ok := durable(ctx)
			if !ok {
				t.Error("read the GitHub durable before publishing the partner's event")
				return
			}
			publishDaemonGrantEnvelope(t, js, "notifications.github.cybermonklabs.legion.pr.42", "github", "handover-partner-pr-opened",
				`{"action":"opened","author":"author","base_ref":"main","body":"Dispatch: CAPTURE-1","head_ref":"legion/CAPTURE-1",`+
					`"head_sha":"head-captured","kind":"pr","number":"42","repo":"cybermonklabs/legion","title":"Captured GitHub event",`+
					`"updated_at":"2026-09-23T00:00:00Z","url":"https://github.com/cybermonklabs/legion/pull/42"}`)
			published, err := stream.Info(ctx)
			if err != nil {
				t.Errorf("read the stream's last sequence: %v", err)
				return
			}
			seq := published.State.LastSeq
			if !testwait.Holds(10*time.Second, testwait.Poll, func() bool {
				info, ok := durable(ctx)
				return ok && info.Delivered.Stream >= seq && (info.AckFloor.Stream >= seq || info.Delivered.Consumer >= before.Delivered.Consumer+2)
			}) {
				t.Error("the replaced intake was never delivered the partner's event and settled it before the swap, so this proves nothing")
			}
		})
	}

	// The operator configures the transfer while the daemon runs.
	before := w.nats.connection()
	changed := cfg
	changed.RepositoryRenames = ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	rebuild.observe(&changed, []config.Change{changeOfPath(t, "repository_renames")})
	if w.nats.connection() == before {
		t.Fatal("the change did not rebuild the NATS holder, so this proves nothing")
	}

	testwait.Eventually(t, "the daemon applying the partner's pull request its rebuild's handover was delivered", func() bool {
		return slices.Contains(heard.repositories(), "cybermonklabs/legion")
	})
}

// readingNATSHolder is the daemon's own NATS holder with a read of the repository mapping made
// the moment its rebuild returns, which is the window a `repository_renames` change opens
// between that rebuild and the mapping's. It answers the NATS holder's own name, so putting it
// in that holder's place moves no holder's position (rebuilder.replace).
type readingNATSHolder struct {
	*natsHolder
	read func()
}

func (h *readingNATSHolder) rebuild(cfg *config.Config) error {
	if err := h.natsHolder.rebuild(cfg); err != nil {
		return err
	}
	h.read()
	return nil
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

// notificationsStreamOn creates Envoy's notification stream on the NATS server at url, through
// workflowJetStream's client, and returns that client and the stream: the stream this project's
// durable consumers are opened on, which every test here creates before the daemon connects,
// and the client a test publishes through or reads another stream with. The tests that use it
// — workflowNATS is this on the package's shared server — cannot boot a daemon against a
// stream of another shape.
func notificationsStreamOn(t *testing.T, url string) (jetstream.JetStream, jetstream.Stream) {
	t.Helper()
	js := workflowJetStream(t, url)
	return js, testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
}

// bootedNATSHolder is a daemon's NATS holder booted on the NATS server at url, with whatever
// else settings describes, built as workflowRuntime.connect builds it: the daemon's own
// openNATS and handover, the consumers hearing what this project heard at boot, and the open
// run on the context boot gives it — readinessAttempt's own, cancelled the moment the open
// returns — so a rebuild that kept boot's context fails here as it fails in a daemon.
//
// The server is a parameter of its own because there is no booting without one: it is the
// holder's whole subject, every caller has a URL in hand (testnats.URL, and the stream its
// consumers are opened on created there already), and written as one more line of settings it
// could be left out, which boots the configuration's default server and says nothing.
//
// It answers the workflow, the settings holder a change goes through, the rebuilder that
// holder's changes are observed by, and the configuration of every open the holder has made,
// boot's first and one per rebuild after it: what the rebuild a change caused was given, which
// a test reads the key it changed out of. The holder is stopped when t ends.
//
// One difference from a daemon's own open, which this pod forces: the seed `nats_nkey_seed_file`
// names is not carried onto the wire, since the server here takes no nkeys ("nkeys not supported
// by the server"). What the open was given is reported all the same, so a test about the seed
// reads the one its change named; that the seed a key names is the one the connection
// authenticates as is TestTheWorkflowConnectsAsTheNatsSeedTheDaemonResolved's, against a server
// that takes one.
func bootedNATSHolder(t *testing.T, url string, settings ...string) (*workflowRuntime, *config.Live, *rebuilder, func() []config.Config) {
	t.Helper()
	w := grantWorkflow(t, &syncBuffer{})
	live := liveSettings(t, append([]string{"nats_urls: [" + url + "]"}, settings...)...)
	heard, err := w.heardAtBoot(t.Context())
	if err != nil {
		t.Fatalf("read the repositories this project hears about: %v", err)
	}
	booting, bootOver := context.WithTimeout(t.Context(), bootTimeout)
	var opened []config.Config
	open := func(ctx context.Context, c config.Config) (*nats.Conn, *intake.Consumers, error) {
		opened = append(opened, c)
		c.NatsNkeySeedFile = ""
		return openNATS(ctx, w, c, func(string) (string, bool) { return "", false }, intake.ConsumerSpec{
			Project: c.Project, Repositories: heard, AckWait: c.WorkerRPCTimeout, NakDelay: time.Second, Logger: w.log,
		})
	}
	conn, consumers, err := open(booting, *live.Snapshot())
	if err != nil {
		t.Fatalf("open the workflow's NATS: %v", err)
	}
	rebuild := newRebuilder(w.log, config.Settled(*live.Snapshot()))
	live.OnChange(rebuild.observe)
	w.nats = newNATSHolder(conn, consumers, w.log, open, &w.mappingMu, w.natsHandover)
	bootOver()
	rebuild.register(w.nats)
	t.Cleanup(w.stop)
	return w, live, rebuild, func() []config.Config { return opened }
}

// routeChange is one settings change made the way the settings route makes it: through the
// rebuilder's own way into the holder (rebuilder.route), which answers what this change's own
// rebuilds could not do beside what it applied. The apply helper of live_settings_test.go calls
// the holder directly, and can say nothing of a rebuild that failed.
func routeChange(t *testing.T, live *config.Live, rebuild *rebuilder, overlay map[string]any) ([]config.Change, api.SettingsRebuildFailures) {
	t.Helper()
	route := rebuild.route(func(_ context.Context, _ config.Caller, by string, changes map[string]any) ([]config.Change, []config.Refusal, error) {
		return live.Apply(config.SourceRoute, by, changes)
	})
	applied, refused, failed, err := route(t.Context(), config.CallerOperator, "someone", overlay)
	if err != nil {
		t.Fatalf("apply %v: %v", overlay, err)
	}
	if len(refused) != 0 {
		t.Fatalf("apply %v refused %+v, want every path applied", overlay, refused)
	}
	return applied, failed
}
