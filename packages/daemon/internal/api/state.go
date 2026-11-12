// Package api serves the Go daemon's HTTP surface and owns the wire shape of its state.
//
// The shape here is designed from the LEGION-208 spec's Issue record, not ported from the
// TypeScript daemon's `packages/contracts/src/legion-daemon-api.ts` (which still carries the
// tossed worker queue and the Dispatch/GitHub mirror). Go is the source of truth for the wire:
// `packages/contracts/src/legion-api.ts` mirrors these types as zod objects, which strip a key
// they do not name on the answer side, and is held to them by the golden fixture in
// `packages/contracts/fixtures/daemon-api/`.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Unrecorded is what this route reads for an issue the workflow does not record, where an
// operator's claim exists and an issue does not (LEGION-272). It is not a workflow phase: no
// issue record holds it, the transition table never reaches it, and no role works it — so it
// lives with the wire shape that carries it rather than in the domain's phases.
const Unrecorded phase.Phase = "unrecorded"

// State is the daemon's own facts, and nothing another system owns (spec: State and store).
type State struct {
	Daemon DaemonInfo `json:"daemon"`
	// Contract is the contract this body was built at and, where the daemon placed the caller in
	// a pod of the project, the contract that pod's claims record (ContractView): the two
	// numbers the narrowing's own log line names (server's stateFor). Absent from a body the
	// route narrowed, which is every caller below the contract that added it; written on every
	// other body, the operator's `legion state --json` among them, which is served whole and
	// says by this which contract it was built at.
	Contract            *ContractView        `json:"contract,omitempty"`
	Admission           Admission            `json:"admission"`
	Issues              map[string]Issue     `json:"issues"` // the issue record, keyed by issue key
	PendingStatusWrites []PendingStatusWrite `json:"pendingStatusWrites"`
	// HeldOutboxRows is every due lifecycle outbox row held behind its issue's running lifecycle
	// row (record.Store.HeldRows), in the order the store lists them: next_at, then id. Never null;
	// empty while every due lifecycle row runs (contract 28).
	HeldOutboxRows []HeldOutboxRow `json:"heldOutboxRows"`
	// StrandedClaims is every claim nothing will wake (workflow.Stranded), by issue and then
	// role: the trees that will not move again until someone acts. Never null; empty while every
	// claim is running or waits on something that will wake it.
	StrandedClaims []StrandedClaim `json:"strandedClaims"`
	// WakesWaiting is every wake the daemon's pacing holds until its turn
	// (worker_wakes_per_minute), in claim order: the wave a restart, a re-admission or a
	// stale-plugin refresh raised, as far as it has not gone out. It is the two places a wake
	// waits read as one list — the launches and refresh relaunches the machines hold
	// (supervise.ClaimView.WakeWaiting) and the catch-ups waiting on their outbox rows
	// (WakesWaitingOf). Never null; empty while nothing waits, which is every moment outside a
	// wave. With `daemon.startedAt` and ClaimsToRefresh it is what says whether a wave is still
	// running.
	WakesWaiting []WakeWaiting `json:"wakesWaiting"`
	// ClaimsToRefresh is how many claims the stale-plugin lane still has to relaunch onto this
	// daemon's plugin: a claim with a recorded session, whose process holds a capability, whose
	// recorded plugin contract is not DaemonAPIVersion, that is in no pod update round, and whose
	// launch installs a plugin — the set the daemon's own pass walks
	// (daemon's refreshStalePlugins). Always a number, never null, and 0 when the lane has
	// nothing left, which is every moment outside a wave (contract 73).
	//
	// It is what `wakesWaiting` alone cannot say. The lane holds at most one minute of the
	// pacing at a time (supervise.RefreshGate), so the wakes waiting are what it has admitted,
	// never what it has left; a reader of the 2026-11-10 wave counted 395 live processes on the
	// old contract against about 600 of the lane's wakes and nothing in the body reconciled the
	// two (dispatch://INFRA-2712 R4).
	//
	// A daemon that can relaunch onto no plugin at all is in the count like any other: the
	// number stands at what that lane cannot act on, beside the Error line such a daemon writes
	// for each of those claims (supervise.NoPluginSourceMessage).
	ClaimsToRefresh int `json:"claimsToRefresh"`
	// ControllerLocator is the project's controller, absent until a session registers as it: with
	// the capability `legion controller start` fetched, or, under `controller: daemon`, with the boot
	// token of the daemon's own controller launch.
	ControllerLocator *ControllerLocator `json:"controllerLocator,omitempty"`
	// AgentSecretsLogin is the daemon's own agent-secrets token login
	// (runtime.kubernetes.agent_secrets), absent when the deployment configures
	// no broker.
	AgentSecretsLogin *AgentSecretsLoginView `json:"agentSecretsLogin,omitempty"`
	// Capabilities is the deployment's capability report (capabilities.Deployment.Report), one row
	// per capability in the table's order: what the worker image was probed for, what the live
	// sessions' reports prove (present), fail (open) or have not reported yet (unchecked), what a
	// cited issue withholds, and what the deployment's own configuration closes, decides or leaves open
	// — each open deployment row with the legion.yaml line that records a decision. Never null; a
	// gap is reported here, never refused (contract 16).
	Capabilities []CapabilityState `json:"capabilities"`
	// Repositories is the project's repository mapping as the daemon serves it: the owners its
	// roles' credentials span, each repository it serves with its cache, each mapped repository it
	// leaves out with why (a GitHub App not installed on it), and the caches of repositories the
	// mapping no longer names. Never null: a daemon with no workflow serves none.
	Repositories RepositoriesView `json:"repositories"`
	// Settings is the daemon's resolved legion.yaml with every credential, credential source and
	// credential file path left out, keyed as the file is (SettingsOf). Required, never null
	// inside: every collection in it is emitted non-nil (contract 35).
	Settings Settings `json:"settings"`
	// Consumers is the lag of the daemon's durable consumers (ConsumerLagView), read from
	// NATS at the request after the state's transaction commits, so no NATS read holds the
	// snapshot open. Never null: a daemon with no workflow has none (contract 43).
	Consumers []ConsumerLagView `json:"consumers"`
	// wakesWaitingHidden is set on the copy the state route serves a caller whose plugin is older
	// than the contract that added WakesWaiting (api's withoutWakesWaiting, below server.go's
	// wakesWaitingContract): that reader knows no `wakesWaiting` and refuses the key as surely
	// when it is empty as when it names a wake, so MarshalJSON leaves the key off that one body
	// rather than writing it empty, as Admission.treesHidden does for `trees`.
	wakesWaitingHidden bool
	// claimsToRefreshHidden is the same for ClaimsToRefresh, set on the copy the route serves a
	// caller below server.go's claimsToRefreshContract (api's withoutClaimsToRefresh): the count
	// is written on every other body, 0 included, and a strict reader before that contract
	// refuses the key when it is zero as surely as when it names a number, so MarshalJSON
	// leaves it off that one body. The two flags compose on one body: a caller below
	// wakesWaitingContract is below this contract too, and is served neither key.
	claimsToRefreshHidden bool
}

// ClaimsToRefreshField is the name ClaimsToRefresh carries on the wire, read from here by the
// tests of its contract band, which look for the key rather than repeating the string: whether
// the key is on the wire at all is what the band is read by, since the reader below it refuses
// the key when it is 0 as surely as when it names a number (server's withoutClaimsToRefresh).
const ClaimsToRefreshField = "claimsToRefresh"

// ConsumerLagView is one durable consumer's lag as the state shows it (intake.ConsumerLag): its
// name; how many messages wait for it (Waiting); the stream time of the oldest and its age in whole
// seconds at the read, both null when nothing waits or when that message could not be read; when
// the daemon last handled one of its messages, null before the first; whether it is stalled,
// messages waiting and none handled for 60 seconds; and Unavailable, what NATS did not answer or
// refused, absent when every read answered.
type ConsumerLagView struct {
	Name             string     `json:"name"`
	Waiting          uint64     `json:"waiting"`
	OldestAt         *time.Time `json:"oldestAt"`
	OldestAgeSeconds *int64     `json:"oldestAgeSeconds"`
	LastHandledAt    *time.Time `json:"lastHandledAt"`
	Stalled          bool       `json:"stalled"`
	Unavailable      string     `json:"unavailable,omitempty"`
}

// ConsumerLagViewsOf is lags as the state shows them, consumer for consumer; no lags is an empty
// list.
func ConsumerLagViewsOf(lags []intake.ConsumerLag) []ConsumerLagView {
	views := make([]ConsumerLagView, len(lags))
	for i, lag := range lags {
		view := ConsumerLagView{Name: lag.Name, Waiting: lag.Waiting, Stalled: lag.Stalled, Unavailable: lag.Unavailable}
		if !lag.OldestAt.IsZero() {
			oldestAt, age := lag.OldestAt, int64(lag.OldestAge/time.Second)
			view.OldestAt, view.OldestAgeSeconds = &oldestAt, &age
		}
		if !lag.LastHandledAt.IsZero() {
			handledAt := lag.LastHandledAt
			view.LastHandledAt = &handledAt
		}
		views[i] = view
	}
	return views
}

