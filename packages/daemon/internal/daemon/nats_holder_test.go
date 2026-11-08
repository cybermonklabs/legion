package daemon

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
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

// That read and the swap onto the new consumers run under one hold of the lock every fact
// applies under (intake.SerializeFacts): a mapping read holds that lock across its own
// SetRepositories (refreshMapping), so one that landed between the read and the swap would set
// its repositories on the consumers the rebuild is replacing, and the repository it mapped
// would go unheard with them until something else moved the mapping again.
func TestANatsRebuildWaitsForAMappingReadHoldingTheFactLock(t *testing.T) {
	url := testnats.URL(t)
	notificationsStreamOn(t, url)
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	if err := connectAsBootDoes(t, w, cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)

	// A mapping read in flight: its transaction holds the fact lock from its record snapshot
	// through its SetRepositories.
	mapping, err := w.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the mapping read's transaction: %v", err)
	}
	defer func() { _ = mapping.Rollback(t.Context()) }()
	if err := intake.SerializeFacts(t.Context(), mapping); err != nil {
		t.Fatalf("take the fact lock as a mapping read does: %v", err)
	}

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- w.nats.rebuild(&cfg) }()
	select {
	case err := <-rebuilt:
		t.Fatalf("the rebuild read what the consumers hear while a mapping read held the fact lock: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	// The mapping read maps a repository and commits; the rebuild then reads the set it left.
	mapped := ghrepo.MustParse("acme/widgets")
	if err := w.nats.intake().SetRepositories(t.Context(), append(w.nats.intake().Repositories(), mapped)); err != nil {
		t.Fatalf("hear the repository the mapping read mapped: %v", err)
	}
	if err := mapping.Commit(t.Context()); err != nil {
		t.Fatalf("commit the mapping read: %v", err)
	}
	select {
	case err := <-rebuilt:
		if err != nil {
			t.Fatalf("rebuild the NATS holder: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rebuild did not return once the mapping read had committed")
	}

	if heard := w.nats.intake().Repositories(); !slices.Contains(heard, mapped) {
		t.Fatalf("after the rebuild GitHub intake hears %v, want the repository the mapping read mapped while the rebuild waited among them", heard)
	}
}

// The fact lock alone does not close the window, because opening the new consumers is itself an
// UpdateConsumer on the very GitHub durable the old intake is still pulling from, and that open
// runs outside the fact lock. So: the rebuild reads set A; a mapping read then widens the
// durable to B and serves the new mapping; the rebuild's open writes A back over B; and until
// the swap sets B again the old intake pulls under A. The pinned nats-server keeps a consumer's
// position across a filter update and steps past what the filter does not match, so a GitHub
// event of a repository in B and not in A that lands in that window is delivered to nothing,
// ever.
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
