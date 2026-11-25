package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// contractShapesFixture records what this daemon serves every contract it supports: for each
// contract of SupportedContracts, for each answer that arrived at or below it, the key paths of
// the body a caller at that contract reads and, for each field of that answer a plugin parses
// with a strict enumeration (answers.go's EnumeratedFields), the values that contract admits.
// It is the program that makes an omission impossible. A shape change with no serving-down
// moves a body away from the record its own contract holds, and the cases below fail by the
// contract and by the key or the value.
//
// Keys and values are both recorded because a strict reader refuses a body for either: it
// refuses a key it does not know, and it refuses a value its enumeration does not name, so a
// widened field is a shape change exactly as an added key is (answers.go's EnumeratedFields).
//
// A record holds keys and the domains of the fields that table declares, so a serving-down that
// moves neither is outside what it can see. The one serving-down that moves no key is
// withoutUnadmittedStrandedStates, which drops the entries of `strandedClaims` whose `state`
// arrived above the caller's contract, today the word contract 56 added; the table does declare
// that field, so a record below 56 admits two values where this daemon's own admits three, and a
// body carrying the third below 56 is red here. state_caller_test.go holds the dropping itself,
// entry by entry.
//
// A settings key path is recorded only where a reader types it, as answers.go's typedSettings
// declares and withoutUntypedSettings applies.
const contractShapesFixture = "contract-shapes.json"

// keyPathRule is how a record names one key. The same rule stands in the fixture's entry in
// `packages/contracts/src/legion-api.test.ts`.
//
// A key path is one key of a body, written as the dotted path from that body's root. A map's key
// and a list's index are both written as `*`, so `issues.*.pullRequest.fixAttempts` is one path,
// whichever issues the body holds and whichever of them carries a pull request. A map is told
// from a struct by the Go type the body is marshalled from, never by the shape of the JSON: both
// are objects on the wire.
const keyPathRule = "a key path is one key of a body, written as the dotted path from the body's root, " +
	"with a map's key and a list's index written as `*`: `issues.*.pullRequest.fixAttempts` is one path"

// valueDomainRule is how a record names the values of one enumerated field. The same rule stands
// in the fixture's entry in `packages/contracts/src/legion-api.test.ts`.
//
// A domain is held under the field's own key path, and it is the sorted set of the values whose
// arrival (answers.go's EnumeratedFields) is at or below that contract: what a reader there
// admits, and not what the fixture bodies happen to carry.
const valueDomainRule = "a value domain is held under the key path of a field answers.go's EnumeratedFields declares, " +
	"and holds the sorted values whose arrival is at or below that contract"

// bootstrapMessage is what a missing fixture reads as: how it is written, and the rules its
// records are read by.
func bootstrapMessage() string {
	return fmt.Sprintf("%s is not written yet: run `go test ./internal/api/ -update` once to bootstrap it, where %s, and %s",
		contractShapesFixture, keyPathRule, valueDomainRule)
}

// contractShape is one record of the fixture: one answer as one contract reads it.
//
// NoBody, KeyPaths and Domains are all written on every record, so an answer that writes no body
// at all (`claims/ready` and its three siblings, which answer 204) reads differently from one
// whose body has no key (the empty object the fact routes answer). The first is
// `"noBody": true` with no key path, the second `"noBody": false` with no key path. An answer
// with no enumerated field of its own writes `"domains": {}`.
type contractShape struct {
	Contract int      `json:"contract"`
	Answer   AnswerID `json:"answer"`
	NoBody   bool     `json:"noBody"`
	KeyPaths []string `json:"keyPaths"`
	// Domains is the value domain of each enumerated field of the answer, by the field's key
	// path (valueDomainRule). It is read from answers.go's table and not from the bodies: a
	// record says what the contract admits, which is more than any sample carries.
	Domains map[string][]string `json:"domains"`
}

// shapeKey is one answer at one contract, the pair a record is held by.
type shapeKey struct {
	contract int
	answer   AnswerID
}

// answerShape is what one answer carries at one contract: whether it writes a body at all, the
// sorted key paths of every body its routes write at that contract, and the sorted values those
// bodies carry at each key path the table declares enumerated.
type answerShape struct {
	noBody bool
	paths  []string
	values map[string][]string
}

// answerFixture is one answer as this package holds it: the Go types its bodies are marshalled
// from, and the bodies themselves.
//
// The bodies are maximal: a record describes every key the answer can carry at a contract, so a
// body that leaves an optional key out would record a shape no reader is held to. Where an
// answer's golden under `packages/contracts/fixtures/daemon-api/` pins one real answer and that
// answer carries fewer keys than the shape allows, the body here is the richer one and the
// comment says so.
type answerFixture struct {
	roots  []reflect.Type
	bodies []any
}

// shape is every key path the answer's roots declare, and which of their nodes are maps. An
// answer with more than one root (the refusal) reads the union of the two.
func (f answerFixture) shape(t *testing.T) declaredShape {
	t.Helper()
	shape := declaredShape{paths: map[string]declaredField{}, maps: map[string]bool{}, opaque: map[string]bool{}}
	for _, root := range f.roots {
		walkDeclaredType(t, root, "", declaredField{}, shape, nil)
	}
	return shape
}

// marshalled is the fixture's bodies as the bytes json.Marshal writes for them, unshaped. The two
// tests that ask what the fixtures carry read these through collectedShape
// (TestEveryDeclaredFieldOfAnAnswerIsRealisedByItsFixture and
// TestEveryDeclaredValueAndKeyPathIsCarriedByAFixtureBody). id names the answer in the failure.
func (f answerFixture) marshalled(t *testing.T, id AnswerID) [][]byte {
	t.Helper()
	encoded := make([][]byte, 0, len(f.bodies))
	for _, body := range f.bodies {
		encodedBody, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal a body of the %s answer: %v", id, err)
		}
		encoded = append(encoded, encodedBody)
	}
	return encoded
}

// declaredShape is every key path a Go type declares, each with the json field that declares it;
// the paths of the nodes that are Go maps, whose keys a key path writes as `*`; and the paths of
// the nodes that carry a document of another owner's, which a key path does not read into.
type declaredShape struct {
	paths  map[string]declaredField
	maps   map[string]bool
	opaque map[string]bool
}

// declaredField is one json field of one Go type. Two key paths of one field — `architect.
// capabilities` and `workers.*.claim.capabilities`, which are both ClaimView.Capabilities — are
// one field, and a fixture that realises either realises the field. A map's value and a list's
// element keep the field of the key that holds them, so `nats_urls` and `nats_urls.*` are
// Settings.NatsURLs both.
type declaredField struct {
	typ  string
	name string
}

func (f declaredField) String() string { return f.typ + "." + f.name }

// shapeSettingsFile is a tmux legion.yaml that sets every key the maximal kubernetes file of
// this package (settingsFixtureFile) leaves unset: the Oh My Pi invocation and its launch
// prefix, a role's stall backstop, a project's memory block and an experiment of its own with
// every field an experiment has, and the `runtime.tmux` block itself, which no file of the
// kubernetes runtime carries. The maximal file sets no `runtime.kubernetes.sandbox` block
// either, and sandboxSettingsBlock is one, so the two files and that block write every key the
// settings block has.
const shapeSettingsFile = `project: SHAPES
postgres_dsn: postgres://legion:fixture-dsn-password@127.0.0.1:5432/legion
state_dir: /var/lib/legion
omp_invocation: /opt/legion/bin/omp
omp_launch_prefix: [/usr/bin/env, OMP_FIXTURE=1]
worker_stall_backstop_hours: {implementer: 6, tester: 8}
role_config_files:
  implementer: [/etc/legion/implementer-overlay.yml]
projects:
  SHAPES:
    memory:
      mode: random
      config_files: [/etc/legion/memory-overlay.yml]
      instructions: /etc/legion/memory-instructions.md
    experiments:
      plan-length:
        mode: on
        on_variant: short
        unit: issue
        roles: [planner, implementer]
        variants:
          - {name: short, weight: 2, config_files: [/etc/legion/short-plan.yml], instructions: /etc/legion/short-plan.md, plan: owner}
          - {name: long, plan: default}
github_apps:
  implement:
    app_id: "1001"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
    installations: {acme: "2002"}
  review:
    app_id: "1002"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
    installations: {acme: "2003"}
runtime:
  tmux:
    pane_env: {FIXTURE_PANE_FLAG: fixture-pane-value}
    pi_config_files: [/etc/legion/pane-overlay.yml]
    take_over: kubernetes
`

// sandboxSettingsBlock is a `runtime.kubernetes.sandbox` block that sets every member the
// settings show (version.go's 151) but `type`, which they show as `gvisor` when the file names
// none: a canary list, and a microvm block with its compute class, its storage class and both
// members of its metadata endpoint. It is appended to settingsFixtureFile, as the settings tests
// append each block of theirs to it (TestSettingsOfTheSandboxShowsEachMember), rather than
// written into that file, where a block of its own would be a second `sandbox` key beside each
// one those tests append. The third state body carries it (stateCarryingEveryDeclaredValue), so
// the fixture bodies realise the microvm block and its keys, which no body of the maximal file
// alone carries (TestEveryDeclaredFieldOfAnAnswerIsRealisedByItsFixture). No record holds them:
// no reader types a key of the block below its top level (withoutUntypedSettings). Names and
// hosts are placeholders.
const sandboxSettingsBlock = `    sandbox:
      canary_issues: [LEGION-208]
      microvm:
        compute_class: example-microvm
        storage_class: example-microvm-disk
        metadata:
          workload_pool: example-project.svc.id.goog
          identity_provider: https://container.googleapis.com/v1/projects/example-project/locations/us-west1/clusters/example
`

// stateCarryingTheRestOfTheShape is the second state body, and it carries what populatedState
// cannot: an issue waiting on a person for a design gate, which is the `waitingOn` of a kind
// the first body has none of; an issue whose wait a person declined to answer, which is the
// value contract 68 added to `answeredBy` and the one this change's serving-down of a value is
// for; a status write the daemon owes Dispatch; a repository cache whose health read failed;
// the project's last pause, which a state carries only while no pause stands, and populatedState
// carries the one that stands (contract 178); and the settings of a tmux deployment
// (shapeSettingsFile). Every key of these is a key the state route writes, so the records
// describe each of them, the settings keys no reader types apart (withoutUntypedSettings), and a
// serving-down that forgets one is caught where a thinner sample would pass.
func stateCarryingTheRestOfTheShape(t *testing.T) State {
	t.Helper()
	since := time.Date(2026, 9, 22, 7, 30, 0, 0, time.UTC)
	declinedAt := time.Date(2026, 9, 22, 8, 45, 0, 0, time.UTC)
	// The pause the controller began and ended from its session, built as the state's read builds
	// it (PauseStateTx, pauseViewOf): every key of an ended pause, a session at `by` and at
	// `endedBy`, and `endedAt` (controllerPause).
	last := pauseViewOf(controllerPause(true))
	return State{
		Daemon: DaemonInfo{
			Project:       "shapes",
			SchemaVersion: 1,
			Boots:         2,
			FirstBootAt:   time.Date(2026, 9, 18, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 22, 9, 15, 2, 0, time.UTC),
		},
		// The mark the shaping writes on every state it serves (stateFor, ContractView). The
		// source of a state answers none, so it is written here, where the walk reads the two
		// numbers of the key and every record describes them.
		Contract:  &ContractView{Daemon: DaemonAPIVersion, Caller: DaemonAPIVersion},
		Admission: Admission{Cap: 1, Active: []string{"SHAPES-1"}, Trees: []string{"SHAPES-1", "SHAPES-2"}},
		Issues: map[string]Issue{
			"SHAPES-1": {
				Key:           "SHAPES-1",
				Generation:    1,
				Phase:         phase.AwaitingPerson,
				Status:        "in_progress",
				Tree:          "SHAPES-1",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Workers:       map[claim.Role]PhaseView{},
				// The other kind of wait: a person has the root's registered spec to review, so
				// the wait names the version and the document rather than an ask.
				WaitingOn: &WaitingOnView{
					Kind:        "gate",
					GateVersion: 4,
					ArtifactID:  "a1c93f7e-52d0-4c86-9b1f-0e7d3a5c8142",
					Since:       &since,
				},
			},
			// The root whose wait a person declined to answer: they closed an ask of it rather
			// than answer it (contract 68, record.AnsweredByDecline). A reader below 68 parses
			// `answeredBy` with an enumeration of eight values and refuses the whole body for
			// the ninth, so this issue is what the serving-down of a value is read for
			// (shaping.go's withoutUnadmittedWaitAnswer): the state served at every contract
			// below 68 carries this wait without its answered pair.
			"SHAPES-2": {
				Key:           "SHAPES-2",
				Generation:    1,
				Phase:         phase.AwaitingPerson,
				Status:        "in_progress",
				Tree:          "SHAPES-2",
				Repo:          "acme/widgets",
				DefaultBranch: "main",
				Workers:       map[claim.Role]PhaseView{},
				WaitingOn: &WaitingOnView{
					Kind:       "ask",
					Ask:        "7f4b2e60-9c18-4a3d-8e52-6b0d1f93a7c4",
					Since:      &since,
					AnsweredAt: &declinedAt,
					AnsweredBy: record.AnsweredByDecline,
				},
			},
		},
		// A status write the daemon owes Dispatch, which its lane has tried twice. The payload
		// is the workflow's own document and no shape of this wire, so a key path stops there.
		PendingStatusWrites: []PendingStatusWrite{{
			Issue:     "SHAPES-1",
			Payload:   json.RawMessage(`{"status":"in_progress"}`),
			Attempts:  2,
			NextAt:    time.Date(2026, 9, 22, 9, 20, 0, 0, time.UTC),
			LastError: "dispatch: 503 Service Unavailable",
		}},
		// A cache the runtime made and whose last health read failed, which is the one key of a
		// cache view the first body carries no example of. Both role Apps are installed on its
		// repository, so a caller below repositoriesLiveContract reads it under `mapped`.
		Repositories: RepositoriesView{
			Live: []LiveRepositoryView{{Repo: "acme/widgets", Apps: &AppInstallationsView{Implement: true, Review: true}, Cache: &CacheView{
				Repo: "acme/widgets", Sandbox: "legion-shapes-cache-acme-widgets-5f2a1b3c", State: "ready", Ready: true,
				Address:     "http://legion-shapes-cache-acme-widgets-5f2a1b3c.legion.svc:8418",
				HealthError: "read http://legion-shapes-cache-acme-widgets-5f2a1b3c.legion.svc:8418/healthz: context deadline exceeded",
			}}},
		},
		LastPause: &last,
		Settings:  SettingsOf(loadSettingsFile(t, shapeSettingsFile)),
	}
}