// RepositoriesView is every repository the daemon works in, as it serves them: the repositories
// the project's Dispatch mapping names and the repositories the project's own records name, each
// with the installation of each role App on it (INFRA-2543 R6). The mapping decides the
// project's default repository and the repositories the daemon prepares before an issue names
// one, and no longer decides where the project may work: the App installation does, so a
// repository a record names is served exactly as a mapped one and no repository is left out for
// want of a mapping.
//
// Owners is every mapped repository's owner account, which is an account both role Apps are
// installed on, since the mapping serves a repository only once they are. It is not the reach
// a pane's addressing names: that is the owners of the pane's own role's App, which the daemon
// reads from what the last gh bundle of that App found (daemon.RepositoriesClause), and this
// block reports no App's installations. Owners, Mapped, Refused, WithoutWebhookApp and Caches
// are sorted, and never null. Caches are the repository caches of repositories the daemon no
// longer serves, each kept until its RetainedUntil (contract 32). WithoutWebhookApp is each
// served repository Dispatch's GitHub App is not known to be installed on, with why
// (contract 43).
type RepositoriesView struct {
	Owners            []string                `json:"owners"`
	Mapped            []MappedRepositoryView  `json:"mapped"`
	Refused           []RefusedRepositoryView `json:"refused"`
	WithoutWebhookApp []WebhookAppGapView     `json:"withoutWebhookApp"`
	Caches            []CacheView             `json:"caches"`
}

// MappedRepositoryView is a repository the daemon serves, as GitHub names it, whether the
// project's Dispatch mapping names it or one of the project's own issue records does: whether
// it is the project's default in Dispatch, which the mapping still decides and a record never
// does; each role App's installation on it (Apps); and, under the Kubernetes runtime, its
// repository cache once the runtime has made one (absent before, and under tmux).
type MappedRepositoryView struct {
	Repo    string               `json:"repo"`
	Default bool                 `json:"default"`
	Apps    AppInstallationsView `json:"apps"`
	Cache   *CacheView           `json:"cache,omitempty"`
}

// AppInstallationsView is each role App's installation on one repository, which is what decides
// whether a project can work there at all (INFRA-2543 R6): the implement App a worker pushes and
// opens pull requests with, and the review App a reviewer approves with. It is a field rather
// than a sentence a reader must parse out of a refusal's reason, because it is the answer an
// operator and a plugin both ask of a repository, and the daemon holds it for every repository it
// has facts for.
//
// Both are true on a repository the daemon serves, which is the admission rule of each of the
// two doors into the served set: a mapped repository one of the two Apps does not reach is
// served for nothing — no token exists for its work — so it stands under
// RepositoriesView.Refused instead, where the reason names the App that is missing or the read
// that did not answer; and an issue is recorded on a repository no mapping names only once
// GitHub has answered that both Apps are installed on it.
type AppInstallationsView struct {
	Implement bool `json:"implement"`
	Review    bool `json:"review"`
}

// CacheView is a repository cache as the Kubernetes runtime shows it: its repository and Sandbox;
// its state — starting, cloning, ready, failing with the kubelet's reason, or refused, naming the
// Service of its name that is not its own and why — and, while its pod is Ready behind its own
// Service, its address; from its last health read (the runtime's minute tick) the default
// branch's head, when it last fetched GitHub and the bytes it serves, or why its /healthz could not
// be read; and, for a repository the mapping no longer names, until when it is kept (contract 32).
type CacheView struct {
	Repo          string    `json:"repo"`
	Sandbox       string    `json:"sandbox"`
	State         string    `json:"state"`
	Reason        string    `json:"reason,omitempty"`
	Ready         bool      `json:"ready"`
	Address       string    `json:"address,omitempty"`
	Head          string    `json:"head,omitempty"`
	LastFetchAt   time.Time `json:"lastFetchAt,omitzero"`
	Bytes         int64     `json:"bytes,omitzero"`
	HealthError   string    `json:"healthError,omitempty"`
	RetainedUntil time.Time `json:"retainedUntil,omitzero"`
}

// RefusedRepositoryView is a repository the daemon leaves out, as Dispatch maps it or as a record
// names it, and why: a role App is not installed on it, or that installation could not be read.
// A repository no mapping names is never refused for that (INFRA-2543 R6), since the mapping
// stopped deciding where a project may work.
type RefusedRepositoryView struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

// WebhookAppGapView is a served repository that Dispatch's GitHub App is not installed on, or
// whose check failed, and why. The daemon serves it all the same.
type WebhookAppGapView struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

// MarshalJSON keeps the five lists arrays on the wire.
func (r RepositoriesView) MarshalJSON() ([]byte, error) {
	type wire RepositoriesView
	out := wire(r)
	if out.Owners == nil {
		out.Owners = []string{}
	}
	if out.Mapped == nil {
		out.Mapped = []MappedRepositoryView{}
	}
	if out.Refused == nil {
		out.Refused = []RefusedRepositoryView{}
	}
	if out.WithoutWebhookApp == nil {
		out.WithoutWebhookApp = []WebhookAppGapView{}
	}
	if out.Caches == nil {
		out.Caches = []CacheView{}
	}
	return json.Marshal(out)
}

// CapabilityState is one row of that report. Status is "present", "unchecked", "withheld",
// "decided" or "open" (the probe's own table also prints "missing" and "reported", which never
// reach the state); Detail is the row's evidence whatever the status; Decision is the operator's
// reason on a decided row; ConfigLine is, on an open deployment row, the legion.yaml line that
// records a decision (`capabilities.decided.<name>: "<reason>"`); an open live row has none.
// Source is, on a deployment row, "configuration" (what legion.yaml alone decides) or "boot"
// (what boot measured); absent on every other row.
type CapabilityState struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	Decision   string `json:"decision,omitempty"`
	ConfigLine string `json:"configLine,omitempty"`
	Source     string `json:"source,omitempty"`
}

// CapabilityStatesOf is report as the state shows it, row for row.
func CapabilityStatesOf(report []capabilities.State) []CapabilityState {
	states := make([]CapabilityState, len(report))
	for i, row := range report {
		states[i] = CapabilityState{Name: string(row.Name), Status: row.Status, Detail: row.Detail, Decision: row.Decision, ConfigLine: row.ConfigLine, Source: string(row.Source)}
	}
	return states
}

// ControllerLocator is the record of the session registered as the project's controller: the
// runtime the daemon runs under, the session, and when it registered. External is true for both
// launches, a wire constant since contract 4: the operator's controller has no process the daemon
// can address, and the daemon's own (`controller: daemon`) is a claim whose process the operator
// routes and the claims list show, as any claim's. The operator's is alive while the Envoy listener
// names that session as the controller role's holder within the liveness window
// (`controller.Prober`).
type ControllerLocator struct {
	Runtime      string    `json:"runtime"`
	External     bool      `json:"external"`
	SessionID    string    `json:"sessionId"`
	RegisteredAt time.Time `json:"registeredAt"`
	// PluginContract is the contract the registered session's plugin declared, 0 when none
	// recorded, always emitted.
	PluginContract int `json:"pluginContract"`
}

// ControllerLocatorOf is record as the state shows it under the runtime runtimeName names: the
// registered session's external record, or nil while no session holds the current capability.
func ControllerLocatorOf(runtimeName string, record controller.Record) *ControllerLocator {
	if !record.Registered() {
		return nil
	}
	return &ControllerLocator{Runtime: runtimeName, External: true, SessionID: record.Session, RegisteredAt: record.RegisteredAt, PluginContract: record.PluginContract}
}

// AgentSecretsLoginView is the daemon's own agent-secrets token login's current status
// (agentsecrets.Client.LoginStatus): State is one of "none" (no login has finished yet), "issued",
// or "failed" (the latest login won no credential). Never a key, a token, a launcher credential, or
// why a login failed — those live only in the daemon's process memory and its log.
type AgentSecretsLoginView struct {
	State string `json:"state"`
}

