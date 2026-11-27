package store

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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

// conversionRelease names the release whose way back the header of issueFactsVersion
// (20261122154645_issue_facts) carries — the step an operator runs before a rollback to the
// release before (R22 of dispatch://INFRA-3320, decision 15 of dispatch://INFRA-4730) — as a
// failure to read that block names it.
const conversionRelease = "the release that stores no tree"

// keysBeforeTheConversion is, for each outbox kind the release before runs, the payload keys its
// type declares (record's payload types at main's ecd967b17a4e, with #5285's launcher workspace
// read, which that release decodes strictly: record.DecodeOutboxPayload, DisallowUnknownFields).
// It is a copy because that release's types are not in the tree; the types they nest are the same
// in both releases. A kind absent here is one that release does not run: retention_end.
var keysBeforeTheConversion = map[record.OutboxKind][]string{
	record.OutboxKindDispatchStatus:  {"kind", "status", "observedStatus", "reason", "handOverTree", "handOverLinger", "closeGeneration", "observedSeq"},
	record.OutboxKindDispatchMessage: {"body", "artifact"},
	record.OutboxKindNotice: {"kind", "role", "phase", "summary", "verdict", "version", "reason", "topic", "failure", "escalation", "resends",
		"paced_at", "paced_from", "resend_of", "catch_up", "openCapabilities", "blocked", "held", "silent", "actor", "ref", "decision", "root",
		"update", "pause", "volume"},
	record.OutboxKindSupervise: {"op", "tree", "role", "task", "kept", "generation", "phase", "entry", "resendEntry", "resumeTask",
		"roundNamed", "ruleResume", "needsSlot", "relaunch", "assignment", "freshSession", "linger", "reason", "checkLook", "checkOpenedAt",
		"pause", "issue", "wakeKind"},
	record.OutboxKindGateSeed:          {"artifactId", "step", "version", "generation"},
	record.OutboxKindIssueSuspend:      {"tree", "generation", "treeGeneration", "turnStart", "podUid"},
	record.OutboxKindIssueBundle:       {"tree", "generation", "treeGeneration", "role", "closeRoles", "turnStart", "launcher", "close", "podUid"},
	record.OutboxKindIssueRelease:      {"tree", "generation", "treeGeneration", "podUid"},
	record.OutboxKindIssueBranch:       {"generation"},
	record.OutboxKindCheckWake:         {"look", "openedAt", "reason"},
	record.OutboxKindCheckLook:         {"look", "openedAt"},
	record.OutboxKindCheckBound:        {},
	record.OutboxKindClaimRelease:      {},
	record.OutboxKindRoleMessage:       {"role", "body"},
	record.OutboxKindPhaseEnded:        {"role", "from", "to", "reason", "mergeForward"},
	record.OutboxKindHookPrompt:        {"hook", "role", "from", "to", "count", "counts", "pullRequest", "prompt"},
	record.OutboxKindUpdateAsk:         {"role", "round", "pod", "text"},
	record.OutboxKindMergeForward:      {"role", "step", "head", "landed", "base", "phase", "pullRequest", "why"},
	record.OutboxKindMergeQueueRemoval: {"number", "head", "unread"},
	record.OutboxKindReparentSeed:      {"parent", "tree", "ran"},
}

// noticeKindsBeforeTheConversion is every notice kind the release before takes, an architect's
// and the controller's (its validNoticeKinds and controllerOnlyNoticeKind): a notice of any other
// kind it refuses at every attempt.
var noticeKindsBeforeTheConversion = []string{
	"phase-finished", "worker-died", "worker-stalled", "worker-silent", "worker-background-stalled", "held",
	"pr-blocked", "pr-merged", "pr-closed-unmerged", "pull-request-missing", "message-refused",
	"design-approved", "design-changes-requested", "carried-gate-opened",
	"basis-ended", "basis-changes-requested", "resting-root",
	"child-closed", "child-status", "children-first", "awaiting-children", "child-release-stalled", "child-not-recorded",
	"catch-up", "checks-red", "review-stuck", "status-reasserted", "volume-near-full",
	"session-lost", "session-forgotten", "session-store-fault",
	"triage", "slot-free", "todo", "tick", "unslotted", "awaiting-person", "parked-decision", "update-stalled", "provider-errors",
	"tree-stalled", "architect-stalled", "project-paused", "project-resumed",
}

