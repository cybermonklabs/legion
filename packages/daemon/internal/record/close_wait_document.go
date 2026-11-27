package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// CloseWaitDocumentName is the name of the issue's Dispatch document that says what its close
// waits for, and CloseWaitDocumentSlug the slug Dispatch files it under, which a read of the
// document names. No mechanism shares a value between this module and Dispatch's, so the slug
// changes with Dispatch's rule of a name's slug.
const (
	CloseWaitDocumentName = "close-wait.md"
	CloseWaitDocumentSlug = "close-wait-md"
)

// StoredWait is what an issue's close waits for, as the workflow stores it on the issue's row
// (issues.close_wait, Issue.CloseWait) at the entry into verifying and again whenever an item
// comes, goes or changes its state, and as close-wait.md renders it (CloseWaitDocument). Since
// when the issue waits is the row's own column (Issue.CloseWaitSince), so it is not kept here
// twice. Each item says since when it stands. Entered is the time of the issue's newest entry
// into verifying, after which alone a read of the checks counts. Ended is the change that ended
// last, said once in the version written when it ended. Withdrawn is each reported defect the
// owning architect withdrew in the write that stores it, with the reason, said once in the version
// written for that withdrawal. Closed is when the issue closed, set in the last version only.
//
// A report of a defect in retro or merging (workflow's defectReport) stores the wait as well, so
// close-wait.md names the defect before the merge: there the wait holds the reported defects, and
// whatever an earlier entry into verifying left.
type StoredWait struct {
	Entered      time.Time        `json:"entered"`
	Checks       []WaitCheck      `json:"checks,omitempty"`
	Production   *WaitProduction  `json:"production,omitempty"`
	Beneath      []WaitIssue      `json:"beneath,omitempty"`
	OpenChildren []WaitIssue      `json:"openChildren,omitempty"`
	Defects      []WaitReason     `json:"defects,omitempty"`
	NextChange   *WaitChange      `json:"nextChange,omitempty"`
	Unread       *WaitUnread      `json:"unread,omitempty"`
	Unapproved   *WaitUnapproved  `json:"unapproved,omitempty"`
	Ended        *WaitChangeEnd   `json:"ended,omitempty"`
	Withdrawn    []WaitWithdrawal `json:"withdrawn,omitempty"`
	Closed       *time.Time       `json:"closed,omitempty"`
}

// WaitCheck is one check of the issue's Verification document whose state is not a pass: its
// block id, its `when`, and Dispatch's answer and reason for it (dispatch.PassStands).
type WaitCheck struct {
	ID     string    `json:"id"`
	When   string    `json:"when"`
	Answer string    `json:"answer"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since"`
}

// WaitProduction is the production check where it holds the close: its state as the close reads
// it (CloseWait.Production: awaiting_change, opening, open, not_run, defect, earlier_merge), the
// generation and the merge commit it is of, and its outcome, or the time of its next look while it
// has none.
type WaitProduction struct {
	State      string     `json:"state"`
	Generation uint64     `json:"generation"`
	Merge      string     `json:"merge,omitempty"`
	Outcome    string     `json:"outcome,omitempty"`
	NextLookAt *time.Time `json:"nextLookAt,omitempty"`
	Since      time.Time  `json:"since"`
}

// WaitIssue is an issue the close waits for: one beneath it in the workflow, or a child Dispatch
// holds open, with its status.
type WaitIssue struct {
	Key    string    `json:"key"`
	Status string    `json:"status"`
	Since  time.Time `json:"since"`
}

// WaitReason is one reason of a next change, or a reported defect that holds the close: its id,
// its kind, its text, the check it is about, where it names one, and, for a reported defect, the
// role that reported it and the head of the pull request it saw the defect at.
type WaitReason struct {
	ID    int64      `json:"id"`
	Kind  DefectKind `json:"kind"`
	Text  string     `json:"text"`
	Check string     `json:"check,omitempty"`
	Role  claim.Role `json:"role,omitempty"`
	Head  string     `json:"head,omitempty"`
	Since time.Time  `json:"since"`
}

