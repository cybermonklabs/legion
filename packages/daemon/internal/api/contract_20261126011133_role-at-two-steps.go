package api

// 20261126011133 role-at-two-steps: a task can name one role at two steps. `POST
// /legion/v1/plans/register` takes a task whose steps name one role twice where at most one of
// their agents starts with it, as `implementer:a reviewer implementer:a`, whose agent of key a
// works steps 1 and 3, or `implementer:a reviewer:b implementer:b`, whose agent b starts as the
// reviewer and becomes the implementer at step 3; it refuses a task in which two agents start with
// one role, with the reason `step <n> of task <id> needs an agent of its own beside the agent of
// step <m>, and both agents start with the role <role>, and this release runs one agent that
// starts with each role for an issue.` (`PLAN_REFUSED`), where a release before it refused every
// task that named a role twice. The daemon keeps a phases row and a report for each step of a
// role (store migration 20261125114500_phase_step_key), so the task of each later step names the
// report of every step before it. `POST /legion/v1/handoff/complete` holds a completion to the
// run whose entry it serves: it refuses the report of the same agent's other step of the role
// with `HANDOFF_STALE_ENTRY`, and a completion of another agent of the role, once the task stands
// at a step of the role that agent does not work, with `HANDOFF_NOT_CURRENT_PHASE`, which names
// that step's agent and says no task comes. A role that stands at two steps names the step's
// place in its completion's key, so the completions of its two steps are two facts.
// `GET /legion/v1/state`'s `workers.<role>` serves the role's row now, the one with the highest
// entry, of the step of the role that started last. No answer changes its shape, so every
// contract is served the same keys and values, and the entry adds no row to a table.
const roleAtTwoStepsContract = 20261126011133

func init() {
	registerEntry(contractEntry{id: roleAtTwoStepsContract, slug: "role-at-two-steps"})
}
