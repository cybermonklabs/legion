package api

//ci:reads packages/contracts/fixtures/daemon-api/* TestPauseRouteGoldens (path from goldenPath, declared)

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden fixtures this package pins")

// goldenPath is the cross-language pin: Go writes the wire shape, and
// `packages/contracts/src/legion-api.test.ts` parses it through the zod schema the plugin reads
// with and holds the parse equal to the file, so a Go field with no zod line is named there. Go
// is the source of truth; the fixture is how the schema is held to it.
func goldenPath(name string) string {
	return filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name)
}

// settingsFixtureFile is the maximal kubernetes legion.yaml the state golden and the settings tests
// share: every key the loader models is set but `roles` and `role_types`, which
// `internal/api/settings_test.go` pins, and every value a credential, its source or its file
// could be is a planted fixture a test can look for in the wire bytes (a PEM that is no key, a DSN
// password that opens nothing, `*_file` paths, the kubeconfig, the session DSN secret's key, each
// provider key's store key, the pod's env values, volume, Secret and ServiceAccount names, userinfo
// on `envoy_url` and the first `nats_urls` entry, which the settings mask, and userinfo and a query
// on `plugins.pi_envoy`'s tarball URL, which the settings mask and drop while `pi_legion` pins the
// version form). Every path the settings emit (`state_dir`, `instructions`, the hook's
// `prompt_file`) is a fixed absolute literal, so the golden does not churn with the temporary
// directory the file is written under.
// Hosts are placeholders.
const settingsFixtureFile = `project: LEGION
port: 13370
bind: 0.0.0.0
advertise_host: legion-daemon.internal.example
postgres_dsn: postgres://legion:fixture-dsn-password@postgres.internal.example:5432/legion
state_dir: /var/lib/legion
admission_cap: 4
daemon_url: http://legion-daemon.internal.example:13370
worker_stream_port: 13371
provider_keys: {ANTHROPIC_API_KEY: ANTHROPIC_API_KEY_FIXTURE, GEMINI_API_KEY: GEMINI_API_KEY_FIXTURE}
instructions: /etc/legion/deployment-instructions.md
worker_boot_timeout_seconds: 180
worker_boot_registration_deadline_intervals: 2
worker_rpc_timeout_seconds: 8
worker_stop_timeout_seconds: 30
tree_stop_timeout_seconds: 90
slow_command_timeout_seconds: 600
probe_interval_seconds: 20
controller_wake_interval_seconds: 1800
launch_failure_limit: 4
prompt_failure_limit: 5
prompt_retire_limit: 3
worker_wakes_per_minute: 12
worker_wake_burst: 2
operator_token_file: /var/run/legion/OPERATOR_TOKEN
envoy_url: http://legion:fixture-envoy-password@envoy-listener.internal.example:9020
nats_urls: [nats://legion:fixture-nats-password@nats.internal.example:4222, nats://nats-2.internal.example:4222]
envoy_token_file: /var/run/legion/ENVOY_TOKEN
nats_nkey_seed_file: /var/run/legion/NATS_NKEY_SEED
nats_daemon_nkey_seed_file: /var/run/legion/NATS_DAEMON_NKEY_SEED
dispatch_url: https://dispatch.internal.example
dispatch_token_file: /var/run/legion/DISPATCH_TOKEN
projects:
  LEGION:
    slack_channels: [C0123ABCD, C0123ABCE]
    deploys:
      - {repo: sjawhar/legion, workflow: .github/workflows/release.yaml, step: Apply}
  WIDGETS:
    production_check: off
    default_steps: [implementer, reviewer]
gates:
  design: root-issues
github_apps:
  implement:
    app_id: "1001"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
    installations: {sjawhar: "2001", acme: "2002"}
  review:
    app_id: "1002"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
    installations: {sjawhar: "2003"}
linger_hours: 1.5
production_check_hours: 240
worker_silence_minutes: 45
worker_background_hours: 6
pod_update_max_open_rounds: 6
pod_update_rounds_per_minute: 2
review_round_cap: 4
controller: daemon
worker_backends: {tester: omp}
repository_renames:
  - {from: acme/widgets-legacy, to: acme/widgets}
capabilities:
  decided:
    pool-capacity: "the pool is sized by hand for the admission cap"
hooks:
  - name: back-to-implementing
    on: work_back
    moves: [reviewing-implementing, testing-implementing]
    every: 2
    roles: [implementer]
    prompt_file: /etc/legion/hooks/back-to-implementing.md
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/cybermonklabs/legion-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    storage_class: gp3
    issue_volume: 30Gi
    kubeconfig: /etc/legion/kubeconfig
    context: legion-daemon@fixture-cluster
    session_store: postgres
    session_dsn_secret: SESSION_DSN_FIXTURE
    scheduling:
      node_selector: {node.kubernetes.io/instance-type: m7i.xlarge}
      tolerations:
        - {key: dedicated, operator: Equal, value: legion, effect: NoSchedule}
        - {key: spot, operator: Exists, effect: NoExecute}
      priority_class: legion-workers
    resources:
      issue: {cpu: 4, memory: 24Gi}
      controller: {cpu: 2, cpu_request: 1, memory: 6Gi, memory_request: 4Gi}
    pod:
      env: {PI_CONFIG_FILES: /etc/legion-operator/overlay.yml, OPERATOR_FIXTURE_FLAG: operator-fixture-value}
      service_account: fixture-operator-account
      volumes:
        - name: operator-creds
          secret: {name: fixture-operator-creds}
      volume_mounts:
        - {volume: operator-creds, mount_path: /etc/legion-operator/creds}
    agent_secrets:
      url: https://agent-secrets.internal.example
      token_file: /var/run/secrets/agent-secrets/token
      audience: fixture-broker-audience
      token_expiry_seconds: 1800
      exchange_audience: fixture-exchange-audience
    plugins:
      pi_legion: 10.4.0
      pi_envoy: https://legion:fixture-plugin-password@plugins.internal.example/pi-envoy-10.4.0.tgz?token=fixture-plugin-token
    setup: [/bin/sh, -c, 'echo fixture-setup']
    repository_cache:
      volume: 40Gi
      refresh_minutes: 15
      retain_hours: 48
      resources: {cpu: 500m, memory: 1Gi}
    memory_guard:
      issue_pods:
        enabled: true
        act_percent: 80
        floor_percent: 15
        wait_seconds: 8
        interval_milliseconds: 250
    volume_trim:
      enabled: true
      high_percent: 90
      low_percent: 60
      min_age_minutes: 120
      interval_seconds: 30
`

// minimalSettingsFile is the smallest tmux legion.yaml that loads: every other setting is a
// default, which is what the Stage 3 and operator-claim goldens and the defaults test show.
const minimalSettingsFile = `project: DEMO
postgres_dsn: postgres://legion:fixture-dsn-password@127.0.0.1:5432/legion
state_dir: /var/lib/legion
`

// loadSettingsFile writes body as a legion.yaml under t.TempDir() and loads it as the daemon does,
// reading no environment, so the resolved values are the file's and the defaults alone.
func loadSettingsFile(t *testing.T, body string) config.Config {
	t.Helper()
	return loadSettingsFileWith(t, body, nil)
}

