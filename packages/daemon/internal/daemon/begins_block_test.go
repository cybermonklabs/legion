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

// moveSentence is the daemon's own sentence a backward move's reason opens with
// (workflow.backwardMoveAsked), for each move back the daemon counts.
func moveSentence(from, to phase.Phase) string {
	return "Legion moved this issue from " + string(from) + " back to " + string(to) + " at the " + string(phase.RoleFor(from)) + "'s request"
}

// R1 of dispatch://INFRA-3686: a daemon text that carries words another author wrote "puts them
// in a block of their own after its facts, which names the author and the time where the text's
// own fields do not already name them, which nothing else shares, and which those words cannot
// close."
//
// A worker's backward move posts on the issue the daemon's own sentence ("Legion moved this issue
// from testing back to implementing at the tester's request") and then the worker's words. Where
// Dispatch refuses that post for good, the architect's message-refused notice quotes its first
// line in begins and names begins in its words object with the asker as its author
// (refusedLineBy, workflow.BackwardMoveAsker). So the block the card draws holds the daemon's
// sentence beside the worker's words, under the worker's name: the block is shared, and it names
// the worker as the author of words the daemon wrote. Each move back the daemon counts, for each
// role that asks one, gives such a block. The facts of the daemon's sentence (the phases and the
// role) belong among the notice's fields; the block holds the worker's words alone.
func TestTheBeginsBlockHoldsTheAskersWordsAlone(t *testing.T) {
	const words = "The fuzz suite hangs on input"
	cause := &dispatch.Error{Status: 404, Code: "NOT_FOUND", Message: "no route for POST /api/v1/issues/LEGION-2/messages"}
	for _, pair := range phase.BackwardPairs() {
		from, to, _ := phase.ParsePair(pair)
		reason := moveSentence(from, to) + ": " + words + "."
		for name, rec := range map[string]texts.MessageRefusedRecord{
			"status":  statusWriteRefused("LEGION-2", record.OrdinaryStatusWrite("in_progress", "testing", reason), cause, statusWrites{status: true}),
			"message": messagePostRefused("LEGION-2", record.MessagePost{Body: reason}, cause),
		} {
			notice, err := record.NoticeOf(rec)
			if err != nil {
				t.Fatalf("%s %s: %v", pair, name, err)
			}
			raw, err := json.Marshal(notice)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Begins string                     `json:"begins"`
				Words  map[string]texts.WordsMeta `json:"words"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			by := payload.Words["begins"].By
			if by != string(phase.RoleFor(from)) {
				t.Errorf("%s %s: begins is named by %q, want the asker %q", pair, name, by, phase.RoleFor(from))
			}
			if strings.Contains(payload.Begins, "Legion moved this issue") {
				t.Errorf("%s %s: the block by %q holds the daemon's own sentence beside the %s's words: %q", pair, name, by, by, payload.Begins)
			}
		}
	}
}
