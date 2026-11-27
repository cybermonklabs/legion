package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A project's access is who may see it and who changes that (internal/dispatch/access): its one
// owner, its public or private setting, and the people and groups its access list names. Every
// route below needs the person scope, because each names or acts as a person; each write is the
// owner's alone, checked against projects.owner read in the write's own transaction, and appends
// project.access_changed, which the route publishes once the transaction commits. A project is
// made private only where the deployment names it: among the projects it has provisioned as
// private (Deps.PrivateProjects), whose switch is the full switch, and needs the hub to take its
// private-projects record (K7, Deps.BusProjects, else 409 PRIVATE_PROJECTS_UNAVAILABLE), or among
// those it makes private before their full switch (Deps.PrivateBeforeFullSwitch), whose switch
// needs no record. A deployment that names none answers 409 PRIVATE_PROJECTS_OFF, and one that
// names others 409 PRIVATE_PROJECT_NOT_PROVISIONED.

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

// BusProjects writes the private-projects record, the contract K7 names: one key per private
// project in the hub's envoy_private_projects bucket, which every listener reads to hold that
// project's messages back from a token that does not list it. No writer of it is in this tree, so
// cmd/dispatch passes none, and every write that needs the record answers 409
// PRIVATE_PROJECTS_UNAVAILABLE (ensurePrivate).
type BusProjects interface {
	// EnsurePrivate puts project's key, reading what it needs through q, the visibility write's
	// own transaction, since the pool refuses that write's request a second connection
	// (store.ErrNestedAcquire). The write calls it last before its commit, and any error refuses
	// the write.
	EnsurePrivate(ctx context.Context, q access.Querier, project string) error
}

// privateProjectsBucket is the hub's key-value bucket the private-projects record is kept in, the
// one a refusal to make a project private names.
const privateProjectsBucket = "envoy_private_projects"

// errPrivateProjectsUnavailable is a write that would make a project private while its
// private-projects record cannot be written: 409 PRIVATE_PROJECTS_UNAVAILABLE.
var errPrivateProjectsUnavailable = errors.New("the project stays public: every listener holds a private project's messages back by its key in the hub's " +
	privateProjectsBucket + " bucket, and that key could not be written")

// errPrivateProjectNotProvisioned is a switch to private of a project the deployment names in
// neither list: 409 PRIVATE_PROJECT_NOT_PROVISIONED, wrapped after the project's key.
var errPrivateProjectNotProvisioned = errors.New("is not one of the projects DISPATCH_PRIVATE_PROJECTS names; a deployment names a project there once it has provisioned that project's private namespace, stores and grants (the infrastructure repository's privateprojects.Projects()), " +
	"or in DISPATCH_PRIVATE_BEFORE_FULL_SWITCH to make it private before its full switch")

// errPrivateProjectsIncomplete is a switch to private while the store cannot hold the project
// private yet: 409 PRIVATE_PROJECTS_INCOMPLETE, naming why and what repairs it.
var errPrivateProjectsIncomplete = errors.New("the project stays public until the store can hold it private")

// privateProjectIncomplete answers, read in the switch's own transaction q, why project key cannot
// be made private yet, as errPrivateProjectsIncomplete: a row of key's whose node does not name it
// (graphcensus.ProjectCompleteFor), which its reads would answer wrongly, a direct message whose
// conversation names no party (conversationsNamingNoParty), or a backfill the switch waits for that
// has not run (backfillMarkersMissing, the store report's own marker check). It answers nil when
// none holds, and a read that fails as that read's error, never as errPrivateProjectsIncomplete,
// since a database that did not answer says nothing of whether the store can hold the project.
func privateProjectIncomplete(ctx context.Context, q graphstore.Queryer, key string) error {
	incomplete, err := graphcensus.ProjectCompleteFor(ctx, q, key)
	if err != nil {
		return err
	}
	if len(incomplete) > 0 {
		return fmt.Errorf("%w: %w", errPrivateProjectsIncomplete, incompleteRowsError(incomplete, "rows of "+key))
	}
	unnamed, err := conversationsNamingNoParty(ctx, q)
	if err != nil {
		return err
	}
	if len(unnamed) > 0 {
		// The messages are the whole deployment's, and while a project is private none passes any
		// viewer but the server's, so the refusal says how many there are and names none to the
		// owner who pressed the switch; the ids, each with its cure, are logged for the operator,
		// and envoy-dispatch census --store names them too.
		slog.Warn("dispatch: a switch to private waits for direct messages whose conversation names no party",
			"project", key, "messages", unnamed)
		return fmt.Errorf("%w: %d direct messages name no party their conversation is with, so while a project is private no reader may see them; "+
			"the operator's log and envoy-dispatch census --store name each and its cure",
			errPrivateProjectsIncomplete, len(unnamed))
	}
	if err := backfillMarkersMissing(ctx, q); err != nil {
		if errors.Is(err, errBackfillNotRun) {
			return fmt.Errorf("%w: %w", errPrivateProjectsIncomplete, err)
		}
		return err
	}
	return nil
}

