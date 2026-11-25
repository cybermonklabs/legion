package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// releaseBeforeMigrations are the migrations that main at 4e851eb1, the release before the one
// that stores no tree, does not carry: the two of this release and the three of the branches it
// stacks on. A store migrated with every migration but these is the store that release leaves.
var releaseBeforeMigrations = []int{20261114064234, 20261114232555, 20261122154643, 20261122154644, 20261122154645}

// oldestReleaseMigrations are the migrations that legion-v22.16.2, the oldest release a project's
// daemon runs before the upgrade, does not carry: releaseBeforeMigrations and every migration main
// added after it, issues.members_owed's 20261123224717 among them. A store migrated with every
// migration but these is the store that release leaves.
var oldestReleaseMigrations = append([]int{20261117034837, 20261119203047, 20261120012547, 20261120042902, 20261120055717,
	20261120144241, 20261121170714, 20261123165139, 20261123212127, 20261123224717}, releaseBeforeMigrations...)

// The members of the world the release before left (seedMembersBackWorld).
const (
	backRoot             = "CAPTURE-10" // a root back in Legion's work, which owes its members
	backReturned         = "CAPTURE-11" // a member of CAPTURE-10 in implementing, suspended by the daemon
	backWaiting          = "CAPTURE-12" // a member of CAPTURE-10 in implementing, suspended by the daemon, with an open child
	backOperator         = "CAPTURE-13" // a member of CAPTURE-10 whose implementer the operator suspended
	backReleased         = "CAPTURE-14" // a member of CAPTURE-10 whose implementer the idle release suspended
	backStopped          = "CAPTURE-15" // a member of CAPTURE-10 a person set to backlog
	backPersonHeld       = "CAPTURE-16" // a member of CAPTURE-10 whose Dispatch claim a person holds
	backHandedOver       = "CAPTURE-17" // a member of CAPTURE-20 with a hand-over in flight
	backAccepting        = "CAPTURE-18" // a member of CAPTURE-10 in accepting, suspended by the daemon
	backChild            = "CAPTURE-19" // an open Dispatch child of CAPTURE-12, which the daemon does not record
	backParked           = "CAPTURE-20" // a root a person set to backlog, whose tree lingers
	backTesting          = "CAPTURE-21" // a member of CAPTURE-20 in testing, suspended before the source existed
	backAcceptingWaiting = "CAPTURE-22" // a member of CAPTURE-10 in accepting, suspended by the daemon, with an open child
	backAcceptingChild   = "CAPTURE-29" // an open Dispatch child of CAPTURE-22, which the daemon does not record
)

