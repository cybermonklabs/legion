package githubrest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/legion/daemon/internal/urljoin"
)

// GraphQLEndpoint is GitHub's GraphQL endpoint under endpoint, GitHub's API root; empty is
// https://api.github.com/graphql. /graphql is joined onto endpoint's own path (urljoin.Join), so an
// endpoint configured with a path and a query keeps both. It is "" where endpoint cannot be read as
// an absolute URL with a host, and GraphQL refuses a client whose API is not one, in fixed words.
// A Client whose API is this sends queries (Client.GraphQL).
func GraphQLEndpoint(endpoint string) string {
	joined, err := urljoin.Join(cmp.Or(endpoint, "https://api.github.com"), "/graphql")
	if err != nil {
		return ""
	}
	return joined.String()
}

// graphQLAccept is the media type a query asks for: the merge-info preview, under which GitHub
// documents a pull request's `mergeStateStatus`, the field the merge-state reads ask for. GitHub
// reads the GraphQL API's previews from the Accept header, as gh's own client sends this one.
const graphQLAccept = "application/vnd.github.merge-info-preview+json"

// GraphQLError is one entry of the `errors` list a GraphQL answer carries: GitHub's message, its
// type where it names one (RATE_LIMITED, NOT_FOUND, FORBIDDEN and others), and the path of the
// field it is about, each step a field name or alias, or an index: ["repository", "p87"] for the
// pull request a query asked for under the alias p87. GitHub's words, the type, the message and
// each name of the path, are held as the rule reads them (graphQLErrors), so the entry prints
// none of the token the query carried; a caller matches a step against an alias it wrote itself.
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

// graphQLAnswered is one entry of an answer's `errors` list as the answer decodes it
// (oauth2call.Answer.Decode): its type and message as GitHub's words, held with the record of the
// query's request, and its path step by step.
type graphQLAnswered struct {
	Type    oauth2call.RemoteText `json:"type"`
	Message oauth2call.RemoteText `json:"message"`
	Path    []graphQLStep         `json:"path"`
}

// graphQLStep is one step of an error's path: a field name or an alias (Name, GitHub's words), or
// an index of a list where Named is false.
type graphQLStep struct {
	Named bool
	Name  oauth2call.RemoteText
	Index int
}

// UnmarshalJSON reads a step: a JSON string as Name, as a RemoteText decodes one, and anything
// else as Index.
func (s *graphQLStep) UnmarshalJSON(data []byte) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '"' {
		s.Named = true
		return json.Unmarshal(trimmed, &s.Name)
	}
	return json.Unmarshal(data, &s.Index)
}

// graphQLErrors is answered as GraphQLErrors, each word of GitHub's as the rule reads it.
func graphQLErrors(answered []graphQLAnswered) GraphQLErrors {
	if len(answered) == 0 {
		return nil
	}
	errs := make(GraphQLErrors, len(answered))
	for i, entry := range answered {
		path := make([]any, len(entry.Path))
		for j, step := range entry.Path {
			path[j] = step.Index
			if step.Named {
				path[j] = step.Name.String()
			}
		}
		errs[i] = GraphQLError{Type: entry.Type.String(), Message: entry.Message.String(), Path: path}
	}
	return errs
}

// graphQLRateLimited says whether answered, the errors of a 200 GitHub sent with headers, read
// as read, is GitHub's rate limit: an error of GraphQL's own type for it (RATE_LIMITED), or,
// whatever the errors' types, the signals a REST answer's limit carries (signals: a retry-after,
// x-ratelimit-remaining: 0, or an answer naming the secondary limit), which GitHub documents it
// may answer a query 200 with. Each is decided on GitHub's words as they came.
func graphQLRateLimited(answered []graphQLAnswered, headers http.Header, named bool) bool {
	for _, entry := range answered {
		if entry.Type.Equal("RATE_LIMITED") {
			return true
		}
	}
	return len(answered) > 0 && signals(headers, named)
}

