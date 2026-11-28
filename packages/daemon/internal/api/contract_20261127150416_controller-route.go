package api

// 20261127150416 controller-route: dispatch://INFRA-978 -- the daemon serves
// `GET /legion/v1/controller` (controllerRoute, controller.go): the session registered as the
// project's controller, `{"controllerLocator": {runtime, external, sessionId, registeredAt,
// pluginContract}}`, exactly the members of the state's `controllerLocator`, built by the same
// ControllerLocatorOf from the same controller record under the runtime the daemon runs
// (Options.RuntimeName); and `{}`, the key absent as the state leaves it out, while no session
// holds the current capability (ControllerResponse). Like `GET /legion/v1/state` it takes no
// credential, since the whole state already shows that session to any caller. A record the store
// cannot read is the daemon's refusal, 500.
// Dispatch reads the route when a caller that is not a person rejects or re-counts a failure of
// the project's record of deploys and failed changes, and takes the write only from the session
// the route names. It reads this route rather than the state because the observed view its whole
// read asks for carries no `controllerLocator`, a whole read runs only for the reasons of
// Dispatch's ReadReason, and the whole state measured about 7.3 MB and up to 8.34 s, so neither is
// a read to make at each write.
// The answer is AnswerController, which is no row of Answers, the plugin's table alone: no plugin
// parses it, and pi-shared's census of that table holds every row to a parse site of the plugin's
// client. It is the entry's one identity answer, served as written to every contract, since it
// arrived whole with its route; its one door is the route (daemon_contract_header_test.go's
// controllerRouteDoor). No key or value of any other answer moves, and no request a plugin sends
// changes. `packages/contracts/src/legion-api.ts` reads the answer as LegionControllerResponse.
const controllerRouteContract = 20261127150416

func init() {
	registerEntry(contractEntry{
		id: controllerRouteContract, slug: "controller-route",
		identityAnswers: []AnswerID{AnswerController},
	})
}

// AnswerController names the answer of `GET /legion/v1/controller` (ControllerResponse), which
// Dispatch reads. Like AnswerObservedState it is no row of Answers, since no plugin parses it.
const AnswerController AnswerID = "controller"
