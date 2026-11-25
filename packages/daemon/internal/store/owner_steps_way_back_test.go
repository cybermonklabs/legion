package store

import (
	"context"
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

// ownerStepsMigration is the migration whose header carries the way back of the release whose
// owner's plan picks the steps, ownerStepsVersion its version and ownerStepsRelease the release a
// failure to read its block names. docs/kubernetes.md carries the same block under "Rolling back
// the release whose owner's plan picks the steps", and internal/textcheck's
// TestTheOwnerStepsWayBackIsOneTextInTheHeaderAndTheGuide holds the two to one text. This file
// reads the header alone (wayBackOf), as the plan ratings' test does.
const (
	ownerStepsMigration = "20261117034837_issue_plans.up.sql"
	ownerStepsVersion   = 20261117034837
	ownerStepsRelease   = "the release whose owner's plan picks the steps"
)

// ownerStepsProjectPredicate is how each queue delete of the way back names the project whose daemon
// rolls back: the project of a row is the part of its issue column before the first hyphen, since a
// project key holds none.
const ownerStepsProjectPredicate = " and split_part(issue, '-', 1) = '<PROJECT>'"

// ownerStepsWayBack is the transaction the header's last paragraph gives (wayBackOf), which names
// the project as <PROJECT>: one transaction, opened by begin and closed by commit. A block of
// another shape stops the test by name.
func ownerStepsWayBack(t *testing.T) string {
	t.Helper()
	block := wayBackOf(t, ownerStepsVersion, ownerStepsRelease)
	if lines := strings.Split(block, "\n"); len(lines) < 2 || lines[0] != "begin;" || lines[len(lines)-1] != "commit;" {
		t.Fatalf("the way back of %s is %q, want one transaction opened by begin and closed by commit", ownerStepsMigration, lines)
	}
	return block
}

// planRolesMigration is the migration of the roles a plan defines, whose table plan_roles holds a
// foreign key on issue_plans. A rollback undoes its newest migration first, so a rollback past
// this release runs that migration's way back before ownerStepsWayBack, as its header and
// docs/kubernetes.md say: the drop of issue_plans is refused while plan_roles stands.
const planRolesMigration = "20261125114700_plan_roles.up.sql"

// runPlanRolesWayBack runs the way back the header of planRolesMigration carries, its psql block
// (headerWayBackOf), on store, as an operator runs it before this release's block.
func runPlanRolesWayBack(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(), headerWayBackOf(t, planRolesMigration), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back of %s before %s's: %v", planRolesMigration, ownerStepsMigration, err)
	}
}

// noticeKindsBeforeOwnerSteps is the list of the notice kinds an architect is told in the release
// this rollback goes back to (its validNoticeKinds): every kind of the tree's list but the five
// that the release whose owner's plan picks the steps and the release whose owner does a step add:
// task-stopped, plan-not-taken and retro-held, and owner-step and merge-forward-lost.
// superviseOpsBeforeOwnerSteps is that release's list of supervise ops, which has neither step_stop
// nor slot_credential. Both are copies because that release's code is not in the tree. Its list of
// the kinds for the controller alone is this release's (controllerOnlyNoticeKind), unchanged.
var (
	noticeKindsBeforeOwnerSteps = []record.NoticeKind{
		"phase-finished", "worker-died", "worker-stalled", "worker-silent", "worker-background-stalled", "held",
		"pr-blocked", "pr-merged", "pr-closed-unmerged", "pull-request-missing", "message-refused",
		"design-approved", "design-changes-requested", "carried-gate-opened",
		"child-closed", "child-status", "children-first", "child-release-stalled", "child-not-recorded",
		"catch-up", "checks-red", "review-stuck", "status-reasserted", record.VolumeNearFullNotice,
		"session-lost", "session-forgotten", "session-store-fault",
	}
	controllerKindsBeforeOwnerSteps = []record.NoticeKind{
		record.TriageNotice, record.SlotFreeNotice, record.TodoNotice, record.TickNotice, record.UnslottedNotice,
		record.AwaitingPersonNotice, record.ParkedDecisionNotice, record.UpdateStalledNotice, record.ProviderErrorsNotice,
		record.TreeStalledNotice, record.ArchitectStalledNotice,
	}
	superviseOpsBeforeOwnerSteps = []record.SuperviseOp{"start", "suspend", "release", "tree_close", "issue_close", "pause_stop"}
)

