package classify

import (
	"sort"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// SettlementClassification states whether a CI settlement can update a head's fence.
type SettlementClassification string

const (
	SettlementStale     SettlementClassification = "stale"
	SettlementDuplicate SettlementClassification = "duplicate"
	SettlementConflict  SettlementClassification = "conflict"
	SettlementNewer     SettlementClassification = "newer"
	// SettlementStatusesOnly is a settlement that names a commit status the pull request does not
	// hold yet (statusNews) and is otherwise no newer than what it keeps: its check runs are older
	// than the ones kept, or the same at a generation no higher than the kept one. Its statuses
	// alone apply, and nothing else of it (ApplySettlement).
	SettlementStatusesOnly SettlementClassification = "statuses-only"
)

// SettlementCandidate is the listener's proposed CI outcome for one commit and its ordering
// identity. Envoy listener settlements are the Go daemon's only input about check runs: it reads
// GitHub's checks nowhere else, and judges none of them - what a settlement names is told to the
// roles that work the issue and read by them (workflow's tellChecks,
// dispatch://INFRA-714/spec R1). The listener publishes a settlement for every settled commit of
// a pull request, its current head or not, and the daemon takes one only for the head itself
// (SettlementFor), since a settlement stands only for the head it names; a settlement of another
// head than the recorded one is applied to the fence the pull request remembers for that head, or
// to an empty one. A check still running is in no settlement at all - the listener settles a
// commit only once every check it has seen there is terminal - so it is no verdict rather than a
// failure. A check_run delivery the listener lost is recovered by Dispatch's webhook redelivery
// sweep (packages/envoy/internal/dispatch/redeliver: it runs only where Dispatch has the App key
// and NATS, every two minutes, and asks GitHub again for each delivery GitHub recorded as failed
// until GitHub records it OK or 72 hours after its first failed attempt; its listing starts an
// hour and six minutes back (redeliver.Lookback) while it runs without a gap, two minutes before
// its stored cursor after one, and 72 hours back on a first sweep with no cursor), by Dispatch's
// CI reconcile once the run has completed (packages/envoy/internal/dispatch/cireconcile: where
// Dispatch has NATS and the App key, a pass every five minutes records each completed check run
// of an open pull request's head that the listener's record of that head lacks), or by the
// check's next run. The listener attributes a check_run with no pull_requests only by its check
// suite's refs/pull/<n>/head branch, and the CI reconcile records any other such run on an open
// pull request's head once it has completed. Nothing on the Go path corrects the rest: a deleted
// check's failure stands until a newer run of that name, and a rerun whose completion neither the
// listener nor the CI reconcile observes holds the last verdict until the check runs again.
type SettlementCandidate struct {
	// Head is the commit the settlement is for, which need not be the pull request's head
	// (SettlementFor).
	Head       string              `json:"head"`
	CheckRuns  []record.AttemptRun `json:"checkRuns"`
	Generation int64               `json:"generation"`
	Snapshot   string              `json:"snapshot"`
	Failing    []string            `json:"failing"`
	Cancelled  []string            `json:"cancelled"`
	// Statuses are the commit statuses the settlement names for Head, in context order, each with
	// GitHub's created_at and with no stamp (record.CommitStatus): the whole list the head's CI
	// record holds, which can be fewer than GitHub holds (record.PullRequest.Statuses says why).
	// A status newer than the one the pull request keeps for its context, or of a context it keeps
	// none of, makes a settlement of an older attempt set, or of the same one at a generation no
	// higher than the kept one, newer for its statuses alone (ClassifySettlement). The list is
	// merged into the kept one per context wherever the settlement applies to the checked head
	// (ApplySettlement). They are no check runs and decide nothing of the fence or what fails in
	// it.
	Statuses []record.CommitStatus `json:"statuses,omitempty"`
}

// CiOutcome is the effective result of a partial listener settlement combined with the stored
// failures and cancellations it omits.
type CiOutcome struct {
	Failing   []string `json:"failing"`
	Cancelled []string `json:"cancelled"`
}

// ClassifySettlement decides whether a listener settlement may update the stored CI fence.
//
// A set that is newer for some check names and older for others is applied rather than thrown
// away. Two settlements of one head are two reports of the same checks arriving in whatever
// order the listener published them, not a disagreement: one can carry a re-run of a check the
// other reported at its first run, and report no run at all of a check the other named. Each
// name is then decided on its own by the newest run of it (mergeAttemptSets, and the conclusion
// that run came to), so no report's facts are lost whichever arrived last.
//
// Commit statuses are decided by their own order, created_at per context (statusNews): a
// settlement can name a status newer than the pull request keeps for its context, or one of a
// context it keeps none of, whatever its generation says. A status has no id, and the listener
// numbers the settlements of a record the bucket's TTL removed and the reconcile wrote again
// afresh, so after such a recreation the generation is no order of a status, and a change of a
// status's state would otherwise be stale or a conflict until the new record's generation passed
// the old one's. So a settlement that names such a status, and that neither its attempt set nor
// its generation makes newer, is newer for its statuses alone (SettlementStatusesOnly): one whose
// attempt set is older, and one of the same attempt set at a generation no higher than the kept
// one. The same set includes one that names fewer checks than the fence, since a name only the
// fence holds is ignored (CompareAttemptSets): a settlement a record sent before a later check
// name appeared compares equal to the fence of the one it sent after. Such a settlement's check
// runs, the names it reports failing and cancelled, its generation and its snapshot are no newer
// view than the kept ones, and taking them would put the fence's numbering, and what a check name
// with no run comes to, back to that view, after which the newer settlement delivered again would
// be newer once more and move the issue for nothing. A recreated record's status changes so apply
// as statuses alone, and its numbering is taken once a newer attempt set or a higher generation
// of it comes, as for its check runs. A settlement older for every check run it reports and every
// status it names is stale, and one of the same attempt set, generation and snapshot that names
// no newer status is the same observation delivered again.
//
// The order of the statuses is read only where the attempt set and the generation leave the
// answer open, so a settlement either of them already makes newer builds no map of the kept
// statuses.
func ClassifySettlement(pr record.PullRequest, in SettlementCandidate) SettlementClassification {
	if pr.CheckRuns == nil {
		return SettlementNewer
	}

	order := CompareAttemptSets(pr.CheckRuns, in.CheckRuns)
	if order == AttemptSetNewer || order == AttemptSetMixed || (order == AttemptSetEqual && in.Generation > pr.Generation) {
		return SettlementNewer
	}
	if statusNews(pr, in) {
		return SettlementStatusesOnly
	}
	if order == AttemptSetOlder || in.Generation < pr.Generation {
		return SettlementStale
	}
	if in.Snapshot == pr.Snapshot {
		return SettlementDuplicate
	}
	return SettlementConflict
}

// statusNews says whether in names a commit status the pull request does not hold yet for
// in.Head: one of a context it keeps no status of for that head, or one created after the status
// it keeps of that context. The kept statuses are those stamped with in.Head
// (record.CommitStatus.StampedWith), so another head's say nothing of it.
//
// Created after is a strictly later created_at, which GitHub gives to the second. So on a record
// the bucket's TTL recreated, a status whose state changed within the same second as the kept
// status of its context is no news, and the settlement naming it waits for the generation rule
// as any settlement that names no newer status does.
func statusNews(pr record.PullRequest, in SettlementCandidate) bool {
	kept := make(map[string]time.Time, len(pr.Statuses))
	for _, status := range pr.Statuses {
		if status.StampedWith(in.Head) {
			kept[status.Context] = status.CreatedAt
		}
	}
	for _, status := range in.Statuses {
		createdAt, held := kept[status.Context]
		if !held || status.CreatedAt.After(createdAt) {
			return true
		}
	}
	return false
}

// EffectiveOutcome preserves the failures and cancellations an incomplete listener observation
// omits: a check the stored settlement names failing or cancelled that the new one does not report
// at all stays as it was.
//
// Each name the fence holds a run for is decided by that run's own conclusion (concludedBy, which
// ApplySettlement layers over this), and this stays apart from it. The two
// answer different questions and only one of them could be folded into the other: a settlement
// may name a check failing and report no run of it - an aggregate job whose needs: never ran, so
// GitHub started nothing - and no run means no conclusion to read, so what the lists say is all
// there is for such a name. Keeping it apart also keeps it the one place that rule lives, which
// the recorded contract fixtures replay by name (contracts/fixtures/classification).
func EffectiveOutcome(pr record.PullRequest, in SettlementCandidate) CiOutcome {
	reported := make(map[string]struct{}, len(in.CheckRuns)+len(in.Failing)+len(in.Cancelled))
	for _, run := range in.CheckRuns {
		reported[run.Name] = struct{}{}
	}
	for _, names := range [][]string{in.Failing, in.Cancelled} {
		for _, name := range names {
			reported[name] = struct{}{}
		}
	}
	kept := func(incoming, stored []string) []string {
		names := append(make([]string, 0, len(incoming)+len(stored)), incoming...)
		for _, name := range stored {
			if _, found := reported[name]; !found {
				names = append(names, name)
			}
		}
		return names
	}
	return CiOutcome{Failing: kept(in.Failing, pr.Failing), Cancelled: kept(in.Cancelled, pr.Cancelled)}
}

// stampedRuns is in's check runs with the two facts the listener's report leaves implicit: the
// head GitHub reported each run on, which is the head the settlement is of, and what the run
// concluded, which the settlement's own lists give - a name in Failing failed, one in Cancelled
// was cancelled, and one in neither concluded without either, since the listener settles a
// commit only once every check it has seen there is terminal. A run in neither list that the
// decoder already took as skipped (intake's decodeChecks, from the settlement's skipped group)
// stays skipped, and every other such run succeeded; the lists come first, so a skipped run whose
// name a list gives is failed or cancelled as the list says. The word GitHub gave the run
// (record.AttemptRun.GitHubConclusion) travels with it unchanged, deciding nothing. They travel
// with the run into the fence, which needs the head to say the run is one of the current head's
// own (ChecksConcludedOnHead) and the conclusion to say what the name's newest run came to
// (mergeAttemptSets, concludedBy).
func stampedRuns(in SettlementCandidate) []record.AttemptRun {
	concluded := make(map[string]record.RunConclusion, len(in.Failing)+len(in.Cancelled))
	for _, name := range in.Cancelled {
		concluded[name] = record.RunCancelled
	}
	// The listener keeps the two lists apart (intake's PullRequestChecks), so a name in both is a
	// report no listener writes; it is read as failed, which is what the roles are told for such
	// a name (workflow's settledChecks) and the conclusion a reader acts on. One run has one
	// conclusion, so that name is failing and not cancelled.
	for _, name := range in.Failing {
		concluded[name] = record.RunFailed
	}
	runs := make([]record.AttemptRun, 0, len(in.CheckRuns))
	for _, run := range in.CheckRuns {
		run.Head = in.Head
		if conclusion, listed := concluded[run.Name]; listed {
			run.Conclusion = conclusion
		} else if run.Conclusion != record.RunSkipped {
			run.Conclusion = record.RunSucceeded
		}
		runs = append(runs, run)
	}
	return runs
}

// mergedStatuses is the statuses pr keeps for in.Head merged with in's: for each context the
// entry with the later created_at, each stamped with in.Head, which is how the pull request keeps
// them (record.PullRequest.Statuses), in context order. On an equal created_at the newer view's
// entry stands: in's where classified says in is applied whole (SettlementNewer), and the kept
// one where it says in changes its statuses alone (SettlementStatusesOnly), since in is then the
// older view. A status stamped with another head is dropped, a context in leaves out stays as it
// is kept, and an older status of a context never puts back a state a newer one replaced. It is
// an empty list where neither names any.
func mergedStatuses(pr record.PullRequest, in SettlementCandidate, classified SettlementClassification) []record.CommitStatus {
	byContext := make(map[string]record.CommitStatus, len(pr.Statuses)+len(in.Statuses))
	for _, status := range pr.Statuses {
		if status.StampedWith(in.Head) {
			byContext[status.Context] = status
		}
	}
	for _, status := range in.Statuses {
		if kept, held := byContext[status.Context]; held && (kept.CreatedAt.After(status.CreatedAt) ||
			(classified == SettlementStatusesOnly && kept.CreatedAt.Equal(status.CreatedAt))) {
			continue
		}
		status.Head = in.Head
		byContext[status.Context] = status
	}
	statuses := make([]record.CommitStatus, 0, len(byContext))
	for _, status := range byContext {
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Context < statuses[j].Context })
	return statuses
}

