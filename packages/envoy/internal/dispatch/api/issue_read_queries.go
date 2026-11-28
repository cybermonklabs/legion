package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// The issue read answers the issue's queries (R8 of the issue-tree spec): Spec, Verification, Plan
// and Decisions, each the artifact that reads it. On a converted issue that is the query node
// (graphstore.QueryNode), an artifact of the issue whose node carries the supertag `query` and the
// query's name; on an issue that has not converted it is the role document of that name - its
// spec, its Verification document and its plan, as the role predicates find them - and Decisions,
// which no document plays (R11 of the decisions-tree spec), answers only where a query node holds
// it. The issue read's verification_artifact_id and plan_artifact_id answer the same artifacts
// (issueQueryDocumentSQL), so a page and a client of the release before, which read the two
// fields, read what `queries` names.

// issueQuery is one query of an issue as GET /api/v1/issues/{key} answers it: the query's name, the
// artifact that reads it, the supertags it takes and the node its walk starts at. A query node
// answers the two values it holds; a role document of an issue that has not converted answers the
// ones a query node of its name is written with (graphstore.QueryTakes, and the issue's own node).
type issueQuery struct {
	Query      string   `json:"query" enum:"spec,verification,plan,decisions"`
	ArtifactID string   `json:"artifact_id"`
	Takes      []string `json:"takes"`
	Subtree    string   `json:"subtree"`
}

// issueQueryNames is the four queries of an issue, in the order the issue read answers them.
var issueQueryNames = []string{graphstore.QuerySpec, graphstore.QueryVerification, graphstore.QueryPlan, graphstore.QueryDecisions}

// issueQueryRoles is, for each query a role document reads on an issue that has not converted, the
// predicate over the artifacts table aliased `a` that finds that document: the spec is the
// primary document, as primary_artifact_id reads it, and the Verification document and the plan
// are the documents their role predicates find (documentRolePredicate).
var issueQueryRoles = map[string]string{
	graphstore.QuerySpec:         "a.is_primary",
	graphstore.QueryVerification: aliasedVerificationRole,
	graphstore.QueryPlan:         aliasedPlanRole,
}

// issueQueryDocumentSQL is the scalar subquery answering, as text, the id of the artifact that
// reads the query named query of the issue the SQL expression issue names, null where none does:
// the issue's query node of that name that is not retired, the oldest where two stand, else its
// role document of that name (issueQueryRoles), which also answers a role document a rolling
// deploy left on a converted issue with no query node over it. A query no document plays,
// Decisions, answers its query node alone.
func issueQueryDocumentSQL(issue, query string) string {
	node := `(select a.id::text from artifacts a
		join graph_nodes n on n.kind = '` + graphstore.KindArtifact + `' and n.id = a.ref_key and n.supertag = '` + graphstore.SupertagQuery + `'
		where a.issue_key = ` + issue + ` and a.retired_at is null and n.attributes->>'` + graphstore.AttributeQuery + `' = '` + query + `'
		order by a.created_at, a.id limit 1)`
	role, played := issueQueryRoles[query]
	if !played {
		return node
	}
	return `coalesce(` + node + `, (select a.id::text from artifacts a where a.issue_key = ` + issue + ` and ` + role + `))`
}

// issueQueriesStatement reads the queries of the issue $1 (issueQuery), in issueQueryNames' order,
// one row for each query an artifact reads (issueQueryDocumentSQL): the query's name, the
// artifact's id, and the takes and subtree values of its node where that node is a query node,
// null for a role document, which scanIssueQueries answers the values of its query's definition.
var issueQueriesStatement = func() string {
	arms := make([]string, len(issueQueryNames))
	for position, query := range issueQueryNames {
		arms[position] = `select ` + strconv.Itoa(position) + ` as position, '` + query + `' as query, ` +
			issueQueryDocumentSQL("$1", query) + ` as id`
	}
	return `
		select held.query, held.id, node.attributes->'` + graphstore.AttributeTakes + `', node.attributes->>'` + graphstore.AttributeSubtree + `'
		from (` + strings.Join(arms, "\n\t\tunion all ") + `) held
		left join artifacts a on a.id = held.id::uuid
		left join graph_nodes node on node.kind = '` + graphstore.KindArtifact + `' and node.id = a.ref_key and node.supertag = '` + graphstore.SupertagQuery + `'
		where held.id is not null
		order by held.position`
}()

// scanIssueQueries reads the rows issueQueriesStatement returns for the issue issueKey. An issue
// whose queries no artifact reads keeps a non-nil empty list, which the response carries as [].
func scanIssueQueries(rows pgx.Rows, issueKey string) ([]issueQuery, error) {
	queries := []issueQuery{}
	for rows.Next() {
		var query issueQuery
		var takes []byte
		var subtree *string
		if err := rows.Scan(&query.Query, &query.ArtifactID, &takes, &subtree); err != nil {
			return nil, fmt.Errorf("read the queries of %s: %w", issueKey, err)
		}
		query.Takes, query.Subtree = graphstore.QueryTakes(query.Query), graphstore.KindIssue+":"+issueKey
		if takes != nil {
			if err := json.Unmarshal(takes, &query.Takes); err != nil {
				return nil, fmt.Errorf("read the takes of the %s query of %s: %w", query.Query, issueKey, err)
			}
		}
		if subtree != nil {
			query.Subtree = *subtree
		}
		queries = append(queries, query)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the queries of %s: %w", issueKey, err)
	}
	return queries, nil
}
