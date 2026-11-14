package appauth

// The token exchange: POST /app/installations/{id}/access_tokens, the one call that mints a
// lease in key mode. Every request carries a body naming the permissions the token holds and no
// repository at all, and the answer is held against those permissions before a lease is served
// (narrowedTo). What a token reaches is the App installation's own business: the account that
// installs the App chooses the repositories, and no body the daemon sends narrows them further
// (INFRA-2543).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// accessTokensBody is GitHub's POST /app/installations/{id}/access_tokens body: the permissions
// the token holds, which every mint names, and nothing else. GitHub reads a body naming no
// repository as the whole installation, which is the reach the installation is there to decide.
type accessTokensBody struct {
	Permissions map[string]string `json:"permissions"`
}

// mintedToken is GitHub's answer to a token exchange: the token, its expiry, and the permissions
// GitHub says the token holds, which the mint holds against what it asked for (narrowedTo). The
// answer also lists the repositories the installation holds, which this daemon neither asks
// about nor reads: the installation decides them, and a list of them is nothing to hold an
// answer to.
type mintedToken struct {
	Token       string            `json:"token"`
	ExpiresAt   string            `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

// narrowedTo reports that GitHub's answer describes a token holding no more than permissions
// does: no permission the request did not ask for, nor one above the access it asked at. GitHub
// applies the body and refuses what it cannot grant, so an answer past it means the body was
// dropped or widened between the daemon and GitHub, leaving a lease wider than the one asked
// for; it is refused rather than served, as the secrets broker refuses one its narrowing does
// not hold (heldBy). The refusal names what was asked for and the permission the answer named,
// never a value the answer carries that a request did not, which for this answer is the token.
//
// An answer naming no permission at all is refused too: GitHub names every permission a token
// holds, and the token it mints for a request whose permissions were dropped on the way holds
// every permission the installation is granted — the token at the installation's full strength
// that this package exists to stop — which no later read of the answer could tell from a
// narrowed one.
//
// An access the answer names that this daemon cannot order (accessOrder) is refused as well: it
// cannot be held to be no wider than the one asked for, and an answer naming it is no answer
// GitHub makes to a request that asked for a word of its own.
func (t mintedToken) narrowedTo(permissions map[string]string) error {
	if len(t.Permissions) == 0 {
		return fmt.Errorf("GitHub App token exchange returned a token wider than the permissions asked for (%s): it names no permission",
			askedFor(permissions))
	}
	for _, permission := range slices.Sorted(maps.Keys(t.Permissions)) {
		access := t.Permissions[permission]
		if rank := accessOrder[access]; rank == 0 || rank > accessOrder[permissions[permission]] {
			return fmt.Errorf("GitHub App token exchange returned a token wider than the permissions asked for (%s): it holds %s: %s",
				askedFor(permissions), permission, access)
		}
	}
	return nil
}

// askedFor is what a token request carried, for a refusal to say what GitHub turned down: its
// permissions, named in a fixed order so one refusal reads like the next. A request body carries
// no token, so neither does this.
func askedFor(permissions map[string]string) string {
	named := make([]string, 0, len(permissions))
	for _, permission := range slices.Sorted(maps.Keys(permissions)) {
		named = append(named, permission+": "+permissions[permission])
	}
	return "permissions " + strings.Join(named, ", ")
}

// exchange asks GitHub for installation's token with permissions, which the access_tokens body
// names alone: the token reaches every repository the installation holds, and the account that
// installs the App is what decides which those are. GitHub reads a request naming no permission
// as the whole installation at every permission it is granted, which is a token no caller here
// asks for, so a request naming permissions is no proof of a token held to them: what GitHub
// says it minted is held against what was asked (narrowedTo) and a wider lease is refused rather
// than served.
func (m *Manager) exchange(ctx context.Context, jwt, installation string, permissions map[string]string) (Lease, error) {
	encoded, err := json.Marshal(accessTokensBody{Permissions: permissions})
	if err != nil {
		return Lease{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/app/installations/"+url.PathEscape(installation)+"/access_tokens", bytes.NewReader(encoded))
	if err != nil {
		return Lease{}, err
	}
	m.appHeaders(request, jwt)
	request.Header.Set("Accept", "application/vnd.github+v3+json")
	request.Header.Set("Content-Type", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return Lease{}, &TransientError{Err: fmt.Errorf("GitHub App token exchange: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body := refusalBody(response)
		failed := fmt.Errorf("GitHub App token exchange failed (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
		if response.StatusCode == http.StatusNotFound {
			return Lease{}, fmt.Errorf("%w: %w", errInstallationGone, failed)
		}
		refused := transient(response, body, failed)
		if response.StatusCode == http.StatusUnprocessableEntity {
			// GitHub documents its rate limits on a 429 and a 403 (githubrest.RateLimited), but
			// the headers or the secondary-limit text of a limit on a 422 are that limit too,
			// and no verdict on what the request asked for: the same request may pass later, so
			// it is GitHub's trouble and never the installation's permissions.
			if githubrest.RateLimitSignals(response, body) {
				return Lease{}, &TransientError{Err: failed}
			}
			// Every other 422 is the permissions this request carried: the installation being
			// granted less than the request asked for of one of them — which a mint asks for as
			// the installation was granted it a moment before (exchangeGranted), so it is a
			// grant that changed between the two calls. GitHub's text is never quoted as a
			// cause, so the refusal names what was asked for and an operator reads what it was.
			return Lease{}, fmt.Errorf("%w (%s): %w", ErrNarrowingRefused, askedFor(permissions), refused)
		}
		return Lease{}, refused
	}
	body, err := readBody(response, "GitHub App token exchange", exchangeAnswerBound)
	if err != nil {
		return Lease{}, err
	}
	var payload mintedToken
	if err := json.Unmarshal(body, &payload); err != nil {
		return Lease{}, fmt.Errorf("GitHub App token exchange returned an invalid response")
	}
	expiresAt, err := time.Parse(time.RFC3339, payload.ExpiresAt)
	if err != nil || payload.Token == "" {
		return Lease{}, fmt.Errorf("GitHub App token exchange returned an invalid response")
	}
	if err := payload.narrowedTo(permissions); err != nil {
		return Lease{}, err
	}
	return Lease{Token: payload.Token, ExpiresAt: expiresAt, Minted: m.now()}, nil
}