// seedMembersBackWorld writes, on st, a store at the schema of a release before the one that
// stores no tree (storeWithholding with releaseBeforeMigrations or oldestReleaseMigrations), what
// that release leaves of the project CAPTURE when a root's departure has suspended its tree, and
// answers the listing the first boot on this release reads. Every member's phase worker holds its
// phases row at entry 3, or 2 for the tester, and its claim is suspended by the source the case
// names:
//
//   - CAPTURE-10, a root whose re-admission has started run 2 and that no promotion has opened, so
//     it owes its members: issues.members_owed where the store holds the column, and the state its
//     migration marks where it does not. CAPTURE-11 and CAPTURE-12 are suspended by the daemon,
//     CAPTURE-12 under CAPTURE-19, an open Dispatch child; CAPTURE-13 by the operator; CAPTURE-14
//     by the idle release; CAPTURE-15, which a person set to backlog, by the daemon; CAPTURE-16,
//     whose Dispatch claim a person holds, by the daemon; and CAPTURE-18 and CAPTURE-22, in
//     accepting, whose architect, the role of that phase, and implementer, a role that ran, are each
//     suspended by the daemon, CAPTURE-22 under CAPTURE-29, an open Dispatch child;
//   - CAPTURE-20, a root a person set to backlog, whose linger is ahead, with CAPTURE-21 in testing,
//     its tester's claim suspended before the source existed (unknown), and CAPTURE-17, suspended
//     by the daemon, which its linger's close handed over with a write that did not land.
func seedMembersBackWorld(t *testing.T, st *store.Store) *copyBoard {
	t.Helper()
	ctx := context.Background()
	pool := st.Pool()
	lingers := time.Now().Add(48 * time.Hour).UTC()
	type seeded struct {
		key, tree, status string
		at                phase.Phase
		generation        int
		role              claim.Role
		by                supervise.SuspendSource
	}
	cases := []seeded{
		{key: backRoot, tree: backRoot, status: "todo", at: phase.Admitted, generation: 2},
		{key: backParked, tree: backParked, status: "backlog", at: phase.Done, generation: 1},
		{key: backReturned, tree: backRoot, status: "in_progress", at: phase.Implementing, generation: 1, role: claim.RoleImplementer, by: supervise.SuspendedByDaemon},
		{key: backWaiting, tree: backRoot, status: "in_progress", at: phase.Implementing, generation: 1, role: claim.RoleImplementer, by: supervise.SuspendedByDaemon},
		{key: backOperator, tree: backRoot, status: "in_progress", at: phase.Implementing, generation: 1, role: claim.RoleImplementer, by: supervise.SuspendedByOperator},
		{key: backReleased, tree: backRoot, status: "in_progress", at: phase.Implementing, generation: 1, role: claim.RoleImplementer, by: supervise.SuspendedByReleased},
		{key: backStopped, tree: backRoot, status: "backlog", at: phase.Implementing, generation: 1, role: claim.RoleImplementer, by: supervise.SuspendedByDaemon},
		{key: backPersonHeld, tree: backRoot, status: "in_progress", at: phase.Implementing, generation: 1, role: claim.RoleImplementer, by: supervise.SuspendedByDaemon},
		{key: backHandedOver, tree: backParked, status: "in_progress", at: phase.Implementing, generation: 1, role: claim.RoleImplementer, by: supervise.SuspendedByDaemon},
		{key: backTesting, tree: backParked, status: "testing", at: phase.Testing, generation: 1, role: claim.RoleTester, by: supervise.SuspendedByUnknown},
		{key: backAccepting, tree: backRoot, status: "in_progress", at: phase.Accepting, generation: 1, role: claim.RoleArchitect, by: supervise.SuspendedByDaemon},
		{key: backAcceptingWaiting, tree: backRoot, status: "in_progress", at: phase.Accepting, generation: 1, role: claim.RoleArchitect, by: supervise.SuspendedByDaemon},
	}
	board := &copyBoard{}
	for _, c := range cases {
		var lingerUntil *time.Time
		if c.key == backParked {
			lingerUntil = &lingers
		}
		if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, dispatch_status, rank,
			last_dispatch_seq, repo, default_branch, github_repo, linger_until)
			values ($1, $2, 'CAPTURE', $1, $3, $4, $5, $5, $1, 1, 'acme/widgets', 'main', 'acme/widgets', $6)`,
			c.key, c.tree, string(c.at), c.generation, c.status, lingerUntil); err != nil {
			t.Fatalf("record %s on the release before's schema: %v", c.key, err)
		}
		parent := ""
		if c.key != c.tree {
			parent = c.tree
		}
		summary := conversionSummary(c.key, c.status, 3, parent)
		if c.key == backRoot {
			summary = conversionSummary(c.key, c.status, 3, parent, "legion")
		}
		if c.key == backPersonHeld {
			summary["claim"] = map[string]any{"actor": map[string]any{"kind": "user", "id": "pat"}}
		}
		board.listing = append(board.listing, summary)
		if c.role == "" {
			continue
		}
		seedConversionClaim(t, pool, c.key, c.role, c.tree, "suspended", "")
		token, err := claim.NewToken("capture", c.key, c.role)
		if err != nil {
			t.Fatal(err)
		}
		entry := 3
		if c.role == claim.RoleTester {
			entry = 2
		}
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`update claims set suspended_by = $2 where token = $1`, []any{string(token), string(c.by)}},
			{`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_raised_at)
				values ($1, $2, $3, '', 0, '', $4, now())`, []any{c.key, string(c.role), string(token), entry}},
		} {
			if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
				t.Fatalf("seed the %s of %s: %v", c.role, c.key, err)
			}
		}
	}
	for _, statement := range []string{
		`insert into dispatch_claims (key, claimed_by, claim_seq) values ('CAPTURE-16', 'user:pat', 1)`,
		`insert into handovers (issue, tree, linger_generation) values ('CAPTURE-17', 'CAPTURE-20', 1)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed what the release before left: %v", err)
		}
	}
	// The implementer of each member in accepting ran before it and stands suspended with its tree.
	for _, key := range []string{backAccepting, backAcceptingWaiting} {
		seedConversionClaim(t, pool, key, claim.RoleImplementer, backRoot, "suspended", "")
		token, err := claim.NewToken("capture", key, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `update claims set suspended_by = $2 where token = $1`, string(token), string(supervise.SuspendedByDaemon)); err != nil {
			t.Fatalf("seed the implementer of %s: %v", key, err)
		}
	}
	var owedKept bool
	if err := pool.QueryRow(ctx, `select exists (select 1 from information_schema.columns where table_name = 'issues'
		and column_name = 'members_owed')`).Scan(&owedKept); err != nil {
		t.Fatalf("read whether the schema holds issues.members_owed: %v", err)
	}
	if owedKept {
		if _, err := pool.Exec(ctx, `update issues set members_owed = true where key = $1`, backRoot); err != nil {
			t.Fatalf("mark %s as owing its members: %v", backRoot, err)
		}
	}
	board.listing = append(board.listing, conversionSummary(backChild, "todo", 3, backWaiting),
		conversionSummary(backAcceptingChild, "todo", 3, backAcceptingWaiting))
	return board
}