// stateCarryingEveryDeclaredValue is the third state body. It carries every value answers.go's
// EnumeratedFields declares above the floor, at every key path of the state the table declares
// the value at, which the first two bodies do not carry between them. The value comparison
// (shapeProblems) reads only the values a served body carries, so a value no body carries is held
// to no record at any contract (TestEveryDeclaredValueAndKeyPathIsCarriedByAFixtureBody).
//
// The values are written here, and not read from the table: a value the table gains with no body
// that carries it is then red in that census, and an edit of the table alone cannot carry it
// here too. Issue i of the body carries value i of each list, and a list shorter than the longest
// starts again at its first value, so the twelve issues carry every value of every list. Each
// issue's architect carries a failure, a pending cause and a held task, as a worker does. No
// other body carries them on an architect, so without this body no record describes those keys
// at their architect paths, and the value comparison reads no value there. Each issue also carries
// a merge wait with every key of one, whose `mergeState` and `toldState` take value i of the
// merge states, so the twelve issues carry all eight words at both key paths. The project's pause
// waits on the stop of one claim of each role, so `paused.waitsOn.*.role` carries every role,
// where populatedState's pause waits on an implementer alone. Its repositories block names five
// live repositories, both role Apps installed on each, whose caches stand in the five states a
// cache can be in, so `repositories.live.*.cache.state` carries every word of that field, which
// arrived with the block at repositoriesLiveContract (answers.go's liveCacheStates). The intake's
// open give-ups list one record of each reason, so `intakeGiveUps.newest.*.reason` carries every
// word, where populatedState's three records carry three of the five.
//
// Its settings are the maximal kubernetes file with a sandbox block that sets every member but
// `type` (sandboxSettingsBlock), which neither other body carries: populatedState's settings are
// that file alone, and stay so because the state golden is written from them, and the second
// body's are a tmux file's.
func stateCarryingEveryDeclaredValue(t *testing.T) State {
	t.Helper()
	at := time.Date(2026, 9, 22, 9, 30, 0, 0, time.UTC)
	// The workflow's phases, which `heldFrom` and `pendingTask.phase` take, with `integrating`,
	// which the plugin's enumeration names and no phase.Phase constant spells.
	phaseWords := []phase.Phase{phase.Admitted, phase.AwaitingPerson, phase.Planning, phase.Implementing,
		phase.Testing, phase.Reviewing, phase.Accepting, phase.Retro, phase.Merging, "integrating", phase.Done,
		phase.Held}
	causeWords := []supervise.FailureCause{supervise.CauseLaunchRefused, supervise.CauseDied,
		supervise.CauseNeverRegistered, supervise.CauseStalePlugin, supervise.CausePromptRetiresExhausted,
		supervise.CauseProviderError, supervise.CauseUnrecorded}
	updateWords := []UpdateState{UpdateStateNone, UpdateStateOpen, UpdateStateAsked, UpdateStateAnswered}
	mergeWords := []record.MergeState{record.MergeStateClean, record.MergeStateUnstable, record.MergeStateHasHooks,
		record.MergeStateBlocked, record.MergeStateDirty, record.MergeStateBehind, record.MergeStateDraft,
		record.MergeStateUnknown}
	// `decisions`, which the plugin's enumeration names and no record.DesignStep constant spells
	// since the step went (answers.go's designSteps).
	stepWords := []record.DesignStep{"decisions", record.DesignStepCriteria, record.DesignStepSpec}
	statusWords := []GatedStepStatus{GatedStepUnregistered, GatedStepWaiting, GatedStepApproved, GatedStepChangesRequested}
	partWords := []record.LinePart{record.PartReturned, record.PartStarting}
	kindWords := []record.LineKind{record.LineReturning, record.LineAnswered, record.LineQueuedRoot,
		record.LineFirstPhase, record.LineWaitingOnIssue}
	whyWords := []WhyState{WhyClaimStopped, WhyWaiting, WhyBlocked, WhyUnlisted}
	groupWords := []string{config.MemoryVariantMemory, config.MemoryVariantNone}
	modeWords := []config.ExperimentMode{config.ExperimentOff, config.ExperimentOn, config.ExperimentRandom}
	heldWords := []record.HeldReason{record.HeldTreeClosed, record.HeldTreeParked, record.HeldLabelOff,
		record.HeldAncestry, record.HeldRootAwaitingPerson, record.HeldDesignGate, record.HeldDispatchBlocker,
		record.HeldChildrenFirst, record.HeldAcceptedAncestor, record.HeldChildrenBeforeRetro, record.HeldTaskStopped,
		record.HeldAtWork, record.HeldUnclassified}
	holdWords := []record.HoldReason{record.HoldEscalated, record.HoldTaskStopped, record.HoldForChildren}
	roleWords := []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer, claim.RoleTester,
		claim.RoleReviewer, claim.RoleMerger}
	stallWords := []record.ArchitectStallArm{record.ArchitectTookNothing, record.ArchitectClaimUnreachable,
		record.ArchitectSlowBeforeGate}
	cacheWords := []string{"cloning", "failing", "ready", "refused", "starting"}
	reasonWords := []record.IntakeGiveUpReason{record.GiveUpBound, record.GiveUpDidNotEnd, record.GiveUpRetention,
		record.GiveUpUndecodable, record.GiveUpConsumerMovedPast}

	// claimView is a claim that failed with cause, whose pending cause is the next word of the
	// list and whose held task was queued for taskPhase: every key of a failure and of a held
	// task, so the architect's paths carry as many keys as a worker's do.
	claimView := func(session string, update UpdateState, cause int, taskPhase phase.Phase) ClaimView {
		return ClaimView{
			Session:     session,
			State:       "failed",
			UpdateState: update,
			Failure: &ClaimFailureView{
				Cause:    causeWords[cause%len(causeWords)],
				Detail:   "launch failures ran out",
				At:       &at,
				Attempts: BudgetsView{LaunchFailures: 4, Deaths: 1, PromptFailures: 2, PromptRetires: 1},
			},
			PendingCause: causeWords[(cause+1)%len(causeWords)],
			PendingTask: &PendingTaskView{
				Phase: taskPhase, QueuedAt: at,
				SendFailures: 1, LastSendError: "post the prompt: dial tcp 10.42.0.17:13371: connect: connection refused",
			},
		}
	}
	notSlot := false
	issues := make(map[string]Issue, len(phaseWords))
	keys := make([]string, 0, len(phaseWords))
	wakes := make([]WakeWaiting, 0, len(roleWords))
	stops := make([]store.PauseStop, 0, len(roleWords))
	for i, heldFrom := range phaseWords {
		key := fmt.Sprintf("VALUES-%d", i+1)
		keys = append(keys, key)
		// The architect's task is queued for the phase after the one the issue was held from, and
		// the worker's for that phase itself, so each view carries every phase over the twelve.
		// The worker's cause starts three words further along the list than the architect's.
		architect := claimView("ses_architect_"+key, updateWords[i%len(updateWords)], i, phaseWords[(i+1)%len(phaseWords)])
		issues[key] = Issue{
			Key:           key,
			Generation:    1,
			Phase:         phase.Held,
			Status:        "in_progress",
			Tree:          key,
			Repo:          "acme/widgets",
			DefaultBranch: "main",
			HeldFrom:      string(heldFrom),
			HoldReason:    string(holdWords[i%len(holdWords)]),
			Architect:     &architect,
			Workers: map[claim.Role]PhaseView{
				claim.RoleImplementer: {Claim: claimView("ses_implementer_"+key, updateWords[(i+1)%len(updateWords)], i+3, heldFrom)},
			},
			PullRequest: &PullRequestView{
				Repo: "acme/widgets", Number: 2000 + i, Head: "5b3d9e2c8f1a47d06b2e5c9f3a8d1e4b7c0f6a29", State: "open",
				MergeState: string(mergeWords[i%len(mergeWords)]),
			},
			MergeWait: &MergeWaitView{
				WaitsOn: "5b3d9e2c8f1a47d06b2e5c9f3a8d1e4b7c0f6a29", MergeState: string(mergeWords[i%len(mergeWords)]),
				StateReadAt: &at, HeadUpdatedAt: &at, CheckRuns: 2,
				ToldHead: "5b3d9e2c8f1a47d06b2e5c9f3a8d1e4b7c0f6a29", ToldState: string(mergeWords[i%len(mergeWords)]),
			},
			GatedSteps: []GatedStepView{{Step: stepWords[i%len(stepWords)], Status: statusWords[i%len(statusWords)]}},
			WaitingLine: &WaitingLineView{
				Place: i + 1, Length: len(phaseWords),
				Part: partWords[i%len(partWords)].String(), Kind: kindWords[i%len(kindWords)].String(),
				PartPlace: 1, PartLength: 1, HoldsSlot: &notSlot,
				Reason: "It stands in the line so that the body carries this kind and this part.",
			},
			Why:            &WhyView{State: whyWords[i%len(whyWords)], Reason: "The body carries this state."},
			MemoryGroup:    groupWords[i%len(groupWords)],
			MemoryDrawMode: string(modeWords[i%len(modeWords)]),
			// One arm of the architect-stalled reading on each issue, so the twelve carry all three.
			ArchitectStalls: []record.ArchitectStallArm{stallWords[i%len(stallWords)]},
		}
		if i < len(roleWords) {
			wakes = append(wakes, WakeWaiting{Claim: claim.Token("legion-values-" + key), Issue: key, Role: roleWords[i],
				Kind: supervise.WakeLaunch, DueAt: at})
			stops = append(stops, store.PauseStop{ID: int64(40 + i), Pause: 7,
				Claim: claim.Token("legion-values-" + key + "-" + string(roleWords[i])), Issue: key, Role: roleWords[i], AskedAt: at})
		}
	}
	// The thirteen held words over the twelve issues, the last word on the first issue again.
	held := make([]HeldIssue, 0, len(heldWords))
	for i, reason := range heldWords {
		held = append(held, HeldIssue{Issue: keys[i%len(keys)], Reason: reason})
	}
	both := AppInstallationsView{Implement: true, Review: true}
	live := make([]LiveRepositoryView, 0, len(cacheWords))
	for _, state := range cacheWords {
		repo := "acme/" + state
		live = append(live, LiveRepositoryView{Repo: repo, Apps: &both, Cache: &CacheView{
			Repo: repo, Sandbox: "legion-values-cache-acme-" + state, State: state,
		}})
	}
	// One record of each reason, newest first as the projection lists them, all of one consumer.
	const valuesConsumer = "legion-go-VALUES-dispatch"
	giveUps := IntakeGiveUpsView{Consumers: []IntakeGiveUpCountView{
		{Name: valuesConsumer, Open: len(reasonWords), NewestSeq: uint64(900 + len(reasonWords))}}}
	for i, reason := range reasonWords {
		seq := uint64(900 + len(reasonWords) - i)
		giveUps.Newest = append(giveUps.Newest, IntakeGiveUpView{ID: int64(seq), Consumer: valuesConsumer,
			Subject: fmt.Sprintf("notifications.dispatch.issue.VALUES-%d.issue.updated", i+1), StreamSeq: seq, StoredAt: at,
			Deliveries: 21, GaveUpAt: at, Reason: reason, LastError: "the handler failed"})
	}
	return State{
		Daemon: DaemonInfo{
			Project:       "values",
			SchemaVersion: 1,
			Boots:         1,
			FirstBootAt:   time.Date(2026, 9, 18, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 22, 9, 15, 2, 0, time.UTC),
		},
		Admission:    Admission{Cap: 1, Waiting: keys, Trees: keys, Held: held},
		Issues:       issues,
		WakesWaiting: wakes,
		Repositories: RepositoriesView{Live: live},
		Settings:     SettingsOf(loadSettingsFile(t, settingsFixtureFile+sandboxSettingsBlock)),
		// Built as the state's read builds it (PauseStateTx), holding no row: populatedState's
		// pause carries the held rows.
		Paused:        pausedViewOf(goldenPause(false), stops, nil),
		IntakeGiveUps: giveUps,
	}
}

