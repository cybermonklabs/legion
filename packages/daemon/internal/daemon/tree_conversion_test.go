package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	planwords "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// conversionIssue is a record of the project CAPTURE as the release before left it: key stored in
// tree, at phase and status, run generation, its Dispatch status the same, holding its own gate
// where it is its own tree, as the migration of 20261122154644 marks every root of that release.
func conversionIssue(key, tree string, at phase.Phase, status string, generation uint64) record.Issue {
	return record.Issue{Key: key, Tree: tree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "CAPTURE",
		Title: key, Phase: at, Generation: generation, Status: status, DispatchStatus: status, Rank: key, HoldsGate: key == tree, LastDispatchSeq: 1}
}

// conversionSummary is key as Dispatch's listing serves it (boardSummary) at seq, in status, under
// parent ("" for none), carrying labels.
func conversionSummary(key, status string, seq int64, parent string, labels ...string) map[string]any {
	summary := boardSummary(key, status, seq, append([]string{}, labels...), key)
	if parent != "" {
		summary["parent"] = parent
	}
	return summary
}

// seedConversionClaim records the claim of role on issue as the release before left it: in tree,
// bound to that tree's epoch one, in state, running in the Sandbox pod pod ("" for no locator).
func seedConversionClaim(t *testing.T, pool *store.Pool, issue string, role claim.Role, tree, state, pod string) {
	t.Helper()
	token, err := claim.NewToken("capture", issue, role)
	if err != nil {
		t.Fatal(err)
	}
	var locator any
	if pod != "" {
		locator = fmt.Sprintf(`{"runtime":"sandbox","sandbox":{"podUid":%q}}`, pod)
	}
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by, tree_epoch, locator)
		values ($1, 'capture', $2, $3, $4, 1, $5, $6, $7, 0, 0, 0, 0, '', 1, $8::jsonb)`,
		string(token), tree, issue, string(role), "ses-"+string(token), "/sessions/"+string(token)+".jsonl", state, locator); err != nil {
		t.Fatalf("record the claim %s: %v", token, err)
	}
}

// queueReleaseBeforeRow writes an outbox row of kind for issue with payload as the release before
// queued it, due long after the test ends, so no run of the outbox takes it.
func queueReleaseBeforeRow(t *testing.T, pool *store.Pool, kind record.OutboxKind, issue, payload string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, $3::jsonb, 0, '2031-01-01T00:00:00Z', '') returning id`, string(kind), issue, payload).Scan(&id); err != nil {
		t.Fatalf("queue the release before's %s row of %s: %v", kind, issue, err)
	}
	return id
}

// storeConversion is a workflow runtime over pool that holds only what the conversion's steps
// that read the store need (convertStore): the store, the project and the clock.
func storeConversion(pool *store.Pool, project string, now time.Time) *workflowRuntime {
	return &workflowRuntime{pool: pool, records: record.NewStore(), dispatchProject: project, projectID: "capture",
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }}
}

// reparentSeedsOf is each re-parent seed the outbox holds, by issue.
func reparentSeedsOf(t *testing.T, pool *store.Pool) map[string][]record.ReparentSeed {
	t.Helper()
	seeds := map[string][]record.ReparentSeed{}
	for _, row := range rowsOfKind(t, pool, record.OutboxKindReparentSeed) {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode the re-parent seed of %s: %v", row.Issue, err)
		}
		seeds[row.Issue] = append(seeds[row.Issue], payload.(record.ReparentSeed))
	}
	return seeds
}

