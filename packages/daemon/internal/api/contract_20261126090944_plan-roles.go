package api

// 20261126090944 plan-roles: an owner's plan can define a role of its own, and the daemon's answers say so. `POST
// /legion/v1/plans/register` takes a `plan_role` block, whose id is the role's name, whose `type`
// and `model` are the role's type and model and whose text is its prompt, and a step of a task
// names that role as it names any other; it refuses (`PLAN_REFUSED`) a role whose name or type the
// step record cannot hold, a name the deployment or Legion holds already, a name Legion gives a
// second or later agent of another role, a model the deployment does not list, a key whose steps
// name roles of two models or two harnesses, a step of the key owner on a model that is not the
// owner's, and a later version that changes a role a step of the running task names, each with
// its reason. `GET /legion/v1/state`'s `settings` answers `models`, the models a role of a plan
// may name (`models` of `legion.yaml`, then each model a role of `roles` names that it lacks),
// absent while there is none. `POST /legion/v1/claims/fresh-session` refuses a role outside the
// list with `INVALID_ROLE` whose message names a role of the issue's plan beside Legion's six and
// a role of the deployment, and `POST /legion/v1/handoff/complete` refuses a verdict of a role
// that reports none with `VERDICT_ROLE_FORBIDDEN`, whose message reads "only a tester, a role of
// the deployment or a role of the issue's plan reports a verdict": a role the issue's plan defines
// reports pass or fail, as a role of the deployment does. A claim's `role`, an issue's `workers`
// and its `stepRole` can name a role the issue's plan defines. The codes, statuses and keys are
// unchanged, and `models` is a key a plugin's loose settings object passes over, so every contract
// is served the same answers and SupportedContracts names 301 as a number: from this entry its
// last number is 301, after `api.startWaitTokenContract`. The change declared no number of its
// own; the forward merge that brought the base's 285 into it found that these answers had changed
// under 285 and took 301, which the contract allocator gave this change from the block it
// reserved for the stack. Read main's own number and every open pull request again before a push
// rather than from this history.
//
// planRolesContract is the daemon contract at which an owner's plan can define a role of its own
// (version.go's 301): `POST /legion/v1/plans/register` takes a `plan_role` block, and the state's
// `settings` answers `models`. No answer changes its shape, so no band names it.
const planRolesContract = 20261126090944

func init() {
	registerEntry(contractEntry{id: planRolesContract, slug: "plan-roles"})
}
