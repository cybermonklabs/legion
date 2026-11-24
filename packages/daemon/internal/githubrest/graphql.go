package githubrest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GraphQLEndpoint is GitHub's GraphQL endpoint under endpoint, GitHub's API root; empty is
// https://api.github.com/graphql. A Client whose API is this sends queries (Client.GraphQL).
func GraphQLEndpoint(endpoint string) string {
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	return strings.TrimRight(endpoint, "/") + "/graphql"
}

// graphQLAccept is the media type a query asks for: the merge-info preview, under which GitHub
// documents a pull request's `mergeStateStatus`, the field the merge-state reads ask for. GitHub
// reads the GraphQL API's previews from the Accept header, as gh's own client sends this one.
const graphQLAccept = "application/vnd.github.merge-info-preview+json"

// GraphQLError is one entry of the `errors` list a GraphQL answer carries: GitHub's message, its
// type where it names one (RATE_LIMITED, NOT_FOUND, FORBIDDEN and others), and the path of the
// field it is about, each step a field name or alias, or an index: ["repository", "p87"] for the
// pull request a query asked for under the alias p87.
type GraphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// GraphQLErrors is a GraphQL answer GitHub sent with a 2xx status and an `errors` list: the query
// was answered with no data the caller can take, or only part of it.
type GraphQLErrors []GraphQLError

func (e GraphQLErrors) Error() string {
	messages := make([]string, 0, len(e))
	for _, entry := range e {
		if entry.Type != "" {
			messages = append(messages, entry.Type+": "+entry.Message)
			continue
		}
		messages = append(messages, entry.Message)
	}
	return "GitHub answered the GraphQL query with errors: " + strings.Join(messages, "; ")
}

// rateLimited says whether one of e is GitHub's GraphQL rate limit, which it answers 200 with.
func (e GraphQLErrors) rateLimited() bool {
	for _, entry := range e {
		if entry.Type == "RATE_LIMITED" {
			return true
		}
	}
	return false
}

// BudgetGraphQL is the GraphQL API's own primary budget, the points of the token's queries, which
// GitHub keeps apart from the REST API's.
const BudgetGraphQL Budget = "graphql"

// GraphQL sends query with variables to the client's API, GitHub's GraphQL endpoint
// (GraphQLEndpoint), and reads the answer's `data` into into, whatever part of it GitHub gave.
// GitHub budgets queries apart from the REST API, by the points each costs, and a query of twenty
// pull requests costs one point. An answer other than 2xx is an *Answer, as a REST call's is, a
// rate limit among them, and so is a 200 whose errors name GitHub's GraphQL rate limit
// (RATE_LIMITED), with the wait its headers ask (limitWait). A 2xx whose `errors` list is not empty
// is GraphQLErrors after the data it carries is read, so a caller that asked for several things
// takes those GitHub answered and reads each error's Path for the rest; one with no `data` and no
// errors is an error too. It is not named Query: the daemon's transaction guard (internal/textcheck)
// reads every method of that name that takes a context first as a database statement.
func (c Client) GraphQL(ctx context.Context, query string, variables map[string]any, into any) error {
	encoded, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("encode the GraphQL query: %w", err)
	}
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors GraphQLErrors   `json:"errors"`
	}
	_, headers, err := c.exchange(ctx, http.MethodPost, c.API, encoded, &answer, graphQLAccept)
	if err != nil {
		return err
	}
	if answer.Errors.rateLimited() {
		path := "/graphql"
		if target, err := url.Parse(c.API); err == nil {
			path = c.answerPath(target)
		}
		return &Answer{Method: http.MethodPost, Path: path, Status: http.StatusOK, Body: answer.Errors.Error(), RateLimited: true,
			RetryAfter: limitWait(headers, time.Now()), Budget: BudgetGraphQL, Limit: limitHeaders(headers)}
	}
	data := len(answer.Data) > 0 && string(answer.Data) != "null"
	if data {
		if err := json.Unmarshal(answer.Data, into); err != nil {
			return err
		}
	}
	if len(answer.Errors) > 0 {
		return answer.Errors
	}
	if !data {
		return errors.New("GitHub answered the GraphQL query with no data")
	}
	return nil
}
