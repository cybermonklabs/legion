package daemon

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The issue of a launch that ran Legion's own texts in place of its arm's folder is told in one
// Dispatch message for each claim and each cause, whichever of the claim's launches met it first:
// a second launch of the claim with the same cause tells nothing more, another cause of the claim
// is told, and so is the same cause of another claim. Each message, as the outbox carries it to
// Dispatch, names the launch (its claim and its generation), the experiment, the arm, the folder,
// the file, or the folder as a whole where no file applies, and the cause.
func TestATextsFallbackPostsOneMessageForEachClaimAndCause(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	const issue, folder = "LEGION-7", "/etc/legion/texts/shorter"
	seedTextsIssue(t, pool, issue)
	logs := &syncBuffer{}
	reports := &textsReports{log: slog.New(slog.NewJSONHandler(logs, nil))}
	reports.bind(pool, textsHandlers(t, pool))

	stale := texts.Fault{Dir: folder, Path: "roles/reviewer.md", Cause: texts.DigestDiffers,
		Detail: "manifest.json records sha256:" + strings.Repeat("1", 64) + ", and Legion's text is sha256:" + strings.Repeat("2", 64)}
	gone := texts.Fault{Dir: folder, Cause: texts.Gone}
	reviewer, tester := mustClaimToken(t, issue, claim.RoleReviewer), mustClaimToken(t, issue, claim.RoleTester)
	launch := func(token claim.Token, role claim.Role, generation uint64, fault texts.Fault) runtime.TextsReport {
		fingerprint := strings.Repeat("ab", 32)
		if fault.Cause == texts.Gone {
			fingerprint = ""
		}
		return runtime.TextsReport{Claim: token, Issue: issue, Role: role, Generation: generation, Experiment: "shorter", Variant: "folder",
			Folder: folder, Fingerprint: fingerprint, Fault: fault}
	}
	for n, report := range []runtime.TextsReport{
		launch(reviewer, claim.RoleReviewer, 1, stale),
		launch(reviewer, claim.RoleReviewer, 2, stale),
		launch(reviewer, claim.RoleReviewer, 3, gone),
		launch(tester, claim.RoleTester, 1, stale),
	} {
		reports.fellBack(report)
		// Each notice is applied before the next launch, as a claim's launches follow each other.
		testwait.Eventually(t, fmt.Sprintf("the notice of launch %d to be applied", n+1), func() bool {
			if failed := loggedMessages(t, logs.String(), textsNoticeFailedLine); len(failed) > 0 {
				t.Fatalf("a notice could not be applied: %v", failed)
			}
			return len(loggedMessages(t, logs.String(), textsNoticedLine))+len(loggedMessages(t, logs.String(), textsNoticedAgainLine)) == n+1
		})
	}

	messages := textsMessages(t, pool, issue)
	want := []struct {
		token      claim.Token
		generation uint64
		file       string
		cause      texts.Cause
	}{
		{reviewer, 1, "the file " + strconv.Quote("roles/reviewer.md"), texts.DigestDiffers},
		{reviewer, 3, "the folder as a whole", texts.Gone},
		{tester, 1, "the file " + strconv.Quote("roles/reviewer.md"), texts.DigestDiffers},
	}
	if len(messages) != len(want) {
		t.Fatalf("the issue was told %d messages, want one for each claim and cause, %d: %q", len(messages), len(want), messages)
	}
	for i, w := range want {
		for _, name := range []string{
			fmt.Sprintf("claim %s at generation %d", w.token, w.generation),
			"experiment " + strconv.Quote("shorter"), "arm " + strconv.Quote("folder"), strconv.Quote(folder), w.file,
			"the cause " + strconv.Quote(string(w.cause)),
		} {
			if !strings.Contains(messages[i], name) {
				t.Errorf("message %d is %q, want it to name %s", i, messages[i], name)
			}
		}
	}
	for token, facts := range map[claim.Token]int{reviewer: 2, tester: 1} {
		if got := textsFacts(t, pool, token); got != facts {
			t.Errorf("intake applied %d fallback facts of %s, want %d, one for each of its causes", got, token, facts)
		}
	}
}
