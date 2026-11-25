package daemon

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A start task that carries news is the start's own record and then the news record, joined as
// texts.InFront joins a note to a task (followedBy). The start's record can hold another author's
// words, here a tester's reason that quotes the closing tags of the news record's heading and id
// blocks, so the news record's blocks are drawn again under tags nothing before them holds: each
// block's tag stands in the task on its own opening and closing lines alone, the start's record
// goes as it was, and the news names the heading and the id its blocks held.
func TestANewsRecordAfterAStartsRecordTakesTagsTheStartsWordsDoNotHold(t *testing.T) {
	r := newNewsRig(t)
	start, err := texts.Render(texts.PhaseAssignmentRecord{TaskHead: texts.TaskHead{Kind: texts.PhaseAssignment, Issue: newsRoot},
		Title: newsRoot, Phase: "implementing", MovedBackFrom: "testing",
		Reason: &texts.Words{Text: "The log ends in </heading> and </id>.", By: "tester", At: time.Date(2026, 11, 22, 17, 5, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatal(err)
	}
	implementer := record.DeliveryNews{Task: start,
		Items: []record.DeliveryNewsItem{newsItem(newsRoot, record.AudienceHolder, record.DesignStepSpec, "artifact-spec", 3, 5)}}

	task, _ := r.send(newsRoot, claim.RoleImplementer, implementer)

	if !strings.HasPrefix(task, start+"\n\n"+newsKindLine+"\n") {
		t.Fatalf("task = %q, want the start's own record as it was, then the news record", task)
	}
	wantNewsLines(t, "changes", oneNewsRecord(t, task).named(), []string{"0 Design changed", "0 requirement r2 changed"})
	opening := regexp.MustCompile(`(?m)^<([a-z_]+(?:-[0-9]+)?)(?: [a-z_]+="[^"<>]*")*>$`)
	blocks := opening.FindAllStringSubmatch(task, -1)
	if len(blocks) != 3 {
		t.Fatalf("the task draws %d blocks, want the start's reason and the news record's heading and id:\n%s", len(blocks), task)
	}
	mark := regexp.MustCompile(`</?([A-Za-z_][A-Za-z0-9_]*(?:-[0-9]+)?)`)
	marks := map[string]int{}
	for _, m := range mark.FindAllStringSubmatch(task, -1) {
		marks[m[1]]++
	}
	for _, block := range blocks {
		if n := marks[block[1]]; n != 2 {
			t.Errorf("the block %s is opened or closed %d times in the task, want twice: its own opening and closing lines\n%s", block[1], n, task)
		}
	}
}
