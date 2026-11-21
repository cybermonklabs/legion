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

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
	tokens, err := s.agentTokensOf(r.Context(), actor.ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, tokens)
}

// agentTokensOf is every personal agent token owner holds, newest first, the revoked ones among
// them with their revoked_at: what GET /api/v1/me/agent-tokens answers whole and GET
// /api/v2/me/agent-tokens pages.
func (s *server) agentTokensOf(ctx context.Context, owner string) ([]model.AgentToken, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select id::text, name, prefix, scopes, created_at, last_used_at, revoked_at
		from agent_tokens
		where owner = $1
		order by created_at desc
	`, owner)
	if err != nil {
		return nil, fmt.Errorf("list agent tokens: %w", err)
	}
	defer rows.Close()

	tokens := []model.AgentToken{}
	for rows.Next() {
		token, err := scanAgentToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent tokens: %w", err)
	}
	return tokens, nil
}

// createAgentTokenInput is the body of POST /api/v1/me/agent-tokens.
type createAgentTokenInput struct {
	Name string `json:"name" required:"true"`
	// Scopes is the set the new token holds. A body that names none at all leaves it to the
	// caller's own set, which is why the field is a pointer: an empty array is a token that
	// holds nothing, and that is a different request from one that asks for the default.
	Scopes *[]string `json:"scopes"`
}

// createAgentToken is POST /api/v1/me/agent-tokens. Its answer is the one time the token's
// plaintext is shown, so an Idempotency-Key never keeps it (keyedWrites, answerHoldsSecret): the
// insert commits in a transaction of its own, which marks the key committed, and a repeat under the
// key is 409 KEY_COMMITTED - the token was made and cannot be shown again - never a second token.
func (s *server) createAgentToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input createAgentTokenInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.createAgentTokenFrom(w, r, actor, input)
}

// createAgentTokenFrom mints the token input asks for as actor and answers it, the plaintext with
// it: the work of POST /api/v1/me/agent-tokens and POST /api/v2/me/agent-tokens once each has read
// its body.
func (s *server) createAgentTokenFrom(w http.ResponseWriter, r *http.Request, actor model.Actor, input createAgentTokenInput) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		writeError(w, r, apicode.AgentTokenInput, "name is required")
		return
	}
	scopes, ok := tokenScopes(w, r, actor, input.Scopes)
	if !ok {
		return
	}

	plaintext, hash, err := newAgentToken()
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	prefix := plaintext[len("dsp_") : len("dsp_")+agentTokenPrefixLength]
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	token, err := scanAgentToken(tx.QueryRow(r.Context(), `
		insert into agent_tokens (owner, name, token_hash, prefix, scopes)
		values ($1, $2, $3, $4, $5)
		returning id::text, name, prefix, scopes, created_at, last_used_at, revoked_at
	`, actor.ID, input.Name, hash[:], prefix, scopes))
	if err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("create agent token: %w", err))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("create agent token: %w", err))
		return
	}
	WriteJSON(w, http.StatusCreated, createAgentTokenResponse{AgentToken: token, Token: plaintext})
}

// updateAgentTokenScopesInput is the body of PATCH /api/v1/me/agent-tokens/{id}.
type updateAgentTokenScopesInput struct {
	Scopes *[]string `json:"scopes" required:"true"`
}

// updateAgentTokenScopes is PATCH /api/v1/me/agent-tokens/{id}: the whole set one of the
// caller's own tokens is to hold, with the mint's refusals. Its answer holds no plaintext — the
// token's secret was shown once, at the mint — so its key keeps that answer, which is why it is
// not in answerHoldsSecret (request_keys.go). The update runs in a transaction of the shared
// pool, as every keyed write does: the commit step marks the request's key committed with the
// write itself (markRequestKeyCommitted).
func (s *server) updateAgentTokenScopes(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "token", apicode.TokenIDInput) {
		return
	}
	var input updateAgentTokenScopesInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.updateAgentTokenScopesFrom(w, r, actor, input)
}

// updateAgentTokenScopesFrom gives the caller's token the path's {id} names the set input holds
// and answers the token: the work of PATCH /api/v1/me/agent-tokens/{id} and PATCH
// /api/v2/me/agent-tokens/{id} once each has read its body. A token of another person's, or
// none, is 404 NOT_FOUND; the /api/v2 route has answered its own 404 for one already.
func (s *server) updateAgentTokenScopesFrom(w http.ResponseWriter, r *http.Request, actor model.Actor, input updateAgentTokenScopesInput) {
	if input.Scopes == nil {
		writeError(w, r, apicode.TokenScopesInput,
			"scopes is required: it is the whole set the token is to hold")
		return
	}
	scopes, ok := tokenScopes(w, r, actor, input.Scopes)
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	token, err := scanAgentToken(tx.QueryRow(r.Context(), `
		update agent_tokens
		set scopes = $3
		where id = $1 and owner = $2
		returning id::text, name, prefix, scopes, created_at, last_used_at, revoked_at
	`, r.PathValue("id"), actor.ID, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, r, apicode.NotFound, "not found")
		return
	}
	if err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("update agent token scopes: %w", err))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("update agent token scopes: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, token)
}

// tokenScopes is the set a mint or an edit gives a token: the one the body names, or, where it
// names none, the set the caller's own credential holds. It writes its own refusal and answers
// false for a set the token may not hold (400 TOKEN_SCOPES_INPUT: a word that is no scope this
// deployment knows, or approve without person, which agent_tokens' own check refuses as well) and
// for one the caller does not hold itself (403 OAUTH_SCOPE_REQUIRED): a credential gives a token
// only what it holds, or a token narrowed to one scope would mint itself a wide one. A scope named
// twice is stored once.
func tokenScopes(w http.ResponseWriter, r *http.Request, actor model.Actor, requested *[]string) ([]string, bool) {
	if requested == nil {
		return callerScopes(actor), true
	}
	// At most the scopes this deployment knows, whatever the body holds: an unknown word is
	// refused and a repeat is folded, so a long array buys no room.
	scopes := make([]string, 0, len(model.Scopes))
	for _, scope := range *requested {
		if !slices.Contains(model.Scopes, scope) {
			writeError(w, r, apicode.TokenScopesInput, fmt.Sprintf(
				"%q is no scope this deployment knows; the scopes are %s",
				scope, strings.Join(model.Scopes, " and ")))
			return nil, false
		}
		// Act is nil for a browser, whose person holds every scope, which is what Holds answers
		// for one; a token's act carries the set its own row names.
		if !actor.Act.Holds(scope) {
			refuseMissingScope(w, r, scope, fmt.Sprintf(
				"this request's credential does not hold the %s scope, so it cannot give it to a token", scope))
			return nil, false
		}
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	if slices.Contains(scopes, model.ScopeApprove) && !slices.Contains(scopes, model.ScopePerson) {
		writeError(w, r, apicode.TokenScopesInput, fmt.Sprintf(
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

func (s *server) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "token", apicode.TokenIDInput) {
		return
	}
	revoked, err := s.revokeAgentTokenOf(r.Context(), r.PathValue("id"), actor.ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !revoked {
		writeError(w, r, apicode.NotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeAgentTokenOf marks the token id names revoked, where owner holds it, and reports whether
// owner holds it at all. A token revoked already keeps its first revoked_at and still counts as
// held. The update runs in a transaction of the shared pool, so a keyed DELETE of /api/v2 marks its
// key committed with it (markRequestKeyCommitted).
func (s *server) revokeAgentTokenOf(ctx context.Context, id, owner string) (bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `
		update agent_tokens
		set revoked_at = coalesce(revoked_at, now())
		where id = $1 and owner = $2
	`, id, owner)
	if err != nil {
		return false, fmt.Errorf("revoke agent token: %w", err)
	}
	if result.RowsAffected() == 0 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("revoke agent token: %w", err)
	}
	return true, nil
}

func (s *server) personalTokenActor(ctx context.Context, plaintext string) (model.Actor, error) {
	hash := sha256.Sum256([]byte(plaintext))
	var id, name, owner string
	var scopes []string
	var revokedAt *time.Time
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select id::text, name, owner, scopes, revoked_at
		from agent_tokens
		where token_hash = $1
	`, hash[:]).Scan(&id, &name, &owner, &scopes, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Actor{}, nil
	}
	if err != nil {
		return model.Actor{}, fmt.Errorf("look up agent token: %w", err)
	}
	if revokedAt != nil {
		return model.Actor{}, errorf(apicode.TokenRevoked, "agent token has been revoked")
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
	// The token's own row says what it may do, and it names the credential that acted: the
	// person's token, not the person at a browser.
	return model.Actor{
		Kind:       "session",
		Owner:      &owner,
		Credential: &model.Credential{Scopes: scopes, TokenID: id, TokenName: name},
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

func scanAgentToken(row rowScanner) (model.AgentToken, error) {
	var token model.AgentToken
	if err := row.Scan(&token.ID, &token.Name, &token.Prefix, &token.Scopes, &token.CreatedAt, &token.LastUsedAt, &token.RevokedAt); err != nil {
		return model.AgentToken{}, fmt.Errorf("scan agent token: %w", err)
	}
	return token, nil
}
