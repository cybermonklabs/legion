package cistore

import (
	"sort"
)

// category buckets a check into one of the rollup groups.
type category int

const (
	catPassed category = iota
	catFailed
	catRunning
	catQueued
	catCancelled
	catSkipped
)

// classify maps a check's (status, conclusion) to a group.
//
// A completed check is judged by its conclusion; an incomplete check by its
// status. All documented GitHub conclusions are handled explicitly; an unknown
// (future/undocumented) completed conclusion is surfaced as failed rather than
// silently passed, so a failure-like new state is not hidden. Unknown statuses
// default to queued.
func classify(c Check) category {
	if c.Status == "completed" {
		switch c.Conclusion {
		case "failure", "timed_out", "action_required", "startup_failure", "stale":
			return catFailed
		case "cancelled":
			return catCancelled
		case "skipped":
			return catSkipped
		case "neutral", "success", "":
			return catPassed
		default:
			return catFailed
		}
	}
	switch c.Status {
	case "in_progress":
		return catRunning
	case "queued", "requested", "waiting", "pending":
		return catQueued
	default:
		return catQueued
	}
}

// StatusGroup is the per-status view: an explicit count plus the full sorted
// list of check names in that status (nothing is collapsed).
type StatusGroup struct {
	Count  int      `json:"count"`
	Checks []string `json:"checks"`
}

// FailingCheck identifies a failed check and its GitHub URL.
type FailingCheck struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Summary is the JSON checks notification body. Every check-run group is always present (count 0,
// empty checks when none), and `Statuses` alone is left out when the record holds none, so
// consumers see a stable schema.
type Summary struct {
	Kind   string `json:"kind"`
	Repo   string `json:"repo"`
	Number string `json:"number"`
	SHA    string `json:"sha"`
	// CheckRuns is the settlement's attempt set: the latest check-run id per
	// check name, sorted by name, each with GitHub's own conclusion of that run
	// (CheckRunRef.Conclusion). Consumers order settlements of one commit by it
	// (within one record per-name ids never decrease) and by each status's
	// created_at (Statuses), then by Generation where neither says which is newer.
	// Consumers name a check by its conclusion word; which group a check is in
	// stays the listener's rule (classify), so a word a consumer does not know
	// still lands in one group.
	CheckRuns            []CheckRunRef  `json:"check_runs"`
	Generation           uint64         `json:"generation"`
	Snapshot             string         `json:"snapshot"`
	SupersededSettlement string         `json:"superseded_settlement,omitempty"`
	Failed               StatusGroup    `json:"failed"`
	Running              StatusGroup    `json:"running"`
	Passed               StatusGroup    `json:"passed"`
	Queued               StatusGroup    `json:"queued"`
	Cancelled            StatusGroup    `json:"cancelled"`
	Skipped              StatusGroup    `json:"skipped"`
	FailingChecks        []FailingCheck `json:"failing_checks"`
	// Statuses names each commit status the record holds with its state and GitHub's created_at,
	// in context order, and is left out when the record holds none, so a head with no status
	// settles in the same bytes as before the record held statuses. A status is no check run:
	// check_runs, the groups from failed to skipped and failing_checks name check runs alone, and
	// a check run and a status of one name are two entries. A status has no id, and a record the
	// bucket's TTL removed numbers its settlements afresh when it is written again, so the
	// generation does not order two statuses of one context across such a record: each entry's
	// created_at does, as it orders them in the record (statusMayReplace).
	Statuses []StatusRef `json:"statuses,omitempty"`
}

// CheckRunRef names one check, its latest GitHub check-run id, and GitHub's own conclusion of
// that run.
type CheckRunRef struct {
	Name string `json:"name"`
	ID   uint64 `json:"id"`
	// Conclusion is the run's conclusion word as GitHub gave it and the record holds it (success,
	// skipped, timed_out, and any word GitHub adds later), passed on as it is so a consumer names
	// the check by GitHub's own word, and left out when the record holds none, as for a run that
	// has not completed. It decides no group: the groups stay the listener's rule (classify).
	Conclusion string `json:"conclusion,omitempty"`
}

// StatusRef names one commit status, its state (success, failure or error) and GitHub's
// created_at as the record keeps it (Status.CreatedAt), GitHub's RFC 3339 text. The snapshot
// (State.Hash) takes the context and the state alone, so a new created_at alone settles nothing.
type StatusRef struct {
	Context   string `json:"context"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

// statusFailed reports whether a commit status in state failed: failure and error did, success
// did not.
func statusFailed(state string) bool {
	return state == "failure" || state == "error"
}

// renderSummary derives the stable checks payload. Names within each group are
// sorted; publication owns the settlement timestamp and JSON encoding.
func renderSummary(s State) Summary {
	groups := map[category][]string{}
	failingChecks := make([]FailingCheck, 0)
	checkRuns := make([]CheckRunRef, 0)
	for key, check := range s.Checks {
		name := check.Name
		if name == "" {
			name = key
		}
		if check.CheckRunID > 0 {
			checkRuns = append(checkRuns, CheckRunRef{Name: name, ID: uint64(check.CheckRunID), Conclusion: check.Conclusion})
		}
		category := classify(check)
		groups[category] = append(groups[category], name)
		if category == catFailed {
			failingChecks = append(failingChecks, FailingCheck{Name: name, URL: check.URL})
		}
	}
	sort.Slice(failingChecks, func(i, j int) bool {
		if failingChecks[i].Name == failingChecks[j].Name {
			return failingChecks[i].URL < failingChecks[j].URL
		}
		return failingChecks[i].Name < failingChecks[j].Name
	})
	sort.Slice(checkRuns, func(i, j int) bool { return checkRuns[i].Name < checkRuns[j].Name })
	failed := group(groups[catFailed])
	sum := Summary{
		Kind:          "checks",
		Repo:          s.Owner + "/" + s.Repo,
		Number:        s.Number,
		SHA:           s.SHA,
		CheckRuns:     checkRuns,
		Generation:    s.Generation,
		Snapshot:      s.Hash(),
		Failed:        failed,
		Running:       group(groups[catRunning]),
		Passed:        group(groups[catPassed]),
		Queued:        group(groups[catQueued]),
		Cancelled:     group(groups[catCancelled]),
		Skipped:       group(groups[catSkipped]),
		FailingChecks: failingChecks,
	}
	if contexts := statusContexts(s.Statuses); contexts != nil {
		sum.Statuses = make([]StatusRef, len(contexts))
		for i, name := range contexts {
			status := s.Statuses[name]
			sum.Statuses[i] = StatusRef{Context: name, State: status.State, CreatedAt: status.CreatedAt}
		}
	}
	if s.EmittedCount > 0 {
		sum.SupersededSettlement = "true"
	}
	return sum
}

// group builds a StatusGroup from a name list: sorted names, explicit count, and
// a non-nil slice so JSON marshals an empty group's checks as [] not null.
func group(names []string) StatusGroup {
	if names == nil {
		names = []string{}
	}
	sort.Strings(names)
	return StatusGroup{Count: len(names), Checks: names}
}