// rowsOfKind is every row of kind the outbox holds, in id order.
func rowsOfKind(t *testing.T, pool *store.Pool, kind record.OutboxKind) []record.OutboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox where kind = $1 order by id`, string(kind))
	if err != nil {
		t.Fatalf("read the %s rows: %v", kind, err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var r record.OutboxRow
		var k string
		err := row.Scan(&r.ID, &k, &r.Issue, &r.Payload)
		r.Kind = record.OutboxKind(k)
		return r, err
	})
	if err != nil {
		t.Fatalf("read the %s rows: %v", kind, err)
	}
	return found
}

// The first boot on this release tells R18's notice of each issue the release before ran as a root
// of its own whose Dispatch parent, in the copy the boot's listing fills, is in Legion's work (R21
// of dispatch://INFRA-3320, step 11 of the first boot's conversion, decision 21 of
// dispatch://INFRA-4730). CAPTURE-500 is recorded as its own tree under CAPTURE-300, a root at
// work, and the release before already told it under that parent (told_parent): after the boot it
// owes one re-parent seed naming CAPTURE-300 as its new parent and no old parent, and the seed's
// read tells its own architect and CAPTURE-300's one reparented notice each. CAPTURE-650 is
// recorded as its own tree under CAPTURE-700, which the daemon does not record and which is in
// Dispatch's backlog: nothing of it is told. A later listing, and a later event, that repeat
// CAPTURE-500's parent tell nobody again. The boot runs the conversion as the daemon does: its
// steps that read the store, then the boot's reconcile, which fills the copy from the listing,
// runs the steps that read it, records the conversion and makes the first admission pass.
func TestTheFirstBootTellsEachRootUnderAParentAtWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const parent, told, apart, outside = "CAPTURE-300", "CAPTURE-500", "CAPTURE-650", "CAPTURE-700"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, conversionIssue(parent, parent, phase.Implementing, "in_progress", 1))
	toldIssue := conversionIssue(told, told, phase.Implementing, "in_progress", 1)
	toldIssue.ToldParent = parent
	putOutboxIssue(t, pool, records, toldIssue)
	putOutboxIssue(t, pool, records, conversionIssue(apart, apart, phase.Implementing, "in_progress", 1))
	for _, key := range []string{parent, told, apart} {
		seedConversionClaim(t, pool, key, claim.RoleArchitect, key, "working", "")
	}
	board := &copyBoard{listing: []map[string]any{
		conversionSummary(parent, "in_progress", 3, ""),
		conversionSummary(told, "in_progress", 3, parent),
		conversionSummary(apart, "in_progress", 3, outside),
		conversionSummary(outside, "backlog", 3, ""),
	}}
	w := copyRuntime(t, pool, board, 3)
	w.consumers = bootClaimConsumers(t, pool)

	if err := w.convertStore(ctx); err != nil {
		t.Fatalf("the conversion's steps that read the store: %v", err)
	}
	if !w.converting {
		t.Fatal("the first boot on the release found its conversion finished, want it to convert")
	}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("the first boot's reconcile: %v", err)
	}

	seeds := reparentSeedsOf(t, pool)
	if got := slices.Sorted(maps.Keys(seeds)); !slices.Equal(got, []string{told}) || len(seeds[told]) != 1 {
		t.Fatalf("the first boot queued re-parent seeds of %v (%v), want one of %s alone", got, seeds, told)
	}
	seed := seeds[told][0]
	if seed.Parent != parent || seed.OldParent != "" || seed.Generation != 1 {
		t.Fatalf("the seed of %s reads %+v, want %s as the new parent, no old parent and run 1", told, seed, parent)
	}
	var mark string
	if err := pool.QueryRow(ctx, `select told_parent from issues where key = $1`, told).Scan(&mark); err != nil || mark != parent {
		t.Fatalf("the told parent of %s is %q (%v), want %s, the once-mark written as the seed is queued", told, mark, err, parent)
	}

	// The seed's executor reads the new parent from Dispatch and applies what it read; a parent
	// with no primary document opens no path (version 0).
	if _, err := intake.ApplyFact(ctx, pool, "outbox", "reparent-read:"+told, intake.ReparentRead{Issue: told, Seed: seed}, w.handlers...); err != nil {
		t.Fatalf("apply the seed's read: %v", err)
	}
	reparented := func() map[string]int {
		t.Helper()
		toldTo := map[string]int{}
		for _, row := range rowsOfKind(t, pool, record.OutboxKindNotice) {
			payload, err := record.DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("decode notice row %d: %v", row.ID, err)
			}
			if notice := payload.(record.Notice); notice.Kind == record.ReparentedNotice {
				toldTo[notice.To]++
			}
		}
		return toldTo
	}
	if got, want := reparented(), map[string]int{told: 1, parent: 1}; !maps.Equal(got, want) {
		t.Fatalf("the reparented notices tell %v, want %v: the issue's own architect and the one above it now, once each", got, want)
	}

	// A later boot's listing and a later event that repeat the parent tell nobody again.
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("the next boot's reconcile: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "repeat-parent", intake.DispatchIssue{Key: told, ResolvedRepo: "acme/widgets", Seq: 9,
		Type: "issue.updated", Status: "in_progress", Title: told, Parent: parent, Rank: told}, w.handlers...); err != nil {
		t.Fatalf("apply an event that repeats the parent of %s: %v", told, err)
	}
	if got := reparentSeedsOf(t, pool); len(got) != 1 || len(got[told]) != 1 {
		t.Fatalf("after a later listing and event under the same parent the seeds are %v, want still the one of %s", got, told)
	}
	if got, want := reparented(), map[string]int{told: 1, parent: 1}; !maps.Equal(got, want) {
		t.Fatalf("after a later listing and event the reparented notices tell %v, want still %v", got, want)
	}
	var conversion record.TreeConversion
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		conversion, err = record.ReadTreeConversion(ctx, tx, "CAPTURE")
		return err
	}); err != nil || conversion.Step != convertRecord || !conversion.Converted() {
		t.Fatalf("the conversion of CAPTURE reads %+v (%v), want its last step recorded with the time it ended", conversion, err)
	}
}

// The conversion's first step gives every row of a fenced kind that the release before queued
// without its fence the parts its kind carries (record.FencedKinds, step 1 of decision 21 of
// dispatch://INFRA-4730), in SQL and without decoding it, so every decoder reads the row with its
// fence afterwards. CAPTURE-208 runs at generation 2 and phase entry 4 (its implementer's phases
// row at entry 3), its implementer in the Sandbox pod pod-uid-208. For each fenced kind a row is
// queued as the release before queued it (this release's payload with none of the fence, which
// encodes as that release's shape): after the conversion it decodes with the run and the entry its
// kind carries, the pod for a kind that acts on one, and what the row named itself kept, and this
// release would queue it as it stands (record.NewOutboxRow). A kind added to the fenced kinds fails
// here until it has a case.
func TestTheFirstBootFencesEachRowTheReleaseBeforeQueued(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const key, pod = "CAPTURE-208", "pod-uid-208"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, conversionIssue(key, key, phase.Implementing, "in_progress", 2))
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Entry: 3, EntryRaisedAt: time.Now()})
	}); err != nil {
		t.Fatalf("raise the implementer's entry of %s: %v", key, err)
	}
	seedConversionClaim(t, pool, key, claim.RoleImplementer, key, "working", pod)
	queued := []record.OutboxPayload{
		record.OrdinaryStatusWrite("testing", "in_progress", ""),
		record.MessagePost{Body: "The checks of the head settled."},
		record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing},
		record.ControllerNotice{Kind: record.TickNotice},
		record.GateSeed{ArtifactID: "artifact-208", Step: record.DesignStepSpec, Version: 1},
		record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: "artifact-208", BasisArtifactID: "artifact-200", BasisVersion: 2,
			Ask: "ask-1", Generation: 1, RegisteredAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)},
		record.IssueSuspend{Tree: key, Generation: 2, TreeGeneration: 2},
		record.IssueWorkspaceCheck{Tree: key, Generation: 2, TreeGeneration: 2, Role: claim.RoleImplementer},
		record.IssueRelease{Tree: key, Generation: 2, TreeGeneration: 2},
		record.IssueBranch{},
		record.CheckWakeDue{Look: 1, Reason: "the agent's own wake"},
		record.CheckLook{Look: 1},
		record.CheckBound{},
		record.RoleMessage{Role: "legion-capture-capture-208-implementer", Body: "The checks of the head settled."},
		record.PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.AwaitingChildren, Reason: "CAPTURE-209 (todo) is open"},
		record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: 1,
			Prompt: "Say what the round missed."},
		record.UpdateAsk{Role: claim.RoleImplementer, Round: "round-1", Text: "Commit and push, then stop."},
		record.MergeForwardMessage{Step: record.MergeForwardAsk, Role: claim.RoleImplementer, Head: "abc123", Base: "main", Phase: phase.Merging,
			PullRequest: 42, Why: record.ConflictMergeNext},
		record.MergeQueueRemoval{Number: 42, Head: "abc123"},
		record.ClaimRelease{},
		record.SuperviseRequest{Op: "suspend", Tree: key, Role: claim.RoleImplementer},
		record.SuperviseRequest{Op: "step_stop", Tree: key, Role: claim.RoleImplementer, Reason: "task a of " + key + " was stopped by its owner"},
		record.SuperviseRequest{Op: "issue_close", Tree: key, Role: claim.RoleImplementer},
		record.SuperviseRequest{Op: "release", Tree: key, Role: claim.RoleImplementer},
	}
	// The release before never queued a retention_end, which this release adds.
	cased := map[record.OutboxKind]bool{record.OutboxKindRetentionEnd: true}
	ids := make([]int64, len(queued))
	for i, payload := range queued {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = queueReleaseBeforeRow(t, pool, payload.OutboxKind(), key, string(encoded))
		cased[payload.OutboxKind()] = true
	}
	for _, keys := range record.FencedKinds() {
		if !cased[keys.Kind] {
			t.Errorf("%s is fenced, and no case queues a row of it as the release before did", keys.Kind)
		}
		if keys.Kind == record.OutboxKindSupervise {
			for _, op := range keys.Ops {
				if !slices.ContainsFunc(queued, func(p record.OutboxPayload) bool { s, ok := p.(record.SuperviseRequest); return ok && s.Op == op }) {
					t.Errorf("a supervise row of op %s is fenced, and no case queues one", op)
				}
			}
		}
	}

	w := storeConversion(pool, "CAPTURE", time.Now())
	if err := w.convertStore(ctx); err != nil {
		t.Fatalf("the conversion's steps that read the store: %v", err)
	}

	stamps := map[record.OutboxKind]record.FenceKeys{}
	for _, keys := range record.FencedKinds() {
		stamps[keys.Kind] = keys
	}
	for i, payload := range queued {
		name := string(payload.OutboxKind())
		if s, ok := payload.(record.SuperviseRequest); ok {
			name += " " + string(s.Op)
		}
		t.Run(name, func(t *testing.T) {
			var stamped []byte
			if err := pool.QueryRow(ctx, `select payload from outbox where id = $1`, ids[i]).Scan(&stamped); err != nil {
				t.Fatalf("read the row: %v", err)
			}
			decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: ids[i], Kind: payload.OutboxKind(), Issue: key, Payload: stamped})
			if err != nil {
				t.Fatalf("decode the stamped row %s: %v", stamped, err)
			}
			got, fenced := record.FenceOf(decoded)
			own, _ := record.FenceOf(payload)
			keys := stamps[payload.OutboxKind()]
			want := record.Fence{Generation: cmp.Or(own.Generation, 2)}
			if keys.Entry {
				want.Entry = 4
			}
			if keys.PodUID {
				want.PodUID = pod
			}
			if !fenced || got != want {
				t.Fatalf("the row reads fence %+v (fenced %t) after the conversion, want %+v: %s", got, fenced, want, stamped)
			}
			if _, err := record.NewOutboxRow(key, decoded, time.Now()); err != nil {
				t.Fatalf("this release would not queue the stamped row as it stands: %v", err)
			}
		})
	}
}

// A root a hand-over carried in before this release keeps, on its own record, the generation it
// carried and whether its old root's gate came with it, and its architect's start and catch-up read
// them there; a linger deadline still ahead at the conversion is the retention deadline of each
// issue it covered, on its own record, with the row that ends it then (k48 of
// dispatch://INFRA-4730's Verification; steps 2 and 3 of decision 21). The store is migrated to the
// schema of the release before (every migration but this release's two), seeded as that release
// leaves it, migrated, and converted: CAPTURE-10 a root a hand-over made at generation 2 with its
// old root's gate; CAPTURE-20 a root whose tree lingers until a deadline ahead, CAPTURE-21 a member
// of its tree out of Legion's work and CAPTURE-22 one in it.
func TestACarriedRunGateAndRetentionLiveOnTheIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := storeWithholding(t, "legion_carried_run", 20261122154644, 20261122154645)
	deadline := time.Date(2031, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, seed := range []struct {
		key, tree, at, status string
		generation            int
		lingers               bool
	}{
		{key: "CAPTURE-10", tree: "CAPTURE-10", at: "planning", status: "in_progress", generation: 2},
		{key: "CAPTURE-20", tree: "CAPTURE-20", at: "done", status: "done", generation: 1, lingers: true},
		{key: "CAPTURE-21", tree: "CAPTURE-20", at: "done", status: "done", generation: 1},
		{key: "CAPTURE-22", tree: "CAPTURE-20", at: "implementing", status: "in_progress", generation: 1},
	} {
		var lingerUntil *time.Time
		if seed.lingers {
			lingerUntil = &deadline
		}
		if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			repo, default_branch, github_repo, linger_until) values ($1, $2, 'CAPTURE', $1, $3, $4, $5, $1, 1, 'acme/widgets', 'main', 'acme/widgets', $6)`,
			seed.key, seed.tree, seed.at, seed.generation, seed.status, lingerUntil); err != nil {
			t.Fatalf("record %s on the release before's schema: %v", seed.key, err)
		}
	}
	if _, err := st.Pool().Exec(ctx, `insert into handed_over_roots (issue, generation, gate_carried) values ('CAPTURE-10', 2, true)`); err != nil {
		t.Fatalf("record the hand-over that made CAPTURE-10: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate to this release: %v", err)
	}

	w := storeConversion(st.Pool(), "CAPTURE", time.Date(2031, 5, 1, 0, 0, 0, 0, time.UTC))
	if err := w.convertStore(ctx); err != nil {
		t.Fatalf("the conversion's steps that read the store: %v", err)
	}

	records := record.NewStore()
	issues := map[string]*record.Issue{}
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, key := range []string{"CAPTURE-10", "CAPTURE-20", "CAPTURE-21", "CAPTURE-22"} {
			issue, err := records.Issue(ctx, tx, key)
			if err != nil {
				return err
			}
			issues[key] = issue
		}
		return nil
	}); err != nil {
		t.Fatalf("read the records: %v", err)
	}
	if root := issues["CAPTURE-10"]; root.CarriedGeneration != 2 || !root.CarriedGate {
		t.Fatalf("CAPTURE-10 carries generation %d and gate %t on its record, want 2 and true from the hand-over that made it", root.CarriedGeneration, root.CarriedGate)
	}
	for key, want := range map[string]*time.Time{"CAPTURE-20": &deadline, "CAPTURE-21": &deadline, "CAPTURE-22": nil} {
		got := issues[key].RetainedUntil
		if (got == nil) != (want == nil) || got != nil && !got.Equal(*want) {
			t.Fatalf("%s is retained until %v, want %v: the linger's deadline covers its root and each issue of its tree out of Legion's work", key, got, want)
		}
	}
	if lingers := issues["CAPTURE-20"].LingerUntil; lingers == nil || !lingers.Equal(deadline) {
		t.Fatalf("CAPTURE-20's linger_until reads %v after the conversion, want %s left for the release before", lingers, deadline)
	}
	ends := map[string]time.Time{}
	rows, err := st.Pool().Query(ctx, `select issue, next_at, payload from outbox where kind = 'retention_end' order by issue`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var issue string
		var at time.Time
		var payload []byte
		if err := rows.Scan(&issue, &at, &payload); err != nil {
			t.Fatal(err)
		}
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKindRetentionEnd, Issue: issue, Payload: payload})
		if err != nil {
			t.Fatalf("decode the retention end of %s: %v", issue, err)
		}
		if fence, _ := record.FenceOf(decoded); fence.Generation != 1 || fence.Entry != 1 {
			t.Fatalf("the retention end of %s is fenced at %+v, want run 1 and entry 1, where the issue stands", issue, fence)
		}
		ends[issue] = at.UTC()
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the retention ends: %v", err)
	}
	if want := map[string]time.Time{"CAPTURE-20": deadline, "CAPTURE-21": deadline}; !maps.Equal(ends, want) {
		t.Fatalf("the retention ends are %v, want %v", ends, want)
	}

	steps := config.DesignGateRootIssues.Steps()
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		start, err := workflow.ArchitectStartOf(ctx, tx, records, steps, *issues["CAPTURE-10"])
		if err != nil {
			return err
		}
		if !start.Carried || !start.GateCarried {
			return fmt.Errorf("the architect's start of CAPTURE-10 reads carried %t and gate %t, want both from its own record", start.Carried, start.GateCarried)
		}
		return workflow.QueueCatchUp(ctx, tx, records, steps, *issues["CAPTURE-10"], 1, "", time.Now(), time.Time{})
	}); err != nil {
		t.Fatal(err)
	}
	var catchUp *record.CatchUp
	for _, row := range rowsOfKind(t, st.Pool(), record.OutboxKindNotice) {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatal(err)
		}
		if notice := payload.(record.Notice); notice.Kind == "catch-up" {
			catchUp = notice.CatchUp
		}
	}
	if catchUp == nil || !catchUp.HandedOver || !catchUp.Carried {
		t.Fatalf("the catch-up of CAPTURE-10's architect reads %+v, want it handed over with its gate carried", catchUp)
	}
}

