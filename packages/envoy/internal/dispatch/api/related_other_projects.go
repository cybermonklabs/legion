package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// relatedOtherProjectsSQL reads the answered questions of the other projects for a related-item
// check whose own write is a question (relatedInput.question). It is one statement of its own,
// not an arm of the check's fused statement, and it searches by meaning alone. The keyword leg
// weighs each word against the document counts of one project, which search_term_stats holds per
// project, so a word weighed against no corpus ranks nothing; a cosine needs no such counts.
//
// It reads the windows of embeddings of kind ask_part and joins asks as the same-project ask arm
// joins them (meaning_ask_candidates in search.go), then issues and artifacts for the owner. A
// row qualifies when the ask is a question, somebody answered it, the answer carries text, and
// the ask belongs to another project than the one that asks. The project test has two parts, and
// the `is not null` part is necessary: `null <> $2` is unknown, so an ask that belongs to no
// project would drop out of the answer and nothing would say so.
//
// Those tests sit inside the candidate CTE, before its LIMIT. The transaction that runs the
// statement sets hnsw.iterative_scan = relaxed_order (searchMeaningScan), which keeps the index
// scan going until the LIMIT is met or hnsw.max_scan_tuples (20,000) rows are read. An arm with
// narrow filters rarely meets its LIMIT, so that budget is what bounds the cost. Because the
// tests sit before the LIMIT, the scan never hands back a row the check must not offer, and such
// a row never takes the place of a qualifying one.
//
// The candidate CTE takes searchWindowCandidates windows, as the same-project ask arm does: one
// ask holds more than one window. The rows at or above searchMeaningFloor are then folded to the
// best window of each ask (distinct on), ordered by the score and cut to the depth every other
// leg is read to (contracts.SearchKindDepth).
//
// Parameters: $1 the question's embedding (a vector literal), $2 the project that asks, $3 the
// embedder's identity (embed.Embedder.Identity), $4 searchMeaningFloor, $5
// contracts.SearchKindDepth.
//
// Columns: the owner (an issue's key, title and status, or an owning project document's id, slug
// and name), the project of the row, the document the ask stands on (its slug, its name and
// whether it is the issue's primary document, which the href reads), the ask's id, the ask's
// typed block while that block is a node of the reference graph (askBlockNodeSQL), the ask's
// state, its answer and its cosine score.
var relatedOtherProjectsSQL = `with candidates as (
  select k.id, k.issue_key, k.state, k.answer,
         case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) end as owner_artifact_id,
         coalesce(k.artifact_id, k.block_artifact_id, ` + store.CastableUUID(`k.anchor->>'artifact_id'`) + `) as artifact_id,
         ` + askBlockNodeSQL + ` as block_id,
         i.title as issue_title, i.status as issue_status,
         coalesce(i.project_key, a.project_key) as project,
         a.slug as owner_slug, a.name as owner_name,
         e.id as window_id,
         1 - (e.embedding <=> $1::vector) as r
    from embeddings e
    join asks k on k.id = ` + windowItemUUID + `
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $3
     and k.kind = 'question' and k.state = 'answered'
     and btrim(coalesce(k.answer->>'text', '')) <> ''
     and coalesce(i.project_key, a.project_key) is not null
     and coalesce(i.project_key, a.project_key) <> $2
   order by e.embedding <=> $1::vector
   ` + searchWindowLimit + `
),
best as (
  select distinct on (candidates.id) * from candidates
   where r >= $4
   order by candidates.id, r desc, window_id
)
select b.issue_key, b.issue_title, b.issue_status, b.owner_artifact_id::text, b.owner_slug, b.owner_name,
       b.project, ar.slug, ar.name, coalesce(ar.is_primary, false), b.id::text, b.block_id,
       b.state, b.answer, b.r
  from best b
  left join artifacts ar on ar.id = b.artifact_id
 order by b.r desc, b.id
 limit $5
`

