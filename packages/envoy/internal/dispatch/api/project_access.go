package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A project's access is who may see it and who changes that (internal/dispatch/access): its one
// owner, its public or private setting, and the people and groups its access list names. Every
// route below needs the person scope, because each names or acts as a person; each write is the
// owner's alone, checked against projects.owner read in the write's own transaction, and appends
// project.access_changed, which the route publishes once the transaction commits. A project is
// made private only where the deployment names it among the projects it has provisioned as
// private (Deps.PrivateProjects: else 409 PRIVATE_PROJECTS_OFF for a deployment that names none,
// and 409 PRIVATE_PROJECT_NOT_PROVISIONED for one that names others) and every hub that serves
// private projects admits Dispatch, proven last before the switch commits (Deps.HubAdmission, else
// 409 PRIVATE_PROJECTS_UNAVAILABLE). No copy of a project's privacy exists outside the database:
// every listener reads the committed rows (GET /api/v1/access/private-projects), so no write of a
// project's access writes anything on the bus.

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

// HubAdmission is the private-hub admission proof a switch to private makes last before its
// commit: every hub that serves private projects admits a new connection of Dispatch's through its
// auth service, and every listener replica answers a fresh read of the private projects. It reads
// nothing of the project and writes nothing. cmd/dispatch passes the deployment's proof
// (busprojects.HubAdmission), or none, and a nil HubAdmission refuses every switch to private 409
// PRIVATE_PROJECTS_UNAVAILABLE (proveHubAdmission).
type HubAdmission interface {
	// Prove answers nil only when every hub and every listener replica the deployment names
	// admits the proof now, and otherwise an error naming the hub or replica that did not. The
	// switch calls it inside its transaction, as its last step before the commit, and any error
	// refuses the switch with nothing changed.
	Prove(ctx context.Context) error
}

// errPrivateProjectsUnavailable is a switch to private whose hub-admission proof failed: 409
// PRIVATE_PROJECTS_UNAVAILABLE, wrapped before the proof's own error.
var errPrivateProjectsUnavailable = errors.New("the project stays public: a switch to private first proves that every hub serving private projects admits Dispatch through its auth service and that every listener replica reads the private projects, and that proof failed")

// patchLockTimeout bounds each lock the PATCH access transaction waits for when it sets the
// visibility or the Slack channels: the project's owner lock and the Slack channel lock. A lock
// not taken within it answers 409 LOCK_TIMEOUT naming the lock (access.ErrLockTimeout), with
// nothing changed. A variable only so a test lowers it.
var patchLockTimeout = 60 * time.Second

// errPrivateProjectNotProvisioned is a switch to private of a project the deployment does not name
// among those it has provisioned as private: 409 PRIVATE_PROJECT_NOT_PROVISIONED, wrapped after the
// project's key.
var errPrivateProjectNotProvisioned = errors.New("is not one of the projects DISPATCH_PRIVATE_PROJECTS names; a deployment names a project there once it has provisioned that project's private namespace, stores and grants (the infrastructure repository's privateprojects.Projects())")

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
		return fmt.Errorf("%w: %w", errPrivateProjectsIncomplete, incompleteRowsError(unnamed, "direct messages"))
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
// has provisioned as private, the only ones a switch to private may name.
func (s *server) privateProjectProvisioned(key string) bool {
	_, found := slices.BinarySearch(s.deps.PrivateProjects, key)
	return found
}

