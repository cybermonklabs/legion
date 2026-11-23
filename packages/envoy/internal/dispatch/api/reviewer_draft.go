package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
)

// reviewer_draft.go serves POST /api/v1/reviewer-drafts (INFRA-3231's R5, Decision 12 of its
// plan): the reviewer's review of a draft, which writes nothing. It answers whenever the
// reviewer's model group is set, whatever DISPATCH_REVIEWER_<KIND> say, since it holds and posts
// nothing.

// reviewerDraftInput is POST /api/v1/reviewer-drafts's body: the draft's kind and its wording, and
// where it would stand, the issue it would be on and the person who would read it.
type reviewerDraftInput struct {
	// Kind is ask, issue, comment, message, document or decision_block.
	Kind string `json:"kind" required:"true"`
	// Text is an ask's question, an issue's spec, or any other kind's body.
	Text string `json:"text" required:"true"`
	// Title is an issue's title.
	Title string `json:"title"`
	// Options and Multiple are an ask's options and its choice.
	Options  []heldOption `json:"options"`
	Multiple bool         `json:"multiple"`
	// Issue is the issue the draft would be on, "" for none: its record is read as a held text's.
	Issue string `json:"issue"`
	// Reader is the login of the person who would read it; empty, the issue's assignee or, else,
	// its project's default assignee.
	Reader string `json:"reader"`
}

// reviewerCall is one model call a review made: what it was for, the deployment its request
// named, the tries it made where it asked again after a malformed reply (absent for one), the
// model id the provider served, the tokens the provider reported over its tries (the prompt, the
// reply, and the prompt tokens written to and read from its cache), and the fault it ended on, ""
// for none.
type reviewerCall struct {
	Purpose          string `json:"purpose"`
	Deployment       string `json:"deployment"`
	Attempts         int    `json:"attempts,omitempty"`
	Served           string `json:"served"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	Fault            string `json:"fault,omitempty"`
}

func callsOf(calls []reviewer.Call) []reviewerCall {
	read := make([]reviewerCall, len(calls))
	for i, call := range calls {
		read[i] = reviewerCall{
			Purpose: call.Purpose, Deployment: call.Deployment, Served: call.Served,
			InputTokens: call.Usage.Input, OutputTokens: call.Usage.Output,
			CacheWriteTokens: call.Usage.CacheWrite, CacheReadTokens: call.Usage.CacheRead,
			Fault: string(call.Fault),
		}
		if call.Attempts > 1 {
			read[i].Attempts = call.Attempts
		}
	}
	return read
}

// droppedFinding is a finding the review dropped because its quote is not in the text: its rule
// set, and its quote as the model wrote it.
type droppedFinding struct {
	RuleSet string `json:"rule_set"`
	Quote   string `json:"quote"`
}

// reviewerDraftAnswer is what the draft route answers: the reader the review read for, the
// findings, each finding the review dropped because its quote is not in the text (absent for
// none), the rewrite (null for none) and why it would not post, and each model call.
type reviewerDraftAnswer struct {
	Reader     string             `json:"reader"`
	Findings   []reviewFinding    `json:"findings"`
	Dropped    []droppedFinding   `json:"dropped,omitempty"`
	Rewrite    *model.TextWording `json:"rewrite"`
	NotApplied string             `json:"not_applied,omitempty"`
	Calls      []reviewerCall     `json:"calls"`
}

// reviewDraft is POST /api/v1/reviewer-drafts. The review reads the reader's recorded words for a
// caller readsWordsOf lets read them, as GET /api/v1/readers/{login}/words serves them, so a
// person at a browser who names another person as the reader, or drafts on an issue another
// person reads, learns nothing of that person's words from the findings; an agent's token's
// review reads any reader's.
func (s *server) reviewDraft(w http.ResponseWriter, r *http.Request) {
	caller, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input reviewerDraftInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if s.deps.Reviewer == nil {
		writeError(w, "REVIEWER_NO_MODEL", http.StatusServiceUnavailable, "this deployment sets no reviewer's model group (DISPATCH_REVIEWER_MODEL_*)")
		return
	}
	chosen, err := s.requestModel(r, s.deps.Reviewer.config.Model)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	kind := reviewer.Kind(strings.TrimSpace(input.Kind))
	if !slices.Contains(reviewer.Kinds, kind) {
		writeError(w, "REVIEWER_DRAFT_INPUT", http.StatusBadRequest, "kind must be ask, issue, comment, message, document or decision_block")
		return
	}
	if strings.TrimSpace(input.Text) == "" {
		writeError(w, "REVIEWER_DRAFT_INPUT", http.StatusBadRequest, "text is required")
		return
	}
	text := reviewer.Text{Kind: kind, Body: input.Text}
	if kind == reviewer.KindIssue {
		text.Title = input.Title
	}
	if kind == reviewer.KindAsk || kind == reviewer.KindDecisionBlock {
		text.Options, text.Multiple = askOptionsOf(input.Options), input.Multiple
	}
	issueKey := strings.TrimSpace(input.Issue)
	reader := model.CanonicalLogin(input.Reader)
	if reader == "" && issueKey != "" {
		if reader, err = issueReader(r.Context(), s.deps.Store.Pool, issueKey); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	reviewed, err := s.review(r.Context(), chosen, reviewer.Subject{Text: text, IssueKey: issueKey, Reader: reader},
		readsWordsOf(caller, human, reader))
	var failed *reviewer.ModelError
	switch {
	case errors.As(err, &failed):
		writeError(w, "REVIEWER_MODEL_FAILED", http.StatusBadGateway, failed.Error())
		return
	case err != nil:
		s.writeHandlerError(w, err)
		return
	}
	answer := reviewerDraftAnswer{
		Reader: reader, Findings: findingsOf(reviewed.Findings), Calls: callsOf(reviewed.Calls),
	}
	for _, dropped := range reviewed.Dropped {
		answer.Dropped = append(answer.Dropped, droppedFinding{RuleSet: string(dropped.RuleSet), Quote: dropped.Quote})
	}
	if reviewed.Rewrite != nil {
		rewrite := wordingOf(*reviewed.Rewrite)
		answer.Rewrite = &rewrite
	}
	if reviewed.NotApplied != nil {
		answer.NotApplied = reviewed.NotApplied.Reason
	}
	WriteJSON(w, http.StatusOK, answer)
}
