//go:build e2e

package daemon

// approved_version_news_sessions_setup_live_test.go holds part two's setup of the
// approved-version-news live proof (approved_version_news_sessions_live_test.go): the services the
// proof starts beside each run's daemon (the model proxy, natsd and the Envoy listener), and each
// run's seed, its setup check and its boot.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// sessionsModelProxy is the way every pane reaches the model: a reverse proxy in front of the
// gateway that counts each model request it forwards, so a run can say how many calls it made. It
// keeps nothing of a request: no header, no body.
type sessionsModelProxy struct {
	url   string
	calls atomic.Int64
}

// newSessionsModelProxy starts the proxy for the test's life.
func newSessionsModelProxy(t *testing.T, gateway string) *sessionsModelProxy {
	t.Helper()
	target, err := url.Parse(gateway)
	if err != nil {
		t.Fatalf("ANTHROPIC_BASE_URL is no URL: %v", err)
	}
	p := &sessionsModelProxy{}
	forward := httputil.NewSingleHostReverseProxy(target)
	// A turn streams its answer: each event goes on as it arrives.
	forward.FlushInterval = -1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages") {
			p.calls.Add(1)
		}
		req.Host = target.Host
		forward.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	p.url = server.URL
	return p
}

// count is how many model requests the proxy has forwarded.
func (p *sessionsModelProxy) count() int64 { return p.calls.Load() }

// startSessionsNATS starts natsd from the checkout, with JetStream on a store of its own, on a port
// the kernel picks, which its log names; it is stopped when the test ends.
func startSessionsNATS(t *testing.T, logs string) string {
	t.Helper()
	binary := testbuild.EnvoyBinary(t, "./internal/testnats/cmd/natsd")
	output := &syncBuffer{}
	command := exec.Command(binary, "-p", "-1", "-js", "-sd", t.TempDir())
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	command.Stdout, command.Stderr = sessionsLog(t, logs, "natsd.log", output), sessionsLog(t, logs, "natsd.log", output)
	startSessionsProcess(t, "natsd", command)
	listening := regexp.MustCompile(`Listening for client connections on [^ ]*:(\d+)`)
	var port string
	sessionsAwait(t, "natsd to listen", 30*time.Second, func() bool {
		if match := listening.FindStringSubmatch(output.String()); match != nil {
			port = match[1]
			return true
		}
		return false
	})
	return "nats://127.0.0.1:" + port
}

// startSessionsListener starts the Envoy listener from the checkout on natsURL, serving on a
// loopback port held for it (ENVOY_LISTEN_FD), and waits for its /healthz to answer healthy. Its
// environment is its own: nothing of this pod's Envoy or Dispatch settings reaches it.
func startSessionsListener(t *testing.T, logs, natsURL string) string {
	t.Helper()
	binary := testbuild.EnvoyBinary(t, "./cmd/listener")
	held, port := heldSocket(t)
	command := exec.Command(binary)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "ENVOY_LISTEN_FD=3", "ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=approved-version-news", "NATS_URLS=" + natsURL,
	}
	command.ExtraFiles = []*os.File{held}
	log := sessionsLog(t, logs, "listener.log", nil)
	command.Stdout, command.Stderr = log, log
	startSessionsProcess(t, "the Envoy listener", command)
	_ = held.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	sessionsAwait(t, "the Envoy listener's /healthz to answer healthy", 90*time.Second, func() bool {
		response, err := pollClient.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		var health struct {
			Status string `json:"status"`
		}
		return json.NewDecoder(response.Body).Decode(&health) == nil && health.Status == "healthy"
	})
	return base
}

// sessionsLog is the file name in the evidence directory, appended to, with also when it is not
// nil.
func sessionsLog(t *testing.T, logs, name string, also io.Writer) io.Writer {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(logs, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { file.Close() })
	if also == nil {
		return file
	}
	return io.MultiWriter(file, also)
}

// startSessionsProcess starts command and stops it when the test ends: SIGTERM, then SIGKILL ten
// seconds on.
func startSessionsProcess(t *testing.T, what string, command *exec.Cmd) {
	t.Helper()
	if err := command.Start(); err != nil {
		t.Fatalf("start %s: %v", what, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-exited
		}
	})
}