// The upgrade stamps no claim's crossings (decision 24 of dispatch://INFRA-4730): the migration
// adds published_since and unpublished_since null with no backfill, and no step of the first boot's
// conversion writes either, so every claim the release before left, in each state and a working one
// among them, reads both null after it, and the repair judges such a claim against the time the
// conversion ended (record.RoleHolderAt.PublishedAt). The store is migrated to the schema of the
// release before (every migration but this release's two), seeded with the claim of a root of its
// own in each state, migrated, and converted as the daemon boots, through its last step. Negative
// control, run when this was written, failing this test and then reverted: with the conversion's
// last step stamping published_since on every claim in a published state, the claims registered,
// ready, working and idle read it set.
func TestTheConversionLeavesEveryClaimsCrossingsNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := storeWithholding(t, "legion_claim_crossings", 20261122154644, 20261122154645)
	states := []string{"queued", "launch_uncertain", "launching", "shim_connected", "registered", "ready", "working", "idle",
		"suspended", "failed", "retired"}
	board := &copyBoard{}
	for i, state := range states {
		key := fmt.Sprintf("CAPTURE-%d", i+1)
		if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			repo, default_branch, github_repo) values ($1, $1, 'CAPTURE', $1, 'implementing', 1, 'in_progress', $1, 1, 'acme/widgets', 'main', 'acme/widgets')`,
			key); err != nil {
			t.Fatalf("record %s on the release before's schema: %v", key, err)
		}
		if _, err := st.Pool().Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority) values ('capture', $1, 1, 'workflow')`, key); err != nil {
			t.Fatalf("open the tree lifecycle of %s: %v", key, err)
		}
		seedConversionClaim(t, st.Pool(), key, claim.RoleImplementer, key, state, "")
		board.listing = append(board.listing, conversionSummary(key, "in_progress", 3, ""))
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate to this release: %v", err)
	}
	w := copyRuntime(t, st.Pool(), board, 3)
	w.projectID = "capture"
	w.now = func() time.Time { return time.Date(2031, 5, 1, 0, 0, 0, 0, time.UTC) }
	if err := bootConversionStoppedAt(ctx, w, convertRecord); !errors.Is(err, errStoppedAfterStep) {
		t.Fatalf("the boot that converts the store ended with %v, want it to reach the last step", err)
	}
	if got := conversionDump(t, st.Pool())["tree_conversions"]; !slices.Equal(got, []string{"CAPTURE 12 t"}) {
		t.Fatalf("the conversion is recorded as %q, want its last step and when it ended", got)
	}
	rows, err := st.Pool().Query(ctx, `select concat_ws(' ', state, published_since is null, unpublished_since is null) from claims
		where project = 'capture' order by state`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, state := range slices.Sorted(slices.Values(states)) {
		want = append(want, state+" t t")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the claims read %q after the upgrade (state, published_since null, unpublished_since null), want %q: no crossing stamped", got, want)
	}
}

