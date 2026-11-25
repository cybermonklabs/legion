package record

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// fieldsOfPublished is the payload a role message's record publishes, as JSON, with each top-level
// key and its value as text.
func fieldsOfPublished(t *testing.T, published any) map[string]string {
	t.Helper()
	encoded, err := json.Marshal(published)
	if err != nil {
		t.Fatalf("encode %T: %v", published, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode %s: %v", encoded, err)
	}
	fields := make(map[string]string, len(decoded))
	for key, value := range decoded {
		fields[key] = fmt.Sprint(value)
	}
	return fields
}

// Main's merge-queue-removal message (record/outbox.go, MergeQueueRemoval.Message, at d46ec4e0)
// says, read or unread: " The event the daemon received names no reason for the removal. Read the
// pull request and its removals on GitHub, as step 5 of your role says." dispatch://INFRA-3274's
// R1 (approved at version 16, done, and main holds it) has that message say it: "The message names
// the pull request and the head. It says that the event as the daemon got it names no reason, so
// the merger reads the pull request on GitHub." The Design's "Approved requirements this changes"
// does not name INFRA-3274, and dispatch://INFRA-3686's R8 says that "a fact or a step that it has
// the text give stays as a field". So the record holds a field for the fact (the event names no
// reason) and one for the step (the merger reads the pull request on GitHub), beside the pull
// request, the head and unread.
func TestAMergeQueueRemovalStatesAsFieldsThatTheEventNamesNoReasonAndThatTheMergerReadsGitHub(t *testing.T) {
	for _, unread := range []bool{false, true} {
		fields := fieldsOfPublished(t, MergeQueueRemoval{Number: 1301, Head: strings.Repeat("ab", 20), Unread: unread}.Record("LEGION-41", "acme/tower"))
		var kept []string
		for key := range fields {
			switch key {
			case "kind", "issue", "pull_request", "head", "unread":
			default:
				kept = append(kept, key)
			}
		}
		if len(kept) == 0 {
			t.Errorf("unread %v: the merge-queue-removal record holds %v and no field for INFRA-3274 R1's fact and step "+
				"(the event as the daemon got it names no reason, so the merger reads the pull request on GitHub)", unread, fields)
		}
	}
}

// Main's merge-forward ask (record/outbox.go, MergeForwardMessage.Message, at d46ec4e0) says "%s: %s
// at head %s conflicts with its base branch %s, and a second read of GitHub confirmed it", whether
// or not the record holds the base, and " Then merge as
// skill://legion-worker/references/conflicts-and-rewrites.md says, and push the issue branch."
// dispatch://INFRA-4374 (done; its ancestor dispatch://INFRA-3118's spec, approved at version 10,
// covers it) has the text give both: its R0 says that in implementing "it tells the implementer
// once for that head, on the implementer's role topic, that the head conflicts with its base, that
// GitHub runs no required check on it, and that it merges the base forward and pushes", and its R1
// that "it sends the implementer a task to merge the base forward and push". The Design's
// "Approved requirements this changes" does not name INFRA-4374, and dispatch://INFRA-3686's R8
// says that "a fact or a step that it has the text give stays as a field". So an ask in
// implementing holds a field that says the head conflicts, where the record holds no base too, and
// one that gives the push.
func TestAMergeForwardAskStatesTheConflictAndThePushAsFields(t *testing.T) {
	for _, base := range []string{"main", ""} {
		ask := MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: strings.Repeat("cd", 20), Base: base,
			Phase: phase.Implementing, PullRequest: 1401, Why: ConflictCheckNotGreen}
		fields := fieldsOfPublished(t, ask.Record("LEGION-41", PullRequest{Repo: "acme/tower"}))
		says := func(word string) bool {
			return slices.ContainsFunc(sortedFieldKeys(fields), func(key string) bool {
				return strings.Contains(key, word) || strings.Contains(fields[key], word)
			})
		}
		if !says("conflict") {
			t.Errorf("base %q: the merge-forward ask holds %v and no field that says the head conflicts with its base (INFRA-4374 R0)", base, fields)
		}
		if !says("push") {
			t.Errorf("base %q: the merge-forward ask holds %v and no field that gives the push of the merged base (INFRA-4374 R0 and R1)", base, fields)
		}
	}
}

// sortedFieldKeys is the keys of fields in order.
func sortedFieldKeys(fields map[string]string) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
