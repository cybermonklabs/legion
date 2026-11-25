package api

// 20261125163006 approval-route: the approval route. `POST /legion/v1/claims/approval` (`{claimToken, sessionId, secret}`,
// api.ClaimApprovalRequest) answers whether a person's approval of a spec stands for the claim's
// own issue: `{approval: null}`, or `{approval: {document, version, message}}`
// (api.ClaimApprovalResponse). `document` is `dispatch://<KEY>/spec`, or the document's id where a
// carried copy's hand-over row names no root; `version` is the version the person approved; and
// `message` is the approval message with both filled in, in the words of
// `packages/daemon/internal/prompts/approval.md`, which pi-legion places as it is. The route
// authenticates by the claim as `claims/activity` does, refuses as its siblings do (a blank field
// by its name with 400, a wrong secret with claim.InvalidSecret, a stale plugin with the
// stale-contract 409), and reads in one read-only transaction. A child's architect, every phase
// worker, the controller, and a deployment that gates no spec step are answered `approval: null`,
// never refused (record.StandingSpecApproval). Two facts the route reads have no wire shape:
// `handed_over_roots.from_root`, the root a carried gate came from, and the table
// `cleared_spec_approvals`, the spec row a root's re-admission cleared, both added by
// `store/migrations/20261120210115_approval_route_records.up.sql`, whose header carries their way
// back. A plugin of an earlier contract never calls the route, and the boot gate pairs a daemon
// and a plugin only at one number, so no plugin of this entry meets a daemon without it. The
// answer arrived with its route (AnswerClaimApproval), and shapeAnswer serves it as written.
// This entry's fixture records the request and answer. Older records carry neither.
// The work hold also adds a claim source in this entry: the operator claim routes'
// `suspendedBy` and `suspensionHeldBy` take `hold` (`legionSuspendedBy`), a claim the work hold
// (`gates.hold: work`) stopped, or a stop of it held for the agent's turn (supervise.SuspendedByHold,
// added to `claims.suspended_by` by `store/migrations/20261120224328_claim_suspended_by_hold.up.sql`,
// whose header carries its way back). As for contract 48's `released`, no session-facing body
// carries the value: an operator reads it on the operator claim routes, which no plugin calls, and
// `legion claims list` prints such a claim `suspended:hold`. So no record holds the value and no
// value strip is owed for it (answers.go's EnumeratedFields).
// The work hold also adds a fifth word to an issue's `why.state`, on `GET /legion/v1/state` and on
// the record route: `held` (api.WhyHeld), an issue whose work is a phase worker's in a running tree
// the work hold holds (record.WorkHold), whose `reason` names the version that closed the tree's
// design gate. A reader from 85 up to the contract before this entry reads `state` with a
// `z.enum` of the four earlier words and refuses the whole body for another. A reader from 85
// up to 192 requires `state` and `reason` together. The strip below (withoutUnadmittedWhy)
// reads the arrival of `held` from the registry. It serves that issue without `why` to a caller
// whose restore contract (the one it states, else the lowest its pod records) is below this entry.
// It leaves every other issue's `why` as written. Every reader from 85 on takes `why` as optional.
// A caller whose strip is below 85 has lost every `why` to that
// contract's band already, so the strip takes nothing from it. No other key or value moves.
//
// approvalRouteContract identifies this entry. The registry gives it a contract number in each
// build. Code that needs the number reads entryNumber(approvalRouteContract).
const approvalRouteContract = 20261125163006

// workHoldContract is the same entry: the approval route and the work hold arrive together.
const workHoldContract = approvalRouteContract

const droppedWhy = "the why of every issue whose why.state this caller does not admit"

func init() {
	registerEntry(contractEntry{
		id: approvalRouteContract, slug: "approval-route",
		answers: []Answer{
			// Whether a person's approval of a spec stands for a root architect's claim.
			{ID: AnswerClaimApproval, Routes: []string{"POST /legion/v1/claims/approval"}, Since: approvalRouteContract},
		},
		identityAnswers: []AnswerID{AnswerClaimApproval},
		enumeratedValues: []enumeratedValues{{
			answer:  AnswerState,
			keyPath: issueKeyPath + "why.state",
			values:  []EnumeratedValue{{Value: string(WhyHeld), Since: workHoldContract}},
		}},
		valueStrips: []valueStrip{{issue: withoutUnadmittedWhy, dropped: droppedWhy}},
	})
}

const AnswerClaimApproval AnswerID = "claim-approval"
