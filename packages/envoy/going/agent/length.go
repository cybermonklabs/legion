package agent

// length.go holds an answer of a run to the bound of the words the bar draws of it
// (agentwire.WordsBound). The prompt says an answer stays under 120 words, its chips' words
// counted, and that did not hold: at 8a3e562b, 6 of 11 answers to "What is the state of this
// issue's open children?" drew 120 words or more as the bar draws them, each chip its key, its
// status and its title. So before a reply in prose ends the turn done, the agent asks the server
// how many words the bar draws of it (drawnWords), and a reply of 120 or more is held back once
// in a turn of the run: it is saved, so the model reads what it wrote, and the next step's model
// is told how many words the person sees and asked for the same facts in fewer than 120 words, or
// for the same answer again where the person's words ask for more than a short answer (longTold).
// No other model call decides whether the words ask for more: that one turn does, as the
// architect ruled (dispatch://INFRA-1588/comment/77ddfc7a-b65c-4d36-b78c-c00e703c7d31). A reply
// after that turn ends the turn whole, whatever its count: the bar never cuts an answer.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/going/agentwire"
)

// longSaid starts the note of a reply held back as long (saveLong): what a later turn of the
// conversation reads of it, and what turnSoFar reads a step older than the history's whole turns
// by.
const longSaid = "The person did not see this reply, which the bar drew in too many words:"

// longTold is what the model is told after a reply held back as long, which the bar draws in
// words words: that count, how the bar counts them, and the architect's words.
func longTold(words int) string {
	return fmt.Sprintf("The person did not see your reply above: the bar draws it in %d words, each link as its item's chip, an issue's with its key, status and title, "+
		"and an answer has fewer than %d. If the person's words ask for more than a short answer, give the same answer again. "+
		"Otherwise give the same facts in fewer than %d words.", words, agentwire.WordsBound, agentwire.WordsBound)
}

// holdLong decides on prose, a reply that would end the run's turn done: it asks the server how
// many words the bar draws of it (drawnWords), which the reply's frame says (DrawnWords), and
// holds it back (saveLong) where they are agentwire.WordsBound or more, the turn held back no
// reply as long before it (lengthened) and the run's level leaves it a model call, as the guard's
// hold checks (endProse). It reports whether it held the reply back, which leaves the reply
// unsaved otherwise, and false for kept where the run is no longer the worker's to go on with by
// the call's end, which saves nothing. A count that fails holds nothing back: the reply ends the
// turn as it is, and the failure is logged.
func (w *worker) holdLong(prose *proseReply, lengthened bool) (held, kept bool) {
	words, err := w.drawnWords(prose.body.Text)
	if w.lost.Load() || w.grantEnded.Load() != nil || w.ctx.Err() != nil {
		return false, false
	}
	if err != nil {
		slog.Warn("agent: the words call of a reply in prose failed, so the reply ends the turn as it is", "run", w.run.ID, "error", err)
		return false, true
	}
	prose.frame.DrawnWords = &words
	if words < agentwire.WordsBound || lengthened || w.level.spent(w.name, w.used) != "" {
		return false, true
	}
	w.saveLong(prose, words)
	return true, true
}

// drawnWords asks the server how many words the bar draws of text as the run's answer: the words
// call (agentwire.KindWords), made through serverCall, so a lease the run no longer holds and a
// grant that ended stop the run as any call's refusal does. It answers an error where the call
// drew no count.
func (w *worker) drawnWords(text string) (int, error) {
	answer, told, _ := w.serverCall(w.ctx, agentwire.CallRequest{Kind: agentwire.KindWords, Text: text})
	if told != "" {
		return 0, errors.New(told)
	}
	var read agentwire.WordsAnswer
	if answer.Status != http.StatusOK || json.Unmarshal(answer.Body, &read) != nil {
		return 0, fmt.Errorf("the words call answered status %d with a body that reads as no count", answer.Status)
	}
	return read.Words, nil
}

// saveLong saves prose as a reply held back as long, which the bar draws in words words: its body
// marked with that count (modelBody.LongWords), so the run's history tells the model longTold
// after it and settled reads no end of the turn in it, a restarted worker's too; its note says
// the person did not see it (longSaid); and its frame marked HeldLong with no text, so the bar
// adds nothing of it to the answer, and the frame still says where the model call's time went.
func (w *worker) saveLong(prose *proseReply, words int) {
	prose.body.LongWords, prose.frame.HeldLong = words, true
	frame, _ := json.Marshal(prose.frame)
	w.save(agentwire.StepRequest{Step: prose.frame.Step, Kind: agentwire.StepModel, Note: note(longSaid + " " + prose.body.Text),
		Frames: []agentwire.Frame{{Name: frameReply, Data: frame}}}, prose.body)
}
