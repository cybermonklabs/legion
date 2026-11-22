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
// Actions workflow run that event started. It counts the events GitHub documents and the one event
// measured counted beside them, and no other:
//
//   - push, pull_request, pull_request_review, pull_request_target, deployment and
//     deployment_status, from GitHub's documentation: "Troubleshooting required status checks",
//     section "Checks from some workflow jobs are not evaluated"
//     (https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks).
//   - merge_group, from the same page, which names it as the event a merge queue's required checks
//     need ("Status checks with GitHub Actions and a Merge queue"). It is taken from the
//     documentation and was not measured in a rollup.
//   - dynamic, measured: CodeQL's default setup makes a suite of it, and GitHub's rollup counts its
//     runs (cybermonklabs/legion#2803 at aa4cd30f holds its required ci-ok in one, rollup SUCCESS;
//     #2725 at 48308c23 fails on one, rollup FAILURE).
//
// Every other event is left out, as the documentation says, and two are measured left out of a
// rollup: workflow_dispatch (#3513 at a368687e, 2026-11-21: the dispatched run's ci-ok is not in
// the rollup, the pull_request run's is) and pull_request_review_comment (#2167 at 57f64b53: of
// four pr-title runs only the two of pull_request_review suites are in it).
func EventCounted(event string) bool {
	switch event {
	case "push", "pull_request", "pull_request_review", "pull_request_target", "deployment", "deployment_status",
		"merge_group", "dynamic":
		return true
	}
	return false
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
// suiteID, and applies what it says. changed reports whether st changed, and left whether an
// uncounted suite left its checks or suites, which re-arms the head (touch) and may bring it back to
// its last published settlement (resettle). An event the record already holds for the suite
// changes nothing: GitHub never changes the event of a run.
//
// A counted suite's check runs that stand for a name now stand for good, so nothing waiting for
// those names can take their place and the record drops what waits for them, and what it keeps of
// the suite's runs that newer runs replaced (Superseded). An uncounted suite leaves the record: its
// suite entry, its check runs that wait, and each of its check runs that stands for a name, in
// whose place the newest check run waiting for that name stands (standInstead). Each of its runs
// that stood and that a newer run replaced joins the excluded runs too, since a consumer that
// missed the settlements after it still holds it as its name's newest run. That is how a record
// that counted the suite before it knew the event, a record written before the store read events
// among them, is corrected.
func (st *State) learnRunEvent(suiteID, event string) (changed, left bool) {
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
		for id, check := range st.Superseded {
			if check.SuiteID == suiteID {
				delete(st.Superseded, id)
			}
		}
		for name, check := range st.Checks {
			if check.SuiteID == suiteID {
				st.dropWaiting(name)
			}
		}
		return true, false
	}
	for id, check := range st.Superseded {
		if check.SuiteID == suiteID {
			st.exclude(check)
			delete(st.Superseded, id)
		}
	}
	for id, check := range st.Waiting {
		if check.SuiteID == suiteID {
			delete(st.Waiting, id)
		}
	}
	if _, held := st.Suites[suiteID]; held {
		delete(st.Suites, suiteID)
		left = true
	}
	for name, check := range st.Checks {
		if check.SuiteID == suiteID {
			st.standInstead(name)
			left = true
		}
	}
	return true, left
}

