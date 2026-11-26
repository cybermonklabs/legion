package agent

// length.go holds an answer of a run to the bound of the words the bar draws of it
// (wordsBound). The prompt says an answer stays under 120 words, its chips' words
// counted, and that did not hold: at 8a3e562b, 6 of 11 answers to "What is the state of this
// issue's open children?" drew 120 words or more as the bar draws them, each chip its key, its
// status and its title. So before a reply in prose ends the turn done, the agent asks the server
// how many words the bar draws of it (drawnWords), and a reply of 120 or more is held back once
// in a turn of the run: it is saved, so the model reads what it wrote, and the next step is its
// rewrite, whose model is told how many words the person sees and asked for the same facts in at
// most retryWords words as the person sees them, without the key, status and title each issue's
// chip already shows, or for the same answer again where the person's words ask for more than a
// short answer (longTold). No other model call decides whether the words ask for more: the
// rewrite does, as the architect ruled
// (dispatch://INFRA-1588/comment/77ddfc7a-b65c-4d36-b78c-c00e703c7d31).
// The rewrite is no new run, as the architect ruled once a rewrite at the run's level wrote 5,526
// to 17,565 reasoning tokens and made the person wait 1.4 to 4.4 minutes for the answer
// (dispatch://INFRA-1588/comment/7c38ba3b-bde1-4bb4-bcc7-f966b0968b8c): it is one model call,
// offered no tool, at the lowest effort the chat model's calls ask for (rewriteEffort), whose
// reasoning the pacer reserves at that effort (rewriteReasoningTokens), within rewriteCallTime of
// what is left of the run's working time (rewrite). At 52f1406a that call was given the run's
// whole conversation, and ran past its bound in 2 of 2 tries of the real model, so, as the
// architect ruled (dispatch://INFRA-1588/comment/8a23ec38-289e-44c4-88a0-b332fd3f5453), it is
// given a short system message of the answer's rules (rewritePrompt), the person's words, the held
// reply and longTold alone: no read and no earlier step of the run. It shortens the held reply and
// adds nothing to it: a reply of it that links what the held reply does not (addedLink), an issue
// key written bare counted as the link the server may make of it, ends the turn done with the
// held reply shown whole (showHeld), and any other reply of it that writes words ends the turn
// whole, whatever its count: the bar never cuts an answer. A rewrite that draws no reply the
// person may read ends the turn done with the held reply shown whole too, so the bar never shows
// nothing: one its bound ended, whose call failed, or whose reply was cut at its output tokens
// (missedRewrite, errRewriteCut); one whose reply writes no words, none or white space alone
// (endRewrite); and one the person stopped while its call worked, which frees the run's lease
// before the agent hears of the stop, so the agent saves nothing and the server's stop saves the
// held reply shown whole as the stopped run's answer (StopStep, stopGoingRunIn in
// internal/dispatch/api/going_agent.go). A grant that ends as the rewrite's reply is counted stops
// the run with the grant's reason, the run's lease still held, so the held reply is shown whole
// first (endRewrite).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/going/chips"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// wordsBound is the bound of the words the bar draws of an answer: an answer has fewer unless the
// person's words ask for more, and a reply of wordsBound or more gets its rewrite (holdLong).
const wordsBound = 120

// retryWords is the length longTold asks of the rewrite: the same facts in at most that many words
// as the person sees them. It stands below wordsBound with a margin, because the model writes the
// reply and the bar counts it: told to stay under wordsBound, a model wrote a second reply over
// it, which the turn then showed whole.
const retryWords = 90

// rewriteCallTime bounds the rewrite, the one model call after a reply held back as long, given
// rewritePrompt, the person's words, the held reply and longTold alone, within what is left of the
// run's working time, as the architect ruled: past it the held reply ends the turn whole
// (missedRewrite). At 84294a61 the rewrite, at rewriteEffort and with that short input, ran past
// it in 4 of 6 held replies of the real model, and the 2 that ended took 19.5 s and 18.4 s, with
// about 1,240 reasoning tokens each. A variable, so a test shortens it.
var rewriteCallTime = 20 * time.Second