// listMembersBack is what `legion members-back` reads of CAPTURE on pool (ListMembersBack), in a
// read-only transaction, as the command reads it.
func listMembersBack(t *testing.T, pool *store.Pool) MembersBackListing {
	t.Helper()
	var listing MembersBackListing
	if err := store.TransactWith(context.Background(), pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		listing, err = ListMembersBack(ctx, tx, "CAPTURE")
		return err
	}); err != nil {
		t.Fatalf("list the members the first boot brings back: %v", err)
	}
	return listing
}

// convertMembersBackWorld migrates st to this release and runs its first boot's conversion over
// board as the daemon boots, through its last step and before its first admission pass
// (bootConversionStoppedAt).
func convertMembersBackWorld(t *testing.T, st *store.Store, board *copyBoard) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate to this release: %v", err)
	}
	w := copyRuntime(t, st.Pool(), board, 3)
	w.projectID = "capture"
	if err := bootConversionStoppedAt(ctx, w, convertRecord); !errors.Is(err, errStoppedAfterStep) {
		t.Fatalf("the first boot's conversion ended with %v, want it to reach its last step", err)
	}
}

// memberRecord is the daemon's record of key on pool.
func memberRecord(t *testing.T, pool *store.Pool, key string) record.Issue {
	t.Helper()
	var issue *record.Issue
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(ctx, tx, key)
		return err
	}); err != nil || issue == nil {
		t.Fatalf("read %s: %v (recorded %t)", key, err, issue != nil)
	}
	return *issue
}

// phaseEntry is the entry of role's phases row of key on pool.
func phaseEntry(t *testing.T, pool *store.Pool, key string, role claim.Role) int64 {
	t.Helper()
	var entry int64
	if err := pool.QueryRow(context.Background(), `select entry from phases where issue = $1 and role = $2`, key, string(role)).Scan(&entry); err != nil {
		t.Fatalf("read the %s entry of %s: %v", role, key, err)
	}
	return entry
}

// roleStarts is each start of role's claim of key that the outbox holds, in id order.
func roleStarts(t *testing.T, pool *store.Pool, key string, role claim.Role) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, row := range rowsOfKind(t, pool, record.OutboxKindSupervise) {
		if row.Issue != key {
			continue
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode outbox row %d: %v", row.ID, err)
		}
		if request := payload.(record.SuperviseRequest); request.Op == "start" && request.Role == role {
			starts = append(starts, request)
		}
	}
	return starts
}