// MarshalJSON keeps `issues` an object and `pendingStatusWrites`, `heldOutboxRows`,
// `strandedClaims`, `wakesWaiting`, `capabilities` and `consumers` arrays on the wire: a nil Go map
// or slice is `null`, which the plugin's reader refuses. The exceptions are the two keys a
// contract band hides from a caller below it, `wakesWaiting` and `claimsToRefresh`: such a body
// carries no key of that name at all, and is marshaled through a value whose own field of that
// name shadows the embedded one, which is how Admission.MarshalJSON leaves off `trees`.
//
// One shadow type carries both keys, and both its fields are pointers. `omitempty` leaves out a
// nil pointer, and a pointer at a value writes that value whatever it holds: `[]` for the empty
// `wakesWaiting` every other body carries, `0` for the `claimsToRefresh` of a drained lane. A
// `[]WakeWaiting` field could not write the empty array, since an empty slice is what
// `omitempty` drops, so the pointer is what lets one type serve both keys — each field set when
// its key is not hidden and left nil when it is — rather than a struct per pair of flags. The
// two bands compose, so a caller below wakesWaitingContract (53) is below claimsToRefreshContract
// (73) as well and is served neither key; a third banded key is one more field here, not four
// more shapes.
//
// A body that hides neither key is marshaled through the embedded value alone. The shadow's own
// fields are declared after the embedded one, so encoding/json writes a key that comes from them
// after every other key of the body, and the fixtures under `packages/contracts/fixtures/daemon-api`
// pin bodies that hide nothing: marshaling those through the shadow would move both keys to the
// end of each pinned body.
func (s State) MarshalJSON() ([]byte, error) {
	type wire State
	out := wire(s)
	if out.Issues == nil {
		out.Issues = map[string]Issue{}
	}
	if out.PendingStatusWrites == nil {
		out.PendingStatusWrites = []PendingStatusWrite{}
	}
	if out.HeldOutboxRows == nil {
		out.HeldOutboxRows = []HeldOutboxRow{}
	}
	if out.StrandedClaims == nil {
		out.StrandedClaims = []StrandedClaim{}
	}
	if out.Capabilities == nil {
		out.Capabilities = []CapabilityState{}
	}
	if out.Consumers == nil {
		out.Consumers = []ConsumerLagView{}
	}
	if out.WakesWaiting == nil {
		out.WakesWaiting = []WakeWaiting{}
	}
	if !s.wakesWaitingHidden && !s.claimsToRefreshHidden {
		return json.Marshal(out)
	}
	shadow := struct {
		wire
		WakesWaiting    *[]WakeWaiting `json:"wakesWaiting,omitempty"`
		ClaimsToRefresh *int           `json:"claimsToRefresh,omitempty"`
	}{wire: out}
	if !s.wakesWaitingHidden {
		shadow.WakesWaiting = &shadow.wire.WakesWaiting
	}
	if !s.claimsToRefreshHidden {
		shadow.ClaimsToRefresh = &shadow.wire.ClaimsToRefresh
	}
	return json.Marshal(shadow)
}

// DaemonInfo is the running daemon itself: which project it coordinates, the schema its store is
// migrated to, and its boot history.
type DaemonInfo struct {
	Project       string    `json:"project"`
	SchemaVersion int       `json:"schemaVersion"`
	Boots         int       `json:"boots"`
	FirstBootAt   time.Time `json:"firstBootAt"`
	StartedAt     time.Time `json:"startedAt"`
}

// ContractView is the mark of the narrowing: the contract this body was built at and the
// contract its caller records (`contract`, contract 75). Daemon is DaemonAPIVersion, what this
// binary declares and serves; Caller is the plugin contract the claims of the caller's own pod
// record, absent for a caller the daemon places in no pod — the operator's `legion state
// --json`, a tmux pane, a pod whose claims record no contract — whose body is served whole and
// narrowed by nothing. The two stand beside the daemon's own block rather than inside it,
// because the second number is the caller's and no fact of the daemon.
//
// The mark rides under the contract that decided the record (server's recordFieldsContract), so
// a caller below that contract is served no `contract` key at all (withoutRecordFields): a key
// added to a narrowed body is exactly what the narrowing exists to prevent, since the plugin
// that body is narrowed for reads the state with a strict schema and would refuse the whole of
// it for one key it does not name. Such a caller is told nothing of the narrowing here; the
// daemon's own log says it, one line per narrowing (server's stateFor).
type ContractView struct {
	Daemon int `json:"daemon"`
	Caller int `json:"caller,omitempty"`
}

// Admission is the issue cap and the issues under it, in Dispatch rank order. Concurrency is
// capped on issues, never on workers, so there is no worker queue here.
type Admission struct {
	Cap int `json:"cap"`
	// Active is every issue holding an admission slot, a child as much as a root: the cap counts
	// the issues the daemon has started rather than the trees (dispatch://INFRA-980), and an
	// issue keeps its slot from the moment the daemon starts it until it is done or leaves the
	// workflow (record.KeepsSlot). So one tree with three started children holds three of these,
	// and an issue waiting on a person, one in accepting, one held, a merger at work in merging,
	// one in merging whose start a closed design gate holds (the outbox's
	// mergerWaitsForTheDesignGate) and a merged root standing in merging while the rest of its
	// tree runs are each here, while an issue whose phase has left the workflow — done, or a
	// status Dispatch took it out of the workflow with — is in neither list.
	Active []string `json:"active"`
	// Waiting is the admission line, in the line's own order: the issues whose work came back,
	// oldest first, then the issues whose first phase can start, by their tree root's Dispatch
	// rank. It carries children as well as roots, for the same reason `active` does. An owed
	// start in it holds no row and waits for one; an issue in it that holds a row already (the
	// kinds record.LineEntry.Holds names) stands in both lists until the pass that promotes it,
	// which takes no second row.
	Waiting []string `json:"waiting"`
	// Trees is the root of every live tree the daemon judges (record.TreeLive), sorted by key: a
	// tree whose lifecycle stands open under the workflow and does not linger, one whose root
	// waits in the line, and one whose root waits on a person. A slot is no longer one per tree,
	// so a live tree can hold no slot at all — a root still waiting in the line for its tree to
	// be opened holds none, and neither does a child of it — and `active` alone no longer tells
	// a reader such as the controller which trees run. Never null; empty when no tree is live.
	Trees []string `json:"trees"`
	// Blocked is every issue Legion is waiting to start that waits on a Dispatch issue nobody has
	// finished (INFRA-225), by issue key: a waiting root admission skips and a child the workflow
	// holds at `admitted`, together in one list, each with what it still waits on. A blocked issue
	// the line carries is in Waiting too. Never null; empty when nothing waits on a link.
	Blocked []BlockedIssue `json:"blocked"`
	// treesHidden is set on the copy the state route serves a caller whose plugin is older than
	// the contract that added Trees (api's withoutTrees, below server.go's treeContract): that
	// reader knows no `trees` and refuses the key as surely when it is empty as when it names a
	// tree, so MarshalJSON leaves the key off that one body rather than writing it empty.
	treesHidden bool
}

// BlockedIssue is one issue of Blocked and the issues it waits on that are not done. It is
// record.BlockedIssue: the state's `blocked` and a tick controller notice's are the same list
// read two ways, and the row is declared once, under both.
type BlockedIssue = record.BlockedIssue

// MarshalJSON keeps `active`, `waiting`, `trees` and `blocked` arrays on the wire, empty rather
// than `null` — except on an admission whose trees are hidden, which carries no `trees` key at
// all. That body is marshaled through a value whose own nil `trees` field, being `omitempty`,
// shadows the embedded one and so is written by nothing; every other key of the block, including
// one a later contract adds above, is still written from the one place that writes it.
func (a Admission) MarshalJSON() ([]byte, error) {
	type wire Admission
	out := wire(a)
	if out.Active == nil {
		out.Active = []string{}
	}
	if out.Waiting == nil {
		out.Waiting = []string{}
	}
	if out.Blocked == nil {
		out.Blocked = []BlockedIssue{}
	}
	if a.treesHidden {
		out.Trees = nil
		return json.Marshal(struct {
			wire
			Trees []string `json:"trees,omitempty"`
		}{wire: out})
	}
	if out.Trees == nil {
		out.Trees = []string{}
	}
	return json.Marshal(out)
}