// sessionsAwait polls ready every 250 ms until it holds, and fails the test naming what after
// within.
func sessionsAwait(t *testing.T, what string, within time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", within, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// seed records every member as the daemon holds it mid-run and starts each role the run seeds
// through the daemon's own start path, in one transaction whose commit runs the hooks its writes
// register (store.Transact), as a fact the daemon applies writes them, as part one's seed does. Each
// member is recorded in its tree, in its phase, holding an admission slot, on an open pull request
// unless it stands in admitted, and its tree's lifecycle is opened under the workflow. A root whose
// deployment gates spec holds its gate open at version 1, which the person approved on Dispatch
// too, and its architect's row of the told record says so (seedArchitectTold). Each role is then
// started by part one's startRoles, which raises its entry and, for a phase worker, writes its
// issue's holder row from that gate; a child's own architect is started by the child's first
// phase start, as every started child's is, which raises no entry. The start's row is taken off
// the outbox (takeStart), and so are the other rows a child's first phase start queued
// (takeFirstPhase), and the claim is written as that start leaves it (seedClaim), once the
// lifecycles are open. No role prompt file is written below the state directory: such a file is
// a launch's whole prompt (specs.rolePromptPaths), and the composed prompts would reach no pane.
//
// A row the seed writes by hand that differs from the row the start path writes fails the run's
// harness here, after the transaction commits, naming the row, the field and both values. The
// claims are checked before the boot (standAtTheirEntries).
func (r *sessionsRun) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	gated := len(r.cfg.Gates.Design) > 0
	if gated {
		for _, member := range r.members {
			if member.spec != "" {
				r.approve(member.spec, 1)
			}
		}
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:5], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	// The engine the booted daemon runs, built from the same configuration (engineConfig), so
	// each start written through it is the row the daemon's own admission writes.
	engine := workflow.New(record.NewStore(), engineConfig(r.cfg, "", nil), quietLogger())
	one := 1
	var starts map[claim.Token]record.SuperviseRequest
	var differences []string
	var firstPhase map[string][]string
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		starts, differences, firstPhase = map[claim.Token]record.SuperviseRequest{}, nil, map[string][]string{}
		issues := make([]record.Issue, len(r.members))
		for i, member := range r.members {
			var listed struct {
				Rank    string `json:"rank"`
				LastSeq int64  `json:"last_seq"`
			}
			r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+member.key, nil, http.StatusOK, &listed)
			status := sessionsStatus(member.phase)
			issue := record.Issue{
				Key: member.key, Repo: r.repo, DefaultBranch: "main", GitHubRepo: r.repo, Tree: member.tree, Project: r.cfg.Project,
				Title: "Approved version news sessions", Phase: member.phase, Generation: 1, Status: status,
				DispatchStatus: status, Rank: listed.Rank, LastDispatchSeq: listed.LastSeq,
			}
			if member.parent != "" {
				parent := member.parent
				issue.Parent = &parent
			}
			issues[i] = issue
			if err := r.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if member.phase != phase.Admitted {
				number++
				if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
					State: record.PullRequestOpen, Issue: member.key, Repo: r.repo, Number: int(number), Branch: "legion/" + member.key,
					HeadSHA: versionNewsHead(member.key), Base: "main", Failing: []string{},
				}); err != nil {
					return err
				}
			}
			if err := r.admit(ctx, tx, member.key); err != nil {
				return err
			}
		}
		for i, member := range r.members {
			if !gated || member.spec == "" {
				continue
			}
			// The opening the architect's row is checked against is read before the gate row is
			// written, so the probe meets a tree whose gate has never opened.
			path, err := toldAtFirstOpening(ctx, tx, engine, issues[i], member.spec)
			if err != nil {
				return err
			}
			if err := r.records.PutGate(ctx, tx, record.DesignGate{
				Issue: member.key, Step: record.DesignStepSpec, ArtifactID: member.spec, LatestVersion: 1, ApprovedVersion: &one, EverOpened: true,
			}); err != nil {
				return err
			}
			differs, err := seedArchitectTold(ctx, tx, member.key, path)
			if err != nil {
				return err
			}
			if differs != "" {
				differences = append(differences, differs)
			}
		}
		for i, member := range r.members {
			// The last row the outbox held before the member's starts, so that the rows a
			// child's first phase start queues beside its architect's are known (takeFirstPhase).
			var before int64
			if member.ownArchitect() {
				if err := tx.QueryRow(ctx, `select coalesce(max(id), 0) from outbox`).Scan(&before); err != nil {
					return err
				}
			}
			if err := startRoles(ctx, tx, r.records, engine, issues[i], member.roles); err != nil {
				return err
			}
			for _, role := range member.roles {
				start, err := takeStart(ctx, tx, member.key, role)
				if err != nil {
					return err
				}
				starts[r.token(member, role)] = start
			}
			if member.ownArchitect() {
				taken, err := takeFirstPhase(ctx, tx, member.key, before)
				if err != nil {
					return err
				}
				firstPhase[member.key] = taken
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the trees through the daemon's start path: %v", err)
	}
	if len(differences) > 0 {
		t.Fatalf("setup: %s", strings.Join(differences, "; "))
	}
	for _, member := range r.members {
		if member.parent == "" {
			if _, err := r.st.OpenTreeLifecycle(ctx, r.project, member.key, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("open the lifecycle of %s: %v", member.key, err)
			}
		}
	}
	for _, member := range r.members {
		for _, role := range member.roles {
			r.seedClaim(ctx, member, role, starts[r.token(member, role)])
		}
		if taken, ok := firstPhase[member.key]; ok {
			r.note("%s's first phase start, which started its own architect, also queued %s, which the daemon before the boot ran: "+
				"the seed took them off the outbox", member.key, strings.Join(taken, ", "))
		}
		if gated && member.spec != "" {
			r.note("%s's architect took %s at its tree's first opening, in a row written by hand and the same as the row that opening writes",
				member.key, sessionsArchitectTook)
		}
	}
}

