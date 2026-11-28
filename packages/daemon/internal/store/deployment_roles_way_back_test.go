package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// deploymentRolesWayBackExtract is the copy of the outbox's way back docs/kubernetes.md gives
// under "Rolling back the release whose deployment defines roles", which internal/textcheck's
// TestTheDeploymentRolesWayBackExtractIsTheGuidesBlock holds to the guide's block, byte for byte.
// This file reads the copy beside it rather than the guide, so the guide stays out of this
// package's CI filter: a test that read the guide would start the whole daemon Go lane on an edit
// of the guide alone.
const deploymentRolesWayBackExtract = "deployment_roles_way_back_extract.txt"

// deploymentRolesWayBack is the block the extract carries: everything under its run of `#` lines,
// with rollingBackProject where the operator writes the key of the project that rolls back. A copy
// with nothing under its header stops the test by name: a block this cannot find is a block it
// would pass while running nothing.
func deploymentRolesWayBack(t *testing.T) string {
	t.Helper()
	text, err := os.ReadFile(deploymentRolesWayBackExtract)
	if err != nil {
		t.Fatalf("read the outbox's way back of the release whose deployment defines roles: %v", err)
	}
	body := string(text)
	for strings.HasPrefix(body, "#") {
		_, body, _ = strings.Cut(body, "\n")
	}
	if body = strings.TrimSpace(body); body == "" {
		t.Fatalf("%s carries nothing under its `#` header", deploymentRolesWayBackExtract)
	}
	return body
}

// deploymentRolesWayBackFor is the block as an operator who rolls project back runs it: with
// project's key in place of rollingBackProject.
func deploymentRolesWayBackFor(t *testing.T, project string) string {
	t.Helper()
	return strings.ReplaceAll(deploymentRolesWayBack(t), rollingBackProject, project)
}

// The release before this one, the release this rollback goes back to, is main at f47b1e17c4f2.
// Its types are not in the tree, so what it reads of a row is written here from its code: the two
// shapes below, which it decodes strictly (record.DecodeOutboxPayload, DisallowUnknownFields), and
// the checks refusedBeforeTheRoles makes. Every other payload of the rows below is this release's
// shape and that release's alike.

// mergeForwardBeforeTheRoles is record.MergeForwardMessage as the release before declares it: every
// field of this release's shape but Worker.
type mergeForwardBeforeTheRoles struct {
	Role        claim.Role              `json:"role"`
	Step        record.MergeForwardStep `json:"step"`
	Head        string                  `json:"head"`
	Landed      string                  `json:"landed,omitempty"`
	Base        string                  `json:"base"`
	Phase       phase.Phase             `json:"phase"`
	PullRequest int                     `json:"pullRequest"`
	Why         record.ConflictWhy      `json:"why,omitempty"`
	SentLaunch  uint64                  `json:"sentLaunch,omitempty"`
}

// catchUpIssueBeforeTheRoles is record.CatchUpIssue as the release before declares it: every field
// of this release's shape but StepRole.
type catchUpIssueBeforeTheRoles struct {
	Key    string      `json:"key"`
	Parent string      `json:"parent,omitempty"`
	Title  string      `json:"title"`
	Phase  phase.Phase `json:"phase"`
	Status string      `json:"status"`
}

// stateFieldsBeforeTheRoles is every field a state change may name in the release before (its
// stateevent.Fields): this release's fields but stepRole.
var stateFieldsBeforeTheRoles = []record.StateField{record.StateFieldPhase, record.StateFieldTree, record.StateFieldSlot,
	record.StateFieldWaitingOn, record.StateFieldClaim}