// stateFixtures are the state bodies the records of the state and of the issue record are taken
// from: the populated state every other golden of this package is written from, a second one of
// this case's own carrying what the first cannot (stateCarryingTheRestOfTheShape), and a third
// carrying every value the enumerated fields declare above the floor and the settings' sandbox
// block (stateCarryingEveryDeclaredValue).
func stateFixtures(t *testing.T) []State {
	t.Helper()
	return []State{populatedState(t), stateCarryingTheRestOfTheShape(t), stateCarryingEveryDeclaredValue(t)}
}

// answerFixtures is one fixture per answer of the table. Every answer needs one: a row with no
// body here is an answer no record can be taken of, which the cases below fail by name.
func answerFixtures(t *testing.T) map[AnswerID]answerFixture {
	t.Helper()
	states := stateFixtures(t)
	bodies := make([]any, 0, len(states))
	issues := []any{}
	for _, state := range states {
		bodies = append(bodies, state)
		for _, key := range slices.Sorted(maps.Keys(state.Issues)) {
			issues = append(issues, state.Issues[key])
		}
	}
	// The route writes one more body than the three states marshal to: the state served a caller
	// below repositoriesLiveContract, whose `repositories` block is the one contract 152's shape
	// writes from the same entries (shaping.go's withoutRepositoriesLive), and which no body at
	// this daemon's own contract carries. The two censuses below read it, so the
	// `repositories.mapped.*.cache.state` answers.go still declares is a key path a fixture body
	// carries. The records are taken through the routes from the three states alone
	// (carriedShapes), so it adds nothing to them.
	older, _ := withoutRepositoriesLive(states[0])
	bodies = append(bodies, older)
	released := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// The controller's pause, standing, and once the controller's resume ended it
	// (controllerPause).
	standing, ended := pauseViewOf(controllerPause(false)), pauseViewOf(controllerPause(true))
	return map[AnswerID]answerFixture{
		// The whole-project body, the answer whose shape has moved most since the floor.
		AnswerState: {roots: typesOf(State{}), bodies: bodies},
		// The record route answers one issue of a state body, so its fixture is every issue the
		// state fixtures hold: each is a body this route writes, and the record is their key
		// paths read as one set.
		AnswerIssueRecord: {roots: typesOf(Issue{}), bodies: issues},
		AnswerHealth:      {roots: typesOf(Health{}), bodies: []any{Health{Status: "ok", Project: "legion"}}},
		// The register golden's own value (TestRegisterResponseGolden).
		AnswerRegister: {roots: typesOf(claim.RegisterResponse{}), bodies: []any{claim.RegisterResponse{
			ClaimToken:   "legion-legion-legion-208-architect",
			Tree:         "LEGION-208",
			Issue:        "LEGION-208",
			Role:         claim.RoleArchitect,
			Generation:   3,
			Secret:       "U3VwZXJ2aXNlZEJ5TGVnaW9u",
			PromptAgents: []string{"deep-worker", "oracle", "plan-gap-analyst"},
		}}},
		// The controller registration golden's own value (TestControllerRegisterResponseGolden).
		AnswerRegisterController: {roots: typesOf(ControllerRegisterResponse{}), bodies: []any{ControllerRegisterResponse{
			ClaimToken: "legion-legion-controller",
			Role:       claim.RoleController,
			Generation: 2,
			Secret:     "Q29udHJvbGxlZEJ5TGVnaW9u",
		}}},
		// 204 and nothing after the headers. The four routes write the status themselves
		// (claims.go's ready, exit and activity, update.go's ready-for-update), and this
		// package's claim tests hold each to a 204 with an empty body, so the fixture holds no
		// body for them and the record says there is none.
		AnswerNoContent: {},
		// The release-records golden's own value (TestReleaseRecordsResponseGolden), built
		// through releaseRecordsBody from the store's rows as the route builds it. Its third
		// resume is a wake a project pause held, the one resume that carries `pause` (contract
		// 178), which the record of every contract below 178 describes no key of.
		AnswerReleaseRecords: {roots: typesOf(ReleaseRecordsResponse{}), bodies: []any{releaseRecordsBody(store.PodReleaseRecords{
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
		})}},
		// The grant golden's own value (TestTask310RouteGoldens).
		AnswerGrant: {roots: typesOf(GrantResponse{}), bodies: []any{GrantResponse{
			GrantID: "grant-for-one-command", ExpiresAt: "2026-09-23T12:01:00Z",
		}}},
		// The handoff-completion golden's own value. Contract 86 made the answer an empty
		// object: READY and the daemon's merge went, and the `note` that reported them with
		// them, so the body this route writes carries no key at any contract from 86 on.
		AnswerHandoffComplete: {roots: typesOf(HandoffCompleteResponse{}), bodies: []any{HandoffCompleteResponse{}}},
		// The empty object the fact routes answer. Six Go types write it, and none of them
		// declares a field, so the record of this answer is an empty set of key paths over a
		// body that exists.
		AnswerEmptyObject: {
			roots: typesOf(EmptyResponse{}, IssueStatusResponse{}, GateRegisterResponse{},
				PhaseBackwardResponse{}, PhaseRetryResponse{}, SignOffResponse{}),
			bodies: []any{EmptyResponse{}},
		},
		// Richer than the golden, which is the answer of a fresh session that restarted the
		// phase and so carries no key: the record describes the key this answer can carry.
		AnswerFreshSession: {roots: typesOf(FreshSessionResponse{}), bodies: []any{FreshSessionResponse{
			PhaseNotRestarted: "the workflow refused the restart of implementing on LEGION-208",
		}}},
		AnswerCompact: {roots: typesOf(CompactResponse{}), bodies: []any{CompactResponse{
			Compacted: false, Reason: "Nothing to compact (session too small)",
		}}},
		// The turn outcome's golden own value (TestTurnOutcomeResponseGolden): the two keys this
		// answer always writes, neither of them omitempty, so `failed` is written false.
		AnswerTurnOutcome: {roots: typesOf(claim.TurnOutcomeResponse{}), bodies: []any{claim.TurnOutcomeResponse{
			RefusedRun: 3,
		}}},
		// Richer than the golden, which releases one child that started nothing: the record
		// describes the two lists a release fills and the keys of a held child.
		AnswerWaveRelease: {roots: typesOf(WaveReleaseResponse{}), bodies: []any{WaveReleaseResponse{
			Released: []string{"LEGION-209", "LEGION-211"},
			Started:  []string{"LEGION-209"},
			Held:     []HeldChild{{Issue: "LEGION-211", BlockedBy: []string{"LEGION-210"}}},
		}}},
		// The refusal has four Go shapes: claim.Refusal, one sentence under `error`, which the
		// claim routes answer; api.Failure, which adds the stable `code` the credential and
		// workflow routes carry; api.PauseFailure, the Failure with what a refusal of the pause
		// names beside it (contract 178); and api.PlanRefusal, the Failure with one reason for
		// each cause of a refused plan beside it (contract 219). A client parses any of them on
		// every answer that is not a success, so the fixture holds each and the record is the keys
		// of the four read as one set. The pause refusals are the three the goldens hold
		// (TestPauseRouteGoldens): the setting that turns the pause off, the pause that stands,
		// and the last pause, ended, which between them carry every key of a PauseFailure. Their
		// pause is the controller's, richer than the goldens' operator's (controllerPause). The
		// plan refusal names a cause of the whole plan and a cause of one task, the two kinds a
		// reason takes.
		AnswerRefusal: {
			roots: typesOf(claim.Refusal{}, Failure{}, PauseFailure{}, PlanRefusal{}),
			bodies: []any{
				claim.SameAgentRefusal,
				Failure{Code: "GRANT_NOT_FOUND", Error: "no grant for this command"},
				PauseFailure{
					Failure: Failure{Code: pause.CodeUnavailable,
						Error: "this daemon cannot pause its project under runtime: tmux: a pause stops and wakes roles through the idle release, which runs only under runtime: kubernetes; nothing was paused"},
					Setting: string(pause.SettingTmux),
				},
				PauseFailure{
					Failure: Failure{Code: pause.CodeAlreadyPaused,
						Error: "the project is already paused (" + describePause(controllerPause(false)) + "); this pause changed nothing"},
					Pause: &standing,
				},
				PauseFailure{
					Failure: Failure{Code: pause.CodeNotPaused,
						Error: "the project is not paused: its last pause (" + describePause(controllerPause(true)) + ") ended 2026-11-15T09:41:30Z by the controller (session ses_controller); this resume changed nothing"},
					LastPause: &ended,
				},
				PlanRefusal{
					Failure: Failure{Code: "PLAN_REFUSED", Error: "Legion does not take this plan: each reason below names its cause."},
					Reasons: []PlanReason{
						{Task: "", Reason: "The plan holds two tasks. This release of Legion does not carry it out yet."},
						{Task: "a", Reason: "Task a names no implementer. This release of Legion does not carry it out yet."},
					},
				},
			},
		},
		// The pause the controller began, richer than the golden, which is the operator's pause
		// and so names no session (controllerPause). A pause answers the pause it began, which no
		// resume has ended, so no body of it carries `endedAt` or `endedBy`
		// (excludedDeclaredFields).
		AnswerProjectPause: {roots: typesOf(ProjectPauseResponse{}), bodies: []any{ProjectPauseResponse{Pause: standing}}},
		// The resume of the golden (TestPauseRouteGoldens), of the controller's pause rather than
		// the operator's: the pause it ended, by whom and when, and the held rows it let out.
		AnswerProjectResume: {roots: typesOf(ProjectResumeResponse{}), bodies: []any{ProjectResumeResponse{
			Pause: ended, Released: 3,
		}}},
		// The plan-register golden's own value (TestOwnerPlanRouteGoldens): one task with the
		// steps its block names.
		AnswerPlanRegister: {roots: typesOf(PlanRegisterResponse{}), bodies: []any{PlanRegisterResponse{Plan: registeredPlan(plan.Plan{
			ArtifactID: "8f14e45f-ceea-467f-a0e6-1d8b2f8c9d11",
			Version:    4,
			Tasks:      []plan.Task{{ID: "a", Steps: []plan.Step{{Role: "implementer"}, {Role: "reviewer"}}}},
		})}}},
		// The replay golden's own value (goldenIntakeReplay, TestIntakeReplayAnswerGolden).
		AnswerIntakeReplay: {roots: typesOf(IntakeReplayResponse{}), bodies: []any{goldenIntakeReplay}},
		// The discard golden's own value (goldenIntakeDiscard, TestIntakeDiscardAnswerGolden).
		AnswerIntakeDiscard: {roots: typesOf(IntakeDiscardResponse{}), bodies: []any{goldenIntakeDiscard}},
	}
}

// controllerPause is the goldens' pause (goldenPause) begun by the controller from its session
// rather than by the operator, and ended by the controller as the goldens' is, so a view of it
// names a session at `by` and, once ended, at `endedBy` too: a fixture body is maximal, and the
// operator's pause names no session at `by`.
func controllerPause(ended bool) store.Pause {
	p := goldenPause(ended)
	p.By = store.PauseActor{Kind: store.PauseByController, Session: "ses_controller"}
	return p
}

// typesOf is the Go types of values, for an answer fixture's roots.
func typesOf(values ...any) []reflect.Type {
	types := make([]reflect.Type, len(values))
	for i, value := range values {
		types[i] = reflect.TypeOf(value)
	}
	return types
}

