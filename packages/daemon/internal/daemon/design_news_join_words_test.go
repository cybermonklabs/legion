package daemon

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A start task that carries news is the start's own record and then the news record, joined by
// followedBy, and main's renderNews (daemon/design_news_send.go at 8966cf45^2) sends "carried's
// own Task, a blank line, then the news". The news record carries another author's words too: the
// heading of a changed section is the person's own, as Dispatch names it. Here that heading quotes
// the closing tag of the tester's reason block in the start's record. dispatch://INFRA-3686/spec
// R1 puts each author's words "in a block of their own after its facts, ... which nothing else
// shares, and which those words cannot close", and Render keeps that for every block of a record
// (texts.blockTags: "nothing in the text opens or closes the tag of a block but that block's own
// lines"). So every block's tag stands in the task on its own opening and closing lines alone,
// whichever record's words hold the other's tag.
func TestANewsRecordsWordsHoldNoTagOfTheStartsBlocks(t *testing.T) {
	r := newNewsRig(t)
	// The implementer took version 4 of the spec, and what changed from 4 to 5 is recorded: one
	// section, whose heading quotes the closing tag of a reason block.
	const heading = "The `</reason>` line"
	r.inTx(func(tx pgx.Tx) error {
		if err := r.records.RaiseTold(r.ctx, tx, newsRoot, record.AudienceHolder,
			raisedTo(map[record.DesignStep]int{record.DesignStepSpec: 4}), time.Now()); err != nil {
			return err
		}
		return r.records.PutDesignChanges(r.ctx, tx, record.DesignChanges{
			VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 4, To: 5}, State: record.DesignChangesRead,
			Sections: []record.SectionChange{{Heading: heading, Change: "changed"}}, ReadAt: time.Now()})
	})
	const reason = "The tests fail on the second run."
	start, err := texts.Render(texts.PhaseAssignmentRecord{TaskHead: texts.TaskHead{Kind: texts.PhaseAssignment, Issue: newsRoot},
		Title: newsRoot, Phase: "implementing", MovedBackFrom: "testing",
		Reason: &texts.Words{Text: reason, By: "tester", At: time.Date(2026, 11, 22, 17, 5, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatal(err)
	}
	implementer := record.DeliveryNews{Task: start,
		Items: []record.DeliveryNewsItem{newsItem(newsRoot, record.AudienceHolder, record.DesignStepSpec, "artifact-spec", 4, 5)}}

	task, _ := r.send(newsRoot, claim.RoleImplementer, implementer)

	if !strings.HasPrefix(task, "kind: "+string(texts.PhaseAssignment)+"\n") || !strings.Contains(task, "\n\n"+newsKindLine+"\n") ||
		!strings.Contains(task, "\n"+reason+"\n") || !strings.Contains(task, "\n"+heading+"\n") {
		t.Fatalf("task = %q, want the start's record with the tester's reason, then the news record naming the heading", task)
	}
	opening := regexp.MustCompile(`(?m)^<([a-z_]+(?:-[0-9]+)?)(?: [a-z_]+="[^"<>]*")*>$`)
	blocks := opening.FindAllStringSubmatch(task, -1)
	if len(blocks) != 2 {
		t.Fatalf("the task draws %d blocks, want the start's reason and the news record's heading:\n%s", len(blocks), task)
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
