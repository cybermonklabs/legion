package api

//ci:reads packages/contracts/fixtures/daemon-api/* golden and stateGolden (testgolden.Check, path from apifixture.Path)

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
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/apifixture"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testgolden"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden fixtures this package pins")

// settingsFixtureFile is the maximal kubernetes legion.yaml the settings tests share with the state
// fixtures composed over it, so a key added here moves them: internal/daemon's
// TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds reads it from this file's source
// (apiSettings) for state.json and the other fixtures whose seeds load it, and
// state-narrowed-repositories.json (TestStateNarrowedRepositoriesGolden) is written from
// populatedState, which reads state.json back. Every key the loader models is set, and every value
// a credential, its source or its file could be is a planted fixture a test can look for in the
// wire bytes (a PEM that is no key, a DSN password that opens nothing, `*_file` paths, the
// kubeconfig, the session DSN secret's key, each provider key's store key, the pod's env values,
// volume, Secret and ServiceAccount names, userinfo on `envoy_url` and the first `nats_urls` entry,
// which the settings mask, and userinfo and a query on `plugins.pi_envoy`'s tarball URL, which the
// settings mask and drop while `pi_legion` pins the version form). Every path the settings emit
// (`state_dir`, `instructions`, `private_name_rules_file`, the hook's `prompt_file`) is a fixed
// absolute literal, so the fixtures do not churn with the temporary directory the file is written
// under.
// Hosts are placeholders.
const settingsFixtureFile = `project: LEGION
port: 13370
bind: 0.0.0.0
advertise_host: legion-daemon.internal.example
postgres_dsn: postgres://legion:fixture-dsn-password@postgres.internal.example:5432/legion
state_dir: /var/lib/legion
admission_cap: 6
admission_p0_bound: 4
daemon_url: http://legion-daemon.internal.example:13370
worker_stream_port: 13371
provider_keys: {ANTHROPIC_API_KEY: ANTHROPIC_API_KEY_FIXTURE, GEMINI_API_KEY: GEMINI_API_KEY_FIXTURE}
instructions: /etc/legion/deployment-instructions.md
private_name_rules_file: /etc/legion/private-names.toml
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
nats_urls: ["nats://fixture-nats-user@nats.internal.example:4222", nats://nats-2.internal.example:4222]
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
  - {from: sjawhar/legion, to: cybermonklabs/legion}
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
    test_postgres: true
    test_postgres_disk: 8Gi
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
// default, which is what the Stage 3 and operator-claim fixtures and the defaults test show.
const minimalSettingsFile = `project: LEGION
postgres_dsn: postgres://legion:fixture-dsn-password@127.0.0.1:5432/legion
state_dir: /var/lib/legion
`

// settingsFixtureConfig is settingsFixtureFile loaded.
func settingsFixtureConfig(t *testing.T) config.Config {
	t.Helper()
	return apifixture.LoadSettings(t, settingsFixtureFile)
}

// minimalSettingsConfig is minimalSettingsFile loaded.
func minimalSettingsConfig(t *testing.T) config.Config {
	t.Helper()
	return apifixture.LoadSettings(t, minimalSettingsFile)
}

// populatedState is the populated state as internal/daemon's state source composes it over its
// seed (TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds), read back from state.json byte
// for byte, so no field of it is lost or changed on the way into this package's tests. The file
// carries no `contract` mark: the mark is the route's own (shaping's stateFor), and both of its
// numbers name this daemon's own contract, which an entry moves, so the composition takes it out
// before it writes, and every route this package's tests serve the state through writes it again.
func populatedState(t *testing.T) State {
	t.Helper()
	return composedFixture(t, apifixture.Path("state.json"))
}

// pausedState is the state of the populated record ten seconds into a pause, as internal/daemon's
// state source composes it, read back from its fixture byte for byte, which the tests of the
// pause's keys read.
func pausedState(t *testing.T) State {
	t.Helper()
	return composedFixture(t, apifixture.Path("state-paused.json"))
}

// operatorClaimState is the state of an operator's spawn on an issue the workflow keeps
// no record of, read back from its fixture: its architect is the one claim of the fixtures in a
// tmux pane.
func operatorClaimState(t *testing.T) State {
	t.Helper()
	return composedFixture(t, apifixture.Path("state-operator-claim.json"))
}

// composedFixture is the state internal/daemon's state source composed at path, held to read back
// byte for byte.
func composedFixture(t *testing.T, path string) State {
	t.Helper()
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run %s): %v", path, apifixture.StateFixturesRewrite, err)
	}
	var state State
	if err := json.Unmarshal(written, &state); err != nil {
		t.Fatalf("decode %s (run %s): %v", path, apifixture.StateFixturesRewrite, err)
	}
	keepNullPriorities(t, path, written, state)
	if again := testgolden.Encode(t, path, state); !bytes.Equal(again, written) {
		line, held, read := testgolden.FirstDifference(written, again)
		t.Fatalf("%s does not read back as it was written (run %s): line %d holds %q, and the state read from it writes %q",
			path, apifixture.StateFixturesRewrite, line, held, read)
	}
	return state
}

// keepNullPriorities puts back each `priority` the file at path writes as null, on an issue and on
// its waitingLine, and each issue's `resolvedPriority` it writes as null. Those are the three keys
// of the state the daemon writes as null rather than leaving out: an issue the workflow records
// with no Dispatch priority, a line member neither it nor its tree root has one for, and an issue
// Dispatch resolves to a known none (PriorityView's MarshalJSON). encoding/json reads a null into
// a pointer as no pointer at all, so a decode alone drops the key, and the read-back would fail on
// a value the daemon serves. Only a key the file holds is put back; an absent one stays absent.
func keepNullPriorities(t *testing.T, path string, written []byte, state State) {
	t.Helper()
	var held struct {
		Issues map[string]struct {
			Priority         json.RawMessage `json:"priority"`
			ResolvedPriority json.RawMessage `json:"resolvedPriority"`
			WaitingLine      *struct {
				Priority json.RawMessage `json:"priority"`
			} `json:"waitingLine"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(written, &held); err != nil {
		t.Fatalf("decode the priorities of %s: %v", path, err)
	}
	null := []byte("null")
	for key, keys := range held.Issues {
		issue := state.Issues[key]
		if bytes.Equal(keys.Priority, null) {
			issue.Priority = new(PriorityView(record.NoPriority))
		}
		if bytes.Equal(keys.ResolvedPriority, null) {
			issue.ResolvedPriority = new(PriorityView(record.NoPriority))
		}
		if keys.WaitingLine != nil && issue.WaitingLine != nil && bytes.Equal(keys.WaitingLine.Priority, null) {
			issue.WaitingLine.Priority = new(PriorityView(record.NoPriority))
		}
		state.Issues[key] = issue
	}
}

