package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// getProjectAccess answers who may see project key and who changes that, as the caller reads it:
// a project the caller does not see answers as a key no project holds. The owner's browser session
// gets the owner's passkey fields too where passkey sign-in is on (withOwnersPasskeys). Every read
// of the answer is one snapshot, a read-only repeatable-read transaction: an enrollment uses its
// link and stores its passkey in one commit, so the answer lists the link open or the passkey,
// never neither, and the Access tab never drops a link with no passkey to show for it.
func (s *server) getProjectAccess(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
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
	tx, err := pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The transaction reads only, so its end is a rollback whatever happened.
	defer tx.Rollback(r.Context())
	answer, err := access.ReadProject(r.Context(), tx, r.PathValue("key"), refs.VisibilityArg(who))
	if err != nil {
		s.writeAccessError(w, err)
		return
	}
	if answer, err = s.withOwnersPasskeys(r.Context(), tx, answer, caller, who.Viewer); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, s.accessAnswer(answer))
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
			if err := s.refuseTransferOfAnotherOwnersPasskey(ctx, tx, key, *input.Owner); err != nil {
				return model.ProjectAccess{}, nil, err
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

// principalWrite is one of access.AddPrincipal and access.RemovePrincipal.
type principalWrite func(ctx context.Context, tx pgx.Tx, ev *events.Broker, key, kind, principal string, by model.Actor) (model.ProjectAccess, model.Event, error)

// putProjectAccessPerson names a person on the project's access list by email. Where passkey sign-in
// is on, an address that holds a passkey enrolled through a link another owner made is 409
// PASSKEY_OF_ANOTHER_OWNER (store.HeldByAnotherOwnersPasskey, read under the address's lock, which
// an enrollment takes to check the other way): that passkey would read this project too, so the
// owner whose link enrolled it resets the person first, and one owner binds an outside person.
func (s *server) putProjectAccessPerson(w http.ResponseWriter, r *http.Request) {
	email := r.PathValue("email")
	s.ownerWrite(w, r, "", "", func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, _ *model.Viewer) (model.ProjectAccess, []model.Event, error) {
		if err := s.refusePasskeyOfAnotherOwner(ctx, tx, email, by.ID); err != nil {
			return model.ProjectAccess{}, nil, err
		}
		answer, event, err := access.AddPrincipal(ctx, tx, s.deps.Events, key, access.PrincipalPerson, email, by)
		if err != nil {
			return model.ProjectAccess{}, nil, err
		}
		return answer, []model.Event{event}, nil
	})
}

// refusePasskeyOfAnotherOwner is errPasskeyOfAnotherOwner when email's address holds a passkey
// enrolled through a link someone other than owner made, read in tx under the address's lock; an
// email that names nobody is left to the list's own refusal. It runs whatever
// DISPATCH_SIGNIN_PASSKEY says: a passkey enrolled while the setting was on reads every project that
// names its person once it is on again, so a list written while it is off must not name them either.
func (s *server) refusePasskeyOfAnotherOwner(ctx context.Context, tx pgx.Tx, email, owner string) error {
	login, err := access.PersonPrincipal(email)
	if err != nil {
		return nil
	}
	if err := store.LockAddressTx(ctx, tx, login); err != nil {
		return err
	}
	held, err := store.HeldByAnotherOwnersPasskey(ctx, tx, login, owner)
	if err != nil {
		return err
	}
	if held {
		return errPasskeyOfAnotherOwner
	}
	return nil
}

// refuseTransferOfAnotherOwnersPasskey is errPasskeyOfAnotherOwner when a person project key's list
// names directly holds a passkey that a link of someone other than owner, the project's next owner,
// enrolled (store.ProjectNamesAnotherOwnersPasskey, read under each such person's address lock):
// handing the project over would leave the link's maker, its owner until now, an account of their
// own binding that reads it. The maker resets the person first, as a list's naming of them waits for
// (refusePasskeyOfAnotherOwner). It runs whatever DISPATCH_SIGNIN_PASSKEY says, as that refusal does.
func (s *server) refuseTransferOfAnotherOwnersPasskey(ctx context.Context, tx pgx.Tx, key, owner string) error {
	held, err := store.ProjectNamesAnotherOwnersPasskey(ctx, tx, key, owner)
	if err != nil {
		return err
	}
	if held {
		return errPasskeyOfAnotherOwner
	}
	return nil
}