// relatedOtherProjects reads the answered questions of the other projects that stand closest in
// meaning to the text of a check whose write is a question. vector is the embedding the check has
// already made of that text, project is the project that asks, and identity is the embedder's own
// identity (embed.Embedder.Identity), which keeps the rows to the vectors of the one model: a
// vector another model wrote lives in another space, and its cosine distance to this one means
// nothing.
//
// It runs in a read transaction of its own (beginMeaningScan, rolled back at the end, since it
// writes nothing), so it reads the index on the settings every other meaning query reads it on:
// searchMeaningScan says what each setting is for, and this statement needs
// hnsw.iterative_scan most of all, because its filters are narrow.
//
// Each row becomes one model.SearchResult of kind "ask". Owner.Project carries the project of the
// row for every row, an issue's as well as a project document's, so that a reader of the item
// knows which project answered the question; the shared scan (scanSearchRows) fills it for a
// document owner alone, and the items of the asking project keep that shape. The row carries its
// own state and answer, so this leg reads no ask state a second time (attachAskStates).
//
// reader is the display tree reader each answer's text is registered with, which is the leg's
// own and never the request's: this runs on a goroutine the stage may abandon at its grace, and
// the goroutine that made the reader resolves it before it hands these results over
// (relatedMeaningStage). It is nil where the caller asked for no tree, which leaves every
// answer's text_tree out, as a nil reader does on every other route.
func (s *server) relatedOtherProjects(
	ctx context.Context, vector []float32, project, identity string, reader *displayTreeReader,
) ([]model.SearchResult, error) {
	// The test seam: a case holds this leg past its grace, or makes it fail, here (server
	// beforeCrossProjectRead). Nothing but a test sets it.
	if s.beforeCrossProjectRead != nil {
		if err := s.beforeCrossProjectRead(ctx); err != nil {
			return nil, err
		}
	}
	tx, err := s.beginMeaningScan(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, relatedOtherProjectsSQL,
		embed.Literal(vector), project, identity, searchMeaningFloor, contracts.SearchKindDepth)
	if err != nil {
		return nil, fmt.Errorf("search the answered questions of other projects: %w", err)
	}
	defer rows.Close()
	results := []model.SearchResult{}
	for rows.Next() {
		var issueKey, issueTitle, issueStatus, ownerArtifactID, ownerSlug, ownerName *string
		var slug, name, blockID *string
		var rowProject, id, state string
		var primary bool
		var answer []byte
		var rank float64
		if err := rows.Scan(
			&issueKey, &issueTitle, &issueStatus, &ownerArtifactID, &ownerSlug, &ownerName,
			&rowProject, &slug, &name, &primary, &id, &blockID, &state, &answer, &rank,
		); err != nil {
			return nil, fmt.Errorf("scan an answered question of another project: %w", err)
		}
		result := model.SearchResult{Kind: "ask", ID: id, Rank: rank}
		if issueKey != nil {
			result.Owner = model.SearchOwner{
				Kind:    "issue",
				Key:     *issueKey,
				Title:   deref(issueTitle),
				Status:  deref(issueStatus),
				Project: rowProject,
			}
		} else {
			result.Owner = model.SearchOwner{
				Kind:       "document",
				Project:    rowProject,
				Slug:       deref(ownerSlug),
				ArtifactID: deref(ownerArtifactID),
				Name:       deref(ownerName),
			}
		}
		if slug != nil {
			result.Artifact = &model.SearchArtifact{Slug: *slug, Name: deref(name)}
		}
		result.Ask = &model.SearchAsk{State: state}
		if len(answer) > 0 {
			var value model.AskAnswer
			if err := json.Unmarshal(answer, &value); err != nil {
				return nil, fmt.Errorf("decode the answer of an ask of another project: %w", err)
			}
			// The answer carries the shape a read gives it, with the display tree of its text
			// where the request asked for the trees and without one where it did not. Which of
			// them it is does not follow from the path. This leg runs for a write that asks a
			// person a question - an ask create, a new decision block - and for the POST form
			// of the related route, which writes nothing and is the read `dispatch related`
			// makes; the GET form of that route, which the dashboard reads under
			// `Dispatch-Trees: 1`, runs the one-project leg of the same check. So the header
			// the request carries decides the tree, on either leg (relatedInput.trees), and the
			// one-project items register theirs the same way (fillRelatedItems).
			result.Ask.Answer = newAskAnswerWithTree(&value, reader)
		}
		// The href of an ask, as the same-project ask rows build it. The query text is empty
		// because an ask's href holds none (searchHref writes it for a document alone), and the
		// matched block is nil because this leg ranks no block of the window it found: it offers
		// the ask, and a reader of the href lands on the ask or on its typed block.
		result.Href = searchHref(result.Kind, result.Owner, result.Artifact, primary, result.ID, blockID, nil, "")
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the answered questions of other projects: %w", err)
	}
	return results, nil
}
