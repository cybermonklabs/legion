package api

import "fmt"

// This file holds the related-item check's own SQL: the four statements it runs
// (relatedKeywordQuery and relatedKeywordExcludingQuery, relatedMeaningQuery and
// relatedMeaningExcludingQuery, paired into relatedKeywordStatement and
// relatedMeaningStatement) and the three fragments they are built from
// (relatedCoordinationRank, relatedTermWeights, relatedFusedSQL). They sit here together,
// apart from the Go of the check that runs them (suggestions.go), so that each can be read,
// measured and compared with search's own statements (search.go) on its own.

var (
	// relatedKeywordLegs is search's keyword legs (searchKeywordLegs) ranked by the chosen
	// lexemes' weights (relatedCoordinationRank).
	relatedKeywordLegs = searchKeywordLegs(relatedCoordinationRank)
	// relatedKeywordQuery is the check's stage 1: search's keyword query (searchQueryBody) over the
	// chosen lexemes, with relatedKeywordLegs and relatedFusedSQL. Parameters: searchQueryBody's
	// $1-$8, then $9 the lexemes and $10 their idf.
	relatedKeywordQuery = relatedQOpener + relatedTermWeights(9) + searchQueryBody(relatedKeywordLegs, relatedFusedSQL, "")
	// relatedKeywordExcludingQuery is relatedKeywordQuery with the exclusion filter over one more
	// parameter, $11 the owners the check leaves out (relatedExcludedOwners).
	relatedKeywordExcludingQuery = relatedQOpener + relatedTermWeights(9) +
		searchQueryBody(relatedKeywordLegs, relatedFusedSQL, relatedExcludedOwners(11))
	// relatedMeaningQuery is the check's stage 2: search's meaning query, which fuses the same
	// keyword legs with the meaning arms, over the same lexemes and the text's embedding.
	// Parameters: searchQueryMeaningBody's $1-$11, then $12 the lexemes and $13 their idf.
	relatedMeaningQuery = relatedQOpener + relatedTermWeights(12) +
		searchQueryMeaningBody(relatedKeywordLegs, relatedFusedSQL, "")
	// relatedMeaningExcludingQuery is relatedMeaningQuery with the same filter, here over $14.
	relatedMeaningExcludingQuery = relatedQOpener + relatedTermWeights(12) +
		searchQueryMeaningBody(relatedKeywordLegs, relatedFusedSQL, relatedExcludedOwners(14))
	// relatedKeywordStatement and relatedMeaningStatement are what relatedSearch runs.
	relatedKeywordStatement = relatedStatement{plain: relatedKeywordQuery, excluding: relatedKeywordExcludingQuery}
	relatedMeaningStatement = relatedStatement{plain: relatedMeaningQuery, excluding: relatedMeaningExcludingQuery}
)

// relatedStatement is one of the check's two searches in both its shapes: the statement itself,
// and the same statement with the exclusion filter over one further parameter. A check that
// leaves nothing out runs the plain one, so a read with nothing to exclude never tests every
// candidate row the arms match against an empty list.
type relatedStatement struct {
	plain     string
	excluding string
}

// relatedCoordinationRank ranks a keyword arm's item, searchColumn its search vector, by
// idf-weighted coordination in place of search's ts_rank_cd: the sum of the idf of every chosen
// lexeme the item holds, so an item holding three of the text's rare words outranks one
// repeating one of them, which cover density favours. On a project's closed duplicates it listed
// more of the issues they were closed into than ts_rank_cd over the same terms or the old
// 40-word OR query (TestRelatedRecallOnDuplicates). ts_rank_cd, normalised into [0, 1) and scaled
// by a thousandth, only orders items holding the same lexemes. Each lexeme is tested with @@
// against a tsquery of its own (relatedTermWeights), a binary search of the item's sorted vector,
// not by unnesting the vector, which costs a document's whole length.
func relatedCoordinationRank(searchColumn string) string {
	return "((select sum(w.idf) from term_weights w where " + searchColumn + " @@ w.tq) + 0.001 * ts_rank_cd(" + searchColumn + ", q.tsq, 32))"
}

// relatedTermWeights is the term_weights CTE relatedCoordinationRank reads: each chosen lexeme
// as a tsquery of its own and its idf, from $lexemes the chosen lexemes and $lexemes+1 their idf,
// in one order.
func relatedTermWeights(lexemes int) string {
	return fmt.Sprintf(`term_weights as (
  select quote_literal(w.lexeme)::tsquery as tq, w.idf from unnest($%d::text[], $%d::float8[]) w(lexeme, idf)
),
`, lexemes, lexemes+1)
}

// relatedFusedSQL is the "fused" CTE the check's four statements pass the two shared bodies
// (searchQueryBody, searchQueryMeaningBody) in place of search's own searchFusedSQL: reciprocal
// rank fusion over pos, the position ranked already numbered each leg's rows by, so a row is
// worth 1 / (searchFusionK + pos) on the leg that found it and a row both legs found is worth
// the sum of its two places.
//
// The check cannot use search's weighted sum, because under this check the two legs' r are not
// on one scale. The keyword leg here is ranked by relatedCoordinationRank, a sum of the chosen
// lexemes' BM25 idf - about 2.3 for one matched lexeme of a small project and higher for more -
// while a meaning leg's r is a cosine similarity, at most 1 and so at most searchMeaningWeight
// (0.4) once weighted. searchMeaningWeight was fitted against search's own keyword rank,
// ts_rank_cd with the length normalisation that maps it onto [0, 1) (searchKeywordLegsSQL),
// never against a sum of idf. Summed that way, every keyword match outscores every meaning-only
// match by a factor of five or more, and the check's page is only suggestionSearchDepth rows
// deep: ten items each holding one chosen word fill it, and an item that means the same but
// shares no word reaches neither the related suggestions nor the related_checks outcome. Ranks
// have no such scales - a leg's first row is its first row whatever its r is worth - so the
// meaning leg's best row is fused level with the keyword leg's best row.
//
// Grouping by (kind, id) is right for the keyword-only body too, which has one list and so one
// row per item in legs: each group is that single row, and the group's score is that row's own
// 1 / (searchFusionK + pos), which is strictly decreasing in pos and so orders the page exactly
// as pos does.
//
// own_lead is carried although it is always false here: relatedQOpener binds own_keys as the
// empty array, since no issue key is the whole of a check's text, so no arm's own column is ever
// true. The page orders by it (searchPageAndTailSQL reads f.own_lead), and the bodies are shared
// with search, which does set it, so the column has to be here for those statements to run at
// all.
const relatedFusedSQL = `fused as (
  select kind, id, bool_or(own) as own_lead, sum(1.0 / ($8 + pos)) as score
    from legs group by kind, id
),
`