// refusedBeforeOwnerSteps is why the release before refuses row, "" where it takes it. That
// release decodes a notice, a controller notice and a supervise row strictly (record's
// DecodeOutboxPayload, DisallowUnknownFields), so it refuses a kind not on its list, a notice naming
// the architect that stopped a task (stoppedBy, a field it does not hold) and an op not on its list,
// and it is no permanent refusal of those kinds, so it fails such a row at every attempt.
func refusedBeforeOwnerSteps(t *testing.T, row record.OutboxRow) string {
	t.Helper()
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("this release's decode of row %d: %v", row.ID, err)
	}
	switch value := payload.(type) {
	case record.Notice:
		if !slices.Contains(noticeKindsBeforeOwnerSteps, value.Kind) {
			return fmt.Sprintf("unknown notice kind %q", value.Kind)
		}
		if value.StoppedBy != "" {
			return `unknown field "stoppedBy"`
		}
	case record.ControllerNotice:
		if !slices.Contains(noticeKindsBeforeOwnerSteps, value.Kind) && !slices.Contains(controllerKindsBeforeOwnerSteps, value.Kind) {
			return fmt.Sprintf("unknown controller notice kind %q", value.Kind)
		}
		if value.StoppedBy != "" {
			return `unknown field "stoppedBy"`
		}
	case record.SuperviseRequest:
		if !slices.Contains(superviseOpsBeforeOwnerSteps, value.Op) {
			return fmt.Sprintf("unknown supervise operation %q", value.Op)
		}
	}
	return ""
}