// sessionsArchitectTook is what the architect's row of the told record holds of a root whose
// deployment gates spec once its gate opened at version 1 of the spec: that version, as the row
// prints it.
const sessionsArchitectTook = `{"spec": 1}`

// seedArchitectTold writes the architect's row of the told record of root by hand, as its tree's
// first opening leaves it, and answers how it differs from path, the row that opening writes
// (toldAtFirstOpening), "" where it does not. The opening takes the row's baseline at the version
// it approves (workflow's putGateTold), so the approval of version 1 that Dispatch carries is news
// to nobody. It settles no opening on the row: a tree whose root holds no gate row has no step
// closed by a row of its own, so its standing gate reads open before the opening as after it, and
// no opened message goes (tellGateMoved). The seed writes the gate open without that opening, so
// the start path writes no such row here.
func seedArchitectTold(ctx context.Context, tx pgx.Tx, root string, path versionNewsTold) (string, error) {
	differs, err := seedToldRow(ctx, tx, root, "architect", sessionsArchitectTook, "the architect's told row of "+root, path)
	if err != nil {
		return "", err
	}
	if len(differs) == 0 {
		return "", nil
	}
	return fmt.Sprintf("the architect's row of the told record of %s: %s, where the start path writes the row at its tree's first opening, "+
		"at version 1 of its spec", root, strings.Join(differs, ", ")), nil
}

// toldAtFirstOpening is the architect's row of the told record the daemon writes for root at its
// tree's first opening, read in a savepoint of tx that is rolled back once the row is read
// (toldInSavepoint). It is read while root holds no gate row, so the tree has never opened. In the
// savepoint the architect's registration of spec at version 1 is applied as the daemon applies it
// (workflow.Engine.Apply), carrying the person's approval of that version as register_gate reads it
// from Dispatch (intake.GateRegistered): that registration opens the gate, which writes the row. It
// is applied in a fact of its own, settled in the savepoint, as intake's settle applies a fact
// (workflow.Engine.OpenFact, workflow.Engine.SettleFact).
func toldAtFirstOpening(ctx context.Context, tx pgx.Tx, engine *workflow.Engine, root record.Issue, spec string) (versionNewsTold, error) {
	return toldInSavepoint(ctx, tx, "the architect's told row of "+root.Key+" at its first opening", func(probe pgx.Tx) (versionNewsTold, error) {
		fact := engine.OpenFact(ctx)
		if _, err := engine.Apply(fact, probe, intake.GateRegistered{Issue: root.Key, Step: record.DesignStepSpec, ArtifactID: spec,
			Version: 1, DispatchVersion: 1, ApprovedVersion: 1}); err != nil {
			return versionNewsTold{}, fmt.Errorf("register version 1 of the spec of %s, approved: %w", root.Key, err)
		}
		if err := engine.SettleFact(fact, probe); err != nil {
			return versionNewsTold{}, fmt.Errorf("settle the registration of version 1 of the spec of %s: %w", root.Key, err)
		}
		return heldTold(ctx, probe, root.Key, "architect")
	})
}

