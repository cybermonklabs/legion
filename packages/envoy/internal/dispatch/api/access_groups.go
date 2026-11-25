package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A group is a named list of full members that a project's access list can name
// (internal/dispatch/access). It has one owner, the full member who made it or was given it, and
// only that owner writes or deletes it. Every route below needs the person scope; a write appends
// access_group.changed, and a delete also takes the group off every access list that named it.

// accessGroupInput is the body of PUT /api/v1/access-groups/{name}.
type accessGroupInput struct {
	// Members are the full members' emails; a person who is a restricted member is refused.
	Members []string `json:"members" required:"true"`
	// Owner gives an existing group to another full member; a new group's owner is its maker.
	Owner *string `json:"owner,omitempty"`
}

// maxAccessGroupPage is the most groups one page of GET /api/v1/access-groups holds, and the page a
// request that names no limit reads.
const maxAccessGroupPage = 100

// listAccessGroups answers one page of the groups, by name: at most ?limit= (1-100, default 100)
// after the name ?after=, with next naming where the next page starts. A limit that is not one
// integer in that range, or an after that is no group name, is 400 INVALID_QUERY naming it.
func (s *server) listAccessGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	query := r.URL.Query()
	limit := maxAccessGroupPage
	if value, present, valid := parseQueryInt(query, "limit", 1, maxAccessGroupPage); present {
		if !valid {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf("limit must be one integer from 1 to %d", maxAccessGroupPage))
			return
		}
		limit = value
	}
	after := query.Get("after")
	if query.Has("after") && !access.ValidGroupName(after) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf("after must be a group name, not %q", after))
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	page, err := access.ListGroups(r.Context(), pool, after, limit)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, page)
}

// putAccessGroup makes group {name}, or writes it again for its owner. A restricted member makes
// none: this route is a write of the deployment (scopeDeployment, routes_table.go), which
// withinProjectScope refuses to every viewer that is not open, a restricted member with 403
// RESTRICTED_MEMBER, before this handler runs.
func (s *server) putAccessGroup(w http.ResponseWriter, r *http.Request) {
	var input accessGroupInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Members == nil {
		writeError(w, "PROJECT_ACCESS_INPUT", http.StatusBadRequest, "members is required, [] for a group of nobody")
		return
	}
	name := r.PathValue("name")
	s.groupWrite(w, r, name, func(ctx context.Context, tx pgx.Tx, by model.Actor, owner *string) (any, []model.Event, int, bool) {
		newOwner := by.ID
		switch {
		case owner == nil:
			if input.Owner != nil && model.CanonicalLogin(*input.Owner) != by.ID {
				writeError(w, "PROJECT_ACCESS_INPUT", http.StatusBadRequest,
					"a new group's owner is the person who makes it; give it to another owner once it exists")
				return nil, nil, 0, false
			}
		case *owner != by.ID:
			writeError(w, "GROUP_OWNER_REQUIRED", http.StatusForbidden, "only the group's owner, "+*owner+", writes it")
			return nil, nil, 0, false
		case input.Owner != nil:
			newOwner = *input.Owner
		}
		group, event, err := access.PutGroup(ctx, tx, s.deps.Events, name, newOwner, input.Members, by)
		if err != nil {
			s.writeAccessError(w, err)
			return nil, nil, 0, false
		}
		return group, []model.Event{event}, http.StatusOK, true
	})
}

// deleteAccessGroup deletes group {name} for its owner and takes it off every access list.
func (s *server) deleteAccessGroup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.groupWrite(w, r, name, func(ctx context.Context, tx pgx.Tx, by model.Actor, owner *string) (any, []model.Event, int, bool) {
		if owner == nil {
			s.writeAccessError(w, access.ErrGroupNotFound)
			return nil, nil, 0, false
		}
		if *owner != by.ID {
			writeError(w, "GROUP_OWNER_REQUIRED", http.StatusForbidden, "only the group's owner, "+*owner+", deletes it")
			return nil, nil, 0, false
		}
		appended, err := access.DeleteGroup(ctx, tx, s.deps.Events, name, by)
		if err != nil {
			s.writeAccessError(w, err)
			return nil, nil, 0, false
		}
		return nil, appended, http.StatusNoContent, true
	})
}

// groupWrite runs one write of group name for a caller acting as a person. It first takes a
// transaction lock on the name, so two makers of one new group are ordered and the second finds
// the first's group and its owner rather than overwriting it, then reads the group's owner (nil
// for a group no row holds) under the row's lock. write answers what to send, the events it
// appended and the status, or writes its own refusal and answers false. It publishes the events
// once the transaction commits.
func (s *server) groupWrite(w http.ResponseWriter, r *http.Request, name string,
	write func(ctx context.Context, tx pgx.Tx, by model.Actor, owner *string) (any, []model.Event, int, bool)) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	name = strings.TrimSpace(name)
	if !access.ValidGroupName(name) {
		s.writeAccessError(w, fmt.Errorf("%w: %q is no group name: a group is named by lowercase letters, digits, dots, underscores and dashes, starting with a letter or digit, at most 63", access.ErrPrincipalInput, name))
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err := access.LockGroupName(r.Context(), tx, name); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var owner *string
	var held string
	switch err := tx.QueryRow(r.Context(), `select owner from access_groups where name = $1 for no key update`, name).Scan(&held); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		s.writeHandlerError(w, err)
		return
	default:
		owner = &held
	}
	body, appended, status, ok := write(r.Context(), tx, actor, owner)
	if !ok {
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), appended...)
	if status == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	WriteJSON(w, status, body)
}
