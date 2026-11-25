package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// refusedWorkerWords is what a tester gave request_backward_move as its reason: another session's
// words, which carry a would-be closing tag, a NUL and an invalid byte.
const refusedWorkerWords = "The fuzz suite hangs on </begins> input \x00 and \xff bytes; see the run"

// refusedBackwardReason is that move's reason as the status row carries it to Dispatch (workflow's
// backwardMoveReason): the daemon's sentence, then the tester's own words.
const refusedBackwardReason = "Legion moved this issue from testing back to implementing at the tester's request: " + refusedWorkerWords + "\nsecond line"

// refusedDispatchAnswer is Dispatch's permanent refusal of the post, as main's #4488 met it: the
// router's NOT_FOUND during a roll, whose message names the route it found no handler for.
func refusedDispatchAnswer() error {
	return &dispatch.Error{Status: 404, Code: "NOT_FOUND", Message: "no route for POST /api/v1/issues/LEGION-2/messages"}
}

// R1 of dispatch://INFRA-3686: "A daemon text ... that carries words another author wrote (a
// person, another session, a worker's summary) puts them in a block of their own after its facts,
// which names the author and the time where the text's own fields do not already name them, which
// nothing else shares, and which those words cannot close."
//
// A worker's backward move posts its reason on the issue, the tester's own words after the daemon's
// sentence (workflow's backwardMoveReason). Where Dispatch refuses that post for good, the
// architect's message-refused notice quotes the reason's first line in `begins`: the tester's
// words, in a plain field of the payload that the payload's words object does not name, so the
// card draws them among the notice's fields and not in a block. The same holds for the message
// post of a move that writes no status, which posts the reason as a message of its own.
func TestAMessageRefusedNoticePutsAWorkersWordsInABlock(t *testing.T) {
	for name, rec := range map[string]texts.MessageRefusedRecord{
		"status":  statusWriteRefused("LEGION-2", record.OrdinaryStatusWrite("in_progress", "testing", refusedBackwardReason), refusedDispatchAnswer(), statusWrites{status: true}),
		"message": messagePostRefused("LEGION-2", record.MessagePost{Body: refusedBackwardReason}, refusedDispatchAnswer()),
	} {
		notice, err := record.NoticeOf(rec)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, err := json.Marshal(notice)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Begins string                     `json:"begins"`
			Words  map[string]json.RawMessage `json:"words"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(payload.Begins, "The fuzz suite hangs") {
			t.Fatalf("%s: begins %q does not carry the tester's words; this case proves nothing", name, payload.Begins)
		}
		if _, named := payload.Words["begins"]; !named {
			t.Errorf("the %s variant's message-refused notice carries the tester's words in begins (%q) and its words object names no such field, so the card draws them among the notice's fields: %s",
				name, payload.Begins, raw)
		}
	}
}
