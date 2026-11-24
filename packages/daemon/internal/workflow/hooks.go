package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// fireWorkBackHooks fires each hook of the deployment that stands on the work-back event and
// counts this move, now that recordPass has counted it and the move's start has written the agent
// of the round it opened (transition). counts is the task's whole backward-move map after the move
// (record.ReturnCounts.BackwardMoves), which is what each hook reads its own number from.
//
// A hook fires when the move is one its Moves selects and the counts of the moves it selects,
// added up after this move, are a positive whole multiple of its Every. Two hooks on one event
// each fire on their own moves and their own multiple, so a deployment reads each firing as the
// count of what that hook watches and of nothing else. The transition calls this for no conflict
// round: such a round raised no pair (recordPass), so nobody sent the work back and no hook counts
// it.
//
// A hook whose Every is not positive fires never. The loader refuses such a hook
// (config's readHookEvery), so this is the guard that keeps a hook written by any other route
// from dividing by nothing rather than a rule of its own.
func (e *Engine) fireWorkBackHooks(ctx context.Context, tx pgx.Tx, issue record.Issue, from, to phase.Phase, counts map[string]int, pr *record.PullRequest) error {
	pair := phase.Pair(from, to)
	for _, hook := range e.cfg.Hooks {
		if hook.On != HookEventWorkBack || hook.Every <= 0 || !hookSelects(hook, pair) {
			continue
		}
		count := hookCount(hook, counts)
		if count <= 0 || count%hook.Every != 0 {
			continue
		}
		if err := e.fireHook(ctx, tx, issue, hook, from, to, count, counts, pr); err != nil {
			return err
		}
	}
	return nil
}

// hookSelects says whether hook counts the move spelled pair. A hook that names no move selects
// every move back, which is what `moves: any` and a hook with no `moves` key at all read as.
func hookSelects(hook Hook, pair string) bool {
	return len(hook.Moves) == 0 || slices.Contains(hook.Moves, pair)
}

// hookCount is the hook's own number of moves back after the move just counted: the counts of the
// moves it selects, added up. A hook over every move adds up the whole map. A move named twice in
// one hook's `moves` is added once, since it is one kind of move however often the operator wrote
// it down.
func hookCount(hook Hook, counts map[string]int) int {
	if len(hook.Moves) == 0 {
		count := 0
		for _, pair := range counts {
			count += pair
		}
		return count
	}
	count, read := 0, make([]string, 0, len(hook.Moves))
	for _, pair := range hook.Moves {
		if slices.Contains(read, pair) {
			continue
		}
		read = append(read, pair)
		count += counts[pair]
	}
	return count
}