// getProjectAccess answers who may see project key and who changes that, as the caller reads it:
// a project the caller does not see answers as a key no project holds.
func (s *server) getProjectAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, err := access.ReadProject(r.Context(), pool, r.PathValue("key"), refs.VisibilityArg(who))
	if err != nil {
		s.writeAccessError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// patchProjectAccess gives the project to another full member, at any time, sets its visibility,
// and names its Slack channels. Only its owner calls it; any of the three fields may be left out,
// and a body naming none is refused. A switch to private is refused before anything is read where the
// deployment names no private project, with the bytes it always answered; once the owner is
// known and before anything is written, for a project the deployment does not name (a private one
// whose key left the list included), and for one whose rows or sessions the store has not filled;
// and, once every write of the body has run, while another private project lists one of the
// project's Slack channels. A switch to private starts the channels' time (slack_channels_changed_at)
// when the project names any, and its last step before the commit is the hub-admission proof
// (proveHubAdmission), so a failed proof leaves the project as it was. A switch to public is refused
// by none of them, and no write here makes a bus call: every listener reads the committed rows.
//
// One lock order (privateArm): the project's owner lock (ownerWrite), then the Slack channel lock
// where a write takes it; a body that sets both the channels and private writes the channels first,
// so the private arm reads the new list, and no write takes the owner lock after the channel lock.
// A body that sets the visibility or the channels bounds each lock wait by patchLockTimeout.
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
	if visibility == model.ProjectPrivate && len(s.deps.PrivateProjects) == 0 {
		writeError(w, "PRIVATE_PROJECTS_OFF", http.StatusConflict,
			"this deployment cannot make a project private yet: nothing was changed")
		return
	}
	var lockTimeout time.Duration
	if input.Visibility != nil || input.SlackChannels != nil {
		lockTimeout = patchLockTimeout
	}
	var sw *privateSwitch
	if visibility == model.ProjectPrivate {
		sw = &privateSwitch{}
	}
	// A shared channel's refusal names the other private project only to a caller who sees it: the
	// viewer ownerWrite resolved from the request's own credential (readerOf), not the person
	// actingPerson answers, by. Read as that person, a token listing no project would see the
	// person's private projects, which the token does not.
	s.ownerWrite(w, r, "", lockTimeout, sw, func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, viewer *model.Viewer) (model.ProjectAccess, []model.Event, error) {
		var appended []model.Event
		var answer model.ProjectAccess
		// switching is a full switch to private: every switch to private but one of a project the
		// full switch already made private, which changes nothing. A project private some other
		// way, before any full switch, runs the whole path.
		switching := false
		if visibility == model.ProjectPrivate {
			if !s.privateProjectProvisioned(key) {
				return model.ProjectAccess{}, nil, fmt.Errorf("%s %w", key, errPrivateProjectNotProvisioned)
			}
			recorded, err := access.FullSwitchRecorded(ctx, tx, key)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			if switching = !recorded; switching {
				// Every repository name the project's own issues and default hold carries its
				// GitHub id before the project turns private, so the GitHub proxy can tell its
				// repositories from ones that take their names later
				// (refuseUnconfirmedRepositories): read under the owner lock ownerWrite holds,
				// which every repository write of the project takes too.
				if err := refuseUnconfirmedRepositories(ctx, tx, key); err != nil {
					return model.ProjectAccess{}, nil, err
				}
				if err := privateProjectIncomplete(ctx, tx, key); err != nil {
					return model.ProjectAccess{}, nil, err
				}
				// The broker records the channels the project will hold once the switch commits:
				// the body's when it names them, checked as their write checks them, else the
				// stored ones.
				var channels *[]string
				if input.SlackChannels != nil {
					named, err := access.NormalizeSlackChannels(*input.SlackChannels)
					if err != nil {
						return model.ProjectAccess{}, nil, err
					}
					channels = &named
				}
				// The closing state comes after every gate that writes nothing and before any write
				// of the body, so a switch it refuses keeps the closing state and nothing else.
				closing, err := s.closeForDeveloperBoxes(ctx, tx, key, by, channels, sw)
				if err != nil {
					return model.ProjectAccess{}, closing, err
				}
				appended = append(appended, closing...)
			}
		}
		if input.SlackChannels != nil {
			written, event, err := access.SetSlackChannels(ctx, tx, s.deps.Events, key, *input.SlackChannels, by, viewer)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			answer, appended = written, append(appended, event)
		}
		if input.Visibility != nil && (visibility != model.ProjectPrivate || switching) {
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
		if switching {
			written, err := s.privateArm(ctx, tx, key, viewer)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			answer = written
		}
		if len(appended) == 0 && !switching {
			written, err := access.ReadWrittenProject(ctx, tx, key)
			if err != nil {
				return model.ProjectAccess{}, nil, err
			}
			answer = written
		}
		return answer, appended, nil
	})
}

// privateArm is the end of a switch of project key to private, after every other write of the body
// has run in tx: under the Slack channel lock, taken after the project's owner lock, it refuses the
// switch while another private project lists one of the project's own channels, naming that project
// only to viewer when viewer sees it (access.RefuseSharedSlackChannels), and starts the channels'
// time when the project names any (access.StartSlackChannelsTime), and records the full switch
// (access.RecordFullSwitch). Its last step, with nothing after it but the commit, is the
// hub-admission proof, so a failed proof rolls back the record with the rest. It answers the
// project's access as the switch leaves it.
func (s *server) privateArm(ctx context.Context, tx pgx.Tx, key string, viewer *model.Viewer) (model.ProjectAccess, error) {
	if err := access.RefuseSharedSlackChannels(ctx, tx, key, viewer); err != nil {
		return model.ProjectAccess{}, err
	}
	if err := access.StartSlackChannelsTime(ctx, tx, key); err != nil {
		return model.ProjectAccess{}, err
	}
	if err := access.RecordFullSwitch(ctx, tx, key); err != nil {
		return model.ProjectAccess{}, err
	}
	answer, err := access.ReadWrittenProject(ctx, tx, key)
	if err != nil {
		return model.ProjectAccess{}, err
	}
	if err := proveHubAdmission(ctx, s.deps.HubAdmission); err != nil {
		return model.ProjectAccess{}, err
	}
	return answer, nil
}

