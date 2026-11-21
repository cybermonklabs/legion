package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const agentTokenPrefixLength = 8

type createAgentTokenResponse struct {
	model.AgentToken
	Token string `json:"token"`
}

func (s *server) listAgentTokens(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}

	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select id::text, name, prefix, scopes, projects, created_at, last_used_at, revoked_at
		from agent_tokens
		where owner = $1
		order by created_at desc
	`, actor.ID)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list agent tokens: %w", err))
		return
	}
	defer rows.Close()

	tokens := []model.AgentToken{}
	for rows.Next() {
		token, err := scanAgentToken(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, fmt.Errorf("iterate agent tokens: %w", err))
		return
	}
	// Each list names only what its reader sees (seeToken). A list in which no token lists a
	// project resolves no viewer, so it costs what it cost before lists.
	if slices.ContainsFunc(tokens, func(token model.AgentToken) bool { return len(token.Projects) > 0 }) {
		reader, ok := s.readerOf(w, r)
		if !ok {
			return
		}
		for i := range tokens {
			seeToken(reader, &tokens[i])
		}
	}
	WriteJSON(w, http.StatusOK, tokens)
}

// seeToken is token as reader reads it: its list cut to the projects reader's viewer sees, so no
// token answer names a project to a credential that does not see it (R3). What the token lists is
// its row's, which this never changes: the list is the copy scanned for this answer, cut in place.
//
// A token whose every project is hidden from its reader reads as one that lists none, so the
// person's own browser (the reader access.Resolve reads as one, Kind "user") is told the cut
// removed some (ProjectsHidden): Settings must not show such a token as one of the public projects
// only. No other reader is told, since telling a credential that a sibling lists a project it
// cannot see says that project exists. A cut that removed nothing leaves the answer as it was.
func seeToken(reader model.Actor, token *model.AgentToken) {
	listed := len(token.Projects)
	token.Projects = slices.DeleteFunc(token.Projects, func(project string) bool {
		return !slices.Contains(reader.Viewer.Sees, project)
	})
	token.ProjectsHidden = reader.Kind == "user" && len(token.Projects) < listed
}

// createAgentTokenInput is the body of POST /api/v1/me/agent-tokens.
type createAgentTokenInput struct {
	Name string `json:"name" required:"true"`
	// Scopes is the set the new token holds. A body that names none at all leaves it to the
	// caller's own set, which is why the field is a pointer: an empty array is a token that
	// holds nothing, and that is a different request from one that asks for the default.
	Scopes *[]string `json:"scopes"`
	// Projects is the list the new token may use. A body that names none leaves it to the
	// caller's own list, for the reason Scopes is a pointer: an empty array is a token that lists
	// nothing, which sees every public project its person sees.
	Projects *[]string `json:"projects"`
}

// createAgentToken is POST /api/v1/me/agent-tokens. Its answer is the one time the token's
// plaintext is shown, so an Idempotency-Key never keeps it (keyedWrites, answerHoldsSecret): the
// insert commits in a transaction of its own, which marks the key committed, and a repeat under the
// key is 409 KEY_COMMITTED - the token was made and cannot be shown again - never a second token.
//
// A token must never outlive the account that minted it. A sign-in under another Google account
// (store.PgPeopleStore.SignInSubject) ends the address's sessions and revokes its tokens in its
// transaction, but a mint whose caller resolved before that sign-in committed would otherwise
// insert after its revoke and leave the replaced account a live token. So the mint takes the
// address's advisory lock shared (store.AddressLockSharedSQL), which every such sign-in takes
// before it reads or writes the address's row, and only then inserts, in a statement read after
// that lock (mintAgentTokenSQL), on condition that its caller still holds the address: a cookie's
// session generation is still its login's, and a personal token is not revoked and its account
// still holds the address (tokenAccountReplaced): such a sign-in records another account, and the
// time it did, in its transaction, and the comparison reads both. A sign-in in flight makes the
// mint wait for it, a first sign-in on an address no row holds included; one that comes after
// waits for the mint, and then the token it made is revoked. A mint whose caller lost the address
// inserts nothing and is refused as that caller's next request is (mintCallerGone): 401
// TOKEN_REVOKED for a personal token, 401 NO_IDENTITY for a cookie. A broker-issued token never
// mints: none holds the approve scope this route requires (brokerPersonScopes). The lock is the
// one statement a mint costs beyond what it always cost; the re-check rides in the insert.
//
// The token records the Google account its minting credential is bound to (agent_tokens.subject,
// credentialSubjectSQL), read in the insert beside that re-check: an approve by the token names
// that account as the approving one (approverStanding), never whichever account holds the address
// when the approve is relayed.
func (s *server) createAgentToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input createAgentTokenInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		writeError(w, "AGENT_TOKEN_INPUT", http.StatusBadRequest, "name is required")
		return
	}
	scopes, ok := tokenScopes(w, actor, input.Scopes)
	if !ok {
		return
	}
	projects, ok := s.tokenProjects(w, r, input.Projects)
	if !ok {
		return
	}

	plaintext, hash, err := newAgentToken()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	prefix := plaintext[len("dsp_") : len("dsp_")+agentTokenPrefixLength]
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), store.AddressLockSharedSQL, actor.ID); err != nil {
		s.writeHandlerError(w, fmt.Errorf("create agent token: lock the owner's address: %w", err))
		return
	}
	cookieLogin, cookieGeneration := s.mintCookie(r, actor)
	var tokenID *string
	if actor.Act != nil && actor.Act.TokenID != "" {
		tokenID = &actor.Act.TokenID
	}
	recordsAccounts := s.deps.SignInIssuer != ""
	token, err := scanAgentToken(tx.QueryRow(r.Context(), mintAgentTokenSQL,
		actor.ID, input.Name, hash[:], prefix, scopes, projects, cookieLogin, cookieGeneration, tokenID, recordsAccounts))
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeAuthenticationError(w, mintCallerGone(tokenID))
		return
	}
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("create agent token: %w", err))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, fmt.Errorf("create agent token: %w", err))
		return
	}
	WriteJSON(w, http.StatusCreated, createAgentTokenResponse{AgentToken: token, Token: plaintext})
}

// mintAgentTokenSQL inserts a personal token only while its caller still holds the address. It is
// a statement of its own after store.AddressLockSharedSQL, so it reads what a sign-in that held
// the lock committed: under read committed, a Postgres statement's snapshot is taken as it starts,
// after the lock statement's wait. $7 and $8 are a cookie caller's login and session generation,
// refused once the login's generation is another (a row the login lacks is a session nothing
// ended since its cookie was checked); $9 is a personal token caller's token id, refused once
// revoked or once its account no longer holds the address (tokenAccountReplaced: people records
// another account, or the token records none and was made before people recorded the address's).
// Each is null for a caller of the other kind. $10 is whether the deployment's sign-in records
// accounts. The token records the minting credential's account (credentialSubjectSQL).
var mintAgentTokenSQL = `
	insert into agent_tokens (owner, name, token_hash, prefix, scopes, projects, subject)
	select $1::text, $2::text, $3::bytea, $4::text, $5::text[], $6::text[], ` +
	credentialSubjectSQL("$1::text", "$7::text", "$9::uuid", "$10::boolean") + `
	where not exists (select 1 from user_sessions where login = $7::text and generation <> $8::bigint)
	  and not exists (select 1 from agent_tokens left join people on people.email = canonical_login(agent_tokens.owner)
		where agent_tokens.id = $9::uuid and (agent_tokens.revoked_at is not null or ` + tokenAccountReplaced("$10::boolean") + `))
	returning id::text, name, prefix, scopes, projects, created_at, last_used_at, revoked_at
