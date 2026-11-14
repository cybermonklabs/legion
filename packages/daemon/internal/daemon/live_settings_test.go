package daemon

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// liveSettings is the settings holder of a daemon whose configuration is a real legion.yaml: the
// one thing config.Settled cannot stand in for, since a settings change is applied by reading
// that file again under the overlay. lines are appended to the file, each a key the body below
// does not carry — a file holding one key twice is a file no operator writes. `envoy_url` is
// among them: the body leaves it out and the loader settles its default
// (http://127.0.0.1:9020), so a test that wants the daemon's notices to reach a listener of its
// own names it here.
func liveSettings(t *testing.T, lines ...string) *config.Live {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "operator-token")
	if err := os.WriteFile(tokenFile, []byte(testOperatorToken+"\n"), 0o600); err != nil {
		t.Fatalf("write the operator token: %v", err)
	}
	port := holdPortOn(t, "127.0.0.1")
	body := fmt.Sprintf(`project: TEST%s
port: %d
bind: 127.0.0.1
postgres_dsn: %s
state_dir: %s
runtime: tmux
admission_cap: 4
daemon_url: http://127.0.0.1:%d
worker_boot_timeout_seconds: 120
worker_boot_registration_deadline_intervals: 3
worker_rpc_timeout_seconds: 5
worker_stop_timeout_seconds: 10
tree_stop_timeout_seconds: 60
slow_command_timeout_seconds: 300
probe_interval_seconds: 30
launch_failure_limit: 3
prompt_failure_limit: 3
prompt_retire_limit: 2
operator_token_file: %s
controller: operator
`, randomSuffix(t), port, testDSN(t), shortTempDir(t), port, tokenFile)
	path := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(path, []byte(body+strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	live, err := config.NewLive(path, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return live
}

// operatorTokenFile is the file the holder's configuration names as the operator bearer's.
func operatorTokenFile(t *testing.T, live *config.Live) string {
	t.Helper()
	file := live.Snapshot().OperatorTokenFile
	if file == "" {
		t.Fatal("the configuration names no operator_token_file")
	}
	return file
}

// apply is one route change against the running daemon's holder, refusing nothing.
func apply(t *testing.T, live *config.Live, overlay map[string]any) []config.Change {
	t.Helper()
	applied, refused, err := live.Apply(config.SourceRoute, "someone", overlay)
	if err != nil {
		t.Fatalf("apply %v: %v", overlay, err)
	}
	if len(refused) != 0 {
		t.Fatalf("apply %v refused %+v, want every path applied", overlay, refused)
	}
	return applied
}

// A decision-class change takes effect at once, with nothing rebuilt: the daemon's next read of
// the admission cap is the new one, the change says so to the caller (at-once, no holder), and
// no holder was built again for it. What admission then does with the new cap is its own test
// (internal/admit's TestTheAdmissionCapChangedThroughTheSettingsHolderIsUsedByTheNextDecision):
// a deployment with no Dispatch, which is what this harness boots, admits nothing.
func TestAnAdmissionCapChangedThroughTheRouteIsReadByTheNextDecision(t *testing.T) {
	live := liveSettings(t)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)

	if got := d.state().Admission.Cap; got != 4 {
		t.Fatalf("the admission cap before the change = %d, want the file's 4", got)
	}
	applied := apply(t, live, map[string]any{"admission_cap": 7})

	if len(applied) != 1 || applied[0].Path != "admission_cap" || applied[0].New != "7" {
		t.Fatalf("the apply reported %+v, want one admission_cap change to 7", applied)
	}
	if applied[0].Class != config.ClassDecision || applied[0].Outcome != config.OutcomeAtOnce || applied[0].Holder != "" {
		t.Fatalf("the change is %+v, want a decision-class change taking effect at once with no holder", applied[0])
	}
	if got := d.state().Admission.Cap; got != 7 {
		t.Fatalf("the admission cap the running daemon reads = %d, want the applied 7", got)
	}
	if strings.Contains(logs.String(), "holder rebuilt") {
		t.Fatalf("a decision-class change rebuilt a holder:\n%s", logs)
	}
}

// A rebuild-class change rebuilds exactly the holders its row names and nothing else, and the
// worker stream's own rebuild keeps every connection: a shim connected before the change is
// still connected after it, and its claim is still the one the daemon serves (acceptance A5).
//
// `worker_rpc_timeout_seconds` names two: the worker stream, the holder the operator is told
// was rebuilt, and the intake consumers on its row's Also, which take the window as their
// acknowledgement window and derive one fact's handler limit from it as they open
// (config.Classes).
func TestARebuildClassChangeRebuildsOnlyItsHolderAndKeepsTheWorkerConnected(t *testing.T) {
	live := liveSettings(t)
	rt := fake.NewRuntime()
	rec := &built{}
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(rt, rec), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	token := d.spawn(architect())
	readyClaim(t, d, rt, token)
	worker := dialShim(t, rec.address, lastLaunch(t, rt, token).BootToken)
	testwait.Eventually(t, "the shim's claim to be ready", func() bool {
		return d.claim(token).State == string(supervise.StateReady)
	})

	applied := apply(t, live, map[string]any{"worker_rpc_timeout_seconds": 9})

	if len(applied) != 1 || applied[0].Holder != config.HolderWorkerStream || applied[0].Outcome != config.OutcomeRebuilt {
		t.Fatalf("the apply reported %+v, want one worker-stream rebuild", applied)
	}
	awaitLog(t, logs, `"holder":"worker-stream"`, 5*time.Second, "the worker stream's settings were not rebuilt")
	row, known := config.ClassOf("worker_rpc_timeout_seconds")
	if !known || !slices.Contains(row.Also, config.HolderNATS) {
		t.Fatalf("the worker_rpc_timeout_seconds row is %+v, want the intake consumers named beside the worker stream", row)
	}
	// Every holder the row does not name. This daemon runs without NATS, so the consumers are
	// registered nowhere and the rebuilder only logs that the change named a holder it did not
	// build; TestAChangedWorkerRPCTimeoutReopensTheIntakeConsumers is where that rebuild is
	// proved against a server.
	for _, other := range []string{"dispatch-client", "envoy-publisher", "github-apps", "api-server", "controller-keeper", "runtime", "launch-specs"} {
		if strings.Contains(logs.String(), `"holder":"`+other+`"`) {
			t.Fatalf("the worker stream's change rebuilt %s too:\n%s", other, logs)
		}
	}
	// The connection is the one the shim dialled before the change: it is still registered, and
	// the daemon still answers on it.
	if got := d.claim(token).State; got != string(supervise.StateReady) {
		t.Fatalf("the worker's claim across the rebuild = %s, want it still ready on the connection it held", got)
	}
	stillOpen(t, worker)
}

// A rebuild that fails leaves the old holder serving: an operator_token_file the daemon cannot
// read is logged and changes nothing, and the bearer the daemon already holds still opens the
// operator routes.
func TestARebuildThatFailsLeavesTheOldHolderServing(t *testing.T) {
	live := liveSettings(t)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	if status, _ := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true); status != http.StatusOK {
		t.Fatalf("the operator bearer before the change = %d, want 200", status)
	}

	missing := filepath.Join(t.TempDir(), "no-such-operator-token")
	apply(t, live, map[string]any{"operator_token_file": missing})

	awaitLog(t, logs, "rebuilding a holder failed", 5*time.Second, "the failed rebuild was not logged")
	if status, body := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true); status != http.StatusOK {
		t.Fatalf("the operator bearer after the failed rebuild = %d (%s), want the old holder still serving", status, body)
	}
}