// proveHubAdmission is a switch to private's hub-admission proof, its last step before the commit.
// It answers errPrivateProjectsUnavailable, wrapped before the cause, for a deployment with no
// proof (a nil admission) and for a proof that failed, naming the hub or replica it names; the
// proof itself takes no database lock and writes nothing.
func proveHubAdmission(ctx context.Context, admission HubAdmission) error {
	if admission == nil {
		return fmt.Errorf("%w: this deployment names no hub to prove (DISPATCH_PRIVATE_HUBS)", errPrivateProjectsUnavailable)
	}
	if err := admission.Prove(ctx); err != nil {
		return fmt.Errorf("%w: %w", errPrivateProjectsUnavailable, err)
	}
	return nil
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
	s.ownerWrite(w, r, group, 0, nil, func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, _ *model.Viewer) (model.ProjectAccess, []model.Event, error) {
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
// rather than raced and a delete of the group is ordered against the write. It publishes what the
// write appended once it commits, and answers the project's access as the write left it. The
// write is handed the caller's viewer, read from the request's own credential (readerOf), which a
// refusal it makes reads to name only what that credential sees (access.SetSlackChannels).
// lockTimeout, when not zero, is the transaction's first statement, `set local lock_timeout`: each
// lock it then takes gives up after that long, in its own statement, and answers 409 LOCK_TIMEOUT
// naming that lock (access.ErrLockTimeout) with nothing changed. `set local` ends with the
// transaction, so the pooled connection keeps no bound of it.
//
// The caller's viewer is read first: a project the caller does not see answers 404
// PROJECT_NOT_FOUND exactly as a key no project holds, before the owner is read. Only a caller who
// sees the project, and so may read its access list, is then told it is not the owner, by the 403
// PROJECT_OWNER_REQUIRED that names the owner; the other order would name a hidden project's owner
// to anyone who guessed its key.
//
// sw is a full switch to private's own state, nil for every other write. With it, the transaction
// runs on a context that ends at privateSwitchDeadline, made before it begins and used for every
// statement and the commit, and the database ends the transaction at the same deadline
// (transaction_timeout); a switch that meets it answers 503 PRIVATE_SWITCH_TIMED_OUT. A switch
// whose transaction ends with no commit sent, after the broker's count left a record, owes the
// broker the forget of that record, and one whose commit's outcome is unknown owes the forget of
// the project's record whatever its number (oweForgetAfter).
func (s *server) ownerWrite(w http.ResponseWriter, r *http.Request, group string, lockTimeout time.Duration, sw *privateSwitch,
	write func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, viewer *model.Viewer) (model.ProjectAccess, []model.Event, error)) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	if !viewerSees(who.Viewer, key) {
		s.writeAccessError(w, fmt.Errorf("%w: %s", access.ErrProjectNotFound, key))
		return
	}
	ctx := r.Context()
	if sw != nil && privateSwitchContextDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, privateSwitchDeadline)
		defer cancel()
	}
	// refuse answers a refusal of the write once its transaction has ended with no commit sent. The
	// forget is written after the rollback: the request holds the switch's connection until then,
	// and the pool refuses it a second one (store.ErrNestedAcquire).
	var tx pgx.Tx
	refuse := func(err error) {
		if tx != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
		if sw != nil && sw.record != nil {
			s.oweForgetAfter(r.Context(), key, sw.record)
		}
		if sw != nil && switchTimedOut(ctx, err) {
			writeError(w, "PRIVATE_SWITCH_TIMED_OUT", http.StatusServiceUnavailable, privateSwitchTimedOut("nothing was changed; try again"))
			return
		}
		s.writeAccessError(w, err)
	}
	begun, err := s.begin(ctx)
	if err != nil {
		refuse(err)
		return
	}
	tx = begun
	defer tx.Rollback(context.WithoutCancel(ctx))
	if lockTimeout > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`set local lock_timeout = '%dms'`, lockTimeout.Milliseconds())); err != nil {
			refuse(err)
			return
		}
	}
	if sw != nil {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`set local transaction_timeout = '%dms'`, privateSwitchDeadline.Milliseconds())); err != nil {
			refuse(err)
			return
		}
	}
	owner, err := access.LockForAccessWrite(ctx, tx, key, group)
	if err != nil {
		refuse(err)
		return
	}
	if owner == nil || *owner != actor.ID {
		writeError(w, "PROJECT_OWNER_REQUIRED", http.StatusForbidden,
			"only the project's owner changes who may see it: ask its owner, "+ownerName(owner))
		return
	}
	answer, appended, err := write(ctx, tx, key, actor, who.Viewer)
	var kept refusalKept
	if err != nil && !errors.As(err, &kept) {
		refuse(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		if sw != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			s.oweForgetAfter(r.Context(), key, nil)
			if switchTimedOut(ctx, err) {
				writeError(w, "PRIVATE_SWITCH_TIMED_OUT", http.StatusServiceUnavailable,
					privateSwitchTimedOut("its commit did not answer in time, so whether the project turned private is unknown: read its access, and try again"))
				return
			}
		}
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), appended...)
	if kept.err != nil {
		s.writeAccessError(w, kept.err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// switchTimedOut reports whether err ended a switch to private at its deadline: ctx, the switch's
// own context, past privateSwitchDeadline, or the database ending the transaction at the same
// deadline (SQLSTATE 25P04, transaction_timeout).
func switchTimedOut(ctx context.Context, err error) bool {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "25P04"
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
	// A switch that waits for its owner's confirmations names the repositories it waits for, beside
	// the code a switch the deployment does not provision answers.
	var unconfirmed *unconfirmedRepositoriesError
	if errors.As(err, &unconfirmed) {
		WriteJSON(w, http.StatusConflict, unconfirmedRepositoriesRefusal{
			Code: "PRIVATE_PROJECT_NOT_PROVISIONED", Error: unconfirmed.Error(), UnconfirmedRepositories: unconfirmed.names,
		})
		return
	}
	for _, refusal := range []struct {
		match  error
		code   string
		status int
	}{
		{access.ErrLockTimeout, "LOCK_TIMEOUT", http.StatusConflict},
		{access.ErrProjectNotFound, "PROJECT_NOT_FOUND", http.StatusNotFound},
		{access.ErrGroupNotFound, "GROUP_NOT_FOUND", http.StatusNotFound},
		{access.ErrOwnerNotAllowed, "OWNER_NOT_ALLOWED", http.StatusBadRequest},
		{access.ErrGroupMemberRestricted, "GROUP_MEMBER_RESTRICTED", http.StatusBadRequest},
		{access.ErrPrincipalInput, "PROJECT_ACCESS_INPUT", http.StatusBadRequest},
		{access.ErrVisibilityInput, "PROJECT_ACCESS_INPUT", http.StatusBadRequest},
		{errPrivateProjectsUnavailable, "PRIVATE_PROJECTS_UNAVAILABLE", http.StatusConflict},
		{errDeveloperBoxTokensLive, "DEVELOPER_BOX_TOKENS_LIVE", http.StatusConflict},
		{errDeveloperBoxTokensUnknown, "DEVELOPER_BOX_TOKENS_UNAVAILABLE", http.StatusServiceUnavailable},
		{errPrivateProjectsNeedBroker, "PRIVATE_PROJECTS_NEED_BROKER", http.StatusConflict},
		{errPrivateProjectNotProvisioned, "PRIVATE_PROJECT_NOT_PROVISIONED", http.StatusConflict},
		{errPrivateProjectsIncomplete, "PRIVATE_PROJECTS_INCOMPLETE", http.StatusConflict},
		{access.ErrListFull, "ACCESS_LIST_FULL", http.StatusConflict},
		{access.ErrGroupFull, "GROUP_FULL", http.StatusConflict},
		{access.ErrSlackChannelInput, "INVALID_SLACK_CHANNEL", http.StatusBadRequest},
		{access.ErrSlackChannelsFull, "CAP_EXCEEDED", http.StatusBadRequest},
		{access.ErrGroupsPerOwnerFull, "CAP_EXCEEDED", http.StatusBadRequest},
		{access.ErrSlackChannelShared, "SLACK_CHANNEL_SHARED", http.StatusConflict},
	} {
		if errors.Is(err, refusal.match) {
			writeError(w, refusal.code, refusal.status, err.Error())
			return
		}
	}
	s.writeHandlerError(w, err)
}
