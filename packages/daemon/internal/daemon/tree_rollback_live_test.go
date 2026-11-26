//go:build e2e

// The live proof of the rollback (k44 of dispatch://INFRA-4730's Verification; R22 of
// dispatch://INFRA-3320): this release boots on a database the release before left, converts it,
// runs work of its own and stops with an unfinished row of each shape it writes; the step
// docs/kubernetes.md gives for a rollback runs; and the release before boots on the database as it
// then is and runs. scripts/e2e/tree-rollback.sh builds this driver with `-tags e2e` from this
// checkout and the release before's driver (tree_release_before_live_test.go laid over the tree of
// the release before, which scripts/e2e/lib/tree-proof.sh finds) from that commit, whose test
// binary it names in
// LEGION_E2E_BEFORE_DRIVER; it owns the nats-server (LEGION_E2E_NATS_URL) and makes the oldest plugin
// contract's root (LEGION_E2E_OLD_TOOL_ROOT); the PostgreSQL is LEGION_TEST_PG_DSN's.
//
// The rig is tree_cutover_live_test.go's (treeRig): Dispatch's parent-status rule is off, since
// production's cut-over and its rollback run before DISPATCH_PARENT_STATUS_RULE is turned on (k33),
// and its DISPATCH_LEGION_STATUS_SCOPE unset, so
// a session's status write of a Legion issue lands. This release reads and writes Dispatch through
// a wrapper (overrides.wrapDispatch, treeHiddenParent) that, once this release has converted and its
// consumer has caught up, answers a read of Q, the parent of K, with a 503 and leaves Q out of every
// listing, so K's chain stays unknown once the driver deletes Q's row from the copy of Dispatch's
// issues; and that answers the first status write of F, a root this release records, with Dispatch's
// 403 LEGION_STATUS_SCOPE_REQUIRED, the refusal whose status-scope notice the outbox queues (with the
// scope unset no real refusal comes). The rollback step's SQL block runs through psql against the
// database, and its relabel loop with a `kubectl` of the driver's own first on PATH, which records
// each label the loop sets on the labels the fake runtime's pods carry (the fake makes no Sandbox):
// what the step can show of the relabel; the `kubectl` call itself needs a cluster.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// treeHiddenParent is the Dispatch client of this release's daemon in the rollback proof: the
// daemon's own, but for the parent it hides once told to (hide), whose read it answers 503 and
// which it leaves out of every listing, and the issue whose next status write it refuses 403
// LEGION_STATUS_SCOPE_REQUIRED once (refuseScopeOnce).
type treeHiddenParent struct {
	dispatch.Client
	mu     sync.Mutex
	parent string
	scope  string
}

// wrap is the overrides.wrapDispatch of the proof: client is the one the daemon built.
func (h *treeHiddenParent) wrap(client dispatch.Client) dispatch.Client {
	h.Client = client
	return h
}

func (h *treeHiddenParent) hide(parent string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.parent = parent
}

func (h *treeHiddenParent) refuseScopeOnce(issue string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scope = issue
}

func (h *treeHiddenParent) hidden(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return key != "" && key == h.parent
}

// refused answers the scope refusal of a status write of key, once.
func (h *treeHiddenParent) refused(key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if key == "" || key != h.scope {
		return nil
	}
	h.scope = ""
	return &dispatch.Error{Status: http.StatusForbidden, Code: statusScopeCode,
		Message: "the token lacks the scope Dispatch asks of a write of a Legion issue's status"}
}

func (h *treeHiddenParent) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	if h.hidden(key) {
		return dispatch.Issue{}, &dispatch.Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: "Dispatch is unavailable"}
	}
	return h.Client.GetIssue(ctx, key)
}

func (h *treeHiddenParent) ListIssues(ctx context.Context, project string, statuses []string) ([]dispatch.IssueSummary, error) {
	issues, err := h.Client.ListIssues(ctx, project, statuses)
	return slices.DeleteFunc(issues, func(issue dispatch.IssueSummary) bool { return h.hidden(issue.Key) }), err
}