// The first boot on the release that stores no tree brings back each member that its root's
// departure suspended under the release before (step 8 of the conversion, bringMembersBack; item
// 46 of dispatch://INFRA-4730, R20 and R21 of dispatch://INFRA-3320). The world is the release
// before's (seedMembersBackWorld), migrated and converted as the daemon boots. CAPTURE-11, whose
// root CAPTURE-10 is back in Legion's work, and CAPTURE-21, whose root CAPTURE-20 a person set to
// backlog, each resume their phase at a new phase entry, with the return's task. CAPTURE-12, whose
// open Dispatch child holds the start of its phase, waits in awaiting_children with no worker
// start. CAPTURE-13, which the operator suspended, CAPTURE-14, which the idle release suspended,
// CAPTURE-15, which a person stopped, CAPTURE-16, whose Dispatch claim a person holds, and
// CAPTURE-17, whose hand-over the conversion's next step decides, stay as they were. Negative
// control, run when this was written, failing this test and then reverted: with the step left out
// of listedConversionSteps, CAPTURE-11's implementer stands at entry 3 after the boot.
func TestTheFirstBootBringsBackEachMemberItsRootsDepartureSuspended(t *testing.T) {
	t.Parallel()
	st, _ := storeWithholding(t, "legion_members_back", releaseBeforeMigrations...)
	board := seedMembersBackWorld(t, st)
	convertMembersBackWorld(t, st, board)
	pool := st.Pool()

	for _, back := range []struct {
		key   string
		at    phase.Phase
		role  claim.Role
		entry int64
	}{
		{backReturned, phase.Implementing, claim.RoleImplementer, 4},
		{backTesting, phase.Testing, claim.RoleTester, 3},
	} {
		if entry := phaseEntry(t, pool, back.key, back.role); entry != back.entry {
			t.Fatalf("the %s of %s stands at entry %d after the first boot, want %d: a new entry of the phase it stood in", back.role, back.key, entry, back.entry)
		}
		starts := roleStarts(t, pool, back.key, back.role)
		if len(starts) != 1 || starts[0].Entry != back.entry || starts[0].Phase != back.at || starts[0].Generation != 1 ||
			!strings.Contains(starts[0].Task, "came back into Legion's work after a stop") {
			t.Fatalf("the starts of %s's %s are %+v, want one of entry %d of run 1 in %s on the return's task", back.key, back.role, starts, back.entry, back.at)
		}
		if issue := memberRecord(t, pool, back.key); issue.Phase != back.at || issue.Generation != 1 {
			t.Fatalf("%s stands in %s at run %d after the first boot, want %s at run 1", back.key, issue.Phase, issue.Generation, back.at)
		}
	}

	waiting := memberRecord(t, pool, backWaiting)
	if waiting.Phase != phase.AwaitingChildren || waiting.ChildrenWait == nil || waiting.ChildrenWait.Start != phase.Implementing ||
		!slices.Contains(waiting.ChildrenWait.Children, backChild) {
		t.Fatalf("%s stands in %s holding %+v after the first boot, want awaiting_children holding the implementing start for %s",
			backWaiting, waiting.Phase, waiting.ChildrenWait, backChild)
	}
	if starts := roleStarts(t, pool, backWaiting, claim.RoleImplementer); len(starts) != 0 {
		t.Fatalf("the implementer of %s is owed the starts %+v, want none while its child is open", backWaiting, starts)
	}

	for _, kept := range []string{backOperator, backReleased, backStopped, backPersonHeld, backHandedOver} {
		if entry := phaseEntry(t, pool, kept, claim.RoleImplementer); entry != 3 {
			t.Fatalf("the implementer of %s stands at entry %d after the first boot, want 3: nothing brings it back", kept, entry)
		}
		if starts := roleStarts(t, pool, kept, claim.RoleImplementer); len(starts) != 0 {
			t.Fatalf("the implementer of %s is owed the starts %+v, want none", kept, starts)
		}
		if issue := memberRecord(t, pool, kept); issue.Phase != phase.Implementing {
			t.Fatalf("%s stands in %s after the first boot, want implementing as the release before left it", kept, issue.Phase)
		}
	}
}

