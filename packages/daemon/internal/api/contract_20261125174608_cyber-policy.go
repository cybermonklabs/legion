package api

import "github.com/sjawhar/legion/daemon/internal/supervise"

// 20261125174608 cyber-policy: a claim OpenAI refused for cyber policy fails under a word of its
// own, and the report of a turn that ran no prompt of the daemon's says when the refusal was that
// one. Two surfaces move. `POST /legion/v1/claims/turn-outcome` takes an optional `code`, the
// class the producer gave a refused turn: `cyber_policy` alone (claim.TurnCodeCyberPolicy), which
// pi-legion sets from Oh My Pi's own classifier (`isCyberPolicyRefusal` of `@oh-my-pi/pi-ai/error`,
// the reading that sets a prompt_result's `code`); the daemon takes any other word as no code and
// warns naming it rather than refusing the report. A refused `unprompted` report carrying it fails
// the claim at once, whatever its status, as that refusal's prompt result does, so no later prompt
// of the daemon's ends Oh My Pi's hold and sends the refused content to OpenAI again; one whose
// turn a prompt ran is judged by that prompt's result, as before. The daemon reads every body
// strictly, so a report from this contract that carries `code` is refused by a daemon before it;
// the boot gate refuses the mixed pair. And `supervise.FailureCause` gains `cyber_policy`
// (supervise.CauseCyberPolicy), which a claim view's `failure.cause` and `pendingCause` of `GET
// /legion/v1/state` and the issue record carry for a claim that failed so, where `provider_error`
// stood: the controller keeper retries such a controller no more, no start of the daemon's own
// relaunches such a claim (supervise.ErrHeldForCyberPolicy), and the operator's `legion claims
// retry` relaunches the controller too. A reader before this entry parses both with a `z.enum` of
// the seven words before, and refuses the whole body for an eighth, so a caller whose contract (the
// one it states, else its pod's lowest) is below it is served `provider_error` in its place
// (shaping.go's withoutUnadmittedFailureCauses, this entry's value strip): the refusal is a
// provider error that cannot clear, which is what such a reader was told of it before. No key of
// an answer moves: the answer records of this entry are the contract's before it but for the
// domains of the four cause fields, which admit `cyber_policy`, and its request record adds `code`
// to the turn outcome's.
const cyberPolicyContract = 20261125174608

// cyberPolicyCause is `cyber_policy` as the four cause fields of a claim view take it, the
// failure record's `cause` and the `pendingCause` beside it, the architect's and each worker's:
// a value this entry adds to each of those rows of answers.go's failureCauses, which the issue
// record's rows of the same fields take too (withValuesAdded).
var cyberPolicyCause = []EnumeratedValue{{Value: string(supervise.CauseCyberPolicy), Since: cyberPolicyContract}}

func init() {
	registerEntry(contractEntry{
		id: cyberPolicyContract, slug: "cyber-policy",
		valueStrips: []valueStrip{{issue: withoutUnadmittedFailureCauses, dropped: droppedFailureCause}},
		enumeratedValues: []enumeratedValues{
			{answer: AnswerState, keyPath: issueKeyPath + "architect.failure.cause", values: cyberPolicyCause},
			{answer: AnswerState, keyPath: issueKeyPath + "workers.*.claim.failure.cause", values: cyberPolicyCause},
			{answer: AnswerState, keyPath: issueKeyPath + "architect.pendingCause", values: cyberPolicyCause},
			{answer: AnswerState, keyPath: issueKeyPath + "workers.*.claim.pendingCause", values: cyberPolicyCause},
		},
	})
}