func (h *treeHiddenParent) SetStatus(ctx context.Context, key, status string) (dispatch.Issue, error) {
	if err := h.refused(key); err != nil {
		return dispatch.Issue{}, err
	}
	return h.Client.SetStatus(ctx, key, status)
}

func (h *treeHiddenParent) SetStatusAt(ctx context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	if err := h.refused(issue.Key); err != nil {
		return dispatch.Issue{}, err
	}
	return h.Client.SetStatusAt(ctx, issue, status)
}

func (h *treeHiddenParent) ChangeStatus(ctx context.Context, key string, change dispatch.StatusChange) (dispatch.StatusChangeResult, error) {
	if err := h.refused(key); err != nil {
		return dispatch.StatusChangeResult{}, err
	}
	return h.Client.ChangeStatus(ctx, key, change)
}

// oldToolCall runs call through the oldest plugin contract's legion tool (memberRootDriver under
// bun in that contract's root) as the registered architect token holds the claim, and answers
// what the tool printed.
func (r *treeDaemon) oldToolCall(token claim.Token, call map[string]any) registerGateAnswer {
	t := r.t
	t.Helper()
	held := r.world.Claims[token]
	input := map[string]any{"daemonUrl": r.d.base, "dispatchUrl": r.world.DispatchURL, "dispatchToken": scratchAgentToken,
		"claim": map[string]any{"sessionID": held.Session, "tree": held.Tree, "issue": held.Issue, "role": "architect",
			"secret": held.Secret, "claimToken": string(token), "generation": held.Generation}, "call": call}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("encode the old tool's input: %v", err)
	}
	stdout, stderr := runBunDriver(t, "the old tool's driver", oldToolRoot(t), memberRootDriver, t.TempDir(), registerGateToolDeadline, encoded)
	var answer registerGateAnswer
	if err := json.Unmarshal(stdout, &answer); err != nil {
		t.Fatalf("decode the old tool's answer %s: %v\nstderr %s", stdout, err, stderr)
	}
	return answer
}

// treeEffect is one effect the driver queues for the release before to run after the step.
type treeEffect struct {
	issue   string
	payload record.OutboxPayload
}