// The state route's body for a caller whose plugin is at the contract before
// repositoriesLiveContract, asking from a pod whose claims record it and stating it in
// `Legion-Plugin-Contract`, as every plugin from shapedAnswersContract on does: the state golden's
// own state, asked through the route itself. The caller is served at the recorded contract at or
// below the one it states (recordedContractAtOrBelow), which the mark's `caller` names: its
// `repositories` block in the shape contract 152 gave it, written from the same entries
// (withoutRepositoriesLive), each assignment without its `plan` (withoutAssignmentPlans,
// assignmentPlanContract's row), no `intakeGiveUps` (withoutIntakeGiveUps,
// intakeGiveUpsContract's row), no issue's `podHold` (withoutPodHold, podHoldContract's row), no
// root's `treeRuns` and no gated steps of a root a step of which stands on an ancestor's basis
// (withoutGateBasis, gateBasisContract's row), the three other rows of stateNarrowings above the
// contract it is served at, no issue's `priority` (withoutPriority, priorityContract's row), no
// claim view's `lastDeathAt` or `lastStartFailureAt` (withoutCauseTimes, causeTimesContract's row)
// and no `dispatchCredential` (withoutDispatchCredential, dispatchCredentialContract's row),
// and every other key as a caller at shapedAnswersContract is served it, since no other
// row of stateNarrowings stands between shapedAnswersContract and repositoriesLiveContract.
// shapedAnswersContract's row stands at or below the contract it is served at, which is why the
// golden carries `lowestPluginContract` and each claim view's `updateState`; the populated state
// holds no pause, so it carries no `paused`. The mark's `daemon` names this daemon's own contract,
// which an entry moves, so it is taken out before the golden is compared or written
// (apifixture.WithoutContractMarks) and held to that number here, as internal/daemon's
// TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds takes out state.json's marks with the
// same strip; the `caller` is 199, which no entry moves, and stays.
// `packages/contracts/src/legion-api.test.ts` parses it with a copy of contract 152's schema of
// the block, so the narrowing is held to the reader it is written for, and its drift gate holds
// every key of the file to that copy.
func TestStateNarrowedRepositoriesGolden(t *testing.T) {
	server := stateServer(t, populatedState(t), podClaims(repositoriesLiveContract-1))
	body, taken := apifixture.WithoutContractMarks(t, stateStating(t, server, statePodAddr+":41000", repositoriesLiveContract-1), "daemon")
	if want := map[string]int{"daemon": DaemonAPIVersion()}; !maps.Equal(taken, want) {
		t.Errorf("the state served below repositoriesLiveContract has the daemon mark %v, want %v, this daemon's own contract", taken, want)
	}
	stateGolden(t, "state-narrowed-repositories.json", body)
}