// takeStart takes off the outbox the one start the daemon's start path queued in tx for role on
// issue, and answers it. Part two's claims are launches the daemon before the boot began, so that
// daemon's outbox ran their starts: the start's row goes, as that run finished it, and no start
// of the boot runs it again against the claim the seed writes in its place (seedClaim).
func takeStart(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (record.SuperviseRequest, error) {
	rows, err := tx.Query(ctx, `delete from outbox where kind = $1 and issue = $2 and payload->>'op' = 'start' and payload->>'role' = $3
		returning id, payload`, string(record.OutboxKindSupervise), issue, string(role))
	if err != nil {
		return record.SuperviseRequest{}, fmt.Errorf("take the start of the %s of %s off the outbox: %w", role, issue, err)
	}
	taken, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var payload []byte
		taken := record.OutboxRow{Kind: record.OutboxKindSupervise, Issue: issue}
		err := row.Scan(&taken.ID, &payload)
		taken.Payload = payload
		return taken, err
	})
	if err != nil {
		return record.SuperviseRequest{}, fmt.Errorf("take the start of the %s of %s off the outbox: %w", role, issue, err)
	}
	if len(taken) != 1 {
		return record.SuperviseRequest{}, fmt.Errorf("the daemon's start path queued %d starts of the %s of %s, where it queues one", len(taken), role, issue)
	}
	payload, err := record.DecodeOutboxPayload(taken[0])
	if err != nil {
		return record.SuperviseRequest{}, err
	}
	start, ok := payload.(record.SuperviseRequest)
	if !ok {
		return record.SuperviseRequest{}, fmt.Errorf("the start of the %s of %s decoded as %T", role, issue, payload)
	}
	return start, nil
}

// takeFirstPhase takes off the outbox every row queued in tx on issue, a child, after the row
// after, once takeStart has taken the starts of its roles, and answers what each row was, oldest
// first. They are the rest of the child's first phase start (workflow.Engine.StartFirstPhase),
// which started its own architect: the row that creates its branch and the notices of its move
// into planning. Its planner's start is not among them: the start of the phase worker the child
// stands in took that row's place, as a phase worker's start takes out the one its issue already
// owes (record.Store.EnqueueStart), and takeStart took that start. The daemon before the boot ran
// these rows long before the child reached the phase it stands in, as it ran the starts, so none
// is left for the boot to run again.
func takeFirstPhase(ctx context.Context, tx pgx.Tx, issue string, after int64) ([]string, error) {
	rows, err := tx.Query(ctx, `with taken as (delete from outbox where issue = $1 and id > $2 returning id, kind, payload)
		select kind, coalesce(payload->>'op', ''), coalesce(payload->>'role', ''), coalesce(payload->>'kind', '') from taken order by id`,
		issue, after)
	if err != nil {
		return nil, fmt.Errorf("take the rest of the first phase start of %s off the outbox: %w", issue, err)
	}
	taken, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var kind, op, role, notice string
		if err := row.Scan(&kind, &op, &role, &notice); err != nil {
			return "", err
		}
		switch {
		case kind == string(record.OutboxKindSupervise):
			return fmt.Sprintf("the %s of its %s", op, role), nil
		case kind == string(record.OutboxKindNotice):
			return fmt.Sprintf("a %s notice", notice), nil
		}
		return "a row of kind " + kind, nil
	})
	if err != nil {
		return nil, fmt.Errorf("take the rest of the first phase start of %s off the outbox: %w", issue, err)
	}
	return taken, nil
}