// walkDeclaredType adds every key path typ declares below path to shape, each with the field
// that declares it, and marks every node of it that is a Go map. owner is the field path
// belongs to, which a map's value and a list's element keep.
//
// It reads declared json tags and nothing else. The shadow struct of a hand-written marshaller is
// the same type with the same tags (`type wire State`), so it declares the same set. The
// unexported flags the shaping sets are no json fields, and the walk skips them. A map's value,
// a list's element and the inner type of a pointer are each walked as the node below `*` or
// below the field's own name.
func walkDeclaredType(t *testing.T, typ reflect.Type, path string, owner declaredField, shape declaredShape, stack []reflect.Type) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(time.Time{}) {
		// A time marshals as one string, and its own fields are no keys of the wire.
		return
	}
	if slices.Contains(stack, typ) {
		t.Fatalf("the type %s is reachable from itself at %q: this walk describes no recursive wire shape", typ, path)
	}
	stack = append(stack, typ)
	switch typ.Kind() {
	case reflect.Struct:
		for i := range typ.NumField() {
			field := typ.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name, written := jsonFieldName(field)
			if !written {
				continue
			}
			if name == "" {
				walkDeclaredType(t, field.Type, path, owner, shape, stack)
				continue
			}
			child := keyPathJoin(path, name)
			declared := declaredField{typ: typeName(typ), name: name}
			shape.paths[child] = declared
			walkDeclaredType(t, field.Type, child, declared, shape, stack)
		}
	case reflect.Map:
		shape.maps[path] = true
		child := keyPathJoin(path, "*")
		shape.paths[child] = owner
		walkDeclaredType(t, typ.Elem(), child, owner, shape, stack)
	case reflect.Slice, reflect.Array:
		if typ == reflect.TypeOf(json.RawMessage(nil)) {
			// A raw message is a document of another owner's — the workflow's status payload —
			// carried whole. Its own keys are no keys of this wire, so the walk and the reading
			// of a body both stop here.
			shape.opaque[path] = true
			return
		}
		if typ.Elem().Kind() == reflect.Uint8 {
			// A byte slice marshals as one base64 string.
			return
		}
		child := keyPathJoin(path, "*")
		shape.paths[child] = owner
		walkDeclaredType(t, typ.Elem(), child, owner, shape, stack)
	}
}

// typeName is the type as a reader of this package names it: `api.ClaimView`, `runtime.Locator`.
func typeName(typ reflect.Type) string {
	if typ.PkgPath() == "" {
		return typ.String()
	}
	parts := strings.Split(typ.PkgPath(), "/")
	return parts[len(parts)-1] + "." + typ.Name()
}

// jsonFieldName is the name field is written under, and false where it is written under none
// (`json:"-"`). An embedded field with no name of its own reads as the empty name: its own
// fields are written at the path of the value that embeds it.
func jsonFieldName(field reflect.StructField) (string, bool) {
	tag, tagged := field.Tag.Lookup("json")
	if !tagged {
		if field.Anonymous {
			return "", true
		}
		return field.Name, true
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return "", false
	}
	if name == "" {
		if field.Anonymous {
			return "", true
		}
		return field.Name, true
	}
	return name, true
}

// keyPathJoin is one key path of its parent's path and one segment.
func keyPathJoin(path, segment string) string {
	if path == "" {
		return segment
	}
	return path + "." + segment
}

// keyPathsOfBody is every key path the marshalled bytes carry, read with shape so that a map's
// keys collapse to `*` and a struct's are its own (keyPathRule), and the values the bytes carry
// at each key path of enumerated, which a caller passes none of where it reads keys alone.
func keyPathsOfBody(t *testing.T, body []byte, shape declaredShape, enumerated []string,
) (map[string]bool, map[string]map[string]bool) {
	t.Helper()
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode the body %s: %v", body, err)
	}
	paths := map[string]bool{}
	// Seeded with the declared paths and no other, so the walk collects the values of the
	// enumerated fields and not of every string of the body.
	values := make(map[string]map[string]bool, len(enumerated))
	for _, path := range enumerated {
		values[path] = map[string]bool{}
	}
	collectKeyPaths(value, "", shape, paths, values)
	return paths, values
}

// collectedShape is every key path the bodies carry between them, read with shape
// (keyPathsOfBody), and the values they carry at each key path of enumerated. servedShapes reads
// the served bodies through it, and the two tests that ask what the fixtures carry read the
// fixtures' own bodies through it (answerFixture.marshalled), so each of them reads a body as the
// gate does.
func collectedShape(t *testing.T, bodies [][]byte, shape declaredShape, enumerated []string,
) (map[string]bool, map[string]map[string]bool) {
	t.Helper()
	paths := map[string]bool{}
	values := map[string]map[string]bool{}
	for _, body := range bodies {
		bodyPaths, bodyValues := keyPathsOfBody(t, body, shape, enumerated)
		maps.Copy(paths, bodyPaths)
		for path, carried := range bodyValues {
			if values[path] == nil {
				values[path] = map[string]bool{}
			}
			maps.Copy(values[path], carried)
		}
	}
	return paths, values
}

// collectKeyPaths adds the key path of every key below path to into, and the value of every
// string leaf whose own path values holds a set for. A node whose own type carries a document of
// another owner's is read no further: `pendingStatusWrites.*.payload` is one key path, and the
// workflow's keys inside it are none of this wire's.
func collectKeyPaths(value any, path string, shape declaredShape, into map[string]bool, values map[string]map[string]bool) {
	if shape.opaque[path] {
		return
	}
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			name := key
			if shape.maps[path] {
				name = "*"
			}
			childPath := keyPathJoin(path, name)
			into[childPath] = true
			collectKeyPaths(child, childPath, shape, into, values)
		}
	case []any:
		childPath := keyPathJoin(path, "*")
		for _, element := range node {
			into[childPath] = true
			collectKeyPaths(element, childPath, shape, into, values)
		}
	case string:
		if carried, enumerated := values[path]; enumerated {
			carried[node] = true
		}
	}
}

// enumeratedPathsOf is the key paths of the fields the table declares enumerated for one answer,
// sorted.
func enumeratedPathsOf(fields []EnumeratedField, id AnswerID) []string {
	paths := []string{}
	for _, field := range fields {
		if field.Answer == id {
			paths = append(paths, field.KeyPath)
		}
	}
	slices.Sort(paths)
	return paths
}

// domainsOf is what one contract admits at each enumerated field of one answer: the values whose
// arrival is at or below it, sorted, by the field's key path (valueDomainRule). An answer with no
// declared field reads as an empty map, never nil, so every record writes `domains`.
func domainsOf(fields []EnumeratedField, id AnswerID, contract int) map[string][]string {
	domains := map[string][]string{}
	for _, field := range fields {
		if field.Answer != id {
			continue
		}
		admitted := []string{}
		for _, value := range field.Values {
			if value.Since <= contract {
				admitted = append(admitted, value.Value)
			}
		}
		slices.Sort(admitted)
		domains[field.KeyPath] = admitted
	}
	return domains
}

// callerKind is one of the two ways a caller's contract reaches the daemon: the caller states it
// in the `Legion-Plugin-Contract` header, or it states nothing and the claims of the pod its
// address is in record it. Both are held to the records, and the second is the population this
// change is about: a pod running an older plugin asks as it always has.
type callerKind struct {
	name string
	// states is whether the caller declares its contract in the header.
	states bool
}

var callerKinds = []callerKind{
	{name: "a caller stating its contract in the header", states: true},
	{name: "a caller of a pod whose claims record it", states: false},
}

// remote is the address the caller asks from: an address of no pod for the caller that states
// its contract, so that the header is the only thing placing it, and the fixture's own pod for
// the caller that states nothing.
func (k callerKind) remote() string {
	if k.states {
		return "192.0.2.1:41000"
	}
	return statePodAddr + ":41000"
}

// stated is the contract the caller writes in the header, and 0 where it writes none.
func (k callerKind) stated(contract int) int {
	if k.states {
		return contract
	}
	return 0
}

// claims are the project's claims the answer is read against: a pod at this daemon's own
// contract for the caller the header places, and a pod at the contract itself for the other.
func (k callerKind) claims(contract int) []supervise.Claim {
	if k.states {
		return podClaims(DaemonAPIVersion)
	}
	return podClaims(contract)
}

// reaches is whether a caller of this kind can be at contract at all. A caller that states its
// contract in the header can be at every contract. A caller of a pod can be at no contract from
// shapedAnswersContract on: every build from that contract on states its contract in the header,
// so podContracts leaves its claims out of the pod reading, and a pod that records such a
// contract is served the body this daemon writes, at DaemonAPIVersion. While
// shapedAnswersContract is the daemon's own number, that body is the shape of the same contract
// by chance. When a later contract lands, it is the shape of the later contract, so a question to
// the pod kind about such a contract asks about a caller that does not exist.
func (k callerKind) reaches(contract int) bool {
	return k.states || contract < shapedAnswersContract
}

// contracts is the pair of contracts the caller is served at, read from contractsFor itself, the
// one function every route puts the question to. The request is built like the caller's own, and
// claims are what the writer hands contractsFor: the caller's pod's (callerKind.claims) where it
// hands any in, and nil where it hands in none.
func (k callerKind) contracts(t *testing.T, contract int, claims []supervise.Claim) (restore, strip int) {
	t.Helper()
	s := &server{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		callerPod: fixturePodOf,
	}
	request := httptest.NewRequest(http.MethodPost, "/legion/v1/grants", nil)
	request.RemoteAddr = k.remote()
	if stated := k.stated(contract); stated > 0 {
		request.Header.Set(pluginContractHeader, strconv.Itoa(stated))
	}
	at := s.contractsFor(request, claims)
	return at.restore, at.strip
}

// fixturePodOf places the fixture's pod at statePodAddr, as stateServer's own CallerPod does.
func fixturePodOf(addr netip.Addr) (string, bool) {
	if addr == netip.MustParseAddr(statePodAddr) {
		return statePodUID, true
	}
	return "", false
}

// askedBody is the body handler answers a GET of target from this caller, which must be a 200
// (asked).
func (k callerKind) askedBody(t *testing.T, handler http.Handler, target string, contract int) []byte {
	t.Helper()
	recorder := asked(t, handler, k.remote(), target, k.stated(contract))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s from %s = %d, want 200; body %s", target, k.name, recorder.Code, recorder.Body)
	}
	return recorder.Body.Bytes()
}

// servedShapes is every answer at every contract of SupportedContracts, as caller reads it: the
// key paths of the bodies it reads that a record can hold, and the values those bodies carry at
// each key path the table declares enumerated (answers.go's EnumeratedFields). The key paths are
// every path the bodies carry (carriedShapes) but the settings paths no reader at that contract
// types (withoutUntypedSettings), so the comparison and the `-update` writer, which both read
// this, hold and write the same set.
//
// The state and the record come from the routes that answer them: this package's own test server
// answers both with no credential of any kind, so each body goes through the whole of
// writeAnswerFrom — contractsFor over the caller's own request and the claims of the state's own
// snapshot, shapeAnswer, and the one writer.
//
// Every other answer goes through shapeAnswer at the pair contractsFor gives a request built like
// the caller's own, from its address and with its header, and with the claims its writer hands
// it: the pod's claims (callerKind.claims) for a body placedByPod names, as writeAnswer reads
// them for it (placingClaims), and none for every other body, as writeAnswer hands it none, so
// the pod kind is placed there as production places it: by no pod, at this daemon's own contract.
// A serving-down on one of those bodies then reaches the caller that states its contract and not
// the caller of a pod, and this test is red for the second until placedByPod answers true for it,
// which places it with the pod's claims in production and here alike.
//
// These answers are not driven through their routes, as the state and the record are. Each
// route wants a store, a registration, a boot token or a grant, and a fixture body such as the
// fresh session's refused restart wants a machine that refuses one. None of that decides the
// shape. What decides the shape is the placement and shapeAnswer, and both are the production
// functions here, with the claims the route passes. The refusal has no route of its own at
// all: a client parses it on every answer that is not a success.
func servedShapes(t *testing.T, caller callerKind) map[shapeKey]answerShape {
	t.Helper()
	return withoutUntypedSettings(carriedShapes(t, caller), typedSettings)
}

