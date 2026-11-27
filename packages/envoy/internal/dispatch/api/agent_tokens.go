package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const agentTokenPrefixLength = 8

// A person's personal tokens are kept in two tables. agent_tokens holds every token that may
// write. agent_read_tokens holds every read-only token: one that holds read (model.ScopeRead),
// alone or beside person, which Dispatch answers on the reads of its read class alone
// (refuseReadOnlyToken). A build before read-only tokens looks a bearer up in agent_tokens alone,
// so it finds no read-only token and answers it 401, rather than read its scopes and let it
// write (migration 20261120023712_agent_read_tokens). Both tables carry the same columns, and
// agentTokenColumns is what every statement here answers of a row, as scanAgentToken reads it.
const agentTokenColumns = `id::text, name, prefix, scopes, created_at, last_used_at, revoked_at`

// The mint and a token's last use each run one of two statements, one per table. Each names its
// table in its own text, so the census of item-row writers (item_writer_census_test.go) reads the
// table it writes, and takes what follows the table from one of these, so the two stay one text:
// agentTokenInsert is the mint's columns, their values and the row it answers (createAgentToken),
// and agentTokenUse is a token's last use, written at most once a minute (personalTokenActor).
const (
	agentTokenInsert = `(owner, name, token_hash, prefix, scopes)
		values ($1, $2, $3, $4, $5)
		returning ` + agentTokenColumns
	agentTokenUse = `set last_used_at = now()
		where token_hash = $1
		  and revoked_at is null
		  and (last_used_at is null or last_used_at < now() - interval '1 minute')`
)

type createAgentTokenResponse struct {
	model.AgentToken
	Token string `json:"token"`
}

// listAgentTokens is GET /api/v1/me/agent-tokens: the caller's tokens of both tables, newest
// first, each with the scopes it holds, so a read-only token is listed holding read.
func (s *server) listAgentTokens(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}

	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+agentTokenColumns+`
		from agent_tokens
		where owner = $1
		union all
		select `+agentTokenColumns+`
		from agent_read_tokens
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
	WriteJSON(w, http.StatusOK, tokens)
}

// createAgentTokenInput is the body of POST /api/v1/me/agent-tokens.
type createAgentTokenInput struct {
	Name string `json:"name" required:"true"`
	// Scopes is the set the new token holds. A body that names none at all leaves it to the
	// caller's own set, which is why the field is a pointer: an empty array is a token that
	// holds nothing, and that is a different request from one that asks for the default. A set
	// that holds read is a read-only token's: read alone, or read beside person.
	Scopes *[]string `json:"scopes" enum:"person,approve,read"`
}

// createAgentToken is POST /api/v1/me/agent-tokens. Its answer is the one time the token's
// plaintext is shown, so an Idempotency-Key never keeps it (keyedWrites, answerHoldsSecret): the
// insert commits in a transaction of its own, which marks the key committed, and a repeat under the
// key is 409 KEY_COMMITTED - the token was made and cannot be shown again - never a second token.
// A token whose set holds read is kept in agent_read_tokens, and every other in agent_tokens.
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
	// Each statement names its table in its own text (agentTokenInsert).
	insert := `insert into agent_tokens ` + agentTokenInsert
	if slices.Contains(scopes, model.ScopeRead) {
		insert = `insert into agent_read_tokens ` + agentTokenInsert
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
	token, err := scanAgentToken(tx.QueryRow(r.Context(), insert, actor.ID, input.Name, hash[:], prefix, scopes))
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

// updateAgentTokenScopesInput is the body of PATCH /api/v1/me/agent-tokens/{id}. Its scopes never
// hold read: no token changes to or from read only (updateAgentTokenScopes).
type updateAgentTokenScopesInput struct {
	Scopes *[]string `json:"scopes" required:"true" enum:"person,approve"`
}

// updateAgentTokenScopes is PATCH /api/v1/me/agent-tokens/{id}: the whole set one of the
// caller's own tokens is to hold, with the mint's refusals. Its answer holds no plaintext — the
// token's secret was shown once, at the mint — so its key keeps that answer, which is why it is
// not in answerHoldsSecret (request_keys.go). The update runs in a transaction of the shared
// pool, as every keyed write does: the commit step marks the request's key committed with the
// write itself (markRequestKeyCommitted).
//
// No token changes to or from read only, so both are 400 TOKEN_SCOPES_INPUT: a set that holds
// read, since a token that writes never gains it, and any set for a read-only token, whose scopes
// never change at all. An agent holding a token keeps what it was given until the token is
// revoked; a person who wants it to read only, or to write, makes a new token.
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
	if slices.Contains(*input.Scopes, model.ScopeRead) {
		writeError(w, codeTokenScopesInput, http.StatusBadRequest,
			"no token's scopes change to or from read: a token that writes never gains read, and a read-only token keeps the scopes it was made with. A person makes a new token for that, and revokes this one")
		return
	}
	scopes, ok := tokenScopes(w, actor, input.Scopes)
	if !ok {
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
		set scopes = $3
		where id = $1 and owner = $2
		returning `+agentTokenColumns, r.PathValue("id"), actor.ID, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		// No token of the caller's that writes. One of the caller's read-only tokens is still
		// refused by name, so the person learns why rather than that it is not there.
		var readOnly bool
		if err := tx.QueryRow(r.Context(), `
			select exists (select 1 from agent_read_tokens where id = $1 and owner = $2)
		`, r.PathValue("id"), actor.ID).Scan(&readOnly); err != nil {
			s.writeHandlerError(w, fmt.Errorf("update agent token scopes: %w", err))
			return
		}
		if readOnly {
			writeError(w, codeTokenScopesInput, http.StatusBadRequest,
				"this token is read only, and a read-only token keeps the scopes it was made with. A person makes a new token for other scopes, and revokes this one")
			return
		}
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
	WriteJSON(w, http.StatusOK, token)
}