// seedClaim records role's claim on member as start, the start the daemon's start path queued for
// it, leaves it, with that start's launch begun and never recorded by the daemon before the boot,
// which the boot relaunches: launching, with the hash of a boot token no pane holds. A start that
// raised an entry gives the claim that entry, with the handshake the queue writes beside it
// (supervise.Claim.GivenEntry, Handshake). A role its step gives a task of its own (member.tasks)
// holds that task as the delivery of that entry, of the start's phase and run, unconfirmed, in
// place of the start's own task: the daemon sends it once the relaunched pane is ready. A reviewer
// its step gives a note (member.notes) holds the start's own task instead, as the start path
// stored it, with the note before the review round's two sentences it ends with, and marked as
// ending with them (supervise.Delivery.RoundNamed), as the start's own row is
// (record.SuperviseRequest.RoundNamed): the daemon sends it as it stands, as it sends a news task
// it put news into. A role with neither took its start's task before the boot
// (supervise.Claim.ServedEntry), as a root architect mid-run has, and holds nothing. The plain
// start of a child's own architect (workflow's childArchitectStart) raises no entry and carries no
// task, so its claim is given none, holds no handshake and serves no run: its first instruction is
// the catch-up at its launch's ready, and a step's task reaches it through the operator's route
// once the run has booted (deliver). The run's runtime launches this claim and the others the run
// seeds, and no other.
func (r *sessionsRun) seedClaim(ctx context.Context, member *sessionsMember, role claim.Role, start record.SuperviseRequest) {
	t := r.t
	t.Helper()
	token := r.token(member, role)
	r.launchable[token] = true
	now := time.Now()
	task, roundNamed := member.tasks[role], false
	if note := member.notes[role]; note != "" {
		noted, ok := sessionsNoteBeforeRound(start.Task, note)
		if !ok || !start.RoundNamed {
			t.Fatalf("setup: the start of the %s of %s carries no review round's two sentences to put the step's note before: %q", role, member.key, start.Task)
		}
		task, roundNamed = noted, true
	}
	seeded := supervise.Claim{
		Token: token, Project: r.project, Tree: member.tree, Issue: member.key, Role: role, Generation: 1,
		State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}
	switch {
	case start.Entry == 0 && task != "":
		t.Fatalf("setup: the step gives the %s of %s a start task, where its start raised no entry to hold it at", role, member.key)
	case start.Entry > 0:
		seeded.ServingGeneration, seeded.GivenEntry = 1, start.Entry
		seeded.Handshake = supervise.EntryHandshake{Entry: start.Entry, Generation: start.Generation, QueuedAt: now}
		if task == "" {
			seeded.ServedEntry, seeded.Handshake.ConfirmedAt = start.Entry, now
		}
	}
	if _, err := r.st.AdmitClaim(ctx, seeded); err != nil {
		t.Fatalf("record the %s's unfinished launch on %s: %v", role, member.key, err)
	}
	if task == "" {
		return
	}
	if err := r.st.PutDelivery(ctx, token, supervise.Delivery{
		ID: "seeded-start-" + randomSuffix(t), Task: task, Generation: start.Generation, Phase: start.Phase, Entry: start.Entry, QueuedAt: now,
		RoundNamed: roundNamed,
	}); err != nil {
		t.Fatalf("record the start task of %s: %v", token, err)
	}
}

// standAtTheirEntries is the seed's SETUP check, part one's (versionNewsLive.standAtTheirEntries)
// by the same rule (claimEntries, entryRefusal): each claim the seed wrote stands at an entry of 1
// or more of its role's phases row, given that entry, and a claim that holds a task holds it at
// that entry. A child's own architect is held to its plain start's rule instead
// (plainStartRefusal): no entry, given none, holding no task. It runs before the boot, where part
// one's runs after it, because part two's claims are written before the boot rather than by it: a
// run whose seed differs stops before any pane starts or spends a model call. A claim that stands
// anywhere else fails the run's harness, and the failure names the claim and both numbers. Each
// tree's claims are noted with their entries, which is the run's evidence of them.
func (r *sessionsRun) standAtTheirEntries() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	var trees []string
	stood := map[string][]string{}
	for _, member := range r.members {
		if !slices.Contains(trees, member.tree) {
			trees = append(trees, member.tree)
		}
		for _, role := range member.roles {
			token := r.token(member, role)
			name := fmt.Sprintf("the %s of %s", role, member.key)
			entry, given, err := claimEntries(ctx, r.st.Pool(), member.key, role, token)
			if err != nil {
				t.Fatalf("setup: read the entries of %s: %v", name, err)
			}
			refusal := entryRefusal(name, entry, given)
			if role == claim.RoleArchitect && member.ownArchitect() {
				refusal = plainStartRefusal(name, entry, given)
			}
			if refusal != "" {
				t.Fatalf("setup: %s", refusal)
			}
			var held *int64
			if err := r.st.Pool().QueryRow(ctx, `select (select entry from pending_task_deliveries where claim_token = $1)`,
				string(token)).Scan(&held); err != nil {
				t.Fatalf("setup: read the task %s holds: %v", name, err)
			}
			holds := "its start's task taken before the boot"
			if entry == 0 {
				holds = "holding no task, as its plain start gives none"
			}
			if held != nil {
				if *held != entry {
					t.Fatalf("setup: %s holds its start task at entry %d, where the daemon's start path raised entry %d for it", name, *held, entry)
				}
				holds = "holding its start task at that entry"
			}
			stood[member.tree] = append(stood[member.tree], fmt.Sprintf("the %s of %s at entry %d, %s", role, member.key, entry, holds))
		}
	}
	for _, tree := range trees {
		r.note("tree %s as the daemon's start path left it, each claim given its entry: %s", tree, strings.Join(stood[tree], "; "))
	}
}