// A hand-over the release before had in flight in a tree whose cleanup it reserved is decided by
// the listing, not by the reservation (R22 of dispatch://INFRA-3320, steps 5 and 9 of the first
// boot's conversion, decision 21 of dispatch://INFRA-4730). CAPTURE-60 lingers at generation 2
// with its tree cleanup reserved; CAPTURE-61 and CAPTURE-62, members in implementing whose
// implementers still work, were handed over, and CAPTURE-63 is a done member whose suspended
// implementer the cleanup waits for. The listing shows CAPTURE-61's write landed (todo with the
// label) and CAPTURE-62's not. After the first boot's conversion and its first admission pass,
// CAPTURE-61 carries on as a root of its own: its lifecycle is open, its implementer is bound to
// it, nothing closes or cleans it up, and it takes one of the free slots. CAPTURE-62, whose
// hand-over is cancelled, and CAPTURE-63 are reserved as the tree's cleanup covered them, with what
// finishes each queued. Negative control, run when this was written, failing this test and then
// reverted: with step 5 reserving every issue the tree cleanup covers, its hand-overs too,
// CAPTURE-61's cleanup is reserved, which no row of its new run ever confirms, and admission leaves
// it waiting for that cleanup.
func TestALandedHandOverUnderAReservedCleanupStartsAsARootOfItsOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const root, landed, cancelled, done = "CAPTURE-60", "CAPTURE-61", "CAPTURE-62", "CAPTURE-63"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	lingering := conversionIssue(root, root, phase.Done, "done", 2)
	deadline := time.Now().Add(48 * time.Hour).UTC()
	lingering.LingerUntil = &deadline
	for _, issue := range []record.Issue{
		lingering,
		conversionIssue(landed, root, phase.Implementing, "in_progress", 1),
		conversionIssue(cancelled, root, phase.Implementing, "in_progress", 1),
		conversionIssue(done, root, phase.Done, "done", 1),
	} {
		putOutboxIssue(t, pool, records, issue)
	}
	seedConversionClaim(t, pool, landed, claim.RoleImplementer, root, "working", "")
	seedConversionClaim(t, pool, cancelled, claim.RoleImplementer, root, "working", "")
	seedConversionClaim(t, pool, done, claim.RoleImplementer, root, "suspended", "")
	for _, statement := range []string{
		`insert into tree_lifecycles (project, tree, epoch, authority, cleanup_started) values ('capture', 'CAPTURE-60', 1, 'workflow', true)`,
		`insert into handovers (issue, tree, linger_generation) values ('CAPTURE-61', 'CAPTURE-60', 2), ('CAPTURE-62', 'CAPTURE-60', 2)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed what the release before left: %v", err)
		}
	}
	board := &copyBoard{listing: []map[string]any{
		conversionSummary(root, "done", 3, ""),
		conversionSummary(landed, "todo", 3, root, "legion"),
		conversionSummary(cancelled, "in_progress", 3, root),
		conversionSummary(done, "done", 3, root),
	}}
	w := copyRuntime(t, pool, board, 3)
	w.projectID = "capture"
	w.consumers = bootClaimConsumers(t, pool)
	if err := w.convertStore(ctx); err != nil {
		t.Fatalf("the conversion's steps that read the store: %v", err)
	}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("the first boot's reconcile: %v", err)
	}

	lifecycles := map[string]string{}
	rows, err := pool.Query(ctx, `select issue, concat_ws(' ', epoch, authority, cleanup_started) from issue_lifecycles where project = 'capture'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var issue, lifecycle string
		if err := rows.Scan(&issue, &lifecycle); err != nil {
			t.Fatal(err)
		}
		lifecycles[issue] = lifecycle
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the issue lifecycles: %v", err)
	}
	want := map[string]string{landed: "1 workflow f", cancelled: "1 workflow t", done: "1 workflow t"}
	if !maps.Equal(lifecycles, want) {
		t.Fatalf("the issue lifecycles read %v (epoch, authority, cleanup reserved), want %v: the landed hand-over's open, the others' reserved", lifecycles, want)
	}
	var bound int
	if err := pool.QueryRow(ctx, `select issue_epoch from claims where issue = $1`, landed).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("the implementer of %s is bound to epoch %d (%v), want 1, its open lifecycle's", landed, bound, err)
	}
	effects := map[string][]string{}
	for _, kind := range []record.OutboxKind{record.OutboxKindSupervise, record.OutboxKindIssueSuspend} {
		for _, row := range rowsOfKind(t, pool, kind) {
			payload, err := record.DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("decode outbox row %d: %v", row.ID, err)
			}
			effect := string(kind)
			if request, ok := payload.(record.SuperviseRequest); ok {
				if request.Op != "issue_close" {
					continue
				}
				effect = string(request.Op) + " " + string(request.Role)
			}
			effects[row.Issue] = append(effects[row.Issue], effect)
		}
	}
	wantEffects := map[string][]string{cancelled: {"issue_close implementer", "issue_suspend"}, done: {"issue_close implementer", "issue_suspend"}}
	if !maps.EqualFunc(effects, wantEffects, slices.Equal[[]string]) {
		t.Fatalf("the conversion queued %v, want %v: what finishes each reserved cleanup, and nothing for the landed hand-over", effects, wantEffects)
	}
	var slotted bool
	if err := pool.QueryRow(ctx, `select exists (select 1 from slots where issue = $1)`, landed).Scan(&slotted); err != nil || !slotted {
		t.Fatalf("%s holds a slot: %t (%v), want it started in one of the free slots as a root of its own", landed, slotted, err)
	}
}

