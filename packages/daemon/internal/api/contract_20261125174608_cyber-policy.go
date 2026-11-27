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
// launches such a claim's agent again (supervise.ErrHeldForCyberPolicy), and the operator's
// `legion claims retry` launches the controller's agent again, in a new session. A reader before
// this entry parses both with a `z.enum` of
// the seven words before, and refuses the whole body for an eighth, so a caller whose contract (the
// one it states, else its pod's lowest) is below it is served `provider_error` in its place
// (withoutUnadmittedFailureCauses, this entry's value strip): the refusal is a provider error that
// cannot clear, which is what such a reader was told of it before. No key of an answer moves: the
// answer records of this entry are the contract's before it but for the domains of the four cause
// fields, which admit `cyber_policy`, and its request record adds `code` to the turn outcome's.
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

// droppedFailureCause is what this entry's row of valueStrips names as dropped. It is named rather
// than written in the row because the test that reads the mixed pod's body names the row by it
// (shaped_answers_test.go). It names what the row takes by the table's rule rather than by today's
// word, so the line stays true of a word the table declares later.
const droppedFailureCause = "the claim views' failure cause and pending cause of a word this caller does not admit, served as an older word that stands for it"

// withoutUnadmittedFailureCauses is the issue whose claim views name no failure cause restore's
// reader does not admit, in `failure.cause` or in `pendingCause`. It follows the declared table
// (answers.go's failureCauses, with the values entries add to its rows) and takes any word the
// table holds back from restore (fieldArrivals.heldBack says which words and why): today the
// `cyber_policy` of cyberPolicyContract, whose value strip this is, and tomorrow any word the
// table gives a later arrival. A reader from recordFieldsContract up to that contract reads both
// with a `z.enum` of the seven words before it and refuses the whole body for another. The four
// fields, the architect's and each worker's, take one list, so the architect's `failure.cause`
// answers for all four.
//
// The word is rewritten rather than dropped, as shaping.go's withoutUnadmittedHeldWords rewrites a
// held row: `cause` is no optional key of a failure record that reader would take as absent, and
// an older word does stand for the newer one (failureCauseStandIn). Every other key of the view
// and of its failure record stays, the sentence that names `cyber_policy` among them, since
// `detail` is a string any reader takes whole.
//
// An issue whose views name no such word is returned as it is, with no copy made; a view it
// rewrites gets a failure record of its own, so the state the source answered is left as it was.
// It reports whether it rewrote a view, which is what the log line of this strip is written from
// (tookOff).
func withoutUnadmittedFailureCauses(issue Issue, restore int) (Issue, bool) {
	arrivals := arrivalsAt(issueKeyPath + "architect.failure.cause")
	unadmitted := func(cause supervise.FailureCause) bool {
		return cause != "" && arrivals.heldBack(string(cause), restore)
	}
	return withEachClaimView(issue, func(view ClaimView) ClaimView {
		if view.Failure != nil && unadmitted(view.Failure.Cause) {
			failure := *view.Failure
			failure.Cause = failureCauseStandIn(failure.Cause)
			view.Failure = &failure
		}
		if unadmitted(view.PendingCause) {
			view.PendingCause = failureCauseStandIn(view.PendingCause)
		}
		return view
	})
}

// failureCauseStandIn is the word an older reader is served for a failure cause it does not admit
// (withoutUnadmittedFailureCauses): `provider_error` for `cyber_policy`, since a cyber-policy
// refusal is a provider error that cannot clear and that reader was told it under that word, and
// `unrecorded`, the word for a failure whose cause the reader was not told, for any word the table
// names no stand-in for. Both arrived with the field, so every reader that knows it admits them.
func failureCauseStandIn(cause supervise.FailureCause) supervise.FailureCause {
	if cause == supervise.CauseCyberPolicy {
		return supervise.CauseProviderError
	}
	return supervise.CauseUnrecorded
}
