package api

import (
	"github.com/sjawhar/legion/daemon/internal/record"
)

// 20261127202231 claim-left: dispatch://INFRA-3144 -- each issue of `GET /legion/v1/state` and of
// `GET /legion/v1/issues/{key}/record` gains `claimLeft`, `{holder?, reason, at}`
// (`api.ClaimLeftView`, projected from the record's `issues.claim_left`, record.Issue.ClaimLeft):
// the outcome of the newest claim release row (record.ClaimRelease, which the outbox's
// releaseClaim runs) that judged the issue's Dispatch holder, when that row left the holder's
// claim. `holder` is the holder Dispatch named, `<kind>:<id>` (dispatch.Claim.Holder), absent where
// it named none: a claim Dispatch shows with no actor, and a read Dispatch refused. `reason` is one
// of eleven words, one for each arm of the release that leaves a claim on purpose: `unread` (a
// claim with no actor named), `person` (a person holds it), `daemon` (the daemon's own session),
// `controller` (a session the project controller's claim recorded), `other-tree` (a session a claim
// under another gate root recorded, current or left behind), `unattributed` (no claim of the
// daemon's project holds the holder's name), `operator-tree` (the operator holds the issue),
// `issue-runs` (a move's row that ran once the issue was in the workflow again), `works-now` (a
// left session's row that found that session the current session of a claim of the issue, or of
// the gate root's tree it stands under on the Dispatch chain, again),
// `held-an-hour` (Dispatch refused the release for an hour) and `refused` (Dispatch refused the
// read or the release with an answer no retry repairs). `at` is the executor's clock when its row
// left the claim. A release that takes the claim, or a row that finds no claim, clears the entry,
// and a left session's row that finds another holder judged nothing and writes nothing; nothing
// else clears it, so the entry is dated by `at` and a reader takes the claim's current holder from
// Dispatch. An issue whose record holds no entry, and one the workflow does not record, carries no
// `claimLeft`. Why: from this change a move out of the workflow and each session the daemon leaves
// behind queue the release of the issue's Dispatch claim, and a release that leaves a claim must
// say why where a role reads its issue (`read_record` hands the record on as the route serves it),
// not only in the daemon's log. A caller whose contract (the one it states, else its pod's) is
// below this entry is served each issue without `claimLeft` on both routes (the entry's band,
// `withoutClaimLeft`), since a reader of an older contract parses the issue with a strict schema
// and would refuse the whole body for a key it does not name. The entry adds one enumerated field,
// the issue's `claimLeft.reason` (`issues.*.claimLeft.reason` of the state, and `claimLeft.reason`
// of the record, which withIssueRecordRows takes from it), its eleven words recorded whole at this
// entry, so no value of it is served down. `packages/contracts/src/legion-api.ts` mirrors the key
// on `legionIssue`, optional, with `reason` a `z.enum` of the eleven words. No route, no request
// and no other key or value of any answer moves.
//
// claimLeftContract is this entry's id, never a contract number: each build numbers the entry by
// its place in main's merge order (entryNumber), and every row below names the entry by this
// constant, so no line moves when another entry merges before it.
const claimLeftContract = 20261127202231

func init() {
	registerEntry(contractEntry{
		id: claimLeftContract, slug: "claim-left",
		bands: []stateNarrowing{
			{contract: claimLeftContract, issue: withoutClaimLeft, dropped: "the issues' claimLeft"},
		},
		enumeratedFields: []EnumeratedField{
			// `reason` of the issue's claimLeft (ClaimLeftView.Reason, record.ClaimLeftReason). It
			// arrived with the key, which is served down whole below this entry
			// (withoutClaimLeft), so no value of it is held back.
			{Answer: AnswerState, KeyPath: issueKeyPath + "claimLeft.reason", Values: claimLeftReasons},
		},
	})
}

// claimLeftReasons is every value of the issue's `claimLeft.reason`, the words of
// record.ClaimLeftReasons, recorded at claimLeftContract, the entry the field arrived at whole.
var claimLeftReasons = valuesFrom(claimLeftContract, record.ClaimLeftReasons()...)