// The way back of the release whose owner's plan picks the steps deletes the queued rows the release
// before cannot read, for the project whose daemon rolls back alone: its notices and controller
// notices of the three kinds this release adds (task-stopped, retro-held, plan-not-taken), and its
// stops of a stopped task's step (step_stop). The outbox is shared by every project's daemon on the
// database, so another project's rows of the same kinds stand, and so does every row of a kind the
// release before reads, of either project. The rows are queued for two projects, LEGION, which rolls
// back, and OTHER, which does not, and the block runs as the migration's header carries it, as one
// exec, with <PROJECT> LEGION.
//
// Two controls. Before the block, the release before's decode (refusedBeforeOwnerSteps) refuses each
// row of a new kind and takes each other row, so the test sees the rows the way back exists for.
// And the same block with its project predicate taken off deletes OTHER's rows of the new kinds too:
// that predicate, and nothing else, is what keeps them.
func TestTheOwnerStepsWayBackDeletesOnlyItsProjectsRowsTheReleaseBeforeCannotRead(t *testing.T) {
	ctx := context.Background()
	queuedAt := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	rowsOf := func(project string) []struct {
		name    string
		payload record.OutboxPayload
		newKind bool
	} {
		key, tree := project+"-208", project+"-208"
		stopper := claim.Token(strings.ToLower(project) + "-" + strings.ToLower(project) + "-" + strings.ToLower(key) + "-architect")
		return []struct {
			name    string
			payload record.OutboxPayload
			newKind bool
		}{
			{name: "the notice of an owner's stop", newKind: true, payload: record.Notice{Kind: "task-stopped", Role: claim.RoleTester, Phase: phase.Testing,
				StoppedBy: stopper, Reason: "task a of " + key + " was stopped by its owner"}},
			{name: "the notice of a stop no step could return from", newKind: true, payload: record.Notice{Kind: "task-stopped", Role: claim.RoleTester,
				Phase: phase.Testing, Reason: "task a of " + key + " was stopped: step 2 (tester) failed and no earlier step of the type implementation can take the work"}},
			{name: "the notice of a retro held for the issues beneath", newKind: true, payload: record.Notice{Kind: "retro-held",
				Role: claim.RoleImplementer, Phase: phase.Retro, Reason: key + " passed its last step"}},
			{name: "the notice of a plan not taken", newKind: true, payload: record.Notice{Kind: "plan-not-taken", Role: claim.RoleArchitect,
				Reason: "The first step of " + key + " took no plan of an earlier owner"}},
			// No writer of this release queues a controller notice of the three kinds: each is told
			// through Engine.notice alone, never noticeWithController. The kind takes every kind an
			// architect is told (record's validateOutboxPayload), so the block deletes such a row too,
			// and this row is that state, which no write of this release reaches.
			{name: "a controller notice of a stop, which no writer of this release queues", newKind: true, payload: record.ControllerNotice{Kind: "task-stopped",
				Role: claim.RoleTester, Phase: phase.Testing, Reason: "task a of " + key + " was stopped"}},
			{name: "the stop of a stopped task's step", newKind: true, payload: record.SuperviseRequest{Op: "step_stop", Tree: tree,
				Role: claim.RoleTester, Generation: 1, Reason: "task a of " + key + " was stopped by its owner"}},
			{name: "a phase-finished notice", payload: record.Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Phase: phase.Implementing}},
			{name: "a held notice for the controller", payload: record.ControllerNotice{Kind: "held", Role: claim.RoleImplementer, Phase: phase.Implementing}},
			{name: "a suspend", payload: record.SuperviseRequest{Op: "suspend", Tree: tree, Role: claim.RoleImplementer, Generation: 1}},
		}
	}
	scoped := ownerStepsWayBack(t)
	unscoped := strings.ReplaceAll(scoped, ownerStepsProjectPredicate, "")
	for _, tc := range []struct {
		name  string
		block string
		// otherKept says whether OTHER's rows of the new kinds stand after the block.
		otherKept bool
	}{
		{name: "control: the block without its project predicate deletes the other project's rows too", block: unscoped},
		{name: "the block deletes the rows of the project that rolls back alone", block: scoped, otherKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.otherKept && unscoped == scoped {
				t.Fatalf("the way back of %s holds no %q to take off: no delete of it names the project that rolls back", ownerStepsMigration,
					ownerStepsProjectPredicate)
			}
			store := emptyStore(t)
			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			type queuedRow struct {
				project, name string
				newKind       bool
				id            int64
			}
			var queued []queuedRow
			for _, project := range []string{"LEGION", "OTHER"} {
				for _, q := range rowsOf(project) {
					row, err := record.NewOutboxRow(project+"-208", q.payload, queuedAt)
					if err != nil {
						t.Fatalf("queue %s of %s: %v", q.name, project, err)
					}
					var id int64
					if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
						values ($1, $2, $3, 0, $4, '') returning id`, string(row.Kind), row.Issue, row.Payload, row.NextAt).Scan(&id); err != nil {
						t.Fatalf("insert %s of %s: %v", q.name, project, err)
					}
					row.ID = id
					if refused := refusedBeforeOwnerSteps(t, row); (refused != "") != q.newKind {
						t.Fatalf("the release before's decode of %s of %s refuses it with %q; the control wants it to refuse the rows of the new kinds alone, "+
							"or this test cannot see the rows the way back exists for", q.name, project, refused)
					}
					queued = append(queued, queuedRow{project: project, name: q.name, newKind: q.newKind, id: id})
				}
			}

			runPlanRolesWayBack(t, store)
			if _, err := store.Pool().Exec(ctx, strings.ReplaceAll(tc.block, "<PROJECT>", "LEGION"), pgx.QueryExecModeSimpleProtocol); err != nil {
				t.Fatalf("run the way back %s's header carries: %v", ownerStepsMigration, err)
			}

			for _, q := range queued {
				var stands bool
				if err := store.Pool().QueryRow(ctx, `select exists (select 1 from outbox where id = $1)`, q.id).Scan(&stands); err != nil {
					t.Fatalf("read row %d: %v", q.id, err)
				}
				kept := !q.newKind || (q.project == "OTHER" && tc.otherKept)
				switch {
				case kept && !stands:
					t.Errorf("the way back deleted %s of %s (row %d), want it kept", q.name, q.project, q.id)
				case !kept && stands:
					t.Errorf("the way back left %s of %s (row %d), want it deleted", q.name, q.project, q.id)
				}
			}
			var tables, version bool
			if err := store.Pool().QueryRow(ctx, `select to_regclass('issue_plans') is not null or to_regclass('plan_tasks') is not null
				or to_regclass('plan_steps') is not null, exists (select 1 from schema_version where version = 20261117034837)`).Scan(&tables, &version); err != nil {
				t.Fatalf("read the schema after the way back: %v", err)
			}
			if tables || version {
				t.Errorf("after the way back a plan table stands (%v) or the migration's version row does (%v), want neither", tables, version)
			}
		})
	}
}

// The way back also clears the two reasons of a hold this release writes, task-stopped and
// children, on the issues of the project that rolls back: the release before serves holdReason as
// the record holds it, and its plugin reads the key with the one value escalated and refuses the
// whole state for another. Each such issue stays held from its phase with no reason, a plain hold.
// An escalated hold keeps its reason, and another project's hold of either reason stands. The
// control: the update with its project predicate taken off clears the other project's too.
func TestTheOwnerStepsWayBackClearsItsProjectsTwoNewHoldReasons(t *testing.T) {
	ctx := context.Background()
	const issuePredicate = " and split_part(key, '-', 1) = '<PROJECT>'"
	scoped := ownerStepsWayBack(t)
	unscoped := strings.ReplaceAll(scoped, issuePredicate, "")
	holds := []struct {
		key    string
		reason record.Hold
	}{
		{"LEGION-208", record.Hold{From: phase.Testing, Reason: record.HoldTaskStopped}},
		{"LEGION-209", record.Hold{From: phase.Reviewing, Reason: record.HoldForChildren}},
		{"LEGION-210", record.Hold{From: phase.Testing, Reason: record.HoldEscalated}},
		{"OTHER-208", record.Hold{From: phase.Testing, Reason: record.HoldTaskStopped}},
		{"OTHER-209", record.Hold{From: phase.Reviewing, Reason: record.HoldForChildren}},
	}
	for _, tc := range []struct {
		name      string
		block     string
		otherKept bool
	}{
		{name: "control: the update without its project predicate clears the other project's holds too", block: unscoped},
		{name: "the update clears the two reasons of the project that rolls back alone", block: scoped, otherKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.otherKept && unscoped == scoped {
				t.Fatalf("the way back of %s holds no %q: no update names the project that rolls back", ownerStepsMigration, issuePredicate)
			}
			store := emptyStore(t)
			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			for _, h := range holds {
				hold := h.reason
				project := strings.SplitN(h.key, "-", 2)[0]
				issue := record.Issue{Key: h.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: h.key, Project: project,
					Title: h.key, Phase: phase.Held, Hold: &hold, Generation: 1, Status: "testing", Rank: "U"}
				if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error { return record.NewStore().PutIssue(ctx, tx, issue) }); err != nil {
					t.Fatalf("seed %s: %v", h.key, err)
				}
			}
			runPlanRolesWayBack(t, store)
			if _, err := store.Pool().Exec(ctx, strings.ReplaceAll(tc.block, "<PROJECT>", "LEGION"), pgx.QueryExecModeSimpleProtocol); err != nil {
				t.Fatalf("run the way back %s's header carries: %v", ownerStepsMigration, err)
			}
			for _, h := range holds {
				var reason, from *string
				if err := store.Pool().QueryRow(ctx, "select hold_reason, held_from from issues where key = $1", h.key).Scan(&reason, &from); err != nil {
					t.Fatalf("read %s: %v", h.key, err)
				}
				cleared := h.reason.Reason != record.HoldEscalated && (strings.HasPrefix(h.key, "LEGION-") || !tc.otherKept)
				switch {
				case from == nil || *from != string(h.reason.From):
					t.Errorf("%s is held from %v after the way back, want it still held from %s", h.key, from, h.reason.From)
				case cleared && reason != nil:
					t.Errorf("%s keeps the hold reason %q, want it cleared to a plain hold", h.key, *reason)
				case !cleared && (reason == nil || *reason != string(h.reason.Reason)):
					t.Errorf("%s holds the reason %v after the way back, want %q kept", h.key, reason, h.reason.Reason)
				}
			}
		})
	}
}

// ownerDoesAStepExtract is this package's copy of the statements docs/kubernetes.md gives under
// "Rolling back below the release whose owner does a step". No migration header carries that
// release's statements on the queued rows the release before cannot read, so the guide is where
// an operator reads them, and internal/textcheck's
// TestTheOwnerDoesAStepWayBackExtractIsTheGuidesBlock holds this copy to the guide's block. This
// file reads the copy alone, as the plan ratings' test reads the header alone, so the guide stays
// out of this package's CI filter.
const ownerDoesAStepExtract = "owner_does_a_step_way_back_extract.txt"

// ownerDoesAStepWayBack is the block ownerDoesAStepExtract carries, with <PROJECT> for the project
// that rolls back: every byte under the blank line that ends the file's `#` header. A copy with no
// blank line under its header, or with nothing under it, stops the test by name, since a block this
// cannot read is a block the test would pass while running nothing.
func ownerDoesAStepWayBack(t *testing.T) string {
	t.Helper()
	text, err := os.ReadFile(ownerDoesAStepExtract)
	if err != nil {
		t.Fatalf("read %s: %v", ownerDoesAStepExtract, err)
	}
	body := string(text)
	for strings.HasPrefix(body, "#") {
		_, body, _ = strings.Cut(body, "\n")
	}
	body, ended := strings.CutPrefix(body, "\n")
	if !ended {
		t.Fatalf("%s carries no blank line under its `#` header: that line is what ends the header, and every byte under it is the block", ownerDoesAStepExtract)
	}
	if block := strings.TrimRight(body, "\n"); block != "" {
		return block
	}
	t.Fatalf("%s carries nothing under its header: its copy of the guide's block is gone", ownerDoesAStepExtract)
	return ""
}

// refusedBeforeOwnerDoesAStep is why the release before the one whose owner does a step refuses
// row, "" where it takes it. That release decodes every payload the tree decodes, strictly, but for
// what the release whose owner does a step adds (ownerDoesAStepExtract): the notice kinds owner-step
// and merge-forward-lost, the supervise op slot_credential, and the field covers of a merge-forward
// message. It fails such a row at every attempt. This is a hand-written stand-in for that release's
// decode, since its code is not in the tree: it reads each row with this release's decoder and
// names what that release lacks.
func refusedBeforeOwnerDoesAStep(t *testing.T, row record.OutboxRow) string {
	t.Helper()
	switch value := mustDecode(t, row).(type) {
	case record.Notice:
		if value.Kind == record.OwnerStepNotice || value.Kind == record.MergeForwardLostNotice {
			return fmt.Sprintf("unknown notice kind %q", value.Kind)
		}
	case record.SuperviseRequest:
		if value.Op == "slot_credential" {
			return fmt.Sprintf("unknown supervise operation %q", value.Op)
		}
	case record.MergeForwardMessage:
		if value.Covers != "" {
			return `unknown field "covers"`
		}
	}
	return ""
}

// The operator's statements of the release whose owner does a step (ownerDoesAStepWayBack) leave
// the project whose daemon rolls back only queued rows the release before reads, and lose no
// message that release can send: they delete its owner-step and merge-forward-lost notices and its
// writes of the owner's slot back to its own App (slot_credential), and take the key covers off its
// merge-forward messages and keep them, so the ask or the review its pull request's record says it
// asked is still sent. Another project's rows of the same kinds stand as they are, and so does
// every row the release before reads, of either project: a merge-forward ask that covers nothing,
// a task-stopped notice, a phase-finished notice and a suspend. The rows are queued for LEGION,
// which rolls back, and OTHER, which does not, and the block runs as the guide gives it
// (ownerDoesAStepExtract), as one exec, with <PROJECT> LEGION. Then each row of LEGION left takes
// the release before's decode, and each row left, of either project, takes this release's decode,
// which the roll forward reads, with the payload it was queued with, less covers where the block
// took it off.
//
// Two controls. Before the block, the release before's decode refuses each row of a kind or a
// field the release whose owner does a step adds and takes each other row, so the test sees the
// rows the statements exist for. And the same block with its project predicate taken off deletes
// and strips OTHER's rows too.
//
// What this cannot show is the earlier daemon running: its binary is not here, so its decode is
// the hand-written stand-in above (refusedBeforeOwnerDoesAStep).
func TestTheOwnerDoesAStepWayBackLeavesItsProjectOnlyRowsTheReleaseBeforeReads(t *testing.T) {
	ctx := context.Background()
	queuedAt := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	// does is what the block does to a row of the project that rolls back.
	type does int
	const (
		keepsIt does = iota
		deletesIt
		stripsCovers
	)
	type queued struct {
		name    string
		payload record.OutboxPayload
		does    does
	}
	rowsOf := func(project string) []queued {
		key, tree := project+"-208", project+"-208"
		token := func(role claim.Role) claim.Token {
			token, err := claim.NewToken(strings.ToLower(project), key, role)
			if err != nil {
				t.Fatalf("the token of %s's %s: %v", key, role, err)
			}
			return token
		}
		return []queued{
			{name: "an owner-step", does: deletesIt, payload: record.Notice{Kind: record.OwnerStepNotice, Role: claim.RoleImplementer,
				Phase: phase.Implementing, Reason: "Step 1 of 2: implementer:owner.", OwnerStep: &record.NoticeStep{Task: "a", Kind: "step", Place: 1, Starts: 1}}},
			{name: "a merge-forward-lost notice", does: deletesIt, payload: record.Notice{Kind: record.MergeForwardLostNotice, Role: claim.RoleReviewer,
				Phase: phase.Reviewing, Reason: "the review of the merge forward of #42 did not reach the reviewer"}},
			{name: "a review that covers the landed", does: stripsCovers, payload: record.MergeForwardMessage{Role: claim.RoleReviewer,
				Claim: token(claim.RoleReviewer), Step: record.MergeForwardReview, Head: "sha-1", Landed: "sha-2", Base: "main", Phase: phase.Reviewing,
				PullRequest: 42, Covers: record.MergeForwardLanded}},
			{name: "an ask of the owner that covers the hold", does: stripsCovers, payload: record.MergeForwardMessage{Role: claim.RoleImplementer,
				Claim: token(claim.RoleArchitect), Step: record.MergeForwardAsk, Head: "sha-1", Base: "main", Phase: phase.Reviewing, PullRequest: 42,
				Why: record.ConflictCheckNotGreen, Covers: record.MergeForwardHold}},
			{name: "the write-back of the owner's slot", does: deletesIt, payload: record.SuperviseRequest{Op: "slot_credential", Tree: tree,
				Role: claim.RoleArchitect, Claim: token(claim.RoleArchitect), Generation: 1}},
			{name: "an ask that covers nothing", payload: record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk,
				Head: "sha-1", Base: "main", Phase: phase.Reviewing, PullRequest: 42, Why: record.ConflictCheckNotGreen}},
			{name: "a task-stopped notice", payload: record.Notice{Kind: "task-stopped", Role: claim.RoleTester, Phase: phase.Testing,
				StoppedBy: token(claim.RoleArchitect), Reason: "task a of " + key + " was stopped by its owner"}},
			{name: "a phase-finished notice", payload: record.Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Phase: phase.Implementing}},
			{name: "a suspend", payload: record.SuperviseRequest{Op: "suspend", Tree: tree, Role: claim.RoleImplementer, Generation: 1}},
		}
	}
	scoped := ownerDoesAStepWayBack(t)
	unscoped := strings.ReplaceAll(scoped, ownerStepsProjectPredicate, "")
	for _, tc := range []struct {
		name  string
		block string
		// otherKept says whether OTHER's rows stand as they were queued after the block.
		otherKept bool
	}{
		{name: "control: the block without its project predicate deletes and strips the other project's rows too", block: unscoped},
		{name: "the block leaves the project that rolls back only rows the release before reads", block: scoped, otherKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.otherKept && unscoped == scoped {
				t.Fatalf("the block of %s holds no %q to take off: no statement of it names the project that rolls back", ownerDoesAStepExtract,
					ownerStepsProjectPredicate)
			}
			store := emptyStore(t)
			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			type queuedRow struct {
				project string
				queued
				id int64
			}
			var rows []queuedRow
			for _, project := range []string{"LEGION", "OTHER"} {
				for _, q := range rowsOf(project) {
					row, err := record.NewOutboxRow(project+"-208", q.payload, queuedAt)
					if err != nil {
						t.Fatalf("queue %s of %s: %v", q.name, project, err)
					}
					var id int64
					if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
						values ($1, $2, $3, 0, $4, '') returning id`, string(row.Kind), row.Issue, row.Payload, row.NextAt).Scan(&id); err != nil {
						t.Fatalf("insert %s of %s: %v", q.name, project, err)
					}
					row.ID = id
					if refused := refusedBeforeOwnerDoesAStep(t, row); (refused != "") != (q.does != keepsIt) {
						t.Fatalf("the release before's decode of %s of %s refuses it with %q; the control wants it to refuse the rows of a new kind or field alone, "+
							"or this test cannot see the rows the statements exist for", q.name, project, refused)
					}
					rows = append(rows, queuedRow{project: project, queued: q, id: id})
				}
			}

			if _, err := store.Pool().Exec(ctx, strings.ReplaceAll(tc.block, "<PROJECT>", "LEGION"), pgx.QueryExecModeSimpleProtocol); err != nil {
				t.Fatalf("run the statements %s gives: %v", ownerDoesAStepExtract, err)
			}

			for _, q := range rows {
				rolledBack := q.project == "LEGION" || !tc.otherKept
				var kind string
				var payload []byte
				err := store.Pool().QueryRow(ctx, `select kind, payload from outbox where id = $1`, q.id).Scan(&kind, &payload)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("read row %d: %v", q.id, err)
				}
				stands, gone := err == nil, rolledBack && q.does == deletesIt
				if stands == gone {
					t.Errorf("after the block %s of %s (row %d) stands: %t, want %t", q.name, q.project, q.id, stands, !gone)
				}
				if !stands || gone {
					continue
				}
				row := record.OutboxRow{ID: q.id, Kind: record.OutboxKind(kind), Issue: q.project + "-208", Payload: payload}
				want := q.payload
				if rolledBack && q.does == stripsCovers {
					stripped := q.payload.(record.MergeForwardMessage)
					stripped.Covers = ""
					want = stripped
				}
				queuedAs, err := record.NewOutboxRow(row.Issue, want, queuedAt)
				if err != nil {
					t.Fatalf("queue %+v of %s: %v", want, row.Issue, err)
				}
				if !reflect.DeepEqual(mustDecode(t, row), mustDecode(t, queuedAs)) {
					t.Errorf("after the block %s of %s (row %d) holds %s, want the payload %+v", q.name, q.project, q.id, payload, want)
				}
				if refused := refusedBeforeOwnerDoesAStep(t, row); rolledBack && refused != "" {
					t.Errorf("after the block the release before refuses %s of %s (row %d) with %q, want it read", q.name, q.project, q.id, refused)
				}
			}
		})
	}
}

// mustDecode is this release's decode of row, which fails the test where it refuses the row.
func mustDecode(t *testing.T, row record.OutboxRow) record.OutboxPayload {
	t.Helper()
	value, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("this release's decode of row %d: %v", row.ID, err)
	}
	return value
}
