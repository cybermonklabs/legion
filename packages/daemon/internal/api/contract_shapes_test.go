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
	"github.com/sjawhar/legion/daemon/internal/phase"
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
// withoutRetiredStranded, which drops the entries of `strandedClaims` whose `state` is a word
// contract 56 added; the table does declare that field, so a record below 56 admits two values
// where this daemon's own admits three, and a body carrying the third below 56 is red here.
// state_caller_test.go holds the dropping itself, entry by entry.
//
// A key under a block the plugin parses loosely is recorded like any other, `settings.hooks.*`
// and its three keys among them, and that is right: loose is not blind. A loose block passes a
// key it does not know and still refuses a key it requires and does not find — the defect this
// change cures included `settings.max_fix_attempts: Invalid input: expected number, received
// undefined`, read under `settings` — so recording those paths is what catches the next removal
// under such a block. It costs an author a declaration for a key added under one, and that is
// the safe error of the two: a mark saying a subtree is loose goes stale the day a plugin
// tightens it, and nothing here would notice.
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
// whose body has no key (the empty object the eight fact routes answer). The first is
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
// prefix, a role's stall backstop, a project's memory block, and the `runtime.tmux` block
// itself, which no file of the kubernetes runtime carries. Together the two files write every
// key the settings block has.
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

// stateCarryingTheRestOfTheShape is the second state body, and it carries what populatedState
// cannot: an issue waiting on a person for a design gate, which is the `waitingOn` of a kind
// the first body has none of; an issue whose wait a person declined to answer, which is the
// value contract 68 added to `answeredBy` and the one this change's serving-down of a value is
// for; a status write the daemon owes Dispatch; a repository cache whose health read failed;
// and the settings of a tmux deployment (shapeSettingsFile). Every key of these is a key the
// state route writes, so the records describe each of them and a serving-down that forgets one
// is caught where a thinner sample would pass.
func stateCarryingTheRestOfTheShape(t *testing.T) State {
	t.Helper()
	since := time.Date(2026, 9, 22, 7, 30, 0, 0, time.UTC)
	declinedAt := time.Date(2026, 9, 22, 8, 45, 0, 0, time.UTC)
	return State{
		Daemon: DaemonInfo{
			Project:       "shapes",
			SchemaVersion: 1,
			Boots:         2,
			FirstBootAt:   time.Date(2026, 9, 18, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 22, 9, 15, 2, 0, time.UTC),
		},
		// The mark the shaping writes on every state it serves (shapeState, ContractView). The
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
		// cache view the first body carries no example of.
		Repositories: RepositoriesView{
			Owners: []string{"acme"},
			Mapped: []MappedRepositoryView{{Repo: "acme/widgets", Default: true, Cache: &CacheView{
				Repo: "acme/widgets", Sandbox: "legion-shapes-cache-acme-widgets-5f2a1b3c", State: "ready", Ready: true,
				Address:     "http://legion-shapes-cache-acme-widgets-5f2a1b3c.legion.svc:8418",
				HealthError: "read http://legion-shapes-cache-acme-widgets-5f2a1b3c.legion.svc:8418/healthz: context deadline exceeded",
			}}},
		},
		Settings: SettingsOf(loadSettingsFile(t, shapeSettingsFile)),
	}
}

