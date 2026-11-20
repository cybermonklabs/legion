package docs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// asAgentReadsIt is document as an agent reads it through the text routes (TextWithAsks): its
// markdown, then a line `asks:` and one line per entry of its asks member (askLines). A person's
// answer is held on the ask's node alone, so the asks member is where a
// test reads it beside the text; it fails t where the markdown holds a server-owned attribute of
// an ask block, which the text routes take off every ask block.
func asAgentReadsIt(t *testing.T, service *Service, document string) string {
	t.Helper()
	text, err := service.TextWithAsks(context.Background(), document, false)
	if err != nil {
		t.Fatalf("read %s as an agent reads it: %v", document, err)
	}
	requireNoAskServerAttributes(t, document, text.Markdown)
	return text.Markdown + "\nasks:\n" + askLines(text.Asks)
}

// askLines is entries as these tests compare them, one line each, each ended by a newline: `#`
// and the block id, then each value the entry holds as name="value", in the order the release
// before wrote them on the block's opening line, the options chosen as their JSON list.
func askLines(entries []model.TextAsk) string {
	var lines strings.Builder
	for _, entry := range entries {
		lines.WriteString("#" + entry.BlockID)
		if entry.State != "" {
			lines.WriteString(` state="` + entry.State + `"`)
		}
		if entry.AnsweredBy != "" {
			lines.WriteString(` answered_by="` + entry.AnsweredBy + `"`)
		}
		if entry.AnsweredAt != "" {
			lines.WriteString(` answered_at="` + entry.AnsweredAt + `"`)
		}
		if entry.Selected != nil {
			selected, _ := json.Marshal(*entry.Selected)
			lines.WriteString(" selected=" + string(selected))
		}
		if entry.Answer != nil {
			lines.WriteString(` answer="` + *entry.Answer + `"`)
		}
		if entry.CopiedFrom != "" {
			lines.WriteString(` copied_from="` + entry.CopiedFrom + `"`)
		}
		if entry.CopiedFromDocument != "" {
			lines.WriteString(` copied_from_document="` + entry.CopiedFromDocument + `"`)
		}
		lines.WriteString("\n")
	}
	return lines.String()
}

// requireNoAskServerAttributes fails t where an ask block's opening line in markdown, the text
// routes' rendering of document, carries one of the server-owned attributes
// (askServerAttributeNames).
func requireNoAskServerAttributes(t *testing.T, document, markdown string) {
	t.Helper()
	for _, line := range strings.Split(markdown, "\n") {
		kind, attrs, opens := pmdoc.ParseTypedDirectiveOpen(strings.TrimLeft(line, " \t>"))
		if !opens || kind != "ask" {
			continue
		}
		for _, name := range askServerAttributeNames {
			if _, held := attrs[name]; held {
				t.Fatalf("the text of %s an agent reads carries the server-owned ask attribute %s:\n%s", document, name, markdown)
			}
		}
	}
}

// While another build serves, settlement keeps the server-owned attributes of an ask block in step
// with its asks row in the live document, for that build's pages (rollWindowAskAttributes), and
// the text routes take them off all the same (TextWithAsks): read through the room that holds the
// document and through the cache once no room does, with its blocks and without, the markdown an
// agent reads holds none of them, the blocks' byte ranges index that markdown, the token is the
// document's own, and the answer reads in the asks member.
func TestTheTextAnAgentReadsHoldsNoServerOwnedAskAttributeWhileAnotherBuildServes(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	repaired := seedAnsweredAskBlock(t, service, artifactID)
	service.settleRoom(artifactID, 0)
	live, token, err := service.TextWithToken(ctx, artifactID)
	if err != nil || live != repaired {
		t.Fatalf("the live document reads %q (%v), want the roll window's attributes on its ask block:\n%s", live, err, repaired)
	}
	answeredAt := strings.Split(strings.Split(repaired, `answered_at="`)[1], `"`)[0]
	const served = ":::ask{#ask-block urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n\nContext.\n"
	wantAsks := `#ask-block state="answered" answered_by="alice" answered_at="` + answeredAt + `" selected=[]` + "\n"
	read := func(what string) {
		t.Helper()
		for _, withBlocks := range []bool{false, true} {
			text, err := service.TextWithAsks(ctx, artifactID, withBlocks)
			if err != nil {
				t.Fatalf("%s, blocks %t: %v", what, withBlocks, err)
			}
			if text.Markdown != served || text.Token != token {
				t.Fatalf("%s, blocks %t: the text reads %q with token %q, want %q with the document's own token %q", what, withBlocks, text.Markdown, text.Token, served, token)
			}
			if got := askLines(text.Asks); got != wantAsks {
				t.Fatalf("%s, blocks %t: the asks read %q, want %q", what, withBlocks, got, wantAsks)
			}
			if withBlocks != (len(text.Blocks) > 0) {
				t.Fatalf("%s, blocks %t: %d blocks", what, withBlocks, len(text.Blocks))
			}
			for _, block := range text.Blocks {
				if block.ID == "ask-block" && text.Markdown[block.From:block.To] != ":::ask{#ask-block urgency=\"med\" multiple=\"false\"}\nShip it?\n:::" {
					t.Fatalf("%s: the ask block's range [%d, %d) reads %q in the text served, want the block as it serves it", what, block.From, block.To, text.Markdown[block.From:block.To])
				}
			}
		}
	}
	read("the room's read")
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("evict the room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
	read("the cold read")
	read("the cached read")
}
