package api

import (
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// reviewer_after.go reviews, after its save, a text an agent changed that a person already sees
// or that is never held: a document version (DISPATCH_REVIEWER_DOCUMENTS=after; R5 says a version
// is saved at once and may be reviewed after the save), and an agent's edit of a posted ask,
// comment or issue title (Decision 8 of INFRA-3231's plan: the old wording is already visible,
// so an edit is not held, and when the kind's setting is after or hold it is reviewed after the
// save). The findings go to the writer session as the frame a held text's findings go in
// (sendFindingsFrame), with no time and no commands to post: the text is posted, so the
// findings are advice, and a writer that does not act leaves the text as it saved. The review is
// recorded reviewed_after, or posted_unreviewed where the model failed (runPostedReview).

// reviewAfterSave starts the review of a text the write r made and committed, once the write's
// answer no longer waits on it: text, of kind, under id, where subject places it. It reviews only
// an agent's write (writingCheckReads; the Legion daemon's session is left out) that a person
// reads (writingCheckReader, on input), that no person typed in a session (personTypedWrite),
// with the kind's setting other than off. Call it after the write's transaction committed.
func (s *server) reviewAfterSave(r *http.Request, actor model.Actor, input any, kind reviewer.Kind, id string, text reviewer.Text, subject reviewer.Subject) {
	review := s.afterSaveReview(r, actor, input, kind, text, subject)
	s.reviewPosted(r.Context(), review, id)
}

// afterSaveReview decides reviewAfterSave's review: nil where the write is not reviewed.
func (s *server) afterSaveReview(r *http.Request, actor model.Actor, input any, kind reviewer.Kind, text reviewer.Text, subject reviewer.Subject) *postedReview {
	if s.deps.Store == nil || s.deps.Reviewer.setting(kind) == ReviewOff || personTypedWrite(r) {
		return nil
	}
	if checked, _ := writingCheckReads(actor); !checked {
		return nil
	}
	ctx := r.Context()
	method, route := routeOf(r)
	reader, err := s.writingCheckReader(ctx, r, method+" "+route, actor, input)
	if err != nil {
		slog.Warn("dispatch: decide the reader of a text reviewed after its save", "route", method+" "+route, "error", err)
		return nil
	}
	if reader == "" {
		return nil
	}
	text.Kind = kind
	subject.Text, subject.Writer, subject.Reader = text, writerOf(actor), reader
	event := store.WritingCheckEvent{
		Route: route, Method: method, ActorKind: actor.Kind, ActorID: actor.ID,
		TokenID: s.writingCheckTokenID(r, actor), Reader: reader, Project: s.projectOfText(ctx, subject),
	}
	if actor.Act != nil {
		event.ActSession = actor.Act.Session
	}
	return &postedReview{kind: kind, text: text, subject: subject, event: event}
}

// reviewDocumentAfterSave starts the review of the document artifactID that the write r saved
// (DISPATCH_REVIEWER_DOCUMENTS=after), on its issue where issueKey names one: the whole document
// as saved, markdown, or as it stands once the write committed where the caller holds none (an
// edit, whose operations the document service applied). Call it after the write committed.
func (s *server) reviewDocumentAfterSave(r *http.Request, actor model.Actor, input any, artifactID string, issueKey *string, markdown string) {
	subject := reviewer.Subject{DocumentID: artifactID}
	if issueKey != nil {
		subject.IssueKey = *issueKey
	}
	review := s.afterSaveReview(r, actor, input, reviewer.KindDocument, reviewer.Text{}, subject)
	if review == nil {
		return
	}
	if markdown == "" {
		text, err := s.deps.Docs.Text(r.Context(), artifactID)
		if err != nil {
			slog.Warn("dispatch: read a document for its review after the save", "document", artifactID, "error", err)
			return
		}
		markdown = text
	}
	review.text.Body, review.subject.Text.Body = markdown, markdown
	s.reviewPosted(r.Context(), review, artifactID)
}

// askReviewSubject places an edited ask for its review after the save: on its issue, in its
// document at its block or at the quote its anchor names, in its own thread. A decision block is
// read as one (reviewer.KindDecisionBlock).
func askReviewSubject(ask model.Ask) (reviewer.Kind, reviewer.Text, reviewer.Subject) {
	kind := reviewer.KindAsk
	subject := reviewer.Subject{AskID: ask.ID}
	if ask.IssueKey != nil {
		subject.IssueKey = *ask.IssueKey
	}
	switch {
	case ask.BlockID != nil && ask.BlockArtifactID != nil:
		kind = reviewer.KindDecisionBlock
		subject.DocumentID, subject.BlockID = *ask.BlockArtifactID, *ask.BlockID
	case ask.Anchor != nil:
		subject.DocumentID, subject.Quote = ask.Anchor.ArtifactID, ask.Anchor.Quote
	case ask.ArtifactID != nil:
		subject.DocumentID = *ask.ArtifactID
	}
	return kind, reviewer.Text{Body: ask.Question, Options: ask.Options, Multiple: ask.Multiple}, subject
}

// commentReviewSubject places an edited comment for its review after the save: on its issue, in
// the thread it answers, at the quote its anchor names.
func commentReviewSubject(comment model.Comment) reviewer.Subject {
	subject := reviewer.Subject{}
	if comment.IssueKey != nil {
		subject.IssueKey = *comment.IssueKey
	}
	if comment.AskID != nil {
		subject.AskID = *comment.AskID
	} else if comment.ReplyTo != nil {
		subject.ReplyTo = *comment.ReplyTo
	}
	switch {
	case comment.Anchor != nil:
		subject.DocumentID, subject.Quote = comment.Anchor.ArtifactID, comment.Anchor.Quote
	case comment.ArtifactID != nil:
		subject.DocumentID = *comment.ArtifactID
	}
	return subject
}
