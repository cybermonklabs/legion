package agent

// length.go holds an answer of a run to the bound of the words the bar draws of it
// (wordsBound). The prompt says an answer stays under 120 words, its chips' words
// counted, and that did not hold: at 8a3e562b, 6 of 11 answers to "What is the state of this
// issue's open children?" drew 120 words or more as the bar draws them, each chip its key, its
// status and its title. So before a reply in prose ends the turn done, the agent asks the server
// how many words the bar draws of it (drawnWords), and a reply of 120 or more is held back once
// in a turn of the run: it is saved, so the model reads what it wrote, and the next step's model
// is told how many words the person sees and asked for the same facts in at most retryWords words
// as the person sees them, without the key, status and title each link already shows, or for the
// same answer again where the person's words ask for more than a short answer (longTold).
// No other model call decides whether the words ask for more: that one turn does, as the
// architect ruled (dispatch://INFRA-1588/comment/77ddfc7a-b65c-4d36-b78c-c00e703c7d31). A reply
// after that turn ends the turn whole, whatever its count: the bar never cuts an answer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/going/agentwire"
)

// wordsBound is the bound of the words the bar draws of an answer: an answer has fewer unless the
// person's words ask for more, and a reply of wordsBound or more gets one more model turn
// (holdLong).
const wordsBound = 120

// retryWords is the length longTold asks of the one more turn: the same facts in at most that many
// words as the person sees them. It stands below wordsBound with a margin, because the model writes
// the reply and the bar counts it: told to stay under wordsBound, a model wrote a second reply over
// it, which the turn then showed whole.
const retryWords = 90

// longSaid starts the note of a reply held back as long (saveLong): what a later turn of the
// conversation reads of it, and what turnSoFar reads a step older than the history's whole turns
// by.
const longSaid = "The person did not see this reply, which the bar drew in too many words:"

// longTold is what the model is told after a reply held back as long, which the bar draws in
// words words: that count and how the bar counts them; that it gives the same answer again where
// the person's words ask for more than a short answer, as the architect ruled; and otherwise the
// same facts in at most retryWords words as the person sees them, the prose repeating nothing that
// a link already shows.
func longTold(words int) string {
	return fmt.Sprintf("The person did not see your reply above: the bar draws it in %d words, each link as its item's chip, an issue's with its key, status and title, "+
		"and an answer has fewer than %d. If the person's words ask for more than a short answer, give the same answer again. "+
		"Otherwise give the same facts in at most %d words as the person sees them: each link already shows its item's key, status and title, so do not repeat them in the prose.",
		words, wordsBound, retryWords)
}

// holdLong decides on prose, a reply that would end the run's turn done: it asks the server how
// many words the bar draws of it (drawnWords), which the reply's frame says (DrawnWords), and
// holds it back (saveLong) where they are wordsBound or more, the turn held back no reply as long
// before it (heldLong) and the run's level leaves it a model call, as the guard's hold checks
// (endProse). It reports whether it held the reply back, which leaves the reply unsaved
// otherwise, and false for kept where the run is no longer the worker's to go on with by the
// call's end, which saves nothing. A count that fails, or that the call drew none of within
// wordsCallTime, holds nothing back: the reply ends the turn as it is, and the failure is logged.
func (w *worker) holdLong(prose *proseReply, heldLong bool) (held, kept bool) {
	words, err := w.drawnWords(prose.body.Text)
	if w.lost.Load() || w.grantEnded.Load() != nil || w.ctx.Err() != nil {
		return false, false
	}
	if err != nil {
		slog.Warn("agent: the words call of a reply in prose failed, so the reply ends the turn as it is", "run", w.run.ID, "error", err)
		return false, true
	}
	prose.frame.DrawnWords = &words
	if words < wordsBound || heldLong || w.level.spent(w.name, w.used) != "" {
		return false, true
	}
	w.saveLong(prose, words)
	return true, true
}

// drawnWords asks the server how many words the bar draws of text as the run's answer: the words
// call (agentwire.KindWords), made through serverCall, so a lease the run no longer holds and a
// grant that ended stop the run as any call's refusal does, within wordsCallTime: the call is
// made once the step's watchdog stopped, and serverCall waits out a sign-in the issuer cannot
// confirm until its context ends. It answers an error where the call drew no count.
func (w *worker) drawnWords(text string) (int, error) {
	ctx, cancel := context.WithTimeout(w.ctx, wordsCallTime)
	defer cancel()
	answer, told, _ := w.serverCall(ctx, agentwire.CallRequest{Kind: agentwire.KindWords, Text: text})
	switch {
	case told == "":
	case ctx.Err() != nil && w.ctx.Err() == nil:
		return 0, fmt.Errorf("the words call drew no count within %s", wordsCallTime)
	default:
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
// the person did not see it (longSaid); and its frame marked HeldLong (saveHeld).
func (w *worker) saveLong(prose *proseReply, words int) {
	prose.body.LongWords, prose.frame.HeldLong = words, true
	w.saveHeld(prose, longSaid)
}
