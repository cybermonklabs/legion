package daemon

//ci:reads packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json TestTeardownEffectsWorldParts reads T0's example world through ../../../pi-legion

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// teardownEffectsDaemonParts are the typed daemon parts of a case's world (heldout-case.ts
// DaemonWorld, `claim` on), each optional, which lane D's seed writes into the real daemon's store
// (teardownEffectsRun.seed) and reads back through the daemon's own routes
// (teardownEffectsDaemonFaults):
//   - Claim: the role's claim, on Issue in Tree, and Session, the session the world's records name
//     as the role's own, whose records the replay writes as its own session;
//   - Issue: the daemon's record of the case's issue where it differs from the default;
//   - Entry: the entry the role's phases row names (Phase), the entry the claim serves (Turn), the
//     task queued at the row's entry (OwedTask) and whether a move back raised it (MovedBack);
//   - Architect: the state of the case's issue's architect claim, nil for none;
//   - Records: the daemon's record of each other issue, with its architect claim's state;
//   - Admission: the cap and the active, waiting and blocked issues of `read_state`;
//   - Gate: the design gate's steps of the claim's tree.
type teardownEffectsDaemonParts struct {
	Claim *struct{ Issue, Tree, Session string }
	Issue *struct {
		Tree        string
		Parent      *string
		Generation  uint64
		LingerUntil *time.Time
	}
	Entry *struct {
		Phase, Turn int64
		OwedTask    *string
		MovedBack   bool
	}
	Architect *teardownEffectsArchitect
	Records   []teardownEffectsDaemonRecord
	Admission *struct {
		Cap                      int
		Active, Waiting, Blocked []string
	}
	Gate []teardownEffectsGateRow
}

// teardownEffectsArchitect is an architect claim as `read_record` names its state.
type teardownEffectsArchitect struct{ State string }

// teardownEffectsDaemonRecord is the daemon's record of an issue other than the case's.
type teardownEffectsDaemonRecord struct {
	Key, Tree  string
	Parent     *string
	Phase      phase.Phase
	Status     string
	Generation uint64
	Architect  *teardownEffectsArchitect
}

// teardownEffectsGateRow is one step of the claim tree's design gate: the file name of its
// document, its newest version, the version a person approved (nil for none) and the id the case's
// records or traces name the document by, where they name one.
type teardownEffectsGateRow struct {
	Step, Document string
	Current        int
	Approved       *int
	ArtifactID     string
}

// teardownEffectsDispatchIssue is one Dispatch issue of the world other than the case's.
type teardownEffectsDispatchIssue struct {
	Key, Title, Status string
	Parent             *string
	Rank               *int
	Labels, BlockedBy  []string
	ClaimedBy          *string
}

// teardownEffectsStepRun is one step run of the case's issue as `dispatch steps` lists it.
type teardownEffectsStepRun struct {
	Task         string
	Step         int
	Kind         string
	Role, Result *string
	StartedAt    time.Time
	EndedAt      *time.Time
	PlanRating   *int
	PlanReason   *string
}

// teardownEffectsServed is each member of the containers of a world's typed parts that lane D
// seeds, by container: a member outside it is a part lane D would drop, which teardownEffectsUnserved
// names. world.dispatch.runsHoldRecords is taken and left unread: it says where lane R's seed writes
// the step runs' times, and lane D writes each run at the world's own instants, since its replay
// reads no step run's time.
var teardownEffectsServed = map[string][]string{
	"world.daemon":              {"phase", "step", "task", "productionCheck", "claim", "issue", "entry", "architect", "records", "admission", "gate"},
	"world.dispatch":            {"issue", "documents", "handoffs", "comments", "asks", "results", "issues", "steps", "runsHoldRecords"},
	"world.dispatch.issue":      {"key", "title", "status", "spec", "parent", "labels"},
	"world.github":              {"repository", "pullRequests", "mergeQueue", "settings", "branchRules"},
	"world.github.pullRequests": {"number", "state", "draft", "head", "base", "title", "body", "mergeCommit", "reviews", "reviewThreads", "checks", "mergeStateStatus", "autoMergeRequest", "nodeId", "diff", "mergeResult"},
}

// UnmarshalJSON decodes a world and names in unserved each member of a typed part's container that
// lane D has no seeding for (teardownEffectsUnserved).
func (w *teardownEffectsWorld) UnmarshalJSON(data []byte) error {
	type plain teardownEffectsWorld
	if err := json.Unmarshal(data, (*plain)(w)); err != nil {
		return err
	}
	unserved, err := teardownEffectsUnserved(data)
	w.unserved = unserved
	return err
}