`

// credentialSubjectSQL is the SQL expression of the Google account (`sub`) a caller's own
// credential is bound to, null for none, over the four values a statement names: the caller's
// address, of the two credentials that may mint or decide the one it presented (the other null),
// and whether the deployment's sign-in records Google accounts (Deps.SignInIssuer non-empty).
// Each is read in the statement that also checks the credential still stands, so it is the
// credential's account and never another's:
//
//   - a session cookie: the address's recorded `sub`, which is the cookie's own while its session
//     generation stands, since the one write of people.sub (store.PgPeopleStore.SignInSubject) ends
//     every session of the address whenever it changes it; none where the sign-in records no
//     account (a Cognito pool's), since a `sub` left there by a Google sign-in before a switch to
//     the pool is not the account that signed this cookie in;
//   - a personal token: the account it recorded at its own mint (agent_tokens.subject).
//
// A token that records none is bound to the address's recorded account when it was made after
// people recorded it: no account that held the address before made it, so what it mints records
// the holder and the no-account state stops there. One made before is refused
// (tokenAccountReplaced) and binds nothing; one with no recording to compare with is bound to none,
// as before. Any other caller is bound to none: a broker-issued token never mints or decides
// (brokerPersonScopes).
func credentialSubjectSQL(address, cookieLogin, tokenID, recordsAccounts string) string {
	return "(case when " + cookieLogin + " is not null then (select sub from people where email = " + address + " and " + recordsAccounts + ")" +
		" when " + tokenID + " is not null then coalesce((select subject from agent_tokens where id = " + tokenID + "), " +
		"(select sub from people where email = " + address + " and " + recordsAccounts +
		" and sub_recorded_at is not null and (select created_at from agent_tokens where id = " + tokenID + ") >= sub_recorded_at))" +
		" end)"
}

// mintCookie is the login and session generation of the cookie a mint's caller signed in with, or
// nil for both when the caller is no cookie: a credential's caller (actor.Act names it), or an
// identity of another kind (identity.SessionCookies).
func (s *server) mintCookie(r *http.Request, actor model.Actor) (*string, *int64) {
	if actor.Act != nil {
		return nil, nil
	}
	cookies, ok := s.deps.Identity.(identity.SessionCookies)
	if !ok {
		return nil, nil
	}
	login, generation, ok := cookies.SessionCookie(r)
	if !ok || model.CanonicalLogin(login) != actor.ID {
		return nil, nil
	}
	return &login, &generation
}

// mintCallerGone is the refusal of a mint whose caller lost the address while it waited on the
// address's lock, the one mintAgentTokenSQL's refusing arm names: a personal token caller's token was
// revoked or its account no longer holds the address (401 TOKEN_REVOKED, as personalTokenActor
// answers it next), and a cookie caller's session was ended (401 NO_IDENTITY). It writes nothing:
// the caller is not resolved again, since resolving writes its credential's bookkeeping, and a
// keyed write's handler writes only in the transaction that marks its key
// (TestEveryKeyedWriteCommitsInsideATransactionTheMarkHooks).
func mintCallerGone(tokenID *string) error {
	if tokenID != nil {
		return errTokenRevoked()
	}
	return identity.ErrNoIdentity
}

// errTokenRevoked is the refusal of a revoked personal token.
func errTokenRevoked() error {
	return errorf(http.StatusUnauthorized, "TOKEN_REVOKED", "agent token has been revoked")
}

// updateAgentTokenScopesInput is the body of PATCH /api/v1/me/agent-tokens/{id}.
type updateAgentTokenScopesInput struct {
	Scopes *[]string `json:"scopes" required:"true"`
	// Projects is the whole list the token is to use, with the mint's refusals. A body that names
	// none leaves the token's list as it stands.
	Projects *[]string `json:"projects"`
}

// updateAgentTokenScopes is PATCH /api/v1/me/agent-tokens/{id}: the whole set one of the
// caller's own tokens is to hold, and the list it is to use when the body names one, with the
// mint's refusals. Its answer holds no plaintext — the token's secret was shown once, at the
// mint — so its key keeps that answer, which is why it is not in answerHoldsSecret
// (request_keys.go). The update runs in a transaction of the shared pool, as every keyed write
// does: the commit step marks the request's key committed with the write itself
// (markRequestKeyCommitted).
func (s *server) updateAgentTokenScopes(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "token") {
		return
	}
	var input updateAgentTokenScopesInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Scopes == nil {
		writeError(w, codeTokenScopesInput, http.StatusBadRequest,
			"scopes is required: it is the whole set the token is to hold")
		return
	}
	scopes, ok := tokenScopes(w, actor, input.Scopes)
	if !ok {
		return
	}
	// Nil keeps the token's list: the update below reads it as null. A list the body names is one
	// the caller sees (tokenProjects); a kept one is cut to what the caller sees in the answer
	// (seeToken), whose viewer is resolved here, before the transaction holds a connection.
	var projects []string
	var reader model.Actor
	if input.Projects != nil {
		if projects, ok = s.tokenProjects(w, r, input.Projects); !ok {
			return
		}
	} else if reader, ok = s.readerOf(w, r); !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	token, err := scanAgentToken(tx.QueryRow(r.Context(), `
		update agent_tokens
		set scopes = $3, projects = coalesce($4::text[], projects)
		where id = $1 and owner = $2
		returning id::text, name, prefix, scopes, projects, created_at, last_used_at, revoked_at
	`, r.PathValue("id"), actor.ID, scopes, projects))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("update agent token scopes: %w", err))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, fmt.Errorf("update agent token scopes: %w", err))
		return
	}
	if reader.Viewer != nil {
		seeToken(reader, &token)
	}
	WriteJSON(w, http.StatusOK, token)
}

// codeTokenScopesInput refuses a set no token may hold: a word that is no scope this deployment
// knows, or approve without person, which agent_tokens' own check refuses as well.
const codeTokenScopesInput = "TOKEN_SCOPES_INPUT"

// tokenScopes is the set a mint or an edit gives a token: the one the body names, or, where it
// names none, the set the caller's own credential holds. It writes its own refusal and answers
// false for a set the token may not hold (codeTokenScopesInput) and for one the caller does not
// hold itself (403 OAUTH_SCOPE_REQUIRED): a credential gives a token only what it holds, or a
// token narrowed to one scope would mint itself a wide one. A scope named twice is stored once.
func tokenScopes(w http.ResponseWriter, actor model.Actor, requested *[]string) ([]string, bool) {
	if requested == nil {
		return callerScopes(actor), true
	}
	// At most the scopes this deployment knows, whatever the body holds: an unknown word is
	// refused and a repeat is folded, so a long array buys no room.
	scopes := make([]string, 0, len(model.Scopes))
	for _, scope := range *requested {
		if !slices.Contains(model.Scopes, scope) {
			writeError(w, codeTokenScopesInput, http.StatusBadRequest, fmt.Sprintf(
				"%q is no scope this deployment knows; the scopes are %s",
				scope, strings.Join(model.Scopes, " and ")))
			return nil, false
		}
		// Act is nil for a browser, whose person holds every scope, which is what Holds answers
		// for one; a token's act carries the set its own row names.
		if !actor.Act.Holds(scope) {
			refuseMissingScope(w, scope, fmt.Sprintf(
				"this request's credential does not hold the %s scope, so it cannot give it to a token", scope))
			return nil, false
		}
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	if slices.Contains(scopes, model.ScopeApprove) && !slices.Contains(scopes, model.ScopePerson) {
		writeError(w, codeTokenScopesInput, http.StatusBadRequest, fmt.Sprintf(
			"a token holding %s holds %s too: settling what a person settles needs a credential that acts as that person",
			model.ScopeApprove, model.ScopePerson))
		return nil, false
	}
	return scopes, true
}

// callerScopes is the set the caller's own credential holds: the scopes actingPerson copied onto
// Act for a token that acted for its person, and both scopes for a browser, whose cookie names
// no agent because the person themselves acted.
func callerScopes(actor model.Actor) []string {
	if actor.Act != nil {
		return actor.Act.Scopes
	}
	return model.Scopes
}

// codeTokenProjectsInput refuses a list no token may hold: more keys than
// contracts.MaxTokenProjects, a key the caller does not see, or, from a credential whose writes a
// list holds, a list naming none.
const codeTokenProjectsInput = "TOKEN_PROJECTS_INPUT"

// tokenProjects is the list a mint or an edit gives a token: the one the body names, or, where it
// names none, the list that holds the caller's own writes (writesHeldTo). It writes its own refusal
// and answers false for a list of more than contracts.MaxTokenProjects keys, for a key the caller
// does not see, and for an empty list from a credential whose writes a list holds, because a token
// listing nothing reaches what that list does not: every public project its person sees, for a
// restricted member's list, and the rows of the whole deployment, for every list
// (withinProjectScope). What the caller sees is the rule's answer for its credential
// (Viewer.Sees, which access.CredentialCovers decides with its person), so a token takes no
// project its caller does not reach: a restricted member's credential gives a token only a part of
// its own list, and a full member's a part of the public projects and of the private ones it lists.
// A key named twice is listed once, and the list is stored sorted, as Credential.Projects holds it.
//
// What the caller sees, and the list its writes are held to, are read off the write's reader
// (resolveReader, which finds the one withinProjectScope put in the request's context) and the
// request's credential: a credential's list as it holds the credential now, so a broker token
// acting as its person, whose list decides nothing while no project is private, gives a token
// what a credential listing none gives (R9). A key the caller does not see is refused in the same
// words whether a hidden project holds it or none does, so the refusal says nothing of a project
// the caller may not see. So the list a mint answers, and the one an edit names, is one its caller
// sees whole, and needs no cut (seeToken).
func (s *server) tokenProjects(w http.ResponseWriter, r *http.Request, requested *[]string) ([]string, bool) {
	caller, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return nil, false
	}
	reader, ok := s.readerOf(w, r)
	if !ok {
		return nil, false
	}
	own := writesHeldTo(reader, caller.Credential)
	named := own
	if requested != nil {
		named = *requested
	}
	projects := slices.Compact(slices.Sorted(slices.Values(named)))
	switch {
	case len(projects) > contracts.MaxTokenProjects:
		writeError(w, codeTokenProjectsInput, http.StatusBadRequest, fmt.Sprintf(
			"a token lists at most %d projects; this one names %d", contracts.MaxTokenProjects, len(projects)))
		return nil, false
	case len(projects) == 0 && len(own) > 0:
		writeError(w, codeTokenProjectsInput, http.StatusBadRequest,
			"this request's credential lists projects, so a token it gives lists some of them: a token listing none reaches what that list does not")
		return nil, false
	case len(projects) == 0:
		// Not nil: agent_tokens.projects is not null, and its empty list is a token listing none.
		return []string{}, true
	}
	for _, project := range projects {
		if reader.Viewer == nil || !slices.Contains(reader.Viewer.Sees, project) {
			writeError(w, codeTokenProjectsInput, http.StatusBadRequest, fmt.Sprintf(
				"%q is no project this request's credential sees", project))
			return nil, false
		}
	}
	return projects, true
}

func (s *server) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "token") {
		return
	}

	result, err := s.deps.Store.Pool.Exec(r.Context(), `
		update agent_tokens
		set revoked_at = coalesce(revoked_at, now())
		where id = $1 and owner = $2
	`, r.PathValue("id"), actor.ID)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("revoke agent token: %w", err))
		return
	}
	if result.RowsAffected() == 0 {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// codeTokenOwnerNotAdmitted refuses a token acting for a restricted member this deployment would
// not sign in now (restrictedOwnerAdmitted): their personal token, or a token the secrets broker
// issued naming them.
const codeTokenOwnerNotAdmitted = "TOKEN_OWNER_NOT_ADMITTED"

// personalTokenActor resolves a personal token (dsp_) to the credential its row says it is: its
// owner, its scopes and its list of projects. A plaintext no row holds is no actor, and a revoked
// token is 401 TOKEN_REVOKED. A token carries its owner's sign-in to an agent, so it acts only
// while its owner may still sign in: a restricted member's token is checked at every use
// (restrictedOwnerAdmitted, which asks the sign-in rule itself), so it stops the moment the rule
// would refuse its owner's sign-in (outside sign-in turned off, the owner's address on a domain
// the Workspace owns, no project naming them any more) and works again if that is undone, while
// the owner's cookie stops at its next hourly confirmation. optionalActor is the one place a
// bearer is resolved, for every route and the live socket's heartbeat (whileCallerResolves), so
// the check holds on all of them.
//
// The owner's member kind is read in the token's own statement, a left join on people by the
// owner in the form people.email holds, model.CanonicalLogin's as SQL spells it once
// (canonical_login, migration 20261117000831: every Unicode space trimmed, A-Z lowered), whatever
// case or surrounding space the row was written with, so a token whose owner differs from its
// people row only so is still held to that row's kind. The join is
// an equality on people's key, so the token of a full member, or of an owner no people row holds,
// costs no read beyond the one it always cost and answers as it always did; only a restricted
// owner's token reads the lists.
//
// The same join reads whether the token's account still holds the address (tokenAccountReplaced):
// a token that recorded a Google account at its mint (agent_tokens.subject) acts only while
// people.sub records that account or none, and one that recorded none only while it was made
// after people recorded the address's account, or no recording time is set. Once another account's
// sign-in records itself for the address, the first recording included (which also revokes the
// address's tokens that recorded another account, store.PgPeopleStore.SignInSubject), the token is
// 401 TOKEN_REVOKED, as a revoked token is, since its account is not the address's any more and
// never will be again under this token.
func (s *server) personalTokenActor(ctx context.Context, plaintext string) (model.Actor, error) {
	hash := sha256.Sum256([]byte(plaintext))
	var id, name, owner string
	var scopes, projects []string
	var revokedAt *time.Time
	var memberKind *string
	var replaced bool
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select agent_tokens.id::text, agent_tokens.name, agent_tokens.owner, agent_tokens.scopes,
			agent_tokens.projects, agent_tokens.revoked_at, people.member_kind,
			`+tokenAccountReplaced("$2::boolean")+`
		from agent_tokens
		left join people on people.email = canonical_login(agent_tokens.owner)
		where agent_tokens.token_hash = $1
	`, hash[:], s.deps.SignInIssuer != "").Scan(&id, &name, &owner, &scopes, &projects, &revokedAt, &memberKind, &replaced)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Actor{}, nil
	}
	if err != nil {
		return model.Actor{}, fmt.Errorf("look up agent token: %w", err)
	}
	if revokedAt != nil || replaced {
		return model.Actor{}, errTokenRevoked()
	}
	if memberKind != nil && model.MemberKind(*memberKind) == model.MemberRestricted {
		if err := s.restrictedOwnerAdmitted(ctx, model.CanonicalLogin(owner)); err != nil {
			return model.Actor{}, err
		}
	}
	if _, err := s.deps.Store.Pool.Exec(ctx, `
		update agent_tokens
		set last_used_at = now()
		where token_hash = $1
		  and revoked_at is null
		  and (last_used_at is null or last_used_at < now() - interval '1 minute')
	`, hash[:]); err != nil {
		return model.Actor{}, fmt.Errorf("record agent token use: %w", err)
	}
	// The token's own row says what it may do and which projects it may use, and it names the
	// credential that acted: the person's token, not the person at a browser.
	return model.Actor{
		Kind:       "session",
		Owner:      &owner,
		Credential: &model.Credential{Scopes: scopes, TokenID: id, TokenName: name, Projects: projects},
	}, nil
}