// loadSettingsFileWith is loadSettingsFile with each of files, by name, written first in the
// directory of the legion.yaml, where a relative path the file names resolves.
func loadSettingsFileWith(t *testing.T, body string, files map[string]string) config.Config {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	path := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	cfg, err := config.Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// settingsFixtureConfig is settingsFixtureFile loaded.
func settingsFixtureConfig(t *testing.T) config.Config {
	t.Helper()
	return loadSettingsFile(t, settingsFixtureFile)
}

// minimalSettingsConfig is minimalSettingsFile loaded.
func minimalSettingsConfig(t *testing.T) config.Config {
	t.Helper()
	return loadSettingsFile(t, minimalSettingsFile)
}

// populatedState carries every optional the state shape has, so the fixture pins the full shape
// rather than Stage 1's empty record.
func populatedState(t *testing.T) State {
	t.Helper()
	approvedVersion, mergedApproval, lineApproval := 6, 4, 3
	reviewApproval, stuckApproval := 5, 2
	// The specs of the two step roots, each approved at the version Dispatch holds.
	stepApproval, heldStepApproval := 2, 1
	waitedSince := time.Date(2026, 9, 22, 8, 5, 0, 0, time.UTC)
	answeredAt := time.Date(2026, 9, 22, 9, 21, 0, 0, time.UTC)
	// When LEGION-210's implementer failed, the moment its claim carries beside the cause.
	implementerFailedAt := time.Date(2026, 9, 22, 8, 47, 3, 0, time.UTC)
	// When LEGION-207's linger runs out: its work finished and its tree lingers until then.
	lingerUntil := time.Date(2026, 9, 22, 10, 10, 12, 0, time.UTC)
	// When the controller paused the project, nine minutes before the state was read.
	pausedSince := time.Date(2026, 9, 22, 9, 9, 0, 0, time.UTC)
	// The two flags the line's members write: LEGION-212 kept the slot it took when the daemon
	// started it and waits for no other (record.KeepsSlot), and LEGION-211's tree has not been
	// opened, so it holds none and queues for one. A member that holds none writes `holdsSlot`
	// false rather than leaving it out, which is what the pointer behind that field is for.
	heldSlot, noSlot := true, false
	// The two line members' own Dispatch priorities: LEGION-212 is P0 and LEGION-211 is P1, each
	// its own tree root. Every other recorded issue has none of its own, and each member's line
	// priority is the one record.PriorityOf gives it (both set at the end of this function).
	p0, p1, noPriority := PriorityView(record.P0), PriorityView(record.P1), PriorityView(record.NoPriority)
	// LEGION-208's architect's handshake, which is the pair of times a tree's own root carries in
	// every phase it stands in (internal/projection): the task of the architect's last entry was
	// queued to its claim, and a turn of it confirmed that task.
	entryQueued := time.Date(2026, 9, 22, 8, 58, 14, 0, time.UTC)
	entryConfirmed := time.Date(2026, 9, 22, 8, 58, 51, 0, time.UTC)
	// LEGION-207's one open production check, which the issue shows under `check` and `checks`.
	goldenCheck := CheckView{
		Repo:        "acme/widgets",
		PullRequest: 1239,
		MergeCommit: "3e9c0a7b5d2f4e8a1c6b9d0f2a4e7c5b8d1f3a60",
		OpenedAt:    time.Date(2026, 9, 22, 8, 40, 12, 0, time.UTC),
		Role:        string(claim.RoleImplementer),
		Looks:       2,
		LookOpen:    true,
		LastWake:    &CheckWakeView{Kind: "issue.deployed", Detail: "run 38230245571 completed 2026-09-22T09:10:44Z", At: time.Date(2026, 9, 22, 9, 10, 51, 0, time.UTC)},
		NextWakeAt:  time.Date(2026, 10, 6, 8, 40, 12, 0, time.UTC),
	}
	// What the two reviewing issues' rounds wait on from GitHub (api.MergeWaitView). LEGION-218's
	// head went up minutes ago and no read of the pull request has answered for it yet, so all
	// the daemon knows of its age is the `updated_at` GitHub gave the pull request itself;
	// LEGION-219's head has stood two days, and every read since the first after its approval has
	// answered `blocked`, the last of them a minute before this state.
	waitingHeadUpdatedAt := time.Date(2026, 9, 22, 8, 58, 40, 0, time.UTC)
	stuckHeadUpdatedAt := time.Date(2026, 9, 20, 14, 5, 33, 0, time.UTC)
	stuckStateReadAt := time.Date(2026, 9, 22, 9, 16, 12, 0, time.UTC)
	// The movements of those two roots (Issue.MovedAt, which the quiet time reads below). Each
	// reviewer completed its round after the head it approved went up, and a completion is a
	// movement (workflow's handoff completion marks the issue). LEGION-218 has had nothing since.
	// LEGION-219's first read after the approval took its merge state from unread to `blocked`,
	// which is news of the pull request and a movement too (workflow's pullRequestNews); every
	// read after it confirmed `blocked` and moved only the read stamp, and telling the architect
	// and the reviewer of the stuck round writes a notice and the told marks, neither of which
	// is a movement.
	waitingApprovedAt := time.Date(2026, 9, 22, 9, 7, 21, 0, time.UTC)
	stuckApprovedAt := time.Date(2026, 9, 20, 16, 42, 10, 0, time.UTC)
	stuckFirstBlockedAt := time.Date(2026, 9, 20, 16, 42, 58, 0, time.UTC)
	// What the daemon last saw two of the fixture's agents do (supervise.StepRecord): the
	// architect's last call ended, so it is in none, and the implementer's `bash` call has a
	// start and no end, which is the shape a stalled turn leaves behind. Neither carries the
	// arguments' text, only their length and the eight-byte fingerprint the daemon keeps.
	architectStep := supervise.StepRecord{
		At:            time.Date(2026, 9, 22, 9, 16, 58, 0, time.UTC),
		TurnStartedAt: time.Date(2026, 9, 22, 9, 16, 40, 0, time.UTC),
		Tool:          "read",
		CallID:        "toolu_arch_3",
		ArgsBytes:     74,
		ArgsHash:      "3f7c1a9d62b40e58",
		StartedAt:     time.Date(2026, 9, 22, 9, 16, 55, 0, time.UTC),
		EndedAt:       time.Date(2026, 9, 22, 9, 16, 58, 0, time.UTC),
	}
	implementerStep := supervise.StepRecord{
		At:            time.Date(2026, 9, 22, 9, 17, 12, 0, time.UTC),
		TurnStartedAt: time.Date(2026, 9, 22, 9, 16, 30, 0, time.UTC),
		Tool:          "bash",
		CallID:        "toolu_impl_11",
		ArgsBytes:     128,
		ArgsHash:      "9b2e45c0d1f8a673",
		StartedAt:     time.Date(2026, 9, 22, 9, 17, 12, 0, time.UTC),
	}
	// What the daemon last saw of the architect's turns and of its launches (contract 86): the
	// turn it began and ended, the ready, the cause its process was last found gone under, and the
	// failure of a start since that ready, with the time the daemon recorded each of the two
	// causes (causeTimesContract): LastDeathAt dates the death, before the ready, so the running
	// agent survived it. One claim view of the fixture carries all seven, so every record
	// describes each key.
	architectTurnStart := time.Date(2026, 9, 22, 9, 16, 40, 0, time.UTC)
	architectTurnEnd := time.Date(2026, 9, 22, 9, 16, 58, 0, time.UTC)
	architectReady := time.Date(2026, 9, 22, 9, 15, 44, 0, time.UTC)
	architectDied := time.Date(2026, 9, 22, 9, 15, 21, 0, time.UTC)
	architectStartFailed := time.Date(2026, 9, 22, 9, 15, 50, 0, time.UTC)
	state := State{
		Daemon: DaemonInfo{
			Project:       "legion",
			SchemaVersion: 1,
			Boots:         4,
			FirstBootAt:   time.Date(2026, 9, 18, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 22, 9, 15, 2, 0, time.UTC),
		},
		Admission: Admission{
			// The state lists `active` by slot index (record.Store.Slots), so each issue's `slot`
			// below is its place in it, 0 to 6: the lowest index free at the pass that took it
			// (admit's nextSlotIndex), at a moment before anything its own record says it did since.
			Cap:    7,
			Active: []string{"LEGION-208", "LEGION-210", "LEGION-212", "LEGION-218", "LEGION-219", "LEGION-240", "LEGION-241"},
			// The line is two long and carries one member of each of its two parts, in the order
			// the line publishes them: LEGION-212, the root a person answered, stands at 1 in the
			// part whose work came back, which takes the free slots first, and LEGION-211, a root
			// another tree handed over whose own tree has not been opened, stands at 2 in the part
			// that has not started. Each of the two carries its own place and its own part's
			// fields (`waitingLine`). LEGION-212 is in `active` as well, which is the shape of
			// every member that already holds a slot: it kept the one it took when the daemon
			// started it (record.KeepsSlot), so its `holdsSlot` is true and its sentence ends in
			// the holder's clause rather than in one about queuing for a free slot. LEGION-218 and
			// LEGION-219 are the two reviewing roots whose round waits on GitHub's merge state, and
			// each holds a slot of its own, which the cap counts. LEGION-240 holds one at step, and
			// LEGION-241, held from step when its checker's claim failed, keeps the one that step
			// held, since the slot rule reads where a hold is from (record.KeepsSlot, StandsAt), as
			// LEGION-210, held from implementing, keeps its own.
			Waiting: []string{"LEGION-212", "LEGION-211"},
			// The roots of the live trees, which the slots no longer name: LEGION-207 closed,
			// LEGION-214 is held with its Legion label off and so stands in no live tree, and every
			// other root of the fixture is a tree of its own, each named below, LEGION-240 and
			// LEGION-241 among them.
			Trees: []string{"LEGION-208", "LEGION-210", "LEGION-211", "LEGION-212", "LEGION-218", "LEGION-219", "LEGION-240", "LEGION-241"},
			// LEGION-211 is the waiting root admission skips, so it is in `waiting` too;
			// LEGION-209 is a child of LEGION-208's tree the workflow holds at `admitted`, which
			// no waiting line carries. Both are listed, by issue key, with what each waits on —
			// LEGION-213 among them a key the daemon has no record of, which counts as unfinished.
			Blocked: []BlockedIssue{
				{Issue: "LEGION-209", BlockedBy: []string{"LEGION-210", "LEGION-213"}},
				{Issue: "LEGION-211", BlockedBy: []string{"LEGION-210"}},
			},
			// The issues the record holds in the workflow that stand in neither list above, each
			// with the one word for why. Both shapes of the row are here, and that is what the
			// fixture is for: LEGION-209 is held by the issues `blocked` names, so its row
			// carries `heldBy`, and LEGION-214, a root of its own tree whose record says the
			// label is off (`label_off`), is held by no issue at all, so its row carries none. The contracts
			// gate walks the keys the file holds (packages/contracts/src/legion-api.test.ts), so
			// a list of one shape would prove nothing of the other — an absent `heldBy` is as
			// much part of the row's schema as a filled one.
			Held: []HeldIssue{
				{Issue: "LEGION-209", Reason: record.HeldDispatchBlocker, HeldBy: []string{"LEGION-210", "LEGION-213"}},
				{Issue: "LEGION-214", Reason: record.HeldLabelOff},
			},
		},
		Issues: map[string]Issue{
			"LEGION-208": {
				Key:           "LEGION-208",
				Generation:    3,
				Phase:         phase.Implementing,
				Status:        "in_progress",
				Tree:          "LEGION-208",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Architect: &ClaimView{
					Session: "ses_architect_208",
					State:   "ready",
					Locator: &runtime.Locator{
						Runtime:     runtime.RuntimeTmux,
						Claim:       "legion-legion-legion-208-architect",
						Incarnation: "31881:9442101",
						Tmux:        &runtime.TmuxLocator{Window: "@4", Pane: "%12"},
					},
					// No round is open over this claim, and the pacing holds a catch-up for it:
					// the notice a child's re-admission wrote it in place of a launch, which the
					// state names under `wakesWaiting` too.
					UpdateState: UpdateStateNone,
					Step:        StepViewOf(architectStep),
					Wake:        &WakeView{Kind: supervise.WakeCatchUp, DueAt: time.Date(2026, 9, 22, 9, 18, 3, 0, time.UTC)},
					// The turns this agent began and ended, its ready, and the two causes with
					// the time each was recorded, LastDeathAt dating the death (contracts 86 and
					// 256). This view carries every key so each record describes it: a live claim
					// carries a start failure only while its agent has not been ready since, which
					// a ready claim never is.
					LastTurnStart:      &architectTurnStart,
					LastTurnEnd:        &architectTurnEnd,
					LastReady:          &architectReady,
					LastDeathCause:     "the pane's process was gone at the daemon's read",
					LastDeathAt:        &architectDied,
					LastStartFailure:   "the pod was not ready within the worker boot timeout",
					LastStartFailureAt: &architectStartFailed,
				},
				Workers: map[claim.Role]PhaseView{
					claim.RolePlanner: {
						Claim: ClaimView{
							Session: "ses_planner_208",
							State:   "suspended",
							Locator: &runtime.Locator{
								Runtime:     runtime.RuntimeSandbox,
								Claim:       "legion-legion-legion-208-planner",
								Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/2",
								Sandbox: &runtime.SandboxLocator{
									Namespace: "legion", Name: "legion-legion-legion-208", PodUID: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
									Container: "roles", Generation: 2,
								},
							},
							// The pod's update round, which every claim of that pod shares: the round
							// asks each role, and the daemon replaces the pod once every asked role has
							// answered, no turn runs and no claim's activity report holds it
							// (internal/daemon's roundEnd.holding), or at the round's bound
							// (pod_update_grace_minutes) whatever the roles answered, an operator's
							// suspension excepted. This one has answered, and reported the head it
							// pushed. The reasons are the round's own, so every claim of the pod
							// carries the same text: what the worker answering is told to act on.
							UpdateRound:   "pod:7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
							UpdateHead:    "4c1b8e2a6f9d3047b5e1c8a2f6d940b3e7c5a128",
							UpdateReasons: "the pod template changed: image ghcr.io/cybermonklabs/legion-worker moved",
							// The worker answered with the ready_for_update call. Its launch onto
							// the pod's replacement waits in the pacing meanwhile.
							UpdateState: UpdateStateAnswered,
							Wake:        &WakeView{Kind: supervise.WakeLaunch, DueAt: time.Date(2026, 9, 22, 9, 18, 11, 0, time.UTC)},
						},
						HandoffRecord: &HandoffRecordView{ID: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", Version: 1},
						Pass:          0,
						Rounds:        0,
					},
					claim.RoleImplementer: {
						Claim: ClaimView{
							Session: "ses_implementer_208",
							State:   "working",
							Locator: &runtime.Locator{
								Runtime:     runtime.RuntimeSandbox,
								Claim:       "legion-legion-legion-208-implementer",
								Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5",
								Sandbox: &runtime.SandboxLocator{
									Namespace: "legion", Name: "legion-legion-legion-208", PodUID: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
									Container: "roles", Generation: 5,
								},
							},
							// The session's capability report (LEGION-663), from the process the locator
							// names: two rows it proved and one it did not, with the fact its check found.
							Capabilities: &CapabilityReportView{
								MeasuredAt:  time.Date(2026, 9, 22, 9, 17, 3, 0, time.UTC),
								Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5",
								OK:          []string{"subagents", "dispatch-envoy-tools"},
								Open:        []CapabilityGap{{Name: "github", Detail: "gh api user: HTTP 401: Bad credentials"}},
							},
							// The same round, on the worker that has not answered it yet: no head is
							// reported until a worker answers, and the reasons are the round's own.
							// The round's ask reached a session of this worker's role.
							UpdateRound:   "pod:7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
							UpdateReasons: "the pod template changed: image ghcr.io/cybermonklabs/legion-worker moved",
							UpdateState:   UpdateStateAsked,
							// The run of the last confirmed delivery this claim retired, which is
							// this issue's own generation: the worker is turning over the run the
							// issue is in.
							ServingGeneration: 3,
							// Two turns of this claim ended in a provider error in a row, and the
							// task went out again behind the daemon's own sentence. It bounds
							// nothing: the controller is told once at six, and nothing fails a
							// claim for it.
							ProviderErrorRun: 2,
							// The task this worker holds, as every agent surface reads it: that one
							// is held, the phase it was queued for and when, and nothing of its
							// text, which the operator routes serve whole.
							PendingTask: &PendingTaskView{Phase: phase.Implementing, QueuedAt: time.Date(2026, 9, 22, 9, 16, 58, 0, time.UTC)},
							// The call this worker is in: a start the daemon received and no end,
							// which is what a turn that has stopped moving leaves behind.
							Step: StepViewOf(implementerStep),
						},
						HandoffCommit: "",
						// Three passes through the stretch, two of them ended by a review that
						// requested changes: the third went back on something else, a tester's fail
						// or a red, which the review rounds do not count.
						Pass:   3,
						Rounds: 2,
						// Both of this issue's rounds came from a move back, and the two were of
						// different kinds, so the counts name two pairs and add up to the rounds.
						// LEGION-207's implementer below carries none: an issue whose rounds were
						// run before the counting landed reads no such key at all.
						BackwardMoves: map[string]int{"reviewing-implementing": 1, "testing-implementing": 1},
					},
				},
				// The implementer holds the work its phases row names: the two numbers are equal,
				// which is every role the daemon has reached, as LEGION-218's and LEGION-219's
				// reviewers are too; LEGION-211 below is the other case. The two times beside them
				// are not the implementer's: a tree's own root carries its architect's handshake in
				// every phase it stands in (internal/projection), so they say when the task of the
				// architect's last entry was queued to its claim and when a turn of it confirmed it.
				// LEGION-210, LEGION-212 and LEGION-207 carry none of the four. The projection writes
				// neither number because no role works held, awaiting_person or done (phase.RoleFor).
				// It writes neither time because the times are an architect claim's, which it shows
				// as the issue's `architect`, and none of the three carries one. The fixture gives an
				// architect claim to LEGION-208 alone: LEGION-210 and LEGION-211 hold none in the
				// record they stand for, as their own comments say, and any architect claim another
				// root would hold is left out on purpose, since a second architect claim would add no
				// key this file does not already hold. That is also why LEGION-211 and the two
				// reviewing roots carry no times.
				PhaseEntry:       4,
				GivenEntry:       4,
				EntryQueuedAt:    &entryQueued,
				EntryConfirmedAt: &entryConfirmed,
				PullRequest: &PullRequestView{
					Repo:   "acme/widgets",
					Number: 1247,
					Head:   "5b3d9e2c8f1a47d06b2e5c9f3a8d1e4b7c0f6a29",
					State:  "open",
					// GitHub's own merge state for the head as the daemon's last read found it
					// (contract 86): a review the repository requires holds this one.
					MergeState:     "blocked",
					ReviewDecision: "changes_requested",
				},
				DesignGate: &GateView{
					ArtifactID:      "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
					CurrentVersion:  7,
					ApprovedVersion: &approvedVersion,
				},
				// The fixture's deployment gates `root-issues`, which resolves to the one step
				// spec, so each root carries one entry: this one's document stands a version above
				// the version its person approved, which is the `waiting` status.
				GatedSteps: []GatedStepView{{
					Step:            record.DesignStepSpec,
					ArtifactID:      "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
					CurrentVersion:  7,
					ApprovedVersion: &approvedVersion,
					Status:          GatedStepWaiting,
				}},
				Slot: &SlotView{
					Index:      0,
					AdmittedAt: time.Date(2026, 9, 21, 18, 42, 11, 0, time.UTC),
				},
				// The tasks of this root's plan, each with every key a task can carry between
				// them (internal/projection's taskView). a is the task that merges: its owner
				// stopped it to add the tasks it now needs, so it keeps its place and its pull
				// request, which is the issue's own, the merge's. b's reviewer approved b's own
				// pull request, whose round waits on GitHub. c's step waits for its agent k, which
				// runs d's first step. e is held, its owner having escalated a failed claim. The
				// issue stands at implementing, the first phase of a running task in today's order.
				Tasks: []TaskView{
					{ID: "a", State: string(record.TaskStopped), Needs: []string{"b", "c", "d", "e"}, Kind: record.PlaceStep,
						Phase: string(phase.Implementing), Branch: "legion/LEGION-208", Position: 1, Role: string(claim.RoleImplementer),
						Agent: "implementer", StoppedReason: "its owner stopped it to add the tasks it needs",
						PullRequest: &PullRequestView{Repo: "acme/widgets", Number: 1247, Head: "5b3d9e2c8f1a47d06b2e5c9f3a8d1e4b7c0f6a29",
							State: "open", MergeState: "blocked", ReviewDecision: "changes_requested"},
						Counts: TaskCountsView{Pass: 3, Rounds: 2}},
					{ID: "b", State: string(record.TaskRunning), Needs: []string{}, Kind: record.PlaceStep, Phase: string(phase.Reviewing),
						Branch: "legion/LEGION-208.b", Position: 2, Role: string(claim.RoleReviewer), Agent: "reviewer",
						PullRequest: &PullRequestView{Repo: "acme/widgets", Number: 1251, Head: "8e1f4c7a2d9b63e05f1a8c4d7b2e9f6a3c0d5b18",
							State: "open", MergeState: "blocked", ReviewDecision: "approved"},
						MergeWait: &MergeWaitView{WaitsOn: "8e1f4c7a2d9b63e05f1a8c4d7b2e9f6a3c0d5b18", MergeState: "blocked",
							StateReadAt: &stuckStateReadAt, HeadUpdatedAt: &waitingHeadUpdatedAt, CheckRuns: 2},
						Counts: TaskCountsView{Pass: 1}},
					{ID: "c", State: string(record.TaskRunning), Needs: []string{}, Kind: record.PlaceStep, Phase: string(phase.Implementing),
						Branch: "legion/LEGION-208.c", Position: 1, Role: string(claim.RoleImplementer), Agent: "k",
						Wait: string(record.StepWaitAgent), WaitsFor: &AgentStepView{Agent: "k", Task: "d", Position: 1}},
					{ID: "d", State: string(record.TaskRunning), Needs: []string{}, Kind: record.PlaceStep, Phase: string(phase.Implementing),
						Branch: "legion/LEGION-208.d", Position: 1, Role: string(claim.RoleImplementer), Agent: "k",
						Counts: TaskCountsView{Pass: 1}},
					{ID: "e", State: string(record.TaskHeld), Needs: []string{}, Kind: record.PlaceStep, Phase: string(phase.Implementing),
						Branch: "legion/LEGION-208.e", Position: 1, Role: string(claim.RoleImplementer), Agent: "implementer-2",
						HoldReason: string(record.HoldEscalated), Counts: TaskCountsView{Pass: 1}},
				},
			},
			"LEGION-210": {
				Key:           "LEGION-210",
				Generation:    1,
				Phase:         phase.Held,
				Status:        "in_progress",
				Tree:          "LEGION-210",
				Repo:          "zeta/tools",
				DefaultBranch: "release/v7.1",
				HoldReason:    "escalated",
				// The phase the hold took this root out of, which its release puts it back to. It
				// stands beside the reason here; the two part on a tree that lingers, where the
				// reason is withheld and this is not (internal/projection).
				HeldFrom: string(phase.Implementing),
				Workers: map[claim.Role]PhaseView{
					// Its implementer failed and nothing has launched it since, so the claim
					// carries why: the one word the supervision chose, the sentence it wrote, the
					// moment, and the four counts it failed with, which a reader reads against
					// the limits in `settings`. The pending cause beside it names the death whose
					// refused relaunches spent that budget, and stands until the agent's ready or
					// a retry clears it with the launch failures it is counted beside. Beside them
					// stands the sentence a launch try was refused with because the shared public
					// leak check could not be installed in the pod, which this fixture carries with
					// the other optional keys of the view at once, as operatorClaim does.
					claim.RoleImplementer: {Claim: ClaimView{
						Session:     "ses_implementer_210",
						State:       "failed",
						UpdateState: UpdateStateNone,
						Failure: &ClaimFailureView{
							Cause:    supervise.CauseDied,
							Detail:   "launch failures ran out",
							At:       &implementerFailedAt,
							Attempts: BudgetsView{LaunchFailures: 4, Deaths: 1},
						},
						PendingCause:   supervise.CauseDied,
						LeakCheckCause: "the shared public leak check could not be installed: GitHub answered the read of example-org/deployment at leak-check-v3 with HTTP 404: Not Found",
					}, Pass: 1, Rounds: 1},
				},
				// Nobody has registered this root's spec, so its one gated step carries no
				// document, no version and no approval, and holds the tree's work.
				GatedSteps: []GatedStepView{{Step: record.DesignStepSpec, Status: GatedStepUnregistered}},
				Slot: &SlotView{
					Index:      1,
					AdmittedAt: time.Date(2026, 9, 21, 19, 5, 26, 0, time.UTC),
				},
				// Its implementer failed while implementing and nothing will wake that claim, so
				// the issue's one rendered answer is the stopped claim rather than a queue
				// (`why`, api.WhyClaimStopped).
				Why: &WhyView{
					State:  WhyClaimStopped,
					Reason: "Its phase is held and the implementer's claim is stranded at failed, so nothing will wake it.",
				},
				// No architect claim of this root stands in the record, so the view carries no
				// `architect`, and the work of its architect's entry has waited past the bound with
				// no start of one queued: the daemon's reading of the second arm holds for it, and the
				// state serves that arm whether or not the controller was told of it
				// (admit.Admission.ArchitectStallArms).
				ArchitectStalls: []record.ArchitectStallArm{record.ArchitectClaimUnreachable},
			},
			// LEGION-211 is a root another tree handed over, carrying the phase it ran there and
			// that run's suspended implementer, whose own tree has not been opened: it holds no
			// slot row, and its promotion opens that tree. So it waits in the second part of the
			// line, the issues that have not started, behind LEGION-212, whose work came back.
			// `admission.blocked` names what it waits on besides the slot.
			"LEGION-211": {
				Key:           "LEGION-211",
				Generation:    2,
				Phase:         phase.Implementing,
				Status:        "in_progress",
				Tree:          "LEGION-211",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Workers: map[claim.Role]PhaseView{
					// A released claim keeps the task it was holding through its suspension, and
					// the two sends that never reached a turn are on it: the diagnosis record a
					// stalled claim is read by, with no budget spent on either.
					claim.RoleImplementer: {Claim: ClaimView{
						Session:     "ses_implementer_211",
						State:       "suspended",
						UpdateState: UpdateStateNone,
						PendingTask: &PendingTaskView{
							Phase: phase.Implementing, QueuedAt: time.Date(2026, 9, 22, 8, 54, 20, 0, time.UTC),
							SendFailures: 2, LastSendError: "post the prompt: dial tcp 10.42.0.17:13371: connect: connection refused",
						},
					}, Rounds: 1},
				},
				// The Dispatch parent chain this labelled root was recorded with: LEGION-209 is a
				// child of LEGION-208's live tree that its architect has not released, so the
				// ancestry hold is what keeps this root out of the line's front while that tree
				// runs. Nothing else on this wire names the chain.
				Ancestry: []string{"LEGION-209", "LEGION-208"},
				// Its implementer was given the work of entry 1 and the transition that sent its
				// work back raised the row to 2, so the two numbers differ: this is the role the
				// daemon has still to send that entry's work to. It carries neither time, since the
				// times are its architect's handshake and its architect has no claim yet: its own
				// tree has not been opened, and the promotion that opens it is what starts that
				// architect (workflow.StartArchitect).
				PhaseEntry: 2,
				GivenEntry: 1,
				DesignGate: &GateView{
					ArtifactID:      "e5a2c1d7-3b46-4f82-9e07-2d4a6c8b1f53",
					CurrentVersion:  3,
					ApprovedVersion: &lineApproval,
				},
				// Its spec stands approved at the version Dispatch holds, which is how its work
				// began: what holds it now is the blocker, not the gate.
				GatedSteps: []GatedStepView{{
					Step:            record.DesignStepSpec,
					ArtifactID:      "e5a2c1d7-3b46-4f82-9e07-2d4a6c8b1f53",
					CurrentVersion:  3,
					ApprovedVersion: &lineApproval,
					Status:          GatedStepApproved,
				}},
				// The line's last member, P1 of its own and so behind LEGION-212's P0 band. Within
				// its band it is the only member of the part that has not started, and its band has
				// no member whose work came back, so its sentence names nothing ahead of it. It
				// carries the ranks that placed it within its band and no `since` at all, since its
				// work never started.
				Priority: &p1,
				WaitingLine: &WaitingLineView{
					Place: 2, Length: 2,
					Part: record.PartStarting.String(), Kind: record.LineQueuedRoot.String(),
					PartPlace: 1, PartLength: 1, HoldsSlot: &noSlot,
					RootRank: "00011", OwnRank: "00011", RootKey: "LEGION-211",
					Reason: "It has not started. It waits at P1, its own priority. " +
						"It is 1st of 1 issue at P1 that has not started, by its own Dispatch rank.",
				},
				// A release suspended its implementer and the wake never brought that pod back,
				// and a stopped claim is read before the line is: the one answer it carries is
				// that claim rather than its place in the queue.
				Why: &WhyView{
					State:  WhyClaimStopped,
					Reason: "Its phase is implementing and the implementer's claim is stranded at suspended, so nothing will wake it.",
				},
			},
			// LEGION-212's architect asked a person a question and handed the turn back, and the
			// person has answered it: the root keeps the slot it took when the daemon started it
			// (record.KeepsSlot) and stands in the line as well, where its promotion resumes the
			// same architect and takes no second slot. That architect's claim is left out of the
			// fixture on purpose, as every root's but LEGION-208's is (the comment on LEGION-208's
			// entry numbers says why), so this root carries no `architect` and no entry times.
			"LEGION-212": {
				Key:           "LEGION-212",
				Generation:    1,
				Phase:         phase.AwaitingPerson,
				Status:        "in_progress",
				Tree:          "LEGION-212",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Workers:       map[claim.Role]PhaseView{},
				// The line's first member, P0 of its own and the only member of the P0 band: its
				// place, its place within its band's returned part, the moment its work came back
				// and no rank at all, since the returned part is ordered by that moment. It holds a
				// slot, so its sentence ends in the holder's clause: telling such a member that it
				// queues for a free slot is the fault this whole answer exists to stop.
				Priority: &p0,
				WaitingLine: &WaitingLineView{
					Place: 1, Length: 2,
					Part: record.PartReturned.String(), Kind: record.LineAnswered.String(),
					PartPlace: 1, PartLength: 1, HoldsSlot: &heldSlot, Since: &answeredAt,
					Reason: "A person answered it, and its work came back at 2026-09-22T09:21:00Z. " +
						"It waits at P0, its own priority. It is 1st of 1 issue at P0 whose work came back. " +
						"It already holds its admission slot, so it resumes at the next pass whatever the cap says.",
				},
				// A tester of its tree retired while the issue stood in its phase, and the
				// stopped claim is read before the line: that is the one answer it carries.
				Why: &WhyView{
					State:  WhyClaimStopped,
					Reason: "Its phase is awaiting_person and the tester's claim is stranded at retired, so nothing will wake it.",
				},
				WaitingOn: &WaitingOnView{
					Kind:       "ask",
					Ask:        "9a8fd348-55d5-4d72-a793-3cd037f75f13",
					Since:      &waitedSince,
					AnsweredAt: &answeredAt,
					AnsweredBy: record.AnsweredByAsk,
				},
				DesignGate: &GateView{
					ArtifactID:     "b71f0a94-6c3e-4d28-85af-19c7d0e3b462",
					CurrentVersion: 2,
				},
				// Its spec's reviewer asked for changes at the version Dispatch holds, which the
				// architect was told of: the one status that carries the reviewer's reason.
				GatedSteps: []GatedStepView{{
					Step:           record.DesignStepSpec,
					ArtifactID:     "b71f0a94-6c3e-4d28-85af-19c7d0e3b462",
					CurrentVersion: 2,
					Status:         GatedStepChangesRequested,
					ChangesReason:  "name the rollback path for the schema change",
				}},
				Slot: &SlotView{
					Index:      2,
					AdmittedAt: time.Date(2026, 9, 22, 8, 1, 30, 0, time.UTC),
				},
			},
			// LEGION-214 is the fixture's one held row that names no issue holding it, which is
			// what it is here for: the contracts gate walks the keys the file holds, so a held list
			// of one shape would prove nothing of a row with no `heldBy`. It is a root of its own
			// tree at admitted whose record says the Legion label is off, so the admission queue
			// does not carry it, it stands in neither `active` nor `waiting`, and its word is
			// `label_off`, which record.HeldReasonOf answers for that shape (measured against the
			// ladder, not assumed). Its tree is no live tree and no draw reached it, so it carries
			// no memory group.
			// LEGION-240 stands at step: its task runs the step of checker, a role the deployment
			// defines, which the phase alone does not name (`stepRole`). It holds a slot and is at
			// work (deploymentRolesContract), but its pod was built before the deployment defined
			// the checker, so the checker's start waits for that pod's update round or its release
			// (record.Store.StartsWaitingForPod): the issue names the checker in `startWaitsForPod`
			// and no claim of the checker exists yet, so it carries no worker.
			// LEGION-241 is held from that step, its checker's claim failed, so its hold names the
			// phase it left and the role of that step, and the failed claim stays its worker, keyed
			// by the role of the deployment, as LEGION-210's failed implementer stays its own.
			"LEGION-240": {
				Key:              "LEGION-240",
				Generation:       1,
				Phase:            phase.Step,
				StepRole:         "checker",
				Status:           "in_progress",
				Tree:             "LEGION-240",
				Repo:             "acme/widgets",
				DefaultBranch:    "main",
				Workers:          map[claim.Role]PhaseView{},
				StartWaitsForPod: []string{"checker"},
				// Its work began once a person approved its spec, which stands approved at the
				// version Dispatch holds: the one step `root-issues` gates, served twice.
				DesignGate: &GateView{
					ArtifactID:      "4b8e2f61-7a3c-4d95-b0e6-1c9f5a2d8e74",
					CurrentVersion:  2,
					ApprovedVersion: &stepApproval,
				},
				GatedSteps: []GatedStepView{{
					Step:            record.DesignStepSpec,
					ArtifactID:      "4b8e2f61-7a3c-4d95-b0e6-1c9f5a2d8e74",
					CurrentVersion:  2,
					ApprovedVersion: &stepApproval,
					Status:          GatedStepApproved,
				}},
				Slot: &SlotView{
					Index:      5,
					AdmittedAt: time.Date(2026, 9, 22, 8, 36, 5, 0, time.UTC),
				},
			},
			"LEGION-241": {
				Key:           "LEGION-241",
				Generation:    1,
				Phase:         phase.Held,
				HeldFrom:      string(phase.Step),
				StepRole:      "checker",
				Status:        "in_progress",
				Tree:          "LEGION-241",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Workers: map[claim.Role]PhaseView{
					"checker": {Claim: ClaimView{Session: "ses_checker_241", State: "failed"}},
				},
				// Its spec stands approved at the version Dispatch holds, as LEGION-240's does: the
				// hold is its checker's failed claim, not the gate.
				DesignGate: &GateView{
					ArtifactID:      "9d3a6c18-2e7f-4b50-8c41-6f0b3e9a5d27",
					CurrentVersion:  1,
					ApprovedVersion: &heldStepApproval,
				},
				GatedSteps: []GatedStepView{{
					Step:            record.DesignStepSpec,
					ArtifactID:      "9d3a6c18-2e7f-4b50-8c41-6f0b3e9a5d27",
					CurrentVersion:  1,
					ApprovedVersion: &heldStepApproval,
					Status:          GatedStepApproved,
				}},
				Slot: &SlotView{
					Index:      6,
					AdmittedAt: time.Date(2026, 9, 22, 8, 39, 47, 0, time.UTC),
				},
				// Its checker failed at step and nothing will wake that claim, so its one rendered
				// answer is the stopped claim, as LEGION-210's is (`why`, api.WhyClaimStopped).
				Why: &WhyView{
					State:  WhyClaimStopped,
					Reason: "Its phase is held and the checker's claim is stranded at failed, so nothing will wake it.",
				},
			},
			"LEGION-214": {
				Key:           "LEGION-214",
				Generation:    1,
				Phase:         phase.Admitted,
				Status:        "todo",
				Tree:          "LEGION-214",
				Repo:          "zeta/tools",
				DefaultBranch: "release/v7.1",
				Workers:       map[claim.Role]PhaseView{},
				// Nothing hands it to the daemon, so no architect of it has registered its spec:
				// its one gated step carries no document, as LEGION-210's does.
				GatedSteps: []GatedStepView{{Step: record.DesignStepSpec, Status: GatedStepUnregistered}},
				// It stands in none of `active`, `waiting` and `blocked` and no stranded claim names
				// it, so its one rendered answer is `unlisted`, with the sentence of its held word,
				// `label_off` (`why`, api.WhyUnlisted).
				Why: &WhyView{
					State:  WhyUnlisted,
					Reason: "It is the root of its own tree and its record says the legion label is off, so nothing hands it to the daemon.",
				},
			},
			// LEGION-207 merged and closed; its production check opened before this release, so the
			// migration left the implementer on it (20261105231916_production_check_role_and_wake),
			// and it is open at its second look, woken by the Dispatch issue.deployed that release
			// still read, with that implementer working the look in the kept pod.
			"LEGION-207": {
				Key:           "LEGION-207",
				Generation:    2,
				Phase:         phase.Done,
				Status:        "done",
				Tree:          "LEGION-207",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				// Its work finished, so its tree lingers: until this moment a todo on it takes the
				// tree back without a re-admission. It is what a reader of a held issue of this
				// tree would read an absent `holdReason` by, the suppression being the linger's.
				LingerUntil: &lingerUntil,
				Workers: map[claim.Role]PhaseView{
					// The pacing holds this claim's relaunch onto this daemon's plugin, which the
					// stale-plugin lane's own gate admitted for it: a wake stands over a claim no
					// round has asked yet, which reads `none` with its wake beside it.
					claim.RoleImplementer: {Claim: ClaimView{
						Session: "ses_implementer_207", State: "working", UpdateState: UpdateStateNone,
						Wake: &WakeView{Kind: supervise.WakePluginRefresh, DueAt: time.Date(2026, 9, 22, 9, 17, 56, 0, time.UTC)},
					}, HandoffCommit: "7d1e4f2a9c3b5e8d0a6f2c4b8e1d3a5f7c9b2e40", Pass: 1, Rounds: 1},
				},
				PullRequest: &PullRequestView{
					Repo:           "acme/widgets",
					Number:         1239,
					Head:           "7d1e4f2a9c3b5e8d0a6f2c4b8e1d3a5f7c9b2e40",
					State:          "merged",
					ReviewDecision: "approved",
				},
				// A check of this release records the project's `production_check` role, the tester;
				// this one opened before it and keeps the implementer, which is the migrated case
				// the release must keep working. Look 2 is open, so no agent's wake stands and the
				// next look is the check's bound. It is the issue's one open check, so `checks`
				// holds it alone and `check` is it.
				Check:  &goldenCheck,
				Checks: []CheckView{goldenCheck},
				DesignGate: &GateView{
					ArtifactID:      "0c7e5a31-9d84-4b26-a0f3-5e81c2d47b90",
					CurrentVersion:  4,
					ApprovedVersion: &mergedApproval,
				},
				// A root whose spec a person approved at the version Dispatch holds: the status
				// that lets the tree's work past the step.
				GatedSteps: []GatedStepView{{
					Step:            record.DesignStepSpec,
					ArtifactID:      "0c7e5a31-9d84-4b26-a0f3-5e81c2d47b90",
					CurrentVersion:  4,
					ApprovedVersion: &mergedApproval,
					Status:          GatedStepApproved,
				}},
			},
			// LEGION-218 and LEGION-219 are the two reviewing roots whose round waits on GitHub's
			// merge state for the head its reviewer's approval stands on
			// (classify.RoundWaitsOnMergeState, the one rule the engine reads to decide whether to
			// read GitHub for a round), so each carries a `mergeWait`. They are the two readings
			// the object was added for: a round GitHub has answered nothing about yet, and a
			// round GitHub has held on `blocked` for days, whose architect and reviewer have been
			// told of it.
			"LEGION-218": {
				Key:           "LEGION-218",
				Generation:    1,
				Phase:         phase.Reviewing,
				Status:        "needs_review",
				Tree:          "LEGION-218",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Workers: map[claim.Role]PhaseView{
					claim.RoleImplementer: {
						Claim:         ClaimView{Session: "ses_implementer_218", State: "idle"},
						HandoffCommit: "6c2f8a0d4b7e13952a8c0f6d3b5e9172c4a8d063",
						Pass:          1,
					},
					claim.RoleReviewer: {Claim: ClaimView{Session: "ses_reviewer_218", State: "idle"}, Pass: 1},
				},
				// Reviewing names the reviewer, so the issue carries its entry numbers as every
				// issue whose phase names a phase worker role does: its reviewer was handed the
				// round's one entry and holds it.
				PhaseEntry: 1,
				GivenEntry: 1,
				PullRequest: &PullRequestView{
					Repo:           "acme/widgets",
					Number:         1251,
					Head:           "6c2f8a0d4b7e13952a8c0f6d3b5e9172c4a8d063",
					State:          "open",
					ReviewDecision: "approved",
				},
				// The round waits for GitHub's first answer about this head: no read of the pull
				// request has answered since the push, so the record holds no merge state and no
				// moment of a read, and the view leaves both out rather than carrying a zero
				// instant a reader would take for a read in 1 CE. No CI settlement has brought a
				// run stamped with the head, which `checkRuns` says as 0 rather than by its
				// absence, and nobody has been told of a stuck round, so neither told mark is
				// on the wire.
				MergeWait: &MergeWaitView{
					WaitsOn:       "6c2f8a0d4b7e13952a8c0f6d3b5e9172c4a8d063",
					HeadUpdatedAt: &waitingHeadUpdatedAt,
				},
				DesignGate: &GateView{
					ArtifactID:      "f3b8d205-7c41-4e69-9a02-5d7e1c6b4f38",
					CurrentVersion:  5,
					ApprovedVersion: &reviewApproval,
				},
				GatedSteps: []GatedStepView{{
					Step:            record.DesignStepSpec,
					ArtifactID:      "f3b8d205-7c41-4e69-9a02-5d7e1c6b4f38",
					CurrentVersion:  5,
					ApprovedVersion: &reviewApproval,
					Status:          GatedStepApproved,
				}},
				Slot: &SlotView{
					Index:      3,
					AdmittedAt: time.Date(2026, 9, 22, 8, 14, 52, 0, time.UTC),
				},
			},
			"LEGION-219": {
				Key:           "LEGION-219",
				Generation:    3,
				Phase:         phase.Reviewing,
				Status:        "needs_review",
				Tree:          "LEGION-219",
				Repo:          "zeta/tools",
				DefaultBranch: "release/v7.1",
				Workers: map[claim.Role]PhaseView{
					claim.RoleImplementer: {
						Claim:         ClaimView{Session: "ses_implementer_219", State: "idle"},
						HandoffCommit: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
						Pass:          2,
						Rounds:        1,
						BackwardMoves: map[string]int{"reviewing-implementing": 1},
					},
					claim.RoleReviewer: {Claim: ClaimView{Session: "ses_reviewer_219", State: "idle"}, Pass: 2, Rounds: 1},
				},
				// The reviewer is on its second round, its second entry, and holds it.
				PhaseEntry: 2,
				GivenEntry: 2,
				PullRequest: &PullRequestView{
					Repo:           "zeta/tools",
					Number:         642,
					Head:           "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
					State:          "open",
					MergeState:     "blocked",
					ReviewDecision: "approved",
				},
				// The stuck round: the approval stands on a head GitHub has held on `blocked` since
				// the first read after the approval, two days ago, and again at the read a minute
				// before this state; the head itself has stood since the push before that approval,
				// and two of the fence's runs are stamped with the head, so the wait is not a head
				// GitHub has seen no run for. The told marks name the head and the state the
				// architect and the reviewer were told of (workflow's tellStuckMergeState), which
				// is what tells this round from LEGION-218's: both wait on GitHub's next answer
				// about the same head, and only this one has been reported.
				MergeWait: &MergeWaitView{
					WaitsOn:       "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
					MergeState:    "blocked",
					StateReadAt:   &stuckStateReadAt,
					HeadUpdatedAt: &stuckHeadUpdatedAt,
					CheckRuns:     2,
					ToldHead:      "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
					ToldState:     "blocked",
				},
				DesignGate: &GateView{
					ArtifactID:      "a9142c7e-0b63-4d85-8f17-3e6a0d9b2c54",
					CurrentVersion:  2,
					ApprovedVersion: &stuckApproval,
				},
				GatedSteps: []GatedStepView{{
					Step:            record.DesignStepSpec,
					ArtifactID:      "a9142c7e-0b63-4d85-8f17-3e6a0d9b2c54",
					CurrentVersion:  2,
					ApprovedVersion: &stuckApproval,
					Status:          GatedStepApproved,
				}},
				Slot: &SlotView{
					Index:      4,
					AdmittedAt: time.Date(2026, 9, 19, 10, 27, 44, 0, time.UTC),
				},
				// Its pod is quiet and kept: its reviewer's last report named an async job still
				// running, so every pass of the idle rule since the evening before has held the pod
				// for background work, the last of them a minute before this state. That streak is
				// the pod's standing hold (contract 246), with the kind the report named.
				PodHold: &PodHoldView{
					Reason:  "background_work",
					FirstAt: time.Date(2026, 9, 21, 22, 4, 0, 0, time.UTC),
					At:      time.Date(2026, 9, 22, 9, 17, 0, 0, time.UTC),
					Detail:  "async-job",
				},
			},
		},
		// LEGION-208's lifecycle lane is running row 801; the supervise row queued after it came due
		// two and a half minutes ago and waits behind it (record.Store.HeldRows).
		HeldOutboxRows: []HeldOutboxRow{{ID: 802, Kind: "supervise", Issue: "LEGION-208", WaitedSeconds: 150, WaitsOn: 801}},
		// The daemon computes this list (workflow.Stranded over the record, which internal/daemon's
		// source.State serves as `strandedClaims`); this fixture holds no record, so the list is
		// written here by hand. It carries one entry of each state the rule names a claim in —
		// failed, suspended and retired — with the same three keys each, which is what a session on
		// the contract before this one parses; who suspended a claim is read on the operator claim
		// routes. LEGION-210's entry is one workflow.Stranded writes for the record this state
		// describes: its implementer failed while implementing, its issue is held from that phase,
		// and nothing retries the claim until its architect retries the phase. LEGION-241's entry
		// is one of the same kind: its checker failed at step and its issue is held from that step.
		// The other two stand here for their states and are not entries the rule would write:
		// LEGION-211's released implementer, whose wake never brought its pod back, is on a root
		// whose own tree has not been opened, which runs nothing (record.TreeRuns), and
		// LEGION-212's retired tester is on a root that waits on a person, whose answer resumes its
		// architect; workflow.Stranded passes over every claim of both. The `why` of each of the
		// four issues is the projection's reading of this list.
		StrandedClaims: []StrandedClaim{
			{Issue: "LEGION-210", Role: claim.RoleImplementer, State: "failed", Token: "legion-legion-legion-210-implementer"},
			{Issue: "LEGION-211", Role: claim.RoleImplementer, State: "suspended", Token: "legion-legion-legion-211-implementer"},
			{Issue: "LEGION-212", Role: claim.RoleTester, State: "retired", Token: "legion-legion-legion-212-tester"},
			{Issue: "LEGION-241", Role: "checker", State: "failed", Token: "legion-legion-legion-241-checker"},
		},
		// A wave going out at the pace its operator set, with one wake of each lane still held:
		// LEGION-207's implementer, working its production check in the pod the daemon keeps,
		// waits for the relaunch the stale-plugin lane's gate admitted for it, its own kind;
		// LEGION-208's architect, already running,
		// waits for the catch-up a child's re-admission wrote it in place of a launch, which waits
		// on its outbox row rather than in a machine; and LEGION-208's planner, whose pod answered
		// its update round, waits for the launch that brings it up on the pod's replacement. The
		// two machine wakes are over a process that still runs, which the queue holds like any
		// other, and the three are one list in claim order whichever lane holds each.
		WakesWaiting: []WakeWaiting{
			{Claim: "legion-legion-legion-207-implementer", Issue: "LEGION-207", Role: claim.RoleImplementer,
				Kind: supervise.WakePluginRefresh, DueAt: time.Date(2026, 9, 22, 9, 17, 56, 0, time.UTC)},
			{Claim: "legion-legion-legion-208-architect", Issue: "LEGION-208", Role: claim.RoleArchitect,
				Kind: "catch-up", DueAt: time.Date(2026, 9, 22, 9, 18, 3, 0, time.UTC)},
			{Claim: "legion-legion-legion-208-planner", Issue: "LEGION-208", Role: claim.RolePlanner,
				Kind: "launch", DueAt: time.Date(2026, 9, 22, 9, 18, 11, 0, time.UTC)},
		},
		// Four claims are still on an earlier plugin, the one whose refresh wake waits above among
		// them: the count is everything the lane has left to do and the wakes are the share of it
		// the pacing is holding now, which is the pair a reader of a running wave needs
		// (contract 73).
		ClaimsToRefresh: 4,
		// The controller paused the project nine minutes before this read (decision 25 of
		// dispatch://INFRA-3759), built through pausedViewOf from the store's own rows as the
		// state's read builds it (PauseStateTx). LEGION-208's implementer is still in the turn the
		// pause found it in, so the pause waits on its stop. Two rows are held for the pause: the
		// start of LEGION-211's released implementer that a wake queued, which names its claim, and
		// a notice for LEGION-209's architect, which names none (the tree walk routes a notice at
		// run time), so it carries no `claim`.
		Paused: pausedViewOf(store.Pause{
			ID: 7, Project: "LEGION", Since: pausedSince, Reason: "the model provider is refusing every turn",
			By: store.PauseActor{Kind: store.PauseByController, Session: "ses_controller"},
		}, []store.PauseStop{{
			ID: 31, Pause: 7, Claim: "legion-legion-legion-208-implementer", Issue: "LEGION-208",
			Role: claim.RoleImplementer, AskedAt: pausedSince,
		}}, []record.PauseHeldRow{
			{OutboxRow: record.OutboxRow{ID: 811, Kind: record.OutboxKindSupervise, Issue: "LEGION-211"},
				Claim: "legion-legion-legion-211-implementer", WaitedSeconds: 412},
			{OutboxRow: record.OutboxRow{ID: 814, Kind: record.OutboxKindNotice, Issue: "LEGION-209"}, WaitedSeconds: 95},
		}),
		ControllerLocator: &ControllerLocator{
			Runtime:      "kubernetes",
			External:     true,
			SessionID:    "ses_controller",
			RegisteredAt: time.Date(2026, 9, 22, 9, 16, 40, 0, time.UTC),
		},
		AgentSecretsLogin: &AgentSecretsLoginView{State: "issued"},
		// A kubernetes deployment whose image passed the probe, with the broker gap decided and
		// one role unreserved, and no session's report yet, so the fixture carries a decided row and
		// an open row beside the present, unchecked and withheld ones, rendered by the report
		// itself: a wording change there rewrites the fixture rather than leaving the pinned words
		// stale. Its one repository cache, acme/widgets' (Ready and read, as `repositories` below
		// says), last answered its health with a repack that failed for lack of space, so the
		// repository-cache-room row is open too, with the third source, `caches`, which a caller
		// below cachesSourceContract is served without.
		Capabilities: CapabilityStatesOf(capabilities.Deployment{
			Runtime: "kubernetes", Probed: true, ModelFallback: "on",
			Decided:        map[capabilities.Name]string{capabilities.Secrets: "pods are enrolled with the secrets broker once dispatch://LEGION-205 lands; until then no pod reads a secret"},
			UnreservedPods: []capabilities.Pod{capabilities.PodIssue},
			Caches: []capabilities.Cache{{
				Repo: "acme/widgets", Claim: "cache-legion-legion-cache-acme-widgets-d782c874", Read: true, Reported: true,
				VolumeBytes: 20 << 30, AvailableBytes: 107374182, Bytes: 6612345678,
				RepackOutOfSpaceAt: time.Date(2026, 10, 16, 2, 15, 33, 0, time.UTC),
			}},
		}.Report()),
		// Five repositories the live records name, of three owners, sorted by name, each showing
		// an answer the block carries: acme/gadgets, the review App not installed on it, which
		// GitHub answered of both Apps; acme/widgets, both role Apps installed, its cache Ready and
		// its last health read answered; sjawhar/legion, which the records keep under its name
		// before a configured transfer and the rename rule serves as cybermonklabs/legion, both
		// Apps installed there; zeta/archive, whose implement App read GitHub did not answer, so it
		// carries why and no answer of the Apps at all; and zeta/tools, both Apps installed and its
		// cache failing, which withoutWebhookApp names as Dispatch's GitHub App not installed on
		// it, served all the same. acme/legacy, which no live record names any more, keeps its
		// cache until its retention ends.
		Repositories: RepositoriesView{
			Live: []LiveRepositoryView{
				{Repo: "acme/gadgets", Apps: &AppInstallationsView{Implement: true}},
				{Repo: "acme/widgets", Apps: &AppInstallationsView{Implement: true, Review: true}, Cache: &CacheView{
					Repo: "acme/widgets", Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true, Address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
					Head: "34a7ac80e9c3389ffbc1bbeece371ccacc043992", LastFetchAt: time.Date(2026, 10, 16, 19, 20, 30, 0, time.UTC), Bytes: 6612345678,
				}},
				{Repo: "sjawhar/legion", ServedAs: "cybermonklabs/legion", Apps: &AppInstallationsView{Implement: true, Review: true}},
				{Repo: "zeta/archive", ReadError: "the implement App's installation on zeta/archive could not be read: GitHub answered 502; Legion reads it again"},
				{
					Repo: "zeta/tools", Apps: &AppInstallationsView{Implement: true, Review: true},
					Cache: &CacheView{
						Repo: "zeta/tools", Sandbox: "legion-legion-cache-zeta-tools-1b2c3d4e", State: "failing",
						Reason: "container cache CrashLoopBackOff: back-off 5m0s restarting failed container (restarted 4 times)",
					},
				},
			},
			WithoutWebhookApp: []WebhookAppGapView{{
				Repo:   "zeta/tools",
				Reason: "Dispatch's GitHub App is not installed on zeta/tools: GitHub sends Legion none of its events, nothing redelivers them, and no periodic pass makes up for them, so its issues stand still until the App is installed",
			}},
			Caches: []CacheView{{
				Repo: "acme/legacy", Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "starting", RetainedUntil: time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC),
			}},
		},
		// The resolved maximal kubernetes file, so the fixture pins every key `settings` can carry
		// but `roles` and `role_types`, which the resolved maximal file sets none of and
		// TestSettingsOfAKubernetesConfigurationNamesEveryKeyAndNoSecret (settings_test.go) pins.
		Settings: SettingsOf(settingsFixtureConfig(t)),
		// The three durable consumers the daemon opens, in the order Lag answers them
		// (intake.Consumers.Lag): the Dispatch consumer is caught up; the GitHub consumer has 412
		// messages waiting and has handled none for over a minute; the listener-wake consumer's
		// info read ran out of the bound.
		Consumers: ConsumerLagViewsOf([]intake.ConsumerLag{
			{Name: "legion-go-LEGION-dispatch", LastHandledAt: time.Date(2026, 9, 22, 9, 17, 50, 0, time.UTC)},
			{
				Name: "legion-go-LEGION-github", Waiting: 412, OldestAt: time.Date(2026, 9, 22, 9, 5, 34, 0, time.UTC), OldestAge: 754 * time.Second,
				LastHandledAt: time.Date(2026, 9, 22, 9, 16, 41, 0, time.UTC), Stalled: true,
			},
			{Name: "legion-go-LEGION-wake", Unavailable: "read the consumer's info: context deadline exceeded"},
		}),
	}
	// LEGION-208's tree was drawn into the memory group, so its root and LEGION-209, a child of
	// that tree, which its `tree` names, both show the draw as internal/projection fills them (a
	// tree row is its root's, shown to each issue of the tree), while every other issue of the
	// fixture belongs to a tree no draw has reached. `memory` is the arm of the experiment of
	// that name (config.Project.ExperimentsDeclared), and the three memory fields are that one
	// assignment spelled out as the readers of contract 49 have always read it.
	//
	// Beside it the fixture carries the shapes an assignment has: a tree experiment, which every
	// issue of the tree shows; an issue experiment, `plan-length`, drawn for each issue of that
	// tree on its own, so a root and its child stand in different arms of one experiment; and,
	// on the root LEGION-210, a row a hand-over settled at no arm at all — an empty variant, an
	// empty mode and no instant — which is how a reader tells a unit settled at no arm from a
	// unit never drawn, which carries no row; and, on the child LEGION-209, the row of an
	// experiment that sets how its issues are done, `planning`, which carries the group its arm set
	// at the draw (`plan`), here `default`.
	memoryDrawn := AssignmentView{Experiment: "memory", Variant: "memory", Mode: string(config.ExperimentRandom), DrawnAt: &memoryDrawnAt}
	root := state.Issues["LEGION-208"]
	root.Assignments = []AssignmentView{
		memoryDrawn,
		{Experiment: "plan-length", Variant: "short", Mode: string(config.ExperimentRandom), DrawnAt: &planDrawnAt},
	}
	root.MemoryGroup = "memory"
	root.MemoryDrawMode = string(config.ExperimentRandom)
	root.MemoryDrawnAt = &memoryDrawnAt
	state.Issues["LEGION-208"] = root
	settled := state.Issues["LEGION-210"]
	settled.Assignments = []AssignmentView{{Experiment: "plan-length"}}
	state.Issues["LEGION-210"] = settled
	state.Issues["LEGION-209"] = Issue{
		Key:           "LEGION-209",
		Generation:    1,
		Phase:         phase.Admitted,
		Status:        "in_progress",
		Tree:          "LEGION-208",
		Repo:          "acme/widgets",
		DefaultBranch: "main",
		Workers:       map[claim.Role]PhaseView{},
		Assignments: []AssignmentView{
			memoryDrawn,
			{Experiment: "plan-length", Variant: "long", Mode: string(config.ExperimentRandom), DrawnAt: &planDrawnAt},
			{Experiment: "planning", Variant: "b", Mode: string(config.ExperimentRandom), DrawnAt: &planDrawnAt, Plan: string(config.PlanGroupDefault)},
		},
		// The child the workflow holds at `admitted`: it stands in no line, and the Dispatch
		// links `admission.blocked` names are what hold it where it stands, which is the
		// rendered answer it carries (`why`, api.WhyBlocked).
		Why: &WhyView{
			State:  WhyBlocked,
			Reason: "It waits on LEGION-210 and LEGION-213, which are not done.",
		},
		MemoryGroup:    "memory",
		MemoryDrawMode: string(config.ExperimentRandom),
		MemoryDrawnAt:  &memoryDrawnAt,
	}
	// The oldest plugin any pod of the fixture's project runs, which the state route reads from
	// the claims of its own snapshot (api's lowestPluginContract). The floor is the reading this
	// change records of the fleet, and the fixture carries a value so that the golden and every
	// walk over it see the field.
	lowest := ServingFloor
	state.LowestPluginContract = &lowest
	// Every issue standing in a tree the admission block names carries that tree's quiet time,
	// the two keys contract 111 added: the measure is the tree's, so each member of one tree
	// carries the same pair, LEGION-209, a child of LEGION-208's tree, carrying that tree's.
	// LEGION-207's tree closed, so its issue is not measured, which is what a reader is served for
	// an issue no live tree holds. Every pair is one reading, at quietReadAt, a little after the
	// newest step record of this state.
	//
	// Each tree's movement is the newest fact the measure reads of it — the `MovedAt` of its
	// members and the step times of their claims (record.QuietTrees) — and its hours are the
	// reading less that movement, worked out here rather than written beside it: a second
	// number can disagree with the first, and one did. LEGION-208's pair said the tree had been
	// still for half an hour while its own implementer's step stood 48 seconds before the
	// reading, so the fixture showed a pair no daemon would serve, which is why the movement of
	// that tree is the step record itself. The other trees hold no claim with a step, so each of
	// them moves at a time of its own. Each reviewing root moved last at the newest of its own
	// movements named above: LEGION-218 at its approval, and LEGION-219 at its first `blocked`
	// read. LEGION-219's head was read again a minute before this state, and that read only
	// confirmed what the record held, which moves no tree (workflow's pullRequestNews), so its
	// tree has been still for more than a day although its read stamp is a minute old.
	// LEGION-240 last moved when it entered step, and LEGION-241 when its checker's claim failed
	// and the hold took it out of step; the failed claim carries no step record.
	quietReadAt := time.Date(2026, 9, 22, 9, 18, 0, 0, time.UTC)
	quietMoved := map[string]time.Time{
		"LEGION-208": implementerStep.At,
		"LEGION-210": time.Date(2026, 9, 22, 6, 18, 0, 0, time.UTC),
		"LEGION-211": time.Date(2026, 9, 21, 9, 18, 0, 0, time.UTC),
		"LEGION-212": time.Date(2026, 9, 22, 9, 3, 0, 0, time.UTC),
		"LEGION-218": waitingApprovedAt,
		"LEGION-219": newest(stuckApprovedAt, stuckFirstBlockedAt),
		"LEGION-240": time.Date(2026, 9, 22, 8, 52, 16, 0, time.UTC),
		"LEGION-241": time.Date(2026, 9, 22, 9, 4, 39, 0, time.UTC),
	}
	for key, issue := range state.Issues {
		movedAt, measured := quietMoved[issue.Tree]
		if !measured {
			continue
		}
		hours := quietReadAt.Sub(movedAt).Hours()
		issue.TreeMovedAt, issue.QuietHours = &movedAt, &hours
		state.Issues[key] = issue
	}
	// Every issue the workflow records carries its own Dispatch priority, null for none: the line's
	// two members carry theirs where they are written above, and every other recorded issue has
	// none. A member's line priority is then the one the projection gives it, record.PriorityOf of
	// its own and its tree root's, so the fixture holds no pair the daemon could not serve.
	for key, issue := range state.Issues {
		if issue.Phase == Unrecorded || issue.Priority != nil {
			continue
		}
		issue.Priority = &noPriority
		state.Issues[key] = issue
	}
	for key, issue := range state.Issues {
		if issue.WaitingLine == nil {
			continue
		}
		root := state.Issues[issue.Tree]
		if issue.Priority == nil || root.Priority == nil {
			t.Fatalf("%s waits in the line without its own or its tree root's priority", key)
		}
		placed := PriorityView(record.PriorityOf(record.Issue{Priority: record.Priority(*issue.Priority)},
			record.Issue{Priority: record.Priority(*root.Priority)}))
		line := *issue.WaitingLine
		line.Priority = &placed
		issue.WaitingLine = &line
		state.Issues[key] = issue
	}
	// The intake's open give-ups (contract 228): the Dispatch consumer has two, a decoded event
	// given up at its bound and a newer message that did not decode, which carries no event id;
	// the GitHub consumer has one, which the sweep gave up once its durable was made again past it.
	// Each consumer's newest sequence is the `throughSeq` of a bulk replay or discard of all of
	// its records.
	state.IntakeGiveUps = IntakeGiveUpsView{
		Consumers: []IntakeGiveUpCountView{
			{Name: "legion-go-LEGION-dispatch", Open: 2, NewestSeq: 6044896},
			{Name: "legion-go-LEGION-github", Open: 1, NewestSeq: 812004},
		},
		Newest: []IntakeGiveUpView{
			{ID: 43, Consumer: "legion-go-LEGION-dispatch", Subject: "notifications.dispatch.issue.LEGION-214.issue.updated",
				StreamSeq: 6044896, StoredAt: time.Date(2026, 9, 22, 8, 40, 12, 0, time.UTC), Deliveries: 1,
				GaveUpAt: time.Date(2026, 9, 22, 8, 40, 13, 0, time.UTC), Reason: "undecodable",
				LastError: "decode Envoy envelope: unexpected end of JSON input"},
			{ID: 42, Consumer: "legion-go-LEGION-github", Subject: "notifications.github.acme.widgets.pr.17.review",
				StreamSeq: 812004, StoredAt: time.Date(2026, 9, 22, 6, 2, 0, 0, time.UTC), EventID: new("github-9b2c71e0"), Deliveries: 3,
				GaveUpAt: time.Date(2026, 9, 22, 7, 55, 0, 0, time.UTC), Reason: "consumer moved past",
				LastError: "the consumer moved past it without delivering it again"},
			{ID: 41, Consumer: "legion-go-LEGION-dispatch", Subject: "notifications.dispatch.issue.LEGION-213.issue.updated",
				StreamSeq: 6044801, StoredAt: time.Date(2026, 9, 22, 6, 30, 0, 0, time.UTC), EventID: new("dispatch-6044801"), Deliveries: 21,
				GaveUpAt: time.Date(2026, 9, 22, 7, 31, 4, 0, time.UTC), Reason: "bound",
				LastError: "apply the issue event: read LEGION-213 from Dispatch: context deadline exceeded"},
		},
	}
	return state
}

// newest is the latest of times, which is how the quiet time takes a tree's movement from the
// several facts that can move it (record.QuietTrees).
func newest(times ...time.Time) time.Time {
	var latest time.Time
	for _, at := range times {
		if at.After(latest) {
			latest = at
		}
	}
	return latest
}

// The state golden is the body the route serves a caller at this daemon's own contract: the
// source answers no mark and the route writes one (shaping's stateFor), so the fixture carries
// the two numbers of `contract` and the plugin's schema is held to them. A caller below the
// contract that added it is served no such key, which TestANarrowedBodySaysSo holds.
func TestStateGolden(t *testing.T) {
	state := populatedState(t)
	state.Contract = &ContractView{Daemon: DaemonAPIVersion, Caller: DaemonAPIVersion}
	golden(t, "state.json", state)
}

// The populated state is written by hand, as a state the daemon serves, so its admission block
// holds to the rules the daemon's own pass builds that block by. An issue that holds a slot or
// waits in the line stands in a live tree, so its tree is one `trees` names (record.LiveRoots: a
// root at work has its lifecycle open, and a root in the line is queued). Every issue the
// state records that has not finished stands in `active`, `waiting` or `held`, since the held
// list is every candidate that stands in neither of the other two (record.HeldList). And
// `active` and each issue's `slot` are read from the same slot rows (internal/projection's
// slotViews), in the order of their index, which the slots table keeps unique and never negative
// (record.Store.Slots): so an issue carries a slot view exactly when `active` names it, and
// `active` lists its issues by that index, each a different one, each slot with the moment the
// pass took it (admit's PutSlot of the pass's now). Each issue the state records in a tree
// `trees` names carries that tree's quiet time, and no other issue carries one
// (internal/projection's quietTrees): one pair for the whole tree, each pair read at the one
// instant the state is measured at, unless its movement stands at or after that instant and it
// reads 0 (record.QuietTrees). The spec row of a root is served twice, as `designGate`
// (record.SpecGate) and as the spec entry of `gatedSteps` (GatedStepViewsOf), so where the
// deployment gates spec an issue carries `designGate` exactly when that entry names a document,
// and the two name the same artifact and versions (internal/projection). Every root carries one
// `gatedSteps` entry for each step its tree is gated on (record.TreeSteps over the steps the
// deployment gates, `settings.gates.design`), and an issue that is no root carries none. Every
// issue the record holds in the workflow that `strandedClaims` names reads `why` claim_stopped,
// with the sentence naming its phase and that claim, and no other issue reads it (projection's
// whyStates reads that very list first). Every other issue in the workflow that stands in none
// of `active`, `waiting` and `blocked` reads `unlisted`, unless its work runs past its review,
// and no other issue reads it. Every issue the workflow records names the root of its tree, a
// root the state records, and carries that root's `memory` draw (internal/projection's
// assignmentViews). A golden that breaks any of these rules shows every reader of state.json a
// state the daemon never serves.
func TestTheStateGoldensAdmissionNamesEveryIssueItHolds(t *testing.T) {
	state := populatedState(t)
	admission := state.Admission
	trees := map[string]bool{}
	for _, tree := range admission.Trees {
		trees[tree] = true
	}
	named := map[string]bool{}
	for _, list := range [][]string{admission.Active, admission.Waiting} {
		for _, key := range list {
			named[key] = true
			issue, recorded := state.Issues[key]
			if !recorded {
				t.Errorf("the admission names %s in a slot or in the line, and the state records no such issue", key)
				continue
			}
			if !trees[issue.Tree] {
				t.Errorf("%s holds a slot or waits in the line in the tree of %s, which `trees` does not name: %v", key, issue.Tree, admission.Trees)
			}
		}
	}
	for _, row := range admission.Held {
		named[row.Issue] = true
	}
	for key, issue := range state.Issues {
		if issue.Phase != phase.Done && !named[key] {
			t.Errorf("%s stands at %s, and the admission names it in none of `active`, `waiting` and `held`", key, issue.Phase)
		}
	}
	active := map[string]bool{}
	previous, previousKey := -1, ""
	for _, key := range admission.Active {
		active[key] = true
		issue, recorded := state.Issues[key]
		if !recorded {
			continue
		}
		if issue.Slot == nil {
			t.Errorf("%s stands in `active`, and its issue carries no `slot`", key)
			continue
		}
		if issue.Slot.AdmittedAt.IsZero() {
			t.Errorf("%s holds slot %d, and its `slot` names no moment it took it", key, issue.Slot.Index)
		}
		if issue.Slot.Index <= previous {
			t.Errorf("%s holds slot %d after %s at slot %d in `active`, which lists the slots by their index, each a different one from 0", key, issue.Slot.Index, previousKey, previous)
		}
		previous, previousKey = issue.Slot.Index, key
	}
	for key, issue := range state.Issues {
		if issue.Slot != nil && !active[key] {
			t.Errorf("%s carries slot %d, and `active` does not name it", key, issue.Slot.Index)
		}
	}
	type quiet struct {
		movedAt time.Time
		hours   float64
	}
	treeQuiet := map[string]quiet{}
	var readAt time.Time
	for _, key := range slices.Sorted(maps.Keys(state.Issues)) {
		issue := state.Issues[key]
		measured := issue.TreeMovedAt != nil || issue.QuietHours != nil
		if !trees[issue.Tree] || issue.Phase == Unrecorded {
			if measured {
				t.Errorf("%s carries a quiet time, and its tree %q is no live tree of a record: %v", key, issue.Tree, admission.Trees)
			}
			continue
		}
		if issue.TreeMovedAt == nil || issue.QuietHours == nil {
			t.Errorf("%s stands in the live tree of %s, and carries no whole quiet time (treeMovedAt %v, quietHours %v)", key, issue.Tree, issue.TreeMovedAt, issue.QuietHours)
			continue
		}
		pair := quiet{movedAt: *issue.TreeMovedAt, hours: *issue.QuietHours}
		if first, seen := treeQuiet[issue.Tree]; seen && (!first.movedAt.Equal(pair.movedAt) || first.hours != pair.hours) {
			t.Errorf("%s carries the quiet time moved %s, quiet %g hours, and another issue of the tree of %s carries moved %s, quiet %g hours",
				key, pair.movedAt.Format(time.RFC3339), pair.hours, issue.Tree, first.movedAt.Format(time.RFC3339), first.hours)
		}
		treeQuiet[issue.Tree] = pair
		if pair.hours == 0 {
			continue
		}
		at := pair.movedAt.Add(time.Duration(pair.hours * float64(time.Hour))).Round(time.Millisecond)
		if readAt.IsZero() {
			readAt = at
		} else if !at.Equal(readAt) {
			t.Errorf("%s's quiet time is read at %s, and the state's other quiet times at %s", key, at, readAt)
		}
	}
	// The design gate: one spec row, served as `designGate` and as the spec entry of `gatedSteps`.
	// Only a root holds gate rows, so an issue that is no root carries neither, and the spec entry
	// names a document exactly when the row exists, which is when `designGate` is served.
	if !slices.Contains(state.Settings.Gates.Design, record.DesignStepSpec) {
		t.Errorf("the golden's deployment gates %v, no spec step, so no issue's `designGate` can be read against its spec entry", state.Settings.Gates.Design)
	}
	for _, key := range slices.Sorted(maps.Keys(state.Issues)) {
		issue := state.Issues[key]
		var spec *GatedStepView
		for i, entry := range issue.GatedSteps {
			if entry.Step == record.DesignStepSpec && entry.ArtifactID != "" {
				spec = &issue.GatedSteps[i]
			}
		}
		gate := issue.DesignGate
		switch {
		case spec == nil && gate != nil:
			t.Errorf("%s carries the design gate of document %s, and no spec step of its `gatedSteps` names a document", key, gate.ArtifactID)
		case spec != nil && gate == nil:
			t.Errorf("%s's spec step names document %s at version %d, and the issue carries no `designGate`", key, spec.ArtifactID, spec.CurrentVersion)
		case spec != nil && (gate.ArtifactID != spec.ArtifactID || gate.CurrentVersion != spec.CurrentVersion || !reflect.DeepEqual(gate.ApprovedVersion, spec.ApprovedVersion)):
			t.Errorf("%s carries the design gate of document %s at version %d approved at %s, and its spec step names document %s at version %d approved at %s",
				key, gate.ArtifactID, gate.CurrentVersion, approvedAt(gate.ApprovedVersion), spec.ArtifactID, spec.CurrentVersion, approvedAt(spec.ApprovedVersion))
		}
	}
	// The gated steps: the projection gives every root GatedStepViewsOf(TreeSteps(steps, rows)),
	// one entry for each step the deployment gates, an `unregistered` one where the root holds no
	// row, with criteria after them where the root registered a criteria document beside a
	// deployment that gates spec and not criteria; and an issue that is no root holds no gate rows
	// and is given no entry. The rows a root holds are the entries that name a document.
	for _, key := range slices.Sorted(maps.Keys(state.Issues)) {
		issue := state.Issues[key]
		rows := map[record.DesignStep]record.DesignGate{}
		steps := make([]record.DesignStep, 0, len(issue.GatedSteps))
		for _, entry := range issue.GatedSteps {
			steps = append(steps, entry.Step)
			if entry.ArtifactID != "" {
				rows[entry.Step] = record.DesignGate{Issue: key, Step: entry.Step, ArtifactID: entry.ArtifactID}
			}
		}
		var want []record.DesignStep
		if key == issue.Tree {
			want = record.TreeSteps(state.Settings.Gates.Design, rows)
		}
		if !slices.Equal(steps, want) {
			t.Errorf("%s, of the tree of %q, carries the gated steps %v, and its tree's gate gives it %v", key, issue.Tree, steps, want)
		}
	}
	// The stopped claims: the daemon hands the projection the claims it judges nothing will wake
	// and serves the same list as `strandedClaims` (internal/daemon's state read), and whyStates
	// reads it before anything else, so every issue the record holds in the workflow that one of
	// them names reads claim_stopped, with the sentence of claimStoppedReason, the worker's claim
	// named over the tree architect's where one issue has both. An issue the workflow does not
	// record is in no record whyStates reads, and no other issue reads that state.
	stopped := map[string]StrandedClaim{}
	for _, stranded := range state.StrandedClaims {
		if named, known := stopped[stranded.Issue]; known && named.Role != claim.RoleArchitect {
			continue
		}
		stopped[stranded.Issue] = stranded
	}
	inWorkflow := func(issue Issue) bool {
		return issue.Phase != Unrecorded && record.InWorkflow(record.Issue{Phase: issue.Phase, Status: issue.Status})
	}
	for _, key := range slices.Sorted(maps.Keys(state.Issues)) {
		issue := state.Issues[key]
		stranded, named := stopped[key]
		if !named || !inWorkflow(issue) {
			if issue.Why != nil && issue.Why.State == WhyClaimStopped {
				t.Errorf("%s reads %s (%q), and no stranded claim of an issue in the workflow names it", key, WhyClaimStopped, issue.Why.Reason)
			}
			continue
		}
		want := fmt.Sprintf("Its phase is %s and the %s's claim is stranded at %s, so nothing will wake it.", issue.Phase, stranded.Role, stranded.State)
		if stranded.Role == claim.RoleArchitect {
			want = fmt.Sprintf("Its phase is %s and its tree's architect claim is stranded at %s, so nothing will wake it.", issue.Phase, stranded.State)
		}
		if issue.Why == nil || *issue.Why != (WhyView{State: WhyClaimStopped, Reason: want}) {
			t.Errorf("%s's %s claim is stranded at %s, and its `why` is %+v, want %s: %q", key, stranded.Role, stranded.State, issue.Why, WhyClaimStopped, want)
		}
	}
	// The unlisted issues: whyStates' last arm. An issue the record holds in the workflow that no
	// stranded claim names, which stands in none of `active`, `waiting` and `blocked`, reads
	// `unlisted` with the sentence of its word in `admission.held` (heldSentences, the
	// unclassified sentence where the table serves none), and no other issue reads that state.
	// The arm passes over an issue whose work runs in a phase that holds no slot in a tree that
	// runs (atWorkInSlotlessPhase); the state does not say which trees run, so such an issue, one
	// standing in accepting, retro or merging and not held, may read `unlisted` or nothing.
	lined, blocked := map[string]bool{}, map[string]bool{}
	for _, key := range admission.Waiting {
		lined[key] = true
	}
	for _, row := range admission.Blocked {
		blocked[row.Issue] = true
	}
	for _, key := range slices.Sorted(maps.Keys(state.Issues)) {
		issue := state.Issues[key]
		_, stranded := stopped[key]
		unlisted := inWorkflow(issue) && !stranded && !active[key] && !lined[key] && !blocked[key]
		reads := issue.Why != nil && issue.Why.State == WhyUnlisted
		if unlisted && issue.Phase != phase.Held && record.InSlotlessPhase(record.Issue{Phase: issue.Phase}) {
			continue
		}
		switch {
		case unlisted && (!reads || issue.Why.Reason == ""):
			t.Errorf("%s stands in the workflow at %s, in none of `active`, `waiting` and `blocked`, no stranded claim names it, and its `why` is %+v, want %s with a sentence", key, issue.Phase, issue.Why, WhyUnlisted)
		case !unlisted && reads:
			t.Errorf("%s reads %s (%q), and it is not an issue in the workflow standing in none of `active`, `waiting` and `blocked` that no stranded claim names", key, WhyUnlisted, issue.Why.Reason)
		}
	}
	// The tree: the projection copies each record's tree, the key of its tree's root, which the
	// issues table holds for every record (`tree text not null`); only an issue the workflow does
	// not record stands in no tree (api.Issue.Tree). The `memory` experiment draws for a tree,
	// since config refuses it any other unit, and a tree row is its root's, shown to each issue of
	// the tree (assignmentViews): so every recorded issue carries its root's `memory` draw, the
	// group, the mode and the instant, or none where its root carries none.
	for _, key := range slices.Sorted(maps.Keys(state.Issues)) {
		issue := state.Issues[key]
		if issue.Phase == Unrecorded {
			continue
		}
		root, recorded := state.Issues[issue.Tree]
		if issue.Tree == "" || !recorded {
			t.Errorf("%s is recorded at %s, and names no root the state records as its tree (tree %q)", key, issue.Phase, issue.Tree)
			continue
		}
		if root.MemoryGroup != issue.MemoryGroup || root.MemoryDrawMode != issue.MemoryDrawMode || !reflect.DeepEqual(root.MemoryDrawnAt, issue.MemoryDrawnAt) {
			t.Errorf("%s carries the memory draw %q (mode %q, at %v), and %s, the root of its tree, carries %q (mode %q, at %v)",
				key, issue.MemoryGroup, issue.MemoryDrawMode, issue.MemoryDrawnAt, issue.Tree, root.MemoryGroup, root.MemoryDrawMode, root.MemoryDrawnAt)
		}
	}
}

// approvedAt is an approved version as a person reads it: the number, or none.
func approvedAt(version *int) string {
	if version == nil {
		return "none"
	}
	return fmt.Sprint(*version)
}

// The state route's body for a caller whose plugin is at the contract before
// repositoriesLiveContract, asking from a pod whose claims record it and stating it in
// `Legion-Plugin-Contract`, as every plugin from shapedAnswersContract on does: the state golden's
// own state, asked through the route itself. The caller is served at the recorded contract at or
// below the one it states (recordedContractAtOrBelow), which the mark names: its `repositories`
// block in the shape contract 152 gave it, written from the same entries
// (withoutRepositoriesLive), each assignment without its `plan`, since assignmentPlanContract's
// row stands above that contract too, no `intakeGiveUps` (withoutIntakeGiveUps), since
// intakeGiveUpsContract's row does as well, no issue's `podHold` (withoutPodHold), since
// podHoldContract's row does too, and every other key as a caller at shapedAnswersContract is
// served it, since no other row of stateNarrowings stands between shapedAnswersContract and
// repositoriesLiveContract. shapedAnswersContract's row and pauseContract's stand at or below the
// contract it is served at, which is why the golden carries `lowestPluginContract`, each claim
// view's `updateState` and `paused`.
// `packages/contracts/src/legion-api.test.ts` parses it with a copy of contract 152's schema of
// the block, so the narrowing is held to the reader it is written for, and its drift gate holds
// every key of the file to that copy.
func TestStateNarrowedRepositoriesGolden(t *testing.T) {
	server := stateServer(t, populatedState(t), podClaims(repositoriesLiveContract-1))
	body := stateStating(t, server, statePodAddr+":41000", repositoriesLiveContract-1)
	golden(t, "state-narrowed-repositories.json", json.RawMessage(body))
}

// One issue's record, what `GET /legion/v1/issues/{key}/record` answers: the very `api.Issue`
// the whole-project body holds under `issues.LEGION-208`, which a test of the route holds the
// two to, field for field.
func TestIssueRecordGolden(t *testing.T) {
	golden(t, "issue-record.json", populatedState(t).Issues[recordGoldenIssue])
}

// What `GET /healthz` answers: that the daemon is serving, and the project it serves.
func TestHealthGolden(t *testing.T) {
	golden(t, "health.json", Health{Status: "ok", Project: "legion"})
}

// Every plugin parses the state with the schema of the contract it was built at, and a
// Kubernetes deployment relaunches no stale session (INFRA-945 spec R10), so a session on the
// contract before this one reads this daemon's state for as long as its pod lives: a key added
// to a session-facing body refuses the whole of it at every plugin whose answer side predates
// INFRA-1048, and is dropped at every plugin after it, which loses the field rather than the
// state. `suspendedBy` is the key this change was tempted to add, and it belongs to the
// operator claim routes alone; the guard beside `legion-api.test.ts`'s schema tests holds the
// same property on the schema side.
func TestNoSessionFacingBodyCarriesSuspendedBy(t *testing.T) {
	// The body where the key does live, so a guard over nothing cannot pass for a clean one.
	operator, err := json.Marshal(OperatorClaims{Claims: []OperatorClaim{
		{Issue: "LEGION-211", Role: claim.RoleImplementer, State: "suspended", SuspendedBy: "released"},
	}})
	if err != nil {
		t.Fatalf("marshal the operator claim list: %v", err)
	}
	if !bytes.Contains(operator, []byte("suspendedBy")) {
		t.Fatalf("the operator claim list does not carry suspendedBy either: %s", operator)
	}
	encoded, err := json.Marshal(populatedState(t))
	if err != nil {
		t.Fatalf("marshal the state: %v", err)
	}
	if bytes.Contains(encoded, []byte("suspendedBy")) {
		t.Fatalf("the state body carries suspendedBy: %s", encoded)
	}
}

func TestStateStage3Golden(t *testing.T) {
	golden(t, "state-stage3.json", State{
		Daemon: DaemonInfo{
			Project:       "LEGION",
			SchemaVersion: 3,
			Boots:         5,
			FirstBootAt:   time.Date(2026, 9, 18, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 22, 17, 30, 0, 0, time.UTC),
		},
		Admission: Admission{
			Cap: 2, Active: []string{"LEGION-208"}, Waiting: []string{"LEGION-209"},
			Trees: []string{"LEGION-208"},
		},
		Issues: map[string]Issue{
			"LEGION-208": {
				Key:           "LEGION-208",
				Generation:    4,
				Phase:         phase.Reviewing,
				Status:        "needs_review",
				Tree:          "LEGION-208",
				Repo:          "sjawhar/legion-smoke",
				DefaultBranch: "main",
				Workers:       map[claim.Role]PhaseView{},
			},
		},
		PendingStatusWrites: []PendingStatusWrite{{
			Issue:     "LEGION-208",
			Payload:   json.RawMessage(`{"status":"needs_review"}`),
			Attempts:  2,
			NextAt:    time.Date(2026, 9, 22, 17, 32, 0, 0, time.UTC),
			LastError: "Dispatch unavailable",
		}},
		Settings: SettingsOf(minimalSettingsConfig(t)),
	})
}

// An operator's claim on an issue the workflow does not record is listed for the claim's sake,
// and says so rather than leaving its phase and status empty: an empty phase is no value of the
// phase enum, so it refuses the whole document, over a row that is a legitimate state.
// internal/projection's own test holds the producer to these values.
func TestStateOperatorClaimGolden(t *testing.T) {
	golden(t, "state-operator-claim.json", State{
		Daemon: DaemonInfo{
			Project:       "AC91849371",
			SchemaVersion: 3,
			Boots:         1,
			FirstBootAt:   time.Date(2026, 9, 25, 5, 50, 0, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 25, 5, 50, 0, 0, time.UTC),
		},
		Admission: Admission{Cap: 2, Active: []string{}, Waiting: []string{}},
		Issues: map[string]Issue{
			"AC91849371-900": {
				Key:     "AC91849371-900",
				Phase:   Unrecorded,
				Status:  string(Unrecorded),
				Workers: map[claim.Role]PhaseView{},
				Architect: &ClaimView{
					Session: "01a0d71d-aace-7448-a0d4-e401d736438f",
					State:   "idle",
					Locator: &runtime.Locator{
						Runtime:     "tmux",
						Claim:       "legion-ac91849371-ac91849371-900-architect",
						Incarnation: "1618075:268131172",
						Tmux:        &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
					},
					// No round is open over an operator's spawn either: every claim view names
					// one of the four states, and the projection writes this one for a claim no
					// round has reached.
					UpdateState: UpdateStateNone,
				},
			},
		},
		PendingStatusWrites: []PendingStatusWrite{},
		Settings:            SettingsOf(minimalSettingsConfig(t)),
	})
}

// goldenPauseSince is when the goldens' pause began, and goldenPauseEnded when its resume ended it.
var (
	goldenPauseSince = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	goldenPauseEnded = time.Date(2026, 11, 15, 9, 41, 30, 0, time.UTC)
)

// goldenPause is the goldens' pause: the operator paused LEGION for a provider's outage, and, once
// ended, the controller resumed it from its session.
func goldenPause(ended bool) store.Pause {
	p := store.Pause{ID: 7, Project: "LEGION", Since: goldenPauseSince, By: store.PauseActor{Kind: store.PauseByOperator},
		Reason: "the model provider is refusing every turn"}
	if ended {
		p.EndedAt, p.EndedBy = goldenPauseEnded, store.PauseActor{Kind: store.PauseByController, Session: "ses_controller"}
	}
	return p
}

// While the project runs after a pause has ended, the state names that pause and who ended it
// (`lastPause`, decision 25 of dispatch://INFRA-3759) and carries no `paused`; populatedState
// carries the other key, the pause that stands. A state carries one of the two at most, so each has
// a state of its own, built as the state's read builds it (pauseViewOf).
func TestStateLastPauseGolden(t *testing.T) {
	last := pauseViewOf(goldenPause(true))
	golden(t, "state-last-pause.json", State{
		Daemon: DaemonInfo{
			Project:       "LEGION",
			SchemaVersion: 3,
			Boots:         5,
			FirstBootAt:   time.Date(2026, 11, 2, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 11, 15, 8, 30, 0, 0, time.UTC),
		},
		Admission:           Admission{Cap: 2, Active: []string{}, Waiting: []string{}},
		PendingStatusWrites: []PendingStatusWrite{},
		LastPause:           &last,
		Settings:            SettingsOf(minimalSettingsConfig(t)),
	})
}

// stubPauses is a pause service that answers what a golden asks of it: the pause routes are held
// to the real service by TestPauseRoutes, and these goldens pin what the routes write of each
// answer and each refusal, through the routes themselves.
type stubPauses struct {
	pause, resumed      store.Pause
	pauseErr, resumeErr error
	released            int
	open, last          store.Pause
	openFound           bool
	lastFound           bool
	paused              int64
}

func (s stubPauses) Pause(context.Context, store.PauseActor, string) (store.Pause, error) {
	return s.pause, s.pauseErr
}

func (s stubPauses) Resume(context.Context, store.PauseActor) (store.Pause, []record.PauseHeldRow, error) {
	return s.resumed, make([]record.PauseHeldRow, s.released), s.resumeErr
}

func (s stubPauses) OpenPause(context.Context) (store.Pause, bool, error) {
	return s.open, s.openFound, nil
}

func (s stubPauses) LastEndedPause(context.Context) (store.Pause, bool, error) {
	return s.last, s.lastFound, nil
}

func (s stubPauses) Paused() (int64, bool) { return s.paused, s.paused != 0 }

// pauseRouteAnswer is what the operator's route at target answers with svc behind it (nil for a
// daemon with no pause service), held to want and decoded strictly into into, so a key the Go type
// does not name fails here before it reaches a fixture. The server reads a record of its own: the
// operator's spawn reads the claim of the agent on the role's step before the pause refuses it, so
// the refusal names the claim the spawn would have reached.
func pauseRouteAnswer(t *testing.T, svc PauseService, target string, body any, want int, into any) {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewServer("127.0.0.1", 8437, Options{Project: testProject, OperatorToken: testOperatorToken, Pause: svc, Log: quiet,
		Pool: testStore(t).Pool(), Record: record.NewStore()}).Handler
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(http.MethodPost, target, reader)
	req.Header.Set("Authorization", "Bearer "+testOperatorToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != want {
		t.Fatalf("POST %s = %d, want %d; body %s", target, recorder.Code, want, recorder.Body)
	}
	decodeInto(t, recorder, into)
}

// Every answer and refusal of the pause routes has a fixture (dispatch://INFRA-3759), written
// from what the route itself writes: the pause it began; the pause a resume ended, with how many
// held rows it let out; a blank reason; a daemon whose configuration cannot hold a pause, naming
// the setting; a daemon with no workflow, which names no setting; a second pause, naming the pause
// that stands; a resume of a running project, naming the last pause; and a turn refused while the
// project is paused (PROJECT_PAUSED, here the operator's spawn), naming the pause that stands.
func TestPauseRouteGoldens(t *testing.T) {
	const pauseRoute, resumeRoute = "/legion/v1/operator/project/pause", "/legion/v1/operator/project/resume"
	reason := OperatorPauseRequest{Reason: goldenPause(false).Reason}

	var paused ProjectPauseResponse
	pauseRouteAnswer(t, stubPauses{pause: goldenPause(false)}, pauseRoute, reason, http.StatusOK, &paused)
	golden(t, "project-pause.json", paused)

	var resumed ProjectResumeResponse
	pauseRouteAnswer(t, stubPauses{resumed: goldenPause(true), released: 3}, resumeRoute, nil, http.StatusOK, &resumed)
	golden(t, "project-resume.json", resumed)

	var blank Failure
	pauseRouteAnswer(t, stubPauses{pauseErr: pause.ErrReasonRequired}, pauseRoute, OperatorPauseRequest{}, http.StatusBadRequest, &blank)
	golden(t, "project-pause-reason-required.json", blank)

	for name, tc := range map[string]struct {
		svc    PauseService
		target string
		body   any
	}{
		"project-pause-unavailable.json": {stubPauses{pauseErr: &pause.UnavailableError{Setting: pause.SettingTmux}}, pauseRoute, reason},
		"project-pause-no-workflow.json": {nil, pauseRoute, reason},
		"project-pause-refused.json": {stubPauses{pauseErr: fmt.Errorf("pause LEGION: %w",
			&store.AlreadyPausedError{Standing: goldenPause(false)})}, pauseRoute, reason},
		"project-resume-refused.json": {stubPauses{resumeErr: &store.NotPausedError{Project: "LEGION"},
			last: goldenPause(true), lastFound: true}, resumeRoute, nil},
		"project-paused.json": {stubPauses{paused: 7, open: goldenPause(false), openFound: true}, "/legion/v1/operator/claims",
			SpawnRequest{Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer}},
	} {
		var refused PauseFailure
		pauseRouteAnswer(t, tc.svc, tc.target, tc.body, http.StatusConflict, &refused)
		golden(t, name, refused)
	}
}

// golden pins one response's wire shape: Go writes it (`-update`), and
// `packages/contracts/src/legion-api.test.ts` parses it through the schema of the response it is
// and holds the parse equal to the file. A fixture that differs from what Go now writes is stale.
func golden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	encoded = append(encoded, '\n')

	path := goldenPath(name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/api/ -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is stale\n got: %s\nwant: %s", name, encoded, want)
	}
}

// The register response is what the plugin reads its claim, its secret and the task agents its
// role's prompts dispatch from.
func TestRegisterResponseGolden(t *testing.T) {
	golden(t, "register.json", claim.RegisterResponse{
		ClaimToken:   "legion-legion-legion-208-architect",
		Tree:         "LEGION-208",
		Issue:        "LEGION-208",
		Role:         claim.RoleArchitect,
		Generation:   3,
		Secret:       "U3VwZXJ2aXNlZEJ5TGVnaW9u",
		PromptAgents: []string{"deep-worker", "oracle", "plan-gap-analyst", "plan-reviewer", "thermonuclear-code-quality", "thermonuclear-deep-review"},
	})
}

// The ready request is what the plugin posts once its session can be prompted, and again carrying
// the capability report the session measured at an idle tick of the 5 s interval it already runs
// (LEGION-663, INFRA-927): the one request body of the claim routes the contract pins, since the
// report is the one with a shape of its own.
func TestReadyRequestGolden(t *testing.T) {
	golden(t, "ready.json", claim.ReadyRequest{
		ClaimToken: "legion-legion-legion-208-implementer",
		SessionID:  "ses_implementer_208",
		Secret:     "U3VwZXJ2aXNlZEJ5TGVnaW9u",
		Generation: 5,
		Capabilities: &claim.CapabilityReport{
			MeasuredAt: time.Date(2026, 10, 10, 2, 18, 59, 0, time.UTC),
			ElapsedMs:  1200,
			Rows: []claim.CapabilityRow{
				{Name: "subagents", OK: true, Detail: "6 task agents discovered: deep-worker, oracle, plan-gap-analyst, plan-reviewer, thermonuclear-code-quality, thermonuclear-deep-review"},
				{Name: "dispatch-envoy-tools", OK: true, Detail: "dispatch read LEGION-208 answered; envoy_whoami is active"},
				{Name: "github", OK: false, Detail: "gh api user: HTTP 401: Bad credentials"},
			},
		},
	})
}

// The ready-for-update request is the worker's answer to its update round's ask: the claim it
// holds, the generation it answers for, the round the ask it read named — the one round the
// answer counts for — and the head of `legion/<issue>@origin` it reports, which the daemon
// records and verifies nothing of.
func TestReadyForUpdateRequestGolden(t *testing.T) {
	golden(t, "ready-for-update.json", claim.ReadyForUpdateRequest{
		ClaimToken: "legion-legion-legion-208-implementer",
		SessionID:  "ses_implementer_208",
		Secret:     "U3VwZXJ2aXNlZEJ5TGVnaW9u",
		Generation: 5,
		Round:      "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22",
		Head:       "9f1c0a7d4b2e6f8a3c5d1e7b9a0f2c4d6e8b1a35",
	})
}

// The release records a pod reads of its deployment's idle release, built through
// releaseRecordsBody from the store's own rows, so the fixture is what the route writes rather
// than a literal. The store's `Project` is no field of the wire — the route answers one
// project's records — so the rows carry only what is served.
//
// Two rows of each list, because one row of each pins less than the shape, and a third resume:
//   - the releases are a release that suspended two roles and saved a bundle, and one whose
//     stops suspended no claim and whose workspace the check read clean (recordPodRelease
//     counts only the claims this release's own stops suspended, and the store reads an empty
//     `roles` column back as none): `roles` is `[]` and never `null`, and `bundleBytes` is 0
//     rather than absent;
//   - the resumes are a wake whose pod was Ready and whose role took its first turn, which is
//     the row that fills both optional stamps, and a wake of a pod with no release row recorded
//     for it (`release` is 0) that went ten minutes with no turn, so it is `stuck` and its
//     `firstTurnAt` is left out until something observes it; and a wake a project pause held
//     (decision 15 of dispatch://INFRA-3759), which pairs with no release and names its pause in
//     `pause`, the key every other resume leaves out;
//   - the holds are a background-work streak over eleven passes with the kinds its session
//     reported in `detail`, and a teardown's overtaken hold, which reads a hold and not the view
//     behind it and so carries no detail at all (outbox.cancelRelease).
func TestReleaseRecordsResponseGolden(t *testing.T) {
	released := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	golden(t, "release-records.json", releaseRecordsBody(store.PodReleaseRecords{
		Releases: []store.PodRelease{
			{ID: 101, Issue: "LEGION-208", Generation: 3, Roles: []string{"implementer", "tester"},
				At: released, BundleBytes: 4096},
			{ID: 102, Issue: "LEGION-209", Generation: 1, At: released.Add(15 * time.Minute)},
		},
		Resumes: []store.PodResume{
			{ID: 201, Release: 101, Issue: "LEGION-208", Role: "implementer", Kind: "delivery",
				At: released.Add(30 * time.Minute), PodReadyAt: released.Add(31 * time.Minute),
				FirstTurnAt: released.Add(32 * time.Minute)},
			{ID: 202, Issue: "LEGION-207", Role: "architect", Kind: "backstop",
				At: released.Add(45 * time.Minute), PodReadyAt: released.Add(46 * time.Minute), Stuck: true},
			{ID: 203, Issue: "LEGION-210", Role: "implementer", Kind: "pause",
				At: released.Add(50 * time.Minute), PodReadyAt: released.Add(80 * time.Minute),
				FirstTurnAt: released.Add(81 * time.Minute), Pause: 7},
		},
		Holds: []store.PodHold{
			{ID: 301, Issue: "LEGION-210", Reason: "background_work", Detail: "subagent,async-job",
				FirstAt: released.Add(time.Hour), At: released.Add(70 * time.Minute), Passes: 11},
			{ID: 302, Issue: "LEGION-211", Reason: "overtaken",
				FirstAt: released.Add(65 * time.Minute), At: released.Add(65 * time.Minute), Passes: 1},
		},
	}))
}

// A session's report shows on its claim as the names it proved and the gaps it did not, each side
// in the report's order, and both sides are arrays on the wire even when empty: a session that
// proved every row has `"open": []`, not `null`, under the plugin's reader.
func TestCapabilityReportViewPartitionsTheRowsAndNeverMarshalsNull(t *testing.T) {
	measured := time.Date(2026, 10, 10, 2, 18, 59, 0, time.UTC)
	report := capabilities.Report{
		Claim: "legion-legion-legion-208-implementer", Generation: 5, MeasuredAt: measured,
		Locator: runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: "legion-legion-legion-208-implementer", Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5"},
		Rows: []capabilities.Row{
			{Name: capabilities.Subagents, OK: true, Detail: "6 task agents discovered"},
			{Name: capabilities.WebSearch, Detail: "web_search is not an active tool"},
			{Name: capabilities.MCP, OK: true, Detail: "no MCP server is configured"},
			{Name: capabilities.GitHub, Detail: "gh api user: HTTP 401: Bad credentials"},
		},
	}
	want := &CapabilityReportView{
		MeasuredAt:  measured,
		Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5",
		OK:          []string{"subagents", "mcp"},
		Open: []CapabilityGap{
			{Name: "web-search", Detail: "web_search is not an active tool"},
			{Name: "github", Detail: "gh api user: HTTP 401: Bad credentials"},
		},
	}
	if got := CapabilityReportViewOf(report); !reflect.DeepEqual(got, want) {
		t.Fatalf("CapabilityReportViewOf = %#v, want %#v", got, want)
	}

	allOK := CapabilityReportViewOf(capabilities.Report{Rows: []capabilities.Row{{Name: capabilities.Subagents, OK: true}}})
	encoded, err := json.Marshal(allOK)
	if err != nil {
		t.Fatalf("marshal the view: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"open":[]`)) {
		t.Fatalf("view = %s, want an empty open array", encoded)
	}
	noneOK := CapabilityReportViewOf(capabilities.Report{Rows: []capabilities.Row{{Name: capabilities.Subagents, Detail: "no task agent discovered"}}})
	if encoded, err = json.Marshal(noneOK); err != nil {
		t.Fatalf("marshal the view: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"ok":[]`)) {
		t.Fatalf("view = %s, want an empty ok array", encoded)
	}
}

// A session that registered with the controller capability learns the project's controller token
// and role, the capability's generation, and its registration's secret — no tree and no issue.
func TestControllerRegisterResponseGolden(t *testing.T) {
	golden(t, "register-controller.json", ControllerRegisterResponse{
		ClaimToken: "legion-legion-controller",
		Role:       claim.RoleController,
		Generation: 2,
		Secret:     "Q29udHJvbGxlZEJ5TGVnaW9u",
	})
}

// What `legion controller start` fetches with the operator's bearer: the capability and the
// design-gate steps the deployment gates, in the order it gates them.
func TestControllerSecretResponseGolden(t *testing.T) {
	golden(t, "controller-secret.json", ControllerSecretResponse{
		Secret:          "Q2FwYWJpbGl0eUZvclRoZUNvbnRyb2xsZXI",
		DesignGateSteps: []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec},
	})
}

// What `legion controller start` fetches for the review App's read-only tokens, one per owner,
// each rendered as the two files `gh` reads, with the owner accounts that App is installed on.
func TestControllerCredentialResponseGolden(t *testing.T) {
	expiresAt := time.Date(2026, 9, 22, 10, 15, 2, 0, time.UTC)
	acme := ghconfig.Render("ghs_placeholder_acme_token", "review", expiresAt)
	sjawhar := ghconfig.Render("ghs_placeholder_sjawhar_token", "review", expiresAt)
	golden(t, "controller-github-credential.json", ControllerCredentialResponse{
		Owners: map[string]ControllerOwnerCredential{
			"acme":    {Hosts: acme.Hosts, Config: acme.Config},
			"sjawhar": {Hosts: sjawhar.Hosts, Config: sjawhar.Config},
		},
		Default:   "acme",
		App:       "review",
		ExpiresAt: expiresAt.UTC().Format(timeFormat),
		Installed: []string{"acme", "sjawhar"},
	})
}

// Every refusal a route answers is one sentence under `error`.
func TestErrorResponseGolden(t *testing.T) {
	golden(t, "error.json", claim.SameAgentRefusal)
}

// operatorClaim carries every optional an operator's view of a claim has.
func operatorClaim() OperatorClaim {
	delivered := time.Date(2026, 9, 22, 9, 31, 4, 0, time.UTC)
	lastSendFailure := time.Date(2026, 9, 22, 9, 31, 2, 0, time.UTC)
	return OperatorClaim{
		Token:       "legion-legion-legion-209-implementer",
		Tree:        "LEGION-208",
		Issue:       "LEGION-209",
		Role:        claim.RoleImplementer,
		Generation:  2,
		State:       "working",
		Session:     "ses_implementer_209",
		SessionFile: "/srv/legion/state/home/.omp/agent/sessions/ses_implementer_209.jsonl",
		Locator: &runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       "legion-legion-legion-209-implementer",
			Incarnation: "40217:9551230",
			Tmux:        &runtime.TmuxLocator{Window: "@7", Pane: "%23"},
		},
		Budgets:         BudgetsView{LaunchFailures: 1, Deaths: 1, PromptFailures: 0, PromptRetires: 0},
		UncertainStreak: 0,
		Pending: &DeliveryView{
			ID:          "OVXA3ZC6BPIKTOQHKXHRZ7TYVB",
			Task:        "Implement the plan recorded on the issue, which `dispatch handoff-read --issue LEGION-208 --phase plan` reads.",
			QueuedAt:    time.Date(2026, 9, 22, 9, 30, 58, 0, time.UTC),
			DeliveredAt: &delivered,
			ConfirmedAt: &delivered,
			Interrupted: true,
			// The record of the sends that failed, carried here beside the confirmation the way
			// this fixture carries every other optional at once. A live delivery shows one or
			// the other: the send that confirms a task clears all three (supervise's
			// confirmCarrying).
			SendFailures:      2,
			LastSendFailureAt: &lastSendFailure,
			LastSendError:     "prompt the agent: context deadline exceeded",
		},
		SuspensionHeld: true,
		// A plugin round an earlier daemon opened over this claim, whose process speaks a plugin
		// contract this daemon does not. This release opens none — the stale-plugin lane asks
		// its worker nothing and relaunches instead — and a boot takes every claim out of one it
		// finds (daemon's endPluginRounds), so the shape is still on the wire in the moments
		// before that pass reaches the claim, which is why the fixture pins it.
		UpdateRound: "plugin:43",
		UpdateHead:  "4c1b8e2a6f9d3047b5e1c8a2f6d940b3e7c5a128",
		// What the daemon last saw this agent do: an `eval` call whose start it received and
		// whose end it has not, which is the shape a turn that stopped moving leaves behind. The
		// arguments are measured and fingerprinted, never kept (supervise.StepRecord).
		Step: StepViewOf(supervise.StepRecord{
			At:            time.Date(2026, 9, 22, 9, 31, 22, 0, time.UTC),
			TurnStartedAt: time.Date(2026, 9, 22, 9, 31, 6, 0, time.UTC),
			Tool:          "eval",
			CallID:        "toolu_impl_209_4",
			ArgsBytes:     412,
			ArgsHash:      "c40d9e1b7a52f368",
			StartedAt:     time.Date(2026, 9, 22, 9, 31, 22, 0, time.UTC),
		}),
	}
}

// What the spawn, deliver, suspend, resume, and stop routes answer.
func TestOperatorClaimGolden(t *testing.T) {
	golden(t, "operator-claim.json", operatorClaim())
}

// What a successful forget-session answers: the failed claim whose session the operator
// abandoned, carrying why it failed. This is the fixture that holds the operator wire's failure
// record, because operatorClaim's is working and a working claim has none: the launch that ran
// it again cleared it (supervise's launchNow), which is why the record stands on a failed claim
// alone.
func TestForgetSessionOperatorClaimGolden(t *testing.T) {
	failedAt := time.Date(2026, 9, 22, 9, 28, 41, 0, time.UTC)
	golden(t, "operator-forget-session.json", OperatorClaim{
		Token:                "legion-legion-legion-208-architect",
		Tree:                 "LEGION-208",
		Issue:                "LEGION-208",
		Role:                 claim.RoleArchitect,
		Generation:           4,
		State:                "failed",
		ForgottenSession:     "ses_architect",
		ForgottenSessionFile: "/sessions/ses_architect.jsonl",
		Budgets:              BudgetsView{LaunchFailures: 3},
		UncertainStreak:      0,
		PluginContract:       38,
		Failure: &ClaimFailureView{
			Cause:    supervise.CauseLaunchRefused,
			Detail:   "launch failures ran out: resume the pane: the session table holds no such session",
			At:       &failedAt,
			Attempts: BudgetsView{LaunchFailures: 3},
		},
	})
}

// What the list route answers: a queued claim carries no locator and no delivery yet. The
// pod-update record is left out of the value this marshals, so the fixture is also the proof
// that OperatorClaims.MarshalJSON writes the zero record for a nil field and the wire carries
// the object either way. The third claim is between two launch tries the runtime refused
// because the shared public leak check could not be installed in its pod: it is launching with
// no process, its two refused tries charged, and the sentence of the latest beside them.
func TestOperatorClaimsGolden(t *testing.T) {
	golden(t, "operator-claims.json", OperatorClaims{Claims: []OperatorClaim{
		{
			Token:      "legion-legion-legion-208-architect",
			Tree:       "LEGION-208",
			Issue:      "LEGION-208",
			Role:       claim.RoleArchitect,
			Generation: 0,
			State:      "queued",
		},
		operatorClaim(),
		{
			Token:          "legion-legion-legion-211-tester",
			Tree:           "LEGION-211",
			Issue:          "LEGION-211",
			Role:           claim.RoleTester,
			Generation:     3,
			State:          "launching",
			Budgets:        BudgetsView{LaunchFailures: 2},
			LeakCheckCause: "the shared public leak check could not be installed: the pod's launcher did not answer the install of leak-check-v3 within 2m0s",
		},
	}})
}

// An issue the daemon has admitted but not yet given a worker still has to answer the plugin's
// reader: `workers` is an object, never `null`.
func TestIssueWithoutWorkersMarshalsAnEmptyObject(t *testing.T) {
	encoded, err := json.Marshal(Issue{Key: "LEGION-209", Phase: phase.Held})
	if err != nil {
		t.Fatalf("marshal issue: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"workers":{}`)) {
		t.Fatalf("issue = %s, want an empty workers object", encoded)
	}
}

// Every Task 3.10 route has a fixture. The TypeScript Go-daemon client parses these exact
// responses, so adding a route cannot quietly leave its wire shape undocumented.
func TestTask310RouteGoldens(t *testing.T) {
	golden(t, "grant.json", GrantResponse{
		GrantID: "grant-for-one-command", ExpiresAt: "2026-09-23T12:01:00Z",
	})
	golden(t, "handoff-complete.json", HandoffCompleteResponse{})
	golden(t, "issue-status.json", IssueStatusResponse{})
	golden(t, "fresh-session.json", FreshSessionResponse{})
	golden(t, "compact.json", CompactResponse{Reason: "Nothing to compact (session too small)"})
	golden(t, "gate-register.json", GateRegisterResponse{})
	golden(t, "wave-release.json", WaveReleaseResponse{Released: []string{"LEGION-209"}})
	golden(t, "phase-backward.json", PhaseBackwardResponse{})
	golden(t, "phase-retry.json", PhaseRetryResponse{})
	golden(t, "signoff.json", SignOffResponse{})
	golden(t, "root-close.json", EmptyResponse{})
	golden(t, "child-rerun.json", EmptyResponse{})
}

// The two routes of the owner's plan (contract 219) each have a fixture, named for its route as
// the routes above are. register_plan answers the plan it took, the tasks its version added and
// each task with the steps its block names as the plan writes them (registeredPlan), and stop_task
// answers the empty object of a fact route. `@legion/contracts` parses both, so a field the daemon
// adds to either is caught there.
func TestOwnerPlanRouteGoldens(t *testing.T) {
	golden(t, "plan-register.json", PlanRegisterResponse{Plan: registeredPlan(plan.Plan{
		ArtifactID: "8f14e45f-ceea-467f-a0e6-1d8b2f8c9d11",
		Version:    4,
		Tasks: []plan.Task{{
			ID:    "a",
			Steps: []plan.Step{{Role: "implementer"}, {Role: "reviewer"}},
		}},
	}, []string{"a"})})
	golden(t, "task-stop.json", EmptyResponse{})
}

// The table of answers a plugin parses (answers.go): one row per response schema, the routes
// that answer it, and the contract that answer arrived at. No route answers this fixture. It is
// written out so that the plugin's own side reads the same rows the daemon shapes by.
func TestAnswersGolden(t *testing.T) {
	golden(t, "answers.json", Answers)
}

// The table of enumerated fields a plugin parses strictly (answers.go): one row per field, with
// each value and the contract it arrived at. No route answers this fixture either. It is
// written out beside answers.json so that the plugin's own side holds its enumerations to the
// daemon's: `packages/contracts/src/legion-api.test.ts` reads this file, holds each row's values
// equal to the members of the `z.enum` it reads at that row's key path, and fails a `z.enum` it
// reads at a path no row declares. A value added on one side and not the other is red rather
// than silent.
func TestEnumeratedFieldsGolden(t *testing.T) {
	golden(t, "enumerated-fields.json", EnumeratedFields)
}

// The turn outcome's answer is what a producer's report of one turn comes back as: the run of
// refused turns the claim's reports now end with, and `failed` (claim.TurnOutcomeResponse says
// what it holds). It is a golden because `@legion/contracts` reads it (LegionTurnOutcomeResponse),
// and every answer schema that module exports is held to one.
func TestTurnOutcomeResponseGolden(t *testing.T) {
	golden(t, "turn-outcome.json", claim.TurnOutcomeResponse{RefusedRun: 3})
}
