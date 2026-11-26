package main

// The served-answer check of replay-check-seed: the answer of each answered or resolved typed ask
// block of a logged document, compared as this build serves it, off the ask's node, with the
// answer production logged for its ask (checkSeed).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// steppedBlockAsks are the asks of is whose typed ask block is in the logged document a and which
// production answered or resolved, in the order of is: the asks of the steps of a's history
// (blockSteps), each once.
func steppedBlockAsks(is *replayIssue, a *replayArtifact) []*replayAsk {
	steps, _ := blockSteps(is, a)
	var stepped []*replayAsk
	for _, st := range steps {
		// An ask's answer and its resolve are two steps in a row.
		if ask := &is.Asks[st.Ask]; len(stepped) == 0 || stepped[len(stepped)-1] != ask {
			stepped = append(stepped, ask)
		}
	}
	return stepped
}

// servedAnswer is an ask's state and answer as this build serves them (servedAnswersQuery).
type servedAnswer struct {
	state, user string
	selected    []string
	text        *string
}

// digest is answerDigest of the served answer.
func (s servedAnswer) digest() string { return answerDigest(s.state, s.user, s.selected, s.text) }

// answerDigest is answerDigest of the answer production logged for the ask.
func (a *replayAsk) answerDigest() string {
	if a.Answer == nil {
		return answerDigest(a.State, "", nil, nil)
	}
	return answerDigest(a.State, a.Answer.User, a.Answer.Selected, a.Answer.Text)
}

// answerDigest names an answer state by the ask's state and, for an answered ask, the canonical
// login of who answered, how many options the answer selects and a digest of its selection and
// its words, never the words themselves. Every read of an ask shows an answer only on an ask that
// is answered, so a resolved ask is named by its state alone.
func answerDigest(state, user string, selected []string, text *string) string {
	if state != "answered" {
		return state
	}
	words := strings.Join(selected, "\n") + "\x00"
	if text != nil {
		words += "\x01" + *text
	}
	return fmt.Sprintf("answered by %s, %d selected, %s", model.CanonicalLogin(user), len(selected), textDigest(words))
}

// servedAnswersQuery reads the ask of each typed ask block of the document $1, with its state and
// its answer as its node holds them, as every read of an ask shows them (api's
// askNodeAnswerColumn): each value and relation of its answer that stands on the node where the
// row names one - the newest tuple of each, while it asserts - and open where the node holds no
// state; the row's own state and answer where the row names no node.
var servedAnswersQuery = `
select a.block_id,
       case when a.node_kind is null then a.state else coalesce(f.state, 'open') end,
       coalesce(case when a.node_kind is null then a.answer->>'user' else f.answered_by end, ''),
       case when a.node_kind is null then a.answer->'selected' else f.selected::jsonb end,
       case when a.node_kind is null then a.answer->>'text' else f.answer end
  from public.asks a
  cross join lateral (
    select max(n.to_id) filter (where n.kind = '` + graphstore.TupleAnswerState + `') as state,
           max(n.to_id) filter (where n.kind = '` + graphstore.TupleAnswer + `') as answer,
           max(n.to_id) filter (where n.kind = '` + graphstore.TupleSelected + `') as selected,
           max(n.to_id) filter (where n.kind = '` + graphstore.TupleAnsweredBy + `') as answered_by
      from (
        select distinct on (t.kind, t.to_kind, case when t.to_kind = '` + graphstore.KindValue + `' then '' else t.to_id end)
               t.kind, t.to_id, t.asserted
          from public.graph_tuples t
         where t.from_kind = '` + graphstore.KindAsk + `' and t.from_id = a.id::text
           and t.kind in ('` + graphstore.TupleAnswerState + `', '` + graphstore.TupleAnswer + `', '` +
	graphstore.TupleSelected + `', '` + graphstore.TupleAnsweredBy + `')
         order by t.kind, t.to_kind, case when t.to_kind = '` + graphstore.KindValue + `' then '' else t.to_id end, t.seq desc
      ) n
     where n.asserted
  ) f
 where a.block_artifact_id = $1 and a.block_id is not null`

// servedAnswers is the served answer of the ask of each typed ask block of the local document
// local, by block id (servedAnswersQuery).
func servedAnswers(ctx context.Context, conn *pgx.Conn, local string) (map[string]servedAnswer, error) {
	rows, err := conn.Query(ctx, servedAnswersQuery, local)
	if err != nil {
		return nil, fmt.Errorf("read the answers of document %s: %w", local, err)
	}
	out := map[string]servedAnswer{}
	var block, state, user string
	var text *string
	var selected []byte
	_, err = pgx.ForEachRow(rows, []any{&block, &state, &user, &selected, &text}, func() error {
		answer := servedAnswer{state: state, user: user, text: text}
		if len(selected) > 0 {
			if err := json.Unmarshal(selected, &answer.selected); err != nil {
				return fmt.Errorf("read the selection of block %s of document %s: %w", block, local, err)
			}
		}
		out[block] = answer
		return nil
	})
	return out, err
}