// A credential file rotated in place names no changed setting — its path is what the
// configuration holds, and the path did not move — so the watcher asks the holders to read their
// files again (rebuilder.rereadCredentials). The API server then compares against the rotated
// bearer, and the one it held before is refused.
func TestARotatedCredentialFileWhosePathDidNotChangeIsReadAgain(t *testing.T) {
	live := liveSettings(t)
	var rebuild *rebuilder
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.settingsRebuilder = func(r *rebuilder) { rebuild = r }
	d := startDaemonOn(t, live, o, quietLogger(), 10*time.Second)
	if status, _ := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true); status != http.StatusOK {
		t.Fatalf("the operator bearer before the rotation = %d, want 200", status)
	}

	rotated := "rotated-" + testOperatorToken
	if err := os.WriteFile(operatorTokenFile(t, live), []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatalf("rotate the operator token: %v", err)
	}
	if rebuild == nil {
		t.Fatal("the daemon handed the test no rebuild observer")
	}
	rebuild.rereadCredentials(live.Snapshot())

	if status, _ := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true); status != http.StatusForbidden {
		t.Fatalf("the bearer the daemon held before the rotation = %d, want 403 once the file was read again", status)
	}
	if status := bearerRequest(t, d, rotated); status != http.StatusOK {
		t.Fatalf("the rotated bearer = %d, want 200", status)
	}
}

