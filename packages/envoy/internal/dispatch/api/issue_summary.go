package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// The summary reads serve what a rendered dispatch:// reference shows, an issue's title and status
// and a document's name, kind, latest version and that version's size and stored type, at a cost a
// page linking a hundred items can pay on every event that touches them: the full reads
// (GET /api/v1/issues/{key}, GET /api/v1/projects/{key}/artifacts/{slug}) read every version,
// approval, child and open ask, and an issue's route roster. They are open to every signed-in
// caller, as those reads are.

// issueSummary is GET /api/v1/issues/{key}/summary: the issue's key, project, title and status,
// and its documents and files in the order the full read lists its artifacts, so an issue link and
// its documents' links share one read.
type issueSummary struct {
	Key       string                 `json:"key"`
	Project   string                 `json:"project"`
	Title     string                 `json:"title"`
	Status    string                 `json:"status"`
	Documents []issueSummaryDocument `json:"documents"`
}

// issueSummaryDocument is one artifact of an issue summary. LatestVersion is its greatest version
// number, null only for an artifact with no version, which no committed write leaves. Size is that
// version's size in bytes and Mime the type it is stored under, both null when the artifact has no
// version. Primary marks the issue's spec, which a reference to a block of the spec
// (`<issue>/spec#b-…`) names.
type issueSummaryDocument struct {
	ID            string  `json:"id"`
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	LatestVersion *int    `json:"latest_version"`
	Size          *int64  `json:"size"`
	Mime          *string `json:"mime"`
	Primary       bool    `json:"primary"`
}

// documentSummary is GET /api/v1/projects/{key}/artifacts/{slug}/summary: the project document
// the full read resolves that reference to, with its greatest version number, that version's
// size in bytes and the type it is stored under.
type documentSummary struct {
	ID            string  `json:"id"`
	Project       string  `json:"project"`
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	LatestVersion *int    `json:"latest_version"`
	Size          *int64  `json:"size"`
	Mime          *string `json:"mime"`
}

// summaryLatest is the join an artifact summary reads its latest version through, over artifacts
// aliased a: one backwards walk of artifact_versions' (artifact_id, number) key per artifact,
// inside the summary's one query, rather than one probe for the number and a second for that
// version's size and type.
const summaryLatest = `left join lateral (
			select v.number, v.size, v.mime from artifact_versions v
			where v.artifact_id = a.id order by v.number desc limit 1
		) latest on true`

// summaryLatestColumns is what that join gives the select list: the artifact's greatest version
// number, which is its newest row's, that version's size in bytes and the type it is stored
// under. All three are null for an artifact with no version, and the size and the type are null
// for a document version, which records neither. A reader that must hold a set of files under a
// byte cap reads the sizes here and refuses before it asks for any bytes; a reader that must say
// what a file is reads the type here, because the download answers a type a browser draws as a
// document (text/html, application/xhtml+xml, image/svg+xml, text/xml, application/xml) as
// application/octet-stream (downloadContentType), so no answer of that route is the file's type.
const summaryLatestColumns = `latest.number, latest.size, latest.mime`

func (s *server) getIssueSummary(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ctx := r.Context()
	var summary issueSummary
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select key, project_key, title, status from issues where key = $1
	`, r.PathValue("key")).Scan(&summary.Key, &summary.Project, &summary.Title, &summary.Status); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select a.id::text, a.slug, a.name, a.kind, `+summaryLatestColumns+`, a.is_primary
		from artifacts a `+summaryLatest+`
		where a.issue_key = $1 order by a.created_at, a.id
	`, summary.Key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	summary.Documents, err = pgx.CollectRows(rows, pgx.RowToStructByPos[issueSummaryDocument])
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, summary)
}

func (s *server) getProjectDocumentSummary(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ctx := r.Context()
	q := s.deps.Store.Pool
	read := func(predicate string, value string) (documentSummary, error) {
		var summary documentSummary
		err := q.QueryRow(ctx, `
			select a.id::text, coalesce(a.project_key, ''), a.slug, a.name, a.kind, `+summaryLatestColumns+`
			from artifacts a `+summaryLatest+`
			where `+predicate, value).Scan(
			&summary.ID, &summary.Project, &summary.Slug, &summary.Name, &summary.Kind, &summary.LatestVersion, &summary.Size, &summary.Mime,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return documentSummary{}, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
		}
		return summary, err
	}
	summary, err := resolveDocumentReference(ctx, s, q, artifactTarget{Project: r.PathValue("key")}, r.PathValue("slug"),
		func(refKey string) (documentSummary, error) { return read(`a.ref_key = $1`, refKey) },
		func(id string) (documentSummary, error) { return read(`a.id = $1::uuid`, id) },
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, summary)
}