// A hand-over whose write landed carries its run's plan into its new run (step 9 of the first
// boot's conversion; main's #4611: an owner's plan picks which steps run). The release before
// carried such a run into a root of its own at a new generation, in the phase it ran in, so that
// run never leaves admitted, where a run's plan is written otherwise. finishHandOver copies the
// plan of the run before to the new generation, each task where it stood (record.Store's CarryPlan
// with carried), and then writes the new run's plan node under that run's own id: the default
// steps where the run before walked them, and its owner's plan, with the document and the version
// Legion took, where its owner gave one. CAPTURE-80 lingers at generation 2; CAPTURE-81, a member
// of its tree at its acceptance, was handed over, and the listing shows that the write landed. This
// is main's TestACarriedRootsNewRunWritesItsPlan (admit's plan_carry_test.go) on the one path of
// this release that finishes a hand-over, the conversion.
func TestALandedHandOverCarriesItsPlanIntoItsNewRun(t *testing.T) {
	t.Parallel()
	const root, member = "CAPTURE-80", "CAPTURE-81"
	for _, tc := range []struct {
		name string
		// owners says whether the run before walked its owner's plan, `implementer reviewer
		// acceptance:owner`, standing at its acceptance, rather than the default steps.
		owners bool
		want   record.PlanRecord
	}{
		{name: "the default steps", want: record.PlanRecord{ID: workflow.PlanID(member, 2), Generation: 2, Source: record.PlanSourceDefault,
			Tasks: []record.PlanRecordTask{{ID: "default", Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}}}}},
		{name: "the owner's plan", owners: true, want: record.PlanRecord{ID: workflow.PlanID(member, 2), Generation: 2, Source: record.PlanSourceOwner,
			PlanDocument: &record.PlanDocument{ArtifactID: "plan-artifact", Version: 2},
			Tasks:        []record.PlanRecordTask{{ID: "a", Steps: []string{"implementer", "reviewer", "acceptance:owner"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			lingering := conversionIssue(root, root, phase.Done, "done", 2)
			deadline := time.Now().Add(48 * time.Hour).UTC()
			lingering.LingerUntil = &deadline
			for _, issue := range []record.Issue{lingering, conversionIssue(member, root, phase.Accepting, "needs_review", 1)} {
				putOutboxIssue(t, pool, records, issue)
			}
			if _, err := pool.Exec(ctx, `insert into handovers (issue, tree, linger_generation) values ('CAPTURE-81', 'CAPTURE-80', 2)`); err != nil {
				t.Fatalf("seed the hand-over the release before left: %v", err)
			}
			if tc.owners {
				seedOwnersPlanAtAcceptance(t, pool, member, root)
			}
			board := &copyBoard{listing: []map[string]any{
				conversionSummary(root, "done", 3, ""),
				conversionSummary(member, "todo", 3, root, "legion"),
			}}
			w := copyRuntime(t, pool, board, 3)
			w.projectID = "capture"
			if err := bootConversionStoppedAt(ctx, w, convertRecord); !errors.Is(err, errStoppedAfterStep) {
				t.Fatalf("the first boot's conversion ended with %v, want it to reach its last step", err)
			}

			var before, after *record.StoredPlan
			var carried record.Issue
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				issue, err := records.Issue(ctx, tx, member)
				if err != nil || issue == nil {
					return fmt.Errorf("read %s: %v (recorded %t)", member, err, issue != nil)
				}
				carried = *issue
				if before, err = records.Plan(ctx, tx, member, 1); err != nil {
					return err
				}
				after, err = records.Plan(ctx, tx, member, 2)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if carried.Generation != 2 || carried.Phase != phase.Accepting || !carried.TakenBack {
				t.Fatalf("%s after the conversion stands at generation %d in %s, taken back %t; want it carrying its run into generation 2 in accepting: the case's premise",
					member, carried.Generation, carried.Phase, carried.TakenBack)
			}
			if want := planAtGeneration(before, 2); !reflect.DeepEqual(after, want) {
				t.Fatalf("the plan of %s at generation 2 = %#v, want the plan of generation 1 with each task where it stood: %#v", member, after, want)
			}
			var plans []record.PlanRecord
			for _, row := range rowsOfKind(t, pool, record.OutboxKindPlanRecord) {
				payload, err := record.DecodeOutboxPayload(row)
				if err != nil {
					t.Fatalf("decode the plan record row %d: %v", row.ID, err)
				}
				plans = append(plans, payload.(record.PlanRecord))
			}
			if len(plans) != 1 || !reflect.DeepEqual(plans[0], tc.want) {
				t.Fatalf("the conversion queued the plans %+v, want the one plan of the new run %+v", plans, tc.want)
			}
		})
	}
}

// A hand-over whose write landed carries the member's open pull request into its new run with the
// CI settlement fence it held (step 9 of the first boot's conversion, finishHandOver). The
// fence's generation is the listener's generation of the settlement that stands, not the issue's,
// so a settlement of the head from before the carry, delivered late or replayed from a message
// the intake gave up on, is as stale after the carry as it was before: the record keeps the newer
// settlement's generation and snapshot, and the later status of its commit. CAPTURE-80 lingers at
// generation 2; CAPTURE-81, a member of its tree at its acceptance with pull request #42 open at
// head-42, was handed over, and the listing shows that the write landed. This is main's
// TestAnEarlierSettlementOfACarriedPullRequestsHeadChangesNothing (admit's
// late_settlement_carry_test.go, #4590) on the one path of this release that carries a pull
// request into a new run, the conversion.
func TestALandedHandOverKeepsItsPullRequestsSettlementFence(t *testing.T) {
	t.Parallel()
	const root, member, head = "CAPTURE-80", "CAPTURE-81", "head-42"
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	lingering := conversionIssue(root, root, phase.Done, "done", 2)
	deadline := time.Now().Add(48 * time.Hour).UTC()
	lingering.LingerUntil = &deadline
	for _, issue := range []record.Issue{lingering, conversionIssue(member, root, phase.Accepting, "needs_review", 1)} {
		putOutboxIssue(t, pool, records, issue)
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutPullRequest(ctx, tx, record.PullRequest{Issue: member, Repo: "acme/widgets", Number: 42, Branch: "legion/" + member,
			HeadSHA: head, State: record.PullRequestOpen, Failing: []string{}, Cancelled: []string{}})
	}); err != nil {
		t.Fatalf("seed the pull request of %s: %v", member, err)
	}
	if _, err := pool.Exec(ctx, `insert into handovers (issue, tree, linger_generation) values ($1, $2, 2)`, member, root); err != nil {
		t.Fatalf("seed the hand-over the release before left: %v", err)
	}
	board := &copyBoard{listing: []map[string]any{
		conversionSummary(root, "done", 3, ""),
		conversionSummary(member, "todo", 3, root, "legion"),
	}}
	w := copyRuntime(t, pool, board, 3)
	w.projectID = "capture"
	at := func(minute int) time.Time { return time.Date(2026, 9, 23, 11, minute, 0, 0, time.UTC) }
	settlement := func(generation int64, state record.CommitStatusState, minute int) intake.PullRequestChecks {
		return intake.PullRequestChecks{Repo: "acme/widgets", Number: 42, HeadSHA: head, CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}},
			Generation: generation, Snapshot: fmt.Sprintf("snapshot-%d", generation), Failing: []string{}, Cancelled: []string{},
			Statuses: []record.CommitStatus{{Context: "deploy", State: state, CreatedAt: at(minute)}}}
	}
	settle := func(eventID string, fact intake.PullRequestChecks) record.PullRequest {
		t.Helper()
		if _, err := intake.ApplyFact(ctx, pool, "github", eventID, fact, w.engine); err != nil {
			t.Fatalf("ApplyFact %s: %v", eventID, err)
		}
		var pr *record.PullRequest
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			pr, err = records.PullRequest(ctx, tx, member)
			return err
		}); err != nil || pr == nil {
			t.Fatalf("read the pull request of %s after %s: %v, %v", member, eventID, pr, err)
		}
		return *pr
	}
	newer, earlier := settlement(40, record.CommitStatusSuccess, 2), settlement(30, record.CommitStatusFailure, 1)

	if held := settle("newer", newer); held.Generation != 40 || held.Snapshot != newer.Snapshot {
		t.Fatalf("the newer settlement left generation %d, snapshot %q; want 40, %q", held.Generation, held.Snapshot, newer.Snapshot)
	}
	if got := settle("earlier-before-the-carry", earlier); got.Generation != 40 || got.Snapshot != newer.Snapshot {
		t.Fatalf("before the carry the earlier settlement left generation %d, snapshot %q; want it stale: 40, %q", got.Generation, got.Snapshot, newer.Snapshot)
	}

	if err := bootConversionStoppedAt(ctx, w, convertRecord); !errors.Is(err, errStoppedAfterStep) {
		t.Fatalf("the first boot's conversion ended with %v, want it to reach its last step", err)
	}
	var carried *record.Issue
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		carried, err = records.Issue(ctx, tx, member)
		return err
	}); err != nil || carried == nil || carried.Generation != 2 || !carried.TakenBack {
		t.Fatalf("%s after the conversion = %+v (%v); want it carrying its run into generation 2: the case's premise", member, carried, err)
	}

	got := settle("earlier-after-the-carry", earlier)
	if got.Generation != 40 || got.Snapshot != newer.Snapshot {
		t.Errorf("after the carry the earlier settlement left generation %d, snapshot %q; want it stale as before the carry: 40, %q",
			got.Generation, got.Snapshot, newer.Snapshot)
	}
	for _, status := range got.Statuses {
		if status.Context == "deploy" && status.State != record.CommitStatusSuccess {
			t.Errorf("after the carry the earlier settlement put deploy back to %s; want the later success", status.State)
		}
	}
}