// One issue's record, what `GET /legion/v1/issues/{key}/record` answers: the very `api.Issue`
// the whole-project body holds under `issues.LEGION-208`, which a test of the route holds the
// two to, field for field.
func TestIssueRecordGolden(t *testing.T) {
	stateGolden(t, "issue-record.json", populatedState(t).Issues[recordGoldenIssue])
}

// The observed view's golden (observed-state.json): what `GET /legion/v1/state?view=observed`
// answers over the state state.json holds (observedOf), the same daemon block, settings and
// admission block, and each issue with the members Dispatch observes (ObservedIssue), so
// `legion-api.test.ts` holds the observed schema to the body a deployment with this state serves.
func TestObservedStateGolden(t *testing.T) {
	stateGolden(t, "observed-state.json", observedOf(populatedState(t)))
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
// operator claim routes and to four of the six agent actions alone
// (AgentActionResponse), which no plugin below the contract that added them calls; the guard
// beside `legion-api.test.ts`'s schema tests holds the same property on the schema side.
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
// does not name fails here before it reaches a fixture.
func pauseRouteAnswer(t *testing.T, svc PauseService, target string, body any, want int, into any) {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewServer("127.0.0.1", 8437, Options{Project: testProject, OperatorToken: testOperatorToken, Pause: svc, Log: quiet}).Handler
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
	reason := OperatorPauseRequest{Reason: apifixture.Pause(false).Reason}

	var paused ProjectPauseResponse
	pauseRouteAnswer(t, stubPauses{pause: apifixture.Pause(false)}, pauseRoute, reason, http.StatusOK, &paused)
	golden(t, "project-pause.json", paused)

	var resumed ProjectResumeResponse
	pauseRouteAnswer(t, stubPauses{resumed: apifixture.Pause(true), released: 3}, resumeRoute, nil, http.StatusOK, &resumed)
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
			&store.AlreadyPausedError{Standing: apifixture.Pause(false)})}, pauseRoute, reason},
		"project-resume-refused.json": {stubPauses{resumeErr: &store.NotPausedError{Project: "LEGION"},
			last: apifixture.Pause(true), lastFound: true}, resumeRoute, nil},
		"project-paused.json": {stubPauses{paused: 7, open: apifixture.Pause(false), openFound: true}, "/legion/v1/operator/claims",
			SpawnRequest{Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer}},
	} {
		var refused PauseFailure
		pauseRouteAnswer(t, tc.svc, tc.target, tc.body, http.StatusConflict, &refused)
		golden(t, name, refused)
	}
}

// golden pins one response's wire shape: Go writes it (`-update`), and
// `packages/contracts/src/legion-api.test.ts` parses it through the schema of the response it is
// and holds the parse equal to the file. A fixture that differs from what Go now writes is stale
// (testgolden.Check).
func golden(t *testing.T, name string, value any) {
	t.Helper()
	testgolden.Check(t, apifixture.Path(name), value, *updateGolden, "go test ./internal/api/ -run Golden -update")
}

// stateGolden is golden for a fixture written from the populated state, whose failure names the
// two commands that write the state and the fixture, in order (apifixture.StateFixturesRewrite).
func stateGolden(t *testing.T, name string, value any) {
	t.Helper()
	testgolden.Check(t, apifixture.Path(name), value, *updateGolden, apifixture.StateFixturesRewrite)
}

