package api

// 20261125174608 cyber-policy: a claim OpenAI refused for cyber policy fails under a word of its own, and the report of a
// turn that ran no prompt of the daemon's says when the refusal was that one. Two surfaces move.
// `POST /legion/v1/claims/turn-outcome` takes an optional `code`, the class the producer gave a
// refused turn: `cyber_policy` alone (claim.TurnCodeCyberPolicy), which pi-legion sets from Oh My
// Pi's own classifier (`isCyberPolicyRefusal` of `@oh-my-pi/pi-ai/error`, the reading that sets a
// prompt_result's `code`); the daemon takes any other word as no code and warns naming it rather
// than refusing the report. A refused `unprompted` report carrying it fails the claim at once,
// whatever its status, as that refusal's prompt result does, so no later prompt of the daemon's
// ends Oh My Pi's hold and sends the refused content to OpenAI again; one whose turn a prompt ran
// is judged by that prompt's result, as before. The daemon reads every body strictly, so a report
// from this contract that carries `code` is refused by a daemon before 264; the boot gate refuses
// the mixed pair. And `supervise.FailureCause` gains `cyber_policy` (supervise.CauseCyberPolicy),
// which a claim view's `failure.cause` and `pendingCause` of `GET /legion/v1/state` and the issue
// record carry for a claim that failed so, where `provider_error` stood: the controller keeper
// retries such a controller no more, no start of the daemon's own relaunches such a claim
// (supervise.ErrHeldForCyberPolicy), and the operator's `legion claims retry` relaunches the
// controller too. A reader below 264 parses both with a `z.enum` of the seven words before, so a
// caller whose contract (the one it states, else its pod's lowest) is below 264 is served
// `provider_error` in its place (answers.go's failureCauses records the word at 264; shaping.go's
// valueStrips, `withoutUnadmittedFailureCauses`). No key of an answer moves: the answer records of
// 264 are 256's under its own number but for the domains of the four cause fields, which admit
// `cyber_policy`, and its request records add `code` to the turn outcome's.
// (This entry took 264, the lowest number above main's 256 that no open pull request declares. A
// reading of `const DaemonAPIVersion` at refs/pull/<n>/head for each of the 142 of the 484 open
// pull requests of cybermonklabs/legion whose files touch this file or whose file list GitHub cut
// short, made between 2026-11-25T03:41:52Z and 03:45:54Z, found 257 (#4580), 258 (#5164), 259
// (#3712 and #5156), 260 (#3831), 261 (#5332), 262 (#5204), 263 (#2230), 265 (#5285), 266 (#5467),
// 267 (#5168 and #5169), 268 (#5384), 270 (#4065), 271 (#4173, #4180, #4182 and #4193), 272
// (#4049), 273 (#4432), 274 (#3993), 275 (#4392), 276 (#4592), 277 (#4771), 278 (#4596), 280
// (#5307), 281 (#4540) and 283 (#5463) declared, and none 264. Read main's own number and every
// open pull request again at the next merge rather than from this line.)
//
// cyberPolicyContract is the daemon contract that added `cyber_policy` to the words a claim can
// fail under (supervise.CauseCyberPolicy, version.go's 264): a claim view's `failure.cause` and
// `pendingCause` take it, and `POST /legion/v1/claims/turn-outcome` takes `code`. A plugin before
// it reads both causes with a `z.enum` of the seven words before, and refuses the whole body for an
// eighth, so a caller below it is served `provider_error` in its place
// (withoutUnadmittedFailureCauses): the refusal is a provider error that cannot clear, which is
// what such a reader was told of it before. It removed no key and added none to an answer.
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract the value arrived at stays where it is. It moves only with this
// change's own number, and it is the last number of answers.go's SupportedContracts, which names
// the constant rather than its number for that reason.
const cyberPolicyContract = 20261125174608

func init() {
	registerEntry(contractEntry{id: cyberPolicyContract, slug: "cyber-policy"})
}
