package cistore

import (
	"strconv"

	"github.com/sjawhar/envoy/internal/contracts"
)

// standing is whether GitHub counts a check run, or a check suite, for the pull request whose head
// a record is of: whether GitHub's own rollup of the pull request holds it.
type standing int

const (
	// counted: GitHub's rollup of the pull request holds it. A check run of any App but GitHub
	// Actions is, whatever started it.
	counted standing = iota
	// unknown: it is of GitHub Actions, or of no App the observation named, and the record does not
	// hold the event that started its suite's workflow run, or it names no suite at all. The record
	// counts it, as it counted every check run before it read events, until it learns the event.
	unknown
	// uncounted: it is of a workflow run whose event GitHub does not count for a pull request
	// (EventCounted), a workflow_dispatch run of the pull request's branch among them. The record
	// holds no such check run in Checks and no such suite in Suites, so it takes no part in the
	// head's checks, its settlement (settlementReady, terminal), its snapshot (Hash) or its quiet
	// period.
	uncounted
)

// EventCounted reports whether GitHub's rollup of a pull request counts the check runs of a GitHub
// Actions workflow run that event started, which every event but two does. The two were measured
// on cybermonklabs/legion against GitHub's own statusCheckRollup of the pull request at that head:
// workflow_dispatch (#3513 at a368687e, 2026-11-21: the dispatched run's ci-ok is not in the
// rollup, the pull_request run's is) and pull_request_review_comment (#2167 at 57f64b53: of four
// pr-title runs only the two of pull_request_review suites are in the rollup).
//
// GitHub documents six events whose runs it evaluates for a pull request, in "Troubleshooting
// required status checks", section "Checks from some workflow jobs are not evaluated"
// (https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks):
// push, pull_request, pull_request_review, pull_request_target, deployment and
// deployment_status. That list is not the whole: the rollup counts the runs of a `dynamic` suite,
// which CodeQL's default setup makes (#2803 at aa4cd30f holds a required ci-ok in one, #2725 at
// 48308c23 fails on one). So the rule names the events measured as not counted rather than the
// events documented as counted: an event left out wrongly would make a red head read green, and
// one counted wrongly keeps only what the store did before it read events.
func EventCounted(event string) bool {
	switch event {
	case "workflow_dispatch", "pull_request_review_comment":
		return false
	}
	return true
}

// standingOf is the standing of a check run or check suite of the App appID in the suite suiteID,
// by the events st holds (State.SuiteEvents). GitHub's restriction is on the checks of workflow
// jobs alone, so a named App other than GitHub Actions is counted. Only a GitHub Actions suite has
// a workflow run, so an event the record holds for suiteID decides for an observation that named
// no App too.
func (st *State) standingOf(appID, suiteID string) standing {
	if appID != "" && appID != contracts.GithubActionsAppID {
		return counted
	}
	if suiteID == "" {
		return unknown
	}
	event, known := st.SuiteEvents[suiteID]
	switch {
	case !known:
		return unknown
	case EventCounted(event):
		return counted
	}
	return uncounted
}

// learnRunEvent records event, the event that started the workflow run of the check suite
// suiteID, and applies what it says. changed reports whether st changed, and moved whether its
// checks or suites did, which re-arms the head (touch). An event the record already holds for the
// suite changes nothing: GitHub never changes the event of a run.
//
// A counted suite's check runs that stand for a name now stand for good, so nothing waiting for
// those names can take their place and the record drops what waits for them. An uncounted suite
// leaves the record: its suite entry, its check runs that wait, and each of its check runs that
// stands for a name, in whose place the newest check run waiting for that name stands
// (standInstead). That is how a record that counted the suite before it knew the event, a record
// written before the store read events among them, is corrected.
func (st *State) learnRunEvent(suiteID, event string) (changed, moved bool) {
	if suiteID == "" || event == "" {
		return false, false
	}
	if _, known := st.SuiteEvents[suiteID]; known {
		return false, false
	}
	if st.SuiteEvents == nil {
		st.SuiteEvents = map[string]string{}
	}
	st.SuiteEvents[suiteID] = event
	if EventCounted(event) {
		for name, check := range st.Checks {
			if check.SuiteID == suiteID {
				st.dropWaiting(name)
			}
		}
		return true, false
	}
	for id, check := range st.Waiting {
		if check.SuiteID == suiteID {
			delete(st.Waiting, id)
		}
	}
	if _, held := st.Suites[suiteID]; held {
		delete(st.Suites, suiteID)
		moved = true
	}
	for name, check := range st.Checks {
		if check.SuiteID == suiteID {
			st.standInstead(name)
			moved = true
		}
	}
	return true, moved
}