// plainStartRefusal is why a child's own architect, named name, that stands at entry of its role's
// phases row and was given given does not stand where its plain start leaves it (workflow's
// childArchitectStart, which the child's first phase start queues): that start raises no entry
// and gives none, so both are 0. It is "" where the claim stands there. A claim given an entry, or
// a child whose architect's row names one, was not started the way the daemon starts every
// child's own architect, and the sentence names the claim and both numbers.
func plainStartRefusal(name string, entry, given int64) string {
	if entry == 0 && given == 0 {
		return ""
	}
	return fmt.Sprintf("%s stands at entry %d of its phases row and was given entry %d, where the plain start of a child's own architect, "+
		"which its first phase start queues, raises no entry and gives none", name, entry, given)
}

// boot boots the whole daemon over the local runtime, in the base arm after writing the base
// commit's prompt files over the snapshot the boot wrote, and waits for every seeded claim's pane
// to report ready. The seed is checked before the daemon starts (standAtTheirEntries).
func (r *sessionsRun) boot() {
	t := r.t
	t.Helper()
	r.seed()
	r.standAtTheirEntries()
	r.daemon = newRecordingLog(r.log())
	gated := &sessionsRuntime{localRuntime: r.rt, launchable: r.launchable}
	r.d = startDaemonWithin(t, r.cfg, overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Source,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			// The boot snapshotted the prompts before it builds the runtime (prompts.New, in the
			// plan), and no pane starts before the runtime exists.
			if r.arm == sessionsBase {
				if err := r.s.writeBasePrompts(r.cfg.StateDir); err != nil {
					return nil, err
				}
			}
			r.rt.conns, r.rt.stream = listener, address
			return gated, nil
		},
		listen:         heldListen,
		paneLegion:     r.rt.legion,
		workflowTokens: &workflowTokenRecorder{},
	}, slog.New(r.daemon), 2*time.Minute)
	r.deadline = time.Now().Add(sessionsRunBound)
	for _, member := range r.members {
		for _, role := range member.roles {
			r.awaitReady(r.token(member, role))
		}
	}
}

// sessionsRuntime is the local runtime with every launch refused but those of the claims a run
// seeded: a phase the daemon starts after a step's act starts no session of its own.
type sessionsRuntime struct {
	*localRuntime
	launchable map[claim.Token]bool
}

func (g *sessionsRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if !g.launchable[spec.Claim] {
		return runtime.Locator{}, fmt.Errorf("spawn %s: this proof starts only the sessions its run seeded", spec.Claim)
	}
	return g.localRuntime.Spawn(ctx, spec)
}

func (g *sessionsRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if !g.launchable[spec.Claim] {
		return runtime.Locator{}, fmt.Errorf("resume %s: this proof starts only the sessions its run seeded", spec.Claim)
	}
	return g.localRuntime.Resume(ctx, prev, spec)
}

// writeBasePrompts writes the base commit's role prompt files over the snapshot below stateDir:
// its roles/ over prompts/shared/ and its go/ over prompts/go/. A snapshot file the base holds no
// copy of fails the arm, since a pane reading it would run a prompt of neither commit.
func (s *sessionsLive) writeBasePrompts(stateDir string) error {
	written, differ := 0, 0
	for _, pair := range [][2]string{{"roles", "shared"}, {"go", "go"}} {
		from := filepath.Join(s.base, "packages", "daemon", "internal", "prompts", pair[0])
		to := filepath.Join(stateDir, "prompts", pair[1])
		if err := filepath.WalkDir(to, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			rel, err := filepath.Rel(to, path)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(from, rel)); err != nil {
				return fmt.Errorf("the snapshot holds %s, which the base commit's prompts hold no copy of: %w", path, err)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			rel, err := filepath.Rel(from, path)
			if err != nil {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			target := filepath.Join(to, rel)
			if head, err := os.ReadFile(target); err != nil || !bytes.Equal(head, body) {
				differ++
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			written++
			return os.WriteFile(target, body, 0o600)
		}); err != nil {
			return err
		}
	}
	fmt.Printf("   the base arm: %d prompt files of %s written over the snapshot below %s, %d of them unlike the head's\n",
		written, s.baseCommit, stateDir, differ)
	return nil
}
