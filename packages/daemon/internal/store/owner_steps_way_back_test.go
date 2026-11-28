package store

import (
	"context"
	"fmt"
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

// noticeKindsBeforeOwnerSteps is the list of the notice kinds an architect is told in the release
// this rollback goes back to (its validNoticeKinds): every kind of this release's list but the
// three it adds and the two #4065 adds, merge-holds-changed and provider-errors.
// superviseOpsBeforeOwnerSteps is that release's list of supervise ops, which has
// no step_stop. Both are copies because that release's code is not in the tree, and so is its list
// of the kinds for the controller alone (controllerKindsBeforeOwnerSteps): it names unslotted and
// awaiting-person by their words, since that release reads both and this one no longer sends either.
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
		record.TriageNotice, record.SlotFreeNotice, record.TodoNotice, record.TickNotice, "unslotted",
		"awaiting-person", record.ParkedDecisionNotice, record.UpdateStalledNotice, record.ProviderErrorsNotice,
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
	t.Parallel()
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
					// As this release queues it: a fenced kind names its issue's run and phase entry (record.Fenced).
					row, err := record.NewOutboxRow(project+"-208", record.Fenced(q.payload, record.FirstFence), queuedAt)
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
	t.Parallel()
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