// seedOwnersPlanAtAcceptance writes the plan the architect of root gave for key's run at
// generation 1: version 2 of plan-artifact, one task a whose steps are `implementer reviewer
// acceptance:owner`, running at its acceptance, step 3, where an issue in accepting stands.
func seedOwnersPlanAtAcceptance(t *testing.T, pool *store.Pool, key, root string) {
	t.Helper()
	givenBy, err := claim.NewToken("capture", root, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	v := record.PlanVersion{Issue: key, Generation: 1, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: givenBy, GivenAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	task := record.PlanTask{Issue: key, Generation: 1, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", State: record.TaskWaiting}
	roles := planwords.BuiltinRoles()
	var steps []record.PlanStep
	for i, step := range []planwords.Step{{Role: "implementer"}, {Role: "reviewer"}, {Role: "acceptance", Key: "owner"}} {
		steps = append(steps, record.PlanStep{Issue: key, Generation: 1, Task: "a", Position: i + 1, Role: step.Role, AgentKey: step.Key,
			Type: roles[step.Role].Type})
	}
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
			return err
		}
		return records.MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 3}, record.TaskRunning)
	}); err != nil {
		t.Fatalf("put the owner's plan of %s at its acceptance: %v", key, err)
	}
}

// planAtGeneration is stored, a plan of one generation as the store reads it, at generation, as
// a carry that keeps each task where it stood writes it; nil for nil.
func planAtGeneration(stored *record.StoredPlan, generation uint64) *record.StoredPlan {
	if stored == nil {
		return nil
	}
	carried := &record.StoredPlan{Version: stored.Version, Steps: map[string][]record.PlanStep{}}
	carried.Version.Generation = generation
	for _, task := range stored.Tasks {
		task.Generation = generation
		carried.Tasks = append(carried.Tasks, task)
	}
	for task, steps := range stored.Steps {
		for _, step := range steps {
			step.Generation = generation
			carried.Steps[task] = append(carried.Steps[task], step)
		}
	}
	return carried
}