// decodedStrictly is the error of decoding payload into into with DisallowUnknownFields, as the
// release before decodes every payload.
func decodedStrictly(payload []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

// refusedBeforeTheRoles is the release before's refusal of row, nil where it takes it, in each way
// that release refuses a row this release queues: a controller notice of the kind pod-wait-held,
// which it does not know (its validateOutboxPayload: "unknown controller notice kind"); a catch-up
// issue naming stepRole, a key its CatchUpIssue does not have; a merge-forward message naming
// worker, a key its MergeForwardMessage does not have, or a hold or a landed to a role outside
// Legion's six, which its MergeForwardReaches names for neither; a pause's stop, a phase-ended
// message, a hook prompt or an update ask to a role outside the six (its claim.IsRole, which holds
// the six alone); and a state change naming the field stepRole, or a claim change of a role outside
// the six (its stateevent.Change.Validate).
func refusedBeforeTheRoles(row record.OutboxRow) error {
	var named struct {
		Kind record.NoticeKind `json:"kind"`
		Role claim.Role        `json:"role"`
		Op   string            `json:"op"`
	}
	if err := json.Unmarshal(row.Payload, &named); err != nil {
		return fmt.Errorf("read the %s row %d: %w", row.Kind, row.ID, err)
	}
	switch row.Kind {
	case record.OutboxKindControllerNotice:
		if named.Kind == record.PodWaitHeldNotice {
			return fmt.Errorf("unknown controller notice kind %q", named.Kind)
		}
	case record.OutboxKindNotice:
		var notice struct {
			CatchUp *struct {
				Issues json.RawMessage `json:"issues"`
			} `json:"catch_up"`
		}
		if err := json.Unmarshal(row.Payload, &notice); err != nil {
			return fmt.Errorf("read the notice row %d: %w", row.ID, err)
		}
		if notice.CatchUp != nil && len(notice.CatchUp.Issues) > 0 {
			var issues []catchUpIssueBeforeTheRoles
			if err := decodedStrictly(notice.CatchUp.Issues, &issues); err != nil {
				return fmt.Errorf("decode the issues of the catch-up on row %d: %w", row.ID, err)
			}
		}
	case record.OutboxKindMergeForward:
		var message mergeForwardBeforeTheRoles
		if err := decodedStrictly(row.Payload, &message); err != nil {
			return fmt.Errorf("decode the merge-forward message on row %d: %w", row.ID, err)
		}
		if (message.Step == record.MergeForwardHold || message.Step == record.MergeForwardLanded) && !claim.IsBuiltin(message.Role) {
			return fmt.Errorf("a merge-forward hold or landed goes to the planner, the tester, the reviewer or the merger; a %s to %q is none of these",
				message.Step, message.Role)
		}
	case record.OutboxKindSupervise:
		if named.Op == "pause_stop" && !claim.IsBuiltin(named.Role) {
			return fmt.Errorf("a pause's stop names its pause and the role it stops; it names role %q", named.Role)
		}
	case record.OutboxKindPhaseEnded, record.OutboxKindHookPrompt, record.OutboxKindUpdateAsk:
		if !claim.IsBuiltin(named.Role) {
			return fmt.Errorf("a %s row goes to %q, which is none of a claim's six", row.Kind, named.Role)
		}
	case record.OutboxKindStateChange:
		var change record.StateChange
		if err := json.Unmarshal(row.Payload, &change); err != nil {
			return fmt.Errorf("read the state change on row %d: %w", row.ID, err)
		}
		for _, moved := range change.Changes {
			if !slices.Contains(stateFieldsBeforeTheRoles, moved.Field) {
				return fmt.Errorf("a state change of %s names the field %q, which is none of %v", change.Issue, moved.Field, stateFieldsBeforeTheRoles)
			}
			if moved.Field == record.StateFieldClaim && !claim.IsBuiltin(moved.Role) {
				return fmt.Errorf("a claim change of %s names the role %q, which is none of a claim's six", change.Issue, moved.Role)
			}
		}
	}
	return nil
}

// deploymentRolesQueued is one pending row of a project as this release queues it. keyed is the
// row's issue column: the issue, or the bare project key for a state row. left is what this
// release reads of the row of the project that rolls back once the way back has run, nil where
// the way back deletes the row. waiting says whether an attempt has met the row already, so that
// it waits to retry.
type deploymentRolesQueued struct {
	name    string
	keyed   string
	payload record.OutboxPayload
	left    record.OutboxPayload
	waiting bool
}

// The project that rolls back, and another project on the same database whose daemon stays on
// this release; the role of the deployment the rows name, LEGION-241's checker, and a second one
// on another issue of the tree; and the daemon run the state rows name.
const (
	deploymentRolesRollingBack            = "LEGION"
	deploymentRolesStaying                = "OTHER"
	deploymentRolesRole        claim.Role = "checker"
	deploymentRolesOtherRole   claim.Role = "auditor"
	deploymentRolesRun                    = "0b9f5c2e-4f1d-4c8a-9d57-2f3a6e8b1c40"
)

// deploymentRolesWayBackRows is project's pending rows as this release queues them, of its issue
// project-311: for each kind the way back clears, a row that names a role of the deployment and
// one that names none. A pod-wait-held notice to the controller, which the way back deletes, and
// a held one. A catch-up whose tree has two issues at a role's step, which the way back strips of
// their stepRole, and one whose tree has none at step. A merge-forward review at step naming the
// step's role as its worker, waiting to retry, which the way back strips of its worker, and one in
// retro. A merge-forward hold and a landed, a pause's stop, a phase-ended message, a hook prompt
// and an update ask to checker, which the way back deletes, and each of them to one of Legion's
// six. A state change into checker's step and one of checker's claim, which the way back deletes,
// and one of the implementer's claim and one of a phase and a slot.
func deploymentRolesWayBackRows(project string) []deploymentRolesQueued {
	issue := project + "-311"
	checker := deploymentRolesRole
	keeps := func(name, keyed string, payload record.OutboxPayload) deploymentRolesQueued {
		return deploymentRolesQueued{name: name, keyed: keyed, payload: payload, left: payload}
	}
	deletes := func(name, keyed string, payload record.OutboxPayload) deploymentRolesQueued {
		return deploymentRolesQueued{name: name, keyed: keyed, payload: payload}
	}
	catchUp := func(issues []record.CatchUpIssue) record.Notice {
		return record.Notice{Kind: "catch-up", Role: claim.RoleArchitect,
			Reason: "the tree of " + issue + " at generation 1 as the daemon records it now; start or resume it as your role says",
			CatchUp: &record.CatchUp{Generation: 1, Launch: 1, CatchUpTree: record.CatchUpTree{
				Gates:  []record.CatchUpGate{{Step: record.DesignStepSpec, Gated: true, Artifact: "artifact-311", Version: 1, Approved: 1, Open: true}},
				Issues: issues}}}
	}
	atStep := []record.CatchUpIssue{
		{Key: issue, Title: "The deployment's own check", Phase: phase.Step, StepRole: checker, Status: "in_progress"},
		{Key: project + "-312", Parent: issue, Title: "The check's fixture", Phase: phase.Implementing, Status: "in_progress"},
		{Key: project + "-313", Parent: issue, Title: "The check's audit", Phase: phase.Step, StepRole: deploymentRolesOtherRole, Status: "in_progress"},
	}
	stripped := slices.Clone(atStep)
	for i := range stripped {
		stripped[i].StepRole = ""
	}
	atNoStep := []record.CatchUpIssue{
		{Key: issue, Title: "The deployment's own check", Phase: phase.Testing, Status: "in_progress"},
		{Key: project + "-312", Parent: issue, Title: "The check's fixture", Phase: phase.Implementing, Status: "in_progress"},
	}
	review := record.MergeForwardMessage{Role: claim.RoleReviewer, Step: record.MergeForwardReview, Head: "c0de", Landed: "d00d",
		Base: "main", Phase: phase.Step, PullRequest: 42, Worker: checker}
	reviewTellingNoWorker := review
	reviewTellingNoWorker.Worker = ""
	stateChange := func(changes ...record.StateFieldChange) record.StateChange {
		return record.StateChange{Project: project, Run: deploymentRolesRun, Issue: issue, Changes: changes}
	}
	claimMoved := func(role claim.Role) record.StateFieldChange {
		return record.StateFieldChange{Field: record.StateFieldClaim, Role: role, Old: json.RawMessage(`"launching"`), New: json.RawMessage(`"working"`)}
	}
	return []deploymentRolesQueued{
		deletes("a pod-wait-held notice to the controller", issue, record.ControllerNotice{Kind: record.PodWaitHeldNotice, Role: checker,
			Reason: "The start of checker on " + issue + " waits for its pod, which was built before the deployment defined checker."}),
		keeps("a held notice to the controller", issue, record.ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning,
			Reason: "escalated"}),
		{name: "a catch-up whose tree has two issues at a role's step", keyed: issue, payload: catchUp(atStep), left: catchUp(stripped)},
		keeps("a catch-up whose tree has no issue at step", issue, catchUp(atNoStep)),
		{name: "a merge-forward review at step naming the step's role, waiting to retry", keyed: issue, payload: review,
			left: reviewTellingNoWorker, waiting: true},
		keeps("a merge-forward review in retro", issue, record.MergeForwardMessage{Role: claim.RoleReviewer, Step: record.MergeForwardReview,
			Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Retro, PullRequest: 42}),
		deletes("a merge-forward hold to a role of the deployment", issue, record.MergeForwardMessage{Role: checker, Step: record.MergeForwardHold,
			Head: "c0de", Base: "main", Phase: phase.Step, PullRequest: 42, Why: record.ConflictMergeNext}),
		deletes("a merge-forward landed to a role of the deployment", issue, record.MergeForwardMessage{Role: checker, Step: record.MergeForwardLanded,
			Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Step, PullRequest: 42, Why: record.ConflictRulesUnread}),
		keeps("a merge-forward hold to the merger", issue, record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardHold,
			Head: "c0de", Base: "main", Phase: phase.Merging, PullRequest: 42, Why: record.ConflictMergeNext}),
		keeps("a merge-forward landed to the tester", issue, record.MergeForwardMessage{Role: claim.RoleTester, Step: record.MergeForwardLanded,
			Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Testing, PullRequest: 42, Why: record.ConflictRulesUnread}),
		deletes("a pause's stop of a role of the deployment", issue, record.SuperviseRequest{Op: "pause_stop", Tree: issue, Role: checker,
			Generation: 3, Pause: 7}),
		keeps("a pause's stop of the implementer", issue, record.SuperviseRequest{Op: "pause_stop", Tree: issue, Role: claim.RoleImplementer,
			Generation: 3, Pause: 7}),
		deletes("a phase-ended message to a role of the deployment", issue, record.PhaseEnded{Role: checker, From: phase.Step,
			To: phase.AwaitingChildren, Reason: "the admission pass stopped checker's step, while these Dispatch children of " + issue +
				" were open: " + project + "-312 (todo)"}),
		keeps("a phase-ended message to the tester", issue, record.PhaseEnded{Role: claim.RoleTester, From: phase.Testing,
			To: phase.AwaitingChildren, Reason: "the admission pass stopped the tester's testing phase, while these Dispatch children of " +
				issue + " were open: " + project + "-312 (todo)"}),
		deletes("a hook prompt to a role of the deployment", issue, record.HookPrompt{Hook: "back-to-checker", Role: checker, From: phase.Step,
			To: phase.Step, Count: 1, Counts: map[string]int{"step-step": 1}, Prompt: "Read what the last check found before you run it again."}),
		keeps("a hook prompt to the implementer", issue, record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer,
			From: phase.Reviewing, To: phase.Implementing, Count: 1, Counts: map[string]int{"reviewing-implementing": 1},
			Prompt: "Read the reviewer's thread before you write anything."}),
		deletes("an update ask to a role of the deployment", issue, record.UpdateAsk{Role: checker, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a",
			Text: "The daemon must replace the pod that runs this issue's roles."}),
		keeps("an update ask to the tester", issue, record.UpdateAsk{Role: claim.RoleTester, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a",
			Text: "The daemon must replace the pod that runs this issue's roles."}),
		deletes("a state change into a role's step", project, stateChange(
			record.StateFieldChange{Field: record.StateFieldPhase, Old: json.RawMessage(`"implementing"`), New: json.RawMessage(`"step"`)},
			record.StateFieldChange{Field: record.StateFieldStepRole, Old: json.RawMessage(`null`), New: json.RawMessage(`"checker"`)})),
		deletes("a state change of a claim of a role of the deployment", project, stateChange(claimMoved(checker))),
		keeps("a state change of the implementer's claim", project, stateChange(claimMoved(claim.RoleImplementer))),
		keeps("a state change of a phase and a slot", project, stateChange(
			record.StateFieldChange{Field: record.StateFieldPhase, Old: json.RawMessage(`"testing"`), New: json.RawMessage(`"reviewing"`)},
			record.StateFieldChange{Field: record.StateFieldSlot, Old: json.RawMessage(`null`), New: json.RawMessage(`3`)})),
	}
}

