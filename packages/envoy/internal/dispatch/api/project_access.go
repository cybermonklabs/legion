package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A project's access is who may see it and who changes that (internal/dispatch/access): its one
// owner, its public or private setting, and the people and groups its access list names. Every
// route below needs the person scope, because each names or acts as a person; each write is the
// owner's alone, checked against projects.owner read in the write's own transaction, and appends
// project.access_changed, which the route publishes once the transaction commits. A project
// cannot be made private in this release (409 PRIVATE_PROJECTS_OFF).

// projectAccessInput is the body of PATCH /api/v1/projects/{key}/access.
type projectAccessInput struct {
	// Owner is the email of the full member the project is given to.
	Owner *string `json:"owner,omitempty"`
	// Visibility is public or private.
	Visibility *string `json:"visibility,omitempty" enum:"public,private"`
	// SlackChannels replaces the Slack channel ids the project's credentials may post to, at
	// most 16, each `C…` or `G…`; [] names none.
	SlackChannels *[]string `json:"slack_channels,omitempty"`
}

// getProjectAccess answers who may see project key and who changes that.
func (s *server) getProjectAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, err := access.ReadProject(r.Context(), pool, r.PathValue("key"))
	if err != nil {
		s.writeAccessError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// patchProjectAccess gives the project to another full member, at any time, sets its visibility,
// and names its Slack channels. Only its owner calls it; any of the three fields may be left out,
// and a body naming none is refused.
func (s *server) patchProjectAccess(w http.ResponseWriter, r *http.Request) {
	var input projectAccessInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Owner == nil && input.Visibility == nil && input.SlackChannels == nil {
		writeError(w, "PROJECT_ACCESS_INPUT", http.StatusBadRequest, "name the owner, the visibility, the Slack channels or several of them")
		return
	}
	if input.Visibility != nil && *input.Visibility == model.ProjectPrivate {
		writeError(w, "PRIVATE_PROJECTS_OFF", http.StatusConflict,
			"this deployment cannot make a project private yet: nothing was changed")
		return
	}
	// A shared channel's refusal names the other private project only to a caller who sees it: the
	// read rule over the request's own credential, not over by, the person actingPerson answers.
	// Read as that person, a token listing no project would see the person's private projects,
	// which the token does not. The caller is resolved here, before ownerWrite opens its
	// transaction: resolving a credential reads the store, and the pool refuses a second
	// connection inside that transaction (store.ErrNestedAcquire), as editAsk resolves its acting
	// person before it begins. SetSlackChannels reads what it sees only once it finds a share.
	var caller model.Actor
	if input.SlackChannels != nil {
		var err error
		if caller, _, err = s.optionalActor(r); err != nil {
			s.writeAuthenticationError(w, err)
			return
		}
	}
	s.ownerWrite(w, r, "", func(ctx context.Context, tx pgx.Tx, key string, by model.Actor) (model.ProjectAccess, []model.Event, error) {
		var appended []model.Event
		var answer model.ProjectAccess
		if input.Visibility != nil {
			written, event, err := access.SetVisibility(ctx, tx, s.deps.Events, key, *input.Visibility, by)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			answer, appended = written, append(appended, event)
		}
		if input.Owner != nil {
			written, event, err := access.SetOwner(ctx, tx, s.deps.Events, key, *input.Owner, by)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			answer, appended = written, append(appended, event)
		}
		if input.SlackChannels != nil {
			written, event, err := access.SetSlackChannels(ctx, tx, s.deps.Events, key, *input.SlackChannels, by, caller)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			answer, appended = written, append(appended, event)
		}
		return answer, appended, nil
	})
}

// principalWrite is one of access.AddPrincipal and access.RemovePrincipal.
type principalWrite func(ctx context.Context, tx pgx.Tx, ev *events.Broker, key, kind, principal string, by model.Actor) (model.ProjectAccess, model.Event, error)

// putProjectAccessPerson names a person on the project's access list by email.
func (s *server) putProjectAccessPerson(w http.ResponseWriter, r *http.Request) {
	s.principalWrite(w, r, access.PrincipalPerson, r.PathValue("email"), access.AddPrincipal)
}

// deleteProjectAccessPerson takes a person off the project's access list.
func (s *server) deleteProjectAccessPerson(w http.ResponseWriter, r *http.Request) {
	s.principalWrite(w, r, access.PrincipalPerson, r.PathValue("email"), access.RemovePrincipal)
}

// putProjectAccessGroup names a group on the project's access list.
func (s *server) putProjectAccessGroup(w http.ResponseWriter, r *http.Request) {
	s.principalWrite(w, r, access.PrincipalGroup, r.PathValue("name"), access.AddPrincipal)
}

// deleteProjectAccessGroup takes a group off the project's access list.
func (s *server) deleteProjectAccessGroup(w http.ResponseWriter, r *http.Request) {
	s.principalWrite(w, r, access.PrincipalGroup, r.PathValue("name"), access.RemovePrincipal)
}

func (s *server) principalWrite(w http.ResponseWriter, r *http.Request, kind, principal string, write principalWrite) {
	// A group's name is locked before the project, as a delete of that group locks them
	// (access.LockForAccessWrite), so the two never wait on each other in a cycle.
	group := ""
	if kind == access.PrincipalGroup {
		group = strings.TrimSpace(principal)
	}
	s.ownerWrite(w, r, group, func(ctx context.Context, tx pgx.Tx, key string, by model.Actor) (model.ProjectAccess, []model.Event, error) {
		answer, event, err := write(ctx, tx, s.deps.Events, key, kind, principal, by)
		if err != nil {
			return model.ProjectAccess{}, nil, err
		}
		return answer, []model.Event{event}, nil
	})
}

// ownerWrite runs one write of project {key}'s access for its owner: the caller acts as a person,
// and is the project's owner as projects.owner reads in the write's own transaction, under the
// locks access.LockForAccessWrite takes in the one order every access write takes them (group,
// the group the write names, or "" for none), so an owner change committed meanwhile is read
// rather than raced and a delete of the group is ordered against the write. Anyone else is 403
// PROJECT_OWNER_REQUIRED before anything is written. It publishes what the write appended once it
// commits, and answers the project's access as the write left it.
func (s *server) ownerWrite(w http.ResponseWriter, r *http.Request, group string,
	write func(ctx context.Context, tx pgx.Tx, key string, by model.Actor) (model.ProjectAccess, []model.Event, error)) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	owner, err := access.LockForAccessWrite(r.Context(), tx, key, group)
	if err != nil {
		s.writeAccessError(w, err)
		return
	}
	if owner == nil || *owner != actor.ID {
		writeError(w, "PROJECT_OWNER_REQUIRED", http.StatusForbidden,
			"only the project's owner changes who may see it: ask its owner, "+ownerName(owner))
		return
	}
	answer, appended, err := write(r.Context(), tx, key, actor)
	if err != nil {
		s.writeAccessError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), appended...)
	WriteJSON(w, http.StatusOK, answer)
}