// conversationsNamingNoParty is every direct message, one on no issue, whose conversation names no
// party as the read rule reads it (access.MessagePartiesSQL): Dispatch holds no node of it, or its
// node names none. Such a message belongs to no project, and while a project is private it passes
// no viewer but the server's (access.PartyClause), not even its own parties, so every project's
// switch reads it. Each is a line as the project census writes one, in id order: one with no node
// is missing and the backfill writes it, and one whose node names no party is named with the
// census that names its cure, since which cure it takes depends on what its node holds.
func conversationsNamingNoParty(ctx context.Context, q graphstore.Queryer) ([]string, error) {
	rows, err := q.Query(ctx, `
		select m.id::text, exists (
			select 1 from graph_nodes node where node.kind = '`+graphstore.KindMessage+`' and node.id = m.id::text)
		from messages m
		where m.issue_key is null and coalesce(cardinality(`+access.MessagePartiesSQL("m.id::text")+`), 0) = 0
		order by m.id`)
	if err != nil {
		return nil, fmt.Errorf("read the direct messages whose conversation names no party: %w", err)
	}
	var lines []string
	var id string
	var noded bool
	if _, err := pgx.ForEachRow(rows, []any{&id, &noded}, func() error {
		line := graphstore.KindMessage + " " + id + ": missing; run envoy-dispatch backfill-graph-store"
		if noded {
			line = graphstore.KindMessage + " " + id + ": its node names no party; envoy-dispatch census --store names its cure"
		}
		lines = append(lines, line)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the direct messages whose conversation names no party: %w", err)
	}
	return lines, nil
}

// privateProjectProvisioned reports whether the deployment names project key among the projects it
// has provisioned as private, whose switch to private is the full switch.
func (s *server) privateProjectProvisioned(key string) bool {
	_, found := slices.BinarySearch(s.deps.PrivateProjects, key)
	return found
}

// privateBeforeFullSwitch reports whether project key is one the deployment makes private before
// its full switch: Deps.PrivateBeforeFullSwitch names it, which NewDeps holds apart from
// Deps.PrivateProjects.
func (s *server) privateBeforeFullSwitch(key string) bool {
	_, found := slices.BinarySearch(s.deps.PrivateBeforeFullSwitch, key)
	return found
}

// accessAnswer is answer as every route of a project's access answers it: marked
// private_before_full_switch while the project is private before its full switch, so its people
// read it as such and not as a fully switched project.
func (s *server) accessAnswer(answer model.ProjectAccess) model.ProjectAccess {
	answer.PrivateBeforeFullSwitch = answer.Visibility == model.ProjectPrivate && s.privateBeforeFullSwitch(answer.Project)
	return answer
}

// patchProjectAccess gives the project to another full member, at any time, sets its visibility,
// and names its Slack channels. Only its owner calls it; any of the three fields may be left out,
// and a body naming none is refused. Where passkey sign-in is on, a transfer is 409
// PASSKEY_OF_ANOTHER_OWNER while a person the list names directly holds a passkey another owner's
// link enrolled (refuseTransferOfAnotherOwnersPasskey).
// A switch to private is refused before anything is read where the
// deployment names no private project in either list, with the bytes it always answered; once the
// owner is known and before anything is written, for a project the deployment names in neither
// list (a private one whose key left them included), and for one whose rows or sessions the store
// has not filled. A project's full switch then writes its private-projects record through the
// transaction as its last step, so a refused record leaves the project as it was; the switch of a
// project private before its full switch writes none and asks no writer, since its bus gate comes
// with its full switch, and answers its access marked private_before_full_switch. A switch to
// public is refused by none of them. The route makes no other bus call: deleting the key of a
// project made public, or of one whose commit failed after its record was written, is the record
// writer's own pass (K7), not this route's.
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
	visibility := ""
	if input.Visibility != nil {
		visibility = *input.Visibility
	}
	if visibility == model.ProjectPrivate && len(s.deps.PrivateProjects) == 0 && len(s.deps.PrivateBeforeFullSwitch) == 0 {
		writeError(w, "PRIVATE_PROJECTS_OFF", http.StatusConflict,
			"this deployment cannot make a project private yet: nothing was changed")
		return
	}
	// A shared channel's refusal names the other private project only to a caller who sees it: the
	// viewer ownerWrite resolved from the request's own credential (readerOf), not the person
	// actingPerson answers, by. Read as that person, a token listing no project would see the
	// person's private projects, which the token does not.
	s.ownerWrite(w, r, "", "", func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, viewer *model.Viewer) (model.ProjectAccess, []model.Event, error) {
		var appended []model.Event
		var answer model.ProjectAccess
		fullSwitch := visibility == model.ProjectPrivate && s.privateProjectProvisioned(key)
		if visibility == model.ProjectPrivate {
			if !fullSwitch && !s.privateBeforeFullSwitch(key) {
				return model.ProjectAccess{}, nil, fmt.Errorf("%s %w", key, errPrivateProjectNotProvisioned)
			}
			if err := privateProjectIncomplete(ctx, tx, key); err != nil {
				return model.ProjectAccess{}, nil, err
			}
		}
		if input.Visibility != nil {
			written, event, err := access.SetVisibility(ctx, tx, s.deps.Events, key, visibility, by)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			answer, appended = written, append(appended, event)
		}
		if input.Owner != nil {
			// A person the writer may not see is refused as one who is not a full member, before
			// anything is read of them (personHiddenFromReader).
			hidden, err := personHiddenFromReader(ctx, tx, *input.Owner)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			if hidden {
				return model.ProjectAccess{}, nil, access.OwnerNotAllowed(*input.Owner)
			}
			written, event, err := access.SetOwner(ctx, tx, s.deps.Events, key, *input.Owner, by)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			answer, appended = written, append(appended, event)
		}
		if input.SlackChannels != nil {
			written, event, err := access.SetSlackChannels(ctx, tx, s.deps.Events, key, *input.SlackChannels, by, viewer)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			answer, appended = written, append(appended, event)
		}
		if fullSwitch {
			if err := ensurePrivate(ctx, tx, s.deps.BusProjects, key); err != nil {
				return model.ProjectAccess{}, nil, err
			}
		}
		return answer, appended, nil
	})
}