// graphQLHasData says whether body, a GraphQL answer as it came, carries `data` that is not
// null: a test of the answer's bytes (oauth2call.Holds), which keeps nothing of them.
func graphQLHasData(body []byte) bool {
	var answer struct {
		Data json.RawMessage `json:"data"`
	}
	return json.Unmarshal(body, &answer) == nil && len(answer.Data) > 0 && string(answer.Data) != "null"
}

// BudgetGraphQL is the GraphQL API's own primary budget, the points of the token's queries, which
// GitHub keeps apart from the REST API's.
const BudgetGraphQL Budget = "graphql"

// GraphQL sends query with variables to the client's API, GitHub's GraphQL endpoint
// (GraphQLEndpoint), and reads the answer's `data` into into, whatever part of it GitHub gave.
// GitHub budgets queries apart from the REST API, by the points each costs, and a query of twenty
// pull requests costs one point. An answer other than 2xx is an *Answer, as a REST call's is, a
// rate limit among them, and so is a 200 whose errors are GitHub's rate limit
// (graphQLRateLimited: GraphQL's RATE_LIMITED, a retry-after, an exhausted budget, or the
// secondary limit's message), with the wait its headers ask, or a minute (LimitHeaders.WaitAt). A
// 2xx whose `errors` list is not empty is GraphQLErrors after the data it carries is read, so a
// caller that asked for several things takes those GitHub answered and reads each error's Path for
// the rest; one with no `data` and no errors is an error too. It is not named Query: the daemon's
// transaction guard (internal/textcheck) reads every method of that name that takes a context
// first as a database statement.
//
// The answer is read as every answer of the client's is (oauth2call.ReadAnswer) and decoded in
// parts through it (oauth2call.Answer.Decode): its errors, and its data into into, so every
// RemoteText into holds is held with the record of the query's request. What the answer is - a
// rate limit, an answer with data - is decided on its words as they came.
func (c Client) GraphQL(ctx context.Context, query string, variables map[string]any, into any) error {
	if _, err := urljoin.Join(c.API, ""); err != nil {
		return fmt.Errorf("send the GitHub GraphQL query: its endpoint: %w", err)
	}
	encoded, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("encode the GraphQL query: %w", err)
	}
	_, response, read, err := c.exchange(ctx, http.MethodPost, c.API, "", encoded, graphQLAccept)
	if err != nil {
		return err
	}
	var answered struct {
		Errors []graphQLAnswered `json:"errors"`
	}
	if err := read.Decode(&answered); err != nil {
		return err
	}
	errs := graphQLErrors(answered.Errors)
	named := oauth2call.Holds(read, namesSecondaryLimit)
	if graphQLRateLimited(answered.Errors, response.Header, named) {
		path := "/graphql"
		if target, err := url.Parse(c.API); err == nil {
			path = ShownPath(c.API, target, "")
		}
		// A primary limit of a query is GraphQL's own budget, which GitHub names in
		// x-ratelimit-resource; spent reads a budget named nowhere as the REST API's core one.
		budget := spent(response.Header, named)
		if budget == BudgetCore {
			budget = BudgetGraphQL
		}
		limit := limitHeaders(response)
		wait, _ := limit.WaitAt(time.Now())
		return &Answer{Method: http.MethodPost, Path: path, Status: http.StatusOK, Body: errs.Error(), RateLimited: true,
			RetryAfter: wait, Budget: budget, Limit: limit, read: read}
	}
	data := oauth2call.Holds(read, graphQLHasData)
	if data {
		// The data member decodes into into itself: an interface value set to a non-nil pointer,
		// which the answer's decode follows.
		held := struct {
			Data any `json:"data"`
		}{Data: into}
		if err := read.Decode(&held); err != nil {
			return err
		}
	}
	if len(errs) > 0 {
		return errs
	}
	if !data {
		return errors.New("GitHub answered the GraphQL query with no data")
	}
	return nil
}
