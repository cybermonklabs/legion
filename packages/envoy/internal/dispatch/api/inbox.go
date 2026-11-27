package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// inboxAssigneeFilter turns ?assignee= into the SQL mode and email the inbox query binds:
// "" (no filter), "unassigned", or "login" with the canonical email of a person who has signed
// in. `me` is the caller; a named person is canonicalised first so a shared `?assignee=Alice@…`
// link never 400s.
// kept for one release: inbox-old-reads
func (s *server) inboxAssigneeFilter(ctx context.Context, raw string, caller model.Actor) (mode, login string, err error) {
	switch value := strings.TrimSpace(raw); value {
	case "":
		return "", "", nil
	case "unassigned":
		return "unassigned", "", nil
	case "me":
		return "login", model.CanonicalLogin(caller.ID), nil
	default:
		login, err := s.allowedLogin(ctx, value)
		if err != nil {
			return "", "", err
		}
		return "login", login, nil
	}
}

// kept for one release: inbox-old-reads
func (s *server) listInbox(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	assigneeMode, assigneeLogin, err := s.inboxAssigneeFilter(r.Context(), r.URL.Query().Get("assignee"), caller)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// waiting_on is askReadColumns' alias for the turn rule, 'human' or 'agent' on an open ask, so
	// sorting it descending puts the human's turn first. ORDER BY can name an output column only
	// bare, never inside an expression such as `waiting_on = 'agent'`, so the rule runs once a row.
	//
	// ?assignee= filters on the person the ask waits on (askedOfPersonExpression), not on the
	// issue's assignee: an ask handed to somebody is in that person's list and out of the
	// assignee's, and an ask on an issue nobody holds is in the list of the person its project
	// defaults to. askReadFrom joins all three tables that expression reads - the ask's issue as
	// i, its own document as ar and their project as p - so this read joins none of them itself
	// and reads the document's columns off the shared join. `unassigned` is left standing for
	// the caller that sends it: every question reaches a person now, so it answers nothing.
	//
	// An ask is open here while its row and its node both say so (askOpenOf): the row's state
	// lets the partial index asks_open (0001) pick the candidates out of every ask of the
	// instance, and the node, the state every row here shows (scanAskRead), decides (R3). An
	// owner's lists by state read one owner's asks through that owner's own index and pick by the
	// node alone (listIssueAsksQueryOpen); the Inbox reads every owner's, which without the row's
	// test would probe the node of every ask stored.
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+openAskRowColumns+`
		`+openAskRowFrom("$4")+`
		where `+askOpenOf("a")+`
		  and (i.key is null or i.closed_at is null)
		  and ($1 = '' or coalesce(i.project_key, ar.project_key) = $1)
		  and ($2 = ''
		       or ($2 = 'unassigned' and `+askedOfPersonExpression+` is null)
		       or ($2 = 'login' and `+askedOfPersonExpression+` = $3))
		order by i.priority asc nulls last, waiting_on desc,
		         coalesce(lr.created_at, a.created_at) desc, a.id desc
	`, project, assigneeMode, assigneeLogin, model.CanonicalLogin(caller.ID))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	asks := []openAskRow{}
	for rows.Next() {
		ask, err := scanOpenAskRow(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		asks = append(asks, ask)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	listed := make([]*openAskRow, len(asks))
	for index := range asks {
		listed[index] = &asks[index]
	}
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	if err := s.finishOpenAskRows(r.Context(), caller, listed, reader); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader.resolve()
	WriteJSON(w, http.StatusOK, asks)
}