// treeEffects is an effect of each outbox kind the release before runs, in this release's shape,
// acting on w, a root at work whose architect is its one claim, at run generation, whose document
// is artifact, in project. A kind the release before's check admits that this map has no effect of
// fails the proof: the list follows that check, so it cannot fall behind it.
func treeEffects(t *testing.T, project, w string, architect claim.Token, artifact string, generation uint64) map[string]treeEffect {
	t.Helper()
	now := time.Now().UTC()
	option := record.OrdinaryStatusWrite("done", "in_progress", "")
	option.IncludeChildren = true
	const head, body = "c0ffee0123456789abcdef0123456789abcdef01", "The tree proof's queued effect."
	return map[string]treeEffect{
		string(record.OutboxKindDispatchStatus):     {w, record.OrdinaryStatusWrite("in_progress", "in_progress", "")},
		string(record.OutboxKindDispatchMessage):    {w, record.MessagePost{Body: body}},
		string(record.OutboxKindNotice):             {w, record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}},
		string(record.OutboxKindControllerNotice):   {w, record.ControllerNotice{Kind: record.TickNotice}},
		string(record.OutboxKindSupervise):          {w, record.SuperviseRequest{Op: "suspend", Tree: w, Role: claim.RoleArchitect}},
		string(record.OutboxKindGateSeed):           {w, record.GateSeed{ArtifactID: artifact, Step: record.DesignStepSpec, Version: 1}},
		string(record.OutboxKindPendingBasisSeed):   {w, record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: artifact, BasisArtifactID: artifact, BasisVersion: 1, Ask: "tree-proof-ask", RegisteredAt: now}},
		string(record.OutboxKindPhaseEnded):         {w, record.PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.AwaitingChildren, Reason: body}},
		string(record.OutboxKindLingerClose):        {w, record.LingerClose{Generation: generation}},
		string(record.OutboxKindIssueSuspend):       {w, record.IssueSuspend{Tree: w, TreeGeneration: generation}},
		string(record.OutboxKindWorkspaceRemove):    {w, record.WorkspaceRemove{Linger: generation, Tree: w}},
		string(record.OutboxKindIssueBranch):        {w, record.IssueBranch{}},
		string(record.OutboxKindCheckWake):          {w, record.CheckWakeDue{Look: 1, Reason: body}},
		string(record.OutboxKindCheckLook):          {w, record.CheckLook{Look: 1}},
		string(record.OutboxKindCheckBound):         {w, record.CheckBound{}},
		string(record.OutboxKindRoleMessage):        {w, record.RoleMessage{Role: string(architect), Body: body}},
		string(record.OutboxKindHookPrompt):         {w, record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: 1, Prompt: body}},
		string(record.OutboxKindUpdateAsk):          {w, record.UpdateAsk{Role: claim.RoleImplementer, Round: "round-1", Text: body}},
		string(record.OutboxKindIssueBundle):        {w, record.IssueWorkspaceCheck{Tree: w, TreeGeneration: generation, Role: claim.RoleArchitect}},
		string(record.OutboxKindIssueRelease):       {w, record.IssueRelease{Tree: w, TreeGeneration: generation}},
		string(record.OutboxKindGatedStepsPublish):  {project, record.GatedStepsPublish{Project: project, Steps: []record.DesignStep{}}},
		string(record.OutboxKindClaimRelease):       {w, record.ClaimRelease{}},
		string(record.OutboxKindDesignGatesPublish): {w, record.DesignGatesPublish{Issue: w, Gates: []record.DesignGateDocument{}}},
		string(record.OutboxKindMergeQueueRead):     {w, record.MergeQueueRead{Number: 42, Head: head}},
		string(record.OutboxKindMergeQueueRemoval):  {w, record.MergeQueueRemoval{Number: 42, Head: head}},
		string(record.OutboxKindRoleHoldersPublish): {w, record.RoleHoldersPublish{Issue: w}},
		string(record.OutboxKindStepRecord): {w, record.StepRecord{ID: uuid.NewString(), Task: body, Step: 1, Kind: "step", Role: string(claim.RoleArchitect),
			Generation: generation}},
		string(record.OutboxKindConflictConfirm): {w, record.ConflictConfirm{Number: 42, Head: head, FirstReadAt: now}},
		string(record.OutboxKindMergeForward): {w, record.MergeForwardMessage{Step: record.MergeForwardAsk, Role: claim.RoleImplementer, Head: head, Base: "main",
			Phase: phase.Merging, PullRequest: 42, Why: record.ConflictMergeNext}},
		string(record.OutboxKindPlanRecord): {w, record.PlanRecord{ID: uuid.NewString(), Generation: generation, Source: "tree proof"}},
		// What the release before cannot run, which the step deletes or strips.
		"dispatch_status with an option": {w, option},
	}
}