// stepStartsOf is each start of a step the outbox holds for key, in id order.
func stepStartsOf(t *testing.T, pool *store.Pool, key string) []record.StepRecord {
	t.Helper()
	var starts []record.StepRecord
	for _, row := range rowsOfKind(t, pool, record.OutboxKindStepRecord) {
		if row.Issue != key {
			continue
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode outbox row %d: %v", row.ID, err)
		}
		if step := payload.(record.StepRecord); step.StartedAt != nil && step.EndedAt == nil {
			starts = append(starts, step)
		}
	}
	return starts
}

// R9 of dispatch://INFRA-3320 judges a member the first boot brings back before its return starts
// anything (bringMembersBack, workflow.Engine.ReturnMemberToWork), and a member in accepting, whose
// return starts no phase worker and brings back every role of it that ran, is held as a member in
// implementing is (CAPTURE-12 in TestTheFirstBootBringsBackEachMemberItsRootsDepartureSuspended).
// CAPTURE-22, under CAPTURE-29, an open Dispatch child of its own project, waits in
// awaiting_children holding the start of accepting, and the outbox holds nothing that starts,
// stops or tells a role of it — no start of its architect or its implementer, no task retirement
// and no phase-ended message — and no start of its step. The control is CAPTURE-18, the same
// member with no open child: it stands in accepting with one start of each role of it that ran,
// its architect and its implementer, with no task, and one start of its step. Negative control,
// run when this was written, failing this test and then reverted: with bringMembersBack calling
// workflow.Engine.ReturnToWork and then the admission pass's stop (StopForChildren), as before,
// CAPTURE-22 waits in awaiting_children with a start of its architect queued in accepting.
func TestTheFirstBootHoldsAMemberInAcceptingOnItsOpenChildAndStartsNoRoleOfIt(t *testing.T) {
	t.Parallel()
	st, _ := storeWithholding(t, "legion_members_back_accepting", releaseBeforeMigrations...)
	board := seedMembersBackWorld(t, st)
	convertMembersBackWorld(t, st, board)
	pool := st.Pool()

	if control := memberRecord(t, pool, backAccepting); control.Phase != phase.Accepting || control.ChildrenWait != nil {
		t.Fatalf("%s with no open child stands in %s holding %+v after the first boot, want accepting, holding nothing",
			backAccepting, control.Phase, control.ChildrenWait)
	}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleImplementer} {
		if starts := roleStarts(t, pool, backAccepting, role); len(starts) != 1 || starts[0].Task != "" || starts[0].Phase != phase.Accepting {
			t.Fatalf("the starts of %s's %s are %+v, want one in accepting with no task: each role of it that ran comes back", backAccepting, role, starts)
		}
	}
	if steps := stepStartsOf(t, pool, backAccepting); len(steps) != 1 {
		t.Fatalf("the step starts of %s are %+v, want one: its step starts again with its return", backAccepting, steps)
	}

	waiting := memberRecord(t, pool, backAcceptingWaiting)
	if waiting.Phase != phase.AwaitingChildren || waiting.ChildrenWait == nil || waiting.ChildrenWait.Start != phase.Accepting ||
		!slices.Equal(waiting.ChildrenWait.Children, []string{backAcceptingChild}) {
		t.Fatalf("%s stands in %s holding %+v after the first boot, want awaiting_children holding the accepting start for %s alone",
			backAcceptingWaiting, waiting.Phase, waiting.ChildrenWait, backAcceptingChild)
	}
	for _, kind := range []record.OutboxKind{record.OutboxKindSupervise, record.OutboxKindPhaseEnded} {
		for _, row := range rowsOfKind(t, pool, kind) {
			if row.Issue == backAcceptingWaiting {
				t.Fatalf("the outbox holds the %s row %s of %s, want none: no role of it starts, stops or is told while its child is open",
					kind, row.Payload, backAcceptingWaiting)
			}
		}
	}
	if steps := stepStartsOf(t, pool, backAcceptingWaiting); len(steps) != 0 {
		t.Fatalf("the step starts of %s are %+v, want none: its step starts again only with the return from its children", backAcceptingWaiting, steps)
	}
}

