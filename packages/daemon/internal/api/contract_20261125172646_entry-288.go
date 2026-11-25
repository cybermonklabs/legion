package api

// 20261125172646 entry-288: `POST /legion/v1/plans/register` (register_plan) takes a grant of the claim role `sub-owner`
// (`claim.RoleSubOwner`) whose agent is the sub-owner of a task of the plan in force
// (`plan.SubOwnerAgent`), for that agent's own issue alone, and answers `403 SUB_OWNER_OUTSIDE_ISSUE`
// to a grant for any other issue. A sub-owner's version changes only the parts of its task: any
// version it gives while the task stands at its report, its retro or its merge is answered
// `409 PLAN_REFUSED` naming the task and where it stands, one that changes whether a task it does
// not write merges is answered `409 PLAN_REFUSED` naming that task, and one given for a task that is
// done or left is answered `409 TASK_DONE`, whose text names the sub-owner's next act. A `plan_task`
// block's `parent` now places it: under a task that names `owner` it is a part of that task, and
// under a task with none it is an element, which no task carries out. `PLAN_REFUSED` gains the
// reasons of a `parent` that names an id no typed block of the version holds, of blocks under each
// other or under themselves, of an element that names steps, an owner or merges, of a need of an
// element, of a task that names both steps and a sub-owner, of a plan whose part can never start,
// of a need of a part of another task, and of two sub-owners of one agent name. This release still
// refuses a task that names `owner` and a task that is a part of another, each with
// `409 PLAN_REFUSED` and a reason that ends "This release of Legion does not carry it out yet."
// (`plan.Supported`), since it starts no sub-owner: until a change starts sub-owners and lifts those
// two refusals, no plan in force names one, and no grant of the role is given. No request or answer
// shape changes with it, and no shape of `GET /legion/v1/state` moves: a caller below 288 is served
// what it was served before.
// The entry's allocated number is 288, given to this change by the architect that allocates the
// numbers, above 267, which the change this one is built on holds. Read the base's number and every
// open pull request's declared number again before pushing a renumber.
const entry288Contract = 20261125172646

func init() {
	registerEntry(contractEntry{id: entry288Contract, slug: "entry-288"})
}