// applyCheckRun folds one check run's observation into st: first the event of its suite's workflow
// run, when the observation carries it (learnRunEvent), and then the run itself. changed reports
// whether st changed, and moved whether the head's checks did, which re-arms the head (touch).
//
// A counted or unknown run stands for its name as before: the newest check-run id, and for one id
// the newest observation (observationMayReplace). An uncounted run stands for nothing and changes
// nothing, unless the record took it before its suite's event was known, in which case it leaves.
// Which of two runs of one name stands does not depend on the order they and their suites' events
// arrive in: a run that loses to a run of a suite whose event the record does not know waits
// (wait), and stands if that suite turns out uncounted. A run that loses to a counted one never
// stands, since a counted run stays counted, and is dropped as before.
//
// A run already recorded that the observation tells only its suite and App changes the record and
// not the head's checks (sameView), so a head settled before the record kept them is not settled
// again for them.
func (st *State) applyCheckRun(observation contracts.CIObservation) (changed, moved bool) {
	changed, moved = st.learnRunEvent(observation.SuiteID, observation.RunEvent)
	if st.Checks == nil {
		st.Checks = map[string]Check{}
	}
	key := observation.CheckName
	next := Check{
		Name:       observation.CheckName,
		CheckRunID: checkRunID(observation.CheckRunID),
		URL:        observation.URL,
		Status:     observation.Status,
		Conclusion: observation.Conclusion,
		ObservedAt: observation.ObservedAt,
		SuiteID:    observation.SuiteID,
		AppID:      observation.AppID,
	}
	current, held := st.Checks[key]
	nextStanding := st.standingOf(next.AppID, next.SuiteID)
	if nextStanding == uncounted {
		if held && current.CheckRunID == next.CheckRunID {
			st.standInstead(key)
			return true, true
		}
		if id := waitingKey(next.CheckRunID); id != "" {
			if _, waiting := st.Waiting[id]; waiting {
				delete(st.Waiting, id)
				return true, moved
			}
		}
		return changed, moved
	}
	if !held {
		st.Checks[key] = next
		return true, true
	}
	if checkRunIDIsOlder(uint64(next.CheckRunID), uint64(current.CheckRunID)) ||
		(next.CheckRunID == current.CheckRunID && !observationMayReplace(next.ObservedAt, current.ObservedAt, next.Status, current.Status)) {
		if next.CheckRunID != current.CheckRunID && st.mayStandInstead(current, next) && st.wait(next) {
			changed = true
		}
		return changed, moved
	}
	if next.CheckRunID != current.CheckRunID && st.mayStandInstead(next, current) && st.wait(current) {
		changed = true
	}
	if nextStanding == counted && st.dropWaiting(key) {
		changed = true
	}
	if current == next {
		return changed, moved
	}
	st.Checks[key] = next
	return true, moved || !sameView(current, next)
}

// mayStandInstead reports whether other, a run of the same name that does not stand, can ever
// stand in place of standing: only if standing's suite may still turn out uncounted, and other is
// of another suite (a run of the same suite shares its standing). A run of no suite, which a
// record written before the store kept suites holds, may be of any suite.
func (st *State) mayStandInstead(standing, other Check) bool {
	if st.standingOf(standing.AppID, standing.SuiteID) != unknown {
		return false
	}
	return standing.SuiteID == "" || other.SuiteID == "" || standing.SuiteID != other.SuiteID
}

// wait keeps check among the runs waiting to stand for their name (State.Waiting) and reports
// whether that changed st: the newest observation of each run, and of each name and suite only
// the newest run, since an older run of one suite never stands in place of a newer one. A check of
// no run id is never kept, since no run id orders it against the others.
func (st *State) wait(check Check) bool {
	id := waitingKey(check.CheckRunID)
	if id == "" {
		return false
	}
	if held, ok := st.Waiting[id]; ok {
		if held == check || !observationMayReplace(check.ObservedAt, held.ObservedAt, check.Status, held.Status) {
			return false
		}
		st.Waiting[id] = check
		return true
	}
	if check.SuiteID != "" {
		for otherID, other := range st.Waiting {
			if other.Name != check.Name || other.SuiteID != check.SuiteID {
				continue
			}
			if other.CheckRunID > check.CheckRunID {
				return false
			}
			delete(st.Waiting, otherID)
		}
	}
	if st.Waiting == nil {
		st.Waiting = map[string]Check{}
	}
	st.Waiting[id] = check
	return true
}

// standInstead puts in place of the check that stands for name, which has turned out uncounted,
// the newest run waiting for that name, or leaves the name with no check when none waits. A counted
// run that comes to stand stands for good, so what still waits for the name is dropped. The run
// that leaves is kept among the record's excluded runs (State.Excluded), which every later
// settlement names, since a settlement may have named it; a check of no run id was named by none.
func (st *State) standInstead(name string) {
	if leaving := st.Checks[name]; leaving.CheckRunID != 0 {
		if st.Excluded == nil {
			st.Excluded = map[string]string{}
		}
		st.Excluded[waitingKey(leaving.CheckRunID)] = name
	}
	var next Check
	nextID := ""
	for id, check := range st.Waiting {
		if check.Name == name && (nextID == "" || check.CheckRunID > next.CheckRunID) {
			next, nextID = check, id
		}
	}
	if nextID == "" {
		delete(st.Checks, name)
		return
	}
	delete(st.Waiting, nextID)
	st.Checks[name] = next
	if st.standingOf(next.AppID, next.SuiteID) == counted {
		st.dropWaiting(name)
	}
}

// dropWaiting drops every run waiting to stand for name and reports whether any did.
func (st *State) dropWaiting(name string) bool {
	dropped := false
	for id, check := range st.Waiting {
		if check.Name == name {
			delete(st.Waiting, id)
			dropped = true
		}
	}
	return dropped
}

// waitingKey is the key of a run in State.Waiting, its check-run id, and "" for a check of none.
func waitingKey(id checkRunID) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(id), 10)
}

// sameView reports whether two observations of one run differ in nothing but the suite and the App
// they name, which neither the snapshot (Hash) nor the settlement reads.
func sameView(one, other Check) bool {
	one.SuiteID, one.AppID = "", ""
	other.SuiteID, other.AppID = "", ""
	return one == other
}
