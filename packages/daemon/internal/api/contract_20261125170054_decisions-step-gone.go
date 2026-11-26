package api

// 20261125170054 decisions-step-gone: the `decisions` design-gate step is gone, since a person's
// decisions are nodes of the spec, which the `spec` step's approval covers. `gates.design` refuses
// that step by name (config's readDesignSteps), so the step of `POST /legion/v1/gates/register` is
// `criteria` or `spec`, and a call naming `decisions` is refused as a step the deployment does not
// gate (`GATE_STEP_NOT_CONFIGURED`); each `gatedSteps[].step` of `GET /legion/v1/state` and each
// entry of `designGateSteps` on `POST /legion/v1/controller/secret` is one of the same two. The
// plugin narrows with it: `legionDesignStep` (`packages/contracts/src/legion-api.ts`) takes the two
// names of `DESIGN_GATE_STEPS`, and `register_gate` offers no other step. The `design_gates` rows
// and the kept approvals an earlier release wrote of the step stay in the store, which no reader
// of this release is handed (record.Postgres.GatesOf), so no state of this daemon names the step.
//
// The entry adds no row to a table: it takes a value out of what the daemon sends and adds none,
// so a plugin of an older contract reads this daemon's state as before, since it parses the three
// names, and `decisions` stays a recorded value of the gated-step view (designSteps in answers.go),
// because a value, once a contract admits it, stays admitted. A loader of this release refuses a
// file whose `gates.design` names `decisions`, so a deployment takes the step out of the file
// before it pins an image of this release.
const decisionsStepGoneContract = 20261125170054

func init() {
	registerEntry(contractEntry{id: decisionsStepGoneContract, slug: "decisions-step-gone"})
}
