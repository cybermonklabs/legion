package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// While a stored rename target that is no owner/name stands, the read owed under the old name is
// read in every pass, and each read fails where the target is split into owner and name. The line
// the pass writes for it names the target, which is a stored setting of any length, so it names it
// to the 140 characters the name rule's own refusal shows (refs.SplitGitHubRepo): the line holds at
// most 140 characters of a target of 3,500, beside its other text.
func TestTheLineOfAnOwedReadUnderATargetWithNoSlashNamesTheTargetToABound(t *testing.T) {
	fastPageRetries(t)
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	const old, number = "acme/legacy-9105", 9105
	target := strings.Repeat("x", 3500)
	renameRepository(t, ctx, pool, old, target)
	if err := owePullRequestRead(ctx, pool, old, number, owedFailure, ""); err != nil {
		t.Fatal(err)
	}
	fake := newFakeGitHub(t)
	serveGitHubPulls(t, fake, "acme/widgets")
	NewReconcile(pool, fake.newTestClient(), events.NewBroker()).runOnce(context.WithoutCancel(ctx))
	var named int
	for _, record := range logs() {
		message, _ := record["msg"].(string)
		cause, _ := record["error"].(string)
		if !strings.Contains(message, "read a pull request owed a read") || !strings.Contains(cause, "xxxx") {
			continue
		}
		named++
		shown := longestRun(cause, 'x')
		t.Logf("level=%v error_bytes=%d target_shown=%d", record["level"], len(cause), shown)
		if shown > shownTargetRunes {
			t.Errorf("the line of the owed read of %s#%d names %d characters of the stored target in its error (%d bytes), want at most %d: a stored setting's text past its cut in every pass",
				old, number, shown, len(cause), shownTargetRunes)
		}
	}
	if named == 0 {
		t.Fatalf("no line of the pass names the read owed under %s and its target: the case did not happen", old)
	}
	if owed := owedCount(t, ctx, pool, old, number); owed != 1 {
		t.Errorf("%s#%d is owed %d reads after the pass, want 1: the read stays owed while the target stands", old, number, owed)
	}
}

// shownTargetRunes is the cut refs.SplitGitHubRepo shows a repository name to.
const shownTargetRunes = 140

// longestRun is the length of the longest run of r in text.
func longestRun(text string, r rune) int {
	longest, run := 0, 0
	for _, c := range text {
		if c != r {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return longest
}