// errRewriteCut ends agentloop.Run once the rewrite's one model call finished with a reply cut at
// its output tokens, which is not saved: a cut reply is no answer the person may read, and the bar
// never cuts one, so the held reply ends the turn whole in its place (missedRewrite), saved as the
// rewrite's step.
var errRewriteCut = errors.New("the rewrite's reply was cut at its output tokens")

// The rewrite's effort and its reserve are the guard's check's (noPlanEffort,
// noPlanReasoningTokens): the lowest reasoning effort the chat model's calls ask for, Fast's too
// (Levels), and the reasoning tokens the pacer reserves for a call at that effort beside its output
// tokens and the tokens of its short prompt, Fast's too (Level.ReasoningTokens). Low is the lowest
// the chat model takes: Azure's grok-4.7 refuses a call that asks for no reasoning (effort off,
// sent as "none") with HTTP 400, which the preview at c0fc22e7 measured in 8 of 8 rewrites.
const (
	rewriteEffort          = noPlanEffort
	rewriteReasoningTokens = noPlanReasoningTokens
)

// longSaid starts the note of a reply held back as long (saveLong): what a later turn of the
// conversation reads of it, and what turnSoFar reads a step older than the history's whole turns
// by.
const longSaid = "The person did not see this reply, which the bar drew in too many words:"

// longTold is what the model is told after a reply held back as long, which the bar draws in
// words words: that count and how the bar counts them, the first link to each issue as the
// issue's chip and a later link to that issue, or a link to another item with words of its own,
// as those words; that it gives the same answer again where the person's words ask for more than
// a short answer, as the architect ruled; and otherwise the same facts in at most retryWords words
// as the person sees them, the prose repeating nothing that an issue's chip already shows.
func longTold(words int) string {
	return fmt.Sprintf("The person did not see your reply above: the bar draws it in %d words, the first link to each issue as the issue's chip with its key, status and title, "+
		"and a later link to that issue, or a link to another item with words of its own, as those words; an answer has fewer than %d. "+
		"If the person's words ask for more than a short answer, give the same answer again. "+
		"Otherwise give the same facts in at most %d words as the person sees them: each issue's chip already shows its key, status and title, so do not repeat them in the prose.",
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
// after it and settled reads no end of the turn in it, a restarted worker's too, and with the tool
// steps the run withheld from the model so far (modelBody.Withheld), which the reply shown whole
// tells of (shownHeld), whoever shows it; its note says the person did not see it (longSaid); and
// its frame marked HeldLong (saveHeld), which tells of no withheld step: the person reads none of
// it.
func (w *worker) saveLong(prose *proseReply, words int) {
	prose.body.LongWords, prose.body.Withheld, prose.frame.HeldLong = words, w.withheld, true
	w.saveHeld(prose, longSaid)
}

// heldLast answers the body of the reply held back as long (saveLong) that the run's history ends
// with, and false where its last step is any other: the next step is then that reply's rewrite
// (rewrite). A held reply the history gives by its note alone makes none: a claim gives its last
// steps whole (agentwire.WholeFrom).
func (w *worker) heldLast() (modelBody, bool) {
	if len(w.history) == 0 {
		return modelBody{}, false
	}
	last := w.history[len(w.history)-1]
	if last.Kind != agentwire.StepModel {
		return modelBody{}, false
	}
	return heldBody(last.Body)
}

// heldBody decodes raw, the saved body of a model step, and answers it and whether it is the body
// of a reply held back as long (saveLong), one that counts its long words (modelBody.LongWords); a
// body that does not decode answers an empty body and false.
func heldBody(raw json.RawMessage) (modelBody, bool) {
	var held modelBody
	if json.Unmarshal(raw, &held) != nil {
		return modelBody{}, false
	}
	return held, held.LongWords > 0
}

// rewrite makes request, that of the step after held, a reply held back as long, the reply's
// rewrite: one model call given rewritePrompt as its system message and three messages - the
// person's words of the run as every step gives them (askedNow), held as the model's own reply,
// and longTold of held's count after it - so no read, no earlier step and no tool of the run, at
// rewriteEffort, within rewriteCallTime of what is left of the run's working time, and bounded at
// the level's output tokens still. It answers the tokens the pacer reserves for the call: a token
// for each charsPerToken characters of its prompt, the level's output tokens and
// rewriteReasoningTokens, not the level's reasoning tokens, as the guard's check reserves its own
// (checkProse).
func (w *worker) rewrite(request *agentloop.Request, held modelBody) int {
	request.System = rewritePrompt
	request.Input = []agentloop.Message{
		{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: askedNow(w.run.Words)}}},
		{Role: agentloop.Assistant, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: held.Text}}},
		{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: longTold(held.LongWords)}}},
	}
	request.Model.Effort = rewriteEffort
	request.Bounds.Calls, request.Bounds.Time = 1, min(request.Bounds.Time, rewriteCallTime)
	level := w.level
	level.ReasoningTokens = rewriteReasoningTokens
	return level.estimate(going.PromptChars(request.System, request.Input, nil))
}