type Issue struct {
	Key        string      `json:"key"`
	Generation uint64      `json:"generation"`
	Phase      phase.Phase `json:"phase"`
	// Status is the status the daemon means the issue to have and writes to Dispatch
	// (record.Issue.Status). It is not the status Dispatch shows: that is
	// record.Issue.DispatchStatus, which this state does not carry. The two differ while a status
	// write is queued, and go on differing where one was lost until the status repair puts it
	// back, so a reader who wants the status Dispatch shows must read Dispatch.
	Status string `json:"status"`
	// Tree is the key of the root of the issue's tree, which equals Key on a root. Since a child
	// holds a slot of its own, neither `admission.active` nor `admission.waiting` says any more
	// which tree an issue belongs to, and a reader needs it to tell a child from a root and to
	// find the members of a tree that lingers. Absent on an issue the workflow does not record
	// (an operator's claim, Unrecorded), which stands in no tree.
	Tree string `json:"tree,omitempty"`
	// Repo is the repository (owner/name) the issue targets and DefaultBranch that repository's
	// default branch, both taken when the daemon first recorded the issue and never changed after.
	// Both are absent on an issue the workflow does not record (an operator's claim, Unrecorded).
	Repo          string `json:"repo,omitempty"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// HoldReason is why a held issue is held, when its hold has one: `escalated` once its architect
	// sent it to the controller. Absent otherwise, and absent while the issue's tree lingers or is
	// closed: the record keeps the reason, and the view shows it again once the tree is re-admitted.
	HoldReason string `json:"holdReason,omitempty"`
	// HeldFrom is the phase the hold took the issue out of (record.Hold.From), and absent for an
	// issue that is not held. Unlike HoldReason it is shown whatever the issue's tree is doing:
	// the reason is a decision someone must act on, which a lingering tree has none of, while the
	// phase is a fact of the record that costs one word, so a reader of this body is held to the
	// simple rule that every issue at `phase: held` names the phase that stopped.
	HeldFrom string `json:"heldFrom,omitempty"`
	// LingerUntil is when the linger of a tree whose work finished runs out
	// (record.Issue.LingerUntil), and is absent for every issue that is not a lingering root:
	// only a root carries a linger, and re-admission clears it. It is what keeps HoldReason's
	// own suppression honest — a held issue of a lingering tree is served no reason, so without
	// this key an absent reason reads either as a hold nobody has given a reason or as a tree
	// that lingers, and a reader of the two cannot tell which. The controller's texts turn on
	// that difference.
	LingerUntil *time.Time `json:"lingerUntil,omitempty"`
	// Ancestry is the keys of a labelled root's Dispatch parent chain, from its parent upward
	// (record.Issue.Ancestry), and is absent for every issue the record holds none for — a root
	// that ran as a tree of its own, and every member of a tree. It is what a root held under an
	// unreleased child of a live tree is held by: that root waits for no slot while any live root
	// stands on its chain, and until now the chain reached no surface at all, so the wait had no
	// reading on this wire.
	Ancestry  []string   `json:"ancestry,omitempty"`
	Architect *ClaimView `json:"architect,omitempty"`
	// Workers is keyed by the role that holds the claim; the vocabulary of a claim belongs to
	// `internal/claim`, which the runtime, the worker stream, and the supervisor all speak
	// without importing this package.
	Workers     map[claim.Role]PhaseView `json:"workers"`
	PullRequest *PullRequestView         `json:"pullRequest,omitempty"`
	DesignGate  *GateView                `json:"designGate,omitempty"`
	// GatedSteps is the deployment's design gate as this issue stands in it: one entry for each
	// step the deployment gates, in the order it gates them (contract 55). It is absent, as the
	// gate itself is, for an issue that is not its tree's root, for a deployment that gates no
	// step and for an issue the workflow does not record, so the wire never carries an empty
	// list. DesignGate is unchanged beside it: it is the spec step's row, which every reader that
	// asks one question of one gate still takes.
	GatedSteps []GatedStepView `json:"gatedSteps,omitempty"`
	Slot       *SlotView       `json:"slot,omitempty"`
	// WaitingLine is where this issue stands in `admission.waiting` (contract 58). It is present
	// only for an issue that list names, a slot holder whose work came back among them, and
	// absent for every other issue.
	WaitingLine *WaitingLineView `json:"waitingLine,omitempty"`
	// Why is the one rendered answer for an issue that is not working now (WhyView). It sits
	// here beside `waitingLine` and never inside it, because an issue whose claim has stopped
	// can stand in the line or outside it — such a member carries this key and its
	// `waitingLine` both, and reads `claim_stopped`, the state that wins where both hold
	// (projection's whyStates) — and it is the same answer for every issue that carries one.
	// The projection decides each issue's state and renders its sentence; an issue that is
	// working, and one the record does not hold in the workflow, carries no such key.
	Why *WhyView `json:"why,omitempty"`
	// WaitingOn is present only while the root is in awaiting_person: what it waits on from a
	// person, when the wait began and, once a person answered, when and by what act.
	WaitingOn *WaitingOnView `json:"waitingOn,omitempty"`
	// Check is the issue's open production check, the work after its close (record.ProductionCheck):
	// present only while the check is open, and absent once it closed or while none was opened.
	Check *CheckView `json:"check,omitempty"`
	// MemoryGroup is the memory group of the issue's tree (memorygroup.Group): `memory` for a
	// tree whose roles run with the project's memory overlays and instructions, `none` for one
	// that runs without them, and absent while nothing has drawn for the tree — a tree first
	// opened before the setting existed, or one the draw never reached. MemoryDrawMode is the
	// project's memory mode in force when that group was drawn (config.MemoryMode's words) and
	// MemoryDrawnAt when it was drawn, both absent with the group. The draw is made once, on the
	// tree's root record, when the tree's lifecycle row is first created, and every issue of the
	// tree shows its root's: an issue's memory group is its tree's (contract 49).
	MemoryGroup    string     `json:"memoryGroup,omitempty"`
	MemoryDrawMode string     `json:"memoryDrawMode,omitempty"`
	MemoryDrawnAt  *time.Time `json:"memoryDrawnAt,omitempty"`
}

// WaitingOnView is what a root in awaiting_person waits on from a person (record.Awaiting). Kind
// is `ask` with Ask, the id of the oldest question of its tree still with the person, or `gate`
// with GateVersion and ArtifactID, the spec version of its registered document a person has not
// reviewed. Since is Dispatch's time of the hand-back the wait began at,
// absent when unknown. AnsweredAt and AnsweredBy are absent until a person's act answers the
// wait; the answered root then stands in admission's waiting line as well, among the issues
// whose work came back, oldest first, until the pass that resumes it. AnsweredBy is every value
// of record.Answer, `declined` — a person who closed an ask of the root rather than answer it
// (contract 68) — among them, and it names how the wait was answered, not always by a person.
type WaitingOnView struct {
	Kind        string        `json:"kind"`
	Ask         string        `json:"ask,omitempty"`
	GateVersion int           `json:"gateVersion,omitempty"`
	ArtifactID  string        `json:"artifactId,omitempty"`
	Since       *time.Time    `json:"since,omitempty"`
	AnsweredAt  *time.Time    `json:"answeredAt,omitempty"`
	AnsweredBy  record.Answer `json:"answeredBy,omitempty"`
}

// MarshalJSON keeps `workers` an object on the wire for an issue that has no worker yet.
func (i Issue) MarshalJSON() ([]byte, error) {
	type wire Issue
	out := wire(i)
	if out.Workers == nil {
		out.Workers = map[claim.Role]PhaseView{}
	}
	return json.Marshal(out)
}

// ClaimView is one role claim as the record holds it: the session its agent registered as, where
// it is in its life (`supervise.ClaimState`), its locator, and the capability report its session
// sent on a `claims/ready` of its own while it ran (LEGION-663, INFRA-927). The locator is the
// runtime's own nested shape —
// `{"runtime":"tmux","claim":…,"incarnation":…,"tmux":{"window":…,"pane":…}}` or the `sandbox`
// member for a pod — marshalled by the standard library from `runtime.Locator`; the process
// incarnation lives there and in the capability report's `incarnation`, which is the reporting
// process's: a reader compares it with `locator.incarnation` to tell the running process's report
// from a predecessor's. A socket path never appears in the view. A claim with no process (queued,
// suspended, failed, retired) has no locator; a claim whose session has not reported, or whose
// role never reports (the controller), has no capabilities. A claim whose agent has stepped —
// begun a turn, or made a call the daemon's stream saw — carries that step too (StepView), and a
// claim the daemon holds a failure record for carries why it failed (ClaimFailureView).
type ClaimView struct {
	Session      string                `json:"session"`
	State        string                `json:"state"`
	Locator      *runtime.Locator      `json:"locator,omitempty"`
	Capabilities *CapabilityReportView `json:"capabilities,omitempty"`
	// ServingGeneration is the issue generation of the last confirmed delivery this claim
	// retired — the run it goes on working until a delivery of another run replaces it
	// (supervise.Claim.ServingGeneration) — and is absent for a claim that has retired no
	// confirmed task of a run. It is what says whether the work a claim serves is of this
	// generation: a reader holding it against the issue's own `generation` sees a worker still
	// turning over a run its issue has left, which nothing else on this wire says.
	ServingGeneration uint64 `json:"servingGeneration,omitempty"`
	// PluginContract is the contract the claim's current registration declared, 0 when none
	// recorded, always emitted.
	PluginContract int `json:"pluginContract"`
	// UpdateRound is the update round the claim is in, and absent when none is open: the pod's
	// drift round (`pod:<pod uid>`), which every claim of that pod shares, and which is the one
	// round this daemon opens. A claim's own plugin round (`plugin:<daemon contract>`) is an
	// earlier daemon's: the stale-plugin lane asks its worker nothing and opens no round, and a
	// boot takes every claim out of one it finds (daemon's endPluginRounds), so the key carries
	// such a round only in the moments before that boot pass reaches it. Once its worker has
	// answered, the process holding
	// this claim is waiting to be replaced and the daemon holds the claim's next task. UpdateHead
	// is the commit the worker reported as the head of its issue branch at origin when it answered
	// the round, and is absent until it answers and for a worker that reported none (a planner
	// before its first push). It is the worker's word, which nothing verifies. UpdateReasons is
	// what the round was opened for, as the daemon found the drift, and is absent while no round
	// is open: the worker answering a round is told to act on it, so the seat that answers reads
	// why its pod is being replaced rather than the task's word for it alone.
	UpdateRound   string `json:"updateRound,omitempty"`
	UpdateHead    string `json:"updateHead,omitempty"`
	UpdateReasons string `json:"updateReasons,omitempty"`
	// LastTurnStart and LastTurnEnd are when the claim's agent last began a turn and last ended
	// one, each absent until that happens: a claim that has worked since this release records
	// them, and one that last ran before it records neither. They are what says whether a worker
	// is still making progress — a working claim whose turn began hours ago is wedged, and an idle
	// one that last ended a turn yesterday has been told nothing since.
	LastTurnStart *time.Time `json:"lastTurnStart,omitempty"`
	LastTurnEnd   *time.Time `json:"lastTurnEnd,omitempty"`
	// LastDeathCause is why the claim's process was last found gone, LastStartFailure why its last
	// start did not produce a running agent — a launcher that refused it, a failed init container,
	// a pod that never became ready — and LastReady when its agent last said it could be prompted.
	// Each is absent until it happens. The two causes are separate because they answer separate
	// questions, and LastReady is what dates them: a cause older than it is one the agent running
	// now has already survived.
	LastDeathCause   string     `json:"lastDeathCause,omitempty"`
	LastStartFailure string     `json:"lastStartFailure,omitempty"`
	LastReady        *time.Time `json:"lastReady,omitempty"`
	// ProviderErrorRun is how many turns of this claim ended in a provider error in a row
	// (supervise.Claim.ProviderErrorRun), and is absent for a claim whose last turn ended with
	// none. It bounds nothing — a limit here would fail every claim of a project at once in a
	// provider outage (dispatch://INFRA-956's R2) — and the project's controller is told of it
	// once, at a run of six, so without this key the number behind that telling stands on no
	// surface its reader can reach.
	ProviderErrorRun int `json:"providerErrorRun,omitempty"`
	// PendingTask is the task the claim holds, reduced to what a reader of this surface decides
	// on and never carrying its text (PendingTaskView); absent for a claim holding none.
	PendingTask *PendingTaskView `json:"pendingTask,omitempty"`
	// Step is what the daemon last saw this claim's agent do (supervise.StepRecord), and is
	// absent for a claim whose agent has neither begun a turn nor made a call the daemon saw.
	Step *StepView `json:"step,omitempty"`
	// Failure is why the claim failed, and is absent for a claim the daemon holds no cause and
	// sentence for (ClaimFailureView, FailureViewOf). PendingCause beside it is what failed the
	// process whose relaunch is now in flight (supervise.Claim.PendingCause), and is absent for a
	// claim relaunching after no failure: a claim that died and whose relaunch the runtime then
	// refused fails under the cause of the death rather than under the refusal, so the cause of
	// the death whose relaunch is in flight is what an architect deciding a retry or an
	// escalation reads while that relaunch is still going on and `failure` is empty.
	Failure      *ClaimFailureView      `json:"failure,omitempty"`
	PendingCause supervise.FailureCause `json:"pendingCause,omitempty"`
}

// ClaimFailureView is the failure record a claim carries as a claim view shows it: Cause is the
// one word the site that failed the claim chose (supervise.FailureCause), Detail the sentence
// that site wrote beside it (supervise.Claim.Failure), At the moment it failed and Attempts the
// four counts it failed with (supervise.Budgets), which the limits in `settings` are read
// against. At is absent for a claim whose record carries no moment, which is every failure the
// migration that added the columns backfilled.
//
// A claim carries the record until it is given another run, because that is what clears it: the
// retry clears the cause with the counts it names and the launch of that run clears it again
// (supervise's retry and launchNow). A claim an operator retired after it failed carries it
// still, which is what lets an agent reading a retired claim say why it is there, and a claim
// that never failed carries none of it.
type ClaimFailureView struct {
	Cause    supervise.FailureCause `json:"cause"`
	Detail   string                 `json:"detail"`
	At       *time.Time             `json:"at,omitempty"`
	Attempts BudgetsView            `json:"attempts"`
}

// FailureViewOf is c's failure record as a claim view shows it, and nil for a claim the daemon
// holds no cause and sentence for, so no such claim carries the key. It is keyed on the record
// rather than on the claim's state: the state says where the claim is now, and the record says
// what the daemon last recorded about it, which outlives a retirement.
//
// Both the cause and the sentence are read, because an image rolled back and forward again can
// leave the one without the other. The release before this one clears `failure` at every launch
// and names `failure_cause` and `failed_at` in no statement (its upsert), so a claim it
// relaunched carries a cause and no sentence, and serving that would say a working claim failed.
// Keying on both loses nothing correct: every live `fail` writes a sentence, and the migration
// backfills only the rows that hold one. The one state it cannot tell apart is a claim that
// failed again under the older daemon, which wrote the new sentence and left the cause and the
// moment of the failure before it: the record then reads with an earlier failure's cause beside
// a later failure's sentence, and nothing in the row says so.
func FailureViewOf(c supervise.Claim) *ClaimFailureView {
	if c.FailureCause == "" || c.Failure == "" {
		return nil
	}
	return &ClaimFailureView{
		Cause:    c.FailureCause,
		Detail:   c.Failure,
		At:       instant(c.FailedAt),
		Attempts: BudgetsViewOf(c.Budgets),
	}
}

// PendingTaskView is the task a claim holds as a claim view shows it (supervise.Delivery),
// reduced to the five facts a reader of an agent surface decides on: that one is held, which the
// key's own presence says; Phase, the issue phase it was queued for, empty for a task of no
// phase (an operator's own, an architect's); QueuedAt, when it was queued; and SendFailures with
// LastSendError, how many sends of it came back without reaching a turn and the error the last
// of them failed with, each absent for a task no send has failed for.
//
// The task's own text is never here. The operator routes serve it whole (DeliveryView), and what
// an agent decides on is that a worker holds a task and has held it since a moment it can read,
// not the prose another seat was sent: a sibling's task text on this surface would be a second
// copy of the workflow's own words, served to every reader of the state.
type PendingTaskView struct {
	Phase         phase.Phase `json:"phase,omitempty"`
	QueuedAt      time.Time   `json:"queuedAt"`
	SendFailures  int         `json:"sendFailures,omitempty"`
	LastSendError string      `json:"lastSendError,omitempty"`
}

// PendingTaskViewOf is d as a claim view shows it, and nil for a claim holding no task, so no
// such claim carries the key.
func PendingTaskViewOf(d *supervise.Delivery) *PendingTaskView {
	if d == nil {
		return nil
	}
	return &PendingTaskView{
		Phase:         d.Phase,
		QueuedAt:      d.QueuedAt,
		SendFailures:  d.SendFailures,
		LastSendError: d.LastSendError,
	}
}

// ClaimViewOf is c as every agent surface shows it: the one place a claim record becomes the
// view `read_record` and `read_state` carry, so what those surfaces serve of a claim is decided
// here and nowhere else (ClaimRecordFields names the decision for each field of the record). The
// capability report is not of the claim — it is what the claim's session last reported — so its
// caller sets it beside this.
func ClaimViewOf(c supervise.Claim) ClaimView {
	return ClaimView{
		Session:           c.Session,
		State:             string(c.State),
		Locator:           c.Locator,
		ServingGeneration: c.ServingGeneration,
		PluginContract:    c.PluginContract,
		UpdateRound:       c.UpdateRound,
		UpdateHead:        c.UpdateHead,
		UpdateReasons:     c.UpdateReasons,
		ProviderErrorRun:  c.ProviderErrorRun,
		PendingTask:       PendingTaskViewOf(c.Pending),
		// What the daemon last saw this claim's agent do, which the stall backstop and an
		// operator read alike; nil for a claim whose agent has not stepped (StepViewOf).
		Step: StepViewOf(c.Step),
		// When the agent last began and last ended a turn, and when it last said it could be
		// prompted, each absent until it happens: they are what tells a worker still making
		// progress from one wedged in a turn, and what dates the two causes below.
		LastTurnStart:    instant(c.LastTurnStart),
		LastTurnEnd:      instant(c.LastTurnEnd),
		LastReady:        instant(c.LastReady),
		LastDeathCause:   c.LastDeathCause,
		LastStartFailure: c.LastStartFailure,
		// Why the claim failed, for as long as the daemon holds a record of it: a claim an
		// operator retired after it failed carries the record still, since nothing but the launch
		// that runs it again clears it (FailureViewOf). It is keyed on the stored record rather
		// than on the claim's state for exactly that reason.
		Failure:      FailureViewOf(c),
		PendingCause: c.PendingCause,
	}
}

// StepView is the claim's step record as a claim view shows it (supervise.StepRecord): when the
// daemon last saw the claim's agent step, when the turn that step belongs to began, and the call
// the agent is in. It is what says a worker mid-turn is still moving, so the stall backstop and
// an operator read the same record.
//
// The call's arguments are never here: their length and an eight-byte fingerprint of them stand
// for them, because a shell command routinely holds a credential and this record reaches every
// surface an operator reads (supervise.StepRecord). Every field is absent where the record holds
// nothing for it: a claim whose turn has begun and whose agent has called nothing carries
// `turnStartedAt` alone; a call still open carries `startedAt` and no `endedAt`, which is the
// shape a stalled turn leaves behind; `argsBytes` and `argsHash` are absent for a call the
// daemon saw no arguments of.
type StepView struct {
	At            *time.Time `json:"at,omitempty"`
	TurnStartedAt *time.Time `json:"turnStartedAt,omitempty"`
	Tool          string     `json:"tool,omitempty"`
	CallID        string     `json:"callId,omitempty"`
	ArgsBytes     int        `json:"argsBytes,omitempty"`
	ArgsHash      string     `json:"argsHash,omitempty"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	EndedAt       *time.Time `json:"endedAt,omitempty"`
}