// fireHook writes one firing, in the move's own transaction: one record.HookPrompt for each role
// the hook names, in the hook's own order with repeats dropped, and then one message on the issue
// naming what fired and whom it reached, or that it reached none.
//
// A hook names roles of the deployment. A role the plan in force of the issue defines is the
// plan's role, even where the deployment defines a role of that name (plan.Overrides, of the one
// rule every reader of a role asks), and it takes none of the settings the deployment keys by that
// name, a hook's prompt among them, so the firing passes it over on that issue
// (record.Store.PlanRoleOf). Legion's names are no plan's and read no plan.
//
// Each prompt names the claim of the agent that does its role's work in the round the move opened,
// read once the move's start has written that agent, and the outbox publishes on that claim's
// topic: a prompt resolved when it runs would reach whichever agent a later entry of the role's
// step names by then, in a round the words were not written for. For one of Legion's roles of a
// step (the planner, the implementer, the tester and the reviewer) that agent is the agent of the
// task's step that does the role's work where the move left it (workAgent): a step of the role's
// type whose role is the role itself or a role the deployment or the plan defines, whatever it is
// called, so a task whose step of the type testing is a checker's prompts the checker for the
// tester, the owner where that step's key is owner, and nobody for a role whose work no step of
// the task does. The claim is the one the start of that step names, whether or not the step has
// run, and whether a role has a session is not read here: a claim no session holds is ended
// undelivered by the outbox with a log line, as any role message is. Every other role a hook names,
// a role of the deployment or the merger, names the claim of the worker on its role's step
// (roleRowWorker): the claim its phases row names, and the owner's own claim where that row runs a
// step the owner works, whatever claim the row names there.
//
// The counts map is the task's own (recordPass) and is handed to each row as it stands: the outbox
// row encodes it at once (record.NewOutboxRow), and nothing writes to it after the move that
// raised it, so the firing copies nothing.
func (e *Engine) fireHook(ctx context.Context, tx pgx.Tx, issue record.Issue, hook Hook, from, to phase.Phase, count int, counts map[string]int, pr *record.PullRequest) error {
	// The pull request is named as GitHub names it, owner/name#n, and is left out for an issue
	// whose record holds none: a hook can fire on a move made before the pull request exists.
	pull := ""
	if pr != nil {
		pull = fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
	}
	prompted := make([]string, 0, len(hook.Roles))
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	for _, role := range hook.Roles {
		if slices.Contains(prompted, string(role)) {
			continue
		}
		if !plan.IsLegionName(string(role)) {
			_, planned, err := e.store.PlanRoleOf(ctx, tx, issue.Key, string(role))
			if err != nil {
				return err
			}
			if plan.Overrides(string(role), planned) {
				continue
			}
		}
		var worker claim.Token
		if _, step := plan.BuiltinRole(string(role)); step && claim.IsBuiltin(role) {
			// A role of Legion's that does a step's work is the agent of the task's step that does
			// that work where the move left it (workAgent), as a merge forward's ask and a
			// settlement's message name the implementer. A task with no step of that work has no
			// such agent, since no worker of it does that work to take back, and so the firing
			// prompts nobody for that role there, as those senders tell nobody.
			agent, named, err := e.workAgent(ctx, tx, issue, w, to, role)
			if err != nil {
				return err
			}
			if !named {
				continue
			}
			worker = agent.claim
		} else if worker, err = e.roleRowWorker(ctx, tx, issue, w, role); err != nil {
			return err
		}
		prompted = append(prompted, string(role))
		if err := e.enqueue(ctx, tx, issue.Key, record.HookPrompt{Hook: hook.Name, Role: role, Claim: worker, From: from, To: to,
			Count: count, Counts: counts, PullRequest: pull, Prompt: hook.Prompt}); err != nil {
			return err
		}
	}
	reached := strings.Join(prompted, ", ")
	if len(prompted) == 0 {
		reached = "none"
	}
	// One fixed line, so that a reader of a fleet can grep the firings out of the issues' messages
	// and count them, which is what today's cap message is read by in production. The count is the
	// hook's own, so it is a whole multiple of that hook's `every`.
	return e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: fmt.Sprintf(
		"Legion hook %s fired at %d moves back: %s to %s. Prompted: %s.",
		hook.Name, count, from, to, reached)})
}

// roleRowWorker is the claim of the worker on role's step in issue's task on w, issue's walk, for a
// role a hook names that is none of Legion's roles of a step (fireHook): a role the deployment
// defines, or the merger. It is the claim role's phases row names (record.WorkerClaim), and the
// owner's own claim (record.OwnerClaim) where that row runs a step of w's task the owner works
// (Walk.OwnerAt), a step of the key owner. There the row names the claim the step's start wrote,
// which can be the architect above (ownerAgent), an architect that does not do the step, while the
// owner does it, since its owner-step and its report are the owner's, and every message to the
// worker of an owner's step reaches the owner (record.Issue.StepWorker). The merger's row runs the
// merge, place 0, which is never the owner's.
func (e *Engine) roleRowWorker(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, role claim.Role) (claim.Token, error) {
	rows, err := e.store.Phases(ctx, tx, issue.Key)
	if err != nil {
		return "", err
	}
	if row := record.PhaseRowOf(rows, role); row.StepTask != "" && row.StepTask == w.Task && row.StepPlace > 0 &&
		w.OwnerAt(record.TaskPlace{Kind: record.PlaceStep, Position: row.StepPlace}) {
		return record.OwnerClaim(issue)
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", err
	}
	return record.WorkerClaim(project, issue.Key, role, rows)
}