// codeTokenScopesInput refuses a set no token may hold: a word that is no scope this deployment
// knows, approve without person, which agent_tokens' own check refuses as well, or read beside
// approve, which agent_read_tokens' own check refuses. It also refuses any change of a token to or
// from read only (updateAgentTokenScopes).
const codeTokenScopesInput = "TOKEN_SCOPES_INPUT"

// tokenWords is every word a personal token's scopes may hold: the scopes a credential acts under
// (model.Scopes), and read (model.ScopeRead), which marks a token that may only read.
var tokenWords = append(slices.Clip(model.Scopes), model.ScopeRead)

// tokenScopes is the set a mint or an edit gives a token: the one the body names, or, where it
// names none, the set the caller's own credential holds. It writes its own refusal and answers
// false for a set the token may not hold (codeTokenScopesInput) and for one the caller does not
// hold itself (403 OAUTH_SCOPE_REQUIRED): a credential gives a token only what it holds, or a
// token narrowed to one scope would mint itself a wide one. read is the one word the caller need
// not hold, since it gives the token nothing: it narrows the token to reading. A read-only token
// holds read alone or beside person, never beside approve, since settling what a person settles
// is a write. A scope named twice is stored once.
func tokenScopes(w http.ResponseWriter, actor model.Actor, requested *[]string) ([]string, bool) {
	if requested == nil {
		return callerScopes(actor), true
	}
	// At most the words this deployment knows, whatever the body holds: an unknown word is
	// refused and a repeat is folded, so a long array buys no room.
	scopes := make([]string, 0, len(tokenWords))
	for _, scope := range *requested {
		if !slices.Contains(tokenWords, scope) {
			writeError(w, codeTokenScopesInput, http.StatusBadRequest, fmt.Sprintf(
				"%q is no scope this deployment knows; the scopes are %s, %s and %s",
				scope, model.ScopePerson, model.ScopeApprove, model.ScopeRead))
			return nil, false
		}
		// Act is nil for a browser, whose person holds every scope, which is what Holds answers
		// for one; a token's act carries the set its own row names.
		if scope != model.ScopeRead && !actor.Act.Holds(scope) {
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
	if slices.Contains(scopes, model.ScopeRead) && slices.Contains(scopes, model.ScopeApprove) {
		writeError(w, codeTokenScopesInput, http.StatusBadRequest, fmt.Sprintf(
			"a token holding %s holds it alone or beside %s, never beside %s: settling what a person settles is a write",
			model.ScopeRead, model.ScopePerson, model.ScopeApprove))
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

// revokeAgentToken is DELETE /api/v1/me/agent-tokens/{id}: it revokes one of the caller's tokens,
// whichever table keeps it, in one statement. A token already revoked keeps the time it was first
// revoked, so a repeat answers 204 as the first did.
func (s *server) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "token") {
		return
	}

	var revoked int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		with writing as (
			update agent_tokens
			set revoked_at = coalesce(revoked_at, now())
			where id = $1 and owner = $2
			returning id
		), reading as (
			update agent_read_tokens
			set revoked_at = coalesce(revoked_at, now())
			where id = $1 and owner = $2
			returning id
		)
		select (select count(*) from writing) + (select count(*) from reading)
	`, r.PathValue("id"), actor.ID).Scan(&revoked); err != nil {
		s.writeHandlerError(w, fmt.Errorf("revoke agent token: %w", err))
		return
	}
	if revoked == 0 {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// personalTokenActor resolves a personal token: it looks the token's hash up in agent_tokens and
// agent_read_tokens in one query, and answers the zero actor for a token neither table holds. A
// row of agent_read_tokens is a read-only credential (model.Credential.ReadOnly), which the
// read-only gate holds to the read class: its scopes are the person scope where the row holds it,
// and never read, which marks the credential and is no scope a route can require. A token's use is
// written to its own row at most once a minute, so a busy agent costs its row one write a minute
// rather than one a request.
func (s *server) personalTokenActor(ctx context.Context, plaintext string) (model.Actor, error) {
	hash := sha256.Sum256([]byte(plaintext))
	var id, name, owner string
	var scopes []string
	var revokedAt *time.Time
	var readOnly bool
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select id::text, name, owner, scopes, revoked_at, false
		from agent_tokens
		where token_hash = $1
		union all
		select id::text, name, owner, scopes, revoked_at, true
		from agent_read_tokens
		where token_hash = $1
	`, hash[:]).Scan(&id, &name, &owner, &scopes, &revokedAt, &readOnly)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Actor{}, nil
	}
	if err != nil {
		return model.Actor{}, fmt.Errorf("look up agent token: %w", err)
	}
	if revokedAt != nil {
		return model.Actor{}, errorf(http.StatusUnauthorized, "TOKEN_REVOKED", "agent token has been revoked")
	}
	// Each statement names its table in its own text (agentTokenUse).
	recordUse := `update agent_tokens ` + agentTokenUse
	if readOnly {
		recordUse = `update agent_read_tokens ` + agentTokenUse
	}
	if _, err := s.deps.Store.Pool.Exec(ctx, recordUse, hash[:]); err != nil {
		return model.Actor{}, fmt.Errorf("record agent token use: %w", err)
	}
	// The token's own row says what it may do, and it names the credential that acted: the
	// person's token, not the person at a browser.
	credential := &model.Credential{Scopes: scopes, TokenID: id, TokenName: name}
	if readOnly {
		credential.Scopes = slices.DeleteFunc(scopes, func(scope string) bool { return scope == model.ScopeRead })
		credential.ReadOnly = true
	}
	return model.Actor{
		Kind:       "session",
		Owner:      &owner,
		Credential: credential,
	}, nil
}

func newAgentToken() (string, [sha256.Size]byte, error) {
	var randomBytes [32]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("generate agent token: %w", err)
	}
	plaintext := "dsp_" + base64.RawURLEncoding.EncodeToString(randomBytes[:])
	return plaintext, sha256.Sum256([]byte(plaintext)), nil
}

// rowScanner is the one method a pgx.Row and a pgx.Rows share, so one scan function reads a row
// from either.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAgentToken(row rowScanner) (model.AgentToken, error) {
	var token model.AgentToken
	if err := row.Scan(&token.ID, &token.Name, &token.Prefix, &token.Scopes, &token.CreatedAt, &token.LastUsedAt, &token.RevokedAt); err != nil {
		return model.AgentToken{}, fmt.Errorf("scan agent token: %w", err)
	}
	return token, nil
}