// deploymentRolesWayBackProblems queues deploymentRolesWayBackRows of each of the two projects on a
// store of its own, runs block over them as one exec (the simple protocol a psql run uses), and
// answers each way the rows it leaves fall short. Of the project that rolls back: a row the way
// back keeps or strips that is gone, a row it deletes that stands, a row the release before still
// refuses, a row this release's decoder reads other than as it was queued with its role of the
// deployment taken out, and a waiting row whose retry state moved. Of the project that stays: a
// row gone, or read other than as it was queued, or a waiting row whose retry state moved, since
// its daemon stays on this release and still owes each row. Each problem names the row's project
// as "of project <key>".
func deploymentRolesWayBackProblems(t *testing.T, block string) []string {
	t.Helper()
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	queuedAt := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	retryAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	const retryError = "publish the merge-forward review: the listener answered 503"
	projects := []string{deploymentRolesRollingBack, deploymentRolesStaying}
	queued := map[string][]deploymentRolesQueued{}
	ids := map[string][]int64{}
	for _, project := range projects {
		queued[project] = deploymentRolesWayBackRows(project)
		for _, q := range queued[project] {
			row, err := record.NewOutboxRow(q.keyed, q.payload, queuedAt)
			if err != nil {
				t.Fatalf("queue %s of project %s: %v", q.name, project, err)
			}
			attempts, nextAt, lastError := 0, row.NextAt, ""
			if q.waiting {
				attempts, nextAt, lastError = 3, retryAt, retryError
			}
			var id int64
			if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
				values ($1, $2, $3, $4, $5, $6) returning id`, string(row.Kind), row.Issue, row.Payload, attempts, nextAt, lastError).Scan(&id); err != nil {
				t.Fatalf("insert %s of project %s: %v", q.name, project, err)
			}
			ids[project] = append(ids[project], id)
		}
	}
	if strings.TrimSpace(block) != "" {
		if _, err := store.Pool().Exec(ctx, block, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run the way back %s carries: %v", deploymentRolesWayBackExtract, err)
		}
	}

	var problems []string
	for _, project := range projects {
		rollsBack := project == deploymentRolesRollingBack
		for i, q := range queued[project] {
			id := ids[project][i]
			want := q.payload
			if rollsBack {
				want = q.left
			}
			var row record.OutboxRow
			var kind string
			var attempts int
			var nextAt time.Time
			var lastError string
			err := store.Pool().QueryRow(ctx, `select id, kind, issue, payload, attempts, next_at, last_error from outbox where id = $1`,
				id).Scan(&row.ID, &kind, &row.Issue, &row.Payload, &attempts, &nextAt, &lastError)
			switch {
			case errors.Is(err, pgx.ErrNoRows) && want == nil:
				continue
			case errors.Is(err, pgx.ErrNoRows) && rollsBack:
				problems = append(problems, fmt.Sprintf("%s of project %s (row %d) is gone after the way back, which keeps it", q.name, project, id))
				continue
			case errors.Is(err, pgx.ErrNoRows):
				problems = append(problems, fmt.Sprintf("%s of project %s (row %d) is gone after the way back, though its daemon stays on this release "+
					"and still owes it", q.name, project, id))
				continue
			case err != nil:
				t.Fatalf("read %s of project %s (row %d) after the way back: %v", q.name, project, id, err)
			}
			row.Kind = record.OutboxKind(kind)
			if want == nil {
				problems = append(problems, fmt.Sprintf("%s of project %s (row %d) stands after the way back, and the release before retries it at "+
					"every attempt: %v", q.name, project, id, refusedBeforeTheRoles(row)))
				continue
			}
			if rollsBack {
				if err := refusedBeforeTheRoles(row); err != nil {
					problems = append(problems, fmt.Sprintf("the release before refuses %s of project %s after the way back, and retries it at "+
						"every attempt: %v", q.name, project, err))
				}
			}
			decoded, err := record.DecodeOutboxPayload(row)
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("this release's decode of %s of project %s after the way back: %v", q.name, project, err))
			case !reflect.DeepEqual(decoded, want) && rollsBack:
				problems = append(problems, fmt.Sprintf("%s of project %s reads %#v after the way back, want %#v: the row as it was queued, "+
					"its role of the deployment taken out", q.name, project, decoded, want))
			case !reflect.DeepEqual(decoded, want):
				problems = append(problems, fmt.Sprintf("%s of project %s reads %#v after the way back, want %#v: the row as it was queued, "+
					"for its daemon, which stays on this release", q.name, project, decoded, want))
			}
			if q.waiting && (attempts != 3 || !nextAt.Equal(retryAt) || lastError != retryError) {
				problems = append(problems, fmt.Sprintf("%s of project %s waits to retry after the way back with %d attempts, at %s, after %q; "+
					"want its 3, %s and %q as they were", q.name, project, attempts, nextAt.UTC(), lastError, retryAt, retryError))
			}
		}
	}
	return problems
}

// The outbox's way back of the release whose deployment defines roles clears, for the project that
// rolls back, each queued row the release before refuses at every attempt: it deletes each
// pod-wait-held notice to the controller, each merge-forward hold or landed, pause's stop,
// phase-ended message, hook prompt and update ask to a role of the deployment, and each state
// change that names the role of a running step or a claim of such a role; and it takes stepRole off
// each issue of a queued catch-up and worker off each merge-forward review. Every other row stays as
// it was queued, a waiting row with its retry state, and the release before then takes each row
// that stands, as this release reads each as it was queued but for what the way back took out.
// Another project's daemon on the same database stays on this release, so each of its rows stands
// as it was. The block is the guide's, as the extract beside this file copies it, run for LEGION as
// an operator runs it.
//
// The controls come first: the release before refuses each queued row the way back deletes or
// strips and takes each row it leaves, and takes each stripped row with its role of the deployment
// taken out, so this test sees the rows the way back exists for. Then the block runs again once for
// each of its statements, with that statement taken out, and each such run must leave a row short:
// a statement this test could drop without failing is one it does not check. Last, the block runs
// once for each statement with that statement's project predicate cut out, and must change or
// delete a row of the other project: so each predicate is what keeps them.
//
// What this cannot show is the earlier daemon running: its binary is not here, so its reading of a
// row is the copy above (refusedBeforeTheRoles and the two shapes it decodes into).
func TestTheDeploymentRolesWayBackLeavesWhatTheReleaseBeforeReads(t *testing.T) {
	t.Parallel()
	for _, q := range deploymentRolesWayBackRows(deploymentRolesRollingBack) {
		row, err := record.NewOutboxRow(q.keyed, q.payload, time.Now())
		if err != nil {
			t.Fatalf("queue %s: %v", q.name, err)
		}
		cleared := !reflect.DeepEqual(q.left, q.payload)
		if err := refusedBeforeTheRoles(row); cleared != (err != nil) {
			t.Fatalf("the release before's reading of %s = %v; the control wants it to refuse each row the way back deletes or strips "+
				"and take each it leaves, or this test cannot see the rows the way back exists for", q.name, err)
		}
		if q.left == nil || !cleared {
			continue
		}
		left, err := record.NewOutboxRow(q.keyed, q.left, time.Now())
		if err != nil {
			t.Fatalf("queue %s with its role of the deployment taken out: %v", q.name, err)
		}
		if err := refusedBeforeTheRoles(left); err != nil {
			t.Fatalf("the release before refuses %s with its role of the deployment taken out: %v; the control wants it taken, "+
				"or a strip could leave no row that release reads", q.name, err)
		}
	}

	block := deploymentRolesWayBackFor(t, deploymentRolesRollingBack)
	if problems := deploymentRolesWayBackProblems(t, block); len(problems) > 0 {
		t.Fatalf("after the way back:\n%s", strings.Join(problems, "\n"))
	}
	statements := statementsOf(block)
	if len(statements) == 0 {
		t.Fatalf("%s carries no statement ending in `;`", deploymentRolesWayBackExtract)
	}
	for i, statement := range statements {
		without := strings.Join(slices.Delete(slices.Clone(statements), i, i+1), "\n")
		if problems := deploymentRolesWayBackProblems(t, without); len(problems) == 0 {
			t.Errorf("the way back without its statement %d leaves the rows as the whole block does, so this test does not check it:\n%s",
				i+1, statement)
		}
	}

	raw := statementsOf(deploymentRolesWayBack(t))
	for i, statement := range raw {
		if scoped := strings.Count(statement, projectScope); scoped != 1 {
			t.Fatalf("the way back's statement %d scopes itself %d times to the project that rolls back (%q), want once:\n%s",
				i+1, scoped, projectScope, statement)
		}
		unscoped := slices.Clone(raw)
		unscoped[i] = strings.ReplaceAll(statement, projectScope, "")
		if problems := deploymentRolesWayBackProblems(t, strings.Join(unscoped, "\n")); !slices.ContainsFunc(problems, func(problem string) bool {
			return strings.Contains(problem, "of project "+deploymentRolesStaying+" ")
		}) {
			t.Errorf("the control, the way back with no project predicate in its statement %d, leaves the rows of project %s as they were "+
				"(problems %q); want one of them changed or deleted, or this test cannot see a statement that clears another project's "+
				"rows:\n%s", i+1, deploymentRolesStaying, problems, statement)
		}
	}
}