func ownerName(owner *string) string {
	if owner == nil {
		return "whom the operator names"
	}
	return *owner
}

// writeAccessError answers one refusal of the access package by its code, and any other error as
// the handlers answer every error.
func (s *server) writeAccessError(w http.ResponseWriter, err error) {
	for _, refusal := range []struct {
		match  error
		code   string
		status int
	}{
		{access.ErrProjectNotFound, "PROJECT_NOT_FOUND", http.StatusNotFound},
		{access.ErrGroupNotFound, "GROUP_NOT_FOUND", http.StatusNotFound},
		{access.ErrOwnerNotAllowed, "OWNER_NOT_ALLOWED", http.StatusBadRequest},
		{access.ErrGroupMemberRestricted, "GROUP_MEMBER_RESTRICTED", http.StatusBadRequest},
		{access.ErrPrincipalInput, "PROJECT_ACCESS_INPUT", http.StatusBadRequest},
		{access.ErrVisibilityInput, "PROJECT_ACCESS_INPUT", http.StatusBadRequest},
		{access.ErrListFull, "ACCESS_LIST_FULL", http.StatusConflict},
		{access.ErrGroupFull, "GROUP_FULL", http.StatusConflict},
		{access.ErrSlackChannelInput, "INVALID_SLACK_CHANNEL", http.StatusBadRequest},
		{access.ErrSlackChannelsFull, "CAP_EXCEEDED", http.StatusBadRequest},
		{access.ErrSlackChannelShared, "SLACK_CHANNEL_SHARED", http.StatusConflict},
	} {
		if errors.Is(err, refusal.match) {
			writeError(w, refusal.code, refusal.status, err.Error())
			return
		}
	}
	s.writeHandlerError(w, err)
}