// StepViewOf is step as a claim view shows it, and nil for the zero record — a claim whose agent
// has neither begun a turn nor made a call the daemon saw — so no such claim carries the key.
func StepViewOf(step supervise.StepRecord) *StepView {
	if step == (supervise.StepRecord{}) {
		return nil
	}
	return &StepView{
		At:            instant(step.At),
		TurnStartedAt: instant(step.TurnStartedAt),
		Tool:          step.Tool,
		CallID:        step.CallID,
		ArgsBytes:     step.ArgsBytes,
		ArgsHash:      step.ArgsHash,
		StartedAt:     instant(step.StartedAt),
		EndedAt:       instant(step.EndedAt),
	}
}

// CapabilityReportView is one session's capability report as the state shows it: when the session
// measured, the incarnation of the process that reported, the live rows it proved (OK) and the
// rows it did not, each with the fact its check found (Open). OK and Open are never null.
type CapabilityReportView struct {
	MeasuredAt  time.Time       `json:"measuredAt"`
	Incarnation string          `json:"incarnation"`
	OK          []string        `json:"ok"`
	Open        []CapabilityGap `json:"open"`
}

// CapabilityGap is one live row a session's check failed, with the fact the check found.
type CapabilityGap struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// CapabilityReportViewOf is report as the state shows it: its rows partitioned into the names that
// passed and the gaps that did not, each side in the report's (Table) order.
func CapabilityReportViewOf(report capabilities.Report) *CapabilityReportView {
	view := &CapabilityReportView{
		MeasuredAt:  report.MeasuredAt,
		Incarnation: report.Locator.Incarnation,
		OK:          []string{},
		Open:        []CapabilityGap{},
	}
	for _, row := range report.Rows {
		if row.OK {
			view.OK = append(view.OK, string(row.Name))
		} else {
			view.Open = append(view.Open, CapabilityGap{Name: string(row.Name), Detail: row.Detail})
		}
	}
	return view
}