// The first boot's conversion marks each claim whose issue holds its own gate before supervision
// reads a claim (step 7, markClaimsOfGateHolders), and the start the first admission pass queues
// marks the architect of a root that a landed hand-over made. CAPTURE-70 is a root of its own whose
// record the release before wrote while a rollback held, so it holds the mark's default, false,
// and its architect is the operator's claim, written by that release with no mark and no launch
// tree, which the workflow never starts. After the steps that read the store, that claim holds the
// mark in the claims the boot's supervision reads (openSupervision), and in the machine the boot's
// restore builds from them (supervisor.restore). CAPTURE-61 is a member of CAPTURE-60's tree whose
// hand-over landed: step 9 makes it a root that holds its own gate, the first admission pass queues
// its architect's start (workflow.Engine.StartArchitect, which admit's promotion of a queued root
// calls), and that start, run by the outbox, makes the architect's claim with the mark it reads on
// the Dispatch chain (the outbox's start, launchOf). Negative control, run when this was written,
// failing this test and then reverted: with step 7 left out of the steps that read the store, the
// operator's claim reads unmarked in the store and in its machine.
func TestTheConversionMarksAnOperatorsRootClaimBeforeSupervisionReadsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const root, landed, operated = "CAPTURE-60", "CAPTURE-61", "CAPTURE-70"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	lingering := conversionIssue(root, root, phase.Done, "done", 2)
	deadline := time.Now().Add(48 * time.Hour).UTC()
	lingering.LingerUntil = &deadline
	// The release before records a root with its key as its tree and the mark at its default.
	rolledBack := conversionIssue(operated, operated, phase.Implementing, "in_progress", 1)
	rolledBack.HoldsGate = false
	for _, issue := range []record.Issue{lingering, conversionIssue(landed, root, phase.Implementing, "in_progress", 1), rolledBack} {
		putOutboxIssue(t, pool, records, issue)
	}
	seedConversionClaim(t, pool, landed, claim.RoleImplementer, root, "working", "")
	seedConversionClaim(t, pool, operated, claim.RoleArchitect, operated, "idle", "")
	for _, statement := range []string{
		`insert into tree_lifecycles (project, tree, epoch, authority, cleanup_started) values ('capture', 'CAPTURE-60', 1, 'workflow', true)`,
		`insert into tree_lifecycles (project, tree, epoch, authority) values ('capture', 'CAPTURE-70', 1, 'operator')`,
		`insert into handovers (issue, tree, linger_generation) values ('CAPTURE-61', 'CAPTURE-60', 2)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed what the release before left: %v", err)
		}
	}
	operator, err := claim.NewToken("capture", operated, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	var marked bool
	if err := pool.QueryRow(ctx, `select holds_gate from claims where token = $1`, string(operator)).Scan(&marked); err != nil || marked {
		t.Fatalf("the operator's claim %s holds the mark %t (%v) before the conversion, want false: the fixture is not a claim the release before wrote", operator, marked, err)
	}
	board := &copyBoard{listing: []map[string]any{
		conversionSummary(root, "done", 3, ""),
		conversionSummary(landed, "todo", 3, root, "legion"),
		conversionSummary(operated, "in_progress", 3, ""),
	}}
	w := copyRuntime(t, pool, board, 3)
	w.projectID = "capture"
	w.consumers = bootClaimConsumers(t, pool)
	if err := w.convertStore(ctx); err != nil {
		t.Fatalf("the conversion's steps that read the store: %v", err)
	}

	// The claims as the boot's supervision reads them, before it builds any machine.
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	read, err := st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims as supervision does at boot: %v", err)
	}
	read = ofProject(read, "capture")
	stored := slices.IndexFunc(read, func(c supervise.Claim) bool { return c.Token == operator })
	if stored < 0 {
		t.Fatalf("the claims supervision reads at boot hold no %s", operator)
	}
	if !read[stored].HoldsGate {
		t.Fatalf("the claims supervision reads at boot hold %s on %s in the tree of %s with no mark, want it holding its gate: the operator's claim on a root is its tree's root",
			operator, read[stored].Issue, read[stored].Tree)
	}
	sup, _ := newOutboxSupervisor(t, "capture", t.TempDir())
	if _, err := sup.restore(ctx, read); err != nil {
		t.Fatalf("restore the claims as the boot does: %v", err)
	}
	machine, found := sup.Machine(operator)
	if !found || !machine.Claim().HoldsGate {
		t.Fatalf("the boot's restore built %s: %t, holding its gate: %t; want a machine of the claim that holds its gate", operator, found, found && machine.Claim().HoldsGate)
	}

	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("the first boot's reconcile: %v", err)
	}
	var start record.OutboxRow
	if err := pool.QueryRow(ctx, `select id, payload from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'architect'`, landed).Scan(&start.ID, &start.Payload); err != nil {
		t.Fatalf("read the architect's start the first admission pass queued for %s: %v", landed, err)
	}
	start.Issue, start.Kind, start.NextAt = landed, record.OutboxKindSupervise, time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "CAPTURE", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "capture", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + landed}, nil
		},
	}
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("run the start of %s's architect: %v", landed, err)
	}
	architect, err := claim.NewToken("capture", landed, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	started, found := sup.Machine(architect)
	if !found || !started.Claim().HoldsGate {
		t.Fatalf("the start of %s's architect made a claim: %t, holding its gate: %t; want the claim of a root that holds its own gate, marked by its start",
			landed, found, found && started.Claim().HoldsGate)
	}
}

// errStoppedAfterStep is what the test's hook stops a boot with after the step it names.
var errStoppedAfterStep = errors.New("the test stopped the boot after a step of its conversion")

// conversionScenario is what the release before left of the project CAPTURE, and the listing the
// first boot on this release reads (copyBoard): every case of the conversion's steps.
//
//   - CAPTURE-1 a root that lingers until a deadline ahead, closed at generation 2, whose tree
//     cleanup that release reserved; CAPTURE-2 a member of its tree that is done, whose suspended
//     claim the reserved cleanup waits for, with that release's linger_close, tree_close and
//     workspace_remove rows; and the tree's draw of an experiment.
//   - CAPTURE-3, a member of CAPTURE-1's tree in implementing that the linger handed over, whose
//     write landed (Dispatch shows it todo with the label), and CAPTURE-4, one whose write did not
//     (Dispatch shows it in progress), each with its hand-over status row.
//   - CAPTURE-6, a root a hand-over made at generation 1 with its gate, which the operator holds;
//     CAPTURE-5, a root of its own whose Dispatch parent is CAPTURE-6; a notice to CAPTURE-6's
//     architect queued with no fence; and the architects of both, written as the release before
//     writes a claim, with no mark that its issue holds its gate.
//   - CAPTURE-7, a root a person set to backlog, and CAPTURE-8, a member of its tree in
//     implementing whose implementer the root's departure suspended, at its third entry.
func conversionScenario(t *testing.T, pool *store.Pool) *copyBoard {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	root := conversionIssue("CAPTURE-1", "CAPTURE-1", phase.Done, "done", 2)
	deadline := time.Date(2031, 6, 1, 12, 0, 0, 0, time.UTC)
	root.LingerUntil = &deadline
	for _, issue := range []record.Issue{
		root,
		conversionIssue("CAPTURE-2", "CAPTURE-1", phase.Done, "done", 1),
		conversionIssue("CAPTURE-3", "CAPTURE-1", phase.Implementing, "in_progress", 1),
		conversionIssue("CAPTURE-4", "CAPTURE-1", phase.Implementing, "in_progress", 1),
		conversionIssue("CAPTURE-5", "CAPTURE-5", phase.Implementing, "in_progress", 1),
		conversionIssue("CAPTURE-6", "CAPTURE-6", phase.Planning, "in_progress", 1),
		conversionIssue("CAPTURE-7", "CAPTURE-7", phase.Done, "backlog", 1),
		conversionIssue("CAPTURE-8", "CAPTURE-7", phase.Implementing, "in_progress", 1),
	} {
		putOutboxIssue(t, pool, records, issue)
	}
	seedConversionClaim(t, pool, "CAPTURE-2", claim.RoleImplementer, "CAPTURE-1", "suspended", "pod-uid-2")
	for _, key := range []string{"CAPTURE-5", "CAPTURE-6"} {
		seedConversionClaim(t, pool, key, claim.RoleArchitect, key, "working", "")
	}
	seedConversionClaim(t, pool, "CAPTURE-8", claim.RoleImplementer, "CAPTURE-7", "suspended", "")
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: "CAPTURE-8", Role: claim.RoleImplementer, Entry: 3, EntryRaisedAt: time.Now()})
	}); err != nil {
		t.Fatalf("raise the implementer's entry of CAPTURE-8: %v", err)
	}
	for _, statement := range []string{
		`insert into tree_lifecycles (project, tree, epoch, authority, cleanup_started) values ('capture', 'CAPTURE-1', 1, 'workflow', true)`,
		`insert into tree_lifecycles (project, tree, epoch, authority) values ('capture', 'CAPTURE-6', 1, 'operator')`,
		`insert into handed_over_roots (issue, generation, gate_carried) values ('CAPTURE-6', 1, true)`,
		`update claims set suspended_by = 'daemon' where issue = 'CAPTURE-8'`,
		`insert into handovers (issue, tree, linger_generation) values ('CAPTURE-3', 'CAPTURE-1', 2), ('CAPTURE-4', 'CAPTURE-1', 2)`,
		`insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at)
			values ('CAPTURE-1', 'prompt-style', 'tree', 'terse', 'draw', '2026-11-01T00:00:00Z')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed what the release before left: %v", err)
		}
	}
	queueReleaseBeforeRow(t, pool, record.OutboxKindLingerClose, "CAPTURE-1", `{"generation":2}`)
	queueReleaseBeforeRow(t, pool, record.OutboxKindSupervise, "CAPTURE-2",
		`{"op":"tree_close","tree":"CAPTURE-1","role":"implementer","generation":1,"linger":2}`)
	queueReleaseBeforeRow(t, pool, record.OutboxKindWorkspaceRemove, "CAPTURE-2", `{"linger":2,"tree":"CAPTURE-1"}`)
	for _, key := range []string{"CAPTURE-3", "CAPTURE-4"} {
		queueReleaseBeforeRow(t, pool, record.OutboxKindDispatchStatus, key,
			`{"kind":"hand-over","status":"todo","observedStatus":"in_progress","handOverTree":"CAPTURE-1","handOverLinger":2}`)
	}
	queueReleaseBeforeRow(t, pool, record.OutboxKindNotice, "CAPTURE-6", `{"kind":"phase-finished","role":"planner","phase":"planning"}`)
	return &copyBoard{listing: []map[string]any{
		conversionSummary("CAPTURE-1", "done", 3, ""),
		conversionSummary("CAPTURE-2", "done", 3, "CAPTURE-1"),
		conversionSummary("CAPTURE-3", "todo", 3, "CAPTURE-1", "legion"),
		conversionSummary("CAPTURE-4", "in_progress", 3, "CAPTURE-1"),
		conversionSummary("CAPTURE-5", "in_progress", 3, "CAPTURE-6"),
		conversionSummary("CAPTURE-6", "in_progress", 3, ""),
		conversionSummary("CAPTURE-7", "backlog", 3, ""),
		conversionSummary("CAPTURE-8", "in_progress", 3, "CAPTURE-7"),
	}}
}

