package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// refusedPayload is the published payload of rec's message-refused notice.
type refusedPayload struct {
	RefusedMove  string                     `json:"refused_move"`
	RefusedClose string                     `json:"refused_close"`
	Begins       string                     `json:"begins"`
	Words        map[string]texts.WordsMeta `json:"words"`
}

func refused(t *testing.T, rec texts.MessageRefusedRecord) refusedPayload {
	t.Helper()
	notice, err := record.NoticeOf(rec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	var payload refusedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

var cause = &dispatch.Error{Status: 404, Code: "NOT_FOUND", Message: "no route for POST /api/v1/issues/LEGION-2/messages"}

// R1 of dispatch://INFRA-3686: another author's words go "in a block of their own ... which
// nothing else shares". The block of a refused backward move holds what follows the daemon's
// sentence in the posted line, and that is not the asker's words alone: the daemon's writer
// (workflow's backwardMoveReason) closes words that end with no stop with a full stop of its own,
// and refusedPreview closes a line cut at 200 runes with an ellipsis of its own. Both are the
// daemon's characters in the asker's block, and a cut asker's words are marked in the block by
// that ellipsis rather than by the words object's own mark of a cut (texts.WordsMeta: rest, more).
func TestTheBeginsBlockHoldsOnlyWhatTheAskerWrote(t *testing.T) {
	long := strings.Repeat("the fuzz suite hangs on this input ", 10) // 350 runes
	for _, words := range []string{"The fuzz suite hangs on input", strings.TrimSpace(long)} {
		reason := moveSentence(phase.Testing, phase.Implementing) + ": " + words
		if !strings.HasSuffix(words, ".") {
			reason += "." // backwardMoveReason's own stop
		}
		got := refused(t, statusWriteRefused("LEGION-2", record.OrdinaryStatusWrite("in_progress", "testing", reason), cause, statusWrites{status: true}))
		if got.Words["begins"].By != "tester" {
			t.Fatalf("begins by %q, want the tester; this case proves nothing", got.Words["begins"].By)
		}
		if !strings.HasPrefix(words, got.Begins) {
			t.Errorf("the tester's block holds %q, which is not what the tester wrote (%q): the daemon's characters share the block", got.Begins, words)
		}
		if meta := got.Words["begins"]; len([]rune(words)) > len([]rune(got.Begins)) && meta.Rest == 0 && !meta.More {
			t.Errorf("the tester's words are cut to %d of %d runes and the words object marks no cut (rest %d, more %t)", len([]rune(got.Begins)), len([]rune(words)), meta.Rest, meta.More)
		}
	}
}
