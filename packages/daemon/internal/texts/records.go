package texts

import (
	"strconv"
	"strings"
)

// This file holds the parts that the records of several kinds share: the heads, the rows of a
// check and of a commit status that did not pass, the facts of a relaunch, and the helpers that
// write a reference the same way in every text. The record of each kind is in records_task.go,
// records_start.go, records_notice.go, records_controller.go and records_role.go.

// TaskHead is the first two fields of a task record: the kind and the issue. Render writes them
// as the first two lines of the task.
type TaskHead struct {
	Kind  Kind   `json:"kind"`
	Issue string `json:"issue"`
}

// NoticeHead is the first fields of the record of a notice to an architect or to the controller:
// the kind, the issue, and the count of the times the daemon queued the notice again after the
// listener could not forward it (the daemon's rehold).
type NoticeHead struct {
	Kind    Kind   `json:"kind"`
	Issue   string `json:"issue"`
	Resends int    `json:"resends,omitempty"`
}

// RoleHead is the first two fields of the record of a role message: the kind and the issue.
type RoleHead struct {
	Kind  Kind   `json:"kind"`
	Issue string `json:"issue"`
}

// CheckConclusion is one check that did not pass on a head: its name and its conclusion, the word
// GitHub gave its run (failure, timed_out, cancelled, action_required and the rest) where the
// settlement carried one, else failed or cancelled (dispatch://INFRA-4135 R1). A list of them is
// the table of the checks that did not pass. A record names the checks GitHub skipped in a list
// of their own, under the key skipped, the conclusion's own name (dispatch://INFRA-4135 R1), and
// says once, in a field of its own beside that list, that a skipped check ran nothing on the head
// (each_skipped_check: ran_nothing, EachSkippedCheck), so it is neither a pass nor a failure and
// no field holds a fixed sentence. The record counts the checks that passed beside both lists.
type CheckConclusion struct {
	Check      string `json:"check"`
	Conclusion string `json:"conclusion"`
}

// RanNothing is what a record's each_skipped_check says of every check its skipped list names:
// the check ran nothing on the head (dispatch://INFRA-4135 R1).
const RanNothing = "ran_nothing"

// EachSkippedCheck is the value of a record's each_skipped_check beside its list of the checks
// GitHub skipped: RanNothing where the list names a check, so the record says it once, and ""
// (no key) where the list names none.
func EachSkippedCheck(skipped []string) string {
	if len(skipped) == 0 {
		return ""
	}
	return RanNothing
}

// StatusState is one commit status that did not pass on a head: its context and its state as
// GitHub spells it (failure or error). A list of them is the table of the commit statuses that did
// not pass, apart from the table of the checks, since a commit status is an entry of its own kind
// (dispatch://INFRA-1997's R6); the statuses that passed are counted with the checks that passed.
type StatusState struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

// RelaunchFacts is what the last let-go of a claim's process ended, as the claim's record of it
// holds it (supervise.EndedWork): the cause in the daemon's words, whether a turn was cut off,
// whether that turn was the turn of the task that follows, each kind of background work that
// stopped, whether background work of no named kind stopped, whether a report of the cut turn's
// background work is on record, and whether the facts go in front of a task.
type RelaunchFacts struct {
	Cause      string   `json:"cause,omitempty"`
	InTurn     bool     `json:"in_turn,omitempty"`
	TaskTurn   bool     `json:"task_turn,omitempty"`
	Stopped    []string `json:"stopped,omitempty"`
	Background bool     `json:"background,omitempty"`
	Reported   bool     `json:"reported,omitempty"`
	Task       bool     `json:"task,omitempty"`
}

// shortDigestLength is the count of hexadecimal characters of a short digest.
const shortDigestLength = 12

// PullRequestRef is the reference of pull request number of repo, which is owner/name:
// owner/name#number.
func PullRequestRef(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// ListPassed is the command that lists every check of pull request number of repo, the checks
// that passed among them: gh pr checks <number> --repo <owner>/<name>.
func ListPassed(repo string, number int) string {
	return "gh pr checks " + strconv.Itoa(number) + " --repo " + repo
}

// EndsRound is the review that ends a review round stuck at head, in the words that
// dispatch://INFRA-3498's R4 keeps: APPROVE of head <sha>.
func EndsRound(head string) string {
	return "APPROVE of head " + head
}

// ShortDigest is the first 12 hexadecimal characters of the digest of image, which is an image
// reference with a digest (name@sha256:<hex>) or a digest alone (sha256:<hex>). A reference
// that carries no digest, such as name:tag, is returned as it is, even where its tag is
// hexadecimal (name:ff33c8c), since it has no digest to cut.
func ShortDigest(image string) string {
	digest := image
	if _, after, found := strings.Cut(image, "@"); found {
		digest = after
	} else if !strings.HasPrefix(image, "sha256:") {
		return image
	}
	_, hex, found := strings.Cut(digest, ":")
	if !found || hex == "" || strings.Trim(hex, "0123456789abcdef") != "" {
		return image
	}
	if len(hex) > shortDigestLength {
		hex = hex[:shortDigestLength]
	}
	return hex
}