// teardownEffectsUnserved names, by its path, each member of world (the case's world as JSON) in
// one of the containers teardownEffectsServed lists that lane D does not seed: a typed part it would
// drop in silence.
func teardownEffectsUnserved(world []byte) ([]string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(world, &top); err != nil {
		return nil, err
	}
	var unserved []string
	check := func(where string, raw json.RawMessage) error {
		if len(raw) == 0 || string(raw) == "null" {
			return nil
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		served := where
		if open := strings.Index(where, "["); open >= 0 {
			served = where[:open]
		}
		for name := range members {
			if !slices.Contains(teardownEffectsServed[served], name) {
				unserved = append(unserved, where+"."+name)
			}
		}
		return nil
	}
	members := func(raw json.RawMessage) map[string]json.RawMessage {
		var held map[string]json.RawMessage
		_ = json.Unmarshal(raw, &held)
		return held
	}
	if err := check("world.daemon", top["daemon"]); err != nil {
		return nil, err
	}
	if err := check("world.dispatch", top["dispatch"]); err != nil {
		return nil, err
	}
	if err := check("world.dispatch.issue", members(top["dispatch"])["issue"]); err != nil {
		return nil, err
	}
	github := top["github"]
	if err := check("world.github", github); err != nil {
		return nil, err
	}
	var pulls []json.RawMessage
	_ = json.Unmarshal(members(github)["pullRequests"], &pulls)
	for i, pull := range pulls {
		if err := check(fmt.Sprintf("world.github.pullRequests[%d]", i), pull); err != nil {
			return nil, err
		}
	}
	slices.Sort(unserved)
	return unserved, nil
}

// teardownEffectsClaimOf is the role's claim the world states (daemon.claim), else the case's issue
// in the tree of its Dispatch parent or its own, as lane R's daemon stand-in holds it, and the
// session the world's records name as the role's own ("" for none).
func teardownEffectsClaimOf(world teardownEffectsWorld) (issue, tree, session string) {
	issue, tree = world.Dispatch.Issue.Key, world.Dispatch.Issue.Parent
	if tree == "" {
		tree = issue
	}
	if world.Daemon != nil && world.Daemon.Claim != nil {
		c := world.Daemon.Claim
		return c.Issue, c.Tree, c.Session
	}
	return issue, tree, ""
}

// teardownEffectsOwnWrite says whether author, the writer a record of the world names, is the role's
// own session (daemon.claim.session), whose record the replay writes as the replay's own session.
func teardownEffectsOwnWrite(world teardownEffectsWorld, author string) bool {
	_, _, session := teardownEffectsClaimOf(world)
	return session != "" && author == session
}

// teardownEffectsClaimStates is every state of a claim the daemon names (supervise's ClaimState).
var teardownEffectsClaimStates = []supervise.ClaimState{supervise.StateQueued, supervise.StateLaunchUncertain, supervise.StateLaunching,
	supervise.StateShimConnected, supervise.StateRegistered, supervise.StateReady, supervise.StateWorking, supervise.StateIdle,
	supervise.StateSuspended, supervise.StateFailed, supervise.StateRetired}

// teardownEffectsGateSteps is the design gate's steps in the order the daemon gates them.
var teardownEffectsGateSteps = []record.DesignStep{record.DesignStepDecisions, record.DesignStepCriteria, record.DesignStepSpec}

// teardownEffectsSeedFaults names, by its part, each fact of world's typed parts lane D cannot seed
// into the real daemon's store, the refusals lane R's daemon stand-in makes of the same facts among
// them (daemon-standin-world.ts worldFaults): an architect claim in a state no claim has, a gate step
// the daemon gates none of, a phase entry of a claim on another issue than the case's, an issue the
// admission names that the replay records nothing of (the case's own, a record of the world and the
// case's Dispatch parent are recorded), a record in no phase, and a step run with no role, which
// Dispatch records no run without.
func teardownEffectsSeedFaults(world teardownEffectsWorld) []string {
	var faults []string
	if world.Daemon != nil {
		parts := world.Daemon.teardownEffectsDaemonParts
		state := func(where string, architect *teardownEffectsArchitect) {
			if architect != nil && !slices.Contains(teardownEffectsClaimStates, supervise.ClaimState(architect.State)) {
				faults = append(faults, fmt.Sprintf("%s.state %s is no state of a claim the daemon names", where, architect.State))
			}
		}
		state("world.daemon.architect", parts.Architect)
		recorded := map[string]bool{world.Dispatch.Issue.Key: true}
		if parent := world.Dispatch.Issue.Parent; parent != "" {
			recorded[parent] = true
		}
		for i, each := range parts.Records {
			state(fmt.Sprintf("world.daemon.records[%d].architect", i), each.Architect)
			if !slices.Contains(phase.All, each.Phase) {
				faults = append(faults, fmt.Sprintf("world.daemon.records[%d].phase %s is no phase of the daemon's", i, each.Phase))
			}
			recorded[each.Key] = true
		}
		for i, row := range parts.Gate {
			if !slices.Contains(teardownEffectsGateSteps, record.DesignStep(row.Step)) {
				faults = append(faults, fmt.Sprintf("world.daemon.gate[%d].step %s is no step of the design gate the daemon names", i, row.Step))
			}
		}
		if claimed, _, _ := teardownEffectsClaimOf(world); parts.Entry != nil && claimed != world.Dispatch.Issue.Key {
			faults = append(faults, fmt.Sprintf("world.daemon.entry is the phase entry of the claim's issue %s, and the world states the phase of %s alone", claimed, world.Dispatch.Issue.Key))
		}
		if admission := parts.Admission; admission != nil {
			for name, keys := range map[string][]string{"active": admission.Active, "waiting": admission.Waiting, "blocked": admission.Blocked} {
				for i, key := range keys {
					if !recorded[key] {
						faults = append(faults, fmt.Sprintf("world.daemon.admission.%s[%d] %s is an issue the replay records nothing of, so no slot or line of the daemon's holds it", name, i, key))
					}
				}
			}
		}
	}
	for i, run := range world.Dispatch.Steps {
		if run.Role == nil || *run.Role == "" {
			faults = append(faults, fmt.Sprintf("world.dispatch.steps[%d].role is null, and Dispatch records no run without its role", i))
		}
	}
	slices.Sort(faults)
	return faults
}

// teardownEffectsDaemonFaults names, by its part, each fact of world's typed daemon parts that the
// real daemon's own reads do not answer as the world states it: read is `read_record` of a key (nil
// for a 404), admission `read_state`'s, and gateIDs the id the replay's Dispatch gives each gate
// step's document. The claim's own architect claim is the replay's own, whose state the pane
// moves, so only that it is there is read. A phase entry is read on the claim's record
// (phaseEntry), and the claim's serving entry, which no read shows, is not.
func teardownEffectsDaemonFaults(world teardownEffectsWorld, role claim.Role, read func(key string) *api.Issue, admission api.Admission, gateIDs map[string]string) []string {
	if world.Daemon == nil {
		return nil
	}
	parts := world.Daemon.teardownEffectsDaemonParts
	claimed, tree, _ := teardownEffectsClaimOf(world)
	var faults []string
	fault := func(format string, args ...any) { faults = append(faults, fmt.Sprintf(format, args...)) }
	architect := func(where, key string, stated *teardownEffectsArchitect, got *api.Issue) {
		own := role == claim.RoleArchitect && key == claimed
		switch {
		case own && got.Architect == nil:
			fault("%s: the daemon's record of %s names no architect claim, and it is the replay's own", where, key)
		case own:
		case stated == nil && got.Architect != nil:
			fault("%s: the daemon's record of %s names an architect claim %s, and the world states none", where, key, got.Architect.State)
		case stated != nil && (got.Architect == nil || got.Architect.State != stated.State):
			held := "none"
			if got.Architect != nil {
				held = got.Architect.State
			}
			fault("%s.state: the daemon answers %s for %s, and the world states %s", where, held, key, stated.State)
		}
	}
	key := world.Dispatch.Issue.Key
	// The case's own record is read for the parts that name it: a controller case's child issue,
	// which the replay does not seed, has none to read otherwise.
	own := read(key)
	switch {
	case parts.Issue == nil && parts.Architect == nil && parts.Entry == nil:
	case own == nil:
		fault("world.daemon: the daemon holds no record of the case's issue %s", key)
	default:
		if stated := parts.Issue; stated != nil {
			if own.Tree != stated.Tree || own.Generation != stated.Generation {
				fault("world.daemon.issue: the daemon records %s in the tree of %s at generation %d, and the world states %s at %d", key, own.Tree, own.Generation, stated.Tree, stated.Generation)
			}
			if (own.LingerUntil == nil) != (stated.LingerUntil == nil) || own.LingerUntil != nil && !own.LingerUntil.Equal(*stated.LingerUntil) {
				fault("world.daemon.issue.lingerUntil: the daemon answers %v for %s, and the world states %v", own.LingerUntil, key, stated.LingerUntil)
			}
		}
		// A world that states no architect of the case's issue (absent and null decode alike) holds
		// the record to nothing; the role's own architect claim is held to being there.
		if parts.Architect != nil || role == claim.RoleArchitect && key == claimed {
			architect("world.daemon.architect", key, parts.Architect, own)
		}
		if entry := parts.Entry; entry != nil && own.PhaseEntry != entry.Phase {
			fault("world.daemon.entry.phase: the daemon's phases row of %s is on entry %d, and the world states %d", key, own.PhaseEntry, entry.Phase)
		}
	}
	for i, stated := range parts.Records {
		where := fmt.Sprintf("world.daemon.records[%d]", i)
		got := read(stated.Key)
		if got == nil {
			fault("%s: the daemon holds no record of %s", where, stated.Key)
			continue
		}
		if got.Tree != stated.Tree || got.Phase != stated.Phase || got.Status != stated.Status || got.Generation != stated.Generation {
			fault("%s: the daemon records %s in the tree of %s at %s, %s, generation %d, and the world states %s at %s, %s, generation %d", where, stated.Key, got.Tree, got.Phase, got.Status, got.Generation, stated.Tree, stated.Phase, stated.Status, stated.Generation)
		}
		architect(where+".architect", stated.Key, stated.Architect, got)
	}
	if stated := parts.Admission; stated != nil {
		blocked := make([]string, 0, len(admission.Blocked))
		for _, each := range admission.Blocked {
			blocked = append(blocked, each.Issue)
		}
		if admission.Cap != stated.Cap || !slices.Equal(admission.Active, stated.Active) || !slices.Equal(admission.Waiting, stated.Waiting) || !slices.Equal(slices.Sorted(slices.Values(blocked)), slices.Sorted(slices.Values(stated.Blocked))) {
			fault("world.daemon.admission: the daemon answers cap %d, active %v, waiting %v, blocked %v, and the world states cap %d, active %v, waiting %v, blocked %v", admission.Cap, admission.Active, admission.Waiting, blocked, stated.Cap, stated.Active, stated.Waiting, stated.Blocked)
		}
	}
	if len(parts.Gate) > 0 {
		root := read(tree)
		for i, row := range parts.Gate {
			where := fmt.Sprintf("world.daemon.gate[%d]", i)
			var step *api.GatedStepView
			if root != nil {
				for j := range root.GatedSteps {
					if string(root.GatedSteps[j].Step) == row.Step {
						step = &root.GatedSteps[j]
					}
				}
			}
			if step == nil {
				fault("%s: the daemon's record of %s holds no %s step", where, tree, row.Step)
				continue
			}
			if step.CurrentVersion != row.Current || (step.ApprovedVersion == nil) != (row.Approved == nil) || step.ApprovedVersion != nil && *step.ApprovedVersion != *row.Approved || step.ArtifactID != gateIDs[row.Step] {
				fault("%s: the daemon answers %s's %s step at version %d (approved %v) of %s, and the world states version %d (approved %v) of %s", where, tree, row.Step, step.CurrentVersion, step.ApprovedVersion, step.ArtifactID, row.Current, row.Approved, gateIDs[row.Step])
			}
		}
	}
	return faults
}

// teardownEffectsIssueRead is what lane D reads back of a Dispatch issue (`GET /api/v1/issues/{key}`,
// which `dispatch read` reads): its status, parent, place on the board, labels, links, claim and
// documents.
type teardownEffectsIssueRead struct {
	Status    string   `json:"status"`
	Parent    *string  `json:"parent"`
	Rank      string   `json:"rank"`
	Labels    []string `json:"labels"`
	BlockedBy []string `json:"blocked_by"`
	Claim     *struct {
		Actor teardownEffectsActor `json:"actor"`
	} `json:"claim"`
	Artifacts []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"artifacts"`
}

// teardownEffectsStepRead is one run as `GET /api/v1/issues/{key}/steps` lists it (model.StepRecord,
// which `dispatch steps` reads).
type teardownEffectsStepRead struct {
	Task         string     `json:"task"`
	Step         int        `json:"step"`
	Kind         string     `json:"kind"`
	Role         string     `json:"role"`
	Result       string     `json:"result"`
	StartedAt    *time.Time `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at"`
	Rating       *int       `json:"rating"`
	RatingReason *string    `json:"rating_reason"`
}

// teardownEffectsDispatchFaults names, by its part, each fact of world's Dispatch parts (the case's
// issue's labels, the other issues, the step runs) that the replay's Dispatch does not answer as the
// world states it: issues holds Dispatch's read of each issue by key, ranks the rank of every issue of
// the project, and runs each run `dispatch steps` lists, in its order. A rank is a place on the board:
// rank 1 before every other issue of the project, each next rank after the one before it.
func teardownEffectsDispatchFaults(world teardownEffectsWorld, issues map[string]teardownEffectsIssueRead, ranks map[string]string, runs []teardownEffectsStepRead) []string {
	var faults []string
	fault := func(format string, args ...any) { faults = append(faults, fmt.Sprintf(format, args...)) }
	same := func(a, b []string) bool {
		return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
	}
	if labels := world.Dispatch.Issue.Labels; labels != nil {
		if got := issues[world.Dispatch.Issue.Key].Labels; !same(got, *labels) {
			fault("world.dispatch.issue.labels: Dispatch answers %v, and the world states %v", got, *labels)
		}
	}
	text := func(value *string) string {
		if value == nil {
			return "none"
		}
		return *value
	}
	var ranked []teardownEffectsDispatchIssue
	for i, stated := range world.Dispatch.Issues {
		where := fmt.Sprintf("world.dispatch.issues[%d]", i)
		got, ok := issues[stated.Key]
		if !ok {
			fault("%s: Dispatch holds no issue %s", where, stated.Key)
			continue
		}
		if got.Status != stated.Status {
			fault("%s.status: Dispatch answers %s, and the world states %s", where, got.Status, stated.Status)
		}
		if text(got.Parent) != text(stated.Parent) {
			fault("%s.parent: Dispatch answers %s, and the world states %s", where, text(got.Parent), text(stated.Parent))
		}
		var holder *string
		if got.Claim != nil {
			holder = &got.Claim.Actor.ID
		}
		if text(holder) != text(stated.ClaimedBy) {
			fault("%s.claimedBy: Dispatch answers %s, and the world states %s", where, text(holder), text(stated.ClaimedBy))
		}
		if !same(got.Labels, stated.Labels) {
			fault("%s.labels: Dispatch answers %v, and the world states %v", where, got.Labels, stated.Labels)
		}
		if !same(got.BlockedBy, stated.BlockedBy) {
			fault("%s.blockedBy: Dispatch answers %v, and the world states %v", where, got.BlockedBy, stated.BlockedBy)
		}
		if stated.Rank != nil {
			ranked = append(ranked, stated)
		}
	}
	slices.SortFunc(ranked, func(a, b teardownEffectsDispatchIssue) int { return *a.Rank - *b.Rank })
	for i, each := range ranked {
		if i > 0 && !(ranks[ranked[i-1].Key] < ranks[each.Key]) {
			fault("world.dispatch.issues rank: Dispatch's board places %s (rank %d) before %s (rank %d)", each.Key, *each.Rank, ranked[i-1].Key, *ranked[i-1].Rank)
		}
	}
	if len(ranked) > 0 {
		first := ranked[0].Key
		for other, rank := range ranks {
			if other != first && rank < ranks[first] {
				fault("world.dispatch.issues rank: %s is rank %d, and Dispatch's board places %s before it", first, *ranked[0].Rank, other)
			}
		}
	}
	if world.Dispatch.Steps != nil {
		if len(runs) != len(world.Dispatch.Steps) {
			fault("world.dispatch.steps: Dispatch lists %d runs, and the world states %d", len(runs), len(world.Dispatch.Steps))
		}
		instant := func(at *time.Time) string {
			if at == nil {
				return "none"
			}
			return at.UTC().Format(time.RFC3339Nano)
		}
		for i, stated := range world.Dispatch.Steps {
			if i >= len(runs) {
				break
			}
			got := runs[i]
			result := ""
			if stated.Result != nil {
				result = *stated.Result
			}
			rating := func(value *int) string {
				if value == nil {
					return "none"
				}
				return fmt.Sprint(*value)
			}
			want := fmt.Sprintf("%s %d %s %s %s %s %s %s %s", stated.Task, stated.Step, stated.Kind, text(stated.Role), result, instant(&stated.StartedAt), instant(stated.EndedAt), rating(stated.PlanRating), text(stated.PlanReason))
			have := fmt.Sprintf("%s %d %s %s %s %s %s %s %s", got.Task, got.Step, got.Kind, got.Role, got.Result, instant(got.StartedAt), instant(got.EndedAt), rating(got.Rating), text(got.RatingReason))
			if have != want {
				fault("world.dispatch.steps[%d]: Dispatch lists %q, and the world states %q", i, have, want)
			}
		}
	}
	return faults
}

// teardownEffectsExampleWorld is the world of T0's public example case id
// (testdata/heldout-case-example.json), decoded as lane D decodes a world, with edit made to its
// JSON first.
func teardownEffectsExampleWorld(t *testing.T, id string, edit func(world map[string]any)) teardownEffectsWorld {
	t.Helper()
	data, err := os.ReadFile(teardownEffectsExample)
	if err != nil {
		t.Fatal(err)
	}
	var file struct{ Cases []map[string]any }
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	for _, c := range file.Cases {
		if c["id"] != id {
			continue
		}
		world := c["world"].(map[string]any)
		edit(world)
		encoded, err := json.Marshal(world)
		if err != nil {
			t.Fatal(err)
		}
		var decoded teardownEffectsWorld
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	t.Fatalf("the example has no case %s", id)
	return teardownEffectsWorld{}
}

// teardownEffectsEveryPart writes every typed part onto P05's world (ACME-12, a child of ACME-10,
// implementing): a claim with its session, the issue's record, an entry moved back, an architect,
// two records, admission, a gate step, the issue's labels, another issue and a step run, and a pull
// request's GitHub fields, with the repository's settings and rules.
func teardownEffectsEveryPart(world map[string]any) {
	daemon := world["daemon"].(map[string]any)
	daemon["claim"] = map[string]any{"issue": "ACME-12", "tree": "ACME-10", "session": "ses-acme-12-implementer"}
	daemon["issue"] = map[string]any{"tree": "ACME-10", "parent": "ACME-10", "generation": 2, "lingerUntil": nil}
	daemon["entry"] = map[string]any{"phase": 2, "turn": 1, "owedTask": "Phase: implementing. Back to implementing.", "movedBack": true}
	daemon["architect"] = map[string]any{"state": "suspended"}
	daemon["records"] = []any{
		map[string]any{"key": "ACME-10", "tree": "ACME-10", "parent": nil, "phase": "planning", "status": "in_progress", "generation": 1, "architect": map[string]any{"state": "idle"}},
		map[string]any{"key": "ACME-13", "tree": "ACME-10", "parent": "ACME-10", "phase": "admitted", "status": "todo", "generation": 1, "architect": nil},
	}
	daemon["admission"] = map[string]any{"cap": 2, "active": []any{"ACME-10", "ACME-12"}, "waiting": []any{"ACME-13"}, "blocked": []any{}}
	daemon["gate"] = []any{map[string]any{"step": "spec", "document": "spec.md", "current": 1, "approved": 1, "artifactId": "6c174565-c501-58c9-b35a-149c49ff8ffa"}}
	dispatch := world["dispatch"].(map[string]any)
	dispatch["issue"].(map[string]any)["labels"] = []any{"legion"}
	dispatch["issues"] = []any{map[string]any{"key": "ACME-13", "status": "todo", "parent": "ACME-10", "rank": 1, "labels": []any{}, "blockedBy": []any{}, "claimedBy": nil}}
	dispatch["steps"] = []any{map[string]any{"task": "default", "step": 1, "kind": "step", "role": "planner", "result": "passed", "startedAt": "2030-01-07T14:00:00Z", "endedAt": "2030-01-07T14:30:00Z"}}
	github := world["github"].(map[string]any)
	github["settings"] = map[string]any{"default_branch": "main", "allow_merge_commit": false}
	github["branchRules"] = []any{map[string]any{"branch": "main", "rules": []any{map[string]any{"type": "merge_queue"}}}}
	github["pullRequests"] = []any{map[string]any{"number": 41, "state": "open", "draft": false, "head": map[string]any{"ref": "legion/ACME-12", "sha": strings.Repeat("c", 40)}, "base": "main", "title": "ACME-12", "body": "", "mergeCommit": nil,
		"reviews": []any{}, "reviewThreads": []any{}, "checks": []any{}, "mergeStateStatus": "BLOCKED", "autoMergeRequest": nil, "nodeId": "PR_kwDOexample41", "diff": "diff --git a/x b/x\n", "mergeResult": map[string]any{"merged": true}}}
}

// Catches: a typed part lane D decodes and drops in silence, as it once dropped every part of
// world.daemon but its phase, step, task and production check, where an unseeded part must fail the
// setup naming it.
func TestTeardownEffectsWorldParts(t *testing.T) {
	t.Parallel()
	world := teardownEffectsExampleWorld(t, "P05", teardownEffectsEveryPart)
	if len(world.unserved) != 0 || len(teardownEffectsSeedFaults(world)) != 0 {
		t.Fatalf("every part of a world with every part is served: unserved %v, faults %v", world.unserved, teardownEffectsSeedFaults(world))
	}
	parts := world.Daemon.teardownEffectsDaemonParts
	if parts.Claim == nil || parts.Claim.Session != "ses-acme-12-implementer" || parts.Entry == nil || !parts.Entry.MovedBack || *parts.Entry.OwedTask == "" || parts.Architect.State != "suspended" || len(parts.Records) != 2 || parts.Admission.Cap != 2 || parts.Gate[0].ArtifactID == "" {
		t.Fatalf("the daemon parts decode as %+v", parts)
	}
	pull := world.GitHub.PullRequests[0]
	if pull.NodeID != "PR_kwDOexample41" || pull.MergeStateStatus != "BLOCKED" || string(pull.AutoMergeRequest) != "null" || pull.Diff == "" || string(pull.MergeResult) != `{"merged":true}` || world.GitHub.BranchRules == nil || len(world.GitHub.Settings) != 2 {
		t.Fatalf("the GitHub parts decode as %+v, settings %v, rules %v", pull, world.GitHub.Settings, world.GitHub.BranchRules)
	}
	if world.Dispatch.Issue.Labels == nil || len(world.Dispatch.Issues) != 1 || len(world.Dispatch.Steps) != 1 {
		t.Fatalf("the Dispatch parts decode as %+v", world.Dispatch)
	}
	plain := teardownEffectsExampleWorld(t, "P05", func(map[string]any) {})
	if plain.Daemon.Claim != nil || plain.Dispatch.Issue.Labels != nil || plain.GitHub.BranchRules != nil {
		t.Fatalf("the example's own world states no typed part, and decodes as %+v", plain)
	}
	future := teardownEffectsExampleWorld(t, "P05", func(world map[string]any) {
		teardownEffectsEveryPart(world)
		world["daemon"].(map[string]any)["slots"] = []any{}
		world["github"].(map[string]any)["pullRequests"].([]any)[0].(map[string]any)["autoMerge"] = true
	})
	if want := []string{"world.daemon.slots", "world.github.pullRequests[0].autoMerge"}; !slices.Equal(future.unserved, want) {
		t.Fatalf("unserved = %v, want %v", future.unserved, want)
	}
}

// Catches: lane D refusing a world that states world.dispatch.runsHoldRecords as a part it seeds
// nothing of, where it takes the field and writes each step run at the world's own instants.
func TestTeardownEffectsRunsHoldRecords(t *testing.T) {
	t.Parallel()
	world := teardownEffectsExampleWorld(t, "P05", func(world map[string]any) {
		teardownEffectsEveryPart(world)
		world["dispatch"].(map[string]any)["runsHoldRecords"] = true
	})
	if len(world.unserved) != 0 || len(teardownEffectsSeedFaults(world)) != 0 {
		t.Fatalf("a world whose runs hold its records is served: unserved %v, faults %v", world.unserved, teardownEffectsSeedFaults(world))
	}
}

// Catches: a part lane R's stand-in refuses at its start that lane D seeds anyway, or one lane D
// cannot hold that it writes in some other shape; each names its part.
func TestTeardownEffectsSeedFaults(t *testing.T) {
	t.Parallel()
	world := teardownEffectsExampleWorld(t, "P05", func(world map[string]any) {
		teardownEffectsEveryPart(world)
		daemon := world["daemon"].(map[string]any)
		daemon["claim"] = map[string]any{"issue": "ACME-10", "tree": "ACME-10"}
		daemon["records"].([]any)[0].(map[string]any)["architect"] = map[string]any{"state": "active"}
		daemon["gate"].([]any)[0].(map[string]any)["step"] = "design"
		daemon["admission"].(map[string]any)["waiting"] = []any{"ACME-99"}
		world["dispatch"].(map[string]any)["steps"].([]any)[0].(map[string]any)["role"] = nil
	})
	want := []string{
		"world.daemon.admission.waiting[0] ACME-99 is an issue the replay records nothing of, so no slot or line of the daemon's holds it",
		"world.daemon.entry is the phase entry of the claim's issue ACME-10, and the world states the phase of ACME-12 alone",
		"world.daemon.gate[0].step design is no step of the design gate the daemon names",
		"world.daemon.records[0].architect.state active is no state of a claim the daemon names",
		"world.dispatch.steps[0].role is null, and Dispatch records no run without its role",
	}
	if got := teardownEffectsSeedFaults(world); !slices.Equal(got, want) {
		t.Fatalf("faults =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Catches: a world record whose writer is the role's own session (daemon.claim.session) written
// under that session's id, which the real daemon then refuses as another session's
// (HANDOFF_SESSION_MISMATCH), where the replay writes it as its own; and any other writer taken for
// the role's.
func TestTeardownEffectsOwnWrite(t *testing.T) {
	t.Parallel()
	world := teardownEffectsExampleWorld(t, "P05", teardownEffectsEveryPart)
	if !teardownEffectsOwnWrite(world, "ses-acme-12-implementer") || teardownEffectsOwnWrite(world, "ses-acme-12-planner") {
		t.Fatal("only the claim's session writes the role's own records")
	}
	plain := teardownEffectsExampleWorld(t, "P05", func(map[string]any) {})
	if teardownEffectsOwnWrite(plain, "ses-acme-12-planner") {
		t.Fatal("a world with no claim session names no record the role's own")
	}
	if issue, tree, session := teardownEffectsClaimOf(plain); issue != "ACME-12" || tree != "ACME-10" || session != "" {
		t.Fatalf("the default claim is %s in %s (%q), want ACME-12 in ACME-10", issue, tree, session)
	}
}

// Catches: a seed whose read-back passes over a fact the real daemon answers otherwise, where the
// seed must fail naming the part; and a read-back that holds the role's own architect claim to the
// state the world wrote, which the live pane moves.
func TestTeardownEffectsDaemonReadBack(t *testing.T) {
	t.Parallel()
	world := teardownEffectsExampleWorld(t, "P05", teardownEffectsEveryPart)
	approved := 1
	gateID := "art-1"
	records := map[string]*api.Issue{
		"ACME-12": {Key: "ACME-12", Tree: "ACME-10", Generation: 2, Phase: phase.Implementing, Status: "in_progress", Architect: &api.ClaimView{State: "suspended"}, PhaseEntry: 2},
		"ACME-10": {Key: "ACME-10", Tree: "ACME-10", Generation: 1, Phase: phase.Planning, Status: "in_progress", Architect: &api.ClaimView{State: "idle"},
			GatedSteps: []api.GatedStepView{{Step: record.DesignStepSpec, ArtifactID: gateID, CurrentVersion: 1, ApprovedVersion: &approved}}},
		"ACME-13": {Key: "ACME-13", Tree: "ACME-10", Generation: 1, Phase: phase.Admitted, Status: "todo"},
	}
	read := func(key string) *api.Issue { return records[key] }
	admission := api.Admission{Cap: 2, Active: []string{"ACME-10", "ACME-12"}, Waiting: []string{"ACME-13"}, Blocked: []api.BlockedIssue{}}
	gates := map[string]string{"spec": gateID}
	if faults := teardownEffectsDaemonFaults(world, claim.RoleImplementer, read, admission, gates); len(faults) != 0 {
		t.Fatalf("a daemon that answers the world has faults %v", faults)
	}
	records["ACME-13"] = &api.Issue{Key: "ACME-13", Tree: "ACME-13", Generation: 1, Phase: phase.Admitted, Status: "todo", Architect: &api.ClaimView{State: "queued"}}
	records["ACME-12"].PhaseEntry = 1
	admission.Waiting = []string{}
	faults := teardownEffectsDaemonFaults(world, claim.RoleImplementer, read, admission, map[string]string{"spec": "art-2"})
	for _, part := range []string{"world.daemon.entry.phase", "world.daemon.records[1]:", "world.daemon.records[1].architect", "world.daemon.admission", "world.daemon.gate[0]"} {
		if !slices.ContainsFunc(faults, func(fault string) bool { return strings.HasPrefix(fault, part) }) {
			t.Errorf("faults %v name no %s", faults, part)
		}
	}
	// The root architect's own claim on ACME-10 is the replay's, whatever its state.
	root := teardownEffectsExampleWorld(t, "P05", func(world map[string]any) {
		teardownEffectsEveryPart(world)
		daemon := world["daemon"].(map[string]any)
		daemon["claim"] = map[string]any{"issue": "ACME-10", "tree": "ACME-10"}
		delete(daemon, "entry")
	})
	records["ACME-10"].Architect = &api.ClaimView{State: "working"}
	for _, fault := range teardownEffectsDaemonFaults(root, claim.RoleArchitect, read, admission, gates) {
		if strings.HasPrefix(fault, "world.daemon.records[0].architect") {
			t.Errorf("the replay's own architect claim is held to the world's state: %s", fault)
		}
	}
}

// Catches: a Dispatch seed that the scratch Dispatch answers otherwise passing as the world: a
// status, a label, a claim, a place on the board or a step run Dispatch does not hold as stated.
func TestTeardownEffectsDispatchReadBack(t *testing.T) {
	t.Parallel()
	world := teardownEffectsExampleWorld(t, "P05", teardownEffectsEveryPart)
	parent := "ACME-10"
	started := time.Date(2030, 1, 7, 14, 0, 0, 0, time.UTC)
	ended := started.Add(30 * time.Minute)
	issues := map[string]teardownEffectsIssueRead{
		"ACME-12": {Status: "in_progress", Labels: []string{"legion"}},
		"ACME-13": {Status: "todo", Parent: &parent, Labels: []string{}, BlockedBy: []string{}},
	}
	ranks := map[string]string{"ACME-10": "b", "ACME-12": "c", "ACME-13": "a"}
	runs := []teardownEffectsStepRead{{Task: "default", Step: 1, Kind: "step", Role: "planner", Result: "passed", StartedAt: &started, EndedAt: &ended}}
	if faults := teardownEffectsDispatchFaults(world, issues, ranks, runs); len(faults) != 0 {
		t.Fatalf("a Dispatch that answers the world has faults %v", faults)
	}
	issues["ACME-13"] = teardownEffectsIssueRead{Status: "backlog", Labels: []string{"ux"}}
	ranks["ACME-13"] = "d"
	runs[0].Result = "failed"
	faults := teardownEffectsDispatchFaults(world, issues, ranks, runs)
	for _, part := range []string{"world.dispatch.issues[0].status", "world.dispatch.issues[0].parent", "world.dispatch.issues[0].labels", "world.dispatch.issues rank", "world.dispatch.steps[0]"} {
		if !slices.ContainsFunc(faults, func(fault string) bool { return strings.HasPrefix(fault, part) }) {
			t.Errorf("faults %v name no %s", faults, part)
		}
	}
}

// Catches: the merge queue's enqueue of a pull request the world names by its node id (H08-B's
// `PR_fixture_144`), which the replay's GitHub found no pull request for, where the world's nodeId
// names it.
func TestTeardownEffectsNodeID(t *testing.T) {
	t.Parallel()
	pulls := map[int]teardownEffectsPull{144: {Number: 144, State: "open", Base: "main", NodeID: "PR_fixture_144"}}
	scratch := teardownEffectsScratch{pulls: pulls, branch: "legion/ACME-144", base: "main", remote: func(string) string { return "" }}
	if err := scratch.change(teardownEffectsPullMutation{Action: "enqueue", ID: "PR_fixture_144"}); err != nil {
		t.Fatal(err)
	}
	if !pulls[144].Queued {
		t.Fatalf("the enqueue left #144 as %+v", pulls[144])
	}
	if err := scratch.change(teardownEffectsPullMutation{Action: "enqueue", ID: "PR_other"}); err == nil || !strings.Contains(err.Error(), "names no pull request the replay's GitHub holds") {
		t.Fatalf("an enqueue of a node id no pull request has = %v", err)
	}
}

// Catches: the public fixture of the typed parts (testdata/teardown-effects/typed-parts-cases.json)
// losing a part, or holding one lane D cannot seed, before the `--controls` Job replays it. Its
// worlds are decoded as --controls decodes a case file.
func TestTeardownEffectsTypedPartsFixture(t *testing.T) {
	t.Parallel()
	var file struct{ Cases []teardownEffectsCase }
	data, err := os.ReadFile(filepath.Join("testdata", "teardown-effects", "typed-parts-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) != 2 {
		t.Fatalf("the fixture holds %d cases, want the example's two", len(file.Cases))
	}
	for _, c := range file.Cases {
		if len(c.World.unserved) != 0 || len(teardownEffectsSeedFaults(c.World)) != 0 {
			t.Fatalf("%s: unserved %v, faults %v", c.ID, c.World.unserved, teardownEffectsSeedFaults(c.World))
		}
	}
	p05 := file.Cases[0].World
	parts := p05.Daemon.teardownEffectsDaemonParts
	if parts.Claim == nil || parts.Issue == nil || parts.Entry == nil || len(parts.Records) == 0 || parts.Admission == nil || len(parts.Gate) == 0 {
		t.Fatalf("P05's world lacks a daemon part: %+v", parts)
	}
	if p05.Dispatch.Issue.Labels == nil || len(p05.Dispatch.Issues) == 0 || len(p05.Dispatch.Steps) == 0 || p05.GitHub.Settings == nil || p05.GitHub.BranchRules == nil {
		t.Fatalf("P05's world lacks a Dispatch or GitHub part: %+v %+v", p05.Dispatch, p05.GitHub)
	}
	pull := p05.GitHub.PullRequests[len(p05.GitHub.PullRequests)-1]
	if pull.MergeStateStatus == "" || pull.AutoMergeRequest == nil || pull.NodeID == "" || pull.Diff == "" || pull.MergeResult == nil {
		t.Fatalf("P05's last pull request lacks a field: %+v", pull)
	}
}