// tokenAccountReplaced is the SQL expression, over the row agent_tokens joined to its owner's
// people row and whether the deployment's sign-in records accounts (recordsAccounts), that the
// personal token's account no longer holds the address (personalTokenActor, the mint's and an
// approve's re-reads). With accounts recorded and people recording one for the address, it is true
// for a token that recorded another account at its mint, and for one that recorded none and was
// made before people recorded the address's account (agent_tokens.created_at before
// people.sub_recorded_at, both from Dispatch's own clock): an account that held the address before
// may have minted it. A token made after the recording, an address recording no account or no
// recording time, and a deployment whose sign-in records none are not replaced.
func tokenAccountReplaced(recordsAccounts string) string {
	return "coalesce(" + recordsAccounts + " and people.sub is not null and (" +
		"(agent_tokens.subject is not null and agent_tokens.subject <> people.sub)" +
		" or (agent_tokens.subject is null and people.sub_recorded_at is not null and agent_tokens.created_at < people.sub_recorded_at)), false)"
}

// restrictedOwnerAdmitted answers nil while a token acting for a restricted member may act - their
// personal token (personalTokenActor) or a token the secrets broker issued naming them
// (brokerPersonAdmitted), the one check both are held to: while the sign-in rule would sign owner
// in now (Deps.RestrictedAdmission, identity.SignInRule's AdmitsRestricted, the very decision a
// restricted sign-in and its hourly confirmation make), so there is no second copy of the rule
// here to fall behind it. Otherwise it is 401 TOKEN_OWNER_NOT_ADMITTED saying why: outside sign-in
// is off (or this server signs nobody in), the address is on a domain the Workspace owns, or no
// project names owner any more. Lists that cannot be read are 503 IDENTITY_UNAVAILABLE, never an
// accept. The answer names no owner: whoever holds the token is told why it stopped, not whose it
// is.
func (s *server) restrictedOwnerAdmitted(ctx context.Context, owner string) error {
	if s.deps.RestrictedAdmission == nil {
		return tokenOwnerOutsideOff()
	}
	err := s.deps.RestrictedAdmission.AdmitsRestricted(ctx, owner)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrMembershipUnreadable):
		slog.Warn("dispatch: a restricted member's token was refused: the access lists could not be read", "error", err)
		return errorf(http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE",
			"whether a project still names this token's owner could not be read; try again in a minute")
	case errors.Is(err, identity.ErrOutsideSignInOff):
		return tokenOwnerOutsideOff()
	case errors.Is(err, identity.ErrWorkspaceAddress):
		return errorf(http.StatusUnauthorized, codeTokenOwnerNotAdmitted,
			"this token's owner is a restricted member from outside the sign-in domain whose address is on a domain the Google Workspace owns (DISPATCH_SIGNIN_DOMAIN or DISPATCH_SIGNIN_WORKSPACE_DOMAINS), which signs in only as a Workspace account, so the token acts for nobody")
	case errors.Is(err, identity.ErrNotListed):
		return errorf(http.StatusUnauthorized, codeTokenOwnerNotAdmitted,
			"this token's owner is a restricted member from outside the sign-in domain, and no project names them any more, as its owner or on its access list, so the token acts for nobody")
	default:
		return errorf(http.StatusUnauthorized, codeTokenOwnerNotAdmitted,
			"this token's owner is a restricted member from outside the sign-in domain whom this deployment would not sign in now, so the token acts for nobody")
	}
}

// tokenOwnerOutsideOff is a restricted member's token on a deployment that signs nobody from
// outside its domain in.
func tokenOwnerOutsideOff() error {
	return errorf(http.StatusUnauthorized, codeTokenOwnerNotAdmitted,
		"this token's owner is a restricted member from outside the sign-in domain, and outside sign-in is off on this deployment (DISPATCH_SIGNIN_OUTSIDE is unset), so the token acts for nobody")
}

func newAgentToken() (string, [sha256.Size]byte, error) {
	var randomBytes [32]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("generate agent token: %w", err)
	}
	plaintext := "dsp_" + base64.RawURLEncoding.EncodeToString(randomBytes[:])
	return plaintext, sha256.Sum256([]byte(plaintext)), nil
}

func scanAgentToken(row rowScanner) (model.AgentToken, error) {
	var token model.AgentToken
	if err := row.Scan(&token.ID, &token.Name, &token.Prefix, &token.Scopes, &token.Projects, &token.CreatedAt, &token.LastUsedAt, &token.RevokedAt); err != nil {
		return model.AgentToken{}, fmt.Errorf("scan agent token: %w", err)
	}
	return token, nil
}