// stateFixtures are the state bodies the records of the state and of the issue record are taken
// from: the populated state every other golden of this package is written from, and a second
// one of this case's own carrying what the first cannot (stateCarryingTheRestOfTheShape).
func stateFixtures(t *testing.T) []State {
	t.Helper()
	return []State{populatedState(t), stateCarryingTheRestOfTheShape(t)}
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
	released := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	return map[AnswerID]answerFixture{
		// The whole-project body, the one answer whose shape has moved since the floor.
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
		// through releaseRecordsBody from the store's rows as the route builds it.
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
			},
			Holds: []store.PodHold{
				{ID: 301, Issue: "LEGION-210", Reason: "background_work", Detail: "subagent,async-job",
					FirstAt: released.Add(time.Hour), At: released.Add(70 * time.Minute), Passes: 11},
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
		// The empty object the eight fact routes answer. Six Go types write it, and none of them
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
		// answer always writes, neither of them omitempty.
		AnswerTurnOutcome: {roots: typesOf(claim.TurnOutcomeResponse{}), bodies: []any{claim.TurnOutcomeResponse{
			RefusedRun: 3, Failed: true,
		}}},
		// Richer than the golden, which releases one child that started nothing: the record
		// describes the two lists a release fills and the keys of a held child.
		AnswerWaveRelease: {roots: typesOf(WaveReleaseResponse{}), bodies: []any{WaveReleaseResponse{
			Released: []string{"LEGION-209", "LEGION-211"},
			Started:  []string{"LEGION-209"},
			Held:     []HeldChild{{Issue: "LEGION-211", BlockedBy: []string{"LEGION-210"}}},
		}}},
		// The refusal has two Go shapes: claim.Refusal, one sentence under `error`, which the
		// claim routes answer, and api.Failure, which adds the stable `code` the credential and
		// workflow routes carry. A client parses either on every answer that is not a success,
		// so the fixture holds both and the record is the keys of the two read as one set.
		AnswerRefusal: {
			roots: typesOf(claim.Refusal{}, Failure{}),
			bodies: []any{
				claim.SameAgentRefusal,
				Failure{Code: "GRANT_NOT_FOUND", Error: "no grant for this command"},
			},
		},
	}
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

// contracts is the pair of contracts the caller is served at, read from contractsFor itself, the
// one function every route puts the question to.
func (k callerKind) contracts(t *testing.T, contract int) (restore, strip int) {
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
	at := s.contractsFor(request, k.claims(contract))
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
// key paths of the bodies it reads, and the values those bodies carry at each key path the
// table declares enumerated (answers.go's EnumeratedFields).
//
// The state and the record come from the routes that answer them: this package's own test server
// answers both with no credential of any kind, so each body goes through the whole of
// writeAnswerFrom — contractsFor over the caller's own request, shapeAnswer, and the one writer.
// Every other answer goes through shapeAnswer with the pair contractsFor read from that same
// caller. Their own routes each want a store, a registration, a boot token or a grant, none of
// which decides the shape of an answer, and shapeAnswer is the whole of what stands between a
// route's body and the wire. The refusal has no route of its own at all: a client parses it on
// every answer that is not a success.
func servedShapes(t *testing.T, caller callerKind) map[shapeKey]answerShape {
	t.Helper()
	fixtures := answerFixtures(t)
	states := stateFixtures(t)
	// What an answer declares is its Go types' and no contract's, so each is walked once here
	// rather than once for every contract below (answerFixture.shape).
	shapes := make(map[AnswerID]declaredShape, len(fixtures))
	for id, fixture := range fixtures {
		shapes[id] = fixture.shape(t)
	}
	// The claims the answers are read against are this daemon's own at every contract for the
	// caller the header places, so that caller's handlers are built once; the other caller's
	// claims are its pod's, which record the contract itself (callerKind.claims).
	var stated []http.Handler
	if caller.states {
		stated = stateHandlers(t, states, caller.claims(DaemonAPIVersion))
	}
	served := map[shapeKey]answerShape{}
	for _, contract := range SupportedContracts {
		handlers := stated
		if handlers == nil {
			handlers = stateHandlers(t, states, caller.claims(contract))
		}
		restore, strip := caller.contracts(t, contract)
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
			paths := map[string]bool{}
			carried := map[string]map[string]bool{}
			enumerated := enumeratedPathsOf(EnumeratedFields, answer.ID)
			for _, body := range servedBodies(t, caller, handlers, states, answer.ID, fixture, contract, restore, strip) {
				bodyPaths, bodyValues := keyPathsOfBody(t, body, shapes[answer.ID], enumerated)
				maps.Copy(paths, bodyPaths)
				for path, values := range bodyValues {
					if carried[path] == nil {
						carried[path] = map[string]bool{}
					}
					maps.Copy(carried[path], values)
				}
			}
			values := make(map[string][]string, len(carried))
			for path, carriedHere := range carried {
				values[path] = slices.Sorted(maps.Keys(carriedHere))
			}
			served[key] = answerShape{paths: slices.Sorted(maps.Keys(paths)), values: values}
		}
	}
	return served
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
// handlers answer the state bodies of states, one handler each.
func servedBodies(t *testing.T, caller callerKind, handlers []http.Handler, states []State, id AnswerID,
	fixture answerFixture, contract, restore, strip int,
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
	byKey := map[shapeKey]contractShape{}
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
// not a reading of today's code. Such a record still changes with the serving-down that produces
// it, by a hand edit of the fixture, and that diff is what a reviewer reads. A pair the fixture
// has no record of at all is no pair `-update` adds either: it fails
// TestTheRecordSetIsOneRecordPerAnswerPerContract by name, so an answer nobody recorded is seen.
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

// Every answer at every contract, served twice: to a caller stating that contract in
// `Legion-Plugin-Contract`, and to a caller stating nothing from a pod whose claims record it.
// Both are held to the same records, because a body is owed to the reader and not to the way it
// said who it is, and the second is the population this change is about.
func TestEveryAnswerAtEveryContractIsTheShapeItsRecordDescribes(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	records := loadContractShapes(t)
	for _, caller := range callerKinds {
		t.Run(caller.name, func(t *testing.T) {
			served := servedShapes(t, caller)
			for _, contract := range SupportedContracts {
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

// The two callers are placed by different readings and served the same pair of contracts, which
// is why one record stands for both.
func TestTheTwoCallersAreServedTheSameContracts(t *testing.T) {
	for _, contract := range SupportedContracts {
		statedRestore, statedStrip := callerKinds[0].contracts(t, contract)
		podRestore, podStrip := callerKinds[1].contracts(t, contract)
		if statedRestore != podRestore || statedStrip != podStrip {
			t.Errorf("contract %d is served (%d, %d) to a caller stating it and (%d, %d) to a caller of a pod recording it",
				contract, statedRestore, statedStrip, podRestore, podStrip)
		}
		if statedRestore != contract || statedStrip != contract {
			t.Errorf("contract %d is served (%d, %d), want the contract itself on both sides: every contract of SupportedContracts is a shape this daemon describes",
				contract, statedRestore, statedStrip)
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
	for _, path := range served.paths {
		if !described[path] {
			problems = append(problems, fmt.Sprintf(
				"contract %d, the %s answer: the body carries `%s` and the contract does not describe it. Serve the key down to this contract (shaping.go's shapeState) or, where the contract's own reader takes it, record it here beside the version.go entry that adds it.",
				key.contract, key.answer, path))
		}
	}
	for _, path := range record.KeyPaths {
		if !carried[path] {
			problems = append(problems, fmt.Sprintf(
				"contract %d, the %s answer: the body lacks `%s` and the contract describes it. A reader at %d requires the key, so write a stand-in back for it (shaping.go's withFixAttempts is one) or, where this contract never read it, correct the record and say why in the version.go entry.",
				key.contract, key.answer, path, key.contract))
		}
	}
	for _, path := range slices.Sorted(maps.Keys(served.values)) {
		for _, value := range served.values[path] {
			if slices.Contains(record.Domains[path], value) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"contract %d, the %s answer: the body carries `%s` at `%s` and the contract admits no such value. A reader at %d parses the field with a strict enumeration and refuses the whole body for a value it does not name, as it refuses a key it does not know, so serve the value down to this contract (shaping.go's shapeIssue) or, where the contract's own reader admits it, record its arrival in answers.go's EnumeratedFields beside the version.go entry that adds it.",
				key.contract, key.answer, value, path, key.contract))
		}
	}
	return problems
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
	// arrived at 118 and carries all nine values of `answeredBy` from its first record — since a
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

// What the gate catches, shown by six changes a shape change would make. Each is applied to a
// copy inside this case, so none of them edits a file and none needs a commit.
func TestTheGateCatchesAShapeChangeWithNoServingDown(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	onFile, written := readContractShapes(t)
	if !written {
		t.Fatal(bootstrapMessage())
	}
	served := servedShapes(t, callerKinds[0])
	// The scratch changes below are what the gate reads after an author ran `-update`, which
	// rewrites this daemon's own contract's records and leaves every earlier one alone. So the
	// first two and the last fail at every contract below this daemon's own and nowhere else,
	// which is the whole point of the serving-down: the reader that is hurt is the older one.
	// fields is the enumerated table `-update` would have read, which is answers.go's own for
	// every change but the value one, whose author widened a field.
	afterUpdate := func(changed map[shapeKey]answerShape, fields []EnumeratedField) map[shapeKey]contractShape {
		records := map[shapeKey]contractShape{}
		for _, record := range updatedRecords(onFile, shapeRecords(changed, fields)) {
			records[shapeKey{contract: record.Contract, answer: record.Answer}] = record
		}
		return records
	}

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

		records := map[shapeKey]contractShape{}
		for _, record := range rewrittenFile {
			records[shapeKey{contract: record.Contract, answer: record.Answer}] = record
		}
		problems := allShapeProblems(records, served)
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

	t.Run("a value is added to a declared enumerated field with no serving-down", func(t *testing.T) {
		const field = "issues.*.waitingOn.answeredBy"
		const added = "recalled"
		// The author gave `answeredBy` a tenth value at this daemon's own contract, wrote its
		// row in answers.go and served it to every caller. `-update` then records the widened
		// domain at this contract, and every older contract's record holds the domain it
		// always held, which is the reader this change exists for.
		widened := withDeclaredValue(t, EnumeratedFields, AnswerState, field, added, DaemonAPIVersion)
		changed := withServedValue(t, served, AnswerState, field, added)
		problems := allShapeProblems(afterUpdate(changed, widened), changed)
		wantProblemsNaming(t, problems, field, SupportedContracts, DaemonAPIVersion)
		for _, problem := range problems {
			if !strings.Contains(problem, "`"+added+"`") {
				t.Errorf("the gate reported a problem naming the field and not the value `%s`: %s", added, problem)
			}
		}
	})

	t.Run("a fourth stranded state is added with no serving-down", func(t *testing.T) {
		const field = "strandedClaims.*.state"
		const added = "parked"
		// The second field whose values a serving-down moves, and the one that carries no
		// serving-down of its own: withoutRetiredStranded drops the whole entry of a retired
		// claim below 56, which is why the three states this daemon writes pass the gate
		// today. A fourth state served to every caller with nothing dropping its entry is
		// caught at every contract below this daemon's own, as a tenth `answeredBy` is.
		widened := withDeclaredValue(t, EnumeratedFields, AnswerState, field, added, DaemonAPIVersion)
		changed := withServedValue(t, served, AnswerState, field, added)
		problems := allShapeProblems(afterUpdate(changed, widened), changed)
		wantProblemsNaming(t, problems, field, SupportedContracts, DaemonAPIVersion)
		for _, problem := range problems {
			if !strings.Contains(problem, "`"+added+"`") {
				t.Errorf("the gate reported a problem naming the field and not the value `%s`: %s", added, problem)
			}
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
// The three entries are the fields of one entry of `settings.projects.<KEY>.deploys`, the key
// contract 54 took the daemon's reading of away. The loader accepts the key and reads nothing
// from it, and SettingsOf writes `[]` for every project whatever the file holds
// (settings.go's DeploySettings, api.ProjectSettings.Deploys), so no body the daemon can write
// carries an entry and no fixture can realise one. The list itself is realised, and it is
// recorded: a reader of the settings reads the key. The three go with the key, when the loader
// stops accepting it.
var excludedDeclaredFields = map[answerField]string{
	{answer: AnswerState, field: declaredField{typ: "api.DeploySettings", name: "repo"}}:     deploysNeverFilled,
	{answer: AnswerState, field: declaredField{typ: "api.DeploySettings", name: "workflow"}}: deploysNeverFilled,
	{answer: AnswerState, field: declaredField{typ: "api.DeploySettings", name: "step"}}:     deploysNeverFilled,
}

// deploysNeverFilled is the reason the three entries above share.
const deploysNeverFilled = "the daemon holds no deploy since contract 54, so `settings.projects.<KEY>.deploys` is `[]` on every body it writes (SettingsOf) and no entry of it can be realised"

// Every json field the answers' wire types declare is realised by a fixture body, so a record
// taken from these bodies describes the whole of each answer rather than the part a thin sample
// happened to carry. `issues.*.check.role` is the field this case exists for: contract 54 added
// it, an issue carries a check only while its production check is open, and a sample with no
// open check would record a state shape with no `role` in it at all.
//
// A field is one json name of one Go type, so the two key paths of ClaimView.Capabilities —
// `architect.capabilities` and `workers.*.claim.capabilities` — are one field, and the fixture
// realises it wherever it carries one. The walk reads declared json tags (walkDeclaredType), so
// a hand-written marshaller's shadow struct declares the same set as the type it shadows, and
// the unexported flags the shaping sets are no json fields of any type and are walked by
// nothing (walkDeclaredType names them). What they do to a body is held all the same, by the
// records: the stand-ins PullRequestView.fixAttemptsShown and Settings.maxFixAttemptsShown put
// `fixAttempts` and `max_fix_attempts` into the records below 59, Settings.podUpdateHoursShown
// puts `pod_update_retask_hours` and `pod_update_notice_hours` into the records below 118 and
// Settings.podUpdateGraceHidden takes `pod_update_grace_minutes` out of them,
// CheckView.roleHidden takes `check.role` out of the records below 54, and PhaseView.passHidden
// takes each worker's `pass` out of the records below 86.
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
		for _, body := range fixture.bodies {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal a body of the %s answer: %v", answer.ID, err)
			}
			bodyPaths, _ := keyPathsOfBody(t, encoded, shape, nil)
			for path := range bodyPaths {
				if field, isDeclared := shape.paths[path]; isDeclared {
					realised[answerField{answer: answer.ID, field: field}] = true
				}
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
		t.Errorf("the %s answer declares %s and no fixture body realises it (`%s`): give the fixture a value for it, so every record describes the key, rather than leaving the field out of the walk",
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
