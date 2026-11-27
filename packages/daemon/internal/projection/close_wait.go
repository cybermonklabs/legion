// Package projection: this file holds what an issue's close waits for, as the state shows it
// (api.Issue.CloseWait), and the sentence `why` reads for an issue in verifying.
package projection

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// closeWaitView is what issue's close waits for (api.CloseWaitView), read from the stored wait its
// row holds (record.StoredWait), which close-wait.md renders, so the state and the document name
// the same items with the same since. An issue in verifying carries every item of it. One in retro
// or merging, or held from either, carries its reported defects alone, and only where it holds
// one: nothing else of the close is read before the merge, and what an earlier entry into
// verifying left in the row is no wait of the change now running. Every other issue, and one with
// no stored wait, carries none.
func closeWaitView(issue record.Issue) (*api.CloseWaitView, error) {
	verifying := issue.Phase == phase.Verifying
	if len(issue.CloseWait) == 0 || !verifying && !record.StandsIn(issue, phase.Retro, phase.Merging) {
		return nil, nil
	}
	var stored record.StoredWait
	if err := json.Unmarshal(issue.CloseWait, &stored); err != nil {
		return nil, fmt.Errorf("read the stored wait of %s: %w", issue.Key, err)
	}
	view := &api.CloseWaitView{NextChanges: issue.NextChanges}
	if since := issue.CloseWaitSince; since != nil {
		at := since.UTC()
		view.Since = &at
	}
	for _, defect := range stored.Defects {
		view.Defects = append(view.Defects, api.CloseWaitDefectView{ID: strconv.FormatInt(defect.ID, 10), Kind: string(defect.Kind),
			Role: string(defect.Role), Text: defect.Text, Head: defect.Head, Check: defect.Check, Since: defect.Since})
	}
	if !verifying {
		if len(view.Defects) == 0 {
			return nil, nil
		}
		return view, nil
	}
	view.Beneath = closeWaitIssues(stored.Beneath)
	view.OpenChildren = closeWaitIssues(stored.OpenChildren)
	for _, check := range stored.Checks {
		view.Checks = append(view.Checks, api.CloseWaitCheckView{ID: check.ID, When: check.When, Answer: check.Answer, Reason: check.Reason, Since: check.Since})
	}
	if production := stored.Production; production != nil {
		view.Production = &api.CloseWaitProductionView{State: production.State, Generation: production.Generation, Merge: production.Merge,
			Outcome: production.Outcome, NextLookAt: production.NextLookAt, Since: production.Since}
	}
	if unapproved := stored.Unapproved; unapproved != nil {
		view.Unapproved = &api.CloseWaitUnapprovedView{Checks: unapproved.Checks, Since: unapproved.Since}
	}
	if unread := stored.Unread; unread != nil {
		view.Unread = &api.CloseWaitUnreadView{Cause: unread.Cause, Since: unread.Since}
	}
	return view, nil
}

// closeWaitIssues is the issues of a stored wait as the state shows them.
func closeWaitIssues(issues []record.WaitIssue) []api.CloseWaitIssueView {
	var views []api.CloseWaitIssueView
	for _, waiting := range issues {
		views = append(views, api.CloseWaitIssueView{Key: waiting.Key, Status: waiting.Status, Since: waiting.Since})
	}
	return views
}

// closeWaitReason is the sentence `why` reads for an issue in verifying (api.WhyWaiting): the
// first thing its close waits for, in the order close-wait.md lists the items, or that it waits
// for the next read of its checks where its stored wait names nothing yet.
func closeWaitReason(wait *api.CloseWaitView) string {
	const waits = "It waits in verifying, and its close waits for "
	switch {
	case wait == nil:
		return "It waits in verifying for the next read of its checks."
	case wait.Unread != nil:
		return waits + "a read of its checks that counts: " + strings.TrimSuffix(wait.Unread.Cause, ".") + "."
	case wait.Unapproved != nil:
		return waits + "a person's approval of its Verification document, since these checks changed: " + strings.Join(wait.Unapproved.Checks, ", ") + "."
	case len(wait.Checks) > 0:
		check := wait.Checks[0]
		answer := check.Answer
		if check.Reason != "" {
			answer += ", " + check.Reason
		}
		return fmt.Sprintf(waits+"check %s, which Dispatch answers %s.", check.ID, answer)
	case wait.Production != nil:
		return fmt.Sprintf(waits+"its production check of generation %d, which is %s.", wait.Production.Generation, wait.Production.State)
	case len(wait.Beneath) > 0:
		return fmt.Sprintf(waits+"%s beneath it, which is %s.", wait.Beneath[0].Key, wait.Beneath[0].Status)
	case len(wait.OpenChildren) > 0:
		return fmt.Sprintf(waits+"its open child %s, which is %s.", wait.OpenChildren[0].Key, wait.OpenChildren[0].Status)
	case len(wait.Defects) > 0:
		return fmt.Sprintf(waits+"defect %s, which the %s reported and its next change carries.", wait.Defects[0].ID, wait.Defects[0].Role)
	}
	return "It waits in verifying for the next read of its checks."
}