// frozenGolden holds the frozen fixture name, the records of the numbered entries, to value in
// golden's form, and writes it under no flag: `-update` writes an entry's fixture, never a frozen
// record (TestEntryFixtures). A frozen record changes only as a correction of a record that was
// wrong, which is a hand edit the contract check refuses without a `Correction:` paragraph in the
// entry's text.
func frozenGolden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	want, err := os.ReadFile(apifixture.Path(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if !bytes.Equal(append(encoded, '\n'), want) {
		t.Fatalf("%s is not in the order and form its records are written in (by contract, then by its answer or route, indented by two spaces); no flag writes it, so put it back in that form by hand", name)
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
	golden(t, "issue-pod-update.json", PodUpdateResponse{
		Round:   "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22",
		Reasons: "the project's controller asked for the replacement of this pod",
		Opened:  true,
	})
	golden(t, "fresh-session.json", FreshSessionResponse{})
	golden(t, "compact.json", CompactResponse{Reason: "Nothing to hand off (already compacted)"})
	golden(t, "agent-stop.json", AgentActionResponse{State: string(supervise.StateRetired)})
	golden(t, "agent-retry.json", AgentActionResponse{State: string(supervise.StateLaunching)})
	golden(t, "agent-suspend.json", AgentActionResponse{
		State: string(supervise.StateSuspended), SuspendedBy: string(supervise.SuspendedByController),
	})
	golden(t, "agent-resume.json", AgentActionResponse{State: string(supervise.StateLaunching)})
	golden(t, "gate-register.json", GateRegisterResponse{})
	golden(t, "wave-release.json", WaveReleaseResponse{Released: []string{"LEGION-209"}})
	golden(t, "phase-backward.json", PhaseBackwardResponse{})
	golden(t, "phase-retry.json", PhaseRetryResponse{})
	golden(t, "signoff.json", SignOffResponse{})
	golden(t, "root-close.json", EmptyResponse{})
	golden(t, "child-rerun.json", EmptyResponse{})
}

// The two routes of the owner's plan (contract 219) each have a fixture, named for its route as
// the routes above are. register_plan answers the plan it took, each task with the steps its block
// names as the plan writes them (registeredPlan), and stop_task answers the empty object of a
// fact route. `@legion/contracts` parses both, so a field the daemon adds to either is caught
// there.
func TestOwnerPlanRouteGoldens(t *testing.T) {
	golden(t, "plan-register.json", PlanRegisterResponse{Plan: registeredPlan(plan.Plan{
		ArtifactID: "8f14e45f-ceea-467f-a0e6-1d8b2f8c9d11",
		Version:    4,
		Tasks: []plan.Task{{
			ID:    "a",
			Steps: []plan.Step{{Role: "implementer"}, {Role: "reviewer"}},
		}},
	})})
	golden(t, "task-stop.json", EmptyResponse{})
}

// The table of answers a plugin parses (answers.go): one row per response schema, the routes
// that answer it, and the contract that answer arrived at. No route answers this fixture. It is
// written out so that the plugin's own side reads the same rows the daemon shapes by. It holds the
// numbered entries' rows alone (frozenAnswers): the rows an entry adds are in that entry's fixture
// (entryFixture's answerRows), so two entries edit no line of it, and TestEntryFixtures holds
// this file and those rows to the build's Answers().
func TestAnswersGolden(t *testing.T) {
	golden(t, "answers.json", frozenAnswers)
}

// The table of enumerated fields a plugin parses strictly (answers.go): one row per field, with
// each value and the contract it arrived at. No route answers this fixture either. It is
// written out beside answers.json so that the plugin's own side holds its enumerations to the
// daemon's: `packages/contracts/src/legion-api.test.ts` reads this file with the rows of the entry
// fixtures added (`daemon-api-records.ts`), holds each row's values equal to the members of the
// `z.enum` it reads at that row's key path, and fails a `z.enum` it reads at a path no row
// declares. A value added on one side and not the other is red rather than silent. It holds the
// numbered entries' rows alone (enumeratedFields()), as answers.json does, and TestEntryFixtures
// holds it and the entries' rows to the build's EnumeratedFields().
func TestEnumeratedFieldsGolden(t *testing.T) {
	golden(t, "enumerated-fields.json", enumeratedFields())
}

// The turn outcome's answer is what a producer's report of one turn comes back as: the run of
// refused turns the claim's reports now end with, and `failed` (claim.TurnOutcomeResponse says
// what it holds). It is a golden because `@legion/contracts` reads it (LegionTurnOutcomeResponse),
// and every answer schema that module exports is held to one.
func TestTurnOutcomeResponseGolden(t *testing.T) {
	golden(t, "turn-outcome.json", claim.TurnOutcomeResponse{RefusedRun: 3})
}
