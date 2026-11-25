package daemon

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A start row an earlier release queued carries that release's task as prose, and the outbox of
// this release runs it: where the claim is owed news of the approved design at that start,
// newsForStart renders the news after the task (renderNews, followedBy). Main's phase task ends with
// the backward move's reason as its author wrote it ("Reason: " + reason + ".", workflow's task at
// 74cb6855), and a tester's reason can hold a log in two `<details>` sections. That reason is no
// record Render drew: texts' drawnBlocks says "such a text is never read here, since its caller
// joins it as its author wrote it", and this change's handoff says an undrawn text goes as written,
// the ruling on an operator's task. followedBy joins it by texts.Joined, which reads the two
// sections as two blocks of one name and renames the second, so the implementer reads a tester's
// reason with a line the tester never wrote.
func TestAStartTaskAnEarlierReleaseQueuedGoesAsWrittenWithTheNewsAfterIt(t *testing.T) {
	const reason = "Two runs fail on the same race:\n<details>\nrun 1: TestRace timed out after 30s\n</details>\n" +
		"<details>\nrun 2: TestRace timed out after 30s\n</details>\nFix the lock order before the next run"
	start := "Continue Fix the race. Issue: LEGION-209. Phase: implementing." +
		" Handoff: test version 2 (dispatch handoff-read --issue LEGION-209 --phase test --version 2)." +
		" Handoff commit: 1f2e3d4c5b6a79881726354a5b6c7d8e9f0a1b2c. Pull request: #42. Reason: " + reason + "."
	news, err := texts.Render(texts.DesignNewsRecord{TaskHead: texts.TaskHead{Kind: texts.DesignNews, Issue: "LEGION-209"}, Tree: "LEGION-200",
		Audience: texts.NewsToHolder, Phase: "implementing",
		Items: []texts.DesignNewsItem{{Step: "spec", Document: "dispatch://LEGION-200/spec", Artifact: "artifact-spec", Version: 5, Taken: new(4),
			Changes: texts.NewsChangesNoneNamed}}})
	if err != nil {
		t.Fatalf("render the news: %v", err)
	}
	sent := followedBy(start, news)
	if !strings.HasPrefix(sent, start+"\n\n") {
		t.Errorf("the start task an earlier release queued is sent as\n%s\nwant it as that release wrote it, the tester's reason whole, then a blank line and the news:\n%s\n\n%s",
			sent, start, news)
	}
}
