package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// issueRefResolved is the body GET /api/v1/issues/resolve answers.
type issueRefResolved struct {
	Key string `json:"key"`
}

func (s *server) resolveIssue(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	key, err := s.resolveIssueRef(r.Context(), who, r.URL.Query().Get("ref"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, issueRefResolved{Key: key})
}

// resolveIssueRef resolves an existing native key or external GitHub link, as who reads it: an
// issue of a project who does not see resolves as one nobody holds.
// Creating a missing external issue is the responsibility of POST /issues.
//
// An external reference finds a link stored under either name of its repository's rename pair
// (refs.Renames.URLSpellings), so `sjawhar/legion#1955` and `cybermonklabs/legion#1955` reach
// the same issue, whichever name its link was stored under.
func (s *server) resolveIssueRef(ctx context.Context, who model.Actor, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	viewer := refs.VisibilityArg(who)
	if issueKeyPattern.MatchString(ref) {
		var key string
		err := s.deps.Store.Pool.QueryRow(ctx, `select key from issues where key = $1 and `+
			access.ProjectClause("project_key", "$2"), ref, viewer).Scan(&key)
		if err != nil {
			return "", err
		}
		return key, nil
	}
	repo, number, err := externalRef(ref)
	if err != nil {
		return "", err
	}
	var urls []string
	for _, link := range externalURLs(repo, number) {
		urls = append(urls, s.deps.Renames.URLSpellings(link)...)
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select distinct l.issue_key from issue_external_links l join issues i on i.key = l.issue_key
		where l.url = any($1) and `+access.ProjectClause("i.project_key", "$2")+`
		order by l.issue_key
	`, urls, viewer)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return "", err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(keys) {
	case 0:
		return "", pgx.ErrNoRows
	case 1:
		return keys[0], nil
	default:
		return "", errorf(
			http.StatusBadRequest,
			"AMBIGUOUS_ISSUE_REF",
			"external issue reference %q is linked to multiple Dispatch issues: %s",
			ref,
			strings.Join(keys, ", "),
		)
	}
}

func validateExternalURL(raw string) error {
	value, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || !value.IsAbs() || value.Host == "" || (value.Scheme != "http" && value.Scheme != "https") {
		return errorf(http.StatusBadRequest, "INVALID_URL", "external link URL must be an absolute HTTP(S) URL")
	}
	return nil
}

// externalRef reads ref, an `owner/name#number` reference, by the one rule (refs.ParseExternalRef),
// and refuses one of another shape 400 INVALID_ISSUE_REF.
func externalRef(ref string) (repo string, number string, err error) {
	repo, number, ok := refs.ParseExternalRef(ref)
	if !ok {
		return "", "", errorf(http.StatusBadRequest, "INVALID_ISSUE_REF", "invalid issue reference %q", ref)
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