// carriedShapes is servedShapes with every key path the bodies carry, the settings paths no
// reader types among them: what the daemon writes, before the record's rule for the settings
// block is applied (withoutUntypedSettings). TestTheGateCatchesAShapeChangeWithNoServingDown
// reads it to apply that rule with a declaration of its own.
func carriedShapes(t *testing.T, caller callerKind) map[shapeKey]answerShape {
	t.Helper()
	fixtures := answerFixtures(t)
	states := stateFixtures(t)
	// What an answer declares is its Go types' and no contract's, so each is walked once here
	// rather than once for every contract below (answerFixture.shape).
	shapes := make(map[AnswerID]declaredShape, len(fixtures))
	for id, fixture := range fixtures {
		shapes[id] = fixture.shape(t)
	}
	// The claims the state and the record are read against are this daemon's own at every
	// contract for the caller the header places, so that caller's handlers are built once. The
	// other caller's claims are its pod's, which record the contract itself (callerKind.claims).
	var stated []http.Handler
	if caller.states {
		stated = stateHandlers(t, states, caller.claims(DaemonAPIVersion))
	}
	served := map[shapeKey]answerShape{}
	for _, contract := range SupportedContracts {
		if !caller.reaches(contract) {
			// No pod reading places a caller here (callerKind.reaches), so no shape of this
			// kind is read for this contract.
			continue
		}
		handlers := stated
		if handlers == nil {
			handlers = stateHandlers(t, states, caller.claims(contract))
		}
		// The pair a body with no claims is shaped at: contractsFor with no claims, as writeAnswer
		// hands it none where placedByPod answers false.
		restore, strip := caller.contracts(t, contract, nil)
		// The pair a body placedByPod names is shaped at, with the pod's claims writeAnswer reads
		// for it (placingClaims).
		placedRestore, placedStrip := caller.contracts(t, contract, caller.claims(contract))
		// placed is the pair one body of an answer is shaped at, the one writeAnswer in production
		// places its caller with, decided by the production predicate itself.
		placed := func(id AnswerID, body any) (int, int) {
			if placedByPod(id, body) {
				return placedRestore, placedStrip
			}
			return restore, strip
		}
		for _, answer := range Answers {
			if answer.Since > contract {
				continue
			}
			fixture, held := fixtures[answer.ID]
			if !held {
				t.Fatalf("no fixture holds a body for the %s answer: every answer of the table needs one", answer.ID)
			}
			key := shapeKey{contract: contract, answer: answer.ID}
			if answer.NoBody {
				if len(fixture.bodies) != 0 {
					t.Fatalf("the %s answer writes no body and its fixture holds %d", answer.ID, len(fixture.bodies))
				}
				served[key] = answerShape{noBody: true}
				continue
			}
			paths, carried := collectedShape(t,
				servedBodies(t, caller, handlers, states, answer.ID, fixture, contract, placed),
				shapes[answer.ID], enumeratedPathsOf(EnumeratedFields, answer.ID))
			values := make(map[string][]string, len(carried))
			for path, carriedHere := range carried {
				values[path] = slices.Sorted(maps.Keys(carriedHere))
			}
			served[key] = answerShape{paths: slices.Sorted(maps.Keys(paths)), values: values}
		}
	}
	return served
}

// settingsKeyPathPrefix is what a key path under the state's settings block begins with.
const settingsKeyPathPrefix = "settings."

// withoutUntypedSettings is served with every key path under the state's `settings` taken off
// that no reader at that contract types: a record holds `settings` itself and, below it, the
// top-level keys typed declares at the record's own contract and nothing else, no path nested
// below a key either (answers.go's typedSettings says why). typed is the declaration, which every
// caller but a scratch change of TestTheGateCatchesAShapeChangeWithNoServingDown passes
// answers.go's own.
func withoutUntypedSettings(served map[shapeKey]answerShape, typed []TypedSetting) map[shapeKey]answerShape {
	kept := make(map[shapeKey]answerShape, len(served))
	for key, shape := range served {
		if key.answer == AnswerState {
			shape.paths = slices.DeleteFunc(slices.Clone(shape.paths), func(path string) bool {
				setting, under := strings.CutPrefix(path, settingsKeyPathPrefix)
				return under && (strings.Contains(setting, ".") || !settingsTyped(typed, key.contract, setting))
			})
		}
		kept[key] = shape
	}
	return kept
}

// settingsTyped is whether typed declares key, a top-level key of the state's settings block,
// typed by the reader at contract (typedAt).
func settingsTyped(typed []TypedSetting, contract int, key string) bool {
	return slices.ContainsFunc(typed, func(setting TypedSetting) bool {
		return setting.Key == key && typedAt(setting, contract)
	})
}

// typedAt is whether row types its key at contract: from the row's Since to its Until, or to
// this daemon's own contract where Until is 0.
func typedAt(row TypedSetting, contract int) bool {
	return row.Since <= contract && (row.Until == 0 || contract <= row.Until)
}

// stateHandlers is one server per state fixture, each serving that state and the claims given.
func stateHandlers(t *testing.T, states []State, claims []supervise.Claim) []http.Handler {
	t.Helper()
	handlers := make([]http.Handler, len(states))
	for i, state := range states {
		handlers[i] = stateServer(t, state, claims)
	}
	return handlers
}

// servedBodies is every body the answer writes at one contract, as the bytes a caller reads.
// handlers answer the state bodies of states, one handler each. placed is the pair each other
// body is shaped at, which contractsFor gives the caller's request with the claims that body's
// writer hands it: the pod's for a body placedByPod names, none for every other body
// (servedShapes says why, and why those answers do not go through their routes).
func servedBodies(t *testing.T, caller callerKind, handlers []http.Handler, states []State, id AnswerID,
	fixture answerFixture, contract int, placed func(AnswerID, any) (int, int),
) [][]byte {
	t.Helper()
	switch id {
	case AnswerState:
		bodies := make([][]byte, 0, len(handlers))
		for _, handler := range handlers {
			bodies = append(bodies, caller.askedBody(t, handler, "/legion/v1/state", contract))
		}
		return bodies
	case AnswerIssueRecord:
		bodies := [][]byte{}
		for i, state := range states {
			for _, key := range slices.Sorted(maps.Keys(state.Issues)) {
				bodies = append(bodies, caller.askedBody(t, handlers[i], "/legion/v1/issues/"+key+"/record", contract))
			}
		}
		return bodies
	default:
		bodies := make([][]byte, 0, len(fixture.bodies))
		for _, body := range fixture.bodies {
			restore, strip := placed(id, body)
			shaped, _ := shapeAnswer(id, body, restore, strip)
			encoded, err := json.Marshal(shaped)
			if err != nil {
				t.Fatalf("marshal the %s answer: %v", id, err)
			}
			bodies = append(bodies, encoded)
		}
		return bodies
	}
}

// shapeRecords is the record set served describes, sorted by contract and then by answer. The
// domains are fields' and not served's: a record says what its contract admits, which no sample
// of bodies carries the whole of (domainsOf). fields is the table they are read from, which
// every caller but a scratch change of TestTheGateCatchesAShapeChangeWithNoServingDown passes
// answers.go's own.
func shapeRecords(served map[shapeKey]answerShape, fields []EnumeratedField) []contractShape {
	records := make([]contractShape, 0, len(served))
	for key, shape := range served {
		paths := shape.paths
		if paths == nil {
			paths = []string{}
		}
		records = append(records, contractShape{
			Contract: key.contract, Answer: key.answer, NoBody: shape.noBody, KeyPaths: paths,
			Domains: domainsOf(fields, key.answer, key.contract),
		})
	}
	return sortedShapeRecords(records)
}

// sortedShapeRecords is records in the fixture's own order: by contract, then by answer.
func sortedShapeRecords(records []contractShape) []contractShape {
	sorted := slices.Clone(records)
	slices.SortFunc(sorted, func(a, b contractShape) int {
		if a.Contract != b.Contract {
			return a.Contract - b.Contract
		}
		return strings.Compare(string(a.Answer), string(b.Answer))
	})
	return sorted
}

// readContractShapes is the fixture as it stands, and false where no file is there yet, which is
// the one bootstrap run.
func readContractShapes(t *testing.T) ([]contractShape, bool) {
	t.Helper()
	encoded, err := os.ReadFile(goldenPath(contractShapesFixture))
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read %s: %v", contractShapesFixture, err)
	}
	var records []contractShape
	if err := json.Unmarshal(encoded, &records); err != nil {
		t.Fatalf("decode %s: %v", contractShapesFixture, err)
	}
	return records, true
}

// loadContractShapes is the fixture, by the pair each record is held by.
func loadContractShapes(t *testing.T) map[shapeKey]contractShape {
	t.Helper()
	records, written := readContractShapes(t)
	if !written {
		t.Fatal(bootstrapMessage())
	}
	return recordsByKey(records)
}

// recordsByKey is records by the pair each is held by. A pair records holds twice keeps its
// later record; TestTheRecordSetIsOneRecordPerAnswerPerContract refuses a fixture that does.
func recordsByKey(records []contractShape) map[shapeKey]contractShape {
	byKey := make(map[shapeKey]contractShape, len(records))
	for _, record := range records {
		byKey[shapeKey{contract: record.Contract, answer: record.Answer}] = record
	}
	return byKey
}

// updatedRecords is what the `-update` path writes: the records of this daemon's own contract as
// the daemon serves them now, and every other record exactly as the fixture holds it.
//
// The whole file was written once, at the bootstrap, and every record below this daemon's own
// contract was read then against the version.go entry that explains it. After that an author who
// meets a red check cannot clear it by writing the records again: `-update` leaves a record of an
// earlier contract where it is, because that record is what a reader at that contract needs and
// not a reading of today's code. A hand edit of such a record passes only where the daemon serves
// that contract exactly what the edited record describes, so the record changes only together
// with a change to what the daemon serves, and a reviewer reads the diff to tell a serving-down
// from a change that has none. A pair the fixture has no record of at all is no pair `-update`
// adds either: it fails TestTheRecordSetIsOneRecordPerAnswerPerContract by name, so an answer
// nobody recorded is seen.
func updatedRecords(onFile, fresh []contractShape) []contractShape {
	records := make([]contractShape, 0, len(onFile)+len(fresh))
	for _, record := range onFile {
		if record.Contract != DaemonAPIVersion {
			records = append(records, record)
		}
	}
	for _, record := range fresh {
		if record.Contract == DaemonAPIVersion {
			records = append(records, record)
		}
	}
	return sortedShapeRecords(records)
}

// The fixture itself. `-update` writes this daemon's own contract's records and keeps every
// other, as updatedRecords says; a run without the flag holds the file to its own order and
// format, and TestEveryAnswerAtEveryContractIsTheShapeItsRecordDescribes holds each record to
// what the daemon serves.
func TestContractShapesFixture(t *testing.T) {
	onFile, written := readContractShapes(t)
	if !*updateGolden {
		if !written {
			t.Fatal(bootstrapMessage())
		}
		golden(t, contractShapesFixture, sortedShapeRecords(onFile))
		return
	}
	fresh := shapeRecords(servedShapes(t, callerKinds[0]), EnumeratedFields)
	if !written {
		golden(t, contractShapesFixture, fresh)
		return
	}
	golden(t, contractShapesFixture, updatedRecords(onFile, fresh))
}

// Every answer at every contract, served to a caller stating that contract in
// `Legion-Plugin-Contract`, and at every contract below shapedAnswersContract to a caller stating
// nothing from a pod whose claims record it (callerKind.reaches says why the pod kind stops
// there). Both are held to the same records, because a body is owed to the reader and not to the
// way it said who it is, and the second is the population this change is about. Each caller is
// placed as production places it (servedShapes): on the state, on the record and on every body
// placedByPod names, by its pod. On every other answer the pod decides no shape (a fresh
// session's pod decides only what a stop means, readsTheStopAsAFailure), so the pod kind is
// served this daemon's own body, and a serving-down there that only the header kind gets is red
// here.
func TestEveryAnswerAtEveryContractIsTheShapeItsRecordDescribes(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	records := loadContractShapes(t)
	for _, caller := range callerKinds {
		t.Run(caller.name, func(t *testing.T) {
			served := servedShapes(t, caller)
			for _, contract := range SupportedContracts {
				if !caller.reaches(contract) {
					// servedShapes reads no shape for a contract no caller of this kind can
					// be at (callerKind.reaches): every build from shapedAnswersContract on
					// states its contract in the header.
					continue
				}
				for _, answer := range Answers {
					if answer.Since > contract {
						continue
					}
					key := shapeKey{contract: contract, answer: answer.ID}
					record, recorded := records[key]
					if !recorded {
						t.Errorf("contract %d: the %s answer has no record in %s", contract, answer.ID, contractShapesFixture)
						continue
					}
					for _, problem := range shapeProblems(key, served[key], record) {
						t.Error(problem)
					}
				}
			}
		})
	}
}

// The two callers are placed by different readings and served the same pair of contracts where
// the route hands contractsFor the claims, as the state and the record routes do. That is why
// one record stands for both. The pod reading is asked only about the contracts a caller
// of a pod can be at (callerKind.reaches): every build from shapedAnswersContract on states its
// contract in the header, so podContracts leaves its claims out, and the pod reading of such a
// contract answers DaemonAPIVersion, which equals the contract only while it is the daemon's own.
func TestTheTwoCallersAreServedTheSameContracts(t *testing.T) {
	for _, contract := range SupportedContracts {
		statedRestore, statedStrip := callerKinds[0].contracts(t, contract, callerKinds[0].claims(contract))
		if statedRestore != contract || statedStrip != contract {
			t.Errorf("contract %d is served (%d, %d), want the contract itself on both sides: every contract of SupportedContracts is a shape this daemon describes",
				contract, statedRestore, statedStrip)
		}
		if !callerKinds[1].reaches(contract) {
			continue
		}
		podRestore, podStrip := callerKinds[1].contracts(t, contract, callerKinds[1].claims(contract))
		if statedRestore != podRestore || statedStrip != podStrip {
			t.Errorf("contract %d is served (%d, %d) to a caller stating it and (%d, %d) to a caller of a pod recording it",
				contract, statedRestore, statedStrip, podRestore, podStrip)
		}
	}
}