// WaitWithdrawal is a reported defect the owning architect withdrew (withdraw_defect): its id, its
// text, the withdrawal's reason and when it was made.
type WaitWithdrawal struct {
	ID     int64     `json:"id"`
	Text   string    `json:"text"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// WaitChange is the next change of the issue that is open: its generation and each reason it
// carries.
type WaitChange struct {
	Generation uint64       `json:"generation"`
	Reasons    []WaitReason `json:"reasons"`
	Since      time.Time    `json:"since"`
}

// WaitUnread says the newest read of Dispatch's states does not count, and why: no read yet, a
// failed read, a read before the newest entry into verifying, or a read that names another merge.
type WaitUnread struct {
	Cause string    `json:"cause"`
	Since time.Time `json:"since"`
}

// WaitUnapproved names each check removed or reworded since the approved version of the
// Verification document, which a person must approve.
type WaitUnapproved struct {
	Checks []string  `json:"checks"`
	Since  time.Time `json:"since"`
}

// WaitChangeEnd is a next change that ended, and how: merged as its pull request (PullRequest is
// its number), or ended with no pull request (PullRequest is 0).
type WaitChangeEnd struct {
	Generation  uint64    `json:"generation"`
	PullRequest int       `json:"pullRequest,omitempty"`
	At          time.Time `json:"at"`
}

// ErrNoStoredWait is CloseWaitDocument's answer for an issue whose row holds no stored wait.
var ErrNoStoredWait = errors.New("the issue holds no stored wait")

// CloseWaitDocument renders the stored wait of issue (Issue.CloseWait) as the markdown of its
// document close-wait.md: since when the issue waits, then each item with its state and since
// when it stands, or that the issue closed, and when. It reads the record as it is handed, so the
// close_wait_publish row renders the wait the record holds when the row runs.
func CloseWaitDocument(issue Issue) (string, error) {
	if len(issue.CloseWait) == 0 {
		return "", fmt.Errorf("render close-wait.md of %s: %w", issue.Key, ErrNoStoredWait)
	}
	var wait StoredWait
	if err := json.Unmarshal(issue.CloseWait, &wait); err != nil {
		return "", fmt.Errorf("render close-wait.md of %s: decode its stored wait: %w", issue.Key, err)
	}
	var doc strings.Builder
	fmt.Fprintf(&doc, "# What the close of %s waits for\n\n", issue.Key)
	if issue.CloseWaitSince != nil {
		fmt.Fprintf(&doc, "%s waits since %s.\n", issue.Key, waitTime(*issue.CloseWaitSince))
	}
	if !wait.Entered.IsZero() {
		fmt.Fprintf(&doc, "It entered verifying at %s.\n", waitTime(wait.Entered))
	}
	if wait.Ended != nil {
		doc.WriteString("\n## The change that ended\n\n")
		if wait.Ended.PullRequest != 0 {
			fmt.Fprintf(&doc, "Change %d merged as pull request #%d at %s.\n", wait.Ended.Generation, wait.Ended.PullRequest, waitTime(wait.Ended.At))
		} else {
			fmt.Fprintf(&doc, "Change %d ended with no pull request at %s.\n", wait.Ended.Generation, waitTime(wait.Ended.At))
		}
	}
	if wait.Closed != nil {
		fmt.Fprintf(&doc, "\n## Closed\n\n%s closed at %s. Nothing holds it now.\n", issue.Key, waitTime(*wait.Closed))
		return doc.String(), nil
	}
	items := 0
	section := func(heading string) {
		items++
		fmt.Fprintf(&doc, "\n## %s\n\n", heading)
	}
	if wait.Unread != nil {
		section("Unread")
		fmt.Fprintf(&doc, "The newest read of the checks does not count: %s. Since %s.\n", wait.Unread.Cause, waitTime(wait.Unread.Since))
	}
	if wait.Unapproved != nil {
		section("Unapproved")
		fmt.Fprintf(&doc, "A person must approve the current Verification document. Since %s, these checks changed:\n\n", waitTime(wait.Unapproved.Since))
		for _, check := range wait.Unapproved.Checks {
			fmt.Fprintf(&doc, "- `%s`\n", check)
		}
	}
	if len(wait.Checks) > 0 {
		section("Checks")
		for _, check := range wait.Checks {
			state := check.Answer
			if check.Reason != "" {
				state += ", " + check.Reason
			}
			fmt.Fprintf(&doc, "- `%s` (%s): %s, since %s\n", check.ID, check.When, state, waitTime(check.Since))
		}
	}
	if production := wait.Production; production != nil {
		section("Production check")
		fmt.Fprintf(&doc, "The production check of generation %d", production.Generation)
		if production.Merge != "" {
			fmt.Fprintf(&doc, " at merge `%s`", production.Merge)
		}
		fmt.Fprintf(&doc, " is %s, since %s.", production.State, waitTime(production.Since))
		switch {
		case production.Outcome != "":
			fmt.Fprintf(&doc, " Its outcome is %s.", production.Outcome)
		case production.NextLookAt != nil:
			fmt.Fprintf(&doc, " Its next look is at %s.", waitTime(*production.NextLookAt))
		}
		doc.WriteString("\n")
	}
	waitIssues := func(heading string, issues []WaitIssue) {
		if len(issues) == 0 {
			return
		}
		section(heading)
		for _, waiting := range issues {
			fmt.Fprintf(&doc, "- %s (%s), since %s\n", waiting.Key, waiting.Status, waitTime(waiting.Since))
		}
	}
	waitIssues("Issues beneath", wait.Beneath)
	waitIssues("Open children", wait.OpenChildren)
	if len(wait.Defects) > 0 {
		section("Reported defects")
		for _, defect := range wait.Defects {
			writeWaitReason(&doc, defect)
		}
	}
	if change := wait.NextChange; change != nil {
		section("Next change")
		fmt.Fprintf(&doc, "Change %d is open since %s. It carries these reasons:\n\n", change.Generation, waitTime(change.Since))
		for _, reason := range change.Reasons {
			writeWaitReason(&doc, reason)
		}
	}
	if items == 0 {
		doc.WriteString("\nNothing holds the close now.\n")
	}
	return doc.String(), nil
}

// writeWaitReason writes one reason of a change, or one reported defect, as an item of the list:
// its kind and id, the check it is about where it names one, since when it stands, and its text
// beneath, each line of it indented under the item.
func writeWaitReason(doc *strings.Builder, reason WaitReason) {
	fmt.Fprintf(doc, "- %s %d", reason.Kind, reason.ID)
	if reason.Check != "" {
		fmt.Fprintf(doc, " about `%s`", reason.Check)
	}
	fmt.Fprintf(doc, ", since %s", waitTime(reason.Since))
	if text := strings.TrimSpace(reason.Text); text != "" {
		doc.WriteString(":\n")
		for line := range strings.SplitSeq(text, "\n") {
			fmt.Fprintf(doc, "  %s\n", line)
		}
		return
	}
	doc.WriteString("\n")
}

// waitTime is a moment as close-wait.md writes it: UTC, to the second.
func waitTime(at time.Time) string {
	return at.UTC().Format("2006-01-02 15:04:05 UTC")
}