// countingHolder is a holder that counts the rebuilds asked of it, under whichever name the test
// registers it as, and answers whatever rebuild failure the test gave it.
type countingHolder struct {
	name  string
	built int
	// fails is what its rebuild answers: a holder the daemon could not build again — a NATS the
	// new seed cannot reach — answers one, and the rebuild succeeded when it is nil.
	fails error
}

func (h *countingHolder) holder() string { return h.name }

func (h *countingHolder) rebuild(*config.Config) error {
	h.built++
	return h.fails
}

// countingCredentialHolder is a countingHolder built from credentials of its own, which it says
// as every credential holder does (credentialHolder).
type countingCredentialHolder struct {
	countingHolder
	reads func(cfg *config.Config) string
}

func (h *countingCredentialHolder) credentials(cfg *config.Config) string { return h.reads(cfg) }

// The file watcher wakes on the state of a whole directory, so a projected token the kubelet
// refreshed wakes it beside credentials that did not move. A reread rebuilds a credential holder
// only when the files that holder reads have moved: the wake that follows a kubelet refresh of
// something else leaves the NATS connection and its durable consumers where they are, and the
// wake that follows a rotation of the operator bearer rebuilds the API server alone.
func TestARereadRebuildsOnlyTheHoldersWhoseCredentialFilesMoved(t *testing.T) {
	dir := t.TempDir()
	operatorFile := filepath.Join(dir, "operator-token")
	seedFile := filepath.Join(dir, "nats-seed")
	for file, contents := range map[string]string{operatorFile: "operator\n", seedFile: "seed\n"} {
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	cfg := &config.Config{OperatorTokenFile: operatorFile, NatsNkeySeedFile: seedFile}
	rebuild := newRebuilder(quietLogger(), config.Settled(*cfg))
	server := &countingCredentialHolder{
		countingHolder: countingHolder{name: config.HolderAPIServer},
		reads:          func(cfg *config.Config) string { return credentialDigest(cfg.OperatorTokenFile) },
	}
	natsClient := &countingCredentialHolder{
		countingHolder: countingHolder{name: config.HolderNATS},
		reads:          func(cfg *config.Config) string { return credentialDigest(cfg.NatsNkeySeedFile) },
	}
	rebuild.register(server, natsClient)

	// The first reread after boot rebuilds nothing: each holder was built from its files as they
	// stand, and what they said was recorded as the holder was registered. Without that the
	// kubelet's first refresh of any watched directory — `agent_secrets.token_file`'s among them
	// — would reconnect NATS, migrate every durable consumer and drop every App lease.
	rebuild.rereadCredentials(cfg)
	if server.built != 0 || natsClient.built != 0 {
		t.Fatalf("the first reread rebuilt the API server %d times and NATS %d, want neither: nothing they read has moved since boot", server.built, natsClient.built)
	}

	// A wake whose watched files did not move rebuilds nothing either.
	rebuild.rereadCredentials(cfg)
	if server.built != 0 || natsClient.built != 0 {
		t.Fatalf("a reread over unmoved files rebuilt the API server %d times and NATS %d, want neither", server.built, natsClient.built)
	}

	// A rotation of the operator bearer in place rebuilds the API server, and only it.
	if err := os.WriteFile(operatorFile, []byte("rotated-operator\n"), 0o600); err != nil {
		t.Fatalf("rotate the operator token: %v", err)
	}
	rebuild.rereadCredentials(cfg)
	if server.built != 1 {
		t.Fatalf("the API server was rebuilt %d times over a rotated operator_token_file, want once", server.built)
	}
	if natsClient.built != 0 {
		t.Fatalf("NATS was rebuilt %d times over a rotation of someone else's file, want never", natsClient.built)
	}

	// And the wake after that rotation, with the file settled again, rebuilds nothing.
	rebuild.rereadCredentials(cfg)
	if server.built != 1 || natsClient.built != 0 {
		t.Fatalf("the reread after the rotation rebuilt the API server %d times and NATS %d, want 1 and 0", server.built, natsClient.built)
	}
}

// bearerRequest is one operator route called with the bearer the test names.
func bearerRequest(t *testing.T, d *daemon, token string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, d.base+"/legion/v1/operator/claims", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := d.client.Do(request)
	if err != nil {
		t.Fatalf("call the operator route: %v", err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

// stillOpen fails unless the shim's connection is still open: a read of it times out rather than
// reaching the end a closed connection gives at once.
func stillOpen(t *testing.T, s *shim) {
	t.Helper()
	if err := s.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	switch _, err := s.lines.ReadByte(); {
	case err == nil:
		t.Fatal("the daemon wrote on the connection where the test expected it idle")
	case os.IsTimeout(err):
	default:
		t.Fatalf("the connection the rebuild kept is gone: %v", err)
	}
}

// The bound of an update round is read where the round is decided, not off the configuration the
// daemon booted on: at every sweep (supervision.live), both where the sweep tells the operator's
// side of a round held past it and where it replaces the pod at it. The rig runs the daemon's own
// sweep and machines over a settings holder a real legion.yaml backs, so a change here is the
// change an operator makes.
//
// The notice: a round pinned by an operator-suspended claim waits. Past the bound the rig's own
// configuration carries, and under the holder's, nothing is told — the sweep reads the holder —
// and the controller is told at the next sweep once the holder's bound is under the wait.
func TestThePodUpdateGraceChangedUnderTheRunningSweepIsToldAtTheNextTick(t *testing.T) {
	live := liveSettings(t, "pod_update_grace_minutes: 480")
	rig := newPodUpdateRigOn(t, live,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true,
			contract: api.DaemonAPIVersion, suspendedBy: supervise.SuspendedByOperator},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.clockAt(podUpdateBound.Add(time.Hour))
	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times %s after the round opened; the holder's bound is 8 hours",
			len(notices), rig.clock.Now().Sub(podUpdateStart))
	}

	apply(t, live, map[string]any{"pod_update_grace_minutes": 60})
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times under the bound the operator changed to an hour, want once", len(notices))
	}
	if notices[0].Update == nil || notices[0].Update.Round != podUpdateRound {
		t.Fatalf("the notice is %+v, want the held round's", notices[0])
	}
}

// The replacement: a worker that answers nothing holds the pod until the bound, and the bound is
// the holder's at that sweep. Past the rig's own and under the holder's, the pod is not replaced;
// once the operator shortens it, the next sweep replaces the pod at the bound.
func TestThePodUpdateGraceChangedUnderTheRunningSweepIsReadByTheNextReplacement(t *testing.T) {
	live := liveSettings(t, "pod_update_grace_minutes: 120")
	rig := newPodUpdateRigOn(t, live, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.clockAt(podUpdateBound.Add(time.Minute))
	rig.tick()
	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times %s after the round opened; the holder's bound is two hours",
			len(calls), rig.clock.Now().Sub(podUpdateStart))
	}

	apply(t, live, map[string]any{"pod_update_grace_minutes": 15})

	rig.awaitReplacement(1, "at the bound the operator shortened")
	bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine)
	if len(bounded) != 1 {
		t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
	}
	wantField(t, bounded[0], "unanswered", claim.RoleImplementer)
}