// shapeProblems is every difference between what a caller is served and what the record
// describes, each naming the contract, the answer, the key or the value, and the direction.
func shapeProblems(key shapeKey, served answerShape, record contractShape) []string {
	problems := []string{}
	if served.noBody != record.NoBody {
		if served.noBody {
			problems = append(problems, fmt.Sprintf(
				"contract %d, the %s answer: the route writes no body and the record describes one. An answer that stopped writing a body is a shape change like any other.",
				key.contract, key.answer))
		} else {
			problems = append(problems, fmt.Sprintf(
				"contract %d, the %s answer: the route writes a body and the record says it writes none. A body on a route that answered 204 is a shape change the reader at %d never asked for.",
				key.contract, key.answer, key.contract))
		}
	}
	described := map[string]bool{}
	for _, path := range record.KeyPaths {
		described[path] = true
	}
	carried := map[string]bool{}
	for _, path := range served.paths {
		carried[path] = true
	}
	// A band or a stand-in is written in the row of shapeAnswer of an answer a pod places: the
	// state's and the record's take theirs from stateNarrowings, and the release records' and the
	// refusal's write theirs in the row itself. A refusal is many bodies, and placedByPod answers
	// true for a PauseFailure alone, so a band for a key of any other refusal body reaches the
	// caller of a pod only once placedByPod answers true for that body too (carriedShapes places
	// each body as writeAnswer does). Every other row is the identity and placedByPod answers
	// false for it, so the advice for those answers is what both need.
	shaped := key.answer == AnswerState || key.answer == AnswerIssueRecord
	ownRow := key.answer == AnswerReleaseRecords || key.answer == AnswerRefusal
	for _, path := range served.paths {
		if !described[path] {
			advice := "Write a band that leaves the key out below the contract that adds it (shaping.go's stateNarrowings)."
			switch {
			case ownRow:
				advice = fmt.Sprintf("Write a band in the %s answer's row of shapeAnswer that leaves the key out below the contract that adds it, and return true from placedByPod (shaping.go) for the body that carries the key, so writeAnswer places its caller by its pod.", key.answer)
			case !shaped:
				advice = "Write a band that leaves the key out below the contract that adds it. " + unshapedServingDown("band", key.answer)
			}
			problems = append(problems, fmt.Sprintf(
				"contract %d, the %s answer: the body carries `%s` and the contract does not describe it. %s Recording the key here applies only to the daemon's own contract, and a shape change at a contract the base already carries owes a new number first (version.go's Bump rule).",
				key.contract, key.answer, path, advice))
		}
	}
	for _, path := range record.KeyPaths {
		if !carried[path] {
			advice := fmt.Sprintf("Serve the key to this contract with a stand-in (shaping.go's withFixAttempts is one): a reader at %d may require the key, and the removed-key half of the serving rule stands for good.",
				key.contract)
			if !shaped && !ownRow {
				advice = fmt.Sprintf("Serve the key to this contract with a stand-in: a reader at %d may require the key, and the removed-key half of the serving rule stands for good. %s",
					key.contract, unshapedServingDown("stand-in", key.answer))
			}
			problems = append(problems, fmt.Sprintf(
				"contract %d, the %s answer: the body lacks `%s` and the contract describes it. %s For a key removed at the daemon's own contract, run `go test ./internal/api/ -update`, which rewrites that contract's record and no other; a shape change at a contract the base already carries owes a new number first (version.go's Bump rule).",
				key.contract, key.answer, path, advice))
		}
	}
	for _, path := range slices.Sorted(maps.Keys(served.values)) {
		for _, value := range served.values[path] {
			if slices.Contains(record.Domains[path], value) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"contract %d, the %s answer: the body carries `%s` at `%s` and the contract admits no such value. A reader at %d parses the field with a strict enumeration and refuses the whole body for a value it does not name, as it refuses a key it does not know, so serve the value down to this contract (shaping.go's shapeIssue). Recording its arrival in answers.go's EnumeratedFields applies only to the daemon's own contract, and a shape change at a contract the base already carries owes a new number first (version.go's Bump rule).",
				key.contract, key.answer, value, path, key.contract))
		}
	}
	return problems
}

// unshapedServingDown is what a serving-down of kind, a band or a stand-in, needs before it
// reaches the answer id, one whose row of shapeAnswer is the identity and for which placedByPod
// answers false, so writeAnswer hands contractsFor no claims. Written in that row alone, it would
// reach the caller that states its contract and not the caller of a pod, and this test would stay
// red for the second, since servedShapes shapes such an answer with no claims (servedShapes says
// why).
func unshapedServingDown(kind string, id AnswerID) string {
	return fmt.Sprintf(
		"A %s reaches the %s answer only once placedByPod (shaping.go) returns true for it, so writeAnswer places its caller by its pod and carriedShapes places its bodies with the pod's claims. shapeAnswer then needs a row of its own for the %s answer to write the %s in, where today that answer shares the identity row (shaping.go).",
		kind, id, id, kind)
}

// allShapeProblems is every problem of every pair, in the fixture's order: what the case above
// reports, as one list a scratch change can be held to.
func allShapeProblems(records map[shapeKey]contractShape, served map[shapeKey]answerShape) []string {
	problems := []string{}
	for _, contract := range SupportedContracts {
		for _, answer := range Answers {
			if answer.Since > contract {
				continue
			}
			key := shapeKey{contract: contract, answer: answer.ID}
			record, recorded := records[key]
			if !recorded {
				problems = append(problems, fmt.Sprintf("contract %d: the %s answer has no record", contract, answer.ID))
				continue
			}
			problems = append(problems, shapeProblems(key, served[key], record)...)
		}
	}
	return problems
}

// One record for each answer the table names, at each contract of SupportedContracts from the
// later of the floor and that answer's arrival up to this daemon's own, and no record for any
// other pair. An answer nobody recorded fails here rather than passing unseen, and an answer no
// plugin parses — the operator's claim answers, the controller's secret and credential — is no
// row of the table and has no record to keep.
//
// Each record carries one domain per enumerated field of its answer and no other: the values
// whose arrival is at or below that contract (answers.go's EnumeratedFields, domainsOf), from
// the later of the floor and that field's answer's arrival, which is every contract the record
// exists at. A field the table declares no row for is a field no record describes, and a domain
// of a field the table dropped or renamed is red here rather than left standing.
func TestTheRecordSetIsOneRecordPerAnswerPerContract(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	onFile, written := readContractShapes(t)
	if !written {
		t.Fatal(bootstrapMessage())
	}
	noBody := map[AnswerID]bool{}
	want := map[shapeKey]bool{}
	for _, answer := range Answers {
		noBody[answer.ID] = answer.NoBody
		if answer.Since > DaemonAPIVersion {
			t.Errorf("the %s answer arrived at %d, above this daemon's own contract %d", answer.ID, answer.Since, DaemonAPIVersion)
		}
		for _, contract := range SupportedContracts {
			if answer.Since <= contract {
				want[shapeKey{contract: contract, answer: answer.ID}] = true
			}
		}
	}
	// The table itself: every row names an answer of Answers, so a row no answer carries is red
	// rather than a domain nothing records, and every arrival is a contract this daemon serves a
	// shape for. A value older than the field's own answer is no error — the record answer
	// arrived at 193 and carries all nine values of `answeredBy` from its first record — since a
	// domain is every value whose arrival is at or below the contract (domainsOf).
	for _, field := range EnumeratedFields {
		if !slices.ContainsFunc(Answers, func(answer Answer) bool { return answer.ID == field.Answer }) {
			t.Errorf("answers.go's EnumeratedFields declares `%s` of the %s answer, which is no row of Answers", field.KeyPath, field.Answer)
		}
		for _, value := range field.Values {
			if value.Since < ServingFloor || value.Since > DaemonAPIVersion {
				t.Errorf("answers.go's EnumeratedFields gives `%s` of `%s` the arrival %d, outside the floor %d and this daemon's own contract %d: a value older than the floor is recorded from the floor up, as an answer is",
					value.Value, field.KeyPath, value.Since, ServingFloor, DaemonAPIVersion)
			}
		}
	}
	seen := map[shapeKey]bool{}
	for _, record := range onFile {
		key := shapeKey{contract: record.Contract, answer: record.Answer}
		if seen[key] {
			t.Errorf("%s records the %s answer at contract %d twice", contractShapesFixture, record.Answer, record.Contract)
		}
		seen[key] = true
		if !want[key] {
			t.Errorf("%s records the %s answer at contract %d, which is no pair of SupportedContracts and Answers: the answer arrived later, or the contract is one this daemon describes no shape for",
				contractShapesFixture, record.Answer, record.Contract)
			continue
		}
		if record.NoBody != noBody[record.Answer] {
			t.Errorf("%s records the %s answer at contract %d with noBody %t and answers.go says %t",
				contractShapesFixture, record.Answer, record.Contract, record.NoBody, noBody[record.Answer])
		}
		if record.NoBody && len(record.KeyPaths) != 0 {
			t.Errorf("%s records the %s answer at contract %d as writing no body and gives it %d key paths",
				contractShapesFixture, record.Answer, record.Contract, len(record.KeyPaths))
		}
		if !slices.IsSorted(record.KeyPaths) {
			t.Errorf("%s records the %s answer at contract %d with its key paths out of order",
				contractShapesFixture, record.Answer, record.Contract)
		}
		wantDomains := domainsOf(EnumeratedFields, record.Answer, record.Contract)
		for path, domain := range record.Domains {
			admitted, declared := wantDomains[path]
			if !declared {
				t.Errorf("%s records a domain of `%s` on the %s answer at contract %d and answers.go's EnumeratedFields declares no such field of that answer",
					contractShapesFixture, path, record.Answer, record.Contract)
				continue
			}
			if !slices.Equal(domain, admitted) {
				t.Errorf("%s records `%s` on the %s answer at contract %d admitting %v and the table says %v: a domain holds the sorted values whose arrival is at or below the contract",
					contractShapesFixture, path, record.Answer, record.Contract, domain, admitted)
			}
		}
		for path := range wantDomains {
			if _, recorded := record.Domains[path]; !recorded {
				t.Errorf("%s records no domain of `%s` on the %s answer at contract %d: every enumerated field the table declares is recorded wherever its answer is",
					contractShapesFixture, path, record.Answer, record.Contract)
			}
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("%s has no record of the %s answer at contract %d: every answer the table names is recorded at every contract from its arrival up",
				contractShapesFixture, key.answer, key.contract)
		}
	}
}

// Every record of the state holds `settings` and, below it, exactly the top-level keys
// answers.go's typedSettings types at that record's contract, each as `settings.<key>`, so the
// declaration and the records agree in both directions. A typed key a record leaves out is a key
// whose removal the reader at that contract refuses and the gate would not see, and any other
// settings path is one no reader refuses the body for, which would make every new legion.yaml key
// a contract change. The declaration's rows are held to the contracts this daemon describes, an
// open end being written as no Until rather than as this daemon's own number, which moves.
func TestEveryStateRecordHoldsTheSettingsKeysItsReaderTypesAndNoOther(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	for _, setting := range typedSettings {
		if setting.Since < ServingFloor || setting.Since > DaemonAPIVersion ||
			(setting.Until != 0 && (setting.Until < setting.Since || setting.Until >= DaemonAPIVersion)) {
			t.Errorf("answers.go's typedSettings types `%s` from %d to %d, outside the floor %d and this daemon's own contract %d, with an end below its start, or with an end at or above %d that no Until writes",
				setting.Key, setting.Since, setting.Until, ServingFloor, DaemonAPIVersion, DaemonAPIVersion)
		}
	}
	records := loadContractShapes(t)
	for _, contract := range SupportedContracts {
		record, recorded := records[shapeKey{contract: contract, answer: AnswerState}]
		if !recorded {
			t.Errorf("contract %d: the state has no record in %s", contract, contractShapesFixture)
			continue
		}
		want := map[string]bool{}
		for _, setting := range typedSettings {
			if typedAt(setting, contract) {
				want[settingsKeyPathPrefix+setting.Key] = true
			}
		}
		held := []string{}
		for _, path := range record.KeyPaths {
			if strings.HasPrefix(path, settingsKeyPathPrefix) {
				held = append(held, path)
			}
		}
		if typed := slices.Sorted(maps.Keys(want)); !slices.Equal(held, typed) {
			t.Errorf("contract %d: the state's record holds the settings paths %v, and answers.go's typedSettings types %v there: a record holds a settings key exactly where a reader at its contract types it",
				contract, held, typed)
		}
		if !slices.Contains(record.KeyPaths, "settings") {
			t.Errorf("contract %d: the state's record holds no `settings`, which every state body this daemon writes carries", contract)
		}
	}
}

