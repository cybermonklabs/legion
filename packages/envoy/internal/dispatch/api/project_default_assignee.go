package api

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A project's default assignee is the last step of the chain that decides who a question
// reaches (askedOfPersonExpression and defaultAssignee, issue_assignee.go): it names a person by
// lowercase email, and every project names one — the column refuses nothing
// (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql) — so both routes
// below need the `person` scope, the gate GET /api/v1/users uses. That is deliberate and not an
// oversight to be tidied away: no credential the deployment configured — the shared token, a
// pod's — gains the power to move a project's default, however the architecture-source
// routes beside it are shaped. Such a credential acts for a machine and for no person; choosing
// which person a project's unowned work lands on is a person's call, which their own credential
// makes, and the only agent-visible effect is the assignee a create comes back with.
//
// Nothing here clears a default: a project always names somebody, so the write below moves the
// default from one person to another and refuses a body that names none.

type projectDefaultAssignee struct {
	Project  string `json:"project"`
	Assignee string `json:"assignee"`
}

// projectDefaultAssigneeUpdated is the payload of settings.project_default_assignee.updated:
// the stored row, plus the people whose lists the move changed. A project's default is the last
// step of the chain that decides who a question reaches (askedOfPersonExpression), so moving it
// takes every question on an issue nobody holds out of one person's Inbox and puts it into the
// other's, exactly as handing one question on moves a single row (api/ask_asked_of.go, which
// names its pair through movedLogins). ThreadLogins is the previous default and the new one,
// sorted and named once, and never null: a write that moved nothing between two people - a PUT
// naming the person the project already defaults to - names that one person once, since
// movedLogins carries each login no more than once.
type projectDefaultAssigneeUpdated struct {
	projectDefaultAssignee
	ThreadLogins []string `json:"thread_logins"`
}

// listProjectDefaultAssignees answers every project with the person it defaults to, by project
// key: the Settings page's one read. Every project is listed, because every project names a
// default, and no project at all is an empty list, never null.
func (s *server) listProjectDefaultAssignees(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select key, default_assignee
		from projects
		order by key
	`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defaults, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (projectDefaultAssignee, error) {
		var entry projectDefaultAssignee
		err := row.Scan(&entry.Project, &entry.Assignee)
		return entry, err
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, defaults)
}

// projectDefaultAssigneeInput is the body of PUT /api/v1/projects/{key}/default-assignee.
type projectDefaultAssigneeInput struct {
	// Assignee is required and names a person: a body whose assignee is absent, null or blank
	// is refused (400 INVALID_PROJECT, putProjectDefaultAssignee below), because a project
	// always names the person its unowned work lands on.
	Assignee string `json:"assignee" required:"true"`
}

// putProjectDefaultAssignee moves a project's default assignee to another person. It needs the
// `person` scope, as the file's head says. Nothing clears a default: a body whose assignee is
// absent, null or blank is refused (400 INVALID_PROJECT), so neither a PUT of `{}` nor one of
// `{"assignee": null}` can leave a project naming nobody. A login that is not a person who has
// signed in is refused here (400 ASSIGNEE_NOT_ALLOWED) so a typo never becomes a stored
// default; a default left stale later by a restore or a direct write is still what a create
// and a question read, since the chain's last step reads the column as it stands.
//
// The event it appends names both people the move carried rows between
// (projectDefaultAssigneeUpdated), so each of their open Inboxes refreshes.
func (s *server) putProjectDefaultAssignee(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	var input projectDefaultAssigneeInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// encoding/json leaves the string empty for an absent key and for a null alike, and both
	// are the same refusal: a project names a person or the write does not happen.
	if strings.TrimSpace(input.Assignee) == "" {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest,
			"assignee is required: a project always names the person its unowned work lands on, "+
				"so give a person's email; the default cannot be cleared")
		return
	}
	assignee, err := s.allowedLogin(r.Context(), input.Assignee)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The default this write moves from, read under the project row's own lock in the
	// transaction that replaces it: two of these at once then name the pair each really moved,
	// and the update below takes no lock this read has not already taken. The level is `for no
	// key update`, which serialises this write against the other writers of the row and still
	// lets a foreign key take its key share, as the room lock's rule requires of every lock on
	// projects (TestNoForUpdateOnOwnerTables, docs/persistence.go's lockDocumentRoom). An
	// unknown project is 404 NOT_FOUND here (pgx.ErrNoRows, writeHandlerError), before anything
	// is written.
	var previous string
	if err := tx.QueryRow(r.Context(), `
		select default_assignee from projects where key = $1 for no key update
	`, key).Scan(&previous); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `update projects set default_assignee = $2 where key = $1`, key, assignee); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	stored := projectDefaultAssignee{Project: key, Assignee: assignee}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(key).event(
		"settings.project_default_assignee.updated", actor, projectDefaultAssigneeUpdated{
			projectDefaultAssignee: stored,
			ThreadLogins:           movedLogins(nil, &previous, &assignee),
		},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, stored)
}
