package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/comments/{id}/state", Auth: authAny, Versions: onlyV2,
		Description: "Move a comment's thread to a state, its body by state. {state: resolved} resolves it, for any " +
			"caller, a bearer naming its session in actor; it is also how a suggestion that can no longer be accepted or " +
			"rejected is closed. {state: open} reopens a resolved thread root, which needs the person scope (a resolved " +
			"reply or ask thread's comment is 400 INVALID_COMMENT). {state: accepted} accepts a suggestion, which needs " +
			"the person scope: its replacement is applied to the live document and a version is named after the comment, " +
			"and the answer carries lost, whether the document already lacks the accepted text (null when the check " +
			"reached no verdict); an anchor the document no longer holds is recorded orphaned and refused. {state: " +
			"rejected} rejects a suggestion, which needs the person scope, and removes its mark. Accepting an " +
			"unanchored suggestion, and accepting or rejecting a comment with no suggestion, are 400 " +
			"INVALID_SUGGESTION. A " +
			"suggestion already accepted or rejected is a thread resolved by whoever decided it: accepting, rejecting or " +
			"resolving it is 409 ALREADY_ACTIONED, but for the body equal to its state from the person who decided it. A " +
			"body equal to the comment's present state, from the caller who set it, answers 200 and writes nothing (an " +
			"accept's answer with lost null), and {state: resolved} from the person who accepted or rejected a suggestion " +
			"is such a body; an open thread records no setter, so a body of open on an open thread writes nothing whoever " +
			"sends it. Answers the comment; 400 STATE_INVALID for any other state, 409 ISSUE_CLOSED on a closed issue, 404 " +
			"COMMENT_NOT_FOUND.",
		Handler:  s.putCommentStateV2,
		Resource: "state", Kind: kindState,
		Body: requestBody{Kind: bodyStateUnion, Arms: map[string]any{
			"open":     commentStateInput{},
			"resolved": resolveCommentStateInput{},
			"accepted": commentStateInput{},
			"rejected": commentStateInput{},
		}},
		Shape: routeShape{
			Status: http.StatusOK, Response: shapedComment{},
			Also: []routeAnswer{{Status: http.StatusOK, Body: acceptedComment{}}},
			// An accept whose replacement a concurrent change removed first (docs.ErrEditLost).
			Refusals: []routeAnswer{{Status: http.StatusConflict, Body: editLostError{}}},
		},
		Authorities: []authorityRule{commentOpenAuthority, commentAcceptedAuthority, commentRejectedAuthority},
		Scope:       scopeProject, ProjectOf: projectPathComment,
		IDs:    v2CommentIDs(),
		Reread: (*server).rereadComment,
		// Each state projects the margin record again, and an accept writes the suggestion's
		// replacement into the document (docs.AcceptSuggestion), a reject takes its mark out.
		Refusals: documentContentRefusals(
			undeclaredKeyRefusal, commentMissingRefusal, commentInvalidRefusal, issueClosedRefusal, postDeletedRefusal,
			editLostRefusal,
			apicode.Refusal{Condition: apicode.SuggestionActionInvalid, Code: apicode.InvalidSuggestion},
			apicode.Refusal{Condition: apicode.SuggestionDecided, Code: apicode.AlreadyActioned},
			apicode.Refusal{Condition: apicode.SuggestionMarkGone, Code: apicode.AnchorOrphaned},
			apicode.Refusal{Condition: apicode.AcceptRefused, Code: apicode.InvalidOp},
		),
	}
})
