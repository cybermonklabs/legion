package githubrest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// GraphQLError is one entry of the `errors` list a GraphQL answer carries: GitHub's message, and
// its type where it names one (RATE_LIMITED, NOT_FOUND, FORBIDDEN and others).
type GraphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
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

// GraphQL sends query with variables to the client's API, GitHub's GraphQL endpoint
// (GraphQLEndpoint), and reads the answer's `data` into into. GitHub budgets queries apart from the
// REST API, by the points each costs, and a query answering a hundred pull requests costs about one
// point. An answer other than 2xx is an *Answer, as a REST call's is, a rate limit among them; a
// 2xx whose `errors` list is not empty is GraphQLErrors, partial data and all, and one with no
// `data` at all is an error too. It is not named Query: the daemon's transaction guard
// (internal/textcheck) reads every method of that name that takes a context first as a database
// statement.
func (c Client) GraphQL(ctx context.Context, query string, variables map[string]any, into any) error {
	encoded, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("encode the GraphQL query: %w", err)
	}
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors GraphQLErrors   `json:"errors"`
	}
	if _, _, err := c.exchange(ctx, http.MethodPost, c.API, encoded, &answer, graphQLAccept); err != nil {
		return err
	}
	if len(answer.Errors) > 0 {
		return answer.Errors
	}
	if len(answer.Data) == 0 || string(answer.Data) == "null" {
		return errors.New("GitHub answered the GraphQL query with no data")
	}
	return json.Unmarshal(answer.Data, into)
}