// decodedBeforeTheConversion is the refusal of the release before's strict decode of a row of kind
// with payload, nil where it takes the row: a kind it does not run, a key its type does not
// declare, or a notice of a kind it does not know.
func decodedBeforeTheConversion(kind record.OutboxKind, payload []byte) string {
	if kind == record.OutboxKindControllerNotice {
		kind = record.OutboxKindNotice
	}
	declared, runs := keysBeforeTheConversion[kind]
	if !runs {
		return "a kind it does not run"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return err.Error()
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(declared, key) {
			return "the unknown field " + key
		}
	}
	if kind == record.OutboxKindNotice {
		var notice struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(payload, &notice); err != nil {
			return err.Error()
		}
		if !slices.Contains(noticeKindsBeforeTheConversion, notice.Kind) {
			return "the notice kind " + notice.Kind
		}
	}
	return ""
}

// The way back of the release that stores no tree leaves the outbox, the records and the
// lifecycles of the project that rolls back as the release before reads them, and changes nothing
// of another project's. The store holds, as this release leaves it when the daemon stops for the
// rollback: LEGION-1, a root of its own; LEGION-2, which the first boot's conversion made a root of
// its own from a hand-over of LEGION-1's tree that had landed, carrying its run in implementing,
// its claims still naming LEGION-1's tree; LEGION-3, another such root that ran nothing and stands
// at admitted; one unfinished row of each kind this release writes, each with the fence it queues
// it with, and a row of each new shape (a retention_end, a reparented notice, a status-scope
// controller notice, a status write with an option, R18's seed, a status write that waits for
// another, one the controller asked for and one carrying the Dispatch status its record held when
// it was queued); another project's status write with its fence and its retention_end; both
// projects' records of their conversion; and session writes the repair
// holds on unknown chains (held_session_writes): on a record in the workflow that Dispatch shows at
// the last of two held writes, on one Dispatch shows at its own status, on records at phase done,
// on a record at each status of record.OutOfWorkflowStatuses, and on another project's record.
//
// The control comes first, on the same rows: the strict decode of the release before refuses each
// row that carries a key this release adds or is of a kind or a notice kind it adds, and takes the
// gate seed and the branch, whose shape it shares. Then the block runs for LEGION as the
// migration's header carries it, as one exec (the simple protocol a psql run uses). After it, the
// rows of the new kinds and of an option are gone; every other LEGION row is still there, the
// release before's decode takes it, and so does this release's, which reads it on the roll
// forward; LEGION-2's and LEGION-3's records, claims, lifecycle, handed_over_roots row and queued
// rows are what that release's own re-admission would have written; LEGION's record of the
// conversion is gone; one reassert status write in the release before's shape sets back the held
// writes of the record in the workflow that Dispatch shows at another status, at the last held
// write's sequence, no row is queued for any other, and every held list of LEGION is empty; the
// crossing times of LEGION's claims (published_since, unpublished_since) are cleared, so a later
// upgrade reads none stamped before the rollback; and OTHER's rows, record, held writes and claim
// crossings stand as they were.
//
// What this cannot show is the earlier daemon running, whose binary is not here (its decode is
// the copy above), and the relabel of a Sandbox, which the block's last statement prints the
// rows of and a cluster's kubectl does.
func TestTheTreeConversionWayBackLeavesWhatTheReleaseBeforeReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	const root, carried, admitted, other = "LEGION-1", "LEGION-2", "LEGION-3", "OTHER-7"
	for _, issue := range []struct {
		key, tree     string
		at            phase.Phase
		generation    int
		carried       int
		gate, project string
	}{
		{key: root, tree: root, at: phase.Implementing, generation: 2, project: "LEGION"},
		{key: carried, tree: root, at: phase.Implementing, generation: 3, carried: 3, gate: "true", project: "LEGION"},
		{key: admitted, tree: root, at: phase.Admitted, generation: 2, carried: 2, gate: "false", project: "LEGION"},
		{key: other, tree: other, at: phase.Implementing, generation: 1, project: "OTHER"},
	} {
		exec("record "+issue.key, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			carried_generation, carried_gate) values ($1, $2, $5, $1, $3, $4, 'todo', 'B', 1, $6, coalesce(nullif($7, '')::boolean, false))`,
			issue.key, issue.tree, string(issue.at), issue.generation, issue.project, issue.carried, issue.gate)
	}
	exec("open LEGION-1's tree lifecycle", `insert into tree_lifecycles (project, tree, epoch, authority) values ('legion', $1, 1, 'workflow')`, root)
	for _, c := range []struct {
		issue string
		role  claim.Role
	}{{root, claim.RoleArchitect}, {carried, claim.RoleArchitect}, {carried, claim.RoleImplementer}} {
		token, err := claim.NewToken("legion", c.issue, c.role)
		if err != nil {
			t.Fatal(err)
		}
		exec("seed the claim "+string(token), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, tree_epoch, launch_tree, published_since, unpublished_since)
			values ($1, 'legion', $2, $3, $4, 1, '', '', 'working', 0, 0, 0, 0, 1, $3, now() - interval '1 hour', now() - interval '2 hours')`,
			string(token), root, c.issue, string(c.role))
	}
	// Another project's claim keeps the crossings its own daemon stamped.
	exec("seed OTHER's claim", `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, tree_epoch, launch_tree, published_since, unpublished_since)
		values ('other-other-other-7-architect', 'other', $1, $1, 'architect', 1, '', '', 'working', 0, 0, 0, 0, 1, $1,
			now() - interval '1 hour', now() - interval '2 hours')`, other)
	for _, project := range []string{"LEGION", "OTHER"} {
		exec("record the conversion of "+project, `insert into tree_conversions (project, step, converted_at) values ($1, 11, now())`, project)
	}
	// The held writes. The literal list of statuses the way back's SQL calls out of the workflow
	// is held to record.OutOfWorkflowStatuses by one record at each of them, so the two cannot
	// drift apart unseen. A root of its own tree that waits on a person is one the release before
	// sets a session's write back on whatever its status (its sessionStatusWrite's waiting root),
	// so its held write is set back at a status out of the workflow too; an issue that waits on a
	// person under another's tree is no such root.
	type heldIssue struct {
		key, project, tree, phase, status, shown, ask, writes string
		// reassert is the payload of the one status write the way back queues for the issue, as
		// JSON, "" for none.
		reassert string
	}
	held := []heldIssue{
		{key: "LEGION-20", phase: "implementing", status: "in_progress", shown: "done",
			writes:   `[{"session":"ses-a","status":"backlog","seq":8},{"session":"ses-b","status":"done","seq":9}]`,
			reassert: `{"kind":"reassert","status":"in_progress","observedStatus":"done","observedSeq":9}`},
		{key: "LEGION-21", phase: "implementing", status: "in_progress", shown: "in_progress", writes: `[{"session":"ses-a","status":"in_progress","seq":4}]`},
		{key: "LEGION-22", phase: "done", status: "done", shown: "backlog", writes: `[{"session":"ses-a","status":"backlog","seq":5}]`},
		{key: "LEGION-23", phase: "done", status: "in_progress", shown: "backlog", writes: `[{"session":"ses-a","status":"backlog","seq":5}]`},
		{key: "LEGION-24", phase: "awaiting_person", status: "backlog", shown: "todo", ask: "ask-24", writes: `[{"session":"ses-a","status":"todo","seq":7}]`,
			reassert: `{"kind":"reassert","status":"backlog","observedStatus":"todo","observedSeq":7}`},
		{key: "LEGION-25", tree: "LEGION-24", phase: "awaiting_person", status: "backlog", shown: "todo", ask: "ask-25", writes: `[{"session":"ses-a","status":"todo","seq":7}]`},
		{key: "OTHER-8", project: "OTHER", phase: "implementing", status: "in_progress", shown: "done", writes: `[{"session":"ses-a","status":"done","seq":3}]`},
	}
	for i, status := range record.OutOfWorkflowStatuses {
		held = append(held, heldIssue{key: fmt.Sprintf("LEGION-%d", 30+i), phase: "implementing", status: status, shown: "todo",
			writes: `[{"session":"ses-a","status":"todo","seq":6}]`})
	}
	for _, h := range held {
		project := h.project
		if project == "" {
			project = "LEGION"
		}
		exec("record "+h.key+" with its held writes", `insert into issues (key, tree, project, title, phase, generation, status, dispatch_status, rank,
			last_dispatch_seq, held_session_writes, awaiting_ask) values ($1, coalesce(nullif($7, ''), $1), $2, $1, $3, 1, $4, $5, 'C', 9, $6::jsonb, nullif($8, ''))`,
			h.key, project, h.phase, h.status, h.shown, h.writes, h.tree, h.ask)
	}

	fence := record.Fence{Generation: 2, Entry: 4, PodUID: "pod-uid-1"}
	carriedFence := record.Fence{Generation: 3, Entry: 5, PodUID: "pod-uid-2"}
	turnAt := time.Date(2026, 11, 21, 8, 30, 0, 0, time.UTC)
	queued := []struct {
		name    string
		issue   string
		payload record.OutboxPayload
		// deleted says the way back deletes the row; refused says the release before refuses it as
		// this release queued it.
		deleted, refused bool
		// after is the payload the release before reads once the way back has run, as JSON; ""
		// for a row the way back leaves as it was queued.
		after string
	}{
		{name: "a phase status write, with the Dispatch status its record held when it was queued", issue: root, payload: record.Fenced(func() record.OutboxPayload {
			write := record.OrdinaryStatusWrite("testing", "in_progress", "")
			write.DispatchStatus = "todo"
			return write
		}(), fence), refused: true, after: `{"kind":"ordinary","status":"testing","observedStatus":"in_progress"}`},
		{name: "a status write with an option", issue: root, payload: record.Fenced(func() record.OutboxPayload {
			write := record.OrdinaryStatusWrite("backlog", "in_progress", "")
			write.UnlinkChildren = true
			return write
		}(), fence), deleted: true, refused: true},
		{name: "a status write the controller asked for, waiting for another", issue: root, payload: record.Fenced(func() record.OutboxPayload {
			write := record.OrdinaryStatusWrite("todo", "backlog", "")
			write.After, write.Controller = 9, true
			return write
		}(), fence), refused: true, after: `{"kind":"ordinary","status":"todo","observedStatus":"backlog"}`},
		{name: "a message", issue: root, payload: record.Fenced(record.MessagePost{Body: "The checks of the head settled."}, fence), refused: true,
			after: `{"body":"The checks of the head settled."}`},
		{name: "a notice", issue: root, payload: record.Fenced(record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}, fence),
			refused: true, after: `{"kind":"phase-finished","role":"tester","phase":"testing"}`},
		{name: "a reparented notice", issue: root, payload: record.Fenced(record.Notice{Kind: record.ReparentedNotice, To: root,
			Reason: "LEGION-1's Dispatch parent moved from LEGION-9 to LEGION-8."}, fence), deleted: true, refused: true},
		{name: "a controller notice", issue: root, payload: record.Fenced(record.ControllerNotice{Kind: record.TickNotice}, fence), refused: true,
			after: `{"kind":"tick"}`},
		{name: "a status-scope controller notice", issue: root, payload: record.Fenced(record.ControllerNotice{Kind: record.StatusScopeNotice,
			Reason: "Dispatch refused LEGION-1's status write: LEGION_STATUS_SCOPE_REQUIRED."}, fence), deleted: true, refused: true},
		{name: "a stop", issue: root, payload: record.Fenced(record.SuperviseRequest{Op: "suspend", Tree: root, Role: claim.RoleImplementer}, fence),
			refused: true, after: `{"op":"suspend","tree":"LEGION-1","role":"implementer","generation":2,"entry":4}`},
		{name: "the carried root's start", issue: carried, payload: record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleImplementer,
			Generation: 3, Phase: phase.Implementing, Task: "Implement it.", Entry: 5},
			after: `{"op":"start","tree":"LEGION-2","role":"implementer","task":"Implement it.","generation":3,"phase":"implementing","entry":5}`},
		{name: "the carried root's cleanup", issue: carried, payload: record.Fenced(record.IssueSuspend{Tree: root, TreeGeneration: 2}, carriedFence),
			refused: true, after: `{"tree":"LEGION-2","generation":3,"treeGeneration":3}`},
		{name: "a suspension without a read pod", issue: root, payload: record.Fenced(record.IssueSuspend{Tree: root, TreeGeneration: 2}, fence),
			refused: true, after: `{"tree":"LEGION-1","generation":2,"treeGeneration":2}`},
		{name: "a suspension with a launcher read pod", issue: root, payload: record.Fenced(record.IssueSuspend{Tree: root, TreeGeneration: 2,
			TurnStart: &turnAt, ReadPodUID: "pod-read-1"}, fence),
			refused: true, after: `{"tree":"LEGION-1","generation":2,"treeGeneration":2,"turnStart":"2026-11-21T08:30:00Z","podUid":"pod-read-1"}`},
		{name: "a workspace check", issue: root, payload: record.Fenced(record.IssueWorkspaceCheck{Tree: root, TreeGeneration: 2, Role: claim.RoleImplementer}, fence),
			refused: true, after: `{"tree":"LEGION-1","generation":2,"treeGeneration":2,"role":"implementer"}`},
		{name: "a launcher workspace check", issue: root, payload: record.Fenced(record.IssueWorkspaceCheck{Tree: root, TreeGeneration: 2,
			TurnStart: turnAt, Launcher: true, Close: true, PodUID: "pod-read-3"}, fence),
			refused: true, after: `{"tree":"LEGION-1","generation":2,"treeGeneration":2,"role":"","turnStart":"2026-11-21T08:30:00Z","launcher":true,"close":true,"podUid":"pod-read-3"}`},
		{name: "a release", issue: root, payload: record.Fenced(record.IssueRelease{Tree: root, TreeGeneration: 2}, fence), refused: true,
			after: `{"tree":"LEGION-1","generation":2,"treeGeneration":2}`},
		{name: "a release with a launcher read pod", issue: root, payload: record.Fenced(record.IssueRelease{Tree: root, TreeGeneration: 2,
			ReadPodUID: "pod-read-2"}, fence), refused: true,
			after: `{"tree":"LEGION-1","generation":2,"treeGeneration":2,"podUid":"pod-read-2"}`},
		{name: "a gate seed", issue: root, payload: record.Fenced(record.GateSeed{ArtifactID: "artifact-1", Step: record.DesignStepSpec, Version: 1}, fence)},
		{name: "a branch", issue: root, payload: record.Fenced(record.IssueBranch{}, fence)},
		{name: "a production check's wake", issue: root, payload: record.Fenced(record.CheckWakeDue{Look: 1, Reason: "the agent's own wake"}, fence),
			refused: true, after: `{"look":1,"reason":"the agent's own wake"}`},
		{name: "a production check's look", issue: root, payload: record.Fenced(record.CheckLook{Look: 1}, fence), refused: true, after: `{"look":1}`},
		{name: "a production check's bound", issue: root, payload: record.Fenced(record.CheckBound{}, fence), refused: true, after: `{}`},
		{name: "a claim release", issue: root, payload: record.Fenced(record.ClaimRelease{}, fence), refused: true, after: `{}`},
		{name: "a role message", issue: root, payload: record.Fenced(record.RoleMessage{Role: "legion-legion-legion-1-implementer",
			Body: "The checks of the head settled."}, fence), refused: true,
			after: `{"role":"legion-legion-legion-1-implementer","body":"The checks of the head settled."}`},
		{name: "a phase's end", issue: root, payload: record.Fenced(record.PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.AwaitingChildren,
			Reason: "LEGION-4 (todo) is open"}, fence), refused: true,
			after: `{"role":"tester","from":"testing","to":"awaiting_children","reason":"LEGION-4 (todo) is open"}`},
		{name: "a hook's prompt", issue: root, payload: record.Fenced(record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer,
			From: phase.Reviewing, To: phase.Implementing, Count: 1, Prompt: "Say what the round missed."}, fence), refused: true},
		{name: "an update's ask", issue: root, payload: record.Fenced(record.UpdateAsk{Role: claim.RoleImplementer, Round: "round-1",
			Text: "Commit and push, then stop."}, fence), refused: true,
			after: `{"role":"implementer","round":"round-1","text":"Commit and push, then stop."}`},
		{name: "a merge forward", issue: root, payload: record.Fenced(record.MergeForwardMessage{Step: record.MergeForwardAsk, Role: claim.RoleImplementer,
			Head: "abc123", Base: "main", Phase: phase.Merging, PullRequest: 42, Why: record.ConflictMergeNext}, fence), refused: true},
		{name: "a merge queue's removal", issue: root, payload: record.Fenced(record.MergeQueueRemoval{Number: 42, Head: "abc123"}, fence), refused: true,
			after: `{"number":42,"head":"abc123"}`},
		{name: "R18's seed", issue: root, payload: record.ReparentSeed{Parent: "LEGION-8", Tree: root, Ran: "its run stays with it", Generation: 2,
			Entry: 4, OldParent: "LEGION-9", Phase: phase.Implementing, Slot: true, Steps: []record.ReparentStep{{Step: record.DesignStepSpec, Open: true}}},
			refused: true, after: `{"parent":"LEGION-8","tree":"LEGION-1","ran":"its run stays with it"}`},
		{name: "a retention's end", issue: root, payload: record.RetentionEnd{Generation: 2, Entry: 4}, deleted: true, refused: true},
		{name: "another project's status write", issue: other, payload: record.Fenced(record.OrdinaryStatusWrite("testing", "in_progress", ""),
			record.Fence{Generation: 1, Entry: 2}), refused: true},
		{name: "another project's retention's end", issue: other, payload: record.RetentionEnd{Generation: 1, Entry: 2}, refused: true},
	}
	ids := make([]int64, len(queued))
	payloads := make([][]byte, len(queued))
	for i, q := range queued {
		row, err := record.NewOutboxRow(q.issue, q.payload, time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("queue %s: %v", q.name, err)
		}
		payloads[i] = row.Payload
		if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3, 0, $4, '') returning id`, string(row.Kind), row.Issue, row.Payload, row.NextAt).Scan(&ids[i]); err != nil {
			t.Fatalf("insert %s: %v", q.name, err)
		}
		if refusal := decodedBeforeTheConversion(row.Kind, row.Payload); q.refused != (refusal != "") {
			t.Fatalf("the release before's decode of %s refuses %q; the control wants it refused %t, or this test cannot see the rows the way back exists for",
				q.name, refusal, q.refused)
		}
	}

	block := strings.ReplaceAll(wayBackOf(t, issueFactsVersion, conversionRelease), rollingBackProject, "LEGION")
	if _, err := store.Pool().Exec(ctx, block, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back the header of migration %d carries: %v", issueFactsVersion, err)
	}

	for i, q := range queued {
		var kind, issue string
		var payload []byte
		err := store.Pool().QueryRow(ctx, `select kind, issue, payload from outbox where id = $1`, ids[i]).Scan(&kind, &issue, &payload)
		if q.deleted {
			if err == nil {
				t.Errorf("the way back left %s (row %d), which the release before refuses at every attempt: %s", q.name, ids[i], payload)
			}
			continue
		}
		if err != nil {
			t.Errorf("read %s (row %d) after the way back: %v", q.name, ids[i], err)
			continue
		}
		if strings.HasPrefix(issue, "OTHER-") {
			if !jsonEqual(t, payload, payloads[i]) {
				t.Errorf("the way back of LEGION changed %s to %s, want it as OTHER's daemon queued it: %s", q.name, payload, payloads[i])
			}
			continue
		}
		if refusal := decodedBeforeTheConversion(record.OutboxKind(kind), payload); refusal != "" {
			t.Errorf("the release before refuses %s after the way back, for %s, and retries it at every attempt: %s", q.name, refusal, payload)
		}
		if q.after != "" && !jsonEqual(t, payload, []byte(q.after)) {
			t.Errorf("%s reads %s after the way back, want %s", q.name, payload, q.after)
		}
		if _, err := record.DecodeOutboxPayload(record.OutboxRow{ID: ids[i], Kind: record.OutboxKind(kind), Issue: issue, Payload: payload}); err != nil {
			t.Errorf("this release's decode of %s after the way back, which the roll forward reads: %v", q.name, err)
		}
	}

	issues := rowsAsText(t, store, `select key || ' ' || tree || ' ' || handed_over_from from issues where key in ('LEGION-1', 'LEGION-2', 'LEGION-3', 'OTHER-7') order by key`)
	if want := []string{"LEGION-1 LEGION-1 ", "LEGION-2 LEGION-2 ", "LEGION-3 LEGION-3 LEGION-1", "OTHER-7 OTHER-7 "}; !slices.Equal(issues, want) {
		t.Errorf("the issues, their stored trees and the trees they left read %q after the way back, want %q: each converted root stores its own key, and the one that never started names the tree it left", issues, want)
	}
	if roots, want := rowsAsText(t, store, `select issue || ' ' || generation || ' ' || gate_carried from handed_over_roots order by issue`),
		[]string{"LEGION-2 3 true", "LEGION-3 2 false"}; !slices.Equal(roots, want) {
		t.Errorf("handed_over_roots reads %q after the way back, want %q", roots, want)
	}
	if lifecycles, want := rowsAsText(t, store, `select tree || ' ' || epoch || ' ' || authority || ' ' || cleanup_started from tree_lifecycles order by tree`),
		[]string{"LEGION-1 1 workflow false", "LEGION-2 1 workflow false", "LEGION-3 1 workflow false"}; !slices.Equal(lifecycles, want) {
		t.Errorf("the tree lifecycles read %q after the way back, want %q: one open for each converted root", lifecycles, want)
	}
	if claims, want := rowsAsText(t, store, `select token || ' ' || tree || ' ' || tree_epoch from claims where project = 'legion' order by token`), []string{
		"legion-legion-legion-1-architect LEGION-1 1", "legion-legion-legion-2-architect LEGION-2 1", "legion-legion-legion-2-implementer LEGION-2 1",
	}; !slices.Equal(claims, want) {
		t.Errorf("the claims read %q after the way back, want %q: each claim of a converted root names its tree, bound to its epoch", claims, want)
	}
	if crossed, want := rowsAsText(t, store, `select token || ' ' || (published_since is not null) || ' ' || (unpublished_since is not null) from claims order by token`), []string{
		"legion-legion-legion-1-architect false false", "legion-legion-legion-2-architect false false", "legion-legion-legion-2-implementer false false",
		"other-other-other-7-architect true true",
	}; !slices.Equal(crossed, want) {
		t.Errorf("the claims' crossings read %q after the way back, want %q: LEGION's cleared, so a later upgrade reads no crossing stamped before the rollback, and OTHER's as its daemon stamped them", crossed, want)
	}
	if conversions, want := rowsAsText(t, store, `select project from tree_conversions order by project`), []string{"OTHER"}; !slices.Equal(conversions, want) {
		t.Errorf("the conversions recorded after the way back are %q, want %q: LEGION's goes, which clears its daemon.convertedAt", conversions, want)
	}
	for _, h := range held {
		rows, err := store.Pool().Query(ctx, `select payload from outbox where kind = 'dispatch_status' and issue = $1`, h.key)
		if err != nil {
			t.Fatalf("read the status writes of %s: %v", h.key, err)
		}
		writes, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			t.Fatalf("read the status writes of %s: %v", h.key, err)
		}
		switch {
		case h.reassert == "" && len(writes) != 0:
			t.Errorf("the way back queued %s for %s (phase %s, status %s, Dispatch showing %s), want nothing", writes, h.key, h.phase, h.status, h.shown)
		case h.reassert != "" && (len(writes) != 1 || !jsonEqual(t, writes[0], []byte(h.reassert))):
			t.Errorf("the way back queued %s for %s, want the one set-back %s", writes, h.key, h.reassert)
		case h.reassert != "":
			if refusal := decodedBeforeTheConversion(record.OutboxKindDispatchStatus, writes[0]); refusal != "" {
				t.Errorf("the release before refuses the set-back of %s's held writes, for %s: %s", h.key, refusal, writes[0])
			}
			if _, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKindDispatchStatus, Issue: h.key, Payload: writes[0]}); err != nil {
				t.Errorf("this release's decode of the set-back of %s's held writes: %v", h.key, err)
			}
		}
	}
	if stillHeld, want := rowsAsText(t, store, `select key || ' ' || held_session_writes::text from issues where held_session_writes <> '[]'::jsonb order by key`),
		[]string{`OTHER-8 [{"seq": 3, "status": "done", "session": "ses-a"}]`}; !slices.Equal(stillHeld, want) {
		t.Errorf("the issues holding session writes after the way back are %q, want %q: every list of LEGION emptied, OTHER's as it was", stillHeld, want)
	}
}

// jsonEqual says whether a and b are the same JSON value, whatever the order of their keys.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}