// A capability decision an operator records reaches the next read of the state: the deployment's
// report is built from the settings holder at every read (supervision.deployment), so the row
// the daemon reported open stands decided with the operator's reason, at once and with nothing
// rebuilt.
func TestACapabilityDecisionChangedThroughTheRouteIsInTheNextStateRead(t *testing.T) {
	live := liveSettings(t)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	if row := capabilityRow(t, d, capabilities.Secrets); row.Status != capabilities.StatusOpen {
		t.Fatalf("the secrets row before the decision = %+v, want it open: this deployment enrolls no process", row)
	}

	const reason = "dispatch://LEGION-205 enrolls pods later"
	applied := apply(t, live, map[string]any{"capabilities.decided": map[string]any{"secrets": reason}})

	if len(applied) != 1 || applied[0].Path != "capabilities.decided" {
		t.Fatalf("the apply reported %+v, want one capabilities.decided change", applied)
	}
	if applied[0].Outcome != config.OutcomeAtOnce || applied[0].Holder != "" {
		t.Fatalf("the change is %+v, want it taking effect at once with no holder", applied[0])
	}
	row := capabilityRow(t, d, capabilities.Secrets)
	if row.Status != capabilities.StatusDecided || row.Decision != reason {
		t.Fatalf("the secrets row after the decision = %+v, want it decided with the operator's reason", row)
	}
	if strings.Contains(logs.String(), "holder rebuilt") {
		t.Fatalf("a decision-class change rebuilt a holder:\n%s", logs)
	}
}

