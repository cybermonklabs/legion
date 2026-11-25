package leakguard

import (
	"slices"
	"strings"
	"testing"
)

// A `gh api` call sends GitHub a GraphQL query wherever its endpoint reaches the path /graphql of
// api.github.com, as a server reads a path: under gh's own word `graphql`, with a slash, a query
// or a fragment, in another case, percent-escaped, through a `..`, and as the API's whole URL on
// any port. A path elsewhere, another host, a longer word and a path whose escapes do not decode
// are none. The endpoint is found past `-X POST`, a cluster's `-iX POST`, a bare `--`, and options
// whose value it is not: a field given before it and a template whose value is itself `-X`.
func TestGhAPIGraphQLIsTheEndpointsPath(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		want     bool
	}{
		{"graphql", true},
		{"/graphql", true},
		{"graphql?x=1", true},
		{"/graphql?x=1", true},
		{"https://api.github.com/graphql", true},
		{"https://api.github.com/graphql?x=1", true},
		{"https://API.GITHUB.COM/graphql", true},
		{"https://api.github.com:443/graphql", true},
		{"GRAPHQL", true},
		{"%67raphql", true},
		{"graphql#x", true},
		{"repos/../graphql", true},
		{"repos/o/r/graphql", false},
		{"graphqlx", false},
		{"https://example.com/graphql", false},
		{"user/graphql", false},
		{"%zz/graphql", false},
	} {
		for _, args := range [][]string{
			{"api", tc.endpoint},
			{"api", "-X", "POST", tc.endpoint},
			{"api", "-iX", "POST", tc.endpoint},
			{"api", "--", tc.endpoint},
			{"api", "-f", "query={ viewer { login } }", tc.endpoint},
			{"api", "-t", "-X", tc.endpoint},
		} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				if got := readGhArgv(args).endpoint.kind == ghEndpointGraphQL; got != tc.want {
					t.Fatalf("readGhArgv(%q).endpoint.kind == ghEndpointGraphQL is %v, want %v", args, got, tc.want)
				}
			})
		}
	}
}

// The lexer of a GraphQL query's string literals: each literal decoded as the GraphQL
// specification decodes it, which is the value GitHub stores, and every text the specification
// does not lex refused, so that the guard reads that text as written (graphQLRecords).
func TestGraphQLStringsAreTheValuesGitHubStores(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         []string
	}{
		{"no literal", `query { viewer { login } }`, nil},
		{"two literals", `{ a(x: "one", y: "two") }`, []string{"one", "two"}},
		{"an empty literal", `{ a(x: "") }`, []string{""}},
		{"the one-character escapes", `"q\" b\\ s\/ \b\f\n\r\t"`, []string{"q\" b\\ s/ \b\f\n\r\t"}},
		{"a four-digit escape", `"\u0041\u00e9"`, []string{"Aé"}},
		{"a surrogate pair", `"\uD83D\uDE00"`, []string{"\U0001F600"}},
		{"a braced escape", `"\u{41}\u{1F600}\u{0000041}"`, []string{"A\U0001F600A"}},
		{"a literal's own bytes", `"é and 😀"`, []string{"é and 😀"}},
		{"a comment", "# \"not a literal\"\n{ a(x: \"one\") } # \"nor this\"", []string{"one"}},
		{"a comment ended by a carriage return", "# \"not a literal\"\r{ a(x: \"one\") }", []string{"one"}},
		{"a # inside a literal", `{ a(x: "# one") }`, []string{"# one"}},
		{"a block string on one line", `"""one "two" three"""`, []string{`one "two" three`}},
		{"a block string's escape", `"""one \""" two"""`, []string{`one """ two`}},
		{"a block string's escapes of a plain literal, as written", `"""a\nb \u0041"""`, []string{`a\nb \u0041`}},
		{
			"a block string's indentation and blank lines",
			"\"\"\"\n\n    one\n      two\n\n    three\n  \"\"\"",
			[]string{"one\n  two\n\nthree"},
		},
		{"a block string's first line keeps its indentation", "\"\"\"  one\n    two\n    three\"\"\"", []string{"  one\ntwo\nthree"}},
		{"a block string's line terminators", "\"\"\"\r\n  one\r  two\r\n\"\"\"", []string{"one\ntwo"}},
		{"a block string of blank lines", "\"\"\"\n   \n\t\n\"\"\"", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := graphQLStrings(tc.source)
			if !ok || !slices.Equal(got, tc.want) {
				t.Fatalf("graphQLStrings(%q) = %q, %v; want %q, true", tc.source, got, ok, tc.want)
			}
		})
	}
	for _, tc := range []struct{ name, source string }{
		{"an unterminated literal", `{ a(x: "one) }`},
		{"a literal across a line", "{ a(x: \"one\ntwo\") }"},
		{"a literal across a carriage return", "{ a(x: \"one\rtwo\") }"},
		{"an unknown escape", `"one \q two"`},
		{"a backslash at the end", `"one \`},
		{"a short four-digit escape", `"\u004"`},
		{"a four-digit escape that is no hex", `"\u00G1"`},
		{"a lone leading surrogate", `"\uD83D"`},
		{"a leading surrogate before a letter", `"\uD83Dx"`},
		{"a leading surrogate before another", `"\uD83D\uD83D"`},
		{"a lone trailing surrogate", `"\uDE00"`},
		{"a braced surrogate", `"\u{D83D}"`},
		{"a braced escape past the last code point", `"\u{110000}"`},
		{"an empty braced escape", `"\u{}"`},
		{"an unclosed braced escape", `"\u{41"`},
		{"a braced escape that is no hex", `"\u{4G}"`},
		{"an unterminated block string", `"""one`},
		{"a block string whose close is escaped", `"""one \"""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := graphQLStrings(tc.source); ok {
				t.Fatalf("graphQLStrings(%q) = %q, true; want it not to lex", tc.source, got)
			}
		})
	}
}
