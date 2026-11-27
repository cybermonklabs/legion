package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/asks", Auth: authAny, Versions: onlyV2,
		Description: "The asks of every project, filtered; with no filter, every open ask of every project the caller reads, " +
			"whoever it waits on. state=open (the default; an open ask on an open issue or a project document), answered, " +
			"resolved or all. project=<key>, repeatable, keeps those projects' asks. waiting_on=me (the person scope) or " +
			"<email> keeps the asks waiting on that person, resolved as each ask is read in three steps: the person the " +
			"question was handed to, then the issue's assignee, then the project's default assignee (an email nobody has " +
			"signed in with is 400 ASSIGNEE_NOT_ALLOWED), and waiting_on=none the asks no step names anybody for. " +
			"author_session=<session> keeps one session's own asks. " +
			"from and to (RFC3339) window created_at, from inclusive and to exclusive. source=<kind>:<id> (kind artifact, " +
			"ask, comment or message; id a whole uuid or a unique prefix of 8 characters or more) reads the question map of " +
			"the document that source's text belongs to: one row for each `:::ask{#id}` block the document's latest version " +
			"declares with the ask settlement made for it, question_block_id naming the block a #id in the source's text " +
			"names, of every state where no state is given; the document's id is beside the page as document, a source on " +
			"no document is 404 NOT_FOUND and a prefix two sources carry 409 ID_AMBIGUOUS (any other source is 400 " +
			"INVALID_QUERY). " + askRowText + " sort=priority (the default: the issue's priority, unset last, then whose " +
			"turn it is, the person's before the agent's, then the latest activity, newest first) or created_at (oldest " +
			"first), each reversed after a -. " + includedPageLimitText(v2AskList, askListV2Query{}) + " {items, total, limit, offset, count, " +
			"waiting_on_human, waiting_on_agent, as_of}: count the asks the filters keep, as total does, the two waiting_on " +
			"counts the asks among them whose state is open by whose turn it is (under state=open, the open asks on open " +
			"issues and project documents; under another state, every ask of state open it keeps, one on a closed issue " +
			"among them), and as_of the database time the read was made at.",
		Handler:  s.listAsksV2,
		Resource: "asks", Kind: kindCollection, Key: "{id}",
		TimeFields:   []string{"created_at"},
		CrossProject: true,
		Query:        askListV2Query{},
		List:         v2AskList,
		Shape:        routeShape{Status: http.StatusOK, Response: askListV2{}},
		Authorities:  []authorityRule{askWaitingOnMeAuthority, askMarkAuthority},
		Refusals: listRefusals(
			actingSessionMismatchRefusal, assigneeNotSignedInRefusal, refPrefixAmbiguousRefusal,
			apicode.Refusal{Condition: apicode.QuestionSourceWithoutDocument, Code: apicode.NotFound},
		),
	}
})