// capabilityRow is one row of the deployment's capability report as the running daemon's state
// carries it.
func capabilityRow(t *testing.T, d *daemon, name capabilities.Name) api.CapabilityState {
	t.Helper()
	for _, row := range d.state().Capabilities {
		if row.Name == string(name) {
			return row
		}
	}
	t.Fatalf("the state carries no %s capability row", name)
	return api.CapabilityState{}
}

// The two boot bounds are the runtime's to rebuild: every machine reads them per decision, and
// the Sandbox runtime holds them too — each wait of a launch and of a pod's observation is
// bounded by the boot timeout it was built with, and its launch bound is derived from it. The
// route answers `rebuilt` naming the runtime, and the runtime is built again under the work it
// is running, so no reader is left bounding a launch by the value the daemon booted on.
func TestTheWorkerBootBoundsChangedThroughTheRouteRebuildTheRuntime(t *testing.T) {
	live := liveSettings(t)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)

	applied := apply(t, live, map[string]any{
		"worker_boot_timeout_seconds":                 90,
		"worker_boot_registration_deadline_intervals": 4,
	})

	if len(applied) != 2 {
		t.Fatalf("the apply reported %+v, want both boot bounds changed", applied)
	}
	for _, change := range applied {
		if change.Class != config.ClassRebuild || change.Outcome != config.OutcomeRebuilt || change.Holder != config.HolderRuntime {
			t.Fatalf("the change of %s is %+v, want a rebuild of the runtime", change.Path, change)
		}
	}
	awaitLog(t, logs, `"holder":"runtime"`, 5*time.Second, "the runtime was not built again for the boot bounds")
	state := d.state()
	if state.Settings.WorkerBootTimeoutSeconds != 90 {
		t.Errorf("the daemon reads worker_boot_timeout_seconds %d, want the applied 90", state.Settings.WorkerBootTimeoutSeconds)
	}
}

// The design gate reaches the next launch: the launch specs hold a copy of it for every tree
// architect's prompt, and the gate's row names them beside the API server (config.Row.Also), so
// an architect started after an operator moved the gate is told the policy the daemon decides
// by — and not the one it booted on.
func TestADesignGateChangedThroughTheRouteReachesTheNextArchitectsLaunch(t *testing.T) {
	live := liveSettings(t)
	rt := fake.NewRuntime()
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	booted := lastLaunch(t, rt, d.spawn(architect()))
	if want := DesignGateFragment(config.DesignGateRootIssues.Steps()); !strings.Contains(booted.Prompt.Addressing, want) {
		t.Fatalf("the first architect is addressed %q, want it to carry %q", booted.Prompt.Addressing, want)
	}

	applied := apply(t, live, map[string]any{"gates.design": "off"})

	// `gates.design` is a list of steps now, and a change of it is reported as that list: `off`
	// is the empty one (config.Render).
	if len(applied) != 1 || applied[0].Path != "gates.design" || applied[0].New != "[]" {
		t.Fatalf("the apply reported %+v, want one gates.design change to off", applied)
	}
	if applied[0].Outcome != config.OutcomeAtOnce || applied[0].Holder != config.HolderAPIServer {
		t.Fatalf("the change is %+v, want it deciding at once with the API server named", applied[0])
	}
	awaitLog(t, logs, `"holder":"launch-specs"`, 5*time.Second, "the launch specs were not built again for the gate")
	next := lastLaunch(t, rt, d.spawn(api.SpawnRequest{
		Tree: "LEGION-2", Issue: "LEGION-2", Role: claim.RoleArchitect, Prompt: "Reply ready and wait.",
	}))
	if want := DesignGateFragment(config.DesignGateOff.Steps()); !strings.Contains(next.Prompt.Addressing, want) {
		t.Fatalf("the architect started after the change is addressed %q, want it to carry %q", next.Prompt.Addressing, want)
	}
}