// queue queues effect, due long after the run ends, as this release queues its kind (record.NewFencedRow,
// which stamps the fence the issue stands at), and answers its id. A kind the store holds one row
// of per issue has that row written over.
func (r *treeDaemon) queue(effect treeEffect) int64 {
	t := r.t
	t.Helper()
	var id int64
	if err := store.Transact(t.Context(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		row, err := record.NewFencedRow(t.Context(), tx, record.NewStore(), effect.issue, effect.payload, time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(t.Context(), `delete from outbox where kind = $1 and issue = $2 and kind in ('gated_steps_publish', 'design_gates_publish', 'role_holders_publish')`,
			string(row.Kind), row.Issue); err != nil {
			return err
		}
		return tx.QueryRow(t.Context(), `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, $2, $3::jsonb, 0, $4, '') returning id`,
			string(row.Kind), row.Issue, string(row.Payload), row.NextAt).Scan(&id)
	}); err != nil {
		t.Fatalf("queue the %T effect on %s: %v", effect.payload, effect.issue, err)
	}
	return id
}

// guideBlock is one fenced block of a section of docs/kubernetes.md: its language, its content and
// the bold line that leads it, the paragraph that says when the block runs ("" for none).
type guideBlock struct {
	language, text, lead string
}

// guideSection is every fenced block of the section of guide that heading opens, in order. A
// section ends at the next heading of its level or above.
func guideSection(guide, heading string) []guideBlock {
	var blocks []guideBlock
	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	inSection, fence, lead := false, false, ""
	var block guideBlock
	var text strings.Builder
	for line := range strings.SplitSeq(guide, "\n") {
		rest := strings.TrimLeft(line, "#")
		switch {
		case !fence && line == heading:
			inSection = true
		case !fence && inSection && rest != line && len(line)-len(rest) <= level && strings.HasPrefix(rest, " "):
			inSection = false
		case inSection && strings.HasPrefix(line, "```"):
			if fence {
				block.text = text.String()
				blocks = append(blocks, block)
				text.Reset()
			} else {
				block = guideBlock{language: strings.TrimPrefix(line, "```"), lead: lead}
			}
			fence = !fence
		case inSection && fence:
			text.WriteString(line + "\n")
		case inSection && strings.HasPrefix(line, "**"):
			lead = line
		}
	}
	return blocks
}

// wayBackHeading opens the section of this release's rollback step, and parentWaitHeading the
// section of INFRA-4728's, the release that holds a parent's start while a child of it is open.
const (
	wayBackHeading    = "#### Rolling back the release that stores no tree"
	parentWaitHeading = "#### Rolling back the release that holds a parent's start while a child of it is open"
)

// parentWaitMigration is INFRA-4728's migration, which a release before that carries INFRA-4728
// embeds. INFRA-4728's step clears the given columns only on a rollback below phaseEntryMigration.
const parentWaitMigration = 20261114232555

// treeWayBack is the rollback step of this release as docs/kubernetes.md gives it: the content of
// each fenced block of its section, by its language, the SQL block and the relabel loop. A section
// that holds no such block, or two, stops the proof, as textcheck/way_back_doc_test.go's reader
// refuses them.
func treeWayBack(t *testing.T, guide string) (sqlBlock, loop string) {
	t.Helper()
	blocks := map[string][]string{}
	for _, block := range guideSection(guide, wayBackHeading) {
		blocks[block.language] = append(blocks[block.language], block.text)
	}
	for _, language := range []string{"sql", "sh"} {
		if len(blocks[language]) != 1 {
			t.Fatalf("the section %q of docs/kubernetes.md holds %d fenced %s blocks, want one", wayBackHeading, len(blocks[language]), language)
		}
	}
	return blocks["sql"][0], blocks["sh"][0]
}

// parentWaitWayBack is the SQL of INFRA-4728's rollback step as docs/kubernetes.md gives it, each
// block in the guide's order: the read of the issues still in awaiting_children, the delete of the
// rows an earlier release refuses, the clear of phases.entry_from, the clear of the given columns
// where the release before does not carry phaseEntryMigration (the guide's "On a rollback below"),
// and the move out of awaiting_children. Its bash read is the one before a deploy, and its sh
// block an operator's retry by hand once the earlier daemon is up, so neither is the step's.
func parentWaitWayBack(t *testing.T, guide string, carries func(int) bool) []string {
	t.Helper()
	var sql []string
	for _, block := range guideSection(guide, parentWaitHeading) {
		if block.language != "sql" {
			continue
		}
		if strings.HasPrefix(block.lead, fmt.Sprintf("**On a rollback below `%d_", phaseEntryMigration)) && carries(phaseEntryMigration) {
			continue
		}
		sql = append(sql, block.text)
	}
	if len(sql) == 0 {
		t.Fatalf("the section %q of docs/kubernetes.md holds no fenced sql block", parentWaitHeading)
	}
	return sql
}

// rollbackStep runs the rollback step of docs/kubernetes.md over the world's database with this
// release stopped: its SQL block through psql, which prints each session write held and what the
// step does with it and each issue with a claim not retired and its stored tree, and then its
// relabel loop, with a `kubectl` first on PATH that records each label the loop sets, applied to
// the labels of the world's pods. It answers what the SQL block printed of the held writes. Where
// the release before does not carry INFRA-4728 (parentWaitMigration is not among its migrations),
// the rollback crosses that release too, so its step runs right after, as an operator runs the
// steps of two releases in the guide's order (parentWaitWayBack).
func (r *treeDaemon) rollbackStep() []string {
	t := r.t
	t.Helper()
	root := os.Getenv(testbuild.RootEnv)
	guide, err := os.ReadFile(filepath.Join(root, "docs", "kubernetes.md"))
	if err != nil {
		t.Fatalf("read the guide: %v", err)
	}
	sqlBlock, loop := treeWayBack(t, string(guide))
	project, err := claim.ProjectToken(r.world.Project)
	if err != nil {
		t.Fatalf("the project token of %s: %v", r.world.Project, err)
	}
	named := strings.NewReplacer("<PROJECT>", r.world.Project, "<project>", string(project))
	dir := t.TempDir()
	file := filepath.Join(dir, "way-back.sql")
	if err := os.WriteFile(file, []byte(named.Replace(sqlBlock)), 0o600); err != nil {
		t.Fatalf("write the step's SQL: %v", err)
	}
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Fatalf("psql is not on PATH, and the step's two blocks run it: %v", err)
	}
	run := func(what, file string) []byte {
		t.Helper()
		printed, err := exec.Command(psql, r.world.DSN, "-X", "-q", "-v", "ON_ERROR_STOP=1", "-At", "-F", "|", "-f", file).CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", what, err, printed)
		}
		treeNote("%s printed:\n%s", what, printed)
		return printed
	}
	printed := run("the step's SQL block", file)
	var held []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(printed)), "\n") {
		if fields := strings.Split(line, "|"); len(fields) == 5 {
			held = append(held, strings.Join(fields, " "))
		}
	}

	bin := filepath.Join(dir, "bin")
	recorded := filepath.Join(dir, "kubectl.calls")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := execfile.Write(filepath.Join(bin, "kubectl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+recorded+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := bin + ":" + filepath.Dir(psql) + ":/usr/bin:/bin"
	command := exec.Command("bash", "-c", "[ \"$(command -v kubectl)\" = \"$1\" ] || { echo \"kubectl resolves to $(command -v kubectl), not the proof's\" >&2; exit 1; }\n"+named.Replace(loop),
		"loop", filepath.Join(bin, "kubectl"))
	command.Env = []string{"PATH=" + path, "DATABASE_URL=" + r.world.DSN, "KUBECONFIG=" + filepath.Join(dir, "no-kubeconfig"), "HOME=" + dir}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("the step's relabel loop failed: %v\n%s", err, output)
	}
	calls, err := os.Open(recorded)
	if err != nil {
		t.Fatalf("the step's relabel loop called no kubectl: %v", err)
	}
	defer calls.Close()
	scanner := bufio.NewScanner(calls)
	for scanner.Scan() {
		var issue, tree string
		for field := range strings.FieldsSeq(scanner.Text()) {
			for selector := range strings.SplitSeq(field, ",") {
				if value, ok := strings.CutPrefix(selector, "legion.dev/issue="); ok {
					issue = value
				}
			}
			if value, ok := strings.CutPrefix(field, "legion.dev/tree="); ok {
				tree = value
			}
		}
		if issue == "" || tree == "" {
			t.Fatalf("the step's relabel loop called kubectl %q, naming no issue and tree", scanner.Text())
		}
		r.world.Labels[issue] = tree
		treeNote("kubectl %s", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the kubectl calls the step's relabel loop made: %v", err)
	}
	if len(r.world.Migrations) == 0 {
		t.Fatal("the release before recorded no migration of its own, so the proof cannot tell which steps the rollback crosses")
	}
	if slices.Contains(r.world.Migrations, parentWaitMigration) {
		return held
	}
	for i, block := range parentWaitWayBack(t, string(guide), func(version int) bool { return slices.Contains(r.world.Migrations, version) }) {
		file := filepath.Join(dir, fmt.Sprintf("parent-wait-%d.sql", i+1))
		if err := os.WriteFile(file, []byte(block), 0o600); err != nil {
			t.Fatalf("write INFRA-4728's step's SQL: %v", err)
		}
		run(fmt.Sprintf("block %d of INFRA-4728's step (the release before does not carry %d)", i+1, parentWaitMigration), file)
	}
	return held
}

// TestTheTreeRollbackLive is k44. Each step prints `== <step>`, what it observed and `ok <step>`.
func TestTheTreeRollbackLive(t *testing.T) {
	rig := newTreeRig(t, "off")
	step := treeStep
	var world *treeWorld
	step("the release before", func() { world = rig.runBefore("TestTreeRollbackReleaseBefore") })
	r := rig.head(world)
	pool := store.NewPool(r.pool)
	key := func(name string) string { return world.key(t, name) }
	a, a2, a4, e, d, k, q, u, w, x := key("A"), key("A2"), key("A4"), key("E"), key("D"), key("K"), key("Q"), key("U"), key("W"), key("X")
	hidden := &treeHiddenParent{}
	r.tune = func(o *overrides) { o.wrapDispatch = hidden.wrap }

	step("this release boots and converts", func() {
		rebindHeldPorts(t, &r.cfg)
		r.boot("this release")
		r.caughtUp()
		state, body := r.state()
		if state.Daemon.ConvertedAt == nil {
			t.Fatalf("this release booted without converting\nstate %s", body)
		}
		hidden.hide(q)
		if _, err := r.pool.Exec(t.Context(), `delete from dispatch_issues where key = $1`, q); err != nil {
			t.Fatalf("delete %s's row from the copy: %v", q, err)
		}
		treeNote("converted at %s; %s's row deleted from the copy, and Dispatch's answers about it hidden from now", state.Daemon.ConvertedAt.UTC().Format(time.RFC3339), q)
	})

	var f string
	step("work of this release's own", func() {
		// A2 moves under E, E's architect releases it, held on X, and the person takes its wait off,
		// so it starts under E: the claims its start makes are made after the change of parent.
		r.reparent(a2, e)
		status, answer := r.d.request(http.MethodPost, "/legion/v1/waves/release",
			api.WaveReleaseRequest{GrantID: r.grant(r.world.token(t, e, claim.RoleArchitect)), Issues: []string{a2}}, false)
		var released api.WaveReleaseResponse
		if status != http.StatusOK || json.Unmarshal(answer, &released) != nil || !slices.Contains(released.Released, a2) {
			t.Fatalf("release_children of %s by %s's architect = %d %s, want it released", a2, e, status, answer)
		}
		dispatchCall(t, world.DispatchURL, http.MethodPatch, "/api/v1/issues/"+a2, map[string]any{"blocked_by": []string{}}, nil)
		r.awaitLaunch(r.world.token(t, a2, claim.RolePlanner))
		r.registerAll()
		for token, tree := range treeRows(t, pool, `select token, tree from claims where issue = $1`, a2) {
			if tree != a {
				t.Fatalf("the claim %s of %s, made after its parent changed to %s, stores the tree %s, want %s", token, a2, e, tree, a)
			}
		}
		treeNote("%s, moved under %s, released there by its architect and its wait taken off, started, and its claims store its first tree %s", a2, e, a)

		n := r.labelled("N", "Pumpkin patch netting", "", x)
		setDispatchStatus(t, world.DispatchURL, n, "todo")
		settled(t, r.d, "record "+n, func(s api.State) bool { _, ok := s.Issues[n]; return ok && !slices.Contains(s.Admission.Active, n) })
		r.reparent(n, a4)
		dispatchCall(t, world.DispatchURL, http.MethodPatch, "/api/v1/issues/"+n, map[string]any{"blocked_by": []string{}}, nil)
		settled(t, r.d, "hold "+n+" under "+a4, func(s api.State) bool { return slices.Contains(placed(s, n), "held") })
		treeNote("%s, recorded after the conversion while it waited on %s, was set under %s, which nobody released, and its wait taken off", n, x, a4)

		r.reparent(u, "")
		treeNote("%s was unlinked", u)

		f = r.labelled("F", "Compost turner gearbox", "")
		hidden.refuseScopeOnce(f)
		architectF := r.admitRoot(f)
		testwait.Eventually(t, "the status-scope notice of "+f+"'s refused write", func() bool {
			return treeCount(t, pool, `select count(*) from outbox where kind = 'controller_notice' and payload->>'kind' = 'status-scope'`) > 0
		})
		told := r.oldToolCall(architectF, map[string]any{"op": "register_gate", "issue": f, "artifactId": dispatchPrimaryArtifact(t, world.DispatchURL, f),
			"version": 1, "step": string(record.DesignStepSpec)})
		if told.IsError || !slices.Contains(told.DaemonRequests, "POST /legion/v1/gates/register") {
			t.Fatalf("the oldest contract's register_gate of %s answered %+v, want it taken", f, told)
		}
		treeNote("%s admitted, its first status write refused 403 once, and the oldest contract's register_gate of it taken", f)

		setDispatchStatus(t, world.DispatchURL, d, "done")
		testwait.Eventually(t, "every claim of "+d+" to retire", func() bool {
			return treeCount(t, pool, `select count(*) from claims where issue = $1 and state <> 'retired'`, d) == 0
		})
		launched := len(r.launches())
		setDispatchStatus(t, world.DispatchURL, d, "todo")
		settled(t, r.d, "take "+d+" back into a slot", func(s api.State) bool { return slices.Contains(s.Admission.Active, d) })
		testwait.Eventually(t, "the architect of "+d+" to launch again", func() bool {
			return slices.ContainsFunc(r.launches()[launched:], func(spec runtime.SpawnSpec) bool { return spec.Issue == d })
		})
		treeNote("%s was closed and reopened", d)

		architectK := r.world.token(t, k, claim.RoleArchitect)
		session := r.world.Claims[architectK].Session
		status, answer = dispatchAnswerWith(t, world.DispatchURL, http.MethodPatch, "/api/v1/issues/"+k,
			map[string]any{"status": "backlog", "actor": map[string]string{"kind": "session", "id": session}}, bearing(scratchAgentToken))
		if status != http.StatusOK {
			t.Fatalf("the backlog of %s by %s = %d: %s", k, session, status, answer)
		}
		testwait.Eventually(t, "this release to hold the backlog "+session+" wrote on "+k, func() bool {
			return treeCount(t, pool, `select count(*) from issues where key = $1 and held_session_writes <> '[]'::jsonb`, k) == 1
		})
		treeNote("%s's backlog on %s, whose chain the copy does not hold, is held", session, k)
		if treeCount(t, pool, `select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'reparented'`) == 0 {
			t.Fatal("this release queued no reparented notice for the changes of parent")
		}
	})

	step("this release stops, with one effect of each kind the release before runs and one row of each shape it does not", func() {
		r.caughtUp()
		r.registerAll()
		r.labelLaunches()
		r.d.stop()
		var generation uint64
		if err := r.pool.QueryRow(t.Context(), `select generation from issues where key = $1`, w).Scan(&generation); err != nil {
			t.Fatalf("read the run of %s: %v", w, err)
		}
		effects := treeEffects(t, world.Project, w, r.world.token(t, w, claim.RoleArchitect), dispatchPrimaryArtifact(t, world.DispatchURL, w), generation)
		world.Queued = map[string]int64{}
		for _, kind := range world.OutboxKinds {
			effect, ok := effects[kind]
			if !ok {
				t.Fatalf("the release before's outbox runs %s, and the proof queues no effect of it: add one to treeEffects", kind)
			}
			world.Queued[kind] = r.queue(effect)
		}
		r.queue(effects["dispatch_status with an option"])
		var moved uint64
		if err := r.pool.QueryRow(t.Context(), `select generation from issues where key = $1`, a2).Scan(&moved); err != nil {
			t.Fatalf("read the run of %s: %v", a2, err)
		}
		seed := record.ReparentSeed{Parent: e, Tree: a, Ran: "it ran under " + a, Generation: moved, Entry: 1, OldParent: a,
			Phase: phase.Planning, Slot: true}
		r.queue(treeEffect{a2, seed})
		for what, query := range map[string]string{
			"a retention_end":                `select count(*) from outbox where kind = 'retention_end'`,
			"a reparented notice":            `select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'reparented'`,
			"a status-scope notice":          `select count(*) from outbox where kind = 'controller_notice' and payload->>'kind' = 'status-scope'`,
			"a status write with an option":  `select count(*) from outbox where kind = 'dispatch_status' and payload ? 'includeChildren'`,
			"a reparent seed of R18's shape": `select count(*) from outbox where kind = 'reparent_seed' and payload ? 'oldParent'`,
			"a held session write":           `select count(*) from issues where held_session_writes <> '[]'::jsonb`,
		} {
			if treeCount(t, pool, query) == 0 {
				t.Fatalf("this release stopped with no unfinished row of %s", what)
			}
		}
		treeNote("queued %d effects, one of each kind %v, and this release's own rows of each new shape", len(world.Queued), world.OutboxKinds)
	})

	step("the rollback step of docs/kubernetes.md", func() {
		world.HeldPrinted = r.rollbackStep()
		if !slices.ContainsFunc(world.HeldPrinted, func(line string) bool {
			return strings.HasPrefix(line, k+" ") && strings.HasSuffix(line, "set back to in_progress")
		}) {
			t.Fatalf("the step printed the held writes %q, without %s's set back to in_progress", world.HeldPrinted, k)
		}
		for what, query := range map[string]string{
			"the conversion's record (daemon.convertedAt)": `select count(*) from tree_conversions where project = $1`,
			"a held session write":                         `select count(*) from issues where project = $1 and held_session_writes <> '[]'::jsonb`,
			"a row of a kind or notice the release before refuses": `select count(*) from outbox where split_part(issue, '-', 1) = $1 and (kind = 'retention_end'
				or payload->>'kind' in ('reparented', 'status-scope') or payload ?| array['includeChildren', 'unlinkChildren', 'includeParent'])`,
		} {
			if n := treeCount(t, pool, query, world.Project); n != 0 {
				t.Fatalf("after the step the database holds %d of %s", n, what)
			}
		}
		if n := treeCount(t, pool, `select count(*) from outbox where kind = 'dispatch_status' and issue = $1 and payload->>'kind' = 'reassert'`, k); n != 1 {
			t.Fatalf("the step queued %d reasserts of %s, want one", n, k)
		}
		treeNote("the step printed %q, queued %s's reassert and cleared daemon.convertedAt", world.HeldPrinted, k)
		world.save(t, rig.path)
	})

	step("the release before after the step", func() { rig.runBefore("TestTreeRollbackReleaseBeforeAfterTheStep") })
	fmt.Println("tree rollback: PASS")
}