// ensurePrivate writes project key's private-projects record through bus and the write's
// transaction tx, the write's last step before its commit. It answers
// errPrivateProjectsUnavailable where it cannot: a deployment with no writer (a nil bus, which is
// every deployment of this build), or a writer that refused the write, as a hub without its auth
// service refuses every one.
func ensurePrivate(ctx context.Context, tx pgx.Tx, bus BusProjects, key string) error {
	if bus == nil {
		return fmt.Errorf("%w: this deployment has no bus to write it to", errPrivateProjectsUnavailable)
	}
	if err := bus.EnsurePrivate(ctx, tx, key); err != nil {
		return fmt.Errorf("%w: %w", errPrivateProjectsUnavailable, err)
	}
	return nil
}

// ownerWrite runs one write of project {key}'s access for its owner (inOwnerWrite) and answers the
// project's access as the write left it, with the owner's passkey fields where they apply
// (withOwnersPasskeys), read in the write's transaction.
func (s *server) ownerWrite(w http.ResponseWriter, r *http.Request, group, removed string,
	write func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, viewer *model.Viewer) (model.ProjectAccess, []model.Event, error)) {
	var answer model.ProjectAccess
	if !s.inOwnerWrite(w, r, group, removed, func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, viewer *model.Viewer) ([]model.Event, error) {
		written, appended, err := write(ctx, tx, key, by, viewer)
		if err != nil {
			return nil, err
		}
		answer = written
		return appended, nil
	}) {
		return
	}
	WriteJSON(w, http.StatusOK, s.accessAnswer(answer))
}