// endRewrite ends the run's turn with prose, the reply of the rewrite of held, the reply held
// back as long, as endProse ends a reply in prose, where it writes words and each link of it is a
// link of held. A reply that writes no words - none, or white space alone - gives the person
// nothing to read, and one that links what held does not (addedLink) added to held, which the
// rewrite may not do: either ends the turn done with held shown whole in its place (showHeld), as
// at the rewrite's bound, and the agent logs why. A grant that ended as endProse counted the reply
// (holdLong) stops the run with the grant's reason and saves nothing of the reply; the run's
// lease is still held, so held is shown whole first, on the run's last word (lastWord), since the
// grant's end cancelled the run's context, and the agent logs why.
func (w *worker) endRewrite(held modelBody, prose *proseReply) stepOutcome {
	if strings.TrimSpace(prose.body.Text) == "" {
		slog.Warn("agent: the rewrite of a reply held back as long wrote no words, so the held reply ends the turn whole", "run", w.run.ID)
		return w.showHeld(w.ctx, prose.frame.Step, held)
	}
	if link, added := addedLink(held.Text, prose.body.Text); added {
		slog.Warn("agent: the rewrite of a reply held back as long links what the held reply does not, so the held reply ends the turn whole", "run", w.run.ID, "link", link)
		return w.showHeld(w.ctx, prose.frame.Step, held)
	}
	outcome := w.endProse(prose)
	if reason := w.grantEnded.Load(); reason != nil && !w.lost.Load() {
		if _, still := w.heldLast(); still {
			slog.Warn("agent: the grant of a run ended as the rewrite of its reply held back as long was counted, so the held reply ends the turn whole", "run", w.run.ID, "reason", *reason)
			ctx, cancel := w.lastWord()
			defer cancel()
			w.showHeld(ctx, prose.frame.Step, held)
		}
	}
	return outcome
}

// addedLink is the first link of rewritten, a rewrite's reply, that is no link of held, the reply
// held back as long, and false where each of its links is a link of held: each link read as
// answerLinks reads it, by what it names, so a key held writes bare is one of its links, and a
// rewrite that links it adds nothing.
func addedLink(held, rewritten string) (string, bool) {
	had := map[string]bool{}
	answerLinks(held, func(link string) { had[link] = true })
	added, found := "", false
	answerLinks(rewritten, func(link string) {
		if !found && !had[link] {
			added, found = link, true
		}
	})
	return added, found
}

// answerLinks gives found each link the bar may draw of answer, an answer's markdown, by what it
// names (linkName). First each issue key answer writes bare is written as the issue's link, by the
// scan the server runs on an answer before it stores it (chips.LinkBareKeys, goingStoredAnswer):
// outside links, code and escaped characters, a whole token. The server links only a key that a
// tool result the run saved holds and that names an issue the run's person may read, which the
// agent cannot know: a restarted worker holds only its last turns' results whole, and the words
// call answers a count, not the person's reads (going_agent_words.go). So every bare word of a
// key's shape counts as a link here, SHA-256 or UTF-8 too: a rewrite that writes one the held
// reply does not adds to it, which it may not do anyway, and the held reply is shown whole. Then
// it reads each markdown link, [words](href) and <href>, outside code, as the server reads them to
// count the words the bar draws (chips.RewriteLinks), and each dispatch:// reference answer
// writes, a bare one too, as Dispatch reads one in text (text.DispatchSpans).
func answerLinks(answer string, found func(link string)) {
	answer = chips.LinkBareKeys(answer, func(string) bool { return true })
	chips.RewriteLinks(answer, func(_ *strings.Builder, _, _, href string) {
		found(linkName(href))
	})
	for _, span := range text.DispatchSpans(answer) {
		found(linkName(answer[span[0]:span[1]]))
	}
}