// applyCheckRun folds one check run's observation into st, the event of its suite's workflow run
// already applied (Store.record calls learnRunEvent first). changed reports whether st changed, and
// moved whether the head's checks did, which re-arms the head (touch).
//
// A counted or unknown run stands for its name as before: the newest check-run id, and for one id
// the newest observation (observationMayReplace). An uncounted run stands for nothing and changes
// nothing, unless the record took it before its suite's event was known, in which case it leaves.
// Which of two runs of one name stands does not depend on the order they and their suites' events
// arrive in: a run that loses to a run of a suite whose event the record does not know waits
// (wait), and stands if that suite turns out uncounted. A run that loses to a counted one never
// stands, since a counted run stays counted, and is dropped as before. A run that stood and that a
// newer run replaces while its own suite's event is not known is kept as superseded (supersede):
// a settlement may have named it, so it is excluded if its suite turns out uncounted.
//
// A run already recorded that the observation tells only its suite and App changes the record and
// not the head's checks (sameView), so a head settled before the record kept them is not settled
// again for them.
func (st *State) applyCheckRun(observation contracts.CIObservation) (changed, moved bool) {
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
	if st.resolveSuperseded(next) {
		changed = true
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
	if next.CheckRunID != current.CheckRunID && st.supersede(current) {
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
// that leaves is kept among the record's excluded runs (exclude), which every later settlement
// names, since a settlement may have named it.
func (st *State) standInstead(name string) {
	st.exclude(st.Checks[name])
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

// trimWaiting drops each run waiting to stand for its name (State.Waiting) that can no longer come
// to stand, a run older than a counted run waiting for the same name, and reports how many it
// dropped. standInstead puts the newest waiting run of a name in place of the run that stands, and
// a counted run that comes to stand stands for good, so no run older than it ever stands. Every
// other waiting run may still stand: the newest of a name, and each older one while every run
// newer than it may still turn out uncounted. It is what a record past its bound gives way
// (Store.atBounds).
func (st *State) trimWaiting() int {
	floor := map[string]checkRunID{}
	for _, check := range st.Waiting {
		if check.CheckRunID > floor[check.Name] && st.standingOf(check.AppID, check.SuiteID) == counted {
			floor[check.Name] = check.CheckRunID
		}
	}
	dropped := 0
	for id, check := range st.Waiting {
		if check.CheckRunID < floor[check.Name] {
			delete(st.Waiting, id)
			dropped++
		}
	}
	return dropped
}

// trimSuperseded drops, of the runs a newer run replaced while their suite's event was not known
// (State.Superseded), all but the newest of each name, and reports how many it dropped. Any of
// them may have been settled, the newest too or none of them; a consumer holds the newest one it
// was sent, so the one kept is the newest that can be held. An older one is held only by a
// consumer that was sent it and none of the settlements after it, and the correction no longer
// names it. It is what a record past its bound gives way after the waiting runs that can no longer
// stand (Store.atBounds).
func (st *State) trimSuperseded() int {
	newest := map[string]checkRunID{}
	for _, check := range st.Superseded {
		if check.CheckRunID > newest[check.Name] {
			newest[check.Name] = check.CheckRunID
		}
	}
	dropped := 0
	for id, check := range st.Superseded {
		if check.CheckRunID != newest[check.Name] {
			delete(st.Superseded, id)
			dropped++
		}
	}
	return dropped
}

// exclude keeps check among the record's excluded runs (State.Excluded), which every later
// settlement names. A check of no run id is kept by none, since no settlement names it.
func (st *State) exclude(check Check) {
	id := waitingKey(check.CheckRunID)
	if id == "" {
		return
	}
	if st.Excluded == nil {
		st.Excluded = map[string]string{}
	}
	st.Excluded[id] = check.Name
}

// supersede keeps replaced, a run that stood for its name and that a newer run has just replaced,
// among the record's superseded runs (State.Superseded) while its suite may still turn out
// uncounted, and reports whether that changed st. A settlement may have named it, so it is
// excluded if its suite turns out uncounted (learnRunEvent). A run of a counted suite, or of no
// run id, is not kept: it never leaves, or no settlement named it.
func (st *State) supersede(replaced Check) bool {
	id := waitingKey(replaced.CheckRunID)
	if id == "" || st.standingOf(replaced.AppID, replaced.SuiteID) != unknown {
		return false
	}
	if held, ok := st.Superseded[id]; ok && held == replaced {
		return false
	}
	if st.Superseded == nil {
		st.Superseded = map[string]Check{}
	}
	st.Superseded[id] = replaced
	return true
}

// resolveSuperseded applies what observation, a later observation of a check run, tells of a
// superseded run of the same id (State.Superseded), and reports whether that changed st. A
// superseded run of no suite, which a record written before the store kept suites holds, takes the
// observation's suite and App; a run whose suite is then known to be counted is dropped, and one
// known to be uncounted is excluded (exclude), as learnRunEvent does once that suite's event
// arrives.
func (st *State) resolveSuperseded(observation Check) bool {
	id := waitingKey(observation.CheckRunID)
	held, ok := st.Superseded[id]
	if id == "" || !ok {
		return false
	}
	changed := false
	if held.SuiteID == "" && observation.SuiteID != "" {
		held.SuiteID, held.AppID = observation.SuiteID, observation.AppID
		st.Superseded[id] = held
		changed = true
	}
	switch st.standingOf(held.AppID, held.SuiteID) {
	case counted:
		delete(st.Superseded, id)
		return true
	case uncounted:
		st.exclude(held)
		delete(st.Superseded, id)
		return true
	}
	return changed
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