// Each enumerated field arrived with the earliest value its list declares, which is what
// shaping.go's arrivalsOf reads the field's own arrival as, and this test reads it through that
// same function. A value strip tells a value newer than its field from one that came with it by
// that earliest arrival (fieldArrivals.heldBack). That holds while the list still declares a
// value the field arrived with. A change that removed every such value would leave the earliest
// arrival later than the field's, and a value arriving at that later contract would read as one
// that came with the field: no strip would take it, and a mixed pod whose older process knows
// the field from before that contract would be served it and refuse the whole body for it.
//
// The key's arrival is read from the records and written nowhere here: it is the earliest
// contract of SupportedContracts whose record of the row's answer carries the row's key path,
// which is the floor for a key the floor's record already carries. Every row the table writes is
// held to it, the state's and the registration's. The record answer's rows are the state's rows
// with the issue map's prefix gone (answers.go's enumeratedFields), so the state's rows hold
// them, and that answer's own records begin at the contract its route arrived at, which says
// nothing of when its fields did.
func TestEveryEnumeratedFieldsEarliestValueArrivedWithItsKey(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	records := loadContractShapes(t)
	for _, field := range EnumeratedFields {
		if field.Answer == AnswerIssueRecord {
			continue
		}
		if len(field.Values) == 0 {
			t.Errorf("answers.go's EnumeratedFields declares `%s` of the %s answer with no value", field.KeyPath, field.Answer)
			continue
		}
		earliest := arrivalsOf(field.Values).field
		key := 0
		for _, contract := range SupportedContracts {
			record, recorded := records[shapeKey{contract: contract, answer: field.Answer}]
			if recorded && slices.Contains(record.KeyPaths, field.KeyPath) && (key == 0 || contract < key) {
				key = contract
			}
		}
		switch {
		case key == 0:
			t.Errorf("no record of the %s answer in %s carries `%s`, which answers.go's EnumeratedFields declares: a field is recorded from the contract its key arrived at",
				field.Answer, contractShapesFixture, field.KeyPath)
		case earliest > key:
			t.Errorf("`%s` of the %s answer has its earliest value at %d and its key at %d, the earliest record carrying it: a value arriving at %d would be read as arriving with its field and served to a mixed pod whose older process refuses it (shaping.go's fieldArrivals)",
				field.KeyPath, field.Answer, earliest, key, earliest)
		case earliest < key:
			t.Errorf("`%s` of the %s answer has its earliest value at %d and its key at %d, the earliest record carrying it: a value arrives with its field or after it, and the table records each at the later of the two (answers.go's EnumeratedFields)",
				field.KeyPath, field.Answer, earliest, key)
		}
	}
}

// What the gate catches, shown by six kinds of change a shape change would make, the fifth of
// them made to two fields, and what it lets pass by design: a settings key no reader types. Each
// is applied to a copy inside this case, so none of them edits a file and none needs a commit.
func TestTheGateCatchesAShapeChangeWithNoServingDown(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	onFile, written := readContractShapes(t)
	if !written {
		t.Fatal(bootstrapMessage())
	}
	// carried is what the daemon writes, every settings path included; served is what a record
	// can hold of it under answers.go's own declaration (withoutUntypedSettings). The two cases
	// of the settings block apply the record's rule themselves, the second with a declaration of
	// its own.
	carried := carriedShapes(t, callerKinds[0])
	served := withoutUntypedSettings(carried, typedSettings)
	// Every scratch change below but the third, which rewrites a record by hand, and the two of
	// the settings block, which read the records as they stand, is what the gate reads after an
	// author ran `-update`, which rewrites this daemon's own contract's records and leaves every
	// earlier one alone. So those four fail at every contract below this daemon's own and
	// nowhere else, which is the whole point of the serving-down: the reader that is hurt is the
	// older one. fields is the enumerated table `-update` would have read, which is answers.go's
	// own for every change but the value one, whose author widened a field.
	afterUpdate := func(changed map[shapeKey]answerShape, fields []EnumeratedField) map[shapeKey]contractShape {
		return recordsByKey(updatedRecords(onFile, shapeRecords(changed, fields)))
	}
	asOnFile := recordsByKey(onFile)

	t.Run("a key the current contract serves is removed with no stand-in", func(t *testing.T) {
		const removed = "daemon.project"
		changed := withoutServedKey(t, served, AnswerState, removed)
		problems := allShapeProblems(afterUpdate(changed, EnumeratedFields), changed)
		wantProblemsNaming(t, problems, removed, SupportedContracts, DaemonAPIVersion)
	})

	t.Run("a key is added with no serving-down", func(t *testing.T) {
		const added = "issues.*.check.nextLook"
		changed := withServedKey(t, served, AnswerState, added)
		problems := allShapeProblems(afterUpdate(changed, EnumeratedFields), changed)
		wantProblemsNaming(t, problems, added, SupportedContracts, DaemonAPIVersion)
	})

	t.Run("a record of a contract below this daemon's own is rewritten", func(t *testing.T) {
		const rewritten = "issues.*.check.role"
		// The record of the floor describes no `check.role`: contract 54 added the key and the
		// floor's reader refuses it. An author who writes it in there, to clear the red a
		// serving-down they deleted leaves, is caught by this comparison and by nothing they
		// can run.
		rewrittenFile := slices.Clone(onFile)
		at := -1
		for i, record := range rewrittenFile {
			if record.Contract == ServingFloor && record.Answer == AnswerState {
				at = i
			}
		}
		if at < 0 {
			t.Fatalf("%s has no record of the state at the floor %d; this proves nothing", contractShapesFixture, ServingFloor)
		}
		if slices.Contains(rewrittenFile[at].KeyPaths, rewritten) {
			t.Fatalf("the record of the state at the floor already describes `%s`; this proves nothing", rewritten)
		}
		rewrittenFile[at].KeyPaths = slices.Sorted(slices.Values(append(slices.Clone(rewrittenFile[at].KeyPaths), rewritten)))

		problems := allShapeProblems(recordsByKey(rewrittenFile), served)
		wantProblemsNaming(t, problems, rewritten, []int{ServingFloor}, 0)

		// And `-update` does not clear it: the fixture's own writer leaves a record below this
		// daemon's own contract where it is, so the red stands until the serving-down is put
		// back or the record is corrected by hand.
		written := updatedRecords(rewrittenFile, shapeRecords(served, EnumeratedFields))
		for _, record := range written {
			if record.Contract == ServingFloor && record.Answer == AnswerState && !slices.Contains(record.KeyPaths, rewritten) {
				t.Errorf("`-update` rewrote the record of the state at the floor: an author who meets a red check could clear it by running the golden test")
			}
		}
	})

	t.Run("a body is added to a route that answers 204", func(t *testing.T) {
		changed := maps.Clone(served)
		for _, contract := range SupportedContracts {
			key := shapeKey{contract: contract, answer: AnswerNoContent}
			changed[key] = answerShape{paths: []string{"accepted"}}
		}
		problems := allShapeProblems(afterUpdate(changed, EnumeratedFields), changed)
		var below []string
		for _, problem := range problems {
			if !strings.Contains(problem, string(AnswerNoContent)) {
				t.Errorf("the gate reported a problem of another answer: %s", problem)
				continue
			}
			below = append(below, problem)
		}
		// Every contract below this daemon's own reads the record that says the route writes no
		// body, and reads a body: two problems each, the body itself and its one key.
		if len(below) != 2*(len(SupportedContracts)-1) {
			t.Errorf("a body on the 204 routes was caught at %d problems, want 2 for each of the %d contracts below this daemon's own:\n%s",
				len(below), len(SupportedContracts)-1, strings.Join(below, "\n"))
		}
		if len(below) > 0 {
			t.Logf("the gate reported %d problems, the first of them: %s", len(below), below[0])
			if !strings.Contains(below[0], "writes a body and the record says it writes none") {
				t.Errorf("the first problem reads %q, want the one that says the route writes a body the record knows nothing of", below[0])
			}
		}
	})

	t.Run("a value is declared and served past its strip", func(t *testing.T) {
		// Each row is a field whose values a strip moves, and each strip reads the values the
		// table declares (withoutUnadmittedWaitAnswer, withoutUnadmittedStrandedStates). So
		// declaring a tenth `answeredBy` or a fourth stranded state at this daemon's own
		// contract is the whole of that strip's serving-down: the daemon would drop the wait's
		// answered pair, or the entry of a claim in that state, from every caller below this
		// contract. So each row proves a strip that is bypassed: the value is declared and
		// served to every caller anyway. `-update` then records the widened domain at this
		// contract, and every older contract's record holds the domain it always held, which is
		// the reader this change exists for, so the gate catches the value at every contract
		// below this daemon's own.
		for _, tc := range []struct {
			name  string
			field string
			added string
		}{
			{name: "a tenth answeredBy", field: "issues.*.waitingOn.answeredBy", added: "recalled"},
			{name: "a fourth stranded state", field: "strandedClaims.*.state", added: "parked"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				widened := withDeclaredValue(t, EnumeratedFields, AnswerState, tc.field, tc.added, DaemonAPIVersion)
				changed := withServedValue(t, served, AnswerState, tc.field, tc.added)
				problems := allShapeProblems(afterUpdate(changed, widened), changed)
				wantProblemsNaming(t, problems, tc.field, SupportedContracts, DaemonAPIVersion)
				for _, problem := range problems {
					if !strings.Contains(problem, "`"+tc.added+"`") {
						t.Errorf("the gate reported a problem naming the field and not the value `%s`: %s", tc.added, problem)
					}
				}
			})
		}
	})

	t.Run("a settings key a reader types is declared at this contract with no -update", func(t *testing.T) {
		// The reader of this daemon's own contract begins typing a key the block already
		// carries, and its author declares it in answers.go's typedSettings, from this contract
		// on, and runs no `-update`. The record of this contract then lacks a key its reader
		// refuses the body without, and the gate names it there and nowhere else: no reader
		// below types the key, so no record below holds it. `-update` writes it into this
		// contract's record, which is the whole of the remedy; a shape change at a contract the
		// base already carries owes a new number first (version.go's Bump rule).
		const typedKey = "admission_cap"
		if settingsTyped(typedSettings, DaemonAPIVersion, typedKey) {
			t.Fatalf("answers.go's typedSettings already types `%s`; declaring it proves nothing", typedKey)
		}
		declared := append(slices.Clone(typedSettings), TypedSetting{Key: typedKey, Since: DaemonAPIVersion})
		changed := withoutUntypedSettings(carried, declared)
		problems := allShapeProblems(asOnFile, changed)
		wantProblemsNaming(t, problems, settingsKeyPathPrefix+typedKey, []int{DaemonAPIVersion}, 0)
		if len(problems) != 1 {
			t.Errorf("the gate reported %d problems for one typed key at this contract, want one:\n%s",
				len(problems), strings.Join(problems, "\n"))
		}
		if after := allShapeProblems(afterUpdate(changed, EnumeratedFields), changed); len(after) != 0 {
			t.Errorf("`-update` left %d problems, the first: %s", len(after), after[0])
		}
	})

	t.Run("a settings key no reader types is added with no serving-down, and the gate stays quiet", func(t *testing.T) {
		// A new legion.yaml key, at the top of the block and below a key of it, written at
		// every contract with no band, no declaration and no `-update`: no reader refuses the
		// body for it (answers.go's typedSettings), so the records hold neither path and the
		// gate reports nothing.
		changed := carried
		for _, added := range []string{"settings.a_later_legion_yaml_key", "settings.runtime.kubernetes.a_later_block_key"} {
			changed = withServedKey(t, changed, AnswerState, added)
		}
		if problems := allShapeProblems(asOnFile, withoutUntypedSettings(changed, typedSettings)); len(problems) != 0 {
			t.Errorf("a settings key no reader types turned the gate red at %d problems, the first: %s", len(problems), problems[0])
		}
	})
}

// withoutServedKey is served with one key path taken off one answer at every contract, as a
// daemon that stopped writing the key and declared no stand-in would serve it.
func withoutServedKey(t *testing.T, served map[shapeKey]answerShape, id AnswerID, path string) map[shapeKey]answerShape {
	t.Helper()
	changed := maps.Clone(served)
	removed := 0
	for key, shape := range served {
		if key.answer != id || !slices.Contains(shape.paths, path) {
			continue
		}
		paths := slices.DeleteFunc(slices.Clone(shape.paths), func(carried string) bool { return carried == path })
		changed[key] = answerShape{noBody: shape.noBody, paths: paths, values: shape.values}
		removed++
	}
	if removed == 0 {
		t.Fatalf("no body of the %s answer carries `%s`; removing it proves nothing", id, path)
	}
	return changed
}