// A change of the project block rebuilds the launch specs and the runtime: the diff names
// `projects` whatever member moved, since the block is compared whole. So one change of any
// member, here the project's production_check (Slack channels need a daemon-launched controller
// this harness does not run), is what proves both were built again.
//
// The change carries `github_apps` beside it because the loader takes a `projects` block in no
// file without one (config's resolveStage3), and a file holding neither is what this harness
// boots: the key this test is about is the project's own block. This daemon has no Dispatch and
// so no App manager to build again, which the rebuilder logs and nothing else.
func TestTheProjectsBlockChangedThroughTheRouteRebuildsTheLaunchSpecsAndTheRuntime(t *testing.T) {
	live := liveSettings(t)
	rt := fake.NewRuntime()
	logs := &syncBuffer{}
	startDaemonOn(t, live, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)

	applied := apply(t, live, map[string]any{
		"projects": map[string]any{
			live.Snapshot().Project: map[string]any{"production_check": "off"},
		},
		"github_apps": map[string]any{
			"implement": map[string]any{"app_id": "1", "private_key": "implement-test-key"},
			"review":    map[string]any{"app_id": "2", "private_key": "review-test-key"},
		},
	})

	projects, held := changeOf(applied, "projects")
	if !held {
		t.Fatalf("the apply reported %+v, want the projects block among the changes", applied)
	}
	if projects.Outcome != config.OutcomeRebuilt || projects.Holder != config.HolderLaunchSpecs {
		t.Fatalf("the change is %+v, want a rebuild naming the launch specs", projects)
	}
	for _, holder := range []string{config.HolderLaunchSpecs, config.HolderRuntime} {
		awaitLog(t, logs, `"holder":"`+holder+`"`, 5*time.Second, "the project's change left "+holder+" holding the boot's block")
	}
}

// A fixed key is settled at boot, and `hooks` is the one whose words live nowhere else: boot
// reads the file of every hook that names a `prompt_file` into a map of its own (readBoot), and
// the list the engine fires is built from the two together, once, from the configuration the
// daemon booted on (openWorkflow, engineHooks). The file arm reads the whole file again
// (internal/settings' Keeper.File), so an operator who renames a file-backed hook, adds one or
// repoints one moves nothing under the running engine, which goes on firing what boot read.
//
// Built from the settings holder at every decision instead, the engine would stand a hook in
// front of every role it names with no words at all — a hook the loader refuses at boot
// (config's readHook) — and `whyHooks` is the reason the route refuses the key. The settle
// pins the key too, back to the list boot read (config's pinFixed), so the snapshot the engine's
// configuration is composed from carries that list, and the line that puts boot's own list on
// that configuration (openWorkflow) is a second defence. What a running engine is handed after
// such a reload is read through the engine itself, in workflow_test.go's
// TestAFileReloadLeavesTheEngineFiringTheHookBootRead.
func TestAFileReloadLeavesTheEnginesHooksAsBootReadThem(t *testing.T) {
	live := liveSettings(t, "hooks: [{name: nudge, on: work_back, every: 2, roles: [implementer], prompt_file: nudge.md}]")
	words := "Say what the round missed."
	if err := os.WriteFile(filepath.Join(filepath.Dir(live.Path()), "nudge.md"), []byte(words+"\n"), 0o600); err != nil {
		t.Fatalf("write the hook's prompt_file: %v", err)
	}
	boot := live.Snapshot()
	prompts := map[string]string{}
	for _, hook := range boot.Hooks {
		read, err := config.ReadHookPrompt(hook.Name, hook.PromptFile)
		if err != nil {
			t.Fatalf("read the hook's words as boot reads them: %v", err)
		}
		prompts[hook.Name] = read
	}
	hooks := engineHooks(boot.Hooks, prompts)
	if len(hooks) != 1 || hooks[0].Name != "nudge" || hooks[0].Prompt != words {
		t.Fatalf("the hook list boot built = %+v, want the file's one hook carrying the words of its prompt_file %q", hooks, words)
	}

	// The operator renames the hook and points it at a file of its own, which is the edit this
	// test is about: nothing reads that file, since nothing reads a hook's file after boot. The
	// same edit raises the admission cap, a decision key, so the reload is seen to have happened.
	setInConfig(t, live, "hooks", "[{name: shove, on: work_back, every: 2, roles: [implementer], prompt_file: shove.md}]")
	setInConfig(t, live, "admission_cap", "9")
	_, refused, err := live.Apply(config.SourceFile, "", map[string]any{})
	if err != nil {
		t.Fatalf("the file arm of the settings route: %v", err)
	}
	if cap := live.Snapshot().AdmissionCap; cap != 9 {
		t.Fatalf("the settled admission cap = %d, want the file's 9: the reload this test is about did not happen", cap)
	}
	if reloaded := live.Snapshot().Hooks; len(reloaded) != 1 || reloaded[0].Name != "nudge" {
		t.Fatalf("the settled configuration's hooks = %+v, want the one boot read: `hooks` is fixed for the process and the settle pins it", reloaded)
	}
	row, _ := config.ClassOf("hooks")
	if len(refused) != 1 || refused[0].Path != "hooks" || refused[0].Reason != row.Why {
		t.Fatalf("the settle refused %+v, want the pinned `hooks` named with its own reason", refused)
	}

	if took := engineConfig(live.Snapshot(), "").Hooks; len(took) != 0 {
		t.Fatalf("the engine's configuration took %+v from the settings holder, want no hook of its own: the words of a hook that names a prompt_file are in no configuration at all, and the engine would fire that one saying nothing", took)
	}
}