// PhaseView is one phase worker's claim, the handoff of its current phase — the commit its
// completion reported, the Dispatch handoff record it named — and the three counts the phase
// carries: the passes it has made through its stretch of the workflow, which every backward move
// counts one of, the review rounds, which only a review that requested changes counts, and the
// counts of the moves back, by kind, that made those rounds.
type PhaseView struct {
	Claim         ClaimView          `json:"claim"`
	HandoffCommit string             `json:"handoffCommit,omitempty"`
	HandoffRecord *HandoffRecordView `json:"handoffRecord,omitempty"`
	Pass          int                `json:"pass"`
	Rounds        int                `json:"rounds"`
	// BackwardMoves is how many times the issue's work has gone back by each kind of move, keyed
	// "<from>-<to>" as phase.BackwardPairs() writes the pairs: "testing-implementing",
	// "reviewing-implementing", "merging-implementing" and the rest. It is absent, and never
	// `{}`, for an issue that has had no move back, so a reader tells an issue that carries no
	// counts at all from one whose counts the daemon holds. Rounds beside it keeps its meaning
	// exactly: it rises on every move back, a conflict round included, where no pair counts a
	// conflict round, since nobody sent the work back. The counts start empty at the migration
	// that added them and are never filled in from the past, so an issue already in flight reads
	// counts that add up to less than its `rounds`.
	BackwardMoves map[string]int `json:"backwardMoves,omitempty"`
}

// HandoffRecordView is the Dispatch handoff record a worker's completion of its current
// file-backed phase named: the record's id, and its version among its phase's records, which
// `dispatch handoff-read --issue <KEY> --phase <word> --version <n>` reads.
type HandoffRecordView struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// PullRequestView is the issue's pull request as the daemon observes it from GitHub.
// ReviewDecision is the latest review round's decision (changes_requested or approved), the one
// the workflow ends the round on when the reviewer completes. It shows from the review that
// decided it until the next round opens or a new generation clears it, so a request for changes
// stays through implementing and testing, and an approval through retro and every phase after
// it; absent until a review in a round decides.
// State is whether the pull request is open, merged, or closed unmerged, always one of the three:
// a merged root waits in merging for the rest of its tree, so an operator reads the merge
// here rather than guessing at it.
// MergeState is GitHub's own merge state for the head, in GitHub's lowercase spelling, as the
// daemon's last read of the pull request found it (record.PullRequest.MergeState); absent until
// that read has run. It is what says whether GitHub would merge the head now ("clean") or holds
// it on a check or a review the repository requires ("blocked").
// No CI verdict is shown: a CI result moves no issue, so the daemon keeps none
// (dispatch://INFRA-714/spec R1), and GitHub's own merge state is what says whether a check the
// repository requires holds the head.
type PullRequestView struct {
	// Repo is the repository (owner/name) the pull request is in, its issue's.
	Repo           string `json:"repo"`
	Number         int    `json:"number"`
	Head           string `json:"head"`
	State          string `json:"state"`
	MergeState     string `json:"mergeState,omitempty"`
	ReviewDecision string `json:"reviewDecision,omitempty"`
}

// GateView is the design gate: the spec document, the version Dispatch currently holds, and the
// version a human approved. ApprovedVersion is nil until someone approves one; the gate is open
// exactly when it equals CurrentVersion.
type GateView struct {
	ArtifactID      string `json:"artifactId"`
	CurrentVersion  int    `json:"currentVersion"`
	ApprovedVersion *int   `json:"approvedVersion"`
}

// GatedStepStatus is where one gated step of a deployment stands, as the issue's gate records say
// it: the four answers a record can give.
type GatedStepStatus string

const (
	// GatedStepUnregistered is a step the deployment gates that the issue holds no gate record
	// for: its architect has registered no document for that step, and the step holds the tree's
	// work until it does.
	GatedStepUnregistered GatedStepStatus = "unregistered"
	// GatedStepWaiting is a registered step whose current document version no person has reviewed:
	// the approval or the changes request it records, if any, is of a version below the current
	// one, which a newer version of the document left behind. It waits on a person.
	GatedStepWaiting GatedStepStatus = "waiting"
	// GatedStepApproved is a step a person approved at its current document version: the one
	// status that lets the tree's work past the step (classify.DesignGateOpen).
	GatedStepApproved GatedStepStatus = "approved"
	// GatedStepChangesRequested is a step whose reviewer asked for changes at its current document
	// version, which the architect was told of; ChangesReason carries the reason the reviewer gave.
	GatedStepChangesRequested GatedStepStatus = "changes_requested"
)

// GatedStepView is one step of the deployment's design gate as one issue stands in it: the step,
// the document its architect registered for the step, that document's current version, the
// version a person approved, where the step stands and, for a step whose reviewer asked for
// changes, the reason they gave. A step nobody has registered carries no document, no version and
// no approval.
type GatedStepView struct {
	Step            record.DesignStep `json:"step"`
	ArtifactID      string            `json:"artifactId,omitempty"`
	CurrentVersion  int               `json:"currentVersion,omitempty"`
	ApprovedVersion *int              `json:"approvedVersion"`
	Status          GatedStepStatus   `json:"status"`
	ChangesReason   string            `json:"changesReason,omitempty"`
}

// GatedStepViewsOf is steps, the design-gate steps a deployment gates, as one issue stands in
// them: gates are that issue's gate rows by step (record.Store.GatesOf), and the answer is one
// entry for each step, in the order the deployment gates them, never nil.
//
// The entries are the deployment's steps and not the issue's rows. A row of a step the deployment
// no longer gates stays in the record — a deployment that gates that step again reads its true
// state — and appears in no entry here, because it holds no work: every reader of the gate asks
// the configured steps (classify.DesignGatesOpen). A configured step with no row is the other
// direction, and is answered unregistered: it holds the tree's work, so a reader must see it.
func GatedStepViewsOf(steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate) []GatedStepView {
	views := make([]GatedStepView, 0, len(steps))
	for _, step := range steps {
		gate, registered := gates[step]
		if !registered {
			views = append(views, GatedStepView{Step: step, Status: GatedStepUnregistered})
			continue
		}
		view := GatedStepView{
			Step:            step,
			ArtifactID:      gate.ArtifactID,
			CurrentVersion:  gate.LatestVersion,
			ApprovedVersion: gate.ApprovedVersion,
			Status:          GatedStepWaiting,
		}
		switch {
		case classify.DesignGateOpen(gate):
			view.Status = GatedStepApproved
		case gate.ToldChangesVersion != nil && *gate.ToldChangesVersion == gate.LatestVersion:
			view.Status, view.ChangesReason = GatedStepChangesRequested, gate.ToldChangesReason
		}
		views = append(views, view)
	}
	return views
}