// `legion members-back` lists, before the upgrade, exactly the members that the first boot brings
// back: it reads the store through the one reading that the conversion's step makes
// (ListMembersBack, record.ReadMembersBack), on the schema of the release before, main at 4e851eb1,
// and on that of legion-v22.16.2, which holds no issues.members_owed and whose root owing its
// members is read by that column's migration's own mark. It gives the same answer once the store
// has migrated to this release and before the boot, and a store whose conversion has run lists
// none. The world is seedMembersBackWorld's: the listing names CAPTURE-11, CAPTURE-12, CAPTURE-18
// and CAPTURE-22 under CAPTURE-10, back in work and owing its members, and CAPTURE-21 under
// CAPTURE-20, set to backlog; the boot then brings back each member it names, by a start of a role
// of it or a wait on its open children, and no other.
func TestTheMembersBackListingIsTheMembersTheFirstBootBringsBack(t *testing.T) {
	t.Parallel()
	for _, schema := range []struct {
		name, database string
		withheld       []int
	}{
		{"main at 4e851eb1", "legion_members_back_listing", releaseBeforeMigrations},
		{"legion-v22.16.2, with no members_owed", "legion_members_back_oldest", oldestReleaseMigrations},
	} {
		t.Run(schema.name, func(t *testing.T) {
			t.Parallel()
			st, _ := storeWithholding(t, schema.database, schema.withheld...)
			board := seedMembersBackWorld(t, st)
			pool := st.Pool()

			before := listMembersBack(t, pool)
			want := []record.MemberBack{
				{Key: backReturned, Phase: phase.Implementing, Root: backRoot, Left: record.TreeLeftReturned},
				{Key: backWaiting, Phase: phase.Implementing, Root: backRoot, Left: record.TreeLeftReturned},
				{Key: backAccepting, Phase: phase.Accepting, Root: backRoot, Left: record.TreeLeftReturned},
				{Key: backTesting, Phase: phase.Testing, Root: backParked, Left: "backlog"},
				{Key: backAcceptingWaiting, Phase: phase.Accepting, Root: backRoot, Left: record.TreeLeftReturned},
			}
			if before.BroughtBack || before.ConvertedAt != nil || !slices.Equal(before.Members, want) {
				t.Fatalf("the listing on the store before the upgrade reads %+v, want the members %+v and no conversion", before, want)
			}
			if _, err := st.Migrate(context.Background()); err != nil {
				t.Fatalf("migrate to this release: %v", err)
			}
			if migrated := listMembersBack(t, pool); !slices.Equal(migrated.Members, before.Members) || migrated.BroughtBack {
				t.Fatalf("the listing once the store migrated reads %+v, want the members it named before, %+v", migrated, before.Members)
			}

			convertMembersBackWorld(t, st, board)
			var brought []string
			for _, key := range []string{backReturned, backWaiting, backOperator, backReleased, backStopped, backPersonHeld, backHandedOver,
				backAccepting, backTesting, backAcceptingWaiting} {
				role := claim.RoleImplementer
				switch key {
				case backTesting:
					role = claim.RoleTester
				case backAccepting, backAcceptingWaiting:
					role = claim.RoleArchitect
				}
				if memberRecord(t, pool, key).Phase == phase.AwaitingChildren || len(roleStarts(t, pool, key, role)) > 0 {
					brought = append(brought, key)
				}
			}
			var listed []string
			for _, member := range before.Members {
				listed = append(listed, member.Key)
			}
			if !slices.Equal(brought, listed) {
				t.Fatalf("the first boot brought back %v, want exactly the members the listing named before it, %v", brought, listed)
			}

			after := listMembersBack(t, pool)
			if !after.BroughtBack || after.ConvertedAt == nil || len(after.Members) != 0 {
				t.Fatalf("the listing after the conversion reads %+v, want the conversion ended and no member", after)
			}
		})
	}
}
