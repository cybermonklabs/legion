package api

import "github.com/sjawhar/legion/daemon/internal/supervise"

// 20261125174608 cyber-policy: a claim OpenAI refused for cyber policy fails under a word of its
// own, and a turn's report says when the refusal was that one. Two surfaces move. `POST
// /legion/v1/claims/turn-outcome` takes an optional `code`, the class the producer gave a refused
// turn: `cyber_policy` alone (claim.TurnCodeCyberPolicy), which pi-legion sets from Oh My Pi's
// own classifier (`isCyberPolicyRefusal` of `@oh-my-pi/pi-ai/error`, the reading that sets a
// prompt_result's `code`); the daemon takes any other word as no code and warns naming it rather
// than refusing the report. A refused report carrying it fails the claim at once, whatever its
// status — the report of a take's turn taking that task back first, as for a status that cannot
// clear, and the report of any other turn alike — so no later take ends Oh My Pi's hold and sends
// the refused content to OpenAI again. The daemon reads every body strictly, so a report from
// this contract that carries `code` is refused by a daemon before it; the boot gate refuses the
// mixed pair. And `supervise.FailureCause` gains `cyber_policy` (supervise.CauseCyberPolicy),
// which a claim view's `failure.cause` and `pendingCause` of `GET /legion/v1/state` and the issue
// record carry for a claim that failed so, where `provider_error` stood: the controller keeper
// retries such a controller no more, no start of the daemon's own launches such a claim's agent
// again (supervise.ErrHeldForCyberPolicy), and the operator's `legion claims retry` launches the
// controller's agent again, in a new session. A reader before this entry parses both with a
// `z.enum` of the words before it, and refuses the whole body for another, so a caller whose
// contract (the one it states, else its pod's lowest) is below it is served `provider_error` in
// its place by the one strip of the failure cause (shaping.go's withoutNeverReconnected and its
// olderCause, which take every word the table holds back): the refusal is a provider error that
// cannot clear, which is what such a reader was told of it before. Every other key of the view and
// of its failure record stays, the sentence that names `cyber_policy` among them, since `detail`
// is a string any reader takes whole. No key of an answer moves: the answer records of this entry
// are the contract's before it but for the domains of the four cause fields, which admit
// `cyber_policy`, and its request record adds `code` to the turn outcome's.
const cyberPolicyContract = 20261125174608

// cyberPolicyCause is `cyber_policy` as the four cause fields of a claim view take it, the
// failure record's `cause` and the `pendingCause` beside it, the architect's and each worker's:
// a value this entry adds to each of those rows of answers.go's failureCauses, which the issue
// record's rows of the same fields take too (withValuesAdded).
var cyberPolicyCause = []EnumeratedValue{{Value: string(supervise.CauseCyberPolicy), Since: cyberPolicyContract}}

func init() {
	registerEntry(contractEntry{
		id: cyberPolicyContract, slug: "cyber-policy",
		enumeratedValues: []enumeratedValues{
			{answer: AnswerState, keyPath: issueKeyPath + "architect.failure.cause", values: cyberPolicyCause},
			{answer: AnswerState, keyPath: issueKeyPath + "workers.*.claim.failure.cause", values: cyberPolicyCause},
			{answer: AnswerState, keyPath: issueKeyPath + "architect.pendingCause", values: cyberPolicyCause},
			{answer: AnswerState, keyPath: issueKeyPath + "workers.*.claim.pendingCause", values: cyberPolicyCause},
		},
	})
}