// deleteProjectAccessPerson takes a person off the project's access list (removePerson). It names
// the person to access.LockForAccessWrite as the one it removes, which takes their machine-login
// approval lock first, before the project's owner lock, where a read that takes no lock finds the
// removal owes the revocation of their machine logins: the record of it waits for an approve of
// theirs the broker has not answered, and so that wait holds up no other write of the project's
// access, and a removal that owes nothing, from a list that does not name them, holds up no approve
// of theirs. Wherever Dispatch keeps passkeys,
// whatever DISPATCH_SIGNIN_PASSKEY says, the removal takes the person's links of this project and
// their address before it removes them (lockPasskeyRemoval), and once no list names them directly
// any more it ends a restricted member's account (endUnlistedPasskeySignIn).
func (s *server) deleteProjectAccessPerson(w http.ResponseWriter, r *http.Request) {
	email := r.PathValue("email")
	s.ownerWrite(w, r, "", model.CanonicalLogin(email), func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, _ *model.Viewer) (model.ProjectAccess, []model.Event, error) {
		if err := s.lockPasskeyRemoval(ctx, tx, key, email); err != nil {
			return model.ProjectAccess{}, nil, err
		}
		answer, event, err := s.removePerson(ctx, tx, s.deps.Events, key, access.PrincipalPerson, email, by)
		if err != nil {
			return model.ProjectAccess{}, nil, err
		}
		if err := s.endUnlistedPasskeySignIn(ctx, tx, email); err != nil {
			return model.ProjectAccess{}, nil, err
		}
		return answer, []model.Event{event}, nil
	})
}

// lockPasskeyRemoval takes, in a removal's transaction tx and before it writes anything, what the
// removal of email from project key's list may end a passkey sign-in under, wherever Dispatch keeps
// passkeys (Deps.PasskeyAccounts), whatever DISPATCH_SIGNIN_PASSKEY says (R5): it deletes the
// person's unused links of key, then takes the address's lock (store.LockAddressTx). That is the
// order an enrollment takes them in: the link, the address, then the person's sessions' owner lock
// (store.RecordReachedProjects, through the account binding). The removal itself takes that last
// lock (access.RemovePrincipal), so it comes after these: taken the other way round, a removal
// holding the sessions' lock and an enrollment holding the address each wait for the other, and
// PostgreSQL fails one of them (40P01).
func (s *server) lockPasskeyRemoval(ctx context.Context, tx pgx.Tx, key, email string) error {
	if s.deps.PasskeyAccounts == nil {
		return nil
	}
	login := model.CanonicalLogin(email)
	if err := store.DeleteInvitationsTx(ctx, tx, key, login); err != nil {
		return err
	}
	return store.LockAddressTx(ctx, tx, login)
}

// removePerson is access.RemovePrincipal for a person, which also owes the broker the revocation of
// a restricted member's machine logins when the removal takes them off the list: it reads in the
// removal's transaction, before it takes them off, whether the removal owes it (access.RemovalOwes:
// the list names the person and they are a restricted member), and only such a removal records
// the debt (recordMachineLoginRevocation), paid after the commit. Every such removal owes it,
// whatever other project's list still names them, as R16's approved words read: a machine login is
// no project's (each trade of it names its own project), so its revoke is whole, and a person
// another list still names approves a new login, at their browser, for the projects that list
// them. Its read under the project's lock decides: where the same read before the locks did not
// find the debt, the person's addition to the list or their becoming a restricted member having
// committed in between, the record takes the person's approval lock then, and while another
// transaction holds it (an approval of theirs in flight, another owner's removal of them, or the
// runbook's restore block) the removal is refused 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT and changes
// nothing, for the owner to send again.
func (s *server) removePerson(ctx context.Context, tx pgx.Tx, ev *events.Broker, key, kind, email string, by model.Actor) (model.ProjectAccess, model.Event, error) {
	owes, err := access.RemovalOwes(ctx, tx, key, email)
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, err
	}
	answer, event, err := access.RemovePrincipal(ctx, tx, ev, key, kind, email, by)
	if err != nil || !owes {
		return answer, event, err
	}
	if err := s.recordMachineLoginRevocation(ctx, tx, email); err != nil {
		if errors.Is(err, store.ErrMachineLoginApprovalInFlight) {
			err = fmt.Errorf("the removal found this person on the list only once it held the project's lock, and an approval or another removal of this person is in progress; remove them again in a few seconds (%w)", err)
		}
		return model.ProjectAccess{}, model.Event{}, err
	}
	return answer, event, nil
}

