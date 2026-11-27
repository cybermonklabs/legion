package api

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// attachAskStates sets Ask, in place, on every ask hit of a search page: the ask's state, its
// answer and its resolution, read from its row through docs.ScanAsk, and Author, the ask's asker,
// from the same row. A resolved ask's answer is the last one its ask.answered events recorded
// (loadAskAnswersByAsk), since a row that document settlement retracted while it was answered
// keeps its answer nowhere else; those events are read only when the page holds a resolved ask.
// One query reads every ask hit's row, and a page with no ask hit reads nothing. An ask removed
// after the search ran has no row left and keeps Ask and Author nil, as does one who may not
// read (access.ItemOwnerClause).
func (s *server) attachAskStates(ctx context.Context, q queryer, who model.Actor, results []model.SearchResult, reader *displayTreeReader) error {
	indexByID := map[string]int{}
	var askIDs []string
	for index := range results {
		if results[index].Kind == "ask" {
			indexByID[results[index].ID] = index
			askIDs = append(askIDs, results[index].ID)
		}
	}
	if len(askIDs) == 0 {
		return nil
	}

	rows, err := q.Query(ctx, `select `+docs.AskColumns+` from asks a where a.id = any($1::uuid[]) and `+
		access.ItemOwnerClause("a.issue_key", "a.artifact_id", "$2"), askIDs, refs.VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load search ask states: %w", err)
	}
	defer rows.Close()
	var resolvedIDs []string
	for rows.Next() {
		ask, err := docs.ScanAsk(rows)
		if err != nil {
			return fmt.Errorf("scan search ask state: %w", err)
		}
		if ask.State == "resolved" {
			resolvedIDs = append(resolvedIDs, ask.ID)
		}
		result := &results[indexByID[ask.ID]]
		result.Ask = &model.SearchAsk{
			State:      ask.State,
			Answer:     newAskAnswerWithTree(ask.Answer, reader),
			Resolution: ask.Resolution,
		}
		result.Author = &ask.Author
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load search ask states: %w", err)
	}

	if len(resolvedIDs) > 0 {
		answers, err := s.loadAskAnswersByAsk(ctx, q, who, resolvedIDs)
		if err != nil {
			return fmt.Errorf("load search ask answers: %w", err)
		}
		for _, id := range resolvedIDs {
			if history := answers[id]; len(history) > 0 {
				results[indexByID[id]].Ask.Answer = newAskAnswerWithTree(&history[len(history)-1], reader)
			}
		}
	}
	return nil
}

// attachItemAuthors sets Author, in place, on every comment hit and message hit of a search page:
// who wrote the item, read from its row, so a reader of the hit alone can say who said it. One
// query reads every such hit's row, and a page with neither kind reads nothing. An item removed
// after the search ran has no row left and keeps Author nil. An ask hit's author is read with its
// state (attachAskStates).
func attachItemAuthors(ctx context.Context, q queryer, results []model.SearchResult) error {
	indexByItem := map[[2]string]int{}
	var commentIDs, messageIDs []string
	for index, result := range results {
		switch result.Kind {
		case "comment":
			commentIDs = append(commentIDs, result.ID)
		case "message":
			messageIDs = append(messageIDs, result.ID)
		default:
			continue
		}
		indexByItem[[2]string{result.Kind, result.ID}] = index
	}
	if len(indexByItem) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `
		select 'comment', id::text, author from comments where id = any($1::uuid[])
		union all
		select 'message', id::text, author from messages where id = any($2::uuid[])
	`, commentIDs, messageIDs)
	if err != nil {
		return fmt.Errorf("load search hit authors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		var author model.Actor
		if err := rows.Scan(&kind, &id, &author); err != nil {
			return fmt.Errorf("scan search hit author: %w", err)
		}
		results[indexByItem[[2]string{kind, id}]].Author = &author
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load search hit authors: %w", err)
	}
	return nil
}