// concludedBy is what outcome comes to once the fence has spoken: a name the fence holds a
// concluded run for is failing or cancelled exactly as that run concluded, whatever an older run
// of it concluded and whatever list the settlements put it in, and every other name keeps the
// conclusion outcome gives it. The fence is the newest run of each name over the settlements of
// the head it stands at (mergeAttemptSets), so this is where a check re-run and passed stops
// being red and one that failed on its newest run starts being red, whichever settlement arrived
// last.
//
// A name with no run in the fence at all keeps outcome's reading: a settlement can name a check
// failing and report no run of it (EffectiveOutcome), and there is no conclusion to read for
// such a name. So does a name whose newest run is a row stored before runs carried a conclusion,
// which says nothing and so decides nothing (record.AttemptRun).
func concludedBy(fence []record.AttemptRun, outcome CiOutcome) CiOutcome {
	decided := make(map[string]record.RunConclusion, len(fence))
	for _, run := range fence {
		if run.Conclusion != "" {
			decided[run.Name] = run.Conclusion
		}
	}
	names := func(listed []string, conclusion record.RunConclusion) []string {
		var kept []string
		seen := make(map[string]bool, len(listed))
		for _, name := range listed {
			if got, found := decided[name]; found && got != conclusion {
				continue
			}
			if !seen[name] {
				seen[name], kept = true, append(kept, name)
			}
		}
		// The fence is sorted by name, so a name the lists never carried joins in name order.
		for _, run := range fence {
			if run.Conclusion == conclusion && !seen[run.Name] {
				seen[run.Name], kept = true, append(kept, run.Name)
			}
		}
		return kept
	}
	return CiOutcome{Failing: names(outcome.Failing, record.RunFailed), Cancelled: names(outcome.Cancelled, record.RunCancelled)}
}