// conversionDump is every table the conversion writes, as text, leaving out what no step decides:
// ids, the moments a row was made or written, the moment a step record names its step started at,
// and when the conversion ended, of which only that it is set is read. The phases rows name each
// role's entry, which a member brought back raises.
func conversionDump(t *testing.T, pool *store.Pool) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"issues": `select concat_ws(' ', key, tree, phase, generation, status, dispatch_status, taken_back, holds_gate, handed_over,
			told_parent, carried_generation, carried_gate, retained_until, handed_over_from, linger_until) from issues order by key`,
		"outbox": `select concat_ws(' ', kind, issue, (payload - 'started_at')::text, case when kind = 'retention_end' then next_at::text end)
			from outbox order by kind, issue, (payload - 'started_at')::text`,
		"issue_lifecycles":       `select concat_ws(' ', project, issue, epoch, authority, cleanup_started) from issue_lifecycles order by issue`,
		"claims":                 `select concat_ws(' ', token, tree, tree_epoch, issue_epoch, holds_gate, state) from claims order by token`,
		"experiment_assignments": `select concat_ws(' ', issue_key, experiment, unit, variant, carried) from experiment_assignments order by 1`,
		"handovers":              `select issue from handovers order by issue`,
		"handed_over_roots":      `select concat_ws(' ', issue, generation, gate_carried) from handed_over_roots order by issue`,
		"tree_conversions":       `select concat_ws(' ', project, step, converted_at is not null) from tree_conversions order by project`,
		"phases":                 `select concat_ws(' ', issue, role, entry) from phases order by issue, role`,
	}
	dump := map[string][]string{}
	for table, query := range queries {
		rows, err := pool.Query(context.Background(), query)
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		dump[table] = lines
	}
	return dump
}

// bootConversionStoppedAt is one boot of w as the daemon boots it, as far as the end of its
// conversion: its steps that read the store, then the boot's reconcile, which fills the copy and
// runs the rest. The test's hook stops it after step stop, which a crash after that step stands
// for; at the last step it stops before the first admission pass, whose writes are no step's, so
// the runtime reads no consumer position and needs no consumers.
func bootConversionStoppedAt(ctx context.Context, w *workflowRuntime, stop int) error {
	w.converting = false
	w.afterConversionStep = func(step int) error {
		if step == stop {
			return errStoppedAfterStep
		}
		return nil
	}
	if err := w.convertStore(ctx); err != nil {
		return err
	}
	if !w.converting {
		return nil
	}
	return w.reconcile(ctx)
}

// A crash after any step of the first boot's conversion, and a second boot, change nothing more
// (R22 of dispatch://INFRA-3320; decision 21 of dispatch://INFRA-4730). A store the release
// before left (conversionScenario) is converted once with no stop; that is the conversion every
// other run is held to. Then, on a store seeded the same way, a boot stops after each step in
// turn and the next boot finishes the conversion from the step after it: every table the
// conversion writes reads as the conversion with no stop left it. A boot of a converted store runs
// no step and changes none of them.
func TestASecondConversionAndAnInterruptedOneChangeNothingMore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	converted := func(t *testing.T, stopFirst int) (map[string][]string, *workflowRuntime, *store.Pool) {
		t.Helper()
		pool := isolatedOutboxPool(t)
		board := conversionScenario(t, pool)
		w := copyRuntime(t, pool, board, 3)
		// No boot here reaches the first admission pass (bootConversionStoppedAt), the one part of
		// the reconcile that reads the consumers' positions, so the runtime opens none.
		w.projectID = "capture"
		w.now = func() time.Time { return time.Date(2031, 5, 1, 0, 0, 0, 0, time.UTC) }
		if stopFirst != convertRecord {
			if err := bootConversionStoppedAt(ctx, w, stopFirst); !errors.Is(err, errStoppedAfterStep) {
				t.Fatalf("the boot that stops after step %d ended with %v, want the stop", stopFirst, err)
			}
		}
		if err := bootConversionStoppedAt(ctx, w, convertRecord); !errors.Is(err, errStoppedAfterStep) {
			t.Fatalf("the boot that finishes the conversion ended with %v, want it to reach the last step", err)
		}
		return conversionDump(t, pool), w, pool
	}
	whole, w, pool := converted(t, convertRecord)
	if got := whole["tree_conversions"]; !slices.Equal(got, []string{"CAPTURE 12 t"}) {
		t.Fatalf("the conversion is recorded as %q, want its last step and when it ended", got)
	}
	// The premise: each step found something of the scenario to convert.
	for what, query := range map[string]string{
		"a row the release before queued fenced (1)": `select count(*) from outbox where kind = 'notice' and issue = 'CAPTURE-6'
			and (payload->>'generation')::bigint = 1 and (payload->>'entry')::bigint = 1`,
		"a handed-over root carried onto its record (2)": `select count(*) from issues where key = 'CAPTURE-6' and carried_generation = 1 and carried_gate`,
		"a linger retained (3)": `select count(*) from issues where key in ('CAPTURE-1', 'CAPTURE-2') and retained_until is not null
			and exists (select 1 from outbox where kind = 'retention_end' and issue = issues.key)`,
		"an operator's hold of an issue (4)": `select count(*) from issue_lifecycles where issue = 'CAPTURE-6' and authority = 'operator'`,
		"a reserved cleanup of an issue and what finishes it (5)": `select count(*) from issue_lifecycles where issue = 'CAPTURE-2' and cleanup_started
			and exists (select 1 from outbox where kind = 'issue_suspend' and issue = 'CAPTURE-2')
			and exists (select 1 from outbox where kind = 'supervise' and issue = 'CAPTURE-2' and payload->>'op' = 'issue_close')`,
		"a tree's draw carried onto a started issue (6)": `select count(*) from experiment_assignments where issue_key = 'CAPTURE-4' and carried`,
		"a claim marked as holding its issue's gate (7)": `select count(*) from claims where issue in ('CAPTURE-5', 'CAPTURE-6') and holds_gate`,
		"a member its root's departure suspended brought back (8)": `select count(*) from phases where issue = 'CAPTURE-8'
			and role = 'implementer' and entry = 4 and exists (select 1 from outbox where kind = 'supervise' and issue = 'CAPTURE-8'
			and payload->>'op' = 'start' and payload->>'role' = 'implementer' and (payload->>'entry')::bigint = 4)`,
		"a landed hand-over finished (9)": `select count(*) from issues where key = 'CAPTURE-3' and tree = 'CAPTURE-1' and generation = 2
			and carried_generation = 2 and taken_back and status = 'todo' and holds_gate`,
		"the rows only the release before runs deleted (10)": `select count(*) where not exists (select 1 from outbox where kind in ('linger_close', 'workspace_remove')
			or (kind = 'supervise' and payload->>'op' = 'tree_close') or payload->>'kind' = 'hand-over') and not exists (select 1 from handovers)`,
		"a root under a parent at work told (11)": `select count(*) from outbox where kind = 'reparent_seed' and issue = 'CAPTURE-5'`,
	} {
		var n int
		if err := pool.QueryRow(ctx, query).Scan(&n); err != nil || n == 0 {
			t.Fatalf("after the conversion the scenario holds no %s (%v): it no longer exercises that step", what, err)
		}
	}
	if err := bootConversionStoppedAt(ctx, w, 0); err != nil {
		t.Fatalf("a boot of the converted store: %v", err)
	}
	if again := conversionDump(t, pool); !maps.EqualFunc(again, whole, slices.Equal[[]string]) {
		t.Fatalf("a boot of the converted store changed what the conversion wrote:\nbefore %q\nafter  %q", whole, again)
	}
	for stop := convertFences; stop < convertRecord; stop++ {
		t.Run(fmt.Sprintf("stopped after step %d", stop), func(t *testing.T) {
			if got, _, _ := converted(t, stop); !maps.EqualFunc(got, whole, slices.Equal[[]string]) {
				t.Fatalf("a conversion stopped after step %d and finished by the next boot left\n%q\nwant what one with no stop leaves\n%q", stop, got, whole)
			}
		})
	}
}