// linkName is what a link of an answer names, one name for two links to the same item: the
// dispatch:// reference of the item the bar reads the link as (chips.ParseTarget,
// chips.Target.Reference), so [ACME-7](/issues/ACME-7) and <dispatch://ACME-7> name one issue,
// and the link's address as written for any other link, such as a web address or a time.
func linkName(href string) string {
	if target, ok := chips.ParseTarget(href); ok {
		return "dispatch://" + target.Reference()
	}
	return href
}

// missedRewrite ends the run's turn done with held, the body of the reply held back as long,
// shown whole (showHeld), where its rewrite, the step numbered base, drew no reply the person may
// read: its bound ended it, its call failed, was refused or was held back for the quota, or its
// reply was cut at its output tokens, which saves nothing (errRewriteCut), as its result and err
// say, which the agent logs.
func (w *worker) missedRewrite(base int, held modelBody, result agentloop.Result, err error) stepOutcome {
	why := modelFailure(result)
	if err != nil {
		why = err.Error()
	}
	slog.Warn("agent: the rewrite of a reply held back as long drew no reply, so the held reply ends the turn whole", "run", w.run.ID, "error", why)
	return w.showHeld(w.ctx, base, held)
}

// showHeld ends the run's turn done with held, the body of the reply held back as long, whose
// rewrite, the step numbered base, drew no reply the person may read: no reply came, or it was cut
// at its output tokens (missedRewrite), or it wrote no words or linked what held does not, or the
// run's grant ended as its reply was counted (endRewrite). The reply is saved again whole as that
// step on ctx, the run's context or its last word's (shownHeld), so the run's history ends the
// turn with it (settled) and a later turn reads it as the answer the person saw; its frame
// carries its prose and the count the bar drew of it (DrawnWords), not HeldLong, so the bar draws
// it as the answer, and nothing is cut. A rewrite the person stopped saves nothing here: the stop
// freed the run's lease, and the server's stop saves the held reply shown whole in its place
// (StopStep).
func (w *worker) showHeld(ctx context.Context, base int, held modelBody) stepOutcome {
	request, body := shownHeld(base, held)
	w.saveIn(ctx, request, body)
	w.merge()
	return stepOutcome{state: agentwire.StateDone}
}

// shownHeld is the step that saves held, the body of a reply held back as long, shown whole as
// the step numbered base, and the body it saves (modelStep): the body with its count of long
// words, its output tokens and its record of withheld steps cleared, and a frame with no call and
// the count the bar drew of it. The reply tells of the tool steps held records the run had
// withheld from the model when it was held back (saveLong): its body says the notice of them after
// its text and its frame their count, as a reply that ends the turn does (saveModel), whichever
// worker shows it, a restarted one too, or the server's stop (StopStep). The frame says no model
// call's time: no call wrote the reply again.
func shownHeld(base int, held modelBody) (agentwire.StepRequest, modelBody) {
	words, withheld := held.LongWords, held.Withheld
	held.LongWords, held.Output, held.Withheld = 0, 0, nil
	return modelStep(held, replyFrame{Step: base, Tools: []string{}, Labels: []string{}, DrawnWords: &words}, nil, withheld)
}

// StopStep is the step the server's stop saves for a run whose last step is a held reply: held,
// the saved body of a reply held back as long, before the reply's rewrite ended the turn
// (internal/dispatch/api's stopGoingRunIn). The stop frees the run's lease, so the run's agent can
// save nothing after it. It is held shown whole as the step numbered base, as showHeld saves it
// (shownHeld), the notice of the tool steps the run had withheld from the model included: the
// step's body, its note and the data of its reply frame, which carries the reply's prose and how
// many results were withheld. It answers false where held is no body of a reply held back as long
// (heldBody).
func StopStep(base int, held json.RawMessage) (body json.RawMessage, line string, frame json.RawMessage, ok bool) {
	read, isHeld := heldBody(held)
	if !isHeld {
		return nil, "", nil, false
	}
	request, shown := shownHeld(base, read)
	// A model step's body always encodes: save encodes it so too.
	body, _ = json.Marshal(shown)
	return body, request.Note, request.Frames[0].Data, true
}
