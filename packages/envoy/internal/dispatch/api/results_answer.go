package api

import (
	"context"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// everyCheckWhen is the `when` answerIssueChecks is given by a caller that reads every check of
// the document, whatever its `when`: the results route.
const everyCheckWhen = ""

// answerIssueChecks computes the body GET /api/v1/issues/{key}/results answers for issueKey: the
// checks of the issue's Verification document as the block index holds them, each with its
// results, what they are compared with and whether a pass of it stands, at at (noCommitNamed for
// the heads and branches each check compares with). held is false where the issue has no
// Verification document, and the body then holds no check. trees is the request's display tree
// reader, nil for a caller that asked for no tree.
//
// It is the whole computation of the read, kept apart from the route so the check states
// settler (check_states.go) publishes the very states the read answers. The route keeps what is
// the route's alone: the query, the 404 for an issue nobody has, and the answer without the
// computed state under an end-to-end test's hide-state file (listCheckResults).
//
// former is the settler's alone, nil for the route: the former heads of issueKey's open pull
// requests (formerHeadsOf), each a pull request's row at a commit it has left. A `before_merge`
// check compares its results with each of them and reads a check CI proves at each, as at an open
// head, so each is a subject whose state the settler publishes there. A result a former head's run
// derives counts at that head alone (deriveCIResults), so at an open head every state is computed
// from the results the route computes it from, and is the one the route answers.
//
// when is the `when` of the checks the caller reads, everyCheckWhen for every check. The settler
// reads its `before_merge` checks alone, the only ones it publishes a state of. A check compared
// with branches would have the settler ask GitHub where each of its result commits stands and
// store the answer (delivery.Branches.CommitOnBranch) on every settle, a read whose answer it
// throws away and that nobody asked for, and the stored answer then decides what the route
// answers while the repository cannot be read. Nothing a `before_merge` check is computed from is
// read through a check of another `when` (learn, collectChangeBranches and deriveCIResults read
// `before_merge` checks alone, and comparedWith keeps the bases of each `when` apart), so its state
// is the one the route answers.
func (s *server) answerIssueChecks(ctx context.Context, issueKey, at string, trees *displayTreeReader, former []delivery.AttributedHead, when string) (issueCheckResults, bool, error) {
	artifact, held, err := s.verificationDocument(ctx, s.deps.Store.Pool, issueKey, "")
	if err != nil {
		return issueCheckResults{}, false, err
	}
	body := issueCheckResults{Checks: []model.CheckBlock{}, Issue: issueKey, VerificationArtifactID: artifact.ID}
	if !held {
		return body, false, nil
	}
	checks, err := s.checkBlocksOf(ctx, s.deps.Store.Pool, artifact, trees)
	if err != nil {
		return issueCheckResults{}, false, err
	}
	if when != everyCheckWhen {
		checks = slices.DeleteFunc(checks, func(check model.CheckBlock) bool { return check.When != when })
	}
	if err != nil {
		return issueCheckResults{}, false, err
	}
	recorded, err := loadCheckResults(ctx, s.deps.Store.Pool, artifact.ID)
	if err != nil {
		return issueCheckResults{}, false, err
	}
	retracted, err := loadCheckResultRetractions(ctx, s.deps.Store.Pool, artifact.ID)
	if err != nil {
		return issueCheckResults{}, false, err
	}
	for index := range checks {
		results := recorded[checks[index].BlockID]
		if results == nil {
			results = []model.CheckResult{}
		}
		for result := range results {
			if retraction, held := retracted[results[result].ID]; held {
				results[result].Retracted = &retraction
			}
		}
		checks[index].Results = results
	}
	// The reader learns the heads GitHub names before any check CI proves is derived
	// (deriveCIResults). A derived result is read at the heads of the issue's pull requests, and a
	// head this read's read-through learns may be one of them: derived first, a first read would
	// answer that Dispatch holds no head while it names the head it learned as another check's
	// basis. A derived result is at one of those heads, which the learned heads already hold, so it
	// leaves nothing to learn. Every head GitHub has to be asked for is learned before the first
	// check is answered, and where the learn stored or learned any head every check's bases are read
	// again, so each check of the answer is answered on the heads as they stand after this read's
	// read-throughs, and two `before_merge` checks of one issue name one list of bases and not two
	// (comparisonReader.learn). The reader holds the read's memo of merge times too
	// (comparisonReader.fills), so the CI read of each check and the comparison after it learn each
	// merge time once (fillMergeTimes). The former heads are the reader's before it learns, since
	// headBases adds them to the issue's heads.
	reader := newComparisonReader(ctx, s, at)
	reader.former = map[string][]delivery.AttributedHead{issueKey: former}
	reader.learn(ctx, checks)
	headCI, headAlone, err := s.deriveCIResults(ctx, reader, issueKey, checks, former)
	if err != nil {
		return issueCheckResults{}, false, err
	}
	reader.headCI, reader.headAlone = headCI, headAlone
	for index := range checks {
		reader.answer(ctx, &checks[index])
	}
	answerCompatibilityCurrency(checks, at)
	keepFieldsOlderCommandsRead(checks)
	// Each result's observed text is registered for its tree once no check's results are copied
	// any more, since what is registered is a pointer into the row: deriveCIResults replaces a
	// check's results, and keepFieldsOlderCommandsRead moves them into new lists, results and the
	// recorded_results of a check CI proves, whose results take their trees as well.
	for index := range checks {
		for result := range checks[index].Results {
			trees.text(&checks[index].Results[result].ObservedTree, checks[index].Results[result].Observed)
		}
		for result := range checks[index].RecordedResults {
			trees.text(&checks[index].RecordedResults[result].ObservedTree, checks[index].RecordedResults[result].Observed)
		}
	}
	body.Checks = checks
	trees.resolve()
	return body, true, nil
}
