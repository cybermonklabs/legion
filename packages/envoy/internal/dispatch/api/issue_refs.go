package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// issueRefResolved is the body GET /api/v1/issues/resolve answers.
type issueRefResolved struct {
	Key string `json:"key"`
}

func (s *server) resolveIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key, err := s.resolveIssueRef(r.Context(), r.URL.Query().Get("ref"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, issueRefResolved{Key: key})
}

// resolveIssueRef resolves an existing native key or external GitHub link.
// Creating a missing external issue is the responsibility of POST /issues.
func (s *server) resolveIssueRef(ctx context.Context, ref string) (string, error) {
	keys, err := s.issueRefKeys(ctx, ref)
	if err != nil {
		return "", err
	}
	switch len(keys) {
	case 0:
		return "", pgx.ErrNoRows
	case 1:
		return keys[0], nil
	default:
		return "", errorf(
			apicode.AmbiguousIssueRef,
			"external issue reference %q is linked to multiple Dispatch issues: %s",
			strings.TrimSpace(ref),
			strings.Join(keys, ", "),
		)
	}
}

// issueRefKeys is every issue an issue reference names, in key order: the issue a native key
// names, and each issue that links the GitHub issue or pull request an owner/repo#n names. A
// reference that names none answers none, and one that is neither is 400 INVALID_ISSUE_REF. GET
// /api/v1/issues/resolve answers the one key it names (resolveIssueRef), and GET
// /api/v2/issues?ref= lists each of them.
func (s *server) issueRefKeys(ctx context.Context, ref string) ([]string, error) {
	ref = strings.TrimSpace(ref)
	if issueKeyPattern.MatchString(ref) {
		var key string
		err := s.deps.Store.Pool.QueryRow(ctx, `select key from issues where key = $1`, ref).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return []string{}, nil
		}
		if err != nil {
			return nil, err
		}
		return []string{key}, nil
	}
	repo, number, err := externalRef(ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select distinct issue_key from issue_external_links where url = any($1) order by issue_key
	`, externalURLs(repo, number))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

func validateExternalURL(raw string) error {
	value, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || !value.IsAbs() || value.Host == "" || (value.Scheme != "http" && value.Scheme != "https") {
		return errorf(apicode.InvalidURL, "external link URL must be an absolute HTTP(S) URL")
	}
	return nil
}

// externalRef reads ref, an `owner/name#number` reference, by the one rule (refs.ParseExternalRef),
// and refuses one of another shape 400 INVALID_ISSUE_REF.
func externalRef(ref string) (repo string, number string, err error) {
	repo, number, ok := refs.ParseExternalRef(ref)
	if !ok {
		return "", "", errorf(apicode.InvalidIssueRef, "invalid issue reference %q", ref)
	}
	return repo, number, nil
}

func externalURL(repo, number string) string {
	return "https://github.com/" + repo + "/issues/" + number
}

func externalURLs(repo, number string) []string {
	return []string{
		externalURL(repo, number),
		"https://github.com/" + repo + "/pull/" + number,
	}
}
