package api

// 20261127193422 watched-workflows: a project names in `legion.yaml` the workflows whose completed
// runs on their repository's default branch reach its controller
// (`projects.<KEY>.watched_workflows`), and the daemon gives that list to the controller in two
// authenticated answers. The controller's answer of `POST /legion/v1/claims/register`
// (`api.ControllerRegisterResponse`) gains `watchedWorkflows`, a list absent when the project
// watches none: one `{owner, repo, workflow, events}` per entry, the owner, the repository and
// the workflow file as the configuration spells them and the trigger events whose runs count, in
// the file's order. Both launch forms are answered it, the daemon's own controller (`controller:
// daemon`) and the one `legion controller start` launches, whatever the runtime, since the list is
// the project's configuration and no launch's. The controller's plugin subscribes one Envoy topic
// for each entry and event,
// `notifications.github.<owner>.<repo>.default_branch.workflow.<workflow>.completed.<event>`,
// once its claim is done. `POST /legion/v1/controller/secret`'s answer (`api.ControllerSecretResponse`)
// gains `watchedWorkflows` too, never absent (`[]` for none), as `designGateSteps` is, which
// `legion controller start` names in the controller's prompt and refuses an answer without; no
// plugin parses that answer. The list never reaches `GET /legion/v1/state`, whose route
// authenticates nobody (settings_test.go's settingsExcluded). No environment variable carries it:
// the sandbox runtime records it with the controller's addresses, so a daemon restarted with
// another list relaunches its controller, and the relaunched controller's registration answers the
// new list. `packages/contracts/src/legion-api.ts` mirrors the key as optional on the
// registration and as required on the secret's answer.
//
// A plugin before this entry is served the controller's registration without the key (the
// controller's row of shaping.go's shapeAnswer, at `entryNumber(watchedWorkflowsContract)`): its
// reader did not ask for the list, and a strict reader refuses a key it does not know. placedByPod
// places the caller of a registration that carries the key by its pod, as one that carries
// `replaces`. The entry removes no key, adds no value to an enumeration, changes no request and
// adds no row to a table.
const watchedWorkflowsContract = 20261127193422

func init() {
	registerEntry(contractEntry{id: watchedWorkflowsContract, slug: "watched-workflows"})
}