// recordMachineLoginRevocation records, in tx, that Dispatch owes the secrets broker the
// revocation of every machine login person approved for their own machines
// (store.RecordMachineLoginRevocation). Its caller decided, in tx, that the write owes it: it took
// a restricted member off a list that named them (access.RemovalOwes). While it is owed, the
// person's approve is 409 (store.HoldMachineLoginApprovals), so only a login they approve once a
// pass has settled it survives. A deployment with no broker (Deps.AgentSecrets nil, where no
// machine login exists) owes nothing. The broker revocation loop pays it after the write commits
// (revokeOwedMachineLogins), so the write never waits on the broker: an owner removes a person
// while the broker is down. Every removal that takes the person off a list records it
// (removePerson); it is written to be the one record any other write that takes a restricted
// member off a project makes in its own transaction, after it names that person to
// access.LockForAccessWrite as the one it removes. The record takes the person's approval lock
// itself without waiting, so a write that did not take it first records under it or is refused
// store.ErrMachineLoginApprovalInFlight, never records unordered against an approval in flight.
func (s *server) recordMachineLoginRevocation(ctx context.Context, tx pgx.Tx, person string) error {
	person = model.CanonicalLogin(person)
	if s.deps.AgentSecrets == nil || person == "" {
		return nil
	}
	return store.RecordMachineLoginRevocation(ctx, tx, person)
}

// endUnlistedPasskeySignIn is a removal's end of a person's passkey sign-in, in its transaction tx,
// wherever Dispatch keeps passkeys, whatever DISPATCH_SIGNIN_PASSKEY says (R5: a removal from the
// last list ends the person for good, an off window included), under the address's lock the removal
// took first (lockPasskeyRemoval): when no project's owner is them and no list names them directly
// any more (access.Listed, read after the removal and under that lock in tx), it ends their account
// as the owner's Reset does (store.PgPasskeys.EndAccountTx): a new passkey subject, which revokes
// their personal tokens and owes the broker the old account's revocation, and their sign-in ended
// and passkeys deleted. So a later owner who names the address names a new account, and nothing the
// old one made acts for it. A full member's address has nothing to end
// (auth.ErrAddressHeldByMember). The lock puts two removals from the last two lists that name the
// person in order, so the second reads the first's commit and ends the account, with no broker
// configured too.
func (s *server) endUnlistedPasskeySignIn(ctx context.Context, tx pgx.Tx, email string) error {
	if s.deps.PasskeyAccounts == nil {
		return nil
	}
	login := model.CanonicalLogin(email)
	listed, err := access.Listed(ctx, tx, login)
	if err != nil {
		return err
	}
	if listed {
		return nil
	}
	if _, err := s.deps.PasskeyAccounts.EndAccountTx(ctx, tx, login); err != nil && !errors.Is(err, auth.ErrAddressHeldByMember) {
		return err
	}
	return nil
}