// SlotView is the admission slot the issue occupies and when it took it.
type SlotView struct {
	Index      int       `json:"index"`
	AdmittedAt time.Time `json:"admittedAt"`
}

// WaitingLineField is the name `waitingLine` carries on the wire, read from here by a test that
// looks for the field rather than repeating the string.
const WaitingLineField = "waitingLine"

// WaitingLineView is the issue's place in `admission.waiting`, where 1 is first in the line,
// and the length of that list at this read, beside the waiting line's own fields for a program
// that acts on the line rather than reading the sentence Reason renders: which part the issue
// waits in and why it waits there (record.LinePart and record.LineKind, whose String methods
// are the words carried here), its place within its own part and that part's length, whether it
// already holds one of the cap's slots, and what placed it — the moment its work came back for
// a member of the returned part, the Dispatch ranks and the tree root's key for a member of the
// starting part. It says where the issue stands, not what holds it: `admission.blocked` names
// what holds it.
//
// Every field beside Place and Length is omitempty, for two readers at once: a member carries
// the fields of its own part alone, so a returned member's body names no rank and a starting
// member's no `since`; and the strip that serves a caller below the contract these fields
// arrive in empties them from a view it still serves Place and Length on, which must leave no
// field of theirs on the wire.
type WaitingLineView struct {
	Place      int    `json:"place"`
	Length     int    `json:"length"`
	Part       string `json:"part,omitempty"`
	Kind       string `json:"kind,omitempty"`
	PartPlace  int    `json:"partPlace,omitempty"`
	PartLength int    `json:"partLength,omitempty"`
	// HoldsSlot says the member already holds one of the cap's slots (record.LineEntry.Holds),
	// so its promotion takes no second one. It is a pointer because `false` is one of the two
	// values this field carries and the strip above is written over the same omitempty: a plain
	// bool would be absent for every member that holds no slot, which is a fact the view must
	// state, and would be written as `false` on the emptied view that strip serves, where no
	// field of this contract is to appear at all.
	HoldsSlot *bool      `json:"holdsSlot,omitempty"`
	Since     *time.Time `json:"since,omitempty"`
	RootRank  string     `json:"rootRank,omitempty"`
	OwnRank   string     `json:"ownRank,omitempty"`
	RootKey   string     `json:"rootKey,omitempty"`
	// Reason is the rendered sentence of this member's wait, the one answer a person reads: what
	// put it in the line and when, where it stands in its own part, and what that place means
	// for the slot it waits for. The projection renders it from the fields above and the length
	// of the other part and from nothing else (projection's waitingLineReason), so the sentence
	// and the fields a program reads say the same thing about the same member.
	Reason string `json:"reason,omitempty"`
}

// WhyView is one rendered answer for an issue that is not working now: State, one of the four
// words below, and Reason, the sentence that says why. The projection reads the four states in
// one order over the facts the pass already holds and renders the sentence of whichever holds
// the issue (projection's whyStates), so an issue carries one answer and never two.
type WhyView struct {
	State  WhyState `json:"state"`
	Reason string   `json:"reason"`
}

// WhyState is the word WhyView.State carries, a closed set a reader branches on, so it is a type
// of its own as GatedStepStatus is: a word no daemon writes cannot be put in the field by
// mistake. A word added to it is a change of the wire that the daemon's own contract number
// covers (version.go), since a reader before it parses the four with a strict enum.
type WhyState string

// The four states WhyView.State carries: the issue's claim has stopped (WhyClaimStopped), it
// waits in the admission line (WhyWaiting), something holds it where it stands (WhyBlocked), or
// it stands in neither the line nor the work at all (WhyUnlisted).
const (
	WhyClaimStopped WhyState = "claim_stopped"
	WhyWaiting      WhyState = "waiting"
	WhyBlocked      WhyState = "blocked"
	WhyUnlisted     WhyState = "unlisted"
)

// CheckView is an issue's open production check as the state shows it: the repository, pull
// request and merge commit the check looks for in production (MergeCommit is empty for a pull
// request merged before the daemon recorded merge commits), when it opened, the role that works
// it (Role, the one the check recorded from the project's `production_check` when it opened), how
// many looks that role has taken and whether the latest is still open, what woke the latest look
// (LastWake, absent before the first look wakes), when the next look is due (NextWakeAt), and
// whether the last look has started.
//
// NextWakeAt is the next look's time, whichever names it: the moment the checking agent named for
// itself on a pending report, the retry of a look nobody reported, or — where neither stands —
// the check's own bound, the moment its last look starts. The bound stays readable without a
// field of its own: NextWakeAt is the bound whenever no agent's time is set, OpenedAt is here,
// and `production_check_hours` is in the state's settings.
type CheckView struct {
	Repo        string         `json:"repo"`
	PullRequest int            `json:"pullRequest"`
	MergeCommit string         `json:"mergeCommit"`
	OpenedAt    time.Time      `json:"openedAt"`
	Role        string         `json:"role"`
	Looks       int            `json:"looks"`
	LookOpen    bool           `json:"lookOpen"`
	LastWake    *CheckWakeView `json:"lastWake,omitempty"`
	NextWakeAt  time.Time      `json:"nextWakeAt"`
	Last        bool           `json:"last"`
}

// CheckWakeView is what woke a production check's latest look: Kind names the cause (`merge` for
// the look the close opens, `check wake` for one the checking agent's own wake_at or the retry of
// a look nobody reported starts, and `bound` for the last look; a check whose look was woken
// before the daemon stopped reading production can hold one of the causes that are gone), Detail
// the cause's own detail — for `check wake`, the summary the agent's report gave or the retry's
// own words — and At when.
type CheckWakeView struct {
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// PendingStatusWrite is a Dispatch-status effect the outbox has not finished, due now or backing off
// after a failed attempt: NextAt is the earliest it can run, and Attempts and LastError say why it
// is still pending. An issue's writes run in the order they were made, so a write waits behind an
// older unfinished one for the same issue, which is listed too; the list runs oldest first, so it
// shows each issue's writes in that order. The payload is intentionally opaque at this boundary:
// the workflow owns its exact effect shape.
type PendingStatusWrite struct {
	Issue     string          `json:"issue"`
	Payload   json.RawMessage `json:"payload"`
	Attempts  int             `json:"attempts"`
	NextAt    time.Time       `json:"nextAt"`
	LastError string          `json:"lastError,omitempty"`
}

// HeldOutboxRow is a due lifecycle effect its issue's lifecycle lane cannot run yet, because
// another lifecycle row of the issue is running: the lane runs an issue's lifecycle rows one at a
// time, in the order they were queued, so a start, a suspend or a workspace removal waits while an
// earlier one of its issue is under a live lease. WaitedSeconds counts from the moment the row
// became due (its next_at), so a retried row's earlier backoffs are not counted as waiting on its
// sibling; WaitsOn is the id of the running row — or, for a role's start fenced behind its issue's
// branch row, that branch row while it runs.
type HeldOutboxRow struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	Issue         string `json:"issue"`
	WaitedSeconds int64  `json:"waitedSeconds"`
	WaitsOn       int64  `json:"waitsOn"`
}

// StrandedClaim is one claim nothing will wake — a worker of its issue's phase, or its tree's
// architect, in a tree that runs — named by its issue, its role and its state; the issue's own
// entry under `issues` carries the rest. Its state, its role and who suspended it say the move
// that brings it back (Move), and that suspender is no field of it: every plugin parses this
// body with the schema of the contract it was built at, and a session whose plugin predates
// INFRA-1048 refuses the whole state over a key it does not know — a daemon that relaunches no
// stale session (spec R10) would break every such pod the moment a claim is stranded. A plugin
// built after it strips the key instead, which loses the field rather than the state, and the
// drift gate in packages/contracts names it at the next golden write. It is served on the
// operator claim routes, which no plugin calls (OperatorClaim.SuspendedBy), so a reader hands
// Move the suspender it holds: the daemon's own boot the claim's (SuspendersOf), `legion state`
// the operator claim list's.
type StrandedClaim struct {
	Issue string     `json:"issue"`
	Role  claim.Role `json:"role"`
	State string     `json:"state"`
}

// Suspenders is who suspended each claim, by the key its issue and role make: the fact a
// stranded claim's move turns on, which the state body does not carry.
type Suspenders map[string]string

// SuspendersOf is who suspended each suspended claim of stranded, as the daemon's own boot reads
// it from the claims themselves. A failed claim carries no suspension.
func SuspendersOf(stranded []supervise.Claim) Suspenders {
	by := make(Suspenders, len(stranded))
	for _, c := range stranded {
		if c.State == supervise.StateSuspended {
			by[strandedKey(c.Issue, c.Role)] = string(c.SuspendedBy)
		}
	}
	return by
}

