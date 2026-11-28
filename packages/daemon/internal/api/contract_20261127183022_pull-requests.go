package api

import "github.com/sjawhar/legion/daemon/internal/record"

// 20261127183022 pull-requests: dispatch://INFRA-5910 -- the pull requests of an issue are a set,
// the pull request of each task of its plan, its own and each other on its branch, and each pull
// request the issue links, in any repository (D2), and the state shows each of them. `GET
// /legion/v1/state` and `GET /legion/v1/issues/{key}/record` gain `pullRequests` on each issue
// (api.Issue.PullRequests, api.SetPullRequestView): each pull request of the set
// (record.Store.PullRequestSetsOf), in the order of their repositories, compared without case, and
// numbers, absent where the set holds none. Each is the view every pull request of the state is,
// with two keys more: `task`, the task whose branch it is on in the issue's repository, absent for
// a pull request the issue links that is no task's and for each pull request of an issue with no
// plan rows, and `linked`, whether the issue links it in Dispatch. `reviewDecision` of each is its
// own: the reviewer's for a task's own pull request, and the one a linked pull request that is no
// task's own holds itself (record.PullRequest.ReviewDecision). `pullRequest` keeps its key, and for
// every caller it is the newest open pull request of the set, the one GitHub changed last
// (record.PullRequest.HeadUpdatedAt, a pull request with none the oldest), then the higher number,
// and the issue's merge where none of the set is open (decision 13). So a caller below this entry
// reads, as the issue's one pull request, the newest that is open (D8). `mergeWait` describes that
// pull request, as it described the one `pullRequest` named before. `repo` of a pull request is
// its own repository, which for a pull request the issue links can be any repository.
//
// A caller below this entry, by the contract it states, else its pod's highest, is served each
// issue without `pullRequests` (this entry's band, withoutPullRequests): its strict reader refuses
// a key it does not name, and `pullRequest` is all it reads of an issue's pull requests. The band
// takes no key another band takes.
//
// The entry adds a hold as well: an issue whose close waits, from merging, for a pull request of
// its set, one that is open, whose read is still owed, or whose repository Legion does not serve or
// cannot read, where no merge is in hand to wait with (record.HoldForPullRequests, decisions 9 and
// 14). Such an issue is held with `holdReason` `pull-requests` and stands in `admission.held`
// under the word `pull_requests` (record.HeldPullRequests), with no `heldBy`. Each word is a value
// of an older enumerated field, which this entry adds to answers.go's holdReasons and heldReasons
// (enumeratedValues), so a value strip holds each back from a caller below it, by the contract it
// states, else its pod's lowest: such an issue is served held from merging with no reason
// (withoutUnadmittedHoldReason), as a hold of no reason is served, and its row of
// `admission.held` as `unclassified` with no `heldBy` (withoutUnadmittedHeldWords), since no word
// of that reader names it. `pullRequests.*.state` and `pullRequests.*.mergeState` are enumerated
// rows of this entry (enumeratedFields), with the words of `pullRequest`'s own two fields, each
// recorded whole at it, since the key arrived with the entry. `packages/contracts/src/legion-api.ts`
// mirrors `pullRequests`, the reason and the word. No request a plugin sends moves.
//
// pullRequestsContract is this entry's id.
const pullRequestsContract = 20261127183022

func init() {
	registerEntry(contractEntry{
		id: pullRequestsContract, slug: "pull-requests",
		bands: []stateNarrowing{
			{contract: pullRequestsContract, issue: withoutPullRequests, dropped: "the issues' pullRequests"},
		},
		enumeratedFields: []EnumeratedField{
			// `state` and `mergeState` of each pull request of an issue's set
			// (api.SetPullRequestView; record.PullRequestState and GitHub's own merge state for the
			// head, as `pullRequest` carries them). Both arrived with `pullRequests`, so no value of
			// either is served down.
			{Answer: AnswerState, KeyPath: issueKeyPath + "pullRequests.*.state", Values: setPullRequestStates},
			{Answer: AnswerState, KeyPath: issueKeyPath + "pullRequests.*.mergeState", Values: setMergeStates},
		},
		enumeratedValues: []enumeratedValues{
			{answer: AnswerState, keyPath: issueKeyPath + "holdReason", values: []EnumeratedValue{
				{Value: string(record.HoldForPullRequests), Since: pullRequestsContract},
			}},
			{answer: AnswerState, keyPath: "admission.held.*.reason", values: []EnumeratedValue{
				{Value: string(record.HeldPullRequests), Since: pullRequestsContract},
			}},
		},
	})
}

// setPullRequestStates and setMergeStates are the words of `pullRequest`'s `state` and
// `mergeState` (answers.go's pullRequestStateWords and mergeStateWords) at this entry, which
// `pullRequests` arrived at: no pull request of the set is served below it at all
// (withoutPullRequests).
var (
	setPullRequestStates = valuesFrom(pullRequestsContract, pullRequestStateWords...)
	setMergeStates       = valuesFrom(pullRequestsContract, mergeStateWords...)
)