// inOwnerWrite runs one write of project {key}'s access for its owner: the caller acts as a person,
// and is the project's owner as projects.owner reads in the write's own transaction, under the
// locks access.LockForAccessWrite takes in the one order every access write takes them (removed,
// the person a removal or a Reset takes off the list or resets, and group, the group the write
// names, each "" for none), so an owner change committed meanwhile is read rather than raced and a
// delete of the group is ordered against the write. It publishes what the write appended once it
// commits, and reports whether it did; a refusal is answered here. The write is handed the
// caller's viewer, read from the request's own credential (readerOf), which a refusal it makes
// reads to name only what that credential sees (access.SetSlackChannels).
//
// The caller's viewer is read first: a project the caller does not see answers 404
// PROJECT_NOT_FOUND exactly as a key no project holds, before the owner is read. Only a caller who
// sees the project, and so may read its access list, is then told it is not the owner, by the 403
// PROJECT_OWNER_REQUIRED that names the owner; the other order would name a hidden project's owner
// to anyone who guessed its key. The owner is read first with no lock (access.ReadOwner), and a
// caller who does not own the project is refused before the write takes any: a removal's first
// lock may be the approval lock of the person it removes, which that person's approve does not
// wait for, so a removal anyone who sees the project may send and the owner check refuses would
// otherwise hold that person's approves at 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT. The read under
// the locks is the one that decides.
func (s *server) inOwnerWrite(w http.ResponseWriter, r *http.Request, group, removed string,
	write func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, viewer *model.Viewer) ([]model.Event, error)) bool {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return false
	}
	key := r.PathValue("key")
	who, ok := s.readerOf(w, r)
	if !ok {
		return false
	}
	if !viewerSees(who.Viewer, key) {
		s.writeAccessError(w, fmt.Errorf("%w: %s", access.ErrProjectNotFound, key))
		return false
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	defer tx.Rollback(r.Context())
	owns := func(owner *string, err error) bool {
		if err != nil {
			s.writeAccessError(w, err)
			return false
		}
		if owner == nil || *owner != actor.ID {
			writeError(w, "PROJECT_OWNER_REQUIRED", http.StatusForbidden,
				"only the project's owner changes who may see it: ask its owner, "+ownerName(owner))
			return false
		}
		return true
	}
	if !owns(access.ReadOwner(r.Context(), tx, key)) || !owns(access.LockForAccessWrite(r.Context(), tx, key, group, removed)) {
		return false
	}
	appended, err := write(r.Context(), tx, key, actor, who.Viewer)
	if err != nil {
		s.writeAccessError(w, err)
		return false
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	s.publish(r.Context(), appended...)
	return true
}

// viewerSees reports whether viewer sees project; no viewer is the server's own read.
func viewerSees(viewer *model.Viewer, project string) bool {
	return viewer == nil || viewer.All || slices.Contains(viewer.Sees, project)
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
		{errPrivateProjectsUnavailable, "PRIVATE_PROJECTS_UNAVAILABLE", http.StatusConflict},
		{errPrivateProjectNotProvisioned, "PRIVATE_PROJECT_NOT_PROVISIONED", http.StatusConflict},
		{errPrivateProjectsIncomplete, "PRIVATE_PROJECTS_INCOMPLETE", http.StatusConflict},
		{access.ErrListFull, "ACCESS_LIST_FULL", http.StatusConflict},
		{access.ErrGroupFull, "GROUP_FULL", http.StatusConflict},
		{access.ErrSlackChannelInput, "INVALID_SLACK_CHANNEL", http.StatusBadRequest},
		{access.ErrSlackChannelsFull, "CAP_EXCEEDED", http.StatusBadRequest},
		{access.ErrGroupsPerOwnerFull, "CAP_EXCEEDED", http.StatusBadRequest},
		{access.ErrSlackChannelShared, "SLACK_CHANNEL_SHARED", http.StatusConflict},
		{store.ErrMachineLoginApprovalInFlight, "MACHINE_LOGIN_APPROVAL_IN_FLIGHT", http.StatusConflict},
	} {
		if errors.Is(err, refusal.match) {
			writeError(w, refusal.code, refusal.status, err.Error())
			return
		}
	}
	s.writeHandlerError(w, err)
}