// `project` is fixed for the process: the project token names every claim, every record in the
// store and the daemon's own registry entry (`whyProject`). The engine's configuration is
// composed from the settings holder at every decision (engineConfig) and the NATS consumers are
// reopened from it at every rebuild, so an operator who edits `project:` in `legion.yaml` ahead
// of a planned restart would otherwise move, at the next settle, the project the engine filters
// every resume and every status repair by (workflow's resume.go, status_repair.go) and the
// project the durable consumers' own subject filter is built from — while admission, intake,
// `dispatchProject` and the claim tokens stayed on the one the daemon booted on. The settle
// pins it instead (config's pinFixed) and names the path, and the edit reaches the next boot.
//
// The same edit widens the RPC window, a rebuild key the NATS holder is named on beside the
// worker stream (config.Classes' Also), so the settle reopens the consumers: the configuration
// that reopen was given is the one the consumer spec is read out of (workflowRuntime.connect).
func TestAFileReloadLeavesTheEnginesProjectAndTheReopenedConsumersOnTheBootProject(t *testing.T) {
	url := testnats.URL(t)
	notificationsStreamOn(t, url)
	w, live, _, opened := bootedNATSHolder(t, url)
	boot := live.Snapshot().Project

	setInConfig(t, live, "project", "OTHERPROJ")
	setInConfig(t, live, "worker_rpc_timeout_seconds", "600")
	applied, refused, err := live.Apply(config.SourceFile, "", map[string]any{})

	if err != nil {
		t.Fatalf("the file arm of the settings route: %v", err)
	}
	row, _ := config.ClassOf("project")
	if len(refused) != 1 || refused[0].Path != "project" || refused[0].Reason != row.Why {
		t.Fatalf("the settle refused %+v, want the pinned `project` named with its own reason", refused)
	}
	if _, moved := changeOf(applied, "project"); moved {
		t.Fatalf("the settle applied %+v, want no change of the project the daemon is running on", applied)
	}
	if project := live.Snapshot().Project; project != boot {
		t.Fatalf("the settled project = %q, want the boot's %q", project, boot)
	}
	if project := engineConfig(live.Snapshot(), "").Project; project != boot {
		t.Fatalf("the engine's project after the reload = %q, want the boot's %q: it owns none of the work the daemon is running on any other", project, boot)
	}

	opens := opened()
	if len(opens) != 2 {
		t.Fatalf("the holder opened %d times, want the boot's and the reload's: the rebuild this test reads the project out of did not happen", len(opens))
	}
	if opens[1].WorkerRPCTimeout != 10*time.Minute {
		t.Fatalf("the reopened consumers were given the window %s, want the file's 10m: the reload did not reach the rebuild", opens[1].WorkerRPCTimeout)
	}
	if opens[1].Project != boot {
		t.Fatalf("the reopened consumers were given the project %q, want the boot's %q: their durable names and their subject filter are built from it", opens[1].Project, boot)
	}
	if w.nats.intake() == nil {
		t.Fatal("the reopened client carries no durable consumers")
	}
}

// passRelaunchBound is how long a stale-plugin relaunch the pass posted is given to leave the
// claim's ready state. A claim the pass posted nothing to never leaves it, so the bound is
// what that reading costs and not a race: generous enough that a loaded machine reads a
// relaunch it did post, short enough that the two readings below cost seconds.
const passRelaunchBound = 5 * time.Second