// withServedKey is served with one key path added to one answer at every contract, as a daemon
// that began writing the key and declared no serving-down would serve it.
func withServedKey(t *testing.T, served map[shapeKey]answerShape, id AnswerID, path string) map[shapeKey]answerShape {
	t.Helper()
	changed := maps.Clone(served)
	added := 0
	for key, shape := range served {
		if key.answer != id {
			continue
		}
		if slices.Contains(shape.paths, path) {
			t.Fatalf("the %s answer already carries `%s` at contract %d; adding it proves nothing", id, path, key.contract)
		}
		changed[key] = answerShape{
			noBody: shape.noBody,
			paths:  slices.Sorted(slices.Values(append(slices.Clone(shape.paths), path))),
			values: shape.values,
		}
		added++
	}
	if added == 0 {
		t.Fatalf("the %s answer is served at no contract; adding a key to it proves nothing", id)
	}
	return changed
}

// withServedValue is served with one value added at one key path of one answer at every
// contract, as a daemon that began writing the value and declared no serving-down would serve
// it. The path must be one the table declares enumerated, or no body of the answer carries a
// value there and the change proves nothing.
func withServedValue(t *testing.T, served map[shapeKey]answerShape, id AnswerID, path, value string) map[shapeKey]answerShape {
	t.Helper()
	changed := maps.Clone(served)
	added := 0
	for key, shape := range served {
		if key.answer != id {
			continue
		}
		carried, enumerated := shape.values[path]
		if !enumerated {
			t.Fatalf("no body of the %s answer carries a value at `%s` at contract %d; adding one proves nothing", id, path, key.contract)
		}
		if slices.Contains(carried, value) {
			t.Fatalf("the %s answer already carries `%s` at `%s` at contract %d; adding it proves nothing", id, value, path, key.contract)
		}
		values := maps.Clone(shape.values)
		values[path] = slices.Sorted(slices.Values(append(slices.Clone(carried), value)))
		changed[key] = answerShape{noBody: shape.noBody, paths: shape.paths, values: values}
		added++
	}
	if added == 0 {
		t.Fatalf("the %s answer is served at no contract; adding a value to it proves nothing", id)
	}
	return changed
}

// withDeclaredValue is the table with one value added to one declared enumerated field, arriving
// at since: the row an author writes in answers.go for a value their change began serving.
func withDeclaredValue(t *testing.T, fields []EnumeratedField, id AnswerID, path, value string, since int) []EnumeratedField {
	t.Helper()
	widened := slices.Clone(fields)
	at := slices.IndexFunc(widened, func(field EnumeratedField) bool {
		return field.Answer == id && field.KeyPath == path
	})
	if at < 0 {
		t.Fatalf("answers.go's EnumeratedFields declares no `%s` of the %s answer; widening it proves nothing", path, id)
	}
	values := append(slices.Clone(widened[at].Values), EnumeratedValue{Value: value, Since: since})
	slices.SortFunc(values, func(a, b EnumeratedValue) int { return strings.Compare(a.Value, b.Value) })
	widened[at] = EnumeratedField{Answer: id, KeyPath: path, Values: values}
	return widened
}

// wantProblemsNaming holds problems to one problem naming path at each of contracts, and none at
// quiet, the contract whose own record `-update` rewrote (0 for a case where no contract is
// quiet). It logs the first problem, so `go test -v` shows the words the scratch change failed
// by.
func wantProblemsNaming(t *testing.T, problems []string, path string, contracts []int, quiet int) {
	t.Helper()
	if len(problems) == 0 {
		t.Fatalf("the gate reported no problem; it catches nothing")
	}
	t.Logf("the gate reported %d problems, the first of them: %s", len(problems), problems[0])
	naming := map[int]int{}
	for _, problem := range problems {
		if !strings.Contains(problem, "`"+path+"`") {
			t.Errorf("the gate reported a problem naming no `%s`: %s", path, problem)
			continue
		}
		for _, contract := range SupportedContracts {
			if strings.HasPrefix(problem, fmt.Sprintf("contract %d, ", contract)) {
				naming[contract]++
			}
		}
	}
	for _, contract := range contracts {
		if contract == quiet {
			if naming[contract] != 0 {
				t.Errorf("the gate reported %d problems at contract %d, the contract `-update` rewrote the record of", naming[contract], contract)
			}
			continue
		}
		if naming[contract] != 1 {
			t.Errorf("the gate reported %d problems naming `%s` at contract %d, want exactly one", naming[contract], path, contract)
		}
	}
}

// answerField is one json field of one answer's wire types, the pair this case reads and an
// exclusion names.
type answerField struct {
	answer AnswerID
	field  declaredField
}

// excludedDeclaredFields is every json field of an answer's wire types that no fixture body
// realises, with why. Each entry names one answer and one exact field of one type. The case
// below fails an entry whose field the walk does not declare and one a fixture does realise, so
// a later reader cannot widen an exclusion by accident: there is no prefix and no wildcard to
// widen, and a stale entry is red rather than quiet.
//
// Three entries are the fields of one entry of `settings.projects.<KEY>.deploys`, the key
// contract 54 took the daemon's reading of away. The loader accepts the key and reads nothing
// from it, and SettingsOf writes `[]` for every project whatever the file holds
// (settings.go's DeploySettings, api.ProjectSettings.Deploys), so no body the daemon can write
// carries an entry and no fixture can realise one. The list itself is realised. No record holds
// it, as no record holds any settings path no reader types (withoutUntypedSettings), and the
// census below reads the fixture bodies rather than the records, so the three stay entries here.
// They go with the key, when the loader stops accepting it.
//
// The other two are the end of the pause the controller's pause answers (contract 178): that
// answer is the pause the route began (pauseProject, pauseViewOf), which no resume has ended, so
// no body of it carries `endedAt` or `endedBy`. Both are realised by the resume's answer, whose
// pause is ended, and by the refusal's `lastPause`.
var excludedDeclaredFields = map[answerField]string{
	{answer: AnswerState, field: declaredField{typ: "api.DeploySettings", name: "repo"}}:      deploysNeverFilled,
	{answer: AnswerState, field: declaredField{typ: "api.DeploySettings", name: "workflow"}}:  deploysNeverFilled,
	{answer: AnswerState, field: declaredField{typ: "api.DeploySettings", name: "step"}}:      deploysNeverFilled,
	{answer: AnswerProjectPause, field: declaredField{typ: "api.PauseView", name: "endedAt"}}: pauseNeverEnded,
	{answer: AnswerProjectPause, field: declaredField{typ: "api.PauseView", name: "endedBy"}}: pauseNeverEnded,
}

// pauseNeverEnded is the reason the two entries of the pause's answer share.
const pauseNeverEnded = "the pause the route answers is the one it began, which no resume has ended, so no body of the answer carries an end (pauseProject)"

// deploysNeverFilled is the reason the three entries of the deploys share.
const deploysNeverFilled = "the daemon holds no deploy since contract 54, so `settings.projects.<KEY>.deploys` is `[]` on every body it writes (SettingsOf) and no entry of it can be realised"

// Every json field the answers' wire types declare is realised by a fixture body, so a record
// taken from these bodies describes the whole of each answer rather than the part a thin sample
// happened to carry. `issues.*.check.role` is the field this case exists for: contract 54 added
// it, an issue carries a check only while its production check is open, and a sample with no
// open check would record a state shape with no `role` in it at all. It reads the fixture bodies
// and not the records, so it asks the same of every field of the settings block, whose records
// hold only the keys a reader types (withoutUntypedSettings): a key typed later is then one the
// bodies already carry.
//
// A field is one json name of one Go type, so the two key paths of ClaimView.Capabilities —
// `architect.capabilities` and `workers.*.claim.capabilities` — are one field, and the fixture
// realises it wherever it carries one. The walk reads declared json tags (walkDeclaredType), so
// a hand-written marshaller's shadow struct declares the same set as the type it shadows, and
// the unexported flags the shaping sets are no json fields of any type and are walked by
// nothing (walkDeclaredType names them). What they do to a body is held all the same, by the
// records: the stand-ins PullRequestView.fixAttemptsShown and Settings.maxFixAttemptsShown put
// `fixAttempts` and `max_fix_attempts` into the records below 59, CheckView.roleHidden takes
// `check.role` out of the records below 54, and PhaseView.passHidden takes each worker's `pass`
// out of the records below 86.
func TestEveryDeclaredFieldOfAnAnswerIsRealisedByItsFixture(t *testing.T) {
	fixtures := answerFixtures(t)
	// paths is one key path of each field, for the message a gap reads as.
	paths := map[answerField]string{}
	declared := map[answerField]bool{}
	realised := map[answerField]bool{}
	for _, answer := range Answers {
		fixture := fixtures[answer.ID]
		if len(fixture.roots) == 0 {
			// The 204 answers write no body, so their types declare nothing to realise.
			continue
		}
		shape := fixture.shape(t)
		for path, field := range shape.paths {
			key := answerField{answer: answer.ID, field: field}
			declared[key] = true
			if held, seen := paths[key]; !seen || len(path) < len(held) {
				paths[key] = path
			}
		}
		bodyPaths, _ := collectedShape(t, fixture.marshalled(t, answer.ID), shape, nil)
		for path := range bodyPaths {
			if field, isDeclared := shape.paths[path]; isDeclared {
				realised[answerField{answer: answer.ID, field: field}] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("the walk declared no field at all; this proves nothing")
	}

	for _, key := range sortedAnswerFields(declared) {
		if realised[key] {
			continue
		}
		if reason, excluded := excludedDeclaredFields[key]; excluded {
			t.Logf("the %s answer declares %s (`%s`) and no fixture realises it: %s", key.answer, key.field, paths[key], reason)
			continue
		}
		t.Errorf("the %s answer declares %s and no fixture body realises it (`%s`): give the fixture a value for it, so the bodies every record is taken from carry every field the daemon can write, and a key a later reader types is one they already carry, rather than leaving the field out of the walk",
			key.answer, key.field, paths[key])
	}
	for _, key := range sortedAnswerFields(excludedDeclaredFields) {
		if !declared[key] {
			t.Errorf("an exclusion names %s of the %s answer and the walk declares no such field: the field is gone or renamed, so the exclusion goes with it",
				key.field, key.answer)
		}
		if realised[key] {
			t.Errorf("an exclusion names %s of the %s answer and a fixture realises it: the exclusion is dead and hides the next field that is not realised",
				key.field, key.answer)
		}
	}
}

// The census of the enumerated fields: every key path answers.go's EnumeratedFields declares is
// carried by some fixture body of its answer, and so is every value the table declares above the
// floor, at its own key path. The value comparison (shapeProblems) reads only the values the
// served bodies carry. A value no fixture body carries is held to no record, so a word served to
// a contract that does not admit it passes the gate. A key path no body carries is in no record
// at all, so neither the key comparison nor the value comparison reads it. The architect's
// `failure.cause`, `pendingCause` and `pendingTask.phase` were three such paths until
// stateCarryingEveryDeclaredValue gave the architect's view a failure and a held task.
//
// A value at the floor is not asked for. Every contract the records hold admits it, so no
// comparison can find it unadmitted.
func TestEveryDeclaredValueAndKeyPathIsCarriedByAFixtureBody(t *testing.T) {
	fixtures := answerFixtures(t)
	carriedPaths := map[AnswerID]map[string]bool{}
	carriedValues := map[AnswerID]map[string]map[string]bool{}
	for _, answer := range Answers {
		fixture := fixtures[answer.ID]
		if len(fixture.roots) == 0 {
			// The 204 answers write no body, so no field of theirs is enumerated.
			continue
		}
		carriedPaths[answer.ID], carriedValues[answer.ID] = collectedShape(t, fixture.marshalled(t, answer.ID),
			fixture.shape(t), enumeratedPathsOf(EnumeratedFields, answer.ID))
	}

	asked := 0
	for _, field := range EnumeratedFields {
		if !carriedPaths[field.Answer][field.KeyPath] {
			t.Errorf("the %s answer: answers.go's EnumeratedFields declares `%s`, and no fixture body carries that key path, so no record describes it and the value comparison reads no value there. Give a fixture body of the answer (answerFixtures) a value at that key path.",
				field.Answer, field.KeyPath)
		}
		for _, value := range field.Values {
			if value.Since <= ServingFloor {
				continue
			}
			asked++
			if carriedValues[field.Answer][field.KeyPath][value.Value] {
				continue
			}
			t.Errorf("the %s answer: answers.go's EnumeratedFields declares `%s` at `%s`, arriving at %d, and no fixture body carries that value there, so the value comparison never holds it to the records of the contracts below %d. Give a fixture body of the answer (answerFixtures) that value at that key path.",
				field.Answer, value.Value, field.KeyPath, value.Since, value.Since)
		}
	}
	if asked == 0 {
		t.Fatal("answers.go's EnumeratedFields declares no value above the floor; this proves nothing")
	}
}

// sortedAnswerFields is the keys of fields, by answer, then by type, then by field name.
func sortedAnswerFields[V any](fields map[answerField]V) []answerField {
	sorted := slices.Collect(maps.Keys(fields))
	slices.SortFunc(sorted, func(a, b answerField) int {
		if a.answer != b.answer {
			return strings.Compare(string(a.answer), string(b.answer))
		}
		return strings.Compare(a.field.String(), b.field.String())
	})
	return sorted
}
