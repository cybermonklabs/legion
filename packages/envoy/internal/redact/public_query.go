package redact

import (
	"net/http"
	"net/url"
)

// QueryTokens is each value of query, which r carries in its URL's query, under a name no wire
// contract declares public on r's route (publicRequestNames): the credentials among query values a
// client adds to its own requests, which whatever answers can echo, so that client can redact them
// (Tokens) from the words it prints, as requestTokens counts every such value of a request a
// RefusalClient records. A name nobody listed counts.
func QueryTokens(r *http.Request, query url.Values) []string {
	var tokens []string
	for name, values := range query {
		if !publicField(r, name) {
			tokens = append(tokens, values...)
		}
	}
	return tokens
}