// SuspendersOfOperatorClaims is who suspended each claim of the operator claim list, the one
// surface that serves it (OperatorClaim.SuspendedBy): how a reader off the daemon, `legion
// state`, names the move of a stranded claim.
func SuspendersOfOperatorClaims(claims []OperatorClaim) Suspenders {
	by := make(Suspenders, len(claims))
	for _, c := range claims {
		by[strandedKey(c.Issue, c.Role)] = c.SuspendedBy
	}
	return by
}

// Of is who suspended c, "" when nobody did or when this lookup does not name it.
func (s Suspenders) Of(c StrandedClaim) string {
	return s[strandedKey(c.Issue, c.Role)]
}

// strandedKey names one claim of a tree in both lookups: an issue runs one claim of a role.
func strandedKey(issue string, role claim.Role) string {
	return issue + " " + string(role)
}

// StrandedMove is the move that brings a stranded claim back (StrandedClaim.Move). Its value is the
// key the boot's `claims nothing will wake` log line lists the claim under.
type StrandedMove string

const (
	StrandedResume           StrandedMove = "resume"
	StrandedSuspendedWorkers StrandedMove = "suspendedWorkers"
	StrandedReleasedNoResume StrandedMove = "releasedNoResume"
	StrandedRetry            StrandedMove = "retry"
	StrandedWorkflowStart    StrandedMove = "needsWorkflowStart"
)

// StrandedMoves is every move, in the order the boot's log line and `legion state` list them.
func StrandedMoves() []StrandedMove {
	return []StrandedMove{StrandedResume, StrandedSuspendedWorkers, StrandedReleasedNoResume, StrandedRetry, StrandedWorkflowStart}
}

// Move is the move that brings c back, given who suspended it (Suspenders.Of, "" for a claim
// nobody suspended or whose suspender the caller cannot read): the one statement of it that the
// boot's log line and `legion state` both group by:
//
//   - StrandedReleasedNoResume, a claim whose idle pod the daemon released and whose wake never
//     brought it back (workflow.Stranded's ten-minute rule): the pod and its disk are gone, so
//     the move is the operator's reading of why the replacement pod never took a turn, not a
//     resume of the claim — a released claim resumes on its next delivery, and the wake that
//     should have resumed it has already been taken.
//   - StrandedResume, a suspended architect: the operator's resume (`legion claims resume`), which
//     a catch-up follows.
//   - StrandedSuspendedWorkers, a suspended phase worker: the move its claim's suspendedBy calls
//     for (the operator claim view): the boot's start once it is marked the daemon's when it was
//     suspended before 0038, the operator's resume when an operator suspended it and it kept its
//     task or its phase start waits, otherwise the operator's delivery of its phase's resume task.
//   - StrandedRetry, a failed architect: the operator's retry (`legion claims retry`), which
//     relaunches its session with fresh budgets, and a catch-up follows.
//   - StrandedWorkflowStart, a failed phase worker: the retry of its held phase — its architect's
//     retry_or_escalate, or the operator's retry (`legion claims retry`, which applies the same
//     retry) — since a failed claim ignores resume and deliver.
//   - StrandedWorkflowStart, a retired claim, whatever its role: only a workflow start relaunches
//     one, since the operator's retry refuses a claim that has not failed and a retired claim
//     ignores resume and deliver. A start of its phase sends it the retry the daemon's own start
//     executor makes of a retired claim, so the move is the act that queues such a start: the
//     architect's retry_or_escalate of a held phase, a transition into the phase, or the tree's
//     close and re-admission for an architect, whose tree's admission is the only start of it.
//
// The retired case is read before every other, because a claim released and then retired keeps
// `suspendedBy: released` on its row and SuspendersOfOperatorClaims copies that value whatever the
// state: read as released, it would name a pod coming back from a wake, and nothing is coming
// back. For the same reason the released case asks for the suspended state itself rather than for
// anything that has not failed.
func (c StrandedClaim) Move(suspendedBy string) StrandedMove {
	state := supervise.ClaimState(c.State)
	switch {
	case state == supervise.StateRetired:
		return StrandedWorkflowStart
	case state == supervise.StateSuspended && suspendedBy == string(supervise.SuspendedByReleased):
		return StrandedReleasedNoResume
	case state == supervise.StateFailed && c.Role == claim.RoleArchitect:
		return StrandedRetry
	case state == supervise.StateFailed:
		return StrandedWorkflowStart
	case c.Role == claim.RoleArchitect:
		return StrandedResume
	default:
		return StrandedSuspendedWorkers
	}
}

// StrandedClaimsOf is stranded as the state shows it, claim for claim.
func StrandedClaimsOf(stranded []supervise.Claim) []StrandedClaim {
	claims := make([]StrandedClaim, len(stranded))
	for i, c := range stranded {
		claims[i] = StrandedClaim{Issue: c.Issue, Role: c.Role, State: string(c.State)}
	}
	return claims
}

// StrandedByMove is each stranded claim named `<issue> <role>`, grouped by its Move under
// suspended, each group in stranded's order; every move has a group, empty when no claim takes
// it.
func StrandedByMove(stranded []StrandedClaim, suspended Suspenders) map[StrandedMove][]string {
	grouped := make(map[StrandedMove][]string, len(StrandedMoves()))
	for _, move := range StrandedMoves() {
		grouped[move] = []string{}
	}
	for _, c := range stranded {
		move := c.Move(suspended.Of(c))
		grouped[move] = append(grouped[move], strandedKey(c.Issue, c.Role))
	}
	return grouped
}

// WakeWaiting is one wake the daemon's pacing holds until its turn, as the state names it
// (State.WakesWaiting): the claim it will wake, that claim's issue and role, what kind of wake it
// is, and the moment the pacing gave it, which is when it runs. The kinds are the three lanes the
// daemon paces, declared once where the machines reserve them (supervise.WakeKind) and each named
// on the wire as the line that wake writes names it (supervise's wakeRan, the outbox's):
//
//   - `launch`, a role process the daemon is about to start, held by the claim's machine;
//   - `plugin-refresh`, the relaunch of a process whose plugin speaks an earlier contract than
//     this daemon's, held by the claim's machine too;
//   - `catch-up`, the notice a start writes to an architect already running in place of a launch,
//     which waits on its own outbox row rather than in a machine.
//
// The project controller's own claim is exempt from the pacing and never waits, so it never
// appears. A wake over a claim whose process still runs appears like any other: it is a wake the
// queue holds, whatever the claim does meanwhile.
type WakeWaiting struct {
	Claim claim.Token        `json:"claim"`
	Issue string             `json:"issue"`
	Role  claim.Role         `json:"role"`
	Kind  supervise.WakeKind `json:"kind"`
	DueAt time.Time          `json:"dueAt"`
}

// WakesWaitingOf is every wake still waiting, from both of the places one can wait, in claim order:
// held are the wakes the daemon's machines hold in memory, each as its machine last published it
// (supervise.Machine.WaitingWake) — the launches and the refresh relaunches — and queued are the wakes
// that wait on a durable row, which the caller composes from the store — today the paced catch-ups
// (daemon's pacedCatchUps). The two are merged here rather than at either source so that one read
// of the state is one ordered list, whichever lane is holding a wave back: an operator or a
// controller asking whether a wave is still running must not have to know which lane each wake of
// it is in.
//
// The machines are read as the operator's claim listing and the capability report read theirs: a
// reader of every claim never waits on the decision in flight, and reading the wake from the
// published view rather than from a claim's stored state leaves what every other reader of that
// state makes of it untouched. Outside a wave nothing waits, so nothing is allocated while both
// sides are empty; the state's own marshalling is what keeps the empty list an array on the wire
// (State.MarshalJSON). The sort is stable, so two wakes of one claim — a row and a machine's, or
// two rows — keep the order their sources gave them.
func WakesWaitingOf(held []supervise.WaitingWake, queued []WakeWaiting) []WakeWaiting {
	var wakes []WakeWaiting
	for _, w := range held {
		wakes = append(wakes, WakeWaiting{Claim: w.Claim, Issue: w.Issue, Role: w.Role, Kind: w.Kind, DueAt: w.At})
	}
	wakes = append(wakes, queued...)
	slices.SortStableFunc(wakes, func(a, b WakeWaiting) int { return cmp.Compare(a.Claim, b.Claim) })
	return wakes
}

// StateSource maps the daemon's durable and live facts through the transaction the state route
// owns. It does not commit it: the route can refuse one failed projection without publishing a
// partial snapshot. It answers the project's claims beside the state, the ones it read through
// that transaction to build the claim views, so the route decides what the caller's plugin reads
// (server's stateFor) from the claims of the snapshot it serves, rather than reading the table a
// second time at another moment.
type StateSource interface {
	State(ctx context.Context, tx pgx.Tx) (State, []supervise.Claim, error)
}

// StateTransactions begins the one snapshot transaction every state response reads through.
type StateTransactions interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}
