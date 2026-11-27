package main

// The served-answer check of replay-check-seed: the answer of each answered or resolved typed ask
// block of a logged document, compared as this build serves it, off the ask's node, with the
// answer production logged for its ask (checkSeed).

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
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

// servedAnswersQuery reads the ask of each typed ask block of the document $1, with its row's state
// and answer and its node's answer, which servedAnswers lays over the row as every read of an ask
// shows them (asks.ReadAnswer): the node's state and answer where the row names a node, open where
// the node holds no state, and the row's own state and answer where the row names no node. The
// node's answer is read from public's table (asks.NodeAnswerColumnIn), since every statement of
// this package names the schema of each Dispatch table it reads, as a schema named as the
// database's login shadows public for every unqualified name (dispatch://INFRA-988).
var servedAnswersQuery = `
select a.block_id, a.state, a.answer, ` + asks.NodeAnswerColumnIn(`public.graph_tuples`, `a`) + `
  from public.asks a
 where a.block_artifact_id = $1 and a.block_id is not null`

// servedAnswers is the served answer of the ask of each typed ask block of the local document
// local, by block id (servedAnswersQuery).
func servedAnswers(ctx context.Context, conn *pgx.Conn, local string) (map[string]servedAnswer, error) {
	rows, err := conn.Query(ctx, servedAnswersQuery, local)
	if err != nil {
		return nil, fmt.Errorf("read the answers of document %s: %w", local, err)
	}
	out := map[string]servedAnswer{}
	var block, state string
	var answer, nodeAnswer []byte
	_, err = pgx.ForEachRow(rows, []any{&block, &state, &answer, &nodeAnswer}, func() error {
		ask, err := asks.ReadAnswer(block, state, answer, nodeAnswer)
		if err != nil {
			return fmt.Errorf("read the answer of block %s of document %s: %w", block, local, err)
		}
		served := servedAnswer{state: ask.State}
		if ask.Answer != nil {
			served.user, served.selected, served.text = ask.Answer.User, ask.Answer.Selected, ask.Answer.Text
		}
		out[block] = served
		return nil
	})
	return out, err
}