// invitationAnswer is POST /api/v1/projects/{key}/access/people/{email}/invitations's answer: the
// link the owner hands the person, the outside origin's enrollment page with the token in its
// fragment, when it was made and when it lapses, on the database's clock. made_at is the key the
// owner's Access tab holds the link by: a read of the access list that began after the link landed
// and lists no open link of the person made at made_at (open_invitations) says it was used or
// ended.
type invitationAnswer struct {
	URL       string    `json:"url"`
	MadeAt    time.Time `json:"made_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// The refusals of the owner's link and Reset (createInvitation, resetProjectAccessPerson).
var (
	errPasskeySignInOff     = errors.New("this deployment signs nobody in with a passkey (DISPATCH_SIGNIN_PASSKEY is unset)")
	errBrowserSessionNeeded = errors.New("only the owner's browser session makes a link or resets a person: no token does")
	errPersonNotListed      = errors.New("the project's access list does not name this email directly: name the person on the list first")
	errWorkspaceAddress     = errors.New("an address on a domain of the company's Workspace signs in with Google, never with a link")
	errFullMember           = errors.New("a full member signs in with Google, never with a link")
	// errPasskeyOfAnotherOwner refuses a list's naming of an address whose passkey another owner's
	// link enrolled (putProjectAccessPerson).
	errPasskeyOfAnotherOwner = errors.New("this address holds a passkey enrolled through another owner's link, which would read this project too: that owner resets the person first")
)

// passkeyOwnerRefusal refuses an owner's passkey route (a link, a Reset) for its caller by, once
// the owner's check passed: 409 PASSKEY_SIGNIN_OFF where passkey sign-in is off, and 403
// BROWSER_SESSION_REQUIRED for a caller that is not a browser's session cookie (a personal token, a
// broker token or the shared token never makes a link); nil otherwise.
func (s *server) passkeyOwnerRefusal(by model.Actor) error {
	if s.deps.Passkeys == nil {
		return errPasskeySignInOff
	}
	if !atBrowserAs(by) {
		return errBrowserSessionNeeded
	}
	return nil
}

// atBrowserAs reports whether actor, a person actingPerson resolved, is that person at a browser
// (their sign-in proved them) rather than a credential acting for them: a personal token and a
// broker token each name the agent that acted (model.Actor.Act), and a browser names none.
func atBrowserAs(actor model.Actor) bool {
	return actor.Act == nil
}

// namedPerson answers email's stored form when project key's list names it directly, read in tx
// (access.NamesPerson): 400 PROJECT_ACCESS_INPUT for a value that names nobody who could sign in,
// 409 PERSON_NOT_LISTED for an email the list does not name as a person (a group that holds it
// never counts), and 409 FULL_MEMBER for a full member's address.
func namedPerson(ctx context.Context, tx pgx.Tx, key, email string) (string, error) {
	login, err := access.PersonPrincipal(email)
	if err != nil {
		return "", err
	}
	named, err := access.NamesPerson(ctx, tx, key, login)
	if err != nil {
		return "", err
	}
	if !named {
		return "", errPersonNotListed
	}
	kind, err := access.MemberKind(ctx, tx, login)
	if err != nil {
		return "", err
	}
	if kind == model.MemberFull {
		return "", errFullMember
	}
	return login, nil
}

// createInvitation is POST /api/v1/projects/{key}/access/people/{email}/invitations: the project's
// owner, in a browser session (passkeyOwnerCall), makes a single-use link for a person its list
// names directly, at an address on no domain of the company's Workspace and of no full member
// (namedPerson), whom no project of another owner names directly (store.ListedByAnotherOwner,
// 409 LISTED_BY_ANOTHER_OWNER: a passkey reads every project that names its person, so one owner
// binds it), which ends the person's earlier unused links of the project
// (store.MakeInvitationTx). It answers 201 {url, made_at, expires_at}: the url is the outside origin's
// enrollment page with 32 random bytes, base64url, in its fragment, which no server logs and no
// browser sends as a Referer, and outside_invitations keeps only their hash. The answer is the
// one time the link is shown, so an Idempotency-Key never keeps it (answerHoldsSecret): a repeat
// under the key is 409 KEY_COMMITTED, never a second link.
func (s *server) createInvitation(w http.ResponseWriter, r *http.Request) {
	var answer invitationAnswer
	if !s.inOwnerWrite(w, r, "", "", func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, _ *model.Viewer) ([]model.Event, error) {
		if err := s.passkeyOwnerRefusal(by); err != nil {
			return nil, err
		}
		login, err := namedPerson(ctx, tx, key, r.PathValue("email"))
		if err != nil {
			return nil, err
		}
		if s.deps.Passkeys.WorkspaceAddress(login) {
			return nil, errWorkspaceAddress
		}
		if other, err := store.ListedByAnotherOwner(ctx, tx, login, by.ID); err != nil {
			return nil, err
		} else if other {
			return nil, store.ErrListedByAnotherOwner
		}
		token, hash, err := identity.NewInvitationToken()
		if err != nil {
			return nil, err
		}
		madeAt, expiresAt, err := store.MakeInvitationTx(ctx, tx, hash, login, key, by.ID)
		if err != nil {
			return nil, err
		}
		answer = invitationAnswer{URL: s.deps.Passkeys.Store().Issuer() + "/enroll#" + token, MadeAt: madeAt, ExpiresAt: expiresAt}
		return nil, nil
	}) {
		return
	}
	WriteJSON(w, http.StatusCreated, answer)
}

// resetProjectAccessPerson is POST /api/v1/projects/{key}/access/people/{email}/reset: the
// project's owner, in a browser session, resets a person its list names directly, in one
// transaction: it records that Dispatch owes the secrets broker the revocation of the machine
// logins they approved (recordMachineLoginRevocation, the record a removal from the list makes),
// deletes their unused links of every project (store.DeletePersonInvitationsTx), gives the
// address a new passkey account, which revokes their personal tokens and owes the broker the
// revocation of the old account, and ends their sign-in and deletes their passkeys
// (store.PgPasskeys.EndAccountTx). So the person signs in again only with a new link, and nothing
// the old account made or approved acts again, a machine login approved while the Reset ran
// included (R5, R16). The Reset names the person to access.LockForAccessWrite, which takes their
// machine-login approval lock before the project's where the Reset owes the revocation
// (access.RemovalOwes: the list names them and they are a restricted member), as a removal's does.
// Then it writes the revocation's row, then deletes the links and takes the address's lock, and
// last ends the account, whose sub change writes the person's session row; a removal writes the
// revocation's row later, after access.RemovePrincipal's sessions lock. Both hold the approval
// lock, or take it without waiting, before either row, so neither order makes a cycle. The person
// stays on the list. A full member's address is 409 FULL_MEMBER, read again under the address's
// lock (store.PgPasskeys.EndAccountTx), and an address on a domain of the company's Workspace is
// 400 WORKSPACE_ADDRESS, as a link for it is. It answers the project's access, as the list's
// writes do.
func (s *server) resetProjectAccessPerson(w http.ResponseWriter, r *http.Request) {
	s.ownerWrite(w, r, "", model.CanonicalLogin(r.PathValue("email")), func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, _ *model.Viewer) (model.ProjectAccess, []model.Event, error) {
		if err := s.passkeyOwnerRefusal(by); err != nil {
			return model.ProjectAccess{}, nil, err
		}
		login, err := namedPerson(ctx, tx, key, r.PathValue("email"))
		if err != nil {
			return model.ProjectAccess{}, nil, err
		}
		if s.deps.Passkeys.WorkspaceAddress(login) {
			return model.ProjectAccess{}, nil, errWorkspaceAddress
		}
		owes, err := access.RemovalOwes(ctx, tx, key, login)
		if err != nil {
			return model.ProjectAccess{}, nil, err
		}
		if owes {
			if err := s.recordMachineLoginRevocation(ctx, tx, login); err != nil {
				return model.ProjectAccess{}, nil, err
			}
		}
		if err := store.DeletePersonInvitationsTx(ctx, tx, login); err != nil {
			return model.ProjectAccess{}, nil, err
		}
		if _, err := s.deps.Passkeys.Store().EndAccountTx(ctx, tx, login); errors.Is(err, auth.ErrAddressHeldByMember) {
			return model.ProjectAccess{}, nil, errFullMember
		} else if err != nil {
			return model.ProjectAccess{}, nil, err
		}
		answer, err := access.ReadProjectWritten(ctx, tx, key)
		return answer, nil, err
	})
}

// withOwnersPasskeys adds to answer, read in q, the owner's passkey fields (model.ProjectAccess's
// Passkeys, Invitable, ListedByAnotherOwner and OpenInvitations) when passkey sign-in is on and the
// caller is the project's owner in a browser session, the one caller who may make a link; every
// other answer carries none. Each passkey names who made the link that enrolled it and, where
// viewer sees it, that link's project: a project of another owner the reader does not see is left
// out, as every read leaves it out. A person a project of another owner names directly is listed
// as such and is not invitable, as the link route refuses them (409 LISTED_BY_ANOTHER_OWNER).
func (s *server) withOwnersPasskeys(ctx context.Context, q store.PasskeyReader, answer model.ProjectAccess, caller model.Actor, viewer *model.Viewer) (model.ProjectAccess, error) {
	if s.deps.Passkeys == nil || answer.Owner != caller.ID || !atBrowserAs(caller) {
		return answer, nil
	}
	passkeys, err := store.EnrolledPasskeys(ctx, q, answer.People)
	if err != nil {
		return model.ProjectAccess{}, err
	}
	// The test seam: a case commits an enrollment here, between the passkeys and the open links
	// (server betweenOwnersPasskeyReads). Nothing but a test sets it.
	if s.betweenOwnersPasskeyReads != nil {
		s.betweenOwnersPasskeyReads()
	}
	for index := range passkeys {
		if project := passkeys[index].Project; project != nil && !viewerSees(viewer, *project) {
			passkeys[index].Project = nil
		}
	}
	full, err := store.FullMembers(ctx, q, answer.People)
	if err != nil {
		return model.ProjectAccess{}, err
	}
	listedElsewhere, err := store.ListedByOtherOwners(ctx, q, answer.People, answer.Owner)
	if err != nil {
		return model.ProjectAccess{}, err
	}
	// Only a person from outside can be refused a link for another owner's listing: a full member
	// or a Workspace address is refused one for what it is (FULL_MEMBER, WORKSPACE_ADDRESS), before
	// the link route reads who else names it, so each person appears in one list at most.
	invitable, others := []string{}, []string{}
	for _, person := range answer.People {
		switch {
		case slices.Contains(full, person) || s.deps.Passkeys.WorkspaceAddress(person):
		case slices.Contains(listedElsewhere, person):
			others = append(others, person)
		default:
			invitable = append(invitable, person)
		}
	}
	open, err := store.OpenInvitations(ctx, q, answer.Project)
	if err != nil {
		return model.ProjectAccess{}, err
	}
	answer.Passkeys, answer.Invitable, answer.ListedByAnotherOwner = &passkeys, &invitable, &others
	answer.OpenInvitations = &open
	return answer, nil
}

// putProjectAccessGroup names a group on the project's access list.
func (s *server) putProjectAccessGroup(w http.ResponseWriter, r *http.Request) {
	s.principalWrite(w, r, access.PrincipalGroup, r.PathValue("name"), "", access.AddPrincipal)
}

// deleteProjectAccessGroup takes a group off the project's access list.
func (s *server) deleteProjectAccessGroup(w http.ResponseWriter, r *http.Request) {
	s.principalWrite(w, r, access.PrincipalGroup, r.PathValue("name"), "", access.RemovePrincipal)
}

// principalWrite runs write, one of access.AddPrincipal and access.RemovePrincipal, or removePerson,
// for its owner (ownerWrite); removed is the person a removal takes off the list, "" for any other
// write.
func (s *server) principalWrite(w http.ResponseWriter, r *http.Request, kind, principal, removed string, write principalWrite) {
	// A group's name is locked before the project, as a delete of that group locks them
	// (access.LockForAccessWrite), so the two never wait on each other in a cycle.
	group := ""
	if kind == access.PrincipalGroup {
		group = strings.TrimSpace(principal)
	}
	s.ownerWrite(w, r, group, removed, func(ctx context.Context, tx pgx.Tx, key string, by model.Actor, _ *model.Viewer) (model.ProjectAccess, []model.Event, error) {
		answer, event, err := write(ctx, tx, s.deps.Events, key, kind, principal, by)
		if err != nil {
			return model.ProjectAccess{}, nil, err
		}
		return answer, []model.Event{event}, nil
	})
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
		answer, err = s.withOwnersPasskeys(ctx, tx, written, by, viewer)
		return appended, err
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
		{errPasskeySignInOff, "PASSKEY_SIGNIN_OFF", http.StatusConflict},
		{errBrowserSessionNeeded, "BROWSER_SESSION_REQUIRED", http.StatusForbidden},
		{errPersonNotListed, "PERSON_NOT_LISTED", http.StatusConflict},
		{errWorkspaceAddress, "WORKSPACE_ADDRESS", http.StatusBadRequest},
		{errFullMember, "FULL_MEMBER", http.StatusConflict},
		{store.ErrListedByAnotherOwner, "LISTED_BY_ANOTHER_OWNER", http.StatusConflict},
		{errPasskeyOfAnotherOwner, "PASSKEY_OF_ANOTHER_OWNER", http.StatusConflict},
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