// `worker_backends` is a launch key an operator moves under the running daemon, and the
// plugin-install rule reads it (noPluginInstall): under Kubernetes a Claude Code role's plugin
// is its pod image's own claude-envoy, which no install reaches and no relaunch moves, so that
// claim is left to the round over its pod. Three readers hold that one rule — this package's
// stale-plugin pass, the machines' stale registration and the API's stale-caller refusal — and
// a change of the key reaches them together or not at all. Reading it from two configurations
// is what that costs: a pass relaunching a worker the machines judge exempt charges the claim
// for the registration that follows, one relaunch at a time toward failed, and an API answering
// for the backend an operator just set while the pass answers for the boot's holds that
// worker's grants, its handoff_complete and its ready on a relaunch nobody started.
//
// The rig wires the machines' reading exactly as the daemon wires both of the readings it hands
// out, which are one call (newPodUpdateRigOn; daemon.go's openSupervision and its API options),
// so the two answers compared here are the daemon's own. Its boot configuration is the
// Kubernetes deployment's, as every pod-update rig's is, and the holder is where the key moves:
// the rule takes the runtime from the boot configuration and the backend from the holder, so
// the `runtime:` of the holder's own file is read by nobody here.
func TestALiveWorkerBackendsChangeAnswersTheSameAtTheStalePluginPassAndTheMachines(t *testing.T) {
	live := liveSettings(t, "worker_backends: {implementer: claude}")
	rig := newPodUpdateRigOn(t, live,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true,
			contract: supervise.PodUpdateContract - 1},
		podUpdateRole{role: claim.RoleTester, state: supervise.StateReady, session: true, inPod: true,
			contract: supervise.PodUpdateContract - 1},
	)
	// The boot configuration is what a boot on that holder's file resolved, the implementer on
	// Claude Code among it: the rule takes the runtime from it and the role's backend from the
	// holder, so at boot the two readings agree, and the route change below is what they must
	// follow together.
	rig.s.cfg.WorkerBackends[claim.RoleImplementer] = runtime.BackendClaude
	// The pass's own answer, read off what it did: a claim it posts nothing to is one whose
	// launch installs no plugin, and a claim it relaunches is one an install reaches.
	passLeaves := func(role claim.Role) bool {
		t.Helper()
		claims, err := rig.sup.Claims(rig.ctx)
		if err != nil {
			t.Fatalf("read the claims for the sweep: %v", err)
		}
		rig.s.refreshStalePlugins(claims, "sweep")
		// A relaunch the pass posted is started on the machine's own goroutine, so the claim is
		// read until it leaves ready; one still ready when the bound passes is one the pass
		// posted nothing to, which is the answer this reads.
		return !testwait.Holds(passRelaunchBound, testwait.Poll, func() bool {
			return rig.claimOf(role).State != supervise.StateReady
		})
	}
	machinesLeave := func(role claim.Role) bool { return rig.sup.deps.NoPluginInstall(rig.claimOf(role)) }

	// The file puts the implementer on Claude Code, and the tester is on Oh My Pi: the control
	// that the pass does post a relaunch for a role whose plugin an install reaches.
	if left, machines := passLeaves(claim.RoleImplementer), machinesLeave(claim.RoleImplementer); !left || !machines {
		t.Fatalf("the Claude Code worker was left running by the pass: %t, and by the machines' rule: %t; want both, since no install of this deployment reaches its plugin", left, machines)
	}
	rig.awaitClaim(claim.RoleTester, "the Oh My Pi worker to be relaunched onto this daemon's plugin",
		func(c supervise.Claim) bool { return c.State == supervise.StateLaunching })

	// The operator moves the implementer onto Oh My Pi, whose plugin a relaunch does install.
	apply(t, live, map[string]any{"worker_backends": map[string]any{"implementer": "omp"}})

	if left, machines := passLeaves(claim.RoleImplementer), machinesLeave(claim.RoleImplementer); left || machines {
		t.Fatalf("after the change the pass left the worker running: %t, and the machines' rule says its launch installs none: %t; want neither, and the two to answer alike whatever they answer", left, machines)
	}
}

// changeOf is the applied change of path, and whether the apply reported one.
func changeOf(applied []config.Change, path string) (config.Change, bool) {
	for _, change := range applied {
		if change.Path == path {
			return change, true
		}
	}
	return config.Change{}, false
}
